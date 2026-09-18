package compose

import (
	_ "embed"
	"fmt"
	"net/url"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/version"
	"sigs.k8s.io/yaml"
)

// The slice release's chart and its source (bumblebee-plans#46 D4).
const (
	// SliceChart is the chart of the `<cluster>-agent-platform` release: the
	// agent-platform meta chart, the cluster's one release of it with the
	// slices as toggles in its values.
	SliceChart = "agent-platform"
	// SliceChartURL is the catalog location of the agent-platform chart.
	SliceChartURL = "oci://gsoci.azurecr.io/charts/giantswarm/agent-platform"
	// MinSliceChartVersion is the floor of the slice release's pin: the
	// first chart whose serving slice places and tolerates the predictors on
	// a tainted GPU pool (modelServing.gpuPool, giantswarm/agent-platform#315).
	// The pin itself is the version the installation's own platform release
	// runs (SliceChartVersion): released by construction and known to work on
	// the installation. A release below the floor is refused, naming why.
	MinSliceChartVersion = "4.27.0"
	// SliceReleaseSuffix names the release after the cluster and the chart.
	SliceReleaseSuffix = "-" + SliceChart
	// LabelMachinePool is the node label the gpu-node-pool chart stamps on a
	// pool's nodes: `<cluster>-<pool>`.
	LabelMachinePool = "giantswarm.io/machine-pool"
	// DefaultCacheClaimName is the connectivity chart's default name of the
	// model cache claim (`modelServing.cache.pvc.name`): what the slice
	// mounts when the values name none.
	DefaultCacheClaimName = "hf-cache"
)

// Where the installation's Dex serves its key set in-cluster
// (giantswarm/cluster-manager#30): the chart's defaults for the platform's
// gateway.jwksEgress, what the platform's release runs when it names none —
// the source its own JWT policies validate against
// (kagent.controllerRoute.jwtAuthentication.jwks).
const (
	// DefaultDexNamespace is the namespace the installation's Dex runs in.
	DefaultDexNamespace = "giantswarm"
	// DefaultDexJWKSPort is Dex's plaintext port.
	DefaultDexJWKSPort int64 = 5556
	// dexService is the Dex Service's name; jwksPath where Dex serves its
	// key set (the chart's jwks.path default).
	dexService = "dex"
	jwksPath   = "/keys"
	// httpsPort is the one port that implies TLS to the connectivity chart.
	httpsPort int64 = 443
)

// servingSliceProfile is the serving-slice values profile of the chart
// (helm/agent-platform/examples/serving-slice.yaml on agent-platform main):
// the component roster, the `nvidia` RuntimeClass and the models Gateway.
//
//go:embed serving-slice.yaml
var servingSliceProfile []byte

// ServingComponents are the components the serving slice switches on; a
// slice release with any other component on carries another slice too and
// is not removed with the last GPU pool.
var ServingComponents = []string{"kserve-crd", "kserve-resources", "kserve-llmisvc-crd", "kserve-llmisvc-resources", "kserve-runtime-configs", "modelServing", "agentgateway"}

// PlatformInputs are the values the slice release takes from the
// installation's own platform release, never invented: the domain, the
// login identity and the wildcard certificate.
type PlatformInputs struct {
	// Release names the platform's release the inputs were read from
	// (`<namespace>/<name>` of its HelmRelease), for the messages.
	Release string
	// ChartVersion is the chart version the platform's release runs
	// (its HelmRelease's status.history[0].chartVersion): the slice
	// release's pin.
	ChartVersion string
	// Domain is the platform's global.domain.
	Domain string
	// Identity is the platform's global.identity as its release has it
	// (issuerUrl, clientId, existingSecret — names, never a credential).
	Identity map[string]any
	// TLSSecretName is the platform's gatewayApi.gateway.tls.secretName,
	// the wildcard certificate of the domain.
	TLSSecretName string
	// Dex is where the installation's Dex serves its key set in-cluster, the
	// platform's gateway.jwksEgress (namespace and port); a field the
	// release leaves unset is the chart's default (DefaultDexNamespace,
	// DefaultDexJWKSPort).
	Dex DexService
}

// DexService is where the installation's Dex serves its key set in-cluster.
type DexService struct {
	Namespace string
	Port      int64
}

