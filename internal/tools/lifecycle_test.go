package tools

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/cluster-manager/internal/detect"
)

// add puts a fixture's objects into a fake client beside what it has. A
// Gateway goes in under its resource: the tracker's guess from the kind is
// "gatewaies".
func (l *lab) add(t *testing.T, dyn dynamic.Interface, fixture string) *lab {
	t.Helper()
	tracker := dyn.(*dynamicfake.FakeDynamicClient).Tracker()
	for _, obj := range loadFixtures(t, fixture) {
		if u := obj.(*unstructured.Unstructured); u.GetKind() == "Gateway" {
			require.NoError(t, tracker.Create(detect.GatewayGVR, u, u.GetNamespace()))
			continue
		}
		require.NoError(t, tracker.Add(obj))
	}
	return l
}

// TestListNodePoolsLifecycle (giantswarm/cluster-manager#41): one pool per
// phase — creating right after create_node_pool (release reconciling, no
// MachinePool), ready at scale-to-zero (0/0, every step done), scaling with a
// NodeClaim terminating, removing while the release is deleted and while a
// partial teardown left it standing without its source — and the default
// pool, whose nodes are still registering. The golden file is the answer's
// contract for the portal's pool panel.
func TestListNodePoolsLifecycle(t *testing.T) {
	l := newLab(t, "installation.yaml").target(t, wc1APIServer, "wc1-nodeclaims.yaml", servingAPIs...)
	l.add(t, l.installation, "lifecycle.yaml")
	pools, err := l.service(Config{Installation: "gazelle"}).ListNodePools(context.Background(), "wc1", "")
	require.NoError(t, err)
	byName := map[string]NodePool{}
	for _, p := range pools.NodePools {
		byName[p.Name] = p
	}
	require.Len(t, byName, 7)

	creating := byName["wc1-gpu-new"]
	assert.Equal(t, PhaseCreating, creating.Phase)
	assert.Equal(t, []string{StepInProgress, StepPending, StepPending}, states(creating.Steps), "the release step in progress, nothing else yet")
	assert.Equal(t, "2026-09-17T09:00:05Z", creating.Steps[0].Since, "since the Ready condition's last transition")
	assert.Equal(t, &ReleaseRef{Name: "wc1-gpu-new", Namespace: "org-acme"}, creating.OwnerRelease)
	assert.Equal(t, "nvidia-l4", creating.Accelerator)

	zero := byName["wc1-gpu-zero"]
	assert.Equal(t, PhaseReady, zero.Phase)
	assert.Equal(t, []string{StepDone, StepDone, StepDone}, states(zero.Steps))
	assert.Equal(t, int64(0), zero.Replicas)
	assert.Contains(t, zero.Steps[2].Message, "scale-to-zero")

	term := byName["wc1-gpu-term"]
	assert.Equal(t, PhaseScaling, term.Phase)
	assert.Equal(t, []string{StepDone, StepDone, StepInProgress}, states(term.Steps))
	assert.Equal(t, "0 NodeClaim(s) launching, 0 ready, 1 terminating — ip-10-0-7-7.eu-west-1.compute.internal terminating since 2026-09-17T09:40:00Z (instance shutting down since 2026-09-17T09:40:41Z, no longer billed; EC2 reports it terminated about five minutes later, then the NodeClaim goes)", term.Steps[2].Message,
		"the terminating node named by its Node, with the NodeClaim's deletion time (giantswarm/cluster-manager#57) and where its termination stands")
	assert.Equal(t, "2026-09-17T09:40:00Z", term.Steps[2].Since, "since the NodeClaim's deletion")

	gone := byName["wc1-gpu-gone"]
	assert.Equal(t, PhaseRemoving, gone.Phase)
	assert.True(t, gone.Deleting)
	require.Len(t, gone.Pending, 1, "the release, deleted last, terminating; source and Secret gone")
	assert.Equal(t, ObjectAction{APIVersion: "helm.toolkit.fluxcd.io/v2", Kind: "HelmRelease", Name: "wc1-gpu-gone", Namespace: "org-acme", Action: actionTerminating}, gone.Pending[0])

	half := byName["wc1-gpu-half"]
	assert.Equal(t, PhaseRemoving, half.Phase, "the source is gone while the release stands: a teardown re-run is pending")
	assert.True(t, half.Deleting)
	assert.Equal(t, []string{actionPending}, actionsOf(half.Pending))

	assert.Equal(t, PhaseReady, byName["wc1-gpu-a10g"].Phase)
	assert.Equal(t, "0 nodes on the cluster: the MachinePool still lists 2 gone (aws:///eu-west-1a/i-0a1b2c3d4e5f60001, aws:///eu-west-1b/i-0a1b2c3d4e5f60002), its list follows within minutes", byName["wc1-gpu-a10g"].Steps[2].Message,
		"the cluster shows no node of the pool: its MachinePool's provider IDs lag (giantswarm/cluster-manager#49)")
	assert.Equal(t, PhaseScaling, byName["wc1-def00"].Phase, "2 of 3 nodes ready, no release step for a pool without one")
	assert.Equal(t, []string{StepMachinePool, StepNodes}, names(byName["wc1-def00"].Steps))

	assertGolden(t, "list_node_pools_lifecycle", pools)
}

func states(steps []Step) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.State)
	}
	return out
}

