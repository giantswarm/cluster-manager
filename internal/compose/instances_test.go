package compose

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
)

// preset is a PresetRequests literal for the tests.
func preset(name, cpu, memory string, gpus int, gpuGiB float64) PresetRequests {
	return PresetRequests{Name: name, CPU: resource.MustParse(cpu), Memory: resource.MustParse(memory), GPUs: gpus, GPUMemoryGiB: gpuGiB}
}

// TestShapesCoverTheChart: every accelerator of the curated list has a family
// with the chart's default sizes, and a size the family lacks is refused
// naming the sizes.
func TestShapesCoverTheChart(t *testing.T) {
	for _, acc := range Accelerators {
		shapes, err := Shapes(acc, nil)
		require.NoError(t, err, acc)
		require.Len(t, shapes, len(DefaultPoolSizes), acc)
		for i, s := range shapes {
			assert.Equal(t, DefaultPoolSizes[i], s.Size)
			assert.Equal(t, InstanceFamily(acc)+"."+s.Size, s.InstanceType)
			assert.Positive(t, s.GPUMemoryGiB, acc)
		}
	}
	_, err := Shapes("nvidia-l4", []string{"xlarge", "xlage"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `size "xlage": not a size of the g6 family (nvidia-l4); the sizes are xlarge, 2xlarge, 4xlarge, 8xlarge, 12xlarge, 16xlarge, 24xlarge, 48xlarge`)
	_, err = Shapes("nvidia-h100", nil)
	require.Error(t, err)
	assert.Nil(t, FamilySizes("nvidia-h100"))
	assert.Equal(t, []string{"xlarge", "2xlarge", "4xlarge", "8xlarge", "12xlarge", "16xlarge", "metal"}, FamilySizes("nvidia-t4"))
}

// TestShapeUsable pins the node model (giantswarm/agent-platform#502): a
// g6.xlarge leaves a predictor 3 vCPU / 11.9 GiB, a g6e.xlarge 3 / 27.1.
func TestShapeUsable(t *testing.T) {
	l4, err := Shapes("nvidia-l4", []string{"xlarge", "2xlarge"})
	require.NoError(t, err)
	assert.Equal(t, InstanceShape{InstanceType: "g6.xlarge", Size: "xlarge", VCPU: 4, MemoryGiB: 16, GPUs: 1, GPUMemoryGiB: 24, UsableVCPU: 3, UsableMemoryGiB: 11.9}, l4[0])
	assert.Equal(t, InstanceShape{InstanceType: "g6.2xlarge", Size: "2xlarge", VCPU: 8, MemoryGiB: 32, GPUs: 1, GPUMemoryGiB: 24, UsableVCPU: 7, UsableMemoryGiB: 27.1}, l4[1])
	l40s, err := Shapes("nvidia-l40s", []string{"xlarge"})
	require.NoError(t, err)
	assert.Equal(t, InstanceShape{InstanceType: "g6e.xlarge", Size: "xlarge", VCPU: 4, MemoryGiB: 32, GPUs: 1, GPUMemoryGiB: 48, UsableVCPU: 3, UsableMemoryGiB: 27.1}, l40s[0])
}

// TestFit places presets against a pool's sizes: the resized L4 preset on an
// xlarge; the old one (4 vCPU / 16 GiB, the gazelle incident) on no size of an
// xlarge-only pool but on the 2xlarge the reason names, hostable; a 128 GB
// preset on no L4 at all, not hostable; a two-GPU preset on a 12xlarge.
func TestFit(t *testing.T) {
	xlarge, err := Shapes("nvidia-l4", []string{"xlarge"})
	require.NoError(t, err)
	defaults, err := Shapes("nvidia-l4", nil)
	require.NoError(t, err)

	resized := preset("qwen3-4b-instruct", "2", "10Gi", 1, 20)
	assert.Equal(t, SizeFit{Preset: "qwen3-4b-instruct", Size: "xlarge", Hostable: true}, Fit(xlarge, resized))

	old := preset("qwen3-4b-instruct", "4", "16Gi", 1, 20)
	fit := Fit(xlarge, old)
	assert.Empty(t, fit.Size)
	assert.True(t, fit.Hostable)
	assert.Equal(t, "requests 4 vCPU / 16 GiB; xlarge leaves a predictor 3 vCPU / 11.9 GiB after the node's kubelet reservations and daemonsets — 2xlarge (8 vCPU / 32 GiB) would host it", fit.Reason)
	assert.Equal(t, SizeFit{Preset: "qwen3-4b-instruct", Size: "2xlarge", Hostable: true}, Fit(defaults, old), "the pool's default sizes host it on the 2xlarge")

	millicores := preset("tight", "3500m", "11.5Gi", 1, 20)
	assert.Empty(t, Fit(xlarge, millicores).Size, "3.5 vCPU is over the 3 an xlarge leaves")
	assert.Equal(t, "2xlarge", Fit(defaults, millicores).Size)

	big := preset("nemotron-3-super-nvfp4", "4", "48Gi", 1, 105)
	fit = Fit(defaults, big)
	assert.Empty(t, fit.Size)
	assert.False(t, fit.Hostable, "an L4 can never serve it: no warning")
	assert.Equal(t, "needs 105 GiB of GPU memory across 1 GPU(s); a g6 GPU has 24 GiB", fit.Reason)

	twoGPU := preset("tp2", "8", "40Gi", 2, 40)
	fit = Fit(defaults, twoGPU)
	assert.Empty(t, fit.Size)
	assert.True(t, fit.Hostable)
	assert.Equal(t, "needs 2 GPUs; 4xlarge, the pool's largest size, carries 1 — 12xlarge (4 GPUs, 48 vCPU / 192 GiB) would host it", fit.Reason)

	sixteenGPU := preset("tp16", "8", "40Gi", 16, 40)
	fit = Fit(defaults, sixteenGPU)
	assert.False(t, fit.Hostable)
	assert.Contains(t, fit.Reason, "no size of the family carries that many")

	huge := preset("hog", "200", "16Gi", 1, 20)
	fit = Fit(defaults, huge)
	assert.False(t, fit.Hostable)
	assert.Contains(t, fit.Reason, "no size of the family leaves enough")

	unset := PresetRequests{Name: "bare", GPUs: 1, GPUMemoryGiB: 20}
	assert.Equal(t, "xlarge", Fit(xlarge, unset).Size, "no requests: judged on the GPU alone")
	assert.Equal(t, "the pool has no sizes", Fit(nil, unset).Reason)
}

// TestPoolSpecValidateSizes: Validate refuses a size outside the family the
// way Shapes does, before anything is composed.
func TestPoolSpecValidateSizes(t *testing.T) {
	spec := PoolSpec{Name: "gpu-l4", Accelerator: "nvidia-l4", MaxGPUs: 1, Sizes: []string{"xlarge", "3xlarge"}}
	err := spec.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `size "3xlarge": not a size of the g6 family`)
	spec.Sizes = []string{"48xlarge"}
	require.NoError(t, spec.Validate())
	spec.Accelerator, spec.Sizes = "nvidia-t4", []string{"metal"}
	require.NoError(t, spec.Validate())
}
