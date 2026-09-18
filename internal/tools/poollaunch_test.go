package tools

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/cluster-manager/internal/detect"
)

// The pool's claims and events as the tests fake them.
func fakeClaim(name, created string, conditions ...map[string]any) *unstructured.Unstructured {
	conds := make([]any, 0, len(conditions))
	for _, c := range conditions {
		conds = append(conds, c)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "karpenter.sh/v1", "kind": "NodeClaim",
		"metadata": map[string]any{"name": name, "creationTimestamp": created, "labels": map[string]any{"karpenter.sh/nodepool": "mc-gpu-l40s"}},
		"status":   map[string]any{"conditions": conds},
	}}
}

func condition(typ, status, reason, message, at string) map[string]any {
	return map[string]any{"type": typ, "status": status, "reason": reason, "message": message, "lastTransitionTime": at}
}

func fakeEvent(claim, reason, typ, at, message string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Event",
		"metadata":       map[string]any{"name": claim + ".18d64909b0856ac3", "namespace": "default"},
		"involvedObject": map[string]any{"apiVersion": "karpenter.sh/v1", "kind": "NodeClaim", "name": claim},
		"reason":         reason, "type": typ, "lastTimestamp": at, "message": message,
	}}
}

const iceMessage = "NodeClaim mc-gpu-l40s-txjc8 event: creating instance, insufficient capacity, with fleet error(s), InsufficientInstanceCapacity: We currently do not have sufficient g6e.2xlarge capacity in the Availability Zone you requested (eu-central-1a). Our system will be working on provisioning additional capacity. You can currently get g6e.2xla..."

