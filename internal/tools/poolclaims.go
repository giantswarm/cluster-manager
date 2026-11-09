package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/giantswarm/cluster-manager/internal/compose"
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
// could not remove. The pool's NodePool goes only after the call, once Flux
// has uninstalled the release; until then Karpenter may launch a node for
// the pods the drain evicts, so the forced call says so and names the
// forced re-run. With the release gone nothing on the workload cluster
// carries cluster-manager's mark: a re-run names what of the pool is left
// (poolLeftovers) instead of answering not found over it, and forced it
// removes the pool's running NodeClaims whose NodePool is gone or is the
// KarpenterMachinePool controller's — never one a hand-made NodePool of the
// same name launched.

var (
	// karpenterNodePoolGVR is Karpenter's NodePool on the workload cluster,
	// named after the pool's release.
	karpenterNodePoolGVR = schema.GroupVersionResource{Group: "karpenter.sh", Version: "v1", Resource: "nodepools"}
	// ec2NodeClassGVR is Karpenter's AWS node class on the workload cluster,
	// named after the pool's release.
	ec2NodeClassGVR = schema.GroupVersionResource{Group: "karpenter.k8s.aws", Version: "v1", Resource: "ec2nodeclasses"}
)

// kmpControllerManagedBy is the managed-by label value the
// KarpenterMachinePool controller puts on the NodePool and EC2NodeClass it
// creates on the workload cluster for a pool.
const kmpControllerManagedBy = "aws-resolver-rules-operator"

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
	out.Warnings = append(out.Warnings, claimsUnreadable(pool, t.Cluster, why))
	return nil
}

// claimsUnreadable says that a pool's NodeClaims cannot be read as the
// caller, and what follows.
func claimsUnreadable(pool, cluster, why string) string {
	return fmt.Sprintf("the NodeClaims of pool %s on %s cannot be read as you (%s): they are not removed by this call, and one Karpenter does not disrupt keeps its instance running — re-run delete_node_pool with force once they can be read", pool, cluster, why)
}

// sweepClaims is a forced delete's last step: the pool's NodeClaims still
// running — one Karpenter launched for a Pending predictor while the
// teardown ran — are removed too, but for the ones whose delete this call
// was refused already (named once). While the pool's NodePool still stands —
// it goes once Flux has uninstalled the release, after the call — Karpenter
// may launch a node for the pods the drain evicts: the answer says so and
// names the forced re-run that removes it. Nothing is read on a dry run or a
// cut teardown, whose pending objects the re-run writes first.
func sweepClaims(td *teardown, t target, pool string) error {
	if td.dryRun || td.cut || t.Reader == nil {
		return nil
	}
	live := readPoolLive(td.ctx, t.Reader, pool, false)
	if live.claimsErr != nil {
		td.out.Warnings = append(td.out.Warnings, claimsUnreadable(pool, t.Cluster, live.claimsErr.Error()))
		return nil
	}
	var left []*poolNode
	for _, n := range live.running() {
		if !td.refused[n.claim.GetName()] {
			left = append(left, n)
		}
	}
	if err := td.deleteNodeClaims(t.Reader, left, true); err != nil {
		return err
	}
	nodePool, err := getOrNone(td.ctx, t, karpenterNodePoolGVR, pool)
	switch {
	case err != nil:
		td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("whether Karpenter NodePool %s still stands on %s cannot be read as you (%v): while it does, Karpenter may launch a node for the pods the drain evicts — re-run delete_node_pool with force once the pool's release is gone, it removes the pool's NodeClaims left", pool, t.Cluster, err))
	case nodePool != nil:
		td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("Karpenter NodePool %s still stands on %s: it goes once Flux has uninstalled the pool's release, and until then Karpenter may launch a node for the pods the drain evicts — re-run delete_node_pool with force once the release is gone, it removes the pool's NodeClaims left", pool, t.Cluster))
	}
	return nil
}

// poolLeftovers is what of a removed pool is left on its workload cluster:
// its Karpenter NodePool and EC2NodeClass, and its NodeClaims with their
// nodes and what holds them; each read that failed with why.
type poolLeftovers struct {
	pool                      string
	nodePool                  *unstructured.Unstructured
	nodeClass                 *unstructured.Unstructured
	nodePoolErr, nodeClassErr error
	live                      *poolLive
}

func readPoolLeftovers(ctx context.Context, t target, pool string) *poolLeftovers {
	l := &poolLeftovers{pool: pool, live: readPoolLive(ctx, t.Reader, pool, true)}
	l.nodePool, l.nodePoolErr = getOrNone(ctx, t, karpenterNodePoolGVR, pool)
	l.nodeClass, l.nodeClassErr = getOrNone(ctx, t, ec2NodeClassGVR, pool)
	return l
}

