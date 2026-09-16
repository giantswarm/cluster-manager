package api

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/cluster-manager/internal/tools"
)

func TestGetInfoReportsClusterAPI(t *testing.T) {
	for _, served := range []bool{true, false} {
		var info Info
		text, isErr := callTool(t, newServerWithClusterAPI(t, served), ToolGetInfo, nil)
		require.False(t, isErr, text)
		require.NoError(t, json.Unmarshal([]byte(text), &info))
		assert.Equal(t, "test", info.Version)
		assert.Equal(t, "cluster.x-k8s.io", info.ClusterAPI.Group)
		if served {
			assert.Equal(t, tools.ClusterAPIServed, info.ClusterAPI.State)
			assert.Empty(t, info.ClusterAPI.Note)
		} else {
			assert.Equal(t, tools.ClusterAPIAbsent, info.ClusterAPI.State)
			assert.Contains(t, info.ClusterAPI.Note, "cluster.x-k8s.io")
		}
	}
}

func TestToolsWithoutClusterAPI(t *testing.T) {
	srv := newServerWithClusterAPI(t, false)

	var answer tools.ClusterList
	text, isErr := callTool(t, srv, ToolListClusters, nil)
	require.False(t, isErr, text)
	require.NoError(t, json.Unmarshal([]byte(text), &answer))
	assert.Equal(t, []tools.Cluster{}, answer.Clusters)
	assert.Equal(t, tools.ClusterAPIAbsent, answer.ClusterAPI.State)

	for _, name := range []string{ToolListNodePools, ToolCreateNodePool, ToolDeleteNodePool} {
		text, isErr := callTool(t, srv, name, map[string]any{"cluster": "nosuchcluster", "name": "gpu00", "dryRun": true})
		require.True(t, isErr, name)
		assert.Equal(t, "cluster nosuchcluster not found: the Cluster API (cluster.x-k8s.io) is not served on this installation", text, name)
	}
}
