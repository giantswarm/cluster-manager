package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/detect"
)

func l4(cluster, name string, dryRun bool) CreateNodePoolInput {
	return CreateNodePoolInput{
		Cluster: cluster, Mode: ModeApply, DryRun: dryRun, Cache: cacheOn(),
		Pool: compose.PoolSpec{Name: name, Accelerator: "nvidia-l4", MaxGPUs: 4, ChartVersion: compose.DefaultPoolChartVersion},
	}
}

// TestCreateNodePoolDryRun renders a new pool for wc1 from its Release CR
// (pins) and its values ConfigMap (snapshot, credentials into a Secret), and
// pins the answer as a golden.
func TestCreateNodePoolDryRun(t *testing.T) {
	svc := newLab(t, "installation.yaml").service(Config{Installation: "gazelle"})
	out, err := svc.CreateNodePool(context.Background(), l4("wc1", "gpu-l4", true))
	require.NoError(t, err)
	assert.Equal(t, "1.31.4", out.KubernetesVersion, "from the Release CR")
	assert.Equal(t, "v1.31.4", out.ControlPlaneVersion)
	assert.Equal(t, "flatcar-stable-4459.2.1-kube-1.31.4-tooling-1.26.1-gs", out.MachineImage, "cluster-aws's image name from the release's components")
	assert.Equal(t, compose.DefaultPoolChartVersion, out.ChartVersion)
	require.Len(t, out.Objects, 8, "OCIRepository, credentials Secret, HelmRelease; the operator's OCIRepository and HelmRelease; the slice's OCIRepository and HelmRelease; the backend ConfigMap")
	assert.Equal(t, compose.RowFlatcar.Name, out.OperatorRow, "Flatcar nodes, no operator: row 1")
	for _, o := range out.Objects {
		assert.Equal(t, "would-create", o.Action, o.Kind)
	}
	hr := out.Manifests[2]
	values, _, _ := unstructured.NestedMap(hr, "spec", "values")
	_, hasTeleport := values["teleport"]
	assert.False(t, hasTeleport, "the chart joins the nodes with the cluster's join token; nothing to set (giantswarm/cluster-manager#86)")
	mirrors, _, _ := unstructured.NestedMap(values, "cluster", "registryMirrors")
	assert.Equal(t, []any{"registry.acme.example.io", "docker.io"}, mirrors["docker.io"], "endpoints without credentials")
	assert.Equal(t, "<redacted>", out.Manifests[1]["stringData"].(map[string]any)["values.yaml"], "the Secret's content is not echoed")
	assertGolden(t, "create_node_pool_dry_run", out)
}

// TestCreateNodePoolCacheDefault (giantswarm/cluster-manager#86): without
// cache a first slice serves without a model cache — a claim is a volume
// billed every month it exists, never created unasked — and the answer says
// so; cache true is the opt-in (the other fixtures carry it, cacheOn).
func TestCreateNodePoolCacheDefault(t *testing.T) {
	svc := newLab(t, "installation.yaml").service(Config{Installation: "gazelle"})
	in := l4("wc1", "gpu-l4", true)
	in.Cache = nil
	out, err := svc.CreateNodePool(context.Background(), in)
	require.NoError(t, err)
	require.NotNil(t, out.Cache, "the slice was composed")
	assert.False(t, out.Cache.Enabled, "a first slice: no cache unasked")
	assert.Contains(t, out.Cache.Note, "modelServing.cache.enabled false on the slice release")
	var slice map[string]any
	for _, m := range out.Manifests {
		if m["kind"] == "HelmRelease" && m["metadata"].(map[string]any)["name"] == "wc1-agent-platform" {
			slice = m
		}
	}
	require.NotNil(t, slice, "the slice release is among the manifests")
	enabled, found, _ := unstructured.NestedBool(slice, "spec", "values", "modelServing", "cache", "enabled")
	require.True(t, found, "the setting is written, not left to the chart's default")
	assert.False(t, enabled)
	assert.Empty(t, out.Zones, "no claim pins the pool")
}

// TestCreateNodePoolPrewarm (giantswarm/cluster-manager#48): the
// installation's own pool takes prewarm — the dry run's pool release carries
// pool.prewarm.enabled: true and nothing else of the block (the hold and the
// image stay the chart's) — while a workload cluster's pool is refused,
// naming the constraint, before anything is composed.
func TestCreateNodePoolPrewarm(t *testing.T) {
	l := newLab(t, "installation.yaml")
	l.add(t, l.installation, "prewarm.yaml")
	svc := l.service(Config{Installation: "gazelle"})
	in := l4("gazelle", "gpu-l40s", true)
	in.Pool.Accelerator, in.Pool.Prewarm = "nvidia-l40s", true
	out, err := svc.CreateNodePool(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, compose.DefaultPoolChartVersion, out.ChartVersion, "the pin carries the option")
	var release map[string]any
	for _, m := range out.Manifests {
		if m["kind"] == "HelmRelease" && m["metadata"].(map[string]any)["name"] == "gazelle-gpu-l40s" {
			release = m
		}
	}
	require.NotNil(t, release, "the pool release is among the manifests")
	prewarm, _, _ := unstructured.NestedMap(release, "spec", "values", "pool", "prewarm")
	assert.Equal(t, map[string]any{"enabled": true}, prewarm)

	in.Pool.Prewarm = false
	out, err = svc.CreateNodePool(context.Background(), in)
	require.NoError(t, err)
	for _, m := range out.Manifests {
		if m["kind"] == "HelmRelease" && m["metadata"].(map[string]any)["name"] == "gazelle-gpu-l40s" {
			_, found, _ := unstructured.NestedMap(m, "spec", "values", "pool", "prewarm")
			assert.False(t, found, "without the argument the values carry no prewarm block")
		}
	}

	wc := l4("wc1", "gpu-l4", true)
	wc.Pool.Prewarm = true
	_, err = svc.CreateNodePool(context.Background(), wc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wc1 is a workload cluster")
	assert.Contains(t, err.Error(), "re-run without prewarm")
}

// TestCreateNodePoolAppLayout reads the snapshot from an App CR's
// user-values ConfigMap; wc2 has a proxy.
func TestCreateNodePoolAppLayout(t *testing.T) {
	svc := newLab(t, "installation.yaml").service(Config{Installation: "gazelle"})
	out, err := svc.CreateNodePool(context.Background(), l4("wc2", "gpu-l4b", true))
	require.NoError(t, err)
	values, _, _ := unstructured.NestedMap(out.Manifests[1], "spec", "values")
	assert.Equal(t, compose.RowPreinstalled.Name, out.OperatorRow, "nodes labelled nvidia.com/gpu.deploy.driver=pre-installed: row 2")
	operator, _, _ := unstructured.NestedMap(out.Manifests[4], "spec", "values", "gpu-operator")
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

// TestCreateNodePoolRefusesAFlatcarWithoutTheDriverExtension
// (giantswarm/cluster-manager#66): wc4's release pins Flatcar 4230.2.1, older
// than the first release shipping the nvidia-drivers system extension a
// pool node of the pinned chart takes its driver from. The apply is refused
// naming the version and the way out, and nothing is written.
func TestCreateNodePoolRefusesAFlatcarWithoutTheDriverExtension(t *testing.T) {
	ctx := context.Background()
	lab := newLab(t, "old-flatcar.yaml")
	_, err := lab.service(Config{Installation: "gazelle"}).CreateNodePool(ctx, l4("wc4", "gpu-l4", false))
	assertRefused(t, err, "pins Flatcar 4230.2.1")
	assert.Contains(t, err.Error(), "needs a cluster release with Flatcar "+compose.MinSysextFlatcarVersion+" or newer")
	for _, gvr := range []schema.GroupVersionResource{compose.OCIRepositoryGVR, HelmReleaseGVR} {
		_, err := lab.installation.Resource(gvr).Namespace("org-acme").Get(ctx, "wc4-gpu-l4", metav1.GetOptions{})
		assert.True(t, apierrors.IsNotFound(err), "%s: nothing written before the refusal", gvr.Resource)
	}
}

// TestDeleteNodePool: refused while the pool runs nodes — named, with the
// models served on the cluster (a Pending one too), or that none is, or why
// that cannot be told —, forced removes the release, its source and nothing
// else; a foreign release is never touched.
// TestDeleteNodePool (giantswarm/cluster-manager#49): the nodes guard reads
// the pool's nodes on the cluster. A node with a predictor on it is refused,
// the node and the models named, the idle one beside; with the cluster not
// readable the MachinePool's provider IDs decide and the refusal says so;
// with force the pool goes regardless, its nodes with its release. Idle
// nodes go with the pool in one call: their NodeClaims are deleted first
// and listed in objects, the dry run lists them too.
func TestDeleteNodePool(t *testing.T) {
	lab := newLab(t, "installation.yaml")
	svc := lab.service(Config{Installation: "gazelle"})
	ctx := context.Background()
	del := DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply}

	lab.target(t, wc1APIServer, "wc1-serving.yaml")
	_, err := svc.DeleteNodePool(ctx, del)
	assertRefused(t, err, "node pool wc1-gpu-a10g still runs 1 busy node(s) on wc1: node wc1-gpu-a10g-node-1 runs model-serving/llama-3-8b-kserve-6649fb66c8-dllt7 (1 GPU), serving 1 model(s) on wc1: LLMInferenceService model-serving/llama-3-8b (meta-llama/Llama-3.1-8B-Instruct) — unload them first (model-manager's unload_model, or the cluster's Serving group) and re-run once the pool is empty, or pass force to delete the pool with its nodes and the models on them; 1 idle node(s) (wc1-gpu-a10g-node-2) go with the pool once the busy ones are free")
	assert.Equal(t, &Refused{
		Nodes:    []string{"wc1-gpu-a10g-node-1"},
		Idle:     []string{"wc1-gpu-a10g-node-2"},
		Models:   []string{"LLMInferenceService model-serving/llama-3-8b (meta-llama/Llama-3.1-8B-Instruct)"},
		Hint:     refusedHint,
		ReadFrom: readFromCluster,
	}, refusedBlock(t, err), "the structured refusal beside the text (giantswarm/cluster-manager#41): the DaemonSet's, the finished and the preemptible pod hold nothing")

	lab.unreachable(wc1APIServer)
	_, err = svc.DeleteNodePool(ctx, del)
	assertRefused(t, err, "node pool wc1-gpu-a10g still runs 2 node(s) as its MachinePool lists them (aws:///eu-west-1a/i-0a1b2c3d4e5f60001, aws:///eu-west-1b/i-0a1b2c3d4e5f60002) — cluster wc1 not readable as you through "+wc1APIServer+": connection refused, so the MachinePool's provider IDs decide, a list that lags a terminated instance by minutes; whether models are served on wc1 cannot be told (cluster wc1 not readable as you through "+wc1APIServer+": connection refused) — check the cluster's Serving group")
	assert.Equal(t, &Refused{Nodes: []string{"aws:///eu-west-1a/i-0a1b2c3d4e5f60001", "aws:///eu-west-1b/i-0a1b2c3d4e5f60002"}, Models: []string{}, Hint: refusedHintMachinePool, ReadFrom: readFromMachinePool}, refusedBlock(t, err))

	lab.target(t, wc1APIServer, "wc1-serving.yaml")
	forced, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply, Force: true, DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"would-delete OCIRepository org-acme/wc1-gpu-a10g", "would-delete HelmRelease org-acme/wc1-gpu-a10g"}, objectNames(forced), "force judges no node: the release's removal takes them down")

	lab.target(t, wc1APIServer, "wc1.yaml", servingAPIs...)
	dry, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply, DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"would-delete NodeClaim /wc1-gpu-a10g-k7m2p", "would-delete NodeClaim /wc1-gpu-a10g-q9x4z", "would-delete OCIRepository org-acme/wc1-gpu-a10g", "would-delete HelmRelease org-acme/wc1-gpu-a10g"}, objectNames(dry), "the idle nodes' NodeClaims first")
	assertGolden(t, "delete_node_pool_dry_run", dry)
	assert.Len(t, poolClaims(t, lab, "wc1-gpu-a10g"), 2, "a dry run touches nothing")

	out, err := svc.DeleteNodePool(ctx, del)
	require.NoError(t, err)
	assert.Equal(t, []string{"delete", "delete", "delete", "delete"}, actions(out))
	assert.Empty(t, poolClaims(t, lab, "wc1-gpu-a10g"), "the NodeClaims are gone: Karpenter drains the nodes and terminates the instances")
	_, err = lab.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Get(ctx, "wc1-gpu-a10g", metav1.GetOptions{})
	assert.Error(t, err, "gone")
	_, err = lab.installation.Resource(compose.OCIRepositoryGVR).Namespace("org-acme").Get(ctx, "wc1-gpu-a10g", metav1.GetOptions{})
	assert.Error(t, err, "gone")

	_, err = svc.DeleteNodePool(ctx, del)
	var notFound *ErrNotFound
	assert.True(t, errors.As(err, &notFound), "a second delete finds nothing: %v", err)

	_, err = svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc2", Name: "gpu-l4", Mode: ModeApply, Force: true})
	assertRefused(t, err, "was not created by cluster-manager")
}

