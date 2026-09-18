package tools

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/cluster-manager/internal/detect"
)

// The pool follows the cache (giantswarm/cluster-manager#59): the model cache
// claim outlives the pool and is one EBS volume, bound in one zone by the
// first predictor. A pool whose nodes come up in another zone strands the
// next predictor Pending — the prewarm placeholder's node landed in another
// zone, the single-GPU limit forbade a second node, and the pool read ready
// with an idle node. So create_node_pool pins a new pool's nodes to the
// claim's zone (the chart's pool.zones) and says so; no claim, no pin.
//
// The person decides where the pool runs (giantswarm/cluster-manager#65): a
// pool created with zones is pinned to them — judged against the claim: a
// Bound claim in a zone outside them is a refusal naming the claim's zone and
// the ways out, a claim inside them pins nothing further — and a pool created
// with cache false serves without the claim, so no zone follows from it. A
// family not offered in a pinned zone fails its launch visibly, named in
// list_node_pools' nodes step with what pinned the pool.

// cacheClaim reads the serving namespace's model cache claim on the target
// as the caller; nil when the target cannot be read.
func (s *Service) cacheClaim(ctx context.Context, t target) *detect.CacheClaim {
	if t.Reader == nil {
		return nil
	}
	return detect.CacheClaimState(ctx, t.Reader, s.cfg.ServingNamespace, s.cfg.cacheClaimName())
}

// readCacheClaim is the create's read of the claim: a target that cannot be
// read as the caller answers a claim whose existence is unknown, with the
// reason, so the pin's absence is said rather than silent.
func (s *Service) readCacheClaim(ctx context.Context, t target) *detect.CacheClaim {
	if t.Reader == nil {
		return &detect.CacheClaim{Namespace: s.cfg.ServingNamespace, Name: s.cfg.cacheClaimName(), Error: t.Reason}
	}
	return s.cacheClaim(ctx, t)
}

// zoneChoice is the caller's part of the pin: the zones create_node_pool was
// given (none for the claim's zone or nothing), and whether the slice's
// predictors mount the cache at all (cache false: no claim is mounted, so
// its zone binds nothing).
type zoneChoice struct {
	zones []string
	cache bool
}

// zonePin is what the pool's zones come to: the zones its nodes are pinned
// to, the sentence the answer carries about it, and — when the claim could
// not be read and matters — the warning instead.
type zonePin struct {
	claim   *detect.CacheClaim
	zones   []string
	note    string
	warning string
}

// CacheZoneRefusal is the structured form of the zones refusal: the claim's
// zone against the zones named, with the ways out (Refused.CacheZone).
type CacheZoneRefusal struct {
	// Claim is the model cache claim as read, Bound in ClaimZone.
	Claim     *detect.CacheClaim `json:"claim"`
	ClaimZone string             `json:"claimZone"`
	// Zones are the zones named on create, the claim's not among them.
	Zones []string `json:"zones"`
	// Remedies are the ways out, each a sentence: name the claim's zone,
	// serve without the cache, remove the claim.
	Remedies []string `json:"remedies"`
}

