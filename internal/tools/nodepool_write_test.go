package tools

import (
	"context"
	"errors"
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
	assert.Equal(t, "flatcar-stable-4459.2.1-kube-1.31.4-tooling-1.26.1-gs", out.MachineImage, "cluster-aws's image name from the release's components")
	assert.Equal(t, compose.DefaultPoolChartVersion, out.ChartVersion)
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

// TestCreateNodePoolAppLayoutNoTeleport reads the snapshot from an App CR's
// user-values ConfigMap; wc2 has a proxy and no teleport Secret.
func TestCreateNodePoolAppLayoutNoTeleport(t *testing.T) {
	svc := newLab(t, "installation.yaml").service(Config{Installation: "gazelle"})
	out, err := svc.CreateNodePool(context.Background(), l4("wc2", "gpu-l4b", true))
	require.NoError(t, err)
	values, _, _ := unstructured.NestedMap(out.Manifests[1], "spec", "values")
	assert.Equal(t, map[string]any{"enabled": false}, values["teleport"])
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
	assertRefused(t, err, "node pool wc1-gpu-a10g still runs 1 busy node(s) on wc1: node wc1-gpu-a10g-node-1 runs model-serving/llama-3-8b-kserve-6649fb66c8-dllt7 (1 GPU), serving 2 model(s) on wc1: InferenceService model-serving/mistral-7b (mistralai/Mistral-7B-Instruct-v0.3), LLMInferenceService model-serving/llama-3-8b (meta-llama/Llama-3.1-8B-Instruct) — unload them first (model-manager's unload_model, or the cluster's Serving group) and re-run once the pool is empty, or pass force to delete the pool with its nodes and the models on them; 1 idle node(s) (wc1-gpu-a10g-node-2) go with the pool once the busy ones are free")
	assert.Equal(t, &Refused{
		Nodes:    []string{"wc1-gpu-a10g-node-1"},
		Idle:     []string{"wc1-gpu-a10g-node-2"},
		Models:   []string{"InferenceService model-serving/mistral-7b (mistralai/Mistral-7B-Instruct-v0.3)", "LLMInferenceService model-serving/llama-3-8b (meta-llama/Llama-3.1-8B-Instruct)"},
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
	assert.Equal(t, compose.InstanceShape{InstanceType: "g6.xlarge", Size: "xlarge", VCPU: 4, MemoryGiB: 16, GPUs: 1, GPUMemoryGiB: 24, UsableVCPU: 3, UsableMemoryGiB: 11.9,
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
// slice's agent-platform release (4.27.2, the fixture's platform) resolves
// for its range, 4.28.0 in the fake registry, not the meta chart's own
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
	assert.Equal(t, `3 preset(s) shipped by agent-platform-connectivity 4.28.0, the chart the slice's agent-platform 4.27.2 release resolves for ">=4.0.0 <5.0.0" at gsoci.azurecr.io — the slice publishes them once it is ready`, out.PresetFit.Source)
	assert.Empty(t, out.PresetFit.Note)
	require.Len(t, out.PresetFit.Presets, 3)
	assert.Equal(t, PresetSizeFit{Preset: "qwen3-14b", DisplayName: "Qwen3 14B", Model: "Qwen/Qwen3-14B", CPU: "4", Memory: "48Gi", GPUs: 1, GPUMemoryGiB: 58,
		Reason: "needs 58 GiB of GPU memory across 1 GPU(s); a g6 GPU has 24 GiB"}, out.PresetFit.Presets[0])
	assert.Equal(t, PresetSizeFit{Preset: "qwen3-8b-fp8", DisplayName: "Qwen3 8B FP8", Model: "Qwen/Qwen3-8B-FP8", CPU: "2", Memory: "10Gi", GPUs: 1, GPUMemoryGiB: 21, Size: "xlarge"}, out.PresetFit.Presets[1])
	assert.Equal(t, PresetSizeFit{Preset: "wide-l4", DisplayName: "Wide L4 recipe", Model: "acme/wide-9b", CPU: "4", Memory: "16Gi", GPUs: 1, GPUMemoryGiB: 20, Size: "2xlarge"}, out.PresetFit.Presets[2])
	assert.Empty(t, out.Warnings, "every hostable preset has a size in the default pool")
	require.NotNil(t, out.Sizes[0].PricePerHourUSD)
	assert.InDelta(t, 1.0064, *out.Sizes[0].PricePerHourUSD, 1e-9, "g6.xlarge in Frankfurt, beside the fit")
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
	assert.Equal(t, "no serving preset is published on wc1 yet — the slice release publishes them once it is ready, and the presets it would publish could not be read from the registry (pull oci://gsoci.azurecr.io/charts/giantswarm/agent-platform 4.27.2: HTTP 404): whether the pool's sizes host them is not judged", out.PresetFit.Note)
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
	assert.Equal(t, "nodes pinned to eu-central-1b: the model cache (claim model-serving/hf-cache, volume pvc-6e577f13-ff22-461c-a453-cfbcdd2d7c80) lives there, and a node launched in another zone strands a predictor mounting it Pending; remove the cache claim to lift the pin (it costs the cached weights and compiled graphs), or pick an accelerator offered in eu-central-1b — a family not offered there fails its launch, named in list_node_pools' nodes step", out.ZonesNote)
	assert.Equal(t, &detect.CacheClaim{Namespace: "model-serving", Name: "hf-cache", Phase: "Bound", Volume: "pvc-6e577f13-ff22-461c-a453-cfbcdd2d7c80", Zone: "eu-central-1b"}, out.CacheClaim)
	assert.Empty(t, out.Warnings, "a pin is not a warning")
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
	_, found, _ = unstructured.NestedSlice(poolManifest(t, out, "wc2-gpu-l4b"), "spec", "values", "pool", "zones")
	assert.False(t, found, "without a claim the values carry no zones block")
}

// TestCreateNodePoolCacheClaimWithoutAZone (giantswarm/cluster-manager#59): a
// claim that is not Bound, or whose volume names no zone, pins nothing and
// the answer says what was found; a claim or volume that cannot be read as
// the caller pins nothing and is a warning naming why — never a guessed
// zone, never a silent absence.
func TestCreateNodePoolCacheClaimWithoutAZone(t *testing.T) {
	ctx := context.Background()
	forbidden := func(resource, name string) k8stesting.ReactionFunc {
		return func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: resource}, name, errors.New("User \"alice\" cannot get resource \""+resource+"\" in API group \"\""))
		}
	}
	cases := []struct {
		name    string
		prepare func(t *testing.T, l *lab)
		note    string
		warning string
	}{
		{"pending claim", func(t *testing.T, l *lab) {
			pvc, err := l.installation.Resource(detect.PersistentVolumeClaimGVR).Namespace("model-serving").Get(ctx, "hf-cache", metav1.GetOptions{})
			require.NoError(t, err)
			unstructured.RemoveNestedField(pvc.Object, "spec", "volumeName")
			require.NoError(t, unstructured.SetNestedField(pvc.Object, "Pending", "status", "phase"))
			_, err = l.installation.Resource(detect.PersistentVolumeClaimGVR).Namespace("model-serving").Update(ctx, pvc, metav1.UpdateOptions{})
			require.NoError(t, err)
		}, "the model cache claim model-serving/hf-cache on gazelle is Pending, bound to no volume yet: the pool's nodes are not pinned to a zone — the first predictor mounting the cache binds it to its node's zone, and every pool created after that follows it", ""},
		{"volume without a zone", func(t *testing.T, l *lab) {
			pv, err := l.installation.Resource(detect.PersistentVolumeGVR).Get(ctx, "pvc-6e577f13-ff22-461c-a453-cfbcdd2d7c80", metav1.GetOptions{})
			require.NoError(t, err)
			unstructured.RemoveNestedField(pv.Object, "spec", "nodeAffinity")
			_, err = l.installation.Resource(detect.PersistentVolumeGVR).Update(ctx, pv, metav1.UpdateOptions{})
			require.NoError(t, err)
		}, "the model cache claim model-serving/hf-cache on gazelle is bound to volume pvc-6e577f13-ff22-461c-a453-cfbcdd2d7c80, whose node affinity names no zone: the pool's nodes are not pinned — a volume every zone reaches strands no predictor", ""},
		{"volume not readable", func(t *testing.T, l *lab) {
			l.installation.(*dynamicfake.FakeDynamicClient).PrependReactor("get", "persistentvolumes", forbidden("persistentvolumes", "pvc-6e577f13-ff22-461c-a453-cfbcdd2d7c80"))
		}, "", "the model cache claim model-serving/hf-cache on gazelle cannot be read as you (get PersistentVolume pvc-6e577f13-ff22-461c-a453-cfbcdd2d7c80 of claim model-serving/hf-cache: persistentvolumes \"pvc-6e577f13-ff22-461c-a453-cfbcdd2d7c80\" is forbidden: User \"alice\" cannot get resource \"persistentvolumes\" in API group \"\"): the pool's nodes are not pinned to the cache's zone — the cache is one volume, bound in one zone, and a node launched in another zone strands a predictor mounting it Pending; re-run once you may read the claim and its volume, or remove the claim (it costs the cached weights)"},
		{"claim not readable", func(t *testing.T, l *lab) {
			l.installation.(*dynamicfake.FakeDynamicClient).PrependReactor("get", "persistentvolumeclaims", forbidden("persistentvolumeclaims", "hf-cache"))
		}, "", "the model cache claim model-serving/hf-cache on gazelle cannot be read as you (get PersistentVolumeClaim model-serving/hf-cache: persistentvolumeclaims \"hf-cache\" is forbidden: User \"alice\" cannot get resource \"persistentvolumeclaims\" in API group \"\"): the pool's nodes are not pinned to the cache's zone"},
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
			require.NotNil(t, out.CacheClaim, "the claim as read is in the answer")
			if tc.note != "" {
				assert.Equal(t, tc.note, out.ZonesNote)
				assert.Empty(t, out.Warnings)
			}
			if tc.warning != "" {
				assert.Empty(t, out.ZonesNote)
				require.Len(t, out.Warnings, 1)
				assert.Contains(t, out.Warnings[0], tc.warning)
				assert.NotEmpty(t, out.CacheClaim.Error)
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
// no busy node. A hand-made InferenceService Pending too is not the
// platform's and does not count. Without a pod yet the model counts still.
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
		Models:      []string{"InferenceService model-serving/mistral-7b (mistralai/Mistral-7B-Instruct-v0.3)", "LLMInferenceService model-serving/llama-3-8b (meta-llama/Llama-3.1-8B-Instruct)"},
		Unscheduled: []string{"LLMInferenceService model-serving/llama-3-8b (meta-llama/Llama-3.1-8B-Instruct)"},
		Hint:        refusedHint,
		ReadFrom:    readFromCluster,
	}, refusedBlock(t, err), "no busy node; the hand-made InferenceService is listed among the served models but does not refuse")
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
