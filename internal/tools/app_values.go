package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// app-operator's values layers: spec.config merges at priority 50,
// spec.userConfig at 100, an extraConfigs entry at its own priority (25 when
// unset), before the layer of the same priority.
const (
	appConfigPriority             = 50
	appUserConfigPriority         = 100
	appExtraConfigPriorityDefault = 25
)

// App values kinds, as spec.extraConfigs names them.
const (
	appValuesConfigMap = "configMap"
	appValuesSecret    = "secret"
)

// appValuesLayer is one values source an App CR names.
type appValuesLayer struct {
	field     string // where the App names it: spec.config, spec.extraConfigs[1], spec.userConfig
	kind      string // configMap or secret
	namespace string
	name      string
	priority  int
	extra     bool // an extraConfigs entry: merged before spec.config or spec.userConfig of its priority
}

func (l appValuesLayer) String() string {
	return fmt.Sprintf("%s %s %s/%s", l.field, l.kind, l.namespace, l.name)
}

// appValues merges an App CR's values ConfigMaps the way app-operator does,
// lowest precedence first: the extraConfigs up to priority 50, spec.config,
// the extraConfigs up to 100, spec.userConfig, the extraConfigs above;
// entries of one priority in list order. Neither the catalog's values nor
// Secrets are read: the snapshot needs no catalog default, and a Secret's
// content is credentials. Every ConfigMap the App names must exist, as
// app-operator requires: a snapshot of partial values would be wrong.
func appValues(ctx context.Context, dyn dynamic.Interface, app *unstructured.Unstructured) (map[string]any, error) {
	ref := app.GetNamespace() + "/" + app.GetName()
	layers := appValuesLayers(app)
	if len(layers) == 0 {
		return nil, fmt.Errorf("the App %s names no values (spec.config, spec.extraConfigs, spec.userConfig): the pool's snapshot comes from the cluster's values", ref)
	}
	merged := map[string]any{}
	var listed, missing []string
	read := 0
	for _, l := range layers {
		listed = append(listed, l.String())
		if l.kind != appValuesConfigMap {
			continue
		}
		vals, err := appConfigMapValues(ctx, dyn, l.namespace, l.name)
		if apierrors.IsNotFound(err) {
			missing = append(missing, l.String())
			continue
		}
		if err != nil {
			return nil, err
		}
		merge(merged, vals)
		read++
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("the App %s names values ConfigMaps that do not exist: %s (it names %s); app-operator cannot merge the cluster's values either", ref, strings.Join(missing, ", "), strings.Join(listed, ", "))
	}
	if read == 0 {
		return nil, fmt.Errorf("the App %s keeps its values only in Secrets (%s): the pool's snapshot is read from values ConfigMaps, never from credentials", ref, strings.Join(listed, ", "))
	}
	return merged, nil
}

// appConfigMapValues is the values document of an App's ConfigMap: its one
// key, whatever the key's name, as app-operator reads it (the GitOps
// layout's installation values are under values.yaml, a cluster's config
// under values). A ConfigMap with several keys is refused: app-operator
// refuses it as spec.config or spec.userConfig and, as an extraConfigs
// entry, reads whichever key it meets first.
func appConfigMapValues(ctx context.Context, dyn dynamic.Interface, ns, name string) (map[string]any, error) {
	cm, err := dyn.Resource(ConfigMapGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get values ConfigMap %s/%s: %w", ns, name, err)
	}
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	switch len(keys) {
	case 0:
		return map[string]any{}, nil
	case 1:
		return parseValues(ns, name, keys[0], data[keys[0]])
	}
	return nil, fmt.Errorf("values ConfigMap %s/%s carries %d keys (%s): app-operator reads an App's values from a ConfigMap's one key", ns, name, len(keys), strings.Join(keys, ", "))
}

// appValuesLayers is every values source the App names, in app-operator's
// merge order.
func appValuesLayers(app *unstructured.Unstructured) []appValuesLayer {
	var layers []appValuesLayer
	for _, top := range []struct {
		field    string
		priority int
	}{{"config", appConfigPriority}, {"userConfig", appUserConfigPriority}} {
		for _, kind := range []string{appValuesConfigMap, appValuesSecret} {
			name, _, _ := unstructured.NestedString(app.Object, "spec", top.field, kind, "name")
			if name == "" {
				continue
			}
			namespace, _, _ := unstructured.NestedString(app.Object, "spec", top.field, kind, "namespace")
			layers = append(layers, appValuesLayer{
				field:     "spec." + top.field,
				kind:      kind,
				namespace: firstNonEmpty(namespace, app.GetNamespace()),
				name:      name,
				priority:  top.priority,
			})
		}
	}
	extras, _, _ := unstructured.NestedSlice(app.Object, "spec", "extraConfigs")
	for i, e := range extras {
		entry, _ := e.(map[string]any)
		name, _ := entry["name"].(string)
		if name == "" {
			continue
		}
		kind, _ := entry["kind"].(string)
		namespace, _ := entry["namespace"].(string)
		priority := appExtraConfigPriorityDefault
		if p := intValue(entry["priority"]); p != 0 {
			priority = p
		}
		layers = append(layers, appValuesLayer{
			field:     fmt.Sprintf("spec.extraConfigs[%d]", i),
			kind:      firstNonEmpty(kind, appValuesConfigMap),
			namespace: firstNonEmpty(namespace, app.GetNamespace()),
			name:      name,
			priority:  priority,
			extra:     true,
		})
	}
	sort.SliceStable(layers, func(i, j int) bool {
		if layers[i].priority != layers[j].priority {
			return layers[i].priority < layers[j].priority
		}
		return layers[i].extra && !layers[j].extra
	})
	return layers
}

// intValue is an unstructured number: int64 from the apiserver's JSON,
// float64 or int from a decoded YAML document.
func intValue(v any) int {
	switch n := v.(type) {
	case int64:
		return int(n)
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}
