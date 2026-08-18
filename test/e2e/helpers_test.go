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

//go:build e2e

package e2e

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/apis/v1alpha1"
	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/providers/instancetype"
)

// e2eInstanceType builds the minimum instance type the instance provider needs: a name, the plan's
// bundled storage size, and one available offering in the target zone. The real instance type
// provider is deliberately not used here — these tests exercise the launch path, not catalogue
// resolution, which the unit tests already cover.
func e2eInstanceType(plan, zone string) *cloudprovider.InstanceType {
	return &cloudprovider.InstanceType{
		Name: plan,
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, plan),
			// 0 exercises the zero-storage path that 102 of UpCloud's 174 plans take, so the disk is
			// sized from the NodeClass default rather than the plan.
			scheduling.NewRequirement(v1alpha1.LabelInstanceStorageSize, corev1.NodeSelectorOpIn, "0"),
			scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn,
				instancetype.PlanCapacityType(plan)),
		),
		Offerings: cloudprovider.Offerings{{
			Requirements: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, zone),
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn,
					instancetype.PlanCapacityType(plan)),
			),
			Price:     1,
			Available: true,
		}},
		Capacity: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
			corev1.ResourcePods:   resource.MustParse("110"),
		},
		Overhead: &cloudprovider.InstanceTypeOverhead{},
	}
}
