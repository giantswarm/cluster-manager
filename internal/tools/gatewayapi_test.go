package tools

import (
	"context"
	"testing"

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

// platformRuns makes the installation's platform release report chart
// version as the one it runs (status.history[0].chartVersion).
func (l *lab) platformRuns(t *testing.T, version string) *lab {
	t.Helper()
	res := l.installation.Resource(HelmReleaseGVR).Namespace("flux-giantswarm")
	hr, err := res.Get(context.Background(), "agent-platform", metav1.GetOptions{})
	require.NoError(t, err)
	history, _, _ := unstructured.NestedSlice(hr.Object, "status", "history")
	require.NotEmpty(t, history)
	history[0].(map[string]any)["chartVersion"] = version
	require.NoError(t, unstructured.SetNestedSlice(hr.Object, history, "status", "history"))
	_, err = res.Update(context.Background(), hr, metav1.UpdateOptions{})
	require.NoError(t, err)
	return l
}

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
	_, err := l.service(Config{Installation: "gazelle"}).EnableModelServing(ctx, serving("wc1", true))
	var refused *ErrRefused
	require.ErrorAs(t, err, &refused)
	assert.Contains(t, refused.Reason, "below "+compose.MinGatewayAPICRDsChartVersion+", the first with the gateway-api-crds component", "an older chart composes no CRDs: refused, never left to a failing connectivity release")

	l.platformRuns(t, compose.MinGatewayAPICRDsChartVersion)
	svc := l.service(Config{Installation: "gazelle"})
	dry, err := svc.EnableModelServing(ctx, serving("wc1", true))
	require.NoError(t, err)
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
		l := newLab(t, "installation.yaml").withoutGatewayAPI(t, wc1APIServer).platformRuns(t, compose.MinGatewayAPICRDsChartVersion)
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
	assert.Equal(t, compose.GatewayAPICRDs, deleted, "every CRD of the bundle, the ones absent already included")
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
