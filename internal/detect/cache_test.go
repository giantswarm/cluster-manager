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

	"github.com/giantswarm/cluster-manager/internal/compose"
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

// noClass is the tier note of a claim that names no StorageClass.
const noClass = "the claim names no StorageClass: its volume's tier cannot be told"

// classed is claim with a class, a size and a creation time: what a claim the
// connectivity chart applied looks like.
func classed(pvc *unstructured.Unstructured, class, size, created string) *unstructured.Unstructured {
	spec := pvc.Object["spec"].(map[string]any)
	spec["storageClassName"] = class
	spec["resources"] = map[string]any{"requests": map[string]any{"storage": size}}
	pvc.Object["metadata"].(map[string]any)["creationTimestamp"] = created
	return pvc
}

func storageClass(name string, params map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "storage.k8s.io/v1", "kind": "StorageClass",
		"metadata":    map[string]any{"name": name},
		"provisioner": "ebs.csi.aws.com",
		"parameters":  params,
	}}
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
		{"pending", []runtime.Object{claim("hf-cache", "Pending", "")}, one(&CacheClaim{Namespace: "model-serving", Name: "hf-cache", Phase: "Pending", TierNote: noClass})},
		{"bound", []runtime.Object{claim("hf-cache", "Bound", "pvc-1"), volume("pvc-1", zoneAffinity(LabelZone, "eu-central-1b"))},
			one(&CacheClaim{Namespace: "model-serving", Name: "hf-cache", Phase: "Bound", Volume: "pvc-1", Zone: "eu-central-1b", TierNote: noClass})},
		{"bound-legacy-label", []runtime.Object{claim("hf-cache", "Bound", "pvc-1"), volume("pvc-1", zoneAffinity(LabelZoneLegacy, "eu-central-1a"))},
			one(&CacheClaim{Namespace: "model-serving", Name: "hf-cache", Phase: "Bound", Volume: "pvc-1", Zone: "eu-central-1a", TierNote: noClass})},
		{"bound-no-zone", []runtime.Object{claim("hf-cache", "Bound", "pvc-1"), volume("pvc-1", nil)},
			one(&CacheClaim{Namespace: "model-serving", Name: "hf-cache", Phase: "Bound", Volume: "pvc-1", TierNote: noClass})},
		{"volume-gone", []runtime.Object{claim("hf-cache", "Bound", "pvc-1")},
			one(&CacheClaim{Namespace: "model-serving", Name: "hf-cache", Phase: "Bound", Volume: "pvc-1", Error: `get PersistentVolume pvc-1 of claim model-serving/hf-cache: persistentvolumes "pvc-1" not found`, TierNote: noClass})},
		{"one per zone, sorted", []runtime.Object{
			claim("hf-cache-eu-central-1a", "Pending", ""),
			claim("hf-cache", "Bound", "pvc-1"), volume("pvc-1", zoneAffinity(LabelZone, "eu-central-1b")),
			claim("hf-cache-eu-central-1c", "Bound", "pvc-3"), volume("pvc-3", zoneAffinity(LabelZone, "eu-central-1c")),
		}, []*CacheClaim{
			{Namespace: "model-serving", Name: "hf-cache", Phase: "Bound", Volume: "pvc-1", Zone: "eu-central-1b", TierNote: noClass},
			{Namespace: "model-serving", Name: "hf-cache-eu-central-1a", Phase: "Pending", TierNote: noClass},
			{Namespace: "model-serving", Name: "hf-cache-eu-central-1c", Phase: "Bound", Volume: "pvc-3", Zone: "eu-central-1c", TierNote: noClass},
		}},
		// The claim the connectivity chart applied (giantswarm/cluster-manager#83):
		// its size, class and tier — what it is billed for —, since when, and
		// whether its volume goes with it.
		{"bound with its class: size, tier, reclaim policy, since when", []runtime.Object{
			classed(claim("hf-cache", "Bound", "pvc-1"), "agent-platform-connectivity-hf-cache", "100Gi", "2026-09-18T20:31:04Z"),
			withReclaim(volume("pvc-1", zoneAffinity(LabelZone, "eu-central-1b")), ReclaimDelete),
			storageClass("agent-platform-connectivity-hf-cache", map[string]any{"type": "gp3", "iops": "3000", "throughput": "500"}),
		}, one(&CacheClaim{Namespace: "model-serving", Name: "hf-cache", Phase: "Bound", Volume: "pvc-1", Zone: "eu-central-1b",
			Capacity: "100Gi", CapacityGiB: 100, StorageClass: "agent-platform-connectivity-hf-cache", Tier: &compose.VolumeTier{Type: "gp3", IOPS: 3000, ThroughputMiBps: 500},
			ReclaimPolicy: ReclaimDelete, Created: "2026-09-18T20:31:04Z"})},
		{"the class gone: the tier is not known and says so", []runtime.Object{
			classed(claim("hf-cache", "Bound", "pvc-1"), "agent-platform-connectivity-hf-cache-old", "100Gi", "2026-09-18T20:31:04Z"),
			withReclaim(volume("pvc-1", zoneAffinity(LabelZone, "eu-central-1b")), ReclaimDelete),
		}, one(&CacheClaim{Namespace: "model-serving", Name: "hf-cache", Phase: "Bound", Volume: "pvc-1", Zone: "eu-central-1b",
			Capacity: "100Gi", CapacityGiB: 100, StorageClass: "agent-platform-connectivity-hf-cache-old", ReclaimPolicy: ReclaimDelete, Created: "2026-09-18T20:31:04Z",
			TierNote: `StorageClass agent-platform-connectivity-hf-cache-old of claim model-serving/hf-cache cannot be read (storageclasses.storage.k8s.io "agent-platform-connectivity-hf-cache-old" not found): the volume's tier — what it is billed for beside its storage — cannot be told`})},
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

