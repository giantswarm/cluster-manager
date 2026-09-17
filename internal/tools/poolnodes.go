package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/detect"
)

// A Karpenter pool's nodes as the cluster shows them, read as the caller
// (giantswarm/cluster-manager#49): Karpenter's NodeClaims of the pool, the
// Nodes registered from them, and the pods on each. delete_node_pool's guard
// judges from these — the MachinePool's providerIDList it used to read lags
// the instances by minutes on both ends: a node Karpenter had terminated
// stayed in it for eight minutes and kept the delete refused — and removes
// the idle NodeClaims itself; list_node_pools names the idle nodes.

// What KServe puts on a predictor's pods: the InferenceService's label on its
// predictor, the LLMInferenceService's part-of on its workload.
const (
	labelKServeInferenceService = "serving.kserve.io/inferenceservice"
	labelPartOf                 = "app.kubernetes.io/part-of"
	partOfLLMISVC               = "llminferenceservice"
	// kindDaemonSet owns the pods that run on every node of the pool by
	// design: the device plugin, GPU feature discovery, the fleet's agents.
	kindDaemonSet = "DaemonSet"
)

// poolNode is one node of the pool: its NodeClaim, its Node (nil while the
// claim is launching), and what holds it.
type poolNode struct {
	claim *unstructured.Unstructured
	node  *unstructured.Unstructured
	// holders are the pods that keep the node busy, each with what holds
	// it: `model-serving/qwen3-8b-fp8-kserve-6649fb66c8-dllt7 (1 GPU)`.
	holders []string
	// holdersErr says why the pods on the node could not be read: whether
	// the node is idle cannot be told then.
	holdersErr error
}

// name is the node's name, else its NodeClaim's while it is launching.
func (n *poolNode) name() string {
	if n.node != nil {
		return n.node.GetName()
	}
	return n.claim.GetName()
}

// terminating: the NodeClaim is deleted — Karpenter drains the node and
// terminates its instance.
func (n *poolNode) terminating() bool {
	return n.claim != nil && n.claim.GetDeletionTimestamp() != nil
}

// idleSince is when the node's last pod left: the NodeClaim's
// status.lastPodEventTime (Karpenter records every pod scheduled on or
// removed from the node), else the claim's Ready transition, else the Node's
// creation.
func (n *poolNode) idleSince() string {
	if n.claim != nil {
		if t := nestedString(n.claim, "status", "lastPodEventTime"); t != "" {
			return t
		}
		if cond, found := detect.ReadyCondition(n.claim); found && cond.LastTransitionTime != "" {
			return cond.LastTransitionTime
		}
	}
	if n.node != nil {
		return detect.Timestamp(n.node.GetCreationTimestamp().Time)
	}
	return ""
}

// poolLive is what the cluster shows of a pool's nodes, by name.
type poolLive struct {
	nodes []*poolNode
	// claimsErr and nodesErr say why the NodeClaims or the Nodes could not
	// be listed; nil when they were (an API the cluster does not serve
	// counts as none of that kind).
	claimsErr, nodesErr error
}

// readable: at least one of the two lists was read. With neither, the
// MachinePool's provider IDs are all there is to judge from.
func (l *poolLive) readable() bool { return l.claimsErr == nil || l.nodesErr == nil }

// claims are the pool's NodeClaims — launching, ready or terminating.
func (l *poolLive) claims() []*unstructured.Unstructured {
	out := make([]*unstructured.Unstructured, 0, len(l.nodes))
	for _, n := range l.nodes {
		if n.claim != nil {
			out = append(out, n.claim)
		}
	}
	return out
}

// idle are the pool's idle nodes: registered, held by nothing, not
// terminating, their NodeClaim there to remove.
func (l *poolLive) idle() []*poolNode {
	var out []*poolNode
	for _, n := range l.nodes {
		if l.holds(n) == "" && !n.terminating() {
			out = append(out, n)
		}
	}
	return out
}

// holds says what keeps a node from going with the pool; "" when the node is
// idle. A terminating node is Karpenter's already and holds nothing.
func (l *poolLive) holds(n *poolNode) string {
	switch {
	case n.terminating():
		return ""
	case n.node == nil && l.nodesErr != nil:
		return fmt.Sprintf("the node of NodeClaim %s cannot be read as you (%v)", n.claim.GetName(), l.nodesErr)
	case n.node == nil:
		return fmt.Sprintf("NodeClaim %s is launching, no node registered yet — a predictor asked for it", n.claim.GetName())
	case n.holdersErr != nil:
		return fmt.Sprintf("whether node %s is idle cannot be told (%v)", n.name(), n.holdersErr)
	case len(n.holders) > 0:
		return fmt.Sprintf("node %s runs %s", n.name(), strings.Join(n.holders, ", "))
	case n.claim == nil && l.claimsErr != nil:
		return fmt.Sprintf("node %s is idle but its NodeClaim cannot be read as you (%v)", n.name(), l.claimsErr)
	case n.claim == nil:
		return fmt.Sprintf("node %s is idle but carries no NodeClaim of the pool", n.name())
	}
	return ""
}

