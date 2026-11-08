package tools

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/giantswarm/cluster-manager/internal/detect"
)

// A pool's Karpenter objects on the workload cluster outlive its release
// (giantswarm/cluster-manager#219). The pool's KarpenterMachinePool
// controller deletes the cluster's NodePool and EC2NodeClass named after the
// pool and leaves the NodeClaims to Karpenter; a NodeClaim Karpenter does not
// disrupt then keeps its instance running and billed for a pool nothing
// manages, and holds the EC2NodeClass (Karpenter's finalizer waits for its
// NodeClaims). So a forced delete removes the pool's NodeClaims itself, as
// the caller, before anything else — as the idle ones go without force — and
// names a NodeClaim still running at its end; and a re-run once the pool's
// release is gone removes what of the pool is left on the cluster
// (poolLeftovers) and names each, instead of answering not found over it.

var (
	// karpenterNodePoolGVR is Karpenter's NodePool on the workload cluster,
	// named after the pool's release.
	karpenterNodePoolGVR = schema.GroupVersionResource{Group: "karpenter.sh", Version: "v1", Resource: "nodepools"}
	// ec2NodeClassGVR is Karpenter's AWS node class on the workload cluster,
	// named after the pool's release.
	ec2NodeClassGVR = schema.GroupVersionResource{Group: "karpenter.k8s.aws", Version: "v1", Resource: "ec2nodeclasses"}
)

// running are the pool's NodeClaims not yet deleted: launching, ready, or
// refused a launch.
func (l *poolLive) running() []*poolNode {
	var out []*poolNode
	for _, n := range l.nodes {
		if n.claim != nil && !n.terminating() {
			out = append(out, n)
		}
	}
	return out
}

// forcedNodes are the nodes a forced delete removes through their NodeClaims:
// every NodeClaim of the pool not deleted yet, busy or not. A cluster or a
// claim list not readable as the caller removes none and says so: the
// re-run removes what outlives the pool.
func forcedNodes(ctx context.Context, t target, pool string, out *WriteResult) []*poolNode {
	why := t.Reason
	if t.Reader != nil {
		live := readPoolLive(ctx, t.Reader, pool, false)
		if live.claimsErr == nil {
			return live.running()
		}
		why = live.claimsErr.Error()
	}
	out.Warnings = append(out.Warnings, fmt.Sprintf("the NodeClaims of pool %s on %s cannot be read as you (%s): they are not removed by this call — re-run delete_node_pool with force once they can, it removes any that outlives the pool", pool, t.Cluster, why))
	return nil
}

// claimsLeft names, at the end of a forced delete, the pool's NodeClaims
// still running on the cluster: one Karpenter launched while the teardown
// ran, or one whose delete was refused. Nothing is read on a dry run or a
// cut teardown, whose pending objects the re-run writes first.
func claimsLeft(td *teardown, t target, pool string) {
	if td.dryRun || td.cut || t.Reader == nil {
		return
	}
	live := readPoolLive(td.ctx, t.Reader, pool, false)
	for _, n := range live.running() {
		td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("NodeClaim %s (node %s) of pool %s still runs on %s after the forced delete: its instance keeps running and billed — re-run delete_node_pool with force, it removes the NodeClaims that outlive the pool", n.claim.GetName(), n.name(), pool, t.Cluster))
	}
}

// poolLeftovers is what of a removed pool is left on its workload cluster:
// its Karpenter NodePool and EC2NodeClass, and its NodeClaims with their
// nodes (the pods on them read unless forced, for what holds them). An
// object not readable as the caller counts as none.
type poolLeftovers struct {
	pool      string
	nodePool  *unstructured.Unstructured
	nodeClass *unstructured.Unstructured
	live      *poolLive
}

func readPoolLeftovers(ctx context.Context, t target, pool string, force bool) *poolLeftovers {
	left := &poolLeftovers{pool: pool, live: readPoolLive(ctx, t.Reader, pool, !force)}
	left.nodePool = getOrNone(ctx, t, karpenterNodePoolGVR, pool)
	left.nodeClass = getOrNone(ctx, t, ec2NodeClassGVR, pool)
	return left
}

