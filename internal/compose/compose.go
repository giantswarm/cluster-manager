// Package compose renders the objects cluster-manager writes: the pool
// release of the `gpu-node-pool` chart (a HelmRelease plus its own
// OCIRepository in `org-<org>`), the `<cluster>-gpu-operator` release and the
// `<cluster>-agent-platform` slice release (bumblebee-plans#46 D2–D4). The
// renderers arrive with the write tools (giantswarm/giantswarm#37637, stage
// 2b); this stage fixes the names and labels the read tools already select
// by, so the two sides agree from the first release.
package compose

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// PoolChart is the chart of a GPU pool release.
	PoolChart = "gpu-node-pool"

	// LabelChartName marks a HelmRelease with the chart it installs; a
	// HelmRelease whose source is an OCIRepository (`spec.chartRef`) names
	// no chart in its spec, so the label is what the read tools select by.
	LabelChartName = "app.kubernetes.io/name"
	// LabelCluster marks an object with the Cluster API cluster it belongs
	// to (the fleet's convention for objects in `org-<org>`).
	LabelCluster = "giantswarm.io/cluster"
	// LabelManagedBy marks the objects cluster-manager created.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// ManagedBy is LabelManagedBy's value on cluster-manager's objects.
	ManagedBy = "cluster-manager"
)

// object builds one composed object of the given resource and kind from its
// metadata and its remaining top-level fields (spec, data, ...).
func object(gvr schema.GroupVersionResource, kind string, metadata map[string]any, fields map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": gvr.GroupVersion().String(),
		"kind":       kind,
		"metadata":   metadata,
	}
	for k, v := range fields {
		obj[k] = v
	}
	return &unstructured.Unstructured{Object: obj}
}
