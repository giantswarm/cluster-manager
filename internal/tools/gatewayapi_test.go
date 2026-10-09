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
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/detect"
)

func gatewayAPICRDsOn(t *testing.T, manifest map[string]any) bool {
	t.Helper()
	values, _, _ := unstructured.NestedMap(manifest, "spec", "values")
	return compose.GatewayAPICRDsOn(values)
}

// TestSliceComposesGatewayAPICRDs (giantswarm/cluster-manager#183): on a
// workload cluster that serves no Gateway kind the slice switches the meta
// chart's gateway-api-crds component on and the answer says so, the dry run
// included; a platform below the chart that has the component is refused,
// naming the floor; once composed, the CRDs stay composed although the
// cluster now reads them as present; a cluster that serves the Gateway API
// gets nothing composed and the answer names the version found.
func TestSliceComposesGatewayAPICRDs(t *testing.T) {
	ctx := context.Background()

	l := newLab(t, "installation.yaml").withoutGatewayAPI(t, wc1APIServer)
	svc := l.service(Config{Installation: "gazelle"})
	dry, err := svc.EnableModelServing(ctx, serving("wc1", true))
	require.NoError(t, err)
	assert.Equal(t, compose.MinGatewayAPICRDsChartVersion, dry.Slice.ChartVersion, "the platform runs an older chart (a GitOps pin): the slice follows the first chart with the component instead")
	assert.True(t, gatewayAPICRDsOn(t, dry.Manifests[1]), "the dry run shows the component the create would switch on")
	assert.Equal(t, GatewayAPICRDs{Composed: true, Version: compose.GatewayAPIVersion, Note: "wc1 serves no Gateway API: the slice's gateway-api-crds component installs the Gateway API v1.6.1 CRDs (standard channel), before the connectivity release and agentgateway"}, dry.Slice.GatewayAPI)

	_, err = svc.EnableModelServing(ctx, serving("wc1", false))
	require.NoError(t, err)
	l.add(t, l.targets[wc1APIServer], "targets/gateway-api.yaml") // the component's Job applied them
	again, err := svc.EnableModelServing(ctx, serving("wc1", false))
	require.NoError(t, err)
	assert.Equal(t, []string{"unchanged", "unchanged", "unchanged"}, actions(again), "the CRDs the slice installed read as present: the component stays on")
	assert.True(t, again.Slice.GatewayAPI.Composed)

	found, err := newLab(t, "installation.yaml").service(Config{Installation: "gazelle"}).EnableModelServing(ctx, serving("wc1", true))
	require.NoError(t, err)
	assert.False(t, gatewayAPICRDsOn(t, found.Manifests[1]), "a cluster with the Gateway API gets no CRDs composed")
	assert.Equal(t, GatewayAPICRDs{Version: "v1.6.1", Note: "wc1 serves Gateway API v1.6.1 (standard channel) already: the slice composes no Gateway API CRDs and leaves them as they are"}, found.Slice.GatewayAPI)
}

// TestSliceRefusesAnUnreadableGatewayAPI: a Gateway CRD the caller cannot
// read is a refusal naming the cluster and the CRD, never taken for absent —
// composing over a bundle at another version would replace it.
func TestSliceRefusesAnUnreadableGatewayAPI(t *testing.T) {
	l := newLab(t, "installation.yaml")
	l.targets[wc1APIServer].(*dynamicfake.FakeDynamicClient).PrependReactor("get", detect.CRDGVR.Resource, func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.(k8stesting.GetAction).GetName() != detect.GatewayAPICRD {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(detect.CRDGVR.GroupResource(), detect.GatewayAPICRD, nil)
	})
	_, err := l.service(Config{Installation: "gazelle"}).EnableModelServing(context.Background(), serving("wc1", true))
	var refused *ErrRefused
	require.ErrorAs(t, err, &refused)
	assert.Contains(t, refused.Reason, "cannot tell whether wc1 serves the Gateway API")
	assert.Contains(t, refused.Reason, "make the CRD gateways.gateway.networking.k8s.io readable as you")
}

