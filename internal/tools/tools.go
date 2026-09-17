// Package tools implements cluster-manager's MCP tools over the Kubernetes
// API of the installation: list_clusters and list_node_pools in this stage,
// the node-pool writes in later ones. Every call reads through the client the
// request's context selects — the caller's own when the forwarded IdP token
// is present, so the caller's RBAC governs what is listed.
package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/detect"
)

// ClusterAPIGroup is the API group the clusters and their MachinePools live
// in; an installation without the KaaS components does not serve it.
const ClusterAPIGroup = "cluster.x-k8s.io"

// Kubernetes resources the tools read.
var (
	ClusterGVR     = schema.GroupVersionResource{Group: ClusterAPIGroup, Version: "v1beta1", Resource: "clusters"}
	MachinePoolGVR = schema.GroupVersionResource{Group: ClusterAPIGroup, Version: "v1beta1", Resource: "machinepools"}
	HelmReleaseGVR = compose.HelmReleaseGVR
	ReleaseGVR     = schema.GroupVersionResource{Group: "release.giantswarm.io", Version: "v1alpha1", Resource: "releases"}
)

// Labels and prefixes of the fleet's conventions.
const (
	LabelClusterName    = "cluster.x-k8s.io/cluster-name"
	LabelReleaseVersion = "release.giantswarm.io/version"
	LabelOrganization   = "giantswarm.io/organization"
	OrgNamespacePrefix  = "org-"
)

// Clients are the Kubernetes clients one call reads through: the dynamic
// client for the resources, discovery for what the apiserver serves.
type Clients struct {
	Dynamic   dynamic.Interface
	Discovery discovery.DiscoveryInterface
}

// ClientsFor returns the clients a call uses: the caller's own when the
// request carries the caller's forwarded token, else the ServiceAccount's.
type ClientsFor func(ctx context.Context) Clients

// Config is the installation-wide configuration of the tools.
type Config struct {
	// Installation is the installation's name; the Cluster of that name is
	// the installation's own cluster (its management cluster).
	Installation string
	// ModelManagerNamespace is where model-manager runs on the installation:
	// the kserve backend document is written there.
	ModelManagerNamespace string
	// ServingNamespace is where the InferenceServices go on a serving
	// cluster (the backend document's target.servingNamespace).
	ServingNamespace string
	// SliceChartVersion, when set, pins the slice release's agent-platform
	// chart as given instead of the version the installation's platform
	// release runs — for a lab running an unreleased chart, or a test.
	SliceChartVersion string
	// TenantServiceAccount is the ServiceAccount in every org namespace the
	// installation's helm-controller runs a composed release under when the
	// release's objects live in that namespace (the pool release, the slice
	// release): `automation` on Giant Swarm installations; empty renders
	// none, for an installation without the tenancy policy.
	TenantServiceAccount string
	// CertificateIssuer is the cert-manager ClusterIssuer the slice release
	// asks for the models host's certificate when the platform's wildcard is
	// not usable (a workload cluster; the own cluster when the platform's
	// release names no gatewayApi.gateway.tls.secretName):
	// `letsencrypt-giantswarm` on Giant Swarm clusters; empty composes none.
	CertificateIssuer string
	// ApplyBudget is how long a write call (create_node_pool,
	// enable_model_serving, delete_node_pool, disable_model_serving) may
	// take from its start before it stops writing and answers with what it
	// did, the rest pending: the aggregator's deadline for an upstream tool
	// call less the answer's way back (giantswarm/cluster-manager#34, #37).
	// A deadline the request itself carries wins when it is earlier. Zero
	// is DefaultApplyBudget.
	ApplyBudget time.Duration
}

// DefaultApplyBudget is the write budget: muster cancels an upstream tool
// call at about ten seconds; two of them are the answer's.
const DefaultApplyBudget = 8 * time.Second

func (c Config) applyBudget() time.Duration {
	if c.ApplyBudget == 0 {
		return DefaultApplyBudget
	}
	return c.ApplyBudget
}

// Service implements the tools.
type Service struct {
	clients ClientsFor
	targets TargetClientsFor
	cfg     Config
	// charts reads the platform's charts from their registry, for the
	// serving presets a slice would publish before it exists (nil: the
	// presets are judged from the cluster's ConfigMaps alone).
	charts compose.ChartReader
}

// Option configures a Service beyond its Config.
type Option func(*Service)

// WithChartReader lets create_node_pool judge the serving presets the slice
// would publish while none is on the cluster yet: read from the connectivity
// chart the slice's agent-platform release resolves in the registry
// (compose.ReadShippedPresets).
func WithChartReader(r compose.ChartReader) Option {
	return func(s *Service) { s.charts = r }
}