// TestLaunchFailures (giantswarm/cluster-manager#55): Karpenter's refusals to
// launch a node of the pool are read from the claims that carry one and
// from the refusal events of the claims it deleted at once — a Warning of a
// refusal reason on a NodeClaim named after the pool, unless a later claim
// of the pool says Karpenter retried —, oldest first, Karpenter's words
// trimmed to a line without the event's prefix. Anything else on the
// events — another pool's claim, a Normal note, a termination warning — is
// not a refusal.
func TestLaunchFailures(t *testing.T) {
	launchFailed := fakeClaim("mc-gpu-l40s-k9d2m", "2026-09-18T06:10:00Z",
		condition("Launched", "False", "LaunchFailed", "creating instance, with fleet error(s),\n  UnauthorizedOperation: You are not authorized to perform this operation.", "2026-09-18T06:10:04Z"),
		condition("Registered", "Unknown", "NotLaunched", "Node not launched", "2026-09-18T06:10:00Z"))
	launching := fakeClaim("mc-gpu-l40s-a1b2c", "2026-09-18T06:30:00Z", condition("Launched", "Unknown", "AwaitingReconciliation", "", "2026-09-18T06:30:00Z"))
	ice := fakeEvent("mc-gpu-l40s-txjc8", "InsufficientCapacityError", "Warning", "2026-09-18T06:00:27Z", iceMessage)
	iceAgain := fakeEvent("mc-gpu-l40s-zgbdh", "InsufficientCapacityError", "Warning", "2026-09-18T06:03:37Z", "NodeClaim mc-gpu-l40s-zgbdh event: creating instance, insufficient capacity, with fleet error(s), InsufficientInstanceCapacity: We currently do not have sufficient g6e.2xlarge capacity in the Availability Zone you requested (eu-central-1c).")
	decoys := []unstructured.Unstructured{
		fakeEvent("mc-gpu-l40s-txjc8", "TerminationGracePeriodExpiring", "Warning", "2026-09-18T06:00:28Z", "All pods will be deleted by 2026-09-18T06:30:27Z"),
		fakeEvent("mc-karpenter-7b4gq", "InsufficientCapacityError", "Warning", "2026-09-18T06:01:00Z", "another pool's refusal"),
		fakeEvent("mc-gpu-l40s-other-xy12z", "InsufficientCapacityError", "Warning", "2026-09-18T06:01:00Z", "a pool whose name starts like ours"),
		fakeEvent("mc-gpu-l40s-7b4gq", "Unconsolidatable", "Normal", "2026-09-18T06:02:00Z", "Can't replace with a cheaper node"),
	}

	t.Run("events of deleted claims, oldest first", func(t *testing.T) {
		got := launchFailures("mc-gpu-l40s", nil, append(decoys, ice, iceAgain))
		assert.Equal(t, []launchFailure{
			{claim: "mc-gpu-l40s-txjc8", reason: "InsufficientCapacityError", at: "2026-09-18T06:00:27Z", message: "creating instance, insufficient capacity, with fleet error(s), InsufficientInstanceCapacity: We currently do not have sufficient g6e.2xlarge capacity in the Availability Zone you requested (eu-central-1a). Our system will be working on provisioning additional capacity. You can currently get g6e.2xla..."},
			{claim: "mc-gpu-l40s-zgbdh", reason: "InsufficientCapacityError", at: "2026-09-18T06:03:37Z", message: "creating instance, insufficient capacity, with fleet error(s), InsufficientInstanceCapacity: We currently do not have sufficient g6e.2xlarge capacity in the Availability Zone you requested (eu-central-1c)."},
		}, got)
	})
	t.Run("a claim carrying the refusal, its message on one line", func(t *testing.T) {
		got := launchFailures("mc-gpu-l40s", []*unstructured.Unstructured{launchFailed}, nil)
		assert.Equal(t, []launchFailure{{claim: "mc-gpu-l40s-k9d2m", reason: "LaunchFailed", at: "2026-09-18T06:10:04Z", message: "creating instance, with fleet error(s), UnauthorizedOperation: You are not authorized to perform this operation."}}, got)
	})
	t.Run("a claim created after the refusal is the wantRetry: the event no longer counts", func(t *testing.T) {
		got := launchFailures("mc-gpu-l40s", []*unstructured.Unstructured{launching}, []unstructured.Unstructured{ice, iceAgain})
		assert.Empty(t, got)
	})
	t.Run("the refused claim itself, still there for a second, is no wantRetry", func(t *testing.T) {
		refused := fakeClaim("mc-gpu-l40s-txjc8", "2026-09-18T06:00:24Z", condition("Launched", "Unknown", "AwaitingReconciliation", "", "2026-09-18T06:00:24Z"))
		got := launchFailures("mc-gpu-l40s", []*unstructured.Unstructured{refused}, []unstructured.Unstructured{ice})
		assert.Len(t, got, 1)
		assert.Equal(t, "mc-gpu-l40s-txjc8", got[0].claim)
	})
	t.Run("a later claim refused in its own right: the claim counts, the older event drops", func(t *testing.T) {
		got := launchFailures("mc-gpu-l40s", []*unstructured.Unstructured{launchFailed}, []unstructured.Unstructured{ice})
		assert.Len(t, got, 1)
		assert.Equal(t, "mc-gpu-l40s-k9d2m", got[0].claim)
	})
	t.Run("a claim refused before the event: both count, oldest first", func(t *testing.T) {
		earlier := fakeClaim("mc-gpu-l40s-e4rly", "2026-09-18T05:50:00Z", condition("Launched", "False", "LaunchFailed", "creating instance, with fleet error(s), UnauthorizedOperation: You are not authorized to perform this operation.", "2026-09-18T05:50:04Z"))
		got := launchFailures("mc-gpu-l40s", []*unstructured.Unstructured{earlier}, []unstructured.Unstructured{ice})
		assert.Equal(t, []string{"mc-gpu-l40s-e4rly", "mc-gpu-l40s-txjc8"}, []string{got[0].claim, got[1].claim})
	})
}

// fleetError is one of AWS's CreateFleet errors as Karpenter relays it: the
// size and zone refused, and AWS's hint naming the other zones.
func fleetError(size, zone, others string) string {
	return fmt.Sprintf("InsufficientInstanceCapacity: We currently do not have sufficient %s capacity in the Availability Zone you requested (%s). Our system will be working on provisioning additional capacity. You can currently get %s capacity by not specifying an Availability Zone in your request or choosing %s.", size, zone, size, others)
}

