package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/detect"
)

func removeCache(cluster, claim string, dryRun bool) RemoveModelCacheInput {
	return RemoveModelCacheInput{Cluster: cluster, Claim: claim, Mode: ModeApply, DryRun: dryRun}
}

// gazelleWithSlice is the installation's own cluster with the model cache
// claim of before and a pool whose slice release runs with the cache on,
// mounting that claim.
func gazelleWithSlice(t *testing.T, fixtures ...string) (*lab, *Service) {
	t.Helper()
	l := newLab(t, "installation.yaml")
	l.add(t, l.installation, "prewarm.yaml") // the own cluster's values and join token
	l.add(t, l.installation, "cache-claim.yaml")
	for _, f := range fixtures {
		l.add(t, l.installation, f)
	}
	svc := l.service(Config{Installation: "gazelle"})
	in := l4("gazelle", "gpu-l40s", false)
	in.Pool.Accelerator, in.Pool.Sizes, in.Pool.MaxGPUs = "nvidia-l40s", []string{"2xlarge"}, 1
	if len(fixtures) > 0 {
		// With a claim per zone the zone has to be named: the one hf-cache
		// is Bound in, so the slice mounts the claim of before.
		in.Pool.Zones = []string{"eu-central-1b"}
	}
	_, err := svc.CreateNodePool(context.Background(), in)
	require.NoError(t, err, "the slice landed with the cache on, mounting hf-cache")
	return l, svc
}

func gazelleCluster(t *testing.T, svc *Service) Cluster {
	t.Helper()
	answer, err := svc.ListClusters(context.Background())
	require.NoError(t, err)
	for _, c := range answer.Clusters {
		if c.Name == "gazelle" {
			return c
		}
	}
	t.Fatal("gazelle not listed")
	return Cluster{}
}

// TestRemoveModelCache (giantswarm/cluster-manager#83): the cache is removed
// in order — the slice release upgraded to serve without it first, so the
// connectivity chart's hook applies no claim again, then the claim deleted on
// the cluster as the caller — the dry run naming both with the claim's price;
// applied, list_clusters shows no claim and the slice off; a re-run finds
// nothing to remove.
func TestRemoveModelCache(t *testing.T) {
	ctx := context.Background()
	_, svc := gazelleWithSlice(t)
	before := gazelleCluster(t, svc)
	require.Len(t, before.Serving.Readiness.CacheClaims, 1)
	assert.True(t, before.Serving.Readiness.CacheClaims[0].Mounted, "list_clusters marks the claim the slice mounts")
	assert.True(t, before.Serving.Readiness.Cache.Enabled)

	dry, err := svc.RemoveModelCache(ctx, removeCache("gazelle", "", true))
	require.NoError(t, err)
	assert.Equal(t, []string{"unchanged", "would-update", "would-delete"}, actions(dry), "the slice's source stands, its release is upgraded, the claim goes")
	assert.Equal(t, "HelmRelease", dry.Objects[1].Kind)
	assert.Equal(t, []string{"spec.values.modelServing.cache.enabled"}, dry.Objects[1].Changes)
	assert.Equal(t, ObjectAction{APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "hf-cache", Namespace: "model-serving", Action: "would-delete"}, dry.Objects[2])
	enabled, found, _ := unstructured.NestedBool(dry.Manifests[1], "spec", "values", "modelServing", "cache", "enabled")
	require.True(t, found)
	assert.False(t, enabled)
	require.Len(t, dry.RemovedClaims, 1)
	assert.Equal(t, "hf-cache", dry.RemovedClaims[0].Name)
	assert.InDelta(t, 65.45, dry.RemovedClaims[0].Price.MonthlyUSD, 1e-9, "what stops being billed")
	assert.True(t, dry.RemovedClaims[0].Mounted)
	require.NotNil(t, dry.Cache)
	assert.False(t, dry.Cache.Enabled)
	assert.Equal(t, "the model cache of gazelle is removed: 1 claim(s) deleted — model-serving/hf-cache (500 GiB gp3 at 500 MiB/s, about $65.45 a month at list prices (AWS EBS gp3 list price, EU (Frankfurt) (eu-central-1), as of "+compose.PriceAsOf+")) — with their volumes under the class's Delete reclaim policy, and with them the cached weights and compiled graphs of every model served from them; the bill stops with the volume; the slice release org-giantswarm/gazelle-agent-platform is upgraded to modelServing.cache.enabled false, so the connectivity chart applies no claim again and every predictor of gazelle downloads its weights into its pod's ephemeral storage (the node's local disk) at each start — about 90 s more per cold start; a later create_node_pool or enable_model_serving with cache true creates the claim anew and is the slice's upgrade back to the cache", dry.Cache.Note)
	assert.Empty(t, dry.Warnings, "the volume goes with the claim: nothing to warn about")
	assertGolden(t, "remove_model_cache_dry_run", dry)

	out, err := svc.RemoveModelCache(ctx, removeCache("gazelle", "", false))
	require.NoError(t, err)
	assert.Equal(t, []string{"unchanged", "update", "delete"}, actions(out))
	after := gazelleCluster(t, svc)
	assert.Equal(t, []*detect.CacheClaim{}, after.Serving.Readiness.CacheClaims, "the claim is gone")
	assert.False(t, after.Serving.Readiness.Cache.Enabled, "the slice serves without the cache")

	_, err = svc.RemoveModelCache(ctx, removeCache("gazelle", "", false))
	var notFound *ErrNotFound
	require.True(t, errors.As(err, &notFound), "nothing left to remove: %v", err)
	assert.Equal(t, "model cache on gazelle (no hf-cache* claim in model-serving, and no slice release of cluster-manager's runs with the cache on there) not found", err.Error())

	// The way back: a create with the cache on is the slice's upgrade and
	// the chart creates the claim anew.
	in := l4("gazelle", "gpu-l40s", true)
	in.Pool.Accelerator, in.Pool.Sizes, in.Pool.MaxGPUs = "nvidia-l40s", []string{"2xlarge"}, 1
	again, err := svc.CreateNodePool(ctx, in)
	require.NoError(t, err)
	assert.True(t, again.Cache.Enabled)
	assert.False(t, again.Cache.Exists, "the claim does not exist yet")
	assert.Contains(t, again.Cache.Note, "the model cache is switched on for every pool of gazelle")
	assert.Contains(t, again.Cache.Note, "it does not exist yet: the connectivity chart creates it and keeps it (no price: the size and tier the connectivity chart creates the claim with could not be read (this server reads no chart registry))")
}

