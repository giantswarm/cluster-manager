package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// A pool's Karpenter objects on the workload cluster outlive its release
// (giantswarm/cluster-manager#219). The pool's KarpenterMachinePool
// controller deletes the cluster's NodePool and EC2NodeClass named after the
// pool and leaves the NodeClaims to Karpenter; a NodeClaim Karpenter does not
// disrupt then keeps its instance running and billed for a pool nothing
// manages, and holds the EC2NodeClass (Karpenter's finalizer waits for its
// NodeClaims). So a forced delete removes the pool's NodeClaims itself, as
// the caller — as the idle ones go without force —: first, and once more at
// its end for one Karpenter launched while the teardown ran, naming one it
// could not remove. The pool's release is what proves the pool
// cluster-manager's: nothing on the workload cluster carries its mark, so a
// re-run once the release is gone removes nothing there and names what of
// the pool is left (poolLeftovers), instead of answering not found over it.

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
// claim list not readable as the caller removes none and says so.
func forcedNodes(ctx context.Context, t target, pool string, out *WriteResult) []*poolNode {
	why := t.Reason
	if t.Reader != nil {
		live := readPoolLive(ctx, t.Reader, pool, false)
		if live.claimsErr == nil {
			return live.running()
		}
		why = live.claimsErr.Error()
	}
	out.Warnings = append(out.Warnings, fmt.Sprintf("the NodeClaims of pool %s on %s cannot be read as you (%s): they are not removed by this call, and one Karpenter does not disrupt keeps its instance running — re-run delete_node_pool with force while the pool's release is still being removed, once they can be read", pool, t.Cluster, why))
	return nil
}

// sweepClaims is a forced delete's last step: the pool's NodeClaims still
// running — one Karpenter launched for a Pending predictor while the
// teardown ran — are removed too. Nothing is read on a dry run or a cut
// teardown, whose pending objects the re-run writes first.
func sweepClaims(td *teardown, t target, pool string) error {
	if td.dryRun || td.cut || t.Reader == nil {
		return nil
	}
	return td.deleteNodeClaims(t.Reader, readPoolLive(td.ctx, t.Reader, pool, false).running(), true)
}

// poolLeftovers is what of a removed pool is left on its workload cluster:
// its Karpenter NodePool and EC2NodeClass, and its NodeClaims with their
// nodes and what holds them. An object not readable as the caller counts as
// none.
type poolLeftovers struct {
	pool      string
	nodePool  *unstructured.Unstructured
	nodeClass *unstructured.Unstructured
	live      *poolLive
}

func readPoolLeftovers(ctx context.Context, t target, pool string) *poolLeftovers {
	return &poolLeftovers{
		pool:      pool,
		live:      readPoolLive(ctx, t.Reader, pool, true),
		nodePool:  getOrNone(ctx, t, karpenterNodePoolGVR, pool),
		nodeClass: getOrNone(ctx, t, ec2NodeClassGVR, pool),
	}
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

// name answers what of the pool is left, each object with where it stands:
// one being deleted as terminating, one that stands with what it costs and
// the command that removes it — cluster-manager removes nothing it cannot
// tell is its own, and with the pool's release gone nothing proves these
// are.
func (l *poolLeftovers) name(td *teardown, t target) {
	const unproven = "cluster-manager no longer tells it its own, the pool's release that proved it being gone"
	l.nameObject(td, t, l.nodePool, "Karpenter removes it once its NodeClaims are gone", "Karpenter may still launch a node for it")
	for _, n := range l.live.nodes {
		if n.claim == nil {
			continue
		}
		name := n.claim.GetName()
		if n.terminating() {
			td.out.Objects = append(td.out.Objects, ObjectAction{APIVersion: n.claim.GetAPIVersion(), Kind: n.claim.GetKind(), Name: name, Action: actionTerminating})
			td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("NodeClaim %s (node %s) of the removed pool %s on %s is terminating since %s: %s", name, n.name(), l.pool, t.Cluster, n.deletedAt(), n.terminationStage()))
			continue
		}
		state := "idle"
		if holds := l.live.holds(n); holds != "" {
			state = holds
		}
		td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("NodeClaim %s (node %s) outlived pool %s on %s and still runs (%s): its instance is billed for a pool nothing manages, and %s — delete it once nothing needs the node (kubectl delete nodeclaim %s); Karpenter drains the node and terminates the instance", name, n.name(), l.pool, t.Cluster, state, unproven, name))
	}
	l.nameObject(td, t, l.nodeClass, "Karpenter removes it once the pool's NodeClaims are gone", "nothing removes it")
}

// nameObject names one of the pool's Karpenter objects left on the cluster
// (nil: none).
func (l *poolLeftovers) nameObject(td *teardown, t target, obj *unstructured.Unstructured, terminating, standing string) {
	if obj == nil {
		return
	}
	kind, name := obj.GetKind(), obj.GetName()
	if ts := obj.GetDeletionTimestamp(); ts != nil {
		td.out.Objects = append(td.out.Objects, ObjectAction{APIVersion: obj.GetAPIVersion(), Kind: kind, Name: name, Action: actionTerminating})
		td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("%s %s of the removed pool %s on %s is terminating since %s: %s", kind, name, l.pool, t.Cluster, ts.UTC().Format(time.RFC3339), terminating))
		return
	}
	td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("%s %s outlived pool %s on %s: %s — delete it once its NodeClaims are gone (kubectl delete %s %s)", kind, name, l.pool, t.Cluster, standing, strings.ToLower(kind), name))
}
