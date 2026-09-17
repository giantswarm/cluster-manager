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
