package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// appValuesOf merges the values of the App name in testdata/app-values.yaml.
func appValuesOf(t *testing.T, name string) (map[string]any, error) {
	t.Helper()
	dyn := newFake(t, "app-values.yaml")
	app, err := dyn.Resource(AppGVR).Namespace("org-acme").Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return appValues(context.Background(), dyn, app)
}

// TestAppValuesMergeOrder: app-operator's precedence, lowest first — the
// extraConfigs up to priority 50 (25 when unset, and before spec.config at
// 50), spec.config, the extraConfigs up to 100, spec.userConfig, the
// extraConfigs above; one priority in list order; maps merge recursively.
// The Secrets the App names are never read.
func TestAppValuesMergeOrder(t *testing.T) {
	vals, err := appValuesOf(t, "layers")
	require.NoError(t, err, "the Secrets the App names do not exist and are not read")
	assert.Equal(t, map[string]any{
		"pre":       "config",  // priority 10 under spec.config
		"below":     "default", // the unset priority (25) over 10
		"default":   "config",  // the unset priority under spec.config
		"atCluster": "config",  // priority 50 before spec.config
		"post":      "first",   // priority 75 over spec.config
		"user":      "user",    // spec.config, overridden by priority 75, overridden by spec.userConfig
		"top":       "top",     // priority 150 over spec.userConfig
		"sameLevel": "second",  // two entries of priority 75: the later one wins
		"nested":    map[string]any{"fromConfig": "config", "overridden": "user"},
	}, vals)
}

// TestAppValuesRefusals: an App whose values the pool's snapshot cannot be
// read from says which sources it names.
func TestAppValuesRefusals(t *testing.T) {
	for _, tc := range []struct {
		app  string
		want []string
	}{
		{"no-values", []string{"the App org-acme/no-values names no values (spec.config, spec.extraConfigs, spec.userConfig)"}},
		{"secrets-only", []string{"the App org-acme/secrets-only keeps its values only in Secrets", "spec.userConfig secret org-acme/secrets-only-user", "spec.extraConfigs[0] secret org-acme/secrets-only-extra"}},
		{"missing", []string{"the App org-acme/missing names values ConfigMaps that do not exist: spec.config configMap org-acme/missing-config (it names spec.extraConfigs[0] configMap org-acme/layers-pre, spec.config configMap org-acme/missing-config)"}},
	} {
		t.Run(tc.app, func(t *testing.T) {
			_, err := appValuesOf(t, tc.app)
			require.Error(t, err)
			for _, want := range tc.want {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}
