package tools

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/cluster-manager/internal/compose"
)

// Resources the write tools read besides the read tools'.
var (
	ConfigMapGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	AppGVR       = schema.GroupVersionResource{Group: "application.giantswarm.io", Version: "v1alpha1", Resource: "apps"}
)

// Write modes. Only apply exists in this stage; commit (a pull request as
// the person) arrives with the epic's next proof (bumblebee-plans#46 D11).
const (
	ModeApply  = "apply"
	ModeCommit = "commit"
)

// Release CR components the pool's pins come from, and the machine image
// they form: `flatcar-<channel>-<flatcar>-kube-<k8s>-tooling-<tooling>-gs`,
// the name cluster-aws builds for its own workers (the controller looks the
// AMI up by name, per region).
const (
	releaseComponentFlatcar   = "flatcar"
	releaseComponentOSTooling = "os-tooling"
	osReleaseChannel          = "stable"

	// labelKustomizeName marks an object Flux's kustomize-controller owns.
	labelKustomizeName = "kustomize.toolkit.fluxcd.io/name"
	// teleportJoinTokenSuffix names the Secret teleport-operator writes.
	teleportJoinTokenSuffix = "-teleport-join-token"
	// flatcarChannel is fixed by the cluster chart for every provider.
)

// ErrRefused is a refusal with the fix in the message: a mode not offered, a
// version skew, a GitOps-owned object, nodes still running.
type ErrRefused struct{ Reason string }

func (e *ErrRefused) Error() string { return e.Reason }

// CreateNodePoolInput is create_node_pool's input.
type CreateNodePoolInput struct {
	Cluster   string
	Namespace string
	Pool      compose.PoolSpec
	// Teleport overrides the default (on when the cluster has its join-token
	// Secret); nil keeps the default.
	Teleport *bool
	Mode     string
	DryRun   bool
}

// DeleteNodePoolInput is delete_node_pool's input.
type DeleteNodePoolInput struct {
	Cluster   string
	Namespace string
	Name      string
	Force     bool
	Mode      string
	DryRun    bool
}

// WriteResult is the answer of a write: what was (or, dry-run, would be)
// done per object, and the rendered manifests.
type WriteResult struct {
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	Pool      string `json:"pool"`
	Mode      string `json:"mode"`
	DryRun    bool   `json:"dryRun"`
	// Pins of a created pool; empty on delete.
	ChartVersion        string         `json:"chartVersion,omitempty"`
	KubernetesVersion   string         `json:"kubernetesVersion,omitempty"`
	ControlPlaneVersion string         `json:"controlPlaneVersion,omitempty"`
	MachineImage        string         `json:"machineImage,omitempty"`
	Objects             []ObjectAction `json:"objects"`
	// Manifests are the rendered objects (create) — the dry-run's answer.
	Manifests []map[string]any `json:"manifests,omitempty"`
}

// ObjectAction is what happened to one object.
type ObjectAction struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	// Action is create, update, unchanged or delete (would-… when dry-run).
	Action string `json:"action"`
	// Changes are the spec paths an update changes — the dry-run's drift
	// check on a re-run.
	Changes []string `json:"changes,omitempty"`
}

