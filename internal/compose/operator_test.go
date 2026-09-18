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
// byte: the two rows on a workload cluster, the Flatcar row on the
// installation's own cluster and with the DCGM exporter on — every one
// delivered through the cluster's kubeconfig Secret into its kube-system,
// running on a pool node the validator without its workload pods, the
// device plugin and GPU feature discovery, and nothing else unless asked.
func TestOperatorGoldens(t *testing.T) {
	own := wc1()
	own.Name, own.Namespace, own.Organization = "gazelle", "org-giantswarm", "giantswarm"
	cases := []struct {
		name    string
		cluster Cluster
		row     OperatorRow
		pools   []string
		// pinned are the machine-pool label values the worker's affinity
		// selects; none renders no affinity.
		pinned []any
		opts   OperatorOptions
	}{
		{"flatcar-workload", wc1(), RowFlatcar, []string{"gpu-l4"}, []any{"wc1-gpu-l4"}, OperatorOptions{}},
		{"preinstalled-workload", wc1(), RowPreinstalled, []string{"gpu-l4"}, []any{"wc1-gpu-l4"}, OperatorOptions{}},
		{"flatcar-own-cluster", own, RowFlatcar, []string{"gpu-l4"}, []any{"gazelle-gpu-l4"}, OperatorOptions{}},
		{"flatcar-two-pools", wc1(), RowFlatcar, []string{"gpu-t4", "gpu-l4", "gpu-t4"}, []any{"wc1-gpu-l4", "wc1-gpu-t4"}, OperatorOptions{}},
		{"flatcar-no-pool", wc1(), RowFlatcar, nil, nil, OperatorOptions{}},
		{"flatcar-dcgm-exporter", wc1(), RowFlatcar, []string{"gpu-l4"}, []any{"wc1-gpu-l4"}, OperatorOptions{DCGMExporter: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := Operator(tc.cluster, tc.row, tc.pools, tc.opts)
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
			for _, component := range []string{"cuda", "plugin"} {
				env, _, _ := unstructured.NestedSlice(release.Object, "spec", "values", OperatorValuesKey, "validator", component, "env")
				assert.Equal(t, []any{map[string]any{"name": ValidatorWorkloadEnv, "value": "false"}}, env, component+"-validation starts no workload pod on the node")
			}
			for _, kept := range []string{"driver", "toolkit"} {
				_, hasEnv, _ := unstructured.NestedSlice(release.Object, "spec", "values", OperatorValuesKey, "validator", kept, "env")
				assert.False(t, hasEnv, kept+" validation stays the chart's: it proves the image's driver and CDI")
			}
			mig, _, _ := unstructured.NestedBool(release.Object, "spec", "values", OperatorValuesKey, "migManager", valueEnabled)
			assert.False(t, mig, "no MIG on the pool's accelerators: mig-manager off")
			dcgm, _, _ := unstructured.NestedBool(release.Object, "spec", "values", OperatorValuesKey, "dcgmExporter", valueEnabled)
			assert.Equal(t, tc.opts.DCGMExporter, dcgm, "the DCGM exporter runs only where the installation's observability scrapes it")
			for _, kept := range []string{"gfd", "devicePlugin"} {
				_, has, _ := unstructured.NestedMap(release.Object, "spec", "values", OperatorValuesKey, kept)
				assert.False(t, has, kept+" keeps the chart's default: the device plugin advertises the GPU, model-manager reads GPU feature discovery's labels")
			}
			sleep, _, _ := unstructured.NestedString(release.Object, "spec", "values", OperatorValuesKey, NFDValuesKey, "worker", "config", "core", "sleepInterval")
			assert.Equal(t, NFDWorkerSleepInterval, sleep, "the worker polls every 10 s, pools or none: a fresh node is labelled within seconds, not upstream's minute")
			config, _, _ := unstructured.NestedMap(release.Object, "spec", "values", OperatorValuesKey, NFDValuesKey, "worker", "config")
			assert.Equal(t, map[string]any{"core": map[string]any{"sleepInterval": NFDWorkerSleepInterval}}, config, "nothing else of worker.config is set: the operator chart's PCI whitelist merges in")
			startup := []any{map[string]any{"key": TaintUninitialized, "operator": "Exists"}, map[string]any{"key": TaintEBSAgentNotReady, "operator": "Exists"}}
			workerTolerations, _, _ := unstructured.NestedSlice(release.Object, "spec", "values", OperatorValuesKey, NFDValuesKey, "worker", "tolerations")
			assert.Equal(t, append([]any{controlPlaneToleration(), gpuToleration()}, startup...), workerTolerations, "the worker keeps the chart's tolerations and tolerates the start-up taints DaemonSets remove, whatever their value and effect")
			operandTolerations, _, _ := unstructured.NestedSlice(release.Object, "spec", "values", OperatorValuesKey, "daemonsets", "tolerations")
			assert.Equal(t, append([]any{gpuToleration()}, startup...), operandTolerations, "the validator, device plugin and GPU feature discovery the same")
			daemonsets, _, _ := unstructured.NestedMap(release.Object, "spec", "values", OperatorValuesKey, "daemonsets")
			assert.Len(t, daemonsets, 1, "nothing else of daemonsets is set: the chart's priority class and update strategy merge in")
			for _, list := range [][]any{workerTolerations, operandTolerations} {
				for _, tol := range list {
					assert.NotEqual(t, "node.cilium.io/agent-not-ready", tol.(map[string]any)["key"], "the operands use the pod network: the CNI must be up before their sandbox, the Cilium taint is not tolerated")
					assert.NotEqual(t, "karpenter.sh/unregistered", tol.(map[string]any)["key"], "Karpenter removes its own taint at registration")
				}
			}
			affinity, hasAffinity, _ := unstructured.NestedMap(release.Object, "spec", "values", OperatorValuesKey, NFDValuesKey, "worker", "affinity")
			if tc.pinned == nil {
				assert.False(t, hasAffinity, "no pool pins the worker nowhere: the chart runs it everywhere, as upstream")
				return
			}
			terms, _, _ := unstructured.NestedSlice(affinity, "nodeAffinity", "requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms")
			require.Len(t, terms, 1)
			expressions, _, _ := unstructured.NestedSlice(terms[0].(map[string]any), "matchExpressions")
			require.Len(t, expressions, 1)
			assert.Equal(t, map[string]any{"key": LabelMachinePool, "operator": "In", "values": tc.pinned}, expressions[0], "NFD's worker runs on the GPU pools' nodes only, sorted and deduplicated")
		})
	}
}

