package compose

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/giantswarm/cluster-manager/internal/registry"
)

// Where the connectivity chart carries its shipped serving presets, and the
// meta chart's component entry that pins the connectivity chart
// (components.agent-platform-connectivity in the meta chart's values: chart,
// repository, versionRange, semverFilter — what the meta chart renders into
// the child's OCIRepository).
const (
	ConnectivityComponent = "agent-platform-connectivity"
	presetsDir            = "files/model-serving/presets"
)

// ChartReader reads charts from a registry: the tag list of a repository and
// one chart archive by exact version (registry.Client, or a fake).
type ChartReader interface {
	Tags(ctx context.Context, ref registry.Ref) ([]string, error)
	Chart(ctx context.Context, ref registry.Ref, version string) (*registry.Chart, error)
}

// ShippedPresets are the serving presets the slice release *would* publish:
// the connectivity chart's shipped set, read from the chart the slice's
// agent-platform release resolves — the meta chart at the slice's pin names
// the connectivity chart and its repository, and either releases it off its
// own tag (`releasedWithChart`: the connectivity version IS the meta chart's,
// rendered as the OCIRepository's exact semver) or names a version range
// Flux resolves on the installation, resolved here against the registry's
// tags the way source-controller does.
type ShippedPresets struct {
	// MetaVersion is the agent-platform chart version the slice pins;
	// Chart and Version the connectivity chart and its version; Range the
	// range that version came from, empty when the chart is released with
	// the meta chart (ReleasedWithChart); Registry the host both came from.
	MetaVersion       string
	Chart             string
	Version           string
	Range             string
	ReleasedWithChart bool
	Registry          string
	// Presets are the preset documents (ServingPreset YAML) by file name
	// without extension, the chart's file order.
	Presets []ShippedPreset
}

// ShippedPreset is one preset file of the chart.
type ShippedPreset struct {
	Name     string
	Document []byte
}

// Source says where the presets were read, for the answer.
func (p *ShippedPresets) Source() string {
	if p.ReleasedWithChart {
		return fmt.Sprintf("%d preset(s) shipped by %s %s, released with the slice's %s %s chart, at %s — the slice publishes them once it is ready", len(p.Presets), p.Chart, p.Version, SliceChart, p.MetaVersion, p.Registry)
	}
	return fmt.Sprintf("%d preset(s) shipped by %s %s, the chart the slice's %s %s release resolves for %q at %s — the slice publishes them once it is ready", len(p.Presets), p.Chart, p.Version, SliceChart, p.MetaVersion, p.Range, p.Registry)
}

// ReadShippedPresets reads the presets the slice at metaVersion would
// publish (ShippedPresets): the meta chart at that version from the slice's
// chart repository, its connectivity component entry, the connectivity
// chart's version — the meta chart's own when the entry is released with it
// (and names no range or filter of its own), else the connectivity chart's
// tags resolved through the entry's range and filter —, the connectivity
// chart at that version, and its preset files. Every step that fails is an
// error naming it; nothing is guessed from another version.
func ReadShippedPresets(ctx context.Context, r ChartReader, metaVersion string) (*ShippedPresets, error) {
	metaRef, err := registry.ParseRef(SliceChartURL)
	if err != nil {
		return nil, err
	}
	meta, err := r.Chart(ctx, metaRef, metaVersion)
	if err != nil {
		return nil, err
	}
	entry, err := connectivityEntry(meta)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", SliceChart, metaVersion, err)
	}
	childRef, err := registry.ParseRef(strings.TrimSuffix(entry.Repository, "/") + "/" + entry.Chart)
	if err != nil {
		return nil, fmt.Errorf("%s %s: components.%s: %w", SliceChart, metaVersion, ConnectivityComponent, err)
	}
	out := &ShippedPresets{MetaVersion: metaVersion, Chart: entry.Chart, Range: entry.VersionRange, Registry: childRef.Host}
	if entry.ownVersion() {
		// Released off the meta chart's tag: one right version, the meta
		// chart's own (the template's rule; a range there fails its render).
		out.Version, out.ReleasedWithChart, out.Range = metaVersion, true, ""
	} else {
		tags, err := r.Tags(ctx, childRef)
		if err != nil {
			return nil, err
		}
		if out.Version, err = registry.Resolve(tags, entry.VersionRange, entry.SemverFilter); err != nil {
			return nil, fmt.Errorf("%s: %w", childRef, err)
		}
	}
	child, err := r.Chart(ctx, childRef, out.Version)
	if err != nil {
		return nil, err
	}
	for _, name := range child.Glob(presetsDir) {
		if path.Ext(name) != ".yaml" {
			continue
		}
		out.Presets = append(out.Presets, ShippedPreset{Name: strings.TrimSuffix(path.Base(name), ".yaml"), Document: child.File(name)})
	}
	sort.Slice(out.Presets, func(i, j int) bool { return out.Presets[i].Name < out.Presets[j].Name })
	return out, nil
}

// componentEntry is the meta chart's components.<name> entry as far as the
// child's source goes: the chart and its repository; releasedWithChart, the
// mark of a chart published off the meta chart's own tag (its version is the
// meta chart's, the template renders an exact semver); else versionRange and
// semverFilter, what the template renders into the OCIRepository. A
// development build keeps its knobs on a released-with entry: a range or a
// filter there selects a dev channel and is resolved as a range.
type componentEntry struct {
	Chart             string `json:"chart"`
	Repository        string `json:"repository"`
	ReleasedWithChart bool   `json:"releasedWithChart"`
	VersionRange      string `json:"versionRange"`
	SemverFilter      string `json:"semverFilter"`
}

// ownVersion reports whether the entry's version is the meta chart's own:
// released with it and no range or filter of its own.
func (e componentEntry) ownVersion() bool {
	return e.ReleasedWithChart && e.VersionRange == "" && e.SemverFilter == ""
}

// connectivityEntry reads components.agent-platform-connectivity from the
// meta chart's values.yaml.
func connectivityEntry(meta *registry.Chart) (componentEntry, error) {
	raw := meta.File("values.yaml")
	if raw == nil {
		return componentEntry{}, fmt.Errorf("the chart carries no values.yaml")
	}
	var values struct {
		Components map[string]componentEntry `json:"components"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		return componentEntry{}, fmt.Errorf("values.yaml: %w", err)
	}
	entry, ok := values.Components[ConnectivityComponent]
	switch {
	case !ok:
		return componentEntry{}, fmt.Errorf("values.yaml names no components.%s: the chart does not pin the connectivity chart", ConnectivityComponent)
	case entry.Chart == "" || entry.Repository == "":
		return componentEntry{}, fmt.Errorf("components.%s names no chart or repository (%+v)", ConnectivityComponent, entry)
	case !entry.ReleasedWithChart && entry.VersionRange == "":
		return componentEntry{}, fmt.Errorf("components.%s is neither released with the chart nor pinned to a versionRange (%+v): no version to read the presets from", ConnectivityComponent, entry)
	}
	return entry, nil
}
