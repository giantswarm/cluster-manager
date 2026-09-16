package compose

import (
	_ "embed"
	"fmt"
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
// refuses a Gateway with neither. The GPU
// pool's label goes to modelServing.gpuPool.nodeSelector (the chart's
// placement contract, giantswarm/agent-platform#315) and, until a pinned
// chart carries that key, to modelServing.serving.nodeSelector, the route the
// discovery ConfigMap takes to the predictors today.
func SliceValues(c Cluster, s SliceSpec) (map[string]any, error) {
	if s.Platform.Domain == "" {
		return nil, fmt.Errorf("the platform's global.domain is empty: the slice's domain and models host derive from it")
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
		set(!s.OwnCluster, "components", "agentgateway", "enabled"),
		set(KubeconfigSecretName(c.Name), "gitops", "target", "kubeConfig", "secretRef", "name"),
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
		if enabled, _ := component["enabled"].(bool); enabled && !serving[name] {
			return true
		}
	}
	return false
}
