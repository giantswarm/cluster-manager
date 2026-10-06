package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/identity"
	"github.com/giantswarm/cluster-manager/internal/registry"
)

// ClusterRBAC is who a created cluster grants cluster-admin by default, as
// composed into its rbac-bootstrap bindings: Source "composed" (the creator
// and the organization's admins), "values" (the caller's own bindings, which
// win whole) or "none", Note saying who is left out and why.
type ClusterRBAC struct {
	Source string `json:"source"`
	Role   string `json:"role,omitempty"`
	compose.ClusterAdmins
	Note string `json:"note,omitempty"`
}

// Sources of a created cluster's default bindings.
const (
	RBACComposed = "composed"
	RBACValues   = "values"
	RBACNone     = "none"
)

// valuesBindings names the values key that replaces the default bindings.
const valuesBindings = "global.apps.rbacBootstrap.values.bindings"

// composeClusterRBAC binds the creator and the organization's admins as
// cluster-admin on the new cluster through its values: the creator under
// the username the cluster's apiserver takes from their id_token, the
// admins as the subjects the organization's namespace binds to
// cluster-admin on the installation. Nothing is composed where the caller's
// values carry their own bindings, or the provider line's cluster chart runs
// no rbac-bootstrap.
func composeClusterRBAC(ctx context.Context, dyn dynamic.Interface, spec compose.ClusterSpec, chart *registry.Chart, installation, values map[string]any) (*ClusterRBAC, error) {
	if compose.CallerBindings(spec.Values) {
		out := &ClusterRBAC{Source: RBACValues, ClusterAdmins: compose.AdminsOf(values)}
		if !out.Empty() {
			out.Role = compose.AdminRole
		}
		return out, nil
	}
	boot, err := compose.ReadRBACBootstrap(chart, installation, values)
	if err != nil {
		return nil, err
	}
	if !boot.Enabled {
		return &ClusterRBAC{Source: RBACNone, Note: fmt.Sprintf("the %s%s cluster chart of release %s runs no rbac-bootstrap, so nobody is bound by default — grant access the way the provider line does", compose.ClusterChartPrefix, spec.Provider, spec.Release)}, nil
	}
	admins, err := orgAdmins(ctx, dyn, spec.Namespace())
	if err != nil {
		return nil, err
	}
	creator, note := creatorUsername(ctx, installation, values)
	if creator != "" {
		admins.Users = append(admins.Users, creator)
	}
	admins.Users, admins.Groups = unique(admins.Users), unique(admins.Groups)
	if admins.Empty() {
		return &ClusterRBAC{Source: RBACNone, Note: joinNotes(note, fmt.Sprintf("%s binds nobody to cluster-admin — pass %s in values to bind someone", spec.Namespace(), valuesBindings))}, nil
	}
	if err := compose.ComposeAdmins(values, boot.Bindings, admins); err != nil {
		return nil, err
	}
	return &ClusterRBAC{Source: RBACComposed, Role: compose.AdminRole, ClusterAdmins: admins, Note: note}, nil
}

// orgAdmins are the organization's admins: the users and groups the
// RoleBindings of its namespace bind to the ClusterRole cluster-admin
// (rbac-operator's write-all-customer-group and the organization's own),
// ServiceAccounts left out. Read as the caller: one who may not read them
// is refused, naming the values key that binds without them.
func orgAdmins(ctx context.Context, dyn dynamic.Interface, ns string) (compose.ClusterAdmins, error) {
	list, err := dyn.Resource(RoleBindingGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if apierrors.IsForbidden(err) {
		return compose.ClusterAdmins{}, &ErrRefused{Reason: fmt.Sprintf("you may not list the RoleBindings of %s, which name the organization's admins the new cluster binds as cluster-admin — pass %s in values to name the bindings yourself", ns, valuesBindings)}
	}
	if err != nil {
		return compose.ClusterAdmins{}, fmt.Errorf("list RoleBindings in %s: %w", ns, err)
	}
	var out compose.ClusterAdmins
	for i := range list.Items {
		rb := &list.Items[i]
		kind, _, _ := unstructured.NestedString(rb.Object, "roleRef", "kind")
		role, _, _ := unstructured.NestedString(rb.Object, "roleRef", "name")
		if kind != "ClusterRole" || role != compose.AdminRole {
			continue
		}
		subjects, _, _ := unstructured.NestedSlice(rb.Object, "subjects")
		for _, s := range subjects {
			m, _ := s.(map[string]any)
			name, _ := m["name"].(string)
			switch m["kind"] {
			case "User":
				out.Users = append(out.Users, name)
			case "Group":
				out.Groups = append(out.Groups, name)
			}
		}
	}
	return out, nil
}

// creatorUsername is the username the new cluster's apiserver gives the
// caller: their email under the cluster's usernamePrefix, when the cluster
// takes the username from the email claim — the claim cluster-manager
// composes. Otherwise empty, with the note saying why.
func creatorUsername(ctx context.Context, installation, values map[string]any) (string, string) {
	id, ok := identity.FromContext(ctx)
	if !ok {
		return "", "this call runs without a signed-in person, so no creator is bound"
	}
	oidc := map[string]any{}
	for _, layer := range []map[string]any{installation, values} {
		if block, found, _ := unstructured.NestedMap(layer, "global", "controlPlane", "oidc"); found {
			merge(oidc, block)
		}
	}
	claim, _ := oidc["usernameClaim"].(string)
	if claim != "email" {
		return "", fmt.Sprintf("the cluster takes the username from the %q claim, not the email, so you are not bound — add yourself through %s", orDefault(claim, "sub"), valuesBindings)
	}
	if id.Email == "" {
		return "", fmt.Sprintf("your sign-in carries no email, the claim the cluster takes the username from, so you are not bound — add yourself through %s", valuesBindings)
	}
	prefix, _ := oidc["usernamePrefix"].(string)
	if prefix == "-" { // the apiserver flag's "no prefix"
		prefix = ""
	}
	return prefix + id.Email, ""
}

func unique(ss []string) []string {
	if len(ss) == 0 {
		return nil
	}
	set := map[string]bool{}
	for _, s := range ss {
		set[s] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func joinNotes(notes ...string) string {
	var out []string
	for _, n := range notes {
		if n != "" {
			out = append(out, n)
		}
	}
	return strings.Join(out, "; ")
}
