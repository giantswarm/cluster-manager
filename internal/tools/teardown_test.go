package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/cluster-manager/internal/detect"
)

// The three well-known configs of the wc1-configs and wc1-stranded fixtures.
var fixtureConfigs = []string{"kserve-config-llm-decode-worker-data-parallel", "kserve-config-llm-scheduler", "kserve-config-llm-template"}

const webhookDenial = `admission webhook "llminferenceserviceconfig.kserve-webhook-server.v1alpha2.validator" denied the request: well-known config %s/%s cannot be deleted`

// finalizing gives the fake at apiServer the apiserver's finalizer semantics
// for LLMInferenceServiceConfigs: a delete of an object carrying finalizers
// marks it terminating instead, and the update that takes the last finalizer
// off drops it.
func (l *lab) finalizing(t *testing.T, apiServer string) *lab {
	t.Helper()
	dyn := fakeTarget(t, l, apiServer)
	gvr, tracker := configsStorageGVR, dyn.Tracker()
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

func fakeTarget(t *testing.T, l *lab, apiServer string) *dynamicfake.FakeDynamicClient {
	t.Helper()
	dyn, ok := l.targets[apiServer].(*dynamicfake.FakeDynamicClient)
	require.True(t, ok)
	return dyn
}

// controllerRuns puts a ready llm-d controller Deployment on the target, and
// with it the controller's webhook: while the Deployment is there, every
// delete of a LLMInferenceServiceConfig is denied the way KServe's validator
// denies it — Helm's, cluster-manager's, anyone's.
func (l *lab) controllerRuns(t *testing.T, apiServer string) *lab {
	t.Helper()
	dep := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": detect.LLMISVCController, "namespace": "org-acme", "labels": map[string]any{detect.LabelControlPlane: detect.LLMISVCController}},
		"status":   map[string]any{"readyReplicas": int64(1)},
	}}
	dyn := fakeTarget(t, l, apiServer)
	_, err := dyn.Resource(detect.DeploymentGVR).Namespace("org-acme").Create(context.Background(), dep, metav1.CreateOptions{})
	require.NoError(t, err)
	gvr, tracker := configsStorageGVR, dyn.Tracker()
	dyn.PrependReactor("delete", gvr.Resource, func(a k8stesting.Action) (bool, runtime.Object, error) {
		if _, err := tracker.Get(detect.DeploymentGVR, "org-acme", detect.LLMISVCController); err != nil {
			return false, nil, nil
		}
		del, _ := a.(k8stesting.DeleteAction)
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource}, del.GetName(), fmt.Errorf(webhookDenial, del.GetNamespace(), del.GetName()))
	})
	return l
}

// fluxUninstallsController is the installation's Flux at work: once the
// controller's child release is deleted, the controller Deployment is gone
// from the target at the given look — the second look exercises the wait.
func fluxUninstallsController(t *testing.T, l *lab, look int32) {
	t.Helper()
	var armed atomic.Bool
	var looks atomic.Int32
	fakeInstallation(t, l).PrependReactor("delete", HelmReleaseGVR.Resource, func(a k8stesting.Action) (bool, runtime.Object, error) {
		if del, _ := a.(k8stesting.DeleteAction); del.GetName() == llmisvcResourcesRelease {
			armed.Store(true)
		}
		return false, nil, nil
	})
	target := fakeTarget(t, l, wc1APIServer)
	target.PrependReactor("list", detect.DeploymentGVR.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		if armed.Load() && looks.Add(1) >= look {
			_ = target.Tracker().Delete(detect.DeploymentGVR, "org-acme", detect.LLMISVCController)
		}
		return false, nil, nil
	})
}

// servingLab is a lab with cluster-manager's slice on wc1 and its two
// KServe child releases beside it on the installation, as the meta chart
// renders them (owned through Flux's labels, not cluster-manager's).
func servingLab(t *testing.T, target string) (*lab, *Service) {
	t.Helper()
	l := newLab(t, "installation.yaml").target(t, wc1APIServer, target).finalizing(t, wc1APIServer)
	svc := l.service(Config{Installation: "gazelle"})
	ctx := context.Background()
	_, err := svc.EnableModelServing(ctx, serving("wc1", false))
	require.NoError(t, err)
	for _, child := range []string{llmisvcResourcesRelease, runtimeConfigsRelease} {
		hr := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease",
			"metadata": map[string]any{"name": child, "namespace": "org-acme", "labels": map[string]any{
				detect.LabelFluxReleaseName: "wc1-agent-platform", detect.LabelFluxReleaseNamespace: "org-acme", detect.LabelHelmChart: "agent-platform-4.27.2",
			}},
			"spec": map[string]any{"chart": map[string]any{"spec": map[string]any{"chart": child}}},
		}}
		_, err = l.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Create(ctx, hr, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	return l, svc
}

