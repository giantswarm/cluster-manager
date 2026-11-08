package tools

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/cluster-manager/internal/detect"
)

// TestForcedDeleteNodePoolRemovesEveryNodeClaim
// (giantswarm/cluster-manager#219): forced, the pool's NodeClaims are deleted
// by the call itself, the busy node's too, before anything else — the
// release's removal leaves them to Karpenter, which kept a GPU node running
// for a pool that was gone. Nothing of the pool runs at the end, and the
// answer names no NodeClaim left.
func TestForcedDeleteNodePoolRemovesEveryNodeClaim(t *testing.T) {
	lab := newLab(t, "installation.yaml").target(t, wc1APIServer, "wc1-serving.yaml")
	svc := lab.service(Config{Installation: "gazelle"})

	out, err := svc.DeleteNodePool(context.Background(), DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply, Force: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"delete NodeClaim /wc1-gpu-a10g-k7m2p", "delete NodeClaim /wc1-gpu-a10g-q9x4z", "delete OCIRepository org-acme/wc1-gpu-a10g", "delete HelmRelease org-acme/wc1-gpu-a10g"}, objectNames(out))
	assert.Empty(t, poolClaims(t, lab, "wc1-gpu-a10g"), "the busy node's NodeClaim went too: Karpenter drains it and terminates the instance")
	assert.Empty(t, out.Warnings)
}

// TestForcedDeleteNodePoolNamesANodeClaimLaunchedMeanwhile: Karpenter launches
// a node for a Pending predictor while the forced teardown runs, after the
// pool's NodeClaims were deleted. The answer names the NodeClaim still
// running and the re-run that removes it, instead of passing it silently.
func TestForcedDeleteNodePoolNamesANodeClaimLaunchedMeanwhile(t *testing.T) {
	lab := newLab(t, "installation.yaml")
	svc := lab.service(Config{Installation: "gazelle"})
	fake := fakeTarget(t, lab, wc1APIServer)
	launched := false
	fake.PrependReactor("delete", detect.NodeClaimGVR.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		if !launched {
			// Through the tracker: the fake holds its lock while it reacts.
			launched = true
			require.NoError(t, fake.Tracker().Create(detect.NodeClaimGVR, nodeClaim("wc1-gpu-a10g-n3w0n", "wc1-gpu-a10g"), ""))
		}
		return false, nil, nil
	})

	out, err := svc.DeleteNodePool(context.Background(), DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply, Force: true})
	require.NoError(t, err)
	require.Len(t, out.Warnings, 1)
	assert.Equal(t, "NodeClaim wc1-gpu-a10g-n3w0n (node wc1-gpu-a10g-n3w0n) of pool wc1-gpu-a10g still runs on wc1 after the forced delete: its instance keeps running and billed — re-run delete_node_pool with force, it removes the NodeClaims that outlive the pool", out.Warnings[0])
}

// TestForcedDeleteNodePoolGoesOnWhenANodeClaimDeleteIsForbidden: forced, a
// NodeClaim the caller may not delete is no refusal — the pool goes — but
// the answer names the node and that its NodeClaim still runs.
func TestForcedDeleteNodePoolGoesOnWhenANodeClaimDeleteIsForbidden(t *testing.T) {
	lab := newLab(t, "installation.yaml")
	svc := lab.service(Config{Installation: "gazelle"})
	fakeTarget(t, lab, wc1APIServer).PrependReactor("delete", detect.NodeClaimGVR.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(detect.NodeClaimGVR.GroupResource(), "", errors.New("User \"alice\" cannot delete resource \"nodeclaims\""))
	})

	out, err := svc.DeleteNodePool(context.Background(), DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply, Force: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"delete OCIRepository org-acme/wc1-gpu-a10g", "delete HelmRelease org-acme/wc1-gpu-a10g"}, objectNames(out)[len(out.Objects)-2:], "the pool goes regardless")
	require.Len(t, out.Warnings, 4)
	assert.Contains(t, out.Warnings[0], "node wc1-gpu-a10g-node-1 of the pool is not removed by this call, and its instance runs until its NodeClaim is deleted")
	assert.Contains(t, out.Warnings[2], "NodeClaim wc1-gpu-a10g-k7m2p (node wc1-gpu-a10g-node-1) of pool wc1-gpu-a10g still runs on wc1 after the forced delete")
	assert.Len(t, poolClaims(t, lab, "wc1-gpu-a10g"), 2)
}