// TestKServeBackendGoldens pins the backend document: a workload cluster
// with its apiserver and CA and the shapes of its pinned pool, and the
// installation's own cluster as the `local` target without a pool; both
// name the slice's release namespace as where the discovery ConfigMap is
// (giantswarm/cluster-manager#19, #26).
func TestKServeBackendGoldens(t *testing.T) {
	shapes, err := Shapes("nvidia-l4", []string{"xlarge", "2xlarge"})
	require.NoError(t, err)
	remote := BackendTarget{
		Cluster: "wc1", Organization: "acme", APIServer: "https://api.wc1.acme.example.io:6443",
		CABundle: "-----BEGIN CERTIFICATE-----\nMIIBfixture\n-----END CERTIFICATE-----\n", ServingNamespace: "model-serving", DiscoveryNamespace: "org-acme",
	}
	local := BackendTarget{Cluster: "gazelle", Organization: "giantswarm", OwnCluster: true, ServingNamespace: "model-serving", DiscoveryNamespace: "org-giantswarm"}
	for _, tc := range []struct {
		name      string
		target    BackendTarget
		instances []InstanceShape
	}{{"workload", remote, shapes}, {"local", local, nil}} {
		t.Run(tc.name, func(t *testing.T) {
			cm, err := KServeBackend("agent-platform", tc.target, tc.instances)
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
			assert.Contains(t, doc, "discovery:\n      namespace: "+tc.target.DiscoveryNamespace, "the discovery ConfigMap is in the slice's release namespace, not the serving namespace")
			if tc.instances == nil {
				assert.NotContains(t, doc, "gpuPool", "no pool pinned: no gpuPool block, model-manager's fit check keeps its answer")
				return
			}
			assert.Contains(t, doc, "gpuPool:\n      instances:\n      - gpuMemoryGiB: 24\n        gpus: 1\n        instanceType: g6.xlarge\n        memoryGiB: 16\n        size: xlarge\n        usableMemoryGiB: 11.9\n        usableVcpu: 3\n        vcpu: 4\n      - gpuMemoryGiB: 24\n        gpus: 1\n        instanceType: g6.2xlarge\n", "the pinned pool's shapes in the pool's order, keyed as model-manager's spec.kserve.gpuPool.instances[] declares them")
			for _, key := range []string{"instanceStore", "price"} {
				assert.NotContains(t, doc, key, "model-manager parses the document strictly: the answer's fields it does not declare are never written")
			}
			assert.NotContains(t, doc, "taint", "the taint and the node selector stay the discovery ConfigMap's")
		})
	}
}
