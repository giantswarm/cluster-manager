package compose

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"

	"github.com/Masterminds/semver/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// The pool release's chart and its source.
const (
	// PoolChartURL is the catalog location of the gpu-node-pool chart.
	PoolChartURL = "oci://gsoci.azurecr.io/charts/giantswarm/gpu-node-pool"
	// DefaultPoolChartVersion is the pin used when the caller names none:
	// the newest released chart. The pool release pins the chart exactly — a
	// bootstrap change rolls GPU nodes under a served model, so bumps are
	// explicit (bumblebee-plans#46 D3). 0.7.0 keeps a pool node's /var/lib
	// on its NVMe instance store (the chart's default `pool.volumes.libSource:
	// instance-store`; cluster-manager writes no volumes block): every size
	// of the curated families has one (the shape table's stores), so no
	// size is refused.
	DefaultPoolChartVersion = "0.7.0"
	// SysextPoolChartVersion is the first chart whose default bootstrap
	// takes the NVIDIA driver from Flatcar's prebuilt, release-matched
	// nvidia-drivers system extension instead of building it at first boot
	// (`pool.nvidiaDriver.source: flatcar-sysext`, giantswarm/gpu-node-pool#19).
	// cluster-manager writes no nvidiaDriver block: the chart's default is
	// the extension.
	SysextPoolChartVersion = "0.6.0"
	// MinSysextFlatcarVersion is the first Flatcar release shipping the
	// extension; an older image would fail its boot looking for it, and the
	// chart refuses it at render.
	MinSysextFlatcarVersion = "4344.0.0"
	// ReleaseInterval is the reconciliation interval of the source and the
	// release.
	ReleaseInterval = "10m"
	// ValuesSecretKey is the key of the valuesFrom Secret carrying the
	// registry credentials.
	ValuesSecretKey = "values.yaml"
	// LabelPool marks the objects of one pool release with the pool's name.
	LabelPool = "giantswarm.io/node-pool"
)

// Resources the pool release consists of.
var (
	OCIRepositoryGVR = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "ocirepositories"}
	HelmReleaseGVR   = schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
	SecretGVR        = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
)

// Accelerators is the curated list of the chart (values enum); the chart
// picks the EC2 instance family from it (InstanceFamily).
var Accelerators = []string{"nvidia-l4", "nvidia-a10g", "nvidia-t4", "nvidia-l40s"}

// DefaultPoolSizes are the chart's `pool.sizes` at DefaultPoolChartVersion,
// what Karpenter may pick when the caller names no sizes.
var DefaultPoolSizes = []string{"xlarge", "2xlarge", "4xlarge"}

// PoolNamePattern is the pool name's shape: five to twenty characters, since
// `<cluster>-<pool>` becomes the NodePool, EC2NodeClass and S3 key names.
var PoolNamePattern = regexp.MustCompile(`^[a-z0-9][-a-z0-9]{3,18}[a-z0-9]$`)

// machineImageFlatcar finds the Flatcar version in a machine image name of
// the fleet's shape, `flatcar-<channel>-<flatcar>-kube-<k8s>-tooling-<tooling>-gs`.
var machineImageFlatcar = regexp.MustCompile(`^flatcar-[a-z]+-(\d+\.\d+\.\d+)-`)

// Cluster is what the pool release needs to know about the cluster it joins:
// its identity, its current release's pins, and the credential-free snapshot
// of its bootstrap settings (bumblebee-plans#46 D3).
type Cluster struct {
	Name         string
	Namespace    string
	Organization string
	// UID is the Cluster's UID; the release's objects own-reference it so
	// they are garbage-collected with the cluster (apply mode).
	UID string
	// TenantServiceAccount is the ServiceAccount in the cluster's org
	// namespace the installation's helm-controller runs a release under when
	// the release's objects live in that namespace (deliverAsTenant):
	// DefaultTenantServiceAccount on Giant Swarm installations; empty
	// renders none.
	TenantServiceAccount string

	// KubernetesVersion (`1.33.1`) and MachineImage are the pool's pins,
	// from the cluster's Release CR.
	KubernetesVersion string
	MachineImage      string

	BaseDomain        string
	ManagementCluster string
	// RegistryMirrors is registry host → ordered endpoint hosts, without
	// credentials.
	RegistryMirrors map[string][]string
	Proxy           Proxy
	CiliumIPAMMode  string
	// Teleport is whether the nodes join Teleport: the cluster has a
	// `<cluster>-teleport-join-token` Secret.
	Teleport bool
	// RegistryCredentials are the credentials of the cluster's registries,
	// endpoint → credential. They never go into spec.values; when present
	// they become a valuesFrom Secret beside the release.
	RegistryCredentials map[string]RegistryCredential
}

