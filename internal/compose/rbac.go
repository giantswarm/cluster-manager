package compose

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/cluster-manager/internal/registry"
)

// A created cluster's RBAC is rendered by the cluster chart's default app
// rbac-bootstrap (giantswarm/rbac-bootstrap-app): every entry of its
// `bindings` becomes a ClusterRoleBinding on the cluster. The cluster chart
// layers the app's values — its defaults, then global.apps.rbacBootstrap.values
// — with Helm's merge, which replaces a list whole: composed bindings carry
// the ones the release would install without them, then the admin binding.
const (
	// AdminRole is the ClusterRole a created cluster grants its creator and
	// the organization's admins.
	AdminRole = "cluster-admin"
	// RBACBootstrapFile is the cluster chart's definition of the
	// rbac-bootstrap app, its default values included, inside a release
	// chart.
	RBACBootstrapFile = "charts/cluster/files/helmreleases/rbac-bootstrap.yaml"
)

// bindingsPath is where the cluster charts take rbac-bootstrap's bindings.
var bindingsPath = []string{valuesGlobal, "apps", "rbacBootstrap", "values", "bindings"}

// rbacBootstrapEnablePath switches the app on per provider line, in the
// provider chart's values for its cluster subchart (on for aws and azure,
// off for eks, whose access the cloud's access entries govern).
var rbacBootstrapEnablePath = []string{"cluster", "providerIntegration", "apps", "rbacBootstrap", "enable"}

// ClusterAdmins are the subjects a created cluster binds to AdminRole:
// usernames and groups as the cluster's apiserver authenticates them.
type ClusterAdmins struct {
	Users  []string `json:"users,omitempty"`
	Groups []string `json:"groups,omitempty"`
}

// Empty reports whether no subject is named.
func (a ClusterAdmins) Empty() bool { return len(a.Users) == 0 && len(a.Groups) == 0 }

// binding is the rbac-bootstrap entry binding the admins cluster-wide.
func (a ClusterAdmins) binding() map[string]any {
	out := map[string]any{"role": AdminRole}
	if len(a.Users) > 0 {
		out["users"] = toAny(a.Users)
	}
	if len(a.Groups) > 0 {
		out["groups"] = toAny(a.Groups)
	}
	return out
}

// CallerBindings reports whether the caller's values carry their own
// rbac-bootstrap bindings, which then win whole over the composed ones.
func CallerBindings(values map[string]any) bool {
	_, found, _ := unstructured.NestedFieldNoCopy(values, bindingsPath...)
	return found
}

// RBACBootstrap is what a release's rbac-bootstrap app installs on the
// cluster before cluster-manager composes anything: whether the provider
// line runs the app, and the bindings it installs — the installation's
// values' when they carry any, else the cluster chart's defaults.
type RBACBootstrap struct {
	Enabled  bool
	Bindings []any
}

// ReadRBACBootstrap reads the release chart's rbac-bootstrap app under the
// installation's and the cluster's values, the layers the release installs
// with. A chart without the app is not enabled.
func ReadRBACBootstrap(chart *registry.Chart, installation, cluster map[string]any) (RBACBootstrap, error) {
	raw := chart.File(RBACBootstrapFile)
	if raw == nil {
		return RBACBootstrap{}, nil
	}
	merged := map[string]any{}
	if v := chart.File(valuesFile); v != nil {
		if err := yaml.Unmarshal(v, &merged); err != nil {
			return RBACBootstrap{}, fmt.Errorf("%s %s: read %s: %w", chart.Ref, chart.Version, valuesFile, err)
		}
	}
	coalesce(merged, installation)
	coalesce(merged, cluster)
	enabled, _, _ := unstructured.NestedBool(merged, rbacBootstrapEnablePath...)
	if bindings, found, _ := unstructured.NestedSlice(installation, bindingsPath...); found {
		return RBACBootstrap{Enabled: enabled, Bindings: bindings}, nil
	}
	var app struct {
		DefaultValues struct {
			Bindings []any `json:"bindings"`
		} `json:"defaultValues"`
	}
	if err := yaml.Unmarshal(raw, &app); err != nil {
		return RBACBootstrap{}, fmt.Errorf("%s %s: read %s: %w", chart.Ref, chart.Version, RBACBootstrapFile, err)
	}
	return RBACBootstrap{Enabled: enabled, Bindings: app.DefaultValues.Bindings}, nil
}

// ComposeAdmins adds the admins' binding to the cluster's values after the
// bindings the release installs anyway. Nothing is composed for no admins or
// where the caller's values carry their own bindings.
func ComposeAdmins(values map[string]any, base []any, a ClusterAdmins) error {
	if a.Empty() || CallerBindings(values) {
		return nil
	}
	bindings := append(deepCopy(base).([]any), a.binding())
	if err := unstructured.SetNestedSlice(values, bindings, bindingsPath...); err != nil {
		return fmt.Errorf("values: %s: %w", strings.Join(bindingsPath, "."), err)
	}
	return nil
}

// AdminsOf reads the subjects the values bind to AdminRole cluster-wide
// (an entry without namespaces), each name once, sorted.
func AdminsOf(values map[string]any) ClusterAdmins {
	bindings, _, _ := unstructured.NestedSlice(values, bindingsPath...)
	users, groups := map[string]bool{}, map[string]bool{}
	for _, b := range bindings {
		m, ok := b.(map[string]any)
		if !ok || m["role"] != AdminRole || m["namespaces"] != nil {
			continue
		}
		for key, set := range map[string]map[string]bool{"users": users, "groups": groups} {
			names, _ := m[key].([]any)
			for _, n := range names {
				if s, ok := n.(string); ok {
					set[s] = true
				}
			}
		}
	}
	return ClusterAdmins{Users: sortedKeys(users), Groups: sortedKeys(groups)}
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