// CreateNodePool composes the pool release for the cluster's current Release
// CR and values and lands it as the caller (apply mode). A second call on the
// same name is the update; its dry-run shows the difference.
func (s *Service) CreateNodePool(ctx context.Context, in CreateNodePoolInput) (*WriteResult, error) {
	if err := checkMode(in.Mode); err != nil {
		return nil, err
	}
	dyn := s.clients(ctx)
	c, err := s.getCluster(ctx, dyn, in.Cluster, in.Namespace)
	if err != nil {
		return nil, err
	}
	facts, err := s.clusterFacts(ctx, dyn, c)
	if err != nil {
		return nil, err
	}
	if in.Teleport != nil {
		facts.Teleport = *in.Teleport
	}
	cpVersion := controlPlaneVersion(ctx, dyn, c)
	if newer, err := versionNewer(facts.KubernetesVersion, strings.TrimPrefix(cpVersion, "v")); err == nil && newer {
		return nil, &ErrRefused{Reason: fmt.Sprintf("the cluster's release pins Kubernetes %s but its control plane runs %s: a pool is never newer than the control plane — finish the cluster's upgrade first, then re-run", facts.KubernetesVersion, cpVersion)}
	}
	objs, err := compose.Pool(facts, in.Pool)
	if err != nil {
		return nil, err
	}
	out := &WriteResult{
		Cluster: c.GetName(), Namespace: c.GetNamespace(), Pool: in.Pool.Name, Mode: in.Mode, DryRun: in.DryRun,
		ChartVersion: nestedString(objs[0], "spec", "ref", "tag"), KubernetesVersion: facts.KubernetesVersion,
		ControlPlaneVersion: cpVersion, MachineImage: facts.MachineImage, Objects: []ObjectAction{},
	}
	// A pool that used to carry credentials and no longer does: the stale
	// Secret goes.
	if len(facts.RegistryCredentials) == 0 {
		if act, err := deleteIfOwned(ctx, dyn, compose.SecretGVR, c.GetNamespace(), compose.ValuesSecretName(c.GetName(), in.Pool.Name), in.DryRun); err != nil {
			return nil, err
		} else if act != nil {
			out.Objects = append(out.Objects, *act)
		}
	}
	for _, obj := range objs {
		act, err := apply(ctx, dyn, obj, in.DryRun)
		if err != nil {
			return nil, err
		}
		out.Objects = append(out.Objects, act)
		out.Manifests = append(out.Manifests, redacted(obj))
	}
	return out, nil
}

// DeleteNodePool removes what create_node_pool created. It refuses while the
// pool's MachinePool has replicas unless forced — the refusal names the nodes.
func (s *Service) DeleteNodePool(ctx context.Context, in DeleteNodePoolInput) (*WriteResult, error) {
	if err := checkMode(in.Mode); err != nil {
		return nil, err
	}
	dyn := s.clients(ctx)
	c, err := s.getCluster(ctx, dyn, in.Cluster, in.Namespace)
	if err != nil {
		return nil, err
	}
	ns, release := c.GetNamespace(), compose.ReleaseName(c.GetName(), in.Name)
	hr, err := dyn.Resource(HelmReleaseGVR).Namespace(ns).Get(ctx, release, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, &ErrNotFound{What: fmt.Sprintf("node pool %s of cluster %s (HelmRelease %s/%s)", in.Name, c.GetName(), ns, release)}
	}
	if err != nil {
		return nil, fmt.Errorf("get HelmRelease %s/%s: %w", ns, release, err)
	}
	if !compose.OwnedBy(hr) {
		return nil, &ErrRefused{Reason: fmt.Sprintf("HelmRelease %s/%s was not created by cluster-manager (%s): delete_node_pool removes only what create_node_pool created — %s", ns, release, ownerDescription(hr), removalHint(hr))}
	}
	if !in.Force {
		if err := s.replicasGuard(ctx, dyn, ns, release); err != nil {
			return nil, err
		}
	}
	out := &WriteResult{Cluster: c.GetName(), Namespace: ns, Pool: in.Name, Mode: in.Mode, DryRun: in.DryRun, Objects: []ObjectAction{}}
	for _, target := range []struct {
		gvr  schema.GroupVersionResource
		name string
	}{
		{HelmReleaseGVR, release},
		{compose.OCIRepositoryGVR, release},
		{compose.SecretGVR, compose.ValuesSecretName(c.GetName(), in.Name)},
	} {
		act, err := deleteIfOwned(ctx, dyn, target.gvr, ns, target.name, in.DryRun)
		if err != nil {
			return nil, err
		}
		if act != nil {
			out.Objects = append(out.Objects, *act)
		}
	}
	return out, nil
}

func checkMode(mode string) error {
	switch mode {
	case ModeApply:
		return nil
	case ModeCommit:
		return &ErrRefused{Reason: "mode commit (a pull request opened as you) is not available yet in this server version; use mode apply — the objects land on the installation as you and are removed with the cluster"}
	default:
		return &ErrRefused{Reason: fmt.Sprintf("mode %q: apply is the only mode this server version offers", mode)}
	}
}