// TestRemoveModelCacheRefusedWhileMounted: a claim a predictor pod mounts is
// not removed — the delete would hang on its protection finalizer, and the
// predictor reads its weights from it — and the refusal names the pod and the
// served model to unload first, beside the structured refused{models, hint}
// block; nothing is written.
func TestRemoveModelCacheRefusedWhileMounted(t *testing.T) {
	ctx := context.Background()
	_, svc := gazelleWithSlice(t, "cache-claim-mounted.yaml")
	_, err := svc.RemoveModelCache(ctx, removeCache("gazelle", "", false))
	assertRefused(t, err, "the model cache of gazelle is in use — model-serving/hf-cache is mounted by pod model-serving/qwen3-8b-kserve-7c9d8-xnkd2 (LLMInferenceService model-serving/qwen3-8b): a claim a pod mounts is not deleted until the pod is gone")
	var refused *ErrRefused
	require.True(t, errors.As(err, &refused))
	assert.Equal(t, []string{"LLMInferenceService model-serving/qwen3-8b"}, refused.Refused.Models)
	assert.Equal(t, "Unload the served model(s) with model-manager's unload_model and re-run: a claim a pod mounts is not deleted until the pod is gone.", refused.Refused.Hint)
	assert.Equal(t, readFromCluster, refused.Refused.ReadFrom)
	after := gazelleCluster(t, svc)
	require.Len(t, after.Serving.Readiness.CacheClaims, 1, "nothing was written")
	assert.True(t, after.Serving.Readiness.Cache.Enabled)
}

// TestRemoveModelCacheOneClaim: the claim named goes alone — the slice keeps
// its cache when it mounts another —, a volume the class keeps is a warning
// naming that it stays and is billed, and a name that is no claim of the
// namespace is not found, the claims there named.
func TestRemoveModelCacheOneClaim(t *testing.T) {
	ctx := context.Background()
	_, svc := gazelleWithSlice(t, "cache-claim-zone.yaml")
	out, err := svc.RemoveModelCache(ctx, removeCache("gazelle", "hf-cache-eu-central-1a", true))
	require.NoError(t, err)
	assert.Equal(t, []string{"would-delete"}, actions(out), "the slice mounts hf-cache, not the zone's claim: it is not touched")
	assert.Equal(t, "hf-cache-eu-central-1a", out.Objects[0].Name)
	assert.Nil(t, out.Slice)
	assert.Equal(t, []string{"volume pvc-0c2f4a1e-1a1a-4a1a-9a1a-000000000a1a of claim model-serving/hf-cache-eu-central-1a is kept (reclaim policy Retain): it stays, and is billed, until deleted by hand"}, out.Warnings)
	require.Len(t, out.RemovedClaims, 1)
	assert.Len(t, out.CacheClaims, 2, "every claim as read")
	assert.NotContains(t, out.Cache.Note, "the slice release", "the slice keeps its cache")
	assert.Contains(t, out.Cache.Note, "1 claim(s) deleted — model-serving/hf-cache-eu-central-1a (")

	_, err = svc.RemoveModelCache(ctx, removeCache("gazelle", "hf-cache-eu-west-1a", true))
	var notFound *ErrNotFound
	require.True(t, errors.As(err, &notFound))
	assert.Equal(t, "model cache claim model-serving/hf-cache-eu-west-1a on gazelle (the claims there: model-serving/hf-cache (Bound in eu-central-1b), model-serving/hf-cache-eu-central-1a (Bound in eu-central-1a)) not found", err.Error())
}

