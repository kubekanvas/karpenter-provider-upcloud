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

package cloudprovider

import (
	"context"

	"github.com/samber/lo"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/kubekanvas/karpenter-upcloud/pkg/apis/v1alpha1"
	"github.com/kubekanvas/karpenter-upcloud/pkg/utils"
)

const (
	// NodeClassDrift means the UpCloudNodeClass spec changed in a way that cannot be applied to a
	// running server, e.g. a different storage template or user data.
	NodeClassDrift cloudprovider.DriftReason = "NodeClassDrift"
	// ZoneDrift means the server's zone is no longer one the NodeClass allows.
	ZoneDrift cloudprovider.DriftReason = "ZoneDrift"
)

func (c *CloudProvider) isNodeClassDrifted(ctx context.Context, nodeClaim *karpv1.NodeClaim, nodeClass *v1alpha1.UpCloudNodeClass) cloudprovider.DriftReason {
	if drifted := c.areStaticFieldsDrifted(nodeClaim, nodeClass); drifted != "" {
		return drifted
	}
	return c.isZoneDrifted(ctx, nodeClaim, nodeClass)
}

// areStaticFieldsDrifted compares the NodeClass hash recorded on the NodeClaim at launch with the
// NodeClass's current hash. A mismatch means the server was launched from a spec that no longer
// exists and has to be replaced to pick up the new one.
func (c *CloudProvider) areStaticFieldsDrifted(nodeClaim *karpv1.NodeClaim, nodeClass *v1alpha1.UpCloudNodeClass) cloudprovider.DriftReason {
	nodeClassHash, foundNodeClassHash := nodeClass.Annotations[v1alpha1.AnnotationUpCloudNodeClassHash]
	nodeClassHashVersion, foundNodeClassHashVersion := nodeClass.Annotations[v1alpha1.AnnotationUpCloudNodeClassHashVersion]
	nodeClaimHash, foundNodeClaimHash := nodeClaim.Annotations[v1alpha1.AnnotationUpCloudNodeClassHash]
	nodeClaimHashVersion, foundNodeClaimHashVersion := nodeClaim.Annotations[v1alpha1.AnnotationUpCloudNodeClassHashVersion]

	if !foundNodeClassHash || !foundNodeClaimHash || !foundNodeClassHashVersion || !foundNodeClaimHashVersion {
		return ""
	}
	// Hashes computed by different algorithm versions are not comparable. Treating them as drift
	// would replace every node in the cluster on a controller upgrade.
	if nodeClassHashVersion != nodeClaimHashVersion {
		return ""
	}
	return lo.Ternary(nodeClassHash != nodeClaimHash, NodeClassDrift, "")
}

// isZoneDrifted reports servers running in a zone the NodeClass no longer lists. Removing a zone
// from spec.zones is otherwise invisible to drift detection, because the zone lives on the server
// rather than in the launch request's hash.
func (c *CloudProvider) isZoneDrifted(ctx context.Context, nodeClaim *karpv1.NodeClaim, nodeClass *v1alpha1.UpCloudNodeClass) cloudprovider.DriftReason {
	uuid, err := utils.ParseInstanceID(nodeClaim.Status.ProviderID)
	if err != nil {
		return ""
	}
	inst, err := c.instanceProvider.Get(ctx, uuid)
	if err != nil {
		return ""
	}
	zones := nodeClass.ZoneIDs()
	if len(zones) == 0 {
		// The NodeClass has not been reconciled yet; nothing to compare against.
		return ""
	}
	return lo.Ternary(lo.Contains(zones, inst.Zone), cloudprovider.DriftReason(""), ZoneDrift)
}
