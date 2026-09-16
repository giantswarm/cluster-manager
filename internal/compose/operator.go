package compose

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The operator release's chart and its source.
const (
	// OperatorChart is the chart of the `<cluster>-gpu-operator` release: the
	// catalog's wrapper of NVIDIA's gpu-operator.
	OperatorChart = "gpu-operator"
	// OperatorChartURL is the catalog location of the gpu-operator chart.
	OperatorChartURL = "oci://gsoci.azurecr.io/charts/giantswarm/gpu-operator"
	// OperatorChartRange is the version range the release follows: the 1.x
	// line, the same range the platform's own chart component tracks.
	OperatorChartRange = "1.x"
	// OperatorReleaseSuffix names the release after the cluster.
	OperatorReleaseSuffix = "-gpu-operator"
	// OperatorNamespace is where the operator installs on the target.
	OperatorNamespace = "kube-system"
	// OperatorValuesKey is the values key the wrapper chart nests the
	// upstream operator under.
	OperatorValuesKey = "gpu-operator"

	// KubeconfigSecretSuffix names the CAPI kubeconfig Secret of a cluster;
	// a release targeting a workload cluster reads it through
	// spec.kubeConfig.secretRef.
	KubeconfigSecretSuffix = "-kubeconfig" //nolint:gosec // a Secret's name, not a credential

	// LabelDriverDeploy is NVIDIA's node label for a pre-installed driver:
	// any value but `true` (the convention is `pre-installed`) tells the
	// operator not to deploy the driver.
	LabelDriverDeploy = "nvidia.com/gpu.deploy.driver"
	// flatcarOSImage is the word the OS image of a Flatcar node carries.
	flatcarOSImage = "Flatcar"
)

// OperatorRow is one row of the operator's configuration table
// (bumblebee-plans#46 D3): which of driver and toolkit the operator deploys.
type OperatorRow struct {
	// Name is the row as the tools report it.
	Name string
	// Driver and Toolkit are the operator's `driver.enabled` and
	// `toolkit.enabled`.
	Driver  bool
	Toolkit bool
}

// The two rows: Flatcar carries driver and toolkit in the image; a node with
// a pre-installed driver still needs the toolkit.
var (
	RowFlatcar      = OperatorRow{Name: "flatcar", Driver: false, Toolkit: false}
	RowPreinstalled = OperatorRow{Name: "pre-installed", Driver: false, Toolkit: true}
)

// Node is what the table reads of one node of the target cluster.
type Node struct {
	Name    string
	OSImage string
	Labels  map[string]string
}

// ErrUnknownRow is the refusal when the target's nodes match no row of the
// table; the message names what was seen and the two rows.
type ErrUnknownRow struct {
	OSImages []string
	Labels   []string
}

func (e *ErrUnknownRow) Error() string {
	labels := "none"
	if len(e.Labels) > 0 {
		labels = strings.Join(e.Labels, ", ")
	}
	return fmt.Sprintf("no GPU operator runs on the cluster and its nodes match no row of the operator's configuration table (OS images seen: %s; nvidia.com labels seen: %s): row 1 Flatcar (the OS image contains Flatcar) — driver and toolkit off; row 2 pre-installed driver (nodes labelled %s with a value other than true) — driver off, toolkit on. A row for this provider is added with the EKS and Azure work (giantswarm/giantswarm#37611); until then install the operator by hand or run the platform's chart component components.gpu-operator, which detection then sees",
		strings.Join(e.OSImages, ", "), labels, LabelDriverDeploy)
}

// DeriveOperatorRow picks the table's row for the target's nodes. A node
// labelled with a pre-installed driver decides for row 2 (the operator is
// cluster-wide, so a driver already present anywhere must not be deployed);
// nodes that are all Flatcar decide for row 1; a cluster without nodes is
// judged by the machine image the pool pins (a Flatcar image: row 1).
func DeriveOperatorRow(nodes []Node, machineImage string) (OperatorRow, error) {
	var (
		osImages = map[string]bool{}
		labels   = map[string]bool{}
		flatcar  = true
	)
	for _, n := range nodes {
		osImages[n.OSImage] = true
		for k, v := range n.Labels {
			if !strings.HasPrefix(k, "nvidia.com/") {
				continue
			}
			labels[k+"="+v] = true
			if k == LabelDriverDeploy && v != "true" {
				return RowPreinstalled, nil
			}
		}
		if !strings.Contains(n.OSImage, flatcarOSImage) {
			flatcar = false
		}
	}
	if len(nodes) == 0 {
		if strings.HasPrefix(machineImage, "flatcar-") {
			return RowFlatcar, nil
		}
		return OperatorRow{}, &ErrUnknownRow{OSImages: []string{"no node readable; pool image " + machineImage}}
	}
	if flatcar {
		return RowFlatcar, nil
	}
	return OperatorRow{}, &ErrUnknownRow{OSImages: sortedKeys(osImages), Labels: sortedKeys(labels)}
}

// OperatorReleaseName names the operator release of a cluster.
func OperatorReleaseName(cluster string) string { return cluster + OperatorReleaseSuffix }

// KubeconfigSecretName names the CAPI kubeconfig Secret of a cluster.
func KubeconfigSecretName(cluster string) string { return cluster + KubeconfigSecretSuffix }

// Operator renders the `<cluster>-gpu-operator` release: an OCIRepository
// following the chart's 1.x line and a HelmRelease installing into
// kube-system of the target — through the cluster's kubeconfig Secret when
// the target is a workload cluster (ownCluster false), plain on the
// installation's own cluster — configured from the table's row. The objects
// carry the fleet's labels and, in apply mode, an ownerReference to the
// Cluster.
func Operator(c Cluster, row OperatorRow, ownCluster bool) []*unstructured.Unstructured {
	name := OperatorReleaseName(c.Name)
	meta := objectMeta(c, map[string]any{
		LabelChartName: OperatorChart,
		LabelManagedBy: ManagedBy,
		LabelCluster:   c.Name,
	})
	source := object(OCIRepositoryGVR, "OCIRepository", meta(name), map[string]any{"spec": ociRepositorySpec(OperatorChartURL, "semver", OperatorChartRange)})
	spec := helmReleaseSpec(OperatorChart, true, map[string]any{
		OperatorValuesKey: map[string]any{
			"driver":  map[string]any{"enabled": row.Driver},
			"toolkit": map[string]any{"enabled": row.Toolkit},
		},
	})
	spec["chartRef"] = map[string]any{"kind": "OCIRepository", "name": name}
	spec["targetNamespace"], spec["storageNamespace"] = OperatorNamespace, OperatorNamespace
	if !ownCluster {
		spec["kubeConfig"] = map[string]any{"secretRef": map[string]any{"name": KubeconfigSecretName(c.Name)}}
	}
	return []*unstructured.Unstructured{source, object(HelmReleaseGVR, "HelmRelease", meta(name), map[string]any{"spec": spec})}
}

// objectMeta builds the metadata of a release's objects in the cluster's
// namespace: the labels and, when the Cluster's UID is known (apply mode),
// the ownerReference that garbage-collects them with the cluster.
func objectMeta(c Cluster, labels map[string]any) func(name string) map[string]any {
	return func(name string) map[string]any {
		m := map[string]any{"name": name, "namespace": c.Namespace, "labels": labels}
		if c.UID != "" {
			m["ownerReferences"] = []any{map[string]any{
				"apiVersion": "cluster.x-k8s.io/v1beta1",
				"kind":       "Cluster",
				"name":       c.Name,
				"uid":        c.UID,
			}}
		}
		return m
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
