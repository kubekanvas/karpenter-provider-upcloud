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
	"fmt"

	"github.com/awslabs/operatorpkg/status"
	"github.com/mitchellh/hashstructure/v2"
	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// UpCloudNodeClassSpec is the top level specification for the UpCloud Karpenter provider. It holds
// everything Karpenter needs to turn a NodeClaim into an UpCloud cloud server.
type UpCloudNodeClassSpec struct {
	// zones restricts the UpCloud zones that servers may be launched into, e.g. ["fi-hel1", "de-fra1"].
	// When empty, the zone the Karpenter controller was configured with is the only candidate.
	// UpCloud has no region concept above the zone, so a NodePool spanning zones spans datacenters.
	// +optional
	// +listType=set
	Zones []string `json:"zones,omitempty"`

	// storage configures the root disk cloned onto every server from a public or private template.
	// +required
	Storage StorageSpec `json:"storage"`

	// userData is cloud-init user data made available to the server through the UpCloud metadata
	// service. This is where the node is joined to the cluster: without it a server boots but never
	// registers, and Karpenter will garbage collect it after the registration TTL.
	// +optional
	UserData *string `json:"userData,omitempty"`

	// loginUser configures the initial account created on the server. SSH keys are strongly
	// preferred over passwords; UpCloud requires at least one of the two for most templates.
	// +optional
	LoginUser *LoginUserSpec `json:"loginUser,omitempty"`

	// network describes the interfaces attached to the server, in order. When empty a single
	// public IPv4 interface plus the utility network interface are attached, which matches the
	// UpCloud console default.
	// +optional
	Network *NetworkSpec `json:"network,omitempty"`

	// serverGroup is the UUID of an UpCloud server group to place servers in. Use a group with an
	// anti-affinity policy to spread nodes across UpCloud hosts.
	// +optional
	ServerGroup *string `json:"serverGroup,omitempty"`

	// labels are additional UpCloud labels applied to every server. Keys must be 2-32 printable
	// ASCII characters and must not collide with the keys Karpenter manages.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// firewall enables the UpCloud network firewall on the server. Defaults to false, since the
	// firewall's default policy would otherwise have to be managed outside of this NodeClass.
	// +optional
	Firewall *bool `json:"firewall,omitempty"`

	// metadata controls the UpCloud metadata service, which is what serves cloud-init user data.
	// It is forced on whenever userData is set, because cloud-init cannot read it otherwise.
	// +optional
	Metadata *bool `json:"metadata,omitempty"`

	// simpleBackup enables UpCloud simple backups on the root disk, e.g. "0100,daily". Node disks
	// are usually disposable, so this is off by default.
	// +optional
	SimpleBackup *string `json:"simpleBackup,omitempty"`

	// timeZone is the server's hardware clock timezone, e.g. "UTC".
	// +optional
	TimeZone *string `json:"timeZone,omitempty"`

	// hostnamePrefix is prepended to the generated hostname of every server. UpCloud hostnames are
	// limited to 128 characters and the generated suffix takes 40 of them.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	HostnamePrefix *string `json:"hostnamePrefix,omitempty"`

	// kubelet defines args used when configuring kubelet on provisioned nodes. They are a subset of
	// the upstream types, recognizing not all options may be supported. Karpenter only uses these
	// to compute allocatable capacity; applying them to the node itself is the responsibility of
	// the bootstrap script in userData.
	// +kubebuilder:validation:XValidation:message="evictionSoft OwnerKey does not have a matching evictionSoftGracePeriod",rule="has(self.evictionSoft) ? self.evictionSoft.all(e, (e in self.evictionSoftGracePeriod)):true"
	// +kubebuilder:validation:XValidation:message="evictionSoftGracePeriod OwnerKey does not have a matching evictionSoft",rule="has(self.evictionSoftGracePeriod) ? self.evictionSoftGracePeriod.all(e, (e in self.evictionSoft)):true"
	// +optional
	Kubelet *KubeletConfiguration `json:"kubelet,omitempty"`
}

// StorageSpec describes the root disk of a provisioned server.
type StorageSpec struct {
	// template is the UUID of the storage template to clone, e.g. the public Ubuntu 24.04 template.
	// Public template UUIDs are stable and listed by `upctl storage list --public --template`.
	// +kubebuilder:validation:MinLength=1
	// +required
	Template string `json:"template"`

	// size is the root disk size in gigabytes. It must be at least as large as the template and no
	// larger than the plan allows. When unset, the plan's included storage size is used.
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=4096
	// +optional
	Size *int `json:"size,omitempty"`

	// tier is the storage tier of the root disk.
	// +kubebuilder:validation:Enum=maxiops;standard;hdd
	// +optional
	Tier *string `json:"tier,omitempty"`

	// encrypted enables at-rest encryption on the root disk.
	// +optional
	Encrypted *bool `json:"encrypted,omitempty"`
}

