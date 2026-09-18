package compose

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

var update = flag.Bool("update", false, "rewrite the golden files from the current output")

// wc1 is the fixture cluster the goldens are rendered for: release aws-31.0.0
// (kubernetes 1.31.4, flatcar 4081.2.1, os-tooling 1.26.1), one registry
// mirror without credentials, no proxy, teleport on, the fleet's tenant
// ServiceAccount in its org namespace.
func wc1() Cluster {
	return Cluster{
		Name: "wc1", Namespace: "org-acme", Organization: "acme", UID: "6f1c0c1e-8a4a-4c1e-9c3a-000000000001",
		TenantServiceAccount: DefaultTenantServiceAccount,
		KubernetesVersion:    "1.31.4", MachineImage: "flatcar-stable-4081.2.1-kube-1.31.4-tooling-1.26.1-gs",
		BaseDomain: "acme.example.io", ManagementCluster: "gazelle",
		RegistryMirrors: map[string][]string{"gsoci.azurecr.io": {"gsoci.azurecr.io"}},
		CiliumIPAMMode:  "kubernetes", Teleport: true,
	}
}

// own is the installation's own cluster: the pool's nodes join the cluster
// the release lives on, the one place the chart renders pool.prewarm.
func own() Cluster {
	c := wc1()
	c.Name, c.Namespace, c.Organization = c.ManagementCluster, "org-giantswarm", "giantswarm"
	return c
}

// TestPoolGoldens pins the HelmRelease and OCIRepository shape byte for byte
// per input: one golden per accelerator, plus the snapshot variants (proxy,
// registry credentials as a valuesFrom Secret, teleport off, explicit sizes)
// and the own cluster's pool with prewarm (giantswarm/cluster-manager#48).
func TestPoolGoldens(t *testing.T) {
	proxied := wc1()
	proxied.Proxy = Proxy{Enabled: true, HTTPProxy: "http://proxy.acme.example.io:3128", HTTPSProxy: "http://proxy.acme.example.io:3128", NoProxy: "10.0.0.0/8,.acme.example.io"}
	proxied.RegistryMirrors = map[string][]string{
		"docker.io":        {"registry.acme.example.io", "docker.io"},
		"gsoci.azurecr.io": {"gsoci.azurecr.io"},
	}
	proxied.RegistryCredentials = map[string]RegistryCredential{"registry.acme.example.io": {Username: "puller", Password: "s3cret"}}
	proxied.Teleport = false

	cases := []struct {
		name    string
		cluster Cluster
		pool    PoolSpec
	}{
		{"l4-default", wc1(), PoolSpec{Name: "gpu-l4", Accelerator: "nvidia-l4", MaxGPUs: 4}},
		{"a10g-sizes", wc1(), PoolSpec{Name: "gpu-a10g", Accelerator: "nvidia-a10g", MaxGPUs: 8, Sizes: []string{"xlarge", "2xlarge"}}},
		{"t4-pinned-chart", wc1(), PoolSpec{Name: "gpu-t4", Accelerator: "nvidia-t4", MaxGPUs: 2, ChartVersion: "0.2.0"}},
		{"l40s-proxy-credentials-no-teleport", proxied, PoolSpec{Name: "gpu-l40s", Accelerator: "nvidia-l40s", MaxGPUs: 4}},
		{"own-cluster-prewarm", own(), PoolSpec{Name: "gpu-l4", Accelerator: "nvidia-l4", MaxGPUs: 4, Prewarm: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs, err := Pool(tc.cluster, tc.pool)
			require.NoError(t, err)
			assertGolden(t, "pool-"+tc.name, objs)

			release := objs[len(objs)-1]
			assert.Equal(t, "HelmRelease", release.GetKind())
			assert.Equal(t, ReleaseName(tc.cluster.Name, tc.pool.Name), release.GetName())
			assert.Equal(t, tc.cluster.Name, release.GetOwnerReferences()[0].Name, "owned by the Cluster in apply mode")
			assert.Equal(t, tc.cluster.Name, release.GetLabels()[LabelCluster])
			_, hasValuesFrom, _ := unstructured.NestedSlice(release.Object, "spec", "valuesFrom")
			assert.Equal(t, len(tc.cluster.RegistryCredentials) > 0, hasValuesFrom, "a valuesFrom Secret exactly when the cluster has registry credentials")
			raw, err := yaml.Marshal(release.Object["spec"].(map[string]any)["values"])
			require.NoError(t, err)
			assert.NotContains(t, string(raw), "s3cret", "credentials never in spec.values")
			sa, _, _ := unstructured.NestedString(release.Object, "spec", "serviceAccountName")
			assert.Equal(t, DefaultTenantServiceAccount, sa, "the pool's Cluster API objects live in the org namespace: delivered as the tenant")
			prewarm, hasPrewarm, _ := unstructured.NestedBool(release.Object, "spec", "values", "pool", "prewarm", "enabled")
			assert.Equal(t, tc.pool.Prewarm, hasPrewarm && prewarm, "pool.prewarm.enabled exactly when asked for; no block otherwise")
			for _, action := range []string{"install", "upgrade"} {
				noJobWait, _, _ := unstructured.NestedBool(release.Object, "spec", action, "disableWaitForJobs")
				assert.True(t, noJobWait, "%s does not wait for the chart's Jobs: the prewarm placeholder holds a node for minutes (giantswarm/cluster-manager#55)", action)
			}
		})
	}
}