func names(steps []Step) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.Name)
	}
	return out
}

func actionsOf(objs []ObjectAction) []string {
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.Action)
	}
	return out
}

// TestListNodePoolsPrewarm (giantswarm/cluster-manager#48): the installation's
// own pool created with prewarm lists the placeholder as a fourth step after
// the nodes — holding the first node while the pool is ready — and a pool
// without the option (every pool of wc1) shows no such step. The golden is
// the answer's contract for the portal's "first node starting" row.
func TestListNodePoolsPrewarm(t *testing.T) {
	l := newLab(t, "installation.yaml")
	l.add(t, l.installation, "prewarm.yaml")
	pools, err := l.service(Config{Installation: "gazelle"}).ListNodePools(context.Background(), "gazelle", "")
	require.NoError(t, err)
	require.Len(t, pools.NodePools, 1)
	pool := pools.NodePools[0]
	assert.Equal(t, PhaseReady, pool.Phase, "the placeholder never decides the phase")
	assert.Equal(t, []string{StepRelease, StepMachinePool, StepNodes, StepPrewarm}, names(pool.Steps))
	assert.Equal(t, []string{StepDone, StepDone, StepDone, StepInProgress}, states(pool.Steps))
	assert.Equal(t, "holding node ip-10-0-1-23.eu-central-1.compute.internal until the first workload preempts the placeholder or its hold ends", pool.Steps[3].Message)
	assert.Equal(t, "2026-09-18T06:04:41Z", pool.Steps[3].Since, "since the pod started on the node")
	assertGolden(t, "list_node_pools_prewarm", pools)

	wc1, err := l.service(Config{Installation: "gazelle"}).ListNodePools(context.Background(), "wc1", "")
	require.NoError(t, err)
	for _, p := range wc1.NodePools {
		assert.NotContains(t, names(p.Steps), StepPrewarm, "%s: no prewarm, no step", p.Name)
	}
}

// TestListNodePoolsPrewarmPendingUntilInstalled (giantswarm/cluster-manager#53):
// a pool created with prewarm whose release is not Ready — failing to install
// here, right after create_node_pool — has no placeholder Job because the
// install that creates it has not happened; the prewarm step is pending,
// saying so, never done. An absent Job means finished only once the release
// step is done. The golden is the answer's contract for the portal's row
// while the pool installs.
func TestListNodePoolsPrewarmPendingUntilInstalled(t *testing.T) {
	l := newLab(t, "installation.yaml")
	l.add(t, l.installation, "prewarm-install-failed.yaml")
	pools, err := l.service(Config{Installation: "gazelle"}).ListNodePools(context.Background(), "gazelle", "")
	require.NoError(t, err)
	require.Len(t, pools.NodePools, 1)
	pool := pools.NodePools[0]
	assert.Equal(t, PhaseFailed, pool.Phase, "the release step failed; the placeholder never decides the phase")
	assert.Equal(t, []string{StepRelease, StepMachinePool, StepNodes, StepPrewarm}, names(pool.Steps))
	assert.Equal(t, []string{StepFailed, StepPending, StepPending, StepPending}, states(pool.Steps))
	assert.Equal(t, "the pool release is not Ready yet: the placeholder Job org-giantswarm/gazelle-gpu-l40s-prewarm is created with the release's install (the release step says where it stands)", pool.Steps[3].Message)
	assert.Empty(t, pool.Steps[3].Since, "nothing has happened to the placeholder yet")
	assertGolden(t, "list_node_pools_prewarm_install_failed", pools)
}

// TestListNodePoolsPrewarmCannotLaunch (giantswarm/cluster-manager#55): the
// installation's own pool created with prewarm whose first node Karpenter
// cannot launch — no capacity for the pool's one size in the zone it tried,
// each claim deleted within seconds, the refusal left as a Warning event on
// the claim — shows the refusal in the nodes step, in Karpenter's words and
// in progress since the last one instead of done with zero nodes, and the
// prewarm step says the placeholder waits for a node Karpenter could not
// launch. The general pool's consolidation notes and the claims' termination
// warnings beside the refusals are not read as one. The golden is the
// answer's contract for the portal's row while capacity is short.
func TestListNodePoolsPrewarmCannotLaunch(t *testing.T) {
	l := newLab(t, "installation.yaml")
	l.add(t, l.installation, "prewarm-cannot-launch.yaml")
	pools, err := l.service(Config{Installation: "gazelle"}).ListNodePools(context.Background(), "gazelle", "")
	require.NoError(t, err)
	require.Len(t, pools.NodePools, 1)
	pool := pools.NodePools[0]
	assert.Equal(t, PhaseScaling, pool.Phase, "Karpenter keeps trying: the nodes step is in progress, never done with none")
	assert.Equal(t, []string{StepRelease, StepMachinePool, StepNodes, StepPrewarm}, names(pool.Steps))
	assert.Equal(t, []string{StepDone, StepDone, StepInProgress, StepInProgress}, states(pool.Steps))
	assert.Equal(t, "2026-09-18T02:26:37Z", pool.Steps[2].Since, "since Karpenter's last refusal")
	assert.Equal(t, `2 NodeClaims could not launch, the last (gazelle-gpu-l40s-zgbdh) at 2026-09-18T02:26:37Z — InsufficientInstanceCapacity for every size of the pool (g6e.2xlarge) in every zone the pool may use (eu-central-1a, eu-central-1b, eu-central-1c — the cluster's node-subnet zones; the pool has no pin) (InsufficientCapacityError); Karpenter: "creating instance, insufficient capacity, with fleet error(s), InsufficientInstanceCapacity: We currently do not have sufficient g6e.2xlarge capacity in the Availability Zone you requested (eu-central-1a). Our system will be working on provisioning additional capacity. You can currently get g6e.2xla..."; it retries while a pod waits — `+wantNoZone, pool.Steps[2].Message)
	assert.Equal(t, "pending: the placeholder waits for the pool's first node, which Karpenter could not launch — InsufficientInstanceCapacity for every size of the pool (g6e.2xlarge) in every zone the pool may use (eu-central-1a, eu-central-1b, eu-central-1c — the cluster's node-subnet zones; the pool has no pin) (the nodes step carries Karpenter's message)", pool.Steps[3].Message)
	assertGolden(t, "list_node_pools_prewarm_cannot_launch", pools)
}