// JWKSSource is where the slice's models Gateway fetches the login issuer's
// key set: a host and a port, TLS implied by 443 alone (the connectivity
// chart's rule: 443 serves no plain HTTP, every other port is plaintext
// unless jwks.tls.enabled asks for TLS). Namespace is the one an in-cluster
// host resolves in — the platform's gateway.jwksEgress.namespace —, empty
// for a public issuer.
type JWKSSource struct {
	Host      string
	Port      int64
	Namespace string
}

// InCluster reports whether the source is a Service of the cluster (the
// platform's Dex) rather than a public issuer.
func (j JWKSSource) InCluster() bool { return j.Namespace != "" }

// URL is the source as a URL, for the messages.
func (j JWKSSource) URL() string {
	if j.Port == httpsPort {
		return "https://" + j.Host + jwksPath
	}
	return fmt.Sprintf("http://%s:%d%s", j.Host, j.Port, jwksPath)
}

// SliceSpec is the slice release's shape for one cluster.
type SliceSpec struct {
	// ChartVersion is an explicit chart pin, honoured as given (a lab's
	// unreleased chart, a test); empty pins the version the platform's
	// release runs, at least MinSliceChartVersion.
	ChartVersion string
	// OwnCluster marks the installation's own cluster: the release runs
	// beside the platform's, which owns the Gateway API data plane, so
	// agentgateway stays off and the domain is the platform's. Delivery
	// does not differ: the own cluster's kubeconfig Secret exists like any
	// cluster's.
	OwnCluster bool
	Platform   PlatformInputs
	// Pool is the GPU pool the predictors are placed on (its name within
	// the cluster); empty places them by the chart's defaults.
	Pool string
	// CertificateIssuer is the cert-manager ClusterIssuer the models
	// Gateway's certificate comes from when the platform's wildcard is not
	// usable: on a workload cluster (another domain), and on the own cluster
	// when the platform's release names no gatewayApi.gateway.tls.secretName
	// (its TLS terminated at the fleet's edge, the wildcard in another
	// namespace). Empty composes no issuer.
	CertificateIssuer string
	// NoCache serves without the model cache: the connectivity chart's
	// `modelServing.cache.enabled: false`, so no cache claim is applied or
	// mounted and every predictor downloads its weights into its pod's
	// ephemeral storage (the node's local disk) at each start. A claim that
	// exists is left as it is: the chart keeps it (helm.sh/resource-policy
	// keep). The zero value keeps the chart's default, the cache on
	// (giantswarm/cluster-manager#65).
	NoCache bool
	// CacheClaim names the model cache claim the slice's predictors mount:
	// the connectivity chart's `modelServing.cache.pvc.name` — the claim of
	// the zone the pool runs in (`<base>-<zone>`), or the one claim of
	// before (giantswarm/cluster-manager#71). The chart creates the claim
	// where it does not exist and keeps it. Empty, or the chart's default
	// (DefaultCacheClaimName), writes nothing; nothing is written with
	// NoCache either, since no claim is mounted then.
	CacheClaim string
}

// SliceReleaseName names the slice release of a cluster.
func SliceReleaseName(cluster string) string { return cluster + SliceReleaseSuffix }

// SliceDomain is the slice's global.domain: the platform's own on the
// installation's cluster, `<cluster>.<base domain>` on a workload cluster —
// the models Gateway answers at models.<domain>.
func SliceDomain(c Cluster, s SliceSpec) string {
	if s.OwnCluster {
		return s.Platform.Domain
	}
	return c.Name + "." + c.BaseDomain
}

// SliceJWKS is the models Gateway's JWKS source. On the installation's own
// cluster it is the platform's Dex service in plaintext
// (dex.<namespace>.svc.cluster.local:5556), the source the platform's own
// JWT policies validate against: the chart's default, the public issuer on
// 443, never yielded a key set there, and every id_token was refused as
// signed by an unknown key until the backend was pointed at the Service by
// hand (giantswarm/agent-platform#505, proof 1 of giantswarm/giantswarm#37639).
// A workload cluster cannot reach the installation's Service and keeps the
// chart's default: the issuer's host on 443, the platform's
// global.identity.issuerUrl — refused when that names no host, since the
// policy would validate nothing.
func SliceJWKS(s SliceSpec) (JWKSSource, error) {
	if s.OwnCluster {
		namespace, port := s.Platform.Dex.Namespace, s.Platform.Dex.Port
		if namespace == "" {
			namespace = DefaultDexNamespace
		}
		if port == 0 {
			port = DefaultDexJWKSPort
		}
		return JWKSSource{Host: fmt.Sprintf("%s.%s.svc.cluster.local", dexService, namespace), Port: port, Namespace: namespace}, nil
	}
	issuer, _ := s.Platform.Identity["issuerUrl"].(string)
	u, err := url.Parse(issuer)
	if err != nil || u.Hostname() == "" {
		return JWKSSource{}, fmt.Errorf("the platform's global.identity.issuerUrl %q names no host: the models Gateway's JWT policy validates a person's id_token against the issuer's key set", issuer)
	}
	return JWKSSource{Host: u.Hostname(), Port: httpsPort}, nil
}