// TestTeardownRemovesTheGatewayAPICRDs: the slice's teardown removes the
// Gateway API CRDs it composed (the chart's hook Job applied them, its
// uninstall leaves them) — unless an object of those kinds none of the
// slice's releases rendered remains, which keeps them, named.
func TestTeardownRemovesTheGatewayAPICRDs(t *testing.T) {
	ctx := context.Background()
	composed := func(t *testing.T) (*lab, *Service) {
		l := newLab(t, "installation.yaml").withoutGatewayAPI(t, wc1APIServer)
		svc := l.service(Config{Installation: "gazelle"})
		_, err := svc.EnableModelServing(ctx, serving("wc1", false))
		require.NoError(t, err)
		l.add(t, l.targets[wc1APIServer], "targets/gateway-api.yaml")
		return l, svc
	}

	l, svc := composed(t)
	out, err := svc.DisableModelServing(ctx, serving("wc1", false))
	require.NoError(t, err)
	var deleted []string
	for _, o := range out.Objects {
		if o.Kind == "CustomResourceDefinition" {
			assert.Equal(t, actionDelete, o.Action)
			deleted = append(deleted, o.Name)
		}
	}
	assert.Equal(t, gatewayAPICRDsInOrder(), deleted, "every CRD of the bundle, the ones absent already included, the marked Gateway CRD last")
	_, err = l.targets[wc1APIServer].Resource(detect.CRDGVR).Get(ctx, detect.GatewayAPICRD, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "the Gateway CRD is gone")

	l, svc = composed(t)
	foreign := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway", "metadata": map[string]any{"name": "edge", "namespace": "ingress"}}}
	_, err = l.targets[wc1APIServer].Resource(detect.GatewayGVR).Namespace("ingress").Create(ctx, foreign, metav1.CreateOptions{})
	require.NoError(t, err)
	out, err = svc.DisableModelServing(ctx, serving("wc1", false))
	require.NoError(t, err)
	for _, o := range out.Objects {
		assert.NotEqual(t, "CustomResourceDefinition", o.Kind, "nothing removed while another Gateway uses the CRDs")
	}
	assert.Contains(t, out.Warnings, "the Gateway API CRDs the slice composed on wc1 are kept: Gateway ingress/edge use them, and removing the CRDs would remove those too")
	_, err = l.targets[wc1APIServer].Resource(detect.CRDGVR).Get(ctx, detect.GatewayAPICRD, metav1.GetOptions{})
	assert.NoError(t, err, "the Gateway CRD stays")
}

// gatewayAPILab is a serving lab whose slice composed the Gateway API CRDs on
// wc1 — its component's Job applied them — with the slice's connectivity
// child release still standing, and helm-controller's finalizer on every
// HelmRelease: a delete leaves it terminating until the test finishes the
// uninstall (finishUninstall).
func gatewayAPILab(t *testing.T) (*lab, *Service) {
	t.Helper()
	l, svc := servingLabOf(t, newLab(t, "installation.yaml").target(t, wc1APIServer, "wc1-configs.yaml").finalizing(t, wc1APIServer).withoutGatewayAPI(t, wc1APIServer))
	l.add(t, l.targets[wc1APIServer], "targets/gateway-api.yaml")
	finalizingResource(t, fakeInstallation(t, l), HelmReleaseGVR)
	connectivity := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease", "metadata": map[string]any{"name": "wc1-agent-platform-connectivity", "namespace": "org-acme"}}}
	_, err := l.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Create(context.Background(), connectivity, metav1.CreateOptions{})
	require.NoError(t, err)
	return l, svc
}

// finishUninstall is Flux finishing the uninstall of the named releases.
func finishUninstall(t *testing.T, l *lab, names ...string) {
	t.Helper()
	for _, name := range names {
		require.NoError(t, fakeInstallation(t, l).Tracker().Delete(HelmReleaseGVR, "org-acme", name))
	}
}

