package tools

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/detect"
)

// The serving slice's teardown (giantswarm/cluster-manager#28, #37). The
// slice release's kserve-runtime-configs child installs KServe's well-known
// LLMInferenceServiceConfigs into the release namespace on the target; the
// llm-d controller (the kserve-llmisvc-resources child) puts
// detect.LLMISVCConfigFinalizer on each, and its validating webhook denies
// every delete of them while it runs — `well-known config … cannot be
// deleted`, to Helm's uninstall of their release as to anyone else. Removing
// the slice release at once uninstalls both children in the same instant: the
// configs go the moment the webhook is gone, the controller that would clear
// their finalizer with it, and they sit terminating until the next slice
// installed there adopts and loses them. So the slice goes in order: the
// controller's release first; then, once no controller runs on the target,
// the configs are removed by cluster-manager with their finalizer taken off
// (nothing else will); then the configs' release — an uninstall that finds
// nothing to delete —, then the rest. Every step within the call's budget:
// what does not fit is pending and the re-run completes it.
//
// The models the platform serves go the same way. A forced teardown takes
// the controller away from under them (the guards refuse it without force),
// and the controller's finalizer on their LLMInferenceServices
// (detect.LLMISVCFinalizer) is then cleared by nothing: a model stopped
// afterwards sat "Stopping" for good on an installation (a forced removal,
// then a stop). So once no controller runs, the served objects model-manager
// composed in the serving namespace are removed by cluster-manager beside the
// configs, finalizer and all; their workloads go with them through their
// owner references. A serving object made by hand or through GitOps is
// someone else's and is named, not touched.
const (
	// runtimeConfigsRelease is the slice's child HelmRelease of the
	// well-known configs, named after its component in the meta chart.
	runtimeConfigsRelease = "kserve-runtime-configs"
	// llmisvcResourcesRelease is the slice's child HelmRelease of the llm-d
	// controller and its webhook.
	llmisvcResourcesRelease = "kserve-llmisvc-resources"
	// requestedAtAnnotation asks Flux to reconcile an object now, the way
	// `flux reconcile` does.
	requestedAtAnnotation = "reconcile.fluxcd.io/requestedAt"
)

// teardown is one write call's removals: written one at a time in order
// within the call's budget. A write about to start with less than
// writeReserve of the budget left is not started — it and every later step
// are pending, the answer partial and in time, the re-run completes them
// (giantswarm/cluster-manager#34's pattern on the delete side). Dry-run
// touches nothing and reports what would happen.
type teardown struct {
	ctx    context.Context
	dyn    dynamic.Interface
	dryRun bool
	out    *WriteResult
	budget time.Time
	cut    bool
}

func newTeardown(ctx context.Context, dyn dynamic.Interface, dryRun bool, out *WriteResult, budget time.Time) *teardown {
	return &teardown{ctx: ctx, dyn: dyn, dryRun: dryRun, out: out, budget: budget}
}

// fits reports whether a write may start: at least writeReserve of the
// budget left. Once it is not, the teardown is cut and stays cut. A dry run
// writes nothing and always fits.
func (td *teardown) fits() bool {
	if td.dryRun || td.cut {
		return !td.cut
	}
	if remaining := time.Until(td.budget); remaining < writeReserve {
		td.cut = true
		slog.WarnContext(td.ctx, "teardown cut short: the remaining budget is below the write reserve", "remaining", remaining.Round(time.Millisecond), "reserve", writeReserve)
	}
	return !td.cut
}

// record appends what happened to one object: pending once the teardown is
// cut, would-… on a dry run.
func (td *teardown) record(act ObjectAction) {
	switch {
	case td.cut:
		act.Action = actionPending
		td.out.Partial = true
	case td.dryRun:
		act.Action = "would-" + act.Action
	}
	td.out.Objects = append(td.out.Objects, act)
}

// delete removes one object when the budget allows and records it; gone
// already is fine.
func (td *teardown) delete(res dynamic.ResourceInterface, act ObjectAction) error {
	act.Action = actionDelete
	if td.fits() && !td.dryRun {
		defer timed(td.ctx, "delete", "kind", act.Kind, "name", act.String())()
		if err := res.Delete(td.ctx, act.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete %s %s: %w", act.Kind, act.String(), err)
		}
	}
	td.record(act)
	return nil
}

// apply lands one planned create or update when the budget allows and
// records it with its manifest; an unchanged object is recorded as it
// stands.
func (td *teardown) apply(p applyPlan) error {
	td.out.Manifests = append(td.out.Manifests, redacted(p.obj))
	if p.act.Action == actionUnchanged {
		td.out.Objects = append(td.out.Objects, p.act)
		return nil
	}
	if td.fits() && !td.dryRun {
		if err := p.write(td.ctx); err != nil {
			return err
		}
	}
	td.record(p.act)
	return nil
}