// TestListNodePoolsCannotLaunchPinned (giantswarm/cluster-manager#65): a pool
// of three sizes pinned to the model cache claim's zone that Karpenter cannot
// launch — AWS refused every size at once, Karpenter's event names the first
// only — has its nodes step name every size of the pool and the pinned zone,
// what pinned the pool (the claim, its volume) and the re-run that moves it
// (zones naming a zone with capacity — the cluster's other node-subnet zones
// as the candidates, whose claim the slice then mounts); a pool without a pin
// refused with the whole nine-error answer has every size in every zone the
// pool may use named, no zone AWS hinted at relayed (the same answer refused
// each) and no zone advised (giantswarm/cluster-manager#75). The golden is the
// portal's row while capacity is short.
func TestListNodePoolsCannotLaunchPinned(t *testing.T) {
	l := newLab(t, "installation.yaml")
	l.add(t, l.installation, "cache-claim.yaml")
	l.add(t, l.installation, "cannot-launch-pinned.yaml")
	pools, err := l.service(Config{Installation: "gazelle"}).ListNodePools(context.Background(), "gazelle", "")
	require.NoError(t, err)
	require.Len(t, pools.NodePools, 2)
	pinned, free := pools.NodePools[0], pools.NodePools[1]
	assert.Equal(t, "gazelle-bench-11", pinned.Name)
	assert.Equal(t, PhaseScaling, pinned.Phase)
	assert.Equal(t, StepInProgress, pinned.Steps[2].State)
	assert.Equal(t, `1 NodeClaim could not launch, the last (gazelle-bench-11-5hpxb) at 2026-09-18T12:14:47Z — InsufficientInstanceCapacity for every size of the pool (g6e.2xlarge, g6e.4xlarge, g6e.8xlarge) in eu-central-1b, the zone the pool is pinned to (InsufficientCapacityError); Karpenter: "creating instance, insufficient capacity, with fleet error(s), InsufficientInstanceCapacity: We currently do not have sufficient g6e.2xlarge capacity in the Availability Zone you requested (eu-central-1b). Our system will be working on provisioning additional capacity. You can currently get g6e.2xla..."; it retries while a pod waits — the pool is pinned to eu-central-1b — the model cache claim model-serving/hf-cache (volume pvc-6e577f13-ff22-461c-a453-cfbcdd2d7c80) lives in eu-central-1b and the pool's slice mounts it: re-run create_node_pool on the pool with zones naming one zone with capacity — not refused in this answer: eu-central-1a, eu-central-1c (the cluster's other node-subnet zones; capacity there is not promised) — with the cache on its slice then mounts that zone's model cache claim (the one Bound there, else hf-cache-<zone>, created there and kept; the weights downloaded once more), with cache false it serves from the node's local disk; wider sizes or another accelerator (a re-run of create_node_pool) give it more to choose from`, pinned.Steps[2].Message)
	assert.Equal(t, "gazelle-bench-12", free.Name)
	assert.Equal(t, `1 NodeClaim could not launch, the last (gazelle-bench-12-k2m4p) at 2026-09-18T12:30:47Z — `+wantEveryL40s+wantNoPin+` (InsufficientCapacityError); Karpenter: "`+nineFleetErrors+`"; it retries while a pod waits — `+wantNoZone, free.Steps[2].Message, "every size in every zone: no AWS hint relayed, no zone advised")
	assertGolden(t, "list_node_pools_cannot_launch_pinned", pools)
}

