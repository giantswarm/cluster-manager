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
	"time"

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
	// Evidence names the objects the verdict rests on — and, on an absent
	// serving layer, the KServe APIs still served without a controller.
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
	DeploymentGVR       = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	AppGVR              = schema.GroupVersionResource{Group: "application.giantswarm.io", Version: "v1alpha1", Resource: "apps"}
	ClusterPolicyGVR    = schema.GroupVersionResource{Group: "nvidia.com", Version: "v1", Resource: "clusterpolicies"}
	InferenceServiceGVR = schema.GroupVersionResource{Group: "serving.kserve.io", Version: "v1beta1", Resource: "inferenceservices"}
	LLMISVCGVR          = schema.GroupVersionResource{Group: "serving.kserve.io", Version: "v1alpha1", Resource: "llminferenceservices"}
	// CRDGVR is the CustomResourceDefinitions, read for the storage version
	// of an API whose conversion webhook may be gone (ConfigsGVR).
	CRDGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	// LLMISVCConfigResource is the well-known LLMInferenceServiceConfigs an
	// LLMInferenceService composes from (the kserve-runtime-configs chart
	// installs ten into the slice's release namespace). Deliberately without
	// a version: the configs are read and removed through the CRD's storage
	// version of the moment (ConfigsGVR), the one version that needs no
	// conversion — the CRD converts through the llmisvc controller's
	// webhook, gone with the controller's release while the teardown removes
	// the configs (giantswarm/cluster-manager#39).
	LLMISVCConfigResource = schema.GroupResource{Group: "serving.kserve.io", Resource: "llminferenceserviceconfigs"}
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
	// LabelServingPreset marks a published serving preset (one ConfigMap per
	// preset, the document under PresetDocumentKey); LabelPreset carries its
	// name.
	LabelServingPreset = "agent-platform.giantswarm.io/serving-preset"
	LabelPreset        = "agent-platform.giantswarm.io/preset"
	// PresetDocumentKey is the ConfigMap key holding the ServingPreset.
	PresetDocumentKey = "preset.yaml"
	// LabelControlPlane is KServe's label on its controller Deployments:
	// `control-plane: kserve-controller-manager` on the InferenceService
	// controller, `llmisvc-controller-manager` on the llm-d one.
	LabelControlPlane = "control-plane"
	// AnnotationModel is model-manager's annotation on a serving object it
	// composed: the id of the model it serves.
	AnnotationModel = "model-manager.giantswarm.io/model"
	// hfScheme prefixes a Hugging Face repository in a serving object's
	// storage uri.
	hfScheme = "hf://"
	// Flux's labels on every object a HelmRelease installed, naming it.
	LabelFluxReleaseName      = "helm.toolkit.fluxcd.io/name"
	LabelFluxReleaseNamespace = "helm.toolkit.fluxcd.io/namespace"
	// LLMISVCController is the `control-plane` label of the llm-d controller,
	// the one that puts LLMISVCConfigFinalizer on every
	// LLMInferenceServiceConfig and clears it once no LLMInferenceService
	// references the config any more.
	LLMISVCController = "llmisvc-controller-manager"
	// LLMISVCConfigFinalizer is that finalizer. With the controller gone
	// nothing clears it: a deleted config sits terminating for as long as
	// the CRD exists, and a slice installed next adopts and loses it
	// (giantswarm/cluster-manager#28).
	LLMISVCConfigFinalizer = "serving.kserve.io/llmisvcconfig-finalizer"
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

// KServeControllers are the Deployments of the KServe control plane, by
// their `control-plane` label: the InferenceService controller and the
// llm-d LLMInferenceService controller (the kserve-resources and
// kserve-llmisvc-resources charts). One of them on the cluster is what makes
// serving present; the CRDs alone do not — Helm never removes CRDs, so a
// serving layer that went leaves them behind, and CRDs serve no model.
var KServeControllers = []string{"kserve-controller-manager", LLMISVCController}

// KServeCharts are the charts of the KServe controllers, as the platform's
// release or a hand install names them in a HelmRelease.
var KServeCharts = []string{"kserve-resources", "kserve-llmisvc-resources"}

