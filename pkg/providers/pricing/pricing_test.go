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

package pricing_test

import (
	"context"
	"math"
	"testing"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"

	"github.com/kubekanvas/karpenter-upcloud/pkg/providers/pricing"
	sdk "github.com/kubekanvas/karpenter-upcloud/pkg/upcloud"
)

// fakeAPI implements only the calls the pricing provider makes. Embedding the interface satisfies
// the rest of it; any accidental call panics loudly rather than silently returning a zero value.
type fakeAPI struct {
	sdk.UpCloudAPI

	prices upcloud.PricesByZone
	err    error
}

func (f *fakeAPI) GetPricesByZone(_ context.Context) (*upcloud.PricesByZone, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &f.prices, nil
}

func TestUpdatePrices(t *testing.T) {
	t.Parallel()

	client := &fakeAPI{prices: upcloud.PricesByZone{
		"fi-hel1": {
			"server_plan_2xCPU-4GB": upcloud.Price{Amount: 1, Price: 3.968},
			"server_plan_1xCPU-1GB": upcloud.Price{Amount: 1, Price: 0.744},
			// Non-plan entries share the zone and must be ignored.
			"server_core":   upcloud.Price{Amount: 1, Price: 1.5},
			"ipv4_address":  upcloud.Price{Amount: 1, Price: 0.4},
			"storage_maxio": upcloud.Price{Amount: 1, Price: 0.03},
		},
		"de-fra1": {
			"server_plan_2xCPU-4GB": upcloud.Price{Amount: 1, Price: 3.968},
		},
	}}
	provider := pricing.NewDefaultProvider(client)
	if err := provider.UpdatePrices(context.Background()); err != nil {
		t.Fatalf("UpdatePrices returned unexpected error: %v", err)
	}

	// UpCloud quotes credits per hour and 100 credits is one euro.
	price, ok := provider.Price("2xCPU-4GB", "fi-hel1")
	if !ok {
		t.Fatal("expected a price for 2xCPU-4GB in fi-hel1")
	}
	if math.Abs(price-0.03968) > 1e-9 {
		t.Errorf("price = %v, want 0.03968 EUR/hour", price)
	}
	if got, _ := provider.Price("1xCPU-1GB", "fi-hel1"); got >= price {
		t.Errorf("1xCPU-1GB (%v) should be cheaper than 2xCPU-4GB (%v)", got, price)
	}

	// A plan that is not sold in a zone must report as unpriced, so no offering is created for it.
	if _, ok := provider.Price("1xCPU-1GB", "de-fra1"); ok {
		t.Error("1xCPU-1GB is not priced in de-fra1 and should report as unavailable")
	}
	if _, ok := provider.Price("2xCPU-4GB", "us-nyc1"); ok {
		t.Error("an unknown zone should report as unavailable")
	}
	// Entries that are not server plans must not leak in as instance types.
	if _, ok := provider.Price("core", "fi-hel1"); ok {
		t.Error("server_core is not a plan and should not be priced as one")
	}
}

func TestUpdatePricesRejectsEmptyPriceList(t *testing.T) {
	t.Parallel()

	// Accepting an empty list would leave every offering unpriced and silently unschedulable, so
	// this has to be an error rather than a successful no-op.
	provider := pricing.NewDefaultProvider(&fakeAPI{prices: upcloud.PricesByZone{
		"fi-hel1": {"server_core": upcloud.Price{Amount: 1, Price: 1.5}},
	}})
	if err := provider.UpdatePrices(context.Background()); err == nil {
		t.Error("UpdatePrices should fail when the price list contains no server plans")
	}
}

func TestUpdatePricesKeepsLastGoodDataOnError(t *testing.T) {
	t.Parallel()

	client := &fakeAPI{prices: upcloud.PricesByZone{
		"fi-hel1": {"server_plan_2xCPU-4GB": upcloud.Price{Amount: 1, Price: 3.968}},
	}}
	provider := pricing.NewDefaultProvider(client)
	if err := provider.UpdatePrices(context.Background()); err != nil {
		t.Fatalf("UpdatePrices returned unexpected error: %v", err)
	}

	// A failed refresh must not blank out prices: doing so would make every offering unschedulable
	// until UpCloud recovered.
	client.err = context.DeadlineExceeded
	if err := provider.UpdatePrices(context.Background()); err == nil {
		t.Fatal("UpdatePrices should propagate the API error")
	}
	if _, ok := provider.Price("2xCPU-4GB", "fi-hel1"); !ok {
		t.Error("prices from the last successful refresh should survive a failed one")
	}
}