// deleteNodeClaims removes the pool's idle nodes through their NodeClaims on
// the cluster, as the caller, before anything else: Karpenter drains each
// node and terminates its instance within the pool's terminationGracePeriod,
// so the pool release's removal finds no node and nobody waits for the
// empty-node consolidation (giantswarm/cluster-manager#49). A delete the
// caller is not allowed is a refusal with the ways out; nothing else has
// been written by then.
func (td *teardown) deleteNodeClaims(reader dynamic.Interface, idle []*poolNode) error {
	if len(idle) == 0 {
		return nil
	}
	res := reader.Resource(detect.NodeClaimGVR)
	for _, n := range idle {
		err := td.delete(res, ObjectAction{APIVersion: n.claim.GetAPIVersion(), Kind: n.claim.GetKind(), Name: n.claim.GetName()})
		if apierrors.IsForbidden(err) {
			return &ErrRefused{Reason: fmt.Sprintf("%v: removing the pool's idle node %s needs the delete of its NodeClaim as you — ask for it, wait for Karpenter to consolidate the empty node, or pass force to remove the pool regardless (its release's removal takes the nodes down)", err, n.name())}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// finish names the re-run when the teardown was cut.
func (td *teardown) finish() {
	if !td.out.Partial {
		return
	}
	pending := 0
	for _, o := range td.out.Objects {
		if o.Action == actionPending {
			pending++
		}
	}
	td.out.NextStep = fmt.Sprintf("%d object(s) are pending: the answer went out within the caller's deadline instead of removing them — re-run with the same arguments, the teardown continues where it stands", pending)
}

// deletePlan is one of cluster-manager's own objects against its live
// counterpart: what the teardown does with it.
type deletePlan struct {
	res dynamic.ResourceInterface
	act ObjectAction
}

// planDeletes reads every target concurrently and decides — every refusal
// before any write: an object of that name someone else owns is refused, an
// absent one skipped. The plans keep the targets' order.
func planDeletes(ctx context.Context, dyn dynamic.Interface, targets []objectRef) ([]deletePlan, error) {
	defer timed(ctx, "plan deletes", "targets", len(targets))()
	plans := make([]*deletePlan, len(targets))
	g, gctx := errgroup.WithContext(ctx)
	for i, target := range targets {
		g.Go(func() error {
			res := dyn.Resource(target.gvr).Namespace(target.ns)
			obj, err := res.Get(gctx, target.name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("get %s %s/%s: %w", target.gvr.Resource, target.ns, target.name, err)
			}
			if !compose.OwnedBy(obj) {
				return &ErrRefused{Reason: fmt.Sprintf("%s %s/%s %s: cluster-manager removes only what it created — %s", obj.GetKind(), target.ns, target.name, ownerDescription(obj), removalHint(obj))}
			}
			plans[i] = &deletePlan{res: res, act: ObjectAction{APIVersion: obj.GetAPIVersion(), Kind: obj.GetKind(), Name: target.name, Namespace: target.ns}}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	out := make([]deletePlan, 0, len(plans))
	for _, p := range plans {
		if p != nil {
			out = append(out, *p)
		}
	}
	return out, nil
}

// deleteAll removes the planned objects in order within the budget.
func (td *teardown) deleteAll(plans []deletePlan) error {
	for _, p := range plans {
		if err := td.delete(p.res, p.act); err != nil {
			return err
		}
	}
	return nil
}

// servingTeardown removes the slice's serving objects that must go before
// the slice release, in order: the llm-d controller's child release; the
// served models model-manager composed in the serving namespace and the
// LLMInferenceServiceConfigs of the release namespace on the target once no
// controller runs there — waited for within the budget, removed with their
// finalizer taken off; the configs' child release. A child release Flux
// could not uninstall while the configs were still there is asked to retry
// now that they are gone. Without force a target that cannot be read as the
// caller is a refusal: whether the configs are gone cannot be told.
func (s *Service) servingTeardown(td *teardown, t target, force bool) error {
	ns, cluster := t.Namespace, t.Cluster
	if t.Reader == nil && !force {
		return &ErrRefused{Reason: fmt.Sprintf("cannot tell whether the LLMInferenceServiceConfigs of %s on %s are gone (%s): the serving slice is removed in order so none is left terminating — make the cluster readable as you and re-run, or pass force to remove the slice regardless", ns, cluster, t.Reason)}
	}
	slice := compose.SliceReleaseName(cluster)
	if err := td.deleteChildRelease(ns, slice, llmisvcResourcesRelease); err != nil {
		return err
	}
	if t.Reader == nil {
		td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("whether LLMInferenceServiceConfigs remain in %s on %s cannot be told (%s): one left terminating with %s uncleared breaks the next serving slice installed there — check the namespace, or heal it by re-running create_node_pool once the cluster is readable as you", ns, cluster, t.Reason, detect.LLMISVCConfigFinalizer))
	} else if err := td.purgeWhenControllerGone(t, s.cfg.ServingNamespace); err != nil {
		return err
	}
	return td.deleteChildRelease(ns, slice, runtimeConfigsRelease)
}

// purgeWhenControllerGone removes, once no llm-d controller runs on the
// target, what the controller alone would clear: the served models
// model-manager composed in the serving namespace, and the
// LLMInferenceServiceConfigs of the release namespace — each deleted with its
// finalizer taken off. While the controller runs its webhook denies every
// delete of a config, and nothing clears either finalizer once it is gone.
// The controller's release was deleted a moment ago; Flux removes the
// controller within seconds, and the wait is bounded by the budget: objects
// the call cannot remove in time are pending. A delete the webhook still
// denies, or that fails while the webhook is going, is retried the same way.
// A serving object someone else made (by hand, through GitOps) is named and
// left: it is theirs to remove where it was created.
func (td *teardown) purgeWhenControllerGone(t target, servingNamespace string) error {
	configs, err := detect.Configs(td.ctx, t.Reader, t.Namespace)
	if err != nil {
		return err
	}
	served, err := detect.ServedObjects(td.ctx, t.Reader, servingNamespace)
	if err != nil {
		return err
	}
	models, others := detect.SplitManaged(served)
	if len(others) > 0 {
		td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("%d LLMInferenceService(s) in %s on %s not composed by model-manager are left as they are (%s): with the llm-d controller gone, nothing clears %s once one is stopped — delete them where they were created, finalizer and all", len(others), servingNamespace, t.Cluster, strings.Join(detect.Names(others), ", "), detect.LLMISVCFinalizer))
	}
	if len(configs) == 0 && len(models) == 0 {
		return nil
	}
	defer timed(td.ctx, "purge", "configs", len(configs), "models", len(models))()
	var reason string // why the objects are not gone yet
	for td.fits() && !td.dryRun {
		if detect.LLMISVCControllerRuns(td.ctx, t.Reader) {
			reason = "the llmisvc controller still runs on " + t.Cluster
		} else {
			err := purgeServing(td.ctx, t.Reader, models, configs)
			if err == nil {
				break
			}
			if !transientAdmission(err) {
				return err
			}
			reason = err.Error()
		}
		if err := sleepWithin(td.ctx, pollInterval(time.Until(td.budget))); err != nil {
			return err
		}
	}
	for _, objs := range [][]unstructured.Unstructured{models, configs} {
		for i := range objs {
			td.record(ObjectAction{APIVersion: objs[i].GetAPIVersion(), Kind: objs[i].GetKind(), Name: objs[i].GetName(), Namespace: objs[i].GetNamespace(), Action: actionDelete})
		}
	}
	if len(models) > 0 {
		names := strings.Join(detect.Names(models), ", ")
		switch {
		case td.cut:
			td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("%d served model(s) in %s on %s are pending (%s): the teardown takes the llm-d controller away from under them, and %s is cleared by nothing once it is gone — the re-run removes them with the finalizer taken off: %s", len(models), servingNamespace, t.Cluster, reason, detect.LLMISVCFinalizer, names))
		case td.dryRun:
			td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("%d served model(s) in %s on %s would be removed by cluster-manager with %s taken off once the llmisvc controller is gone (the teardown takes the controller away from under them; nothing clears the finalizer once it is gone): %s", len(models), servingNamespace, t.Cluster, detect.LLMISVCFinalizer, names))
		default:
			td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("%d served model(s) in %s on %s removed by cluster-manager with %s taken off (the teardown took the llm-d controller away from under them, and nothing clears the finalizer once it is gone; their workloads go with them): %s", len(models), servingNamespace, t.Cluster, detect.LLMISVCFinalizer, names))
		}
	}
	if len(configs) > 0 {
		names := strings.Join(detect.Names(configs), ", ")
		switch {
		case td.cut:
			td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("%d LLMInferenceServiceConfig(s) in %s on %s are pending (%s): their release's uninstall and any delete are denied by the llmisvc webhook while the controller runs, and %s is cleared by nothing once it is gone — the re-run removes them with the finalizer taken off: %s", len(configs), t.Namespace, t.Cluster, reason, detect.LLMISVCConfigFinalizer, names))
		case td.dryRun:
			td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("%d LLMInferenceServiceConfig(s) in %s on %s would be removed by cluster-manager with %s taken off once the llmisvc controller is gone (its webhook denies every delete while it runs; nothing clears the finalizer once it is gone): %s", len(configs), t.Namespace, t.Cluster, detect.LLMISVCConfigFinalizer, names))
		default:
			td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("%d LLMInferenceServiceConfig(s) in %s on %s removed by cluster-manager with %s taken off (the llmisvc controller's webhook denies every delete while it runs, and nothing clears the finalizer once it is gone): %s", len(configs), t.Namespace, t.Cluster, detect.LLMISVCConfigFinalizer, names))
		}
	}
	return nil
}

