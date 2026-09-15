package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/cluster-manager/internal/compose"
)

func l4(cluster, name string, dryRun bool) CreateNodePoolInput {
	return CreateNodePoolInput{
		Cluster: cluster, Mode: ModeApply, DryRun: dryRun,
		Pool: compose.PoolSpec{Name: name, Accelerator: "nvidia-l4", MaxGPUs: 4, ChartVersion: compose.DefaultPoolChartVersion},
	}
}

// TestCreateNodePoolDryRun renders a new pool for wc1 from its Release CR
// (pins), its values ConfigMap (snapshot, credentials into a Secret) and its
// teleport Secret, and pins the answer as a golden.
func TestCreateNodePoolDryRun(t *testing.T) {
	svc := New(newFakeClients(t, "installation.yaml"), Config{Installation: "gazelle"})
	out, err := svc.CreateNodePool(context.Background(), l4("wc1", "gpu-l4", true))
	require.NoError(t, err)
	assert.Equal(t, "1.31.4", out.KubernetesVersion, "from the Release CR")
	assert.Equal(t, "v1.31.4", out.ControlPlaneVersion)
	assert.Equal(t, "flatcar-stable-4081.2.1-kube-1.31.4-tooling-1.26.1-gs", out.MachineImage, "cluster-aws's image name from the release's components")
	assert.Equal(t, "0.3.0", out.ChartVersion)
	require.Len(t, out.Objects, 3, "OCIRepository, credentials Secret, HelmRelease")
	for _, o := range out.Objects {
		assert.Equal(t, "would-create", o.Action, o.Kind)
	}
	hr := out.Manifests[2]
	values, _, _ := unstructured.NestedMap(hr, "spec", "values")
	assert.Equal(t, map[string]any{"enabled": true}, values["teleport"], "the join-token Secret exists")
	mirrors, _, _ := unstructured.NestedMap(values, "cluster", "registryMirrors")
	assert.Equal(t, []any{"registry.acme.example.io", "docker.io"}, mirrors["docker.io"], "endpoints without credentials")
	assert.Equal(t, "<redacted>", out.Manifests[1]["stringData"].(map[string]any)["values.yaml"], "the Secret's content is not echoed")
	assertGolden(t, "create_node_pool_dry_run", out)
}

// TestCreateNodePoolAppLayoutNoTeleport reads the snapshot from an App CR's
// user-values ConfigMap; wc2 has a proxy and no teleport Secret.
func TestCreateNodePoolAppLayoutNoTeleport(t *testing.T) {
	svc := New(newFakeClients(t, "installation.yaml"), Config{Installation: "gazelle"})
	out, err := svc.CreateNodePool(context.Background(), l4("wc2", "gpu-l4b", true))
	require.NoError(t, err)
	values, _, _ := unstructured.NestedMap(out.Manifests[len(out.Manifests)-1], "spec", "values")
	assert.Equal(t, map[string]any{"enabled": false}, values["teleport"])
	proxy, _, _ := unstructured.NestedMap(values, "cluster", "proxy")
	assert.Equal(t, "10.0.0.0/8,.acme.example.io", proxy["noProxy"])
	require.Len(t, out.Objects, 2, "no credentials: no Secret")
	assertGolden(t, "create_node_pool_app_layout", out)
}

// TestCreateNodePoolIdempotent: apply creates, the re-run is unchanged, a
// changed input is the update and its dry-run names the difference.
func TestCreateNodePoolIdempotent(t *testing.T) {
	clients := newFakeClients(t, "installation.yaml")
	svc := New(clients, Config{Installation: "gazelle"})
	ctx := context.Background()

	out, err := svc.CreateNodePool(ctx, l4("wc1", "gpu-l4", false))
	require.NoError(t, err)
	assert.Equal(t, []string{"create", "create", "create"}, actions(out))
	hr, err := clients(ctx).Resource(HelmReleaseGVR).Namespace("org-acme").Get(ctx, "wc1-gpu-l4", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "wc1", hr.GetOwnerReferences()[0].Name, "owned by the Cluster")
	assert.Equal(t, "cluster-manager", hr.GetLabels()[compose.LabelManagedBy])

	again, err := svc.CreateNodePool(ctx, l4("wc1", "gpu-l4", false))
	require.NoError(t, err)
	assert.Equal(t, []string{"unchanged", "unchanged", "unchanged"}, actions(again))

	bigger := l4("wc1", "gpu-l4", true)
	bigger.Pool.MaxGPUs = 8
	drift, err := svc.CreateNodePool(ctx, bigger)
	require.NoError(t, err)
	assert.Equal(t, []string{"unchanged", "unchanged", "would-update"}, actions(drift), "only the release changes; its source and Secret stand")
	assert.Equal(t, []string{"spec.values.pool.maxSize.nvidia.com/gpu"}, drift.Objects[2].Changes, "the dry-run is the drift check")

	bigger.DryRun = false
	updated, err := svc.CreateNodePool(ctx, bigger)
	require.NoError(t, err)
	assert.Equal(t, []string{"unchanged", "unchanged", "update"}, actions(updated)[0:3])
}

