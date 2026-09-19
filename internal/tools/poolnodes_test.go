package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/cluster-manager/internal/detect"
)

// TestHolder (giantswarm/cluster-manager#49): what holds a GPU pool's node —
// a pod on it that requests a GPU or is a KServe predictor's — and what does
// not: a pod on another node, a finished one, a DaemonSet's (on every node of
// the pool by design), one preemptible by design (the pool's prewarm
// placeholder, a negative priority), a pod without a GPU that is nobody's
// predictor. GPU counts come from limits or requests, as a quantity or a
// number, the init containers' when the containers ask for none.
func TestHolder(t *testing.T) {
	pod := func(ns, name string, spec, status map[string]any, labels map[string]any, owners []any) *unstructured.Unstructured {
		meta := map[string]any{"name": name, "namespace": ns}
		if labels != nil {
			meta["labels"] = labels
		}
		if owners != nil {
			meta["ownerReferences"] = owners
		}
		spec["nodeName"] = "node-1"
		return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": meta, "spec": spec, "status": status}}
	}
	gpu := func(n any) map[string]any {
		return map[string]any{"containers": []any{map[string]any{"name": "main", "resources": map[string]any{"limits": map[string]any{"nvidia.com/gpu": n}}}}}
	}
	running := map[string]any{"phase": "Running"}

	for _, tc := range []struct {
		name string
		pod  *unstructured.Unstructured
		node string
		want string
		held bool
	}{
		{"predictor with one GPU", pod("model-serving", "qwen3-8b-fp8-kserve-1", gpu(int64(1)), running, nil, nil), "node-1", "model-serving/qwen3-8b-fp8-kserve-1 (1 GPU)", true},
		{"two GPUs as a quantity", pod("model-serving", "big", gpu("2"), running, nil, nil), "node-1", "model-serving/big (2 GPUs)", true},
		{"GPU requested, no limit", pod("jobs", "train", map[string]any{"containers": []any{map[string]any{"name": "t", "resources": map[string]any{"requests": map[string]any{"nvidia.com/gpu": int64(1)}}}}}, running, nil, nil), "node-1", "jobs/train (1 GPU)", true},
		{"init container's GPU", pod("jobs", "init", map[string]any{"containers": []any{map[string]any{"name": "m"}}, "initContainers": []any{map[string]any{"name": "i", "resources": map[string]any{"limits": map[string]any{"nvidia.com/gpu": int64(1)}}}}}, running, nil, nil), "node-1", "jobs/init (1 GPU)", true},
		{"pending predictor", pod("model-serving", "p", gpu(int64(1)), map[string]any{"phase": "Pending"}, nil, nil), "node-1", "model-serving/p (1 GPU)", true},
		{"the classic path's predictor without a GPU limit: nobody's", pod("model-serving", "isvc", map[string]any{"containers": []any{map[string]any{"name": "kserve-container"}}}, running, map[string]any{"serving.kserve.io/inferenceservice": "mistral-7b"}, nil), "node-1", "", false},
		{"LLMInferenceService's workload without a GPU limit", pod("model-serving", "llm", map[string]any{"containers": []any{map[string]any{"name": "main"}}}, running, map[string]any{detect.LabelPartOf: detect.PartOfLLMISVC}, nil), "node-1", "model-serving/llm (KServe predictor)", true},
		{"on another node", pod("model-serving", "elsewhere", gpu(int64(1)), running, nil, nil), "node-2", "", false},
		{"finished", pod("gpu-operator", "nvidia-cuda-validator", gpu(int64(1)), map[string]any{"phase": "Succeeded"}, nil, nil), "node-1", "", false},
		{"failed", pod("jobs", "crashed", gpu(int64(1)), map[string]any{"phase": "Failed"}, nil, nil), "node-1", "", false},
		{"a DaemonSet's", pod("gpu-operator", "nvidia-device-plugin", gpu(int64(1)), running, nil, []any{map[string]any{"apiVersion": "apps/v1", "kind": kindDaemonSet, "name": "nvidia-device-plugin-daemonset", "uid": "1"}}), "node-1", "", false},
		{"preemptible by design", pod("org-acme", "prewarm", map[string]any{"priority": int64(-1000), "containers": gpu(int64(1))["containers"]}, running, nil, nil), "node-1", "", false},
		{"no GPU, nobody's predictor", pod("model-serving", "mm-scan-1", map[string]any{"containers": []any{map[string]any{"name": "scan"}}}, running, nil, nil), "node-1", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, held := holder(tc.pod, tc.node)
			assert.Equal(t, tc.held, held)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestPoolNodeIdleSince: the NodeClaim's lastPodEventTime — when Karpenter
// last saw a pod scheduled on or removed from the node — else its Ready
// transition, else the Node's creation.
func TestPoolNodeIdleSince(t *testing.T) {
	claim := func(status map[string]any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "karpenter.sh/v1", "kind": "NodeClaim", "metadata": map[string]any{"name": "c"}, "status": status}}
	}
	node := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Node", "metadata": map[string]any{"name": "n", "creationTimestamp": "2026-09-17T22:44:11Z"}}}
	ready := []any{map[string]any{"type": "Ready", "status": "True", "lastTransitionTime": "2026-09-17T22:45:02Z"}}

	assert.Equal(t, "2026-09-17T22:55:41Z", (&poolNode{claim: claim(map[string]any{"lastPodEventTime": "2026-09-17T22:55:41Z", "conditions": ready}), node: node}).idleSince())
	assert.Equal(t, "2026-09-17T22:45:02Z", (&poolNode{claim: claim(map[string]any{"conditions": ready}), node: node}).idleSince())
	assert.Equal(t, "2026-09-17T22:44:11Z", (&poolNode{claim: claim(map[string]any{}), node: node}).idleSince())
	assert.Equal(t, "2026-09-17T22:44:11Z", (&poolNode{node: node}).idleSince())
}