// SliceChartVersion resolves the slice release's chart pin: the spec's
// explicit version when set, else the version the installation's platform
// release runs — refused below MinSliceChartVersion, the first chart that
// places the predictors on a tainted GPU pool, and when the release has not
// deployed a chart yet. Flux records the chart's digest as the version's
// build metadata (`4.27.2+b9d9972a5aca`); the pin is the chart's tag, so
// the metadata is dropped.
func SliceChartVersion(s SliceSpec) (string, error) {
	if s.ChartVersion != "" {
		return s.ChartVersion, nil
	}
	release := s.Platform.Release
	if release == "" {
		release = "the platform's release"
	}
	if s.Platform.ChartVersion == "" {
		return "", fmt.Errorf("%s has not deployed a chart yet (no status.history): the slice release pins the %s chart version the platform runs — wait for the platform's release to be ready and re-run", release, SliceChart)
	}
	running, err := version.ParseSemantic(s.Platform.ChartVersion)
	if err != nil {
		return "", fmt.Errorf("%s runs %s chart %q, not a semantic version: the slice release pins the chart version the platform runs", release, SliceChart, s.Platform.ChartVersion)
	}
	tag, _, _ := strings.Cut(s.Platform.ChartVersion, "+")
	if running.LessThan(version.MustParseSemantic(MinSliceChartVersion)) {
		return "", fmt.Errorf("%s runs %s chart %s, below %s, the first whose serving slice places the predictors on a tainted GPU pool (modelServing.gpuPool, giantswarm/agent-platform#315): upgrade the platform to %s or newer and re-run", release, SliceChart, tag, MinSliceChartVersion, MinSliceChartVersion)
	}
	return tag, nil
}

// Slice renders the `<cluster>-agent-platform` release: an OCIRepository
// pinning the chart exactly (SliceChartVersion) and a HelmRelease in the
// cluster's namespace on the installation whose values are the serving-slice
// profile filled from the platform's inputs. The HelmRelease runs under the
// org's tenant ServiceAccount (see Delivery in compose.go): the meta chart
// renders the component HelmReleases into that namespace on the
// installation, and the target knob in its values
// (gitops.target.kubeConfig.secretRef) makes the installation's
// helm-controller install each of them into the cluster through its
// kubeconfig Secret — the installation's own cluster included.
func Slice(c Cluster, s SliceSpec) ([]*unstructured.Unstructured, error) {
	values, err := SliceValues(c, s)
	if err != nil {
		return nil, err
	}
	name := SliceReleaseName(c.Name)
	version, err := SliceChartVersion(s)
	if err != nil {
		return nil, err
	}
	meta := objectMeta(c, map[string]any{
		LabelChartName: SliceChart,
		LabelManagedBy: ManagedBy,
		LabelCluster:   c.Name,
	})
	source := object(OCIRepositoryGVR, "OCIRepository", meta(name), map[string]any{"spec": ociRepositorySpec(SliceChartURL, "tag", version)})
	spec := helmReleaseSpec(name, true, values)
	deliverAsTenant(spec, c.TenantServiceAccount)
	return []*unstructured.Unstructured{source, object(HelmReleaseGVR, "HelmRelease", meta(name), map[string]any{"spec": spec})}, nil
}

