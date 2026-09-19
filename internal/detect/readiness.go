package detect

import (
	"context"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// Resources the readiness reads on the target, beside the detection's.
var (
	DaemonSetGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}
	// NodeClaimGVR is Karpenter's NodeClaim: one per node it launches,
	// labelled with its NodePool (LabelKarpenterNodePool).
	NodeClaimGVR = schema.GroupVersionResource{Group: "karpenter.sh", Version: "v1", Resource: "nodeclaims"}
	// EventsGVR is the core events: Karpenter's refusal to launch a
	// NodeClaim it deleted at once lives on as a Warning event on the claim
	// in the default namespace (giantswarm/cluster-manager#55).
	EventsGVR = schema.GroupVersionResource{Version: "v1", Resource: "events"}
	// GatewayGVR is the Gateway API's Gateway — the slice's models Gateway.
	GatewayGVR = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"}
)

const (
	// LabelKarpenterNodePool is Karpenter's label on a NodeClaim naming the
	// NodePool it belongs to; a Karpenter pool's NodePool carries the
	// KarpenterMachinePool's name.
	LabelKarpenterNodePool = "karpenter.sh/nodepool"
	// labelApp is the operator's app label on its operand DaemonSets.
	labelApp = "app"
	// ConditionProgrammed is the Gateway API's condition saying a Gateway's
	// data plane is set up.
	ConditionProgrammed = "Programmed"
	// modelsHostPrefix is the host the slice's models Gateway listens on.
	modelsHostPrefix = "models."
)

// operandApps are the operator's operands whose DaemonSets the readiness
// counts: the device plugin and GPU feature discovery, which run on the
// cluster's GPU nodes only — 0/0 at scale-to-zero.
var operandApps = []string{"nvidia-device-plugin-daemonset", "gpu-feature-discovery"}