// TestDeleteNodePoolIgnoresTheMachinePoolsLingeringProviderIDs
// (giantswarm/cluster-manager#49): the cluster shows no NodeClaim and no Node
// of the pool while its MachinePool still lists two provider IDs — the list
// follows a terminated instance minutes later. The live reads decide: the
// pool goes without force and without a NodeClaim to remove.
func TestDeleteNodePoolIgnoresTheMachinePoolsLingeringProviderIDs(t *testing.T) {
	lab := newLab(t, "installation.yaml").target(t, wc1APIServer, "wc1-nodeclaims.yaml", servingAPIs...)
	svc := lab.service(Config{Installation: "gazelle"})
	ctx := context.Background()
	mp, err := lab.installation.Resource(MachinePoolGVR).Namespace("org-acme").Get(ctx, "wc1-gpu-a10g", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, int64(2), nestedInt(mp, "spec", "replicas"), "the MachinePool still counts the gone instances")

	out, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply})
	require.NoError(t, err)
	assert.Equal(t, []string{"delete OCIRepository org-acme/wc1-gpu-a10g", "delete HelmRelease org-acme/wc1-gpu-a10g"}, objectNames(out))
}

// TestDeleteNodePoolRefusesWhatItCannotJudge: a NodeClaim still launching (no
// node registered: a predictor asked for it), pods on a node not readable as
// the caller, and a NodeClaim the caller may not delete are each a refusal
// with the way out — never a node torn from under a person.
func TestDeleteNodePoolRefusesWhatItCannotJudge(t *testing.T) {
	ctx := context.Background()
	del := DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply}

	lab := newLab(t, "installation.yaml")
	svc := lab.service(Config{Installation: "gazelle"})
	launching := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "karpenter.sh/v1", "kind": "NodeClaim",
		"metadata": map[string]any{"name": "wc1-gpu-a10g-n3w0n", "labels": map[string]any{detect.LabelKarpenterNodePool: "wc1-gpu-a10g"}},
		"status":   map[string]any{"providerID": "aws:///eu-west-1a/i-0a1b2c3d4e5f60003"},
	}}
	_, err := lab.targets[wc1APIServer].Resource(detect.NodeClaimGVR).Create(ctx, launching, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = svc.DeleteNodePool(ctx, del)
	assertRefused(t, err, "node pool wc1-gpu-a10g still runs 1 busy node(s) on wc1: NodeClaim wc1-gpu-a10g-n3w0n is launching, no node registered yet — a predictor asked for it and wc1 serves no model: nothing of the platform's serving holds them — scale what is named away and re-run once the pool is empty, or pass force to delete the pool with its nodes; 2 idle node(s) (wc1-gpu-a10g-node-1, wc1-gpu-a10g-node-2) go with the pool once the busy ones are free")
	assert.Equal(t, []string{"wc1-gpu-a10g-n3w0n"}, refusedBlock(t, err).Nodes)

	lab = newLab(t, "installation.yaml")
	svc = lab.service(Config{Installation: "gazelle"})
	fakeTarget(t, lab, wc1APIServer).PrependReactor("list", detect.PodsGVR.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("User \"alice\" cannot list resource \"pods\" in API group \"\" at the cluster scope"))
	})
	_, err = svc.DeleteNodePool(ctx, del)
	assertRefused(t, err, "node pool wc1-gpu-a10g still runs 2 busy node(s) on wc1: whether node wc1-gpu-a10g-node-1 is idle cannot be told (list the pods on node wc1-gpu-a10g-node-1: pods is forbidden: User \"alice\" cannot list resource \"pods\" in API group \"\" at the cluster scope); whether node wc1-gpu-a10g-node-2 is idle cannot be told (")
	assert.Equal(t, readFromCluster, refusedBlock(t, err).ReadFrom)

	lab = newLab(t, "installation.yaml")
	svc = lab.service(Config{Installation: "gazelle"})
	fakeTarget(t, lab, wc1APIServer).PrependReactor("delete", detect.NodeClaimGVR.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(detect.NodeClaimGVR.GroupResource(), "wc1-gpu-a10g-k7m2p", errors.New("User \"alice\" cannot delete resource \"nodeclaims\" in API group \"karpenter.sh\" at the cluster scope"))
	})
	_, err = svc.DeleteNodePool(ctx, del)
	assertRefused(t, err, "delete NodeClaim wc1-gpu-a10g-k7m2p: nodeclaims.karpenter.sh \"wc1-gpu-a10g-k7m2p\" is forbidden: User \"alice\" cannot delete resource \"nodeclaims\" in API group \"karpenter.sh\" at the cluster scope: removing the pool's idle node wc1-gpu-a10g-node-1 needs the delete of its NodeClaim as you — ask for it, wait for Karpenter to consolidate the empty node, or pass force to remove the pool regardless (its release's removal takes the nodes down)")
	_, err = lab.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Get(ctx, "wc1-gpu-a10g", metav1.GetOptions{})
	require.NoError(t, err, "nothing else was written: the NodeClaims go first")
}

// poolClaims lists the NodeClaims of a pool on wc1.
func poolClaims(t *testing.T, l *lab, pool string) []unstructured.Unstructured {
	t.Helper()
	claims, err := l.targets[wc1APIServer].Resource(detect.NodeClaimGVR).List(context.Background(), metav1.ListOptions{LabelSelector: detect.LabelKarpenterNodePool + "=" + pool})
	require.NoError(t, err)
	return claims.Items
}

func actions(out *WriteResult) []string {
	acts := make([]string, 0, len(out.Objects))
	for _, o := range out.Objects {
		acts = append(acts, o.Action)
	}
	return acts
}

func refusedBlock(t *testing.T, err error) *Refused {
	t.Helper()
	var refused *ErrRefused
	require.True(t, errors.As(err, &refused), "expected a refusal, got %v", err)
	return refused.Refused
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
	assert.Equal(t, compose.InstanceShape{InstanceType: "g6.xlarge", Size: "xlarge", VCPU: 4, MemoryGiB: 16, GPUs: 1, GPUMemoryGiB: 24, InstanceStoreGB: 250, InstanceStoreDisks: 1, InstanceStoreDiskGB: 250, UsableVCPU: 3, UsableMemoryGiB: 11.9,
		PriceNote: "no on-demand price: AWS lists no on-demand g6.xlarge in EU (Ireland) (eu-west-1) — the size is not offered there"}, out.Sizes[0], "wc2 runs in Ireland, where AWS offers no g6: no price, and why")
	require.NotNil(t, out.PresetFit)
	assert.Equal(t, PresetOriginPublished, out.PresetFit.Origin)
	assert.Equal(t, "3 preset ConfigMap(s) in agent-platform on wc2", out.PresetFit.Source)
	assert.Empty(t, out.PresetFit.Note)
	require.Len(t, out.PresetFit.Presets, 3)
	assert.Equal(t, PresetSizeFit{Preset: "acme-l4-wide", DisplayName: "Acme's wide L4 recipe", Model: "acme/wide-9b", CPU: "4", Memory: "16Gi", GPUs: 1, GPUMemoryGiB: 21,
		Reason: "requests 4 vCPU / 16 GiB; xlarge leaves a predictor 3 vCPU / 11.9 GiB after the node's kubelet reservations and daemonsets — 2xlarge (8 vCPU / 32 GiB) would host it"}, out.PresetFit.Presets[0])
	assert.Equal(t, PresetSizeFit{Preset: "qwen3-14b", DisplayName: "Qwen3 14B", Model: "Qwen/Qwen3-14B", CPU: "4", Memory: "48Gi", GPUs: 1, GPUMemoryGiB: 58,
		Reason: "needs 58 GiB of GPU memory across 1 GPU(s); a g6 GPU has 24 GiB"}, out.PresetFit.Presets[1])
	assert.Equal(t, PresetSizeFit{Preset: "qwen3-4b-instruct", DisplayName: "Qwen3 4B Instruct", Model: "Qwen/Qwen3-4B-Instruct-2507", CPU: "2", Memory: "10Gi", GPUs: 1, GPUMemoryGiB: 20, Size: "xlarge"}, out.PresetFit.Presets[2])
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
	require.Len(t, none.Sizes, 3)
	require.NotNil(t, none.Sizes[0].PricePerHourUSD, "wc1 runs in Frankfurt")
	assert.InDelta(t, 1.0064, *none.Sizes[0].PricePerHourUSD, 1e-9)
	assert.Equal(t, "AWS EC2 on-demand Linux list price, EU (Frankfurt) (eu-central-1)", none.Sizes[0].PriceSource)
	assert.Equal(t, compose.PriceAsOf, none.Sizes[0].PriceAsOf)
	assert.Equal(t, "no serving preset is published on wc1 yet — the slice release publishes them once it is ready; a dryRun re-run then says which of the pool's sizes host each", none.PresetFit.Note, "this server reads no charts: the note stands")
	assert.Empty(t, none.PresetFit.Origin)
	assert.Empty(t, none.PresetFit.Presets)
	assert.Empty(t, none.Warnings)

	bad := l4("wc1", "gpu-l4", true)
	bad.Pool.Sizes = []string{"xlarge", "3xlarge"}
	_, err = svc.CreateNodePool(ctx, bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `size "3xlarge": not a size of the g6 family (nvidia-l4); the sizes are xlarge, 2xlarge, 4xlarge, 8xlarge, 12xlarge, 16xlarge, 24xlarge, 48xlarge`)
}

