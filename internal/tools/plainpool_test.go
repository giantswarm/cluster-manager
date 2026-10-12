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
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/detect"
)

// forbidIssuers makes every read of a ClusterIssuer on the cluster at
// apiServer forbidden, as for a person bound to the view role only.
func forbidIssuers(t *testing.T, l *lab, apiServer string) {
	t.Helper()
	fakeTarget(t, l, apiServer).PrependReactor("*", detect.ClusterIssuerGVR.Resource, func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(detect.ClusterIssuerGVR.GroupResource(), compose.DefaultCertificateIssuer, errors.New(`User "jane" cannot get resource "clusterissuers" in API group "cert-manager.io" at the cluster scope`))
	})
}

// issuerReads counts the ClusterIssuer reads (not the fixture's create) on the cluster at apiServer.
func issuerReads(t *testing.T, l *lab, apiServer string) int {
	t.Helper()
	n := 0
	for _, a := range fakeTarget(t, l, apiServer).Actions() {
		if a.GetResource().Resource == detect.ClusterIssuerGVR.Resource && (a.GetVerb() == "get" || a.GetVerb() == "list") {
			n++
		}
	}
	return n
}

func plain(cluster, name string, dryRun bool) CreateNodePoolInput {
	in := l4(cluster, name, dryRun)
	in.Cache, in.NoServing = nil, true
	return in
}

// TestPlainPoolReadsNoClusterIssuer (giantswarm/cluster-manager#238): a pool
// created with serving false composes no slice and registers no backend, so
// the ClusterIssuer is never read — a person who may not read it at cluster
// scope creates the pool, the dry run and the apply alike.
func TestPlainPoolReadsNoClusterIssuer(t *testing.T) {
	l := newLab(t, "installation.yaml")
	forbidIssuers(t, l, wc1APIServer)
	svc := l.service(Config{Installation: "gazelle"})
	ctx := context.Background()
	for _, dryRun := range []bool{true, false} {
		out, err := svc.CreateNodePool(ctx, plain("wc1", "gpu-l4", dryRun))
		require.NoError(t, err, "dryRun %v", dryRun)
		assert.Nil(t, out.Slice, "no slice composed")
		assert.Nil(t, out.Backend, "no backend registered")
		assert.Nil(t, out.Cache)
		assert.Empty(t, out.PrefetchImages)
		for _, o := range out.Objects {
			assert.NotEqual(t, compose.SliceReleaseName("wc1"), o.Name)
			assert.NotEqual(t, "ConfigMap", o.Kind, "no backend document: %s", o)
		}
	}
	assert.Zero(t, issuerReads(t, l, wc1APIServer), "no ClusterIssuer read")
	_, err := l.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Get(ctx, "wc1-gpu-l4", metav1.GetOptions{})
	require.NoError(t, err, "the pool's release applied")
	_, err = l.installation.Resource(HelmReleaseGVR).Namespace("org-acme").Get(ctx, compose.SliceReleaseName("wc1"), metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "no slice release")
}

// TestServingPoolRefusedWhereTheIssuerIsForbidden: a pool that serves
// composes the slice and judges its issuer; a forbidden read is refused
// naming the missing right, who holds it on the cluster, and the plain pool
// as the way without it.
func TestServingPoolRefusedWhereTheIssuerIsForbidden(t *testing.T) {
	l := newLab(t, "installation.yaml")
	forbidIssuers(t, l, wc1APIServer)
	svc := l.service(Config{Installation: "gazelle"})
	ctx := context.Background()

	_, err := svc.CreateNodePool(ctx, l4("wc1", "gpu-l4", true))
	assertRefused(t, err, "you may not read ClusterIssuer letsencrypt-giantswarm on wc1: the serving slice's models host models.wc1.acme.example.io gets its certificate from it, and cluster-manager judges the issuer as you before composing the slice — missing: get on clusterissuers.cert-manager.io at cluster scope; a ClusterRoleBinding on the cluster to a role with that right (cluster-admin has it) — re-run once you hold it; a pool that serves no models needs no certificate: serving false creates it without the slice")

	// A cluster create_cluster made names the subjects it binds as
	// cluster-admin.
	values := map[string]any{}
	require.NoError(t, unstructured.SetNestedSlice(values, []any{map[string]any{"role": compose.AdminRole, "groups": []any{"customer:acme:Admins"}, "users": []any{"jane@acme.example"}}}, "global", "apps", "rbacBootstrap", "values", "bindings"))
	raw, err := yaml.Marshal(values)
	require.NoError(t, err)
	cm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": compose.ClusterValuesName("wc1"), "namespace": "org-acme", "labels": map[string]any{compose.LabelManagedBy: compose.ManagedBy}},
		"data":     map[string]any{compose.ValuesSecretKey: string(raw)},
	}}
	_, err = l.installation.Resource(ConfigMapGVR).Namespace("org-acme").Create(ctx, cm, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = svc.EnableModelServing(ctx, serving("wc1", true))
	assertRefused(t, err, "missing: get on clusterissuers.cert-manager.io at cluster scope; the cluster binds cluster-admin, which has it, to group customer:acme:Admins, user jane@acme.example — re-run once you hold it")
	assert.NotContains(t, err.Error(), "serving false", "enable_model_serving has no plain pool")
}

// TestPlainPoolRefusedWhereClusterManagerServes: a plain pool on a cluster
// cluster-manager serves on would serve all the same — the slice serves from
// every GPU pool — and the cache is the slice's.
func TestPlainPoolRefusedWhereClusterManagerServes(t *testing.T) {
	l := newLab(t, "installation.yaml")
	svc := l.service(Config{Installation: "gazelle"})
	ctx := context.Background()

	in := plain("wc1", "gpu-l4", true)
	in.Cache = cacheOn()
	_, err := svc.CreateNodePool(ctx, in)
	assertRefused(t, err, "serving false with cache true")

	_, err = svc.EnableModelServing(ctx, serving("wc1", false))
	require.NoError(t, err)
	_, err = svc.CreateNodePool(ctx, plain("wc1", "gpu-l4", true))
	assertRefused(t, err, "serving false: cluster-manager serves on wc1 already")
}
