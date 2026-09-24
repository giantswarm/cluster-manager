package detect

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/cluster-manager/internal/compose"
)

// The model cache claims (giantswarm/cluster-manager#59, #71): the serving
// slice's connectivity chart creates one PersistentVolumeClaim in the serving
// namespace for the weights and compiled graphs of every served model, kept
// when the slice goes (`helm.sh/resource-policy: keep`). On AWS it is one EBS
// volume, bound in one availability zone by the first predictor that mounts
// it, and every predictor after it is pinned to that zone by the volume's
// node affinity — a pool whose nodes come up elsewhere strands them Pending.
// So the cache is one claim per zone: the claim of a zone is named after it
// (`<base>-<zone>`, ZoneClaimName), a pool created in the zone mounts it, and
// the one claim of before (`<base>`) is the claim of the zone it is Bound in.

// Resources the cache claims are read from: the claims, their volumes, and
// the StorageClass a claim's tier — what it is billed for beside its storage
// — is read from (giantswarm/cluster-manager#83).
var (
	PersistentVolumeClaimGVR = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
	PersistentVolumeGVR      = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumes"}
	StorageClassGVR          = schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}
)

// ReclaimDelete is the reclaim policy under which a volume goes with its
// claim; Retain keeps it — and its bill — after the claim is deleted.
const (
	ReclaimDelete = "Delete"
	ReclaimRetain = "Retain"
)

// The zone labels a volume's node affinity names: the topology label, and
// the beta label older provisioners wrote.
const (
	LabelZone       = "topology.kubernetes.io/zone"
	LabelZoneLegacy = "failure-domain.beta.kubernetes.io/zone"
)

// ClaimBound is a claim's status.phase once a volume is bound to it.
const ClaimBound = "Bound"

// The tier annotations the connectivity chart stamps on a cache claim at
// create (giantswarm/agent-platform#605): the EBS CSI StorageClass parameters
// the claim was provisioned with — `type`, `iops`, `throughput` (MiB/s) —, so
// its tier survives its class, which is Helm-owned and goes with its release
// or with a change of its parameters while the claim stays.
const (
	AnnotationVolumeType       = "agent-platform.giantswarm.io/volume-type"
	AnnotationVolumeIOPS       = "agent-platform.giantswarm.io/volume-iops"
	AnnotationVolumeThroughput = "agent-platform.giantswarm.io/volume-throughput"
)

// Where a claim's tier is read from: its StorageClass, or — the class gone
// or not readable — the claim's tier annotations.
const (
	TierSourceStorageClass     = "storageClass"
	TierSourceClaimAnnotations = "claimAnnotations"
)

// CacheClaim is one model cache claim of the serving namespace as
// list_clusters and create_node_pool report it.
type CacheClaim struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Phase is the claim's status.phase: Pending until a volume is bound,
	// Bound, Lost.
	Phase string `json:"phase,omitempty"`
	// Volume is the bound PersistentVolume's name; empty while none is.
	Volume string `json:"volume,omitempty"`
	// Zone is the availability zone the bound volume's node affinity names
	// (LabelZone, else LabelZoneLegacy): where every node mounting it must
	// be. Empty while no volume is bound, when the volume names no zone (a
	// network file system), or when the volume cannot be read (Error).
	Zone string `json:"zone,omitempty"`
	// Error says why the claim's volume could not be read as the caller;
	// Phase and Volume are then as far as the read got.
	Error string `json:"error,omitempty"`
	// Capacity is the volume's size once Bound (status.capacity.storage),
	// else the claim's request — what the claim is billed for; CapacityGiB
	// the same in GiB (giantswarm/cluster-manager#83).
	Capacity    string  `json:"capacity,omitempty"`
	CapacityGiB float64 `json:"capacityGiB,omitempty"`
	// StorageClass is the claim's class; Tier what its volume is provisioned
	// with (volume type, IOPS, throughput), read from the class where it can
	// be read, else from the claim's tier annotations (the class is gone: a
	// Helm-owned class goes with its release while the claim stays) —
	// TierSource says which —, nil with TierNote saying why it is not known.
	StorageClass string              `json:"storageClass,omitempty"`
	Tier         *compose.VolumeTier `json:"tier,omitempty"`
	TierSource   string              `json:"tierSource,omitempty"`
	TierNote     string              `json:"tierNote,omitempty"`
	// ReclaimPolicy is the bound volume's: Delete, the volume goes with the
	// claim; Retain, it stays and is billed until deleted by hand.
	ReclaimPolicy string `json:"reclaimPolicy,omitempty"`
	// Created is the claim's creation time, RFC3339: since when it stands.
	Created string `json:"created,omitempty"`
	// Price is what the volume is billed per month at list prices in the
	// cluster's region (filled in by the tools, which know the region);
	// PriceNote says why there is none.
	Price     *compose.ClaimPrice `json:"price,omitempty"`
	PriceNote string              `json:"priceNote,omitempty"`
	// Mounted marks the claim the cluster's slice release mounts (its
	// modelServing.cache.pvc.name with the cache on); filled in by the tools.
	Mounted bool `json:"mounted,omitempty"`
}

