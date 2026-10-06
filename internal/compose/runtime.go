package compose

import (
	"context"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/giantswarm/cluster-manager/internal/registry"
)

// The serving runtime's image sets giantswarm/llm-d publishes on gsoci: the
// fast set, the chart's default, and the slim set beside it, which leaves out
// what the Ampere and Ada GPUs never load (giantswarm/llm-d#29). Hopper,
// Blackwell and older GPUs keep the fast set.
const (
	FastRuntimePrefix = "gsoci.azurecr.io/giantswarm/llm-d-fast/"
	SlimRuntimePrefix = "gsoci.azurecr.io/giantswarm/llm-d-slim/"
)

// SlimAccelerators are the curated accelerators the slim runtime serves:
// A10G (Ampere), L4 and L40S (Ada). The T4 (Turing) is not among them: vLLM
// attends through FlashInfer there, whose ahead-of-time modules the slim set
// leaves out.
var SlimAccelerators = []string{"nvidia-a10g", "nvidia-l4", "nvidia-l40s"}

// SlimServes reports whether the slim runtime serves every accelerator of a
// cluster's GPU pools: none is no pool, and a cluster without one keeps the
// chart's default.
func SlimServes(accelerators []string) bool {
	if len(accelerators) == 0 {
		return false
	}
	for _, a := range accelerators {
		if !contains(SlimAccelerators, a) {
			return false
		}
	}
	return true
}

// RuntimeImages are the serving runtime's image references of the slice's
// chart: the registry prefix and the main image of each well-known
// LLMInferenceServiceConfig (kserve-runtime-configs.kserve.llmisvcConfigs),
// and the pre-pull's images in order (modelServing.prepull.images), with
// where they were read.
type RuntimeImages struct {
	Registry string
	Images   map[string]string
	Prepull  []string
	Source   string
}

// ReadRuntimeImages reads the runtime images from the values of the
// agent-platform chart at metaVersion, the chart the slice's release pins.
// Every step that fails is an error naming it; nothing is guessed from
// another version.
func ReadRuntimeImages(ctx context.Context, r ChartReader, metaVersion string) (*RuntimeImages, error) {
	ref, err := registry.ParseRef(SliceChartURL)
	if err != nil {
		return nil, err
	}
	meta, err := r.Chart(ctx, ref, metaVersion)
	if err != nil {
		return nil, err
	}
	raw := meta.File("values.yaml")
	if raw == nil {
		return nil, fmt.Errorf("%s %s carries no values.yaml", SliceChart, metaVersion)
	}
	var values struct {
		RuntimeConfigs struct {
			KServe struct {
				LLMISvcConfigs struct {
					ImageRegistry string                       `json:"imageRegistry"`
					Images        map[string]map[string]string `json:"images"`
				} `json:"llmisvcConfigs"`
			} `json:"kserve"`
		} `json:"kserve-runtime-configs"`
		ModelServing struct {
			Prepull struct {
				Images []string `json:"images"`
			} `json:"prepull"`
		} `json:"modelServing"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("%s %s values.yaml: %w", SliceChart, metaVersion, err)
	}
	configs := values.RuntimeConfigs.KServe.LLMISvcConfigs
	out := &RuntimeImages{
		Registry: configs.ImageRegistry,
		Images:   make(map[string]string, len(configs.Images)),
		Prepull:  values.ModelServing.Prepull.Images,
		Source:   fmt.Sprintf("kserve-runtime-configs.kserve.llmisvcConfigs and modelServing.prepull.images of %s %s", SliceChart, metaVersion),
	}
	for name, containers := range configs.Images {
		if main := containers["main"]; main != "" {
			out.Images[name] = main
		}
	}
	if out.Registry == "" || len(out.Images) == 0 {
		return nil, fmt.Errorf("%s %s values.yaml names no kserve-runtime-configs.kserve.llmisvcConfigs imageRegistry and images", SliceChart, metaVersion)
	}
	return out, nil
}

// Slim is the same set from the slim prefix: every reference of the fast
// set moved to its slim twin, the tag kept, so the chart's line still pins
// it. Refused when the chart's runtime is not the fast set, which has no
// slim twin.
func (ri RuntimeImages) Slim() (*RuntimeImages, error) {
	if ri.Registry != FastRuntimePrefix {
		return nil, fmt.Errorf("the chart's runtime registry is %s, not %s: it has no slim twin (%s)", ri.Registry, FastRuntimePrefix, ri.Source)
	}
	out := &RuntimeImages{Registry: SlimRuntimePrefix, Images: make(map[string]string, len(ri.Images)), Source: ri.Source}
	for name, image := range ri.Images {
		out.Images[name] = slimRef(image)
	}
	for _, image := range ri.Prepull {
		out.Prepull = append(out.Prepull, slimRef(image))
	}
	return out, nil
}

// slimRef moves a reference of the fast set to the slim set; any other
// reference (the storage-initializer) stays.
func slimRef(image string) string {
	if rest, ok := strings.CutPrefix(image, FastRuntimePrefix); ok {
		return SlimRuntimePrefix + rest
	}
	return image
}

// configImages are the configs' main images as the chart's values take
// them (llmisvcConfigs.images.<config>.main).
func (ri RuntimeImages) configImages() map[string]any {
	images := make(map[string]any, len(ri.Images))
	for name, image := range ri.Images {
		images[name] = map[string]any{"main": image}
	}
	return images
}

// prepullImages are the pre-pull's images as the chart's values take them.
func (ri RuntimeImages) prepullImages() []any {
	out := make([]any, 0, len(ri.Prepull))
	for _, image := range ri.Prepull {
		out = append(out, image)
	}
	return out
}
