package compose

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/cluster-manager/internal/registry"
)

// fakeCharts is a ChartReader over charts held in memory, keyed by
// repository path and version.
type fakeCharts struct {
	charts map[string]map[string]*registry.Chart
	reads  []string
}

func (f *fakeCharts) add(url, version string, files map[string][]byte) *fakeCharts {
	ref, err := registry.ParseRef(url)
	if err != nil {
		panic(err)
	}
	if f.charts == nil {
		f.charts = map[string]map[string]*registry.Chart{}
	}
	if f.charts[ref.Path] == nil {
		f.charts[ref.Path] = map[string]*registry.Chart{}
	}
	f.charts[ref.Path][version] = &registry.Chart{Ref: ref, Version: version, Files: files}
	return f
}

func (f *fakeCharts) Tags(_ context.Context, ref registry.Ref) ([]string, error) {
	f.reads = append(f.reads, "tags "+ref.Path)
	versions, ok := f.charts[ref.Path]
	if !ok {
		return nil, fmt.Errorf("%s: HTTP 404", ref)
	}
	tags := make([]string, 0, len(versions))
	for v := range versions {
		tags = append(tags, v)
	}
	return tags, nil
}

func (f *fakeCharts) Chart(_ context.Context, ref registry.Ref, version string) (*registry.Chart, error) {
	f.reads = append(f.reads, "chart "+ref.Path+"@"+version)
	chart, ok := f.charts[ref.Path][version]
	if !ok {
		return nil, fmt.Errorf("pull %s %s: HTTP 404", ref, version)
	}
	return chart, nil
}

const connectivityURL = "oci://gsoci.azurecr.io/charts/giantswarm/agent-platform-connectivity"

func metaValues(rng string) []byte {
	return []byte(fmt.Sprintf(`components:
  muster:
    chart: muster
    repository: oci://gsoci.azurecr.io/charts/giantswarm
    versionRange: ">=5.0.0 <6.0.0"
  agent-platform-connectivity:
    chart: agent-platform-connectivity
    repository: oci://gsoci.azurecr.io/charts/giantswarm
    versionRange: %q
    forwardAllValues: true
`, rng))
}

var (
	qwen8b = []byte("apiVersion: agent-platform.giantswarm.io/v1alpha1\nkind: ServingPreset\nmetadata:\n  name: qwen3-8b-fp8\nspec:\n  resources:\n    gpus: 1\n    requests: {cpu: \"2\", memory: 10Gi}\n  requirements: {weightsGiB: 9, overheadGiB: 12}\n")
	big    = []byte("apiVersion: agent-platform.giantswarm.io/v1alpha1\nkind: ServingPreset\nmetadata:\n  name: nemotron\nspec:\n  requirements: {weightsGiB: 70, overheadGiB: 35}\n")
)

// TestReadShippedPresets: the meta chart at the slice's pin names the
// connectivity chart and its range, the range resolves to the newest
// connectivity release in the registry (not the meta chart's own version),
// and the presets are that chart's files.
func TestReadShippedPresets(t *testing.T) {
	f := (&fakeCharts{}).
		add(SliceChartURL, "4.29.1", map[string][]byte{"values.yaml": metaValues(">=4.0.0 <5.0.0")}).
		add(connectivityURL, "4.29.1", map[string][]byte{"files/model-serving/presets/qwen3-8b-fp8.yaml": qwen8b}).
		add(connectivityURL, "4.30.0", map[string][]byte{"files/model-serving/presets/qwen3-8b-fp8.yaml": qwen8b, "files/model-serving/presets/nemotron.yaml": big, "files/model-serving/presets/README.md": []byte("not a preset")}).
		add(connectivityURL, "5.0.0-dev.1", map[string][]byte{})
	got, err := ReadShippedPresets(context.Background(), f, "4.29.1")
	require.NoError(t, err)
	assert.Equal(t, "4.29.1", got.MetaVersion)
	assert.Equal(t, "agent-platform-connectivity", got.Chart)
	assert.Equal(t, "4.30.0", got.Version, "the range resolves to the newest release, as the installation's Flux resolves it")
	assert.Equal(t, ">=4.0.0 <5.0.0", got.Range)
	assert.Equal(t, "gsoci.azurecr.io", got.Registry)
	require.Len(t, got.Presets, 2, "the .yaml files alone")
	assert.Equal(t, "nemotron", got.Presets[0].Name)
	assert.Equal(t, "qwen3-8b-fp8", got.Presets[1].Name)
	assert.Equal(t, qwen8b, got.Presets[1].Document)
	assert.Equal(t, `2 preset(s) shipped by agent-platform-connectivity 4.30.0, the chart the slice's agent-platform 4.29.1 release resolves for ">=4.0.0 <5.0.0" at gsoci.azurecr.io — the slice publishes them once it is ready`, got.Source())
	assert.Equal(t, []string{"chart charts/giantswarm/agent-platform@4.29.1", "tags charts/giantswarm/agent-platform-connectivity", "chart charts/giantswarm/agent-platform-connectivity@4.30.0"}, f.reads)
}

// TestReadShippedPresetsErrors: every step that fails names itself; nothing
// is read from another version.
func TestReadShippedPresetsErrors(t *testing.T) {
	ctx := context.Background()
	f := (&fakeCharts{}).add(SliceChartURL, "4.29.1", map[string][]byte{"values.yaml": metaValues(">=4.0.0 <5.0.0")})
	_, err := ReadShippedPresets(ctx, f, "4.27.0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pull oci://gsoci.azurecr.io/charts/giantswarm/agent-platform 4.27.0: HTTP 404")

	_, err = ReadShippedPresets(ctx, f, "4.29.1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent-platform-connectivity: HTTP 404", "no connectivity chart in the registry")

	f.add(connectivityURL, "3.9.0", map[string][]byte{})
	_, err = ReadShippedPresets(ctx, f, "4.29.1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no tag satisfies ">=4.0.0 <5.0.0" among 1 tag(s)`)

	f.add(SliceChartURL, "4.0.0", map[string][]byte{"values.yaml": []byte("components:\n  muster: {chart: muster}\n")})
	_, err = ReadShippedPresets(ctx, f, "4.0.0")
	require.Error(t, err)
	assert.Equal(t, "agent-platform 4.0.0: values.yaml names no components.agent-platform-connectivity: the chart does not pin the connectivity chart", err.Error())

	f.add(SliceChartURL, "4.0.1", map[string][]byte{"Chart.yaml": []byte("name: agent-platform\n")})
	_, err = ReadShippedPresets(ctx, f, "4.0.1")
	require.Error(t, err)
	assert.Equal(t, "agent-platform 4.0.1: the chart carries no values.yaml", err.Error())

	f.add(SliceChartURL, "4.0.2", map[string][]byte{"values.yaml": []byte("components:\n  agent-platform-connectivity: {chart: agent-platform-connectivity}\n")})
	_, err = ReadShippedPresets(ctx, f, "4.0.2")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "names no chart, repository or versionRange")
}
