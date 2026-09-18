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

// add puts a fixture's objects into a fake client beside what it has.
func (l *lab) add(t *testing.T, dyn dynamic.Interface, fixture string) *lab {
	t.Helper()
	tracker := dyn.(*dynamicfake.FakeDynamicClient).Tracker()
	for _, obj := range loadFixtures(t, fixture) {
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
	assert.Equal(t, "0 NodeClaim(s) launching, 0 ready, 1 terminating", term.Steps[2].Message)
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
	cases := []struct {
		name     string
		state    prewarmState
		want     string
		message  string
		since    string
		finished string
	}{
		{"absent", prewarmState{}, StepDone, "no placeholder Job org-acme/mc-gpu-l4-prewarm: removed ten minutes after it ended, or prewarm was set on an existing pool (the Job is created with the release's install only)", "", ""},
		{"created, no pod", prewarmState{job: job(map[string]any{"active": int64(0)})}, StepInProgress, "placeholder Job org-acme/mc-gpu-l4-prewarm created, its pod not yet", "2026-09-18T06:00:08Z", ""},
		{"pending", prewarmState{job: job(map[string]any{"active": int64(1)}), pods: []unstructured.Unstructured{pod("Pending", nil, map[string]any{"conditions": []any{map[string]any{"type": "PodScheduled", "status": "False", "reason": "Unschedulable", "message": "0/6 nodes are available: 6 Insufficient nvidia.com/gpu."}}})}},
			StepInProgress, "pending: the placeholder waits for the pool's first node, launched by Karpenter for its GPU (0/6 nodes are available: 6 Insufficient nvidia.com/gpu.)", "2026-09-18T06:00:09Z", ""},
		{"holding", prewarmState{job: job(map[string]any{"active": int64(1)}), pods: []unstructured.Unstructured{pod("Running", nil, map[string]any{"startTime": "2026-09-18T06:04:41Z"})}},
			StepInProgress, "holding node ip-10-0-1-23.eu-central-1.compute.internal until the first workload preempts the placeholder or its hold ends", "2026-09-18T06:04:41Z", ""},
		{"being preempted", prewarmState{job: job(map[string]any{"active": int64(1)}), pods: []unstructured.Unstructured{pod("Running", map[string]any{"deletionTimestamp": "2026-09-18T06:09:00Z"}, map[string]any{"startTime": "2026-09-18T06:04:41Z"})}},
			StepInProgress, "preempted: the placeholder's pod is terminating, the first workload takes its node ip-10-0-1-23.eu-central-1.compute.internal", "2026-09-18T06:09:00Z", ""},
		{"preempted", prewarmState{job: job(failed("BackoffLimitExceeded", "Job has reached the specified backoff limit", "2026-09-18T06:09:02Z"))},
			StepDone, "preempted: the first workload took the placeholder's node before its hold ended (BackoffLimitExceeded Job has reached the specified backoff limit)", "2026-09-18T06:00:08Z", "2026-09-18T06:09:02Z"},
		{"finished", prewarmState{job: job(map[string]any{"completionTime": "2026-09-18T06:19:45Z", "conditions": []any{map[string]any{"type": "Complete", "status": "True", "lastTransitionTime": "2026-09-18T06:19:45Z"}}})},
			StepDone, "finished: the hold ended without a workload; Karpenter consolidates the empty node", "2026-09-18T06:00:08Z", "2026-09-18T06:19:45Z"},
		{"no node came", prewarmState{job: job(failed("DeadlineExceeded", "Job was active longer than specified deadline", "2026-09-18T06:25:08Z"))},
			StepFailed, "no node came within the placeholder's deadline of 1500 s (DeadlineExceeded Job was active longer than specified deadline): check the pool's NodeClaims and Karpenter's log", "2026-09-18T06:00:08Z", "2026-09-18T06:25:08Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.state.namespace, tc.state.name = "org-acme", "mc-gpu-l4-prewarm"
			st := prewarmStep(&tc.state)
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