// Serving reports the serving layer on the target: present with a KServe
// controller — one of KServeControllers running, a HelmRelease of
// KServeCharts, the model-serving discovery ConfigMap the platform's chart
// renders — or with cluster-manager's own `<cluster>-agent-platform` slice
// release in the cluster's namespace, whose children they then are. The
// KServe and llmisvc APIs served are evidence, never the verdict: on their
// own they are the CRDs a serving layer left behind, and the answer is
// absent with a note.
func Serving(ctx context.Context, t Target) Component {
	var found []finding
	var children []string
	provider := ProviderManual
	if t.Installation != nil {
		hr, err := t.Installation.Resource(compose.HelmReleaseGVR).Namespace(t.Namespace).Get(ctx, compose.SliceReleaseName(t.Cluster), metav1.GetOptions{})
		if err == nil && compose.OwnedBy(hr) {
			provider = ProviderClusterManager
			found = append(found, finding{provider, "HelmRelease " + t.Namespace + "/" + hr.GetName()})
			children = sliceChildrenNotReady(ctx, t.Installation, t.Namespace, hr.GetName())
		}
	}
	if t.Reader == nil {
		if len(found) == 0 {
			return Unknown(t.Reason)
		}
		c := verdict(found)
		c.Evidence = append(c.Evidence, children...)
		sort.Strings(c.Evidence)
		return c
	}
	found = append(found, servingControllers(ctx, t.Reader, provider)...)
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
	apis := servedAPIs(ctx, t.Reader)
	stranded := strandedEvidence(ctx, t.Reader, t.Namespace)
	if len(found) == 0 {
		c := Component{Status: StatusAbsent}
		for _, api := range apis {
			c.Evidence = append(c.Evidence, api+" (CRDs only, no controller)")
		}
		c.Evidence = append(c.Evidence, stranded...)
		return c
	}
	c := verdict(found)
	c.Evidence = append(c.Evidence, apis...)
	c.Evidence = append(c.Evidence, stranded...)
	c.Evidence = append(c.Evidence, children...)
	sort.Strings(c.Evidence)
	return c
}

// sliceChildrenNotReady names the children of the slice release that are not
// Ready — the component HelmReleases the meta chart renders into the slice's
// namespace on the installation, labelled by Flux as the release's — with
// their Ready condition's reason and message, the child's own account of
// what failed. The meta release reports Ready whether or not a child
// installed: on gazelle the connectivity child failed its render and the
// slice served no models Gateway while serving read present
// (giantswarm/cluster-manager#30). Nothing when every child is Ready.
func sliceChildrenNotReady(ctx context.Context, installation dynamic.Interface, namespace, release string) []string {
	selector := fmt.Sprintf("%s=%s,%s=%s", LabelFluxReleaseName, release, LabelFluxReleaseNamespace, namespace)
	hrs, err := list(ctx, installation, compose.HelmReleaseGVR, namespace, selector)
	if err != nil {
		return nil
	}
	var out []string
	for i := range hrs.Items {
		hr := &hrs.Items[i]
		ready, found := ReadyCondition(hr)
		if found && ready.Status == "True" {
			continue
		}
		detail := "no Ready condition yet"
		if found {
			detail = "Ready=" + ready.Status
			if ready.Reason != "" {
				detail += " [" + ready.Reason + "]"
			}
			if message := strings.Join(strings.Fields(ready.Message), " "); message != "" {
				detail += " " + message
			}
		}
		if deleted := hr.GetDeletionTimestamp(); deleted != nil {
			// A child whose uninstall failed stays, deleted, until Flux's
			// retry succeeds (giantswarm/cluster-manager#37).
			detail = "deleted since " + deleted.UTC().Format(time.RFC3339) + ", " + detail
		}
		out = append(out, fmt.Sprintf("HelmRelease %s/%s not Ready (%s)", hr.GetNamespace(), hr.GetName(), detail))
	}
	return out
}

// Condition is one entry of an object's status.conditions.
type Condition struct {
	Status  string
	Reason  string
	Message string
}

// ReadyCondition reads the Ready condition of an object's status.conditions;
// found is false when it has none.
func ReadyCondition(obj *unstructured.Unstructured) (ready Condition, found bool) {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != "Ready" {
			continue
		}
		ready.Status, _ = m["status"].(string)
		ready.Reason, _ = m["reason"].(string)
		ready.Message, _ = m["message"].(string)
		return ready, true
	}
	return Condition{}, false
}

// strandedEvidence names the LLMInferenceServiceConfigs terminating in the
// slice's release namespace on the target: left by a serving layer that went
// with their finalizer uncleared, they break the next slice installed there
// (its release adopts and loses them) until create_node_pool or
// enable_model_serving heals them. Nothing when there are none.
func strandedEvidence(ctx context.Context, reader dynamic.Interface, namespace string) []string {
	configs, err := Configs(ctx, reader, namespace)
	if err != nil {
		return nil
	}
	terminating := Terminating(configs)
	if len(terminating) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("%d LLMInferenceServiceConfig(s) terminating in %s with %s uncleared (stranded; healed by the next create_node_pool or enable_model_serving): %s",
		len(terminating), namespace, LLMISVCConfigFinalizer, strings.Join(Names(terminating), ", "))}
}

