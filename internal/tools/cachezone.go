package tools

import (
	"context"
	"fmt"

	"github.com/giantswarm/cluster-manager/internal/detect"
)

// The pool follows the cache (giantswarm/cluster-manager#59): the model cache
// claim outlives the pool and is one EBS volume, bound in one zone by the
// first predictor. A pool whose nodes come up in another zone strands the
// next predictor Pending — the prewarm placeholder's node landed in another
// zone, the single-GPU limit forbade a second node, and the pool read ready
// with an idle node. So create_node_pool pins a new pool's nodes to the
// claim's zone (the chart's pool.zones) and says so; no claim, no pin. A
// family not offered in that zone fails its launch visibly, named in
// list_node_pools' nodes step, and the way out is to remove the claim or
// pick another accelerator — never a silently stranded predictor.

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

// zonePin is what the cache claim decides for a pool: the zones its nodes
// are pinned to, the sentence the answer carries about it, and — when the
// claim could not be read — the warning instead.
type zonePin struct {
	claim   *detect.CacheClaim
	zones   []string
	note    string
	warning string
}

// zonePinFor decides the pin from the claim as read on cluster: the bound
// volume's zone pins the pool; a claim that is not Bound, or a volume that
// names no zone, pins nothing and the note says so; a claim that cannot be
// read pins nothing and is a warning naming why; no claim is nothing.
func zonePinFor(claim *detect.CacheClaim, cluster string) zonePin {
	pin := zonePin{claim: claim}
	switch {
	case claim == nil:
	case claim.Error != "":
		pin.warning = fmt.Sprintf("the model cache claim %s on %s cannot be read as you (%s): the pool's nodes are not pinned to the cache's zone — the cache is one volume, bound in one zone, and a node launched in another zone strands a predictor mounting it Pending; re-run once you may read the claim and its volume, or remove the claim (it costs the cached weights)", claim, cluster, claim.Error)
	case claim.Phase != detect.ClaimBound || claim.Volume == "":
		phase := claim.Phase
		if phase == "" {
			phase = "not Bound"
		}
		pin.note = fmt.Sprintf("the model cache claim %s on %s is %s, bound to no volume yet: the pool's nodes are not pinned to a zone — the first predictor mounting the cache binds it to its node's zone, and every pool created after that follows it", claim, cluster, phase)
	case claim.Zone == "":
		pin.note = fmt.Sprintf("the model cache claim %s on %s is bound to volume %s, whose node affinity names no zone: the pool's nodes are not pinned — a volume every zone reaches strands no predictor", claim, cluster, claim.Volume)
	default:
		pin.zones = []string{claim.Zone}
		pin.note = fmt.Sprintf("nodes pinned to %s: the model cache (claim %s, volume %s) lives there, and a node launched in another zone strands a predictor mounting it Pending; remove the cache claim to lift the pin (it costs the cached weights and compiled graphs), or pick an accelerator offered in %s — a family not offered there fails its launch, named in list_node_pools' nodes step", claim.Zone, claim, claim.Volume, claim.Zone)
	}
	return pin
}

// warnings appends the pin's warning, when there is one, to the create's.
func (p zonePin) warnings(warnings []string) []string {
	if p.warning == "" {
		return warnings
	}
	return append(warnings, p.warning)
}
