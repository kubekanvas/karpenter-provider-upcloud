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

// Package operator wires the UpCloud providers together and hands them to the Karpenter operator.
package operator

import (
	"context"
	"fmt"

	"github.com/patrickmn/go-cache"
	"sigs.k8s.io/karpenter/pkg/operator"

	upcloudcache "github.com/kubekanvas/karpenter-upcloud/pkg/cache"
	"github.com/kubekanvas/karpenter-upcloud/pkg/operator/options"
	"github.com/kubekanvas/karpenter-upcloud/pkg/providers/instance"
	"github.com/kubekanvas/karpenter-upcloud/pkg/providers/instancetype"
	"github.com/kubekanvas/karpenter-upcloud/pkg/providers/pricing"
	sdk "github.com/kubekanvas/karpenter-upcloud/pkg/upcloud"
)

const userAgent = "karpenter-upcloud"

// Operator holds everything the UpCloud CloudProvider and its controllers are built from.
type Operator struct {
	*operator.Operator

	UpCloudClient             sdk.UpCloudAPI
	UnavailableOfferingsCache *upcloudcache.UnavailableOfferings
	ValidationCache           *cache.Cache
	InstanceTypesProvider     *instancetype.DefaultProvider
	InstanceProvider          instance.Provider
	PricingProvider           *pricing.DefaultProvider
}

// NewOperator builds the UpCloud operator. Pass a non-nil client to inject a fake in tests;
// otherwise credentials are read from the environment.
func NewOperator(ctx context.Context, op *operator.Operator, upcloudClient sdk.UpCloudAPI) (*Operator, error) {
	if upcloudClient == nil {
		config, err := sdk.ClientConfigFromEnv()
		if err != nil {
			return nil, fmt.Errorf("configuring upcloud client, %w", err)
		}
		if upcloudClient, err = sdk.NewClient(config, userAgent); err != nil {
			return nil, fmt.Errorf("creating upcloud client, %w", err)
		}
	}
	opts := options.FromContext(ctx)

	unavailableOfferings := upcloudcache.NewUnavailableOfferings()
	validationCache := cache.New(upcloudcache.ValidationTTL, upcloudcache.DefaultCleanupInterval)

	pricingProvider := pricing.NewDefaultProvider(upcloudClient)
	instanceTypeProvider := instancetype.NewDefaultProvider(
		upcloudClient,
		instancetype.NewDefaultResolver(),
		pricingProvider,
		cache.New(upcloudcache.InstanceTypesAndOfferingsTTL, upcloudcache.DefaultCleanupInterval),
		cache.New(upcloudcache.InstanceTypesAndOfferingsTTL, upcloudcache.DefaultCleanupInterval),
		cache.New(upcloudcache.DiscoveredCapacityCacheTTL, upcloudcache.DefaultCleanupInterval),
		unavailableOfferings,
	)

	// Plans and prices must be loaded before any controller that depends on them starts, otherwise
	// the first scheduling pass sees an empty instance type list and reports the cluster as unable
	// to scale. Refreshes after this point are handled asynchronously by controllers.
	if err := pricingProvider.UpdatePrices(ctx); err != nil {
		return nil, fmt.Errorf("hydrating upcloud prices, %w", err)
	}
	if err := instanceTypeProvider.UpdatePlans(ctx); err != nil {
		return nil, fmt.Errorf("hydrating upcloud server plans, %w", err)
	}

	instanceProvider := instance.NewDefaultProvider(
		opts.ClusterName,
		upcloudClient,
		unavailableOfferings,
		cache.New(upcloudcache.DefaultTTL, upcloudcache.DefaultCleanupInterval),
	)

	return &Operator{
		Operator:                  op,
		UpCloudClient:             upcloudClient,
		UnavailableOfferingsCache: unavailableOfferings,
		ValidationCache:           validationCache,
		InstanceTypesProvider:     instanceTypeProvider,
		InstanceProvider:          instanceProvider,
		PricingProvider:           pricingProvider,
	}, nil
}
