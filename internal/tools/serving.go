package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/detect"
)

// platformReleaseName is the name the fleet gives the platform's own release
// of the agent-platform chart; another name is accepted when it is the
// installation's only release of the chart that is not cluster-manager's.
const platformReleaseName = compose.SliceChart

// ModelServingInput is enable_model_serving's and disable_model_serving's
// input.
type ModelServingInput struct {
	Cluster   string
	Namespace string
	Mode      string
	DryRun    bool
	// Force (disable) removes the slice while models are still served.
	Force bool
}

// SliceRelease is the `<cluster>-agent-platform` release as a write reports
// it.
type SliceRelease struct {
	Name         string `json:"name"`
	Namespace    string `json:"namespace"`
	ChartVersion string `json:"chartVersion"`
	// Domain is the slice's global.domain; ModelsHost where a served model
	// answers.
	Domain     string `json:"domain"`
	ModelsHost string `json:"modelsHost"`
	// GPUPool is the pool the predictors are placed on, empty for none.
	GPUPool string `json:"gpuPool,omitempty"`
}

// EnableModelServing creates the cluster's slice release with the serving
// slice on — or updates the one that exists, never a second release of the
// chart on one cluster — and registers the cluster's kserve backend with
// model-manager. A pool is not required. Refused where the platform's own
// release or a human provides serving already.
func (s *Service) EnableModelServing(ctx context.Context, in ModelServingInput) (*WriteResult, error) {
	if err := checkMode(in.Mode); err != nil {
		return nil, err
	}
	k := s.clients(ctx)
	c, err := s.getCluster(ctx, k, in.Cluster, in.Namespace)
	if err != nil {
		return nil, err
	}
	dyn := k.Dynamic
	facts, err := s.sliceCluster(ctx, dyn, c)
	if err != nil {
		return nil, err
	}
	target := s.target(ctx, dyn, c)
	pool, err := onlyPool(ctx, dyn, c.GetNamespace(), c.GetName(), "")
	if err != nil {
		return nil, err
	}
	serving, slice, objs, err := s.sliceRelease(ctx, dyn, target, facts, pool)
	if err != nil {
		return nil, err
	}
	if slice == nil {
		return nil, &ErrRefused{Reason: fmt.Sprintf("serving is already present on %s, provided by %s (%s): the slice release is composed only where nothing provides serving — nothing to do", c.GetName(), providerDescription(serving.Provider), strings.Join(serving.Evidence, "; "))}
	}
	backend, err := s.backendDocument(ctx, dyn, target)
	if err != nil {
		return nil, err
	}
	out := &WriteResult{
		Cluster: c.GetName(), Namespace: c.GetNamespace(), Mode: in.Mode, DryRun: in.DryRun, Objects: []ObjectAction{},
		Serving: serving, Slice: slice,
		Backend: &BackendRegistration{Kind: compose.BackendKindKServe, Namespace: backend.GetNamespace(), Name: backend.GetName(), Target: backendTargetName(target.backend)},
	}
	for _, obj := range append(objs, backend) {
		act, err := apply(ctx, dyn, obj, in.DryRun)
		if err != nil {
			return nil, err
		}
		out.Objects = append(out.Objects, act)
		out.Manifests = append(out.Manifests, redacted(obj))
	}
	return out, nil
}

// DisableModelServing removes the slice release cluster-manager created and
// the backend it registered. Unless forced it refuses while models are
// served on the cluster, naming them — or plainly when the cluster cannot
// be read as the caller.
func (s *Service) DisableModelServing(ctx context.Context, in ModelServingInput) (*WriteResult, error) {
	if err := checkMode(in.Mode); err != nil {
		return nil, err
	}
	k := s.clients(ctx)
	c, err := s.getCluster(ctx, k, in.Cluster, in.Namespace)
	if err != nil {
		return nil, err
	}
	dyn := k.Dynamic
	ns, name := c.GetNamespace(), compose.SliceReleaseName(c.GetName())
	hr, err := dyn.Resource(HelmReleaseGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, &ErrNotFound{What: fmt.Sprintf("model serving of cluster %s (HelmRelease %s/%s)", c.GetName(), ns, name)}
	}
	if err != nil {
		return nil, fmt.Errorf("get HelmRelease %s/%s: %w", ns, name, err)
	}
	if !compose.OwnedBy(hr) {
		return nil, &ErrRefused{Reason: fmt.Sprintf("HelmRelease %s/%s %s: disable_model_serving removes only the slice release cluster-manager created — switch the serving slice off by the means that created the release", ns, name, ownerDescription(hr))}
	}
	if !in.Force {
		if err := servedModelsGuard(ctx, s.target(ctx, dyn, c)); err != nil {
			return nil, err
		}
	}
	out := &WriteResult{Cluster: c.GetName(), Namespace: ns, Mode: in.Mode, DryRun: in.DryRun, Objects: []ObjectAction{}}
	targets, err := s.sliceRemovals(ctx, dyn, ns, c.GetName())
	if err != nil {
		return nil, err
	}
	return out, deleteAll(ctx, dyn, targets, in.DryRun, out)
}

