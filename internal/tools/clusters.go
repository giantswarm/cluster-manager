package tools

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/dynamic"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/detect"
)

// Cluster is one cluster of the installation as list_clusters reports it:
// what neither the portal's Clusters pages nor the Cluster API tools show
// (bumblebee-plans#46 D2).
type Cluster struct {
	Name         string `json:"name"`
	Namespace    string `json:"namespace"`
	Organization string `json:"organization"`
	// ReleaseVersion is the Giant Swarm release the cluster runs (the
	// release.giantswarm.io/version label), empty when the cluster is not
	// a release-managed one.
	ReleaseVersion string `json:"releaseVersion"`
	// OwnCluster marks the installation's own cluster (its management
	// cluster).
	OwnCluster bool `json:"ownCluster"`
	// GPUOperator and Serving say whether the component is present and who
	// provides it.
	GPUOperator GPUOperatorComponent `json:"gpuOperator"`
	Serving     ServingComponent     `json:"serving"`
	// PoolReleases are the GPU pool releases (HelmReleases of the
	// gpu-node-pool chart) of the cluster.
	PoolReleases []PoolRelease `json:"poolReleases"`
	// Zones are the availability zones of the cluster's node subnets — the
	// zones a pool's nodes may come up in, the ones create_node_pool's zones
	// are checked against (giantswarm/cluster-manager#79); empty with
	// ZonesNote saying why they cannot be read.
	Zones     []string `json:"zones"`
	ZonesNote string   `json:"zonesNote,omitempty"`
	// CommitTarget is where commit mode writes the cluster's releases: the
	// git repository, branch and directory from the Flux provenance of the
	// cluster's HelmRelease or App, null when no Kustomization owns it.
	CommitTarget *CommitTarget `json:"commitTarget"`
}

// GPUOperatorComponent is the GPU operator's detection with its readiness:
// the operator release, the ClusterPolicy's state, the operands' pods.
type GPUOperatorComponent struct {
	detect.Component
	Readiness detect.OperatorReadiness `json:"readiness"`
}

// ServingComponent is the serving layer's detection with its readiness: the
// slice release and its children, the controllers, the configs, the backend
// registration, the presets, the models Gateway.
type ServingComponent struct {
	detect.Component
	Readiness detect.ServingReadiness `json:"readiness"`
}

// PoolRelease is one GPU pool release of a cluster.
type PoolRelease struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// ChartVersion is the chart version last attempted by helm-controller,
	// else the version pinned in the spec.
	ChartVersion string `json:"chartVersion"`
	// Ready mirrors the HelmRelease's Ready condition; null until it reports
	// one.
	Ready *bool `json:"ready"`
	// Deleting: the release carries a deletionTimestamp (a removal under way).
	Deleting bool `json:"deleting,omitempty"`
}

// CommitTarget is where commit mode writes a cluster's releases.
type CommitTarget struct {
	// Repository is owner/name, Branch the one the Kustomization follows.
	Repository string `json:"repository"`
	Branch     string `json:"branch"`
	// Path is the directory the releases go to: CommitDirectory under the
	// Kustomization's spec.path.
	Path string `json:"path"`
	// Kustomization (namespace/name) lands the files; Prune says whether a
	// file removed from git is removed from the installation too.
	Kustomization string `json:"kustomization"`
	Prune         bool   `json:"prune"`
	// Note says why the provenance could not be read, the rest then empty.
	Note string `json:"note,omitempty"`
}

// commitTargetOf is the cluster's commit target for list_clusters: nil when
// no Kustomization owns it, the reason when it cannot be followed.
func commitTargetOf(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured) *CommitTarget {
	loc, err := commitLocationOf(ctx, dyn, c)
	switch {
	case errors.Is(err, errNotFromGit):
		return nil
	case err != nil:
		return &CommitTarget{Note: err.Error()}
	}
	return loc.target()
}

// ClusterList is list_clusters' answer: the clusters, and whether the
// installation serves the Cluster API at all — an empty list on an
// installation without it comes with the note saying why.
type ClusterList struct {
	Clusters   []Cluster  `json:"clusters"`
	ClusterAPI ClusterAPI `json:"clusterApi"`
}

