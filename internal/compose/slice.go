package compose

import (
	_ "embed"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	// DefaultSliceChartVersion is the exact pin of the slice release: the
	// newest released chart at the time of this version, the first with the
	// serving-slice profile (giantswarm/agent-platform#481). A bump is
	// explicit, like the pool chart's.
	DefaultSliceChartVersion = "4.25.0"
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
	// ChartVersion is the exact chart pin; empty means DefaultSliceChartVersion.
	ChartVersion string
	// OwnCluster marks the installation's own cluster: the release runs
	// beside the platform's, which owns the Gateway API data plane, so
	// agentgateway stays off and the domain is the platform's.
	OwnCluster bool
	Platform   PlatformInputs
	// Pool is the GPU pool the predictors are placed on (its name within
	// the cluster); empty places them by the chart's defaults.
	Pool string
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

// Slice renders the `<cluster>-agent-platform` release: an OCIRepository
// pinning the chart exactly and a HelmRelease in the cluster's namespace on
// the installation whose values are the serving-slice profile filled from
// the platform's inputs. The HelmRelease itself carries no kubeConfig: the
// meta chart renders the component HelmReleases onto the installation and
// the target knob in its values (gitops.target.kubeConfig.secretRef) makes
// the installation's helm-controller install them into a workload cluster.
func Slice(c Cluster, s SliceSpec) ([]*unstructured.Unstructured, error) {
	values, err := SliceValues(c, s)
	if err != nil {
		return nil, err
	}
	name := SliceReleaseName(c.Name)
	version := s.ChartVersion
	if version == "" {
		version = DefaultSliceChartVersion
	}
	meta := objectMeta(c, map[string]any{
		LabelChartName: SliceChart,
		LabelManagedBy: ManagedBy,
		LabelCluster:   c.Name,
	})
	source := object(OCIRepositoryGVR, "OCIRepository", meta(name), map[string]any{"spec": ociRepositorySpec(SliceChartURL, "tag", version)})
	spec := helmReleaseSpec(name, true, values)
	return []*unstructured.Unstructured{source, object(HelmReleaseGVR, "HelmRelease", meta(name), map[string]any{"spec": spec})}, nil
}

// SliceValues is the release's values: the profile with the installation's
// inputs layered on. agentgateway is on for a workload cluster (the target
// runs no controller of its own) and off beside the platform's release; a
// workload cluster gets the target knob with its kubeconfig Secret. The GPU
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
	}
	if s.OwnCluster && s.Platform.TLSSecretName != "" {
		steps = append(steps, set(s.Platform.TLSSecretName, "gatewayApi", "gateway", "tls", "secretName"))
	}
	if !s.OwnCluster {
		steps = append(steps, set(KubeconfigSecretName(c.Name), "gitops", "target", "kubeConfig", "secretRef", "name"))
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