func withReclaim(pv *unstructured.Unstructured, policy string) *unstructured.Unstructured {
	pv.Object["spec"].(map[string]any)["persistentVolumeReclaimPolicy"] = policy
	return pv
}

// TestCacheClaimPricedAndStanding (giantswarm/cluster-manager#83): a claim's
// monthly list price in the cluster's region from its capacity and tier, and
// the sentence an answer carries; a tier that is not known is a note, never
// a storage-only figure passed off as the price.
func TestCacheClaimPricedAndStanding(t *testing.T) {
	c := &CacheClaim{Namespace: "model-serving", Name: "hf-cache", Capacity: "100Gi", CapacityGiB: 100, Tier: &compose.VolumeTier{Type: "gp3", IOPS: 3000, ThroughputMiBps: 500}}
	c.Priced(compose.Region{Name: "eu-central-1"})
	require.NotNil(t, c.Price)
	assert.InDelta(t, 27.37, c.Price.MonthlyUSD, 1e-9, "100 GiB × $0.0952 + 375 MiB/s × $0.0476; the tier's IOPS are included")
	assert.Equal(t, "100 GiB gp3 at 500 MiB/s, about $27.37 a month at list prices (AWS EBS gp3 list price, EU (Frankfurt) (eu-central-1), as of "+compose.PriceAsOf+")", c.Standing())

	unknown := &CacheClaim{Namespace: "model-serving", Name: "hf-cache", Capacity: "100Gi", CapacityGiB: 100, TierNote: "the class is gone"}
	unknown.Priced(compose.Region{Name: "eu-central-1"})
	assert.Nil(t, unknown.Price)
	assert.Equal(t, "no price: the volume's tier is not known (its StorageClass could not be read), and a gp3 volume is billed for its provisioned throughput and IOPS beside its storage", unknown.PriceNote)
	assert.Equal(t, "100 GiB, "+unknown.PriceNote, unknown.Standing())
}

func TestZoneClaimName(t *testing.T) {
	assert.Equal(t, "hf-cache-eu-central-1a", ZoneClaimName("hf-cache", "eu-central-1a"))
	assert.True(t, CacheClaim{Phase: ClaimBound, Volume: "pvc-1", Zone: "eu-central-1a"}.BoundIn("eu-central-1a"))
	assert.False(t, CacheClaim{Phase: "Pending"}.BoundIn(""), "a claim without a volume is Bound nowhere")
}
