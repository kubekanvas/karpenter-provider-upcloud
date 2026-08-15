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

// Package instancetype maps UpCloud server plans onto Karpenter instance types.
package instancetype

import (
	"context"
	"fmt"
	"sync"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"

	upcloudcache "github.com/kubekanvas/karpenter-upcloud/pkg/cache"
	"github.com/kubekanvas/karpenter-upcloud/pkg/providers/instancetype/offering"
	"github.com/kubekanvas/karpenter-upcloud/pkg/providers/pricing"
	sdk "github.com/kubekanvas/karpenter-upcloud/pkg/upcloud"
)

type Provider interface {
	Get(ctx context.Context, nodeClass NodeClass, name string) (*cloudprovider.InstanceType, error)
	List(ctx context.Context, nodeClass NodeClass) ([]*cloudprovider.InstanceType, error)
}

type DefaultProvider struct {
	client   sdk.UpCloudAPI
	resolver Resolver

	muPlans sync.RWMutex
	plans   map[string]upcloud.Plan

	instanceTypesCache      *cache.Cache
	discoveredCapacityCache *cache.Cache
	cm                      *pretty.ChangeMonitor

	offeringProvider *offering.DefaultProvider
}

func NewDefaultProvider(
	client sdk.UpCloudAPI,
	resolver Resolver,
	pricingProvider pricing.Provider,
	instanceTypesCache *cache.Cache,
	offeringCache *cache.Cache,
	discoveredCapacityCache *cache.Cache,
	unavailableOfferings *upcloudcache.UnavailableOfferings,
) *DefaultProvider {
	return &DefaultProvider{
		client:                  client,
		resolver:                resolver,
		plans:                   map[string]upcloud.Plan{},
		instanceTypesCache:      instanceTypesCache,
		discoveredCapacityCache: discoveredCapacityCache,
		cm:                      pretty.NewChangeMonitor(),
		offeringProvider:        offering.NewDefaultProvider(pricingProvider, unavailableOfferings, offeringCache),
	}
}

func (p *DefaultProvider) List(ctx context.Context, nodeClass NodeClass) ([]*cloudprovider.InstanceType, error) {
	p.muPlans.RLock()
	defer p.muPlans.RUnlock()

	if len(p.plans) == 0 {
		return nil, fmt.Errorf("no upcloud server plans found")
	}
	zones := nodeClass.ZoneIDs()
	if len(zones) == 0 {
		return nil, fmt.Errorf("nodeclass has no resolved zones")
	}

	key := p.cacheKey(nodeClass)
	var instanceTypes []*cloudprovider.InstanceType
	if item, ok := p.instanceTypesCache.Get(key); ok {
		instanceTypes = item.([]*cloudprovider.InstanceType)
	} else {
		instanceTypes = lo.FilterMapToSlice(p.plans, func(name string, _ upcloud.Plan) (*cloudprovider.InstanceType, bool) {
			it, err := p.get(ctx, nodeClass, name)
			if err != nil {
				return nil, false
			}
			return it, true
		})
		p.instanceTypesCache.SetDefault(key, instanceTypes)
	}
	return p.offeringProvider.InjectOfferings(instanceTypes, zones), nil
}

func (p *DefaultProvider) Get(ctx context.Context, nodeClass NodeClass, name string) (*cloudprovider.InstanceType, error) {
	instanceTypes, err := p.List(ctx, nodeClass)
	if err != nil {
		return nil, err
	}
	it, ok := lo.Find(instanceTypes, func(i *cloudprovider.InstanceType) bool { return i.Name == name })
	if !ok {
		return nil, fmt.Errorf("instance type %q not found", name)
	}
	return it, nil
}