// stuckChild puts the configs' child release into the state helm-controller
// leaves it in when the webhook denied its uninstall: deleted, Ready=False
// UninstallFailed (gazelle, 2026-09-17 07:41Z).
func stuckChild(t *testing.T, l *lab) {
	t.Helper()
	ctx := context.Background()
	res := l.installation.Resource(HelmReleaseGVR).Namespace("org-acme")
	hr, err := res.Get(ctx, runtimeConfigsRelease, metav1.GetOptions{})
	require.NoError(t, err)
	deleted := metav1.NewTime(time.Date(2026, 9, 17, 7, 41, 45, 0, time.UTC))
	hr.SetDeletionTimestamp(&deleted)
	hr.SetFinalizers([]string{"finalizers.fluxcd.io"})
	require.NoError(t, unstructured.SetNestedSlice(hr.Object, []any{map[string]any{
		"type": "Ready", "status": "False", "reason": "UninstallFailed",
		"message": "Helm uninstall failed for release org-acme/kserve-runtime-configs.v1 with chart kserve-runtime-configs@0.2.4: failed to delete release: kserve-runtime-configs",
	}}, "status", "conditions"))
	_, err = res.Update(ctx, hr, metav1.UpdateOptions{})
	require.NoError(t, err)
}

// emptyPool scales wc1's pool to zero on the installation: with the cluster
// not readable, the MachinePool's replicas are all the nodes guard has.
func emptyPool(t *testing.T, l *lab) {
	t.Helper()
	ctx := context.Background()
	mp, err := l.installation.Resource(MachinePoolGVR).Namespace("org-acme").Get(ctx, "wc1-gpu-a10g", metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedField(mp.Object, int64(0), "spec", "replicas"))
	_, err = l.installation.Resource(MachinePoolGVR).Namespace("org-acme").Update(ctx, mp, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func deleteLastPool(t *testing.T, svc *Service, ctx context.Context, force, dryRun bool) *WriteResult {
	t.Helper()
	out, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply, Force: force, DryRun: dryRun})
	require.NoError(t, err)
	return out
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

// assertGone checks that none of the installation's HelmReleases named
// remains.
func assertGone(t *testing.T, l *lab, names ...string) {
	t.Helper()
	for _, name := range names {
		_, err := l.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Get(context.Background(), name, metav1.GetOptions{})
		assert.True(t, apierrors.IsNotFound(err), "HelmRelease %s should be gone: %v", name, err)
	}
}

// withActions rewrites the action of every name from the given index on.
func withActions(names []string, action string, from int) []string {
	out := make([]string, len(names))
	for i, n := range names {
		if i >= from {
			n = action + n[strings.Index(n, " "):]
		}
		out[i] = n
	}
	return out
}

// The ordered teardown: the controller's release, the configs — removed by
// cluster-manager once the controller is gone —, the configs' release, the
// backend registration, the slice, and the pool last (its release the very
// last: the re-run's anchor).
var orderedTeardown = []string{
	"delete HelmRelease org-acme/kserve-llmisvc-resources",
	"delete LLMInferenceServiceConfig org-acme/kserve-config-llm-decode-worker-data-parallel",
	"delete LLMInferenceServiceConfig org-acme/kserve-config-llm-scheduler",
	"delete LLMInferenceServiceConfig org-acme/kserve-config-llm-template",
	"delete HelmRelease org-acme/kserve-runtime-configs",
	"delete ConfigMap agent-platform/model-backend-kserve",
	"delete OCIRepository org-acme/wc1-agent-platform",
	"delete HelmRelease org-acme/wc1-agent-platform",
	"delete OCIRepository org-acme/wc1-gpu-a10g",
	"delete HelmRelease org-acme/wc1-gpu-a10g",
}

// TestDeleteLastPoolTearsDownInOrder (giantswarm/cluster-manager#28, #37):
// with the controller running and its webhook denying every delete of the
// configs, the controller's release goes first, the configs are removed by
// cluster-manager once Flux has taken the controller away (at the second
// look), then the configs' release and the rest; nothing remains and
// nothing is left terminating. The dry run says what would happen.
func TestDeleteLastPoolTearsDownInOrder(t *testing.T) {
	l, svc := servingLab(t, "wc1-configs.yaml")
	l.controllerRuns(t, wc1APIServer)
	fluxUninstallsController(t, l, 2)
	ctx := context.Background()

	dry := deleteLastPool(t, svc, ctx, false, true)
	assert.Equal(t, withActions(orderedTeardown, "would-delete", 0), objectNames(dry))
	assert.Equal(t, fixtureConfigs, remainingConfigs(t, l), "a dry-run touches nothing")
	require.Len(t, dry.Warnings, 1)
	assert.Contains(t, dry.Warnings[0], "3 LLMInferenceServiceConfig(s) in org-acme on wc1 would be removed by cluster-manager with serving.kserve.io/llmisvcconfig-finalizer taken off once the llmisvc controller is gone")
	assertGolden(t, "delete_node_pool_ordered_teardown_dry_run", dry)

	out := deleteLastPool(t, svc, ctx, false, false)
	assert.False(t, out.Partial)
	assert.Equal(t, orderedTeardown, objectNames(out))
	assert.Empty(t, remainingConfigs(t, l), "no config remains, terminating or not")
	require.Len(t, out.Warnings, 1)
	assert.Contains(t, out.Warnings[0], "3 LLMInferenceServiceConfig(s) in org-acme on wc1 removed by cluster-manager with serving.kserve.io/llmisvcconfig-finalizer taken off (the llmisvc controller's webhook denies every delete while it runs, and nothing clears the finalizer once it is gone): kserve-config-llm-decode-worker-data-parallel, kserve-config-llm-scheduler, kserve-config-llm-template")
	assertGone(t, l, llmisvcResourcesRelease, runtimeConfigsRelease, "wc1-agent-platform", "wc1-gpu-a10g")
}

// conversionFailure is the apiserver's answer to a request for a
// LLMInferenceServiceConfig through a version other than the storage one
// once the CRD's conversion webhook service is gone (gazelle, 2026-09-17
// 08:29Z).
const conversionFailure = `conversion webhook for serving.kserve.io/v1alpha2, Kind=LLMInferenceServiceConfig failed: Post "https://llmisvc-webhook-server-service.org-acme.svc:443/convert?timeout=30s": service "llmisvc-webhook-server-service" not found`

// conversionWebhookGoesWithTheController makes the fake at wc1 convert the
// way the apiserver does: a request for a LLMInferenceServiceConfig through
// a version other than the CRD's storage version needs the conversion
// webhook, the llmisvc controller's — once the controller Deployment is gone
// it fails with conversionFailure; the storage version needs no conversion
// and is served throughout.
func conversionWebhookGoesWithTheController(t *testing.T, l *lab) {
	t.Helper()
	dyn := fakeTarget(t, l, wc1APIServer)
	dyn.PrependReactor("*", configsStorageGVR.Resource, func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetResource().Version == configsStorageGVR.Version {
			return false, nil, nil
		}
		if _, err := dyn.Tracker().Get(detect.DeploymentGVR, "org-acme", detect.LLMISVCController); err == nil {
			return false, nil, nil
		}
		return true, nil, apierrors.NewInternalError(errors.New(conversionFailure))
	})
}

