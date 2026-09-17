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
	// NFDValuesKey is the values key of the operator chart's Node Feature
	// Discovery subchart, under OperatorValuesKey.
	NFDValuesKey = "node-feature-discovery"

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

// The two rows: the Giant Swarm Flatcar image carries driver and toolkit,
// and the gpu-node-pool chart's bootstrap makes the toolkit serve them (the
// runtime in CDI mode, the driver's CDI specification written after
// nvidia.service; the chart README's image contract); a node with a
// pre-installed driver still needs the toolkit.
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

// Operator renders the `<cluster>-gpu-operator` release: an OCIRepository
// following the chart's 1.x line and a HelmRelease installing into
// kube-system of the target through the cluster's kubeconfig Secret — the
// installation's own cluster included, whose Secret points at the same API
// server (see Delivery in compose.go) — configured from the table's row,
// with Node Feature Discovery's worker pinned to the cluster's GPU pools
// (pools: the pool names within the cluster; see PoolAffinity). The objects
// carry the fleet's labels and, in apply mode, an ownerReference to the
// Cluster.
func Operator(c Cluster, row OperatorRow, pools []string) []*unstructured.Unstructured {
	name := OperatorReleaseName(c.Name)
	meta := objectMeta(c, map[string]any{
		LabelChartName: OperatorChart,
		LabelManagedBy: ManagedBy,
		LabelCluster:   c.Name,
	})
	source := object(OCIRepositoryGVR, "OCIRepository", meta(name), map[string]any{"spec": ociRepositorySpec(OperatorChartURL, "semver", OperatorChartRange)})
	values := map[string]any{
		"driver":  map[string]any{valueEnabled: row.Driver},
		"toolkit": map[string]any{valueEnabled: row.Toolkit},
	}
	if affinity := PoolAffinity(c, pools); affinity != nil {
		values[NFDValuesKey] = map[string]any{"worker": map[string]any{"affinity": affinity}}
	}
	spec := helmReleaseSpec(OperatorChart, true, map[string]any{OperatorValuesKey: values})
	spec["chartRef"] = map[string]any{"kind": "OCIRepository", "name": name}
	spec["targetNamespace"], spec["storageNamespace"] = OperatorNamespace, OperatorNamespace
	deliverThroughKubeconfig(spec, c.Name)
	return []*unstructured.Unstructured{source, object(HelmReleaseGVR, "HelmRelease", meta(name), map[string]any{"spec": spec})}
}

// PoolAffinity is the node affinity that keeps a DaemonSet on the nodes of
// the cluster's GPU pools: a required `giantswarm.io/machine-pool In` term
// over the pool releases' names (the value every pool node carries), sorted,
// duplicates dropped; nil without pools. The operator's Node Feature
// Discovery worker takes it (gpu-operator-app#164): the worker is what
// asserts `nvidia.com/gpu.present`, so a GPU-family node of the general pool
// without a driver (an AWS g6f spot instance) gets neither the label nor the
// operands stuck in Init that came with it. The set form covers one pool as
// well as several; a re-run recomposes the list.
func PoolAffinity(c Cluster, pools []string) map[string]any {
	if len(pools) == 0 {
		return nil
	}
	releases := map[string]bool{}
	for _, p := range pools {
		releases[ReleaseName(c.Name, p)] = true
	}
	values := make([]any, 0, len(releases))
	for _, r := range sortedKeys(releases) {
		values = append(values, r)
	}
	return map[string]any{
		"nodeAffinity": map[string]any{
			"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
				"nodeSelectorTerms": []any{
					map[string]any{"matchExpressions": []any{
						map[string]any{"key": LabelMachinePool, "operator": "In", "values": values},
					}},
				},
			},
		},
	}
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
