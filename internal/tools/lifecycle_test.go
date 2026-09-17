package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
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
