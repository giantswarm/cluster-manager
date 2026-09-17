package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/detect"
)

// PresetFit is where the serving presets stand against the pool's sizes
// (create_node_pool; giantswarm/agent-platform#502): per preset the smallest
// size that hosts it, or why none does. Origin says whose presets they are —
// `published`, the ConfigMaps a serving layer publishes on the cluster, which
// take precedence; `chart`, the connectivity chart's shipped set the slice
// would publish, read from the registry while nothing is published yet
// (giantswarm/cluster-manager#44) — and Source names where exactly they were
// read. Note says why none were judged, when none were.
type PresetFit struct {
	Origin  string          `json:"origin,omitempty"`
	Source  string          `json:"source,omitempty"`
	Note    string          `json:"note,omitempty"`
	Presets []PresetSizeFit `json:"presets,omitempty"`
	// unpublished marks a cluster read fine with no preset published on it
	// — the case the chart's shipped set answers.
	unpublished bool
}

// Preset origins.
const (
	PresetOriginPublished = "published"
	PresetOriginChart     = "chart"
)

// PresetSizeFit is one preset against the pool's sizes.
type PresetSizeFit struct {
	Preset string `json:"preset"`
	// DisplayName and Model are the preset's title and model id as it
	// declares them, for a picker; empty when it names none.
	DisplayName string `json:"displayName,omitempty"`
	Model       string `json:"model,omitempty"`
	// CPU and Memory are the predictor's requests as the preset writes them
	// (`unset` when it names none: the runtime's envelope applies), GPUs its
	// accelerator count, GPUMemoryGiB its weights plus overhead.
	CPU          string  `json:"cpu"`
	Memory       string  `json:"memory"`
	GPUs         int     `json:"gpus"`
	GPUMemoryGiB float64 `json:"gpuMemoryGiB"`
	// Size is the smallest of the pool's sizes that hosts the preset; empty
	// with Reason when none does.
	Size   string `json:"size,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// publishedPreset is the part of a ServingPreset document (the connectivity
// chart's serving-preset.schema.json) the fit reads.
type publishedPreset struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		DisplayName string `json:"displayName"`
		Model       struct {
			ID string `json:"id"`
		} `json:"model"`
		Resources struct {
			GPUs     *int           `json:"gpus"`
			Requests map[string]any `json:"requests"`
		} `json:"resources"`
		Requirements struct {
			WeightsGiB  float64  `json:"weightsGiB"`
			OverheadGiB *float64 `json:"overheadGiB"`
		} `json:"requirements"`
	} `json:"spec"`
}

// The schema's defaults: one GPU, 30 GiB of overhead.
const (
	presetDefaultGPUs        = 1
	presetDefaultOverheadGiB = 30
	presetUnset              = "unset"
)

// presetDocument is one preset to judge: its name as its source labels it
// and its ServingPreset document.
type presetDocument struct {
	name string
	doc  string
}

// presetFit reads the serving presets published on the target — the
// connectivity chart's preset ConfigMaps, in whatever namespace the release
// that renders them lives — and places each against the pool's sizes. Never
// a refusal: what cannot be read is a note. A cluster with none published
// is marked so (PresetFit.unpublished) for presetFitFromChart to answer.
func (s *Service) presetFit(ctx context.Context, t target, pool string, shapes []compose.InstanceShape) (*PresetFit, []string) {
	if t.Reader == nil {
		return &PresetFit{Note: fmt.Sprintf("the serving presets on %s are not readable as you (%s): whether the pool's sizes host them is not judged", t.Cluster, t.Reason)}, nil
	}
	cms, err := t.Reader.Resource(ConfigMapGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: detect.LabelServingPreset + "=true"})
	if err != nil {
		return &PresetFit{Note: fmt.Sprintf("listing the serving presets on %s failed (%v): whether the pool's sizes host them is not judged", t.Cluster, err)}, nil
	}
	if len(cms.Items) == 0 {
		return &PresetFit{Note: fmt.Sprintf("no serving preset is published on %s yet — the slice release publishes them once it is ready; a dryRun re-run then says which of the pool's sizes host each", t.Cluster), unpublished: true}, nil
	}
	namespaces := map[string]bool{}
	docs := make([]presetDocument, 0, len(cms.Items))
	for i := range cms.Items {
		cm := &cms.Items[i]
		namespaces[cm.GetNamespace()] = true
		data, _ := cm.Object["data"].(map[string]any)
		doc, _ := data[detect.PresetDocumentKey].(string)
		docs = append(docs, presetDocument{name: presetName(cm.Object), doc: doc})
	}
	names := make([]string, 0, len(namespaces))
	for ns := range namespaces {
		names = append(names, ns)
	}
	sort.Strings(names)
	out, warnings := judgePresets(pool, shapes, docs)
	out.Origin = PresetOriginPublished
	out.Source = fmt.Sprintf("%d preset ConfigMap(s) in %s on %s", len(cms.Items), strings.Join(names, ", "), t.Cluster)
	return out, warnings
}

// presetFitFromChart judges the presets the slice would publish when none is
// published on the target yet (giantswarm/cluster-manager#44): the
// connectivity chart's shipped set at the version the slice's agent-platform
// release resolves, read from the registry (compose.ReadShippedPresets). The
// published fit is kept when it judged anything, when the target could not be
// read, or when this server reads no charts; a registry that cannot be read
// is a note, never a guess.
func (s *Service) presetFitFromChart(ctx context.Context, published *PresetFit, chartVersion, pool string, shapes []compose.InstanceShape) (*PresetFit, []string) {
	if !published.unpublished || s.charts == nil || chartVersion == "" {
		return published, nil
	}
	shipped, err := compose.ReadShippedPresets(ctx, s.charts, chartVersion)
	if err != nil {
		return &PresetFit{Note: fmt.Sprintf("%s, and the presets it would publish could not be read from the registry (%v): whether the pool's sizes host them is not judged", strings.TrimSuffix(published.Note, "; a dryRun re-run then says which of the pool's sizes host each"), err)}, nil
	}
	docs := make([]presetDocument, 0, len(shipped.Presets))
	for _, p := range shipped.Presets {
		docs = append(docs, presetDocument{name: p.Name, doc: string(p.Document)})
	}
	out, warnings := judgePresets(pool, shapes, docs)
	out.Origin = PresetOriginChart
	out.Source = shipped.Source()
	return out, warnings
}

// judgePresets places each preset document against the pool's sizes. The
// warnings name the presets the accelerator could serve but no size of the
// pool hosts — a predictor composed from one sits Pending while Karpenter
// refuses every size.
func judgePresets(pool string, shapes []compose.InstanceShape, docs []presetDocument) (*PresetFit, []string) {
	out := &PresetFit{}
	var warnings []string
	for _, d := range docs {
		entry, req, err := presetRequests(d.name, d.doc)
		if err != nil {
			out.Presets = append(out.Presets, PresetSizeFit{Preset: d.name, Reason: err.Error()})
			continue
		}
		fit := compose.Fit(shapes, req)
		entry.Size, entry.Reason = fit.Size, fit.Reason
		out.Presets = append(out.Presets, entry)
		if fit.Size == "" && fit.Hostable {
			warnings = append(warnings, fmt.Sprintf("serving preset %s fits no size of pool %s: %s — a predictor composed from it would sit Pending while Karpenter refuses every size (giantswarm/agent-platform#502); add the size to sizes or serve a smaller preset", entry.Preset, pool, fit.Reason))
		}
	}
	sort.Slice(out.Presets, func(i, j int) bool { return out.Presets[i].Preset < out.Presets[j].Preset })
	sort.Strings(warnings)
	return out, warnings
}

// presetRequests decodes a preset document into the fit's input and the
// answer's row. A request the preset does not name is zero for the fit and
// `unset` in the answer.
func presetRequests(name, doc string) (PresetSizeFit, compose.PresetRequests, error) {
	if doc == "" {
		return PresetSizeFit{}, compose.PresetRequests{}, fmt.Errorf("the ConfigMap carries no %s", detect.PresetDocumentKey)
	}
	var p publishedPreset
	if err := yaml.Unmarshal([]byte(doc), &p); err != nil {
		return PresetSizeFit{}, compose.PresetRequests{}, fmt.Errorf("%s is not a ServingPreset: %w", detect.PresetDocumentKey, err)
	}
	if p.Metadata.Name != "" {
		name = p.Metadata.Name
	}
	req := compose.PresetRequests{Name: name, GPUs: presetDefaultGPUs}
	if p.Spec.Resources.GPUs != nil {
		req.GPUs = *p.Spec.Resources.GPUs
	}
	overhead := float64(presetDefaultOverheadGiB)
	if p.Spec.Requirements.OverheadGiB != nil {
		overhead = *p.Spec.Requirements.OverheadGiB
	}
	req.GPUMemoryGiB = p.Spec.Requirements.WeightsGiB + overhead
	row := PresetSizeFit{Preset: name, DisplayName: p.Spec.DisplayName, Model: p.Spec.Model.ID, CPU: presetUnset, Memory: presetUnset, GPUs: req.GPUs, GPUMemoryGiB: req.GPUMemoryGiB}
	var err error
	if req.CPU, row.CPU, err = quantityOf(p.Spec.Resources.Requests, "cpu"); err != nil {
		return PresetSizeFit{}, compose.PresetRequests{}, err
	}
	if req.Memory, row.Memory, err = quantityOf(p.Spec.Resources.Requests, "memory"); err != nil {
		return PresetSizeFit{}, compose.PresetRequests{}, err
	}
	return row, req, nil
}

// quantityOf parses requests.<key> as a Kubernetes quantity; absent is zero
// and `unset`.
func quantityOf(requests map[string]any, key string) (resource.Quantity, string, error) {
	v, ok := requests[key]
	if !ok || v == nil {
		return resource.Quantity{}, presetUnset, nil
	}
	text := fmt.Sprint(v)
	q, err := resource.ParseQuantity(text)
	if err != nil {
		return resource.Quantity{}, "", fmt.Errorf("resources.requests.%s %q is not a quantity: %w", key, text, err)
	}
	return q, q.String(), nil
}

// presetName is the preset's name as the ConfigMap labels it, else the
// ConfigMap's name.
func presetName(cm map[string]any) string {
	metadata, _ := cm["metadata"].(map[string]any)
	labels, _ := metadata["labels"].(map[string]any)
	if n, _ := labels[detect.LabelPreset].(string); n != "" {
		return n
	}
	n, _ := metadata["name"].(string)
	return n
}
