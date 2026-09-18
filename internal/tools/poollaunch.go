package tools

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/detect"
)

// Karpenter's refusal to launch a node of the pool, as list_node_pools shows
// it (giantswarm/cluster-manager#55). A NodeClaim it cannot launch carries
// the refusal as a condition — Launched or Registered False with the reason
// and message — for as long as Karpenter keeps the claim. For no capacity
// (every size of the pool refused in every zone) and for a NodeClass not
// ready it keeps nothing: it publishes a Warning event on the claim and
// deletes the claim in the same second, so the pool shows no NodeClaim at
// all while the placeholder or a predictor waits for a node that cannot
// come. The event outlives the claim by the API server's event TTL, an hour
// by default, in the default namespace where the events of a cluster-scoped
// object go; a refusal counts until Karpenter creates a claim after it (the
// retry speaks for itself then).
//
// The event carries AWS's answer per size and zone, cut by Karpenter after
// the first (giantswarm/cluster-manager#65): what it lacks is read from the
// pool release and the cluster — Karpenter gives a claim up for capacity only
// once every size of the pool was refused in every zone it may use, the pin
// or the cluster's node-subnet zones (giantswarm/cluster-manager#75) — and
// the way out points at what decided where the launch was tried: the zones
// the pool is pinned to, and the model cache claim when its zone is the pin;
// it never points at a zone the same answer refused.

// The NodeClaim conditions that decide whether a node came: Launched (the
// instance exists) and Registered (the node joined the cluster).
var launchConditions = []string{"Launched", "Registered"}

// Karpenter's event reasons for a claim it gave up on and deleted at once.
const (
	reasonInsufficientCapacity = "InsufficientCapacityError"
	reasonNodeClassNotReady    = "NodeClassNotReady"
)

var launchRefusalEvents = map[string]bool{reasonInsufficientCapacity: true, reasonNodeClassNotReady: true}

const (
	eventTypeWarning  = "Warning"
	eventPrefixFormat = "NodeClaim %s event: "
	kindNodeClaim     = "NodeClaim"
)

// The shapes of AWS's answer in Karpenter's message: the code, the instance
// type and the zone, once per size and zone refused (capacityRefusal); and
// the zones AWS names as having the capacity, when the message still carries
// them (capacityElsewhere).
var (
	capacityRefusal   = regexp.MustCompile(`(\w+): We currently do not have sufficient (\S+) capacity in the Availability Zone you requested \(([^)]+)\)`)
	capacityElsewhere = regexp.MustCompile(`by not specifying an Availability Zone in your request or choosing ([a-z0-9-]+(?:, [a-z0-9-]+)*)\.`)
)

// launchContext is what the pool release and the cluster say about the node
// Karpenter launches: the instance types of the pool's sizes, the zones its
// nodes are pinned to (pool.zones), the zones of the cluster's node subnets
// — every zone a pool without a pin may launch in, its EC2NodeClass selecting
// the node subnets — and the model cache claims on the cluster when the pool
// is pinned — so the refusal names what the truncated message cannot, and
// the remedy what decided where the launch was tried and what a move costs.
// Empty for a pool without a release, or one that launches its nodes.
type launchContext struct {
	instanceTypes []string
	zones         []string
	clusterZones  []string
	claims        cacheClaims
}

// allowed are the zones Karpenter may launch the pool's nodes in: the pin,
// else every node-subnet zone of the cluster; none while neither is known
// (the AWSCluster not readable).
func (lc launchContext) allowed() []string {
	if lc.pinned() {
		return lc.zones
	}
	return lc.clusterZones
}

// pinned: the pool's nodes are pinned to zones.
func (lc launchContext) pinned() bool { return len(lc.zones) > 0 }

// launchContext reads the context of a pool's refusals, only while it has
// one: the sizes and zones from the release's values, the cluster's
// node-subnet zones as list_node_pools read them, the cache claims on the
// cluster when the pool is pinned to zones.
func (s *Service) launchContext(ctx context.Context, t target, release *unstructured.Unstructured, live *poolLive, clusterZones []string) launchContext {
	if release == nil || live == nil || len(live.failures) == 0 {
		return launchContext{}
	}
	lc := launchContext{zones: poolZones(release), clusterZones: clusterZones}
	accelerator, sizes := poolValues(release)
	if shapes, err := compose.Shapes(accelerator, sizes); err == nil {
		for _, shape := range shapes {
			lc.instanceTypes = append(lc.instanceTypes, shape.InstanceType)
		}
	}
	if len(lc.zones) > 0 {
		lc.claims = s.readCacheClaims(ctx, t)
	}
	return lc
}

