package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
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
	// Cache (enable) is whether the slice's predictors mount a model cache
	// claim (false composes compose.SliceSpec.NoCache); nil keeps the
	// release's setting, off for a first slice (cacheChoice) — the claim the
	// release mounts already, else the base name (giantswarm/cluster-manager#71).
	Cache *bool
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
	// JWKS is where the models Gateway fetches the login issuer's key set:
	// the platform's Dex service in-cluster on the installation's own
	// cluster, the public issuer on a workload cluster.
	JWKS string `json:"jwks"`
}

// EnableModelServing creates the cluster's slice release with the serving
// slice on — or updates the one that exists, never a second release of the
// chart on one cluster — and registers the cluster's kserve backend with
// model-manager, the document carrying the instance shapes of the cluster's
// GPU pools, read from their releases (backendPools). A pool is not
// required. Refused where the platform's own release or a human provides
// serving already. LLMInferenceServiceConfigs a serving layer that went left
// terminating in the release namespace are healed before the slice lands
// (giantswarm/cluster-manager#28).
func (s *Service) EnableModelServing(ctx context.Context, in ModelServingInput) (*WriteResult, error) {
	start := time.Now()
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
	releases, err := ownPools(ctx, dyn, c.GetNamespace(), c.GetName())
	if err != nil {
		return nil, err
	}
	pool := onlyOf(poolNamesOf(releases, ""))
	reads, err := s.readSlice(ctx, dyn, target)
	if err != nil {
		return nil, err
	}
	// The slice keeps the claim its release mounts (the zone's claim the
	// last pool named), else the base name: enable_model_serving never moves
	// the cache to another zone (giantswarm/cluster-manager#71).
	pin := zonePin{cache: cacheChoice(in.Cache, reads.cache), claimName: reads.cacheClaim}
	if pin.claimName == "" {
		pin.claimName = s.cfg.cacheClaimName()
	}
	// The answer says where the claim the slice mounts stands, what it
	// costs, and what becomes of the claims that exist with the cache off.
	claims := s.readCacheClaims(ctx, target)
	infra := awsInfrastructure(ctx, dyn, c)
	// The cache is the cluster's setting: off while the release runs with
	// it on is refused, the way out being to remove the cache
	// (giantswarm/cluster-manager#83).
	if !pin.cache && reads.cache != nil && reads.cache.Enabled {
		return nil, cacheOnRefusal(claims, c.GetName(), reads.release, reads.cache, infra.region)
	}
	serving, slice, objs, err := s.sliceRelease(reads, target, facts, pool, pin.sliceCache())
	if err != nil {
		return nil, err
	}
	if slice == nil {
		return nil, &ErrRefused{Reason: fmt.Sprintf("serving is already present on %s, provided by %s (%s): the slice release is composed only where nothing provides serving — nothing to do", c.GetName(), providerDescription(serving.Provider), strings.Join(serving.Evidence, "; "))}
	}
	pin.claim = claims.named(pin.claimName)
	claims.priced(infra.region, reads.cache)
	pools, err := backendPools(c.GetName(), releases, "", nil)
	if err != nil {
		return nil, err
	}
	existing, err := existingBackend(ctx, dyn, s.cfg.ModelManagerNamespace)
	if err != nil {
		return nil, err
	}
	backend, err := s.backendDocument(target, pools, existing)
	if err != nil {
		return nil, err
	}
	out := &WriteResult{
		Cluster: c.GetName(), Namespace: c.GetNamespace(), Mode: in.Mode, DryRun: in.DryRun, Objects: []ObjectAction{},
		Serving: serving, Slice: slice, Cache: s.cacheSettingFor(slice, claims, pin, s.cacheFacts(ctx, c.GetName(), infra.region, reads, slice, pin)),
		CacheClaim: pin.claim, CacheClaims: claims.claims,
		Backend: &BackendRegistration{Kind: compose.BackendKindKServe, Namespace: backend.GetNamespace(), Name: backend.GetName(), Target: backendTargetName(target.backend)},
	}
	if err := healStrandedConfigs(ctx, target, in.DryRun, out); err != nil {
		return nil, err
	}
	if err := applyAll(ctx, dyn, append(objs, backend), in.DryRun, out, s.budget(ctx, start)); err != nil {
		return nil, err
	}
	logApplied(ctx, "enable_model_serving", out, start)
	return out, nil
}