func gatewayAPICRDsOnWC1(t *testing.T, l *lab) []string {
	t.Helper()
	var left []string
	for _, name := range compose.GatewayAPICRDs {
		_, err := l.targets[wc1APIServer].Resource(detect.CRDGVR).Get(context.Background(), name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		require.NoError(t, err)
		left = append(left, name)
	}
	return left
}

func crdActions(out *WriteResult) []string {
	var acts []string
	for _, o := range out.Objects {
		if o.Kind == "CustomResourceDefinition" {
			acts = append(acts, o.Action+" "+o.Name)
		}
	}
	return acts
}

// TestDeleteNodePoolRemovesTheGatewayAPICRDsOnTheReRun
// (giantswarm/cluster-manager#203): the call's budget runs out while the
// connectivity release is uninstalled; the Gateway API CRDs the slice
// composed are pending — no manual step named —, the Gateway CRD marked as
// the slice's. The repeated call, the slice release gone by then, removes
// them: with the pool's release still terminating, and with the pool gone.
func TestDeleteNodePoolRemovesTheGatewayAPICRDsOnTheReRun(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		poolGone bool
	}{
		{name: "pool still uninstalling"},
		{name: "pool gone", poolGone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, svc := gatewayAPILab(t)

			short, cancel := context.WithTimeout(ctx, writeReserve+300*time.Millisecond)
			defer cancel()
			cut, err := svc.DeleteNodePool(short, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply})
			require.NoError(t, err)
			assert.True(t, cut.Partial)
			pending := make([]string, 0, len(compose.GatewayAPICRDs))
			for _, name := range gatewayAPICRDsInOrder() {
				pending = append(pending, actionPending+" "+name)
			}
			assert.Equal(t, pending, crdActions(cut))
			assert.Contains(t, cut.Warnings, "the Gateway API CRDs the slice composed on wc1 are pending: org-acme/wc1-agent-platform-connectivity is still being uninstalled — the re-run removes them once it is gone and nothing else uses them")
			for _, w := range cut.Warnings {
				assert.NotContains(t, w, "kubectl", "no manual step")
			}
			assert.Contains(t, cut.NextStep, "re-run with the same arguments")
			assert.Equal(t, []string{detect.GatewayAPICRD}, gatewayAPICRDsOnWC1(t, l), "the CRDs the component applied stay (the lab's bundle is the Gateway CRD)")
			gateway, err := l.targets[wc1APIServer].Resource(detect.CRDGVR).Get(ctx, detect.GatewayAPICRD, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, "org-acme/wc1-agent-platform", gateway.GetAnnotations()[GatewayAPIComposedAnnotation])

			finishUninstall(t, l, "wc1-agent-platform", "wc1-agent-platform-connectivity")
			if tc.poolGone {
				finishUninstall(t, l, "wc1-gpu-a10g")
			}
			again := deleteLastPool(t, svc, ctx, false, false)
			assert.False(t, again.Partial)
			deleted := make([]string, 0, len(compose.GatewayAPICRDs))
			for _, name := range gatewayAPICRDsInOrder() {
				deleted = append(deleted, actionDelete+" "+name)
			}
			assert.Equal(t, deleted, crdActions(again))
			assert.Empty(t, gatewayAPICRDsOnWC1(t, l), "no Gateway API CRD the slice composed remains")

			if tc.poolGone {
				_, err = svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply})
				var notFound *ErrNotFound
				require.ErrorAs(t, err, &notFound, "nothing of the pool or its slice is left")
			}
		})
	}
}

// TestTeardownLeavesGatewayAPICRDsItDidNotMark: a re-run that finds the
// slice release gone removes only the Gateway API CRDs marked as that
// cluster's slice's — CRDs marked by another cluster's slice, or installed by
// someone else, stay, and with nothing else left the re-run finds nothing.
func TestTeardownLeavesGatewayAPICRDsItDidNotMark(t *testing.T) {
	ctx := context.Background()
	l, svc := servingLab(t, "wc1-configs.yaml")
	gateway, err := l.targets[wc1APIServer].Resource(detect.CRDGVR).Get(ctx, detect.GatewayAPICRD, metav1.GetOptions{})
	require.NoError(t, err)
	gateway.SetAnnotations(map[string]string{GatewayAPIComposedAnnotation: "org-acme/wc2-agent-platform"})
	_, err = l.targets[wc1APIServer].Resource(detect.CRDGVR).Update(ctx, gateway, metav1.UpdateOptions{})
	require.NoError(t, err)

	out := disableWC1(t, svc, false)
	assert.Empty(t, crdActions(out), "the slice composed no CRDs")
	_, err = svc.DisableModelServing(ctx, serving("wc1", false))
	var notFound *ErrNotFound
	require.ErrorAs(t, err, &notFound, "another slice's mark is not this teardown's")
	assert.Equal(t, []string{detect.GatewayAPICRD}, gatewayAPICRDsOnWC1(t, l))
}
