package compose

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func platform() PlatformInputs {
	return PlatformInputs{
		Release:       "flux-giantswarm/agent-platform",
		ChartVersion:  "4.27.2",
		Domain:        "gazelle.example.io",
		Identity:      map[string]any{"issuerUrl": "https://dex.gazelle.example.io", "clientId": "dex-k8s-authenticator", "existingSecret": "agent-platform-identity"},
		TLSSecretName: "gazelle-wildcard-tls",
	}
}

// TestSliceGoldens pins the `<cluster>-agent-platform` release byte for
// byte: the slice beside the platform's release on the installation's own
// cluster (agentgateway off, the platform's domain and certificate), the same
// where the platform's TLS is terminated at the fleet's edge and its release
// names no wildcard (a Certificate of the models host from the fleet's
// ClusterIssuer instead), onto a workload cluster with a pool (agentgateway
// on, the pool's label as node selector, the issuer's Certificate), and onto
// a workload cluster without a pool (enable_model_serving before any pool).
func TestSliceGoldens(t *testing.T) {
	own := wc1()
	own.Name, own.Namespace, own.Organization, own.UID = "gazelle", "org-giantswarm", "giantswarm", "6f1c0c1e-8a4a-4c1e-9c3a-000000000000"
	edge := platform()
	edge.TLSSecretName = ""
	cases := []struct {
		name    string
		cluster Cluster
		spec    SliceSpec
		domain  string
	}{
		{"own-cluster", own, SliceSpec{OwnCluster: true, Platform: platform(), Pool: "gpu-l4", CertificateIssuer: DefaultCertificateIssuer}, "gazelle.example.io"},
		{"own-cluster-edge-tls", own, SliceSpec{OwnCluster: true, Platform: edge, Pool: "gpu-l4", CertificateIssuer: DefaultCertificateIssuer}, "gazelle.example.io"},
		{"workload", wc1(), SliceSpec{Platform: platform(), Pool: "gpu-l4", CertificateIssuer: DefaultCertificateIssuer}, "wc1.acme.example.io"},
		{"workload-no-pool", wc1(), SliceSpec{Platform: platform(), CertificateIssuer: DefaultCertificateIssuer}, "wc1.acme.example.io"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs, err := Slice(tc.cluster, tc.spec)
			require.NoError(t, err)
			assertGolden(t, "slice-"+tc.name, objs)
			require.Len(t, objs, 2)
			source, release := objs[0], objs[1]
			assert.Equal(t, SliceReleaseName(tc.cluster.Name), release.GetName())
			assert.Equal(t, SliceChart, release.GetLabels()[LabelChartName])
			assert.Equal(t, tc.cluster.Name, release.GetOwnerReferences()[0].Name, "owned by the Cluster in apply mode")
			tag, _, _ := unstructured.NestedString(source.Object, "spec", "ref", "tag")
			assert.Equal(t, platform().ChartVersion, tag, "the slice pins the version the platform's release runs")
			_, hasKubeconfig, _ := unstructured.NestedString(release.Object, "spec", "kubeConfig", "secretRef", "name")
			assert.False(t, hasKubeconfig, "the meta chart's own HelmRelease stays on the installation; the target knob is in its values")
			sa, _, _ := unstructured.NestedString(release.Object, "spec", "serviceAccountName")
			assert.Equal(t, DefaultTenantServiceAccount, sa, "the meta chart's Flux objects live in the org namespace: delivered as the tenant")

			values, _, _ := unstructured.NestedMap(release.Object, "spec", "values")
			domain, _, _ := unstructured.NestedString(values, "global", "domain")
			assert.Equal(t, tc.domain, domain)
			assert.Equal(t, "models."+tc.domain, ModelsHost(domain))
			identity, _, _ := unstructured.NestedMap(values, "global", "identity")
			assert.Equal(t, platform().Identity, identity, "the platform's identity as its release has it")
			flux, _, _ := unstructured.NestedBool(values, "components", "flux", "enabled")
			assert.False(t, flux, "the installation's Flux delivers")
			modelManager, _, _ := unstructured.NestedBool(values, "components", "model-manager", "enabled")
			assert.False(t, modelManager, "one model-manager per installation")
			agentgateway, _, _ := unstructured.NestedBool(values, "components", "agentgateway", "enabled")
			assert.Equal(t, !tc.spec.OwnCluster, agentgateway)
			target, _, _ := unstructured.NestedString(values, "gitops", "target", "kubeConfig", "secretRef", "name")
			assert.Equal(t, KubeconfigSecretName(tc.cluster.Name), target, "the target knob on every cluster: each child release is kubeconfig-delivered")
			tls, hasTLS, _ := unstructured.NestedString(values, "gatewayApi", "gateway", "tls", "secretName")
			wildcard := tc.spec.OwnCluster && tc.spec.Platform.TLSSecretName != ""
			assert.Equal(t, wildcard, hasTLS, "the platform's wildcard certificate covers models.<domain> only on its own cluster, and only where the platform names it")
			if hasTLS {
				assert.Equal(t, "gazelle-wildcard-tls", tls)
			}
			issuer, hasIssuer, _ := unstructured.NestedString(values, "modelServing", "modelsGateway", "tls", "issuerRef", "name")
			assert.Equal(t, !wildcard, hasIssuer, "without a usable wildcard the models host gets a Certificate from the fleet's ClusterIssuer")
			if hasIssuer {
				assert.Equal(t, DefaultCertificateIssuer, issuer)
			}
			for _, component := range ServingComponents[:6] {
				on, _, _ := unstructured.NestedBool(values, "components", component, "enabled")
				assert.True(t, on, component)
			}
			runtimeClass, _, _ := unstructured.NestedString(values, "modelServing", "serving", "runtimeClassName")
			assert.Equal(t, "nvidia", runtimeClass)
			selector, hasSelector, _ := unstructured.NestedMap(values, "modelServing", "gpuPool", "nodeSelector")
			assert.Equal(t, tc.spec.Pool != "", hasSelector)
			if hasSelector {
				assert.Equal(t, map[string]any{LabelMachinePool: tc.cluster.Name + "-" + tc.spec.Pool}, selector)
				serving, _, _ := unstructured.NestedMap(values, "modelServing", "serving", "nodeSelector")
				assert.Equal(t, selector, serving, "the same selector on the route the pinned chart honours")
			}
			assert.False(t, OtherSliceOn(values), "the serving slice alone")
		})
	}
}