// TestDeleteRemovedPoolRemovesWhatOutlivesIt (giantswarm/cluster-manager#219):
// the pool's release is gone, and on the cluster its NodeClaims and its
// EC2NodeClass are left — nothing manages them any more, and the node keeps
// running and billed. The re-run removes the idle node's NodeClaim and names
// the busy one, kept without force; forced it removes that one too; the
// EC2NodeClass, terminating on Karpenter's finalizer until its NodeClaims are
// gone, and a NodeClaim terminating are named with where they stand. With
// nothing left the pool is not found.
func TestDeleteRemovedPoolRemovesWhatOutlivesIt(t *testing.T) {
	ctx := context.Background()
	lab := newLab(t, "installation.yaml").target(t, wc1APIServer, "wc1-serving.yaml")
	svc := lab.service(Config{Installation: "gazelle"})
	require.NoError(t, lab.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Delete(ctx, "wc1-gpu-a10g", metav1.DeleteOptions{}))
	target := lab.targets[wc1APIServer]
	nodeClass := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "karpenter.k8s.aws/v1", "kind": "EC2NodeClass",
		"metadata": map[string]any{"name": "wc1-gpu-a10g", "deletionTimestamp": "2026-10-10T04:00:00Z", "finalizers": []any{"karpenter.k8s.aws/termination"}},
	}}
	_, err := target.Resource(ec2NodeClassGVR).Create(ctx, nodeClass, metav1.CreateOptions{})
	require.NoError(t, err)
	del := DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply}

	dry := del
	dry.DryRun = true
	out, err := svc.DeleteNodePool(ctx, dry)
	require.NoError(t, err)
	assert.Equal(t, []string{"would-delete NodeClaim /wc1-gpu-a10g-q9x4z", "terminating EC2NodeClass /wc1-gpu-a10g"}, objectNames(out))
	assert.Len(t, poolClaims(t, lab, "wc1-gpu-a10g"), 2, "a dry run touches nothing")

	out, err = svc.DeleteNodePool(ctx, del)
	require.NoError(t, err)
	assert.Equal(t, "gpu-a10g", out.Pool)
	assert.Equal(t, []string{"delete NodeClaim /wc1-gpu-a10g-q9x4z", "terminating EC2NodeClass /wc1-gpu-a10g"}, objectNames(out))
	assert.Equal(t, []string{
		"NodeClaim wc1-gpu-a10g-k7m2p outlived pool wc1-gpu-a10g on wc1 and is kept: node wc1-gpu-a10g-node-1 runs model-serving/llama-3-8b-kserve-6649fb66c8-dllt7 (1 GPU) — its instance runs and is billed for a pool nothing manages; free the node and re-run delete_node_pool, or re-run with force to remove it regardless",
		"NodeClaim wc1-gpu-a10g-q9x4z (node wc1-gpu-a10g-node-2) outlived pool wc1-gpu-a10g on wc1: removed, Karpenter drains the node and terminates its instance",
		"EC2NodeClass wc1-gpu-a10g of the removed pool wc1-gpu-a10g on wc1 is terminating since 2026-10-10T04:00:00Z: Karpenter removes it once the pool's NodeClaims are gone",
	}, out.Warnings)

	forced := del
	forced.Force = true
	out, err = svc.DeleteNodePool(ctx, forced)
	require.NoError(t, err)
	assert.Equal(t, []string{"delete NodeClaim /wc1-gpu-a10g-k7m2p", "terminating EC2NodeClass /wc1-gpu-a10g"}, objectNames(out))
	assert.Empty(t, poolClaims(t, lab, "wc1-gpu-a10g"))

	terminating := nodeClaim("wc1-gpu-a10g-z8y7x", "wc1-gpu-a10g")
	terminating.SetDeletionTimestamp(&metav1.Time{Time: time.Date(2026, 10, 10, 4, 0, 0, 0, time.UTC)})
	_, err = target.Resource(detect.NodeClaimGVR).Create(ctx, terminating, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, target.Resource(ec2NodeClassGVR).Delete(ctx, "wc1-gpu-a10g", metav1.DeleteOptions{}))
	out, err = svc.DeleteNodePool(ctx, del)
	require.NoError(t, err)
	assert.Equal(t, []string{"terminating NodeClaim /wc1-gpu-a10g-z8y7x"}, objectNames(out))
	assert.Equal(t, []string{"NodeClaim wc1-gpu-a10g-z8y7x (node wc1-gpu-a10g-z8y7x) of the removed pool wc1-gpu-a10g on wc1 is terminating since 2026-10-10T04:00:00Z: Karpenter drains the node, then terminates its instance"}, out.Warnings)

	require.NoError(t, target.Resource(detect.NodeClaimGVR).Delete(ctx, "wc1-gpu-a10g-z8y7x", metav1.DeleteOptions{}))
	_, err = svc.DeleteNodePool(ctx, del)
	var notFound *ErrNotFound
	assert.True(t, errors.As(err, &notFound), "nothing of the pool left: %v", err)
}

// nodeClaim is a launched NodeClaim of a pool, no node registered yet.
func nodeClaim(name, pool string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "karpenter.sh/v1", "kind": "NodeClaim",
		"metadata": map[string]any{"name": name, "labels": map[string]any{detect.LabelKarpenterNodePool: pool}},
	}}
}