// DisableModelServing removes the slice release cluster-manager created and
// the backend it registered. Unless forced it refuses while models are
// served on the cluster, naming them — or plainly when the cluster cannot
// be read as the caller. The slice goes in order (servingTeardown), within
// the call's budget, the slice release last: the re-run finds it and
// continues where the teardown stands (giantswarm/cluster-manager#28, #37).
func (s *Service) DisableModelServing(ctx context.Context, in ModelServingInput) (*WriteResult, error) {
	start := time.Now()
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
	t := s.target(ctx, dyn, c)
	if !in.Force {
		if err := servedModelsGuard(ctx, t); err != nil {
			return nil, err
		}
	}
	out := &WriteResult{Cluster: c.GetName(), Namespace: ns, Mode: in.Mode, DryRun: in.DryRun, Objects: []ObjectAction{}}
	targets, err := s.sliceRemovals(ctx, dyn, ns, c.GetName())
	if err != nil {
		return nil, err
	}
	plans, err := planDeletes(ctx, dyn, targets)
	if err != nil {
		return nil, err
	}
	td := newTeardown(ctx, dyn, in.DryRun, out, s.budget(ctx, start))
	if err := s.servingTeardown(td, t, in.Force); err != nil {
		return nil, err
	}
	if err := td.deleteAll(plans); err != nil {
		return nil, err
	}
	td.finish()
	logApplied(ctx, "disable_model_serving", out, start)
	return out, nil
}

// sliceReads is what the slice's part of a write reads: the serving layer
// detected on the target and, where the slice would be composed, the
// platform's inputs and the model cache claim the slice release mounts
// already (its values' modelServing.cache.pvc.name; empty for the chart's
// default, or no release yet).
type sliceReads struct {
	serving    detect.Component
	platform   compose.PlatformInputs
	cacheClaim string
	// release names cluster-manager's slice release of the cluster
	// (`namespace/name`) and cache its model cache setting as its values
	// state it; nil while there is no release (giantswarm/cluster-manager#83).
	release string
	cache   *detect.SliceCache
}

// readSlice detects serving on the target and, where the slice would be
// composed, reads the platform's inputs and the cluster's releases of the
// chart — a second release under another name is a refusal, the slice's own
// says which claim it mounts — the two concurrently. A refusal from either
// is returned as the error.
func (s *Service) readSlice(ctx context.Context, dyn dynamic.Interface, t target) (sliceReads, error) {
	r := sliceReads{serving: detect.Serving(ctx, t.Target)}
	if !composesSlice(r.serving) {
		return r, nil
	}
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		hr, err := s.sliceReleaseOf(gctx, dyn, t.Namespace, t.Cluster)
		if hr != nil {
			r.cacheClaim, _, _ = unstructured.NestedString(hr.Object, "spec", "values", "modelServing", "cache", "pvc", "name")
			r.release = hr.GetNamespace() + "/" + hr.GetName()
			r.cache = detect.SliceCacheOf(hr)
		}
		return err
	})
	g.Go(func() (err error) {
		r.platform, err = s.platformInputs(gctx, dyn)
		return err
	})
	return r, g.Wait()
}

// cacheFacts gathers what the cache block is worded from: the cluster's
// region, the slice's cache setting before the write, and — for a slice
// composed with the cache on whose claim does not exist yet — the size and
// tier the connectivity chart at the slice's version creates it with, read
// from the registry (compose.ReadCacheDefaults); a read that fails is the
// note, never a guess (giantswarm/cluster-manager#83).
func (s *Service) cacheFacts(ctx context.Context, cluster string, region compose.Region, reads sliceReads, slice *SliceRelease, pin zonePin) cacheFacts {
	f := cacheFacts{cluster: cluster, region: region, before: reads.cache}
	if slice == nil || !pin.cache || pin.claim != nil {
		return f
	}
	if s.charts == nil {
		f.defaultsNote = "this server reads no chart registry"
		return f
	}
	defer timed(ctx, "cache defaults from the chart")()
	defaults, err := compose.ReadCacheDefaults(ctx, s.charts, slice.ChartVersion)
	if err != nil {
		f.defaultsNote = err.Error()
		return f
	}
	f.defaults = defaults
	return f
}

