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

func claim(name, phase, volume string) *unstructured.Unstructured {
	pvc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": name, "namespace": "model-serving"},
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

// claimListKinds registers the claims' list kind with the fake client.
var claimListKinds = map[schema.GroupVersionResource]string{PersistentVolumeClaimGVR: "PersistentVolumeClaimList"}

func zoneAffinity(key, zone string) map[string]any {
	return map[string]any{"required": map[string]any{"nodeSelectorTerms": []any{
		map[string]any{"matchExpressions": []any{map[string]any{"key": key, "operator": "In", "values": []any{zone}}}},
	}}}
}

// TestCacheClaims (giantswarm/cluster-manager#59, #71): the claims of the
// base name and of the zones (`<base>-<zone>`) are listed sorted by name,
// any other claim of the namespace is not; a claim's zone is its bound
// volume's node affinity, by the topology label or the beta label older
// provisioners wrote; a claim not Bound has none yet; a volume that names no
// zone pins nothing; a volume that cannot be read carries the error, never
// a guessed zone; no claim is an empty list; the list refused is the error.
func TestCacheClaims(t *testing.T) {
	ctx := context.Background()
	one := func(c *CacheClaim) []*CacheClaim { return []*CacheClaim{c} }
	cases := []struct {
		name string
		objs []runtime.Object
		want []*CacheClaim
	}{
		{"none", nil, []*CacheClaim{}},
		{"another claim of the namespace is not the cache", []runtime.Object{claim("hf-cachet", "Bound", "pvc-9"), claim("models", "Bound", "pvc-8")}, []*CacheClaim{}},
		{"pending", []runtime.Object{claim("hf-cache", "Pending", "")}, one(&CacheClaim{Namespace: "model-serving", Name: "hf-cache", Phase: "Pending"})},
		{"bound", []runtime.Object{claim("hf-cache", "Bound", "pvc-1"), volume("pvc-1", zoneAffinity(LabelZone, "eu-central-1b"))},
			one(&CacheClaim{Namespace: "model-serving", Name: "hf-cache", Phase: "Bound", Volume: "pvc-1", Zone: "eu-central-1b"})},
		{"bound-legacy-label", []runtime.Object{claim("hf-cache", "Bound", "pvc-1"), volume("pvc-1", zoneAffinity(LabelZoneLegacy, "eu-central-1a"))},
			one(&CacheClaim{Namespace: "model-serving", Name: "hf-cache", Phase: "Bound", Volume: "pvc-1", Zone: "eu-central-1a"})},
		{"bound-no-zone", []runtime.Object{claim("hf-cache", "Bound", "pvc-1"), volume("pvc-1", nil)},
			one(&CacheClaim{Namespace: "model-serving", Name: "hf-cache", Phase: "Bound", Volume: "pvc-1"})},
		{"volume-gone", []runtime.Object{claim("hf-cache", "Bound", "pvc-1")},
			one(&CacheClaim{Namespace: "model-serving", Name: "hf-cache", Phase: "Bound", Volume: "pvc-1", Error: `get PersistentVolume pvc-1 of claim model-serving/hf-cache: persistentvolumes "pvc-1" not found`})},
		{"one per zone, sorted", []runtime.Object{
			claim("hf-cache-eu-central-1a", "Pending", ""),
			claim("hf-cache", "Bound", "pvc-1"), volume("pvc-1", zoneAffinity(LabelZone, "eu-central-1b")),
			claim("hf-cache-eu-central-1c", "Bound", "pvc-3"), volume("pvc-3", zoneAffinity(LabelZone, "eu-central-1c")),
		}, []*CacheClaim{
			{Namespace: "model-serving", Name: "hf-cache", Phase: "Bound", Volume: "pvc-1", Zone: "eu-central-1b"},
			{Namespace: "model-serving", Name: "hf-cache-eu-central-1a", Phase: "Pending"},
			{Namespace: "model-serving", Name: "hf-cache-eu-central-1c", Phase: "Bound", Volume: "pvc-3", Zone: "eu-central-1c"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), claimListKinds, tc.objs...)
			got, err := CacheClaims(ctx, dyn, "model-serving", "hf-cache")
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("list-forbidden", func(t *testing.T) {
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), claimListKinds, claim("hf-cache", "Bound", "pvc-1"))
		dyn.PrependReactor("list", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumeclaims"}, "", errors.New("User \"alice\" cannot list resource \"persistentvolumeclaims\" in API group \"\" in the namespace \"model-serving\""))
		})
		got, err := CacheClaims(ctx, dyn, "model-serving", "hf-cache")
		require.Error(t, err, "claims that cannot be listed are not the same as none")
		assert.Nil(t, got)
		assert.Contains(t, err.Error(), `list PersistentVolumeClaims in model-serving: persistentvolumeclaims is forbidden`)
	})
}

func TestZoneClaimName(t *testing.T) {
	assert.Equal(t, "hf-cache-eu-central-1a", ZoneClaimName("hf-cache", "eu-central-1a"))
	assert.True(t, CacheClaim{Phase: ClaimBound, Volume: "pvc-1", Zone: "eu-central-1a"}.BoundIn("eu-central-1a"))
	assert.False(t, CacheClaim{Phase: "Pending"}.BoundIn(""), "a claim without a volume is Bound nowhere")
}
