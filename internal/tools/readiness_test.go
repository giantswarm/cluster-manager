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
				"name": "model-backend-kserve-wc2", "namespace": "agent-platform",
				"labels": map[string]any{compose.LabelManagedBy: compose.ManagedBy, compose.LabelCluster: "wc2"},
			},
		}}
		_, err := l.installation.Resource(compose.ConfigMapGVR).Namespace("agent-platform").Create(context.Background(), cm, metav1.CreateOptions{})
		require.NoError(t, err)

		registered, notRegistered := true, false
		assert.Equal(t, detect.BackendState{Registered: &registered, Backend: "kserve-wc2", Namespace: "agent-platform", Name: "model-backend-kserve-wc2"},
			clusterNamed(t, l, "wc2").Serving.Readiness.Backend)
		assert.Equal(t, detect.BackendState{Registered: &notRegistered}, clusterNamed(t, l, "wc1").Serving.Readiness.Backend,
			"another cluster's document is not this cluster's registration")
	})

	t.Run("not readable", func(t *testing.T) {
		l := newLab(t, "installation.yaml")
		fakeInstallation(t, l).PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(compose.ConfigMapGVR.GroupResource(), "model-backend-kserve-wc2", errors.New("no RBAC on the namespace"))
		})
		got := clusterNamed(t, l, "wc2").Serving.Readiness.Backend
		assert.Nil(t, got.Registered, "unknown, not false")
		assert.Empty(t, got.Name)
		assert.Contains(t, got.Error, "get ConfigMap agent-platform/model-backend-kserve-wc2")
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

