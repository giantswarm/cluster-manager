package tools

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/cluster-manager/internal/detect"
)

// The three well-known configs of the wc1-configs and wc1-stranded fixtures.
var fixtureConfigs = []string{"kserve-config-llm-decode-worker-data-parallel", "kserve-config-llm-scheduler", "kserve-config-llm-template"}

// finalizing gives the fake at apiServer the apiserver's finalizer semantics
// for LLMInferenceServiceConfigs: a delete of an object carrying finalizers
// marks it terminating instead, and the update that takes the last finalizer
// off drops it.
func (l *lab) finalizing(t *testing.T, apiServer string) *lab {
	t.Helper()
	dyn, ok := l.targets[apiServer].(*dynamicfake.FakeDynamicClient)
	require.True(t, ok)
	gvr, tracker := detect.LLMISVCConfigGVR, dyn.Tracker()
	dyn.PrependReactor("delete", gvr.Resource, func(a k8stesting.Action) (bool, runtime.Object, error) {
		del, _ := a.(k8stesting.DeleteAction)
		obj, err := tracker.Get(gvr, del.GetNamespace(), del.GetName())
		if err != nil {
			return true, nil, err
		}
		u, _ := obj.(*unstructured.Unstructured)
		if len(u.GetFinalizers()) == 0 {
			return false, nil, nil
		}
		if u.GetDeletionTimestamp() == nil {
			now := metav1.Now()
			u.SetDeletionTimestamp(&now)
			return true, nil, tracker.Update(gvr, u, del.GetNamespace())
		}
		return true, nil, nil
	})
	dyn.PrependReactor("update", gvr.Resource, func(a k8stesting.Action) (bool, runtime.Object, error) {
		upd, _ := a.(k8stesting.UpdateAction)
		u, _ := upd.GetObject().(*unstructured.Unstructured)
		if u.GetDeletionTimestamp() != nil && len(u.GetFinalizers()) == 0 {
			return true, u, tracker.Delete(gvr, u.GetNamespace(), u.GetName())
		}
		return false, nil, nil
	})
	return l
}

// controllerRuns puts a ready llm-d controller Deployment on the target.
func (l *lab) controllerRuns(t *testing.T, apiServer string) *lab {
	t.Helper()
	dep := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": detect.LLMISVCController, "namespace": "org-acme", "labels": map[string]any{detect.LabelControlPlane: detect.LLMISVCController}},
		"status":   map[string]any{"readyReplicas": int64(1)},
	}}
	_, err := l.targets[apiServer].Resource(detect.DeploymentGVR).Namespace("org-acme").Create(context.Background(), dep, metav1.CreateOptions{})
	require.NoError(t, err)
	return l
}

// controllerClears makes the target's llm-d controller do its job at the
// second look: the configs vanish, as they do once their release is
// uninstalled and no LLMInferenceService references them.
func (l *lab) controllerClears(t *testing.T, apiServer string) *lab {
	t.Helper()
	dyn, ok := l.targets[apiServer].(*dynamicfake.FakeDynamicClient)
	require.True(t, ok)
	gvr, tracker := detect.LLMISVCConfigGVR, dyn.Tracker()
	var looks atomic.Int32
	dyn.PrependReactor("list", gvr.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		if looks.Add(1) >= 2 {
			for _, name := range fixtureConfigs {
				_ = tracker.Delete(gvr, "org-acme", name)
			}
		}
		return false, nil, nil
	})
	return l
}

// servingLab is a lab with cluster-manager's slice on wc1 and its
// kserve-runtime-configs child release beside it on the installation, as the
// meta chart renders it (owned through Flux's labels, not cluster-manager's).
func servingLab(t *testing.T, target string) (*lab, *Service) {
	t.Helper()
	l := newLab(t, "installation.yaml").target(t, wc1APIServer, target).finalizing(t, wc1APIServer)
	svc := l.service(Config{Installation: "gazelle", ConfigTeardownTimeout: 40 * time.Millisecond})
	ctx := context.Background()
	_, err := svc.EnableModelServing(ctx, serving("wc1", false))
	require.NoError(t, err)
	child := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease",
		"metadata": map[string]any{"name": runtimeConfigsRelease, "namespace": "org-acme", "labels": map[string]any{
			detect.LabelFluxReleaseName: "wc1-agent-platform", detect.LabelFluxReleaseNamespace: "org-acme", detect.LabelHelmChart: "agent-platform-4.27.2",
		}},
		"spec": map[string]any{"chart": map[string]any{"spec": map[string]any{"chart": runtimeConfigsRelease}}},
	}}
	_, err = l.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Create(ctx, child, metav1.CreateOptions{})
	require.NoError(t, err)
	return l, svc
}

