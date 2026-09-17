package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/detect"
	"github.com/giantswarm/cluster-manager/internal/tools"
)

type messageHandler interface {
	HandleMessage(ctx context.Context, message json.RawMessage) mcp.JSONRPCMessage
}

func rpc(t *testing.T, srv messageHandler, method string, params map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	require.NoError(t, err)
	out, err := json.Marshal(srv.HandleMessage(context.Background(), raw))
	require.NoError(t, err)
	var parsed struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(out, &parsed))
	require.Nil(t, parsed.Error, string(out))
	return parsed.Result
}

func callTool(t *testing.T, srv messageHandler, name string, args map[string]any) (string, bool) {
	t.Helper()
	result := rpc(t, srv, "tools/call", map[string]any{"name": name, "arguments": args})
	var parsed struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	require.NoError(t, json.Unmarshal(result, &parsed))
	require.NotEmpty(t, parsed.Content, string(result))
	return parsed.Content[0].Text, parsed.IsError
}

func cluster(name, namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cluster.x-k8s.io/v1beta1", "kind": "Cluster",
		"metadata": map[string]any{"name": name, "namespace": namespace, "labels": map[string]any{"release.giantswarm.io/version": "31.0.0"}},
	}}
}

func newServer(t *testing.T) *handlersServer {
	t.Helper()
	return newServerWithClusterAPI(t, true)
}

// newServerWithClusterAPI builds the server over two clusters; without the
// Cluster API the discovery serves nothing, as an installation without the
// KaaS components does.
func newServerWithClusterAPI(t *testing.T, clusterAPI bool) *handlersServer {
	t.Helper()
	disc := &discoveryfake.FakeDiscovery{Fake: &k8stesting.Fake{}}
	if clusterAPI {
		disc.Resources = []*metav1.APIResourceList{{GroupVersion: tools.ClusterGVR.GroupVersion().String(), APIResources: []metav1.APIResource{{Name: tools.ClusterGVR.Resource, Kind: "Cluster", Namespaced: true}}}}
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		tools.ClusterGVR: "ClusterList", tools.MachinePoolGVR: "MachinePoolList", tools.HelmReleaseGVR: "HelmReleaseList", tools.ReleaseGVR: "ReleaseList",
		tools.AppGVR: "AppList", tools.ConfigMapGVR: "ConfigMapList", compose.SecretGVR: "SecretList", compose.OCIRepositoryGVR: "OCIRepositoryList",
		detect.NodesGVR: "NodeList", detect.DeploymentGVR: "DeploymentList", detect.ClusterPolicyGVR: "ClusterPolicyList", detect.InferenceServiceGVR: "InferenceServiceList", detect.LLMISVCGVR: "LLMInferenceServiceList", detect.LLMISVCConfigResource.WithVersion("v1alpha2"): "LLMInferenceServiceConfigList",
	}, cluster("gazelle", "org-giantswarm"), cluster("wc1", "org-acme"))
	svc := tools.New(func(context.Context) tools.Clients { return tools.Clients{Dynamic: dyn, Discovery: disc} }, nil, tools.Config{Installation: "gazelle"})
	return &handlersServer{NewMCPServer(svc, "test")}
}

type handlersServer struct{ messageHandler }

func TestToolsListAndSchemas(t *testing.T) {
	srv := newServer(t)
	var listed struct {
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
			Annotations map[string]any `json:"annotations"`
		} `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(rpc(t, srv, "tools/list", nil), &listed))
	names := map[string]bool{}
	writes := map[string]bool{ToolCreateNodePool: false, ToolDeleteNodePool: true, ToolEnableModelServing: false, ToolDisableModelServing: true}
	for _, tool := range listed.Tools {
		names[tool.Name] = true
		destructive, isWrite := writes[tool.Name]
		if !isWrite {
			assert.Equal(t, true, tool.Annotations["readOnlyHint"], "%s is read-only", tool.Name)
			continue
		}
		assert.NotEqual(t, true, tool.Annotations["readOnlyHint"], "%s writes", tool.Name)
		assert.Equal(t, destructive, tool.Annotations["destructiveHint"], "%s destructive", tool.Name)
		assert.Equal(t, true, tool.Annotations["idempotentHint"], "%s is idempotent: the re-run is the update", tool.Name)
		props := tool.InputSchema["properties"].(map[string]any)
		assert.Equal(t, []any{"apply", "commit"}, props["mode"].(map[string]any)["enum"], "%s offers both modes in the schema", tool.Name)
		assert.Equal(t, "apply", props["mode"].(map[string]any)["default"])
	}
	assert.Len(t, listed.Tools, len(ToolNames()))
	for _, want := range ToolNames() {
		assert.True(t, names[want], "tool %s missing", want)
	}
}

func TestWriteToolsRefuseCommitMode(t *testing.T) {
	srv := newServer(t)
	for _, tool := range []string{ToolCreateNodePool, ToolDeleteNodePool} {
		text, isErr := callTool(t, srv, tool, map[string]any{"cluster": "wc1", "name": "gpu-l4", "mode": "commit"})
		assert.True(t, isErr, "%s: %s", tool, text)
		assert.Contains(t, text, "mode commit", tool)
		assert.Contains(t, text, "use mode apply", tool)
	}
}

func TestGetInfo(t *testing.T) {
	text, isErr := callTool(t, newServer(t), ToolGetInfo, nil)
	require.False(t, isErr, text)
	var info Info
	require.NoError(t, json.Unmarshal([]byte(text), &info))
	assert.Equal(t, "test", info.Version)
	assert.True(t, info.Modes.Apply)
	assert.False(t, info.Modes.Commit, "commit mode follows the epic's first proof")
	assert.Equal(t, ToolNames(), info.Tools)
}

func TestListClustersAndNodePools(t *testing.T) {
	srv := newServer(t)
	text, isErr := callTool(t, srv, ToolListClusters, nil)
	require.False(t, isErr, text)
	var clusters struct {
		Clusters []tools.Cluster `json:"clusters"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &clusters))
	require.Len(t, clusters.Clusters, 2)
	assert.True(t, clusters.Clusters[1].OwnCluster)

	text, isErr = callTool(t, srv, ToolListNodePools, map[string]any{"cluster": "wc1"})
	require.False(t, isErr, text)
	var pools tools.NodePools
	require.NoError(t, json.Unmarshal([]byte(text), &pools))
	assert.Equal(t, "org-acme", pools.Namespace)
	assert.Empty(t, pools.NodePools)

	text, isErr = callTool(t, srv, ToolListNodePools, map[string]any{"cluster": "nope"})
	require.True(t, isErr)
	assert.Contains(t, text, "cluster nope not found")
	assert.Contains(t, text, "list_clusters", "the error points at the tool that names the clusters")

	text, isErr = callTool(t, srv, ToolListNodePools, nil)
	require.True(t, isErr, "cluster is required")
	assert.Contains(t, text, "cluster")
}