// moreConfigs adds n well-known configs to wc1 beside the fixture's three, as
// the fake keeps them: in the storage version, finalizer on.
func moreConfigs(t *testing.T, l *lab, n int) {
	t.Helper()
	res := l.targets[wc1APIServer].Resource(configsStorageGVR).Namespace("org-acme")
	for i := range n {
		c := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": configsStorageGVR.GroupVersion().String(), "kind": "LLMInferenceServiceConfig",
			"metadata": map[string]any{"name": fmt.Sprintf("kserve-config-llm-extra-%d", i), "namespace": "org-acme", "finalizers": []any{detect.LLMISVCConfigFinalizer}},
		}}
		_, err := res.Create(context.Background(), c, metav1.CreateOptions{})
		require.NoError(t, err)
	}
}

// TestDeleteLastPoolPurgesTheConfigsThroughTheStorageVersion: with the
// controller's release gone the CRD's conversion webhook is gone too, and a
// purge through a hard-coded v1alpha1 fails on every config (gazelle,
// 2026-09-17 08:29Z: two partial calls, written=0 on the re-run). The purge
// goes through the storage version the CRD names — v1alpha2 — and ten
// configs are gone on the first call (giantswarm/cluster-manager#39).
func TestDeleteLastPoolPurgesTheConfigsThroughTheStorageVersion(t *testing.T) {
	l, svc := servingLab(t, "wc1-configs.yaml")
	l.controllerRuns(t, wc1APIServer)
	fluxUninstallsController(t, l, 1)
	moreConfigs(t, l, 7)
	conversionWebhookGoesWithTheController(t, l)
	ctx := context.Background()
	require.Len(t, remainingConfigs(t, l), 10)

	out := deleteLastPool(t, svc, ctx, false, false)
	assert.False(t, out.Partial)
	assert.Empty(t, remainingConfigs(t, l), "all ten gone on the first call")
	var purged []string
	for _, o := range out.Objects {
		if o.Kind == "LLMInferenceServiceConfig" {
			assert.Equal(t, "serving.kserve.io/v1alpha2", o.APIVersion, "removed through the storage version")
			purged = append(purged, o.Name)
		}
	}
	assert.Len(t, purged, 10)
	assertGone(t, l, llmisvcResourcesRelease, runtimeConfigsRelease, "wc1-agent-platform", "wc1-gpu-a10g")

	// The fake bites: the same delete through v1alpha1 is what the apiserver
	// refused on gazelle.
	err := l.targets[wc1APIServer].Resource(detect.LLMISVCConfigResource.WithVersion("v1alpha1")).Namespace("org-acme").Delete(ctx, "kserve-config-llm-template", metav1.DeleteOptions{})
	require.Error(t, err)
	assert.True(t, apierrors.IsInternalError(err))
	assert.Contains(t, err.Error(), `service "llmisvc-webhook-server-service" not found`)
}