// The refusal of one CreateFleet as an installation saw it
// (giantswarm/cluster-manager#75): a pool of three sizes without a pin on a
// cluster with three node-subnet zones — nine fleet errors, every size in
// every zone, each error's hint naming the two other zones, which the next
// errors refuse — as the claim's condition carries it (nineFleetErrors) and
// as Karpenter's event cuts it after the first (nineFleetErrorsCut).
var (
	nineFleetErrors = "creating instance, insufficient capacity, with fleet error(s), " + strings.Join([]string{
		fleetError("g6e.8xlarge", "eu-central-1a", "eu-central-1b, eu-central-1c"),
		fleetError("g6e.2xlarge", "eu-central-1c", "eu-central-1a, eu-central-1b"),
		fleetError("g6e.4xlarge", "eu-central-1a", "eu-central-1b, eu-central-1c"),
		fleetError("g6e.4xlarge", "eu-central-1b", "eu-central-1a, eu-central-1c"),
		fleetError("g6e.4xlarge", "eu-central-1c", "eu-central-1a, eu-central-1b"),
		fleetError("g6e.2xlarge", "eu-central-1a", "eu-central-1b, eu-central-1c"),
		fleetError("g6e.2xlarge", "eu-central-1b", "eu-central-1a, eu-central-1c"),
		fleetError("g6e.8xlarge", "eu-central-1c", "eu-central-1a, eu-central-1b"),
		fleetError("g6e.8xlarge", "eu-central-1b", "eu-central-1a, eu-central-1c"),
	}, "; ")
	nineFleetErrorsCut = "creating instance, insufficient capacity, with fleet error(s), InsufficientInstanceCapacity: We currently do not have sufficient g6e.8xlarge capacity in the Availability Zone you requested (eu-central-1a). Our system will be working on provisioning additional capacity. You can currently get g6e.8xla..."
	threeZones         = []string{"eu-central-1a", "eu-central-1b", "eu-central-1c"}
	l40sSizes          = []string{"g6e.2xlarge", "g6e.4xlarge", "g6e.8xlarge"}
)

const (
	wantWiden     = "wider sizes or another accelerator (a re-run of create_node_pool) give it more to choose from"
	wantRetry     = "Karpenter retries every few minutes (its unavailable-offering cache is three minutes) and the pool launches its node on its own once the family has capacity again"
	wantNoZone    = "every zone the pool may use was refused in the same answer, so there is no zone to move to (zones on a re-run would only pin the pool to one of them); " + wantRetry + "; " + wantWiden
	wantUnknown   = " at least (Karpenter's event names the first fleet error only, and the zones the pool may use could not be read)"
	wantEveryL40s = "InsufficientInstanceCapacity for every size of the pool (g6e.2xlarge, g6e.4xlarge, g6e.8xlarge)"
	wantNoPin     = " in every zone the pool may use (eu-central-1a, eu-central-1b, eu-central-1c — the cluster's node-subnet zones; the pool has no pin)"
)

