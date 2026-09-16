package tools

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
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
)

// The apply of create_node_pool against the aggregator's deadline
// (giantswarm/cluster-manager#34): the write order, the partial answer, a
// failed write, a refusal on a late object.

// writeLog records the writes the installation's fake apiserver sees, in
// order, as `<verb> <Kind> <name>`.
type writeLog struct {
	mu     sync.Mutex
	writes []string
}

func (w *writeLog) seen() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.writes...)
}

func (w *writeLog) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = nil
}

// recordWrites installs the recording reactors on the lab's installation;
// the writes go on to the fake's tracker.
func recordWrites(t *testing.T, l *lab) *writeLog {
	t.Helper()
	log := &writeLog{}
	for _, verb := range []string{"create", "update"} {
		fakeInstallation(t, l).PrependReactor(verb, "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
			obj := objectOf(t, a)
			log.mu.Lock()
			log.writes = append(log.writes, a.GetVerb()+" "+obj.GetKind()+" "+obj.GetName())
			log.mu.Unlock()
			return false, nil, nil
		})
	}
	return log
}

func fakeInstallation(t *testing.T, l *lab) *dynamicfake.FakeDynamicClient {
	t.Helper()
	dyn, ok := l.installation.(*dynamicfake.FakeDynamicClient)
	require.True(t, ok)
	return dyn
}

// objectOf is the object a create or update action carries.
func objectOf(t *testing.T, a k8stesting.Action) *unstructured.Unstructured {
	t.Helper()
	withObject, ok := a.(interface{ GetObject() runtime.Object })
	require.True(t, ok, "%s carries no object", a.GetVerb())
	obj, ok := withObject.GetObject().(*unstructured.Unstructured)
	require.True(t, ok)
	return obj
}

// The composed objects of wc1's gpu-l4 pool in the order they land.
func wc1PoolWrites() []string {
	return []string{
		"create OCIRepository wc1-gpu-l4",
		"create Secret " + compose.ValuesSecretName("wc1", "gpu-l4"),
		"create HelmRelease wc1-gpu-l4",
		"create OCIRepository " + compose.SliceReleaseName("wc1"),
		"create HelmRelease " + compose.SliceReleaseName("wc1"),
		"create ConfigMap " + compose.BackendConfigMapName,
		"create OCIRepository " + compose.OperatorReleaseName("wc1"),
		"create HelmRelease " + compose.OperatorReleaseName("wc1"),
	}
}

// TestCreateNodePoolWriteOrder: the objects land pool, slice, the slice's
// backend registration, operator — the ConfigMap right after the slice's
// HelmRelease, so a call that does not reach the end never leaves a slice
// model-manager does not know — and on a re-run the objects that are
// missing before the updates. The answer keeps the composed order.
func TestCreateNodePoolWriteOrder(t *testing.T) {
	l := newLab(t, "installation.yaml")
	svc := l.service(Config{Installation: "gazelle"})
	ctx := context.Background()
	log := recordWrites(t, l)

	out, err := svc.CreateNodePool(ctx, l4("wc1", "gpu-l4", false))
	require.NoError(t, err)
	assert.Equal(t, wc1PoolWrites(), log.seen())
	assert.Equal(t, []string{"create", "create", "create", "create", "create", "create", "create", "create"}, actions(out))

	// The backend registration is gone and the pool grows: the missing
	// object is written before the update.
	require.NoError(t, l.installation.Resource(ConfigMapGVR).Namespace("agent-platform").Delete(ctx, compose.BackendConfigMapName, metav1.DeleteOptions{}))
	bigger := l4("wc1", "gpu-l4", false)
	bigger.Pool.MaxGPUs = 8
	log.reset()
	again, err := svc.CreateNodePool(ctx, bigger)
	require.NoError(t, err)
	assert.Equal(t, []string{"create ConfigMap " + compose.BackendConfigMapName, "update HelmRelease wc1-gpu-l4"}, log.seen(), "what is missing first, then the updates")
	assert.Equal(t, []string{"unchanged", "unchanged", "update", "unchanged", "unchanged", "create", "unchanged", "unchanged"}, actions(again), "the answer keeps the composed order")
	assert.False(t, again.Partial)
}

