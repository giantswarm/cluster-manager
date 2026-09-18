package compose

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

// The chart's curated accelerators as EC2 instance families (the gpu-node-pool
// chart's `gpu-node-pool.accelerators` helper), the memory of one GPU of each,
// and the sizes AWS offers in each family. What a pool's sizes leave a
// predictor decides whether a serving preset can ever be scheduled onto the
// pool (giantswarm/agent-platform#502): on gazelle a preset requesting 4 vCPU /
// 16 GiB sat Pending on a pool of `xlarge` nodes while Karpenter refused the
// size, and nothing had said so.
var (
	instanceFamilies = map[string]string{"nvidia-l4": "g6", "nvidia-a10g": "g5", "nvidia-t4": "g4dn", "nvidia-l40s": "g6e"}
	gpuMemoryGiB     = map[string]int{"g6": 24, "g5": 24, "g4dn": 16, "g6e": 48}
	// familySizes lists every size of a family with its nominal vCPU, memory
	// (GiB) and GPUs, smallest first. The G families share their size names,
	// vCPU and GPU counts (gSizes); they differ in the memory per vCPU — 4 GiB
	// on g4dn, g5 and g6, 8 GiB on g6e — and g4dn has no 24xlarge and ends
	// in metal instead of 48xlarge. So g6.xlarge is 4 vCPU / 16 GiB / 1 L4, g6.2xlarge 8 / 32 / 1,
	// g6.12xlarge 48 / 192 / 4, g6e.xlarge 4 / 32 / 1 L40S, g4dn.metal
	// 96 / 384 / 8 T4.
	familySizes = map[string][]nominal{
		"g6":   gFamily(4, false),
		"g5":   gFamily(4, false),
		"g6e":  gFamily(8, false),
		"g4dn": gFamily(4, true),
	}
	// gSizes are the sizes of the G families: one GPU up to 8xlarge and on
	// 16xlarge, four on 12xlarge and 24xlarge, eight on 48xlarge (metal).
	gSizes = []nominal{{"xlarge", 4, 0, 1}, {"2xlarge", 8, 0, 1}, {"4xlarge", 16, 0, 1}, {"8xlarge", 32, 0, 1}, {"12xlarge", 48, 0, 4}, {"16xlarge", 64, 0, 1}, {"24xlarge", 96, 0, 4}, {"48xlarge", 192, 0, 8}}
	// instanceStores is the local NVMe instance store of every size of the
	// curated families as AWS lists it (`aws ec2 describe-instance-types`,
	// InstanceStorageInfo, read 2026-09-18): the devices and the GB of each.
	// Every size has one, which is what lets gpu-node-pool keep a pool
	// node's /var/lib on it (0.7.0, `pool.volumes.libSource: instance-store`)
	// without refusing a size; the chart formats the first device, so a size
	// with two gives /var/lib one of them.
	instanceStores = map[string]instanceStore{
		"g6.xlarge": {1, 250}, "g6.2xlarge": {1, 450}, "g6.4xlarge": {1, 600}, "g6.8xlarge": {2, 450},
		"g6.12xlarge": {4, 940}, "g6.16xlarge": {2, 940}, "g6.24xlarge": {4, 940}, "g6.48xlarge": {8, 940},
		"g6e.xlarge": {1, 250}, "g6e.2xlarge": {1, 450}, "g6e.4xlarge": {1, 600}, "g6e.8xlarge": {2, 450},
		"g6e.12xlarge": {2, 1900}, "g6e.16xlarge": {2, 950}, "g6e.24xlarge": {2, 1900}, "g6e.48xlarge": {4, 1900},
		"g5.xlarge": {1, 250}, "g5.2xlarge": {1, 450}, "g5.4xlarge": {1, 600}, "g5.8xlarge": {1, 900},
		"g5.12xlarge": {1, 3800}, "g5.16xlarge": {1, 1900}, "g5.24xlarge": {1, 3800}, "g5.48xlarge": {2, 3800},
		"g4dn.xlarge": {1, 125}, "g4dn.2xlarge": {1, 225}, "g4dn.4xlarge": {1, 225}, "g4dn.8xlarge": {1, 900},
		"g4dn.12xlarge": {1, 900}, "g4dn.16xlarge": {1, 900}, "g4dn.metal": {2, 900},
	}
)

// instanceStore is one size's local NVMe: disks devices of diskGB each.
type instanceStore struct{ disks, diskGB int }

// gFamily fills gSizes' memory from the family's GiB per vCPU. metal is the
// g4dn shape: no 24xlarge, and a 96 vCPU / 8 GPU bare-metal size in place of
// the 48xlarge.
func gFamily(gibPerVCPU int, metal bool) []nominal {
	out := make([]nominal, 0, len(gSizes))
	for _, n := range gSizes {
		if metal {
			switch n.size {
			case "24xlarge":
				continue
			case "48xlarge":
				n = nominal{"metal", 96, 0, 8}
			}
		}
		n.gib = n.vcpu * gibPerVCPU
		out = append(out, n)
	}
	return out
}