// TestCreateNodePoolPresetFitFromChart (giantswarm/cluster-manager#44): wc1
// publishes no preset and the call would compose the slice, so the presets
// that slice would publish are judged — read from the connectivity chart the
// slice's agent-platform release (4.44.1, the fixture's platform) resolves
// for its range, 4.45.0 in the fake registry, not the meta chart's own
// version. The 8B preset fits an xlarge; the 14B none (no warning: no L4
// serves it); the wide recipe only a 2xlarge — a warning on an xlarge-only
// pool. wc2's published ConfigMaps keep precedence over the chart, and a
// registry that cannot be read is a note.
func TestCreateNodePoolPresetFitFromChart(t *testing.T) {
	lab := newLab(t, "installation.yaml")
	svc := lab.service(Config{Installation: "gazelle"}, WithChartReader(shippedPresetCharts()))
	ctx := context.Background()

	out, err := svc.CreateNodePool(ctx, l4("wc1", "gpu-l4", true))
	require.NoError(t, err)
	require.NotNil(t, out.PresetFit)
	assert.Equal(t, PresetOriginChart, out.PresetFit.Origin)
	assert.Equal(t, `3 preset(s) shipped by agent-platform-connectivity 4.45.0, the chart the slice's agent-platform 4.44.1 release resolves for ">=4.0.0 <5.0.0" at gsoci.azurecr.io — the slice publishes them once it is ready`, out.PresetFit.Source)
	assert.Empty(t, out.PresetFit.Note)
	require.Len(t, out.PresetFit.Presets, 3)
	assert.Equal(t, PresetSizeFit{Preset: "qwen3-14b", DisplayName: "Qwen3 14B", Model: "Qwen/Qwen3-14B", CPU: "4", Memory: "48Gi", GPUs: 1, GPUMemoryGiB: 58,
		Reason: "needs 58 GiB of GPU memory across 1 GPU(s); a g6 GPU has 24 GiB"}, out.PresetFit.Presets[0])
	assert.Equal(t, PresetSizeFit{Preset: "qwen3-8b-fp8", DisplayName: "Qwen3 8B FP8", Model: "Qwen/Qwen3-8B-FP8", CPU: "2", Memory: "10Gi", GPUs: 1, GPUMemoryGiB: 21, Size: "xlarge"}, out.PresetFit.Presets[1])
	assert.Equal(t, PresetSizeFit{Preset: "wide-l4", DisplayName: "Wide L4 recipe", Model: "acme/wide-9b", CPU: "4", Memory: "16Gi", GPUs: 1, GPUMemoryGiB: 20, Size: "2xlarge"}, out.PresetFit.Presets[2])
	assert.Empty(t, out.Warnings, "every hostable preset has a size in the default pool")
	require.NotNil(t, out.Sizes[0].PricePerHourUSD)
	assert.InDelta(t, 1.0064, *out.Sizes[0].PricePerHourUSD, 1e-9, "g6.xlarge in Frankfurt, beside the fit")
	// The claim the slice would create, priced from the same chart's defaults
	// (giantswarm/cluster-manager#83): what the cache costs before it exists.
	require.NotNil(t, out.Cache)
	assert.True(t, out.Cache.Enabled)
	assert.False(t, out.Cache.Exists)
	assert.Equal(t, "100Gi", out.Cache.Capacity)
	assert.Equal(t, "gp3, 500 MiB/s, 3000 IOPS", out.Cache.Tier)
	require.NotNil(t, out.Cache.MonthlyPriceUSD, out.Cache.PriceNote)
	assert.InDelta(t, 27.37, *out.Cache.MonthlyPriceUSD, 1e-9, "100 GiB gp3 at 500 MiB/s in Frankfurt")
	assert.Equal(t, "AWS EBS gp3 list price, EU (Frankfurt) (eu-central-1)", out.Cache.PriceSource)
	assert.Contains(t, out.Cache.Note, "it does not exist yet: the connectivity chart creates it and keeps it at its defaults, 100Gi gp3, 500 MiB/s, 3000 IOPS: about $27.37 a month at list prices (AWS EBS gp3 list price, EU (Frankfurt) (eu-central-1), as of "+compose.PriceAsOf+"), billed from its first bind while the claim exists — after every pool of the cluster is removed too — until the cache is removed with remove_model_cache")
	assertGolden(t, "create_node_pool_preset_fit_from_chart", out)

	narrow := l4("wc1", "gpu-l4", true)
	narrow.Pool.Sizes = []string{"xlarge"}
	out, err = svc.CreateNodePool(ctx, narrow)
	require.NoError(t, err)
	assert.Equal(t, "xlarge", out.PresetFit.Presets[1].Size)
	assert.Empty(t, out.PresetFit.Presets[2].Size)
	require.Len(t, out.Warnings, 1)
	assert.Contains(t, out.Warnings[0], "serving preset wide-l4 fits no size of pool gpu-l4: requests 4 vCPU / 16 GiB")

	published, err := svc.CreateNodePool(ctx, l4("wc2", "gpu-l4b", true))
	require.NoError(t, err)
	assert.Equal(t, PresetOriginPublished, published.PresetFit.Origin, "the cluster's own presets take precedence over the chart's")
	assert.Equal(t, "3 preset ConfigMap(s) in agent-platform on wc2", published.PresetFit.Source)

	unreadable := lab.service(Config{Installation: "gazelle"}, WithChartReader(&fakeCharts{}))
	out, err = unreadable.CreateNodePool(ctx, l4("wc1", "gpu-l4", true))
	require.NoError(t, err)
	assert.Equal(t, "no serving preset is published on wc1 yet — the slice release publishes them once it is ready, and the presets it would publish could not be read from the registry (pull oci://gsoci.azurecr.io/charts/giantswarm/agent-platform 4.44.1: HTTP 404): whether the pool's sizes host them is not judged", out.PresetFit.Note)
	assert.Empty(t, out.PresetFit.Presets)
	assert.Empty(t, out.PresetFit.Origin)
}

// TestCreateNodePoolBackendDocumentFollowsTheSizes (giantswarm/cluster-manager#26):
// the kserve backend document of a cluster's only pool names the pool's
// instance shapes (spec.kserve.gpuPool.instances) for model-manager's fit
// check at the pool's scale-from-zero, and a re-run with other sizes updates
// them.
func TestCreateNodePoolBackendDocumentFollowsTheSizes(t *testing.T) {
	svc := newLab(t, "installation.yaml").service(Config{Installation: "gazelle"})
	ctx := context.Background()

	narrow := l4("wc2", "gpu-l4b", false)
	narrow.Pool.Sizes = []string{"xlarge"}
	out, err := svc.CreateNodePool(ctx, narrow)
	require.NoError(t, err)
	assert.Contains(t, backendDoc(out), "gpuPool:\n      instances:\n      - gpuMemoryGiB: 24\n        gpus: 1\n        instanceType: g6.xlarge\n        memoryGiB: 16\n        size: xlarge\n        usableMemoryGiB: 11.9\n        usableVcpu: 3\n        vcpu: 4\n", "the one pool's shapes, in the pool's order")
	assert.NotContains(t, backendDoc(out), "g6.2xlarge", "the pool's sizes only")

	drift, err := svc.CreateNodePool(ctx, l4("wc2", "gpu-l4b", true))
	require.NoError(t, err)
	backend := backendAction(drift)
	assert.Equal(t, "would-update", backend.Action, "the document follows the pool's sizes")
	assert.Equal(t, []string{"data.backend.yaml"}, backend.Changes)
	assert.Contains(t, backendDoc(drift), "instanceType: g6.4xlarge\n", "the chart's default sizes on the re-run")
}

// backendDoc is the kserve backend document among the answer's manifests.
func backendDoc(out *WriteResult) string {
	for _, m := range out.Manifests {
		if m["kind"] != "ConfigMap" {
			continue
		}
		data, _ := m["data"].(map[string]any)
		doc, _ := data["backend.yaml"].(string)
		return doc
	}
	return ""
}

// backendAction is the kserve backend document's entry among the answer's objects.
func backendAction(out *WriteResult) ObjectAction {
	for _, o := range out.Objects {
		if o.Kind == "ConfigMap" && o.Name == compose.BackendConfigMapName {
			return o
		}
	}
	return ObjectAction{}
}

func sizeNames(shapes []compose.InstanceShape) []string {
	out := make([]string, 0, len(shapes))
	for _, s := range shapes {
		out = append(out, s.Size)
	}
	return out
}