// TestRemoveModelCacheRefusals: where someone else provides serving the cache
// is that layer's setting; a cluster without a claim or a slice has nothing
// to remove; a cluster that cannot be read as the caller is a refusal.
func TestRemoveModelCacheRefusals(t *testing.T) {
	ctx := context.Background()
	l := newLab(t, "installation.yaml")
	svc := l.service(Config{Installation: "gazelle"})
	_, err := svc.RemoveModelCache(ctx, removeCache("wc2", "", true))
	assertRefused(t, err, "the model cache on wc2 is the serving layer's setting, and serving there is provided by the platform's own release")

	_, err = svc.RemoveModelCache(ctx, removeCache("wc1", "", true))
	var notFound *ErrNotFound
	require.True(t, errors.As(err, &notFound), "%v", err)

	l.unreachable(wc1APIServer)
	_, err = svc.RemoveModelCache(ctx, removeCache("wc1", "", true))
	assertRefused(t, err, "the model cache of wc1 cannot be read as you")
}

// TestCreateNodePoolCacheOffRefusedWhileTheClusterKeepsOne
// (giantswarm/cluster-manager#83): the slice release is the cluster's one, so
// cache false on a second pool would switch the cache off for the models
// served on every pool while the claim stays and keeps costing — refused,
// naming the claim, its price and the ways out (refused{cacheOn}); the same
// for enable_model_serving. Cache on stands, and is every pool's.
func TestCreateNodePoolCacheOffRefusedWhileTheClusterKeepsOne(t *testing.T) {
	ctx := context.Background()
	_, svc := gazelleWithSlice(t)
	off := false
	in := l4("gazelle", "gpu-l4", true)
	in.Cache = &off
	_, err := svc.CreateNodePool(ctx, in)
	assertRefused(t, err, "cache false: the model cache is on for every pool of gazelle — the slice release org-giantswarm/gazelle-agent-platform is the cluster's one and mounts model-serving/hf-cache (Bound in eu-central-1b; 500 GiB gp3 at 500 MiB/s, about $65.45 a month at list prices (AWS EBS gp3 list price, EU (Frankfurt) (eu-central-1), as of "+compose.PriceAsOf+")) — and cache false on this pool would switch it off for the models served on every pool while the claim stays and keeps costing; leave cache on (the default): this pool's slice mounts the cluster's cache like every other pool's, or remove the cache with remove_model_cache: the slice release is upgraded to serve without it, the claim and its volume go, and every model served on the cluster downloads and compiles again at its next start")
	var refused *ErrRefused
	require.True(t, errors.As(err, &refused))
	require.NotNil(t, refused.Refused.CacheOn)
	assert.Equal(t, "hf-cache", refused.Refused.CacheOn.ClaimName)
	require.NotNil(t, refused.Refused.CacheOn.Claim)
	assert.InDelta(t, 65.45, refused.Refused.CacheOn.Claim.Price.MonthlyUSD, 1e-9)
	assert.Len(t, refused.Refused.CacheOn.Remedies, 2)
	assert.Equal(t, "Leave cache on, or remove the cache with remove_model_cache, and re-run.", refused.Refused.Hint)

	// The same call with the cache on stands, and the note says whose the
	// setting is.
	in.Cache = nil
	out, err := svc.CreateNodePool(ctx, in)
	require.NoError(t, err)
	assert.True(t, out.Cache.Enabled)
	assert.Contains(t, out.Cache.Note, "the slice release is the cluster's one, so every predictor of the cluster mounts this claim from now on")
	assert.NotContains(t, out.Cache.Note, "switched on", "the cache was on already: nothing switched")

	sin := serving("gazelle", true)
	sin.Cache = &off
	_, err = svc.EnableModelServing(ctx, sin)
	assertRefused(t, err, "cache false: the model cache is on for every pool of gazelle")
}