// TestSliceWithoutCertificateIssuer: an empty issuer composes no tls at all
// (a lab whose own cluster carries the wildcard needs none; the connectivity
// chart refuses a models Gateway with neither, naming the knobs).
func TestSliceWithoutCertificateIssuer(t *testing.T) {
	values, err := SliceValues(wc1(), SliceSpec{Platform: platform()})
	require.NoError(t, err)
	_, hasIssuer, _ := unstructured.NestedString(values, "modelServing", "modelsGateway", "tls", "issuerRef", "name")
	assert.False(t, hasIssuer)
	_, hasTLS, _ := unstructured.NestedString(values, "gatewayApi", "gateway", "tls", "secretName")
	assert.False(t, hasTLS, "the platform's wildcard does not cover a workload cluster's domain")
}

func TestSliceRefusesWithoutDomain(t *testing.T) {
	_, err := Slice(wc1(), SliceSpec{})
	require.ErrorContains(t, err, "global.domain is empty")
}

func TestOtherSliceOn(t *testing.T) {
	values, err := SliceValues(wc1(), SliceSpec{Platform: platform()})
	require.NoError(t, err)
	assert.False(t, OtherSliceOn(values))
	require.NoError(t, unstructured.SetNestedField(values, true, "components", "kagent", "enabled"))
	assert.True(t, OtherSliceOn(values), "the runtime slice shares the release")
}

// TestSliceChartVersion: the pin is the version the platform's release runs
// without the digest Flux records as build metadata, refused below the floor (naming the release, the version, the floor and
// why), before the first deployment and for a non-semver revision; an
// explicit version is honoured as given, floor or not.
func TestSliceChartVersion(t *testing.T) {
	cases := []struct {
		name    string
		spec    SliceSpec
		want    string
		refusal string
	}{
		{"platform's version", SliceSpec{Platform: platform()}, "4.27.2", ""},
		{"digest as build metadata dropped", SliceSpec{Platform: PlatformInputs{Release: "flux-giantswarm/agent-platform", ChartVersion: "4.28.0+1c7eb3256e07"}}, "4.28.0", ""},
		{"below the floor with build metadata", SliceSpec{Platform: PlatformInputs{Release: "flux-giantswarm/agent-platform", ChartVersion: "4.25.0+c78155660389"}}, "", "runs agent-platform chart 4.25.0, below 4.27.0"},
		{"exactly the floor", SliceSpec{Platform: PlatformInputs{Release: "flux-giantswarm/agent-platform", ChartVersion: MinSliceChartVersion}}, MinSliceChartVersion, ""},
		{"below the floor", SliceSpec{Platform: PlatformInputs{Release: "flux-giantswarm/agent-platform", ChartVersion: "4.25.0"}}, "", "flux-giantswarm/agent-platform runs agent-platform chart 4.25.0, below 4.27.0, the first whose serving slice places the predictors on a tainted GPU pool (modelServing.gpuPool, giantswarm/agent-platform#315): upgrade the platform to 4.27.0 or newer and re-run"},
		{"prerelease below the floor", SliceSpec{Platform: PlatformInputs{Release: "flux-giantswarm/agent-platform", ChartVersion: "4.27.0-rc.1"}}, "", "runs agent-platform chart 4.27.0-rc.1, below 4.27.0"},
		{"not deployed yet", SliceSpec{Platform: PlatformInputs{Release: "flux-giantswarm/agent-platform"}}, "", "flux-giantswarm/agent-platform has not deployed a chart yet (no status.history)"},
		{"not a semver", SliceSpec{Platform: PlatformInputs{Release: "flux-giantswarm/agent-platform", ChartVersion: "latest"}}, "", `runs agent-platform chart "latest", not a semantic version`},
		{"override wins", SliceSpec{ChartVersion: "0.0.0-lab", Platform: PlatformInputs{ChartVersion: "4.25.0"}}, "0.0.0-lab", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SliceChartVersion(tc.spec)
			if tc.refusal != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.refusal)
				_, err = Slice(wc1(), tc.spec)
				assert.Error(t, err, "Slice refuses the same way")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