// TestCreateNodePoolFollowsTheCacheZone (giantswarm/cluster-manager#59): with
// the serving namespace's model cache claim Bound, the pool release carries
// pool.zones with the volume's zone and the answer names the pin, the claim
// and its volume — on the installation's own pool and on a workload
// cluster's, whose claim is read on the cluster as the caller. Without a
// claim the values carry no zones block and the answer says nothing of it.
func TestCreateNodePoolFollowsTheCacheZone(t *testing.T) {
	ctx := context.Background()
	l := newLab(t, "installation.yaml")
	l.add(t, l.installation, "prewarm.yaml")
	l.add(t, l.installation, "cache-claim.yaml")
	svc := l.service(Config{Installation: "gazelle"})
	in := l4("gazelle", "gpu-l40s", true)
	in.Pool.Accelerator, in.Pool.Sizes, in.Pool.MaxGPUs, in.Pool.Prewarm = "nvidia-l40s", []string{"2xlarge", "4xlarge"}, 1, true
	out, err := svc.CreateNodePool(ctx, in)
	require.NoError(t, err)
	assert.Equal(t, []string{"eu-central-1b"}, out.Zones)
	assert.Equal(t, "nodes pinned to eu-central-1b: the model cache (claim model-serving/hf-cache, volume pvc-6e577f13-ff22-461c-a453-cfbcdd2d7c80) lives there, and a node launched in another zone strands a predictor mounting it Pending; name zones on create to run the pool elsewhere — its slice then mounts that zone's claim (hf-cache-<zone>, created there and kept; the weights downloaded once more) —, or pick an accelerator offered in eu-central-1b — a family not offered there fails its launch, named in list_node_pools' nodes step", out.ZonesNote)
	require.NotNil(t, out.Cache, "the slice was composed: the answer says the cache is on")
	assert.True(t, out.Cache.Enabled)
	assert.Equal(t, "model-serving/hf-cache", out.Cache.Claim)
	require.NotNil(t, out.CacheClaim)
	assert.Equal(t, "hf-cache", out.CacheClaim.Name)
	assert.Equal(t, "eu-central-1b", out.CacheClaim.Zone)
	assert.Equal(t, "500Gi", out.CacheClaim.Capacity, "what the claim is billed for (giantswarm/cluster-manager#83)")
	assert.Equal(t, &compose.VolumeTier{Type: "gp3", IOPS: 3000, ThroughputMiBps: 500}, out.CacheClaim.Tier, "from its StorageClass")
	require.NotNil(t, out.CacheClaim.Price, out.CacheClaim.PriceNote)
	assert.InDelta(t, 65.45, out.CacheClaim.Price.MonthlyUSD, 1e-9, "500 GiB gp3 at 500 MiB/s in Frankfurt")
	assert.Equal(t, "2026-09-16T10:12:00Z", out.CacheClaim.Created)
	assert.Equal(t, detect.ReclaimDelete, out.CacheClaim.ReclaimPolicy)
	assert.Equal(t, []*detect.CacheClaim{out.CacheClaim}, out.CacheClaims, "every claim of the namespace, as read")
	assert.Equal(t, "500Gi", out.Cache.Capacity)
	assert.Equal(t, "gp3, 500 MiB/s, 3000 IOPS", out.Cache.Tier)
	assert.True(t, out.Cache.Exists)
	assert.InDelta(t, 65.45, *out.Cache.MonthlyPriceUSD, 1e-9, "the cache block carries the claim's price")
	assert.Contains(t, out.Cache.Note, "500 GiB gp3 at 500 MiB/s, about $65.45 a month at list prices (AWS EBS gp3 list price, EU (Frankfurt) (eu-central-1), as of "+compose.PriceAsOf+"), billed while the claim exists until the cache is removed with remove_model_cache — after every pool of the cluster is removed too")
	assert.Empty(t, out.Warnings, "a pin is not a warning")
	_, found, _ := unstructured.NestedString(poolManifest(t, out, "gazelle-agent-platform"), "spec", "values", "modelServing", "cache", "pvc", "name")
	assert.False(t, found, "the one claim of before is the chart's default: nothing written for it")
	zones, found, _ := unstructured.NestedStringSlice(poolManifest(t, out, "gazelle-gpu-l40s"), "spec", "values", "pool", "zones")
	require.True(t, found, "the pool release carries pool.zones")
	assert.Equal(t, []string{"eu-central-1b"}, zones)
	assertGolden(t, "create_node_pool_cache_zone", out)

	// The workload cluster's claim lives on the workload cluster.
	l.add(t, l.targets[wc1APIServer], "cache-claim.yaml")
	out, err = svc.CreateNodePool(ctx, l4("wc1", "gpu-l4", true))
	require.NoError(t, err)
	assert.Equal(t, []string{"eu-central-1b"}, out.Zones, "read on wc1 as the caller")
	zones, _, _ = unstructured.NestedStringSlice(poolManifest(t, out, "wc1-gpu-l4"), "spec", "values", "pool", "zones")
	assert.Equal(t, []string{"eu-central-1b"}, zones)

	// No claim on wc2: no pin, no note, no block.
	out, err = svc.CreateNodePool(ctx, l4("wc2", "gpu-l4b", true))
	require.NoError(t, err)
	assert.Nil(t, out.Zones)
	assert.Empty(t, out.ZonesNote)
	assert.Nil(t, out.CacheClaim)
	assert.Equal(t, []*detect.CacheClaim{}, out.CacheClaims, "readable, none: an empty list")
	_, found, _ = unstructured.NestedSlice(poolManifest(t, out, "wc2-gpu-l4b"), "spec", "values", "pool", "zones")
	assert.False(t, found, "without a claim the values carry no zones block")
}

