package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	discoveryfake "k8s.io/client-go/discovery/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/cluster-manager/internal/compose"
)

// serviceWithDiscovery builds the tools over the installation fixture whose
// apiserver answers every Cluster API list with the 404 an installation
// without the group gives, and the discovery given.
func serviceWithDiscovery(t *testing.T, disc discovery.DiscoveryInterface) *Service {
	t.Helper()
	dyn := newFake(t, "installation.yaml", ClusterGVR, MachinePoolGVR)
	return New(func(context.Context) Clients { return Clients{Dynamic: dyn, Discovery: disc} }, nil, Config{Installation: "gazelle"})
}

func TestWithoutClusterAPI(t *testing.T) {
	ctx := context.Background()
	svc := serviceWithDiscovery(t, fakeDiscovery(false))
	const absentNote = "the Cluster API (cluster.x-k8s.io) is not served on this installation"

	answer, err := svc.ListClusters(ctx)
	require.NoError(t, err)
	assert.Equal(t, []Cluster{}, answer.Clusters, "an installation without the Cluster API has no clusters")
	assert.Equal(t, ClusterAPI{Group: "cluster.x-k8s.io", Version: "v1beta1", State: ClusterAPIAbsent, Note: absentNote}, answer.ClusterAPI)
	assert.Equal(t, answer.ClusterAPI, svc.ClusterAPI(ctx), "get_info reports the same state")

	refusals := map[string]func() error{
		"list_node_pools": func() error {
			_, err := svc.ListNodePools(ctx, "nosuchcluster", "")
			return err
		},
		"create_node_pool dry run": func() error {
			_, err := svc.CreateNodePool(ctx, CreateNodePoolInput{Cluster: "nosuchcluster", Pool: compose.PoolSpec{Name: "gpu00", Accelerator: compose.Accelerators[0], MaxGPUs: 4, ChartVersion: compose.DefaultPoolChartVersion}, Mode: ModeApply, DryRun: true})
			return err
		},
		"delete_node_pool dry run": func() error {
			_, err := svc.DeleteNodePool(ctx, DeleteNodePoolInput{Cluster: "nosuchcluster", Name: "gpu00", Mode: ModeApply, DryRun: true})
			return err
		},
	}
	for name, call := range refusals {
		t.Run(name, func(t *testing.T) {
			err := call()
			var absent *ErrClusterAPIAbsent
			require.ErrorAs(t, err, &absent)
			assert.Equal(t, "cluster nosuchcluster not found: "+absentNote, err.Error())
			assert.NotContains(t, err.Error(), "could not find the requested resource", "the apiserver's bare 404 never reaches the caller")
		})
	}

	_, err = svc.ListNodePools(ctx, "nosuchcluster", "org-acme")
	assert.EqualError(t, err, "cluster org-acme/nosuchcluster not found: "+absentNote, "a namespaced ask names the namespace")
}

func TestClusterAPIUnknownWhenDiscoveryFails(t *testing.T) {
	ctx := context.Background()
	disc := &discoveryfake.FakeDiscovery{Fake: &k8stesting.Fake{}}
	disc.PrependReactor("get", "resource", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	svc := serviceWithDiscovery(t, disc)

	api := svc.ClusterAPI(ctx)
	assert.Equal(t, ClusterAPIUnknown, api.State)
	assert.Equal(t, "discover cluster.x-k8s.io/v1beta1: connection refused", api.Note)

	_, err := svc.ListClusters(ctx)
	assert.EqualError(t, err, api.Note, "a failed discovery is an error, not an empty list")
	_, err = svc.ListNodePools(ctx, "gazelle", "")
	assert.EqualError(t, err, api.Note)
}

func TestClusterAPIServed(t *testing.T) {
	svc := newLab(t, "installation.yaml").service(Config{Installation: "gazelle"})
	api := svc.ClusterAPI(context.Background())
	assert.Equal(t, ClusterAPI{Group: "cluster.x-k8s.io", Version: "v1beta1", State: ClusterAPIServed}, api, "served: no note")
}