// TestPrewarmStep words every state of the placeholder from its Job and pod.
func TestPrewarmStep(t *testing.T) {
	job := func(status map[string]any) *unstructured.Unstructured {
		j := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "batch/v1", "kind": "Job",
			"metadata": map[string]any{"name": "mc-gpu-l4-prewarm", "namespace": "org-acme", "creationTimestamp": "2026-09-18T06:00:08Z"},
			"spec":     map[string]any{"activeDeadlineSeconds": int64(1500)},
			"status":   status,
		}}
		return j
	}
	pod := func(phase string, meta map[string]any, status map[string]any) unstructured.Unstructured {
		m := map[string]any{"name": "mc-gpu-l4-prewarm-abc12", "namespace": "org-acme", "creationTimestamp": "2026-09-18T06:00:09Z"}
		for k, v := range meta {
			m[k] = v
		}
		status["phase"] = phase
		return unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": m, "spec": map[string]any{"nodeName": "ip-10-0-1-23.eu-central-1.compute.internal"}, "status": status}}
	}
	failed := func(reason, message, at string) map[string]any {
		return map[string]any{"conditions": []any{map[string]any{"type": "Failed", "status": "True", "reason": reason, "message": message, "lastTransitionTime": at}}}
	}
	holding := func() prewarmState {
		return prewarmState{job: job(map[string]any{"active": int64(1)}), pods: []unstructured.Unstructured{pod("Running", nil, map[string]any{"startTime": "2026-09-18T06:04:41Z"})}}
	}
	cases := []struct {
		name  string
		state prewarmState
		// releaseDone is whether the pool release's step is done: the chart
		// creates the Job with the install, so before it an absent Job is
		// pending, not finished (giantswarm/cluster-manager#53).
		releaseDone bool
		// refusal is Karpenter's last refusal to launch a node of the pool
		// in a line, as the nodes step reads it; "" while it launches them
		// (giantswarm/cluster-manager#55).
		refusal  string
		want     string
		message  string
		since    string
		finished string
	}{
		{"absent while the release installs", prewarmState{}, false, "", StepPending, "the pool release is not Ready yet: the placeholder Job org-acme/mc-gpu-l4-prewarm is created with the release's install (the release step says where it stands)", "", ""},
		{"absent after the release installed", prewarmState{}, true, "", StepDone, "no placeholder Job org-acme/mc-gpu-l4-prewarm: removed ten minutes after it ended, or prewarm was set on an existing pool (the Job is created with the release's install only)", "", ""},
		{"created, no pod", prewarmState{job: job(map[string]any{"active": int64(0)})}, true, "", StepInProgress, "placeholder Job org-acme/mc-gpu-l4-prewarm created, its pod not yet", "2026-09-18T06:00:08Z", ""},
		{"pending", prewarmState{job: job(map[string]any{"active": int64(1)}), pods: []unstructured.Unstructured{pod("Pending", nil, map[string]any{"conditions": []any{map[string]any{"type": "PodScheduled", "status": "False", "reason": "Unschedulable", "message": "0/6 nodes are available: 6 Insufficient nvidia.com/gpu."}}})}}, true, "",
			StepInProgress, "pending: the placeholder waits for the pool's first node, launched by Karpenter for its GPU (0/6 nodes are available: 6 Insufficient nvidia.com/gpu.)", "2026-09-18T06:00:09Z", ""},
		{"holding", holding(), true, "",
			StepInProgress, "holding node ip-10-0-1-23.eu-central-1.compute.internal until the first workload preempts the placeholder or its hold ends", "2026-09-18T06:04:41Z", ""},
		{"holding while the release reconciles again", holding(), false, "",
			StepInProgress, "holding node ip-10-0-1-23.eu-central-1.compute.internal until the first workload preempts the placeholder or its hold ends", "2026-09-18T06:04:41Z", ""},
		{"being preempted", prewarmState{job: job(map[string]any{"active": int64(1)}), pods: []unstructured.Unstructured{pod("Running", map[string]any{"deletionTimestamp": "2026-09-18T06:09:00Z"}, map[string]any{"startTime": "2026-09-18T06:04:41Z"})}}, true, "",
			StepInProgress, "preempted: the placeholder's pod is terminating, the first workload takes its node ip-10-0-1-23.eu-central-1.compute.internal", "2026-09-18T06:09:00Z", ""},
		{"preempted", prewarmState{job: job(failed("BackoffLimitExceeded", "Job has reached the specified backoff limit", "2026-09-18T06:09:02Z"))}, true, "",
			StepDone, "preempted: the first workload took the placeholder's node before its hold ended (BackoffLimitExceeded Job has reached the specified backoff limit)", "2026-09-18T06:00:08Z", "2026-09-18T06:09:02Z"},
		{"finished", prewarmState{job: job(map[string]any{"completionTime": "2026-09-18T06:19:45Z", "conditions": []any{map[string]any{"type": "Complete", "status": "True", "lastTransitionTime": "2026-09-18T06:19:45Z"}}})}, true, "",
			StepDone, "finished: the hold ended without a workload; Karpenter consolidates the empty node", "2026-09-18T06:00:08Z", "2026-09-18T06:19:45Z"},
		{"no node came", prewarmState{job: job(failed("DeadlineExceeded", "Job was active longer than specified deadline", "2026-09-18T06:25:08Z"))}, true, "",
			StepFailed, "no node came within the placeholder's deadline of 1500 s (DeadlineExceeded Job was active longer than specified deadline): check the pool's NodeClaims and Karpenter's log", "2026-09-18T06:00:08Z", "2026-09-18T06:25:08Z"},
		{"pending while Karpenter cannot launch the node", prewarmState{job: job(map[string]any{"active": int64(1)}), pods: []unstructured.Unstructured{pod("Pending", nil, map[string]any{"conditions": []any{map[string]any{"type": "PodScheduled", "status": "False", "reason": "Unschedulable", "message": "0/6 nodes are available: 6 Insufficient nvidia.com/gpu."}}})}}, true,
			"InsufficientInstanceCapacity for g6e.2xlarge in eu-central-1a",
			StepInProgress, "pending: the placeholder waits for the pool's first node, which Karpenter could not launch — InsufficientInstanceCapacity for g6e.2xlarge in eu-central-1a (the nodes step carries Karpenter's message)", "2026-09-18T06:00:09Z", ""},
		{"no node came, Karpenter said why", prewarmState{job: job(failed("DeadlineExceeded", "Job was active longer than specified deadline", "2026-09-18T06:25:08Z"))}, true,
			"InsufficientInstanceCapacity for g6e.2xlarge in eu-central-1a",
			StepFailed, "no node came within the placeholder's deadline of 1500 s (DeadlineExceeded Job was active longer than specified deadline): Karpenter could not launch one — InsufficientInstanceCapacity for g6e.2xlarge in eu-central-1a (the nodes step carries its message)", "2026-09-18T06:00:08Z", "2026-09-18T06:25:08Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.state.namespace, tc.state.name = "org-acme", "mc-gpu-l4-prewarm"
			st := prewarmStep(&tc.state, tc.releaseDone, tc.refusal, nil)
			assert.Equal(t, StepPrewarm, st.Name)
			assert.Equal(t, tc.want, st.State)
			assert.Equal(t, tc.message, st.Message)
			assert.Equal(t, tc.since, st.Since)
			assert.Equal(t, tc.finished, st.FinishedAt)
		})
	}
}

