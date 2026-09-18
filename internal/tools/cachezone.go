package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/giantswarm/cluster-manager/internal/detect"
)

// The pool follows the cache (giantswarm/cluster-manager#59): a model cache
// claim outlives the pool and is one EBS volume, bound in one zone by the
// first predictor. A pool whose nodes come up in another zone strands the
// next predictor Pending — the prewarm placeholder's node landed in another
// zone, the single-GPU limit forbade a second node, and the pool read ready
// with an idle node. So create_node_pool pins a new pool's nodes to the
// claim's zone (the chart's pool.zones) and says so; no claim, no pin.
//
// The person decides where the pool runs (giantswarm/cluster-manager#65): a
// pool created with zones is pinned to them, and a pool created with cache
// false serves without a claim, so no zone follows from one. A family not
// offered in a pinned zone fails its launch visibly, named in
// list_node_pools' nodes step with what pinned the pool.
//
// The cache is one claim per zone (giantswarm/cluster-manager#71): one zonal
// claim pinned every pool to its zone, and when that zone had no capacity
// for the accelerator the ways out were to lose the cache or to serve
// without it. Now a pool created with one zone and the cache on mounts that
// zone's claim — the claim Bound there, else `<base>-<zone>`, which the
// connectivity chart creates and keeps (modelServing.cache.pvc.name on the
// slice release) — so a person picks the zone by capacity and the zone
// brings its own cache; several zones with the cache on are refused, a
// claim being one volume in one zone. Without zones the claims decide as
// before: one claim, its zone pins the pool; several, the zone has to be
// named. The slice release is the cluster's one, so its predictors mount
// the claim the last create_node_pool with the cache on named.

// cacheClaims is the serving namespace's model cache claims as read on the
// target as the caller (detect.CacheClaims): the claim of the base name and
// the claims per zone — or why they could not be read.
type cacheClaims struct {
	namespace, base string
	// claims are the claims as read, sorted by name; nil when they could
	// not be read (err).
	claims []*detect.CacheClaim
	// err says why the claims could not be listed as the caller: the target
	// not readable, the list forbidden.
	err string
}

// readCacheClaims reads the serving namespace's model cache claims on the
// target as the caller; a target that cannot be read answers claims that
// cannot be told, with the reason, so their absence is said rather than
// silent.
func (s *Service) readCacheClaims(ctx context.Context, t target) cacheClaims {
	r := cacheClaims{namespace: s.cfg.ServingNamespace, base: s.cfg.cacheClaimName()}
	if t.Reader == nil {
		r.err = t.Reason
		return r
	}
	claims, err := detect.CacheClaims(ctx, t.Reader, r.namespace, r.base)
	if err != nil {
		r.err = err.Error()
		return r
	}
	r.claims = claims
	return r
}

