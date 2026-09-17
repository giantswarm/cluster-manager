package tools

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/cluster-manager/internal/compose"
)

// awsClusterKind is the Cluster API infrastructure object of an AWS cluster,
// whose spec.region names the region the pool's nodes launch in.
const awsClusterKind = "AWSCluster"

// awsRegion reads the cluster's AWS region from the infrastructure object its
// Cluster names (spec.infrastructureRef → AWSCluster.spec.region), for the
// price of a pool's sizes. What cannot be read is a reason, never a guess:
// the price table is AWS's, per region.
func awsRegion(ctx context.Context, dyn dynamic.Interface, c *unstructured.Unstructured) compose.Region {
	ref := nestedRef(c, "spec", "infrastructureRef")
	if ref == nil {
		return compose.Region{Reason: fmt.Sprintf("Cluster %s names no infrastructureRef", c.GetName())}
	}
	if kind, _ := ref["kind"].(string); kind != awsClusterKind {
		return compose.Region{Reason: fmt.Sprintf("the infrastructure of cluster %s is a %s, not an %s: the price table is AWS's", c.GetName(), kind, awsClusterKind)}
	}
	infra, err := getRef(ctx, dyn, ref, c.GetNamespace())
	if err != nil {
		return compose.Region{Reason: err.Error()}
	}
	region := nestedString(infra, "spec", "region")
	if region == "" {
		return compose.Region{Reason: fmt.Sprintf("%s %s/%s carries no spec.region", awsClusterKind, infra.GetNamespace(), infra.GetName())}
	}
	return compose.Region{Name: region}
}
