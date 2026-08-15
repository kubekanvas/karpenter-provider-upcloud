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

// Package controllers assembles the UpCloud-specific controllers that run alongside Karpenter core.
package controllers

import (
	"context"

	"github.com/awslabs/operatorpkg/controller"
	"github.com/awslabs/operatorpkg/status"
	"github.com/patrickmn/go-cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/kubekanvas/karpenter-upcloud/pkg/apis/v1alpha1"
	nodeclaimgarbagecollection "github.com/kubekanvas/karpenter-upcloud/pkg/controllers/nodeclaim/garbagecollection"
	nodeclaimlabeling "github.com/kubekanvas/karpenter-upcloud/pkg/controllers/nodeclaim/labeling"
	"github.com/kubekanvas/karpenter-upcloud/pkg/controllers/nodeclass"
	nodeclasshash "github.com/kubekanvas/karpenter-upcloud/pkg/controllers/nodeclass/hash"
	nodeclasstermination "github.com/kubekanvas/karpenter-upcloud/pkg/controllers/nodeclass/termination"
	controllersinstancetype "github.com/kubekanvas/karpenter-upcloud/pkg/controllers/providers/instancetype"
	controllerspricing "github.com/kubekanvas/karpenter-upcloud/pkg/controllers/providers/pricing"
	"github.com/kubekanvas/karpenter-upcloud/pkg/operator/options"
	"github.com/kubekanvas/karpenter-upcloud/pkg/providers/instance"
	"github.com/kubekanvas/karpenter-upcloud/pkg/providers/instancetype"
	"github.com/kubekanvas/karpenter-upcloud/pkg/providers/pricing"
	sdk "github.com/kubekanvas/karpenter-upcloud/pkg/upcloud"
)

func NewControllers(
	ctx context.Context,
	mgr manager.Manager,
	kubeClient client.Client,
	upcloudClient sdk.UpCloudAPI,
	validationCache *cache.Cache,
	cloudProvider cloudprovider.CloudProvider,
	instanceProvider instance.Provider,
	instanceTypeProvider *instancetype.DefaultProvider,
	pricingProvider *pricing.DefaultProvider,
) []controller.Controller {
	opts := options.FromContext(ctx)
	// operatorpkg's status controller takes the legacy client-go record.EventRecorder, so the
	// deprecated accessor is the only one whose type fits. Both recorders are used consistently
	// here rather than mixing event APIs across controllers.
	recorder := mgr.GetEventRecorderFor("karpenter") //nolint:staticcheck // SA1019: required by github.com/awslabs/operatorpkg/status.NewController.
	return []controller.Controller{
		nodeclass.NewController(kubeClient, upcloudClient, opts.ClusterZone, validationCache, opts.DisableDryRun),
		nodeclasshash.NewController(kubeClient),
		nodeclasstermination.NewController(kubeClient, recorder),
		nodeclaimgarbagecollection.NewController(kubeClient, cloudProvider),
		nodeclaimlabeling.NewController(kubeClient, instanceProvider),
		controllersinstancetype.NewController(instanceTypeProvider),
		controllerspricing.NewController(pricingProvider),
		status.NewController[*v1alpha1.UpCloudNodeClass](kubeClient, recorder, status.EmitDeprecatedMetrics),
	}
}