// emptyPool scales wc1's pool to zero, so a delete needs no force.
func emptyPool(t *testing.T, l *lab) {
	t.Helper()
	ctx := context.Background()
	mp, err := l.installation.Resource(MachinePoolGVR).Namespace("org-acme").Get(ctx, "wc1-gpu-a10g", metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedField(mp.Object, int64(0), "spec", "replicas"))
	_, err = l.installation.Resource(MachinePoolGVR).Namespace("org-acme").Update(ctx, mp, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func objectNames(out *WriteResult) []string {
	names := make([]string, 0, len(out.Objects))
	for _, o := range out.Objects {
		names = append(names, o.Action+" "+o.Kind+" "+o.Namespace+"/"+o.Name)
	}
	return names
}

func remainingConfigs(t *testing.T, l *lab) []string {
	t.Helper()
	configs, err := detect.Configs(context.Background(), l.targets[wc1APIServer], "org-acme")
	require.NoError(t, err)
	return detect.Names(configs)
}

// The ordered teardown: the configs' release goes first, the configs are
// seen gone, then the pool, the slice release and the backend.
var orderedTeardown = []string{
	"delete HelmRelease org-acme/kserve-runtime-configs",
	"delete LLMInferenceServiceConfig org-acme/kserve-config-llm-decode-worker-data-parallel",
	"delete LLMInferenceServiceConfig org-acme/kserve-config-llm-scheduler",
	"delete LLMInferenceServiceConfig org-acme/kserve-config-llm-template",
	"delete HelmRelease org-acme/wc1-gpu-a10g",
	"delete OCIRepository org-acme/wc1-gpu-a10g",
	"delete HelmRelease org-acme/wc1-agent-platform",
	"delete OCIRepository org-acme/wc1-agent-platform",
	"delete ConfigMap agent-platform/model-backend-kserve",
}

// TestDeleteLastPoolWithForceStripsTheConfigs (giantswarm/cluster-manager#28):
// force removes the configs with their finalizer instead of waiting for a
// controller, and nothing is left terminating.
func TestDeleteLastPoolWithForceStripsTheConfigs(t *testing.T) {
	l, svc := servingLab(t, "wc1-configs.yaml")
	ctx := context.Background()

	dry, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply, Force: true, DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"would-delete", "would-delete", "would-delete", "would-delete", "would-delete", "would-delete", "would-delete", "would-delete", "would-delete"}, actions(dry))
	assert.Equal(t, fixtureConfigs, remainingConfigs(t, l), "a dry-run touches nothing")
	assertGolden(t, "delete_node_pool_ordered_teardown_dry_run", dry)

	out, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply, Force: true})
	require.NoError(t, err)
	assert.Equal(t, orderedTeardown, objectNames(out))
	assert.Empty(t, remainingConfigs(t, l), "no config remains, terminating or not")
	require.Len(t, out.Warnings, 1)
	assert.Contains(t, out.Warnings[0], "3 LLMInferenceServiceConfig(s) in org-acme on wc1 removed by cluster-manager with serving.kserve.io/llmisvcconfig-finalizer taken off (removed with force): kserve-config-llm-decode-worker-data-parallel, kserve-config-llm-scheduler, kserve-config-llm-template")
	_, err = l.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Get(ctx, runtimeConfigsRelease, metav1.GetOptions{})
	assert.Error(t, err, "the child release is gone")
}

