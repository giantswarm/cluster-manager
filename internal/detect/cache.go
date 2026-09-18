package detect

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// The model cache claim (giantswarm/cluster-manager#59): the serving slice's
// connectivity chart creates one PersistentVolumeClaim in the serving
// namespace for the weights and compiled graphs of every served model, kept
// when the slice goes (`helm.sh/resource-policy: keep`). On AWS it is one EBS
// volume, bound in one availability zone by the first predictor that mounts
// it, and every predictor after it is pinned to that zone by the volume's
// node affinity — a pool whose nodes come up elsewhere strands them Pending.

// Resources the cache claim is read from.
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

// CacheClaim is the serving namespace's model cache claim as list_clusters
// and create_node_pool report it.
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
	// Error says why the claim or its volume could not be read as the
	// caller; Phase, Volume and Zone are then as far as the read got.
	Error string `json:"error,omitempty"`
}

// String names the claim: `namespace/name`.
func (c CacheClaim) String() string { return c.Namespace + "/" + c.Name }

// CacheClaimState reads the model cache claim of the serving namespace on
// the target as the caller and, once it is Bound, the zone its volume is
// bound to. Nil when there is no claim of that name; a claim that cannot be
// read, or whose volume cannot be, carries the error — the pin then has no
// ground and the caller says so, never a guessed zone.
func CacheClaimState(ctx context.Context, reader dynamic.Interface, namespace, name string) *CacheClaim {
	c := &CacheClaim{Namespace: namespace, Name: name}
	pvc, err := reader.Resource(PersistentVolumeClaimGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		c.Error = fmt.Sprintf("get PersistentVolumeClaim %s: %v", c, err)
		return c
	}
	c.Phase, _, _ = unstructured.NestedString(pvc.Object, "status", "phase")
	c.Volume, _, _ = unstructured.NestedString(pvc.Object, "spec", "volumeName")
	if c.Phase != ClaimBound || c.Volume == "" {
		return c
	}
	pv, err := reader.Resource(PersistentVolumeGVR).Get(ctx, c.Volume, metav1.GetOptions{})
	if err != nil {
		c.Error = fmt.Sprintf("get PersistentVolume %s of claim %s: %v", c.Volume, c, err)
		return c
	}
	c.Zone = volumeZone(pv)
	return c
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