// New builds the tools over the per-call clients of the installation and,
// for detection on workload clusters, of their apiservers (nil: workload
// clusters are not read, their detection reports unknown).
func New(clients ClientsFor, targets TargetClientsFor, cfg Config, opts ...Option) *Service {
	s := &Service{clients: clients, targets: targets, cfg: cfg}
	for _, opt := range opts {
		opt(s)
	}
	return s
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

// ErrClusterAPIAbsent is returned when a cluster is asked for on an
// installation that does not serve the Cluster API: no cluster can exist
// there, so the message names the cluster and the missing API group instead
// of the apiserver's bare 404.
type ErrClusterAPIAbsent struct{ Cluster string }

func (e *ErrClusterAPIAbsent) Error() string {
	if e.Cluster == "" {
		return clusterAPIAbsentNote
	}
	return fmt.Sprintf("cluster %s not found: %s", e.Cluster, clusterAPIAbsentNote)
}

// Cluster API states as ClusterAPI.State reports them.
const (
	ClusterAPIServed  = "served"
	ClusterAPIAbsent  = "absent"
	ClusterAPIUnknown = "unknown"
)

var clusterAPIAbsentNote = fmt.Sprintf("the Cluster API (%s) is not served on this installation", ClusterAPIGroup)

// ClusterAPI reports whether the installation serves the Cluster API the
// tools read clusters and MachinePools from. An installation without it has
// no clusters: list_clusters answers an empty list with this note, and the
// tools that name a cluster refuse.
type ClusterAPI struct {
	Group   string `json:"group"`
	Version string `json:"version"`
	// State is served, absent, or unknown when discovery failed (Note
	// carries the error).
	State string `json:"state"`
	// Note explains an absent or unknown state; empty when served.
	Note string `json:"note,omitempty"`
}

// ClusterAPI checks, with one discovery request, whether the installation
// serves the Cluster API's group version. Discovery is open to every
// authenticated principal, so the answer does not depend on the caller's
// RBAC on the clusters themselves. Never an error: a failed discovery is
// the unknown state.
func (s *Service) ClusterAPI(ctx context.Context) ClusterAPI {
	return clusterAPI(s.clients(ctx).Discovery)
}

func clusterAPI(disc discovery.DiscoveryInterface) ClusterAPI {
	gv := ClusterGVR.GroupVersion()
	out := ClusterAPI{Group: gv.Group, Version: gv.Version, State: ClusterAPIServed}
	_, err := disc.ServerResourcesForGroupVersion(gv.String())
	switch {
	case err == nil:
	case apierrors.IsNotFound(err):
		out.State, out.Note = ClusterAPIAbsent, clusterAPIAbsentNote
	default:
		out.State, out.Note = ClusterAPIUnknown, fmt.Sprintf("discover %s: %v", gv, err)
	}
	return out
}

// requireClusterAPI is the check before a Cluster API read: absent refuses
// naming the cluster asked for (empty for a list), unknown surfaces the
// discovery error.
func requireClusterAPI(disc discovery.DiscoveryInterface, cluster string) error {
	switch api := clusterAPI(disc); api.State {
	case ClusterAPIAbsent:
		return &ErrClusterAPIAbsent{Cluster: cluster}
	case ClusterAPIUnknown:
		return errors.New(api.Note)
	}
	return nil
}

// getCluster finds a Cluster by name: in namespace when given, else across
// the installation's namespaces (an error when the name is ambiguous). On
// an installation without the Cluster API the answer is ErrClusterAPIAbsent.
func (s *Service) getCluster(ctx context.Context, k Clients, name, namespace string) (*unstructured.Unstructured, error) {
	qualified := name
	if namespace != "" {
		qualified = namespace + "/" + name
	}
	if err := requireClusterAPI(k.Discovery, qualified); err != nil {
		return nil, err
	}
	dyn := k.Dynamic
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
// identity is what every composed release knows about its cluster before
// anything is read from it: the names, the UID the objects own-reference
// and the tenant the org namespace's releases run under.
func (s *Service) identity(c *unstructured.Unstructured) compose.Cluster {
	return compose.Cluster{Name: c.GetName(), Namespace: c.GetNamespace(), Organization: organization(c), UID: string(c.GetUID()), TenantServiceAccount: s.cfg.TenantServiceAccount}
}

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

// nestedInt reads an integer field, whichever decoder produced the object.
func nestedInt(obj *unstructured.Unstructured, path ...string) int64 {
	return detect.NestedInt(obj, path...)
}