// sliceRelease decides the slice's part of a write: nothing when serving
// runs on the target and someone else provides it (the platform's chart, a
// human), the `<cluster>-agent-platform` release filled from the platform's
// inputs when none does — or when the one running is cluster-manager's own,
// so the re-run is its update —, a refusal when the cluster cannot be read
// or already has a release of the chart under another name.
func (s *Service) sliceRelease(ctx context.Context, dyn dynamic.Interface, t target, facts compose.Cluster, pool string) (detect.Component, *SliceRelease, []*unstructured.Unstructured, error) {
	serving := detect.Serving(ctx, t.Target)
	switch {
	case serving.Status == detect.StatusUnknown:
		return serving, nil, nil, &ErrRefused{Reason: fmt.Sprintf("cannot tell whether serving runs on %s (%s): the slice release is composed only when nothing provides serving — make the cluster readable as you (its apiserver must trust the installation's identity provider) and re-run", t.Cluster, serving.Reason)}
	case serving.Present() && serving.Provider != detect.ProviderClusterManager:
		return serving, nil, nil, nil
	}
	if err := s.refuseSecondRelease(ctx, dyn, t.Namespace, t.Cluster); err != nil {
		return serving, nil, nil, err
	}
	platform, err := s.platformInputs(ctx, dyn)
	if err != nil {
		return serving, nil, nil, err
	}
	spec := compose.SliceSpec{ChartVersion: s.cfg.SliceChartVersion, OwnCluster: t.backend.OwnCluster, Platform: platform, Pool: pool, CertificateIssuer: s.cfg.CertificateIssuer}
	if spec.ChartVersion, err = compose.SliceChartVersion(spec); err != nil {
		return serving, nil, nil, &ErrRefused{Reason: err.Error()}
	}
	objs, err := compose.Slice(facts, spec)
	if err != nil {
		return serving, nil, nil, err
	}
	domain := compose.SliceDomain(facts, spec)
	slice := &SliceRelease{
		Name: objs[1].GetName(), Namespace: objs[1].GetNamespace(), ChartVersion: nestedString(objs[0], "spec", "ref", "tag"),
		Domain: domain, ModelsHost: compose.ModelsHost(domain), GPUPool: pool,
	}
	return serving, slice, objs, nil
}

// refuseSecondRelease refuses when the cluster already has a release of the
// agent-platform chart under another name than `<cluster>-agent-platform`:
// one release of the chart per cluster, its slices toggles in the values.
func (s *Service) refuseSecondRelease(ctx context.Context, dyn dynamic.Interface, ns, cluster string) error {
	hrs, err := dyn.Resource(HelmReleaseGVR).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: compose.LabelCluster + "=" + cluster})
	if err != nil {
		return fmt.Errorf("list releases of %s: %w", cluster, err)
	}
	for i := range hrs.Items {
		hr := &hrs.Items[i]
		if hr.GetName() != compose.SliceReleaseName(cluster) && chartOf(ctx, dyn, hr) == compose.SliceChart {
			return &ErrRefused{Reason: fmt.Sprintf("cluster %s already has a release of the %s chart under another name (HelmRelease %s/%s, %s): a cluster has one release of the chart, its slices toggles in the values — switch the serving slice on in that release, or remove it and re-run", cluster, compose.SliceChart, ns, hr.GetName(), ownerDescription(hr))}
		}
	}
	return nil
}

