package compose

import (
	"fmt"
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
