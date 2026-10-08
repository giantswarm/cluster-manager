package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/cluster-manager/internal/compose"
)

// sharedBackendFor seeds the shared `kserve` document a cluster-manager from
// before one backend per cluster registered for cluster.
func sharedBackendFor(t *testing.T, l *lab, cluster string) {
	t.Helper()
	cm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{
			"name": "model-backend-kserve", "namespace": "agent-platform",
			"labels": map[string]any{compose.LabelBackend: "true", compose.LabelManagedBy: compose.ManagedBy, compose.LabelCluster: cluster},
		},
		"data": map[string]any{compose.BackendDocumentKey: "kind: kserve\n"},
	}}
	_, err := l.installation.Resource(compose.ConfigMapGVR).Namespace("agent-platform").Create(context.Background(), cm, metav1.CreateOptions{})
	require.NoError(t, err)
}

// backendDocuments names the backend ConfigMaps in model-manager's namespace
// by the cluster each is registered for.
func backendDocuments(t *testing.T, l *lab) map[string]string {
	t.Helper()
	list, err := l.installation.Resource(compose.ConfigMapGVR).Namespace("agent-platform").List(context.Background(), metav1.ListOptions{LabelSelector: compose.LabelBackend + "=true"})
	require.NoError(t, err)
	out := map[string]string{}
	for i := range list.Items {
		out[list.Items[i].GetName()] = list.Items[i].GetLabels()[compose.LabelCluster]
	}
	return out
}

// TestBackendPerCluster: pools on two workload clusters of one installation
// coexist, each registering its own backend, `kserve-<cluster>`; list_node_pools
// and list_clusters name each cluster's; deleting one cluster's last pool
// removes that cluster's backend only (giantswarm/cluster-manager#185).
func TestBackendPerCluster(t *testing.T) {
	l := newLab(t, "installation.yaml")
	svc := l.service(Config{Installation: "gazelle"})
	ctx := context.Background()

	wc1, err := svc.CreateNodePool(ctx, l4("wc1", "gpu-l4", false))
	require.NoError(t, err)
	assert.Equal(t, "kserve-wc1", wc1.Backend.Backend)
	wc2, err := svc.CreateNodePool(ctx, l4("wc2", "gpu-l4b", false))
	require.NoError(t, err, "a second cluster's pool is not refused for the first cluster's backend")
	assert.Equal(t, "kserve-wc2", wc2.Backend.Backend)
	assert.Equal(t, "model-backend-kserve-wc2", wc2.Backend.Name)
	assert.Equal(t, map[string]string{"model-backend-kserve-wc1": "wc1", "model-backend-kserve-wc2": "wc2"}, backendDocuments(t, l))

	cm, err := l.installation.Resource(compose.ConfigMapGVR).Namespace("agent-platform").Get(ctx, "model-backend-kserve-wc2", metav1.GetOptions{})
	require.NoError(t, err)
	doc, _, _ := unstructured.NestedString(cm.Object, "data", compose.BackendDocumentKey)
	assert.Contains(t, doc, "name: kserve-wc2")
	assert.Contains(t, doc, "cluster: wc2")
	assert.Contains(t, doc, "gpuPool:", "the cluster's one pool, its sizes")
	assert.NotContains(t, doc, "wc1", "and nothing of another cluster's")

	for cluster, backend := range map[string]string{"wc1": "kserve-wc1", "wc2": "kserve-wc2"} {
		pools, err := svc.ListNodePools(ctx, cluster, "")
		require.NoError(t, err)
		assert.Equal(t, backend, pools.Backend.Backend, "list_node_pools names %s's backend", cluster)
		assert.Equal(t, backend, clusterNamed(t, l, cluster).Serving.Readiness.Backend.Backend, "list_clusters names %s's backend", cluster)
	}

	// wc1 carries the fixture's gpu-a10g beside gpu-l4: both go, the last
	// with wc1's backend.
	_, err = svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-l4", Mode: ModeApply})
	require.NoError(t, err)
	last, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply, Force: true})
	require.NoError(t, err)
	assert.True(t, last.LastPool)
	assert.Equal(t, map[string]string{"model-backend-kserve-wc2": "wc2"}, backendDocuments(t, l), "wc2's backend stays registered")
}