// Proxy is the cluster's HTTP proxy configuration.
type Proxy struct {
	Enabled    bool
	HTTPProxy  string
	HTTPSProxy string
	NoProxy    string
}

// RegistryCredential is one registry's credential as the cluster's values
// carry it.
type RegistryCredential struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Auth     string `json:"auth,omitempty"`
}

// PoolSpec is the caller's part of a pool.
type PoolSpec struct {
	Name        string
	Accelerator string
	// Sizes are the instance sizes Karpenter may pick, each a size of the
	// accelerator's family (Shapes); nil keeps the chart's default
	// (DefaultPoolSizes).
	Sizes []string
	// MaxGPUs bounds the pool (Karpenter's `nvidia.com/gpu` limit).
	MaxGPUs int
	// ChartVersion is the exact chart pin; empty means DefaultPoolChartVersion.
	ChartVersion string
	// Prewarm launches the pool's first node with the release instead of
	// with the first predictor: the chart's `pool.prewarm.enabled`, a
	// one-shot placeholder Job holding one GPU at negative priority until the
	// first workload preempts it. The Job runs in the release namespace on
	// the installation, so the chart renders it only for the installation's
	// own pool (cluster.name == cluster.managementCluster) and fails the
	// release otherwise; Pool refuses it for a workload cluster instead.
	Prewarm bool
	// Zones pins the pool's nodes to availability zones: the chart's
	// `pool.zones`, a `topology.kubernetes.io/zone In [...]` requirement on
	// the Karpenter NodePool, so every node — prewarm placeholder and
	// workload alike — comes up there. create_node_pool sets it to the zones
	// the caller names, judged against the serving namespace's kept model
	// cache claim — one EBS volume, bound in one zone by the first predictor,
	// which a node in another zone strands the predictor mounting it
	// (giantswarm/cluster-manager#59, #65) — or, with none named, to the
	// claim's zone. Nil writes no zones block: the chart's default constrains
	// nothing.
	Zones []string
}

// Validate checks the caller's part against the chart's contract.
func (p PoolSpec) Validate() error {
	if !PoolNamePattern.MatchString(p.Name) {
		return fmt.Errorf("pool name %q: must match %s (five to twenty lowercase characters, digits and dashes; `gpu` is too short, `gpu00` and `gpu-l4` pass)", p.Name, PoolNamePattern)
	}
	if !contains(Accelerators, p.Accelerator) {
		return fmt.Errorf("accelerator %q: not in the curated list %v", p.Accelerator, Accelerators)
	}
	if _, err := Shapes(p.Accelerator, p.Sizes); err != nil {
		return err
	}
	if p.MaxGPUs < 1 {
		return fmt.Errorf("maxGpus %d: the pool's upper bound must be at least 1", p.MaxGPUs)
	}
	return nil
}

// validatePrewarm holds the chart's constraint on pool.prewarm: the
// placeholder Job is created in the release namespace on the installation,
// and the pool's nodes join c.Name — the same cluster only for the
// installation's own pool. Refused, naming the constraint, rather than left
// to the chart's render to fail the release.
func (p PoolSpec) validatePrewarm(c Cluster) error {
	if !p.Prewarm || c.Name == c.ManagementCluster {
		return nil
	}
	return fmt.Errorf("prewarm: the placeholder Job runs in the release namespace on the installation (%s), where only the installation's own pool's nodes join; %s is a workload cluster, whose pool's first node launches with the first predictor — re-run without prewarm", c.ManagementCluster, c.Name)
}