// ConfigsGVR is the LLMInferenceServiceConfigs API of the target in its
// CRD's storage version, read now: the one version a request needs no
// conversion for, so the configs can be listed and removed once the CRD's
// conversion webhook — the llmisvc controller's — is gone. On gazelle
// (2026-09-17 08:29Z) every delete through v1alpha1 of a config stored as
// v1alpha2 failed with `conversion webhook … service
// "llmisvc-webhook-server-service" not found` once the controller's release
// was deleted, while the same through v1alpha2 succeeded
// (giantswarm/cluster-manager#39). served is false where the CRD is not
// there; a CRD that cannot be read is an error, never a guessed version.
func ConfigsGVR(ctx context.Context, reader dynamic.Interface) (gvr schema.GroupVersionResource, served bool, err error) {
	crd, err := reader.Resource(CRDGVR).Get(ctx, LLMISVCConfigResource.String(), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return schema.GroupVersionResource{}, false, nil
		}
		return schema.GroupVersionResource{}, false, fmt.Errorf("read CRD %s: %w", LLMISVCConfigResource, err)
	}
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	for _, v := range versions {
		version, _ := v.(map[string]any)
		if storage, _ := version["storage"].(bool); storage {
			name, _ := version["name"].(string)
			return LLMISVCConfigResource.WithVersion(name), true, nil
		}
	}
	return schema.GroupVersionResource{}, false, fmt.Errorf("CRD %s names no storage version", LLMISVCConfigResource)
}

// Configs lists the LLMInferenceServiceConfigs of a namespace on the target
// through the CRD's storage version (ConfigsGVR), sorted by name; an API the
// target does not serve has none. Each carries the version it was read in,
// the one to remove it through.
func Configs(ctx context.Context, reader dynamic.Interface, namespace string) ([]unstructured.Unstructured, error) {
	gvr, served, err := ConfigsGVR(ctx, reader)
	if err != nil || !served {
		return nil, err
	}
	items, err := reader.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list %s in %s: %w", gvr.GroupResource(), namespace, err)
	}
	sort.Slice(items.Items, func(i, j int) bool { return items.Items[i].GetName() < items.Items[j].GetName() })
	return items.Items, nil
}

// Terminating is the subset of objs with a deletionTimestamp.
func Terminating(objs []unstructured.Unstructured) []unstructured.Unstructured {
	var out []unstructured.Unstructured
	for i := range objs {
		if objs[i].GetDeletionTimestamp() != nil {
			out = append(out, objs[i])
		}
	}
	return out
}

// Names lists objects by name, in order.
func Names(objs []unstructured.Unstructured) []string {
	out := make([]string, 0, len(objs))
	for i := range objs {
		out = append(out, objs[i].GetName())
	}
	return out
}

// LLMISVCControllerRuns reports whether the llm-d controller has a ready
// replica on the target — the only thing that clears LLMISVCConfigFinalizer.
// Unreadable Deployments count as no controller: what cannot be seen cannot
// be waited for.
func LLMISVCControllerRuns(ctx context.Context, reader dynamic.Interface) bool {
	deps, err := list(ctx, reader, DeploymentGVR, metav1.NamespaceAll, LabelControlPlane+"="+LLMISVCController)
	if err != nil {
		return false
	}
	for i := range deps.Items {
		if NestedInt(&deps.Items[i], "status", "readyReplicas") > 0 {
			return true
		}
	}
	return false
}

// servedAPIs names the KServe APIs the target serves.
func servedAPIs(ctx context.Context, reader dynamic.Interface) []string {
	var out []string
	for _, api := range []struct {
		gvr  schema.GroupVersionResource
		what string
	}{{InferenceServiceGVR, "KServe API"}, {LLMISVCGVR, "llmisvc API"}} {
		if _, err := list(ctx, reader, api.gvr, metav1.NamespaceAll, ""); err == nil {
			out = append(out, api.what+" "+api.gvr.GroupVersion().String()+" served")
		}
	}
	return out
}

