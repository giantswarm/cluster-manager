package tools

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/cluster-manager/internal/detect"
)

// remove_model_cache (giantswarm/cluster-manager#83): the model cache claims
// outlive every pool and the slice release by design, and are billed every
// month they exist. Until now nothing removed them — every refusal naming
// "remove the claim" as a way out pointed at nothing a caller could do
// through cluster-manager. The tool removes a cluster's model cache in
// order: cluster-manager's slice release, where it runs with the cache on,
// is upgraded to serve without it first — the connectivity chart applies the
// claim from a hook on every upgrade, so a claim deleted under a slice with
// the cache on came back with the next one —, then every claim (or the one
// named) is deleted on the cluster as the caller; the volume goes with the
// claim under the class's Delete reclaim policy, a Retain volume is a
// warning. Refused while a pod mounts a claim to be removed: the delete
// would hang on the claim's protection finalizer until the pod is gone, and
// the served models are what to unload first.

// RemoveModelCacheInput is remove_model_cache's input.
type RemoveModelCacheInput struct {
	Cluster   string
	Namespace string
	// Claim names one model cache claim to remove; empty removes every
	// claim of the serving namespace.
	Claim  string
	Mode   string
	DryRun bool
}

// claimHold is a claim a pod of the serving namespace mounts: the pods and,
// where a pod is a predictor's, the served models behind them.
type claimHold struct {
	claim  *detect.CacheClaim
	pods   []string
	models []string
}

// RemoveModelCache removes the cluster's model cache: the slice release's
// cache off where it is on, then the claims deleted on the cluster as the
// caller, within the call's budget (the pending rest on the re-run).
func (s *Service) RemoveModelCache(ctx context.Context, in RemoveModelCacheInput) (*WriteResult, error) {
	start := time.Now()
	if err := s.checkMode(in.Mode, false); err != nil {
		return nil, err
	}
	k := s.clients(ctx)
	c, err := s.getCluster(ctx, k, in.Cluster, in.Namespace)
	if err != nil {
		return nil, err
	}
	dyn := k.Dynamic
	t := s.target(ctx, dyn, c)
	cluster := c.GetName()
	if t.Reader == nil {
		return nil, &ErrRefused{Reason: fmt.Sprintf("the model cache of %s cannot be read as you (%s): its claims live in the serving namespace on the cluster — make the cluster readable as you and re-run", cluster, t.Reason)}
	}
	reads, err := s.readSlice(ctx, dyn, t)
	if err != nil {
		return nil, err
	}
	switch {
	case reads.serving.Status == detect.StatusUnknown:
		return nil, &ErrRefused{Reason: fmt.Sprintf("whether serving on %s is cluster-manager's cannot be told (%s): the model cache is the serving layer's setting — make the cluster readable as you and re-run", cluster, reads.serving.Reason)}
	case reads.serving.Present() && reads.serving.Provider != detect.ProviderClusterManager:
		return nil, &ErrRefused{Reason: fmt.Sprintf("the model cache on %s is the serving layer's setting, and serving there is provided by %s (%s), not composed by cluster-manager — remove the cache where that layer is configured", cluster, providerDescription(reads.serving.Provider), strings.Join(reads.serving.Evidence, "; "))}
	}
	read := s.readCacheClaims(ctx, t)
	if read.err != "" {
		return nil, &ErrRefused{Reason: fmt.Sprintf("the model cache claims of %s on %s cannot be read as you (%s): re-run once you may list the claims and their volumes", read.namespace, cluster, read.err)}
	}
	infra := awsInfrastructure(ctx, dyn, c)
	read.priced(infra.region, reads.cache)
	selected, err := selectClaims(read, in.Claim, cluster)
	if err != nil {
		return nil, err
	}
	// The slice serves without the cache from now on when it mounts a claim
	// that goes — every claim, or the named one being the mounted one.
	sliceOff := reads.cache != nil && reads.cache.Enabled && (in.Claim == "" || in.Claim == reads.cache.Claim)
	if len(selected) == 0 && !sliceOff {
		return nil, &ErrNotFound{What: fmt.Sprintf("model cache on %s (no %s* claim in %s, and no slice release of cluster-manager's runs with the cache on there)", cluster, read.base, read.namespace)}
	}
	held, err := heldClaims(ctx, t.Reader, read.namespace, selected)
	if err != nil {
		return nil, &ErrRefused{Reason: fmt.Sprintf("cannot tell whether a pod mounts the model cache of %s (%v): a claim a pod mounts is not deleted until the pod is gone — re-run once you may list the pods of %s", cluster, err, read.namespace)}
	}
	if len(held) > 0 {
		return nil, heldRefusal(cluster, held)
	}
	out := &WriteResult{Cluster: cluster, Namespace: c.GetNamespace(), Mode: in.Mode, DryRun: in.DryRun, Objects: []ObjectAction{}, CacheClaims: read.claims, RemovedClaims: selected}
	budget := s.budget(ctx, start)
	if sliceOff {
		if err := s.sliceWithoutCache(ctx, dyn, c, t, reads, in.DryRun, out, budget); err != nil {
			return nil, err
		}
	}
	td := newTeardown(ctx, dyn, in.DryRun, out, budget)
	res := t.Reader.Resource(detect.PersistentVolumeClaimGVR).Namespace(read.namespace)
	for _, claim := range selected {
		if err := td.delete(res, ObjectAction{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: claim.Name, Namespace: read.namespace}); err != nil {
			return nil, err
		}
		if claim.ReclaimPolicy == detect.ReclaimRetain {
			out.Warnings = append(out.Warnings, fmt.Sprintf("volume %s of claim %s is kept (reclaim policy Retain): it stays, and is billed, until deleted by hand", claim.Volume, claim))
		}
	}
	td.finish()
	out.Cache = &CacheSetting{Note: removalNote(cluster, reads, sliceOff, selected)}
	logApplied(ctx, "remove_model_cache", out, start)
	return out, nil
}