// platformInputs reads global.domain, global.identity, the wildcard
// certificate and the chart version it runs (status.history[0].chartVersion)
// from the installation's own release of the agent-platform chart: the
// HelmRelease named agent-platform, else the one release of the chart that
// is not cluster-manager's. Secrets it references are not read.
func (s *Service) platformInputs(ctx context.Context, dyn dynamic.Interface) (compose.PlatformInputs, error) {
	hrs, err := dyn.Resource(HelmReleaseGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return compose.PlatformInputs{}, fmt.Errorf("list HelmReleases: %w", err)
	}
	var candidates []*unstructured.Unstructured
	for i := range hrs.Items {
		hr := &hrs.Items[i]
		if compose.OwnedBy(hr) || chartOf(ctx, dyn, hr) != compose.SliceChart {
			continue
		}
		if hr.GetName() == platformReleaseName {
			candidates = []*unstructured.Unstructured{hr}
			break
		}
		candidates = append(candidates, hr)
	}
	switch len(candidates) {
	case 0:
		return compose.PlatformInputs{}, &ErrRefused{Reason: fmt.Sprintf("no release of the %s chart found on the installation: the slice's domain and identity are read from the platform's own release, never invented — install the platform first", compose.SliceChart)}
	case 1:
	default:
		names := make([]string, 0, len(candidates))
		for _, hr := range candidates {
			names = append(names, hr.GetNamespace()+"/"+hr.GetName())
		}
		sort.Strings(names)
		return compose.PlatformInputs{}, &ErrRefused{Reason: fmt.Sprintf("several releases of the %s chart on the installation and none named %s (%s): cannot tell which is the platform's own", compose.SliceChart, platformReleaseName, strings.Join(names, ", "))}
	}
	hr := candidates[0]
	vals, err := helmReleaseValues(ctx, dyn, hr)
	if err != nil {
		return compose.PlatformInputs{}, err
	}
	out := compose.PlatformInputs{Release: hr.GetNamespace() + "/" + hr.GetName(), ChartVersion: runningChartVersion(hr)}
	out.Domain, _, _ = unstructured.NestedString(vals, "global", "domain")
	if out.Domain == "" {
		return out, &ErrRefused{Reason: fmt.Sprintf("the platform's release (HelmRelease %s/%s) carries no global.domain in its values or valuesFrom ConfigMaps: the slice's domain derives from it", hr.GetNamespace(), hr.GetName())}
	}
	out.Identity, _, _ = unstructured.NestedMap(vals, "global", "identity")
	out.TLSSecretName, _, _ = unstructured.NestedString(vals, "gatewayApi", "gateway", "tls", "secretName")
	return out, nil
}

// runningChartVersion is the chart version a HelmRelease runs: the newest
// entry of its status.history, empty before the first deployment.
func runningChartVersion(hr *unstructured.Unstructured) string {
	history, _, _ := unstructured.NestedSlice(hr.Object, "status", "history")
	if len(history) == 0 {
		return ""
	}
	newest, _ := history[0].(map[string]any)
	v, _ := newest["chartVersion"].(string)
	return v
}

// chartOf names the chart a HelmRelease installs: its chart label, its
// chart spec, or the last path element of the OCIRepository it references.
func chartOf(ctx context.Context, dyn dynamic.Interface, hr *unstructured.Unstructured) string {
	if chart := hr.GetLabels()[compose.LabelChartName]; chart != "" {
		return chart
	}
	if chart := nestedString(hr, "spec", "chart", "spec", "chart"); chart != "" {
		return chart
	}
	if nestedString(hr, "spec", "chartRef", "kind") == "OCIRepository" {
		repo, err := dyn.Resource(compose.OCIRepositoryGVR).Namespace(hr.GetNamespace()).Get(ctx, nestedString(hr, "spec", "chartRef", "name"), metav1.GetOptions{})
		if err == nil {
			url := nestedString(repo, "spec", "url")
			return url[strings.LastIndex(url, "/")+1:]
		}
	}
	return ""
}

// sliceCluster reads what the slice release needs to know about the
// cluster: its identity and, on a workload cluster, the base domain from
// its values (the models host is models.<cluster>.<base domain>).
func (s *Service) sliceCluster(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured) (compose.Cluster, error) {
	facts := s.identity(c)
	if s.ownCluster(c) {
		return facts, nil
	}
	vals, err := clusterValues(ctx, dyn, c)
	if err != nil {
		return facts, err
	}
	facts.BaseDomain, _, _ = unstructured.NestedString(vals, "global", "connectivity", "baseDomain")
	if facts.BaseDomain == "" {
		return facts, fmt.Errorf("values of cluster %s carry no global.connectivity.baseDomain: the models host derives from it", c.GetName())
	}
	return facts, nil
}