// servingControllers finds the KServe controllers on the target: the
// HelmReleases of their charts and the controller Deployments. With
// cluster-manager's slice release on the cluster (owner) they are its
// children; otherwise a release rendered by the platform's chart is the
// chart's, a Deployment is its release's (Flux labels every object it
// installs with the release), and anything else is a hand install.
func servingControllers(ctx context.Context, reader dynamic.Interface, owner Provider) []finding {
	var found []finding
	releases := map[string]Provider{}
	if hrs, err := list(ctx, reader, compose.HelmReleaseGVR, metav1.NamespaceAll, ""); err == nil {
		for i := range hrs.Items {
			hr := &hrs.Items[i]
			if !isKServeRelease(hr) {
				continue
			}
			p := owner
			if p != ProviderClusterManager {
				p = releaseProvider(hr)
			}
			releases[hr.GetNamespace()+"/"+hr.GetName()] = p
			found = append(found, finding{p, fmt.Sprintf("HelmRelease %s/%s", hr.GetNamespace(), hr.GetName())})
		}
	}
	selector := LabelControlPlane + " in (" + strings.Join(KServeControllers, ",") + ")"
	if deps, err := list(ctx, reader, DeploymentGVR, metav1.NamespaceAll, selector); err == nil {
		for i := range deps.Items {
			d := &deps.Items[i]
			p := owner
			if p != ProviderClusterManager {
				labels := d.GetLabels()
				if rp, ok := releases[labels[LabelFluxReleaseNamespace]+"/"+labels[LabelFluxReleaseName]]; ok {
					p = rp
				}
			}
			ready, replicas := NestedInt(d, "status", "readyReplicas"), NestedInt(d, "spec", "replicas")
			found = append(found, finding{p, fmt.Sprintf("Deployment %s/%s (%d/%d ready)", d.GetNamespace(), d.GetName(), ready, replicas)})
		}
	}
	return found
}

// isKServeRelease recognises a HelmRelease of a KServe controller chart by
// its name, chart label, chart spec or chart source.
func isKServeRelease(hr *unstructured.Unstructured) bool {
	chart, _, _ := unstructured.NestedString(hr.Object, "spec", "chart", "spec", "chart")
	ref, _, _ := unstructured.NestedString(hr.Object, "spec", "chartRef", "name")
	for _, c := range KServeCharts {
		if hr.GetName() == c || hr.GetLabels()[compose.LabelChartName] == c || chart == c || ref == c {
			return true
		}
	}
	return false
}

// ServedModel is one model served on a target: the serving object and the
// model it serves, as model-manager names it (its unload_model takes the
// object's name or the model's id).
type ServedModel struct {
	Kind      string
	Namespace string
	Name      string
	// Model is the served model's id: model-manager's annotation on the
	// object, else the LLMInferenceService's spec.model.name or the
	// repository of its hf:// uri, else the repository of the
	// InferenceService's hf:// storageUri; empty when the object names none.
	Model string
}

// String names the object and, when known, its model:
// `LLMInferenceService model-serving/qwen3-4b-instruct (Qwen/Qwen3-4B-Instruct-2507)`.
func (m ServedModel) String() string {
	s := m.Kind + " " + m.Namespace + "/" + m.Name
	if m.Model != "" {
		s += " (" + m.Model + ")"
	}
	return s
}

// ServedModels lists the models served on the target — every
// LLMInferenceService and InferenceService whatever its readiness: a
// predictor still Pending holds its GPU node as a Ready one does — read as
// the caller; an API the target does not serve counts as no model of that
// kind.
func ServedModels(ctx context.Context, reader dynamic.Interface) ([]ServedModel, error) {
	var out []ServedModel
	for _, gvr := range []schema.GroupVersionResource{LLMISVCGVR, InferenceServiceGVR} {
		items, err := reader.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
				continue
			}
			return nil, fmt.Errorf("list %s: %w", gvr.GroupResource(), err)
		}
		for i := range items.Items {
			obj := &items.Items[i]
			out = append(out, ServedModel{Kind: obj.GetKind(), Namespace: obj.GetNamespace(), Name: obj.GetName(), Model: modelOf(obj)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

// modelOf is the model a serving object serves: model-manager's annotation,
// else the LLMInferenceService's spec.model.name, else the repository of an
// hf:// uri (spec.model.uri, or the InferenceService's
// spec.predictor.model.storageUri).
func modelOf(obj *unstructured.Unstructured) string {
	if m := obj.GetAnnotations()[AnnotationModel]; m != "" {
		return m
	}
	if m, _, _ := unstructured.NestedString(obj.Object, "spec", "model", "name"); m != "" {
		return m
	}
	for _, path := range [][]string{{"spec", "model", "uri"}, {"spec", "predictor", "model", "storageUri"}} {
		if uri, _, _ := unstructured.NestedString(obj.Object, path...); strings.HasPrefix(uri, hfScheme) {
			return strings.TrimPrefix(uri, hfScheme)
		}
	}
	return ""
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

// NestedInt reads an integer field of an object; the API machinery's decoder
// carries numbers as int64, a JSON-decoded object as float64. Missing is 0.
func NestedInt(obj *unstructured.Unstructured, path ...string) int64 {
	v, found, err := unstructured.NestedFieldNoCopy(obj.Object, path...)
	if err != nil || !found {
		return 0
	}
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
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
