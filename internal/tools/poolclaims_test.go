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

// TestForcedDeleteNodePoolSweepsANodeClaimLaunchedMeanwhile: Karpenter
// launches a node for a Pending predictor while the forced teardown runs,
// after the pool's NodeClaims were deleted. The call's last step removes it
// too, named in the answer.
func TestForcedDeleteNodePoolSweepsANodeClaimLaunchedMeanwhile(t *testing.T) {
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
	assert.Equal(t, []string{"delete NodeClaim /wc1-gpu-a10g-k7m2p", "delete NodeClaim /wc1-gpu-a10g-q9x4z", "delete OCIRepository org-acme/wc1-gpu-a10g", "delete HelmRelease org-acme/wc1-gpu-a10g", "delete NodeClaim /wc1-gpu-a10g-n3w0n"}, objectNames(out))
	assert.Empty(t, out.Warnings)
	assert.Empty(t, poolClaims(t, lab, "wc1-gpu-a10g"))
}

// TestForcedDeleteNodePoolGoesOnWhenANodeClaimDeleteIsForbidden: forced, a
// NodeClaim the caller may not delete is no refusal — the pool goes — but
// the answer names each node whose NodeClaim still runs, and how it goes.
func TestForcedDeleteNodePoolGoesOnWhenANodeClaimDeleteIsForbidden(t *testing.T) {
	lab := newLab(t, "installation.yaml")
	svc := lab.service(Config{Installation: "gazelle"})
	fakeTarget(t, lab, wc1APIServer).PrependReactor("delete", detect.NodeClaimGVR.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(detect.NodeClaimGVR.GroupResource(), "", errors.New("User \"alice\" cannot delete resource \"nodeclaims\""))
	})

	out, err := svc.DeleteNodePool(context.Background(), DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply, Force: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"delete OCIRepository org-acme/wc1-gpu-a10g", "delete HelmRelease org-acme/wc1-gpu-a10g"}, objectNames(out)[len(out.Objects)-2:], "the pool goes regardless")
	require.Len(t, out.Warnings, 4, "the first step and the last both name the two nodes")
	assert.Contains(t, out.Warnings[0], "node wc1-gpu-a10g-node-1 of the pool is not removed by this call, and its instance runs until its NodeClaim is deleted — have someone allowed to delete it (kubectl delete nodeclaim wc1-gpu-a10g-k7m2p)")
	assert.Contains(t, out.Warnings[3], "node wc1-gpu-a10g-node-2 of the pool is not removed by this call")
	assert.Len(t, poolClaims(t, lab, "wc1-gpu-a10g"), 2)
}

// TestDeleteRemovedPoolNamesWhatOutlivesIt (giantswarm/cluster-manager#219):
// the pool's release is gone, and on the cluster its NodeClaims, its
// NodePool and its EC2NodeClass are left — the node keeps running and
// billed. Nothing there carries cluster-manager's mark, and the release that
// proved the pool its own is gone: the re-run removes nothing on the cluster
// and names each object with where it stands, forced or not — a NodeClaim
// with what holds its node, one being deleted as terminating. With nothing
// left the pool is not found.
func TestDeleteRemovedPoolNamesWhatOutlivesIt(t *testing.T) {
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
	nodePool := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "karpenter.sh/v1", "kind": "NodePool", "metadata": map[string]any{"name": "wc1-gpu-a10g"}}}
	_, err = target.Resource(karpenterNodePoolGVR).Create(ctx, nodePool, metav1.CreateOptions{})
	require.NoError(t, err)

	for _, force := range []bool{false, true} {
		out, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply, Force: force})
		require.NoError(t, err)
		assert.Equal(t, "gpu-a10g", out.Pool)
		assert.Equal(t, []string{"terminating EC2NodeClass /wc1-gpu-a10g"}, objectNames(out))
		assert.Equal(t, []string{
			"NodePool wc1-gpu-a10g outlived pool wc1-gpu-a10g on wc1: Karpenter may still launch a node for it — delete it once its NodeClaims are gone (kubectl delete nodepool wc1-gpu-a10g)",
			"NodeClaim wc1-gpu-a10g-k7m2p (node wc1-gpu-a10g-node-1) outlived pool wc1-gpu-a10g on wc1 and still runs (node wc1-gpu-a10g-node-1 runs model-serving/llama-3-8b-kserve-6649fb66c8-dllt7 (1 GPU)): its instance is billed for a pool nothing manages, and cluster-manager no longer tells it its own, the pool's release that proved it being gone — delete it once nothing needs the node (kubectl delete nodeclaim wc1-gpu-a10g-k7m2p); Karpenter drains the node and terminates the instance",
			"NodeClaim wc1-gpu-a10g-q9x4z (node wc1-gpu-a10g-node-2) outlived pool wc1-gpu-a10g on wc1 and still runs (idle): its instance is billed for a pool nothing manages, and cluster-manager no longer tells it its own, the pool's release that proved it being gone — delete it once nothing needs the node (kubectl delete nodeclaim wc1-gpu-a10g-q9x4z); Karpenter drains the node and terminates the instance",
			"EC2NodeClass wc1-gpu-a10g of the removed pool wc1-gpu-a10g on wc1 is terminating since 2026-10-10T04:00:00Z: Karpenter removes it once the pool's NodeClaims are gone",
		}, out.Warnings, "force %v", force)
		assert.Len(t, poolClaims(t, lab, "wc1-gpu-a10g"), 2, "nothing removed on the cluster, force %v", force)
	}

	for _, name := range []string{"wc1-gpu-a10g-k7m2p", "wc1-gpu-a10g-q9x4z"} {
		require.NoError(t, target.Resource(detect.NodeClaimGVR).Delete(ctx, name, metav1.DeleteOptions{}))
	}
	require.NoError(t, target.Resource(ec2NodeClassGVR).Delete(ctx, "wc1-gpu-a10g", metav1.DeleteOptions{}))
	require.NoError(t, target.Resource(karpenterNodePoolGVR).Delete(ctx, "wc1-gpu-a10g", metav1.DeleteOptions{}))
	terminating := nodeClaim("wc1-gpu-a10g-z8y7x", "wc1-gpu-a10g")
	terminating.SetDeletionTimestamp(&metav1.Time{Time: time.Date(2026, 10, 10, 4, 0, 0, 0, time.UTC)})
	_, err = target.Resource(detect.NodeClaimGVR).Create(ctx, terminating, metav1.CreateOptions{})
	require.NoError(t, err)
	out, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply})
	require.NoError(t, err)
	assert.Equal(t, []string{"terminating NodeClaim /wc1-gpu-a10g-z8y7x"}, objectNames(out))
	assert.Equal(t, []string{"NodeClaim wc1-gpu-a10g-z8y7x (node wc1-gpu-a10g-z8y7x) of the removed pool wc1-gpu-a10g on wc1 is terminating since 2026-10-10T04:00:00Z: Karpenter drains the node, then terminates its instance"}, out.Warnings)

	require.NoError(t, target.Resource(detect.NodeClaimGVR).Delete(ctx, "wc1-gpu-a10g-z8y7x", metav1.DeleteOptions{}))
	_, err = svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply})
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