// TestDeleteLastPoolWithForceTakesTheSameOrder: force skips the guards, not
// the order — the webhook denies force's deletes like any other.
func TestDeleteLastPoolWithForceTakesTheSameOrder(t *testing.T) {
	l, svc := servingLab(t, "wc1-configs.yaml")
	l.controllerRuns(t, wc1APIServer)
	fluxUninstallsController(t, l, 1)
	out := deleteLastPool(t, svc, context.Background(), true, false)
	assert.Equal(t, orderedTeardown, objectNames(out))
	assert.Empty(t, remainingConfigs(t, l))
}

// TestDeleteLastPoolWithoutAControllerStripsTheConfigs: with no llm-d
// controller on the cluster nothing denies the delete and nothing would ever
// clear the finalizer, so the configs go without a wait.
func TestDeleteLastPoolWithoutAControllerStripsTheConfigs(t *testing.T) {
	l, svc := servingLab(t, "wc1-configs.yaml")
	out := deleteLastPool(t, svc, context.Background(), false, false)
	assert.Equal(t, orderedTeardown, objectNames(out))
	assert.Empty(t, remainingConfigs(t, l))
	require.Len(t, out.Warnings, 1)
}

// TestDeleteLastPoolAnswersPartialWhileTheControllerLingers (#37): Flux has
// not taken the controller away within the call's deadline — the answer
// goes out in time with the controller's release deleted and everything
// from the configs on pending; nothing is half-done. The re-run, once the
// controller is gone, completes the teardown.
func TestDeleteLastPoolAnswersPartialWhileTheControllerLingers(t *testing.T) {
	l, svc := servingLab(t, "wc1-configs.yaml")
	l.controllerRuns(t, wc1APIServer)
	ctx, cancel := context.WithTimeout(context.Background(), writeReserve+300*time.Millisecond)
	defer cancel()

	out := deleteLastPool(t, svc, ctx, false, false)
	assert.True(t, out.Partial)
	assert.Equal(t, withActions(orderedTeardown, "pending", 1), objectNames(out))
	assert.Contains(t, out.NextStep, "9 object(s) are pending")
	assert.Contains(t, out.NextStep, "re-run with the same arguments")
	require.Len(t, out.Warnings, 1)
	assert.Contains(t, out.Warnings[0], "3 LLMInferenceServiceConfig(s) in org-acme on wc1 are pending (the llmisvc controller still runs on wc1)")
	assert.Equal(t, fixtureConfigs, remainingConfigs(t, l), "denied by the webhook, none was deleted")
	assertGone(t, l, llmisvcResourcesRelease)

	require.NoError(t, fakeTarget(t, l, wc1APIServer).Tracker().Delete(detect.DeploymentGVR, "org-acme", detect.LLMISVCController), "Flux takes the controller away")
	again := deleteLastPool(t, svc, context.Background(), false, false)
	assert.False(t, again.Partial)
	assert.Equal(t, orderedTeardown[1:], objectNames(again))
	assert.Empty(t, remainingConfigs(t, l))
	assertGone(t, l, runtimeConfigsRelease, "wc1-agent-platform", "wc1-gpu-a10g")
}

