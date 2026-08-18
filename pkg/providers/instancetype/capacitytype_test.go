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
	"testing"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/apis/v1alpha1"
	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/providers/instancetype"
)

func TestPlanCapacityType(t *testing.T) {
	t.Parallel()

	// UpCloud publishes 174 plans, 30 of them spot, and encodes it only in the name.
	for _, tc := range []struct {
		plan string
		want string
	}{
		{plan: "GPU-SPOT-8xCPU-64GB-1xL4", want: karpv1.CapacityTypeSpot},
		{plan: "GPU-SPOT-12xCPU-240GB-1xH100", want: karpv1.CapacityTypeSpot},
		{plan: "GPU-8xCPU-64GB-1xL40S", want: karpv1.CapacityTypeOnDemand},
		{plan: "2xCPU-4GB", want: karpv1.CapacityTypeOnDemand},
		{plan: "CLOUDNATIVE-2xCPU-4GB", want: karpv1.CapacityTypeOnDemand},
		{plan: "HIMEM-4xCPU-32GB", want: karpv1.CapacityTypeOnDemand},
		// Matched as a segment, so a plan merely containing the letters is not misread as
		// interruptible capacity.
		{plan: "SPOTLIGHT-2xCPU-4GB", want: karpv1.CapacityTypeOnDemand},
	} {
		t.Run(tc.plan, func(t *testing.T) {
			t.Parallel()
			if got := instancetype.PlanCapacityType(tc.plan); got != tc.want {
				t.Errorf("PlanCapacityType(%q) = %q, want %q", tc.plan, got, tc.want)
			}
		})
	}
}

func TestSpotPlanSurfacesAsSpotRequirement(t *testing.T) {
	t.Parallel()

	spot := instancetype.NewInstanceType(testContext(),
		&upcloud.Plan{Name: "GPU-SPOT-8xCPU-64GB-1xL4", CoreNumber: 8, MemoryAmount: 65536,
			StorageSize: 0, StorageTier: "maxiops", GPUAmount: 1, GPUModel: "L4"},
		[]string{"de-fra1"}, 50, &v1alpha1.KubeletConfiguration{})

	if got := spot.Requirements.Get(karpv1.CapacityTypeLabelKey).Any(); got != karpv1.CapacityTypeSpot {
		t.Errorf("capacity type = %q, want %q — a NodePool cannot avoid spot capacity otherwise", got, karpv1.CapacityTypeSpot)
	}
	// The family must still read as gpu; spot is orthogonal to it.
	if got := spot.Requirements.Get(v1alpha1.LabelInstanceFamily).Any(); got != "gpu" {
		t.Errorf("instance family = %q, want %q", got, "gpu")
	}
}

func TestRootDiskDefaultsWhenPlanBundlesNoStorage(t *testing.T) {
	t.Parallel()

	nodeClass := &v1alpha1.UpCloudNodeClass{
		Status: v1alpha1.UpCloudNodeClassStatus{
			StorageTemplate: &v1alpha1.StorageTemplate{UUID: "tmpl", Size: 10},
		},
	}

	// 102 of 174 plans report 0 here, meaning "bundles no storage" rather than "wants no disk".
	if got := nodeClass.RootDiskSizeGB(0); got != v1alpha1.DefaultRootDiskGB {
		t.Errorf("RootDiskSizeGB(0) = %d, want the %dGB default", got, v1alpha1.DefaultRootDiskGB)
	}
	// A plan that does bundle storage keeps its own allowance.
	if got := nodeClass.RootDiskSizeGB(80); got != 80 {
		t.Errorf("RootDiskSizeGB(80) = %d, want 80", got)
	}
	// The template floor still wins, because a clone cannot shrink below its source.
	big := &v1alpha1.UpCloudNodeClass{
		Status: v1alpha1.UpCloudNodeClassStatus{
			StorageTemplate: &v1alpha1.StorageTemplate{UUID: "tmpl", Size: 200},
		},
	}
	if got := big.RootDiskSizeGB(80); got != 200 {
		t.Errorf("RootDiskSizeGB(80) with a 200GB template = %d, want 200", got)
	}
}