// onlyPool is the GPU pool the slice's predictors are pinned to: the
// cluster's one pool release of cluster-manager's (adding counts as one),
// by pool name. Empty when the cluster has none or several — the predictors
// are then placed by their GPU request alone, on whichever pool offers it —
// so the rule is the same for every write and never flips between pools.
func onlyPool(ctx context.Context, dyn dynamic.Interface, ns, cluster, adding string) (string, error) {
	pools, err := dyn.Resource(HelmReleaseGVR).Namespace(ns).List(ctx, metav1.ListOptions{
		LabelSelector: compose.LabelChartName + "=" + compose.PoolChart + "," + compose.LabelCluster + "=" + cluster + "," + compose.LabelManagedBy + "=" + compose.ManagedBy,
	})
	if err != nil {
		return "", fmt.Errorf("list pool releases of %s: %w", cluster, err)
	}
	names := map[string]bool{}
	if adding != "" {
		names[adding] = true
	}
	for i := range pools.Items {
		names[pools.Items[i].GetLabels()[compose.LabelPool]] = true
	}
	if len(names) != 1 {
		return "", nil
	}
	for name := range names {
		return name, nil
	}
	return "", nil
}

// servedModelsGuard refuses while models are served on the target, naming
// them; plainly when the target cannot be read as the caller.
func servedModelsGuard(ctx context.Context, t target) error {
	if t.Reader == nil {
		return &ErrRefused{Reason: fmt.Sprintf("cannot tell whether models are served on %s (%s): make the cluster readable as you and re-run, or pass force to remove the serving slice regardless", t.Cluster, t.Reason)}
	}
	models, err := detect.ServedModels(ctx, t.Reader)
	if err != nil {
		return &ErrRefused{Reason: fmt.Sprintf("cannot tell whether models are served on %s (%v): re-run once you may list its inference services, or pass force to remove the serving slice regardless", t.Cluster, err)}
	}
	if len(models) > 0 {
		return &ErrRefused{Reason: fmt.Sprintf("%d model(s) are served on %s (%s): unload them first (model-manager's unload_model, or the cluster's Serving group), or pass force to remove the serving slice with them", len(models), t.Cluster, joinModels(models))}
	}
	return nil
}

// joinModels names served models for a message: the object and its model.
func joinModels(models []detect.ServedModel) string {
	names := make([]string, 0, len(models))
	for _, m := range models {
		names = append(names, m.String())
	}
	return strings.Join(names, ", ")
}

// sliceRemovals names the slice release's objects and, when registered for
// the cluster, the backend document.
func (s *Service) sliceRemovals(ctx context.Context, dyn dynamic.Interface, ns, cluster string) ([]objectRef, error) {
	name := compose.SliceReleaseName(cluster)
	targets := []objectRef{{HelmReleaseGVR, ns, name}, {compose.OCIRepositoryGVR, ns, name}}
	registered, err := backendRegisteredFor(ctx, dyn, s.cfg.ModelManagerNamespace, cluster)
	if err != nil {
		return nil, err
	}
	if registered {
		targets = append(targets, objectRef{compose.ConfigMapGVR, s.cfg.ModelManagerNamespace, compose.BackendConfigMapName})
	}
	return targets, nil
}

// deleteAll removes cluster-manager's objects among targets, recording each
// in out.
func deleteAll(ctx context.Context, dyn dynamic.Interface, targets []objectRef, dryRun bool, out *WriteResult) error {
	for _, target := range targets {
		act, err := deleteIfOwned(ctx, dyn, target.gvr, target.ns, target.name, dryRun)
		if err != nil {
			return err
		}
		if act != nil {
			out.Objects = append(out.Objects, *act)
		}
	}
	return nil
}

// providerDescription names a serving provider for a refusal.
func providerDescription(p detect.Provider) string {
	switch p {
	case detect.ProviderChart:
		return "the platform's own release"
	case detect.ProviderManual:
		return "a hand install"
	default:
		return string(p)
	}
}
