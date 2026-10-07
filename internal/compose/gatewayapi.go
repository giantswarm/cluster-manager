package compose

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

// The Gateway API CRDs of a slice (giantswarm/cluster-manager#183): the
// connectivity release renders the models Gateway and its routes, and a
// cluster whose API serves no Gateway kind fails its install. The meta
// chart's gateway-api-crds component is the fleet's gateway-api-crds chart
// (the CRD bundle the installations run, standard channel), ordered before
// connectivity and agentgateway; the slice turns it on where the target has
// none.
const (
	// GatewayAPICRDsComponent is the meta chart's component of the Gateway
	// API CRDs.
	GatewayAPICRDsComponent = "gateway-api-crds"
	// GatewayAPIVersion is the Gateway API bundle the component's chart
	// line installs (gateway-api-crds 1.9.x).
	GatewayAPIVersion = "v1.6.1"
	// MinGatewayAPICRDsChartVersion is the first agent-platform chart with
	// the component: an older one takes components.gateway-api-crds for a
	// feature switch and renders no release, so the connectivity release
	// fails on the missing kinds as before.
	MinGatewayAPICRDsChartVersion = "4.117.0"
)

// GatewayAPICRDs are the CRDs the component installs: the standard channel
// of GatewayAPIVersion, the gateway-api-crds chart's defaults. The CRDs are
// applied by the chart's hook Job, no object of its release, so an uninstall
// leaves them; a teardown that composed them removes these.
var GatewayAPICRDs = []string{
	"backendtlspolicies.gateway.networking.k8s.io",
	"gatewayclasses.gateway.networking.k8s.io",
	"gateways.gateway.networking.k8s.io",
	"grpcroutes.gateway.networking.k8s.io",
	"httproutes.gateway.networking.k8s.io",
	"listenersets.gateway.networking.k8s.io",
	"referencegrants.gateway.networking.k8s.io",
	"tcproutes.gateway.networking.k8s.io",
	"tlsroutes.gateway.networking.k8s.io",
	"udproutes.gateway.networking.k8s.io",
}

// GatewayAPICRDsOn reports whether a slice release's values switch the
// Gateway API CRDs component on: the slice composed them.
func GatewayAPICRDsOn(values map[string]any) bool {
	on, _, _ := unstructured.NestedBool(values, "components", GatewayAPICRDsComponent, valueEnabled)
	return on
}
