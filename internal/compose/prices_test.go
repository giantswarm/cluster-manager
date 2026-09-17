package compose

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPriced: the table's figure for the region, its source and date; a
// size the region does not offer, a region the table lacks and a region that
// could not be read each leave the price empty and say why.
func TestPriced(t *testing.T) {
	shapes, err := Shapes("nvidia-l4", []string{"xlarge", "2xlarge"})
	require.NoError(t, err)

	frankfurt := Priced(shapes, Region{Name: "eu-central-1"})
	require.NotNil(t, frankfurt[0].PricePerHourUSD)
	assert.InDelta(t, 1.0064, *frankfurt[0].PricePerHourUSD, 1e-9, "g6.xlarge in Frankfurt")
	assert.InDelta(t, 1.22249, *frankfurt[1].PricePerHourUSD, 1e-9, "g6.2xlarge in Frankfurt, the figure as AWS prints it")
	assert.Equal(t, "AWS EC2 on-demand Linux list price, EU (Frankfurt) (eu-central-1)", frankfurt[0].PriceSource)
	assert.Equal(t, PriceAsOf, frankfurt[0].PriceAsOf)
	assert.Empty(t, frankfurt[0].PriceNote)
	assert.Nil(t, shapes[0].PricePerHourUSD, "the input is not changed")

	virginia := Priced(shapes, Region{Name: "us-east-1"})
	assert.InDelta(t, 0.8048, *virginia[0].PricePerHourUSD, 1e-9, "the same size is a fifth cheaper in Virginia: never guessed across regions")

	ireland := Priced(shapes, Region{Name: "eu-west-1"})
	assert.Nil(t, ireland[0].PricePerHourUSD)
	assert.Equal(t, "no on-demand price: AWS lists no on-demand g6.xlarge in EU (Ireland) (eu-west-1) — the size is not offered there", ireland[0].PriceNote)
	assert.Empty(t, ireland[0].PriceSource)

	china := Priced(shapes, Region{Name: "cn-north-1"})
	assert.Nil(t, china[0].PricePerHourUSD)
	assert.Equal(t, "no on-demand price: the price table has no figures for region cn-north-1 (it covers ap-southeast-1, eu-central-1, eu-north-1, eu-west-1, eu-west-2, us-east-1, us-west-2); a price from another region is not guessed", china[0].PriceNote)

	unknown := Priced(shapes, Region{Reason: "the Cluster names no infrastructureRef"})
	assert.Nil(t, unknown[0].PricePerHourUSD)
	assert.Equal(t, "no on-demand price: the cluster's region is not known (the Cluster names no infrastructureRef)", unknown[0].PriceNote)
}

// TestPriceTableCoversTheFamilies: every region of the table prices the
// smallest size of at least one curated family, and Frankfurt prices the
// chart's default sizes of every family it offers.
func TestPriceTableCoversTheFamilies(t *testing.T) {
	for _, region := range PriceRegions() {
		assert.NotEmpty(t, onDemandPrices[region], region)
		assert.NotEmpty(t, regionLocations[region], region)
	}
	for _, acc := range Accelerators {
		shapes, err := Shapes(acc, nil)
		require.NoError(t, err)
		for _, s := range Priced(shapes, Region{Name: "eu-central-1"}) {
			assert.NotNil(t, s.PricePerHourUSD, s.InstanceType)
		}
	}
}