// ReleaseState is a HelmRelease's readiness, from its Ready condition.
type ReleaseState struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Ready mirrors the Ready condition; null until the release reports one.
	Ready   *bool  `json:"ready"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	// Since is the Ready condition's lastTransitionTime (RFC3339).
	Since string `json:"since,omitempty"`
	// DeletedAt is the release's deletionTimestamp (RFC3339) — Flux is
	// uninstalling it, or its uninstall failed and Flux retries.
	DeletedAt string `json:"deletedAt,omitempty"`
}

// OperatorReadiness is the GPU operator's readiness on a cluster.
type OperatorReadiness struct {
	// Release is the operator's HelmRelease (the provider's), null when a
	// hand install left none.
	Release *ReleaseState `json:"release"`
	// ClusterPolicy is the operator's ClusterPolicy with its status.state
	// (ready, notReady, ignored), null when the cluster has none or cannot
	// be read.
	ClusterPolicy *ClusterPolicyState `json:"clusterPolicy"`
	// Operands are the device plugin and GPU feature discovery DaemonSets
	// with their scheduled and ready pods; empty while the operator has
	// created none (OperandsMessage says so) or when the cluster cannot be
	// read (OperandsError says so).
	Operands []OperandState `json:"operands"`
	// OperandsError is the read failure when the DaemonSets could not be
	// listed: Operands is then empty for that reason, not for want of GPU
	// nodes.
	OperandsError string `json:"operandsError,omitempty"`
	// OperandsMessage says why Operands is empty when the read succeeded
	// and a ClusterPolicy exists: the operator creates its operand
	// DaemonSets once a GPU node joins, so a pool at scale-to-zero has none
	// (OperandsAbsent).
	OperandsMessage string `json:"operandsMessage,omitempty"`
	// releaseProvider ranks Release while the releases are collected.
	releaseProvider Provider
}

// ClusterPolicyState is a ClusterPolicy's name and status.state.
type ClusterPolicyState struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// OperandState is one operand DaemonSet: desiredNumberScheduled and
// numberReady.
type OperandState struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Desired   int64  `json:"desired"`
	Ready     int64  `json:"ready"`
}

// ServingReadiness is the serving layer's readiness on a cluster.
type ServingReadiness struct {
	// Release is cluster-manager's slice release, null when the layer is
	// the platform's own or a hand install.
	Release *ReleaseState `json:"release"`
	// Children are the slice release's child HelmReleases (the KServe
	// charts, the connectivity child, kserve-runtime-configs), every one
	// with its Ready condition; empty without a slice release.
	Children []ReleaseState `json:"children"`
	// Controllers are the llm-d controller Deployments on the cluster.
	Controllers []ControllerState `json:"controllers"`
	// Configs counts the LLMInferenceServiceConfigs of the release
	// namespace; null when the cluster cannot be read.
	Configs *Count `json:"configs"`
	// Backend is model-manager's kserve backend registration for the
	// cluster (filled in by the tools, which know the namespace).
	Backend BackendState `json:"backend"`
	// Presets counts the serving presets published on the cluster; null
	// when the cluster cannot be read.
	Presets *Count `json:"presets"`
	// ModelsGateway is the Gateway listening on models.<domain>, null where
	// the slice runs none (or the cluster cannot be read).
	ModelsGateway *GatewayState `json:"modelsGateway"`
	// Cache is the model cache as cluster-manager's slice release states it
	// in its values (the chart's defaults where they name nothing): whether
	// the predictors mount a claim, and which one — the claim of the zone the
	// last pool with the cache on was created in (giantswarm/cluster-manager#71).
	// Null where the slice release is not cluster-manager's.
	Cache *SliceCache `json:"cache"`
	// CacheClaims are the model cache claims of the serving namespace — the
	// one claim of before and the claims per zone — each with its phase and
	// the zone its volume is bound to, where a pool mounting it lands (filled
	// in by the tools, which know the namespace and the base name); empty for
	// none, null when the cluster cannot be read.
	CacheClaims []*CacheClaim `json:"cacheClaims"`
}

// SliceCache is the model cache as the slice release's values state it.
type SliceCache struct {
	Enabled bool `json:"enabled"`
	// Claim names the claim the predictors mount (`modelServing.cache.pvc.name`,
	// the chart's default when the values name none); empty with the cache
	// off.
	Claim string `json:"claim,omitempty"`
}

// ControllerState is one KServe controller Deployment, the llm-d one.
type ControllerState struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Available int64  `json:"available"`
	Replicas  int64  `json:"replicas"`
}

// Count is a number of objects.
type Count struct {
	Count int `json:"count"`
}

// BackendState is the kserve backend document cluster-manager registers with
// model-manager, when it is registered for the cluster. Registered is
// tri-state: true or false when the document was read, omitted with Error
// set when it could not be read — never a bare false for a failed read.
type BackendState struct {
	Registered *bool  `json:"registered,omitempty"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name,omitempty"`
	// Error is the read failure; whether the document exists is unknown.
	Error string `json:"error,omitempty"`
}

// GatewayState is a Gateway's readiness: its Programmed condition.
type GatewayState struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Ready     *bool  `json:"ready"`
	Reason    string `json:"reason,omitempty"`
}

// NewReleaseState reads a HelmRelease's Ready condition and deletion.
func NewReleaseState(hr *unstructured.Unstructured) ReleaseState {
	r := ReleaseState{Name: hr.GetName(), Namespace: hr.GetNamespace()}
	if deleted := hr.GetDeletionTimestamp(); deleted != nil {
		r.DeletedAt = Timestamp(deleted.Time)
	}
	if cond, found := ReadyCondition(hr); found {
		ready := cond.Status == "True"
		r.Ready, r.Reason, r.Since = &ready, cond.Reason, cond.LastTransitionTime
		if !ready {
			r.Message = strings.Join(strings.Fields(cond.Message), " ")
		}
	}
	return r
}

