package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/registry"
)

// Release states of a Release CR (its CRD's enum); only an active release is
// offered for a new cluster.
const ReleaseActive = "active"

// Release is one Release CR as list_releases answers it.
type Release struct {
	// Name is the Release CR's name, `<provider>-<version>`.
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Version  string `json:"version"`
	// State is the Release CR's spec.state: active, deprecated, wip or
	// preview.
	State             string `json:"state"`
	Date              string `json:"date,omitempty"`
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`
	// ClusterChart is the cluster chart the release pins
	// (`cluster-aws@10.3.1`).
	ClusterChart string `json:"clusterChart,omitempty"`
	// ReleaseChart is the chart a cluster of this release installs.
	ReleaseChart ReleaseChart `json:"releaseChart"`
	// Offered is whether create_cluster creates a cluster of this release:
	// active, its release chart published, and a provider line
	// create_cluster offers. Note says why not.
	Offered bool   `json:"offered"`
	Note    string `json:"note,omitempty"`
}

// ReleaseChart is the release chart of one release: `release-<provider>` at
// the release's version, the cluster chart renamed by the releases
// repository.
type ReleaseChart struct {
	URL     string `json:"url"`
	Version string `json:"version"`
	// Published is whether the registry lists the tag; null when the
	// registry could not be read (Note says why).
	Published *bool  `json:"published"`
	Note      string `json:"note,omitempty"`
}

// ReleasesResult is list_releases' answer.
type ReleasesResult struct {
	// Providers are the provider lines create_cluster offers.
	Providers []string  `json:"providers"`
	Releases  []Release `json:"releases"`
}

// ListReleases lists the installation's Release CRs, of one provider line
// when provider is given, newest first, each with its state, Kubernetes
// version, the cluster chart it pins and its release chart, whose tag is
// checked in the registry (one tag list per provider line). Read as the
// caller.
func (s *Service) ListReleases(ctx context.Context, provider string) (*ReleasesResult, error) {
	list, err := s.clients(ctx).Dynamic.Resource(ReleaseGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list releases: %w", err)
	}
	out := &ReleasesResult{Providers: compose.Providers(), Releases: []Release{}}
	for i := range list.Items {
		r, ok := releaseOf(&list.Items[i])
		if !ok || (provider != "" && r.Provider != provider) {
			continue
		}
		out.Releases = append(out.Releases, r)
	}
	sortReleases(out.Releases)
	published := s.publishedTags(ctx, out.Releases)
	for i := range out.Releases {
		r := &out.Releases[i]
		tags := published[r.Provider]
		if tags.err != nil {
			r.ReleaseChart.Note = tags.err.Error()
		} else {
			p := tags.set[r.Version]
			r.ReleaseChart.Published = &p
		}
		r.Offered, r.Note = offered(*r)
	}
	return out, nil
}

// releaseOf reads a Release CR; false for a name that is not
// `<provider>-<version>`.
func releaseOf(obj *unstructured.Unstructured) (Release, bool) {
	provider, version, ok := splitReleaseName(obj.GetName())
	if !ok {
		return Release{}, false
	}
	components := releaseComponents(obj)
	r := Release{
		Name: obj.GetName(), Provider: provider, Version: version,
		State:             nestedString(obj, "spec", "state"),
		Date:              nestedString(obj, "spec", "date"),
		KubernetesVersion: components[releaseComponentKubernetes],
		ReleaseChart:      ReleaseChart{URL: compose.ReleaseChartURL(provider), Version: version},
	}
	if v := components[compose.ClusterChartPrefix+provider]; v != "" {
		r.ClusterChart = compose.ClusterChartPrefix + provider + "@" + v
	}
	return r, true
}

// splitReleaseName splits `<provider>-<version>` at the dash before the
// version.
func splitReleaseName(name string) (provider, version string, ok bool) {
	i := strings.LastIndex(name, "-")
	for i > 0 {
		if _, err := semver.StrictNewVersion(name[i+1:]); err == nil {
			return name[:i], name[i+1:], true
		}
		i = strings.LastIndex(name[:i], "-")
	}
	return "", "", false
}

// sortReleases orders by provider, then newest version first.
func sortReleases(rs []Release) {
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].Provider != rs[j].Provider {
			return rs[i].Provider < rs[j].Provider
		}
		return semver.MustParse(rs[i].Version).GreaterThan(semver.MustParse(rs[j].Version))
	})
}

// offered is whether create_cluster creates a cluster of r, and why not.
func offered(r Release) (bool, string) {
	switch {
	case !providerOffered(r.Provider):
		return false, fmt.Sprintf("create_cluster offers the provider lines %s", strings.Join(compose.Providers(), ", "))
	case r.State != ReleaseActive:
		return false, fmt.Sprintf("the release is %s: only an active release is offered for a new cluster", stateOrUnset(r.State))
	case r.ClusterChart == "":
		return false, fmt.Sprintf("the release pins no %s%s component", compose.ClusterChartPrefix, r.Provider)
	case r.ReleaseChart.Published == nil:
		return false, "the registry could not be read: whether the release chart is published is unknown"
	case !*r.ReleaseChart.Published:
		return false, fmt.Sprintf("%s:%s is not published in the registry", r.ReleaseChart.URL, r.Version)
	}
	return true, ""
}

func providerOffered(provider string) bool {
	for _, p := range compose.Providers() {
		if p == provider {
			return true
		}
	}
	return false
}

func stateOrUnset(state string) string {
	if state == "" {
		return "without a state"
	}
	return state
}

// tagSet is one release chart's published tags, or why they are unknown.
type tagSet struct {
	set map[string]bool
	err error
}

// publishedTags reads the tags of every provider line's release chart the
// releases name, concurrently.
func (s *Service) publishedTags(ctx context.Context, rs []Release) map[string]tagSet {
	providers := map[string]bool{}
	for _, r := range rs {
		providers[r.Provider] = true
	}
	out := make(map[string]tagSet, len(providers))
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for p := range providers {
		wg.Go(func() {
			set, err := s.releaseChartTags(ctx, p)
			mu.Lock()
			defer mu.Unlock()
			out[p] = tagSet{set: set, err: err}
		})
	}
	wg.Wait()
	return out
}

// releaseChartTags is the set of a provider line's published release chart
// tags.
func (s *Service) releaseChartTags(ctx context.Context, provider string) (map[string]bool, error) {
	if s.charts == nil {
		return nil, fmt.Errorf("this server reads no registry: the release charts' tags are unknown")
	}
	ref, err := registry.ParseRef(compose.ReleaseChartURL(provider))
	if err != nil {
		return nil, err
	}
	tags, err := s.charts.Tags(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("list the tags of %s: %w", ref, err)
	}
	set := make(map[string]bool, len(tags))
	for _, t := range tags {
		set[t] = true
	}
	return set, nil
}

// CreateClusterInput is create_cluster's input.
type CreateClusterInput struct {
	Organization string
	Name         string
	Provider     string
	// Release is the release version (`36.0.0`); empty is the newest active
	// release of the provider line.
	Release     string
	Identity    string
	Description string
	Values      map[string]any
	Mode        string
	DryRun      bool
}

// CreateCluster composes a workload cluster — the release chart's
// HelmRelease, its OCIRepository at the release's tag and the cluster's
// values ConfigMap in `org-<organization>` (giantswarm/giantswarm#37614) —
// and lands it as the caller. Before anything is written: the name is a DNS
// label of at most 20 characters no Cluster on the installation uses (a
// re-run on a cluster create_cluster created passes, and changes nothing),
// the release is active and its chart tag published, the organization's
// namespace carries the installation's values (and, applied, its tenant
// ServiceAccount is bound), and the values the release
// would install — the chart's defaults, the installation's, the cluster's —
// match the chart's own values.schema.json. Mode commit opens the pull
// request that adds the cluster to the repository owning its Organization
// instead (commitCreateCluster).
func (s *Service) CreateCluster(ctx context.Context, in CreateClusterInput) (*WriteResult, error) {
	start := time.Now()
	if err := s.checkMode(in.Mode, true); err != nil {
		return nil, err
	}
	spec := compose.ClusterSpec{
		Organization: in.Organization, Name: in.Name, Provider: in.Provider, Release: in.Release,
		Identity: in.Identity, Description: in.Description, Values: in.Values,
		TenantServiceAccount: s.cfg.TenantServiceAccount,
	}
	if err := spec.Validate(); err != nil {
		return nil, &ErrRefused{Reason: err.Error()}
	}
	k := s.clients(ctx)
	if err := requireClusterAPI(k.Discovery, spec.Name); err != nil {
		return nil, err
	}
	dyn := k.Dynamic
	var (
		installation map[string]any
		release      *Release
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return nameFree(gctx, dyn, spec) })
	g.Go(func() (err error) {
		installation, err = installationValues(gctx, dyn, spec.Namespace())
		return err
	})
	g.Go(func() (err error) {
		release, err = resolveRelease(gctx, dyn, spec.Provider, spec.Release)
		return err
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}
	if in.Mode != ModeCommit {
		if err := tenantBound(ctx, dyn, spec.Namespace(), spec.TenantServiceAccount); err != nil {
			return nil, err
		}
	}
	spec.Release = release.Version
	chart, err := s.releaseChart(ctx, *release)
	if err != nil {
		return nil, err
	}
	values, err := compose.ClusterValues(spec)
	if err != nil {
		return nil, &ErrRefused{Reason: err.Error()}
	}
	if err := compose.ValidateClusterValues(chart, installation, values); err != nil {
		return nil, &ErrRefused{Reason: err.Error()}
	}
	objs, err := compose.ClusterRelease(spec, values)
	if err != nil {
		return nil, err
	}
	out := &WriteResult{
		Cluster: spec.Name, Namespace: spec.Namespace(), Mode: in.Mode, DryRun: in.DryRun,
		Release: release.Name, ChartVersion: release.Version, KubernetesVersion: release.KubernetesVersion,
		Objects: []ObjectAction{},
	}
	if in.Mode == ModeCommit {
		err = s.commitCreateCluster(ctx, dyn, spec, objs, in.DryRun, out)
	} else {
		err = applyAll(ctx, dyn, objs, in.DryRun, out, s.budget(ctx, start))
	}
	if err != nil {
		return nil, err
	}
	logApplied(ctx, "create_cluster", out, start)
	return out, nil
}

// nameFree refuses a name a Cluster on the installation uses — in any
// namespace, since the name prefixes the cluster's cloud resources — unless
// it is the cluster create_cluster created under that name in the same
// organization: then the call is a re-run.
func nameFree(ctx context.Context, dyn dynamic.Interface, spec compose.ClusterSpec) error {
	list, err := dyn.Resource(ClusterGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list clusters: %w", err)
	}
	for _, c := range list.Items {
		if c.GetName() != spec.Name {
			continue
		}
		if c.GetNamespace() == spec.Namespace() {
			hr, err := dyn.Resource(HelmReleaseGVR).Namespace(spec.Namespace()).Get(ctx, spec.Name, metav1.GetOptions{})
			if err == nil && compose.OwnedBy(hr) {
				return nil
			}
		}
		return &ErrRefused{Reason: fmt.Sprintf("cluster name %s is taken: cluster %s/%s exists on this installation — choose another name", spec.Name, c.GetNamespace(), c.GetName())}
	}
	return nil
}

// installationValues reads the installation's cluster values the release
// reads first; the organization's namespace without them is refused, since
// the release would not reconcile.
func installationValues(ctx context.Context, dyn dynamic.Interface, ns string) (map[string]any, error) {
	cm, err := dyn.Resource(ConfigMapGVR).Namespace(ns).Get(ctx, compose.InstallationValuesConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, &ErrRefused{Reason: fmt.Sprintf("ConfigMap %s/%s does not exist: the installation syncs its cluster values into every organization's namespace, so the organization or its namespace does not exist yet — create the organization first", ns, compose.InstallationValuesConfigMap)}
	}
	if err != nil {
		return nil, fmt.Errorf("get ConfigMap %s/%s: %w", ns, compose.InstallationValuesConfigMap, err)
	}
	raw, _, _ := unstructured.NestedString(cm.Object, "data", compose.ValuesSecretKey)
	return parseValues(ns, compose.InstallationValuesConfigMap, compose.ValuesSecretKey, raw)
}

// tenantBound refuses an organization whose tenant ServiceAccount, the one
// the release installs under, is not bound yet: rbac-operator binds it
// minutes after the namespace and its installation values appear, and a
// release installed before then fails its first reconcile and waits out its
// interval. Checked after installationValues, so a missing namespace is
// refused as such; skipped in mode commit, whose cluster lands when its pull
// request merges, and on an installation without the tenancy policy.
func tenantBound(ctx context.Context, dyn dynamic.Interface, ns, serviceAccount string) error {
	if serviceAccount == "" {
		return nil
	}
	_, err := dyn.Resource(RoleBindingGVR).Namespace(ns).Get(ctx, compose.TenantRoleBinding, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return &ErrRefused{Reason: fmt.Sprintf("RoleBinding %s/%s does not exist yet: the organization's ServiceAccount %s, which installs the cluster, has no rights until rbac-operator binds it, within minutes of creating the organization — re-run create_cluster then", ns, compose.TenantRoleBinding, serviceAccount)}
	}
	if err != nil {
		return fmt.Errorf("get RoleBinding %s/%s: %w", ns, compose.TenantRoleBinding, err)
	}
	return nil
}

// resolveRelease is the release a new cluster runs: the version named, or the
// newest active release of the provider line. A named release that is not
// active, or pins no cluster chart, is refused.
func resolveRelease(ctx context.Context, dyn dynamic.Interface, provider, version string) (*Release, error) {
	list, err := dyn.Resource(ReleaseGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list releases: %w", err)
	}
	var line []Release
	for i := range list.Items {
		if r, ok := releaseOf(&list.Items[i]); ok && r.Provider == provider {
			line = append(line, r)
		}
	}
	sortReleases(line)
	for _, r := range line {
		if version != "" && r.Version != strings.TrimPrefix(version, "v") {
			continue
		}
		if version == "" && r.State != ReleaseActive {
			continue
		}
		switch {
		case r.State != ReleaseActive:
			return nil, &ErrRefused{Reason: fmt.Sprintf("release %s is %s: only an active release is offered for a new cluster — list_releases names them", r.Name, stateOrUnset(r.State))}
		case r.ClusterChart == "":
			return nil, &ErrRefused{Reason: fmt.Sprintf("release %s pins no %s%s component", r.Name, compose.ClusterChartPrefix, provider)}
		}
		return &r, nil
	}
	if version != "" {
		return nil, &ErrRefused{Reason: fmt.Sprintf("release %s-%s does not exist on this installation — list_releases names the releases", provider, strings.TrimPrefix(version, "v"))}
	}
	return nil, &ErrRefused{Reason: fmt.Sprintf("the installation has no active %s release — list_releases names the releases", provider)}
}

// releaseChart pulls the release's chart, after checking its tag is
// published: an unknown tag is refused naming the chart.
func (s *Service) releaseChart(ctx context.Context, r Release) (*registry.Chart, error) {
	tags, err := s.releaseChartTags(ctx, r.Provider)
	if err != nil {
		return nil, err
	}
	if !tags[r.Version] {
		return nil, &ErrRefused{Reason: fmt.Sprintf("release chart %s:%s is not published in the registry: the releases repository publishes it with the release — choose another release, list_releases names the ones offered", r.ReleaseChart.URL, r.Version)}
	}
	ref, err := registry.ParseRef(r.ReleaseChart.URL)
	if err != nil {
		return nil, err
	}
	chart, err := s.charts.Chart(ctx, ref, r.Version)
	if err != nil {
		return nil, fmt.Errorf("pull %s:%s: %w", r.ReleaseChart.URL, r.Version, err)
	}
	return chart, nil
}