// TestDeleteLastPoolWithoutAControllerStripsTheConfigs: with no llm-d
// controller on the cluster nothing would ever clear the finalizer, so the
// configs are removed without a wait.
func TestDeleteLastPoolWithoutAControllerStripsTheConfigs(t *testing.T) {
	l, svc := servingLab(t, "wc1-configs.yaml")
	emptyPool(t, l)
	out, err := svc.DeleteNodePool(context.Background(), DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply})
	require.NoError(t, err)
	assert.Equal(t, orderedTeardown, objectNames(out))
	assert.Empty(t, remainingConfigs(t, l))
	require.Len(t, out.Warnings, 1)
	assert.Contains(t, out.Warnings[0], "(no llmisvc controller runs on wc1 to clear the finalizer)")
}

// TestDeleteLastPoolWaitsForTheController: with the controller running the
// configs go with their release and the controller clears them; nothing of
// cluster-manager's touches them and the answer carries no warning.
func TestDeleteLastPoolWaitsForTheController(t *testing.T) {
	l, svc := servingLab(t, "wc1-configs.yaml")
	l.controllerRuns(t, wc1APIServer).controllerClears(t, wc1APIServer)
	emptyPool(t, l)
	ctx := context.Background()

	dry, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply, DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, "would-delete HelmRelease org-acme/kserve-runtime-configs", objectNames(dry)[0])
	assert.Len(t, dry.Objects, 6, "the child release, the pool, the slice, the backend; the configs are the controller's")

	out, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"delete HelmRelease org-acme/kserve-runtime-configs",
		"delete HelmRelease org-acme/wc1-gpu-a10g", "delete OCIRepository org-acme/wc1-gpu-a10g",
		"delete HelmRelease org-acme/wc1-agent-platform", "delete OCIRepository org-acme/wc1-agent-platform",
		"delete ConfigMap agent-platform/model-backend-kserve",
	}, objectNames(out))
	assert.Empty(t, remainingConfigs(t, l))
	assert.Empty(t, out.Warnings)
}

// TestDeleteLastPoolStripsWhatTheControllerLeaves: a controller that does
// not clear the configs within the wait is not waited for forever — the
// configs are removed and the answer says so.
func TestDeleteLastPoolStripsWhatTheControllerLeaves(t *testing.T) {
	l, svc := servingLab(t, "wc1-configs.yaml")
	l.controllerRuns(t, wc1APIServer)
	emptyPool(t, l)
	out, err := svc.DeleteNodePool(context.Background(), DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply})
	require.NoError(t, err)
	assert.Equal(t, orderedTeardown, objectNames(out))
	assert.Empty(t, remainingConfigs(t, l))
	require.Len(t, out.Warnings, 1)
	assert.Contains(t, out.Warnings[0], "(still present 40ms after the kserve-runtime-configs release was removed)")
}

// TestDeleteLastPoolUnreadableTarget: without force, a cluster that cannot
// be read as the caller is a refusal — whether the configs are gone cannot
// be told; force proceeds with a warning.
func TestDeleteLastPoolUnreadableTarget(t *testing.T) {
	l, svc := servingLab(t, "wc1-configs.yaml")
	emptyPool(t, l)
	l.unreachable(wc1APIServer)
	ctx := context.Background()
	_, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply})
	assertRefused(t, err, "cannot tell whether the LLMInferenceServiceConfigs of org-acme on wc1 are gone (cluster wc1 not readable as you through "+wc1APIServer+": connection refused): the serving slice is removed in order so none is left terminating — make the cluster readable as you and re-run, or pass force")
	_, err = l.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Get(ctx, "wc1-gpu-a10g", metav1.GetOptions{})
	require.NoError(t, err, "a refusal deletes nothing")

	out, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply, Force: true})
	require.NoError(t, err)
	assert.Equal(t, "delete HelmRelease org-acme/kserve-runtime-configs", objectNames(out)[0])
	require.Len(t, out.Warnings, 1)
	assert.Contains(t, out.Warnings[0], "whether LLMInferenceServiceConfigs remain in org-acme on wc1 cannot be told")
}

