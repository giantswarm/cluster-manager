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
	"github.com/giantswarm/cluster-manager/internal/detect"
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
	svc := newLab(t, "installation.yaml").service(Config{Installation: "gazelle"})
	out, err := svc.CreateNodePool(context.Background(), l4("wc1", "gpu-l4", true))
	require.NoError(t, err)
	assert.Equal(t, "1.31.4", out.KubernetesVersion, "from the Release CR")
	assert.Equal(t, "v1.31.4", out.ControlPlaneVersion)
	assert.Equal(t, "flatcar-stable-4081.2.1-kube-1.31.4-tooling-1.26.1-gs", out.MachineImage, "cluster-aws's image name from the release's components")
	assert.Equal(t, "0.3.1", out.ChartVersion)
	require.Len(t, out.Objects, 8, "OCIRepository, credentials Secret, HelmRelease; the operator's OCIRepository and HelmRelease; the slice's OCIRepository and HelmRelease; the backend ConfigMap")
	assert.Equal(t, compose.RowFlatcar.Name, out.OperatorRow, "Flatcar nodes, no operator: row 1")
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
	svc := newLab(t, "installation.yaml").service(Config{Installation: "gazelle"})
	out, err := svc.CreateNodePool(context.Background(), l4("wc2", "gpu-l4b", true))
	require.NoError(t, err)
	values, _, _ := unstructured.NestedMap(out.Manifests[1], "spec", "values")
	assert.Equal(t, map[string]any{"enabled": false}, values["teleport"])
	assert.Equal(t, compose.RowPreinstalled.Name, out.OperatorRow, "nodes labelled nvidia.com/gpu.deploy.driver=pre-installed: row 2")
	operator, _, _ := unstructured.NestedMap(out.Manifests[3], "spec", "values", "gpu-operator")
	assert.Equal(t, map[string]any{"enabled": false}, operator["driver"])
	assert.Equal(t, map[string]any{"enabled": true}, operator["toolkit"])
	assert.Equal(t, compose.PoolAffinity(compose.Cluster{Name: "wc2"}, []string{"gpu-l4b"}), operator[compose.NFDValuesKey].(map[string]any)["worker"].(map[string]any)["affinity"], "NFD's worker pinned to the pool being created; wc2's GitOps-owned pool release is not cluster-manager's and does not count")
	proxy, _, _ := unstructured.NestedMap(values, "cluster", "proxy")
	assert.Equal(t, "10.0.0.0/8,.acme.example.io", proxy["noProxy"])
	require.Len(t, out.Objects, 5, "no credentials: no Secret; the platform's release serves wc2: no slice")
	assert.Equal(t, detect.ProviderChart, out.Serving.Provider)
	assert.Nil(t, out.Slice, "a chart-provided serving layer is never re-created")
	assertGolden(t, "create_node_pool_app_layout", out)
}

// TestCreateNodePoolIdempotent: apply creates, the re-run is unchanged, a
// changed input is the update and its dry-run names the difference.
func TestCreateNodePoolIdempotent(t *testing.T) {
	lab := newLab(t, "installation.yaml")
	svc := lab.service(Config{Installation: "gazelle"})
	ctx := context.Background()

	out, err := svc.CreateNodePool(ctx, l4("wc1", "gpu-l4", false))
	require.NoError(t, err)
	assert.Equal(t, []string{"create", "create", "create", "create", "create", "create", "create", "create"}, actions(out))
	hr, err := lab.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Get(ctx, "wc1-gpu-l4", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "wc1", hr.GetOwnerReferences()[0].Name, "owned by the Cluster")
	assert.Equal(t, "cluster-manager", hr.GetLabels()[compose.LabelManagedBy])

	again, err := svc.CreateNodePool(ctx, l4("wc1", "gpu-l4", false))
	require.NoError(t, err)
	assert.Equal(t, []string{"unchanged", "unchanged", "unchanged", "unchanged", "unchanged", "unchanged", "unchanged", "unchanged"}, actions(again))

	bigger := l4("wc1", "gpu-l4", true)
	bigger.Pool.MaxGPUs = 8
	drift, err := svc.CreateNodePool(ctx, bigger)
	require.NoError(t, err)
	assert.Equal(t, []string{"unchanged", "unchanged", "would-update", "unchanged", "unchanged", "unchanged", "unchanged", "unchanged"}, actions(drift), "only the pool release changes; its source and Secret, the operator, the slice and the backend stand")
	assert.Equal(t, []string{"spec.values.pool.maxSize.nvidia.com/gpu"}, drift.Objects[2].Changes, "the dry-run is the drift check")

	bigger.DryRun = false
	updated, err := svc.CreateNodePool(ctx, bigger)
	require.NoError(t, err)
	assert.Equal(t, []string{"unchanged", "unchanged", "update"}, actions(updated)[0:3])
}