func (p *DefaultProvider) get(ctx context.Context, nodeClass NodeClass, name string) (*cloudprovider.InstanceType, error) {
	plan, ok := p.plans[name]
	if !ok {
		return nil, fmt.Errorf("plan %q not found in cache", name)
	}
	it := p.resolver.Resolve(ctx, &plan, nodeClass)
	if it == nil {
		return nil, fmt.Errorf("failed to generate instance type %q", name)
	}
	// Prefer memory capacity observed on a real node over the estimate, since the hypervisor and
	// kernel overhead we subtract is only ever an approximation.
	if cached, ok := p.discoveredCapacityCache.Get(discoveredCapacityCacheKey(it.Name, nodeClass)); ok {
		it.Capacity[corev1.ResourceMemory] = cached.(resource.Quantity)
	}
	InstanceTypeVCPU.Set(float64(plan.CoreNumber), map[string]string{instanceTypeLabel: plan.Name})
	InstanceTypeMemory.Set(float64(plan.MemoryAmount)*1024*1024, map[string]string{instanceTypeLabel: plan.Name})
	return it, nil
}

// UpdatePlans refreshes the plan catalog from UpCloud. It is called once at startup and then
// periodically by the instance type controller.
func (p *DefaultProvider) UpdatePlans(ctx context.Context) error {
	// DO NOT REMOVE THIS LOCK ----------------------------------------------------------------------
	// It serializes concurrent refreshes so that a burst of callers turns into one UpCloud request
	// rather than one per caller.
	p.muPlans.Lock()
	defer p.muPlans.Unlock()

	plans, err := p.client.GetPlans(ctx)
	if err != nil {
		return fmt.Errorf("listing upcloud server plans, %w", err)
	}
	if len(plans.Plans) == 0 {
		return fmt.Errorf("upcloud returned no server plans")
	}

	if p.cm.HasChanged("plans", plans.Plans) {
		// None of the cached instance types are valid once the plan catalog moves.
		p.instanceTypesCache.Flush()
		log.FromContext(ctx).WithValues("count", len(plans.Plans)).V(1).Info("discovered server plans")
	}
	p.plans = lo.SliceToMap(plans.Plans, func(plan upcloud.Plan) (string, upcloud.Plan) {
		return plan.Name, plan
	})
	return nil
}

// UpdateInstanceTypeCapacityFromNode records the memory a real node of this instance type reports.
// Karpenter over-provisions when its estimate is too high, so the smallest observed value wins.
func (p *DefaultProvider) UpdateInstanceTypeCapacityFromNode(ctx context.Context, node *corev1.Node, nodeClass NodeClass) error {
	instanceTypeName, ok := node.Labels[corev1.LabelInstanceTypeStable]
	if !ok {
		return nil
	}
	key := discoveredCapacityCacheKey(instanceTypeName, nodeClass)
	actualCapacity := node.Status.Capacity.Memory()
	cachedCapacity, found := p.discoveredCapacityCache.Get(key)
	if found && actualCapacity.Cmp(cachedCapacity.(resource.Quantity)) > 0 {
		return nil
	}
	// Set even when the values are equal, to refresh the TTL.
	p.discoveredCapacityCache.SetDefault(key, *actualCapacity)
	if !found || actualCapacity.Cmp(cachedCapacity.(resource.Quantity)) < 0 {
		log.FromContext(ctx).WithValues(
			"memory-capacity", actualCapacity,
			"instance-type", instanceTypeName).V(1).Info("updating discovered capacity cache")
	}
	return nil
}

func (p *DefaultProvider) cacheKey(nodeClass NodeClass) string {
	return p.resolver.CacheKey(nodeClass)
}

func discoveredCapacityCacheKey(instanceType string, nodeClass NodeClass) string {
	// The root disk template determines the kernel, and the kernel determines how much of the
	// plan's memory the guest actually sees, so capacity observations cannot be shared across
	// NodeClasses with different templates.
	return fmt.Sprintf("%s-%s", instanceType, nodeClass.TemplateID())
}

// Reset drops all cached plan and capacity data. Used by tests.
func (p *DefaultProvider) Reset() {
	p.muPlans.Lock()
	defer p.muPlans.Unlock()

	p.plans = map[string]upcloud.Plan{}
	p.instanceTypesCache.Flush()
	p.discoveredCapacityCache.Flush()
}
