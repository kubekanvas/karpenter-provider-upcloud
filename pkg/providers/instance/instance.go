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

// Package instance launches, inspects and terminates UpCloud cloud servers on behalf of NodeClaims.
package instance

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"
	"github.com/awslabs/operatorpkg/option"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/kubekanvas/karpenter-upcloud/pkg/apis/v1alpha1"
	upcloudcache "github.com/kubekanvas/karpenter-upcloud/pkg/cache"
	sdk "github.com/kubekanvas/karpenter-upcloud/pkg/upcloud"
	"github.com/kubekanvas/karpenter-upcloud/pkg/utils"
)

const (
	// stopTimeout is how long UpCloud is asked to wait for a graceful ACPI shutdown before pulling
	// the plug. Karpenter has already drained the node by the time Delete is called, so there is
	// nothing left to lose by being impatient.
	stopTimeout = 30 * time.Second

	// listConcurrency bounds the parallel details lookups performed while listing servers. UpCloud
	// rate limits per account, and garbage collection is not latency sensitive.
	listConcurrency = 8

	// interfaceType* mirror the values UpCloud accepts for a network interface.
	interfaceTypePublic  = "public"
	interfaceTypeUtility = "utility"
	interfaceTypePrivate = "private"

	ipFamilyIPv4 = "IPv4"
)

// SkipCache forces Get to bypass the instance cache. Use it whenever a stale answer would be acted
// on destructively, such as before terminating a server.
var SkipCache = func(opts *options) {
	opts.SkipCache = true
}

type options struct {
	SkipCache bool
}

// Options is the functional option type accepted by Get.
type Options = option.Function[options]

type Provider interface {
	Create(ctx context.Context, nodeClass *v1alpha1.UpCloudNodeClass, nodeClaim *karpv1.NodeClaim, labels map[string]string, instanceTypes []*cloudprovider.InstanceType) (*Instance, error)
	Get(ctx context.Context, uuid string, opts ...Options) (*Instance, error)
	List(ctx context.Context) ([]*Instance, error)
	Delete(ctx context.Context, uuid string) error
	UpdateLabels(ctx context.Context, uuid string, labels map[string]string) error
}

type DefaultProvider struct {
	clusterName          string
	client               sdk.UpCloudAPI
	unavailableOfferings *upcloudcache.UnavailableOfferings
	instanceCache        *cache.Cache
}

func NewDefaultProvider(
	clusterName string,
	client sdk.UpCloudAPI,
	unavailableOfferings *upcloudcache.UnavailableOfferings,
	instanceCache *cache.Cache,
) *DefaultProvider {
	return &DefaultProvider{
		clusterName:          clusterName,
		client:               client,
		unavailableOfferings: unavailableOfferings,
		instanceCache:        instanceCache,
	}
}

func (p *DefaultProvider) Create(
	ctx context.Context,
	nodeClass *v1alpha1.UpCloudNodeClass,
	nodeClaim *karpv1.NodeClaim,
	labels map[string]string,
	instanceTypes []*cloudprovider.InstanceType,
) (*Instance, error) {
	instanceType, zone, err := cheapestOffering(instanceTypes, nodeClaim)
	if err != nil {
		return nil, err
	}
	req, err := p.createRequest(nodeClass, nodeClaim, instanceType, zone, labels)
	if err != nil {
		return nil, cloudprovider.NewCreateError(err, "CreateRequestFailed", "Failed to build UpCloud server request")
	}

	details, err := p.client.CreateServer(ctx, req)
	if err != nil {
		p.recordLaunchFailure(ctx, err, instanceType.Name, zone)
		if sdk.IsInsufficientCapacity(err) {
			return nil, cloudprovider.NewInsufficientCapacityError(fmt.Errorf("creating server, %w", err))
		}
		return nil, cloudprovider.NewCreateError(fmt.Errorf("creating server, %w", err),
			"ServerCreationFailed", fmt.Sprintf("Failed to create UpCloud server: %s", err))
	}

	log.FromContext(ctx).WithValues(
		"uuid", details.UUID,
		"plan", details.Plan,
		"zone", details.Zone).V(1).Info("launched server")

	instance := NewInstanceFromDetails(details)
	p.instanceCache.SetDefault(instance.UUID, instance)
	return instance, nil
}

