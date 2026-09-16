package tools

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// Flux labels helm-controller puts on every object a HelmRelease applies.
const (
	labelFluxName      = "helm.toolkit.fluxcd.io/name"
	labelFluxNamespace = "helm.toolkit.fluxcd.io/namespace"

	// labelInstanceType is the well-known node label a Karpenter pool's
	// requirements constrain.
	labelInstanceType = "node.kubernetes.io/instance-type"
	// releaseComponentKubernetes is the Release CR component naming the
	// control plane's Kubernetes version.
	releaseComponentKubernetes = "kubernetes"
)

// NodePools is list_node_pools' answer: the cluster and its MachinePools.
type NodePools struct {
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	// ControlPlaneVersion is the control plane's Kubernetes version: the
	// control plane object's spec.version, else the kubernetes component of
	// the cluster's Release CR; empty when neither is readable.
	ControlPlaneVersion string     `json:"controlPlaneVersion"`
	NodePools           []NodePool `json:"nodePools"`
}

// NodePool is one MachinePool of a cluster. The pool's version and the
// control plane's are two fields; no "stale" or "edge" verdict is drawn
// (bumblebee-plans#46 round 4, Q6c).
type NodePool struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Version is the pool's Kubernetes version (spec.template.spec.version).
	Version             string `json:"version"`
	ControlPlaneVersion string `json:"controlPlaneVersion"`
	// Replicas is spec.replicas (on a Karpenter pool written by its
	// controller from the NodeClaims); ReadyReplicas is status.readyReplicas.
	Replicas      int64 `json:"replicas"`
	ReadyReplicas int64 `json:"readyReplicas"`
	// InstanceTypes are the instance types the pool's infrastructure names:
	// a launch template's instanceType, or a Karpenter pool's
	// node.kubernetes.io/instance-type requirement. Empty when unreadable.
	InstanceTypes []string `json:"instanceTypes"`
	// Accelerator is the accelerator of the owning pool release's values,
	// empty when the pool has no release or the release names none.
	Accelerator string `json:"accelerator,omitempty"`
	// OwnerRelease is the HelmRelease that applied the pool (Flux's labels),
	// null for a pool created by other means.
	OwnerRelease *ReleaseRef `json:"ownerRelease"`
}

// ReleaseRef names a HelmRelease.
type ReleaseRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// ListNodePools lists the MachinePools of one cluster; namespace may be empty
// when the cluster's name is unique on the installation.
func (s *Service) ListNodePools(ctx context.Context, cluster, namespace string) (*NodePools, error) {
	k := s.clients(ctx)
	c, err := s.getCluster(ctx, k, cluster, namespace)
	if err != nil {
		return nil, err
	}
	dyn := k.Dynamic
	cpVersion := controlPlaneVersion(ctx, dyn, c)

	pools, err := dyn.Resource(MachinePoolGVR).Namespace(c.GetNamespace()).List(ctx, metav1.ListOptions{
		LabelSelector: LabelClusterName + "=" + c.GetName(),
	})
	if err != nil {
		return nil, fmt.Errorf("list machinepools of %s/%s: %w", c.GetNamespace(), c.GetName(), err)
	}
	out := &NodePools{Cluster: c.GetName(), Namespace: c.GetNamespace(), ControlPlaneVersion: cpVersion, NodePools: []NodePool{}}
	accelerators := map[ReleaseRef]string{}
	for i := range pools.Items {
		mp := &pools.Items[i]
		np := NodePool{
			Name:                mp.GetName(),
			Namespace:           mp.GetNamespace(),
			Version:             nestedString(mp, "spec", "template", "spec", "version"),
			ControlPlaneVersion: cpVersion,
			Replicas:            nestedInt(mp, "spec", "replicas"),
			ReadyReplicas:       nestedInt(mp, "status", "readyReplicas"),
			InstanceTypes:       instanceTypes(ctx, dyn, mp),
		}
		if owner := ownerRelease(mp); owner != nil {
			np.OwnerRelease = owner
			if _, seen := accelerators[*owner]; !seen {
				accelerators[*owner] = releaseAccelerator(ctx, dyn, *owner)
			}
			np.Accelerator = accelerators[*owner]
		}
		out.NodePools = append(out.NodePools, np)
	}
	sort.Slice(out.NodePools, func(i, j int) bool { return out.NodePools[i].Name < out.NodePools[j].Name })
	return out, nil
}

