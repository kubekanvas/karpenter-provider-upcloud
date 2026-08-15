/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package instancetype

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/mitchellh/hashstructure/v2"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/utils/resources"

	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/apis/v1alpha1"
	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/operator/options"
)

const (
	memoryAvailable = "memory.available"

	// defaultMaxPods matches the kubelet default. UpCloud imposes no per-server pod or ENI limit of
	// its own, so unlike on AWS this is a plain constant rather than a per-plan lookup.
	defaultMaxPods = 110

	// osReservedStorageGB is carved out of the root disk before it is advertised as
	// ephemeral-storage. It covers the template's own image plus room for logs and the container
	// runtime's own metadata.
	osReservedStorageGB = 4

	// familyGeneral is the label value for plans whose name carries no family prefix, e.g.
	// "2xCPU-4GB". UpCloud calls these "general purpose".
	familyGeneral = "general"

	// nvidiaGPUResource is the extended resource the NVIDIA device plugin advertises. UpCloud's GPU
	// plans are NVIDIA-only, so GPU capacity is reported under this name.
	nvidiaGPUResource corev1.ResourceName = "nvidia.com/gpu"
)

// NodeClass is the subset of UpCloudNodeClass that instance type resolution depends on. Keeping it
// an interface lets the resolver be exercised without constructing a full NodeClass.
type NodeClass interface {
	KubeletConfiguration() *v1alpha1.KubeletConfiguration
	ZoneIDs() []string
	TemplateID() string
	RootDiskSizeGB(planStorageGB int) int
}

// Resolver turns a raw UpCloud plan into a Karpenter InstanceType.
type Resolver interface {
	// CacheKey tells the InstanceType cache what about the NodeClass changes the resulting
	// InstanceTypes. Anything Resolve reads off the NodeClass must be reflected here.
	CacheKey(nodeClass NodeClass) string
	// Resolve generates an InstanceType from a plan and the NodeClass settings.
	Resolve(ctx context.Context, plan *upcloud.Plan, nodeClass NodeClass) *cloudprovider.InstanceType
}

type DefaultResolver struct{}

func NewDefaultResolver() *DefaultResolver {
	return &DefaultResolver{}
}

func (d DefaultResolver) CacheKey(nodeClass NodeClass) string {
	// The zone set changes the instance type's requirements; the template changes the root disk
	// size and therefore ephemeral-storage capacity; the kubelet configuration changes both
	// capacity and overhead. Anything Resolve reads has to be reflected here.
	hash, _ := hashstructure.Hash([]interface{}{
		nodeClass.ZoneIDs(),
		nodeClass.TemplateID(),
		nodeClass.KubeletConfiguration(),
	}, hashstructure.FormatV2, &hashstructure.HashOptions{SlicesAsSets: true})
	return fmt.Sprintf("%016x", hash)
}

func (d DefaultResolver) Resolve(ctx context.Context, plan *upcloud.Plan, nodeClass NodeClass) *cloudprovider.InstanceType {
	// !!! Important !!!
	// Everything read off the NodeClass here must also be reflected in CacheKey, or stale
	// InstanceTypes will be served after the NodeClass changes.
	// !!! Important !!!
	kc := &v1alpha1.KubeletConfiguration{}
	if resolved := nodeClass.KubeletConfiguration(); resolved != nil {
		kc = resolved
	}
	return NewInstanceType(ctx, plan, nodeClass.ZoneIDs(), nodeClass.RootDiskSizeGB(plan.StorageSize), kc)
}

func NewInstanceType(
	ctx context.Context,
	plan *upcloud.Plan,
	zones []string,
	rootDiskGB int,
	kc *v1alpha1.KubeletConfiguration,
) *cloudprovider.InstanceType {
	return &cloudprovider.InstanceType{
		Name:         plan.Name,
		Requirements: computeRequirements(plan, zones),
		Capacity:     computeCapacity(ctx, plan, rootDiskGB, kc.MaxPods, kc.PodsPerCore),
		Overhead: &cloudprovider.InstanceTypeOverhead{
			KubeReserved:      kubeReservedResources(cpu(plan), pods(plan, kc.MaxPods, kc.PodsPerCore), kc.KubeReserved),
			SystemReserved:    systemReservedResources(kc.SystemReserved),
			EvictionThreshold: evictionThreshold(memory(ctx, plan), kc.EvictionHard, kc.EvictionSoft),
		},
	}
}