// getOrNone reads a cluster-scoped object on the target; nil when it is
// absent or its API not served.
func getOrNone(ctx context.Context, t target, gvr schema.GroupVersionResource, name string) (*unstructured.Unstructured, error) {
	obj, err := t.Reader.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil, nil
	}
	return obj, err
}

// empty: nothing of the pool is left, as far as the caller can read.
func (l *poolLeftovers) empty() bool {
	return l.nodePool == nil && l.nodeClass == nil && len(l.live.claims()) == 0 &&
		l.live.claimsErr == nil && l.nodePoolErr == nil && l.nodeClassErr == nil
}

// orphaned: the pool's NodeClaims are nobody else's to keep — their NodePool
// is gone, or it is the KarpenterMachinePool controller's, going with the
// pool's KarpenterMachinePool. A hand-made NodePool of the pool's name, or
// one that cannot be read, keeps its NodeClaims.
func (l *poolLeftovers) orphaned() bool {
	if l.nodePoolErr != nil {
		return false
	}
	return l.nodePool == nil || l.nodePool.GetLabels()[compose.LabelManagedBy] == kmpControllerManagedBy
}

// resolve answers what of the pool is left, each object with where it
// stands: one being deleted as terminating, one that stands with what it
// costs and the command that removes it, one that cannot be read with why.
// Forced, the pool's running NodeClaims are removed when orphaned; nothing
// else is touched — with the pool's release gone nothing proves the NodePool
// and EC2NodeClass cluster-manager's.
func (l *poolLeftovers) resolve(td *teardown, t target, force bool) error {
	l.nameObject(td, t, "NodePool", l.nodePool, l.nodePoolErr, "Karpenter removes it once its NodeClaims are gone", "Karpenter may still launch a node for it")
	if err := l.claims(td, t, force); err != nil {
		return err
	}
	l.nameObject(td, t, "EC2NodeClass", l.nodeClass, l.nodeClassErr, "Karpenter removes it once the pool's NodeClaims are gone", "nothing removes it")
	return nil
}

// claims answers the pool's NodeClaims left, and removes the running ones
// when forced and orphaned.
func (l *poolLeftovers) claims(td *teardown, t target, force bool) error {
	if l.live.claimsErr != nil {
		td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("the NodeClaims of the removed pool %s on %s cannot be read as you (%v): whether one still runs, its instance billed for a pool nothing manages, cannot be told — re-run once they can be read", l.pool, t.Cluster, l.live.claimsErr))
		return nil
	}
	remove := force && l.orphaned()
	var running []*poolNode
	var states []string
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
		if remove {
			running = append(running, n)
			states = append(states, state)
			continue
		}
		why := "re-run delete_node_pool with force to remove it"
		if !l.orphaned() {
			why = fmt.Sprintf("its NodePool %s stands and is not the KarpenterMachinePool controller's, so cluster-manager does not remove it", l.pool)
		}
		td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("NodeClaim %s (node %s) outlived pool %s on %s and still runs (%s): its instance is billed for a pool nothing manages — %s, or delete it once nothing needs the node (kubectl delete nodeclaim %s)", name, n.name(), l.pool, t.Cluster, state, why, name))
	}
	if err := td.deleteNodeClaims(t.Reader, running, true); err != nil {
		return err
	}
	for i, n := range running {
		if !td.refused[n.claim.GetName()] {
			td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("NodeClaim %s (node %s) outlived pool %s on %s and still ran (%s): %s", n.claim.GetName(), n.name(), l.pool, t.Cluster, states[i], td.outcome("removed, Karpenter drains the node and terminates its instance")))
		}
	}
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

// nameObject names one of the pool's Karpenter objects left on the cluster
// (nil and no error: none).
func (l *poolLeftovers) nameObject(td *teardown, t target, kind string, obj *unstructured.Unstructured, readErr error, terminating, standing string) {
	if readErr != nil {
		td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("whether %s %s of the removed pool is left on %s cannot be read as you (%v)", kind, l.pool, t.Cluster, readErr))
		return
	}
	if obj == nil {
		return
	}
	name := obj.GetName()
	if ts := obj.GetDeletionTimestamp(); ts != nil {
		td.out.Objects = append(td.out.Objects, ObjectAction{APIVersion: obj.GetAPIVersion(), Kind: kind, Name: name, Action: actionTerminating})
		td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("%s %s of the removed pool %s on %s is terminating since %s: %s", kind, name, l.pool, t.Cluster, ts.UTC().Format(time.RFC3339), terminating))
		return
	}
	td.out.Warnings = append(td.out.Warnings, fmt.Sprintf("%s %s outlived pool %s on %s: %s — delete it once its NodeClaims are gone (kubectl delete %s %s)", kind, name, l.pool, t.Cluster, standing, strings.ToLower(kind), name))
}
