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

// Package nodeclass reconciles UpCloudNodeClasses against the UpCloud API, resolving the zones and
// storage template a NodeClass names and reporting readiness.
package nodeclass

import (
	"context"
	"fmt"
	"time"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"
	"github.com/awslabs/operatorpkg/reasonable"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/sets"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/karpenter/pkg/operator/injection"

	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/apis/v1alpha1"
	sdk "github.com/kubekanvas/karpenter-provider-upcloud/pkg/upcloud"
	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/utils"
)

// resyncPeriod bounds how stale a NodeClass's resolved zones and template can be. UpCloud adds
// zones and retires templates rarely, so this only needs to be faster than a human noticing.
const resyncPeriod = time.Minute * 5

type Controller struct {
	kubeClient      client.Client
	client          sdk.UpCloudAPI
	defaultZone     string
	validationCache *cache.Cache
	disableDryRun   bool
}

func NewController(
	kubeClient client.Client,
	upcloudClient sdk.UpCloudAPI,
	defaultZone string,
	validationCache *cache.Cache,
	disableDryRun bool,
) *Controller {
	return &Controller{
		kubeClient:      kubeClient,
		client:          upcloudClient,
		defaultZone:     defaultZone,
		validationCache: validationCache,
		disableDryRun:   disableDryRun,
	}
}

func (c *Controller) Reconcile(ctx context.Context, nodeClass *v1alpha1.UpCloudNodeClass) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "nodeclass.status")

	if !nodeClass.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}
	stored := nodeClass.DeepCopy()

	// Each step records its own condition and does not short-circuit the others, so a NodeClass
	// with both a bad zone and a bad template reports both problems at once instead of one per
	// reconcile.
	zoneErr := c.resolveZones(ctx, nodeClass)
	templateErr := c.resolveStorageTemplate(ctx, nodeClass)
	validationErr := c.validate(ctx, nodeClass)

	if !equality.Semantic.DeepEqual(stored, nodeClass) {
		if err := c.kubeClient.Status().Patch(ctx, nodeClass, client.MergeFrom(stored)); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}
	// Transient UpCloud failures are worth retrying with backoff; a NodeClass that simply names a
	// zone that does not exist is not, and is left to be corrected by the user.
	for _, err := range []error{zoneErr, templateErr, validationErr} {
		if err != nil && sdk.IsRetryable(err) {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{RequeueAfter: resyncPeriod}, nil
}

// resolveZones intersects the zones the NodeClass asks for with the zones UpCloud actually offers
// this account, so that a typo surfaces as a NotReady NodeClass rather than as failing launches.
func (c *Controller) resolveZones(ctx context.Context, nodeClass *v1alpha1.UpCloudNodeClass) error {
	zones, err := c.client.GetZones(ctx)
	if err != nil {
		nodeClass.StatusConditions().SetUnknownWithReason(v1alpha1.ConditionTypeZonesReady,
			"ZoneResolutionFailed", fmt.Sprintf("Failed to list UpCloud zones, %s", err))
		return fmt.Errorf("listing zones, %w", err)
	}

	requested := nodeClass.Spec.Zones
	if len(requested) == 0 {
		requested = []string{c.defaultZone}
	}
	available := lo.SliceToMap(zones.Zones, func(z upcloud.Zone) (string, upcloud.Zone) { return z.ID, z })

	resolved := make([]v1alpha1.Zone, 0, len(requested))
	var unknown []string
	for _, id := range requested {
		zone, ok := available[id]
		if !ok {
			unknown = append(unknown, id)
			continue
		}
		resolved = append(resolved, v1alpha1.Zone{ID: zone.ID, Description: zone.Description})
	}
	if len(unknown) > 0 {
		nodeClass.Status.Zones = nil
		nodeClass.StatusConditions().SetFalse(v1alpha1.ConditionTypeZonesReady,
			"ZonesNotFound", fmt.Sprintf("Unknown UpCloud zones: %s", utils.PrettySlice(unknown, 5)))
		return fmt.Errorf("unknown zones %v", unknown)
	}
	if len(resolved) == 0 {
		nodeClass.Status.Zones = nil
		nodeClass.StatusConditions().SetFalse(v1alpha1.ConditionTypeZonesReady,
			"ZonesNotFound", "No zones configured; set spec.zones or --cluster-zone")
		return fmt.Errorf("no zones configured")
	}

	nodeClass.Status.Zones = resolved
	nodeClass.StatusConditions().SetTrue(v1alpha1.ConditionTypeZonesReady)
	return nil
}

// resolveStorageTemplate confirms the template exists and records its size, which sets the floor on
// the root disk size: UpCloud refuses to clone a template into a smaller disk.
func (c *Controller) resolveStorageTemplate(ctx context.Context, nodeClass *v1alpha1.UpCloudNodeClass) error {
	details, err := c.client.GetStorageDetails(ctx, &request.GetStorageDetailsRequest{
		UUID: nodeClass.Spec.Storage.Template,
	})
	if sdk.IsNotFound(err) {
		nodeClass.Status.StorageTemplate = nil
		nodeClass.StatusConditions().SetFalse(v1alpha1.ConditionTypeStorageTemplateReady,
			"StorageTemplateNotFound", fmt.Sprintf("Storage template %q not found", nodeClass.Spec.Storage.Template))
		return err
	}
	if err != nil {
		nodeClass.StatusConditions().SetUnknownWithReason(v1alpha1.ConditionTypeStorageTemplateReady,
			"StorageTemplateResolutionFailed", fmt.Sprintf("Failed to resolve storage template, %s", err))
		return fmt.Errorf("resolving storage template, %w", err)
	}
	if details.Type != upcloud.StorageTypeTemplate {
		nodeClass.Status.StorageTemplate = nil
		nodeClass.StatusConditions().SetFalse(v1alpha1.ConditionTypeStorageTemplateReady,
			"StorageNotATemplate", fmt.Sprintf("Storage %q is of type %q, expected %q",
				details.UUID, details.Type, upcloud.StorageTypeTemplate))
		return fmt.Errorf("storage %q is not a template", details.UUID)
	}

	nodeClass.Status.StorageTemplate = &v1alpha1.StorageTemplate{
		UUID:  details.UUID,
		Title: details.Title,
		Size:  details.Size,
	}
	nodeClass.StatusConditions().SetTrue(v1alpha1.ConditionTypeStorageTemplateReady)
	return nil
}

// validate checks the parts of the NodeClass that CRD validation cannot express on its own, either
// because they depend on other fields or because they depend on UpCloud's own constraints.
func (c *Controller) validate(ctx context.Context, nodeClass *v1alpha1.UpCloudNodeClass) error {
	if err := utils.ValidateLabels(nodeClass.Spec.Labels); err != nil {
		nodeClass.StatusConditions().SetFalse(v1alpha1.ConditionTypeValidationSucceeded,
			"LabelValidationFailed", err.Error())
		return err
	}
	if err := validateNetwork(nodeClass); err != nil {
		nodeClass.StatusConditions().SetFalse(v1alpha1.ConditionTypeValidationSucceeded,
			"NetworkValidationFailed", err.Error())
		return err
	}
	if err := validateStorageSize(nodeClass); err != nil {
		nodeClass.StatusConditions().SetFalse(v1alpha1.ConditionTypeValidationSucceeded,
			"StorageValidationFailed", err.Error())
		return err
	}
	if err := c.validateServerGroup(ctx, nodeClass); err != nil {
		nodeClass.StatusConditions().SetFalse(v1alpha1.ConditionTypeValidationSucceeded,
			"ServerGroupValidationFailed", err.Error())
		return err
	}
	nodeClass.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)
	return nil
}

