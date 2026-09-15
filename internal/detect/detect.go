// Package detect answers, per cluster, whether the GPU operator and the
// serving layer are present and who provides them: the platform's own release
// (a chart component switched on in GitOps), cluster-manager's releases, or a
// human by hand (a HelmRelease/App, a ClusterPolicy, GPU node labels). A
// chart-provided component always wins: cluster-manager never re-creates
// what the platform's release provides (bumblebee-plans#46 D2, round 3).
//
// This stage ships the vocabulary and the stubs; the detection itself arrives
// with the write tools (giantswarm/giantswarm#37637, stage 2c).
package detect

import "context"

// Status is whether a component was found on a cluster.
type Status string

const (
	// StatusPresent: the component is installed.
	StatusPresent Status = "present"
	// StatusAbsent: nothing provides the component.
	StatusAbsent Status = "absent"
	// StatusUnknown: the detection did not run (or could not read the
	// cluster); Provider is empty.
	StatusUnknown Status = "unknown"
)

// Provider is who provides a present component.
type Provider string

const (
	// ProviderChart: a component of the platform's own release, switched on
	// in GitOps (e.g. `components.gpu-operator` of the agent-platform chart).
	ProviderChart Provider = "chart"
	// ProviderClusterManager: a release cluster-manager created.
	ProviderClusterManager Provider = "cluster-manager"
	// ProviderManual: installed by hand — a HelmRelease or App of the
	// component, a ClusterPolicy, or GPU node labels without an operator
	// release.
	ProviderManual Provider = "manual"
)

// Component is one detected component of a cluster as list_clusters reports
// it.
type Component struct {
	Status   Status   `json:"status"`
	Provider Provider `json:"provider,omitempty"`
}

// Unknown is the answer of a detection that has not run.
func Unknown() Component { return Component{Status: StatusUnknown} }

// GPUOperator reports the GPU operator on the cluster named by namespace/name.
//
// TODO(giantswarm/giantswarm#37637, stage 2c): detect the chart component,
// a gpu-operator HelmRelease/App, a ClusterPolicy and GPU node labels.
func GPUOperator(_ context.Context, _, _ string) Component { return Unknown() }

// Serving reports the serving layer (KServe and llmisvc APIs, the discovery
// ConfigMap, the models Gateway) on the cluster named by namespace/name.
//
// TODO(giantswarm/giantswarm#37637, stage 2c): detect the chart slice, a
// serving HelmRelease/App and the serving APIs.
func Serving(_ context.Context, _, _ string) Component { return Unknown() }
