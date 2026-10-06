package compose

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/cluster-manager/internal/registry"
)

// newCluster is the fixture cluster the cluster goldens are rendered for.
func newCluster(provider, release string) ClusterSpec {
	return ClusterSpec{
		Organization: "acme", Name: "dev01", Provider: provider, Release: release,
		Identity: "acme-dev", Description: "Acme's development cluster",
		Values: map[string]any{
			"global": map[string]any{"controlPlane": map[string]any{"instanceType": "m6i.xlarge"}},
		},
		TenantServiceAccount: DefaultTenantServiceAccount,
	}
}

// TestClusterGoldens pins the HelmRelease, OCIRepository and values
// ConfigMap of a new cluster byte for byte per provider line create_cluster
// offers: the identity lands where each line's cluster chart reads it.
func TestClusterGoldens(t *testing.T) {
	for _, tc := range []struct{ provider, release string }{
		{"aws", "36.0.0"},
		{"eks", "34.0.1"},
		{"azure", "36.0.0"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			spec := newCluster(tc.provider, tc.release)
			require.NoError(t, spec.Validate())
			values, err := ClusterValues(spec)
			require.NoError(t, err)
			objs, err := ClusterRelease(spec, values)
			require.NoError(t, err)
			assertGolden(t, "cluster-"+tc.provider, objs)
		})
	}
}

// TestClusterValuesLeaveTheCallersAlone: composing never changes the values
// the caller passed in.
func TestClusterValuesLeaveTheCallersAlone(t *testing.T) {
	spec := newCluster("aws", "36.0.0")
	_, err := ClusterValues(spec)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"global": map[string]any{"controlPlane": map[string]any{"instanceType": "m6i.xlarge"}}}, spec.Values)
}

// TestClusterValuesRefuseAContradiction: a value the arguments compose is
// refused when the caller's values set it otherwise, naming the argument;
// the same value is accepted.
func TestClusterValuesRefuseAContradiction(t *testing.T) {
	spec := newCluster("aws", "36.0.0")
	spec.Values = map[string]any{"global": map[string]any{"metadata": map[string]any{"name": "other"}}}
	_, err := ClusterValues(spec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "global.metadata.name is other, but the name argument sets it to \"dev01\"")

	spec.Values = map[string]any{"global": map[string]any{"providerSpecific": map[string]any{"awsClusterRoleIdentityName": "acme-dev"}}}
	_, err = ClusterValues(spec)
	assert.NoError(t, err)
}

// dexOIDC is an installation Dex with a private certificate.
func dexOIDC() *ClusterOIDC {
	return &ClusterOIDC{IssuerURL: "https://dex.gazelle.example", ClientID: "dex-k8s-authenticator", UsernameClaim: "email", GroupsClaim: "groups", CAPem: "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"}
}

// TestClusterValuesComposeTheOIDC: the installation's identity provider lands
// in global.controlPlane.oidc beside the caller's other control-plane values
// and passes the release chart's schema; a caller's own block wins whole;
// without a provider nothing is composed.
func TestClusterValuesComposeTheOIDC(t *testing.T) {
	spec := newCluster("aws", "36.0.0")
	spec.OIDC = dexOIDC()
	values, err := ClusterValues(spec)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"instanceType": "m6i.xlarge",
		"oidc": map[string]any{
			"issuerUrl": "https://dex.gazelle.example", "clientId": "dex-k8s-authenticator",
			"usernameClaim": "email", "groupsClaim": "groups", "caPem": spec.OIDC.CAPem,
		},
	}, values["global"].(map[string]any)["controlPlane"])
	require.NoError(t, ValidateClusterValues(releaseAWS(t), installationValues(), values))
	assert.False(t, CallerOIDC(spec.Values), "composing never changes the caller's values")

	own := map[string]any{"issuerUrl": "https://login.acme.example", "clientId": "acme"}
	spec.Values = map[string]any{"global": map[string]any{"controlPlane": map[string]any{"oidc": own}}}
	values, err = ClusterValues(spec)
	require.NoError(t, err)
	assert.Equal(t, own, values["global"].(map[string]any)["controlPlane"].(map[string]any)["oidc"])

	spec = newCluster("aws", "36.0.0")
	values, err = ClusterValues(spec)
	require.NoError(t, err)
	assert.False(t, CallerOIDC(values))
}