// ListClusters lists the installation's clusters with their GPU pool
// releases, sorted by namespace and name. On an installation without the
// Cluster API the list is empty and ClusterAPI carries the note.
func (s *Service) ListClusters(ctx context.Context) (*ClusterList, error) {
	k := s.clients(ctx)
	api := clusterAPI(k.Discovery)
	switch api.State {
	case ClusterAPIAbsent:
		return &ClusterList{Clusters: []Cluster{}, ClusterAPI: api}, nil
	case ClusterAPIUnknown:
		return nil, errors.New(api.Note)
	}
	dyn := k.Dynamic
	clusters, err := dyn.Resource(ClusterGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}
	releases, err := dyn.Resource(HelmReleaseGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		LabelSelector: compose.LabelChartName + "=" + compose.PoolChart,
	})
	if err != nil {
		return nil, fmt.Errorf("list pool releases: %w", err)
	}
	pools := map[string][]PoolRelease{}
	for i := range releases.Items {
		hr := &releases.Items[i]
		key := hr.GetNamespace() + "/" + hr.GetLabels()[compose.LabelCluster]
		pools[key] = append(pools[key], poolRelease(hr))
	}

	out := make([]Cluster, len(clusters.Items))
	g, gctx := errgroup.WithContext(ctx)
	for i := range clusters.Items {
		c := &clusters.Items[i]
		g.Go(func() error {
			out[i] = s.cluster(gctx, dyn, c, sortedPools(pools[c.GetNamespace()+"/"+c.GetName()]))
			return nil
		})
	}
	_ = g.Wait()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return &ClusterList{Clusters: out, ClusterAPI: api}, nil
}

// cluster is one cluster of the answer; the operator and serving detections
// read the cluster concurrently.
func (s *Service) cluster(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured, pools []PoolRelease) Cluster {
	target := s.target(ctx, dyn, c)
	var operator GPUOperatorComponent
	var serving ServingComponent
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		operator.Component, operator.Readiness = detect.GPUOperatorState(gctx, target.Target)
		return nil
	})
	g.Go(func() error {
		serving.Component, serving.Readiness = detect.ServingState(gctx, target.Target)
		return nil
	})
	// The backend is read on the installation beside the target reads and
	// joins the serving readiness after the wait: assigned into it while
	// ServingState assigns the whole struct, it was lost whenever the
	// target reads finished last (giantswarm/cluster-manager#41).
	var backend detect.BackendState
	g.Go(func() error { backend = s.backendState(gctx, dyn, c.GetName()); return nil })
	// The cache claims likewise: the tools know their namespace and base
	// name. Null when they cannot be read as the caller.
	var claims cacheClaims
	g.Go(func() error { claims = s.readCacheClaims(gctx, target); return nil })
	// The node subnets' zones, from the AWSCluster: what a pool may be
	// pinned to, for a caller to offer as choices.
	var infra awsInfra
	g.Go(func() error { infra = awsInfrastructure(gctx, dyn, c); return nil })
	_ = g.Wait()
	serving.Readiness.Backend = backend
	// Each claim priced in the cluster's region, the one the slice mounts
	// marked (giantswarm/cluster-manager#83).
	serving.Readiness.CacheClaims = claims.priced(infra.region, serving.Readiness.Cache)
	return Cluster{
		Name:           c.GetName(),
		Namespace:      c.GetNamespace(),
		Organization:   organization(c),
		ReleaseVersion: c.GetLabels()[LabelReleaseVersion],
		OwnCluster:     s.ownCluster(c),
		GPUOperator:    operator,
		Serving:        serving,
		PoolReleases:   pools,
		Zones:          infra.zonesList(),
		ZonesNote:      infra.zonesReason,
		CommitTarget:   commitTargetOf(ctx, dyn, c),
	}
}

// backendState says whether model-manager's kserve backend document is
// registered for the cluster, and where it is; a read failure is answered
// as such, with registered unknown.
func (s *Service) backendState(ctx context.Context, dyn dynamic.Interface, cluster string) detect.BackendState {
	registered, err := backendRegisteredFor(ctx, dyn, s.cfg.ModelManagerNamespace, cluster)
	if err != nil {
		return detect.BackendState{Error: err.Error()}
	}
	state := detect.BackendState{Registered: &registered}
	if registered {
		state.Namespace, state.Name = s.cfg.ModelManagerNamespace, compose.BackendConfigMapName
	}
	return state
}

func poolRelease(hr *unstructured.Unstructured) PoolRelease {
	version := nestedString(hr, "status", "lastAttemptedRevision")
	if version == "" {
		version = nestedString(hr, "spec", "chart", "spec", "version")
	}
	return PoolRelease{Name: hr.GetName(), Namespace: hr.GetNamespace(), ChartVersion: version, Ready: readyCondition(hr), Deleting: hr.GetDeletionTimestamp() != nil}
}

// readyCondition reads the Ready condition of a status.conditions list: true
// or false by its status, nil while there is none.
func readyCondition(obj *unstructured.Unstructured) *bool {
	cond, found := detect.ReadyCondition(obj)
	if !found {
		return nil
	}
	ready := cond.Status == "True"
	return &ready
}

func sortedPools(p []PoolRelease) []PoolRelease {
	if p == nil {
		return []PoolRelease{}
	}
	sort.Slice(p, func(i, j int) bool { return p[i].Name < p[j].Name })
	return p
}