// TestCreateNodePoolReRunUpdatesAnExistingPool: wc1-gpu-a10g exists from an
// earlier version of the tool; the re-run brings it to the current shape.
func TestCreateNodePoolReRunUpdatesAnExistingPool(t *testing.T) {
	svc := New(newFakeClients(t, "installation.yaml"), Config{Installation: "gazelle"})
	in := l4("wc1", "gpu-a10g", true)
	in.Pool.Accelerator = "nvidia-a10g"
	out, err := svc.CreateNodePool(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, "would-update", out.Objects[2].Action)
	assert.Contains(t, out.Objects[2].Changes, "spec.values.pool.kubernetesVersion")
	assert.Contains(t, out.Objects[2].Changes, "metadata.ownerReferences")
}

func TestCreateNodePoolRefusals(t *testing.T) {
	svc := New(newFakeClients(t, "installation.yaml"), Config{Installation: "gazelle"})
	ctx := context.Background()

	commit := l4("wc1", "gpu-l4", true)
	commit.Mode = ModeCommit
	_, err := svc.CreateNodePool(ctx, commit)
	assertRefused(t, err, "mode commit")

	_, err = svc.CreateNodePool(ctx, l4("wc2", "gpu-l4", true))
	assertRefused(t, err, "owned by GitOps (Flux Kustomization workload-clusters)")

	_, err = svc.CreateNodePool(ctx, l4("wc1", "gpu", true))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pool name")

	_, err = svc.CreateNodePool(ctx, l4("gazelle", "gpu-l4", true))
	require.Error(t, err, "the installation's own cluster has no values object in the fixture")
	assert.Contains(t, err.Error(), "values of cluster gazelle")

	skew := New(newFakeClients(t, "skew.yaml"), Config{Installation: "gazelle"})
	_, err = skew.CreateNodePool(ctx, l4("wc3", "gpu-l4", true))
	assertRefused(t, err, "never newer than the control plane")
}

// TestDeleteNodePool: refused while the pool runs nodes (named), forced
// removes the release, its source and nothing else; a foreign release is
// never touched.
func TestDeleteNodePool(t *testing.T) {
	clients := newFakeClients(t, "installation.yaml")
	svc := New(clients, Config{Installation: "gazelle"})
	ctx := context.Background()

	_, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply})
	assertRefused(t, err, "i-0a1b2c3d4e5f60001, aws:///eu-west-1b/i-0a1b2c3d4e5f60002")

	dry, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply, Force: true, DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"would-delete", "would-delete"}, actions(dry))
	assertGolden(t, "delete_node_pool_dry_run", dry)

	out, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply, Force: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"delete", "delete"}, actions(out))
	_, err = clients(ctx).Resource(HelmReleaseGVR).Namespace("org-acme").Get(ctx, "wc1-gpu-a10g", metav1.GetOptions{})
	assert.Error(t, err, "gone")
	_, err = clients(ctx).Resource(compose.OCIRepositoryGVR).Namespace("org-acme").Get(ctx, "wc1-gpu-a10g", metav1.GetOptions{})
	assert.Error(t, err, "gone")

	_, err = svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply})
	var notFound *ErrNotFound
	assert.True(t, errors.As(err, &notFound), "a second delete finds nothing: %v", err)

	_, err = svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc2", Name: "gpu-l4", Mode: ModeApply, Force: true})
	assertRefused(t, err, "was not created by cluster-manager")
}

func actions(out *WriteResult) []string {
	acts := make([]string, 0, len(out.Objects))
	for _, o := range out.Objects {
		acts = append(acts, o.Action)
	}
	return acts
}

func assertRefused(t *testing.T, err error, contains string) {
	t.Helper()
	var refused *ErrRefused
	require.True(t, errors.As(err, &refused), "expected a refusal, got %v", err)
	assert.Contains(t, err.Error(), contains)
}
