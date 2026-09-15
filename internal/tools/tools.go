// Package tools implements cluster-manager's MCP tools over the Kubernetes
// API of the installation: list_clusters and list_node_pools in this stage,
// the node-pool writes in later ones. Every call reads through the client the
// request's context selects — the caller's own when the forwarded IdP token
// is present, so the caller's RBAC governs what is listed.
package tools

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// Kubernetes resources the tools read.
var (
	ClusterGVR     = schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "v1beta1", Resource: "clusters"}
	MachinePoolGVR = schema.GroupVersionResource{Group: "cluster.x-k8s.io", Version: "v1beta1", Resource: "machinepools"}
	HelmReleaseGVR = schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
	ReleaseGVR     = schema.GroupVersionResource{Group: "release.giantswarm.io", Version: "v1alpha1", Resource: "releases"}
)

// Labels and prefixes of the fleet's conventions.
const (
	LabelClusterName    = "cluster.x-k8s.io/cluster-name"
	LabelReleaseVersion = "release.giantswarm.io/version"
	LabelOrganization   = "giantswarm.io/organization"
	OrgNamespacePrefix  = "org-"
)

// ClientsFor returns the dynamic client a call uses: the caller's own when
// the request carries the caller's forwarded token, else the ServiceAccount's.
type ClientsFor func(ctx context.Context) dynamic.Interface

// Config is the installation-wide configuration of the tools.
type Config struct {
	// Installation is the installation's name; the Cluster of that name is
	// the installation's own cluster (its management cluster).
	Installation string
}

// Service implements the tools.
type Service struct {
	clients ClientsFor
	cfg     Config
}

// New builds the tools over the per-call clients.
func New(clients ClientsFor, cfg Config) *Service {
	return &Service{clients: clients, cfg: cfg}
}

// ErrNotFound is returned when a named cluster does not exist.
type ErrNotFound struct{ What string }

func (e *ErrNotFound) Error() string { return e.What + " not found" }

// ErrAmbiguous is returned when a cluster name matches in several namespaces
// and the caller named none.
type ErrAmbiguous struct {
	Name       string
	Namespaces []string
}

func (e *ErrAmbiguous) Error() string {
	return fmt.Sprintf("cluster %s exists in several namespaces (%s): name the namespace", e.Name, strings.Join(e.Namespaces, ", "))
}

// getCluster finds a Cluster by name: in namespace when given, else across
// the installation's namespaces (an error when the name is ambiguous).
func (s *Service) getCluster(ctx context.Context, dyn dynamic.Interface, name, namespace string) (*unstructured.Unstructured, error) {
	if namespace != "" {
		obj, err := dyn.Resource(ClusterGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil, &ErrNotFound{What: fmt.Sprintf("cluster %s/%s", namespace, name)}
		}
		if err != nil {
			return nil, fmt.Errorf("get cluster %s/%s: %w", namespace, name, err)
		}
		return obj, nil
	}
	list, err := dyn.Resource(ClusterGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}
	var found []unstructured.Unstructured
	for _, item := range list.Items {
		if item.GetName() == name {
			found = append(found, item)
		}
	}
	switch len(found) {
	case 0:
		return nil, &ErrNotFound{What: "cluster " + name}
	case 1:
		return &found[0], nil
	default:
		nss := make([]string, 0, len(found))
		for _, c := range found {
			nss = append(nss, c.GetNamespace())
		}
		return nil, &ErrAmbiguous{Name: name, Namespaces: nss}
	}
}

// organization derives a cluster's organization: the organization label,
// else the `org-` namespace prefix stripped.
func organization(obj *unstructured.Unstructured) string {
	if org := obj.GetLabels()[LabelOrganization]; org != "" {
		return org
	}
	return strings.TrimPrefix(obj.GetNamespace(), OrgNamespacePrefix)
}

// refGVR turns an object reference's apiVersion and kind into the resource to
// read it through — the same guess the API machinery makes (lowercase kind,
// plural s/es).
func refGVR(apiVersion, kind string) (schema.GroupVersionResource, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("apiVersion %q: %w", apiVersion, err)
	}
	gvr, _ := meta.UnsafeGuessKindToResource(gv.WithKind(kind))
	return gvr, nil
}

// getRef reads the object an object reference (apiVersion, kind, name,
// optional namespace) points at; the namespace defaults to the referrer's.
func getRef(ctx context.Context, dyn dynamic.Interface, ref map[string]any, defaultNamespace string) (*unstructured.Unstructured, error) {
	apiVersion, _ := ref["apiVersion"].(string)
	kind, _ := ref["kind"].(string)
	name, _ := ref["name"].(string)
	if apiVersion == "" || kind == "" || name == "" {
		return nil, fmt.Errorf("incomplete object reference %v", ref)
	}
	gvr, err := refGVR(apiVersion, kind)
	if err != nil {
		return nil, err
	}
	namespace, _ := ref["namespace"].(string)
	if namespace == "" {
		namespace = defaultNamespace
	}
	obj, err := dyn.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get %s %s/%s: %w", kind, namespace, name, err)
	}
	return obj, nil
}

// nestedRef reads an object reference at path as a plain map, nil when
// absent.
func nestedRef(obj *unstructured.Unstructured, path ...string) map[string]any {
	ref, found, err := unstructured.NestedMap(obj.Object, path...)
	if err != nil || !found {
		return nil
	}
	return ref
}

func nestedString(obj *unstructured.Unstructured, path ...string) string {
	v, _, _ := unstructured.NestedString(obj.Object, path...)
	return v
}

// nestedInt reads an integer field; a JSON-decoded object carries numbers as
// float64, the API machinery's decoder as int64.
func nestedInt(obj *unstructured.Unstructured, path ...string) int64 {
	v, found, err := unstructured.NestedFieldNoCopy(obj.Object, path...)
	if err != nil || !found {
		return 0
	}
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}
