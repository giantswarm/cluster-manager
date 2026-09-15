package tools

import (
	"context"
	"fmt"
	"sort"

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
	GPUOperator detect.Component `json:"gpuOperator"`
	Serving     detect.Component `json:"serving"`
	// PoolReleases are the GPU pool releases (HelmReleases of the
	// gpu-node-pool chart) of the cluster.
	PoolReleases []PoolRelease `json:"poolReleases"`
	// CommitTarget is the git repository and directory owning the cluster
	// (from its Flux provenance), null when the cluster has none or commit
	// mode is not available.
	CommitTarget *CommitTarget `json:"commitTarget"`
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
}

// CommitTarget is where commit mode would write a cluster's files.
type CommitTarget struct {
	Repository string `json:"repository"`
	Path       string `json:"path"`
}

// ListClusters lists the installation's clusters with their GPU pool
// releases, sorted by namespace and name.
func (s *Service) ListClusters(ctx context.Context) ([]Cluster, error) {
	dyn := s.clients(ctx)
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

	out := make([]Cluster, 0, len(clusters.Items))
	for i := range clusters.Items {
		c := &clusters.Items[i]
		key := c.GetNamespace() + "/" + c.GetName()
		target := s.target(ctx, dyn, c)
		out = append(out, Cluster{
			Name:           c.GetName(),
			Namespace:      c.GetNamespace(),
			Organization:   organization(c),
			ReleaseVersion: c.GetLabels()[LabelReleaseVersion],
			OwnCluster:     s.ownCluster(c),
			GPUOperator:    detect.GPUOperator(ctx, target.Target),
			Serving:        detect.Serving(ctx, target.Target),
			PoolReleases:   sortedPools(pools[key]),
			// TODO(giantswarm/giantswarm#37637, commit mode): repository and
			// path from the Flux provenance of the cluster's owning object.
			CommitTarget: nil,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func poolRelease(hr *unstructured.Unstructured) PoolRelease {
	version := nestedString(hr, "status", "lastAttemptedRevision")
	if version == "" {
		version = nestedString(hr, "spec", "chart", "spec", "version")
	}
	return PoolRelease{Name: hr.GetName(), Namespace: hr.GetNamespace(), ChartVersion: version, Ready: readyCondition(hr)}
}

// readyCondition reads the Ready condition of a status.conditions list.
func readyCondition(obj *unstructured.Unstructured) *bool {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != "Ready" {
			continue
		}
		ready := m["status"] == "True"
		return &ready
	}
	return nil
}

func sortedPools(p []PoolRelease) []PoolRelease {
	if p == nil {
		return []PoolRelease{}
	}
	sort.Slice(p, func(i, j int) bool { return p[i].Name < p[j].Name })
	return p
}
