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

//go:build e2e

// Package e2e exercises the provider against a real UpCloud account.
//
// These tests create and destroy billable infrastructure, so they are behind a build tag and skip
// unless credentials are present. Run them with:
//
//	make test-e2e
//
// Every unit test in this repository stubs the UpCloud API, which is exactly why three launch-path
// bugs reached a live cluster undetected: an explicit SourceIPFiltering=false rejected every create,
// instance-type filtering rejected every plan, and a soft stop left servers in "maintenance" for
// ~90s. This suite is where that class of bug is meant to surface.
package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/patrickmn/go-cache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/apis/v1alpha1"
	upcloudcache "github.com/kubekanvas/karpenter-provider-upcloud/pkg/cache"
	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/providers/instance"
	sdk "github.com/kubekanvas/karpenter-provider-upcloud/pkg/upcloud"
)

const (
	// envZone selects where servers are launched. There is no safe default: launching into the
	// wrong zone would create servers an operator is not watching.
	envZone = "UPCLOUD_E2E_ZONE"
	// envTemplate is the storage template UUID to clone.
	envTemplate = "UPCLOUD_E2E_TEMPLATE"
	// envPlan pins the plan, so a test run cannot accidentally provision an expensive GPU node.
	envPlan = "UPCLOUD_E2E_PLAN"
)

// testEnv holds a live client plus the settings the tests launch with.
type testEnv struct {
	client   sdk.UpCloudAPI
	zone     string
	template string
	plan     string
}

// setup skips the test unless the environment is fully configured. Skipping rather than failing
// keeps `go test ./...` green for anyone without an UpCloud account.
func setup(t *testing.T) testEnv {
	t.Helper()

	config, err := sdk.ClientConfigFromEnv()
	if err != nil {
		t.Skipf("skipping live e2e: %v", err)
	}
	zone, template := os.Getenv(envZone), os.Getenv(envTemplate)
	if zone == "" || template == "" {
		t.Skipf("skipping live e2e: set %s and %s", envZone, envTemplate)
	}
	plan := os.Getenv(envPlan)
	if plan == "" {
		plan = "1xCPU-1GB"
	}

	client, err := sdk.NewClient(config, "karpenter-provider-upcloud-e2e")
	if err != nil {
		t.Fatalf("creating UpCloud client: %v", err)
	}
	return testEnv{client: client, zone: zone, template: template, plan: plan}
}

func (e testEnv) instanceProvider() *instance.DefaultProvider {
	return instance.NewDefaultProvider(
		"karpenter-e2e",
		e.client,
		upcloudcache.NewUnavailableOfferings(),
		cache.New(time.Minute, time.Minute),
	)
}

// TestServerLifecycle launches a real server and terminates it, asserting the two things the unit
// tests cannot: that UpCloud accepts the create request this provider builds, and that termination
// leaves nothing behind to bill for.
func TestServerLifecycle(t *testing.T) {
	env := setup(t)
	ctx := context.Background()
	provider := env.instanceProvider()

	nodeClass := &v1alpha1.UpCloudNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e"},
		Spec: v1alpha1.UpCloudNodeClassSpec{
			Zones:   []string{env.zone},
			Storage: v1alpha1.StorageSpec{Template: env.template},
			Network: &v1alpha1.NetworkSpec{
				Interfaces: []v1alpha1.NetworkInterface{{Type: "public"}, {Type: "utility"}},
			},
		},
		Status: v1alpha1.UpCloudNodeClassStatus{
			Zones:           []v1alpha1.Zone{{ID: env.zone}},
			StorageTemplate: &v1alpha1.StorageTemplate{UUID: env.template},
		},
	}
	nodeClaim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-" + time.Now().Format("150405")},
	}

	launched, err := provider.Create(ctx, nodeClass, nodeClaim,
		map[string]string{v1alpha1.LabelKeyManagedBy: "karpenter-e2e"},
		[]*cloudprovider.InstanceType{e2eInstanceType(env.plan, env.zone)})
	if err != nil {
		t.Fatalf("Create returned %v", err)
	}
	t.Logf("launched %s (%s) in %s", launched.UUID, launched.Plan, launched.Zone)

	// Always attempt teardown, even if an assertion below fails: a leaked server bills hourly.
	t.Cleanup(func() {
		if err := terminate(context.Background(), provider, launched.UUID); err != nil {
			t.Errorf("LEAKED SERVER %s: %v", launched.UUID, err)
		}
	})

	if launched.Zone != env.zone {
		t.Errorf("server launched in %q, want %q", launched.Zone, env.zone)
	}
	if launched.Plan != env.plan {
		t.Errorf("server launched with plan %q, want %q", launched.Plan, env.plan)
	}

	// The managed-by label is what List filters on; without it the server is invisible to garbage
	// collection and would leak if its NodeClaim ever vanished.
	if got := launched.Labels[v1alpha1.LabelKeyManagedBy]; got != "karpenter-e2e" {
		t.Errorf("managed-by label = %q, want %q", got, "karpenter-e2e")
	}
}

// terminate drives Delete to completion. Karpenter's contract is to retry until NodeClaimNotFound,
// and this mirrors that loop so the test tears the server down the same way production does.
func terminate(ctx context.Context, provider *instance.DefaultProvider, uuid string) error {
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		err := provider.Delete(ctx, uuid)
		if err == nil {
			continue // deletion issued; loop once more to confirm it is gone
		}
		if cloudprovider.IsNodeClaimNotFoundError(err) {
			return nil
		}
		time.Sleep(10 * time.Second)
	}
	return context.DeadlineExceeded
}