// remedy is the way around the refusal r — shown, never chosen for the
// person. For a pool that may use every zone: wider sizes or another
// accelerator; when every zone was refused there is no zone to move to — a
// re-run with zones would only pin the pool to a refused one — and
// Karpenter's retry brings the node on its own once the family has capacity
// again (every few minutes: its unavailable-offering cache is three minutes;
// on an installation the third claim launched six minutes after the first
// refusal with nothing changed). For a pool pinned to zones (the zone named
// on create, or the one model cache claim's): the pin, the claim living in
// it when one does, and the re-run that moves the pool — naming as
// candidates only the cluster's node-subnet zones the answer did not refuse
// (capacity there is not promised) — to another zone, whose claim the slice
// then mounts (giantswarm/cluster-manager#71), or without the cache; with
// every zone of the cluster refused, again no zone to move to.
func (lc launchContext) remedy(r refused) string {
	const (
		widen = "wider sizes or another accelerator (a re-run of create_node_pool) give it more to choose from"
		retry = "Karpenter retries every few minutes (its unavailable-offering cache is three minutes) and the pool launches its node on its own once the family has capacity again"
	)
	others := difference(lc.clusterZones, r.zones)
	if !lc.pinned() {
		if r.everyZone {
			return "every zone the pool may use was refused in the same answer, so there is no zone to move to (zones on a re-run would only pin the pool to one of them); " + retry + "; " + widen
		}
		return widen
	}
	msg := "the pool is pinned to " + strings.Join(lc.zones, ", ")
	for _, zone := range lc.zones {
		if claim := lc.claims.boundIn(zone); claim != nil {
			msg += fmt.Sprintf(" — the model cache claim %s (volume %s) lives in %s and the pool's slice mounts it", claim, claim.Volume, zone)
		}
	}
	if len(others) == 0 && len(lc.clusterZones) > 0 {
		return fmt.Sprintf("%s: every node-subnet zone of the cluster was refused in the same answer, so there is no zone to move to; %s; %s", msg, retry, widen)
	}
	move := "re-run create_node_pool on the pool with zones naming one zone with capacity"
	if len(others) > 0 {
		move += fmt.Sprintf(" — not refused in this answer: %s (the cluster's other node-subnet %s; capacity there is not promised)", strings.Join(others, ", "), plural(len(others), "zone"))
	}
	return fmt.Sprintf("%s: %s — with the cache on its slice then mounts that zone's model cache claim (the one Bound there, else %s, created there and kept; the weights downloaded once more), with cache false it serves from the node's local disk; %s", msg, move, detect.ZoneClaimName(lc.claims.base, "<zone>"), widen)
}

// poolZones are the zones a pool release pins its nodes to (pool.zones);
// none for no pin.
func poolZones(hr *unstructured.Unstructured) []string {
	zones, _, _ := unstructured.NestedStringSlice(hr.Object, "spec", "values", "pool", "zones")
	return zones
}

// launchFailure is one node of the pool Karpenter could not launch.
type launchFailure struct {
	// claim is the NodeClaim's name.
	claim string
	// reason and message are Karpenter's words, from the condition or the
	// event: the message with its whitespace collapsed and the event's
	// "NodeClaim <name> event: " prefix removed.
	reason, message string
	// at is when Karpenter said so (RFC3339): the condition's transition,
	// the event's last occurrence.
	at string
}

// refused is AWS's capacity answer behind a launch failure as a set: the
// codes, the instance types and the zones refused. Read from every fleet
// error Karpenter's message carries — the claim's condition or its event,
// one error per instance type and zone AWS tried — and, when Karpenter gave
// the claim up for capacity (reasonInsufficientCapacity: it does so only once
// every offering it asked for was refused, and cuts its event after the
// first error), completed to every size of the pool in every zone the pool
// may use (giantswarm/cluster-manager#65, #75). On an installation one
// answer refused three sizes in three zones, nine errors with the event
// naming the first, and the step named that error's zone as the zone.
type refused struct {
	codes, types, zones []string
	// everySize and everyZone say the types, the zones, are the pool's whole
	// set rather than what the message happened to name.
	everySize, everyZone bool
}