// Serving is not ready before the llmisvc admission webhook serves
// (giantswarm/cluster-manager#166): the controller's release reads Ready and
// its Deployment counts an available replica while the webhook Service has no
// ready endpoint or the CA bundle is not injected, and a load_model in that
// window fails admission with "failed calling webhook". The webhook is named
// in the evidence until its Service has a ready endpoint and every
// configuration carries its bundle.
func TestListClustersLLMISVCWebhook(t *testing.T) {
	const service = "Service agent-platform/llmisvc-webhook-server-service"
	webhook := func(t *testing.T, l *lab, cluster string) (ServingComponent, *detect.WebhookState) {
		t.Helper()
		serving := clusterNamed(t, l, cluster).Serving
		require.NotNil(t, serving.Readiness.Webhook)
		return serving, serving.Readiness.Webhook
	}
	ready, notReady := true, false

	t.Run("serving", func(t *testing.T) {
		serving, w := webhook(t, newLab(t, "installation.yaml"), "wc2")
		assert.Equal(t, &ready, w.Ready)
		assert.Equal(t, []string{"mutatingwebhookconfigurations/llminferenceservice.serving.kserve.io", "validatingwebhookconfigurations/llminferenceservice.serving.kserve.io"}, w.Configurations,
			"kyverno's TTL webhook admits */* and has no endpoint on wc2: not the llmisvc webhook")
		assert.Equal(t, "agent-platform/llmisvc-webhook-server-service", w.Namespace+"/"+w.Service)
		assert.Equal(t, 1, w.Endpoints)
		assert.Empty(t, w.Reason)
		assert.Empty(t, w.Message)
		for _, e := range serving.Evidence {
			assert.NotContains(t, e, "webhook", "a serving webhook holds nothing back")
		}
	})

	t.Run("no configuration yet", func(t *testing.T) {
		l := newLab(t, "installation.yaml")
		for _, gvr := range []schema.GroupVersionResource{detect.MutatingWebhookGVR, detect.ValidatingWebhookGVR} {
			require.NoError(t, l.targets[wc2APIServer].Resource(gvr).Delete(context.Background(), "llminferenceservice.serving.kserve.io", metav1.DeleteOptions{}))
		}
		serving, w := webhook(t, l, "wc2")
		assert.Equal(t, &notReady, w.Ready)
		assert.Equal(t, detect.WebhookNoConfiguration, w.Reason)
		assert.Equal(t, "no webhook configuration admits LLMInferenceServices yet", w.Message)
		assert.Equal(t, []string{}, w.Configurations, "empty, not null")
		assert.Empty(t, w.Service)
		assert.Contains(t, serving.Evidence, "llmisvc webhook not ready (no webhook configuration admits LLMInferenceServices yet)")
	})

	t.Run("endpoint absent", func(t *testing.T) {
		l := newLab(t, "installation.yaml")
		require.NoError(t, l.targets[wc2APIServer].Resource(detect.EndpointSliceGVR).Namespace("agent-platform").Delete(context.Background(), "llmisvc-webhook-server-service-x7k2p", metav1.DeleteOptions{}))
		serving, w := webhook(t, l, "wc2")
		assert.Equal(t, &notReady, w.Ready)
		assert.Equal(t, detect.WebhookNoEndpoints, w.Reason)
		assert.Equal(t, service+" has no endpoint yet", w.Message)
		assert.Equal(t, 0, w.Endpoints)
		assert.Contains(t, serving.Evidence, "llmisvc webhook not ready ("+service+" has no endpoint yet)")
	})

	t.Run("endpoint not ready", func(t *testing.T) {
		l := newLab(t, "installation.yaml").target(t, wc1APIServer, "chart-kserve.yaml")
		serving, w := webhook(t, l, "wc1")
		assert.Equal(t, &notReady, w.Ready)
		assert.Equal(t, detect.WebhookNoEndpoints, w.Reason)
		assert.Equal(t, service+" has no ready endpoint (0 of 1 ready)", w.Message)
		assert.Equal(t, 0, w.Endpoints)
		assert.Contains(t, serving.Evidence, "llmisvc webhook not ready ("+service+" has no ready endpoint (0 of 1 ready))")
	})

	t.Run("CA bundle not injected", func(t *testing.T) {
		l := newLab(t, "installation.yaml")
		configs := l.targets[wc2APIServer].Resource(detect.MutatingWebhookGVR)
		obj, err := configs.Get(context.Background(), "llminferenceservice.serving.kserve.io", metav1.GetOptions{})
		require.NoError(t, err)
		hooks, _, _ := unstructured.NestedSlice(obj.Object, "webhooks")
		unstructured.RemoveNestedField(hooks[0].(map[string]any), "clientConfig", "caBundle")
		require.NoError(t, unstructured.SetNestedSlice(obj.Object, hooks, "webhooks"))
		_, err = configs.Update(context.Background(), obj, metav1.UpdateOptions{})
		require.NoError(t, err)

		serving, w := webhook(t, l, "wc2")
		assert.Equal(t, &notReady, w.Ready)
		assert.Equal(t, detect.WebhookNoCABundle, w.Reason)
		assert.Equal(t, "caBundle not injected yet on llminferenceservice.serving.kserve.io/llminferenceservice.kserve-webhook-server.v1alpha2.defaulter", w.Message)
		assert.Equal(t, 1, w.Endpoints, "the endpoint is ready, the bundle holds it back")
		assert.Contains(t, serving.Evidence, "llmisvc webhook not ready ("+w.Message+")")
	})

	t.Run("endpoints not readable", func(t *testing.T) {
		l := newLab(t, "installation.yaml")
		fakeTarget(t, l, wc2APIServer).PrependReactor("list", detect.EndpointSliceGVR.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(detect.EndpointSliceGVR.GroupResource(), "", errors.New("no RBAC on endpointslices"))
		})
		serving, w := webhook(t, l, "wc2")
		assert.Nil(t, w.Ready, "unknown, not false")
		assert.Empty(t, w.Reason)
		assert.Contains(t, w.Message, "EndpointSlices of "+service+" not readable")
		assert.Contains(t, w.Message, "no RBAC on endpointslices")
		assert.Contains(t, serving.Evidence, "llmisvc webhook readiness unknown ("+w.Message+")")
	})
}

