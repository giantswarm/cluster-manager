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

func serving(cluster string, dryRun bool) ModelServingInput {
	return ModelServingInput{Cluster: cluster, Mode: ModeApply, DryRun: dryRun, Cache: cacheOn()}
}

// TestEnableModelServingOwnCluster composes the slice beside the platform's
// release on the installation's own cluster: the platform's domain,
// identity and wildcard certificate, agentgateway off, the target knob with
// its own kubeconfig, the backend as the local target. No pool exists: no
// node selector.
func TestEnableModelServingOwnCluster(t *testing.T) {
	svc := newLab(t, "installation.yaml").service(Config{Installation: "gazelle"})
	out, err := svc.EnableModelServing(context.Background(), serving("gazelle", true))
	require.NoError(t, err)
	assert.Equal(t, []string{"would-create", "would-create", "would-create"}, actions(out), "slice source, release; backend")
	assert.Equal(t, &SliceRelease{Name: "gazelle-agent-platform", Namespace: "org-giantswarm", ChartVersion: "4.44.1", Domain: "gazelle.example.io", ModelsHost: "models.gazelle.example.io", JWKS: "http://dex.giantswarm.svc.cluster.local:5556/keys"}, out.Slice, "the dry run names the JWKS source: the platform's Dex service")
	assert.Equal(t, compose.BackendTargetLocal, out.Backend.Target)
	values, _, _ := unstructured.NestedMap(out.Manifests[1], "spec", "values")
	jwksHost, _, _ := unstructured.NestedString(values, "modelServing", "modelsGateway", "jwtAuthentication", "jwks", "host")
	assert.Equal(t, "dex.giantswarm.svc.cluster.local", jwksHost, "from the platform's gateway.jwksEgress, the source its own JWT policies use (giantswarm/cluster-manager#30)")
	jwksPort, _, _ := unstructured.NestedInt64(values, "modelServing", "modelsGateway", "jwtAuthentication", "jwks", "port")
	assert.Equal(t, int64(5556), jwksPort)
	agentgateway, _, _ := unstructured.NestedBool(values, "components", "agentgateway", "enabled")
	assert.False(t, agentgateway, "the platform's release owns the Gateway API data plane of its own cluster")
	target, _, _ := unstructured.NestedString(values, "gitops", "target", "kubeConfig", "secretRef", "name")
	assert.Equal(t, "gazelle-kubeconfig", target, "the target knob on the own cluster too: its children are kubeconfig-delivered, as the fleet's tenancy policy wants")
	sa, _, _ := unstructured.NestedString(out.Manifests[1], "spec", "serviceAccountName")
	assert.Equal(t, compose.DefaultTenantServiceAccount, sa, "the slice release runs as the org's tenant")
	tls, _, _ := unstructured.NestedString(values, "gatewayApi", "gateway", "tls", "secretName")
	assert.Equal(t, "gazelle-wildcard-tls", tls, "from the platform's inline values")
	issuer, _, _ := unstructured.NestedString(values, "global", "identity", "issuerUrl")
	assert.Equal(t, "https://dex.gazelle.example.io", issuer, "from the platform's valuesFrom ConfigMap")
	assert.NotContains(t, backendDoc(out), "gpuPool", "no pool: the document names no sizes")
	ingress, _, _ := unstructured.NestedStringSlice(values, "modelServing", "networkPolicy", "additionalIngressNamespaces")
	assert.Equal(t, []string{"agent-platform", compose.DefaultSubstrateNamespace}, ingress, "the namespace the platform's release is deployed in (status.history) and the chart's Substrate namespace: model-manager, the agentgateway data plane and the agents' egress reach the model pods (giantswarm/cluster-manager#103)")
	assertGolden(t, "enable_model_serving_own_cluster", out)
}

