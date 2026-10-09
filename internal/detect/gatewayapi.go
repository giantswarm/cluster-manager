package detect

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// The Gateway API on the target (giantswarm/cluster-manager#183). The serving
// slice's connectivity release renders the models Gateway and its routes in
// gateway.networking.k8s.io/v1, and its agentgateway serves them: a cluster
// whose API serves no Gateway kind fails that release's install. The
// installation's own cluster runs the CRDs as an app of its own; a workload
// cluster created with kubectl-gs has none.
const (
	// GatewayAPIGroup is the Gateway API's API group.
	GatewayAPIGroup = "gateway.networking.k8s.io"
	// GatewayAPICRD is the CRD whose presence says the target serves the
	// Gateway API.
	GatewayAPICRD = "gateways." + GatewayAPIGroup
	// gatewayAPIBundleVersion and gatewayAPIChannel are the annotations the
	// Gateway API project stamps on every CRD of its bundle.
	gatewayAPIBundleVersion = GatewayAPIGroup + "/bundle-version"
	gatewayAPIChannel       = GatewayAPIGroup + "/channel"
	// AgentgatewayControllerName is the controller of the GatewayClass the
	// agentgateway controller creates for itself at startup: an object of no
	// release, which outlives the controller's uninstall.
	AgentgatewayControllerName = "agentgateway.dev/agentgateway"
)

// GatewayAPI is the Gateway API as the target serves it: whether its Gateway
// CRD is there and, from that CRD's annotations, the bundle version and the
// channel (empty where the CRD carries none).
type GatewayAPI struct {
	Served  bool
	Version string
	Channel string
}

// String words what was found for an answer.
func (g GatewayAPI) String() string {
	if !g.Served {
		return "no Gateway API CRDs"
	}
	version := g.Version
	if version == "" {
		version = "an unannotated bundle"
	}
	if g.Channel == "" {
		return "Gateway API " + version
	}
	return fmt.Sprintf("Gateway API %s (%s channel)", version, g.Channel)
}

// ReadGatewayAPI reads the Gateway CRD on the target. A CRD that cannot be
// read is an error, never taken for absent: composing the CRDs over a bundle
// at another version would replace it.
func ReadGatewayAPI(ctx context.Context, reader dynamic.Interface) (GatewayAPI, error) {
	crd, err := reader.Resource(CRDGVR).Get(ctx, GatewayAPICRD, metav1.GetOptions{})
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return GatewayAPI{}, nil
	}
	if err != nil {
		return GatewayAPI{}, fmt.Errorf("read CRD %s: %w", GatewayAPICRD, err)
	}
	annotations := crd.GetAnnotations()
	return GatewayAPI{Served: true, Version: annotations[gatewayAPIBundleVersion], Channel: annotations[gatewayAPIChannel]}, nil
}

// ForeignGatewayAPIObjects names the objects of the Gateway API kinds crds on
// the target that none of the slice's child releases rendered — a release
// of the installation's namespace whose name starts with `<cluster>-`
// (Flux stamps its labels on every object of a release) — nor the slice's
// agentgateway created for itself (the GatewayClass of
// AgentgatewayControllerName): removing the CRDs would take them along. A
// kind the target does not serve has none.
func ForeignGatewayAPIObjects(ctx context.Context, reader dynamic.Interface, crds []string, namespace, cluster string) ([]string, error) {
	var foreign []string
	for _, name := range crds {
		plural, group, _ := strings.Cut(name, ".")
		gvr, served, err := storageGVR(ctx, reader, schema.GroupResource{Group: group, Resource: plural})
		if err != nil {
			return nil, err
		}
		if !served {
			continue
		}
		list, err := reader.Resource(gvr).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", name, err)
		}
		for _, o := range list.Items {
			labels := o.GetLabels()
			if labels[LabelFluxReleaseNamespace] == namespace && strings.HasPrefix(labels[LabelFluxReleaseName], cluster+"-") {
				continue
			}
			if controller, _, _ := unstructured.NestedString(o.Object, "spec", "controllerName"); o.GetKind() == "GatewayClass" && controller == AgentgatewayControllerName {
				// The slice's agentgateway made it, not a release
				// (giantswarm/cluster-manager#203); a Gateway of the class
				// someone else made is named on its own.
				continue
			}
			ref := o.GetName()
			if ns := o.GetNamespace(); ns != "" {
				ref = ns + "/" + ref
			}
			foreign = append(foreign, o.GetKind()+" "+ref)
		}
	}
	slices.Sort(foreign)
	return foreign, nil
}