// validateFlatcar holds the chart's constraint on the machine image: from
// SysextPoolChartVersion a pool node takes its NVIDIA driver from Flatcar's
// prebuilt nvidia-drivers system extension, which the OS downloads for its
// own version at first boot — an image older than MinSysextFlatcarVersion
// has none to download and fails the boot, and the chart refuses it at
// render. Refused first, naming the version and the way out (a newer
// cluster release; the chart's image-build source is not offered), rather
// than left to the render to fail a release already written. An image whose
// name carries no Flatcar version is refused the same way. A pin to an
// older chart builds the driver at boot and is not judged.
func validateFlatcar(c Cluster, chartVersion string) error {
	chart, err := semver.NewVersion(chartVersion)
	if err != nil {
		return fmt.Errorf("chart version %q: not a version: %w", chartVersion, err)
	}
	if chart.LessThan(semver.MustParse(SysextPoolChartVersion)) {
		return nil
	}
	m := machineImageFlatcar.FindStringSubmatch(c.MachineImage)
	if m == nil {
		return fmt.Errorf("machine image %q of cluster %s names no Flatcar version (`flatcar-<channel>-<version>-…`): a pool node of gpu-node-pool %s takes its NVIDIA driver from the Flatcar release's prebuilt nvidia-drivers system extension, picked by that version", c.MachineImage, c.Name, chartVersion)
	}
	flatcar := semver.MustParse(m[1])
	if !flatcar.LessThan(semver.MustParse(MinSysextFlatcarVersion)) {
		return nil
	}
	return fmt.Errorf("the release of cluster %s pins Flatcar %s, and a pool node of gpu-node-pool %s takes its NVIDIA driver from Flatcar's prebuilt nvidia-drivers system extension, first shipped with Flatcar %s — an older image has no extension to download and fails its boot: the pool needs a cluster release with Flatcar %s or newer; upgrade the cluster, then re-run", c.Name, m[1], chartVersion, MinSysextFlatcarVersion, MinSysextFlatcarVersion)
}

// ReleaseName is the name of the pool's HelmRelease and OCIRepository, the
// same `<cluster>-<pool>` the chart gives the MachinePool.
func ReleaseName(cluster, pool string) string { return cluster + "-" + pool }

// ValuesSecretName names the valuesFrom Secret of a pool release.
func ValuesSecretName(cluster, pool string) string { return ReleaseName(cluster, pool) + "-values" }

// Pool renders the pool release: the OCIRepository, the HelmRelease and — only
// when the cluster carries registry credentials — the valuesFrom Secret. The
// release runs under the org's tenant ServiceAccount (its Cluster API objects
// live in the org namespace on the installation; see Delivery in
// compose.go), does not wait for the chart's Jobs (the prewarm placeholder
// holds a node for minutes; withoutJobWait) and does not wait for its
// uninstall to see the MachinePool gone (the instance's termination takes
// minutes; withoutUninstallWait). The objects carry the fleet's labels and,
// in apply mode, an ownerReference to the Cluster.
func Pool(c Cluster, p PoolSpec) ([]*unstructured.Unstructured, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if err := p.validatePrewarm(c); err != nil {
		return nil, err
	}
	version := p.ChartVersion
	if version == "" {
		version = DefaultPoolChartVersion
	}
	if err := validateFlatcar(c, version); err != nil {
		return nil, err
	}
	name := ReleaseName(c.Name, p.Name)
	meta := objectMeta(c, map[string]any{
		LabelChartName: PoolChart,
		LabelManagedBy: ManagedBy,
		LabelCluster:   c.Name,
		LabelPool:      p.Name,
	})

	source := object(OCIRepositoryGVR, "OCIRepository", meta(name), map[string]any{"spec": ociRepositorySpec(PoolChartURL, "tag", version)})
	spec := helmReleaseSpec(name, false, values(c, p))
	withoutJobWait(spec)
	withoutUninstallWait(spec)
	deliverAsTenant(spec, c.TenantServiceAccount)
	objs := []*unstructured.Unstructured{source}
	if len(c.RegistryCredentials) > 0 {
		secret, err := credentialsSecret(c, p, meta(ValuesSecretName(c.Name, p.Name)))
		if err != nil {
			return nil, err
		}
		spec["valuesFrom"] = []any{map[string]any{"kind": "Secret", "name": secret.GetName(), "valuesKey": ValuesSecretKey}}
		objs = append(objs, secret)
	}
	return append(objs, object(HelmReleaseGVR, "HelmRelease", meta(name), map[string]any{"spec": spec})), nil
}