// TestEnableModelServingOwnClusterIngressNamespaces
// (giantswarm/cluster-manager#103): the namespaces the platform's release
// names win over the deployed namespace and the chart's default —
// gitops.targetNamespace for its workloads, components.substrate
// .targetNamespace for its Substrate.
func TestEnableModelServingOwnClusterIngressNamespaces(t *testing.T) {
	l := newLab(t, "installation.yaml")
	ctx := context.Background()
	platform, err := l.installation.Resource(HelmReleaseGVR).Namespace("flux-giantswarm").Get(ctx, "agent-platform", metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedField(platform.Object, "platform-workloads", "spec", "values", "gitops", "targetNamespace"))
	require.NoError(t, unstructured.SetNestedField(platform.Object, "substrate", "spec", "values", "components", "substrate", "targetNamespace"))
	_, err = l.installation.Resource(HelmReleaseGVR).Namespace("flux-giantswarm").Update(ctx, platform, metav1.UpdateOptions{})
	require.NoError(t, err)

	out, err := l.service(Config{Installation: "gazelle"}).EnableModelServing(ctx, serving("gazelle", true))
	require.NoError(t, err)
	values, _, _ := unstructured.NestedMap(out.Manifests[1], "spec", "values")
	ingress, _, _ := unstructured.NestedStringSlice(values, "modelServing", "networkPolicy", "additionalIngressNamespaces")
	assert.Equal(t, []string{"platform-workloads", "substrate"}, ingress)
}

// TestEnableModelServingWorkload composes the slice onto wc1 before any
// pool of this call: the one pool release wc1 has (gpu-a10g) pins the
// predictors, the target knob names the kubeconfig Secret, agentgateway is
// on; applied, the re-run is unchanged and list_clusters reports serving
// present through cluster-manager.
func TestEnableModelServingWorkload(t *testing.T) {
	l := newLab(t, "installation.yaml")
	svc := l.service(Config{Installation: "gazelle"})
	ctx := context.Background()
	dry, err := svc.EnableModelServing(ctx, serving("wc1", true))
	require.NoError(t, err)
	assert.Equal(t, "gpu-a10g", dry.Slice.GPUPool)
	assert.Contains(t, backendDoc(dry), "gpuPool:\n      instances:\n      - gpuMemoryGiB: 24\n        gpus: 1\n        instanceType: g5.xlarge\n", "the pinned pool's shapes, read from its release: nvidia-a10g in the chart's default sizes (giantswarm/cluster-manager#26)")
	assert.Contains(t, backendDoc(dry), "instanceType: g5.4xlarge\n")
	assert.Equal(t, "models.wc1.acme.example.io", dry.Slice.ModelsHost, "models.<cluster>.<base domain>")
	assert.Equal(t, "https://dex.gazelle.example.io/keys", dry.Slice.JWKS, "a workload cluster cannot reach the installation's Dex service: the public issuer")
	values, _, _ := unstructured.NestedMap(dry.Manifests[1], "spec", "values")
	_, hasJWKS, _ := unstructured.NestedMap(values, "modelServing", "modelsGateway", "jwtAuthentication")
	assert.False(t, hasJWKS, "the chart's default JWKS source, nothing composed")
	_, hasIngress, _ := unstructured.NestedSlice(values, "modelServing", "networkPolicy", "additionalIngressNamespaces")
	assert.False(t, hasIngress, "the platform's namespaces are the installation's: a workload cluster's model pods admit nothing more (giantswarm/cluster-manager#103)")
	target, _, _ := unstructured.NestedString(values, "gitops", "target", "kubeConfig", "secretRef", "name")
	assert.Equal(t, "wc1-kubeconfig", target)
	agentgateway, _, _ := unstructured.NestedBool(values, "components", "agentgateway", "enabled")
	assert.True(t, agentgateway, "a workload cluster runs no controller of its own")
	assertGolden(t, "enable_model_serving_workload", dry)

	out, err := svc.EnableModelServing(ctx, serving("wc1", false))
	require.NoError(t, err)
	assert.Equal(t, []string{"create", "create", "create"}, actions(out))
	again, err := svc.EnableModelServing(ctx, serving("wc1", false))
	require.NoError(t, err)
	assert.Equal(t, []string{"unchanged", "unchanged", "unchanged"}, actions(again), "idempotent")
	assert.Equal(t, detect.ProviderClusterManager, again.Serving.Provider)
	assert.Equal(t, detect.ProviderClusterManager, wc1Cluster(t, l).Serving.Provider, "list_clusters: serving present through cluster-manager")
}