// TestLaunchFailureSummary: AWS's capacity answer in Karpenter's message is
// read into the codes, the sizes and the zones refused — every fleet error
// of it —; any other refusal is its reason. Karpenter cuts the event after
// the first error and gives a claim up for capacity only once every size of
// the pool was refused in every zone it may use (giantswarm/cluster-manager#65,
// #75): with the pool's context the set is every size of the pool in every
// zone it may use — the pin, else the cluster's node-subnet zones — said so;
// without it, the message's sizes and zones, said to be the first only.
func TestLaunchFailureSummary(t *testing.T) {
	twoSizes := "creating instance, insufficient capacity, with fleet error(s), InsufficientInstanceCapacity: We currently do not have sufficient g6e.2xlarge capacity in the Availability Zone you requested (eu-central-1a).; InsufficientInstanceCapacity: We currently do not have sufficient g6e.2xlarge capacity in the Availability Zone you requested (eu-central-1c).; InsufficientInstanceCapacity: We currently do not have sufficient g6e.xlarge capacity in the Availability Zone you requested (eu-central-1a)."
	ice := func(message string) launchFailure {
		return launchFailure{reason: "InsufficientCapacityError", message: message}
	}
	cases := []struct {
		name    string
		failure launchFailure
		lc      launchContext
		want    string
	}{
		{"one size, one zone, no context: the first error only", ice(iceMessage), launchContext{}, "InsufficientInstanceCapacity for g6e.2xlarge in eu-central-1a" + wantUnknown},
		{"two sizes, two zones, each once", ice(twoSizes), launchContext{}, "InsufficientInstanceCapacity for g6e.2xlarge, g6e.xlarge in eu-central-1a, eu-central-1c" + wantUnknown},
		{"no capacity answer: the reason", launchFailure{reason: "LaunchFailed", message: "creating instance, with fleet error(s), UnauthorizedOperation: You are not authorized to perform this operation."}, launchContext{}, "LaunchFailed"},
		{"the event cut after the first size: every size of the pool, the pinned zones", ice(iceMessage), launchContext{instanceTypes: l40sSizes, zones: []string{"eu-central-1a", "eu-central-1b"}, clusterZones: threeZones}, wantEveryL40s + " in eu-central-1a, eu-central-1b, the zones the pool is pinned to"},
		{"one zone: the pin", ice(iceMessage), launchContext{instanceTypes: l40sSizes, zones: []string{"eu-central-1a"}, clusterZones: threeZones}, wantEveryL40s + " in eu-central-1a, the zone the pool is pinned to"},
		{"the run's answer, the event cut: every size in every zone the pool may use", ice(nineFleetErrorsCut), launchContext{instanceTypes: l40sSizes, clusterZones: threeZones}, wantEveryL40s + wantNoPin},
		{"the run's answer in full: the same set", ice(nineFleetErrors), launchContext{instanceTypes: l40sSizes, clusterZones: threeZones}, wantEveryL40s + wantNoPin},
		{"the message names every size itself: still the pool's", ice(twoSizes), launchContext{instanceTypes: []string{"g6e.xlarge", "g6e.2xlarge"}}, "InsufficientInstanceCapacity for every size of the pool (g6e.xlarge, g6e.2xlarge) in eu-central-1a, eu-central-1c" + wantUnknown},
		{"another reason keeps the message's sizes and zones", launchFailure{reason: "LaunchFailed", message: iceMessage}, launchContext{instanceTypes: l40sSizes, zones: []string{"eu-central-1b"}, clusterZones: threeZones}, "InsufficientInstanceCapacity for g6e.2xlarge in eu-central-1a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.failure.summary(tc.lc))
		})
	}
	r := ice(nineFleetErrors).refused(launchContext{instanceTypes: l40sSizes, clusterZones: threeZones})
	assert.Equal(t, refused{codes: []string{"InsufficientInstanceCapacity"}, types: l40sSizes, zones: threeZones, everySize: true, everyZone: true}, r, "nine errors read into three sizes and three zones, the pool's order")
}

