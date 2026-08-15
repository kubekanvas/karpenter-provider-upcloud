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

package instance

import (
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"

	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/utils"
)

// Instance is this provider's view of an UpCloud cloud server.
//
// It is always built from a server details response rather than from a list response, because
// UpCloud's list endpoint omits labels — and labels are how a server is mapped back to its
// NodePool and NodeClass. Listing therefore costs one details call per server, which the instance
// cache exists to amortize.
type Instance struct {
	UUID   string
	Zone   string
	Plan   string
	State  string
	Title  string
	Labels map[string]string
}

// NewInstanceFromDetails builds an Instance from a server details response.
func NewInstanceFromDetails(details *upcloud.ServerDetails) *Instance {
	return &Instance{
		UUID:   details.UUID,
		Zone:   details.Zone,
		Plan:   details.Plan,
		State:  details.State,
		Title:  details.Title,
		Labels: utils.LabelSliceToMap(details.Labels),
	}
}

// ProviderID renders the instance as a Kubernetes providerID.
func (i *Instance) ProviderID() string {
	return utils.ProviderID(i.UUID)
}
