package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/cluster-manager/internal/compose"
)

var installationOIDC = &compose.ClusterOIDC{IssuerURL: "https://dex.gazelle.example", ClientID: "dex-k8s-authenticator", UsernameClaim: "email", GroupsClaim: "groups"}

// kubectlGSCluster makes wc2 a cluster as `kubectl gs template cluster`
// leaves it: its user values ConfigMap carries the values given.
func kubectlGSCluster(t *testing.T, l *lab, values string) {
	t.Helper()
	cms := l.installation.Resource(ConfigMapGVR).Namespace("org-acme")
	cm, err := cms.Get(context.Background(), "wc2-user-values", metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedField(cm.Object, values, "data", "values"))
	_, err = cms.Update(context.Background(), cm, metav1.UpdateOptions{})
	require.NoError(t, err)
}

// installationValuesIn syncs the installation's cluster values into the
// organization's namespace.
func installationValuesIn(t *testing.T, l *lab, values string) {
	t.Helper()
	cm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": compose.InstallationValuesConfigMap, "namespace": "org-acme"},
		"data":     map[string]any{compose.ValuesSecretKey: values},
	}}
	_, err := l.installation.Resource(ConfigMapGVR).Namespace("org-acme").Create(context.Background(), cm, metav1.CreateOptions{})
	require.NoError(t, err)
}

func poolClusterValues(t *testing.T, out *WriteResult) map[string]any {
	t.Helper()
	vals, _, _ := unstructured.NestedMap(out.Manifests[1], "spec", "values", "cluster")
	return vals
}

const trustingDex = "global:\n  controlPlane:\n    oidc:\n      issuerUrl: https://dex.gazelle.example/\n      clientId: dex-k8s-authenticator\n"

// TestEffectiveValuesBaseDomain: a kubectl-gs cluster's values carry no
// base domain; the installation's cluster values in the organization's
// namespace (app-operator's catalog layer) supply it, and the cluster's own
// values win where they set it.
func TestEffectiveValuesBaseDomain(t *testing.T) {
	t.Run("from the installation's values", func(t *testing.T) {
		l := newLab(t, "installation.yaml")
		kubectlGSCluster(t, l, trustingDex)
		installationValuesIn(t, l, "global:\n  managementCluster: gazelle\n  connectivity:\n    baseDomain: installation.example.io\n")
		out, err := l.service(Config{Installation: "gazelle", ClusterOIDC: installationOIDC}).CreateNodePool(context.Background(), l4("wc2", "gpu-l4b", true))
		require.NoError(t, err)
		assert.Equal(t, "installation.example.io", poolClusterValues(t, out)["baseDomain"])
		assert.Equal(t, "gazelle", poolClusterValues(t, out)["managementCluster"])
	})
	t.Run("the cluster's own over the installation's", func(t *testing.T) {
		l := newLab(t, "installation.yaml")
		installationValuesIn(t, l, "global:\n  connectivity:\n    baseDomain: installation.example.io\n")
		out, err := l.service(Config{Installation: "gazelle"}).CreateNodePool(context.Background(), l4("wc2", "gpu-l4b", true))
		require.NoError(t, err)
		assert.Equal(t, "acme.example.io", poolClusterValues(t, out)["baseDomain"])
	})
}