func computeRequirements(plan *upcloud.Plan, zones []string) scheduling.Requirements {
	requirements := scheduling.NewRequirements(
		// Well known upstream.
		scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, plan.Name),
		// UpCloud exposes no grouping above a zone, and its cloud-controller-manager sets both
		// topology labels on the node to the zone id. Mirroring that here keeps NodeClaim labels and
		// Node labels consistent, which is what lets Karpenter register the node.
		scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, zones...),
		scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, zones...),
		// Every UpCloud plan runs on x86-64 Linux, and UpCloud has no spot or preemptible market.
		scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, karpv1.ArchitectureAmd64),
		scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, string(corev1.Linux)),
		scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
		// Well known to UpCloud.
		scheduling.NewRequirement(v1alpha1.LabelInstanceCPU, corev1.NodeSelectorOpIn, fmt.Sprint(plan.CoreNumber)),
		scheduling.NewRequirement(v1alpha1.LabelInstanceMemory, corev1.NodeSelectorOpIn, fmt.Sprint(plan.MemoryAmount)),
		scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpIn, PlanFamily(plan.Name)),
		scheduling.NewRequirement(v1alpha1.LabelInstanceStorageSize, corev1.NodeSelectorOpIn, fmt.Sprint(plan.StorageSize)),
		scheduling.NewRequirement(v1alpha1.LabelInstanceStorageTier, corev1.NodeSelectorOpIn, plan.StorageTier),
		scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpIn, fmt.Sprint(plan.GPUAmount)),
		scheduling.NewRequirement(v1alpha1.LabelInstancePublicTrafficOut, corev1.NodeSelectorOpIn, fmt.Sprint(plan.PublicTrafficOut)),
	)
	// Only GPU plans carry a model. Declaring DoesNotExist on the others keeps a NodePool that
	// selects on the model from matching every CPU plan.
	if plan.GPUModel != "" {
		requirements.Add(scheduling.NewRequirement(v1alpha1.LabelInstanceGPUModel, corev1.NodeSelectorOpIn, plan.GPUModel))
	} else {
		requirements.Add(scheduling.NewRequirement(v1alpha1.LabelInstanceGPUModel, corev1.NodeSelectorOpDoesNotExist))
	}
	return requirements
}

// PlanFamily extracts the family prefix from an UpCloud plan name. Plans are named
// "<FAMILY>-<n>xCPU-<m>GB" with the family omitted for general purpose plans, e.g. "2xCPU-4GB",
// "HIMEM-4xCPU-32GB", "DEV-1xCPU-1GB".
func PlanFamily(name string) string {
	prefix, _, ok := strings.Cut(name, "-")
	if !ok || strings.HasSuffix(prefix, "xCPU") {
		return familyGeneral
	}
	return strings.ToLower(prefix)
}

func computeCapacity(
	ctx context.Context,
	plan *upcloud.Plan,
	rootDiskGB int,
	maxPods, podsPerCore *int32,
) corev1.ResourceList {
	capacity := corev1.ResourceList{
		corev1.ResourceCPU:              *cpu(plan),
		corev1.ResourceMemory:           *memory(ctx, plan),
		corev1.ResourcePods:             *pods(plan, maxPods, podsPerCore),
		corev1.ResourceEphemeralStorage: *ephemeralStorage(rootDiskGB),
	}
	if plan.GPUAmount > 0 {
		capacity[nvidiaGPUResource] = *resources.Quantity(strconv.Itoa(plan.GPUAmount))
	}
	return capacity
}

func cpu(plan *upcloud.Plan) *resource.Quantity {
	return resources.Quantity(strconv.Itoa(plan.CoreNumber))
}

func memory(ctx context.Context, plan *upcloud.Plan) *resource.Quantity {
	// UpCloud reports plan memory in MiB. What the guest kernel actually sees is lower, because the
	// hypervisor and the kernel itself take a cut, so subtract a configurable percentage. The
	// capacity controller replaces this estimate with the real value once a node of this plan has
	// joined the cluster.
	mem := resources.Quantity(fmt.Sprintf("%dMi", plan.MemoryAmount))
	overheadMiB := int64(math.Ceil(float64(mem.Value()) * options.FromContext(ctx).VMMemoryOverheadPercent / 1024 / 1024))
	mem.Sub(resource.MustParse(fmt.Sprintf("%dMi", overheadMiB)))
	return mem
}