// purgeServing removes the served models, then the configs they compose
// from, each set concurrently (purgeAll). The first error is returned.
func purgeServing(ctx context.Context, reader dynamic.Interface, models, configs []unstructured.Unstructured) error {
	if err := purgeAll(ctx, reader, detect.LLMISVCResource, detect.LLMISVCFinalizer, models); err != nil {
		return err
	}
	return purgeConfigs(ctx, reader, configs)
}

// transientAdmission reports an error the going llmisvc webhook explains: a
// delete it denied, or one that failed while it could not be called.
func transientAdmission(err error) bool {
	return apierrors.IsForbidden(err) || apierrors.IsInternalError(err) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err)
}

// sleepWithin sleeps d or until ctx is done.
func sleepWithin(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// healStrandedConfigs is the counterpart before a slice is composed: the
// LLMInferenceServiceConfigs terminating in the release namespace with no
// llm-d controller to clear them are removed, finalizer and all, so the
// slice's release creates them afresh instead of adopting and losing them;
// with a controller running they are its (models still reference them) and
// are named as a warning only. Nothing happens on a target that cannot be
// read: the slice's composition has refused that case before.
func healStrandedConfigs(ctx context.Context, t target, dryRun bool, out *WriteResult) error {
	if t.Reader == nil {
		return nil
	}
	configs, err := detect.Configs(ctx, t.Reader, t.Namespace)
	if err != nil {
		return err
	}
	stranded := detect.Terminating(configs)
	if len(stranded) == 0 {
		return nil
	}
	if detect.LLMISVCControllerRuns(ctx, t.Reader) {
		out.Warnings = append(out.Warnings, fmt.Sprintf("%d LLMInferenceServiceConfig(s) in %s on %s are terminating while an llmisvc controller runs (%s): models still referencing them hold them — unload those models, or wait for the controller to clear them; a slice release installed over a terminating config adopts and loses it", len(stranded), t.Namespace, t.Cluster, strings.Join(detect.Names(stranded), ", ")))
		return nil
	}
	if !dryRun {
		if err := purgeConfigs(ctx, t.Reader, stranded); err != nil {
			return err
		}
	}
	verb := "removed"
	if dryRun {
		verb = "would be removed"
	}
	for i := range stranded {
		act := ObjectAction{APIVersion: stranded[i].GetAPIVersion(), Kind: stranded[i].GetKind(), Name: stranded[i].GetName(), Namespace: stranded[i].GetNamespace(), Action: actionDelete}
		if dryRun {
			act.Action = "would-delete"
		}
		out.Objects = append(out.Objects, act)
	}
	out.Warnings = append(out.Warnings, fmt.Sprintf("%d LLMInferenceServiceConfig(s) in %s on %s %s by cluster-manager with %s taken off (left terminating by a serving layer that went, no llmisvc controller to clear the finalizer; removed so the slice's release creates them afresh): %s", len(stranded), t.Namespace, t.Cluster, verb, detect.LLMISVCConfigFinalizer, strings.Join(detect.Names(stranded), ", ")))
	return nil
}

// deleteChildRelease deletes one child HelmRelease of a slice release — an
// object Helm rendered, owned by the release through Flux's labels, never by
// cluster-manager's own —; not found, or someone else's, is left alone. A
// child deleted already whose uninstall failed is asked to retry now: with
// the teardown's earlier steps done, its objects are gone.
func (td *teardown) deleteChildRelease(ns, release, child string) error {
	res := td.dyn.Resource(HelmReleaseGVR).Namespace(ns)
	hr, err := res.Get(td.ctx, child, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get HelmRelease %s/%s: %w", ns, child, err)
	}
	labels := hr.GetLabels()
	if labels[detect.LabelFluxReleaseName] != release || labels[detect.LabelFluxReleaseNamespace] != ns {
		return nil
	}
	if hr.GetDeletionTimestamp() != nil {
		return td.retryUninstall(res, hr)
	}
	return td.delete(res, ObjectAction{APIVersion: hr.GetAPIVersion(), Kind: hr.GetKind(), Name: child, Namespace: ns})
}

// retryUninstall asks Flux to reconcile a deleted child release now when its
// last uninstall failed — helm-controller retries with a growing backoff,
// minutes apart — and names it; a child Flux is still uninstalling is its.
func (td *teardown) retryUninstall(res dynamic.ResourceInterface, hr *unstructured.Unstructured) error {
	ready, found := detect.ReadyCondition(hr)
	if !found || ready.Status == "True" || !strings.HasSuffix(ready.Reason, "Failed") {
		return nil
	}
	name := hr.GetNamespace() + "/" + hr.GetName()
	if td.dryRun {
		td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("HelmRelease %s has been deleted since %s but its uninstall failed (%s: %s): Flux would be asked to retry it once its objects are gone", name, hr.GetDeletionTimestamp().UTC().Format(time.RFC3339), ready.Reason, strings.Join(strings.Fields(ready.Message), " ")))
		return nil
	}
	if !td.fits() {
		return nil
	}
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, requestedAtAnnotation, time.Now().UTC().Format(time.RFC3339Nano))
	if _, err := res.Patch(td.ctx, hr.GetName(), types.MergePatchType, []byte(patch), metav1.PatchOptions{FieldManager: compose.ManagedBy}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("ask Flux to reconcile HelmRelease %s: %w", name, err)
	}
	td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("HelmRelease %s has been deleted since %s but its uninstall failed (%s: %s): its objects are gone now and Flux was asked to retry the uninstall at once (%s)", name, hr.GetDeletionTimestamp().UTC().Format(time.RFC3339), ready.Reason, strings.Join(strings.Fields(ready.Message), " "), requestedAtAnnotation))
	return nil
}