// replicasGuard refuses while the pool's MachinePool has replicas: on a
// Karpenter pool nodes exist exactly while something is scheduled on them.
func (s *Service) replicasGuard(ctx context.Context, dyn dynamic.Interface, ns, pool string) error {
	mp, err := dyn.Resource(MachinePoolGVR).Namespace(ns).Get(ctx, pool, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get MachinePool %s/%s: %w", ns, pool, err)
	}
	replicas := nestedInt(mp, "spec", "replicas")
	if replicas == 0 {
		return nil
	}
	nodes := "unknown"
	if ref := nestedRef(mp, "spec", "template", "spec", "infrastructureRef"); ref != nil {
		if infra, err := getRef(ctx, dyn, ref, ns); err == nil {
			ids, _, _ := unstructured.NestedStringSlice(infra.Object, "spec", "providerIDList")
			if len(ids) == 0 {
				ids, _, _ = unstructured.NestedStringSlice(infra.Object, "status", "providerIDList")
			}
			if len(ids) > 0 {
				nodes = strings.Join(ids, ", ")
			}
		}
	}
	return &ErrRefused{Reason: fmt.Sprintf("node pool %s still runs %d node(s) (%s): something is scheduled on them — check the cluster's Serving group, scale the workloads away and re-run once the pool is empty, or pass force to delete the pool with its nodes", pool, replicas, nodes)}
}

// clusterFacts reads what the pool release needs: the pins from the
// cluster's Release CR, the credential-free snapshot from the cluster's
// values, teleport from the join-token Secret's presence.
func (s *Service) clusterFacts(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured) (compose.Cluster, error) {
	facts := compose.Cluster{Name: c.GetName(), Namespace: c.GetNamespace(), Organization: organization(c), UID: string(c.GetUID())}
	if err := s.releasePins(ctx, dyn, c, &facts); err != nil {
		return facts, err
	}
	vals, err := clusterValues(ctx, dyn, c)
	if err != nil {
		return facts, err
	}
	facts.BaseDomain, _, _ = unstructured.NestedString(vals, "global", "connectivity", "baseDomain")
	if facts.BaseDomain == "" {
		return facts, fmt.Errorf("values of cluster %s carry no global.connectivity.baseDomain: the pool's bootstrap needs the installation's base domain", c.GetName())
	}
	facts.ManagementCluster, _, _ = unstructured.NestedString(vals, "global", "managementCluster")
	if facts.ManagementCluster == "" {
		facts.ManagementCluster = s.cfg.Installation
	}
	facts.CiliumIPAMMode, _, _ = unstructured.NestedString(vals, "global", "connectivity", "cilium", "ipamMode")
	facts.RegistryMirrors, facts.RegistryCredentials = registries(vals)
	facts.Proxy = proxy(vals)
	facts.Teleport = exists(ctx, dyn, compose.SecretGVR, c.GetNamespace(), c.GetName()+teleportJoinTokenSuffix)
	return facts, nil
}

// releasePins sets the Kubernetes version and machine image from the
// cluster's Release CR.
func (s *Service) releasePins(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured, facts *compose.Cluster) error {
	version := c.GetLabels()[LabelReleaseVersion]
	if version == "" {
		return fmt.Errorf("cluster %s carries no %s label: the pool's pins come from the cluster's release", c.GetName(), LabelReleaseVersion)
	}
	release, err := findRelease(ctx, dyn, version)
	if err != nil {
		return err
	}
	components := releaseComponents(release)
	for _, name := range []string{releaseComponentKubernetes, releaseComponentFlatcar, releaseComponentOSTooling} {
		if components[name] == "" {
			return fmt.Errorf("release %s names no %s component: the pool's pins come from the cluster's release", release.GetName(), name)
		}
	}
	facts.KubernetesVersion = components[releaseComponentKubernetes]
	facts.MachineImage = fmt.Sprintf("flatcar-%s-%s-kube-%s-tooling-%s-gs", osReleaseChannel, components[releaseComponentFlatcar], components[releaseComponentKubernetes], components[releaseComponentOSTooling])
	return nil
}

// findRelease is the Release CR of a release version (`<provider>-<version>`).
func findRelease(ctx context.Context, dyn dynamic.Interface, version string) (*unstructured.Unstructured, error) {
	releases, err := dyn.Resource(ReleaseGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list releases: %w", err)
	}
	for i := range releases.Items {
		if strings.HasSuffix(releases.Items[i].GetName(), "-"+version) {
			return &releases.Items[i], nil
		}
	}
	return nil, &ErrNotFound{What: "release " + version + " (Release CR)"}
}

