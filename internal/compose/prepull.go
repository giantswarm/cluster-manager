package compose

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// LabelPrepull marks the pods of the serving layer's pre-pull DaemonSet, the
// connectivity chart's `modelServing.prepull` (its selector label).
const LabelPrepull = "agent-platform.giantswarm.io/model-serving-prepull"

// PrepullImages are the images the serving layer's pre-pull keeps present on
// every GPU node, in its order — the storage-initializer, the serving
// runtime —, and where they were read. A pool's nodes fetch their content
// while they join (the chart's `pool.prefetchImages`): the pre-pull starts
// only once the GPU is usable, and the runtime's gigabytes then only unpack
// (giantswarm/agent-platform#812).
type PrepullImages struct {
	Images []string
	Source string
}

// ReadPrepullImages reads the pre-pull's images from the connectivity chart
// the slice at metaVersion resolves (connectivityChart): its values'
// `modelServing.prepull.images`, none when `modelServing.prepull.enabled` is
// false. What cluster-manager's slice pre-pulls is the chart's default — the
// slice's values set no images of their own —, readable before the slice's
// DaemonSet exists, which a hook renders only once the release is installed.
// Every step that fails is an error naming it; nothing is guessed from
// another version.
func ReadPrepullImages(ctx context.Context, r ChartReader, metaVersion string) (*PrepullImages, error) {
	child, src, err := connectivityChart(ctx, r, metaVersion)
	if err != nil {
		return nil, err
	}
	raw := child.File("values.yaml")
	if raw == nil {
		return nil, fmt.Errorf("%s %s carries no values.yaml", src.Chart, src.Version)
	}
	var values struct {
		ModelServing struct {
			Prepull struct {
				Enabled *bool    `json:"enabled"`
				Images  []string `json:"images"`
			} `json:"prepull"`
		} `json:"modelServing"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("%s %s values.yaml: %w", src.Chart, src.Version, err)
	}
	prepull := values.ModelServing.Prepull
	out := &PrepullImages{Source: fmt.Sprintf("modelServing.prepull of %s %s, the chart the slice's %s %s release resolves", src.Chart, src.Version, SliceChart, metaVersion)}
	if prepull.Enabled == nil {
		return nil, fmt.Errorf("%s %s values.yaml names no modelServing.prepull.enabled", src.Chart, src.Version)
	}
	if *prepull.Enabled {
		out.Images = prepull.Images
	}
	return out, nil
}

// PrepullImagesOf reads the images of a running pre-pull DaemonSet: its init
// containers' images in order, each pulled by the kubelet before the next.
func PrepullImagesOf(ds *unstructured.Unstructured) []string {
	containers, _, _ := unstructured.NestedSlice(ds.Object, "spec", "template", "spec", "initContainers")
	var out []string
	for _, c := range containers {
		if image, _ := c.(map[string]any)["image"].(string); image != "" {
			out = append(out, image)
		}
	}
	return out
}