// pollInterval is a twentieth of the wait, between 10ms and 2s.
func pollInterval(timeout time.Duration) time.Duration {
	return max(10*time.Millisecond, min(2*time.Second, timeout/20))
}

// purgeConfigs removes the configs from the target: deleted, then their
// llmisvc finalizer taken off (purgeAll).
func purgeConfigs(ctx context.Context, reader dynamic.Interface, configs []unstructured.Unstructured) error {
	return purgeAll(ctx, reader, detect.LLMISVCConfigResource, detect.LLMISVCConfigFinalizer, configs)
}

// purgeAll removes gr's objects from the target, concurrently: deleted, then
// finalizer taken off — a terminating object goes the moment its last
// finalizer does, and a controller cannot put one back on an object that is
// gone. Every request goes through the version the object was listed in —
// the CRD's storage version (detect.ConfigsGVR, detect.ServedGVR), which
// needs no conversion: the CRD's conversion webhook is the llmisvc
// controller's, gone with its release a step before
// (giantswarm/cluster-manager#39). The first error is returned.
func purgeAll(ctx context.Context, reader dynamic.Interface, gr schema.GroupResource, finalizer string, objs []unstructured.Unstructured) error {
	g, gctx := errgroup.WithContext(ctx)
	for i := range objs {
		obj := &objs[i]
		g.Go(func() error { return purgeOne(gctx, reader, gr, finalizer, obj) })
	}
	return g.Wait()
}

// purgeOne deletes one object and takes finalizer off it, retrying the
// update on a conflict with the controller; gone already is fine.
func purgeOne(ctx context.Context, reader dynamic.Interface, gr schema.GroupResource, finalizer string, obj *unstructured.Unstructured) error {
	ns, name, kind := obj.GetNamespace(), obj.GetName(), obj.GetKind()
	res := reader.Resource(gr.WithVersion(obj.GroupVersionKind().Version)).Namespace(ns)
	if err := res.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %s %s/%s: %w", kind, ns, name, err)
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		c, err := res.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		finalizers := withoutFinalizer(c.GetFinalizers(), finalizer)
		if len(finalizers) == len(c.GetFinalizers()) {
			return nil
		}
		c.SetFinalizers(finalizers)
		_, err = res.Update(ctx, c, metav1.UpdateOptions{FieldManager: compose.ManagedBy})
		return err
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("clear %s of %s %s/%s: %w", finalizer, kind, ns, name, err)
	}
	return nil
}

// withoutFinalizer is finalizers without name.
func withoutFinalizer(finalizers []string, name string) []string {
	out := make([]string, 0, len(finalizers))
	for _, f := range finalizers {
		if f != name {
			out = append(out, f)
		}
	}
	return out
}
