package compose

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const runtimeValuesYAML = `kserve-runtime-configs:
  kserve:
    llmisvcConfigs:
      imageRegistry: gsoci.azurecr.io/giantswarm/llm-d-fast/
      images:
        kserve-config-llm-template: {main: gsoci.azurecr.io/giantswarm/llm-d-fast/llm-d-cuda:v0.8.0}
        kserve-config-llm-decode-template: {main: gsoci.azurecr.io/giantswarm/llm-d-fast/llm-d-cuda:v0.8.0}
modelServing:
  prepull:
    images:
      - gsoci.azurecr.io/giantswarm/storage-initializer:v0.21.0
      - gsoci.azurecr.io/giantswarm/llm-d-fast/llm-d-cuda:v0.8.0
`

// TestSlimServes (giantswarm/llm-d#29): the slim runtime serves a cluster
// whose every pool is Ampere or Ada; a T4 pool, or no pool, keeps the
// chart's default.
func TestSlimServes(t *testing.T) {
	assert.True(t, SlimServes([]string{"nvidia-l4"}))
	assert.True(t, SlimServes([]string{"nvidia-a10g", "nvidia-l40s"}))
	assert.False(t, SlimServes([]string{"nvidia-l4", "nvidia-t4"}), "one Turing pool keeps the cluster on the fast set")
	assert.False(t, SlimServes(nil), "no pool keeps the chart's default")
}

// TestReadRuntimeImages: the runtime images of the slice's chart, their slim
// twins with the tag kept and the storage-initializer left, and the errors
// for a chart that names none or a runtime with no slim twin.
func TestReadRuntimeImages(t *testing.T) {
	ctx := context.Background()
	charts := (&fakeCharts{}).
		add(SliceChartURL, "4.95.0", map[string][]byte{"values.yaml": []byte(runtimeValuesYAML)}).
		add(SliceChartURL, "4.94.0", map[string][]byte{"values.yaml": []byte("modelServing: {}\n")}).
		add(SliceChartURL, "4.93.0", map[string][]byte{"values.yaml": []byte("kserve-runtime-configs:\n  kserve:\n    llmisvcConfigs:\n      imageRegistry: ghcr.io/llm-d/\n      images:\n        kserve-config-llm-template: {main: ghcr.io/llm-d/llm-d-cuda:v0.8.0}\n")})

	fast, err := ReadRuntimeImages(ctx, charts, "4.95.0")
	require.NoError(t, err)
	slim, err := fast.Slim()
	require.NoError(t, err)
	assert.Equal(t, &RuntimeImages{
		Registry: SlimRuntimePrefix,
		Images: map[string]string{
			"kserve-config-llm-template":        "gsoci.azurecr.io/giantswarm/llm-d-slim/llm-d-cuda:v0.8.0",
			"kserve-config-llm-decode-template": "gsoci.azurecr.io/giantswarm/llm-d-slim/llm-d-cuda:v0.8.0",
		},
		Prepull: []string{"gsoci.azurecr.io/giantswarm/storage-initializer:v0.21.0", "gsoci.azurecr.io/giantswarm/llm-d-slim/llm-d-cuda:v0.8.0"},
		Source:  "kserve-runtime-configs.kserve.llmisvcConfigs and modelServing.prepull.images of agent-platform 4.95.0",
	}, slim)

	_, err = ReadRuntimeImages(ctx, charts, "4.94.0")
	require.EqualError(t, err, "agent-platform 4.94.0 values.yaml names no kserve-runtime-configs.kserve.llmisvcConfigs imageRegistry and images")

	mirror, err := ReadRuntimeImages(ctx, charts, "4.93.0")
	require.NoError(t, err)
	_, err = mirror.Slim()
	require.ErrorContains(t, err, "has no slim twin")

	_, err = ReadRuntimeImages(ctx, charts, "4.99.0")
	require.Error(t, err, "a version the registry lacks is an error, never another version's images")
}

// TestSliceValuesRuntime: a slice with a runtime set points the configs'
// registry and main images and the pre-pull at it; without one the chart's
// defaults stand.
func TestSliceValuesRuntime(t *testing.T) {
	slim := &RuntimeImages{
		Registry: SlimRuntimePrefix,
		Images:   map[string]string{"kserve-config-llm-template": "gsoci.azurecr.io/giantswarm/llm-d-slim/llm-d-cuda:v0.8.0"},
		Prepull:  []string{"gsoci.azurecr.io/giantswarm/storage-initializer:v0.21.0", "gsoci.azurecr.io/giantswarm/llm-d-slim/llm-d-cuda:v0.8.0"},
	}
	values, err := SliceValues(wc1(), SliceSpec{Platform: platform(), Runtime: slim})
	require.NoError(t, err)
	registry, _, _ := unstructured.NestedString(values, "kserve-runtime-configs", "kserve", "llmisvcConfigs", "imageRegistry")
	assert.Equal(t, SlimRuntimePrefix, registry)
	main, _, _ := unstructured.NestedString(values, "kserve-runtime-configs", "kserve", "llmisvcConfigs", "images", "kserve-config-llm-template", "main")
	assert.Equal(t, "gsoci.azurecr.io/giantswarm/llm-d-slim/llm-d-cuda:v0.8.0", main)
	prepull, _, _ := unstructured.NestedStringSlice(values, "modelServing", "prepull", "images")
	assert.Equal(t, slim.Prepull, prepull)

	values, err = SliceValues(wc1(), SliceSpec{Platform: platform()})
	require.NoError(t, err)
	_, found, _ := unstructured.NestedFieldNoCopy(values, "kserve-runtime-configs", "kserve", "llmisvcConfigs", "imageRegistry")
	assert.False(t, found, "no runtime set keeps the chart's default")
}
