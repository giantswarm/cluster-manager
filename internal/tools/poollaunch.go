package tools

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

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

// The NodeClaim conditions that decide whether a node came: Launched (the
// instance exists) and Registered (the node joined the cluster).
var launchConditions = []string{"Launched", "Registered"}

// Karpenter's event reasons for a claim it gave up on and deleted at once.
var launchRefusalEvents = map[string]bool{"InsufficientCapacityError": true, "NodeClassNotReady": true}

const (
	eventTypeWarning  = "Warning"
	eventPrefixFormat = "NodeClaim %s event: "
	kindNodeClaim     = "NodeClaim"
)

// capacityRefusal is the shape of AWS's answer in Karpenter's message: the
// code, the instance type and the zone, once per size and zone refused.
var capacityRefusal = regexp.MustCompile(`(\w+): We currently do not have sufficient (\S+) capacity in the Availability Zone you requested \(([^)]+)\)`)

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
// the reason.
func (f launchFailure) summary() string {
	var codes, sizes, zones []string
	for _, m := range capacityRefusal.FindAllStringSubmatch(f.message, -1) {
		codes, sizes, zones = appendNew(codes, m[1]), appendNew(sizes, m[2]), appendNew(zones, m[3])
	}
	if len(codes) == 0 {
		return f.reason
	}
	return strings.Join(codes, ", ") + " for " + strings.Join(sizes, ", ") + " in " + strings.Join(zones, ", ")
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
// claims could not launch and the last one, the refusal in a line with
// Karpenter's reason and message verbatim, and the way around it — the
// refusal is shown, the person decides; no size or zone is ever chosen for
// them. The pool's other claims, when it has any, follow.
func launchFailureMessage(failures []launchFailure, launching, ready, terminating int) string {
	last := failures[len(failures)-1]
	claims := "NodeClaim"
	if len(failures) > 1 {
		claims = "NodeClaims"
	}
	refusal := last.summary()
	if refusal != last.reason {
		refusal += " (" + last.reason + ")"
	}
	msg := fmt.Sprintf("%d %s could not launch, the last (%s) at %s — %s; Karpenter: %q; it retries while a pod waits — wider sizes or another accelerator (a re-run of create_node_pool) give it more to choose from",
		len(failures), claims, last.claim, last.at, refusal, last.message)
	if launching+ready+terminating > 0 {
		msg += fmt.Sprintf("; besides, %d NodeClaim(s) launching, %d ready, %d terminating", launching, ready, terminating)
	}
	return msg
}