// releaseComponents is component name → version without the v prefix.
func releaseComponents(release *unstructured.Unstructured) map[string]string {
	out := map[string]string{}
	components, _, _ := unstructured.NestedSlice(release.Object, "spec", "components")
	for _, comp := range components {
		m, ok := comp.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		version, _ := m["version"].(string)
		out[name] = strings.TrimPrefix(version, "v")
	}
	return out
}

// clusterValues is the cluster's own values: the fleet's HelmRelease named
// like the cluster (spec.valuesFrom ConfigMaps under spec.values, Flux's
// precedence), else the App CR of that name (its user-values ConfigMap).
// Secrets referenced from either are not read: their content is credentials.
func clusterValues(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured) (map[string]any, error) {
	ns, name := c.GetNamespace(), c.GetName()
	if hr, err := dyn.Resource(HelmReleaseGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
		merged := map[string]any{}
		refs, _, _ := unstructured.NestedSlice(hr.Object, "spec", "valuesFrom")
		for _, r := range refs {
			ref, _ := r.(map[string]any)
			if ref["kind"] != "ConfigMap" {
				continue
			}
			key, _ := ref["valuesKey"].(string)
			if key == "" {
				key = "values.yaml"
			}
			cmName, _ := ref["name"].(string)
			vals, err := configMapValues(ctx, dyn, ns, cmName, key)
			if err != nil {
				return nil, err
			}
			merge(merged, vals)
		}
		inline, _, _ := unstructured.NestedMap(hr.Object, "spec", "values")
		merge(merged, inline)
		return merged, nil
	}
	if app, err := dyn.Resource(AppGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
		cmName, _, _ := unstructured.NestedString(app.Object, "spec", "userConfig", "configMap", "name")
		cmNS, _, _ := unstructured.NestedString(app.Object, "spec", "userConfig", "configMap", "namespace")
		if cmNS == "" {
			cmNS = ns
		}
		if cmName == "" {
			return nil, fmt.Errorf("the App %s/%s names no user-values ConfigMap: the pool's snapshot comes from the cluster's values", ns, name)
		}
		return configMapValues(ctx, dyn, cmNS, cmName, "values")
	}
	return nil, &ErrNotFound{What: fmt.Sprintf("values of cluster %s (neither a HelmRelease nor an App named %s in %s)", name, name, ns)}
}

func configMapValues(ctx context.Context, dyn dynamic.Interface, ns, name, key string) (map[string]any, error) {
	cm, err := dyn.Resource(ConfigMapGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get values ConfigMap %s/%s: %w", ns, name, err)
	}
	raw, _, _ := unstructured.NestedString(cm.Object, "data", key)
	vals := map[string]any{}
	if err := yaml.Unmarshal([]byte(raw), &vals); err != nil {
		return nil, fmt.Errorf("values ConfigMap %s/%s key %s: %w", ns, name, key, err)
	}
	return vals, nil
}

// merge overlays src onto dst, maps recursively.
func merge(dst, src map[string]any) {
	for k, v := range src {
		if sv, ok := v.(map[string]any); ok {
			if dv, ok := dst[k].(map[string]any); ok {
				merge(dv, sv)
				continue
			}
		}
		dst[k] = v
	}
}

// registries splits global.components.containerd.containerRegistries into
// the credential-free mirrors (host → endpoints) and the credentials
// (endpoint → credential).
func registries(vals map[string]any) (map[string][]string, map[string]compose.RegistryCredential) {
	mirrors := map[string][]string{}
	creds := map[string]compose.RegistryCredential{}
	regs, _, _ := unstructured.NestedMap(vals, "global", "components", "containerd", "containerRegistries")
	for host, v := range regs {
		entries, _ := v.([]any)
		for _, e := range entries {
			entry, _ := e.(map[string]any)
			endpoint, _ := entry["endpoint"].(string)
			if endpoint == "" {
				continue
			}
			mirrors[host] = append(mirrors[host], endpoint)
			if cred, ok := entry["credentials"].(map[string]any); ok && len(cred) > 0 {
				user, _ := cred["username"].(string)
				pass, _ := cred["password"].(string)
				auth, _ := cred["auth"].(string)
				creds[endpoint] = compose.RegistryCredential{Username: user, Password: pass, Auth: auth}
			}
		}
	}
	return mirrors, creds
}