// TestClusterSpecValidate holds the name rules and the offered provider
// lines.
func TestClusterSpecValidate(t *testing.T) {
	for name, wantErr := range map[string]string{
		"dev01":                 "",
		"a":                     "",
		"abcdefghijklmnopqrst":  "",
		"abcdefghijklmnopqrstu": "DNS label of at most 20",
		"Dev01":                 "DNS label",
		"1dev":                  "starting with a letter",
		"dev-":                  "DNS label",
		"dev_01":                "DNS label",
		"":                      "DNS label",
	} {
		spec := newCluster("aws", "36.0.0")
		spec.Name = name
		err := spec.Validate()
		if wantErr == "" {
			assert.NoError(t, err, name)
			continue
		}
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), wantErr, name)
	}
	spec := newCluster("vsphere", "30.0.0")
	assert.ErrorContains(t, spec.Validate(), `provider "vsphere": create_cluster offers aws, azure, eks`)
	spec = newCluster("aws", "36.0.0")
	spec.Organization = ""
	assert.ErrorContains(t, spec.Validate(), "organization: required")
}

// releaseAWS is release-aws 36.0.0's values.yaml and values.schema.json,
// extracted unchanged from the chart archive in the registry
// (oci://gsoci.azurecr.io/charts/giantswarm/release-aws:36.0.0).
func releaseAWS(t *testing.T) *registry.Chart {
	t.Helper()
	dir := filepath.Join("testdata", "charts", "release-aws-36.0.0")
	files := map[string][]byte{}
	for _, name := range []string{valuesFile, valuesSchemaFile, RBACBootstrapFile} {
		raw, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // fixture named by the test
		require.NoError(t, err)
		files[name] = raw
	}
	ref, err := registry.ParseRef(ReleaseChartURL("aws"))
	require.NoError(t, err)
	return &registry.Chart{Ref: ref, Version: "36.0.0", Files: files}
}

// installationValues are the keys a Giant Swarm installation's
// cluster-app-installation-values carries.
func installationValues() map[string]any {
	return map[string]any{"global": map[string]any{
		"connectivity":      map[string]any{"baseDomain": "gazelle.example.io"},
		"managementCluster": "gazelle",
	}}
}

// TestValidateClusterValues checks the merged values against the real
// release chart's schema: the composed cluster passes, and a type error, an
// enum and a pattern violation and an unknown top-level key are each named
// with their path.
func TestValidateClusterValues(t *testing.T) {
	chart := releaseAWS(t)
	spec := newCluster("aws", "36.0.0")
	values, err := ClusterValues(spec)
	require.NoError(t, err)
	require.NoError(t, ValidateClusterValues(chart, installationValues(), values))

	spec.Values = map[string]any{
		"global": map[string]any{"controlPlane": map[string]any{"instanceType": "R6I XL"}, "metadata": map[string]any{"servicePriority": "urgent", "preventDeletion": "yes"}},
		"typo":   true,
	}
	values, err = ClusterValues(spec)
	require.NoError(t, err)
	err = ValidateClusterValues(chart, installationValues(), values)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the values do not match oci://gsoci.azurecr.io/charts/giantswarm/release-aws 36.0.0's schema")
	assert.Contains(t, err.Error(), "/global/metadata/servicePriority")
	assert.Contains(t, err.Error(), "/global/metadata/preventDeletion")
	assert.Contains(t, err.Error(), "/global/controlPlane/instanceType")
	assert.Contains(t, err.Error(), "typo")
}

// TestValidateClusterValuesWithoutSchema refuses a chart that ships no
// schema rather than passing the values unchecked.
func TestValidateClusterValuesWithoutSchema(t *testing.T) {
	chart := releaseAWS(t)
	delete(chart.Files, valuesSchemaFile)
	assert.ErrorContains(t, ValidateClusterValues(chart, nil, nil), "ships no values.schema.json")
}

// TestCoalesceLayers: maps merge key by key, a later layer wins, a null
// deletes, and a merged layer is never changed by a later one.
func TestCoalesceLayers(t *testing.T) {
	installation := map[string]any{"global": map[string]any{"a": "installation", "b": "installation"}}
	dst := map[string]any{}
	coalesce(dst, installation)
	coalesce(dst, map[string]any{"global": map[string]any{"a": "cluster", "b": nil, "c": "cluster"}})
	assert.Equal(t, map[string]any{"global": map[string]any{"a": "cluster", "c": "cluster"}}, dst)
	assert.Equal(t, map[string]any{"global": map[string]any{"a": "installation", "b": "installation"}}, installation)
}