// zonePinFor decides the pin from the claim as read on cluster and the
// caller's choice. With zones named: they pin the pool; a Bound claim whose
// zone is outside them is a refusal (the person would strand every predictor
// mounting the cache) unless the cache is off for the slice; a claim inside
// them, not Bound, naming no zone or absent pins nothing further and the note
// says what was found; a claim that cannot be read is a warning. With none
// named: the bound volume's zone pins the pool (giantswarm/cluster-manager#59)
// while the cache is on; a claim that is not Bound, or a volume that names no
// zone, pins nothing and the note says so; a claim that cannot be read pins
// nothing and is a warning naming why; no claim is nothing. With the cache
// off nothing follows from the claim either way, and the note says so.
func zonePinFor(claim *detect.CacheClaim, cluster string, choice zoneChoice) (zonePin, error) {
	pin := zonePin{claim: claim, zones: choice.zones}
	if !choice.cache {
		pin.note = noCacheNote(claim, cluster, choice.zones)
		return pin, nil
	}
	if len(choice.zones) > 0 {
		return chosenZonesPin(pin, claim, cluster, choice.zones)
	}
	switch {
	case claim == nil:
	case claim.Error != "":
		pin.warning = fmt.Sprintf("the model cache claim %s on %s cannot be read as you (%s): the pool's nodes are not pinned to the cache's zone — the cache is one volume, bound in one zone, and a node launched in another zone strands a predictor mounting it Pending; re-run once you may read the claim and its volume, or remove the claim (it costs the cached weights)", claim, cluster, claim.Error)
	case claim.Phase != detect.ClaimBound || claim.Volume == "":
		pin.note = fmt.Sprintf("the model cache claim %s on %s is %s, bound to no volume yet: the pool's nodes are not pinned to a zone — the first predictor mounting the cache binds it to its node's zone, and every pool created after that follows it", claim, cluster, claimPhase(claim))
	case claim.Zone == "":
		pin.note = fmt.Sprintf("the model cache claim %s on %s is bound to volume %s, whose node affinity names no zone: the pool's nodes are not pinned — a volume every zone reaches strands no predictor", claim, cluster, claim.Volume)
	default:
		pin.zones = []string{claim.Zone}
		pin.note = fmt.Sprintf("nodes pinned to %s: the model cache (claim %s, volume %s) lives there, and a node launched in another zone strands a predictor mounting it Pending; name zones on create to run the pool elsewhere — with cache false, so this pool's slice serves without the cache —, or remove the cache claim to lift the pin (it costs the cached weights and compiled graphs), or pick an accelerator offered in %s — a family not offered there fails its launch, named in list_node_pools' nodes step", claim.Zone, claim, claim.Volume, claim.Zone)
	}
	return pin, nil
}

// chosenZonesPin is the pin for the zones the caller named while the cache
// is on: the claim's zone must be among them, or the claim must bind nothing
// yet.
func chosenZonesPin(pin zonePin, claim *detect.CacheClaim, cluster string, zones []string) (zonePin, error) {
	named := strings.Join(zones, ", ")
	switch {
	case claim == nil:
		pin.note = fmt.Sprintf("nodes pinned to %s, the zones named on create; %s has no model cache claim, so nothing else constrains them", named, cluster)
	case claim.Error != "":
		pin.note = fmt.Sprintf("nodes pinned to %s, the zones named on create", named)
		pin.warning = fmt.Sprintf("the model cache claim %s on %s cannot be read as you (%s): whether its volume lies in %s cannot be told — the cache is one volume, bound in one zone, and a node launched in another zone strands a predictor mounting it Pending; re-run once you may read the claim and its volume, or pass cache false so this pool's slice serves without it", claim, cluster, claim.Error, named)
	case claim.Phase != detect.ClaimBound || claim.Volume == "":
		pin.note = fmt.Sprintf("nodes pinned to %s, the zones named on create; the model cache claim %s on %s is %s, bound to no volume yet — the first predictor mounting the cache binds it to its node's zone, one of these, and every pool created after that follows it", named, claim, cluster, claimPhase(claim))
	case claim.Zone == "":
		pin.note = fmt.Sprintf("nodes pinned to %s, the zones named on create; the model cache claim %s on %s is bound to volume %s, whose node affinity names no zone — a volume every zone reaches strands no predictor", named, claim, cluster, claim.Volume)
	case slices.Contains(zones, claim.Zone):
		pin.note = fmt.Sprintf("nodes pinned to %s, the zones named on create; the model cache (claim %s, volume %s) lives in %s, one of them — a predictor mounting it needs its node there", named, claim, claim.Volume, claim.Zone)
	default:
		remedies := []string{
			fmt.Sprintf("name %s among the zones", claim.Zone),
			"pass cache false, so this pool's slice serves without the cache — the weights land in the predictor pod's ephemeral storage and the claim is left as it is",
			"remove the claim (it costs the cached weights and compiled graphs)",
		}
		return pin, &ErrRefused{
			Reason: fmt.Sprintf("zones %s: the model cache claim %s on %s is bound to volume %s in %s, outside them — a pool node launched in %s cannot mount the cache, and every predictor mounting it sits Pending (`didn't match PersistentVolume's node affinity`); %s", named, claim, cluster, claim.Volume, claim.Zone, named, strings.Join(remedies, ", or ")),
			Refused: &Refused{
				Nodes: []string{}, Models: []string{}, ReadFrom: readFromCluster,
				Hint:      "Name the claim's zone among zones, pass cache false, or remove the claim, and re-run.",
				CacheZone: &CacheZoneRefusal{Claim: claim, ClaimZone: claim.Zone, Zones: zones, Remedies: remedies},
			},
		}
	}
	return pin, nil
}