// Priced fills the claim's price for the region from its capacity and tier
// (compose.PriceClaim), or the note saying why there is none.
func (c *CacheClaim) Priced(region compose.Region) *CacheClaim {
	var tier compose.VolumeTier
	if c.Tier != nil {
		tier = *c.Tier
	}
	c.Price, c.PriceNote = compose.PriceClaim(region, c.CapacityGiB, tier)
	return c
}

// Standing words the claim's size, tier and price for a sentence:
// `100 GiB gp3 at 500 MiB/s, about $27.37 a month at list prices (AWS EBS gp3
// list price, EU (Frankfurt) (eu-central-1), as of 2026-09-19)`, or as far as
// the figures go with the note for what is missing.
func (c *CacheClaim) Standing() string {
	var parts []string
	if c.Capacity != "" {
		size := c.Capacity
		if c.CapacityGiB > 0 {
			size = fmt.Sprintf("%s GiB", trimFloat(c.CapacityGiB))
		}
		if c.Tier != nil && c.Tier.Type != "" {
			size += " " + c.Tier.Type
			if c.Tier.ThroughputMiBps > 0 {
				size += fmt.Sprintf(" at %d MiB/s", c.Tier.ThroughputMiBps)
			}
		}
		parts = append(parts, size)
	}
	switch {
	case c.Price != nil:
		parts = append(parts, fmt.Sprintf("about $%.2f a month at list prices (%s, as of %s)", c.Price.MonthlyUSD, c.Price.Source, c.Price.AsOf))
	case c.PriceNote != "":
		parts = append(parts, c.PriceNote)
	}
	return strings.Join(parts, ", ")
}

// trimFloat prints a figure without a fractional part when it has none.
func trimFloat(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%.1f", v)
}

// String names the claim: `namespace/name`.
func (c CacheClaim) String() string { return c.Namespace + "/" + c.Name }

// BoundIn reports whether the claim is Bound to a volume in the zone.
func (c CacheClaim) BoundIn(zone string) bool {
	return c.Phase == ClaimBound && c.Volume != "" && c.Zone == zone
}

// ZoneClaimName names the model cache claim of a zone: `<base>-<zone>`
// (`hf-cache-eu-central-1a`).
func ZoneClaimName(base, zone string) string { return base + "-" + zone }

// CacheClaims reads the model cache claims of the serving namespace on the
// target as the caller — the claim named base and every claim named
// `<base>-<suffix>` (the claims per zone) — and, for each that is Bound, the
// zone its volume is bound to; sorted by name, empty for none. A claim whose
// volume cannot be read carries the error, never a guessed zone. The list
// failing is the error: the claims then cannot be told at all, and the
// caller says so.
func CacheClaims(ctx context.Context, reader dynamic.Interface, namespace, base string) ([]*CacheClaim, error) {
	pvcs, err := reader.Resource(PersistentVolumeClaimGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list PersistentVolumeClaims in %s: %w", namespace, err)
	}
	claims := []*CacheClaim{}
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if name := pvc.GetName(); name == base || strings.HasPrefix(name, base+"-") {
			claims = append(claims, &CacheClaim{Namespace: namespace, Name: name})
		}
	}
	sort.Slice(claims, func(i, j int) bool { return claims[i].Name < claims[j].Name })
	var wg sync.WaitGroup
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		for _, c := range claims {
			if c.Name != pvc.GetName() {
				continue
			}
			wg.Add(1)
			go func(c *CacheClaim) {
				defer wg.Done()
				claimState(ctx, reader, pvc, c)
			}(c)
		}
	}
	wg.Wait()
	return claims, nil
}