// TestCreateNodePoolZones (giantswarm/cluster-manager#65, #71): the person
// decides where the pool runs, and the zone brings its own cache. The one
// zone named on create pins the pool and its slice mounts the zone's claim:
// the claim Bound there (the one of before, in its zone), else the claim
// named after the zone, created by the chart — the slice release carrying
// modelServing.cache.pvc.name; several zones with the cache on follow the
// cache (giantswarm/cluster-manager#79): the one claim Bound among them pins
// the pool to its zone at once, none Bound among them mounts the base claim
// for the first predictor to bind, claims in several of them and the base
// claim Bound outside them are structured refusals, as are several claims
// without zones; a zone the
// cluster has no node subnet in is refused naming the zones it has; a
// cluster whose node subnets cannot be read refuses every zone; without a
// claim the zone stands alone and gets its claim.
func TestCreateNodePoolZones(t *testing.T) {
	ctx := context.Background()
	l := newLab(t, "installation.yaml")
	l.add(t, l.installation, "prewarm.yaml") // the own cluster's values and join token
	l.add(t, l.installation, "cache-claim.yaml")
	svc := l.service(Config{Installation: "gazelle"})
	pool := func(zones ...string) CreateNodePoolInput {
		in := l4("gazelle", "gpu-l40s", true)
		in.Pool.Accelerator, in.Pool.Sizes, in.Pool.MaxGPUs, in.Pool.Zones = "nvidia-l40s", []string{"2xlarge", "4xlarge"}, 1, zones
		return in
	}

	sliceClaim := func(t *testing.T, out *WriteResult) (string, bool) {
		t.Helper()
		name, found, _ := unstructured.NestedString(poolManifest(t, out, "gazelle-agent-platform"), "spec", "values", "modelServing", "cache", "pvc", "name")
		return name, found
	}

	t.Run("the zone the claim is Bound in: the slice mounts it", func(t *testing.T) {
		out, err := svc.CreateNodePool(ctx, pool("eu-central-1b"))
		require.NoError(t, err)
		assert.Equal(t, []string{"eu-central-1b"}, out.Zones)
		assert.Equal(t, "nodes pinned to eu-central-1b, the zone named on create; the slice mounts the zone's model cache claim model-serving/hf-cache (volume pvc-6e577f13-ff22-461c-a453-cfbcdd2d7c80), Bound there — the weights and compiled graphs of the models served from it are kept, and a predictor mounting it needs its node in eu-central-1b", out.ZonesNote)
		assert.Empty(t, out.Warnings)
		zones, _, _ := unstructured.NestedStringSlice(poolManifest(t, out, "gazelle-gpu-l40s"), "spec", "values", "pool", "zones")
		assert.Equal(t, []string{"eu-central-1b"}, zones, "the pool release carries the person's zone")
		assert.True(t, out.Cache.Enabled)
		assert.Equal(t, "model-serving/hf-cache", out.Cache.Claim, "the one claim of before is the zone's: reused, not doubled")
		assert.Equal(t, "eu-central-1b", out.CacheClaim.Zone)
		_, found := sliceClaim(t, out)
		assert.False(t, found, "the chart's default name: nothing written")
		assertGolden(t, "create_node_pool_zones", out)
	})
	t.Run("another zone: the zone's own claim, created by the chart", func(t *testing.T) {
		out, err := svc.CreateNodePool(ctx, pool("eu-central-1a"))
		require.NoError(t, err, "the claim of another zone binds nothing here (giantswarm/cluster-manager#71)")
		assert.Equal(t, []string{"eu-central-1a"}, out.Zones)
		assert.Equal(t, "nodes pinned to eu-central-1a, the zone named on create; the slice mounts the zone's model cache claim model-serving/hf-cache-eu-central-1a, which does not exist yet: the connectivity chart creates it, the first predictor of the pool binds it to a volume in eu-central-1a, and it is kept when the pool goes — a later pool in eu-central-1a reuses it; the other claims — model-serving/hf-cache (Bound in eu-central-1b) — are left as they are, each its zone's", out.ZonesNote)
		assert.Empty(t, out.Warnings)
		assert.Equal(t, "model-serving/hf-cache-eu-central-1a", out.Cache.Claim)
		assert.Equal(t, "the predictors mount the model cache claim model-serving/hf-cache-eu-central-1a — it does not exist yet: the connectivity chart creates it and keeps it (no price: the size and tier the connectivity chart creates the claim with could not be read (this server reads no chart registry)), billed from its first bind while the claim exists — after every pool of the cluster is removed too — until the cache is removed with remove_model_cache; the first predictor binds it to a volume in its node's zone; the weights and compiled graphs of every model served from this slice are kept there, and a pool created in the claim's zone with the cache on reuses it; the slice release is the cluster's one, so every predictor of the cluster mounts this claim from now on", out.Cache.Note)
		assert.False(t, out.Cache.Exists)
		assert.Contains(t, out.Cache.PriceNote, "this server reads no chart registry")
		assert.Nil(t, out.CacheClaim, "the zone's claim does not exist yet")
		assert.Len(t, out.CacheClaims, 1, "the one claim of before, as read")
		name, found := sliceClaim(t, out)
		require.True(t, found, "the slice release names the zone's claim")
		assert.Equal(t, "hf-cache-eu-central-1a", name)
		assertGolden(t, "create_node_pool_zone_claim", out)
	})
	t.Run("several zones with the cache on, the claim Bound in one of them: the pool follows its cache", func(t *testing.T) {
		out, err := svc.CreateNodePool(ctx, pool("eu-central-1a", "eu-central-1b"))
		require.NoError(t, err, "no refusal: the pool follows the claim (giantswarm/cluster-manager#79)")
		assert.Equal(t, []string{"eu-central-1b"}, out.Zones, "the one zone of the named the claim is Bound in")
		assert.Equal(t, "nodes pinned to eu-central-1b, of eu-central-1a, eu-central-1b named on create: the model cache claim model-serving/hf-cache (volume pvc-6e577f13-ff22-461c-a453-cfbcdd2d7c80) is Bound there, and a predictor mounting it runs nowhere else — the pool follows its cache, and a node in eu-central-1a would only strand a predictor Pending or hold a placeholder it cannot use; pass cache false to run across eu-central-1a, eu-central-1b without the cache, or name one of eu-central-1a alone to serve from it with its own claim (hf-cache-<zone>, created there and kept)", out.ZonesNote)
		assert.Empty(t, out.Warnings)
		zones, _, _ := unstructured.NestedStringSlice(poolManifest(t, out, "gazelle-gpu-l40s"), "spec", "values", "pool", "zones")
		assert.Equal(t, []string{"eu-central-1b"}, zones, "the pool release carries the claim's zone, loudly narrowed")
		assert.Equal(t, "model-serving/hf-cache", out.Cache.Claim)
		_, found := sliceClaim(t, out)
		assert.False(t, found, "the chart's default name: nothing written")
		assertGolden(t, "create_node_pool_zones_follow_cache", out)
	})
	t.Run("several zones with the cache on, the base claim Bound outside them: refused, structured", func(t *testing.T) {
		_, err := svc.CreateNodePool(ctx, pool("eu-central-1a", "eu-central-1c"))
		assertRefused(t, err, "zones eu-central-1a, eu-central-1c: the model cache claim model-serving/hf-cache on gazelle — the claim a pool across several zones binds in the zone its first predictor lands in — is bound to volume pvc-6e577f13-ff22-461c-a453-cfbcdd2d7c80 in eu-central-1b already, outside every zone named, and a pool node launched in eu-central-1a, eu-central-1c cannot mount it: every predictor mounting it would sit Pending (`didn't match PersistentVolume's node affinity`); name eu-central-1b among the zones — the pool then follows the claim there, or name one zone — the slice then mounts that zone's model cache claim (the one Bound there, else hf-cache-<zone>, which the connectivity chart creates and keeps), or pass cache false, so this pool's slice serves without the cache across the zones — the weights land in each predictor pod's ephemeral storage and the claim is left as it is")
		refused := refusedBlock(t, err)
		require.NotNil(t, refused.CacheZone, "the portal renders the refusal without parsing prose")
		assert.Equal(t, "hf-cache", refused.CacheZone.Claim.Name)
		assert.Equal(t, "eu-central-1b", refused.CacheZone.ClaimZone)
		assert.Equal(t, []string{"eu-central-1a", "eu-central-1c"}, refused.CacheZone.Zones)
		assert.Len(t, refused.CacheZone.Remedies, 3)
		assert.Equal(t, readFromCluster, refused.ReadFrom)
		assert.Empty(t, refused.Nodes)
		assert.Empty(t, refused.Models)
	})
	t.Run("several zones with the cache on and no claim: the base claim, bound by the first predictor", func(t *testing.T) {
		in := l4("wc1", "gpu-l4", true)
		in.Pool.Zones = []string{"eu-central-1a", "eu-central-1b"}
		out, err := svc.CreateNodePool(ctx, in)
		require.NoError(t, err)
		assert.Equal(t, []string{"eu-central-1a", "eu-central-1b"}, out.Zones, "the person's zones stand while no claim is Bound")
		assert.Equal(t, "nodes pinned to eu-central-1a, eu-central-1b, the zones named on create; the slice mounts the model cache claim model-serving/hf-cache, which does not exist yet: the connectivity chart creates it and keeps it, and the first predictor binds it to a volume in its node's zone, one of eu-central-1a, eu-central-1b, and from then on every predictor mounting it runs there — the pool follows its cache: a re-run of this create_node_pool pins the pool's nodes to that zone, and a later pool naming that zone alone reuses the claim", out.ZonesNote)
		assert.Nil(t, out.CacheClaim)
		assert.Equal(t, "model-serving/hf-cache", out.Cache.Claim)
		zones, _, _ := unstructured.NestedStringSlice(poolManifest(t, out, "wc1-gpu-l4"), "spec", "values", "pool", "zones")
		assert.Equal(t, []string{"eu-central-1a", "eu-central-1b"}, zones)
		_, found, _ := unstructured.NestedString(poolManifest(t, out, "wc1-agent-platform"), "spec", "values", "modelServing", "cache", "pvc", "name")
		assert.False(t, found, "the chart's default name: nothing written")
	})
	t.Run("a zone the cluster has no node subnet in", func(t *testing.T) {
		_, err := svc.CreateNodePool(ctx, pool("eu-central-1b", "eu-west-1a"))
		assertRefused(t, err, "zones eu-west-1a: cluster gazelle has no node subnet there — its node subnets are in eu-central-1a, eu-central-1b, eu-central-1c, and a pool's nodes come up in those; name zones among them")
	})
	t.Run("no claim on the cluster: the zone stands alone and gets its claim", func(t *testing.T) {
		in := l4("wc1", "gpu-l4", true)
		in.Pool.Zones = []string{"eu-central-1a"}
		out, err := svc.CreateNodePool(ctx, in)
		require.NoError(t, err)
		assert.Equal(t, []string{"eu-central-1a"}, out.Zones)
		assert.Equal(t, "nodes pinned to eu-central-1a, the zone named on create; the slice mounts the zone's model cache claim model-serving/hf-cache-eu-central-1a, which does not exist yet: the connectivity chart creates it, the first predictor of the pool binds it to a volume in eu-central-1a, and it is kept when the pool goes — a later pool in eu-central-1a reuses it", out.ZonesNote)
		assert.Nil(t, out.CacheClaim)
		assert.Equal(t, []*detect.CacheClaim{}, out.CacheClaims)
		assert.Equal(t, "model-serving/hf-cache-eu-central-1a", out.Cache.Claim)
	})
	t.Run("a claim per zone: no zones is refused naming them, a zone mounts its own", func(t *testing.T) {
		l.add(t, l.installation, "cache-claim-zone.yaml")
		_, err := svc.CreateNodePool(ctx, pool())
		assertRefused(t, err, "model-serving on gazelle has several model cache claims — model-serving/hf-cache (Bound in eu-central-1b), model-serving/hf-cache-eu-central-1a (Bound in eu-central-1a): a pool's slice mounts one, the claim of the zone the pool runs in, and without zones none is chosen for you; name zones with the one zone the pool runs in — the slice then mounts that zone's claim (the one Bound there, else hf-cache-<zone>, which the connectivity chart creates and keeps), or pass cache false, so this pool's slice serves without the cache — the weights land in each predictor pod's ephemeral storage, and the claims are left as they are")
		refused := refusedBlock(t, err)
		require.NotNil(t, refused.CacheClaims, "the portal renders the refusal without parsing prose")
		assert.Len(t, refused.CacheClaims.Claims, 2)
		assert.Equal(t, "eu-central-1a", refused.CacheClaims.Claims[1].Zone)
		assert.Len(t, refused.CacheClaims.Remedies, 2)
		assert.Nil(t, refused.CacheZone)

		out, err := svc.CreateNodePool(ctx, pool("eu-central-1a"))
		require.NoError(t, err)
		assert.Equal(t, "model-serving/hf-cache-eu-central-1a", out.Cache.Claim, "the zone's claim, Bound there")
		assert.Equal(t, "eu-central-1a", out.CacheClaim.Zone)
		assert.Equal(t, "nodes pinned to eu-central-1a, the zone named on create; the slice mounts the zone's model cache claim model-serving/hf-cache-eu-central-1a (volume pvc-0c2f4a1e-1a1a-4a1a-9a1a-000000000a1a), Bound there — the weights and compiled graphs of the models served from it are kept, and a predictor mounting it needs its node in eu-central-1a; the other claims — model-serving/hf-cache (Bound in eu-central-1b) — are left as they are, each its zone's", out.ZonesNote)
		assert.Len(t, out.CacheClaims, 2)
		name, found := sliceClaim(t, out)
		require.True(t, found)
		assert.Equal(t, "hf-cache-eu-central-1a", name)

		out, err = svc.CreateNodePool(ctx, pool("eu-central-1b"))
		require.NoError(t, err)
		assert.Equal(t, "model-serving/hf-cache", out.Cache.Claim, "the other zone's claim, the one of before")

		_, err = svc.CreateNodePool(ctx, pool("eu-central-1a", "eu-central-1b"))
		assertRefused(t, err, "zones eu-central-1a, eu-central-1b: model cache claims on gazelle are Bound in 2 of them — model-serving/hf-cache (Bound in eu-central-1b), model-serving/hf-cache-eu-central-1a (Bound in eu-central-1a): a pool's slice mounts one claim, the one of the zone the pool runs in, and none is chosen for you; name one zone — the slice then mounts that zone's model cache claim (the one Bound there, else hf-cache-<zone>, which the connectivity chart creates and keeps), or pass cache false, so this pool's slice serves without the cache across the zones — the weights land in each predictor pod's ephemeral storage, and the claims are left as they are")
		refused = refusedBlock(t, err)
		require.NotNil(t, refused.CacheClaims, "claims in several of the named zones: the structured claims refusal")
		assert.Len(t, refused.CacheClaims.Claims, 2)
		assert.Nil(t, refused.CacheZone)

		out, err = svc.CreateNodePool(ctx, pool("eu-central-1b", "eu-central-1c"))
		require.NoError(t, err, "one claim among the named zones: the pool follows it")
		assert.Equal(t, []string{"eu-central-1b"}, out.Zones)
		assert.Equal(t, "model-serving/hf-cache", out.Cache.Claim)
		assert.Contains(t, out.ZonesNote, "nodes pinned to eu-central-1b, of eu-central-1b, eu-central-1c named on create: the model cache claim model-serving/hf-cache")
		assert.Contains(t, out.ZonesNote, "; the other claims — model-serving/hf-cache-eu-central-1a (Bound in eu-central-1a) — are left as they are, each its zone's")

		off := false
		in := pool("eu-central-1a", "eu-central-1c")
		in.Cache = &off
		out, err = svc.CreateNodePool(ctx, in)
		require.NoError(t, err, "several zones without the cache: nothing is mounted, so nothing pins")
		assert.Equal(t, "nodes pinned to eu-central-1a, eu-central-1c, the zones named on create; this pool's slice serves without the model cache (cache false): no predictor mounts the claims model-serving/hf-cache (Bound in eu-central-1b), model-serving/hf-cache-eu-central-1a (Bound in eu-central-1a) on gazelle, so their zones pin nothing and they are left as they are", out.ZonesNote)
		assert.Contains(t, out.Cache.Note, "the existing claims model-serving/hf-cache (Bound in eu-central-1b), model-serving/hf-cache-eu-central-1a (Bound in eu-central-1a) are left as they are — Helm keeps them, each billed while it exists until the cache is removed with remove_model_cache — and pin nothing")

		clusters, err := svc.ListClusters(ctx)
		require.NoError(t, err)
		for _, c := range clusters.Clusters {
			if c.Name == "gazelle" {
				assert.Len(t, c.Serving.Readiness.CacheClaims, 2, "list_clusters names every claim with its zone and phase")
			}
		}
	})
	t.Run("the node subnets cannot be read: every zone refused, naming why", func(t *testing.T) {
		in := l4("wc2", "gpu-l4b", true)
		in.Pool.Zones = []string{"eu-west-1a"}
		_, err := svc.CreateNodePool(ctx, in)
		assertRefused(t, err, "zones eu-west-1a: the node subnets of cluster wc2 cannot be read (AWSCluster org-acme/wc2 lists no node subnet (spec.network.subnets tagged giantswarm.io/role: nodes)) — a pool's nodes come up in the cluster's node subnets, so the zones cannot be checked against them")
	})
}

