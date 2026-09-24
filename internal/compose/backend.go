package compose

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// The backend document of model-manager's runtime-registration contract
// (giantswarm/model-manager docs/backends.md): a ConfigMap in model-manager's
// namespace, found by label, carrying a ModelBackend document.
const (
	// BackendKindKServe is the backend cluster-manager registers: the
	// serving cluster's KServe.
	BackendKindKServe = "kserve"
	// BackendConfigMapName names the one document per kind.
	BackendConfigMapName = "model-backend-" + BackendKindKServe
	// BackendDocumentKey is the ConfigMap key holding the document.
	BackendDocumentKey = "backend.yaml"
	// LabelBackend is the selector model-manager watches.
	LabelBackend = "agent-platform.giantswarm.io/model-backend"
	// LabelBackendSource repeats spec.source on the ConfigMap.
	LabelBackendSource = "agent-platform.giantswarm.io/model-backend-source"
	// BackendAPIVersion and BackendKind are the document's type.
	BackendAPIVersion = "agent-platform.giantswarm.io/v1alpha1"
	BackendKind       = "ModelBackend"
	// BackendTargetLocal is the target of the cluster model-manager itself
	// runs on: the installation's own cluster needs no apiserver or CA.
	BackendTargetLocal = "local"
)

// ConfigMapGVR is the backend document's resource.
var ConfigMapGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

// BackendTarget is the serving cluster as the kserve backend reaches it:
// the apiserver and its CA from the cluster's kubeconfig Secret — never a
// credential; every call the backend makes presents the caller's own token.
type BackendTarget struct {
	Cluster      string
	Organization string
	// OwnCluster marks the installation's own cluster: the document names
	// the `local` target and carries no apiserver or CA.
	OwnCluster bool
	APIServer  string
	// CABundle is the apiserver's CA, PEM.
	CABundle string
	// ServingNamespace is where the InferenceServices go on the target.
	ServingNamespace string
	// DiscoveryNamespace is where the slice release renders the
	// model-serving discovery ConfigMap (agent-platform-model-serving) on
	// the target: the slice's release namespace, the cluster's org
	// namespace — not the serving namespace model-manager would assume.
	// Empty leaves the document without a discovery block.
	DiscoveryNamespace string
}

// documentShape is one size as model-manager's backend document declares it
// (spec.kserve.gpuPool.instances[], spec.kserve.gpuPools[<pool>].instances[]):
// the node as AWS lists it and what it leaves a predictor. model-manager reads
// the document strictly — a key it does not declare fails the parse and with
// it the fit check — so the shape's other fields (the price, the instance
// store) are the answer's and are never written here.
type documentShape struct {
	InstanceType    string  `json:"instanceType"`
	Size            string  `json:"size"`
	VCPU            int     `json:"vcpu"`
	MemoryGiB       int     `json:"memoryGiB"`
	GPUs            int     `json:"gpus"`
	GPUMemoryGiB    int     `json:"gpuMemoryGiB"`
	UsableVCPU      float64 `json:"usableVcpu"`
	UsableMemoryGiB float64 `json:"usableMemoryGiB"`
}

func documentShapes(shapes []InstanceShape) []documentShape {
	out := make([]documentShape, 0, len(shapes))
	for _, s := range shapes {
		out = append(out, documentShape{s.InstanceType, s.Size, s.VCPU, s.MemoryGiB, s.GPUs, s.GPUMemoryGiB, s.UsableVCPU, s.UsableMemoryGiB})
	}
	return out
}

// KServeBackend renders the kserve backend document into model-manager's
// namespace, labelled with the cluster it registers so the last pool's
// deletion finds it. pools are the instance shapes of the cluster's GPU pools
// by release name (the value of the node label giantswarm.io/machine-pool):
// what model-manager's fit check judges a model against while a pool has no
// node, and load_model refuses what no size hosts (model-manager 0.23.7,
// giantswarm/model-manager#97). One pool is written as
// spec.kserve.gpuPool.instances, the pool the predictors are pinned to;
// several as spec.kserve.gpuPools, each pool's instances under its release
// name, so model-manager judges — and pins — a model against the pool it
// picks, a pool with no node yet included (giantswarm/cluster-manager#89,
// giantswarm/model-manager#152); none writes neither block, and the
// discovery ConfigMap's taint and node selector stand either way.
func KServeBackend(namespace string, t BackendTarget, pools map[string][]InstanceShape) (*unstructured.Unstructured, error) {
	target := map[string]any{
		"cluster":          t.Cluster,
		"organization":     t.Organization,
		"apiServer":        t.APIServer,
		"caBundle":         t.CABundle,
		"servingNamespace": t.ServingNamespace,
	}
	if t.OwnCluster {
		target["cluster"] = BackendTargetLocal
		delete(target, "apiServer")
		delete(target, "caBundle")
	}
	kserve := map[string]any{"target": target}
	if t.DiscoveryNamespace != "" {
		kserve["discovery"] = map[string]any{"namespace": t.DiscoveryNamespace}
	}
	switch {
	case len(pools) == 1:
		for _, instances := range pools {
			kserve["gpuPool"] = map[string]any{"instances": documentShapes(instances)}
		}
	case len(pools) > 1:
		keyed := make(map[string]any, len(pools))
		for release, instances := range pools {
			keyed[release] = map[string]any{"instances": documentShapes(instances)}
		}
		kserve["gpuPools"] = keyed
	}
	doc := map[string]any{
		"apiVersion": BackendAPIVersion,
		"kind":       BackendKind,
		"metadata":   map[string]any{"name": BackendKindKServe},
		"spec": map[string]any{
			"kind":   BackendKindKServe,
			"source": ManagedBy,
			"kserve": kserve,
		},
	}
	raw, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode backend document: %w", err)
	}
	metadata := map[string]any{
		"name":      BackendConfigMapName,
		"namespace": namespace,
		"labels": map[string]any{
			LabelBackend:       "true",
			LabelBackendSource: ManagedBy,
			LabelManagedBy:     ManagedBy,
			LabelCluster:       t.Cluster,
		},
	}
	return object(ConfigMapGVR, "ConfigMap", metadata, map[string]any{"data": map[string]any{BackendDocumentKey: string(raw)}}), nil
}