func validateNetwork(nodeClass *v1alpha1.UpCloudNodeClass) error {
	if nodeClass.Spec.Network == nil || len(nodeClass.Spec.Network.Interfaces) == 0 {
		return nil
	}
	var hasPrivateAddress bool
	seenSDN := sets.New[string]()
	for i, iface := range nodeClass.Spec.Network.Interfaces {
		switch iface.Type {
		case "private":
			if lo.FromPtr(iface.Network) == "" {
				return fmt.Errorf("network.interfaces[%d]: private interfaces require a network UUID", i)
			}
			if seenSDN.Has(*iface.Network) {
				return fmt.Errorf("network.interfaces[%d]: network %q is attached more than once", i, *iface.Network)
			}
			seenSDN.Insert(*iface.Network)
			hasPrivateAddress = true
		case "utility":
			hasPrivateAddress = true
		}
	}
	// The UpCloud cloud-controller-manager refuses to initialize a node that has no private
	// address, which leaves the node tainted as uninitialized forever.
	if !hasPrivateAddress {
		return fmt.Errorf("network.interfaces: at least one utility or private interface is required so the node has a private address")
	}
	return nil
}

func validateStorageSize(nodeClass *v1alpha1.UpCloudNodeClass) error {
	if nodeClass.Spec.Storage.Size == nil || nodeClass.Status.StorageTemplate == nil {
		return nil
	}
	if *nodeClass.Spec.Storage.Size < nodeClass.Status.StorageTemplate.Size {
		return fmt.Errorf("storage.size (%dGB) is smaller than template %q (%dGB); a clone cannot shrink its source",
			*nodeClass.Spec.Storage.Size, nodeClass.Status.StorageTemplate.UUID, nodeClass.Status.StorageTemplate.Size)
	}
	return nil
}

func (c *Controller) validateServerGroup(ctx context.Context, nodeClass *v1alpha1.UpCloudNodeClass) error {
	group := lo.FromPtr(nodeClass.Spec.ServerGroup)
	if group == "" || c.disableDryRun {
		return nil
	}
	// Server groups change rarely and this runs on every NodeClass reconcile, so a successful
	// lookup is remembered rather than repeated.
	cacheKey := fmt.Sprintf("server-group-%s", group)
	if _, ok := c.validationCache.Get(cacheKey); ok {
		return nil
	}
	if _, err := c.client.GetServerGroup(ctx, &request.GetServerGroupRequest{UUID: group}); err != nil {
		if sdk.IsNotFound(err) {
			return fmt.Errorf("server group %q not found", group)
		}
		return fmt.Errorf("resolving server group %q, %w", group, err)
	}
	c.validationCache.SetDefault(cacheKey, struct{}{})
	return nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("nodeclass.status").
		For(&v1alpha1.UpCloudNodeClass{}).
		WithOptions(controller.Options{
			RateLimiter:             reasonable.RateLimiter(),
			MaxConcurrentReconciles: 10,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