// LoginUserSpec describes the account created on a provisioned server.
type LoginUserSpec struct {
	// username of the account to create. Defaults to the template's default user.
	// +optional
	Username *string `json:"username,omitempty"`

	// sshKeys are the public keys authorized for the login user.
	// +optional
	// +listType=set
	SSHKeys []string `json:"sshKeys,omitempty"`
}

// NetworkSpec describes the interfaces attached to a provisioned server.
type NetworkSpec struct {
	// interfaces attached to the server, in the order given. Exactly one interface should carry the
	// address the kubelet registers with; on UpCloud that is conventionally the utility or an SDN
	// interface, since the cloud-controller-manager requires a private address on every node.
	// +optional
	// +listType=atomic
	Interfaces []NetworkInterface `json:"interfaces,omitempty"`
}

// NetworkInterface is a single network interface on a provisioned server.
type NetworkInterface struct {
	// type of the interface.
	// +kubebuilder:validation:Enum=public;utility;private
	// +required
	Type string `json:"type"`

	// network is the UUID of the SDN private network to attach to. Required for private
	// interfaces and ignored otherwise.
	// +optional
	Network *string `json:"network,omitempty"`

	// ipFamilies requested on the interface. Public interfaces default to IPv4 only; utility and
	// private interfaces are always IPv4.
	// +optional
	// +listType=set
	// +kubebuilder:validation:items:Enum=IPv4;IPv6
	IPFamilies []string `json:"ipFamilies,omitempty"`

	// sourceIPFiltering drops traffic whose source address is not assigned to the interface.
	// +optional
	SourceIPFiltering *bool `json:"sourceIPFiltering,omitempty"`
}

// KubeletConfiguration defines args to be used when configuring kubelet on provisioned nodes.
// They are a subset of the upstream types, recognizing not all options may be supported.
// Wherever possible, the types and names should reflect the upstream kubelet types.
// https://pkg.go.dev/k8s.io/kubelet/config/v1beta1#KubeletConfiguration
type KubeletConfiguration struct {
	// clusterDNS is a list of IP addresses for the cluster DNS server.
	// +optional
	ClusterDNS []string `json:"clusterDNS,omitempty"`
	// maxPods is an override for the maximum number of pods that can run on a worker node instance.
	// +kubebuilder:validation:Minimum:=0
	// +optional
	MaxPods *int32 `json:"maxPods,omitempty"`
	// podsPerCore is an override for the number of pods that can run on a worker node instance based
	// on the number of cpu cores. This value cannot exceed maxPods, so, if maxPods is a lower value,
	// that value will be used.
	// +kubebuilder:validation:Minimum:=0
	// +optional
	PodsPerCore *int32 `json:"podsPerCore,omitempty"`
	// systemReserved contains resources reserved for OS system daemons and kernel memory.
	// +kubebuilder:validation:XValidation:message="valid keys for systemReserved are ['cpu','memory','ephemeral-storage','pid']",rule="self.all(x, x=='cpu' || x=='memory' || x=='ephemeral-storage' || x=='pid')"
	// +kubebuilder:validation:XValidation:message="systemReserved value cannot be a negative resource quantity",rule="self.all(x, !self[x].startsWith('-'))"
	// +optional
	SystemReserved map[string]string `json:"systemReserved,omitempty"`
	// kubeReserved contains resources reserved for Kubernetes system components.
	// +kubebuilder:validation:XValidation:message="valid keys for kubeReserved are ['cpu','memory','ephemeral-storage','pid']",rule="self.all(x, x=='cpu' || x=='memory' || x=='ephemeral-storage' || x=='pid')"
	// +kubebuilder:validation:XValidation:message="kubeReserved value cannot be a negative resource quantity",rule="self.all(x, !self[x].startsWith('-'))"
	// +optional
	KubeReserved map[string]string `json:"kubeReserved,omitempty"`
	// evictionHard is the map of signal names to quantities that define hard eviction thresholds.
	// +kubebuilder:validation:XValidation:message="valid keys for evictionHard are ['memory.available','nodefs.available','nodefs.inodesFree','imagefs.available','imagefs.inodesFree','pid.available']",rule="self.all(x, x in ['memory.available','nodefs.available','nodefs.inodesFree','imagefs.available','imagefs.inodesFree','pid.available'])"
	// +optional
	EvictionHard map[string]string `json:"evictionHard,omitempty"`
	// evictionSoft is the map of signal names to quantities that define soft eviction thresholds.
	// +kubebuilder:validation:XValidation:message="valid keys for evictionSoft are ['memory.available','nodefs.available','nodefs.inodesFree','imagefs.available','imagefs.inodesFree','pid.available']",rule="self.all(x, x in ['memory.available','nodefs.available','nodefs.inodesFree','imagefs.available','imagefs.inodesFree','pid.available'])"
	// +optional
	EvictionSoft map[string]string `json:"evictionSoft,omitempty"`
	// evictionSoftGracePeriod is the map of signal names to quantities that define grace periods for
	// each eviction signal.
	// +kubebuilder:validation:XValidation:message="valid keys for evictionSoftGracePeriod are ['memory.available','nodefs.available','nodefs.inodesFree','imagefs.available','imagefs.inodesFree','pid.available']",rule="self.all(x, x in ['memory.available','nodefs.available','nodefs.inodesFree','imagefs.available','imagefs.inodesFree','pid.available'])"
	// +optional
	EvictionSoftGracePeriod map[string]metav1.Duration `json:"evictionSoftGracePeriod,omitempty"`
	// evictionMaxPodGracePeriod is the maximum allowed grace period (in seconds) to use when
	// terminating pods in response to soft eviction thresholds being met.
	// +optional
	EvictionMaxPodGracePeriod *int32 `json:"evictionMaxPodGracePeriod,omitempty"`
	// cpuCFSQuota enables CPU CFS quota enforcement for containers that specify CPU limits.
	// +optional
	CPUCFSQuota *bool `json:"cpuCFSQuota,omitempty"`
}