// TestDeleteLastPoolCutAfterSomeWritesAnswersPartial (#37): slow writes
// against the budget — two fit, the rest is pending and the answer says so
// instead of the call being cancelled mid-write; the re-run removes what is
// left.
func TestDeleteLastPoolCutAfterSomeWritesAnswersPartial(t *testing.T) {
	const slow, head = 500 * time.Millisecond, 800 * time.Millisecond
	l, svc := servingLab(t, "wc1-configs.yaml")
	var slowWrites atomic.Bool
	slowWrites.Store(true)
	fakeInstallation(t, l).PrependReactor("delete", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		if slowWrites.Load() {
			time.Sleep(slow)
		}
		return false, nil, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), writeReserve+head)
	defer cancel()

	out := deleteLastPool(t, svc, ctx, false, false)
	assert.True(t, out.Partial)
	assert.Equal(t, withActions(orderedTeardown, "pending", 5), objectNames(out), "the two child releases and the configs went; the rest is pending")
	assert.Contains(t, out.NextStep, "5 object(s) are pending")
	assert.Empty(t, remainingConfigs(t, l))
	assertGone(t, l, llmisvcResourcesRelease, runtimeConfigsRelease)

	slowWrites.Store(false)
	again := deleteLastPool(t, svc, context.Background(), false, false)
	assert.False(t, again.Partial)
	assert.Equal(t, orderedTeardown[5:], objectNames(again))
	assertGone(t, l, "wc1-agent-platform", "wc1-gpu-a10g")
}

// TestDeleteLastPoolRetriesAStuckChildRelease (#37, F38 of proof 1): the
// configs' release was deleted while the controller ran and helm-controller's
// uninstall failed on the webhook's denial — list_clusters names it; the
// teardown removes the configs once the controller is gone and asks Flux to
// retry the uninstall at once instead of waiting out its backoff.
func TestDeleteLastPoolRetriesAStuckChildRelease(t *testing.T) {
	l, svc := servingLab(t, "wc1-configs.yaml")
	l.controllerRuns(t, wc1APIServer)
	fluxUninstallsController(t, l, 1)
	stuckChild(t, l)

	assert.Contains(t, wc1Cluster(t, l).Serving.Evidence, "HelmRelease org-acme/kserve-runtime-configs not Ready (deleted since 2026-09-17T07:41:45Z, Ready=False [UninstallFailed] Helm uninstall failed for release org-acme/kserve-runtime-configs.v1 with chart kserve-runtime-configs@0.2.4: failed to delete release: kserve-runtime-configs)")

	out := deleteLastPool(t, svc, context.Background(), false, false)
	assert.False(t, out.Partial)
	assert.Equal(t, append(append([]string{}, orderedTeardown[:4]...), orderedTeardown[5:]...), objectNames(out), "the deleted child is not deleted again")
	assert.Empty(t, remainingConfigs(t, l))
	require.Len(t, out.Warnings, 2)
	assert.Equal(t, "HelmRelease org-acme/kserve-runtime-configs has been deleted since 2026-09-17T07:41:45Z but its uninstall failed (UninstallFailed: Helm uninstall failed for release org-acme/kserve-runtime-configs.v1 with chart kserve-runtime-configs@0.2.4: failed to delete release: kserve-runtime-configs): its objects are gone now and Flux was asked to retry the uninstall at once (reconcile.fluxcd.io/requestedAt)", out.Warnings[1])
	hr, err := l.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Get(context.Background(), runtimeConfigsRelease, metav1.GetOptions{})
	require.NoError(t, err, "Flux's, until its retry succeeds")
	assert.NotEmpty(t, hr.GetAnnotations()[requestedAtAnnotation])
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

	out := deleteLastPool(t, svc, ctx, true, false)
	assert.Equal(t, []string{"delete HelmRelease org-acme/kserve-llmisvc-resources", "delete HelmRelease org-acme/kserve-runtime-configs"}, objectNames(out)[:2])
	require.Len(t, out.Warnings, 1)
	assert.Contains(t, out.Warnings[0], "whether LLMInferenceServiceConfigs remain in org-acme on wc1 cannot be told")
}

// TestDisableModelServingTearsDownInOrder: disable_model_serving takes the
// same ordered path as the last pool's delete, the slice release last.
func TestDisableModelServingTearsDownInOrder(t *testing.T) {
	l, svc := servingLab(t, "wc1-configs.yaml")
	out, err := svc.DisableModelServing(context.Background(), ModelServingInput{Cluster: "wc1", Mode: ModeApply})
	require.NoError(t, err)
	assert.Equal(t, orderedTeardown[:8], objectNames(out))
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