// TestPoolRefusesPrewarmOnAWorkloadCluster: the chart fails the release when
// pool.prewarm is set for a cluster other than the management cluster (the
// placeholder Job runs where the release lives); the composer refuses it
// first, naming the constraint, instead of landing a release that fails.
func TestPoolRefusesPrewarmOnAWorkloadCluster(t *testing.T) {
	_, err := Pool(wc1(), PoolSpec{Name: "gpu-l4", Accelerator: "nvidia-l4", MaxGPUs: 1, Prewarm: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wc1 is a workload cluster")
	assert.Contains(t, err.Error(), "re-run without prewarm")
	_, err = Pool(own(), PoolSpec{Name: "gpu-l4", Accelerator: "nvidia-l4", MaxGPUs: 1, Prewarm: true})
	assert.NoError(t, err, "the installation's own pool takes prewarm")
}

// TestPoolWithoutTenantServiceAccount: an installation without the tenancy
// policy names no ServiceAccount, and the release carries none.
func TestPoolWithoutTenantServiceAccount(t *testing.T) {
	c := wc1()
	c.TenantServiceAccount = ""
	objs, err := Pool(c, PoolSpec{Name: "gpu-l4", Accelerator: "nvidia-l4", MaxGPUs: 1})
	require.NoError(t, err)
	_, found, _ := unstructured.NestedString(objs[len(objs)-1].Object, "spec", "serviceAccountName")
	assert.False(t, found)
}

func TestPoolRefusesBadInput(t *testing.T) {
	for _, p := range []PoolSpec{
		{Name: "gpu", Accelerator: "nvidia-l4", MaxGPUs: 1},
		{Name: "gpu-l4", Accelerator: "nvidia-h100", MaxGPUs: 1},
		{Name: "gpu-l4", Accelerator: "nvidia-l4", MaxGPUs: 0},
	} {
		_, err := Pool(wc1(), p)
		assert.Error(t, err, "%+v", p)
	}
	_, err := Pool(wc1(), PoolSpec{Name: "gpu00", Accelerator: "nvidia-l4", MaxGPUs: 1})
	assert.NoError(t, err, "gpu00 passes the name pattern")
}

// assertGolden compares the objects' YAML with testdata/<name>.golden.yaml
// (-update rewrites the file) and holds every HelmRelease among them to the
// fleet's multi-tenancy policy.
func assertGolden(t *testing.T, name string, objs []*unstructured.Unstructured) {
	t.Helper()
	var sb strings.Builder
	for i, o := range objs {
		if i > 0 {
			sb.WriteString("---\n")
		}
		raw, err := yaml.Marshal(o.Object)
		require.NoError(t, err)
		sb.Write(raw)
		if o.GetKind() == "HelmRelease" {
			assertMultiTenancy(t, o)
		}
	}
	path := filepath.Join("testdata", name+".golden.yaml")
	if *update {
		require.NoError(t, os.MkdirAll("testdata", 0o750))
		require.NoError(t, os.WriteFile(path, []byte(sb.String()), 0o600))
	}
	want, err := os.ReadFile(path) //nolint:gosec // golden file named by the test
	require.NoError(t, err, "run with -update to create the golden file")
	assert.Equal(t, string(want), sb.String())
}

// assertMultiTenancy is the fleet's `flux-multi-tenancy` policy as the
// admission webhook applies it to a HelmRelease in `org-<org>` (the
// composer's dry run cannot see admission, so the goldens hold the constraint
// instead): either spec.serviceAccountName or spec.kubeConfig.secretRef.name
// is set, and targetNamespace and storageNamespace leave the release's
// namespace only through a kubeconfig.
func assertMultiTenancy(t *testing.T, release *unstructured.Unstructured) {
	t.Helper()
	spec, _, _ := unstructured.NestedMap(release.Object, "spec")
	sa, _, _ := unstructured.NestedString(spec, "serviceAccountName")
	kubeconfig, _, _ := unstructured.NestedString(spec, "kubeConfig", "secretRef", "name")
	assert.True(t, sa != "" || kubeconfig != "", "%s: either .spec.serviceAccountName or .spec.kubeConfig.secretRef.name is required", release.GetName())
	for _, field := range []string{"targetNamespace", "storageNamespace"} {
		ns, set, _ := unstructured.NestedString(spec, field)
		if set && ns != release.GetNamespace() {
			assert.NotEmpty(t, kubeconfig, "%s: spec.%s must be the same as metadata.namespace unless kubeConfig.secretRef.name is set", release.GetName(), field)
		}
	}
}