// TestCreateNodePoolPrerequisites: a kubectl-gs cluster missing the base
// domain and the installation's issuer is refused once, before anything of
// the cluster is read, naming both with the values to add to its user
// values ConfigMap; nothing is written.
func TestCreateNodePoolPrerequisites(t *testing.T) {
	l := newLab(t, "installation.yaml")
	kubectlGSCluster(t, l, "global:\n  metadata:\n    name: wc2\n")
	_, err := l.service(Config{Installation: "gazelle", ClusterOIDC: installationOIDC}).CreateNodePool(context.Background(), l4("wc2", "gpu-l4b", true))
	var refused *ErrRefused
	require.ErrorAs(t, err, &refused)
	assert.Contains(t, refused.Reason, "cluster wc2 misses 2 prerequisite(s) of a GPU pool")
	assert.Contains(t, refused.Reason, "global.connectivity.baseDomain")
	assert.Contains(t, refused.Reason, "global.controlPlane.oidc trusting https://dex.gazelle.example for the audience dex-k8s-authenticator")
	assert.Contains(t, refused.Reason, "add to the ConfigMap org-acme/wc2-user-values (the App's spec.userConfig)")
	assert.Contains(t, refused.Reason, `global:
  connectivity:
    baseDomain: <the installation's base domain>
  controlPlane:
    oidc:
      clientId: dex-k8s-authenticator
      groupsClaim: groups
      issuerUrl: https://dex.gazelle.example
      usernameClaim: email
`)
	hrs, err := l.installation.Resource(HelmReleaseGVR).Namespace("org-acme").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	for _, hr := range hrs.Items {
		assert.NotEqual(t, "wc2-gpu-l4b", hr.GetName(), "nothing written")
	}

	t.Run("the base domain present, only the issuer missing", func(t *testing.T) {
		l := newLab(t, "installation.yaml")
		_, err := l.service(Config{Installation: "gazelle", ClusterOIDC: installationOIDC}).CreateNodePool(context.Background(), l4("wc2", "gpu-l4b", true))
		require.ErrorAs(t, err, &refused)
		assert.Contains(t, refused.Reason, "misses 1 prerequisite(s)")
		assert.NotContains(t, refused.Reason, "baseDomain")
	})
	t.Run("no provider configured: no trust to check", func(t *testing.T) {
		_, err := newLab(t, "installation.yaml").service(Config{Installation: "gazelle"}).CreateNodePool(context.Background(), l4("wc2", "gpu-l4b", true))
		require.NoError(t, err)
	})
}

// TestTrustsIssuer: the installation's issuer with its audience, in the
// plain block or among the structured authentication's issuers.
func TestTrustsIssuer(t *testing.T) {
	structured := func(issuers ...any) map[string]any {
		return map[string]any{"global": map[string]any{"controlPlane": map[string]any{"oidc": map[string]any{
			"structuredAuthentication": map[string]any{"enabled": true, "issuers": issuers},
		}}}}
	}
	plain := func(issuer, client string) map[string]any {
		return map[string]any{"global": map[string]any{"controlPlane": map[string]any{"oidc": map[string]any{"issuerUrl": issuer, "clientId": client}}}}
	}
	other := map[string]any{"issuerUrl": "https://login.example/v2.0", "clientId": "abc"}
	dex := map[string]any{"issuerUrl": "https://dex.gazelle.example", "clientId": "dex-k8s-authenticator"}
	for name, tc := range map[string]struct {
		vals map[string]any
		want bool
	}{
		"none":                      {map[string]any{}, false},
		"plain":                     {plain("https://dex.gazelle.example/", "dex-k8s-authenticator"), true},
		"plain, another audience":   {plain("https://dex.gazelle.example", "other"), false},
		"plain, another issuer":     {plain("https://login.example/v2.0", "dex-k8s-authenticator"), false},
		"structured, among others":  {structured(other, dex), true},
		"structured, without it":    {structured(other), false},
		"structured off, plain dex": {map[string]any{"global": map[string]any{"controlPlane": map[string]any{"oidc": map[string]any{"issuerUrl": "https://dex.gazelle.example", "clientId": "dex-k8s-authenticator", "structuredAuthentication": map[string]any{"enabled": false}}}}}, true},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, trustsIssuer(tc.vals, installationOIDC))
		})
	}
}

// TestPrerequisitesStructuredSnippet: a cluster on structured authentication
// is told to add the issuer as one more entry of its issuers.
func TestPrerequisitesStructuredSnippet(t *testing.T) {
	l := newLab(t, "installation.yaml")
	kubectlGSCluster(t, l, "global:\n  connectivity:\n    baseDomain: acme.example.io\n  controlPlane:\n    oidc:\n      structuredAuthentication:\n        enabled: true\n        issuers:\n        - issuerUrl: https://login.example/v2.0\n          clientId: abc\n")
	_, err := l.service(Config{Installation: "gazelle", ClusterOIDC: installationOIDC}).CreateNodePool(context.Background(), l4("wc2", "gpu-l4b", true))
	var refused *ErrRefused
	require.ErrorAs(t, err, &refused)
	assert.Contains(t, refused.Reason, "the issuer as one more entry of the issuers already there")
	assert.Contains(t, refused.Reason, "    oidc:\n      structuredAuthentication:\n        issuers:\n        - clientId: dex-k8s-authenticator\n")
}