// SliceValues is the release's values: the profile with the installation's
// inputs layered on. agentgateway is on for a workload cluster (the target
// runs no controller of its own) and off beside the platform's release; every
// cluster gets the target knob with its kubeconfig Secret, so each child
// release is kubeconfig-delivered and may install outside the org namespace
// (see Delivery in compose.go). The models Gateway's certificate is the
// platform's wildcard where it covers models.<domain> and is referenceable
// (the own cluster, the platform naming it), else a cert-manager Certificate
// of the host from the configured ClusterIssuer — the connectivity chart
// refuses a Gateway with neither. The models Gateway's JWKS source is the
// platform's Dex service on the own cluster (SliceJWKS), and with an
// in-cluster host travels gateway.jwksEgress — enabled, the host's namespace
// and port —, the connectivity chart's precondition for one: it renders no
// route to an in-cluster issuer its egress does not name and refuses the
// values (validate.yaml, `gateway.jwksEgress.enabled is false`), which on
// gazelle left the slice without its models Gateway while the meta release
// read Ready (giantswarm/cluster-manager#30). The slice runs no agentgateway
// controller of its own beside the platform's release, so the block states
// the same facts the platform's release does and opens nothing new. A
// workload cluster keeps the chart's default, the public issuer, and no
// jwksEgress. The GPU
// pool's label goes to modelServing.gpuPool.nodeSelector (the chart's
// placement contract, giantswarm/agent-platform#315) and, until a pinned
// chart carries that key, to modelServing.serving.nodeSelector, the route the
// discovery ConfigMap takes to the predictors today. The model cache is the
// chart's default, on, unless the spec switches it off (NoCache) or names the
// claim the predictors mount (CacheClaim, the claim of the pool's zone).
func SliceValues(c Cluster, s SliceSpec) (map[string]any, error) {
	if s.Platform.Domain == "" {
		return nil, fmt.Errorf("the platform's global.domain is empty: the slice's domain and models host derive from it")
	}
	jwks, err := SliceJWKS(s)
	if err != nil {
		return nil, err
	}
	values := map[string]any{}
	if err := yaml.Unmarshal(servingSliceProfile, &values); err != nil {
		return nil, fmt.Errorf("decode the serving-slice profile: %w", err)
	}
	identity := map[string]any{}
	for k, v := range s.Platform.Identity {
		identity[k] = v
	}
	set := func(v any, path ...string) error { return unstructured.SetNestedField(values, v, path...) }
	steps := []error{
		set(SliceDomain(c, s), "global", "domain"),
		set(identity, "global", "identity"),
		set(!s.OwnCluster, "components", "agentgateway", valueEnabled),
		set(KubeconfigSecretName(c.Name), "gitops", "target", "kubeConfig", "secretRef", "name"),
	}
	if jwks.InCluster() {
		steps = append(steps,
			set(jwks.Host, "modelServing", "modelsGateway", "jwtAuthentication", "jwks", "host"),
			set(jwks.Port, "modelServing", "modelsGateway", "jwtAuthentication", "jwks", "port"),
			set(true, "gateway", "jwksEgress", valueEnabled),
			set(jwks.Namespace, "gateway", "jwksEgress", "namespace"),
			set(jwks.Port, "gateway", "jwksEgress", "port"),
		)
	}
	switch {
	case s.OwnCluster && s.Platform.TLSSecretName != "":
		steps = append(steps, set(s.Platform.TLSSecretName, "gatewayApi", "gateway", "tls", "secretName"))
	case s.CertificateIssuer != "":
		steps = append(steps, set(s.CertificateIssuer, "modelServing", "modelsGateway", "tls", "issuerRef", "name"))
	}
	if s.Pool != "" {
		selector := map[string]any{LabelMachinePool: ReleaseName(c.Name, s.Pool)}
		steps = append(steps,
			set(selector, "modelServing", "gpuPool", "nodeSelector"),
			set(selector, "modelServing", "serving", "nodeSelector"),
		)
	}
	switch {
	case s.NoCache:
		steps = append(steps, set(false, "modelServing", "cache", "enabled"))
	case s.CacheClaim != "" && s.CacheClaim != DefaultCacheClaimName:
		steps = append(steps, set(s.CacheClaim, "modelServing", "cache", "pvc", "name"))
	}
	for _, err := range steps {
		if err != nil {
			return nil, fmt.Errorf("compose slice values: %w", err)
		}
	}
	return values, nil
}

// ModelsHost is where a served model answers for the slice's domain.
func ModelsHost(domain string) string { return "models." + domain }

// OtherSliceOn reports whether a slice release's values switch on a
// component outside the serving slice — another slice shares the release,
// which then stays when model serving goes.
func OtherSliceOn(values map[string]any) bool {
	components, _, _ := unstructured.NestedMap(values, "components")
	serving := map[string]bool{}
	for _, name := range ServingComponents {
		serving[name] = true
	}
	for name, v := range components {
		component, _ := v.(map[string]any)
		if enabled, _ := component[valueEnabled].(bool); enabled && !serving[name] {
			return true
		}
	}
	return false
}