// TestCreateNodePoolAnswersPartialWithinTheBudget: a write about to start
// with less than the reserve of the budget left is not started; the answer
// goes out in time, marks the objects not reached pending and names the
// re-run, which writes them first. The budget is the request's own deadline
// when the caller set one, else the configured apply budget.
func TestCreateNodePoolAnswersPartialWithinTheBudget(t *testing.T) {
	// A write that takes slow leaves less than the reserve once the first
	// one is done: the budget is the reserve plus a head start the reads
	// (on the fake, milliseconds) fit into.
	const slow, head = 900 * time.Millisecond, 700 * time.Millisecond
	budget := writeReserve + head

	for name, tc := range map[string]struct {
		cfg Config
		ctx func() (context.Context, context.CancelFunc)
	}{
		"request deadline": {
			cfg: Config{Installation: "gazelle"},
			ctx: func() (context.Context, context.CancelFunc) { return context.WithTimeout(context.Background(), budget) },
		},
		"configured budget": {
			cfg: Config{Installation: "gazelle", ApplyBudget: budget},
			ctx: func() (context.Context, context.CancelFunc) { return context.Background(), func() {} },
		},
	} {
		t.Run(name, func(t *testing.T) {
			l := newLab(t, "installation.yaml")
			svc := l.service(tc.cfg)
			var slowWrites atomic.Bool
			slowWrites.Store(true)
			fakeInstallation(t, l).PrependReactor("create", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
				if slowWrites.Load() {
					time.Sleep(slow)
				}
				return false, nil, nil
			})

			ctx, cancel := tc.ctx()
			defer cancel()
			out, err := svc.CreateNodePool(ctx, l4("wc1", "gpu-l4", false))
			require.NoError(t, err, "the answer goes out instead of the call being cancelled mid-write")
			assert.True(t, out.Partial)
			assert.Equal(t, []string{"create", "pending", "pending", "pending", "pending", "pending", "pending", "pending"}, actions(out))
			assert.Contains(t, out.NextStep, "7 of 8 object(s) are pending")
			assert.Contains(t, out.NextStep, "re-run with the same arguments")
			assert.Len(t, out.Manifests, 8, "the manifests are the composed objects, pending ones included")
			_, err = l.installation.Resource(compose.OCIRepositoryGVR).Namespace("org-acme").Get(context.Background(), "wc1-gpu-l4", metav1.GetOptions{})
			require.NoError(t, err, "the first write landed")
			_, err = l.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Get(context.Background(), "wc1-gpu-l4", metav1.GetOptions{})
			assert.True(t, apierrors.IsNotFound(err), "the second was not started")

			// The re-run completes the pool.
			slowWrites.Store(false)
			log := recordWrites(t, l)
			again, err := svc.CreateNodePool(context.Background(), l4("wc1", "gpu-l4", false))
			require.NoError(t, err)
			assert.False(t, again.Partial)
			assert.Equal(t, []string{"unchanged", "create", "create", "create", "create", "create", "create", "create"}, actions(again))
			assert.Equal(t, wc1PoolWrites()[1:], log.seen())
		})
	}
}

// TestCreateNodePoolFailedWriteLeavesTheBackendRegistered: the operator's
// source fails to create — the seventh write, after the slice's HelmRelease
// and its backend registration — so the slice is known to model-manager, and
// the re-run writes the two objects that are missing.
func TestCreateNodePoolFailedWriteLeavesTheBackendRegistered(t *testing.T) {
	l := newLab(t, "installation.yaml")
	svc := l.service(Config{Installation: "gazelle"})
	ctx := context.Background()
	log := recordWrites(t, l)
	var fail atomic.Bool
	fail.Store(true)
	fakeInstallation(t, l).PrependReactor("create", "ocirepositories", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if fail.Load() && objectOf(t, a).GetName() == compose.OperatorReleaseName("wc1") {
			return true, nil, apierrors.NewInternalError(errors.New("etcdserver: request timed out"))
		}
		return false, nil, nil
	})

	_, err := svc.CreateNodePool(ctx, l4("wc1", "gpu-l4", false))
	require.ErrorContains(t, err, "create OCIRepository org-acme/"+compose.OperatorReleaseName("wc1"))
	assert.Equal(t, wc1PoolWrites()[:6], log.seen(), "six writes landed before the failure")
	_, err = l.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Get(ctx, compose.SliceReleaseName("wc1"), metav1.GetOptions{})
	require.NoError(t, err, "the slice's HelmRelease landed")
	_, err = l.installation.Resource(ConfigMapGVR).Namespace("agent-platform").Get(ctx, compose.BackendConfigMapName, metav1.GetOptions{})
	require.NoError(t, err, "its backend registration right after it: model-manager knows the slice")

	fail.Store(false)
	log.reset()
	again, err := svc.CreateNodePool(ctx, l4("wc1", "gpu-l4", false))
	require.NoError(t, err)
	assert.Equal(t, wc1PoolWrites()[6:], log.seen(), "the re-run writes what is missing")
	assert.Equal(t, []string{"unchanged", "unchanged", "unchanged", "unchanged", "unchanged", "unchanged", "create", "create"}, actions(again))
}

// TestCreateNodePoolRefusesAnOwnedObjectBeforeAnyWrite: the operator's
// HelmRelease — the last object to land — exists and is GitOps-owned; the
// refusal comes before the first write, not after six of them.
func TestCreateNodePoolRefusesAnOwnedObjectBeforeAnyWrite(t *testing.T) {
	l := newLab(t, "installation.yaml")
	ctx := context.Background()
	owned := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": HelmReleaseGVR.GroupVersion().String(), "kind": "HelmRelease",
		"metadata": map[string]any{"name": compose.OperatorReleaseName("wc1"), "namespace": "org-acme", "labels": map[string]any{labelKustomizeName: "flux-acme"}},
	}}
	_, err := l.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Create(ctx, owned, metav1.CreateOptions{})
	require.NoError(t, err)
	log := recordWrites(t, l)

	_, err = l.service(Config{Installation: "gazelle"}).CreateNodePool(ctx, l4("wc1", "gpu-l4", false))
	assertRefused(t, err, "HelmRelease org-acme/"+compose.OperatorReleaseName("wc1")+" exists and is owned by GitOps (Flux Kustomization flux-acme)")
	assert.Empty(t, log.seen(), "every refusal comes before any write")
	assertNothingLanded(t, l, "wc1-gpu-l4")
}