// TestPrewarmStepReadsTheNodes (giantswarm/cluster-manager#85): the pool's
// nodes speak beside the placeholder's Job and pod. A node that joined but
// advertises no GPU yet is waited for — the pod Pending, or rejected by the
// node's kubelet in that window and the Job Failed under backoffLimit 0 —,
// never read as a preemption or a failure; the GPU advertised, the step is
// done; a workload on the pool's node (the first model served) is done
// whatever the Job and its pod say — the placeholder Pending behind it for
// the pool's whole life, or ended at its deadline; a hold container that
// failed, or a rejected pod with no node left, is failed with the reason.
func TestPrewarmStepReadsTheNodes(t *testing.T) {
	const nodeName = "ip-10-0-1-23.eu-central-1.compute.internal"
	job := func(status map[string]any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "batch/v1", "kind": "Job",
			"metadata": map[string]any{"name": "mc-gpu-l4-prewarm", "namespace": "org-acme", "creationTimestamp": "2026-09-18T06:00:08Z"},
			"spec":     map[string]any{"activeDeadlineSeconds": int64(1500)},
			"status":   status,
		}}
	}
	active := func() *unstructured.Unstructured { return job(map[string]any{"active": int64(1)}) }
	failedJob := func(reason, message, at string) *unstructured.Unstructured {
		return job(map[string]any{"conditions": []any{map[string]any{"type": "Failed", "status": "True", "reason": reason, "message": message, "lastTransitionTime": at}}})
	}
	pod := func(phase, bound string, status map[string]any) []unstructured.Unstructured {
		status["phase"] = phase
		spec := map[string]any{}
		if bound != "" {
			spec["nodeName"] = bound
		}
		return []unstructured.Unstructured{{Object: map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": "mc-gpu-l4-prewarm-abc12", "namespace": "org-acme", "creationTimestamp": "2026-09-18T06:00:09Z"},
			"spec":     spec, "status": status,
		}}}
	}
	unschedulable := func() map[string]any {
		return map[string]any{"conditions": []any{map[string]any{"type": "PodScheduled", "status": "False", "reason": "Unschedulable", "message": "0/6 nodes are available: 6 Insufficient nvidia.com/gpu."}}}
	}
	rejected := func() []unstructured.Unstructured {
		return pod("Failed", nodeName, map[string]any{"reason": "UnexpectedAdmissionError", "message": "Allocate failed due to no healthy devices present; cannot allocate unhealthy devices nvidia.com/gpu, which is unexpected"})
	}
	// node is the pool's node as the cluster shows it: joined at 06:04:07,
	// advertising gpus (none: the device plugin has not registered them),
	// held by the given workloads.
	node := func(gpus string, holders ...string) *poolLive {
		status := map[string]any{"allocatable": map[string]any{"cpu": "7910m"}}
		if gpus != "" {
			status["allocatable"].(map[string]any)[detect.GPUResource] = gpus
		}
		n := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Node",
			"metadata": map[string]any{"name": nodeName, "creationTimestamp": "2026-09-18T06:04:07Z"},
			"status":   status,
		}}
		return &poolLive{nodes: []*poolNode{{node: n, holders: holders}}}
	}
	const predictor = "model-serving/qwen3-8b-fp8-kserve-6649fb66c8-dllt7 (1 GPU)"
	cases := []struct {
		name     string
		state    prewarmState
		live     *poolLive
		want     string
		message  string
		since    string
		finished string
	}{
		{
			name:  "fresh node without its GPU: waiting for the GPU",
			state: prewarmState{job: active(), pods: pod("Pending", "", unschedulable())},
			live:  node(""),
			want:  StepInProgress,
			message: "waiting for the pool's node " + nodeName + " to advertise its GPU: it joined at 2026-09-18T06:04:07Z, its device plugin has not registered nvidia.com/gpu yet (the GPU operator installs the driver and the container toolkit first) " +
				"(0/6 nodes are available: 6 Insufficient nvidia.com/gpu.)",
			since: "2026-09-18T06:04:07Z",
		},
		{
			name:    "fresh node advertising zero GPUs: waiting for the GPU",
			state:   prewarmState{job: active(), pods: pod("Pending", "", map[string]any{})},
			live:    node("0"),
			want:    StepInProgress,
			message: "waiting for the pool's node " + nodeName + " to advertise its GPU: it joined at 2026-09-18T06:04:07Z, its device plugin has not registered nvidia.com/gpu yet (the GPU operator installs the driver and the container toolkit first)",
			since:   "2026-09-18T06:04:07Z",
		},
		{
			name:  "rejected by the node before its GPU, Job failed: waiting for the GPU, never BackoffLimitExceeded",
			state: prewarmState{job: failedJob("BackoffLimitExceeded", "Job has reached the specified backoff limit", "2026-09-18T06:04:41Z"), pods: rejected()},
			live:  node(""),
			want:  StepInProgress,
			message: "waiting for the pool's node " + nodeName + " to advertise its GPU: it joined at 2026-09-18T06:04:07Z, its device plugin has not registered nvidia.com/gpu yet (the GPU operator installs the driver and the container toolkit first); " +
				"the node rejected the placeholder's pod mc-gpu-l4-prewarm-abc12 meanwhile (UnexpectedAdmissionError: Allocate failed due to no healthy devices present; cannot allocate unhealthy devices nvidia.com/gpu, which is unexpected), which the Job does not replace (backoffLimit 0): the node stays, and the first workload is placed on it once the GPU is advertised",
			since: "2026-09-18T06:04:07Z",
		},
		{
			name:  "rejected, the Job not concluded yet: waiting for the GPU",
			state: prewarmState{job: active(), pods: rejected()},
			live:  node(""),
			want:  StepInProgress,
			message: "waiting for the pool's node " + nodeName + " to advertise its GPU: it joined at 2026-09-18T06:04:07Z, its device plugin has not registered nvidia.com/gpu yet (the GPU operator installs the driver and the container toolkit first); " +
				"the node rejected the placeholder's pod mc-gpu-l4-prewarm-abc12 meanwhile (UnexpectedAdmissionError: Allocate failed due to no healthy devices present; cannot allocate unhealthy devices nvidia.com/gpu, which is unexpected), which the Job does not replace (backoffLimit 0): the node stays, and the first workload is placed on it once the GPU is advertised",
			since: "2026-09-18T06:04:07Z",
		},
		{
			name:     "rejected before its GPU, the GPU advertised since: done",
			state:    prewarmState{job: failedJob("BackoffLimitExceeded", "Job has reached the specified backoff limit", "2026-09-18T06:04:41Z"), pods: rejected()},
			live:     node("1"),
			want:     StepDone,
			message:  "the pool's node " + nodeName + " is up and advertises 1 nvidia.com/gpu; it rejected the placeholder's pod mc-gpu-l4-prewarm-abc12 before (UnexpectedAdmissionError: Allocate failed due to no healthy devices present; cannot allocate unhealthy devices nvidia.com/gpu, which is unexpected), which the Job does not replace (backoffLimit 0): nothing holds the node until the first workload, and Karpenter consolidates it if it stays empty",
			since:    "2026-09-18T06:00:08Z",
			finished: "2026-09-18T06:04:41Z",
		},
		{
			name:    "GPU advertised, the placeholder not placed yet: pending on the scheduler",
			state:   prewarmState{job: active(), pods: pod("Pending", "", unschedulable())},
			live:    node("1"),
			want:    StepInProgress,
			message: "pending: the pool's node " + nodeName + " advertises 1 nvidia.com/gpu, the scheduler has not placed the placeholder yet (0/6 nodes are available: 6 Insufficient nvidia.com/gpu.)",
			since:   "2026-09-18T06:04:07Z",
		},
		{
			name:    "GPU advertised, the placeholder bound: starting",
			state:   prewarmState{job: active(), pods: pod("Pending", nodeName, map[string]any{})},
			live:    node("1"),
			want:    StepInProgress,
			message: "starting on node " + nodeName + ": the scheduler placed the placeholder, its container is being created",
			since:   "2026-09-18T06:00:09Z",
		},
		{
			name:    "GPU advertised, the placeholder running: holding the node",
			state:   prewarmState{job: active(), pods: pod("Running", nodeName, map[string]any{"startTime": "2026-09-18T06:04:41Z"})},
			live:    node("1"),
			want:    StepInProgress,
			message: "holding node " + nodeName + " until the first workload preempts the placeholder or its hold ends",
			since:   "2026-09-18T06:04:41Z",
		},
		{
			name:     "GPU advertised, the hold ended: done",
			state:    prewarmState{job: job(map[string]any{"completionTime": "2026-09-18T06:19:45Z", "conditions": []any{map[string]any{"type": "Complete", "status": "True", "lastTransitionTime": "2026-09-18T06:19:45Z"}}})},
			live:     node("1"),
			want:     StepDone,
			message:  "finished: the hold ended without a workload; Karpenter consolidates the empty node",
			since:    "2026-09-18T06:00:08Z",
			finished: "2026-09-18T06:19:45Z",
		},
		{
			name:     "node Ready serving a model, the placeholder Pending behind it: done",
			state:    prewarmState{job: active(), pods: pod("Pending", "", unschedulable())},
			live:     node("1", predictor),
			want:     StepDone,
			message:  "the first workload runs on the pool's node " + nodeName + ": " + predictor + "; the placeholder's pod mc-gpu-l4-prewarm-abc12 (Pending) waits behind it at negative priority until the Job's deadline",
			since:    "2026-09-18T06:00:08Z",
			finished: "2026-09-18T06:04:07Z",
		},
		{
			name:     "node serving a model, the placeholder ended at its deadline: done, not failed",
			state:    prewarmState{job: failedJob("DeadlineExceeded", "Job was active longer than specified deadline", "2026-09-18T06:25:08Z")},
			live:     node("1", predictor),
			want:     StepDone,
			message:  "the first workload runs on the pool's node " + nodeName + ": " + predictor + "; the placeholder Job has ended (DeadlineExceeded Job was active longer than specified deadline)",
			since:    "2026-09-18T06:00:08Z",
			finished: "2026-09-18T06:25:08Z",
		},
		{
			name:     "preempted, its pod kept with the disruption: preempted",
			state:    prewarmState{job: failedJob("BackoffLimitExceeded", "Job has reached the specified backoff limit", "2026-09-18T06:09:02Z"), pods: pod("Failed", nodeName, map[string]any{"conditions": []any{map[string]any{"type": "DisruptionTarget", "status": "True", "reason": "PreemptionByScheduler"}}})},
			live:     node("1"),
			want:     StepDone,
			message:  "preempted: the first workload took the placeholder's node before its hold ended (BackoffLimitExceeded Job has reached the specified backoff limit)",
			since:    "2026-09-18T06:00:08Z",
			finished: "2026-09-18T06:09:02Z",
		},
		{
			name:     "rejected and the pool has no node left: failed",
			state:    prewarmState{job: failedJob("BackoffLimitExceeded", "Job has reached the specified backoff limit", "2026-09-18T06:04:41Z"), pods: rejected()},
			live:     &poolLive{},
			want:     StepFailed,
			message:  "the placeholder's pod mc-gpu-l4-prewarm-abc12 was rejected by node " + nodeName + " (UnexpectedAdmissionError: Allocate failed due to no healthy devices present; cannot allocate unhealthy devices nvidia.com/gpu, which is unexpected) and the pool has no node now: the first predictor launches one",
			since:    "2026-09-18T06:00:08Z",
			finished: "2026-09-18T06:04:41Z",
		},
		{
			name: "the hold container failed: failed with its reason",
			state: prewarmState{job: failedJob("BackoffLimitExceeded", "Job has reached the specified backoff limit", "2026-09-18T06:05:02Z"), pods: pod("Failed", nodeName, map[string]any{"containerStatuses": []any{
				map[string]any{"name": "hold", "state": map[string]any{"terminated": map[string]any{"reason": "Error", "exitCode": int64(127)}}},
			}})},
			live:     node("1"),
			want:     StepFailed,
			message:  "the placeholder's pod mc-gpu-l4-prewarm-abc12 failed on node " + nodeName + " (container hold terminated: Error, exit code 127): check the placeholder image and the node's kubelet log",
			since:    "2026-09-18T06:00:08Z",
			finished: "2026-09-18T06:05:02Z",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.state.namespace, tc.state.name = "org-acme", "mc-gpu-l4-prewarm"
			st := prewarmStep(&tc.state, true, "", tc.live)
			assert.Equal(t, StepPrewarm, st.Name)
			assert.Equal(t, tc.want, st.State)
			assert.Equal(t, tc.message, st.Message)
			assert.Equal(t, tc.since, st.Since)
			assert.Equal(t, tc.finished, st.FinishedAt)
		})
	}
}

