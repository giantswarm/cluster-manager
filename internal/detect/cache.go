package detect

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
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

// Resources the cache claims are read from.
var (
	PersistentVolumeClaimGVR = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
	PersistentVolumeGVR      = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumes"}
)

// The zone labels a volume's node affinity names: the topology label, and
// the beta label older provisioners wrote.
const (
	LabelZone       = "topology.kubernetes.io/zone"
	LabelZoneLegacy = "failure-domain.beta.kubernetes.io/zone"
)

// ClaimBound is a claim's status.phase once a volume is bound to it.
const ClaimBound = "Bound"

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

// claimState fills in a claim's phase and volume from the object and, once
// Bound, the zone its volume is bound to.
func claimState(ctx context.Context, reader dynamic.Interface, pvc *unstructured.Unstructured, c *CacheClaim) {
	c.Phase, _, _ = unstructured.NestedString(pvc.Object, "status", "phase")
	c.Volume, _, _ = unstructured.NestedString(pvc.Object, "spec", "volumeName")
	if c.Phase != ClaimBound || c.Volume == "" {
		return
	}
	pv, err := reader.Resource(PersistentVolumeGVR).Get(ctx, c.Volume, metav1.GetOptions{})
	if err != nil {
		c.Error = fmt.Sprintf("get PersistentVolume %s of claim %s: %v", c.Volume, c, err)
		return
	}
	c.Zone = volumeZone(pv)
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
