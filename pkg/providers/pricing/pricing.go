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

// Package pricing turns UpCloud's /price response into the per-hour prices Karpenter uses to rank
// offerings and to compute consolidation savings.
package pricing

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"

	sdk "github.com/kubekanvas/karpenter-upcloud/pkg/upcloud"
)

const (
	// planPricePrefix is how UpCloud names a plan's price entry within a zone, e.g. the price of
	// the "2xCPU-4GB" plan is reported as "server_plan_2xCPU-4GB".
	planPricePrefix = "server_plan_"

	// creditsPerEuro converts UpCloud's billing unit into euros. UpCloud quotes /price in credits
	// per hour, where 100 credits is one euro. Karpenter only ever compares prices to each other,
	// so the conversion is cosmetic — but it makes the cost metrics Karpenter exports read in a
	// real currency instead of an UpCloud-internal unit.
	creditsPerEuro = 100.0
)

// Provider resolves the hourly price of a plan in a zone.
type Provider interface {
	Price(plan, zone string) (float64, bool)
	UpdatePrices(ctx context.Context) error
}

type DefaultProvider struct {
	client sdk.UpCloudAPI

	mu sync.RWMutex
	// prices is keyed by zone and then by plan name.
	prices map[string]map[string]float64

	cm *pretty.ChangeMonitor
}

func NewDefaultProvider(client sdk.UpCloudAPI) *DefaultProvider {
	return &DefaultProvider{
		client: client,
		prices: map[string]map[string]float64{},
		cm:     pretty.NewChangeMonitor(),
	}
}

// Price returns the hourly price in euros for running plan in zone. The second return value is
// false when UpCloud does not price that plan in that zone, which is how plans that are simply not
// offered in a zone are identified: /plan is global, /price is per zone.
func (p *DefaultProvider) Price(plan, zone string) (float64, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	zonePrices, ok := p.prices[zone]
	if !ok {
		return 0, false
	}
	price, ok := zonePrices[plan]
	return price, ok
}

// Zones returns every zone UpCloud published prices for.
func (p *DefaultProvider) Zones() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	zones := make([]string, 0, len(p.prices))
	for zone := range p.prices {
		zones = append(zones, zone)
	}
	return zones
}

func (p *DefaultProvider) UpdatePrices(ctx context.Context) error {
	pricesByZone, err := p.client.GetPricesByZone(ctx)
	if err != nil {
		return fmt.Errorf("getting upcloud prices, %w", err)
	}

	prices := map[string]map[string]float64{}
	for zone, items := range *pricesByZone {
		for item, price := range items {
			plan, ok := strings.CutPrefix(item, planPricePrefix)
			if !ok {
				continue
			}
			if _, ok := prices[zone]; !ok {
				prices[zone] = map[string]float64{}
			}
			// Amount is the number of units the quoted price covers. It is 1 for every plan today,
			// but dividing keeps the value correct if UpCloud ever quotes plans in blocks.
			amount := float64(price.Amount)
			if amount <= 0 {
				amount = 1
			}
			prices[zone][plan] = price.Price / amount / creditsPerEuro
		}
	}
	if len(prices) == 0 {
		return fmt.Errorf("no server plan prices found in upcloud price list")
	}

	p.mu.Lock()
	p.prices = prices
	p.mu.Unlock()

	if p.cm.HasChanged("pricing", prices) {
		log.FromContext(ctx).WithValues("zones", len(prices)).V(1).Info("discovered pricing")
	}
	return nil
}

// Reset drops all cached prices. Used by tests.
func (p *DefaultProvider) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.prices = map[string]map[string]float64{}
}