// selectClaims is the claims the call removes: the one named — a name that
// is no claim of the namespace is not found —, else every claim as read.
func selectClaims(read cacheClaims, name, cluster string) ([]*detect.CacheClaim, error) {
	if name == "" {
		return read.claims, nil
	}
	claim := read.named(name)
	if claim == nil {
		return nil, &ErrNotFound{What: fmt.Sprintf("model cache claim %s on %s (the claims there: %s)", read.ref(name), cluster, describeClaimsOrNone(read.claims))}
	}
	return []*detect.CacheClaim{claim}, nil
}

// describeClaimsOrNone words the claims for a message, `none` for no claim.
func describeClaimsOrNone(claims []*detect.CacheClaim) string {
	if len(claims) == 0 {
		return "none"
	}
	return describeClaims(claims)
}

// heldClaims lists, among the claims, the ones a pod of the serving namespace
// mounts — a pod that has not finished, whatever its node — with the pods
// and the served models behind the predictors among them, read as the
// caller.
func heldClaims(ctx context.Context, reader dynamic.Interface, namespace string, claims []*detect.CacheClaim) ([]claimHold, error) {
	if len(claims) == 0 {
		return nil, nil
	}
	pods, err := reader.Resource(detect.PodsGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list the pods of %s: %w", namespace, err)
	}
	models, err := detect.ServedModels(ctx, reader)
	if err != nil {
		return nil, err
	}
	served := map[string]string{}
	for _, m := range models {
		served[m.Kind+"/"+m.Name] = m.String()
	}
	byName := map[string]*claimHold{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		switch nestedString(pod, "status", "phase") {
		case "Succeeded", "Failed":
			continue
		}
		for _, name := range claimsMounted(pod) {
			var claim *detect.CacheClaim
			for _, c := range claims {
				if c.Name == name {
					claim = c
				}
			}
			if claim == nil {
				continue
			}
			hold := byName[name]
			if hold == nil {
				hold = &claimHold{claim: claim}
				byName[name] = hold
			}
			hold.pods = appendNew(hold.pods, pod.GetNamespace()+"/"+pod.GetName())
			if kind, model, ok := detect.PredictorOf(pod); ok {
				if s, known := served[kind+"/"+model]; known {
					hold.models = appendNew(hold.models, s)
				} else {
					hold.models = appendNew(hold.models, kind+" "+pod.GetNamespace()+"/"+model)
				}
			}
		}
	}
	var out []claimHold
	for _, hold := range byName {
		sort.Strings(hold.pods)
		sort.Strings(hold.models)
		out = append(out, *hold)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].claim.Name < out[j].claim.Name })
	return out, nil
}