// TestListNodePoolsNamesIdleNodesOfGPUPoolsOnly (giantswarm/cluster-manager#49):
// what holds a node — a GPU workload or a predictor — is a GPU pool's
// notion. The cluster's general Karpenter pool, made by other means, carries
// the cluster's workloads: its nodes are never named idle and their pods are
// not read (on an installation that was fifteen pod lists per call and every
// node of the platform's pool "idle"); the GPU pool's nodes are judged, one
// pod list per node.
func TestListNodePoolsNamesIdleNodesOfGPUPoolsOnly(t *testing.T) {
	l := newLab(t, "installation.yaml")
	l.add(t, l.installation, "general-pool.yaml").add(t, l.targets[wc1APIServer], "targets/wc1-general.yaml")
	var podLists atomic.Int32
	fakeTarget(t, l, wc1APIServer).PrependReactor("list", detect.PodsGVR.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		podLists.Add(1)
		return false, nil, nil
	})
	pools, err := l.service(Config{Installation: "gazelle"}).ListNodePools(context.Background(), "wc1", "")
	require.NoError(t, err)
	byName := map[string]NodePool{}
	for _, p := range pools.NodePools {
		byName[p.Name] = p
	}
	require.Len(t, byName, 3)

	general := byName["wc1-general"]
	assert.Equal(t, PhaseReady, general.Phase)
	assert.Nil(t, general.OwnerRelease)
	assert.Equal(t, "1 node(s) ready (aws:///eu-west-1a/i-0a1b2c3d4e5f60010)", general.Steps[len(general.Steps)-1].Message, "no idle clause on a pool that is not a GPU pool")

	gpu := byName["wc1-gpu-a10g"]
	assert.Contains(t, gpu.Steps[2].Message, "2 idle since 2026-09-16T12:30:00Z (wc1-gpu-a10g-node-1, wc1-gpu-a10g-node-2)")
	assert.Equal(t, int32(2), podLists.Load(), "the GPU pool's two nodes are read, the general pool's node is not")
}

