package tools

import (
	"context"
	"fmt"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/registry"
)

// fakeCharts is a compose.ChartReader over charts held in memory: the
// registry as the tests need it — the agent-platform meta chart at the
// fixture's platform version naming the connectivity chart and its range, and
// the connectivity chart's releases with their shipped presets.
type fakeCharts struct {
	charts map[string]map[string]*registry.Chart
}

func (f *fakeCharts) add(url, version string, files map[string]string) *fakeCharts {
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
	chart := &registry.Chart{Ref: ref, Version: version, Files: map[string][]byte{}}
	for name, content := range files {
		chart.Files[name] = []byte(content)
	}
	f.charts[ref.Path][version] = chart
	return f
}

func (f *fakeCharts) Tags(_ context.Context, ref registry.Ref) ([]string, error) {
	versions, ok := f.charts[ref.Path]
	if !ok {
		return nil, fmt.Errorf("list tags of %s: HTTP 404", ref)
	}
	tags := make([]string, 0, len(versions))
	for v := range versions {
		tags = append(tags, v)
	}
	return tags, nil
}

func (f *fakeCharts) Chart(_ context.Context, ref registry.Ref, version string) (*registry.Chart, error) {
	chart, ok := f.charts[ref.Path][version]
	if !ok {
		return nil, fmt.Errorf("pull %s %s: HTTP 404", ref, version)
	}
	return chart, nil
}

const connectivityChartURL = "oci://gsoci.azurecr.io/charts/giantswarm/agent-platform-connectivity"

// The fixture's platform runs agent-platform 4.27.2 (installation.yaml);
// the connectivity range resolves to 4.28.0, which ships the resized L4
// preset, a 14B preset no L4 hosts and a preset only a 2xlarge hosts.
func shippedPresetCharts() *fakeCharts {
	return (&fakeCharts{}).
		add(compose.SliceChartURL, "4.27.2", map[string]string{"values.yaml": "components:\n  agent-platform-connectivity:\n    chart: agent-platform-connectivity\n    repository: oci://gsoci.azurecr.io/charts/giantswarm\n    versionRange: \">=4.0.0 <5.0.0\"\n"}).
		add(connectivityChartURL, "4.27.2", map[string]string{"files/model-serving/presets/qwen3-4b-instruct.yaml": presetDoc("qwen3-4b-instruct", "Qwen3 4B Instruct", "Qwen/Qwen3-4B-Instruct-2507", "2", "10Gi", 8, 12)}).
		add(connectivityChartURL, "4.28.0", map[string]string{
			"files/model-serving/presets/qwen3-8b-fp8.yaml": presetDoc("qwen3-8b-fp8", "Qwen3 8B FP8", "Qwen/Qwen3-8B-FP8", "2", "10Gi", 9, 12),
			"files/model-serving/presets/qwen3-14b.yaml":    presetDoc("qwen3-14b", "Qwen3 14B", "Qwen/Qwen3-14B", "4", "48Gi", 28, 30),
			"files/model-serving/presets/wide-l4.yaml":      presetDoc("wide-l4", "Wide L4 recipe", "acme/wide-9b", "4", "16Gi", 8, 12),
		}).
		add(connectivityChartURL, "5.0.0", map[string]string{})
}

func presetDoc(name, display, model, cpu, memory string, weights, overhead int) string {
	return fmt.Sprintf(`apiVersion: agent-platform.giantswarm.io/v1alpha1
kind: ServingPreset
metadata:
  name: %s
spec:
  displayName: %s
  model:
    id: %s
  resources:
    gpus: 1
    requests:
      cpu: %q
      memory: %s
  requirements:
    weightsGiB: %d
    overheadGiB: %d
`, name, display, model, cpu, memory, weights, overhead)
}