// controlPlaneVersion is the control plane object's spec.version, else the
// kubernetes component of the cluster's Release CR. A failed read is logged
// and yields ""; the tool still answers with the pools.
func controlPlaneVersion(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured) string {
	if ref := nestedRef(c, "spec", "controlPlaneRef"); ref != nil {
		cp, err := getRef(ctx, dyn, ref, c.GetNamespace())
		if err == nil {
			if v := nestedString(cp, "spec", "version"); v != "" {
				return v
			}
		} else {
			slog.Debug("control plane not readable, falling back to the Release CR", "cluster", c.GetName(), "error", err)
		}
	}
	return releaseKubernetesVersion(ctx, dyn, c.GetLabels()[LabelReleaseVersion])
}

// releaseKubernetesVersion reads the kubernetes component of the Release CR
// for a release version (`<provider>-<version>`, e.g. aws-31.0.0), "" when
// none matches.
func releaseKubernetesVersion(ctx context.Context, dyn dynamic.Interface, version string) string {
	if version == "" {
		return ""
	}
	releases, err := dyn.Resource(ReleaseGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		slog.Debug("releases not readable", "error", err)
		return ""
	}
	for i := range releases.Items {
		r := &releases.Items[i]
		if !strings.HasSuffix(r.GetName(), "-"+version) {
			continue
		}
		components, _, _ := unstructured.NestedSlice(r.Object, "spec", "components")
		for _, comp := range components {
			m, ok := comp.(map[string]any)
			if !ok || m["name"] != releaseComponentKubernetes {
				continue
			}
			if v, ok := m["version"].(string); ok && v != "" {
				if !strings.HasPrefix(v, "v") {
					v = "v" + v
				}
				return v
			}
		}
	}
	return ""
}

func ownerRelease(mp *unstructured.Unstructured) *ReleaseRef {
	labels := mp.GetLabels()
	name := labels[labelFluxName]
	if name == "" {
		return nil
	}
	ns := labels[labelFluxNamespace]
	if ns == "" {
		ns = mp.GetNamespace()
	}
	return &ReleaseRef{Name: name, Namespace: ns}
}

// releaseAccelerator is the accelerator of a pool release — the chart's
// pool.accelerator value, as compose.Pool writes it — "" when the release
// is unreadable or names none.
func releaseAccelerator(ctx context.Context, dyn dynamic.Interface, ref ReleaseRef) string {
	hr, err := dyn.Resource(HelmReleaseGVR).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		slog.Debug("owner release not readable", "release", ref.Namespace+"/"+ref.Name, "error", err)
		return ""
	}
	accelerator, _ := poolValues(hr)
	return accelerator
}

// instanceTypes reads the pool's infrastructure object: a launch template's
// instanceType (AWSMachinePool), else every node.kubernetes.io/instance-type
// requirement it carries (KarpenterMachinePool). Empty when unreadable.
func instanceTypes(ctx context.Context, dyn dynamic.Interface, mp *unstructured.Unstructured) []string {
	ref := nestedRef(mp, "spec", "template", "spec", "infrastructureRef")
	if ref == nil {
		return []string{}
	}
	infra, err := getRef(ctx, dyn, ref, mp.GetNamespace())
	if err != nil {
		slog.Debug("infrastructure of the pool not readable", "pool", mp.GetName(), "error", err)
		return []string{}
	}
	if t := nestedString(infra, "spec", "awsLaunchTemplate", "instanceType"); t != "" {
		return []string{t}
	}
	types := requirementValues(infra.Object["spec"], labelInstanceType)
	sort.Strings(types)
	return types
}

// requirementValues collects the values of every {key, values} requirement
// with the given key anywhere below v.
func requirementValues(v any, key string) []string {
	out := []string{}
	switch x := v.(type) {
	case map[string]any:
		if x["key"] == key {
			values, _ := x["values"].([]any)
			for _, val := range values {
				if s, ok := val.(string); ok {
					out = append(out, s)
				}
			}
			return out
		}
		for _, child := range x {
			out = append(out, requirementValues(child, key)...)
		}
	case []any:
		for _, child := range x {
			out = append(out, requirementValues(child, key)...)
		}
	}
	return out
}
