package compose

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Region is a cluster's AWS region as read from its infrastructure
// (AWSCluster.spec.region), or why it could not be (Reason, with Name empty).
type Region struct {
	Name   string
	Reason string
}

// PriceRegions lists the regions the price table covers, sorted.
func PriceRegions() []string {
	out := make([]string, 0, len(onDemandPrices))
	for r := range onDemandPrices {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// Priced fills each shape's on-demand price for the region from the table
// (prices_table.go): the list price of the instance type in USD per hour, the
// list it came from and the day it was read. A region the table does not
// cover, a size AWS does not offer there, or a region that could not be read
// leaves the price empty and says why in PriceNote — never a figure from
// another region. The shapes are copied; the input is not changed.
func Priced(shapes []InstanceShape, region Region) []InstanceShape {
	out := make([]InstanceShape, len(shapes))
	for i, s := range shapes {
		out[i] = priced(s, region)
	}
	return out
}

func priced(s InstanceShape, region Region) InstanceShape {
	if region.Name == "" {
		reason := region.Reason
		if reason == "" {
			reason = "not read"
		}
		s.PriceNote = fmt.Sprintf("no on-demand price: the cluster's region is not known (%s)", reason)
		return s
	}
	table, ok := onDemandPrices[region.Name]
	if !ok {
		s.PriceNote = fmt.Sprintf("no on-demand price: the price table has no figures for region %s (it covers %s); a price from another region is not guessed", region.Name, strings.Join(PriceRegions(), ", "))
		return s
	}
	location := regionLocations[region.Name]
	price, ok := table[s.InstanceType]
	if !ok {
		s.PriceNote = fmt.Sprintf("no on-demand price: AWS lists no on-demand %s in %s (%s) — the size is not offered there", s.InstanceType, location, region.Name)
		return s
	}
	s.PricePerHourUSD = &price
	s.PriceSource = fmt.Sprintf("AWS EC2 on-demand Linux list price, %s (%s)", location, region.Name)
	s.PriceAsOf = PriceAsOf
	return s
}

// The gp3 tier's baseline: what a volume gets at the storage price alone.
// Provisioned throughput above it and IOPS above it are billed on top.
const (
	GP3BaselineThroughputMiBps int64 = 125
	GP3BaselineIOPS            int64 = 3000
	// gp3VolumeType is the EBS volume type the price table prices; the model
	// cache's StorageClass renders it by default.
	gp3VolumeType = "gp3"
)

// gp3Price is what a gp3 volume is billed per month in one region
// (prices_table.go).
type gp3Price struct {
	storageGiBMonth      float64
	throughputMiBpsMonth float64
	iopsMonth            float64
}

// VolumeTier is a volume's provisioned tier as its StorageClass parameters
// state it (the EBS CSI driver's `type`, `iops`, `throughput`).
type VolumeTier struct {
	Type            string `json:"type,omitempty"`
	IOPS            int64  `json:"iops,omitempty"`
	ThroughputMiBps int64  `json:"throughputMiBps,omitempty"`
}

// String words the tier: `gp3, 500 MiB/s, 3000 IOPS`.
func (t VolumeTier) String() string {
	parts := []string{t.Type}
	if t.ThroughputMiBps > 0 {
		parts = append(parts, fmt.Sprintf("%d MiB/s", t.ThroughputMiBps))
	}
	if t.IOPS > 0 {
		parts = append(parts, fmt.Sprintf("%d IOPS", t.IOPS))
	}
	return strings.Join(parts, ", ")
}

// ClaimPrice is what a model cache claim's volume is billed per month at
// AWS's list prices: the figure, the list it came from and the day it was
// read.
type ClaimPrice struct {
	MonthlyUSD float64 `json:"monthlyUSD"`
	Source     string  `json:"source"`
	AsOf       string  `json:"asOf"`
}

// PriceClaim prices a gp3 volume of capacityGiB at tier in the region:
// storage per GiB-month, provisioned throughput above the tier's baseline
// per MiB/s-month, provisioned IOPS above the baseline per IOPS-month — the
// list prices of the region, rounded to the cent. A region the table does
// not cover, one that could not be read, a volume type other than gp3 or an
// unknown capacity is no price and a note saying why — never a figure from
// another region or another tier. A tier without IOPS or throughput is the
// baseline's: what the EBS CSI driver provisions when the class names none.
func PriceClaim(region Region, capacityGiB float64, tier VolumeTier) (*ClaimPrice, string) {
	switch {
	case region.Name == "":
		reason := region.Reason
		if reason == "" {
			reason = "not read"
		}
		return nil, fmt.Sprintf("no price: the cluster's region is not known (%s)", reason)
	case capacityGiB <= 0:
		return nil, "no price: the claim's capacity is not known"
	case tier.Type == "":
		return nil, "no price: the volume's tier is not known (its StorageClass could not be read), and a gp3 volume is billed for its provisioned throughput and IOPS beside its storage"
	case tier.Type != gp3VolumeType:
		return nil, fmt.Sprintf("no price: the price table prices %s volumes only, and this volume is %s", gp3VolumeType, tier.Type)
	}
	p, ok := ebsGP3Prices[region.Name]
	if !ok {
		return nil, fmt.Sprintf("no price: the price table has no EBS figures for region %s (it covers %s); a price from another region is not guessed", region.Name, strings.Join(PriceRegions(), ", "))
	}
	monthly := capacityGiB * p.storageGiBMonth
	if extra := tier.ThroughputMiBps - GP3BaselineThroughputMiBps; extra > 0 {
		monthly += float64(extra) * p.throughputMiBpsMonth
	}
	if extra := tier.IOPS - GP3BaselineIOPS; extra > 0 {
		monthly += float64(extra) * p.iopsMonth
	}
	return &ClaimPrice{
		MonthlyUSD: math.Round(monthly*100) / 100,
		Source:     fmt.Sprintf("AWS EBS gp3 list price, %s (%s)", regionLocations[region.Name], region.Name),
		AsOf:       PriceAsOf,
	}, ""
}
