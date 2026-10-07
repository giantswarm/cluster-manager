package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/cluster-manager/internal/compose"
)

// TestJudgePresetsShippedFP8Preset judges the FP8 preset as the connectivity
// chart ships it (testdata/presets/qwen3-5-9b-fp8.yaml: agent-platform
// 4.118.0's files/model-serving/presets/qwen3-5-9b-fp8.yaml, verbatim), the
// first chart whose presets declare requirements.minComputeCapability
// (giantswarm/agent-platform#832), so the live answer on an installation
// running that chart is known before it runs there: an A10G pool lists the
// preset with no size and the compute-capability reason, an L4 pool hosts it
// on xlarge, and the row names the preset's own title, model and requests.
func TestJudgePresetsShippedFP8Preset(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "presets", "qwen3-5-9b-fp8.yaml"))
	require.NoError(t, err)
	docs := []presetDocument{{name: "qwen3-5-9b-fp8", doc: string(raw)}}

	a10g, err := compose.Shapes("nvidia-a10g", nil)
	require.NoError(t, err)
	out, warnings := judgePresets("gpu-a10g", a10g, docs)
	require.Len(t, out.Presets, 1)
	assert.Equal(t, PresetSizeFit{Preset: "qwen3-5-9b-fp8", DisplayName: "Qwen3.5 9B (FP8, one 24 GB GPU)", Model: "RedHatAI/Qwen3.5-9B-FP8-dynamic", CPU: "2", Memory: "10Gi", GPUs: 1, GPUMemoryGiB: 23, MinComputeCapability: "8.9",
		Reason: "needs compute capability 8.9; a g5 GPU (A10G) has 8.6"}, out.Presets[0])
	assert.Empty(t, warnings, "no A10G size ever hosts it: nothing for the pool to add")

	l4, err := compose.Shapes("nvidia-l4", nil)
	require.NoError(t, err)
	out, warnings = judgePresets("gpu-l4", l4, docs)
	require.Len(t, out.Presets, 1)
	assert.Equal(t, "xlarge", out.Presets[0].Size)
	assert.Empty(t, out.Presets[0].Reason)
	assert.Empty(t, warnings)
}

// TestJudgePresetsComputeCapability (giantswarm/cluster-manager#178): an A10G
// pool lists the FP8 preset with no size and the compute-capability reason,
// and no warning — no size of the family would host it; an L4 pool still
// reports xlarge. The row carries the declared floor for the picker.
func TestJudgePresetsComputeCapability(t *testing.T) {
	doc := presetDoc("qwen3-5-9b-fp8", "Qwen3.5 9B FP8", "Qwen/Qwen3.5-9B-FP8", "2", "10Gi", 11, 12) + "    minComputeCapability: \"8.9\"\n"
	docs := []presetDocument{{name: "qwen3-5-9b-fp8", doc: doc}}

	a10g, err := compose.Shapes("nvidia-a10g", nil)
	require.NoError(t, err)
	out, warnings := judgePresets("gpu-a10g", a10g, docs)
	require.Len(t, out.Presets, 1)
	assert.Equal(t, PresetSizeFit{Preset: "qwen3-5-9b-fp8", DisplayName: "Qwen3.5 9B FP8", Model: "Qwen/Qwen3.5-9B-FP8", CPU: "2", Memory: "10Gi", GPUs: 1, GPUMemoryGiB: 23, MinComputeCapability: "8.9",
		Reason: "needs compute capability 8.9; a g5 GPU (A10G) has 8.6"}, out.Presets[0])
	assert.Empty(t, warnings, "no A10G size ever hosts it: nothing for the pool to add")

	l4, err := compose.Shapes("nvidia-l4", nil)
	require.NoError(t, err)
	out, warnings = judgePresets("gpu-l4", l4, docs)
	require.Len(t, out.Presets, 1)
	assert.Equal(t, "xlarge", out.Presets[0].Size)
	assert.Empty(t, out.Presets[0].Reason)
	assert.Empty(t, warnings)
}