// The slice's meta release reports Ready once its own manifests — the
// children's HelmReleases — are applied, about 30 s before the children have
// installed, kserve-llmisvc-resources last (giantswarm/cluster-manager#177):
// a caller reading the slice from the parent alone concluded it was ready
// while a child still installed, and a load_model in that window failed.
// serving.readiness.release is ready only once every child is, naming the
// pending child with its condition until then; the parent's own condition
// stands while it is not Ready itself.
func TestListClustersSliceReleaseChildren(t *testing.T) {
	ready, notReady := true, false
	const (
		parent    = "wc1-agent-platform"
		llmisvc   = "wc1-kserve-llmisvc-resources"
		installed = "2026-10-07T09:00:00Z"
	)
	condition := func(status, reason, message string) []any {
		return []any{map[string]any{"type": "Ready", "status": status, "reason": reason, "message": message, "lastTransitionTime": installed}}
	}
	// sliceLab is wc1 with cluster-manager's slice release in the state
	// helm-controller reports and the children of the fixture beside it.
	sliceLab := func(t *testing.T, parentCondition []any) *lab {
		t.Helper()
		l := newLab(t, "installation.yaml")
		_, err := l.service(Config{Installation: "gazelle"}).EnableModelServing(context.Background(), serving("wc1", false))
		require.NoError(t, err)
		releases := l.installation.Resource(HelmReleaseGVR).Namespace("org-acme")
		hr, err := releases.Get(context.Background(), parent, metav1.GetOptions{})
		require.NoError(t, err)
		require.NoError(t, unstructured.SetNestedSlice(hr.Object, parentCondition, "status", "conditions"))
		_, err = releases.Update(context.Background(), hr, metav1.UpdateOptions{})
		require.NoError(t, err)
		for _, child := range loadFixtures(t, "slice-children-installing.yaml") {
			_, err := releases.Create(context.Background(), child.(*unstructured.Unstructured), metav1.CreateOptions{})
			require.NoError(t, err)
		}
		return l
	}
	release := func(t *testing.T, l *lab) (ServingComponent, *detect.ReleaseState) {
		t.Helper()
		serving := clusterNamed(t, l, "wc1").Serving
		require.NotNil(t, serving.Readiness.Release)
		return serving, serving.Readiness.Release
	}
	const pending = "HelmRelease org-acme/wc1-kserve-llmisvc-resources not Ready (Ready=Unknown [Progressing] Running 'install' action with timeout of 10m0s)"

	t.Run("a child still installing", func(t *testing.T) {
		serving, r := release(t, sliceLab(t, condition("True", "InstallSucceeded", "")))
		assert.Equal(t, &notReady, r.Ready, "the parent reads Ready; the slice is not")
		assert.Equal(t, detect.ReasonChildNotReady, r.Reason)
		assert.Equal(t, pending, r.Message, "the pending child, with helm-controller's own account")
		assert.Equal(t, installed, r.Since, "the parent's own transition")
		assert.Contains(t, serving.Evidence, pending)
		assert.Contains(t, serving.Evidence, "HelmRelease org-acme/"+parent)
		assert.Len(t, serving.Readiness.Children, 3)
	})

	t.Run("every child Ready", func(t *testing.T) {
		l := sliceLab(t, condition("True", "InstallSucceeded", ""))
		releases := l.installation.Resource(HelmReleaseGVR).Namespace("org-acme")
		hr, err := releases.Get(context.Background(), llmisvc, metav1.GetOptions{})
		require.NoError(t, err)
		require.NoError(t, unstructured.SetNestedSlice(hr.Object, condition("True", "InstallSucceeded", ""), "status", "conditions"))
		_, err = releases.Update(context.Background(), hr, metav1.UpdateOptions{})
		require.NoError(t, err)

		serving, r := release(t, l)
		assert.Equal(t, &ready, r.Ready)
		assert.Equal(t, "InstallSucceeded", r.Reason, "the parent's own reason")
		assert.Empty(t, r.Message)
		for _, e := range serving.Evidence {
			assert.NotContains(t, e, "not Ready", "nothing holds the slice back")
		}
	})

	t.Run("the parent not Ready itself", func(t *testing.T) {
		_, r := release(t, sliceLab(t, condition("False", "InstallFailed", "Helm install failed for release org-acme/wc1-agent-platform: timed out waiting for the condition")))
		assert.Equal(t, &notReady, r.Ready)
		assert.Equal(t, "InstallFailed", r.Reason, "the parent's own account, not the child's")
		assert.Equal(t, "Helm install failed for release org-acme/wc1-agent-platform: timed out waiting for the condition", r.Message)
	})

	t.Run("the parent without a condition yet", func(t *testing.T) {
		_, r := release(t, sliceLab(t, []any{}))
		assert.Nil(t, r.Ready, "null until the parent reports one: nothing to hold back")
		assert.Empty(t, r.Reason)
	})
}
