package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/detect"
)

// wc1Cluster is wc1 as list_clusters reports it over the lab.
func wc1Cluster(t *testing.T, l *lab) Cluster {
	t.Helper()
	answer, err := l.service(Config{Installation: "gazelle"}).ListClusters(context.Background())
	require.NoError(t, err)
	clusters := answer.Clusters
	for _, c := range clusters {
		if c.Name == "wc1" {
			return c
		}
	}
	t.Fatal("wc1 not listed")
	return Cluster{}
}

// TestDetectGPUOperator runs every branch of the detection over wc1's
// apiserver: the platform's chart component, a HelmRelease or ClusterPolicy
// by hand, GPU node labels, nothing, and a cluster that cannot be read.
func TestDetectGPUOperator(t *testing.T) {
	cases := []struct {
		fixture  string
		want     detect.Component
		evidence []string
	}{
		{"chart.yaml", detect.Component{Status: detect.StatusPresent, Provider: detect.ProviderChart}, []string{
			"ClusterPolicy cluster-policy", "HelmRelease agent-platform/gpu-operator", "node wc1-gpu-1 labelled nvidia.com/gpu.present=true"}},
		{"manual-helmrelease.yaml", detect.Component{Status: detect.StatusPresent, Provider: detect.ProviderManual}, []string{"HelmRelease gpu-operator/nvidia"}},
		{"clusterpolicy.yaml", detect.Component{Status: detect.StatusPresent, Provider: detect.ProviderManual}, []string{"ClusterPolicy cluster-policy"}},
		{"node-labels.yaml", detect.Component{Status: detect.StatusPresent, Provider: detect.ProviderManual}, []string{
			"node wc1-gpu-1 labelled nvidia.com/gpu.present=true", "node wc1-gpu-2 advertises nvidia.com/gpu"}},
		{"unknown.yaml", detect.Component{Status: detect.StatusAbsent}, nil},
		{"wc1.yaml", detect.Component{Status: detect.StatusAbsent}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			l := newLab(t, "installation.yaml").target(t, wc1APIServer, tc.fixture, servingAPIs...)
			got := wc1Cluster(t, l).GPUOperator
			tc.want.Evidence = tc.evidence
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("unreachable", func(t *testing.T) {
		got := wc1Cluster(t, newLab(t, "installation.yaml").unreachable(wc1APIServer)).GPUOperator
		assert.Equal(t, detect.StatusUnknown, got.Status)
		assert.Contains(t, got.Reason, "not readable as you through "+wc1APIServer)
	})

	t.Run("cluster-manager's own release", func(t *testing.T) {
		l := newLab(t, "installation.yaml")
		_, err := l.service(Config{Installation: "gazelle"}).CreateNodePool(context.Background(), l4("wc1", "gpu-l4", false))
		require.NoError(t, err)
		got := wc1Cluster(t, l).GPUOperator
		assert.Equal(t, detect.Component{Status: detect.StatusPresent, Provider: detect.ProviderClusterManager, Evidence: []string{"HelmRelease org-acme/wc1-gpu-operator"}}, got,
			"the release in org-acme targets wc1 through its kubeconfig; the installation side sees it even when the cluster does not")
	})
}

// TestDetectServing runs every branch of the serving detection over wc1's
// apiserver: the platform's chart (its KServe releases and controllers, or
// its discovery ConfigMap), a controller by hand, the KServe CRDs alone —
// what Helm leaves behind when a serving layer goes — and nothing.
func TestDetectServing(t *testing.T) {
	cases := []struct {
		fixture  string
		absent   []schema.GroupVersionResource
		want     detect.Component
		evidence []string
	}{
		{"chart-kserve.yaml", nil, detect.Component{Status: detect.StatusPresent, Provider: detect.ProviderChart}, []string{
			"Deployment agent-platform/kserve-controller-manager (1/1 ready)", "Deployment agent-platform/llmisvc-controller-manager (0/1 ready)",
			"HelmRelease agent-platform/kserve-llmisvc-resources", "HelmRelease agent-platform/kserve-resources",
			"KServe API serving.kserve.io/v1beta1 served", "llmisvc API serving.kserve.io/v1alpha1 served"}},
		{"wc2.yaml", nil, detect.Component{Status: detect.StatusPresent, Provider: detect.ProviderChart}, []string{
			"KServe API serving.kserve.io/v1beta1 served", "discovery ConfigMap agent-platform/agent-platform-model-serving", "llmisvc API serving.kserve.io/v1alpha1 served"}},
		{"manual-kserve.yaml", nil, detect.Component{Status: detect.StatusPresent, Provider: detect.ProviderManual}, []string{
			"Deployment kserve/kserve-controller-manager (1/1 ready)", "KServe API serving.kserve.io/v1beta1 served", "llmisvc API serving.kserve.io/v1alpha1 served"}},
		{"crds-only.yaml", nil, detect.Component{Status: detect.StatusAbsent}, []string{
			"KServe API serving.kserve.io/v1beta1 served (CRDs only, no controller)", "llmisvc API serving.kserve.io/v1alpha1 served (CRDs only, no controller)"}},
		{"wc1.yaml", servingAPIs, detect.Component{Status: detect.StatusAbsent}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			l := newLab(t, "installation.yaml").target(t, wc1APIServer, tc.fixture, tc.absent...)
			got := wc1Cluster(t, l).Serving
			tc.want.Evidence = tc.evidence
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("CRDs alone compose the slice", func(t *testing.T) {
		l := newLab(t, "installation.yaml").target(t, wc1APIServer, "crds-only.yaml")
		out, err := l.service(Config{Installation: "gazelle"}).CreateNodePool(context.Background(), l4("wc1", "gpu-l4", true))
		require.NoError(t, err)
		assert.Equal(t, detect.StatusAbsent, out.Serving.Status)
		require.NotNil(t, out.Slice, "the CRDs a serving layer left behind serve no model: the slice is composed")
		assert.Equal(t, "wc1-agent-platform", out.Slice.Name)
		require.Len(t, out.Objects, 8, "pool source, Secret, release; operator source, release; slice source, release; backend")
		assertGolden(t, "serving_crds_only", out.Serving)
	})

	t.Run("cluster-manager's own slice release", func(t *testing.T) {
		l := newLab(t, "installation.yaml")
		svc := l.service(Config{Installation: "gazelle"})
		_, err := svc.EnableModelServing(context.Background(), serving("wc1", false))
		require.NoError(t, err)
		l.target(t, wc1APIServer, "chart-kserve.yaml")
		got := wc1Cluster(t, l).Serving
		assert.Equal(t, detect.ProviderClusterManager, got.Provider, "the controllers on the cluster are the slice release's children, whatever their chart label says")
		assert.Contains(t, got.Evidence, "HelmRelease org-acme/wc1-agent-platform")
		assert.Contains(t, got.Evidence, "Deployment agent-platform/kserve-controller-manager (1/1 ready)")
	})
}

// TestCreateNodePoolComposesTheOperator: wc1 is Flatcar without an operator,
// so the pool comes with the `<cluster>-gpu-operator` release of the
// table's Flatcar row, kubeconfig-targeted, and the kserve backend
// registration; a second call changes nothing.
func TestCreateNodePoolComposesTheOperator(t *testing.T) {
	l := newLab(t, "installation.yaml")
	svc := l.service(Config{Installation: "gazelle"})
	ctx := context.Background()
	out, err := svc.CreateNodePool(ctx, l4("wc1", "gpu-l4", false))
	require.NoError(t, err)
	assert.Equal(t, detect.Component{Status: detect.StatusAbsent}, out.GPUOperator)
	assert.Equal(t, compose.RowFlatcar.Name, out.OperatorRow)
	assert.Equal(t, []string{"create", "create", "create", "create", "create", "create", "create", "create"}, actions(out), "pool source, Secret, release; operator source, release; slice source, release; backend")

	hr := out.Manifests[4]
	assert.Equal(t, "wc1-gpu-operator", hr["metadata"].(map[string]any)["name"])
	kubeconfig, _, _ := unstructured.NestedString(hr, "spec", "kubeConfig", "secretRef", "name")
	assert.Equal(t, "wc1-kubeconfig", kubeconfig, "a workload cluster is targeted through its kubeconfig Secret")
	values, _, _ := unstructured.NestedMap(hr, "spec", "values", "gpu-operator")
	assert.Equal(t, map[string]any{"driver": map[string]any{"enabled": false}, "toolkit": map[string]any{"enabled": false}}, values, "the Flatcar row")

	slice := out.Manifests[6]
	assert.Equal(t, "wc1-agent-platform", slice["metadata"].(map[string]any)["name"])
	assert.Equal(t, detect.Component{Status: detect.StatusAbsent}, out.Serving, "nothing served on wc1: the slice is composed")
	assert.Equal(t, &SliceRelease{Name: "wc1-agent-platform", Namespace: "org-acme", ChartVersion: "4.27.2", Domain: "wc1.acme.example.io", ModelsHost: "models.wc1.acme.example.io"}, out.Slice, "wc1 has two pools now: the predictors are placed by their GPU request alone")
	target, _, _ := unstructured.NestedString(slice, "spec", "values", "gitops", "target", "kubeConfig", "secretRef", "name")
	assert.Equal(t, "wc1-kubeconfig", target, "the target knob: the components install into the workload cluster")

	backend := out.Manifests[7]
	assert.Equal(t, &BackendRegistration{Kind: "kserve", Namespace: "agent-platform", Name: "model-backend-kserve", Target: "wc1 (" + wc1APIServer + ")"}, out.Backend)
	doc, _, _ := unstructured.NestedString(backend, "data", "backend.yaml")
	assert.Contains(t, doc, "apiServer: "+wc1APIServer)
	assert.Contains(t, doc, "-----BEGIN CERTIFICATE-----", "the CA from the kubeconfig's cluster section")
	assert.NotContains(t, doc, "client-certificate", "never the kubeconfig's user section")
	assert.Contains(t, doc, "servingNamespace: model-serving")

	again, err := svc.CreateNodePool(ctx, l4("wc1", "gpu-l4", false))
	require.NoError(t, err)
	assert.Equal(t, []string{"unchanged", "unchanged", "unchanged", "unchanged", "unchanged", "unchanged", "unchanged", "unchanged"}, actions(again), "idempotent, the slice and the backend too")
	assert.Equal(t, detect.ProviderClusterManager, again.Serving.Provider, "the re-run meets its own slice release and updates it in place")
}

// TestCreateNodePoolNeverRecreatesAChartOperator: the platform's release
// provides the operator on wc1 — nothing of it is composed, the pool and the
// backend still are.
func TestCreateNodePoolNeverRecreatesAChartOperator(t *testing.T) {
	l := newLab(t, "installation.yaml").target(t, wc1APIServer, "chart.yaml")
	out, err := l.service(Config{Installation: "gazelle"}).CreateNodePool(context.Background(), l4("wc1", "gpu-l4", true))
	require.NoError(t, err)
	assert.Equal(t, detect.ProviderChart, out.GPUOperator.Provider)
	assert.Empty(t, out.OperatorRow)
	assert.Equal(t, []string{"would-create", "would-create", "would-create", "would-create"}, actions(out), "pool source, Secret, release; backend — no operator")
	for _, o := range out.Objects {
		assert.NotEqual(t, "wc1-gpu-operator", o.Name)
	}
	assert.Equal(t, detect.ProviderChart, out.GPUOperator.Provider)
	assertGolden(t, "create_node_pool_chart_operator", out)
}

// TestCreateNodePoolRefusesBeforeWriting: no row for the nodes, a cluster
// that cannot be read, and model-manager's kserve document held by another
// cluster each refuse with the fix — and nothing lands.
func TestCreateNodePoolRefusesBeforeWriting(t *testing.T) {
	ctx := context.Background()

	t.Run("unknown row", func(t *testing.T) {
		l := newLab(t, "installation.yaml").target(t, wc1APIServer, "unknown.yaml", servingAPIs...)
		_, err := l.service(Config{Installation: "gazelle"}).CreateNodePool(ctx, l4("wc1", "gpu-l4", false))
		assertRefused(t, err, "OS images seen: Ubuntu 24.04.2 LTS")
		assertRefused(t, err, "nvidia.com labels seen: nvidia.com/gpu.deploy.driver=true")
		assertRefused(t, err, "row 1 Flatcar")
		assertRefused(t, err, "row 2 pre-installed driver")
		assertRefused(t, err, "giantswarm/giantswarm#37611")
		assertNothingLanded(t, l, "wc1-gpu-l4")
	})

	t.Run("unreadable cluster", func(t *testing.T) {
		l := newLab(t, "installation.yaml").unreachable(wc1APIServer)
		_, err := l.service(Config{Installation: "gazelle"}).CreateNodePool(ctx, l4("wc1", "gpu-l4", false))
		assertRefused(t, err, "cannot tell whether a GPU operator runs on wc1")
		assertNothingLanded(t, l, "wc1-gpu-l4")
	})

	t.Run("backend registered for another cluster", func(t *testing.T) {
		l := newLab(t, "installation.yaml")
		svc := l.service(Config{Installation: "gazelle"})
		_, err := svc.CreateNodePool(ctx, l4("wc1", "gpu-l4", false))
		require.NoError(t, err)
		_, err = svc.CreateNodePool(ctx, l4("wc2", "gpu-l4b", false))
		assertRefused(t, err, "is registered for cluster wc1")
		assertNothingLanded(t, l, "wc2-gpu-l4b")
	})
}

// TestDeleteLastPoolRemovesOperatorAndBackend: with pools gpu-a10g (fixture)
// and gpu-l4 (created) on wc1, deleting gpu-l4 leaves the operator and the
// backend; deleting gpu-a10g, the last, takes them.
func TestDeleteLastPoolRemovesOperatorAndBackend(t *testing.T) {
	l := newLab(t, "installation.yaml")
	svc := l.service(Config{Installation: "gazelle"})
	ctx := context.Background()
	_, err := svc.CreateNodePool(ctx, l4("wc1", "gpu-l4", false))
	require.NoError(t, err)

	first, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-l4", Mode: ModeApply})
	require.NoError(t, err)
	assert.False(t, first.LastPool)
	assert.Equal(t, []string{"delete", "delete", "delete"}, actions(first), "release, source, values Secret — the operator stays with gpu-a10g")

	dry, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply, Force: true, DryRun: true})
	require.NoError(t, err)
	assert.True(t, dry.LastPool)
	names := make([]string, 0, len(dry.Objects))
	for _, o := range dry.Objects {
		names = append(names, o.Kind+" "+o.Namespace+"/"+o.Name)
	}
	assert.Equal(t, []string{
		"HelmRelease org-acme/wc1-gpu-a10g", "OCIRepository org-acme/wc1-gpu-a10g",
		"HelmRelease org-acme/wc1-gpu-operator", "OCIRepository org-acme/wc1-gpu-operator",
		"HelmRelease org-acme/wc1-agent-platform", "OCIRepository org-acme/wc1-agent-platform",
		"ConfigMap agent-platform/model-backend-kserve",
	}, names)
	assertGolden(t, "delete_node_pool_last_pool", dry)

	last, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "wc1", Name: "gpu-a10g", Mode: ModeApply, Force: true})
	require.NoError(t, err)
	assert.Len(t, last.Objects, 7)
	assert.Empty(t, last.SliceKept)
	assert.Equal(t, detect.StatusAbsent, wc1Cluster(t, l).GPUOperator.Status, "nothing of cluster-manager's remains")
	assert.Equal(t, detect.StatusAbsent, wc1Cluster(t, l).Serving.Status, "the slice release went with the last pool")

	l.target(t, wc1APIServer, "crds-only.yaml")
	assert.Equal(t, detect.StatusAbsent, wc1Cluster(t, l).Serving.Status, "the KServe CRDs Helm left behind are not a serving layer")
	next, err := svc.CreateNodePool(ctx, l4("wc1", "gpu-l4", true))
	require.NoError(t, err)
	assert.NotNil(t, next.Slice, "the next pool composes the slice again")
	assert.NotNil(t, next.Backend, "and registers the backend")
}

// assertNothingLanded checks that a refused create left no release.
func assertNothingLanded(t *testing.T, l *lab, release string) {
	t.Helper()
	hrs, err := l.installation.Resource(HelmReleaseGVR).Namespace("org-acme").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	for i := range hrs.Items {
		assert.NotEqual(t, release, hrs.Items[i].GetName(), "a refusal writes nothing")
	}
}
