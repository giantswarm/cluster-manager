package tools

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/cluster-manager/internal/compose"
)

// effectiveValues is what the cluster's chart renders with, as far as
// cluster-manager reads it: the cluster's own values (clusterValues) over the
// installation's cluster values in the organization's namespace. The GitOps
// layouts name that ConfigMap as their lowest layer; an App that names no
// such layer — a cluster made by `kubectl gs template cluster` — gets the
// same values from app-operator's catalog layer, which cluster-manager does
// not read. A namespace without the ConfigMap adds nothing.
func effectiveValues(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured) (map[string]any, error) {
	vals, err := clusterValues(ctx, dyn, c)
	if err != nil {
		return nil, err
	}
	ns := c.GetNamespace()
	cm, err := dyn.Resource(ConfigMapGVR).Namespace(ns).Get(ctx, compose.InstallationValuesConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return vals, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get ConfigMap %s/%s: %w", ns, compose.InstallationValuesConfigMap, err)
	}
	raw, _, _ := unstructured.NestedString(cm.Object, "data", compose.ValuesSecretKey)
	out, err := parseValues(ns, compose.InstallationValuesConfigMap, compose.ValuesSecretKey, raw)
	if err != nil {
		return nil, err
	}
	merge(out, vals)
	return out, nil
}

// clusterPrerequisites refuses a pool on a cluster whose values miss what
// the pool needs, before anything of the cluster is read or written: the
// installation's base domain (the pool's bootstrap) and, on a workload
// cluster, an apiserver that trusts the installation's identity provider —
// cluster-manager reads the cluster as the person with that provider's
// id_token, and a cluster that does not trust it answers Unauthorized. One
// refusal names every missing prerequisite with the values to add.
func (s *Service) clusterPrerequisites(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured, vals map[string]any) error {
	var missing []string
	snippet := map[string]any{}
	if domain, _, _ := unstructured.NestedString(vals, "global", "connectivity", "baseDomain"); domain == "" {
		missing = append(missing, fmt.Sprintf("global.connectivity.baseDomain (the pool's bootstrap needs the installation's base domain; neither the cluster's values nor %s/%s carry it)", c.GetNamespace(), compose.InstallationValuesConfigMap))
		_ = unstructured.SetNestedField(snippet, "<the installation's base domain>", "global", "connectivity", "baseDomain")
	}
	if o := s.cfg.ClusterOIDC; o != nil && !s.ownCluster(c) && !trustsIssuer(vals, o) {
		missing = append(missing, fmt.Sprintf("global.controlPlane.oidc trusting %s for the audience %s (cluster-manager reads the cluster as you with that provider's id_token; without it the cluster answers Unauthorized — adding it rolls the control plane, about 15 minutes)", o.IssuerURL, o.ClientID))
		if structuredAuthentication(vals) {
			_ = unstructured.SetNestedSlice(snippet, []any{oidcIssuer(o)}, "global", "controlPlane", "oidc", "structuredAuthentication", "issuers")
		} else {
			_ = unstructured.SetNestedMap(snippet, oidcIssuer(o), "global", "controlPlane", "oidc")
		}
	}
	if len(missing) == 0 {
		return nil
	}
	doc, err := yaml.Marshal(snippet)
	if err != nil {
		return fmt.Errorf("render the prerequisites' values: %w", err)
	}
	add := "add to " + valuesHome(ctx, dyn, c)
	if _, found, _ := unstructured.NestedSlice(snippet, "global", "controlPlane", "oidc", "structuredAuthentication", "issuers"); found {
		add += " (the issuer as one more entry of the issuers already there)"
	}
	return &ErrRefused{Reason: fmt.Sprintf("cluster %s misses %d prerequisite(s) of a GPU pool: %s — %s, let the cluster reconcile and re-run:\n%s", c.GetName(), len(missing), strings.Join(missing, "; "), add, doc)}
}

// trustsIssuer reports whether the cluster's apiserver, as its values
// configure it, accepts the provider's id_token: the issuer with the
// provider's client id as audience, in global.controlPlane.oidc or, with
// structured authentication on, among its issuers.
func trustsIssuer(vals map[string]any, o *compose.ClusterOIDC) bool {
	matches := func(m map[string]any) bool {
		issuer, _ := m["issuerUrl"].(string)
		client, _ := m["clientId"].(string)
		return strings.TrimSuffix(issuer, "/") == strings.TrimSuffix(o.IssuerURL, "/") && client == o.ClientID
	}
	if structuredAuthentication(vals) {
		issuers, _, _ := unstructured.NestedSlice(vals, "global", "controlPlane", "oidc", "structuredAuthentication", "issuers")
		for _, i := range issuers {
			if m, ok := i.(map[string]any); ok && matches(m) {
				return true
			}
		}
		return false
	}
	block, _, _ := unstructured.NestedMap(vals, "global", "controlPlane", "oidc")
	return matches(block)
}

// structuredAuthentication reports whether the cluster's apiserver takes
// its issuers from global.controlPlane.oidc.structuredAuthentication.
func structuredAuthentication(vals map[string]any) bool {
	on, _, _ := unstructured.NestedBool(vals, "global", "controlPlane", "oidc", "structuredAuthentication", "enabled")
	return on
}

// oidcIssuer is the provider as the cluster charts take one issuer.
func oidcIssuer(o *compose.ClusterOIDC) map[string]any {
	out := map[string]any{"issuerUrl": o.IssuerURL, "clientId": o.ClientID}
	for k, v := range map[string]string{"usernameClaim": o.UsernameClaim, "groupsClaim": o.GroupsClaim, "caPem": o.CAPem} {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// valuesHome names where the cluster's own values are kept, for the
// refusal's fix: the HelmRelease's values, else the App's user values
// ConfigMap (a kubectl-gs cluster's `<name>-userconfig`) or its config
// ConfigMap.
func valuesHome(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured) string {
	ns, name := c.GetNamespace(), c.GetName()
	if _, err := dyn.Resource(HelmReleaseGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
		return fmt.Sprintf("the values of the HelmRelease %s/%s", ns, name)
	}
	if app, err := dyn.Resource(AppGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
		for _, field := range []string{"userConfig", "config"} {
			if cm, _, _ := unstructured.NestedString(app.Object, "spec", field, appValuesConfigMap, "name"); cm != "" {
				cmNS, _, _ := unstructured.NestedString(app.Object, "spec", field, appValuesConfigMap, "namespace")
				return fmt.Sprintf("the ConfigMap %s/%s (the App's spec.%s)", firstNonEmpty(cmNS, ns), cm, field)
			}
		}
	}
	return fmt.Sprintf("the values of cluster %s", name)
}
