package compose

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"

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
	// explicit (bumblebee-plans#46 D3).
	DefaultPoolChartVersion = "0.3.1"
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

// ReleaseName is the name of the pool's HelmRelease and OCIRepository, the
// same `<cluster>-<pool>` the chart gives the MachinePool.
func ReleaseName(cluster, pool string) string { return cluster + "-" + pool }

// ValuesSecretName names the valuesFrom Secret of a pool release.
func ValuesSecretName(cluster, pool string) string { return ReleaseName(cluster, pool) + "-values" }

// Pool renders the pool release: the OCIRepository, the HelmRelease and — only
// when the cluster carries registry credentials — the valuesFrom Secret. The
// release runs under the org's tenant ServiceAccount (its Cluster API objects
// live in the org namespace on the installation; see Delivery in
// compose.go). The objects carry the fleet's labels and, in apply mode, an
// ownerReference to the Cluster.
func Pool(c Cluster, p PoolSpec) ([]*unstructured.Unstructured, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	name := ReleaseName(c.Name, p.Name)
	version := p.ChartVersion
	if version == "" {
		version = DefaultPoolChartVersion
	}
	meta := objectMeta(c, map[string]any{
		LabelChartName: PoolChart,
		LabelManagedBy: ManagedBy,
		LabelCluster:   c.Name,
		LabelPool:      p.Name,
	})

	source := object(OCIRepositoryGVR, "OCIRepository", meta(name), map[string]any{"spec": ociRepositorySpec(PoolChartURL, "tag", version)})
	spec := helmReleaseSpec(name, false, values(c, p))
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
// snapshot does not override (minSize 0, volumes, consolidation, maxPods)
// are left to the chart.
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
			"enabled":    true,
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
	return map[string]any{
		"cluster":  cluster,
		"pool":     pool,
		"teleport": map[string]any{"enabled": c.Teleport},
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
