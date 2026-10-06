package tools

import (
	"context"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/cluster-manager/internal/compose"
)

// poolAccelerators are the accelerators of the cluster's GPU pools of
// cluster-manager's: each pool release's, with adding's — the pool a
// create_node_pool composes, empty for none — as composed (accelerator),
// never what its release declared before the write. Sorted, each once.
func poolAccelerators(releases map[string]*unstructured.Unstructured, adding, accelerator string) []string {
	seen := map[string]bool{}
	for pool, hr := range releases {
		if pool == adding {
			continue
		}
		a, _ := poolValues(hr)
		seen[a] = true
	}
	if adding != "" {
		seen[accelerator] = true
	}
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// readRuntime is the runtime image set the slice this write composes points
// at: the slim set when every pool of the cluster has an accelerator it
// serves (compose.SlimServes; giantswarm/llm-d#29), read from the chart the
// slice pins; nil keeps the chart's default — for a cluster with another
// accelerator or no pool, where cluster-manager composes no slice, and on a
// server that reads no chart registry (a lab's, a test's). Every
// write that composes the slice decides it the same way, so the set follows
// the cluster's pools and never flips between writes. A chart the registry
// does not serve is a refusal: the slice is never composed with another set than the
// pools call for.
func (s *Service) readRuntime(ctx context.Context, slice sliceReads, accelerators []string) (*compose.RuntimeImages, error) {
	if !composesSlice(slice.serving) || !compose.SlimServes(accelerators) || s.charts == nil {
		return nil, nil
	}
	defer timed(ctx, "runtime images from the chart")()
	version, err := compose.SliceChartVersion(compose.SliceSpec{ChartVersion: s.cfg.SliceChartVersion, Platform: slice.platform})
	if err != nil {
		return nil, &ErrRefused{Reason: err.Error()}
	}
	images, err := compose.ReadRuntimeImages(ctx, s.charts, version)
	if err != nil {
		return nil, &ErrRefused{Reason: fmt.Sprintf("the slim serving runtime for the cluster's pools (%v): %v", accelerators, err)}
	}
	slim, err := images.Slim()
	if err != nil {
		return nil, &ErrRefused{Reason: fmt.Sprintf("the slim serving runtime for the cluster's pools (%v): %v", accelerators, err)}
	}
	return slim, nil
}