// values is the chart's values for the pool: the snapshot of the cluster's
// settings, the pins and the caller's shape. Chart defaults that the
// snapshot does not override (minSize 0, volumes and their throughput,
// consolidation, maxPods, the placeholder's hold and image) are left to the
// chart; the prewarm block and the zones are written only when the caller
// asks for them, so a re-run without them removes them.
func values(c Cluster, p PoolSpec) map[string]any {
	mirrors := map[string]any{}
	for host, endpoints := range c.RegistryMirrors {
		list := make([]any, 0, len(endpoints))
		for _, e := range endpoints {
			list = append(list, e)
		}
		mirrors[host] = list
	}
	cluster := map[string]any{
		"name":              c.Name,
		"organization":      c.Organization,
		"baseDomain":        c.BaseDomain,
		"managementCluster": c.ManagementCluster,
		"registryMirrors":   mirrors,
	}
	if c.Proxy.Enabled {
		cluster["proxy"] = map[string]any{
			valueEnabled: true,
			"httpProxy":  c.Proxy.HTTPProxy,
			"httpsProxy": c.Proxy.HTTPSProxy,
			"noProxy":    c.Proxy.NoProxy,
		}
	}
	if c.CiliumIPAMMode != "" {
		cluster["cilium"] = map[string]any{"ipamMode": c.CiliumIPAMMode}
	}
	pool := map[string]any{
		"name":              p.Name,
		"kubernetesVersion": c.KubernetesVersion,
		"machineImage":      c.MachineImage,
		"accelerator":       p.Accelerator,
		"maxSize":           map[string]any{"nvidia.com/gpu": strconv.Itoa(p.MaxGPUs)},
	}
	if len(p.Sizes) > 0 {
		sizes := make([]any, 0, len(p.Sizes))
		for _, s := range p.Sizes {
			sizes = append(sizes, s)
		}
		pool["sizes"] = sizes
	}
	if p.Prewarm {
		pool["prewarm"] = map[string]any{valueEnabled: true}
	}
	if len(p.Zones) > 0 {
		zones := make([]any, 0, len(p.Zones))
		for _, z := range p.Zones {
			zones = append(zones, z)
		}
		pool["zones"] = zones
	}
	return map[string]any{
		"cluster":  cluster,
		"pool":     pool,
		"teleport": map[string]any{valueEnabled: c.Teleport},
	}
}

// credentialsSecret carries the registry credentials as chart values
// (`cluster.registryCredentials.<endpoint>`), read by the release through
// valuesFrom so they never appear in spec.values.
func credentialsSecret(c Cluster, p PoolSpec, meta map[string]any) (*unstructured.Unstructured, error) {
	endpoints := make([]string, 0, len(c.RegistryCredentials))
	for e := range c.RegistryCredentials {
		endpoints = append(endpoints, e)
	}
	sort.Strings(endpoints)
	creds := map[string]RegistryCredential{}
	for _, e := range endpoints {
		creds[e] = c.RegistryCredentials[e]
	}
	raw, err := yaml.Marshal(map[string]any{"cluster": map[string]any{"registryCredentials": creds}})
	if err != nil {
		return nil, fmt.Errorf("encode registry credentials: %w", err)
	}
	return object(SecretGVR, "Secret", meta, map[string]any{
		"type":       "Opaque",
		"stringData": map[string]any{ValuesSecretKey: string(raw)},
	}), nil
}

// OwnedBy reports whether obj is one of cluster-manager's objects.
func OwnedBy(obj metav1.Object) bool {
	return obj.GetLabels()[LabelManagedBy] == ManagedBy
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