// composesSlice reports whether the slice's part of a write is composed: when
// nothing provides serving on the target, or the serving there is
// cluster-manager's own (the re-run is its update). Not when someone else
// provides it, nor when the target cannot be read.
func composesSlice(serving detect.Component) bool {
	if serving.Status == detect.StatusUnknown {
		return false
	}
	return !serving.Present() || serving.Provider == detect.ProviderClusterManager
}

// sliceRelease decides the slice's part of a write from what was read:
// nothing when serving runs on the target and someone else provides it (the
// platform's chart, a human), the `<cluster>-agent-platform` release filled
// from the platform's inputs when none does — or when the one running is
// cluster-manager's own, so the re-run is its update —, a refusal when the
// cluster cannot be read. cache is the model cache the slice serves from:
// on with the claim its predictors mount, or off.
func (s *Service) sliceRelease(r sliceReads, t target, facts compose.Cluster, pool string, cache sliceCache) (detect.Component, *SliceRelease, []*unstructured.Unstructured, error) {
	serving := r.serving
	switch {
	case serving.Status == detect.StatusUnknown:
		return serving, nil, nil, &ErrRefused{Reason: fmt.Sprintf("cannot tell whether serving runs on %s (%s): the slice release is composed only when nothing provides serving — make the cluster readable as you (its apiserver must trust the installation's identity provider) and re-run", t.Cluster, serving.Reason)}
	case !composesSlice(serving):
		return serving, nil, nil, nil
	}
	var err error
	spec := compose.SliceSpec{ChartVersion: s.cfg.SliceChartVersion, OwnCluster: t.backend.OwnCluster, Platform: r.platform, Pool: pool, CertificateIssuer: s.cfg.CertificateIssuer, NoCache: !cache.on, CacheClaim: cache.claim}
	if spec.ChartVersion, err = compose.SliceChartVersion(spec); err != nil {
		return serving, nil, nil, &ErrRefused{Reason: err.Error()}
	}
	jwks, err := compose.SliceJWKS(spec)
	if err != nil {
		return serving, nil, nil, &ErrRefused{Reason: err.Error()}
	}
	objs, err := compose.Slice(facts, spec)
	if err != nil {
		return serving, nil, nil, err
	}
	domain := compose.SliceDomain(facts, spec)
	slice := &SliceRelease{
		Name: objs[1].GetName(), Namespace: objs[1].GetNamespace(), ChartVersion: nestedString(objs[0], "spec", "ref", "tag"),
		Domain: domain, ModelsHost: compose.ModelsHost(domain), GPUPool: pool, JWKS: jwks.URL(),
	}
	return serving, slice, objs, nil
}

// sliceReleaseOf reads the cluster's releases of the agent-platform chart:
// the `<cluster>-agent-platform` release as it exists (nil for none), and a
// refusal when the cluster has a release of the chart under another name —
// one release of the chart per cluster, its slices toggles in the values.
func (s *Service) sliceReleaseOf(ctx context.Context, dyn dynamic.Interface, ns, cluster string) (*unstructured.Unstructured, error) {
	hrs, err := dyn.Resource(HelmReleaseGVR).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: compose.LabelCluster + "=" + cluster})
	if err != nil {
		return nil, fmt.Errorf("list releases of %s: %w", cluster, err)
	}
	var own *unstructured.Unstructured
	for i := range hrs.Items {
		hr := &hrs.Items[i]
		switch {
		case hr.GetName() == compose.SliceReleaseName(cluster):
			own = hr
		case chartOf(ctx, dyn, hr) == compose.SliceChart:
			return nil, &ErrRefused{Reason: fmt.Sprintf("cluster %s already has a release of the %s chart under another name (HelmRelease %s/%s, %s): a cluster has one release of the chart, its slices toggles in the values — switch the serving slice on in that release, or remove it and re-run", cluster, compose.SliceChart, ns, hr.GetName(), ownerDescription(hr))}
		}
	}
	return own, nil
}