// noCacheNote is the zones sentence of a pool whose slice serves without
// the cache: the zones named pin the pool, or nothing does, and the claim —
// whatever its state — pins nothing since no predictor mounts it.
func noCacheNote(claim *detect.CacheClaim, cluster string, zones []string) string {
	pinned := "the pool's nodes are not pinned to a zone"
	if len(zones) > 0 {
		pinned = fmt.Sprintf("nodes pinned to %s, the zones named on create", strings.Join(zones, ", "))
	}
	if claim == nil {
		return pinned + "; this pool's slice serves without the model cache (cache false), so no zone follows from a claim"
	}
	return fmt.Sprintf("%s; this pool's slice serves without the model cache (cache false): no predictor mounts the claim %s on %s, so its zone pins nothing and the claim is left as it is", pinned, claim, cluster)
}

// claimPhase is the claim's phase for a sentence; "not Bound" when it has none.
func claimPhase(claim *detect.CacheClaim) string {
	if claim.Phase == "" {
		return "not Bound"
	}
	return claim.Phase
}

// warnings appends the pin's warning, when there is one, to the create's.
func (p zonePin) warnings(warnings []string) []string {
	if p.warning == "" {
		return warnings
	}
	return append(warnings, p.warning)
}

// CacheSetting is the answer's word on the model cache for the slice the
// write composed (create_node_pool, enable_model_serving): whether the
// predictors mount the serving namespace's cache claim, which one, and what
// follows.
type CacheSetting struct {
	Enabled bool `json:"enabled"`
	// Claim names the claim the predictors mount (`namespace/name`); empty
	// when the cache is off.
	Claim string `json:"claim,omitempty"`
	Note  string `json:"note"`
}

// cacheSettingFor is the answer's cache block for the slice a write composed;
// nil when none was.
func (s *Service) cacheSettingFor(slice *SliceRelease, enabled bool, claim *detect.CacheClaim) *CacheSetting {
	if slice == nil {
		return nil
	}
	return cacheSetting(enabled, s.cfg.ServingNamespace+"/"+s.cfg.cacheClaimName(), claim)
}

// cacheSetting words the cache setting: on, the claim the connectivity
// chart applies and every predictor mounts; off, the slice's
// modelServing.cache.enabled false, the weights in the pod's ephemeral
// storage, no zone following from a claim — and the claim as read, when one
// exists, left as it is.
func cacheSetting(enabled bool, claimRef string, claim *detect.CacheClaim) *CacheSetting {
	if enabled {
		return &CacheSetting{Enabled: true, Claim: claimRef, Note: fmt.Sprintf("the predictors mount the model cache claim %s (the connectivity chart applies it where it does not exist, and keeps it): the weights and compiled graphs of every served model are kept there, and a pool created while the claim is Bound follows its zone", claimRef)}
	}
	note := fmt.Sprintf("modelServing.cache.enabled false on the slice release: no claim %s is applied or mounted, every predictor downloads its weights into its pod's ephemeral storage (the node's local disk) at each start, and no zone pin follows from a claim; a re-run with cache true is the slice's upgrade back to the cache", claimRef)
	if claim != nil && claim.Error == "" {
		note += fmt.Sprintf("; the existing claim %s (%s", claim, claimPhase(claim))
		if claim.Volume != "" {
			note += ", volume " + claim.Volume
		}
		if claim.Zone != "" {
			note += " in " + claim.Zone
		}
		note += ") is left as it is — Helm keeps it — and pins nothing"
	}
	return &CacheSetting{Note: note}
}