// TestCreateNodePoolReRunUpdatesAnExistingPool: wc1-gpu-a10g exists from an
// earlier version of the tool; the re-run brings it to the current shape.
func TestCreateNodePoolReRunUpdatesAnExistingPool(t *testing.T) {
	svc := newLab(t, "installation.yaml").service(Config{Installation: "gazelle"})
	in := l4("wc1", "gpu-a10g", true)
	in.Pool.Accelerator = "nvidia-a10g"
	out, err := svc.CreateNodePool(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, "would-update", out.Objects[2].Action)
	assert.Contains(t, out.Objects[2].Changes, "spec.values.pool.kubernetesVersion")
	assert.Contains(t, out.Objects[2].Changes, "metadata.ownerReferences")
	assert.Equal(t, "would-update", out.Objects[0].Action)
	assert.Contains(t, out.Objects[0].Changes, "spec.ref.tag", "the source moves its pin")
	assert.NotContains(t, out.Objects[0].Changes, "spec.provider", "the API server's default on the live source is not drift")
	assert.NotContains(t, out.Objects[0].Changes, "spec.timeout", "the API server's default on the live source is not drift")
}

// TestChangedPathsIgnoresServerDefaults: a leaf the composed object leaves
// unset and the live object carries at the API server's default is not
// drift; any other value there is, and so is the same leaf when composed.
func TestChangedPathsIgnoresServerDefaults(t *testing.T) {
	source := func(spec map[string]any) *unstructured.Unstructured {
		obj := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
		obj.SetAPIVersion(compose.OCIRepositoryGVR.GroupVersion().String())
		obj.SetKind("OCIRepository")
		return obj
	}
	want := source(map[string]any{"interval": "10m", "url": "oci://example/chart", "ref": map[string]any{"tag": "0.3.1"}})
	defaulted := source(map[string]any{"interval": "10m", "url": "oci://example/chart", "ref": map[string]any{"tag": "0.3.1"}, "provider": "generic", "timeout": "60s"})
	assert.Empty(t, changedPaths(defaulted, want), "the server's defaults are not drift")

	edited := defaulted.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(edited.Object, "aws", "spec", "provider"))
	assert.Equal(t, []string{"spec.provider"}, changedPaths(edited, want), "a value other than the default is: the update resets it")

	composed := want.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(composed.Object, "azure", "spec", "provider"))
	assert.Equal(t, []string{"spec.provider"}, changedPaths(defaulted, composed), "a leaf the composed object sets is compared as any other")

	release := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"interval": "10m", "timeout": "60s"}}}
	release.SetAPIVersion(HelmReleaseGVR.GroupVersion().String())
	release.SetKind("HelmRelease")
	bare := release.DeepCopy()
	unstructured.RemoveNestedField(bare.Object, "spec", "timeout")
	assert.Equal(t, []string{"spec.timeout"}, changedPaths(release, bare), "a kind without server defaults keeps every leaf")
}

func TestCreateNodePoolRefusals(t *testing.T) {
	svc := newLab(t, "installation.yaml").service(Config{Installation: "gazelle"})
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

	skew := newLab(t, "skew.yaml").service(Config{Installation: "gazelle"})
	_, err = skew.CreateNodePool(ctx, l4("wc3", "gpu-l4", true))
	assertRefused(t, err, "never newer than the control plane")
}

// TestDeleteNodePool: refused while the pool runs nodes — named, with the
// models served on the cluster (a Pending one too), or that none is, or why
// that cannot be told —, forced removes the release, its source and nothing
// else; a foreign release is never touched.
func TestDeleteNodePool(t *testing.T) {
	lab := newLab(t, "installation.yaml")
	svc := lab.service(Config{Installation: "gazelle"})
	ctx := context.Background()

	_, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply})
	assertRefused(t, err, "node pool wc1-gpu-a10g still runs 2 node(s) (")
	assertRefused(t, err, "i-0a1b2c3d4e5f60001, aws:///eu-west-1b/i-0a1b2c3d4e5f60002) and wc1 serves no model: nothing of the platform's serving holds them")

	lab.target(t, wc1APIServer, "wc1-serving.yaml")
	_, err = svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply})
	assertRefused(t, err, "i-0a1b2c3d4e5f60002), serving 2 model(s) on wc1: InferenceService model-serving/mistral-7b (mistralai/Mistral-7B-Instruct-v0.3), LLMInferenceService model-serving/llama-3-8b (meta-llama/Llama-3.1-8B-Instruct) — unload them first (model-manager's unload_model, or the cluster's Serving group) and re-run once the pool is empty, or pass force to delete the pool with its nodes and the models on them")

	lab.unreachable(wc1APIServer)
	_, err = svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply})
	assertRefused(t, err, "; whether models are served on wc1 cannot be told (cluster wc1 not readable as you through "+wc1APIServer+": connection refused) — check the cluster's Serving group")
	lab.target(t, wc1APIServer, "wc1.yaml", servingAPIs...)

	dry, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply, Force: true, DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"would-delete", "would-delete"}, actions(dry))
	assertGolden(t, "delete_node_pool_dry_run", dry)

	out, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply, Force: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"delete", "delete"}, actions(out))
	_, err = lab.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Get(ctx, "wc1-gpu-a10g", metav1.GetOptions{})
	assert.Error(t, err, "gone")
	_, err = lab.installation.Resource(compose.OCIRepositoryGVR).Namespace("org-acme").Get(ctx, "wc1-gpu-a10g", metav1.GetOptions{})
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

