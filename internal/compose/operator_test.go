package compose

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const flatcar = "Flatcar Container Linux by Kinvolk 4081.2.1 (Oklo)"

// TestDeriveOperatorRow walks the two-row table: Flatcar nodes, a
// pre-installed driver anywhere, a cluster without nodes judged by the
// pool's image, and the refusal naming what was seen.
func TestDeriveOperatorRow(t *testing.T) {
	flatcarNodes := []Node{{Name: "cp-1", OSImage: flatcar}, {Name: "w-1", OSImage: flatcar, Labels: map[string]string{"nvidia.com/gpu.present": "true"}}}
	row, err := DeriveOperatorRow(flatcarNodes, "flatcar-stable-4081.2.1-kube-1.31.4-tooling-1.26.1-gs")
	require.NoError(t, err)
	assert.Equal(t, RowFlatcar, row)

	mixed := append(flatcarNodes, Node{Name: "dgx-1", OSImage: "Ubuntu 24.04.2 LTS", Labels: map[string]string{LabelDriverDeploy: "pre-installed"}})
	row, err = DeriveOperatorRow(mixed, "flatcar-stable-4081.2.1-kube-1.31.4-tooling-1.26.1-gs")
	require.NoError(t, err)
	assert.Equal(t, RowPreinstalled, row, "a pre-installed driver anywhere decides: the operator is cluster-wide")

	row, err = DeriveOperatorRow([]Node{{Name: "dgx-1", OSImage: "Ubuntu 24.04.2 LTS", Labels: map[string]string{LabelDriverDeploy: "false"}}}, "")
	require.NoError(t, err)
	assert.Equal(t, RowPreinstalled, row, "any value but true is NVIDIA's pre-installed convention")

	row, err = DeriveOperatorRow(nil, "flatcar-stable-4081.2.1-kube-1.31.4-tooling-1.26.1-gs")
	require.NoError(t, err)
	assert.Equal(t, RowFlatcar, row, "no node yet: the pool pins a Flatcar image")

	_, err = DeriveOperatorRow([]Node{
		{Name: "cp-1", OSImage: "Ubuntu 24.04.2 LTS"},
		{Name: "w-1", OSImage: "Amazon Linux 2023", Labels: map[string]string{LabelDriverDeploy: "true", "nvidia.com/gpu.count": "1", "topology.kubernetes.io/zone": "a"}},
	}, "")
	var unknown *ErrUnknownRow
	require.ErrorAs(t, err, &unknown)
	assert.Equal(t, []string{"Amazon Linux 2023", "Ubuntu 24.04.2 LTS"}, unknown.OSImages)
	assert.Equal(t, []string{"nvidia.com/gpu.count=1", "nvidia.com/gpu.deploy.driver=true"}, unknown.Labels, "only nvidia.com labels, sorted")
	msg := err.Error()
	for _, want := range []string{"OS images seen: Amazon Linux 2023, Ubuntu 24.04.2 LTS", "row 1 Flatcar", "row 2 pre-installed driver", "giantswarm/giantswarm#37611"} {
		assert.Contains(t, msg, want)
	}

	_, err = DeriveOperatorRow(nil, "ami-ubuntu-eks")
	assert.ErrorAs(t, err, &unknown, "no node and no Flatcar image: refused")
}

// TestOperatorGoldens pins the `<cluster>-gpu-operator` release byte for
// byte: the two rows on a workload cluster and the Flatcar row on the
// installation's own cluster — every one delivered through the cluster's
// kubeconfig Secret into its kube-system.
func TestOperatorGoldens(t *testing.T) {
	own := wc1()
	own.Name, own.Namespace, own.Organization = "gazelle", "org-giantswarm", "giantswarm"
	cases := []struct {
		name    string
		cluster Cluster
		row     OperatorRow
	}{
		{"flatcar-workload", wc1(), RowFlatcar},
		{"preinstalled-workload", wc1(), RowPreinstalled},
		{"flatcar-own-cluster", own, RowFlatcar},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := Operator(tc.cluster, tc.row)
			assertGolden(t, "operator-"+tc.name, objs)
			require.Len(t, objs, 2)
			source, release := objs[0], objs[1]
			assert.Equal(t, "OCIRepository", source.GetKind())
			assert.Equal(t, OperatorReleaseName(tc.cluster.Name), release.GetName())
			assert.Equal(t, tc.cluster.Name, release.GetLabels()[LabelCluster])
			assert.Equal(t, tc.cluster.Name, release.GetOwnerReferences()[0].Name, "owned by the Cluster in apply mode")
			semver, _, _ := unstructured.NestedString(source.Object, "spec", "ref", "semver")
			assert.Equal(t, OperatorChartRange, semver, "the operator follows the 1.x line")
			kubeconfig, _, _ := unstructured.NestedString(release.Object, "spec", "kubeConfig", "secretRef", "name")
			assert.Equal(t, KubeconfigSecretName(tc.cluster.Name), kubeconfig, "into kube-system through the cluster's kubeconfig, own cluster included")
			_, hasSA, _ := unstructured.NestedString(release.Object, "spec", "serviceAccountName")
			assert.False(t, hasSA, "the kubeconfig is the identity")
			driver, _, _ := unstructured.NestedBool(release.Object, "spec", "values", OperatorValuesKey, "driver", "enabled")
			toolkit, _, _ := unstructured.NestedBool(release.Object, "spec", "values", OperatorValuesKey, "toolkit", "enabled")
			assert.Equal(t, tc.row, OperatorRow{Name: tc.row.Name, Driver: driver, Toolkit: toolkit})
		})
	}
}

// TestKServeBackendGoldens pins the backend document: a workload cluster
// with its apiserver and CA, and the installation's own cluster as the
// `local` target.
func TestKServeBackendGoldens(t *testing.T) {
	remote := BackendTarget{
		Cluster: "wc1", Organization: "acme", APIServer: "https://api.wc1.acme.example.io:6443",
		CABundle: "-----BEGIN CERTIFICATE-----\nMIIBfixture\n-----END CERTIFICATE-----\n", ServingNamespace: "model-serving",
	}
	local := BackendTarget{Cluster: "gazelle", Organization: "giantswarm", OwnCluster: true, ServingNamespace: "model-serving"}
	for _, tc := range []struct {
		name   string
		target BackendTarget
	}{{"workload", remote}, {"local", local}} {
		t.Run(tc.name, func(t *testing.T) {
			cm, err := KServeBackend("agent-platform", tc.target)
			require.NoError(t, err)
			assertGolden(t, "backend-kserve-"+tc.name, []*unstructured.Unstructured{cm})
			assert.Equal(t, BackendConfigMapName, cm.GetName())
			assert.Equal(t, "true", cm.GetLabels()[LabelBackend])
			assert.Equal(t, ManagedBy, cm.GetLabels()[LabelBackendSource])
			assert.Equal(t, tc.target.Cluster, cm.GetLabels()[LabelCluster])
			doc, _, _ := unstructured.NestedString(cm.Object, "data", BackendDocumentKey)
			if tc.target.OwnCluster {
				assert.Contains(t, doc, "cluster: local")
				assert.NotContains(t, doc, "apiServer")
			} else {
				assert.Contains(t, doc, "apiServer: "+tc.target.APIServer)
				assert.Contains(t, doc, "caBundle: |")
			}
			assert.NotContains(t, doc, "credentials", "the target never carries credentials")
		})
	}
}