// TestEnableModelServingWithoutCache (giantswarm/cluster-manager#65): cache
// false composes the slice with modelServing.cache.enabled false and the
// answer says so, the existing claim named as left alone; a re-run with the
// cache on writes nothing for it — the chart's default — and names the claim
// the predictors mount.
func TestEnableModelServingWithoutCache(t *testing.T) {
	l := newLab(t, "installation.yaml")
	l.add(t, l.installation, "cache-claim.yaml")
	svc := l.service(Config{Installation: "gazelle"})
	ctx := context.Background()
	off := false
	in := serving("gazelle", true)
	in.Cache = &off
	out, err := svc.EnableModelServing(ctx, in)
	require.NoError(t, err)
	values, _, _ := unstructured.NestedMap(out.Manifests[1], "spec", "values")
	enabled, found, _ := unstructured.NestedBool(values, "modelServing", "cache", "enabled")
	require.True(t, found)
	assert.False(t, enabled)
	require.NotNil(t, out.Cache)
	assert.False(t, out.Cache.Enabled)
	assert.Contains(t, out.Cache.Note, "the existing claim model-serving/hf-cache (Bound, volume pvc-6e577f13-ff22-461c-a453-cfbcdd2d7c80 in eu-central-1b) is left as it is — Helm keeps it, 500 GiB gp3 at 500 MiB/s, about $65.45 a month at list prices")

	on, err := svc.EnableModelServing(ctx, serving("gazelle", true))
	require.NoError(t, err)
	values, _, _ = unstructured.NestedMap(on.Manifests[1], "spec", "values")
	_, found, _ = unstructured.NestedBool(values, "modelServing", "cache", "enabled")
	assert.False(t, found, "the cache on is the chart's default: nothing written")
	assert.True(t, on.Cache.Enabled)
	assert.Equal(t, "model-serving/hf-cache", on.Cache.Claim)
}

// TestEnableModelServingKeepsTheZoneClaim (giantswarm/cluster-manager#71): a
// re-run of enable_model_serving on a slice release that mounts a zone's
// claim keeps that claim — it never moves the cache to another zone — and
// names it; with the cache off the claim name is not written, and the claims
// that exist are named as left alone.
func TestEnableModelServingKeepsTheZoneClaim(t *testing.T) {
	l := newLab(t, "installation.yaml")
	l.add(t, l.installation, "prewarm.yaml")
	l.add(t, l.installation, "cache-claim.yaml")
	svc := l.service(Config{Installation: "gazelle"})
	ctx := context.Background()
	in := l4("gazelle", "gpu-l40s", false)
	in.Pool.Accelerator, in.Pool.Sizes, in.Pool.MaxGPUs, in.Pool.Zones = "nvidia-l40s", []string{"2xlarge"}, 1, []string{"eu-central-1a"}
	_, err := svc.CreateNodePool(ctx, in)
	require.NoError(t, err, "the slice landed mounting the zone's claim")

	out, err := svc.EnableModelServing(ctx, serving("gazelle", true))
	require.NoError(t, err)
	name, found, _ := unstructured.NestedString(out.Manifests[1], "spec", "values", "modelServing", "cache", "pvc", "name")
	require.True(t, found, "the re-run keeps the claim the release mounts")
	assert.Equal(t, "hf-cache-eu-central-1a", name)
	assert.True(t, out.Cache.Enabled)
	assert.Equal(t, "model-serving/hf-cache-eu-central-1a", out.Cache.Claim)
	assert.Contains(t, out.Cache.Note, "it does not exist yet: the connectivity chart creates it")
	for _, o := range out.Objects {
		if o.Kind == "HelmRelease" && o.Name == "gazelle-agent-platform" {
			assert.NotContains(t, o.Changes, "spec.values.modelServing.cache.pvc.name", "nothing to change for the claim")
		}
	}

	// The cache is the cluster's setting: off while the release runs with it
	// on is refused, the way to serve without it being remove_model_cache
	// (giantswarm/cluster-manager#83).
	off := false
	in2 := serving("gazelle", true)
	in2.Cache = &off
	_, err = svc.EnableModelServing(ctx, in2)
	assertRefused(t, err, "cache false: the model cache is on for every pool of gazelle — the slice release org-giantswarm/gazelle-agent-platform is the cluster's one and mounts model-serving/hf-cache-eu-central-1a")
	var refused *ErrRefused
	require.True(t, errors.As(err, &refused))
	require.NotNil(t, refused.Refused.CacheOn)
	assert.Equal(t, "hf-cache-eu-central-1a", refused.Refused.CacheOn.ClaimName)
	assert.Nil(t, refused.Refused.CacheOn.Claim, "the zone's claim does not exist yet")
	assert.Len(t, refused.Refused.CacheOn.Remedies, 2)
}