func ephemeralStorage(rootDiskGB int) *resource.Quantity {
	usable := rootDiskGB - osReservedStorageGB
	if usable < 1 {
		usable = 1
	}
	return resources.Quantity(fmt.Sprintf("%dG", usable))
}

func pods(plan *upcloud.Plan, maxPods, podsPerCore *int32) *resource.Quantity {
	count := int64(defaultMaxPods)
	if maxPods != nil {
		count = int64(lo.FromPtr(maxPods))
	}
	if lo.FromPtr(podsPerCore) > 0 {
		count = lo.Min([]int64{int64(lo.FromPtr(podsPerCore)) * int64(plan.CoreNumber), count})
	}
	return resources.Quantity(fmt.Sprint(count))
}

func systemReservedResources(systemReserved map[string]string) corev1.ResourceList {
	return lo.MapEntries(systemReserved, func(k string, v string) (corev1.ResourceName, resource.Quantity) {
		return corev1.ResourceName(k), resource.MustParse(v)
	})
}

// kubeReservedResources mirrors the tiered kube-reserved defaults used by the other Karpenter
// providers, which in turn follow the GKE/Bottlerocket formula. UpCloud ships no opinion of its
// own here, so matching the ecosystem default is the least surprising choice.
func kubeReservedResources(cpus, pods *resource.Quantity, kubeReserved map[string]string) corev1.ResourceList {
	resourceList := corev1.ResourceList{
		corev1.ResourceMemory:           resource.MustParse(fmt.Sprintf("%dMi", (11*pods.Value())+255)),
		corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
	}
	for _, cpuRange := range []struct {
		start      int64
		end        int64
		percentage float64
	}{
		{start: 0, end: 1000, percentage: 0.06},
		{start: 1000, end: 2000, percentage: 0.01},
		{start: 2000, end: 4000, percentage: 0.005},
		{start: 4000, end: 1 << 31, percentage: 0.0025},
	} {
		cpuMilli := cpus.MilliValue()
		if cpuMilli < cpuRange.start {
			continue
		}
		r := float64(cpuRange.end - cpuRange.start)
		if cpuMilli < cpuRange.end {
			r = float64(cpuMilli - cpuRange.start)
		}
		cpuOverhead := resourceList.Cpu()
		cpuOverhead.Add(*resource.NewMilliQuantity(int64(r*cpuRange.percentage), resource.DecimalSI))
		resourceList[corev1.ResourceCPU] = *cpuOverhead
	}
	return lo.Assign(resourceList, lo.MapEntries(kubeReserved, func(k string, v string) (corev1.ResourceName, resource.Quantity) {
		return corev1.ResourceName(k), resource.MustParse(v)
	}))
}

func evictionThreshold(memory *resource.Quantity, evictionHard, evictionSoft map[string]string) corev1.ResourceList {
	overhead := corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse("100Mi"),
	}

	override := corev1.ResourceList{}
	var evictionSignals []map[string]string
	if evictionHard != nil {
		evictionSignals = append(evictionSignals, evictionHard)
	}
	if evictionSoft != nil {
		evictionSignals = append(evictionSignals, evictionSoft)
	}
	for _, m := range evictionSignals {
		temp := corev1.ResourceList{}
		if v, ok := m[memoryAvailable]; ok {
			temp[corev1.ResourceMemory] = computeEvictionSignal(*memory, v)
		}
		override = resources.MaxResources(override, temp)
	}
	// Assign merges left to right, so the override always wins.
	return lo.Assign(overhead, override)
}

// computeEvictionSignal resolves an eviction signal against the node's capacity, handling both
// absolute quantities and the percentage form documented at
// https://kubernetes.io/docs/concepts/scheduling-eviction/node-pressure-eviction/#eviction-signals
func computeEvictionSignal(capacity resource.Quantity, signalValue string) resource.Quantity {
	if strings.HasSuffix(signalValue, "%") {
		p := mustParsePercentage(signalValue)
		return resource.MustParse(fmt.Sprint(math.Ceil(capacity.AsApproximateFloat64() / 100 * p)))
	}
	return resource.MustParse(signalValue)
}

func mustParsePercentage(v string) float64 {
	p, err := strconv.ParseFloat(strings.Trim(v, "%"), 64)
	if err != nil {
		panic(fmt.Sprintf("expected percentage value to be a float but got %s, %v", v, err))
	}
	// 100% disables the threshold.
	// https://kubernetes.io/docs/reference/config-api/kubelet-config.v1beta1/
	if p == 100 {
		p = 0
	}
	return p
}
