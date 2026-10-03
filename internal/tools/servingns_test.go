package tools

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/cluster-manager/internal/detect"
)

// servingNamespace puts the serving namespace the wc1 slice's connectivity
// release created onto wc1, with a model cache claim in it when cache.
func servingNamespace(t *testing.T, l *lab, cache bool, deleting bool) {
	t.Helper()
	ctx := context.Background()
	meta := map[string]any{"name": "model-serving", "labels": map[string]any{
		detect.LabelFluxReleaseName: "wc1-agent-platform-connectivity", detect.LabelFluxReleaseNamespace: "org-acme",
	}}
	if deleting {
		meta["deletionTimestamp"] = "2026-10-02T20:00:00Z"
		meta["finalizers"] = []any{"kubernetes"}
	}
	target := l.targets[wc1APIServer]
	_, err := target.Resource(NamespaceGVR).Create(ctx, &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": meta}}, metav1.CreateOptions{})
	require.NoError(t, err)
	if cache {
		pvc := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": "hf-cache", "namespace": "model-serving"}}}
		_, err = target.Resource(detect.PersistentVolumeClaimGVR).Namespace("model-serving").Create(ctx, pvc, metav1.CreateOptions{})
		require.NoError(t, err)
	}
}

func servingNamespaceOnWC1(t *testing.T, l *lab) *unstructured.Unstructured {
	t.Helper()
	ns, err := l.targets[wc1APIServer].Resource(NamespaceGVR).Get(context.Background(), "model-serving", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	require.NoError(t, err)
	return ns
}

func disableWC1(t *testing.T, svc *Service, dryRun bool) *WriteResult {
	t.Helper()
	out, err := svc.DisableModelServing(context.Background(), ModelServingInput{Cluster: "wc1", Mode: ModeApply, DryRun: dryRun})
	require.NoError(t, err)
	return out
}

func lastObject(out *WriteResult) string {
	names := objectNames(out)
	return names[len(names)-1]
}

// TestDisableModelServingRetiresTheServingNamespace
// (giantswarm/cluster-manager#154): the namespace the connectivity chart
// keeps on uninstall is marked retired while the model cache claim holds
// it, the mark naming the claim and the way out; the re-run after
// remove_model_cache's work removes it, and the next re-run finds nothing.
func TestDisableModelServingRetiresTheServingNamespace(t *testing.T) {
	l, svc := servingLab(t, "wc1-configs.yaml")
	servingNamespace(t, l, true, false)

	dry := disableWC1(t, svc, true)
	assert.Equal(t, "would-retire Namespace /model-serving", lastObject(dry))
	assert.Empty(t, servingNamespaceOnWC1(t, l).GetAnnotations()[RetiredAnnotation], "a dry run touches nothing")

	out := disableWC1(t, svc, false)
	assert.Equal(t, "retire Namespace /model-serving", lastObject(out))
	retired := servingNamespaceOnWC1(t, l).GetAnnotations()[RetiredAnnotation]
	assert.Contains(t, retired, "model serving is off on wc1, and the namespace still holds PersistentVolumeClaim hf-cache — remove_model_cache removes the model cache claims")
	assert.Contains(t, out.Warnings[len(out.Warnings)-1], "the serving namespace model-serving on wc1 is kept, marked retired ("+RetiredAnnotation+")")

	again := disableWC1(t, svc, false)
	assert.Equal(t, []string{"unchanged Namespace /model-serving"}, objectNames(again), "the slice is gone: the re-run looks at the namespace alone")
	assert.Equal(t, retired, servingNamespaceOnWC1(t, l).GetAnnotations()[RetiredAnnotation])

	require.NoError(t, l.targets[wc1APIServer].Resource(detect.PersistentVolumeClaimGVR).Namespace("model-serving").Delete(context.Background(), "hf-cache", metav1.DeleteOptions{}))
	removed := disableWC1(t, svc, false)
	assert.Equal(t, []string{"delete Namespace /model-serving"}, objectNames(removed))
	assert.Nil(t, servingNamespaceOnWC1(t, l))

	_, err := svc.DisableModelServing(context.Background(), serving("wc1", false))
	var notFound *ErrNotFound
	require.ErrorAs(t, err, &notFound, "nothing left to remove")
}

// TestDisableModelServingRemovesAnEmptyServingNamespace: without a cache
// claim, the namespace goes with the slice in the same call.
func TestDisableModelServingRemovesAnEmptyServingNamespace(t *testing.T) {
	l, svc := servingLab(t, "wc1-configs.yaml")
	servingNamespace(t, l, false, false)
	out := disableWC1(t, svc, false)
	assert.Equal(t, "delete Namespace /model-serving", lastObject(out))
	assert.Nil(t, servingNamespaceOnWC1(t, l))
}

// TestDisableModelServingLeavesAnotherNamespace: a serving namespace the
// slice did not create is not touched.
func TestDisableModelServingLeavesAnotherNamespace(t *testing.T) {
	l, svc := servingLab(t, "wc1-configs.yaml")
	ns := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "model-serving"}}}
	_, err := l.targets[wc1APIServer].Resource(NamespaceGVR).Create(context.Background(), ns, metav1.CreateOptions{})
	require.NoError(t, err)
	out := disableWC1(t, svc, false)
	assert.NotContains(t, objectNames(out), "delete Namespace /model-serving")
	assert.NotNil(t, servingNamespaceOnWC1(t, l))
}