// UpCloudNodeClass is the Schema for the UpCloudNodeClass API
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status",description=""
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""
// +kubebuilder:printcolumn:name="Zones",type="string",JSONPath=".spec.zones",priority=1,description=""
// +kubebuilder:resource:path=upcloudnodeclasses,scope=Cluster,categories=karpenter,shortName={ucnc,ucncs}
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
type UpCloudNodeClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   UpCloudNodeClassSpec   `json:"spec,omitempty"`
	Status UpCloudNodeClassStatus `json:"status,omitempty"`
}

// UpCloudNodeClassHashVersion must be bumped when any of the following happens:
//  1. A hashed field changes its default value.
//  2. A field with an already-set value is added to the hash calculation.
//  3. A field is removed from the hash calculation.
//
// Bumping it prevents Karpenter from mass-replacing existing nodes just because the hash algorithm
// moved underneath them.
const UpCloudNodeClassHashVersion = "v1"

// Hash generates a hash of the fields of the UpCloudNodeClass that require a node to be replaced
// when they change. Fields that can be reconciled in place on a running server are excluded.
func (in *UpCloudNodeClass) Hash() string {
	hashableSpec := in.Spec
	// UpCloud labels are updated in place on running servers, so they must not force replacement.
	hashableSpec.Labels = nil
	return fmt.Sprint(lo.Must(hashstructure.Hash([]interface{}{
		hashableSpec,
	}, hashstructure.FormatV2, &hashstructure.HashOptions{
		SlicesAsSets:    true,
		IgnoreZeroValue: true,
		ZeroNil:         true,
	})))
}

func (in *UpCloudNodeClass) KubeletConfiguration() *KubeletConfiguration {
	return in.Spec.Kubelet
}

// TemplateID is the resolved root disk template UUID, falling back to whatever was requested while
// the NodeClass has not been reconciled yet.
func (in *UpCloudNodeClass) TemplateID() string {
	if in.Status.StorageTemplate != nil {
		return in.Status.StorageTemplate.UUID
	}
	return in.Spec.Storage.Template
}

// RootDiskSizeGB is the size the root disk will be created with for the given plan. UpCloud plans
// bundle a storage allowance; spec.storage.size overrides it, and a template larger than either
// wins because a clone can never shrink below its source.
func (in *UpCloudNodeClass) RootDiskSizeGB(planStorageGB int) int {
	size := planStorageGB
	if in.Spec.Storage.Size != nil {
		size = *in.Spec.Storage.Size
	}
	if in.Status.StorageTemplate != nil && in.Status.StorageTemplate.Size > size {
		size = in.Status.StorageTemplate.Size
	}
	return size
}

func (in *UpCloudNodeClass) GetConditions() []status.Condition {
	return in.Status.Conditions
}

func (in *UpCloudNodeClass) SetConditions(conditions []status.Condition) {
	in.Status.Conditions = conditions
}

func (in *UpCloudNodeClass) StatusConditions(opts ...status.ForOption) status.ConditionSet {
	return status.NewReadyConditions(
		ConditionTypeZonesReady,
		ConditionTypeStorageTemplateReady,
		ConditionTypeValidationSucceeded,
	).For(in, opts...)
}

// +kubebuilder:object:root=true

// UpCloudNodeClassList contains a list of UpCloudNodeClass
type UpCloudNodeClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UpCloudNodeClass `json:"items"`
}