func (p *DefaultProvider) Get(ctx context.Context, uuid string, opts ...Options) (*Instance, error) {
	if !option.Resolve(opts...).SkipCache {
		if cached, ok := p.instanceCache.Get(uuid); ok {
			return cached.(*Instance), nil
		}
	}

	details, err := p.client.GetServerDetails(ctx, &request.GetServerDetailsRequest{UUID: uuid})
	if sdk.IsNotFound(err) {
		p.instanceCache.Delete(uuid)
		return nil, cloudprovider.NewNodeClaimNotFoundError(err)
	}
	if err != nil {
		return nil, fmt.Errorf("getting server %q, %w", uuid, err)
	}

	instance := NewInstanceFromDetails(details)
	p.instanceCache.SetDefault(uuid, instance)
	return instance, nil
}

// List returns every server this controller manages for the configured cluster. The label filter is
// applied server-side, so servers belonging to other clusters — or created by hand — are never
// returned and therefore never garbage collected.
func (p *DefaultProvider) List(ctx context.Context) ([]*Instance, error) {
	servers, err := p.client.GetServersWithFilters(ctx, &request.GetServersWithFiltersRequest{
		Filters: []request.QueryFilter{
			request.FilterLabel{Label: upcloud.Label{
				Key:   v1alpha1.LabelKeyManagedBy,
				Value: p.clusterName,
			}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("listing servers, %w", err)
	}

	// UpCloud's list response omits labels, so each server needs a details lookup to recover its
	// NodePool and NodeClass. Cached entries skip the call entirely.
	instances := make([]*Instance, len(servers.Servers))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(listConcurrency)
	for i, server := range servers.Servers {
		group.Go(func() error {
			instance, err := p.Get(groupCtx, server.UUID)
			if cloudprovider.IgnoreNodeClaimNotFoundError(err) != nil {
				return err
			}
			// A server that vanished between the list and the details call is simply gone.
			instances[i] = instance
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, fmt.Errorf("describing servers, %w", err)
	}
	return lo.Filter(instances, func(i *Instance, _ int) bool { return i != nil }), nil
}

// Delete terminates a server. UpCloud refuses to delete a running server, so this stops it first
// and returns without error; Karpenter keeps calling Delete until it reports NodeClaimNotFound, and
// the next call finds the server stopped and removes it along with its disks.
func (p *DefaultProvider) Delete(ctx context.Context, uuid string) error {
	instance, err := p.Get(ctx, uuid, SkipCache)
	if err != nil {
		return err
	}

	switch instance.State {
	case upcloud.ServerStateStopped:
		// Ready to be removed.
	case upcloud.ServerStateMaintenance:
		// UpCloud is already doing something to this server; retry once it settles.
		return fmt.Errorf("server %q is in maintenance, waiting before deletion", uuid)
	default:
		log.FromContext(ctx).WithValues("uuid", uuid, "state", instance.State).V(1).Info("stopping server before deletion")
		if _, err := p.client.StopServer(ctx, &request.StopServerRequest{
			UUID: uuid,
			// Ask for a graceful shutdown but let UpCloud force it once the timeout elapses, so a
			// wedged guest cannot keep a drained node billable indefinitely.
			StopType: request.ServerStopTypeSoft,
			Timeout:  stopTimeout,
		}); err != nil {
			if sdk.IsNotFound(err) {
				p.instanceCache.Delete(uuid)
				return cloudprovider.NewNodeClaimNotFoundError(err)
			}
			return fmt.Errorf("stopping server %q, %w", uuid, err)
		}
		p.instanceCache.Delete(uuid)
		return fmt.Errorf("server %q is stopping, deletion will be retried", uuid)
	}

	// Storages are deleted along with the server: the root disk is created by this controller and
	// has no life of its own, and leaving it behind would silently accrue cost.
	if err := p.client.DeleteServerAndStorages(ctx, &request.DeleteServerAndStoragesRequest{UUID: uuid}); err != nil {
		if sdk.IsNotFound(err) {
			p.instanceCache.Delete(uuid)
			return cloudprovider.NewNodeClaimNotFoundError(err)
		}
		return fmt.Errorf("deleting server %q, %w", uuid, err)
	}
	p.instanceCache.Delete(uuid)
	return nil
}

// UpdateLabels replaces the labels on a running server. UpCloud's PATCH replaces the whole label
// set, so callers must pass the complete desired set rather than a delta.
func (p *DefaultProvider) UpdateLabels(ctx context.Context, uuid string, labels map[string]string) error {
	labelSlice := utils.MapToLabelSlice(labels)
	if _, err := p.client.ModifyServer(ctx, &request.ModifyServerRequest{
		UUID:   uuid,
		Labels: &labelSlice,
	}); err != nil {
		if sdk.IsNotFound(err) {
			return cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("updating server labels, %w", err))
		}
		return fmt.Errorf("updating labels on server %q, %w", uuid, err)
	}
	p.instanceCache.Delete(uuid)
	return nil
}

func (p *DefaultProvider) createRequest(
	nodeClass *v1alpha1.UpCloudNodeClass,
	nodeClaim *karpv1.NodeClaim,
	instanceType *cloudprovider.InstanceType,
	zone string,
	labels map[string]string,
) (*request.CreateServerRequest, error) {
	if nodeClass.TemplateID() == "" {
		return nil, fmt.Errorf("nodeclass has no resolved storage template")
	}
	hostname := hostname(nodeClass, nodeClaim)
	labelSlice := utils.MapToLabelSlice(labels)
	// The plan's own storage allowance is carried on the instance type as a requirement, which
	// saves the instance provider a second lookup against the plan catalog.
	planStorageGB, err := strconv.Atoi(instanceType.Requirements.Get(v1alpha1.LabelInstanceStorageSize).Any())
	if err != nil {
		return nil, fmt.Errorf("reading plan storage size for %q, %w", instanceType.Name, err)
	}

	req := &request.CreateServerRequest{
		Hostname: hostname,
		Title:    hostname,
		Zone:     zone,
		Plan:     instanceType.Name,
		Labels:   &labelSlice,
		// cloud-init reads user data from the metadata service, so enabling it is not optional
		// whenever user data is set. Leaving it on unconditionally also lets the node discover its
		// own UUID, which the cloud-controller-manager needs.
		Metadata:   upcloud.True,
		UserData:   lo.FromPtr(nodeClass.Spec.UserData),
		Networking: networking(nodeClass),
		StorageDevices: request.CreateServerStorageDeviceSlice{{
			Action:    "clone",
			Storage:   nodeClass.TemplateID(),
			Title:     fmt.Sprintf("%s-root", hostname),
			Size:      nodeClass.RootDiskSizeGB(planStorageGB),
			Tier:      lo.FromPtr(nodeClass.Spec.Storage.Tier),
			Encrypted: upcloud.FromBool(lo.FromPtr(nodeClass.Spec.Storage.Encrypted)),
		}},
	}
	if nodeClass.Spec.ServerGroup != nil {
		req.ServerGroup = *nodeClass.Spec.ServerGroup
	}
	if nodeClass.Spec.SimpleBackup != nil {
		req.SimpleBackup = *nodeClass.Spec.SimpleBackup
	}
	if nodeClass.Spec.TimeZone != nil {
		req.TimeZone = *nodeClass.Spec.TimeZone
	}
	if lo.FromPtr(nodeClass.Spec.Firewall) {
		req.Firewall = "on"
	}
	if nodeClass.Spec.LoginUser != nil {
		req.LoginUser = &request.LoginUser{
			Username: lo.FromPtr(nodeClass.Spec.LoginUser.Username),
			SSHKeys:  nodeClass.Spec.LoginUser.SSHKeys,
		}
		// Without this, UpCloud emails a generated root password to the account owner on every
		// single node launch.
		req.PasswordDelivery = "none"
	}
	return req, nil
}

// networking renders the NodeClass interfaces, defaulting to a public IPv4 interface plus the
// utility network. The utility interface is what gives the node a private address, which the
// UpCloud cloud-controller-manager requires on every node it manages.
func networking(nodeClass *v1alpha1.UpCloudNodeClass) *request.CreateServerNetworking {
	specInterfaces := []v1alpha1.NetworkInterface{
		{Type: interfaceTypePublic},
		{Type: interfaceTypeUtility},
	}
	if nodeClass.Spec.Network != nil && len(nodeClass.Spec.Network.Interfaces) > 0 {
		specInterfaces = nodeClass.Spec.Network.Interfaces
	}

	interfaces := make(request.CreateServerInterfaceSlice, 0, len(specInterfaces))
	for i, iface := range specInterfaces {
		families := iface.IPFamilies
		if len(families) == 0 || iface.Type != interfaceTypePublic {
			// Only public interfaces can carry IPv6; utility and SDN interfaces are IPv4 only.
			families = []string{ipFamilyIPv4}
		}
		addresses := make(request.CreateServerIPAddressSlice, 0, len(families))
		for _, family := range families {
			addresses = append(addresses, request.CreateServerIPAddress{Family: family})
		}
		created := request.CreateServerInterface{
			Index:             i + 1,
			Type:              iface.Type,
			IPAddresses:       addresses,
			SourceIPFiltering: upcloud.FromBool(lo.FromPtr(iface.SourceIPFiltering)),
		}
		if iface.Type == interfaceTypePrivate {
			created.Network = lo.FromPtr(iface.Network)
		}
		interfaces = append(interfaces, created)
	}
	return &request.CreateServerNetworking{Interfaces: interfaces}
}

// hostname derives a stable, DNS-safe hostname from the NodeClaim. NodeClaim names are already
// valid DNS labels and are unique for the lifetime of the node, so they need no suffix of their own.
func hostname(nodeClass *v1alpha1.UpCloudNodeClass, nodeClaim *karpv1.NodeClaim) string {
	if prefix := lo.FromPtr(nodeClass.Spec.HostnamePrefix); prefix != "" {
		return fmt.Sprintf("%s-%s", prefix, nodeClaim.Name)
	}
	return nodeClaim.Name
}

// recordLaunchFailure takes the offering that just failed out of rotation, so the next scheduling
// pass picks a different plan or zone instead of hammering the same one.
func (p *DefaultProvider) recordLaunchFailure(ctx context.Context, err error, plan, zone string) {
	switch {
	case sdk.IsInsufficientCapacity(err):
		p.unavailableOfferings.MarkUnavailable(ctx, err.Error(), plan, zone)
	case sdk.IsRetryable(err):
		// A 5xx is about UpCloud, not about this plan, so pull the whole zone rather than
		// convincing ourselves that one plan is short on capacity.
		p.unavailableOfferings.MarkZoneUnavailable(ctx, err.Error(), zone)
	default:
		log.FromContext(ctx).WithValues("plan", plan, "zone", zone).Error(err, "failed to launch server")
	}
}

// cheapestOffering picks the lowest-priced available offering that satisfies the NodeClaim. UpCloud
// exposes no batch or fleet launch API, so unlike on AWS there is no way to hand it a list of
// acceptable plans and let it choose — the choice has to be made here.
func cheapestOffering(instanceTypes []*cloudprovider.InstanceType, nodeClaim *karpv1.NodeClaim) (*cloudprovider.InstanceType, string, error) {
	reqs := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)

	var bestType *cloudprovider.InstanceType
	var bestOffering *cloudprovider.Offering
	for _, it := range instanceTypes {
		if err := it.Requirements.Compatible(reqs, scheduling.AllowUndefinedWellKnownLabels); err != nil {
			continue
		}
		if !resourcesFit(it, nodeClaim) {
			continue
		}
		offering := it.Offerings.Available().Compatible(reqs).Cheapest()
		if offering == nil {
			continue
		}
		if bestOffering == nil || offering.Price < bestOffering.Price {
			bestType, bestOffering = it, offering
		}
	}
	if bestOffering == nil {
		return nil, "", cloudprovider.NewInsufficientCapacityError(
			fmt.Errorf("no available upcloud offering satisfies the nodeclaim"))
	}
	return bestType, bestOffering.Requirements.Get(corev1.LabelTopologyZone).Any(), nil
}

func resourcesFit(it *cloudprovider.InstanceType, nodeClaim *karpv1.NodeClaim) bool {
	allocatable := it.Allocatable()
	for name, quantity := range nodeClaim.Spec.Resources.Requests {
		available, ok := allocatable[name]
		if !ok || available.Cmp(quantity) < 0 {
			return false
		}
	}
	return true
}
