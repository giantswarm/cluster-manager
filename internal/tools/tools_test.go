package tools

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/yaml"
)

var update = flag.Bool("update", false, "rewrite the golden files from the current output")

// loadFixtures reads the multi-document YAML fixture into unstructured objects.
func loadFixtures(t *testing.T, name string) []runtime.Object {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // fixture named by the test
	require.NoError(t, err)
	var objs []runtime.Object
	for _, doc := range strings.Split(string(raw), "\n---") {
		if strings.TrimSpace(strings.ReplaceAll(doc, "#", "")) == "" {
			continue
		}
		j, err := yaml.YAMLToJSON([]byte(doc))
		require.NoError(t, err, doc)
		u := &unstructured.Unstructured{}
		require.NoError(t, json.Unmarshal(j, &u.Object), doc)
		objs = append(objs, u)
	}
	return objs
}

// newFakeClients is a fake dynamic client over the fixture, with the list
// kinds of every resource the tools list.
func newFakeClients(t *testing.T, fixture string) ClientsFor {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		ClusterGVR:     "ClusterList",
		MachinePoolGVR: "MachinePoolList",
		HelmReleaseGVR: "HelmReleaseList",
		ReleaseGVR:     "ReleaseList",
	}, loadFixtures(t, fixture)...)
	return func(context.Context) dynamic.Interface { return dyn }
}

// assertGolden compares v's JSON with testdata/<name>.golden.json; -update
// rewrites the file. The golden files are the tools' output contract.
func assertGolden(t *testing.T, name string, v any) {
	t.Helper()
	got, err := json.MarshalIndent(v, "", "  ")
	require.NoError(t, err)
	got = append(got, '\n')
	path := filepath.Join("testdata", name+".golden.json")
	if *update {
		require.NoError(t, os.WriteFile(path, got, 0o600))
	}
	want, err := os.ReadFile(path) //nolint:gosec // golden file named by the test
	require.NoError(t, err, "run with -update to create the golden file")
	assert.Equal(t, string(want), string(got))
}

func TestListClusters(t *testing.T) {
	svc := New(newFakeClients(t, "installation.yaml"), Config{Installation: "gazelle"})
	clusters, err := svc.ListClusters(context.Background())
	require.NoError(t, err)
	require.Len(t, clusters, 3)

	byName := map[string]Cluster{}
	for _, c := range clusters {
		byName[c.Name] = c
	}
	assert.True(t, byName["gazelle"].OwnCluster, "the installation's own cluster")
	assert.Equal(t, "giantswarm", byName["gazelle"].Organization, "organization from the org- namespace")
	assert.False(t, byName["wc1"].OwnCluster)
	assert.Equal(t, "acme", byName["wc1"].Organization, "organization from the label")
	assert.Equal(t, "31.0.0", byName["wc1"].ReleaseVersion)
	require.Len(t, byName["wc1"].PoolReleases, 1, "only wc1's pool release, by chart label and cluster label")
	assert.Equal(t, "0.3.0", byName["wc1"].PoolReleases[0].ChartVersion, "version from status.lastAttemptedRevision")
	require.Len(t, byName["wc2"].PoolReleases, 1)
	assert.Equal(t, "0.2.0", byName["wc2"].PoolReleases[0].ChartVersion, "version from the chart spec when nothing was attempted")
	assert.Nil(t, byName["wc2"].PoolReleases[0].Ready, "no Ready condition yet")
	assert.Nil(t, byName["wc1"].CommitTarget, "commit mode is not available in this stage")
	assert.Equal(t, "unknown", string(byName["wc1"].GPUOperator.Status), "detection is stubbed")

	assertGolden(t, "list_clusters", clusters)
}

func TestListClustersWithoutInstallationName(t *testing.T) {
	svc := New(newFakeClients(t, "installation.yaml"), Config{})
	clusters, err := svc.ListClusters(context.Background())
	require.NoError(t, err)
	for _, c := range clusters {
		assert.False(t, c.OwnCluster, "no installation name: no cluster is claimed as the installation's own")
	}
}

func TestListNodePools(t *testing.T) {
	svc := New(newFakeClients(t, "installation.yaml"), Config{Installation: "gazelle"})
	pools, err := svc.ListNodePools(context.Background(), "wc1", "")
	require.NoError(t, err)
	assert.Equal(t, "org-acme", pools.Namespace, "the namespace is found from the name")
	assert.Equal(t, "v1.31.4", pools.ControlPlaneVersion, "from the control plane object")
	require.Len(t, pools.NodePools, 2, "wc1's pools only")

	gpu := pools.NodePools[1]
	assert.Equal(t, "wc1-gpu-a10g", gpu.Name)
	assert.Equal(t, "v1.31.4", gpu.Version)
	assert.Equal(t, "v1.31.4", gpu.ControlPlaneVersion, "the two versions are two fields")
	assert.Equal(t, int64(2), gpu.Replicas)
	assert.Equal(t, int64(2), gpu.ReadyReplicas)
	assert.Equal(t, []string{"g5.2xlarge", "g5.xlarge"}, gpu.InstanceTypes, "Karpenter requirements")
	assert.Equal(t, "nvidia-a10g", gpu.Accelerator, "from the owning release's values")
	assert.Equal(t, &ReleaseRef{Name: "wc1-gpu-a10g", Namespace: "org-acme"}, gpu.OwnerRelease)

	def := pools.NodePools[0]
	assert.Equal(t, "wc1-def00", def.Name)
	assert.Equal(t, "v1.30.8", def.Version, "a pool behind the control plane is reported as it is, without a verdict")
	assert.Equal(t, []string{"m5.xlarge"}, def.InstanceTypes, "launch template")
	assert.Empty(t, def.Accelerator)
	assert.Nil(t, def.OwnerRelease, "made by other means")

	assertGolden(t, "list_node_pools", pools)
}

func TestListNodePoolsFallsBackToTheReleaseCR(t *testing.T) {
	svc := New(newFakeClients(t, "installation.yaml"), Config{})
	pools, err := svc.ListNodePools(context.Background(), "wc2", "org-acme")
	require.NoError(t, err)
	assert.Equal(t, "v1.30.8", pools.ControlPlaneVersion, "the Release CR's kubernetes component when the control plane object is unreadable")
	require.Len(t, pools.NodePools, 1)
	assert.Equal(t, []string{}, pools.NodePools[0].InstanceTypes, "unreadable infrastructure: empty, not an error")
	assert.Equal(t, int64(0), pools.NodePools[0].Replicas, "no spec.replicas")
}

func TestListNodePoolsUnknownCluster(t *testing.T) {
	svc := New(newFakeClients(t, "installation.yaml"), Config{})
	_, err := svc.ListNodePools(context.Background(), "nope", "")
	var notFound *ErrNotFound
	require.True(t, errors.As(err, &notFound), err)
	_, err = svc.ListNodePools(context.Background(), "wc1", "org-other")
	require.True(t, errors.As(err, &notFound), err)
}

func TestRequirementValues(t *testing.T) {
	spec := map[string]any{
		"a": []any{map[string]any{"key": "x", "values": []any{"1"}}},
		"b": map[string]any{"c": map[string]any{"key": "x", "values": []any{"2", 3}}},
	}
	assert.ElementsMatch(t, []string{"1", "2"}, requirementValues(spec, "x"))
	assert.Empty(t, requirementValues(spec, "y"))
}