// TestZonePinForChoice words every case of the caller's choice against the
// claims (giantswarm/cluster-manager#65, #71): the cache off pins nothing
// from a claim whatever its state, and allows several zones; one zone with
// the cache on mounts the zone's claim — the one Bound there (the claim
// named after the zone first, else the one of before), else the claim named
// after the zone, not existing yet, Pending, naming no zone or not readable,
// each said beside the pin; no zones lets the one claim decide; several
// zones follow the cache — the one claim Bound among them pins its zone,
// none mounts the base claim (giantswarm/cluster-manager#79) —; several
// claims, claims in several of the zones named and a claim Bound elsewhere
// are the refusals.
func TestZonePinForChoice(t *testing.T) {
	claim := func(name, phase, volume, zone, err string) *detect.CacheClaim {
		return &detect.CacheClaim{Namespace: "model-serving", Name: name, Phase: phase, Volume: volume, Zone: zone, Error: err}
	}
	read := func(claims ...*detect.CacheClaim) cacheClaims {
		return cacheClaims{namespace: "model-serving", base: "hf-cache", claims: claims}
	}
	bound := claim("hf-cache", "Bound", "pvc-1", "eu-central-1b", "")
	pending := claim("hf-cache", "Pending", "", "", "")
	noZone := claim("hf-cache", "Bound", "pvc-1", "", "")
	unreadable := claim("hf-cache", "Bound", "pvc-1", "", "forbidden")
	zoneA := claim("hf-cache-eu-central-1a", "Bound", "pvc-a", "eu-central-1a", "")
	zoneAPending := claim("hf-cache-eu-central-1a", "Pending", "", "", "")
	zoneANoZone := claim("hf-cache-eu-central-1a", "Bound", "pvc-a", "", "")
	zoneAUnreadable := claim("hf-cache-eu-central-1a", "Bound", "pvc-a", "", "forbidden")
	zoneB := claim("hf-cache-eu-central-1b", "Bound", "pvc-b", "eu-central-1b", "")
	unlisted := cacheClaims{namespace: "model-serving", base: "hf-cache", err: "forbidden"}
	a, b, ab := []string{"eu-central-1a"}, []string{"eu-central-1b"}, []string{"eu-central-1a", "eu-central-1b"}
	on := func(zones ...string) zoneChoice { return zoneChoice{zones: zones, cache: true} }
	off := func(zones ...string) zoneChoice { return zoneChoice{zones: zones} }
	cases := []struct {
		name      string
		read      cacheClaims
		choice    zoneChoice
		zones     []string
		claimName string
		claim     *detect.CacheClaim
		note      string
		warning   string
	}{
		{"cache off, no zones, a bound claim", read(bound), off(), nil, "", nil, "the pool's nodes are not pinned to a zone; this pool's slice serves without the model cache (cache false): no predictor mounts the claim model-serving/hf-cache on mc, so its zone pins nothing and the claim is left as it is", ""},
		{"cache off, two zones, no claim", read(), off(ab...), ab, "", nil, "nodes pinned to eu-central-1a, eu-central-1b, the zones named on create; this pool's slice serves without the model cache (cache false), so no zone follows from a claim", ""},
		{"cache off, the claims not listable is no warning", unlisted, off(a...), a, "", nil, "nodes pinned to eu-central-1a, the zones named on create; this pool's slice serves without the model cache (cache false), so no zone follows from a claim", ""},
		{"cache off, two claims", read(bound, zoneA), off(), nil, "", nil, "the pool's nodes are not pinned to a zone; this pool's slice serves without the model cache (cache false): no predictor mounts the claims model-serving/hf-cache (Bound in eu-central-1b), model-serving/hf-cache-eu-central-1a (Bound in eu-central-1a) on mc, so their zones pin nothing and they are left as they are", ""},
		{"one zone, no claim: the zone's claim, to be created", read(), on(a...), a, "hf-cache-eu-central-1a", nil, "nodes pinned to eu-central-1a, the zone named on create; the slice mounts the zone's model cache claim model-serving/hf-cache-eu-central-1a, which does not exist yet: the connectivity chart creates it, the first predictor of the pool binds it to a volume in eu-central-1a, and it is kept when the pool goes — a later pool in eu-central-1a reuses it", ""},
		{"one zone, the claim of before elsewhere: the zone's own, the other left", read(bound), on(a...), a, "hf-cache-eu-central-1a", nil, "nodes pinned to eu-central-1a, the zone named on create; the slice mounts the zone's model cache claim model-serving/hf-cache-eu-central-1a, which does not exist yet: the connectivity chart creates it, the first predictor of the pool binds it to a volume in eu-central-1a, and it is kept when the pool goes — a later pool in eu-central-1a reuses it; the other claims — model-serving/hf-cache (Bound in eu-central-1b) — are left as they are, each its zone's", ""},
		{"one zone, the claim of before Bound there: reused", read(bound), on(b...), b, "hf-cache", bound, "nodes pinned to eu-central-1b, the zone named on create; the slice mounts the zone's model cache claim model-serving/hf-cache (volume pvc-1), Bound there — the weights and compiled graphs of the models served from it are kept, and a predictor mounting it needs its node in eu-central-1b", ""},
		{"one zone, two claims Bound there: the one named after the zone", read(bound, zoneB), on(b...), b, "hf-cache-eu-central-1b", zoneB, "nodes pinned to eu-central-1b, the zone named on create; the slice mounts the zone's model cache claim model-serving/hf-cache-eu-central-1b (volume pvc-b), Bound there — the weights and compiled graphs of the models served from it are kept, and a predictor mounting it needs its node in eu-central-1b; the other claims — model-serving/hf-cache (Bound in eu-central-1b) — are left as they are, each its zone's", ""},
		{"one zone, its claim Pending", read(zoneAPending), on(a...), a, "hf-cache-eu-central-1a", zoneAPending, "nodes pinned to eu-central-1a, the zone named on create; the slice mounts the zone's model cache claim model-serving/hf-cache-eu-central-1a, Pending and bound to no volume yet — the first predictor mounting it binds it to a volume in eu-central-1a, and a later pool there reuses it", ""},
		{"one zone, its claim's volume without a zone", read(zoneANoZone), on(a...), a, "hf-cache-eu-central-1a", zoneANoZone, "nodes pinned to eu-central-1a, the zone named on create; the slice mounts the model cache claim model-serving/hf-cache-eu-central-1a, bound to volume pvc-a, whose node affinity names no zone — a volume every zone reaches strands no predictor", ""},
		{"one zone, its claim not readable", read(zoneAUnreadable), on(a...), a, "hf-cache-eu-central-1a", zoneAUnreadable, "nodes pinned to eu-central-1a, the zone named on create; the slice mounts the zone's model cache claim model-serving/hf-cache-eu-central-1a", "the model cache claim model-serving/hf-cache-eu-central-1a on mc cannot be read as you (forbidden): whether its volume lies in eu-central-1a cannot be told — the cache is one volume, bound in one zone, and a node launched in another zone strands a predictor mounting it Pending; re-run once you may read the claim and its volume, or pass cache false so this pool's slice serves without it"},
		{"one zone, the claims not listable", unlisted, on(a...), a, "hf-cache-eu-central-1a", nil, "nodes pinned to eu-central-1a, the zone named on create; the slice mounts the zone's model cache claim model-serving/hf-cache-eu-central-1a", "the model cache claims of model-serving on mc cannot be read as you (forbidden): whether a claim is Bound in eu-central-1a, and which, cannot be told — the slice mounts model-serving/hf-cache-eu-central-1a, the claim named after the zone, which the connectivity chart creates where it does not exist; re-run once you may list the claims, or pass cache false so this pool's slice serves without one"},
		{"no zones, cache on: the one claim's zone (giantswarm/cluster-manager#59)", read(bound), on(), b, "hf-cache", bound, "nodes pinned to eu-central-1b: the model cache (claim model-serving/hf-cache, volume pvc-1) lives there, and a node launched in another zone strands a predictor mounting it Pending; name zones on create to run the pool elsewhere — its slice then mounts that zone's claim (hf-cache-<zone>, created there and kept; the weights downloaded once more) —, or pick an accelerator offered in eu-central-1b — a family not offered there fails its launch, named in list_node_pools' nodes step", ""},
		{"no zones, cache on, the one claim a zone's", read(zoneA), on(), a, "hf-cache-eu-central-1a", zoneA, "nodes pinned to eu-central-1a: the model cache (claim model-serving/hf-cache-eu-central-1a, volume pvc-a) lives there, and a node launched in another zone strands a predictor mounting it Pending; name zones on create to run the pool elsewhere — its slice then mounts that zone's claim (hf-cache-<zone>, created there and kept; the weights downloaded once more) —, or pick an accelerator offered in eu-central-1a — a family not offered there fails its launch, named in list_node_pools' nodes step", ""},
		{"no zones, cache on, a pending claim", read(pending), on(), nil, "hf-cache", pending, "the model cache claim model-serving/hf-cache on mc is Pending, bound to no volume yet: the pool's nodes are not pinned to a zone — the first predictor mounting the cache binds it to its node's zone, and every pool created after that without zones follows it", ""},
		{"no zones, cache on, a volume without a zone", read(noZone), on(), nil, "hf-cache", noZone, "the model cache claim model-serving/hf-cache on mc is bound to volume pvc-1, whose node affinity names no zone: the pool's nodes are not pinned — a volume every zone reaches strands no predictor", ""},
		{"no zones, cache on, an unreadable claim", read(unreadable), on(), nil, "hf-cache", unreadable, "", "the model cache claim model-serving/hf-cache on mc cannot be read as you (forbidden): the pool's nodes are not pinned to the cache's zone — the cache is one volume, bound in one zone, and a node launched in another zone strands a predictor mounting it Pending; re-run once you may read the claim and its volume, or name zones on create so the slice mounts that zone's claim"},
		{"no zones, cache on, the claims not listable", unlisted, on(), nil, "hf-cache", nil, "", "the model cache claims of model-serving on mc cannot be read as you (forbidden): the pool's nodes are not pinned to a cache's zone, and the slice mounts model-serving/hf-cache — a cache claim is one volume, bound in one zone, and a node launched in another zone strands a predictor mounting it Pending; re-run once you may list the claims and their volumes, or name zones on create so the slice mounts that zone's claim"},
		{"no zones, cache on, no claim", read(), on(), nil, "hf-cache", nil, "", ""},
		{"two zones, cache on, no claim: the base claim, the zones stand", read(), on(ab...), ab, "hf-cache", nil, "nodes pinned to eu-central-1a, eu-central-1b, the zones named on create; the slice mounts the model cache claim model-serving/hf-cache, which does not exist yet: the connectivity chart creates it and keeps it, and the first predictor binds it to a volume in its node's zone, one of eu-central-1a, eu-central-1b, and from then on every predictor mounting it runs there — the pool follows its cache: a re-run of this create_node_pool pins the pool's nodes to that zone, and a later pool naming that zone alone reuses the claim", ""},
		{"two zones, cache on, the base claim Pending", read(pending), on(ab...), ab, "hf-cache", pending, "nodes pinned to eu-central-1a, eu-central-1b, the zones named on create; the slice mounts the model cache claim model-serving/hf-cache, Pending and bound to no volume yet — the first predictor binds it to a volume in its node's zone, one of eu-central-1a, eu-central-1b, and from then on every predictor mounting it runs there — the pool follows its cache: a re-run of this create_node_pool pins the pool's nodes to that zone, and a later pool naming that zone alone reuses the claim", ""},
		{"two zones, cache on, the base claim's volume without a zone", read(noZone), on(ab...), ab, "hf-cache", noZone, "nodes pinned to eu-central-1a, eu-central-1b, the zones named on create; the slice mounts the model cache claim model-serving/hf-cache, bound to volume pvc-1, whose node affinity names no zone — a volume every zone reaches strands no predictor", ""},
		{"two zones, cache on, the base claim unreadable", read(unreadable), on(ab...), ab, "hf-cache", unreadable, "nodes pinned to eu-central-1a, eu-central-1b, the zones named on create; the slice mounts the model cache claim model-serving/hf-cache", "the model cache claim model-serving/hf-cache on mc cannot be read as you (forbidden): whether its volume lies in one of eu-central-1a, eu-central-1b cannot be told — the cache is one volume, bound in one zone, and a node launched in another zone strands a predictor mounting it Pending; re-run once you may read the claim and its volume, or pass cache false so this pool's slice serves without it"},
		{"two zones, cache on, the claims not listable", unlisted, on(ab...), ab, "hf-cache", nil, "nodes pinned to eu-central-1a, eu-central-1b, the zones named on create; the slice mounts the model cache claim model-serving/hf-cache", "the model cache claims of model-serving on mc cannot be read as you (forbidden): whether a claim is Bound in one of eu-central-1a, eu-central-1b, and which, cannot be told — the slice mounts model-serving/hf-cache, which the connectivity chart creates where it does not exist; a claim Bound outside the zones named strands every predictor mounting it Pending; re-run once you may list the claims, or pass cache false so this pool's slice serves without one"},
		{"two zones, cache on, the base claim Bound in one of them: the pool follows it", read(bound), on(ab...), b, "hf-cache", bound, "nodes pinned to eu-central-1b, of eu-central-1a, eu-central-1b named on create: the model cache claim model-serving/hf-cache (volume pvc-1) is Bound there, and a predictor mounting it runs nowhere else — the pool follows its cache, and a node in eu-central-1a would only strand a predictor Pending or hold a placeholder it cannot use; pass cache false to run across eu-central-1a, eu-central-1b without the cache, or name one of eu-central-1a alone to serve from it with its own claim (hf-cache-<zone>, created there and kept)", ""},
		{"two zones, cache on, a zone's claim Bound in one of them, another's elsewhere", read(zoneA, zoneB), on("eu-central-1a", "eu-central-1c"), a, "hf-cache-eu-central-1a", zoneA, "nodes pinned to eu-central-1a, of eu-central-1a, eu-central-1c named on create: the model cache claim model-serving/hf-cache-eu-central-1a (volume pvc-a) is Bound there, and a predictor mounting it runs nowhere else — the pool follows its cache, and a node in eu-central-1c would only strand a predictor Pending or hold a placeholder it cannot use; pass cache false to run across eu-central-1a, eu-central-1c without the cache, or name one of eu-central-1c alone to serve from it with its own claim (hf-cache-<zone>, created there and kept); the other claims — model-serving/hf-cache-eu-central-1b (Bound in eu-central-1b) — are left as they are, each its zone's", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pin, err := zonePinFor(tc.read, "mc", tc.choice)
			require.NoError(t, err)
			assert.Equal(t, tc.zones, pin.zones)
			assert.Equal(t, tc.claimName, pin.claimName, "the claim the slice mounts")
			assert.Equal(t, tc.claim, pin.claim, "the claim as read travels with the pin")
			assert.Equal(t, tc.note, pin.note)
			assert.Equal(t, tc.warning, pin.warning)
			assert.Equal(t, tc.choice.cache, pin.sliceCache().on)
		})
	}

	_, err := zonePinFor(read(bound, zoneA), "mc", on(ab...))
	require.Error(t, err, "several zones with claims Bound in several of them")
	refused := refusedBlock(t, err)
	assert.Nil(t, refused.CacheZone)
	assert.Equal(t, []*detect.CacheClaim{bound, zoneA}, refused.CacheClaims.Claims)
	assert.Equal(t, []string{"name one zone — the slice then mounts that zone's model cache claim (the one Bound there, else hf-cache-<zone>, which the connectivity chart creates and keeps)", "pass cache false, so this pool's slice serves without the cache across the zones — the weights land in each predictor pod's ephemeral storage, and the claims are left as they are"}, refused.CacheClaims.Remedies)

	ac := []string{"eu-central-1a", "eu-central-1c"}
	_, err = zonePinFor(read(bound), "mc", on(ac...))
	require.Error(t, err, "several zones with the base claim Bound outside them")
	refused = refusedBlock(t, err)
	assert.Equal(t, bound, refused.CacheZone.Claim)
	assert.Equal(t, "eu-central-1b", refused.CacheZone.ClaimZone)
	assert.Equal(t, ac, refused.CacheZone.Zones)
	assert.Len(t, refused.CacheZone.Remedies, 3)

	_, err = zonePinFor(read(bound, zoneA), "mc", on())
	require.Error(t, err, "several claims without zones")
	assert.Equal(t, []*detect.CacheClaim{bound, zoneA}, refusedBlock(t, err).CacheClaims.Claims)

	elsewhere := claim("hf-cache-eu-central-1a", "Bound", "pvc-x", "eu-central-1b", "")
	_, err = zonePinFor(read(elsewhere), "mc", on(a...))
	require.Error(t, err, "the zone's claim Bound elsewhere")
	assertRefused(t, err, "zones eu-central-1a: the model cache claim model-serving/hf-cache-eu-central-1a on mc, the zone's by name, is bound to volume pvc-x in eu-central-1b, outside it — a pool node launched in eu-central-1a cannot mount the cache, and every predictor mounting it sits Pending (`didn't match PersistentVolume's node affinity`); name eu-central-1b as the zone, or pass cache false, so this pool's slice serves without the cache — the weights land in the predictor pod's ephemeral storage and the claim is left as it is, or remove the claim (it costs the cached weights and compiled graphs)")
	refused = refusedBlock(t, err)
	assert.Equal(t, "eu-central-1b", refused.CacheZone.ClaimZone)
	assert.Equal(t, elsewhere, refused.CacheZone.Claim)
	assert.Len(t, refused.CacheZone.Remedies, 3)
}