// named is the claim of that name as read, nil for none.
func (r cacheClaims) named(name string) *detect.CacheClaim {
	for _, c := range r.claims {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// boundIn is the claim of a zone among those Bound there: the one named
// after the zone first, else the first by name — the one claim of before,
// Bound in the zone its first predictor was in. Nil when none is Bound
// there.
func (r cacheClaims) boundIn(zone string) *detect.CacheClaim {
	if c := r.named(detect.ZoneClaimName(r.base, zone)); c != nil && c.BoundIn(zone) {
		return c
	}
	for _, c := range r.claims {
		if c.BoundIn(zone) {
			return c
		}
	}
	return nil
}

// zoneClaim is the claim of a zone — the one Bound there, else the claim
// named after the zone — by name, and as read when it exists (nil when it
// does not exist yet).
func (r cacheClaims) zoneClaim(zone string) (string, *detect.CacheClaim) {
	if c := r.boundIn(zone); c != nil {
		return c.Name, c
	}
	name := detect.ZoneClaimName(r.base, zone)
	return name, r.named(name)
}

// others are the claims other than the named one.
func (r cacheClaims) others(name string) []*detect.CacheClaim {
	var others []*detect.CacheClaim
	for _, c := range r.claims {
		if c.Name != name {
			others = append(others, c)
		}
	}
	return others
}

// ref is a claim's `namespace/name` for the answer.
func (r cacheClaims) ref(name string) string { return r.namespace + "/" + name }

// describeClaims words claims for a sentence, each with where it stands:
// `model-serving/hf-cache (Bound in eu-central-1b), model-serving/hf-cache-eu-central-1a (Pending)`.
func describeClaims(claims []*detect.CacheClaim) string {
	parts := make([]string, 0, len(claims))
	for _, c := range claims {
		parts = append(parts, fmt.Sprintf("%s (%s)", c, claimWhere(c)))
	}
	return strings.Join(parts, ", ")
}

// claimWhere is where a claim stands, in a few words: `Bound in
// eu-central-1b`, `Bound, volume pvc-1 names no zone`, `Pending`, `not
// Bound`, `not readable as you`.
func claimWhere(c *detect.CacheClaim) string {
	switch {
	case c.Error != "":
		return "not readable as you"
	case c.Phase == detect.ClaimBound && c.Zone != "":
		return "Bound in " + c.Zone
	case c.Phase == detect.ClaimBound && c.Volume != "":
		return "Bound, volume " + c.Volume + " names no zone"
	default:
		return claimPhase(c)
	}
}

// claimPhase is the claim's phase for a sentence; "not Bound" when it has none.
func claimPhase(claim *detect.CacheClaim) string {
	if claim.Phase == "" {
		return "not Bound"
	}
	return claim.Phase
}

// otherClaimsNote is the clause naming the claims a pool's slice does not
// mount, when there are any: each is another zone's, and Helm keeps it.
func otherClaimsNote(read cacheClaims, mounted string) string {
	others := read.others(mounted)
	if len(others) == 0 {
		return ""
	}
	return fmt.Sprintf("; the other claims — %s — are left as they are, each its zone's", describeClaims(others))
}

// zoneChoice is the caller's part of the pin: the zones create_node_pool was
// given (none for the claims to decide), and whether the slice's predictors
// mount the cache at all (cache false: no claim is mounted, so no zone
// follows from one).
type zoneChoice struct {
	zones []string
	cache bool
}

// zonePin is what the pool's zones and cache come to: the zones its nodes
// are pinned to; the claim the slice mounts — its name, and the claim as
// read, nil while it does not exist yet —; the sentence the answer carries;
// and, when the claims could not be read and matter, the warning instead.
type zonePin struct {
	zones     []string
	cache     bool
	claimName string
	claim     *detect.CacheClaim
	note      string
	warning   string
}

// sliceCache is the pin's word for the slice a write composes: the cache on
// with the claim the predictors mount (compose.SliceSpec.CacheClaim), or off
// (compose.SliceSpec.NoCache).
type sliceCache struct {
	on    bool
	claim string
}

func (p zonePin) sliceCache() sliceCache { return sliceCache{on: p.cache, claim: p.claimName} }

// warnings appends the pin's warning, when there is one, to the create's.
func (p zonePin) warnings(warnings []string) []string {
	if p.warning == "" {
		return warnings
	}
	return append(warnings, p.warning)
}

// CacheZoneRefusal is the structured form of the zones refusals
// (Refused.CacheZone): the zones named against the model cache — several
// zones with the cache on (a claim is one volume in one zone; Claim is nil),
// or the zone's claim Bound elsewhere (Claim, Bound in ClaimZone) — with the
// ways out.
type CacheZoneRefusal struct {
	// Claim is the model cache claim of the zone named, as read, when it is
	// Bound outside it (ClaimZone); nil for the several-zones refusal.
	Claim     *detect.CacheClaim `json:"claim,omitempty"`
	ClaimZone string             `json:"claimZone,omitempty"`
	// Zones are the zones named on create.
	Zones []string `json:"zones"`
	// Remedies are the ways out, each a sentence.
	Remedies []string `json:"remedies"`
}

// CacheClaimsRefusal is the structured form of the claims refusal
// (Refused.CacheClaims): several model cache claims in the serving namespace
// and no zones named — the pool's slice mounts one claim, its zone's, so the
// zone has to be named.
type CacheClaimsRefusal struct {
	Claims   []*detect.CacheClaim `json:"claims"`
	Remedies []string             `json:"remedies"`
}

// zonePinFor decides the pool's pin and the claim its slice mounts from the
// claims as read on the cluster and the caller's choice
// (giantswarm/cluster-manager#59, #65, #71). With the cache off nothing
// follows from a claim: the zones named pin the pool, or nothing does, and
// the note says so. With one zone named it pins the pool and the slice
// mounts the zone's claim — the one Bound there, else the claim named after
// the zone, created by the chart where it does not exist yet; a claim named
// after the zone but Bound elsewhere is a refusal; a claim not Bound yet,
// naming no zone or not readable is said beside the pin. Several zones with
// the cache on are refused: a claim is one volume in one zone. With none
// named the claims decide: one claim — Bound, its zone pins the pool and the
// slice mounts it; not Bound or naming no zone, no pin and the note says what
// was found; not readable, no pin and a warning —; no claim, the base name
// and no pin; several claims, a refusal naming them and asking for zones.
func zonePinFor(read cacheClaims, cluster string, choice zoneChoice) (zonePin, error) {
	pin := zonePin{zones: choice.zones, cache: choice.cache}
	if !choice.cache {
		pin.note = noCacheNote(read, cluster, choice.zones)
		return pin, nil
	}
	switch len(choice.zones) {
	case 0:
		return claimsDecide(pin, read, cluster)
	case 1:
		return chosenZonePin(pin, read, cluster, choice.zones[0])
	default:
		return pin, severalZonesRefusal(read, choice.zones)
	}
}

// claimsDecide is the pin with the cache on and no zones named: the claims
// of the serving namespace decide, as they did before there was a claim per
// zone.
func claimsDecide(pin zonePin, read cacheClaims, cluster string) (zonePin, error) {
	pin.claimName = read.base
	switch {
	case read.err != "":
		pin.warning = fmt.Sprintf("the model cache claims of %s on %s cannot be read as you (%s): the pool's nodes are not pinned to a cache's zone, and the slice mounts %s — a cache claim is one volume, bound in one zone, and a node launched in another zone strands a predictor mounting it Pending; re-run once you may list the claims and their volumes, or name zones on create so the slice mounts that zone's claim", read.namespace, cluster, read.err, read.ref(read.base))
		return pin, nil
	case len(read.claims) == 0:
		return pin, nil
	case len(read.claims) > 1:
		return pin, severalClaimsRefusal(read, cluster)
	}
	claim := read.claims[0]
	pin.claim, pin.claimName = claim, claim.Name
	switch {
	case claim.Error != "":
		pin.warning = fmt.Sprintf("the model cache claim %s on %s cannot be read as you (%s): the pool's nodes are not pinned to the cache's zone — the cache is one volume, bound in one zone, and a node launched in another zone strands a predictor mounting it Pending; re-run once you may read the claim and its volume, or name zones on create so the slice mounts that zone's claim", claim, cluster, claim.Error)
	case claim.Phase != detect.ClaimBound || claim.Volume == "":
		pin.note = fmt.Sprintf("the model cache claim %s on %s is %s, bound to no volume yet: the pool's nodes are not pinned to a zone — the first predictor mounting the cache binds it to its node's zone, and every pool created after that without zones follows it", claim, cluster, claimPhase(claim))
	case claim.Zone == "":
		pin.note = fmt.Sprintf("the model cache claim %s on %s is bound to volume %s, whose node affinity names no zone: the pool's nodes are not pinned — a volume every zone reaches strands no predictor", claim, cluster, claim.Volume)
	default:
		pin.zones = []string{claim.Zone}
		pin.note = fmt.Sprintf("nodes pinned to %s: the model cache (claim %s, volume %s) lives there, and a node launched in another zone strands a predictor mounting it Pending; name zones on create to run the pool elsewhere — its slice then mounts that zone's claim (%s, created there and kept; the weights downloaded once more) —, or pick an accelerator offered in %s — a family not offered there fails its launch, named in list_node_pools' nodes step", claim.Zone, claim, claim.Volume, detect.ZoneClaimName(read.base, "<zone>"), claim.Zone)
	}
	return pin, nil
}

// chosenZonePin is the pin for the one zone the caller named while the cache
// is on: the zone pins the pool, and the slice mounts the zone's claim.
func chosenZonePin(pin zonePin, read cacheClaims, cluster, zone string) (zonePin, error) {
	pinned := fmt.Sprintf("nodes pinned to %s, the zone named on create", zone)
	name, claim := read.zoneClaim(zone)
	pin.claimName, pin.claim = name, claim
	ref := read.ref(name)
	switch {
	case read.err != "":
		pin.note = fmt.Sprintf("%s; the slice mounts the zone's model cache claim %s", pinned, ref)
		pin.warning = fmt.Sprintf("the model cache claims of %s on %s cannot be read as you (%s): whether a claim is Bound in %s, and which, cannot be told — the slice mounts %s, the claim named after the zone, which the connectivity chart creates where it does not exist; re-run once you may list the claims, or pass cache false so this pool's slice serves without one", read.namespace, cluster, read.err, zone, ref)
	case claim == nil:
		pin.note = fmt.Sprintf("%s; the slice mounts the zone's model cache claim %s, which does not exist yet: the connectivity chart creates it, the first predictor of the pool binds it to a volume in %s, and it is kept when the pool goes — a later pool in %s reuses it%s", pinned, ref, zone, zone, otherClaimsNote(read, name))
	case claim.Error != "":
		pin.note = fmt.Sprintf("%s; the slice mounts the zone's model cache claim %s", pinned, claim)
		pin.warning = fmt.Sprintf("the model cache claim %s on %s cannot be read as you (%s): whether its volume lies in %s cannot be told — the cache is one volume, bound in one zone, and a node launched in another zone strands a predictor mounting it Pending; re-run once you may read the claim and its volume, or pass cache false so this pool's slice serves without it", claim, cluster, claim.Error, zone)
	case claim.Phase != detect.ClaimBound || claim.Volume == "":
		pin.note = fmt.Sprintf("%s; the slice mounts the zone's model cache claim %s, %s and bound to no volume yet — the first predictor mounting it binds it to a volume in %s, and a later pool there reuses it%s", pinned, claim, claimPhase(claim), zone, otherClaimsNote(read, name))
	case claim.Zone == "":
		pin.note = fmt.Sprintf("%s; the slice mounts the model cache claim %s, bound to volume %s, whose node affinity names no zone — a volume every zone reaches strands no predictor%s", pinned, claim, claim.Volume, otherClaimsNote(read, name))
	case claim.Zone == zone:
		pin.note = fmt.Sprintf("%s; the slice mounts the zone's model cache claim %s (volume %s), Bound there — the weights and compiled graphs of the models served from it are kept, and a predictor mounting it needs its node in %s%s", pinned, claim, claim.Volume, zone, otherClaimsNote(read, name))
	default:
		return pin, claimElsewhereRefusal(cluster, zone, claim)
	}
	return pin, nil
}

// severalZonesRefusal refuses several zones with the cache on: a claim is one
// volume in one zone, and the slice mounts one claim.
func severalZonesRefusal(read cacheClaims, zones []string) error {
	named := strings.Join(zones, ", ")
	remedies := []string{
		fmt.Sprintf("name one zone — the slice then mounts that zone's model cache claim (the one Bound there, else %s, which the connectivity chart creates and keeps)", detect.ZoneClaimName(read.base, "<zone>")),
		"pass cache false, so this pool's slice serves without the cache across the zones — the weights land in each predictor pod's ephemeral storage",
	}
	return &ErrRefused{
		Reason: fmt.Sprintf("zones %s with the cache on: a model cache claim is one volume in one zone, and the pool's slice mounts one claim — a predictor launched outside the claim's zone sits Pending (`didn't match PersistentVolume's node affinity`); %s", named, strings.Join(remedies, ", or ")),
		Refused: &Refused{
			Nodes: []string{}, Models: []string{}, ReadFrom: readFromCluster,
			Hint:      "Name one zone, or pass cache false, and re-run.",
			CacheZone: &CacheZoneRefusal{Zones: zones, Remedies: remedies},
		},
	}
}

// severalClaimsRefusal refuses no zones with several claims: the slice
// mounts one claim, the zone's, and none is chosen for the person.
func severalClaimsRefusal(read cacheClaims, cluster string) error {
	remedies := []string{
		fmt.Sprintf("name zones with the one zone the pool runs in — the slice then mounts that zone's claim (the one Bound there, else %s, which the connectivity chart creates and keeps)", detect.ZoneClaimName(read.base, "<zone>")),
		"pass cache false, so this pool's slice serves without the cache — the weights land in each predictor pod's ephemeral storage, and the claims are left as they are",
	}
	return &ErrRefused{
		Reason: fmt.Sprintf("%s on %s has several model cache claims — %s: a pool's slice mounts one, the claim of the zone the pool runs in, and without zones none is chosen for you; %s", read.namespace, cluster, describeClaims(read.claims), strings.Join(remedies, ", or ")),
		Refused: &Refused{
			Nodes: []string{}, Models: []string{}, ReadFrom: readFromCluster,
			Hint:        "Name zones with the one zone the pool runs in, or pass cache false, and re-run.",
			CacheClaims: &CacheClaimsRefusal{Claims: read.claims, Remedies: remedies},
		},
	}
}

// claimElsewhereRefusal refuses the zone whose claim, named after it, is
// Bound in another zone: a pool node there cannot mount it.
func claimElsewhereRefusal(cluster, zone string, claim *detect.CacheClaim) error {
	remedies := []string{
		fmt.Sprintf("name %s as the zone", claim.Zone),
		"pass cache false, so this pool's slice serves without the cache — the weights land in the predictor pod's ephemeral storage and the claim is left as it is",
		"remove the claim (it costs the cached weights and compiled graphs)",
	}
	return &ErrRefused{
		Reason: fmt.Sprintf("zones %s: the model cache claim %s on %s, the zone's by name, is bound to volume %s in %s, outside it — a pool node launched in %s cannot mount the cache, and every predictor mounting it sits Pending (`didn't match PersistentVolume's node affinity`); %s", zone, claim, cluster, claim.Volume, claim.Zone, zone, strings.Join(remedies, ", or ")),
		Refused: &Refused{
			Nodes: []string{}, Models: []string{}, ReadFrom: readFromCluster,
			Hint:      "Name the claim's zone, pass cache false, or remove the claim, and re-run.",
			CacheZone: &CacheZoneRefusal{Claim: claim, ClaimZone: claim.Zone, Zones: []string{zone}, Remedies: remedies},
		},
	}
}

// noCacheNote is the zones sentence of a pool whose slice serves without
// the cache: the zones named pin the pool, or nothing does, and the claims —
// whatever their state — pin nothing since no predictor mounts them.
func noCacheNote(read cacheClaims, cluster string, zones []string) string {
	pinned := "the pool's nodes are not pinned to a zone"
	if len(zones) > 0 {
		pinned = fmt.Sprintf("nodes pinned to %s, the zones named on create", strings.Join(zones, ", "))
	}
	switch {
	case len(read.claims) == 0:
		return pinned + "; this pool's slice serves without the model cache (cache false), so no zone follows from a claim"
	case len(read.claims) == 1:
		return fmt.Sprintf("%s; this pool's slice serves without the model cache (cache false): no predictor mounts the claim %s on %s, so its zone pins nothing and the claim is left as it is", pinned, read.claims[0], cluster)
	default:
		return fmt.Sprintf("%s; this pool's slice serves without the model cache (cache false): no predictor mounts the claims %s on %s, so their zones pin nothing and they are left as they are", pinned, describeClaims(read.claims), cluster)
	}
}

// CacheSetting is the answer's word on the model cache for the slice the
// write composed (create_node_pool, enable_model_serving): whether the
// predictors mount a cache claim, which one, and what follows.
type CacheSetting struct {
	Enabled bool `json:"enabled"`
	// Claim names the claim the predictors mount (`namespace/name`); empty
	// when the cache is off.
	Claim string `json:"claim,omitempty"`
	Note  string `json:"note"`
}

// cacheSettingFor is the answer's cache block for the slice a write composed;
// nil when none was.
func (s *Service) cacheSettingFor(slice *SliceRelease, read cacheClaims, pin zonePin) *CacheSetting {
	if slice == nil {
		return nil
	}
	return cacheSetting(read, pin)
}

// cacheSetting words the cache setting: on, the claim the slice's predictors
// mount and where it stands — not existing yet (the connectivity chart
// creates it), Bound in its zone, or as read; off, the slice's
// modelServing.cache.enabled false, the weights in the pod's ephemeral
// storage, no zone following from a claim — and the claims as read, when
// there are any, left as they are.
func cacheSetting(read cacheClaims, pin zonePin) *CacheSetting {
	if pin.cache {
		ref := read.ref(pin.claimName)
		note := "the predictors mount the model cache claim " + ref
		switch {
		case pin.claim == nil:
			note += " — it does not exist yet: the connectivity chart creates it and keeps it, and the first predictor binds it to a volume in its node's zone"
		case pin.claim.Error == "" && pin.claim.BoundIn(pin.claim.Zone) && pin.claim.Zone != "":
			note += fmt.Sprintf(" — Bound in %s (volume %s), kept by the connectivity chart", pin.claim.Zone, pin.claim.Volume)
		default:
			note += fmt.Sprintf(" (%s; the connectivity chart keeps it)", claimWhere(pin.claim))
		}
		note += ": the weights and compiled graphs of every model served from this slice are kept there, and a pool created in the claim's zone with the cache on reuses it; the slice release is the cluster's one, so every predictor of the cluster mounts this claim from now on"
		return &CacheSetting{Enabled: true, Claim: ref, Note: note}
	}
	note := "modelServing.cache.enabled false on the slice release: no claim is applied or mounted, every predictor downloads its weights into its pod's ephemeral storage (the node's local disk) at each start, and no zone pin follows from a claim; a re-run with cache true is the slice's upgrade back to the cache"
	switch {
	case len(read.claims) == 1 && read.claims[0].Error == "":
		claim := read.claims[0]
		note += fmt.Sprintf("; the existing claim %s (%s", claim, claimPhase(claim))
		if claim.Volume != "" {
			note += ", volume " + claim.Volume
		}
		if claim.Zone != "" {
			note += " in " + claim.Zone
		}
		note += ") is left as it is — Helm keeps it — and pins nothing"
	case len(read.claims) > 1:
		note += fmt.Sprintf("; the existing claims %s are left as they are — Helm keeps them — and pin nothing", describeClaims(read.claims))
	}
	return &CacheSetting{Note: note}
}
