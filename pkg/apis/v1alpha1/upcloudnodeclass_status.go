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
	"github.com/awslabs/operatorpkg/status"
)

const (
	ConditionTypeZonesReady           = "ZonesReady"
	ConditionTypeStorageTemplateReady = "StorageTemplateReady"
	ConditionTypeValidationSucceeded  = "ValidationSucceeded"
)

// Zone is a resolved UpCloud zone that servers may be launched into.
type Zone struct {
	// id of the zone, e.g. "fi-hel1".
	// +required
	ID string `json:"id"`
	// description of the zone as reported by UpCloud, e.g. "Helsinki #1".
	// +optional
	Description string `json:"description,omitempty"`
}

// StorageTemplate is the resolved root disk template.
type StorageTemplate struct {
	// uuid of the template.
	// +required
	UUID string `json:"uuid"`
	// title of the template as reported by UpCloud.
	// +optional
	Title string `json:"title,omitempty"`
	// size is the template's own size in gigabytes. A root disk can never be smaller than this.
	// +optional
	Size int `json:"size,omitempty"`
}

// UpCloudNodeClassStatus contains the resolved state of the UpCloudNodeClass.
type UpCloudNodeClassStatus struct {
	// zones contains the zones resolved from spec.zones that are known to UpCloud and available to
	// this account. Offerings are only created for these zones.
	// +optional
	Zones []Zone `json:"zones,omitempty"`
	// storageTemplate contains the resolved root disk template.
	// +optional
	StorageTemplate *StorageTemplate `json:"storageTemplate,omitempty"`
	// conditions contains signals for health and readiness.
	// +optional
	Conditions []status.Condition `json:"conditions,omitempty"`
}

// ZoneIDs returns the resolved zone ids, which is the form the instance type provider consumes.
func (in *UpCloudNodeClass) ZoneIDs() []string {
	ids := make([]string, 0, len(in.Status.Zones))
	for _, z := range in.Status.Zones {
		ids = append(ids, z.ID)
	}
	return ids
}
