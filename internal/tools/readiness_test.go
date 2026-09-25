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
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/detect"
)

// The backend block of serving.readiness tells "not registered" from "could
// not read" (giantswarm/cluster-manager#41): the ConfigMap cluster-manager
// wrote answers registered true, another cluster's document false, and a
// read failure carries the error with registered unknown — never a bare
// false. The block is read on the installation beside the target reads and
// has to survive them: assigned into the serving readiness concurrently, it
// was lost whenever the target reads finished last (the live check of
// v0.8.0; `go test -race` names the write).
func TestListClustersBackend(t *testing.T) {
	t.Run("registered for the cluster", func(t *testing.T) {
		l := newLab(t, "installation.yaml")
		cm := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{
				"name": compose.BackendConfigMapName, "namespace": "agent-platform",
				"labels": map[string]any{compose.LabelManagedBy: compose.ManagedBy, compose.LabelCluster: "wc2"},
			},
		}}
		_, err := l.installation.Resource(compose.ConfigMapGVR).Namespace("agent-platform").Create(context.Background(), cm, metav1.CreateOptions{})
		require.NoError(t, err)

		registered, notRegistered := true, false
		assert.Equal(t, detect.BackendState{Registered: &registered, Namespace: "agent-platform", Name: compose.BackendConfigMapName},
			clusterNamed(t, l, "wc2").Serving.Readiness.Backend)
		assert.Equal(t, detect.BackendState{Registered: &notRegistered}, clusterNamed(t, l, "wc1").Serving.Readiness.Backend,
			"another cluster's document is not this cluster's registration")
	})

	t.Run("not readable", func(t *testing.T) {
		l := newLab(t, "installation.yaml")
		fakeInstallation(t, l).PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(compose.ConfigMapGVR.GroupResource(), compose.BackendConfigMapName, errors.New("no RBAC on the namespace"))
		})
		got := clusterNamed(t, l, "wc2").Serving.Readiness.Backend
		assert.Nil(t, got.Registered, "unknown, not false")
		assert.Empty(t, got.Name)
		assert.Contains(t, got.Error, "get ConfigMap agent-platform/"+compose.BackendConfigMapName)
		assert.Contains(t, got.Error, "no RBAC on the namespace")
	})
}

// gpuOperator.readiness.operands says why it is empty: the DaemonSets could
// not be listed (operandsError), as opposed to the operator having created
// none yet (operandsMessage, TestDetectGPUOperator).
func TestGPUOperatorOperandsUnreadable(t *testing.T) {
	l := newLab(t, "installation.yaml").target(t, wc1APIServer, "clusterpolicy.yaml", servingAPIs...)
	fakeTarget(t, l, wc1APIServer).PrependReactor("list", detect.DaemonSetGVR.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("etcdserver: request timed out"))
	})
	got := wc1Cluster(t, l).GPUOperator.Readiness
	assert.Equal(t, []detect.OperandState{}, got.Operands, "empty, not null")
	assert.Contains(t, got.OperandsError, "DaemonSets of wc1 not readable")
	assert.Contains(t, got.OperandsError, "etcdserver: request timed out")
	assert.Empty(t, got.OperandsMessage, "a failed read is not scale-to-zero")
}

// The models Gateway is ready only with its https listener's certificate
// (giantswarm/cluster-manager#110): a Gateway reads Programmed while the
// listener's Certificate is never issued and every client fails the TLS
// handshake. Serving then reads not ready, naming the listener, the
// Certificate and the pending ACME challenge.
func TestListClustersModelsGateway(t *testing.T) {
	gateway := func(t *testing.T, fixture string) *detect.GatewayState {
		t.Helper()
		l := newLab(t, "installation.yaml")
		l.add(t, l.targets[wc2APIServer], fixture)
		serving := clusterNamed(t, l, "wc2").Serving
		gw := serving.Readiness.ModelsGateway
		require.NotNil(t, gw)
		return gw
	}

	t.Run("listener without its certificate", func(t *testing.T) {
		gw := gateway(t, "models-gateway-no-certificate.yaml")
		notReady := false
		assert.Equal(t, &notReady, gw.Ready)
		assert.Equal(t, "InvalidCertificateRef", gw.Reason)
		assert.Equal(t, "listener https not ready (ResolvedRefs=False [InvalidCertificateRef]); Certificate org-acme/models-tls not Ready: Issuing certificate as Secret does not exist; ACME challenge DNS-01 pending: Error presenting challenge: failed to determine Route 53 hosted zone ID: zone not found for _acme-challenge.models.wc2.acme.example.io.", gw.Message)
		assert.Equal(t, "https", gw.Listener.Name)
		assert.Equal(t, &notReady, gw.Listener.ResolvedRefs)
		require.NotNil(t, gw.Certificate)
		assert.Equal(t, "org-acme/models-tls", gw.Certificate.Namespace+"/"+gw.Certificate.Name)
		assert.Equal(t, &notReady, gw.Certificate.Ready)
		assert.Equal(t, "DoesNotExist", gw.Certificate.Reason)
		assert.Equal(t, "DNS-01 pending: Error presenting challenge: failed to determine Route 53 hosted zone ID: zone not found for _acme-challenge.models.wc2.acme.example.io.", gw.Certificate.Challenge)
	})

	t.Run("listener resolved, certificate pending", func(t *testing.T) {
		l := newLab(t, "installation.yaml")
		l.add(t, l.targets[wc2APIServer], "models-gateway-no-certificate.yaml")
		// The listener resolves a Secret left from before while the
		// Certificate renews and its challenge fails.
		gws := l.targets[wc2APIServer].Resource(detect.GatewayGVR).Namespace("org-acme")
		obj, err := gws.Get(context.Background(), "models", metav1.GetOptions{})
		require.NoError(t, err)
		listeners, _, _ := unstructured.NestedSlice(obj.Object, "status", "listeners")
		conds := listeners[0].(map[string]any)["conditions"].([]any)
		conds[1] = map[string]any{"type": "ResolvedRefs", "status": "True", "reason": "ResolvedRefs"}
		require.NoError(t, unstructured.SetNestedSlice(obj.Object, listeners, "status", "listeners"))
		_, err = gws.Update(context.Background(), obj, metav1.UpdateOptions{})
		require.NoError(t, err)

		serving := clusterNamed(t, l, "wc2").Serving
		gw := serving.Readiness.ModelsGateway
		require.NotNil(t, gw)
		notReady := false
		assert.Equal(t, &notReady, gw.Ready)
		assert.Equal(t, "DoesNotExist", gw.Reason)
		assert.Equal(t, "Certificate org-acme/models-tls not Ready: Issuing certificate as Secret does not exist; ACME challenge DNS-01 pending: Error presenting challenge: failed to determine Route 53 hosted zone ID: zone not found for _acme-challenge.models.wc2.acme.example.io.", gw.Message)
		assert.Contains(t, serving.Evidence, "Gateway org-acme/models not ready ("+gw.Message+")")
	})

	t.Run("certificate issued", func(t *testing.T) {
		gw := gateway(t, "models-gateway-ready.yaml")
		ready := true
		assert.Equal(t, &ready, gw.Ready)
		assert.Equal(t, "Programmed", gw.Reason)
		assert.Empty(t, gw.Message)
		assert.Equal(t, &ready, gw.Listener.Programmed)
		assert.Equal(t, &ready, gw.Listener.ResolvedRefs)
		require.NotNil(t, gw.Certificate)
		assert.Equal(t, &ready, gw.Certificate.Ready)
		assert.Empty(t, gw.Certificate.Challenge)
	})
}