// TestCreateNodePoolWithoutCache (giantswarm/cluster-manager#65): cache false
// composes the slice with modelServing.cache.enabled false, pins no zone from
// the Bound claim — the zones named stand, or nothing —, answers cache and
// leaves the claim alone; landed, a re-run with the cache on is the slice's
// upgrade naming the path; refused where the platform provides serving.
func TestCreateNodePoolWithoutCache(t *testing.T) {
	ctx := context.Background()
	l := newLab(t, "installation.yaml")
	l.add(t, l.installation, "prewarm.yaml") // the own cluster's values and join token
	l.add(t, l.installation, "cache-claim.yaml")
	svc := l.service(Config{Installation: "gazelle"})
	off := false
	in := l4("gazelle", "gpu-l40s", true)
	in.Pool.Accelerator, in.Pool.Sizes, in.Pool.MaxGPUs, in.Pool.Zones, in.Cache = "nvidia-l40s", []string{"2xlarge", "4xlarge"}, 1, []string{"eu-central-1a"}, &off
	out, err := svc.CreateNodePool(ctx, in)
	require.NoError(t, err, "the claim's zone does not bind a pool whose slice mounts no claim")
	assert.Equal(t, []string{"eu-central-1a"}, out.Zones)
	assert.Equal(t, "nodes pinned to eu-central-1a, the zones named on create; this pool's slice serves without the model cache (cache false): no predictor mounts the claim model-serving/hf-cache on gazelle, so its zone pins nothing and the claim is left as it is", out.ZonesNote)
	require.NotNil(t, out.Cache)
	assert.False(t, out.Cache.Enabled)
	assert.Empty(t, out.Cache.Claim)
	assert.Equal(t, "modelServing.cache.enabled false on the slice release: no claim is applied or mounted, every predictor downloads its weights into its pod's ephemeral storage (the node's local disk) at each start, and no zone pin follows from a claim; a re-run with cache true is the slice's upgrade back to the cache; the existing claim model-serving/hf-cache (Bound, volume pvc-6e577f13-ff22-461c-a453-cfbcdd2d7c80 in eu-central-1b) is left as it is — Helm keeps it, 500 GiB gp3 at 500 MiB/s, about $65.45 a month at list prices (AWS EBS gp3 list price, EU (Frankfurt) (eu-central-1), as of "+compose.PriceAsOf+"), billed while the claim exists until the cache is removed with remove_model_cache — and pins nothing", out.Cache.Note, "the standing cost is named (giantswarm/cluster-manager#83)")
	assert.Empty(t, out.Cache.MonthlyPriceUSD, "no figure for a slice without the cache")
	assert.Nil(t, out.CacheClaim, "no claim is mounted")
	require.Len(t, out.CacheClaims, 1, "the claims as read stay in the answer")
	assert.Equal(t, "eu-central-1b", out.CacheClaims[0].Zone)
	enabled, found, _ := unstructured.NestedBool(poolManifest(t, out, "gazelle-agent-platform"), "spec", "values", "modelServing", "cache", "enabled")
	require.True(t, found, "the slice release switches the cache off")
	assert.False(t, enabled)
	assertGolden(t, "create_node_pool_no_cache", out)

	// Without zones nothing pins: the claim's zone is not mounted.
	in.Pool.Zones = nil
	out, err = svc.CreateNodePool(ctx, in)
	require.NoError(t, err)
	assert.Nil(t, out.Zones)
	assert.Equal(t, "the pool's nodes are not pinned to a zone; this pool's slice serves without the model cache (cache false): no predictor mounts the claim model-serving/hf-cache on gazelle, so its zone pins nothing and the claim is left as it is", out.ZonesNote)
	_, found, _ = unstructured.NestedSlice(poolManifest(t, out, "gazelle-gpu-l40s"), "spec", "values", "pool", "zones")
	assert.False(t, found, "no zones block")

	// Landed with the cache off, a re-run without cache keeps it off — the
	// slice's setting stands (giantswarm/cluster-manager#86) — and one asking
	// for it is the slice's upgrade.
	in.DryRun = false
	_, err = svc.CreateNodePool(ctx, in)
	require.NoError(t, err)
	in.Cache, in.DryRun = nil, true
	still, err := svc.CreateNodePool(ctx, in)
	require.NoError(t, err)
	assert.False(t, still.Cache.Enabled, "nothing asked for the cache: the slice keeps serving without it")
	in.Cache = cacheOn()
	again, err := svc.CreateNodePool(ctx, in)
	require.NoError(t, err)
	assert.True(t, again.Cache.Enabled)
	assert.Equal(t, "model-serving/hf-cache", again.Cache.Claim)
	assert.True(t, strings.HasPrefix(again.Cache.Note, "the model cache is switched on for every pool of gazelle — the slice release is the cluster's one: "), again.Cache.Note)
	assert.Equal(t, []string{"eu-central-1b"}, again.Zones, "the cache on again: the claim's zone pins the pool")
	for _, o := range again.Objects {
		if o.Kind == "HelmRelease" && o.Name == "gazelle-agent-platform" {
			assert.Equal(t, "would-update", o.Action)
			assert.Equal(t, []string{"spec.values.modelServing.cache.enabled"}, o.Changes, "the upgrade back to the cache")
		}
	}

	// Serving the platform's release provides (wc2): the setting is not this pool's.
	wc2 := l4("wc2", "gpu-l4b", true)
	wc2.Cache = &off
	_, err = svc.CreateNodePool(ctx, wc2)
	assertRefused(t, err, "cache false: serving on wc2 is provided by the platform's own release")
}

