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

package instancetype_test

import (
	"context"
	"testing"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/apis/v1alpha1"
	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/operator/options"
	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/providers/instancetype"
)

func TestPlanFamily(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		plan string
		want string
	}{
		{plan: "1xCPU-1GB", want: "general"},
		{plan: "2xCPU-4GB", want: "general"},
		{plan: "DEV-1xCPU-1GB", want: "dev"},
		{plan: "HICPU-8xCPU-16GB", want: "hicpu"},
		{plan: "HIMEM-4xCPU-32GB", want: "himem"},
		{plan: "CLOUDNATIVE-2xCPU-4GB", want: "cloudnative"},
		{plan: "GPU-8xCPU-64GB-1xL40S", want: "gpu"},
		{plan: "weird", want: "general"},
	} {
		t.Run(tc.plan, func(t *testing.T) {
			t.Parallel()
			if got := instancetype.PlanFamily(tc.plan); got != tc.want {
				t.Errorf("PlanFamily(%q) = %q, want %q", tc.plan, got, tc.want)
			}
		})
	}
}

func testContext() context.Context {
	return options.ToContext(context.Background(), &options.Options{VMMemoryOverheadPercent: 0.075})
}

func TestNewInstanceTypeRequirements(t *testing.T) {
	t.Parallel()

	plan := &upcloud.Plan{
		Name:             "HIMEM-4xCPU-32GB",
		CoreNumber:       4,
		MemoryAmount:     32768,
		StorageSize:      100,
		StorageTier:      "maxiops",
		PublicTrafficOut: 4096,
	}
	it := instancetype.NewInstanceType(testContext(), plan, []string{"fi-hel1", "de-fra1"}, 100, &v1alpha1.KubeletConfiguration{})

	if it.Name != plan.Name {
		t.Errorf("Name = %q, want %q", it.Name, plan.Name)
	}
	// Zone and region must carry the same values: the UpCloud cloud-controller-manager labels nodes
	// that way, and a mismatch would stop NodeClaims from ever matching their Node.
	zones := it.Requirements.Get(corev1.LabelTopologyZone).Values()
	regions := it.Requirements.Get(corev1.LabelTopologyRegion).Values()
	if len(zones) != 2 || len(regions) != 2 {
		t.Fatalf("zones = %v, regions = %v, want two of each", zones, regions)
	}
	for _, zone := range zones {
		if !it.Requirements.Get(corev1.LabelTopologyRegion).Has(zone) {
			t.Errorf("region requirement is missing zone %q", zone)
		}
	}
	if got := it.Requirements.Get(karpv1.CapacityTypeLabelKey).Any(); got != karpv1.CapacityTypeOnDemand {
		t.Errorf("capacity type = %q, want %q (UpCloud sells no interruptible capacity)", got, karpv1.CapacityTypeOnDemand)
	}
	if got := it.Requirements.Get(v1alpha1.LabelInstanceFamily).Any(); got != "himem" {
		t.Errorf("instance family = %q, want %q", got, "himem")
	}
	// A non-GPU plan must not match a NodePool selecting on a GPU model.
	if got := it.Requirements.Get(v1alpha1.LabelInstanceGPUModel).Operator(); got != corev1.NodeSelectorOpDoesNotExist {
		t.Errorf("gpu model requirement = %v, want DoesNotExist for a CPU plan", got)
	}
}

func TestNewInstanceTypeCapacity(t *testing.T) {
	t.Parallel()

	plan := &upcloud.Plan{Name: "2xCPU-4GB", CoreNumber: 2, MemoryAmount: 4096, StorageSize: 80, StorageTier: "maxiops"}
	it := instancetype.NewInstanceType(testContext(), plan, []string{"fi-hel1"}, 80, &v1alpha1.KubeletConfiguration{})

	if got := it.Capacity.Cpu().Value(); got != 2 {
		t.Errorf("cpu = %d, want 2", got)
	}
	// Memory is reported below the plan's nominal figure, because the guest never sees all of it.
	nominal := int64(4096) * 1024 * 1024
	if got := it.Capacity.Memory().Value(); got >= nominal {
		t.Errorf("memory = %d, want less than the nominal %d after virtualisation overhead", got, nominal)
	}
	if got := it.Capacity.Pods().Value(); got != 110 {
		t.Errorf("pods = %d, want the kubelet default of 110", got)
	}
	// Ephemeral storage is the root disk minus room for the image itself.
	ephemeral := it.Capacity[corev1.ResourceEphemeralStorage]
	if ephemeral.IsZero() || ephemeral.Value() >= 80*1000*1000*1000 {
		t.Errorf("ephemeral-storage = %s, want a non-zero value below the 80G root disk", ephemeral.String())
	}
	if _, ok := it.Capacity["nvidia.com/gpu"]; ok {
		t.Error("a CPU plan should not advertise GPU capacity")
	}
}

func TestNewInstanceTypeGPUCapacity(t *testing.T) {
	t.Parallel()

	plan := &upcloud.Plan{
		Name: "GPU-8xCPU-64GB-1xL40S", CoreNumber: 8, MemoryAmount: 65536,
		StorageSize: 300, StorageTier: "maxiops", GPUAmount: 1, GPUModel: "L40S",
	}
	it := instancetype.NewInstanceType(testContext(), plan, []string{"fi-hel1"}, 300, &v1alpha1.KubeletConfiguration{})

	gpus, ok := it.Capacity["nvidia.com/gpu"]
	if !ok || gpus.Value() != 1 {
		t.Errorf("nvidia.com/gpu = %v (present=%v), want 1", gpus, ok)
	}
	if got := it.Requirements.Get(v1alpha1.LabelInstanceGPUModel).Any(); got != "L40S" {
		t.Errorf("gpu model = %q, want %q", got, "L40S")
	}
}

func TestNewInstanceTypeMaxPodsOverride(t *testing.T) {
	t.Parallel()

	plan := &upcloud.Plan{Name: "2xCPU-4GB", CoreNumber: 2, MemoryAmount: 4096, StorageSize: 80, StorageTier: "maxiops"}
	maxPods := int32(58)
	podsPerCore := int32(20)
	it := instancetype.NewInstanceType(testContext(), plan, []string{"fi-hel1"}, 80, &v1alpha1.KubeletConfiguration{
		MaxPods:     &maxPods,
		PodsPerCore: &podsPerCore,
	})

	// podsPerCore * cores is 40, which is below maxPods, so it wins.
	if got := it.Capacity.Pods().Value(); got != 40 {
		t.Errorf("pods = %d, want 40 (podsPerCore x cores, capped by maxPods)", got)
	}
}
