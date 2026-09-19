package compose

import (
	"context"
	"fmt"
	"strconv"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

// CacheDefaults are the size and tier the connectivity chart creates the
// model cache claim with where none exists (its values' modelServing.cache:
// pvc.size, storageClass.parameters), read from the chart the slice's
// agent-platform release resolves — what a claim the slice mounts will cost
// before it exists (giantswarm/cluster-manager#83).
type CacheDefaults struct {
	// Chart and Version name the connectivity chart read; MetaVersion the
	// agent-platform chart the slice pins.
	MetaVersion string
	Chart       string
	Version     string
	// Size is the claim's requested storage as the chart writes it
	// (`100Gi`); SizeGiB the same in GiB.
	Size    string
	SizeGiB float64
	// Tier is the StorageClass the chart renders for the claim: the volume
	// type, IOPS and throughput of its parameters. Empty type when the chart
	// renders no class of its own (storageClass.create false): the claim
	// then binds on the class storageClass.name or the cluster's default
	// names, whose tier the chart does not know.
	Tier VolumeTier
}

// Source says where the defaults were read, for a note.
func (d *CacheDefaults) Source() string {
	return fmt.Sprintf("the defaults of %s %s, the chart the slice's %s %s release resolves", d.Chart, d.Version, SliceChart, d.MetaVersion)
}

// ReadCacheDefaults reads the model cache claim's defaults from the
// connectivity chart the slice at metaVersion resolves (connectivityChart).
// Every step that fails is an error naming it; nothing is guessed from
// another version.
func ReadCacheDefaults(ctx context.Context, r ChartReader, metaVersion string) (*CacheDefaults, error) {
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
			Cache struct {
				PVC struct {
					Size string `json:"size"`
				} `json:"pvc"`
				StorageClass struct {
					Create     bool              `json:"create"`
					Parameters map[string]string `json:"parameters"`
				} `json:"storageClass"`
			} `json:"cache"`
		} `json:"modelServing"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("%s %s values.yaml: %w", src.Chart, src.Version, err)
	}
	cache := values.ModelServing.Cache
	out := &CacheDefaults{MetaVersion: metaVersion, Chart: src.Chart, Version: src.Version, Size: cache.PVC.Size}
	if cache.PVC.Size == "" {
		return nil, fmt.Errorf("%s %s values.yaml names no modelServing.cache.pvc.size", src.Chart, src.Version)
	}
	q, err := resource.ParseQuantity(cache.PVC.Size)
	if err != nil {
		return nil, fmt.Errorf("%s %s values.yaml modelServing.cache.pvc.size %q: %w", src.Chart, src.Version, cache.PVC.Size, err)
	}
	out.SizeGiB = QuantityGiB(q)
	if cache.StorageClass.Create {
		out.Tier = TierOf(cache.StorageClass.Parameters)
	}
	return out, nil
}

// QuantityGiB is a storage quantity in GiB (100Gi → 100; 500G → 465.66).
func QuantityGiB(q resource.Quantity) float64 {
	return float64(q.Value()) / (1024 * 1024 * 1024)
}

// TierOf reads a volume's tier from its StorageClass parameters as the EBS
// CSI driver names them: `type`, `iops`, `throughput` (MiB/s), each a
// string. A figure that does not parse is left zero.
func TierOf(parameters map[string]string) VolumeTier {
	t := VolumeTier{Type: parameters["type"]}
	if v, err := strconv.ParseInt(parameters["iops"], 10, 64); err == nil {
		t.IOPS = v
	}
	if v, err := strconv.ParseInt(parameters["throughput"], 10, 64); err == nil {
		t.ThroughputMiBps = v
	}
	return t
}