// TestLaunchContextRemedy: the way around a refusal points at what decided
// where the launch was tried (giantswarm/cluster-manager#65, #71, #75) — the
// pin, the model cache claim living in a pinned zone when one does, and the
// re-run that moves the pool to a zone the answer did not refuse, whose claim
// the slice then mounts; never at a refused zone: with every zone the pool
// may use refused there is no zone to move to, and Karpenter's retry brings
// the node. AWS's per-error hint is relayed for the zones outside the refused
// set only.
func TestLaunchContextRemedy(t *testing.T) {
	claim := &detect.CacheClaim{Namespace: "model-serving", Name: "hf-cache", Phase: "Bound", Volume: "pvc-1", Zone: "eu-central-1b"}
	read := cacheClaims{namespace: "model-serving", base: "hf-cache", claims: []*detect.CacheClaim{claim}}
	const mounts = " — with the cache on its slice then mounts that zone's model cache claim (the one Bound there, else hf-cache-<zone>, created there and kept; the weights downloaded once more), with cache false it serves from the node's local disk; " + wantWiden
	const move = "re-run create_node_pool on the pool with zones naming one zone with capacity"
	pinnedB := refused{zones: []string{"eu-central-1b"}, everyZone: true}

	assert.Equal(t, wantWiden, launchContext{}.remedy(refused{}))
	assert.Equal(t, wantWiden, launchContext{claims: read}.remedy(refused{}), "a claim pins nothing while the pool names no zones")
	assert.Equal(t, wantWiden, launchContext{clusterZones: threeZones}.remedy(refused{zones: []string{"eu-central-1a"}}), "no pin and the zones the pool may use not known to be all refused: wantWiden")
	assert.Equal(t, wantNoZone, launchContext{clusterZones: threeZones}.remedy(refused{zones: threeZones, everyZone: true}), "no pin, every zone refused: no zone to move to")
	assert.Equal(t, "the pool is pinned to eu-central-1b — the model cache claim model-serving/hf-cache (volume pvc-1) lives in eu-central-1b and the pool's slice mounts it: "+move+" — not refused in this answer: eu-central-1a, eu-central-1c (the cluster's other node-subnet zones; capacity there is not promised)"+mounts,
		launchContext{zones: []string{"eu-central-1b"}, clusterZones: threeZones, claims: read}.remedy(pinnedB), "the other node-subnet zones are the candidates")
	assert.Equal(t, "the pool is pinned to eu-central-1b — the model cache claim model-serving/hf-cache (volume pvc-1) lives in eu-central-1b and the pool's slice mounts it: "+move+" — not refused in this answer: eu-central-1a (the cluster's other node-subnet zone; capacity there is not promised)"+mounts,
		launchContext{zones: []string{"eu-central-1b"}, clusterZones: []string{"eu-central-1a", "eu-central-1b"}, claims: read}.remedy(pinnedB), "one candidate")
	assert.Equal(t, "the pool is pinned to eu-central-1a: "+move+mounts,
		launchContext{zones: []string{"eu-central-1a"}, claims: read}.remedy(refused{zones: []string{"eu-central-1a"}, everyZone: true}), "no claim lives in the pool's zone, the cluster's zones wantUnknown: no candidate named")
	assert.Equal(t, "the pool is pinned to eu-central-1a, eu-central-1b, eu-central-1c: every node-subnet zone of the cluster was refused in the same answer, so there is no zone to move to; "+wantRetry+"; "+wantWiden,
		launchContext{zones: threeZones, clusterZones: threeZones}.remedy(refused{zones: threeZones, everyZone: true}), "a pool without the cache pinned to every zone: nowhere to move")

	hint := launchFailure{message: "You can currently get g6e.2xlarge capacity by not specifying an Availability Zone in your request or choosing eu-central-1a, eu-central-1c."}
	assert.Equal(t, []string{"eu-central-1a", "eu-central-1c"}, hint.elsewhere(pinnedB), "AWS's hint outside the refused set")
	assert.Nil(t, hint.elsewhere(refused{zones: threeZones}), "every zone AWS named was refused in the same answer")
	assert.Nil(t, launchFailure{message: nineFleetErrors}.elsewhere(refused{zones: threeZones}), "nine hints, every zone refused")
	assert.Equal(t, threeZones, launchFailure{message: nineFleetErrors}.elsewhere(refused{}), "the hints of every error, deduplicated and sorted, when nothing is known refused")
	assert.Nil(t, launchFailure{message: iceMessage}.elsewhere(refused{}), "cut before AWS's list")
}

