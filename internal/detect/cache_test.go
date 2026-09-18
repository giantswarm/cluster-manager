package detect

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func claim(phase, volume string) *unstructured.Unstructured {
	pvc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": "hf-cache", "namespace": "model-serving"},
		"spec":     map[string]any{},
		"status":   map[string]any{"phase": phase},
	}}
	if volume != "" {
		pvc.Object["spec"].(map[string]any)["volumeName"] = volume
	}
	return pvc
}

func volume(name string, affinity map[string]any) *unstructured.Unstructured {
	pv := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolume",
		"metadata": map[string]any{"name": name},
		"spec":     map[string]any{},
	}}
	if affinity != nil {
		pv.Object["spec"].(map[string]any)["nodeAffinity"] = affinity
	}
	return pv
}

func zoneAffinity(key, zone string) map[string]any {
	return map[string]any{"required": map[string]any{"nodeSelectorTerms": []any{
		map[string]any{"matchExpressions": []any{map[string]any{"key": key, "operator": "In", "values": []any{zone}}}},
	}}}
}

// TestCacheClaimState (giantswarm/cluster-manager#59): the claim's zone is
// its bound volume's node affinity, by the topology label or the beta label
// older provisioners wrote; a claim not Bound has none yet; a volume that
// names no zone pins nothing; a claim or volume that cannot be read carries
// the error, never a guessed zone; no claim is nil.
func TestCacheClaimState(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		objs []runtime.Object
		want *CacheClaim
	}{
		{"none", nil, nil},
		{"pending", []runtime.Object{claim("Pending", "")}, &CacheClaim{Namespace: "model-serving", Name: "hf-cache", Phase: "Pending"}},
		{"bound", []runtime.Object{claim("Bound", "pvc-1"), volume("pvc-1", zoneAffinity(LabelZone, "eu-central-1b"))},
			&CacheClaim{Namespace: "model-serving", Name: "hf-cache", Phase: "Bound", Volume: "pvc-1", Zone: "eu-central-1b"}},
		{"bound-legacy-label", []runtime.Object{claim("Bound", "pvc-1"), volume("pvc-1", zoneAffinity(LabelZoneLegacy, "eu-central-1a"))},
			&CacheClaim{Namespace: "model-serving", Name: "hf-cache", Phase: "Bound", Volume: "pvc-1", Zone: "eu-central-1a"}},
		{"bound-no-zone", []runtime.Object{claim("Bound", "pvc-1"), volume("pvc-1", nil)},
			&CacheClaim{Namespace: "model-serving", Name: "hf-cache", Phase: "Bound", Volume: "pvc-1"}},
		{"volume-gone", []runtime.Object{claim("Bound", "pvc-1")},
			&CacheClaim{Namespace: "model-serving", Name: "hf-cache", Phase: "Bound", Volume: "pvc-1", Error: `get PersistentVolume pvc-1 of claim model-serving/hf-cache: persistentvolumes "pvc-1" not found`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), tc.objs...)
			assert.Equal(t, tc.want, CacheClaimState(ctx, dyn, "model-serving", "hf-cache"))
		})
	}

	t.Run("claim-forbidden", func(t *testing.T) {
		dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), claim("Bound", "pvc-1"))
		dyn.PrependReactor("get", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumeclaims"}, "hf-cache", errors.New("User \"alice\" cannot get resource \"persistentvolumeclaims\" in API group \"\" in the namespace \"model-serving\""))
		})
		got := CacheClaimState(ctx, dyn, "model-serving", "hf-cache")
		require.NotNil(t, got, "a claim that cannot be read is not the same as none")
		assert.Equal(t, "", got.Phase)
		assert.Contains(t, got.Error, `get PersistentVolumeClaim model-serving/hf-cache: persistentvolumeclaims "hf-cache" is forbidden`)
	})
}