// TestNodesStepNamesTerminatingNodes (giantswarm/cluster-manager#57): a node
// whose NodeClaim is deleted is named — by its Node, else by the claim while
// none is registered — with the claim's deletion time and what goes on
// meanwhile, after the counts; a launching claim is counted beside; the step
// is in progress since the latest event among the claims.
func TestNodesStepNamesTerminatingNodes(t *testing.T) {
	mp := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cluster.x-k8s.io/v1beta1", "kind": "MachinePool",
		"metadata": map[string]any{"name": "mc-gpu-l40s", "namespace": "org-acme", "creationTimestamp": "2026-09-18T03:22:00Z"},
		"spec":     map[string]any{"replicas": int64(2)},
		"status":   map[string]any{"replicas": int64(2), "readyReplicas": int64(2), "conditions": []any{condition("Ready", "True", "", "", "2026-09-18T03:22:14Z")}},
	}}
	registered := fakeClaim("mc-gpu-l40s-rwwqj", "2026-09-18T03:22:23Z", condition("Ready", "True", "", "", "2026-09-18T03:25:36Z"))
	registered.Object["metadata"].(map[string]any)["deletionTimestamp"] = "2026-09-18T03:36:08Z"
	node := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Node",
		"metadata": map[string]any{"name": "ip-10-0-147-35.eu-central-1.compute.internal"},
	}}
	unregistered := fakeClaim("mc-gpu-l40s-x7k2p", "2026-09-18T03:30:00Z")
	unregistered.Object["metadata"].(map[string]any)["deletionTimestamp"] = "2026-09-18T03:31:00Z"
	launching := fakeClaim("mc-gpu-l40s-n3wb1", "2026-09-18T03:37:00Z")

	live := &poolLive{nodes: []*poolNode{{claim: registered, node: node}, {claim: unregistered}, {claim: launching}}}
	st := nodesStep(mp, nil, live, true, true, launchContext{})
	assert.Equal(t, StepInProgress, st.State)
	assert.Equal(t, "2026-09-18T03:37:00Z", st.Since, "since the latest event among the claims: the launching one's creation")
	assert.Equal(t, "1 NodeClaim(s) launching, 0 ready, 2 terminating — ip-10-0-147-35.eu-central-1.compute.internal terminating since 2026-09-18T03:36:08Z (Karpenter drains the node, then terminates its instance); mc-gpu-l40s-x7k2p terminating since 2026-09-18T03:31:00Z (Karpenter drains the node, then terminates its instance)", st.Message,
		"the registered node by its Node's name, the unregistered by its claim's, each since its deletion")

	only := &poolLive{nodes: []*poolNode{{claim: registered, node: node}}}
	st = nodesStep(mp, nil, only, true, true, launchContext{})
	assert.Equal(t, StepInProgress, st.State)
	assert.Equal(t, "2026-09-18T03:36:08Z", st.Since, "since the NodeClaim's deletion")
	assert.Equal(t, "0 NodeClaim(s) launching, 0 ready, 1 terminating — ip-10-0-147-35.eu-central-1.compute.internal terminating since 2026-09-18T03:36:08Z (Karpenter drains the node, then terminates its instance)", st.Message)
}

