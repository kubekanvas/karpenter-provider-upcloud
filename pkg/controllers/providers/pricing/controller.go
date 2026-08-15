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

// Package pricing periodically refreshes UpCloud's price list.
package pricing

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/karpenter/pkg/operator/injection"

	"github.com/kubekanvas/karpenter-upcloud/pkg/providers/pricing"
)

// refreshPeriod controls how quickly a price change is picked up. UpCloud prices are stable, but
// this also picks up plans becoming available in new zones, which is worth noticing within a day.
const refreshPeriod = 12 * time.Hour

type Controller struct {
	pricingProvider *pricing.DefaultProvider
}

func NewController(pricingProvider *pricing.DefaultProvider) *Controller {
	return &Controller{pricingProvider: pricingProvider}
}

func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = injection.WithControllerName(ctx, "providers.pricing")

	if err := c.pricingProvider.UpdatePrices(ctx); err != nil {
		return reconciler.Result{}, fmt.Errorf("updating prices, %w", err)
	}
	return reconciler.Result{RequeueAfter: refreshPeriod}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("providers.pricing").
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}