// refused reads the failure's refusal set in the pool's context.
func (f launchFailure) refused(lc launchContext) refused {
	var r refused
	for _, m := range capacityRefusal.FindAllStringSubmatch(f.message, -1) {
		r.codes, r.types, r.zones = appendNew(r.codes, m[1]), appendNew(r.types, m[2]), appendNew(r.zones, m[3])
	}
	if len(r.codes) == 0 || f.reason != reasonInsufficientCapacity {
		return r
	}
	if len(lc.instanceTypes) > 0 {
		r.types, r.everySize = union(lc.instanceTypes, r.types), true
	}
	if allowed := lc.allowed(); len(allowed) > 0 {
		r.zones, r.everyZone = union(allowed, r.zones), true
	}
	return r
}

// line is the refusal in a line: the codes, the sizes and the zones —
// `InsufficientInstanceCapacity for every size of the pool (g6e.2xlarge,
// g6e.4xlarge, g6e.8xlarge) in every zone the pool may use (eu-central-1a,
// eu-central-1b, eu-central-1c — the cluster's node-subnet zones; the pool
// has no pin)`, for a pinned pool `in eu-central-1b, the zone the pool is
// pinned to`; while the zones the pool may use cannot be read, the message's
// zones, said to be the first fleet error's only.
func (r refused) line(lc launchContext, reason string) string {
	types := strings.Join(r.types, ", ")
	if r.everySize {
		types = "every size of the pool (" + types + ")"
	}
	zones := strings.Join(r.zones, ", ")
	switch {
	case r.everyZone && lc.pinned():
		zones += ", the " + plural(len(r.zones), "zone") + " the pool is pinned to"
	case r.everyZone:
		zones = "every zone the pool may use (" + zones + " — the cluster's node-subnet zones; the pool has no pin)"
	case reason == reasonInsufficientCapacity:
		zones += " at least (Karpenter's event names the first fleet error only, and the zones the pool may use could not be read)"
	}
	return strings.Join(r.codes, ", ") + " for " + types + " in " + zones
}

// summary is the refusal in a line (refused.line) when Karpenter's message
// carries AWS's capacity answer, else the reason.
func (f launchFailure) summary(lc launchContext) string {
	r := f.refused(lc)
	if len(r.codes) == 0 {
		return f.reason
	}
	return r.line(lc, f.reason)
}

// elsewhere are the zones AWS named as having the capacity — its per-error
// hint, when Karpenter's message still carries it — outside the refused set
// r: the hint is per fleet error and the next error of the same answer
// refuses the zone it names (on an installation every hint of a nine-error
// answer named zones refused two errors later), so a refused zone is never
// relayed; none when the message was cut before the hint or every zone it
// names was refused; sorted.
func (f launchFailure) elsewhere(r refused) []string {
	var out []string
	for _, m := range capacityElsewhere.FindAllStringSubmatch(f.message, -1) {
		for _, zone := range strings.Split(m[1], ", ") {
			if !slices.Contains(r.zones, zone) {
				out = appendNew(out, zone)
			}
		}
	}
	slices.Sort(out)
	return out
}

// appendNew appends s unless list has it.
func appendNew(list []string, s string) []string {
	if slices.Contains(list, s) {
		return list
	}
	return append(list, s)
}

// union is a followed by what b adds, in order, without duplicates.
func union(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	for _, s := range a {
		out = appendNew(out, s)
	}
	for _, s := range b {
		out = appendNew(out, s)
	}
	return out
}

// difference is a without what b has, in order.
func difference(a, b []string) []string {
	var out []string
	for _, s := range a {
		if !slices.Contains(b, s) {
			out = appendNew(out, s)
		}
	}
	return out
}

// plural is word, or its plural for a count other than one.
func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// launchFailures collects Karpenter's refusals on the pool's NodeClaims —
// the claims that carry one and the refusal events of claims it deleted —
// oldest first.
func launchFailures(pool string, claims []*unstructured.Unstructured, events []unstructured.Unstructured) []launchFailure {
	var out []launchFailure
	for _, claim := range claims {
		if f, refused := claimLaunchFailure(claim); refused {
			out = append(out, f)
		}
	}
	out = append(out, eventLaunchFailures(pool, claims, events)...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].at < out[j].at })
	return out
}

