package tools

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/cluster-manager/internal/compose"
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
	// empty when the pool has no release or the release names none. Sizes
	// are the release's sizes (the chart's default when it names none) as
	// create_node_pool answers them — the node as AWS lists it, what it
	// leaves a predictor, its on-demand price per hour in the cluster's
	// region (giantswarm/cluster-manager#44); empty without a release.
	Accelerator string                  `json:"accelerator,omitempty"`
	Sizes       []compose.InstanceShape `json:"sizes,omitempty"`
	// OwnerRelease is the HelmRelease that applied the pool (Flux's labels),
	// null for a pool created by other means.
	OwnerRelease *ReleaseRef `json:"ownerRelease"`
	// Phase is where the pool stands: creating, ready, scaling, removing or
	// failed; Steps are the steps behind it — the pool release Ready, the
	// MachinePool ready, the nodes — each with its state and timestamps, and
	// for a pool created with prewarm the placeholder's step after them
	// (informational: it never decides the phase).
	Phase Phase  `json:"phase"`
	Steps []Step `json:"steps"`
	// Deleting: a delete_node_pool is under way; Pending are the teardown's
	// objects still present, terminating where deleted — the pool stays
	// listed, removing, until its HelmRelease is gone.
	Deleting bool           `json:"deleting,omitempty"`
	Pending  []ObjectAction `json:"pending,omitempty"`
}

// ReleaseRef names a HelmRelease.
type ReleaseRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// ListNodePools lists the pools of one cluster — its MachinePools, and the
// pool releases whose MachinePool is not there (right after create_node_pool,
// or while helm-controller uninstalls it) — with their lifecycle; namespace
// may be empty when the cluster's name is unique on the installation. Every
// read is concurrent: the call stays inside the aggregator's deadline
// (giantswarm/cluster-manager#34's pattern).
func (s *Service) ListNodePools(ctx context.Context, cluster, namespace string) (*NodePools, error) {
	defer timed(ctx, "list node pools", "cluster", cluster)()
	k := s.clients(ctx)
	c, err := s.getCluster(ctx, k, cluster, namespace)
	if err != nil {
		return nil, err
	}
	dyn := k.Dynamic
	var (
		cpVersion string
		pools     *unstructured.UnstructuredList
		releases  map[string]*unstructured.Unstructured
		t         target
		region    compose.Region
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { cpVersion = controlPlaneVersion(gctx, dyn, c); return nil })
	g.Go(func() error { region = awsInfrastructure(gctx, dyn, c).region; return nil })
	g.Go(func() (err error) {
		pools, err = dyn.Resource(MachinePoolGVR).Namespace(c.GetNamespace()).List(gctx, metav1.ListOptions{
			LabelSelector: LabelClusterName + "=" + c.GetName(),
		})
		if err != nil {
			return fmt.Errorf("list machinepools of %s/%s: %w", c.GetNamespace(), c.GetName(), err)
		}
		return nil
	})
	g.Go(func() (err error) { releases, err = poolReleases(gctx, dyn, c.GetNamespace(), c.GetName()); return err })
	g.Go(func() error { t = s.target(gctx, dyn, c); return nil })
	if err := g.Wait(); err != nil {
		return nil, err
	}

	type entry struct{ mp, release *unstructured.Unstructured }
	entries := map[string]entry{}
	for i := range pools.Items {
		mp := &pools.Items[i]
		e := entry{mp: mp, release: releases[mp.GetName()]}
		if owner := ownerRelease(mp); owner != nil && releases[owner.Name] != nil {
			e.release = releases[owner.Name]
		}
		entries[mp.GetName()] = e
	}
	for name, hr := range releases {
		if _, listed := entries[name]; !listed {
			entries[name] = entry{release: hr}
		}
	}
	out := &NodePools{Cluster: c.GetName(), Namespace: c.GetNamespace(), ControlPlaneVersion: cpVersion, NodePools: make([]NodePool, 0, len(entries))}
	var mu sync.Mutex
	g, gctx = errgroup.WithContext(ctx)
	for name, e := range entries {
		g.Go(func() error {
			np := s.nodePool(gctx, dyn, c, t, e.mp, e.release, name, cpVersion, region)
			mu.Lock()
			out.NodePools = append(out.NodePools, np)
			mu.Unlock()
			return nil
		})
	}
	_ = g.Wait()
	sort.Slice(out.NodePools, func(i, j int) bool { return out.NodePools[i].Name < out.NodePools[j].Name })
	return out, nil
}

// nodePool is one pool of the answer: the MachinePool's facts where it
// exists, the release's otherwise, and the lifecycle derived from both.
func (s *Service) nodePool(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured, t target, mp, release *unstructured.Unstructured, name, cpVersion string, region compose.Region) NodePool {
	r := s.readPoolState(ctx, dyn, c, t, mp, release, name)
	np := NodePool{Name: name, Namespace: c.GetNamespace(), ControlPlaneVersion: cpVersion, InstanceTypes: []string{}}
	if mp != nil {
		np.Version = nestedString(mp, "spec", "template", "spec", "version")
		np.Replicas = nestedInt(mp, "spec", "replicas")
		np.ReadyReplicas = nestedInt(mp, "status", "readyReplicas")
		np.InstanceTypes = instanceTypes(r.infra)
		np.OwnerRelease = ownerRelease(mp)
	} else if r.release != nil {
		np.OwnerRelease = &ReleaseRef{Name: r.release.GetName(), Namespace: r.release.GetNamespace()}
	}
	if r.release != nil {
		var sizes []string
		np.Accelerator, sizes = poolValues(r.release)
		if shapes, err := compose.Shapes(np.Accelerator, sizes); err == nil {
			np.Sizes = compose.Priced(shapes, region)
		} else {
			slog.Debug("pool release names sizes outside the curated shapes", "cluster", c.GetName(), "pool", name, "error", err)
		}
	}
	np.Phase, np.Steps, np.Deleting, np.Pending = lifecycle(r)
	return np
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

// instanceTypes reads the pool's infrastructure object: a launch template's
// instanceType (AWSMachinePool), else every node.kubernetes.io/instance-type
// requirement it carries (KarpenterMachinePool). Empty when unreadable.
func instanceTypes(infra *unstructured.Unstructured) []string {
	if infra == nil {
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