// TestLaunchFailureMessage words the nodes step: the count and the last
// refusal with Karpenter's reason and message verbatim, the way around it,
// and the pool's other claims when it has any.
func TestLaunchFailureMessage(t *testing.T) {
	failures := []launchFailure{
		{claim: "mc-gpu-l40s-txjc8", reason: "InsufficientCapacityError", at: "2026-09-18T06:00:27Z", message: "creating instance, insufficient capacity, with fleet error(s), InsufficientInstanceCapacity: We currently do not have sufficient g6e.2xlarge capacity in the Availability Zone you requested (eu-central-1a)."},
		{claim: "mc-gpu-l40s-zgbdh", reason: "InsufficientCapacityError", at: "2026-09-18T06:03:37Z", message: "creating instance, insufficient capacity, with fleet error(s), InsufficientInstanceCapacity: We currently do not have sufficient g6e.2xlarge capacity in the Availability Zone you requested (eu-central-1c)."},
	}
	assert.Equal(t, `2 NodeClaims could not launch, the last (mc-gpu-l40s-zgbdh) at 2026-09-18T06:03:37Z — InsufficientInstanceCapacity for g6e.2xlarge in eu-central-1c`+wantUnknown+` (InsufficientCapacityError); Karpenter: "creating instance, insufficient capacity, with fleet error(s), InsufficientInstanceCapacity: We currently do not have sufficient g6e.2xlarge capacity in the Availability Zone you requested (eu-central-1c)."; it retries while a pod waits — `+wantWiden,
		launchFailureMessage(failures, 0, 0, 0, launchContext{}))
	assert.Equal(t, `1 NodeClaim could not launch, the last (mc-gpu-l40s-k9d2m) at 2026-09-18T06:10:04Z — LaunchFailed; Karpenter: "creating instance, with fleet error(s), UnauthorizedOperation: You are not authorized to perform this operation."; it retries while a pod waits — `+wantWiden+`; besides, 0 NodeClaim(s) launching, 1 ready, 0 terminating`,
		launchFailureMessage([]launchFailure{{claim: "mc-gpu-l40s-k9d2m", reason: "LaunchFailed", at: "2026-09-18T06:10:04Z", message: "creating instance, with fleet error(s), UnauthorizedOperation: You are not authorized to perform this operation."}}, 0, 1, 0, launchContext{}),
		"a second node refused beside a ready one")
	run := launchFailureMessage([]launchFailure{{claim: "mc-gpu-l40s-44j24", reason: "InsufficientCapacityError", at: "2026-09-18T16:14:01Z", message: nineFleetErrors}}, 0, 0, 0, launchContext{instanceTypes: l40sSizes, clusterZones: threeZones})
	assert.Equal(t, `1 NodeClaim could not launch, the last (mc-gpu-l40s-44j24) at 2026-09-18T16:14:01Z — `+wantEveryL40s+wantNoPin+` (InsufficientCapacityError); Karpenter: "`+nineFleetErrors+`"; it retries while a pod waits — `+wantNoZone, run,
		"the run's refusal: every size in every zone, no AWS hint relayed, no zone advised")
}

// TestNodesStepWithARefusedClaim (giantswarm/cluster-manager#55): a NodeClaim
// carrying Karpenter's refusal keeps the nodes step in progress with the
// refusal, since Karpenter said so — counted apart from the launching and
// ready claims, and never done with no node for the pod that waits.
func TestNodesStepWithARefusedClaim(t *testing.T) {
	mp := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cluster.x-k8s.io/v1beta1", "kind": "MachinePool",
		"metadata": map[string]any{"name": "mc-gpu-l40s", "namespace": "org-acme", "creationTimestamp": "2026-09-18T06:00:00Z"},
		"spec":     map[string]any{"replicas": int64(0)},
		"status":   map[string]any{"replicas": int64(0), "readyReplicas": int64(0), "conditions": []any{condition("Ready", "True", "", "", "2026-09-18T06:00:05Z")}},
	}}
	refused := fakeClaim("mc-gpu-l40s-k9d2m", "2026-09-18T06:10:00Z", condition("Launched", "False", "LaunchFailed", "creating instance, with fleet error(s), UnauthorizedOperation: You are not authorized to perform this operation.", "2026-09-18T06:10:04Z"))
	live := &poolLive{nodes: []*poolNode{{claim: refused}}}
	live.failures = launchFailures("mc-gpu-l40s", live.claims(), nil)

	st := nodesStep(mp, nil, live, true, true, launchContext{})
	assert.Equal(t, StepInProgress, st.State)
	assert.Equal(t, "2026-09-18T06:10:04Z", st.Since, "since Karpenter's refusal")
	assert.Equal(t, `1 NodeClaim could not launch, the last (mc-gpu-l40s-k9d2m) at 2026-09-18T06:10:04Z — LaunchFailed; Karpenter: "creating instance, with fleet error(s), UnauthorizedOperation: You are not authorized to perform this operation."; it retries while a pod waits — wider sizes or another accelerator (a re-run of create_node_pool) give it more to choose from`, st.Message)
	assert.Equal(t, "LaunchFailed", live.refusal(launchContext{}))

	without := &poolLive{}
	assert.Equal(t, StepDone, nodesStep(mp, nil, without, true, true, launchContext{}).State, "no claim, no refusal: scale-to-zero")
	assert.Empty(t, without.refusal(launchContext{}))
	var none *poolLive
	assert.Empty(t, none.refusal(launchContext{}), "an unreadable cluster refuses nothing")
}