// TestDisableModelServingTearsDownInOrder: disable_model_serving takes the
// same ordered path as the last pool's delete.
func TestDisableModelServingTearsDownInOrder(t *testing.T) {
	l, svc := servingLab(t, "wc1-configs.yaml")
	out, err := svc.DisableModelServing(context.Background(), ModelServingInput{Cluster: "wc1", Mode: ModeApply})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"delete HelmRelease org-acme/kserve-runtime-configs",
		"delete LLMInferenceServiceConfig org-acme/kserve-config-llm-decode-worker-data-parallel",
		"delete LLMInferenceServiceConfig org-acme/kserve-config-llm-scheduler",
		"delete LLMInferenceServiceConfig org-acme/kserve-config-llm-template",
		"delete HelmRelease org-acme/wc1-agent-platform", "delete OCIRepository org-acme/wc1-agent-platform",
		"delete ConfigMap agent-platform/model-backend-kserve",
	}, objectNames(out))
	assert.Empty(t, remainingConfigs(t, l))
}

// TestCreateNodePoolHealsStrandedConfigs: on a cluster whose release
// namespace holds configs a serving layer left terminating, list_clusters
// names them, and create_node_pool removes them before the slice lands — so
// the slice's release creates them afresh instead of adopting and losing them.
// With a controller running they are its and are only named.
func TestCreateNodePoolHealsStrandedConfigs(t *testing.T) {
	l := newLab(t, "installation.yaml").target(t, wc1APIServer, "wc1-stranded.yaml").finalizing(t, wc1APIServer)
	svc := l.service(Config{Installation: "gazelle"})
	ctx := context.Background()

	detected := wc1Cluster(t, l).Serving
	assert.Equal(t, detect.StatusAbsent, detected.Status)
	assert.Contains(t, detected.Evidence, "3 LLMInferenceServiceConfig(s) terminating in org-acme with serving.kserve.io/llmisvcconfig-finalizer uncleared (stranded; healed by the next create_node_pool or enable_model_serving): kserve-config-llm-decode-worker-data-parallel, kserve-config-llm-scheduler, kserve-config-llm-template")

	dry, err := svc.CreateNodePool(ctx, l4("wc1", "gpu-l4", true))
	require.NoError(t, err)
	assert.Equal(t, []string{
		"would-delete LLMInferenceServiceConfig org-acme/kserve-config-llm-decode-worker-data-parallel",
		"would-delete LLMInferenceServiceConfig org-acme/kserve-config-llm-scheduler",
		"would-delete LLMInferenceServiceConfig org-acme/kserve-config-llm-template",
	}, objectNames(dry)[:3], "the heal comes before anything lands")
	assert.Equal(t, fixtureConfigs, remainingConfigs(t, l), "a dry-run touches nothing")
	require.Len(t, dry.Warnings, 1)
	assert.Contains(t, dry.Warnings[0], "3 LLMInferenceServiceConfig(s) in org-acme on wc1 would be removed by cluster-manager with serving.kserve.io/llmisvcconfig-finalizer taken off (left terminating by a serving layer that went, no llmisvc controller to clear the finalizer; removed so the slice's release creates them afresh)")

	out, err := svc.CreateNodePool(ctx, l4("wc1", "gpu-l4", false))
	require.NoError(t, err)
	assert.Len(t, out.Objects, 11, "three configs healed; pool source, Secret, release; operator source, release; slice source, release; backend")
	assert.Empty(t, remainingConfigs(t, l), "nothing terminating is left for the slice's release to adopt")
	assert.NotContains(t, wc1Cluster(t, l).Serving.Evidence[0], "terminating")

	l.target(t, wc1APIServer, "wc1-stranded.yaml").controllerRuns(t, wc1APIServer)
	kept, err := svc.EnableModelServing(ctx, serving("wc1", true))
	require.NoError(t, err)
	assert.Equal(t, fixtureConfigs, remainingConfigs(t, l), "a running controller's configs are not touched")
	require.Len(t, kept.Warnings, 1)
	assert.Contains(t, kept.Warnings[0], "3 LLMInferenceServiceConfig(s) in org-acme on wc1 are terminating while an llmisvc controller runs (kserve-config-llm-decode-worker-data-parallel, kserve-config-llm-scheduler, kserve-config-llm-template): models still referencing them hold them")
}