// TestCreateNodePoolCacheClaimWithoutAZone (giantswarm/cluster-manager#59): a
// claim that is not Bound, or whose volume names no zone, pins nothing and
// the answer says what was found; a volume that cannot be read as the
// caller, or the claims that cannot be listed, pin nothing and are a warning
// naming why — never a guessed zone, never a silent absence.
func TestCreateNodePoolCacheClaimWithoutAZone(t *testing.T) {
	ctx := context.Background()
	forbidden := func(verb, resource, name string) k8stesting.ReactionFunc {
		return func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: resource}, name, errors.New("User \"alice\" cannot "+verb+" resource \""+resource+"\" in API group \"\""))
		}
	}
	cases := []struct {
		name    string
		prepare func(t *testing.T, l *lab)
		note    string
		warning string
		// unlisted: the claims themselves could not be read, so none is in the answer.
		unlisted bool
	}{
		{"pending claim", func(t *testing.T, l *lab) {
			pvc, err := l.installation.Resource(detect.PersistentVolumeClaimGVR).Namespace("model-serving").Get(ctx, "hf-cache", metav1.GetOptions{})
			require.NoError(t, err)
			unstructured.RemoveNestedField(pvc.Object, "spec", "volumeName")
			require.NoError(t, unstructured.SetNestedField(pvc.Object, "Pending", "status", "phase"))
			_, err = l.installation.Resource(detect.PersistentVolumeClaimGVR).Namespace("model-serving").Update(ctx, pvc, metav1.UpdateOptions{})
			require.NoError(t, err)
		}, "the model cache claim model-serving/hf-cache on gazelle is Pending, bound to no volume yet: the pool's nodes are not pinned to a zone — the first predictor mounting the cache binds it to its node's zone, and every pool created after that without zones follows it", "", false},
		{"volume without a zone", func(t *testing.T, l *lab) {
			pv, err := l.installation.Resource(detect.PersistentVolumeGVR).Get(ctx, "pvc-6e577f13-ff22-461c-a453-cfbcdd2d7c80", metav1.GetOptions{})
			require.NoError(t, err)
			unstructured.RemoveNestedField(pv.Object, "spec", "nodeAffinity")
			_, err = l.installation.Resource(detect.PersistentVolumeGVR).Update(ctx, pv, metav1.UpdateOptions{})
			require.NoError(t, err)
		}, "the model cache claim model-serving/hf-cache on gazelle is bound to volume pvc-6e577f13-ff22-461c-a453-cfbcdd2d7c80, whose node affinity names no zone: the pool's nodes are not pinned — a volume every zone reaches strands no predictor", "", false},
		{"volume not readable", func(t *testing.T, l *lab) {
			l.installation.(*dynamicfake.FakeDynamicClient).PrependReactor("get", "persistentvolumes", forbidden("get", "persistentvolumes", "pvc-6e577f13-ff22-461c-a453-cfbcdd2d7c80"))
		}, "", "the model cache claim model-serving/hf-cache on gazelle cannot be read as you (get PersistentVolume pvc-6e577f13-ff22-461c-a453-cfbcdd2d7c80 of claim model-serving/hf-cache: persistentvolumes \"pvc-6e577f13-ff22-461c-a453-cfbcdd2d7c80\" is forbidden: User \"alice\" cannot get resource \"persistentvolumes\" in API group \"\"): the pool's nodes are not pinned to the cache's zone — the cache is one volume, bound in one zone, and a node launched in another zone strands a predictor mounting it Pending; re-run once you may read the claim and its volume, or name zones on create so the slice mounts that zone's claim", false},
		{"claims not listable", func(t *testing.T, l *lab) {
			l.installation.(*dynamicfake.FakeDynamicClient).PrependReactor("list", "persistentvolumeclaims", forbidden("list", "persistentvolumeclaims", ""))
		}, "", "the model cache claims of model-serving on gazelle cannot be read as you (list PersistentVolumeClaims in model-serving: persistentvolumeclaims is forbidden: User \"alice\" cannot list resource \"persistentvolumeclaims\" in API group \"\"): the pool's nodes are not pinned to a cache's zone, and the slice mounts model-serving/hf-cache", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newLab(t, "installation.yaml")
			l.add(t, l.installation, "prewarm.yaml")
			l.add(t, l.installation, "cache-claim.yaml")
			tc.prepare(t, l)
			svc := l.service(Config{Installation: "gazelle"})
			in := l4("gazelle", "gpu-l40s", true)
			in.Pool.Accelerator, in.Pool.Prewarm = "nvidia-l40s", true
			out, err := svc.CreateNodePool(ctx, in)
			require.NoError(t, err)
			assert.Nil(t, out.Zones, "no pin")
			_, found, _ := unstructured.NestedSlice(poolManifest(t, out, "gazelle-gpu-l40s"), "spec", "values", "pool", "zones")
			assert.False(t, found, "no zones block")
			assert.Equal(t, "model-serving/hf-cache", out.Cache.Claim, "the slice mounts the one claim of the namespace, or the base name")
			if tc.unlisted {
				assert.Nil(t, out.CacheClaim, "nothing was read")
				assert.Nil(t, out.CacheClaims, "null, not empty: the claims could not be read")
			} else {
				require.NotNil(t, out.CacheClaim, "the claim as read is in the answer")
			}
			if tc.note != "" {
				assert.Equal(t, tc.note, out.ZonesNote)
				assert.Empty(t, out.Warnings)
			}
			if tc.warning != "" {
				assert.Empty(t, out.ZonesNote)
				require.Len(t, out.Warnings, 1)
				assert.Contains(t, out.Warnings[0], tc.warning)
				if !tc.unlisted {
					assert.NotEmpty(t, out.CacheClaim.Error)
				}
			}
		})
	}
}

// poolManifest is the pool's HelmRelease among a create's manifests.
func poolManifest(t *testing.T, out *WriteResult, name string) map[string]any {
	t.Helper()
	for _, m := range out.Manifests {
		if m["kind"] == "HelmRelease" && m["metadata"].(map[string]any)["name"] == name {
			return m
		}
	}
	t.Fatalf("no HelmRelease %s among the manifests", name)
	return nil
}

// TestDeleteNodePoolRefusesWhileAPredictorWaitsForANode
// (giantswarm/cluster-manager#59): the pool's nodes are idle, yet a model
// model-manager serves has its predictor Pending on no node — waiting for a
// node of the pool. The delete is refused naming the model and its pod, the
// idle nodes beside; the structured refusal lists it under unscheduled with
// no busy node. A Pending predictor pod of the classic serving path
// (giantswarm/agent-platform#574) is nobody's and does not count. Without a
// pod yet the model counts still.
// Pods not readable as the caller are a refusal: what cannot be seen cannot
// be judged idle. Force deletes regardless.
func TestDeleteNodePoolRefusesWhileAPredictorWaitsForANode(t *testing.T) {
	ctx := context.Background()
	del := DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply}
	lab := newLab(t, "installation.yaml").target(t, wc1APIServer, "wc1-waiting.yaml")
	svc := lab.service(Config{Installation: "gazelle"})

	_, err := svc.DeleteNodePool(ctx, del)
	assertRefused(t, err, "node pool wc1-gpu-a10g runs no busy node on wc1, but 1 served model(s) wait for a node of the pool: LLMInferenceService model-serving/llama-3-8b (meta-llama/Llama-3.1-8B-Instruct): pod model-serving/llama-3-8b-kserve-6649fb66c8-xnkd2 Pending on no node — removing the pool strands them (with the cluster's last pool the serving slice, its controller and the backend go too, and the serving object is left behind with a finalizer nothing clears) — unload them first (model-manager's unload_model) and re-run, or pass force to delete the pool regardless; 2 idle node(s) (wc1-gpu-a10g-node-1, wc1-gpu-a10g-node-2) go with the pool once the models are unloaded")
	assert.Equal(t, &Refused{
		Nodes:       []string{},
		Idle:        []string{"wc1-gpu-a10g-node-1", "wc1-gpu-a10g-node-2"},
		Models:      []string{"LLMInferenceService model-serving/llama-3-8b (meta-llama/Llama-3.1-8B-Instruct)"},
		Unscheduled: []string{"LLMInferenceService model-serving/llama-3-8b (meta-llama/Llama-3.1-8B-Instruct)"},
		Hint:        refusedHint,
		ReadFrom:    readFromCluster,
	}, refusedBlock(t, err), "no busy node; the classic path's Pending predictor pod is nobody's and neither counts nor refuses")
	assert.Len(t, poolClaims(t, lab, "wc1-gpu-a10g"), 2, "nothing was written")

	// The controller has not created the predictor's pod yet: the model
	// counts all the same.
	require.NoError(t, lab.targets[wc1APIServer].Resource(detect.PodsGVR).Namespace("model-serving").Delete(ctx, "llama-3-8b-kserve-6649fb66c8-xnkd2", metav1.DeleteOptions{}))
	_, err = svc.DeleteNodePool(ctx, del)
	assertRefused(t, err, "LLMInferenceService model-serving/llama-3-8b (meta-llama/Llama-3.1-8B-Instruct): no predictor pod yet")

	// Pods not readable: refused, with the way out.
	fakeTarget(t, lab, wc1APIServer).PrependReactor("list", detect.PodsGVR.Resource, func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() != "model-serving" {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("User \"alice\" cannot list resource \"pods\" in API group \"\" in the namespace \"model-serving\""))
	})
	_, err = svc.DeleteNodePool(ctx, del)
	assertRefused(t, err, "node pool wc1-gpu-a10g runs no busy node on wc1, but whether a served model waits for one cannot be told (list the pods of model-serving: pods is forbidden: User \"alice\" cannot list resource \"pods\" in API group \"\" in the namespace \"model-serving\"): a predictor Pending for a node of the pool would be stranded by the delete — re-run once you may list the pods of model-serving, or pass force to delete the pool regardless")

	forced, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply, Force: true, DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"would-delete OCIRepository org-acme/wc1-gpu-a10g", "would-delete HelmRelease org-acme/wc1-gpu-a10g"}, objectNames(forced), "force judges no model")
}
