package tools

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/cluster-manager/internal/compose"
	"github.com/giantswarm/cluster-manager/internal/detect"
)

// prefetchReads is what a pool's nodes fetch while they join: the serving
// layer's pre-pull images (compose.PoolSpec.PrefetchImages), and the note
// saying where they were read, or why the pool fetches none.
type prefetchReads struct {
	images []string
	note   string
}

// readPrefetch reads the images the serving layer on the target pre-pulls
// once a GPU node's GPU is usable, for the pool's nodes to fetch beforehand
// (giantswarm/agent-platform#812). The serving layer's provider decides the
// source: for the slice this write composes — cluster-manager's own, or none
// yet — the slim runtime's images where the slice takes that set (readRuntime),
// else the connectivity chart at the slice's version, read from the registry
// (compose.ReadPrepullImages), since the slice's DaemonSet exists only once
// its release is installed; for serving someone else provides (the platform's
// own release, a human), the pre-pull DaemonSet running on the target. A read
// that fails is the note and the pool fetches nothing, never another
// version's images: the pull then downloads after the GPU is usable, as
// without the fetch.
func (s *Service) readPrefetch(ctx context.Context, t target, slice sliceReads, runtime *compose.RuntimeImages) prefetchReads {
	defer timed(ctx, "prefetch images")()
	if composesSlice(slice.serving) && runtime != nil {
		return prefetchReads{images: runtime.Prepull, note: "the slim runtime's pre-pull images, from " + runtime.Source}
	}
	if composesSlice(slice.serving) {
		if s.charts == nil {
			return prefetchReads{note: "this server reads no chart registry: the pool's nodes fetch no image ahead of the pre-pull"}
		}
		version, err := compose.SliceChartVersion(compose.SliceSpec{ChartVersion: s.cfg.SliceChartVersion, Platform: slice.platform})
		if err != nil {
			return prefetchReads{note: fmt.Sprintf("the slice's chart version is not known (%v): the pool's nodes fetch no image ahead of the pre-pull", err)}
		}
		images, err := compose.ReadPrepullImages(ctx, s.charts, version)
		if err != nil {
			return prefetchReads{note: fmt.Sprintf("the slice's pre-pull images could not be read from the registry (%v): the pool's nodes fetch no image ahead of the pre-pull", err)}
		}
		return prefetchReads{images: images.Images, note: images.Source}
	}
	if !slice.serving.Present() {
		return prefetchReads{note: fmt.Sprintf("serving on %s is not known: the pool's nodes fetch no image ahead of the pre-pull", t.Cluster)}
	}
	if t.Reader == nil {
		return prefetchReads{note: fmt.Sprintf("the pre-pull on %s is not readable as you (%s): the pool's nodes fetch no image ahead of it", t.Cluster, t.Reason)}
	}
	list, err := t.Reader.Resource(detect.DaemonSetGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: compose.LabelPrepull + "=true"})
	if err != nil {
		return prefetchReads{note: fmt.Sprintf("listing the pre-pull DaemonSet on %s failed (%v): the pool's nodes fetch no image ahead of it", t.Cluster, err)}
	}
	switch len(list.Items) {
	case 0:
		return prefetchReads{note: fmt.Sprintf("%s provides serving on %s without a pre-pull DaemonSet: the pool's nodes fetch no image", providerDescription(slice.serving.Provider), t.Cluster)}
	case 1:
		ds := &list.Items[0]
		return prefetchReads{images: compose.PrepullImagesOf(ds), note: fmt.Sprintf("the init containers of the pre-pull DaemonSet %s/%s on %s", ds.GetNamespace(), ds.GetName(), t.Cluster)}
	default:
		names := make([]string, 0, len(list.Items))
		for i := range list.Items {
			names = append(names, list.Items[i].GetNamespace()+"/"+list.Items[i].GetName())
		}
		return prefetchReads{note: fmt.Sprintf("%d pre-pull DaemonSets on %s (%s): which one serves the pool is not known, the pool's nodes fetch no image", len(list.Items), t.Cluster, strings.Join(names, ", "))}
	}
}