// proxy is global.connectivity.proxy; noProxy is taken as the values state
// it (a string, or a list joined by commas).
func proxy(vals map[string]any) compose.Proxy {
	p, _, _ := unstructured.NestedMap(vals, "global", "connectivity", "proxy")
	enabled, _ := p["enabled"].(bool)
	if !enabled {
		return compose.Proxy{}
	}
	out := compose.Proxy{Enabled: true}
	out.HTTPProxy, _ = p["httpProxy"].(string)
	out.HTTPSProxy, _ = p["httpsProxy"].(string)
	switch np := p["noProxy"].(type) {
	case string:
		out.NoProxy = np
	case []any:
		parts := make([]string, 0, len(np))
		for _, x := range np {
			parts = append(parts, fmt.Sprint(x))
		}
		out.NoProxy = strings.Join(parts, ",")
	case map[string]any:
		if addrs, ok := np["addresses"].([]any); ok {
			parts := make([]string, 0, len(addrs))
			for _, x := range addrs {
				parts = append(parts, fmt.Sprint(x))
			}
			out.NoProxy = strings.Join(parts, ",")
		}
	}
	return out
}

func exists(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns, name string) bool {
	_, err := dyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	return err == nil
}

// apply lands one composed object as the caller: created when absent,
// updated when cluster-manager created it, refused when someone else owns
// it — apply mode lands new objects only, a GitOps-owned object is never
// patched. Dry-run touches nothing and reports what would happen.
func apply(ctx context.Context, dyn dynamic.Interface, obj *unstructured.Unstructured, dryRun bool) (ObjectAction, error) {
	gvr, err := refGVR(obj.GetAPIVersion(), obj.GetKind())
	if err != nil {
		return ObjectAction{}, err
	}
	act := ObjectAction{APIVersion: obj.GetAPIVersion(), Kind: obj.GetKind(), Name: obj.GetName(), Namespace: obj.GetNamespace()}
	res := dyn.Resource(gvr).Namespace(obj.GetNamespace())
	existing, err := res.Get(ctx, obj.GetName(), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		act.Action = "create"
		if !dryRun {
			if _, err := res.Create(ctx, obj, metav1.CreateOptions{FieldManager: compose.ManagedBy}); err != nil {
				return act, fmt.Errorf("create %s %s/%s: %w", obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
			}
		}
	case err != nil:
		return act, fmt.Errorf("get %s %s/%s: %w", obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
	case !compose.OwnedBy(existing):
		return act, &ErrRefused{Reason: fmt.Sprintf("%s %s/%s exists and %s: apply mode lands new objects only and never patches an object someone else owns — %s", obj.GetKind(), obj.GetNamespace(), obj.GetName(), ownerDescription(existing), removalHint(existing))}
	default:
		act.Changes = changedPaths(existing, obj)
		if len(act.Changes) == 0 {
			act.Action = "unchanged"
			return act, nil
		}
		act.Action = "update"
		if !dryRun {
			obj.SetResourceVersion(existing.GetResourceVersion())
			if _, err := res.Update(ctx, obj, metav1.UpdateOptions{FieldManager: compose.ManagedBy}); err != nil {
				return act, fmt.Errorf("update %s %s/%s: %w", obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
			}
		}
	}
	if dryRun {
		act.Action = "would-" + act.Action
	}
	return act, nil
}

// deleteIfOwned deletes one of cluster-manager's objects; nil when it does
// not exist. An object of that name someone else owns is left alone.
func deleteIfOwned(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns, name string, dryRun bool) (*ObjectAction, error) {
	res := dyn.Resource(gvr).Namespace(ns)
	obj, err := res.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get %s %s/%s: %w", gvr.Resource, ns, name, err)
	}
	if !compose.OwnedBy(obj) {
		return nil, &ErrRefused{Reason: fmt.Sprintf("%s %s/%s %s: cluster-manager removes only what it created — %s", obj.GetKind(), ns, name, ownerDescription(obj), removalHint(obj))}
	}
	act := &ObjectAction{APIVersion: obj.GetAPIVersion(), Kind: obj.GetKind(), Name: name, Namespace: ns, Action: "delete"}
	if dryRun {
		act.Action = "would-delete"
		return act, nil
	}
	if err := res.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("delete %s %s/%s: %w", obj.GetKind(), ns, name, err)
	}
	return act, nil
}

// ownerDescription says who owns an object that is not cluster-manager's.
func ownerDescription(obj *unstructured.Unstructured) string {
	labels := obj.GetLabels()
	if k := labels[labelKustomizeName]; k != "" {
		return "is owned by GitOps (Flux Kustomization " + k + ")"
	}
	if m := labels[compose.LabelManagedBy]; m != "" {
		return "is managed by " + m
	}
	return "was not created by cluster-manager"
}

// removalHint points at the way to change an object cluster-manager does not
// own: the git repository for a GitOps-owned one, else its owner.
func removalHint(obj *unstructured.Unstructured) string {
	if obj.GetLabels()[labelKustomizeName] != "" {
		return "change it in the git repository that owns the cluster (mode commit, once available), or pick another pool name"
	}
	return "remove it by the means that created it, or pick another pool name"
}

// changedPaths lists the spec, label and ownerReference paths of want that
// differ from have — the drift a re-run would correct.
func changedPaths(have, want *unstructured.Unstructured) []string {
	var paths []string
	diff(have.Object["spec"], want.Object["spec"], "spec", &paths)
	diff(have.Object["stringData"], want.Object["stringData"], "stringData", &paths)
	diff(map[string]any{"labels": have.GetLabels()}, map[string]any{"labels": want.GetLabels()}, "metadata", &paths)
	if !reflect.DeepEqual(have.GetOwnerReferences(), want.GetOwnerReferences()) {
		paths = append(paths, "metadata.ownerReferences")
	}
	sort.Strings(paths)
	return paths
}

// diff walks maps side by side and records every leaf path whose values
// differ (numbers compared by value, whatever their Go type); a subtree one
// side lacks is walked as empty, so the paths name the leaves.
func diff(have, want any, path string, out *[]string) {
	hm, hok := have.(map[string]any)
	wm, wok := want.(map[string]any)
	if hok && want == nil {
		wm, wok = map[string]any{}, true
	}
	if wok && have == nil {
		hm, hok = map[string]any{}, true
	}
	if hok && wok {
		keys := map[string]bool{}
		for k := range hm {
			keys[k] = true
		}
		for k := range wm {
			keys[k] = true
		}
		for k := range keys {
			diff(hm[k], wm[k], path+"."+k, out)
		}
		return
	}
	if !equalLeaf(have, want) {
		*out = append(*out, path)
	}
}

func equalLeaf(a, b any) bool {
	if reflect.DeepEqual(a, b) {
		return true
	}
	na, aok := number(a)
	nb, bok := number(b)
	return aok && bok && na == nb
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

// versionNewer reports whether a (`1.31.4`) is newer than b; false with an
// error when either does not parse (an unknown control plane version does
// not block — the controller's own skew check does).
func versionNewer(a, b string) (bool, error) {
	pa, err := parseVersion(a)
	if err != nil {
		return false, err
	}
	pb, err := parseVersion(b)
	if err != nil {
		return false, err
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i], nil
		}
	}
	return false, nil
}

func parseVersion(v string) ([3]int, error) {
	var out [3]int
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	if len(parts) != 3 {
		return out, errors.New("not a three-part version: " + v)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(strings.SplitN(p, "-", 2)[0])
		if err != nil {
			return out, fmt.Errorf("version %s: %w", v, err)
		}
		out[i] = n
	}
	return out, nil
}

// redacted is the manifest as the tool returns it: a Secret's data replaced
// by the keys it carries.
func redacted(obj *unstructured.Unstructured) map[string]any {
	if obj.GetKind() != "Secret" {
		return obj.Object
	}
	cp := obj.DeepCopy()
	data, _, _ := unstructured.NestedMap(cp.Object, "stringData")
	keys := map[string]any{}
	for k := range data {
		keys[k] = "<redacted>"
	}
	cp.Object["stringData"] = keys
	return cp.Object
}