// TestCreateNodePoolUpdatesTheSliceInPlace: with the slice on wc1 from
// enable_model_serving, a second pool makes create_node_pool update the one
// release — its dry-run names the changed paths (the selector goes, and the
// backend document's sizes with it: two pools, no pool pinned) — and never
// composes a second release of the chart.
func TestCreateNodePoolUpdatesTheSliceInPlace(t *testing.T) {
	l := newLab(t, "installation.yaml")
	svc := l.service(Config{Installation: "gazelle"})
	ctx := context.Background()
	_, err := svc.EnableModelServing(ctx, serving("wc1", false))
	require.NoError(t, err)

	drift, err := svc.CreateNodePool(ctx, l4("wc1", "gpu-l4", true))
	require.NoError(t, err)
	assert.Equal(t, []string{"would-create", "would-create", "would-create", "unchanged", "would-update", "would-update", "would-create", "would-create"}, actions(drift), "pool new; the slice's source stands, its release updates; the backend loses the pinned pool's sizes; operator new")
	assert.Equal(t, []string{"spec.values.modelServing.gpuPool.nodeSelector.giantswarm.io/machine-pool", "spec.values.modelServing.serving.nodeSelector.giantswarm.io/machine-pool"}, drift.Objects[4].Changes)
	assert.Equal(t, []string{"data.backend.yaml"}, drift.Objects[5].Changes, "two pools: no pool is pinned, the document names no sizes")
	assert.NotContains(t, backendDoc(drift), "gpuPool")
	assertGolden(t, "create_node_pool_updates_slice", drift)

	out, err := svc.CreateNodePool(ctx, l4("wc1", "gpu-l4", false))
	require.NoError(t, err)
	assert.Equal(t, "update", out.Objects[4].Action)
	assert.Equal(t, "update", out.Objects[5].Action)
	hrs, err := l.installation.Resource(HelmReleaseGVR).Namespace("org-acme").List(ctx, metav1.ListOptions{LabelSelector: compose.LabelChartName + "=" + compose.SliceChart})
	require.NoError(t, err)
	assert.Len(t, hrs.Items, 1, "one release of the chart per cluster")
}

// TestEnableModelServingRefusals: a chart-provided serving layer (wc2), a
// release of the chart under another name, an unreadable cluster. Nothing
// lands.
func TestEnableModelServingRefusals(t *testing.T) {
	l := newLab(t, "installation.yaml")
	svc := l.service(Config{Installation: "gazelle"})
	ctx := context.Background()

	_, err := svc.EnableModelServing(ctx, serving("wc2", false))
	assertRefused(t, err, "provided by the platform's own release")

	other := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease",
		"metadata": map[string]any{"name": "wc1-model-serving", "namespace": "org-acme", "labels": map[string]any{
			compose.LabelChartName: compose.SliceChart, compose.LabelCluster: "wc1", "kustomize.toolkit.fluxcd.io/name": "workload-clusters",
		}},
		"spec": map[string]any{"chartRef": map[string]any{"kind": "OCIRepository", "name": "wc1-model-serving"}},
	}}
	_, err = l.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Create(ctx, other, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = svc.EnableModelServing(ctx, serving("wc1", false))
	assertRefused(t, err, "already has a release of the agent-platform chart under another name (HelmRelease org-acme/wc1-model-serving, is owned by GitOps (Flux Kustomization workload-clusters))")
	assertNothingLanded(t, l, "wc1-agent-platform")
	require.NoError(t, l.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Delete(ctx, "wc1-model-serving", metav1.DeleteOptions{}))

	platform, err := l.installation.Resource(HelmReleaseGVR).Namespace("flux-giantswarm").Get(ctx, "agent-platform", metav1.GetOptions{})
	require.NoError(t, err)
	history, _, _ := unstructured.NestedSlice(platform.Object, "status", "history")
	require.NoError(t, unstructured.SetNestedSlice(platform.Object, []any{map[string]any{"chartVersion": "4.43.2"}}, "status", "history"))
	_, err = l.installation.Resource(HelmReleaseGVR).Namespace("flux-giantswarm").Update(ctx, platform, metav1.UpdateOptions{})
	require.NoError(t, err)
	_, err = svc.EnableModelServing(ctx, serving("wc1", false))
	assertRefused(t, err, "flux-giantswarm/agent-platform runs agent-platform chart 4.43.2, below 4.44.0")
	assertNothingLanded(t, l, "wc1-agent-platform")
	out, err := l.service(Config{Installation: "gazelle", SliceChartVersion: "4.30.0"}).EnableModelServing(ctx, serving("wc1", true))
	require.NoError(t, err)
	assert.Equal(t, "4.30.0", out.Slice.ChartVersion, "an explicit pin is honoured as given, the platform's version notwithstanding")
	require.NoError(t, unstructured.SetNestedSlice(platform.Object, history, "status", "history"))
	_, err = l.installation.Resource(HelmReleaseGVR).Namespace("flux-giantswarm").Update(ctx, platform, metav1.UpdateOptions{})
	require.NoError(t, err)

	l.unreachable(wc1APIServer)
	_, err = svc.EnableModelServing(ctx, serving("wc1", false))
	assertRefused(t, err, "cannot tell whether serving runs on wc1")
	assertNothingLanded(t, l, "wc1-agent-platform")
}

