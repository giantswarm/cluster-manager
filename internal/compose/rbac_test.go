package compose

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chartDefaultBindings are release-aws 36.0.0's rbac-bootstrap defaults.
var chartDefaultBindings = []any{map[string]any{"role": "view", "groups": []any{
	"giantswarm-ad:giantswarm-admins", "giantswarm-ad:giantswarm:giantswarm-admins",
	"giantswarm-github:giantswarm:giantswarm-admins", "giantswarm-github:giantswarm-admins",
}}}

func bindingsValues(b ...any) map[string]any {
	return map[string]any{"global": map[string]any{"apps": map[string]any{"rbacBootstrap": map[string]any{"values": map[string]any{"bindings": b}}}}}
}

// TestReadRBACBootstrap: the aws release runs the app with the chart's
// default bindings; the installation's bindings replace them; a provider
// line that switches it off (eks) or a chart without it is not enabled.
func TestReadRBACBootstrap(t *testing.T) {
	chart := releaseAWS(t)
	got, err := ReadRBACBootstrap(chart, installationValues(), nil)
	require.NoError(t, err)
	assert.Equal(t, RBACBootstrap{Enabled: true, Bindings: chartDefaultBindings}, got)

	own := []any{map[string]any{"role": "view", "groups": []any{"installation:readers"}}}
	got, err = ReadRBACBootstrap(chart, bindingsValues(own...), nil)
	require.NoError(t, err)
	assert.Equal(t, own, got.Bindings)

	off := map[string]any{"cluster": map[string]any{"providerIntegration": map[string]any{"apps": map[string]any{"rbacBootstrap": map[string]any{"enable": false}}}}}
	got, err = ReadRBACBootstrap(chart, installationValues(), off)
	require.NoError(t, err)
	assert.False(t, got.Enabled)

	delete(chart.Files, RBACBootstrapFile)
	got, err = ReadRBACBootstrap(chart, installationValues(), nil)
	require.NoError(t, err)
	assert.Equal(t, RBACBootstrap{}, got)
}

// TestComposeAdmins: the admin binding follows the bindings the release
// installs anyway and passes the release chart's schema; the caller's own
// bindings win whole; no admins compose nothing; AdminsOf reads back the
// cluster-wide cluster-admin subjects only.
func TestComposeAdmins(t *testing.T) {
	admins := ClusterAdmins{Users: []string{"jane@acme.example"}, Groups: []string{"customer:acme:Admins"}}
	values, err := ClusterValues(newCluster("aws", "36.0.0"))
	require.NoError(t, err)
	require.NoError(t, ComposeAdmins(values, chartDefaultBindings, admins))
	assert.Equal(t, bindingsValues(chartDefaultBindings[0], map[string]any{"role": "cluster-admin", "users": []any{"jane@acme.example"}, "groups": []any{"customer:acme:Admins"}})["global"].(map[string]any)["apps"], values["global"].(map[string]any)["apps"])
	require.NoError(t, ValidateClusterValues(releaseAWS(t), installationValues(), values))
	assert.Equal(t, admins, AdminsOf(values))
	assert.Len(t, chartDefaultBindings, 1, "the base bindings are never changed")

	theirs := bindingsValues(map[string]any{"role": "edit", "groups": []any{"customer:acme:Devs"}})
	require.NoError(t, ComposeAdmins(theirs, chartDefaultBindings, admins))
	assert.Equal(t, bindingsValues(map[string]any{"role": "edit", "groups": []any{"customer:acme:Devs"}}), theirs)
	assert.True(t, AdminsOf(theirs).Empty())

	none := map[string]any{}
	require.NoError(t, ComposeAdmins(none, chartDefaultBindings, ClusterAdmins{}))
	assert.Empty(t, none)

	scoped := bindingsValues(map[string]any{"role": "cluster-admin", "users": []any{"ns-admin"}, "namespaces": []any{"team"}})
	assert.True(t, AdminsOf(scoped).Empty(), "a namespaced binding is not a cluster admin")
}
