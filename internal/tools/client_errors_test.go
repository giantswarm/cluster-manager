package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClientSelectionErrors(t *testing.T) {
	selectionError := errors.New("caller client construction failed")
	service := New(func(context.Context) (Clients, error) {
		return Clients{}, selectionError
	}, nil, Config{})
	tests := []struct {
		name string
		call func(context.Context) error
	}{
		{"list clusters", func(ctx context.Context) error { _, err := service.ListClusters(ctx); return err }},
		{"list releases", func(ctx context.Context) error { _, err := service.ListReleases(ctx, ""); return err }},
		{"list pools", func(ctx context.Context) error { _, err := service.ListNodePools(ctx, "dev01", "org-acme"); return err }},
		{"create cluster", func(ctx context.Context) error { _, err := service.CreateCluster(ctx, dev01()); return err }},
		{"delete cluster", func(ctx context.Context) error {
			_, err := service.DeleteCluster(ctx, DeleteClusterInput{Organization: "acme", Name: "dev01", Mode: ModeApply})
			return err
		}},
		{"create pool", func(ctx context.Context) error {
			_, err := service.CreateNodePool(ctx, CreateNodePoolInput{Mode: ModeApply})
			return err
		}},
		{"delete pool", func(ctx context.Context) error {
			_, err := service.DeleteNodePool(ctx, DeleteNodePoolInput{Mode: ModeApply})
			return err
		}},
		{"enable serving", func(ctx context.Context) error {
			_, err := service.EnableModelServing(ctx, ModelServingInput{Mode: ModeApply})
			return err
		}},
		{"disable serving", func(ctx context.Context) error {
			_, err := service.DisableModelServing(ctx, ModelServingInput{Mode: ModeApply})
			return err
		}},
		{"remove cache", func(ctx context.Context) error {
			_, err := service.RemoveModelCache(ctx, RemoveModelCacheInput{Mode: ModeApply})
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.ErrorIs(t, test.call(t.Context()), selectionError)
		})
	}
	require.Equal(t, ClusterAPIUnknown, service.ClusterAPI(t.Context()).State)
}