// What a node keeps from its nominal shape before a predictor may have the
// rest — the fleet's shape as measured on gazelle: the hypervisor's ~5 % of
// the memory, the kubelet's reservations (0.6 vCPU and ~1.8 GiB: a 4 vCPU /
// 16 GiB node reports 3.4 vCPU / 13.4 GiB allocatable) and the daemonsets that
// follow a GPU pool's taint (Cilium, the exporters, Alloy, the DNS cache, the
// GPU operator's operands: ~0.4 vCPU / ~1.5 GiB). An estimate, so what it
// rules out is a warning in the answer, never a refusal.
const (
	kubeletReservedVCPU   = 0.6
	daemonSetVCPU         = 0.4
	hypervisorMemoryShare = 0.05
	kubeletReservedGiB    = 1.8
	daemonSetGiB          = 1.5
)

// nominal is one size as AWS lists it.
type nominal struct {
	size            string
	vcpu, gib, gpus int
}

// InstanceShape is one size of a pool's family: the node as AWS lists it and
// what it leaves a predictor.
type InstanceShape struct {
	// InstanceType is `<family>.<size>` (g6.xlarge), Size the size within
	// the family (xlarge).
	InstanceType string `json:"instanceType"`
	Size         string `json:"size"`
	VCPU         int    `json:"vcpu"`
	MemoryGiB    int    `json:"memoryGiB"`
	GPUs         int    `json:"gpus"`
	// GPUMemoryGiB is the memory of one GPU.
	GPUMemoryGiB int `json:"gpuMemoryGiB"`
	// InstanceStoreGB is the node's local NVMe instance store as AWS lists
	// it, InstanceStoreDisks devices of InstanceStoreDiskGB each — local to
	// the host, included in the price, gone with the node. From gpu-node-pool
	// 0.7.0 a pool node's /var/lib (containerd's image unpacks, the kubelet's
	// directories, the pods' emptyDirs and writable layers) is the first
	// device, so InstanceStoreDiskGB is what a node of the size gives its
	// pods as ephemeral storage: 250 GB on a g6.xlarge, 450 of a g6.8xlarge's
	// 2 × 450.
	InstanceStoreGB     int `json:"instanceStoreGB"`
	InstanceStoreDisks  int `json:"instanceStoreDisks"`
	InstanceStoreDiskGB int `json:"instanceStoreDiskGB"`
	// UsableVCPU and UsableMemoryGiB are what a predictor may request on a
	// node of this size once the kubelet's reservations and the fleet's
	// daemonsets have theirs — an estimate of the fleet's shape.
	UsableVCPU      float64 `json:"usableVcpu"`
	UsableMemoryGiB float64 `json:"usableMemoryGiB"`
	// PricePerHourUSD is the node's on-demand Linux list price in the
	// cluster's region (Priced), with PriceSource naming the list and
	// PriceAsOf the day it was read; absent with PriceNote saying why when
	// the region is not known, not in the table, or does not offer the size.
	PricePerHourUSD *float64 `json:"pricePerHourUSD,omitempty"`
	PriceSource     string   `json:"priceSource,omitempty"`
	PriceAsOf       string   `json:"priceAsOf,omitempty"`
	PriceNote       string   `json:"priceNote,omitempty"`
}