// readPoolLive lists the pool's NodeClaims and Nodes concurrently, pairs
// them (the claim's status.nodeName, else its providerID against the Node's
// spec.providerID), and reads the pods on every registered node.
func readPoolLive(ctx context.Context, reader dynamic.Interface, pool string) *poolLive {
	defer timed(ctx, "read pool nodes", "pool", pool)()
	l := &poolLive{}
	var claims, nodes *unstructured.UnstructuredList
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		claims, l.claimsErr = listOrNone(gctx, reader, detect.NodeClaimGVR, detect.LabelKarpenterNodePool+"="+pool)
		return nil
	})
	g.Go(func() error {
		nodes, l.nodesErr = listOrNone(gctx, reader, detect.NodesGVR, compose.LabelMachinePool+"="+pool)
		return nil
	})
	_ = g.Wait()
	byNode, byProvider := map[string]*poolNode{}, map[string]*poolNode{}
	if nodes != nil {
		for i := range nodes.Items {
			node := &nodes.Items[i]
			n := &poolNode{node: node}
			byNode[node.GetName()] = n
			if id := nestedString(node, "spec", "providerID"); id != "" {
				byProvider[id] = n
			}
			l.nodes = append(l.nodes, n)
		}
	}
	if claims != nil {
		for i := range claims.Items {
			claim := &claims.Items[i]
			n := byNode[nestedString(claim, "status", "nodeName")]
			if n == nil {
				n = byProvider[nestedString(claim, "status", "providerID")]
			}
			if n == nil {
				l.nodes = append(l.nodes, &poolNode{claim: claim})
				continue
			}
			n.claim = claim
		}
	}
	sort.Slice(l.nodes, func(i, j int) bool { return l.nodes[i].name() < l.nodes[j].name() })
	g, gctx = errgroup.WithContext(ctx)
	for _, n := range l.nodes {
		if n.node == nil {
			continue
		}
		g.Go(func() error {
			n.holders, n.holdersErr = holders(gctx, reader, n.node.GetName())
			return nil
		})
	}
	_ = g.Wait()
	return l
}

// listOrNone lists a cluster-scoped resource by label; an API the cluster
// does not serve is an empty list.
func listOrNone(ctx context.Context, reader dynamic.Interface, gvr schema.GroupVersionResource, selector string) (*unstructured.UnstructuredList, error) {
	items, err := reader.Resource(gvr).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return &unstructured.UnstructuredList{}, nil
		}
		return nil, fmt.Errorf("list %s: %w", gvr.GroupResource(), err)
	}
	return items, nil
}

// holders names the pods that hold a node: every pod on it that is not
// finished, not a DaemonSet's (those run on every node of the pool by
// design), not preemptible by design (a negative priority: the pool's
// prewarm placeholder, which the first predictor evicts), and that requests
// a GPU or is a KServe predictor's. Read as the caller across namespaces — a
// GPU job outside the serving namespace holds the node as much as a
// predictor does.
func holders(ctx context.Context, reader dynamic.Interface, node string) ([]string, error) {
	pods, err := reader.Resource(detect.PodsGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + node})
	if err != nil {
		return nil, fmt.Errorf("list the pods on node %s: %w", node, err)
	}
	out := []string{}
	for i := range pods.Items {
		if h, ok := holder(&pods.Items[i], node); ok {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out, nil
}

// holder is the pod's name with what holds the node, when it does.
func holder(pod *unstructured.Unstructured, node string) (string, bool) {
	if nestedString(pod, "spec", "nodeName") != node {
		return "", false
	}
	switch nestedString(pod, "status", "phase") {
	case "Succeeded", "Failed":
		return "", false
	}
	for _, owner := range pod.GetOwnerReferences() {
		if owner.Kind == kindDaemonSet {
			return "", false
		}
	}
	if v, found, _ := unstructured.NestedFieldNoCopy(pod.Object, "spec", "priority"); found {
		if priority, ok := number(v); ok && priority < 0 {
			return "", false
		}
	}
	name := pod.GetNamespace() + "/" + pod.GetName()
	if gpus := gpuRequest(pod); gpus > 0 {
		unit := "GPU"
		if gpus > 1 {
			unit = "GPUs"
		}
		return fmt.Sprintf("%s (%d %s)", name, gpus, unit), true
	}
	labels := pod.GetLabels()
	if labels[labelKServeInferenceService] != "" || labels[labelPartOf] == partOfLLMISVC {
		return name + " (KServe predictor)", true
	}
	return "", false
}

// gpuRequest is the GPUs the pod's containers ask for — limits, else
// requests: an extended resource's two are equal —, the init containers'
// when the containers ask for none (a pod is placed for the larger of the
// two).
func gpuRequest(pod *unstructured.Unstructured) int64 {
	for _, field := range []string{"containers", "initContainers"} {
		var total int64
		containers, _, _ := unstructured.NestedSlice(pod.Object, "spec", field)
		for _, c := range containers {
			if m, ok := c.(map[string]any); ok {
				total += containerGPUs(m)
			}
		}
		if total > 0 {
			return total
		}
	}
	return 0
}

// containerGPUs is one container's GPU limit, else request.
func containerGPUs(c map[string]any) int64 {
	for _, kind := range []string{"limits", "requests"} {
		v, found, _ := unstructured.NestedFieldNoCopy(c, "resources", kind, detect.GPUResource)
		if !found {
			continue
		}
		switch q := v.(type) {
		case int64:
			return q
		case float64:
			return int64(q)
		case string:
			if parsed, err := resource.ParseQuantity(q); err == nil {
				return parsed.Value()
			}
		}
	}
	return 0
}

// nodeNames lists nodes by name.
func nodeNames(nodes []*poolNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.name())
	}
	return out
}

// earliestIdle is the earliest idleSince among nodes (lexical order holds
// for RFC3339 in UTC).
func earliestIdle(nodes []*poolNode) string {
	var earliest string
	for _, n := range nodes {
		if since := n.idleSince(); since != "" && (earliest == "" || since < earliest) {
			earliest = since
		}
	}
	return earliest
}
