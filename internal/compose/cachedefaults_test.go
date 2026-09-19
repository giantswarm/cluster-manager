package compose

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The connectivity chart's values as far as the cache defaults go: 100Gi on
// a class of the chart's own, gp3 at 500 MiB/s and 3000 IOPS.
const connectivityCacheValues = `modelServing:
  enabled: false
  cache:
    enabled: true
    pvc:
      name: hf-cache
      size: 100Gi
    storageClass:
      create: true
      provisioner: ebs.csi.aws.com
      parameters:
        type: gp3
        iops: "3000"
        throughput: "500"
`

// TestReadCacheDefaults (giantswarm/cluster-manager#83): the size and tier
// the connectivity chart at the slice's version creates the claim with, read
// from the chart the meta chart resolves; a chart that renders no class of
// its own leaves the tier unknown; a chart without the size is an error.
func TestReadCacheDefaults(t *testing.T) {
	ctx := context.Background()
	charts := (&fakeCharts{}).
		add(SliceChartURL, "4.44.1", map[string][]byte{"values.yaml": releasedWithChartValues("")}).
		add(connectivityURL, "4.44.1", map[string][]byte{"values.yaml": []byte(connectivityCacheValues)}).
		add(SliceChartURL, "4.50.0", map[string][]byte{"values.yaml": releasedWithChartValues("")}).
		add(connectivityURL, "4.50.0", map[string][]byte{"values.yaml": []byte("modelServing:\n  cache:\n    pvc:\n      size: 200Gi\n    storageClass:\n      create: false\n      name: gp3\n")}).
		add(SliceChartURL, "4.51.0", map[string][]byte{"values.yaml": releasedWithChartValues("")}).
		add(connectivityURL, "4.51.0", map[string][]byte{"values.yaml": []byte("modelServing:\n  cache:\n    enabled: true\n")})

	d, err := ReadCacheDefaults(ctx, charts, "4.44.1")
	require.NoError(t, err)
	assert.Equal(t, &CacheDefaults{MetaVersion: "4.44.1", Chart: "agent-platform-connectivity", Version: "4.44.1", Size: "100Gi", SizeGiB: 100, Tier: VolumeTier{Type: "gp3", IOPS: 3000, ThroughputMiBps: 500}}, d)
	assert.Equal(t, "the defaults of agent-platform-connectivity 4.44.1, the chart the slice's agent-platform 4.44.1 release resolves", d.Source())

	noClass, err := ReadCacheDefaults(ctx, charts, "4.50.0")
	require.NoError(t, err)
	assert.Equal(t, "200Gi", noClass.Size)
	assert.Equal(t, VolumeTier{}, noClass.Tier, "the class is the operator's: its tier is not the chart's to know")

	_, err = ReadCacheDefaults(ctx, charts, "4.51.0")
	require.EqualError(t, err, "agent-platform-connectivity 4.51.0 values.yaml names no modelServing.cache.pvc.size")

	_, err = ReadCacheDefaults(ctx, charts, "4.99.0")
	require.Error(t, err, "a version the registry lacks is an error, never another version's defaults")
}

// TestPriceClaim (giantswarm/cluster-manager#83): what a gp3 volume is billed
// per month in a region — storage, throughput above 125 MiB/s, IOPS above
// 3000 —, rounded to the cent; the baseline tier is the storage price alone;
// a region the table lacks, an unknown region, another volume type, an
// unknown tier or capacity is no price and a note.
func TestPriceClaim(t *testing.T) {
	gp3 := VolumeTier{Type: "gp3", IOPS: 3000, ThroughputMiBps: 500}
	price, note := PriceClaim(Region{Name: "eu-central-1"}, 100, gp3)
	require.NotNil(t, price, note)
	assert.InDelta(t, 27.37, price.MonthlyUSD, 1e-9, "100 × 0.0952 + 375 × 0.0476")
	assert.Equal(t, "AWS EBS gp3 list price, EU (Frankfurt) (eu-central-1)", price.Source)
	assert.Equal(t, PriceAsOf, price.AsOf)

	price, _ = PriceClaim(Region{Name: "eu-central-1"}, 500, VolumeTier{Type: "gp3", IOPS: 4000, ThroughputMiBps: 1000})
	assert.InDelta(t, 95.25, price.MonthlyUSD, 1e-9, "the 500 GiB / 1000 MiB/s / 4000 IOPS claim of before: storage 47.60, throughput 41.65, IOPS 6.00")

	price, _ = PriceClaim(Region{Name: "us-east-1"}, 100, VolumeTier{Type: "gp3"})
	assert.InDelta(t, 8, price.MonthlyUSD, 1e-9, "no IOPS or throughput named: the baseline tier, the storage alone, at Virginia's price")

	_, note = PriceClaim(Region{Name: "eu-central-1"}, 100, VolumeTier{Type: "gp2"})
	assert.Equal(t, "no price: the price table prices gp3 volumes only, and this volume is gp2", note)
	_, note = PriceClaim(Region{Name: "eu-central-1"}, 100, VolumeTier{})
	assert.Contains(t, note, "the volume's tier is not known")
	_, note = PriceClaim(Region{Name: "eu-central-1"}, 0, gp3)
	assert.Equal(t, "no price: the claim's capacity is not known", note)
	_, note = PriceClaim(Region{Name: "cn-north-1"}, 100, gp3)
	assert.Contains(t, note, "the price table has no EBS figures for region cn-north-1")
	_, note = PriceClaim(Region{Reason: "the Cluster names no infrastructureRef"}, 100, gp3)
	assert.Equal(t, "no price: the cluster's region is not known (the Cluster names no infrastructureRef)", note)
}