func newShape(family string, n nominal) InstanceShape {
	store := instanceStores[family+"."+n.size]
	return InstanceShape{
		InstanceType: family + "." + n.size, Size: n.size,
		VCPU: n.vcpu, MemoryGiB: n.gib, GPUs: n.gpus, GPUMemoryGiB: gpuMemoryGiB[family],
		InstanceStoreGB: store.disks * store.diskGB, InstanceStoreDisks: store.disks, InstanceStoreDiskGB: store.diskGB,
		UsableVCPU:      round1(float64(n.vcpu) - kubeletReservedVCPU - daemonSetVCPU),
		UsableMemoryGiB: round1(float64(n.gib)*(1-hypervisorMemoryShare) - kubeletReservedGiB - daemonSetGiB),
	}
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

// InstanceFamily is the EC2 instance family of an accelerator of the curated
// list; empty for one outside it.
func InstanceFamily(accelerator string) string { return instanceFamilies[accelerator] }

// FamilySizes lists the sizes of an accelerator's family, smallest first;
// nil for an accelerator outside the curated list.
func FamilySizes(accelerator string) []string {
	family := InstanceFamily(accelerator)
	if family == "" {
		return nil
	}
	out := make([]string, 0, len(familySizes[family]))
	for _, n := range familySizes[family] {
		out = append(out, n.size)
	}
	return out
}

// Shapes resolves a pool's sizes (the chart's default when none are given) to
// the shapes of its family, in the pool's order. A size the family does not
// have is an error naming the family's sizes: Karpenter would never launch
// it, and the pool would sit empty for ever.
func Shapes(accelerator string, sizes []string) ([]InstanceShape, error) {
	family := InstanceFamily(accelerator)
	if family == "" {
		return nil, fmt.Errorf("accelerator %q: not in the curated list %v", accelerator, Accelerators)
	}
	if len(sizes) == 0 {
		sizes = DefaultPoolSizes
	}
	out := make([]InstanceShape, 0, len(sizes))
	for _, size := range sizes {
		found := false
		for _, n := range familySizes[family] {
			if n.size == size {
				out = append(out, newShape(family, n))
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("size %q: not a size of the %s family (%s); the sizes are %s", size, family, accelerator, strings.Join(FamilySizes(accelerator), ", "))
		}
	}
	return out, nil
}

// PresetRequests is what a published serving preset asks of the node it is
// served on: the predictor's CPU and memory requests, its GPUs and the GPU
// memory its weights and overhead need (the fit check's GPU-side number).
type PresetRequests struct {
	Name   string
	CPU    resource.Quantity
	Memory resource.Quantity
	GPUs   int
	// GPUMemoryGiB is weightsGiB + overheadGiB across the preset's GPUs.
	GPUMemoryGiB float64
}

// SizeFit places one preset against a pool's sizes: Size is the smallest of
// them that hosts it, empty with Reason when none does. Hostable says whether
// a size of the family would — the pool merely lacks it, which the reason
// names — as opposed to an accelerator the preset can never be served on (too
// little GPU memory, too few GPUs on any size).
type SizeFit struct {
	Preset   string
	Size     string
	Reason   string
	Hostable bool
}

// Fit is the smallest of the shapes that hosts the preset, else why none does.
func Fit(shapes []InstanceShape, p PresetRequests) SizeFit {
	if len(shapes) == 0 {
		return SizeFit{Preset: p.Name, Reason: "the pool has no sizes"}
	}
	sorted := make([]InstanceShape, len(shapes))
	copy(sorted, shapes)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].VCPU < sorted[j].VCPU })
	for _, s := range sorted {
		if hosts(s, p) {
			return SizeFit{Preset: p.Name, Size: s.Size, Hostable: true}
		}
	}
	largest := sorted[len(sorted)-1]
	family, _, _ := strings.Cut(largest.InstanceType, ".")
	gpus := max(p.GPUs, 1)
	switch {
	case p.GPUMemoryGiB > float64(largest.GPUMemoryGiB*gpus):
		return SizeFit{Preset: p.Name, Reason: fmt.Sprintf("needs %s GiB of GPU memory across %d GPU(s); a %s GPU has %d GiB", trim(p.GPUMemoryGiB), gpus, family, largest.GPUMemoryGiB)}
	case p.GPUs > largest.GPUs:
		reason := fmt.Sprintf("needs %d GPUs; %s, the pool's largest size, carries %d", p.GPUs, largest.Size, largest.GPUs)
		if s, ok := familyHost(family, p); ok {
			return SizeFit{Preset: p.Name, Reason: fmt.Sprintf("%s — %s (%d GPUs, %d vCPU / %d GiB) would host it", reason, s.Size, s.GPUs, s.VCPU, s.MemoryGiB), Hostable: true}
		}
		return SizeFit{Preset: p.Name, Reason: reason + "; no size of the family carries that many"}
	default:
		reason := fmt.Sprintf("requests %s vCPU / %s GiB; %s leaves a predictor %s vCPU / %s GiB after the node's kubelet reservations and daemonsets",
			trim(vcpuOf(p.CPU)), trim(gibOf(p.Memory)), largest.Size, trim(largest.UsableVCPU), trim(largest.UsableMemoryGiB))
		if s, ok := familyHost(family, p); ok {
			return SizeFit{Preset: p.Name, Reason: fmt.Sprintf("%s — %s (%d vCPU / %d GiB) would host it", reason, s.Size, s.VCPU, s.MemoryGiB), Hostable: true}
		}
		return SizeFit{Preset: p.Name, Reason: reason + "; no size of the family leaves enough"}
	}
}

// hosts reports whether a node of shape s can run the preset's predictor.
func hosts(s InstanceShape, p PresetRequests) bool {
	gpus := max(p.GPUs, 1)
	return vcpuOf(p.CPU) <= s.UsableVCPU && gibOf(p.Memory) <= s.UsableMemoryGiB &&
		p.GPUs <= s.GPUs && p.GPUMemoryGiB <= float64(s.GPUMemoryGiB*gpus)
}

// familyHost is the smallest size of the family that hosts the preset.
func familyHost(family string, p PresetRequests) (InstanceShape, bool) {
	for _, n := range familySizes[family] {
		if s := newShape(family, n); hosts(s, p) {
			return s, true
		}
	}
	return InstanceShape{}, false
}

func vcpuOf(q resource.Quantity) float64 { return float64(q.MilliValue()) / 1000 }

func gibOf(q resource.Quantity) float64 { return float64(q.Value()) / (1 << 30) }

// trim prints a number without a trailing .0, to one decimal otherwise.
func trim(f float64) string { return fmt.Sprintf("%g", round1(f)) }