// platformInputs reads global.domain, global.identity, the wildcard
// certificate, where the installation's Dex serves its key set in-cluster
// (gateway.jwksEgress), the namespaces its workloads and its Substrate run in
// (gitops.targetNamespace, else the release's deployed namespace;
// components.substrate.targetNamespace) and the chart version it runs
// (status.history[0].chartVersion) from the installation's own release of
// the agent-platform chart: the HelmRelease named agent-platform, else the
// one release of the chart that is not cluster-manager's. Secrets it
// references are not read. The releases named agent-platform are looked at
// first: a fleet release names its chart through an OCIRepository, read per
// release, and an installation has hundreds of releases — the one named
// agent-platform settles it with one read, the others are read only when no
// release of that name is the chart's.
func (s *Service) platformInputs(ctx context.Context, dyn dynamic.Interface) (compose.PlatformInputs, error) {
	hrs, err := dyn.Resource(HelmReleaseGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return compose.PlatformInputs{}, fmt.Errorf("list HelmReleases: %w", err)
	}
	var candidates []*unstructured.Unstructured
	for _, named := range []bool{true, false} {
		for i := range hrs.Items {
			hr := &hrs.Items[i]
			if (hr.GetName() == platformReleaseName) != named || compose.OwnedBy(hr) || chartOf(ctx, dyn, hr) != compose.SliceChart {
				continue
			}
			if named {
				candidates = []*unstructured.Unstructured{hr}
				break
			}
			candidates = append(candidates, hr)
		}
		if len(candidates) > 0 {
			break
		}
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
	out.Dex.Namespace, _, _ = unstructured.NestedString(vals, "gateway", "jwksEgress", "namespace")
	out.Dex.Port = nestedInt(&unstructured.Unstructured{Object: vals}, "gateway", "jwksEgress", "port")
	out.Namespace, _, _ = unstructured.NestedString(vals, "gitops", "targetNamespace")
	if out.Namespace == "" {
		out.Namespace = runningNamespace(hr)
	}
	out.SubstrateNamespace, _, _ = unstructured.NestedString(vals, "components", "substrate", "targetNamespace")
	return out, nil
}

// runningChartVersion is the chart version a HelmRelease runs, and
// runningNamespace the namespace its release is deployed in: the newest entry
// of its status.history, empty before the first deployment.
func runningChartVersion(hr *unstructured.Unstructured) string {
	return newestHistory(hr, "chartVersion")
}

func runningNamespace(hr *unstructured.Unstructured) string { return newestHistory(hr, "namespace") }

func newestHistory(hr *unstructured.Unstructured, field string) string {
	history, _, _ := unstructured.NestedSlice(hr.Object, "status", "history")
	if len(history) == 0 {
		return ""
	}
	newest, _ := history[0].(map[string]any)
	v, _ := newest[field].(string)
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

// ownPools are the cluster's GPU pool releases of cluster-manager's, by pool
// name.
func ownPools(ctx context.Context, dyn dynamic.Interface, ns, cluster string) (map[string]*unstructured.Unstructured, error) {
	pools, err := dyn.Resource(HelmReleaseGVR).Namespace(ns).List(ctx, metav1.ListOptions{
		LabelSelector: compose.LabelChartName + "=" + compose.PoolChart + "," + compose.LabelCluster + "=" + cluster + "," + compose.LabelManagedBy + "=" + compose.ManagedBy,
	})
	if err != nil {
		return nil, fmt.Errorf("list pool releases of %s: %w", cluster, err)
	}
	out := make(map[string]*unstructured.Unstructured, len(pools.Items))
	for i := range pools.Items {
		out[pools.Items[i].GetLabels()[compose.LabelPool]] = &pools.Items[i]
	}
	return out, nil
}

// poolNames are the cluster's GPU pools of cluster-manager's, by pool name,
// sorted: its pool releases plus adding (the pool a write creates), counted
// once. The operator's Node Feature Discovery worker is pinned to all of
// them (compose.PoolAffinity); the slice's predictors to the one of them.
func poolNames(ctx context.Context, dyn dynamic.Interface, ns, cluster, adding string) ([]string, error) {
	releases, err := ownPools(ctx, dyn, ns, cluster)
	if err != nil {
		return nil, err
	}
	return poolNamesOf(releases, adding), nil
}

// poolNamesOf names the pools of releases plus adding, counted once, sorted.
func poolNamesOf(releases map[string]*unstructured.Unstructured, adding string) []string {
	out := make([]string, 0, len(releases)+1)
	if _, ok := releases[adding]; adding != "" && !ok {
		out = append(out, adding)
	}
	for name := range releases {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// onlyPool is the GPU pool the slice's predictors are pinned to: the
// cluster's one pool release of cluster-manager's (adding counts as one),
// by pool name. Empty when the cluster has none or several — the predictors
// are then placed by their GPU request alone, on whichever pool offers it —
// so the rule is the same for every write and never flips between pools.
func onlyPool(ctx context.Context, dyn dynamic.Interface, ns, cluster, adding string) (string, error) {
	pools, err := poolNames(ctx, dyn, ns, cluster, adding)
	if err != nil {
		return "", err
	}
	return onlyOf(pools), nil
}

// onlyOf is the one name of a list of one, else empty.
func onlyOf(names []string) string {
	if len(names) != 1 {
		return ""
	}
	return names[0]
}

// backendPools are the instance shapes of the cluster's GPU pools as the
// kserve backend document names them (compose.KServeBackend), by release
// name — the node label giantswarm.io/machine-pool the chart stamps: each
// pool release's, read from its values (the chart's pool.accelerator and
// pool.sizes; no sizes is the chart's default), and adding's — the pool a
// create_node_pool composes, empty for none — as composed (shapes), never
// what its release declared before the write (giantswarm/cluster-manager#89).
func backendPools(cluster string, releases map[string]*unstructured.Unstructured, adding string, shapes []compose.InstanceShape) (map[string][]compose.InstanceShape, error) {
	out := make(map[string][]compose.InstanceShape, len(releases)+1)
	for pool, hr := range releases {
		if pool == adding {
			continue
		}
		accelerator, sizes := poolValues(hr)
		poolShapes, err := compose.Shapes(accelerator, sizes)
		if err != nil {
			return nil, fmt.Errorf("pool %s of %s (HelmRelease %s/%s): %w", pool, cluster, hr.GetNamespace(), hr.GetName(), err)
		}
		out[compose.ReleaseName(cluster, pool)] = poolShapes
	}
	if adding != "" {
		out[compose.ReleaseName(cluster, adding)] = shapes
	}
	return out, nil
}

// poolValues reads a pool release's accelerator and sizes: the chart's
// pool.accelerator and pool.sizes values, as compose.Pool writes them.
func poolValues(hr *unstructured.Unstructured) (string, []string) {
	raw, _, _ := unstructured.NestedSlice(hr.Object, "spec", "values", "pool", "sizes")
	sizes := make([]string, 0, len(raw))
	for _, s := range raw {
		if size, ok := s.(string); ok {
			sizes = append(sizes, size)
		}
	}
	return nestedString(hr, "spec", "values", "pool", "accelerator"), sizes
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
	registered, err := backendRegisteredFor(ctx, dyn, s.cfg.ModelManagerNamespace, cluster)
	if err != nil {
		return nil, err
	}
	var targets []objectRef
	if registered {
		targets = append(targets, objectRef{compose.ConfigMapGVR, s.cfg.ModelManagerNamespace, compose.BackendConfigMapName})
	}
	name := compose.SliceReleaseName(cluster)
	return append(targets, objectRef{compose.OCIRepositoryGVR, ns, name}, objectRef{HelmReleaseGVR, ns, name}), nil
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