// claimState fills in a claim's phase, volume, capacity, class and creation
// time from the object; its tier (claimTier); and, once Bound, the zone its
// volume is bound to and the volume's reclaim policy.
func claimState(ctx context.Context, reader dynamic.Interface, pvc *unstructured.Unstructured, c *CacheClaim) {
	c.Phase, _, _ = unstructured.NestedString(pvc.Object, "status", "phase")
	c.Volume, _, _ = unstructured.NestedString(pvc.Object, "spec", "volumeName")
	c.StorageClass, _, _ = unstructured.NestedString(pvc.Object, "spec", "storageClassName")
	if created := pvc.GetCreationTimestamp(); !created.IsZero() {
		c.Created = created.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	capacity, _, _ := unstructured.NestedString(pvc.Object, "status", "capacity", "storage")
	if capacity == "" {
		capacity, _, _ = unstructured.NestedString(pvc.Object, "spec", "resources", "requests", "storage")
	}
	if q, err := resource.ParseQuantity(capacity); err == nil && capacity != "" {
		c.Capacity, c.CapacityGiB = capacity, compose.QuantityGiB(q)
	}
	// The claim's name, printed before the reads fill in its fields at once:
	// printing c itself copies the whole struct while they are written.
	name := c.String()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.Tier, c.TierSource, c.TierNote = claimTier(ctx, reader, pvc, c.StorageClass, name)
	}()
	if c.Phase == ClaimBound && c.Volume != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pv, err := reader.Resource(PersistentVolumeGVR).Get(ctx, c.Volume, metav1.GetOptions{})
			if err != nil {
				c.Error = fmt.Sprintf("get PersistentVolume %s of claim %s: %v", c.Volume, name, err)
				return
			}
			c.Zone = volumeZone(pv)
			c.ReclaimPolicy, _, _ = unstructured.NestedString(pv.Object, "spec", "persistentVolumeReclaimPolicy")
		}()
	}
	wg.Wait()
}

// claimTier reads the claim's tier from its StorageClass, named class —
// the EBS CSI driver's parameters — where the class can be read, whatever the
// claim's annotations say (source TierSourceStorageClass). A class that is
// gone (Helm-owned, it goes with its release or a change of its parameters
// while the claim stays; a bound volume works on without it), not readable as
// the caller, or not named leaves the tier to the claim's annotations
// (annotationTier, source TierSourceClaimAnnotations); a claim without them
// leaves the tier nil with the note saying why it is not known.
func claimTier(ctx context.Context, reader dynamic.Interface, pvc *unstructured.Unstructured, class, claim string) (tier *compose.VolumeTier, source, note string) {
	if class == "" {
		note = "the claim names no StorageClass: its volume's tier cannot be told"
	} else {
		sc, err := reader.Resource(StorageClassGVR).Get(ctx, class, metav1.GetOptions{})
		if err == nil {
			params, _, _ := unstructured.NestedStringMap(sc.Object, "parameters")
			if t := compose.TierOf(params); t.Type != "" {
				return &t, TierSourceStorageClass, ""
			}
			return nil, "", fmt.Sprintf("StorageClass %s of claim %s names no volume type in its parameters: the tier cannot be told", class, claim)
		}
		note = fmt.Sprintf("StorageClass %s of claim %s cannot be read (%v): the volume's tier — what it is billed for beside its storage — cannot be told", class, claim, err)
	}
	if t := annotationTier(pvc); t.Type != "" {
		return &t, TierSourceClaimAnnotations, ""
	}
	return nil, "", note
}

// annotationTier is the tier the claim's annotations name
// (AnnotationVolumeType, AnnotationVolumeIOPS, AnnotationVolumeThroughput),
// read as the StorageClass parameters they mirror; its Type is empty when the
// claim names none.
func annotationTier(pvc *unstructured.Unstructured) compose.VolumeTier {
	a := pvc.GetAnnotations()
	return compose.TierOf(map[string]string{
		"type":       a[AnnotationVolumeType],
		"iops":       a[AnnotationVolumeIOPS],
		"throughput": a[AnnotationVolumeThroughput],
	})
}

// volumeZone is the one zone a volume's required node affinity names — the
// first value of the first LabelZone (else LabelZoneLegacy) expression among
// its node selector terms — or "" when the affinity names none.
func volumeZone(pv *unstructured.Unstructured) string {
	terms, _, _ := unstructured.NestedSlice(pv.Object, "spec", "nodeAffinity", "required", "nodeSelectorTerms")
	for _, key := range []string{LabelZone, LabelZoneLegacy} {
		for _, t := range terms {
			term, _ := t.(map[string]any)
			exprs, _ := term["matchExpressions"].([]any)
			for _, e := range exprs {
				expr, _ := e.(map[string]any)
				if expr["key"] != key {
					continue
				}
				values, _ := expr["values"].([]any)
				if len(values) > 0 {
					if zone, ok := values[0].(string); ok {
						return zone
					}
				}
			}
		}
	}
	return ""
}
