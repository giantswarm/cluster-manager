package compose

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestReadPrepullImages (giantswarm/agent-platform#812): the images the
// connectivity chart at the slice's version pre-pulls, in order; none when the
// chart's pre-pull is off; a chart that does not say whether it pre-pulls, or
// a version the registry lacks, is an error.
func TestReadPrepullImages(t *testing.T) {
	ctx := context.Background()
	on := "modelServing:\n  prepull:\n    enabled: true\n    images:\n      - gsoci.azurecr.io/giantswarm/storage-initializer:v0.21.0\n      - gsoci.azurecr.io/giantswarm/llm-d-fast/llm-d-cuda:v0.8.0\n"
	charts := (&fakeCharts{}).
		add(SliceChartURL, "4.85.0", map[string][]byte{"values.yaml": releasedWithChartValues("")}).
		add(connectivityURL, "4.85.0", map[string][]byte{"values.yaml": []byte(on)}).
		add(SliceChartURL, "4.50.0", map[string][]byte{"values.yaml": releasedWithChartValues("")}).
		add(connectivityURL, "4.50.0", map[string][]byte{"values.yaml": []byte("modelServing:\n  prepull:\n    enabled: false\n    images:\n      - gsoci.azurecr.io/giantswarm/llm-d-fast/llm-d-cuda:v0.8.0\n")}).
		add(SliceChartURL, "4.51.0", map[string][]byte{"values.yaml": releasedWithChartValues("")}).
		add(connectivityURL, "4.51.0", map[string][]byte{"values.yaml": []byte("modelServing:\n  cache:\n    enabled: true\n")})

	p, err := ReadPrepullImages(ctx, charts, "4.85.0")
	require.NoError(t, err)
	assert.Equal(t, &PrepullImages{
		Images: []string{"gsoci.azurecr.io/giantswarm/storage-initializer:v0.21.0", "gsoci.azurecr.io/giantswarm/llm-d-fast/llm-d-cuda:v0.8.0"},
		Source: "modelServing.prepull of agent-platform-connectivity 4.85.0, the chart the slice's agent-platform 4.85.0 release resolves",
	}, p)

	off, err := ReadPrepullImages(ctx, charts, "4.50.0")
	require.NoError(t, err)
	assert.Empty(t, off.Images, "a chart that pre-pulls nothing gives the pool nothing to fetch")

	_, err = ReadPrepullImages(ctx, charts, "4.51.0")
	require.EqualError(t, err, "agent-platform-connectivity 4.51.0 values.yaml names no modelServing.prepull.enabled")

	_, err = ReadPrepullImages(ctx, charts, "4.99.0")
	require.Error(t, err, "a version the registry lacks is an error, never another version's images")
}

// TestPrepullImagesOf: a running pre-pull DaemonSet's init containers'
// images, in the order the kubelet pulls them.
func TestPrepullImagesOf(t *testing.T) {
	ds := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
		"initContainers": []any{
			map[string]any{"name": "pull-0-storage-initializer", "image": "gsoci.azurecr.io/giantswarm/storage-initializer:v0.21.0"},
			map[string]any{"name": "pull-1-llm-d-cuda", "image": "gsoci.azurecr.io/giantswarm/llm-d-fast/llm-d-cuda:v0.8.0"},
		},
		"containers": []any{map[string]any{"name": "pause", "image": "gsoci.azurecr.io/giantswarm/pause:3.10.2"}},
	}}}}}
	assert.Equal(t, []string{"gsoci.azurecr.io/giantswarm/storage-initializer:v0.21.0", "gsoci.azurecr.io/giantswarm/llm-d-fast/llm-d-cuda:v0.8.0"}, PrepullImagesOf(ds))
}

// TestPoolPrefetchImages: the pool's prefetched images are the chart's
// pool.prefetchImages, written only when the pool has them.
func TestPoolPrefetchImages(t *testing.T) {
	c := Cluster{Name: "wc1", Namespace: "org-acme", Organization: "acme", KubernetesVersion: "1.33.1", MachineImage: "flatcar-stable-4459.2.1-kube-1.33.1-tooling-1.26.1-gs"}
	p := PoolSpec{Name: "gpu-l4", Accelerator: "nvidia-l4", MaxGPUs: 1}
	assert.NotContains(t, values(c, p)["pool"], "prefetchImages")
	p.PrefetchImages = []string{"gsoci.azurecr.io/giantswarm/llm-d-fast/llm-d-cuda:v0.8.0"}
	assert.Equal(t, []any{"gsoci.azurecr.io/giantswarm/llm-d-fast/llm-d-cuda:v0.8.0"}, values(c, p)["pool"].(map[string]any)["prefetchImages"])
}
