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
}

// KServeBackend renders the kserve backend document into model-manager's
// namespace, labelled with the cluster it registers so the last pool's
// deletion finds it.
func KServeBackend(namespace string, t BackendTarget) (*unstructured.Unstructured, error) {
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
	doc := map[string]any{
		"apiVersion": BackendAPIVersion,
		"kind":       BackendKind,
		"metadata":   map[string]any{"name": BackendKindKServe},
		"spec": map[string]any{
			"kind":   BackendKindKServe,
			"source": ManagedBy,
			"kserve": map[string]any{"target": target},
		},
	}
	raw, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode backend document: %w", err)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      BackendConfigMapName,
			"namespace": namespace,
			"labels": map[string]any{
				LabelBackend:       "true",
				LabelBackendSource: ManagedBy,
				LabelManagedBy:     ManagedBy,
				LabelCluster:       t.Cluster,
			},
		},
		"data": map[string]any{BackendDocumentKey: string(raw)},
	}}, nil
}