// TestCreateNodePoolSizesAndPresetFit (giantswarm/agent-platform#502): the
// answer lists the pool's sizes with what each leaves a predictor; on wc2,
// which publishes three presets, the resized L4 preset fits an xlarge, the
// 128 GB preset no L4 (no warning: the accelerator cannot serve it) and the
// installation's 4 vCPU / 16 GiB preset only a 2xlarge — a warning on an
// xlarge-only pool, a size on the default pool. On wc1, which publishes none,
// a note. A size the family lacks is refused before any write.
func TestCreateNodePoolSizesAndPresetFit(t *testing.T) {
	svc := newLab(t, "installation.yaml").service(Config{Installation: "gazelle"})
	ctx := context.Background()

	narrow := l4("wc2", "gpu-l4b", true)
	narrow.Pool.Sizes = []string{"xlarge"}
	out, err := svc.CreateNodePool(ctx, narrow)
	require.NoError(t, err)
	require.Len(t, out.Sizes, 1)
	assert.Equal(t, compose.InstanceShape{InstanceType: "g6.xlarge", Size: "xlarge", VCPU: 4, MemoryGiB: 16, GPUs: 1, GPUMemoryGiB: 24, UsableVCPU: 3, UsableMemoryGiB: 11.9}, out.Sizes[0])
	require.NotNil(t, out.PresetFit)
	assert.Equal(t, "3 preset ConfigMap(s) in agent-platform on wc2", out.PresetFit.Source)
	assert.Empty(t, out.PresetFit.Note)
	require.Len(t, out.PresetFit.Presets, 3)
	assert.Equal(t, PresetSizeFit{Preset: "acme-l4-wide", CPU: "4", Memory: "16Gi", GPUs: 1, GPUMemoryGiB: 21,
		Reason: "requests 4 vCPU / 16 GiB; xlarge leaves a predictor 3 vCPU / 11.9 GiB after the node's kubelet reservations and daemonsets — 2xlarge (8 vCPU / 32 GiB) would host it"}, out.PresetFit.Presets[0])
	assert.Equal(t, PresetSizeFit{Preset: "qwen3-14b", CPU: "4", Memory: "48Gi", GPUs: 1, GPUMemoryGiB: 58,
		Reason: "needs 58 GiB of GPU memory across 1 GPU(s); a g6 GPU has 24 GiB"}, out.PresetFit.Presets[1])
	assert.Equal(t, PresetSizeFit{Preset: "qwen3-4b-instruct", CPU: "2", Memory: "10Gi", GPUs: 1, GPUMemoryGiB: 20, Size: "xlarge"}, out.PresetFit.Presets[2])
	require.Len(t, out.Warnings, 1, "the 128 GB preset is no warning: no L4 serves it")
	assert.Equal(t, "serving preset acme-l4-wide fits no size of pool gpu-l4b: requests 4 vCPU / 16 GiB; xlarge leaves a predictor 3 vCPU / 11.9 GiB after the node's kubelet reservations and daemonsets — 2xlarge (8 vCPU / 32 GiB) would host it — a predictor composed from it would sit Pending while Karpenter refuses every size (giantswarm/agent-platform#502); add the size to sizes or serve a smaller preset", out.Warnings[0])
	assertGolden(t, "create_node_pool_preset_fit_warning", out)

	wide, err := svc.CreateNodePool(ctx, l4("wc2", "gpu-l4b", true))
	require.NoError(t, err)
	assert.Equal(t, []string{"xlarge", "2xlarge", "4xlarge"}, sizeNames(wide.Sizes), "the chart's default sizes")
	assert.Equal(t, "2xlarge", wide.PresetFit.Presets[0].Size)
	assert.Empty(t, wide.Warnings)

	none, err := svc.CreateNodePool(ctx, l4("wc1", "gpu-l4", true))
	require.NoError(t, err)
	assert.Len(t, none.Sizes, 3)
	assert.Equal(t, "no serving preset is published on wc1 yet — the slice release publishes them once it is ready; a dryRun re-run then says which of the pool's sizes host each", none.PresetFit.Note)
	assert.Empty(t, none.PresetFit.Presets)
	assert.Empty(t, none.Warnings)

	bad := l4("wc1", "gpu-l4", true)
	bad.Pool.Sizes = []string{"xlarge", "3xlarge"}
	_, err = svc.CreateNodePool(ctx, bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `size "3xlarge": not a size of the g6 family (nvidia-l4); the sizes are xlarge, 2xlarge, 4xlarge, 8xlarge, 12xlarge, 16xlarge, 24xlarge, 48xlarge`)
}

func sizeNames(shapes []compose.InstanceShape) []string {
	out := make([]string, 0, len(shapes))
	for _, s := range shapes {
		out = append(out, s.Size)
	}
	return out
}
