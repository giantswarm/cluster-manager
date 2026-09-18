package tools

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/cluster-manager/internal/compose"
)

// The cluster's AWS infrastructure object: the Cluster API AWSCluster its
// Cluster names, whose spec.region is where the pool's nodes launch and whose
// spec.network.subnets are the subnets a pool's nodes may come up in.
const (
	awsClusterKind = "AWSCluster"
	// subnetRoleTag marks what a subnet is for; subnetRoleNodes is the value
	// of the node subnets — the ones the gpu-node-pool chart's EC2NodeClass
	// selects (its subnetSelectorTerms), and the ones cluster-aws tags so on
	// every private subnet that is not a pod subnet.
	subnetRoleTag   = "giantswarm.io/role"
	subnetRoleNodes = "nodes"
)

// awsInfra is what the AWSCluster says about a pool's launch: the region
// (the sizes' prices) and the availability zones of the node subnets (the
// zones a pool may be pinned to).
type awsInfra struct {
	region compose.Region
	// zones are the zones of the cluster's node subnets, sorted; empty with
	// zonesReason saying why they are unknown — no infrastructure object, not
	// an AWSCluster, no subnet tagged for nodes. What cannot be read is a
	// reason, never a guess.
	zones       []string
	zonesReason string
}

// awsInfrastructure reads the cluster's AWSCluster once
// (spec.infrastructureRef): the region for the price of a pool's sizes and
// the node subnets' zones for the zones a caller names.
func awsInfrastructure(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured) awsInfra {
	ref := nestedRef(c, "spec", "infrastructureRef")
	if ref == nil {
		return awsInfra{}.unknown(fmt.Sprintf("Cluster %s names no infrastructureRef", c.GetName()))
	}
	if kind, _ := ref["kind"].(string); kind != awsClusterKind {
		return awsInfra{}.unknown(fmt.Sprintf("the infrastructure of cluster %s is a %s, not an %s", c.GetName(), kind, awsClusterKind))
	}
	infra, err := getRef(ctx, dyn, ref, c.GetNamespace())
	if err != nil {
		return awsInfra{}.unknown(err.Error())
	}
	out := awsInfra{}
	if region := nestedString(infra, "spec", "region"); region != "" {
		out.region = compose.Region{Name: region}
	} else {
		out.region = compose.Region{Reason: fmt.Sprintf("%s %s/%s carries no spec.region", awsClusterKind, infra.GetNamespace(), infra.GetName())}
	}
	out.zones = nodeSubnetZones(infra)
	if len(out.zones) == 0 {
		out.zonesReason = fmt.Sprintf("%s %s/%s lists no node subnet (spec.network.subnets tagged %s: %s)", awsClusterKind, infra.GetNamespace(), infra.GetName(), subnetRoleTag, subnetRoleNodes)
	}
	return out
}

// unknown is the infrastructure that could not be read, the reason on both
// the region (the price table is AWS's) and the zones.
func (a awsInfra) unknown(reason string) awsInfra {
	a.region = compose.Region{Reason: reason + ": the price table is AWS's"}
	a.zonesReason = reason
	return a
}

// nodeSubnetZones are the distinct zones of the subnets tagged for nodes,
// sorted.
func nodeSubnetZones(infra *unstructured.Unstructured) []string {
	subnets, _, _ := unstructured.NestedSlice(infra.Object, "spec", "network", "subnets")
	var zones []string
	for _, s := range subnets {
		subnet, _ := s.(map[string]any)
		tags, _ := subnet["tags"].(map[string]any)
		if tags[subnetRoleTag] != subnetRoleNodes {
			continue
		}
		if zone, _ := subnet["availabilityZone"].(string); zone != "" {
			zones = appendNew(zones, zone)
		}
	}
	slices.Sort(zones)
	return zones
}

// checkZones refuses a zone the cluster has no node subnet in — the pool's
// EC2NodeClass selects the node subnets, so a node could never come up there
// — naming the zones the cluster has; and refuses every zone while the node
// subnets cannot be read, naming why. Nothing named is nothing to check.
func (a awsInfra) checkZones(zones []string, cluster string) error {
	if len(zones) == 0 {
		return nil
	}
	if len(a.zones) == 0 {
		return &ErrRefused{Reason: fmt.Sprintf("zones %s: the node subnets of cluster %s cannot be read (%s) — a pool's nodes come up in the cluster's node subnets, so the zones cannot be checked against them", strings.Join(zones, ", "), cluster, a.zonesReason)}
	}
	var unknown []string
	for _, z := range zones {
		if !slices.Contains(a.zones, z) {
			unknown = appendNew(unknown, z)
		}
	}
	if len(unknown) > 0 {
		return &ErrRefused{Reason: fmt.Sprintf("zones %s: cluster %s has no node subnet there — its node subnets are in %s, and a pool's nodes come up in those; name zones among them", strings.Join(unknown, ", "), cluster, strings.Join(a.zones, ", "))}
	}
	return nil
}