// TestDisableModelServing: refused while a model is served (named), plainly
// when the cluster is unreadable; force removes the slice and the backend;
// a second call finds nothing.
func TestDisableModelServing(t *testing.T) {
	l := newLab(t, "installation.yaml")
	svc := l.service(Config{Installation: "gazelle"})
	ctx := context.Background()
	_, err := svc.DisableModelServing(ctx, serving("wc1", false))
	var notFound *ErrNotFound
	require.ErrorAs(t, err, &notFound)

	_, err = svc.EnableModelServing(ctx, serving("wc1", false))
	require.NoError(t, err)
	l.target(t, wc1APIServer, "wc1-serving.yaml")
	_, err = svc.DisableModelServing(ctx, serving("wc1", false))
	assertRefused(t, err, "1 model(s) are served on wc1 (LLMInferenceService model-serving/llama-3-8b (meta-llama/Llama-3.1-8B-Instruct)): unload them first (model-manager's unload_model")

	l.unreachable(wc1APIServer)
	_, err = svc.DisableModelServing(ctx, serving("wc1", false))
	assertRefused(t, err, "cannot tell whether models are served on wc1")

	dry, err := svc.DisableModelServing(ctx, ModelServingInput{Cluster: "wc1", Mode: ModeApply, Force: true, DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"would-delete", "would-delete", "would-delete"}, actions(dry), "release, source, backend")
	assertGolden(t, "disable_model_serving_dry_run", dry)

	out, err := svc.DisableModelServing(ctx, ModelServingInput{Cluster: "wc1", Mode: ModeApply, Force: true})
	require.NoError(t, err)
	assert.Len(t, out.Objects, 3)
	_, err = svc.DisableModelServing(ctx, serving("wc1", false))
	require.ErrorAs(t, err, &notFound, "nothing left to remove")
}

// TestDeleteLastPoolKeepsASharedSlice: a slice release carrying another
// slice (the runtime slice's kagent on) stays when the last pool goes, and
// the backend registration with it.
func TestDeleteLastPoolKeepsASharedSlice(t *testing.T) {
	l := newLab(t, "installation.yaml")
	svc := l.service(Config{Installation: "gazelle"})
	ctx := context.Background()
	_, err := svc.EnableModelServing(ctx, serving("wc1", false))
	require.NoError(t, err)
	hr, err := l.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Get(ctx, "wc1-agent-platform", metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedField(hr.Object, true, "spec", "values", "components", "kagent", "enabled"))
	_, err = l.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Update(ctx, hr, metav1.UpdateOptions{})
	require.NoError(t, err)

	out, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply, Force: true, DryRun: true})
	require.NoError(t, err)
	assert.True(t, out.LastPool)
	assert.Equal(t, "org-acme/wc1-agent-platform", out.SliceKept)
	assert.Equal(t, []string{"would-delete", "would-delete"}, actions(out), "pool release and source (no operator of cluster-manager's on wc1 here); the slice and the backend stay")
}