// childEvidence names the children that are not Ready, the way the evidence
// always has: with the condition's reason and message, and the deletion
// where Flux's uninstall failed and retries (giantswarm/cluster-manager#37).
func childEvidence(children []ReleaseState) []string {
	var out []string
	for _, c := range children {
		if c.Ready != nil && *c.Ready {
			continue
		}
		detail := "no Ready condition yet"
		if c.Ready != nil {
			detail = "Ready=False"
			if c.Reason != "" {
				detail += " [" + c.Reason + "]"
			}
			if c.Message != "" {
				detail += " " + c.Message
			}
		}
		if c.DeletedAt != "" {
			detail = "deleted since " + c.DeletedAt + ", " + detail
		}
		out = append(out, "HelmRelease "+c.Namespace+"/"+c.Name+" not Ready ("+detail+")")
	}
	return out
}

// OperandsAbsent is OperatorReadiness.OperandsMessage while the operator has
// created no operand DaemonSet: gpu-operator skips them until a GPU node
// joins ("No GPU node in the cluster, do not create DaemonSets"), so a pool
// at scale-to-zero has none while its ClusterPolicy reads ready.
const OperandsAbsent = "no operand DaemonSet: the GPU operator creates the device plugin and GPU feature discovery DaemonSets once a GPU node joins (none at scale-to-zero)"

// operands lists the operator's operand DaemonSets on the target; the list
// is empty, never nil, with the error when the target cannot list them.
func operands(ctx context.Context, reader dynamic.Interface) ([]OperandState, error) {
	out := []OperandState{}
	dss, err := list(ctx, reader, DaemonSetGVR, metav1.NamespaceAll, labelApp+" in ("+strings.Join(operandApps, ",")+")")
	if err != nil {
		return out, err
	}
	for i := range dss.Items {
		ds := &dss.Items[i]
		out = append(out, OperandState{Name: ds.GetName(), Namespace: ds.GetNamespace(),
			Desired: NestedInt(ds, "status", "desiredNumberScheduled"), Ready: NestedInt(ds, "status", "numberReady")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// count counts the objects of a resource; nil when the target cannot list
// them.
func count(ctx context.Context, reader dynamic.Interface, gvr schema.GroupVersionResource, namespace, selector string) *Count {
	items, err := list(ctx, reader, gvr, namespace, selector)
	if err != nil {
		return nil
	}
	return &Count{Count: len(items.Items)}
}

// modelsGateway is the Gateway with a listener on models.<domain>, nil when
// the target runs none.
func modelsGateway(ctx context.Context, reader dynamic.Interface) *GatewayState {
	gws, err := list(ctx, reader, GatewayGVR, metav1.NamespaceAll, "")
	if err != nil {
		return nil
	}
	for i := range gws.Items {
		gw := &gws.Items[i]
		listeners, _, _ := unstructured.NestedSlice(gw.Object, "spec", "listeners")
		for _, l := range listeners {
			m, _ := l.(map[string]any)
			if host, _ := m["hostname"].(string); !strings.HasPrefix(host, modelsHostPrefix) {
				continue
			}
			state := &GatewayState{Name: gw.GetName(), Namespace: gw.GetNamespace()}
			if cond, found := condition(gw, ConditionProgrammed); found {
				ready := cond.Status == "True"
				state.Ready, state.Reason = &ready, cond.Reason
			}
			return state
		}
	}
	return nil
}

// ConditionOf reads one condition of an object's status.conditions by type;
// found is false when it has none of that type.
func ConditionOf(obj *unstructured.Unstructured, typ string) (Condition, bool) {
	return condition(obj, typ)
}

// condition reads one condition of status.conditions by type.
func condition(obj *unstructured.Unstructured, typ string) (Condition, bool) {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != typ {
			continue
		}
		cond := Condition{Type: typ}
		cond.Status, _ = m["status"].(string)
		cond.Reason, _ = m["reason"].(string)
		cond.Message, _ = m["message"].(string)
		cond.LastTransitionTime, _ = m["lastTransitionTime"].(string)
		return cond, true
	}
	return Condition{}, false
}

// Timestamp formats a time as the readiness reports it (RFC3339, UTC);
// "" for the zero time.
func Timestamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