// TestSharedBackendRetired: a workload cluster registered under the shared
// `kserve` name by a cluster-manager from before one backend per cluster
// moves to its own name on the next write, the shared document removed —
// model-manager never holds two backends for one cluster — and is found by
// the cluster's last pool's deletion until then.
func TestSharedBackendRetired(t *testing.T) {
	ctx := context.Background()

	t.Run("create_node_pool moves it", func(t *testing.T) {
		l := newLab(t, "installation.yaml")
		sharedBackendFor(t, l, "wc1")
		out, err := l.service(Config{Installation: "gazelle"}).CreateNodePool(ctx, l4("wc1", "gpu-l4", false))
		require.NoError(t, err)
		assert.Equal(t, "kserve-wc1", out.Backend.Backend)
		assert.Contains(t, out.Objects, ObjectAction{APIVersion: "v1", Kind: "ConfigMap", Name: "model-backend-kserve", Namespace: "agent-platform", Action: "delete"})
		assert.Equal(t, map[string]string{wc1Backend: "wc1"}, backendDocuments(t, l))
	})

	t.Run("a dry run says so and writes nothing", func(t *testing.T) {
		l := newLab(t, "installation.yaml")
		sharedBackendFor(t, l, "wc1")
		out, err := l.service(Config{Installation: "gazelle"}).CreateNodePool(ctx, l4("wc1", "gpu-l4", true))
		require.NoError(t, err)
		assert.Contains(t, out.Objects, ObjectAction{APIVersion: "v1", Kind: "ConfigMap", Name: "model-backend-kserve", Namespace: "agent-platform", Action: "would-delete"})
		assert.Equal(t, map[string]string{"model-backend-kserve": "wc1"}, backendDocuments(t, l))
	})

	t.Run("another cluster's shared document stays", func(t *testing.T) {
		l := newLab(t, "installation.yaml")
		sharedBackendFor(t, l, "wc2")
		_, err := l.service(Config{Installation: "gazelle"}).CreateNodePool(ctx, l4("wc1", "gpu-l4", false))
		require.NoError(t, err)
		assert.Equal(t, map[string]string{wc1Backend: "wc1", "model-backend-kserve": "wc2"}, backendDocuments(t, l))
	})

	t.Run("the last pool's deletion removes it", func(t *testing.T) {
		l := newLab(t, "installation.yaml")
		sharedBackendFor(t, l, "wc1")
		out, err := l.service(Config{Installation: "gazelle"}).DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply, Force: true, DryRun: true})
		require.NoError(t, err)
		assert.Contains(t, out.Objects, ObjectAction{APIVersion: "v1", Kind: "ConfigMap", Name: "model-backend-kserve", Namespace: "agent-platform", Action: "would-delete"})
	})

	t.Run("listed under the shared name until moved", func(t *testing.T) {
		l := newLab(t, "installation.yaml")
		sharedBackendFor(t, l, "wc1")
		assert.Equal(t, "kserve", clusterNamed(t, l, "wc1").Serving.Readiness.Backend.Backend)
	})

	t.Run("the own cluster is refused while a workload cluster holds the shared name", func(t *testing.T) {
		l := newLab(t, "installation.yaml")
		l.add(t, l.installation, "prewarm.yaml") // the own cluster's values and join token
		sharedBackendFor(t, l, "wc1")
		in := l4("gazelle", "gpu-l40s", false)
		in.Pool.Accelerator, in.Pool.Sizes, in.Pool.MaxGPUs = "nvidia-l40s", []string{"2xlarge"}, 1
		_, err := l.service(Config{Installation: "gazelle"}).CreateNodePool(ctx, in)
		assertRefused(t, err, "model-manager's backend kserve (ConfigMap agent-platform/model-backend-kserve) is registered for cluster wc1")
		assertRefused(t, err, "re-run create_node_pool for one of wc1's pools, which registers its backend as kserve-wc1 and frees the name")
		assertNothingLanded(t, l, "gazelle-gpu-l40s")
	})
}