// getOrNone reads a cluster-scoped object on the target; nil when it is
// absent, its API not served, or it is not readable as the caller.
func getOrNone(ctx context.Context, t target, gvr schema.GroupVersionResource, name string) *unstructured.Unstructured {
	obj, err := t.Reader.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return obj
}

// empty: nothing of the pool is left.
func (l *poolLeftovers) empty() bool {
	return l.nodePool == nil && l.nodeClass == nil && len(l.live.claims()) == 0
}

// remove removes what is left of the pool, in order: the NodePool first, so
// Karpenter launches no node for it any more, then the NodeClaims — busy ones
// only when forced, a busy one otherwise named with what holds it —, the
// EC2NodeClass last (Karpenter's finalizer holds it until its NodeClaims are
// gone). Each is named in the answer; one being deleted already is answered
// terminating with where it stands.
func (l *poolLeftovers) remove(td *teardown, t target, force bool) error {
	if err := l.removeObject(td, t, karpenterNodePoolGVR, l.nodePool, "Karpenter launches no node for it any more"); err != nil {
		return err
	}
	res := t.Reader.Resource(detect.NodeClaimGVR)
	for _, n := range l.live.nodes {
		if n.claim == nil {
			continue
		}
		act := ObjectAction{APIVersion: n.claim.GetAPIVersion(), Kind: n.claim.GetKind(), Name: n.claim.GetName()}
		if n.terminating() {
			act.Action = actionTerminating
			td.out.Objects = append(td.out.Objects, act)
			td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("NodeClaim %s (node %s) of the removed pool %s on %s is terminating since %s: %s", act.Name, n.name(), l.pool, t.Cluster, n.deletedAt(), n.terminationStage()))
			continue
		}
		if holds := l.live.holds(n); holds != "" && !force {
			td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("NodeClaim %s outlived pool %s on %s and is kept: %s — its instance runs and is billed for a pool nothing manages; free the node and re-run delete_node_pool, or re-run with force to remove it regardless", act.Name, l.pool, t.Cluster, holds))
			continue
		}
		err := td.delete(res, act)
		if apierrors.IsForbidden(err) {
			td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("NodeClaim %s (node %s) outlived pool %s on %s and is left: %v — its instance runs and is billed until someone allowed to deletes it (kubectl delete nodeclaim %s)", act.Name, n.name(), l.pool, t.Cluster, err, act.Name))
			continue
		}
		if err != nil {
			return err
		}
		td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("NodeClaim %s (node %s) outlived pool %s on %s: %s", act.Name, n.name(), l.pool, t.Cluster, td.outcome("removed, Karpenter drains the node and terminates its instance")))
	}
	return l.removeObject(td, t, ec2NodeClassGVR, l.nodeClass, "Karpenter removes it once the pool's NodeClaims are gone")
}

// removeObject removes one of the pool's Karpenter objects left on the
// cluster (nil: none), named in the answer.
func (l *poolLeftovers) removeObject(td *teardown, t target, gvr schema.GroupVersionResource, obj *unstructured.Unstructured, then string) error {
	if obj == nil {
		return nil
	}
	act := ObjectAction{APIVersion: obj.GetAPIVersion(), Kind: obj.GetKind(), Name: obj.GetName()}
	if ts := obj.GetDeletionTimestamp(); ts != nil {
		act.Action = actionTerminating
		td.out.Objects = append(td.out.Objects, act)
		td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("%s %s of the removed pool %s on %s is terminating since %s: %s", act.Kind, act.Name, l.pool, t.Cluster, ts.UTC().Format(time.RFC3339), then))
		return nil
	}
	err := td.delete(t.Reader.Resource(gvr), act)
	if apierrors.IsForbidden(err) {
		td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("%s %s outlived pool %s on %s and is left: %v — remove it as someone allowed to", act.Kind, act.Name, l.pool, t.Cluster, err))
		return nil
	}
	if err != nil {
		return err
	}
	td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("%s %s outlived pool %s on %s: %s", act.Kind, act.Name, l.pool, t.Cluster, td.outcome("removed")))
	return nil
}

// outcome says what became of a removal just recorded: done, pending once
// the teardown is cut, or what a dry run would do.
func (td *teardown) outcome(done string) string {
	switch {
	case td.cut:
		return "pending, the re-run removes it"
	case td.dryRun:
		return "a dry run removes nothing; the call removes it"
	}
	return done
}