// TestDisableModelServingWaitsForTheConnectivityUninstall: while the
// connectivity release is still being uninstalled the namespace step is
// pending within the budget, and the re-run completes it.
func TestDisableModelServingWaitsForTheConnectivityUninstall(t *testing.T) {
	l, svc := servingLab(t, "wc1-configs.yaml")
	servingNamespace(t, l, false, false)
	ctx := context.Background()
	hr := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease", "metadata": map[string]any{"name": "wc1-agent-platform-connectivity", "namespace": "org-acme"}}}
	_, err := l.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Create(ctx, hr, metav1.CreateOptions{})
	require.NoError(t, err)

	short, cancel := context.WithTimeout(ctx, writeReserve+300*time.Millisecond)
	defer cancel()
	out, err := svc.DisableModelServing(short, ModelServingInput{Cluster: "wc1", Mode: ModeApply})
	require.NoError(t, err)
	assert.True(t, out.Partial)
	assert.Equal(t, "pending Namespace /model-serving", lastObject(out))
	assert.Contains(t, out.Warnings[len(out.Warnings)-1], "its connectivity release org-acme/wc1-agent-platform-connectivity is still being uninstalled")
	assert.NotNil(t, servingNamespaceOnWC1(t, l))

	require.NoError(t, l.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Delete(ctx, "wc1-agent-platform-connectivity", metav1.DeleteOptions{}))
	again := disableWC1(t, svc, false)
	assert.Equal(t, []string{"delete Namespace /model-serving"}, objectNames(again))
}

// TestEnableModelServingTakesBackARetiredNamespace: the slice composed again
// takes the retired mark off (Helm adopts the namespace as it stands), and a
// namespace being deleted is a refusal before anything lands.
func TestEnableModelServingTakesBackARetiredNamespace(t *testing.T) {
	l, svc := servingLab(t, "wc1-configs.yaml")
	servingNamespace(t, l, true, false)
	disableWC1(t, svc, false)
	require.NotEmpty(t, servingNamespaceOnWC1(t, l).GetAnnotations()[RetiredAnnotation])
	ctx := context.Background()

	dry, err := svc.EnableModelServing(ctx, serving("wc1", true))
	require.NoError(t, err)
	assert.Equal(t, "would-update Namespace /model-serving", objectNames(dry)[0])
	assert.NotEmpty(t, servingNamespaceOnWC1(t, l).GetAnnotations()[RetiredAnnotation], "a dry run touches nothing")

	out, err := svc.EnableModelServing(ctx, serving("wc1", false))
	require.NoError(t, err)
	assert.Equal(t, "update Namespace /model-serving", objectNames(out)[0])
	assert.NotContains(t, servingNamespaceOnWC1(t, l).GetAnnotations(), RetiredAnnotation)

	deleting, svc2 := servingLab(t, "wc1-configs.yaml")
	disableWC1(t, svc2, false)
	servingNamespace(t, deleting, false, true)
	_, err = svc2.EnableModelServing(ctx, serving("wc1", false))
	assertRefused(t, err, "the serving namespace model-serving on wc1 is being deleted since 2026-10-02T20:00:00Z")
}
