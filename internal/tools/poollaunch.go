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
// pool release — Karpenter gives a claim up for capacity only once every size
// of the pool was refused in every zone it may use — and the way out points
// at what decided where the launch was tried: the zones the pool is pinned
// to, and the model cache claim when its zone is the pin.

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

// launchContext is what the pool release says about the node Karpenter
// launches: the instance types of the pool's sizes, the zones its nodes are
// pinned to (pool.zones), and the model cache claim on the cluster when the
// pool is pinned — so the refusal names what the truncated message cannot,
// and the remedy what decided where the launch was tried. Empty for a pool
// without a release, or one that launches its nodes.
type launchContext struct {
	instanceTypes []string
	zones         []string
	claim         *detect.CacheClaim
}

// launchContext reads the context of a pool's refusals, only while it has
// one: the sizes and zones from the release's values, the cache claim on the
// cluster when the pool is pinned to zones.
func (s *Service) launchContext(ctx context.Context, t target, release *unstructured.Unstructured, live *poolLive) launchContext {
	if release == nil || live == nil || len(live.failures) == 0 {
		return launchContext{}
	}
	lc := launchContext{zones: poolZones(release)}
	accelerator, sizes := poolValues(release)
	if shapes, err := compose.Shapes(accelerator, sizes); err == nil {
		for _, shape := range shapes {
			lc.instanceTypes = append(lc.instanceTypes, shape.InstanceType)
		}
	}
	if len(lc.zones) > 0 {
		lc.claim = s.cacheClaim(ctx, t)
	}
	return lc
}

// pinnedByClaim: the pool's one zone is the model cache claim's.
func (lc launchContext) pinnedByClaim() bool {
	return lc.claim != nil && lc.claim.Zone != "" && len(lc.zones) == 1 && lc.zones[0] == lc.claim.Zone
}

// remedy is the way around the refusal — shown, never chosen for the
// person: wider sizes or another accelerator for a pool that may use every
// zone; for a pool pinned by the model cache claim, the pin and its two
// ways out; for a pool pinned by the zones named on create, other zones —
// with cache false where the claim's zone is among the pinned ones.
func (lc launchContext) remedy() string {
	const widen = "wider sizes or another accelerator (a re-run of create_node_pool) give it more to choose from"
	switch {
	case len(lc.zones) == 0:
		return widen
	case lc.pinnedByClaim():
		return fmt.Sprintf("the pool is pinned to %s by the model cache claim %s (volume %s lives there): re-run create_node_pool on the pool with zones naming a zone with capacity and cache false — this pool's slice then serves without the cache, the weights in the pod's ephemeral storage —, or remove the claim (it costs the cached weights and compiled graphs); %s", lc.zones[0], lc.claim, lc.claim.Volume, widen)
	}
	msg := fmt.Sprintf("the pool is pinned to %s by the zones named on create: re-run create_node_pool with zones naming a zone with capacity", strings.Join(lc.zones, ", "))
	if lc.claim != nil && lc.claim.Zone != "" && slices.Contains(lc.zones, lc.claim.Zone) {
		msg += fmt.Sprintf(" — the model cache claim %s lives in %s, so zones without it take cache false", lc.claim, lc.claim.Zone)
	}
	return msg + "; " + widen
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

// summary is the refusal in a line: the code, sizes and zones when
// Karpenter's message carries AWS's capacity answer
// (`InsufficientInstanceCapacity for g6e.2xlarge in eu-central-1a`), else
// the reason. Karpenter cuts the event after the first size and gives a
// claim up for capacity only once every size of the pool was refused in
// every zone it may use: the sizes the message lacks are the pool's, said
// so, and the zones the pool's pin when it has one.
func (f launchFailure) summary(lc launchContext) string {
	var codes, sizes, zones []string
	for _, m := range capacityRefusal.FindAllStringSubmatch(f.message, -1) {
		codes, sizes, zones = appendNew(codes, m[1]), appendNew(sizes, m[2]), appendNew(zones, m[3])
	}
	if len(codes) == 0 {
		return f.reason
	}
	refused := strings.Join(sizes, ", ")
	if f.reason == reasonInsufficientCapacity {
		if len(lc.instanceTypes) > len(sizes) {
			refused = "every size of the pool (" + strings.Join(lc.instanceTypes, ", ") + ")"
		}
		if len(lc.zones) > len(zones) {
			zones = lc.zones
		}
	}
	return strings.Join(codes, ", ") + " for " + refused + " in " + strings.Join(zones, ", ")
}

// elsewhere are the zones AWS named as having the capacity, when Karpenter's
// message still carries them; none when it was cut before.
func (f launchFailure) elsewhere() []string {
	m := capacityElsewhere.FindStringSubmatch(f.message)
	if m == nil {
		return nil
	}
	return strings.Split(m[1], ", ")
}

// appendNew appends s unless list has it.
func appendNew(list []string, s string) []string {
	if slices.Contains(list, s) {
		return list
	}
	return append(list, s)
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
// size refused, the zones tried, the zones AWS named as having capacity when
// the message carries them — with Karpenter's reason and message verbatim,
// and the way around it (launchContext.remedy) — the refusal is shown, the
// person decides; no size or zone is ever chosen for them. The pool's other
// claims, when it has any, follow.
func launchFailureMessage(failures []launchFailure, launching, ready, terminating int, lc launchContext) string {
	last := failures[len(failures)-1]
	claims := "NodeClaim"
	if len(failures) > 1 {
		claims = "NodeClaims"
	}
	refusal := last.summary(lc)
	if refusal != last.reason {
		refusal += " (" + last.reason + ")"
	}
	if elsewhere := last.elsewhere(); len(elsewhere) > 0 {
		refusal += "; AWS named " + strings.Join(elsewhere, ", ") + " as having the capacity"
	}
	msg := fmt.Sprintf("%d %s could not launch, the last (%s) at %s — %s; Karpenter: %q; it retries while a pod waits — %s",
		len(failures), claims, last.claim, last.at, refusal, last.message, lc.remedy())
	if launching+ready+terminating > 0 {
		msg += fmt.Sprintf("; besides, %d NodeClaim(s) launching, %d ready, %d terminating", launching, ready, terminating)
	}
	return msg
}