// claimLaunchFailure reads a NodeClaim's refusal: its Launched, else
// Registered, condition False.
func claimLaunchFailure(claim *unstructured.Unstructured) (launchFailure, bool) {
	for _, typ := range launchConditions {
		if cond, found := detect.ConditionOf(claim, typ); found && cond.Status == "False" {
			return launchFailure{claim: claim.GetName(), reason: cond.Reason, message: trimMessage(claim.GetName(), cond.Message), at: cond.LastTransitionTime}, true
		}
	}
	return launchFailure{}, false
}

// eventLaunchFailures reads the refusal events of the pool's claims: a
// Warning of a refusal reason on a NodeClaim named after the pool (Karpenter
// names a claim `<nodepool>-<five characters>`), unless Karpenter created
// another claim of the pool after it — the retry says where the pool stands.
func eventLaunchFailures(pool string, claims []*unstructured.Unstructured, events []unstructured.Unstructured) []launchFailure {
	named := regexp.MustCompile(`^` + regexp.QuoteMeta(pool) + `-[a-z0-9]{5}$`)
	var out []launchFailure
	for i := range events {
		ev := &events[i]
		claim := nestedString(ev, "involvedObject", "name")
		if nestedString(ev, "type") != eventTypeWarning || !launchRefusalEvents[nestedString(ev, "reason")] || nestedString(ev, "involvedObject", "kind") != kindNodeClaim || !named.MatchString(claim) {
			continue
		}
		at := eventTime(ev)
		if retried(claims, claim, at) {
			continue
		}
		out = append(out, launchFailure{claim: claim, reason: nestedString(ev, "reason"), message: trimMessage(claim, nestedString(ev, "message")), at: at})
	}
	return out
}

// retried: a claim of the pool other than the refused one was created after
// the refusal.
func retried(claims []*unstructured.Unstructured, refused, at string) bool {
	for _, c := range claims {
		if c.GetName() != refused && detect.Timestamp(c.GetCreationTimestamp().Time) > at {
			return true
		}
	}
	return false
}

// eventTime is when an event last happened: lastTimestamp, else eventTime,
// else its creation.
func eventTime(ev *unstructured.Unstructured) string {
	for _, field := range []string{"lastTimestamp", "eventTime"} {
		if t := nestedString(ev, field); t != "" {
			return t
		}
	}
	return detect.Timestamp(ev.GetCreationTimestamp().Time)
}

// trimMessage collapses Karpenter's message to one line and drops the
// event's prefix naming the claim, which the step names itself.
func trimMessage(claim, message string) string {
	return strings.TrimPrefix(strings.Join(strings.Fields(message), " "), fmt.Sprintf(eventPrefixFormat, claim))
}

// launchFailureMessage words the refusals for the nodes step: how many
// claims could not launch and the last one, the refusal in a line — every
// size and every zone refused, the zones AWS named as having capacity when
// the message carries them and the answer did not refuse them — with
// Karpenter's reason and message verbatim, and the way around it
// (launchContext.remedy) — the refusal is shown, the person decides; no size
// or zone is ever chosen for them, and no zone the answer refused is ever
// pointed at. The pool's other claims, when it has any, follow.
func launchFailureMessage(failures []launchFailure, launching, ready, terminating int, lc launchContext) string {
	last := failures[len(failures)-1]
	claims := plural(len(failures), "NodeClaim")
	r := last.refused(lc)
	refusal := last.reason
	if len(r.codes) > 0 {
		refusal = r.line(lc, last.reason) + " (" + last.reason + ")"
	}
	if elsewhere := last.elsewhere(r); len(elsewhere) > 0 {
		refusal += "; AWS named " + strings.Join(elsewhere, ", ") + " as having the capacity"
	}
	msg := fmt.Sprintf("%d %s could not launch, the last (%s) at %s — %s; Karpenter: %q; it retries while a pod waits — %s",
		len(failures), claims, last.claim, last.at, refusal, last.message, lc.remedy(r))
	if launching+ready+terminating > 0 {
		msg += fmt.Sprintf("; besides, %d NodeClaim(s) launching, %d ready, %d terminating", launching, ready, terminating)
	}
	return msg
}
