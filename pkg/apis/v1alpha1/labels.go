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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/kubekanvas/karpenter-upcloud/pkg/apis"
)

func init() {
	// UpCloud has no notion of a region that is distinct from a zone: a zone id such as
	// "fi-hel1" is the smallest and the largest unit of placement the API exposes, and the
	// UpCloud cloud-controller-manager labels nodes with the zone id for both topology keys.
	// We therefore keep both well known topology labels and set them to the same value.
	//
	// Windows is never a possibility on UpCloud-provisioned Karpenter nodes, so drop the
	// Windows build label from the well known set to keep scheduling requirements minimal.
	unused := []string{
		corev1.LabelWindowsBuild,
	}
	karpv1.RestrictedLabelDomains = karpv1.RestrictedLabelDomains.Insert(RestrictedLabelDomains...)
	karpv1.WellKnownLabels = karpv1.WellKnownLabels.Union(UpCloudWellKnownLabels)
	karpv1.WellKnownLabels = karpv1.WellKnownLabels.Delete(unused...)
}

var (
	RestrictedLabelDomains = []string{
		apis.Group,
	}

	// UpCloudWellKnownLabels belong to RestrictedLabelDomains but are allowed: Karpenter is aware
	// of them, so NodePools and pods can narrow scheduling down by any of these dimensions.
	UpCloudWellKnownLabels = sets.New(
		LabelInstanceCPU,
		LabelInstanceMemory,
		LabelInstanceFamily,
		LabelInstanceStorageSize,
		LabelInstanceStorageTier,
		LabelInstanceGPUCount,
		LabelInstanceGPUModel,
		LabelInstancePublicTrafficOut,
	)
)

const (
	// LabelNodeClass is set on every provisioned server so that servers can be mapped back to the
	// UpCloudNodeClass that launched them without consulting the Kubernetes API.
	LabelNodeClass = apis.Group + "/nodeclass"

	LabelInstanceCPU              = apis.Group + "/instance-cpu"
	LabelInstanceMemory           = apis.Group + "/instance-memory"
	LabelInstanceFamily           = apis.Group + "/instance-family"
	LabelInstanceStorageSize      = apis.Group + "/instance-storage-size"
	LabelInstanceStorageTier      = apis.Group + "/instance-storage-tier"
	LabelInstanceGPUCount         = apis.Group + "/instance-gpu-count"
	LabelInstanceGPUModel         = apis.Group + "/instance-gpu-model"
	LabelInstancePublicTrafficOut = apis.Group + "/instance-public-traffic-out"

	AnnotationUpCloudNodeClassHash        = apis.Group + "/upcloudnodeclass-hash"
	AnnotationUpCloudNodeClassHashVersion = apis.Group + "/upcloudnodeclass-hash-version"

	// TerminationFinalizer is placed on UpCloudNodeClasses so that the class cannot disappear while
	// servers launched from it are still running.
	TerminationFinalizer = apis.Group + "/termination"
)

// UpCloud server label keys used for resource discovery. UpCloud restricts label keys to 2-32
// printable ASCII characters, which every key below satisfies. Changing any of them is a breaking
// change: previously launched servers would stop being discovered and would leak.
const (
	// LabelKeyManagedBy holds the cluster name, mirroring the semantics of karpenter.sh/managed-by
	// on other providers. It is the key the instance provider filters on when listing servers.
	LabelKeyManagedBy = "karpenter.sh/managed-by"
	// LabelKeyNodePool holds the owning NodePool name.
	LabelKeyNodePool = karpv1.NodePoolLabelKey
	// LabelKeyNodeClaim holds the owning NodeClaim name.
	LabelKeyNodeClaim = "karpenter.sh/nodeclaim"
	// LabelKeyNodeClass holds the owning UpCloudNodeClass name.
	LabelKeyNodeClass = LabelNodeClass
	// LabelKeyCreatedAt holds the launch time as Unix seconds.
	//
	// UpCloud's API reports no creation timestamp for a server, and garbage collection needs one:
	// without it, a server launched moments ago — whose NodeClaim the controller has not observed
	// yet — is indistinguishable from an orphan and would be reaped immediately.
	LabelKeyCreatedAt = "karpenter.sh/created-at"
)
