// Package compose renders the objects cluster-manager writes: the pool
// release of the `gpu-node-pool` chart (a HelmRelease plus its own
// OCIRepository in `org-<org>`), the `<cluster>-gpu-operator` release and the
// `<cluster>-agent-platform` slice release (bumblebee-plans#46 D2–D4). The
// renderers arrive with the write tools (giantswarm/giantswarm#37637, stage
// 2b); this stage fixes the names and labels the read tools already select
// by, so the two sides agree from the first release.
package compose

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// PoolChart is the chart of a GPU pool release.
	PoolChart = "gpu-node-pool"

	// LabelChartName marks a HelmRelease with the chart it installs; a
	// HelmRelease whose source is an OCIRepository (`spec.chartRef`) names
	// no chart in its spec, so the label is what the read tools select by.
	LabelChartName = "app.kubernetes.io/name"
	// LabelCluster marks an object with the Cluster API cluster it belongs
	// to (the fleet's convention for objects in `org-<org>`).
	LabelCluster = "giantswarm.io/cluster"
	// LabelManagedBy marks the objects cluster-manager created.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// ManagedBy is LabelManagedBy's value on cluster-manager's objects.
	ManagedBy = "cluster-manager"
)

// releaseRetries is how often helm-controller retries an install or upgrade
// of one of cluster-manager's releases before it gives up.
const releaseRetries = int64(3)

// Delivery is how the installation's helm-controller reaches a release's
// objects. Giant Swarm installations enforce Flux multi-tenancy on every
// HelmRelease outside the platform's namespaces (the `flux-multi-tenancy`
// Kyverno policy, admission-enforced): a release carries
// `spec.serviceAccountName` or `spec.kubeConfig.secretRef.name`, and leaves
// its namespace (`targetNamespace`, `storageNamespace`) only through a
// kubeconfig. The fleet's own releases in `org-<org>` take one of two shapes
// by what they render, and cluster-manager's mirror them:
//
//   - a release whose objects live on the target cluster (the operator, into
//     kube-system) is delivered through the cluster's Cluster API kubeconfig
//     Secret `<cluster>-kubeconfig` — the installation's own cluster included,
//     whose Secret points at the same API server (deliverThroughKubeconfig);
//   - a release whose objects live in the org namespace on the installation
//     (the pool's Cluster API objects, the slice's Flux objects) runs under the
//     org's tenant ServiceAccount, `automation` on Giant Swarm installations
//     (deliverAsTenant); the slice's child releases carry the kubeconfig
//     through the meta chart's target knob.
const (
	// KubeconfigSecretSuffix names the Cluster API kubeconfig Secret of a
	// cluster, the route of deliverThroughKubeconfig.
	KubeconfigSecretSuffix = "-kubeconfig" //nolint:gosec // a Secret's name, not a credential
	// DefaultTenantServiceAccount is the org namespace's tenant
	// ServiceAccount on Giant Swarm installations.
	DefaultTenantServiceAccount = "automation"
)

// KubeconfigSecretName names the Cluster API kubeconfig Secret of a cluster.
func KubeconfigSecretName(cluster string) string { return cluster + KubeconfigSecretSuffix }

// deliverThroughKubeconfig targets spec at the cluster through its kubeconfig
// Secret; helm-controller reads the Secret's default key, the one the
// Cluster API writes.
func deliverThroughKubeconfig(spec map[string]any, cluster string) {
	spec["kubeConfig"] = map[string]any{"secretRef": map[string]any{"name": KubeconfigSecretName(cluster)}}
}

// deliverAsTenant runs the release under the org namespace's tenant
// ServiceAccount; empty (an installation without the policy) renders none.
func deliverAsTenant(spec map[string]any, serviceAccount string) {
	if serviceAccount != "" {
		spec["serviceAccountName"] = serviceAccount
	}
}

// helmReleaseSpec is the common shape of cluster-manager's HelmReleases: the
// reconciliation interval, the chartRef to the OCIRepository of the same
// name, install and upgrade with retries — CRDs created and replaced when the
// chart carries any — and the values. Callers add what is theirs (valuesFrom,
// a target namespace) and the delivery (deliverThroughKubeconfig or
// deliverAsTenant).
func helmReleaseSpec(name string, crds bool, values map[string]any) map[string]any {
	remediation := func() map[string]any {
		m := map[string]any{"remediation": map[string]any{"retries": releaseRetries}}
		if crds {
			m["crds"] = "CreateReplace"
		}
		return m
	}
	return map[string]any{
		"interval":    ReleaseInterval,
		"releaseName": name,
		"chartRef":    map[string]any{"kind": "OCIRepository", "name": name},
		"install":     remediation(),
		"upgrade":     remediation(),
		"values":      values,
	}
}

// ociRepositorySpec is the source of one of cluster-manager's releases: the
// chart's catalog location and the reference that pins or follows it
// (`tag` for an exact pin, `semver` for a range).
func ociRepositorySpec(url, refKey, ref string) map[string]any {
	return map[string]any{
		"interval": ReleaseInterval,
		"url":      url,
		"ref":      map[string]any{refKey: ref},
	}
}

// object builds one composed object of the given resource and kind from its
// metadata and its remaining top-level fields (spec, data, ...).
func object(gvr schema.GroupVersionResource, kind string, metadata map[string]any, fields map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": gvr.GroupVersion().String(),
		"kind":       kind,
		"metadata":   metadata,
	}
	for k, v := range fields {
		obj[k] = v
	}
	return &unstructured.Unstructured{Object: obj}
}
