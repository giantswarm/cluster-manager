// Package detect answers, per cluster, whether the GPU operator and the
// serving layer are present and who provides them: the platform's own release
// (a chart component switched on in GitOps), cluster-manager's releases, or a
// human by hand (a HelmRelease/App, a ClusterPolicy, GPU node labels). A
// chart-provided component always wins: cluster-manager never re-creates
// what the platform's release provides, and never edits it — the handover
// is the human deleting cluster-manager's release (bumblebee-plans#46 D3).
//
// Detection reads the target cluster through its own apiserver as the
// caller (the kubeconfig Secret is not used for reading), and the
// installation for the releases that target the cluster from `org-<org>`.
package detect

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/cluster-manager/internal/compose"
)

// Status is whether a component was found on a cluster.
type Status string

const (
	// StatusPresent: the component is installed.
	StatusPresent Status = "present"
	// StatusAbsent: nothing provides the component.
	StatusAbsent Status = "absent"
	// StatusUnknown: the detection could not read the cluster; Reason says
	// why and Provider is empty.
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

// precedence orders providers: the chart wins over cluster-manager, both
// over a manual install.
var precedence = map[Provider]int{ProviderChart: 3, ProviderClusterManager: 2, ProviderManual: 1}

// Component is one detected component of a cluster as list_clusters reports
// it.
type Component struct {
	Status   Status   `json:"status"`
	Provider Provider `json:"provider,omitempty"`
	// Evidence names the objects the verdict rests on.
	Evidence []string `json:"evidence,omitempty"`
	// Reason says why the status is unknown.
	Reason string `json:"reason,omitempty"`
}

// Present reports whether the component was found.
func (c Component) Present() bool { return c.Status == StatusPresent }

// Unknown is the answer of a detection that could not read the cluster.
func Unknown(reason string) Component { return Component{Status: StatusUnknown, Reason: reason} }

// Target is the cluster a detection reads.
type Target struct {
	Cluster   string
	Namespace string
	// Installation reads the installation: the releases in `org-<org>`
	// that target the cluster.
	Installation dynamic.Interface
	// Reader reads the target cluster itself as the caller; nil when it is
	// not reachable, with Reason saying why.
	Reader dynamic.Interface
	Reason string
}

// Resources the detection reads on the target.
var (
	NodesGVR            = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
	AppGVR              = schema.GroupVersionResource{Group: "application.giantswarm.io", Version: "v1alpha1", Resource: "apps"}
	ClusterPolicyGVR    = schema.GroupVersionResource{Group: "nvidia.com", Version: "v1", Resource: "clusterpolicies"}
	InferenceServiceGVR = schema.GroupVersionResource{Group: "serving.kserve.io", Version: "v1beta1", Resource: "inferenceservices"}
	LLMISVCGVR          = schema.GroupVersionResource{Group: "serving.kserve.io", Version: "v1alpha1", Resource: "llminferenceservices"}
)

// Labels the detection reads.
const (
	// LabelHelmChart is Helm's chart label; the platform's own components
	// carry `agent-platform-<version>`.
	LabelHelmChart = "helm.sh/chart"
	// platformChartPrefix marks an object rendered by the platform's chart.
	platformChartPrefix = "agent-platform-"
	// LabelGPUPresent is the label GPU feature discovery sets on a node
	// with a GPU — an operator (or its device plugin) ran there.
	LabelGPUPresent = "nvidia.com/gpu.present"
	// GPUResource is the extended resource the device plugin advertises.
	GPUResource = "nvidia.com/gpu"
	// LabelServingConfig marks the model-serving discovery ConfigMap.
	LabelServingConfig = "agent-platform.giantswarm.io/model-serving-config"
	// SliceReleaseSuffix names cluster-manager's serving slice release.
	SliceReleaseSuffix = "-agent-platform"
)

// GPUOperator reports the GPU operator on the target, in this order of
// precedence: the platform's chart component (a HelmRelease `gpu-operator`
// rendered by an agent-platform release), any other HelmRelease or App of the
// gpu-operator chart (cluster-manager's own, or by hand), a ClusterPolicy,
// GPU labels or resources on nodes.
func GPUOperator(ctx context.Context, t Target) Component {
	var found []finding
	found = append(found, operatorReleases(ctx, t)...)
	if t.Reader == nil {
		if len(found) == 0 {
			return Unknown(t.Reason)
		}
		return verdict(found)
	}
	if apps, err := list(ctx, t.Reader, AppGVR, metav1.NamespaceAll, ""); err == nil {
		for i := range apps.Items {
			app := &apps.Items[i]
			if name, _, _ := unstructured.NestedString(app.Object, "spec", "name"); name == compose.OperatorChart {
				found = append(found, finding{ProviderManual, fmt.Sprintf("App %s/%s of chart %s", app.GetNamespace(), app.GetName(), name)})
			}
		}
	}
	if policies, err := list(ctx, t.Reader, ClusterPolicyGVR, "", ""); err == nil {
		for i := range policies.Items {
			found = append(found, finding{ProviderManual, "ClusterPolicy " + policies.Items[i].GetName()})
		}
	}
	nodes, err := Nodes(ctx, t.Reader)
	if err != nil && len(found) == 0 {
		return Unknown("nodes of " + t.Cluster + " not readable: " + err.Error())
	}
	for _, n := range nodes {
		if n.Labels[LabelGPUPresent] == "true" {
			found = append(found, finding{ProviderManual, fmt.Sprintf("node %s labelled %s=true", n.Name, LabelGPUPresent)})
		} else if n.gpuAllocatable {
			found = append(found, finding{ProviderManual, fmt.Sprintf("node %s advertises %s", n.Name, GPUResource)})
		}
	}
	return verdict(found)
}

// operatorReleases lists the HelmReleases of the gpu-operator chart: on the
// target itself (the platform's component, an install by hand) and, from
// the installation, the releases in the cluster's namespace that target it
// through its kubeconfig (cluster-manager's own among them).
func operatorReleases(ctx context.Context, t Target) []finding {
	var found []finding
	seen := map[string]bool{}
	add := func(hr *unstructured.Unstructured) {
		key := hr.GetNamespace() + "/" + hr.GetName()
		if seen[key] || !isOperatorRelease(hr) {
			return
		}
		seen[key] = true
		found = append(found, finding{releaseProvider(hr), "HelmRelease " + key})
	}
	if t.Reader != nil {
		if hrs, err := list(ctx, t.Reader, compose.HelmReleaseGVR, metav1.NamespaceAll, ""); err == nil {
			for i := range hrs.Items {
				add(&hrs.Items[i])
			}
		}
	}
	if t.Installation != nil && t.Installation != t.Reader {
		if hrs, err := list(ctx, t.Installation, compose.HelmReleaseGVR, t.Namespace, compose.LabelCluster+"="+t.Cluster); err == nil {
			for i := range hrs.Items {
				add(&hrs.Items[i])
			}
		}
	}
	return found
}

// isOperatorRelease recognises a HelmRelease of the gpu-operator chart by
// its name, chart label, chart spec or chart source.
func isOperatorRelease(hr *unstructured.Unstructured) bool {
	chart, _, _ := unstructured.NestedString(hr.Object, "spec", "chart", "spec", "chart")
	ref, _, _ := unstructured.NestedString(hr.Object, "spec", "chartRef", "name")
	return hr.GetName() == compose.OperatorChart ||
		strings.HasSuffix(hr.GetName(), compose.OperatorReleaseSuffix) ||
		hr.GetLabels()[compose.LabelChartName] == compose.OperatorChart ||
		chart == compose.OperatorChart || ref == compose.OperatorChart
}

// releaseProvider says who owns a HelmRelease: the platform's chart when
// an agent-platform release rendered it, cluster-manager when it carries
// its label, a human otherwise.
func releaseProvider(hr *unstructured.Unstructured) Provider {
	labels := hr.GetLabels()
	switch {
	case strings.HasPrefix(labels[LabelHelmChart], platformChartPrefix):
		return ProviderChart
	case labels[compose.LabelManagedBy] == compose.ManagedBy:
		return ProviderClusterManager
	default:
		return ProviderManual
	}
}

// Serving reports the serving layer on the target: the KServe and llmisvc
// APIs served, the model-serving discovery ConfigMap (rendered by the
// platform's chart — or, once cluster-manager's `<cluster>-agent-platform`
// slice release exists in the cluster's namespace, by that).
func Serving(ctx context.Context, t Target) Component {
	var found []finding
	provider := ProviderManual
	if t.Installation != nil {
		hr, err := t.Installation.Resource(compose.HelmReleaseGVR).Namespace(t.Namespace).Get(ctx, t.Cluster+SliceReleaseSuffix, metav1.GetOptions{})
		if err == nil && compose.OwnedBy(hr) {
			provider = ProviderClusterManager
			found = append(found, finding{provider, "HelmRelease " + t.Namespace + "/" + hr.GetName()})
		}
	}
	if t.Reader == nil {
		if len(found) == 0 {
			return Unknown(t.Reason)
		}
		return verdict(found)
	}
	for _, api := range []struct {
		gvr  schema.GroupVersionResource
		what string
	}{{InferenceServiceGVR, "KServe API"}, {LLMISVCGVR, "llmisvc API"}} {
		if _, err := list(ctx, t.Reader, api.gvr, metav1.NamespaceAll, ""); err == nil {
			found = append(found, finding{provider, api.what + " " + api.gvr.GroupVersion().String() + " served"})
		}
	}
	if cms, err := list(ctx, t.Reader, compose.ConfigMapGVR, metav1.NamespaceAll, LabelServingConfig+"=true"); err == nil {
		for i := range cms.Items {
			cm := &cms.Items[i]
			p := provider
			if p == ProviderManual {
				p = ProviderChart
			}
			found = append(found, finding{p, fmt.Sprintf("discovery ConfigMap %s/%s", cm.GetNamespace(), cm.GetName())})
		}
	}
	return verdict(found)
}

// Node is one node of the target as the detection and the operator's
// configuration table read it.
type Node struct {
	compose.Node
	gpuAllocatable bool
}

// Nodes reads the target's nodes: name, OS image, labels and whether a GPU
// resource is allocatable.
func Nodes(ctx context.Context, reader dynamic.Interface) ([]Node, error) {
	nodes, err := list(ctx, reader, NodesGVR, "", "")
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(nodes.Items))
	for i := range nodes.Items {
		n := &nodes.Items[i]
		osImage, _, _ := unstructured.NestedString(n.Object, "status", "nodeInfo", "osImage")
		gpus, _, _ := unstructured.NestedString(n.Object, "status", "allocatable", GPUResource)
		out = append(out, Node{Node: compose.Node{Name: n.GetName(), OSImage: osImage, Labels: n.GetLabels()}, gpuAllocatable: gpus != "" && gpus != "0"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ComposeNodes strips the nodes to what the configuration table reads.
func ComposeNodes(nodes []Node) []compose.Node {
	out := make([]compose.Node, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Node)
	}
	return out
}

type finding struct {
	provider Provider
	evidence string
}

// verdict is the component the findings add up to: present with the
// highest-ranked provider, absent when there are none.
func verdict(found []finding) Component {
	if len(found) == 0 {
		return Component{Status: StatusAbsent}
	}
	c := Component{Status: StatusPresent, Provider: found[0].provider}
	for _, f := range found {
		if precedence[f.provider] > precedence[c.Provider] {
			c.Provider = f.provider
		}
		c.Evidence = append(c.Evidence, f.evidence)
	}
	sort.Strings(c.Evidence)
	return c
}

// list lists a resource; an API the target does not serve is an error the
// callers read as absence.
func list(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, namespace, selector string) (*unstructured.UnstructuredList, error) {
	items, err := dyn.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil, fmt.Errorf("%s not served", gvr.GroupResource())
		}
		return nil, err
	}
	return items, nil
}