// claimsMounted names the PersistentVolumeClaims a pod's volumes mount.
func claimsMounted(pod *unstructured.Unstructured) []string {
	volumes, _, _ := unstructured.NestedSlice(pod.Object, "spec", "volumes")
	var out []string
	for _, v := range volumes {
		volume, _ := v.(map[string]any)
		pvc, _ := volume["persistentVolumeClaim"].(map[string]any)
		if name, _ := pvc["claimName"].(string); name != "" {
			out = appendNew(out, name)
		}
	}
	return out
}

// heldRefusal refuses the removal while a pod mounts a claim, naming the
// claims, the pods and the served models to unload first — beside the text
// the structured refused{models, hint} block the portal renders.
func heldRefusal(cluster string, held []claimHold) error {
	var parts, models []string
	for _, h := range held {
		what := fmt.Sprintf("%s is mounted by pod %s", h.claim, strings.Join(h.pods, ", "))
		if len(h.models) > 0 {
			what += fmt.Sprintf(" (%s)", strings.Join(h.models, ", "))
		}
		parts = append(parts, what)
		for _, m := range h.models {
			models = appendNew(models, m)
		}
	}
	slices.Sort(models)
	hint := "Unload the served model(s) with model-manager's unload_model and re-run: a claim a pod mounts is not deleted until the pod is gone."
	if len(models) == 0 {
		hint = "Wait for the pods mounting the claim to finish, or remove them, and re-run: a claim a pod mounts is not deleted until the pod is gone."
	}
	return &ErrRefused{
		Reason:  fmt.Sprintf("the model cache of %s is in use — %s: a claim a pod mounts is not deleted until the pod is gone (its protection finalizer holds it), and a predictor's weights are read from it; %s", cluster, strings.Join(parts, "; "), strings.ToLower(hint[:1])+hint[1:]),
		Refused: &Refused{Nodes: []string{}, Models: models, Hint: hint, ReadFrom: readFromCluster},
	}
}

// sliceWithoutCache upgrades cluster-manager's slice release of the cluster
// to serve without the cache (modelServing.cache.enabled false) — the same
// composition enable_model_serving with cache false writes — so the
// connectivity chart's hook applies no claim again.
func (s *Service) sliceWithoutCache(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured, t target, reads sliceReads, dryRun bool, out *WriteResult, budget time.Time) error {
	facts, err := s.sliceCluster(ctx, dyn, c)
	if err != nil {
		return err
	}
	pool, err := onlyPool(ctx, dyn, c.GetNamespace(), c.GetName(), "")
	if err != nil {
		return err
	}
	serving, slice, objs, err := s.sliceRelease(reads, t, facts, pool, sliceCache{on: false})
	if err != nil {
		return err
	}
	out.Serving, out.Slice = serving, slice
	return applyAll(ctx, dyn, objs, dryRun, out, budget)
}

// removalNote is the answer's word on what went and what follows.
func removalNote(cluster string, reads sliceReads, sliceOff bool, removed []*detect.CacheClaim) string {
	var parts []string
	if len(removed) > 0 {
		names := make([]string, 0, len(removed))
		for _, c := range removed {
			name := c.String()
			if standing := c.Standing(); standing != "" {
				name += " (" + standing + ")"
			}
			names = append(names, name)
		}
		parts = append(parts, fmt.Sprintf("%d claim(s) deleted — %s — with their volumes under the class's Delete reclaim policy, and with them the cached weights and compiled graphs of every model served from them; the bill stops with the volume", len(removed), strings.Join(names, ", ")))
	}
	if sliceOff {
		parts = append(parts, fmt.Sprintf("the slice release %s is upgraded to modelServing.cache.enabled false, so the connectivity chart applies no claim again and every predictor of %s downloads its weights into its pod's ephemeral storage (the node's local disk) at each start — about 90 s more per cold start", reads.release, cluster))
	}
	parts = append(parts, "a later create_node_pool or enable_model_serving with cache true creates the claim anew and is the slice's upgrade back to the cache")
	return fmt.Sprintf("the model cache of %s is removed: %s", cluster, strings.Join(parts, "; "))
}