// TestTerminationStageFollowsKarpentersConditions:
// a terminating node says where its termination stands from its NodeClaim's
// conditions — draining with Karpenter's message, drained with the volumes
// detaching, the instance shutting down and no longer billed — and falls
// back to what Karpenter does when the claim carries none of them.
func TestTerminationStageFollowsKarpentersConditions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		conds []map[string]any
		want  string
	}{
		{"no termination condition yet", nil, "Karpenter drains the node, then terminates its instance"},
		{"draining", []map[string]any{condition("Drained", "Unknown", "Draining", "awaiting pod eviction: model-serving/qwen3-predictor-0", "2026-10-07T17:08:03Z")}, "draining: awaiting pod eviction: model-serving/qwen3-predictor-0"},
		{"drained", []map[string]any{condition("Drained", "True", "Drained", "", "2026-10-07T17:08:40Z"), condition("VolumesDetached", "Unknown", "AwaitingVolumeDetachment", "", "2026-10-07T17:08:40Z")}, "drained since 2026-10-07T17:08:40Z, its volumes detaching before the instance is terminated"},
		{"instance shutting down", []map[string]any{condition("Drained", "True", "Drained", "", "2026-10-07T17:08:40Z"), condition("VolumesDetached", "True", "VolumesDetached", "", "2026-10-07T17:08:41Z"), condition("InstanceTerminating", "True", "InstanceTerminating", "", "2026-10-07T17:08:42Z")}, "instance shutting down since 2026-10-07T17:08:42Z, no longer billed; EC2 reports it terminated about five minutes later, then the NodeClaim goes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim := fakeClaim("mc-gpu-a10g-mtv2k", "2026-10-07T16:57:34Z", tc.conds...)
			claim.Object["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-07T17:08:02Z"
			assert.Equal(t, tc.want, (&poolNode{claim: claim}).terminationStage())
		})
	}
}
