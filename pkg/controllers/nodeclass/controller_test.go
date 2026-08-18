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

package nodeclass_test

import (
	"context"
	"testing"
	"time"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/patrickmn/go-cache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/apis/v1alpha1"
	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/controllers/nodeclass"
	upcloudfake "github.com/kubekanvas/karpenter-provider-upcloud/pkg/fake"
)

const templateUUID = "01000000-0000-4000-8000-000030220200"

func newAPI() *upcloudfake.UpCloudAPI {
	api := upcloudfake.NewUpCloudAPI()
	api.Zones = []upcloud.Zone{
		{ID: "de-fra1", Description: "Frankfurt #1"},
		{ID: "fi-hel1", Description: "Helsinki #1"},
	}
	api.Storages[templateUUID] = upcloud.StorageDetails{
		Storage: upcloud.Storage{
			UUID:  templateUUID,
			Title: "Ubuntu Server 22.04 LTS (Jammy Jellyfish)",
			Type:  upcloud.StorageTypeTemplate,
			Size:  10,
		},
	}
	return api
}

func nodeClassWith(spec *v1alpha1.UpCloudNodeClassSpec) *v1alpha1.UpCloudNodeClass {
	return &v1alpha1.UpCloudNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec:       *spec,
	}
}

func reconcileClass(t *testing.T, api *upcloudfake.UpCloudAPI, nc *v1alpha1.UpCloudNodeClass) *v1alpha1.UpCloudNodeClass {
	t.Helper()

	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(nc).
		WithStatusSubresource(nc).
		Build()

	c := nodeclass.NewController(kubeClient, api, "de-fra1",
		cache.New(time.Minute, time.Minute), true /* disableDryRun */)
	if _, err := c.Reconcile(context.Background(), nc); err != nil {
		// A resolution failure is reported through conditions, not the error, unless it is
		// retryable — so an error here is itself worth surfacing.
		t.Logf("Reconcile returned %v", err)
	}
	return nc
}

func condition(nc *v1alpha1.UpCloudNodeClass, t string) metav1.ConditionStatus {
	c := nc.StatusConditions().Get(t)
	if c == nil {
		return metav1.ConditionUnknown
	}
	return c.Status
}

func TestZonesResolveFromSpec(t *testing.T) {
	t.Parallel()

	nc := reconcileClass(t, newAPI(), nodeClassWith(&v1alpha1.UpCloudNodeClassSpec{
		Zones:   []string{"de-fra1", "fi-hel1"},
		Storage: v1alpha1.StorageSpec{Template: templateUUID},
	}))

	if got := condition(nc, v1alpha1.ConditionTypeZonesReady); got != metav1.ConditionTrue {
		t.Fatalf("ZonesReady = %s, want True", got)
	}
	if len(nc.Status.Zones) != 2 {
		t.Fatalf("resolved %d zones, want 2", len(nc.Status.Zones))
	}
	if nc.Status.Zones[0].Description != "Frankfurt #1" {
		t.Errorf("zone description = %q, want it carried through from the API", nc.Status.Zones[0].Description)
	}
}

func TestZonesFallBackToTheClusterZone(t *testing.T) {
	t.Parallel()

	nc := reconcileClass(t, newAPI(), nodeClassWith(&v1alpha1.UpCloudNodeClassSpec{
		Storage: v1alpha1.StorageSpec{Template: templateUUID},
	}))

	if got := condition(nc, v1alpha1.ConditionTypeZonesReady); got != metav1.ConditionTrue {
		t.Fatalf("ZonesReady = %s, want True", got)
	}
	if len(nc.Status.Zones) != 1 || nc.Status.Zones[0].ID != "de-fra1" {
		t.Errorf("resolved zones = %v, want just the controller's cluster zone", nc.Status.Zones)
	}
}

func TestUnknownZoneIsRejected(t *testing.T) {
	t.Parallel()

	// A typo must surface as a NotReady NodeClass rather than as launches that fail later.
	nc := reconcileClass(t, newAPI(), nodeClassWith(&v1alpha1.UpCloudNodeClassSpec{
		Zones:   []string{"de-fra1", "xx-nope1"},
		Storage: v1alpha1.StorageSpec{Template: templateUUID},
	}))

	if got := condition(nc, v1alpha1.ConditionTypeZonesReady); got != metav1.ConditionFalse {
		t.Errorf("ZonesReady = %s, want False for an unknown zone", got)
	}
	if len(nc.Status.Zones) != 0 {
		t.Errorf("status kept %d zones, want none when resolution failed", len(nc.Status.Zones))
	}
}

func TestStorageTemplateResolves(t *testing.T) {
	t.Parallel()

	nc := reconcileClass(t, newAPI(), nodeClassWith(&v1alpha1.UpCloudNodeClassSpec{
		Storage: v1alpha1.StorageSpec{Template: templateUUID},
	}))

	if got := condition(nc, v1alpha1.ConditionTypeStorageTemplateReady); got != metav1.ConditionTrue {
		t.Fatalf("StorageTemplateReady = %s, want True", got)
	}
	if nc.Status.StorageTemplate == nil || nc.Status.StorageTemplate.Size != 10 {
		t.Errorf("storage template = %+v, want the resolved size recorded", nc.Status.StorageTemplate)
	}
}

func TestMissingStorageTemplateIsRejected(t *testing.T) {
	t.Parallel()

	nc := reconcileClass(t, newAPI(), nodeClassWith(&v1alpha1.UpCloudNodeClassSpec{
		Storage: v1alpha1.StorageSpec{Template: "00000000-dead-beef-0000-000000000000"},
	}))

	if got := condition(nc, v1alpha1.ConditionTypeStorageTemplateReady); got != metav1.ConditionFalse {
		t.Errorf("StorageTemplateReady = %s, want False for a template that does not exist", got)
	}
}

func TestNonTemplateStorageIsRejected(t *testing.T) {
	t.Parallel()

	// Cloning a plain disk instead of a template would succeed at the API level and produce a node
	// with someone else's filesystem on it.
	api := newAPI()
	api.Storages["not-a-template"] = upcloud.StorageDetails{
		Storage: upcloud.Storage{UUID: "not-a-template", Type: upcloud.StorageTypeNormal, Size: 10},
	}

	nc := reconcileClass(t, api, nodeClassWith(&v1alpha1.UpCloudNodeClassSpec{
		Storage: v1alpha1.StorageSpec{Template: "not-a-template"},
	}))

	if got := condition(nc, v1alpha1.ConditionTypeStorageTemplateReady); got != metav1.ConditionFalse {
		t.Errorf("StorageTemplateReady = %s, want False for a non-template storage", got)
	}
}

func TestNetworkWithoutAPrivateAddressIsRejected(t *testing.T) {
	t.Parallel()

	// UpCloud's cloud-controller-manager refuses to initialize a node with no private address, which
	// leaves it tainted as uninitialized forever.
	nc := reconcileClass(t, newAPI(), nodeClassWith(&v1alpha1.UpCloudNodeClassSpec{
		Storage: v1alpha1.StorageSpec{Template: templateUUID},
		Network: &v1alpha1.NetworkSpec{
			Interfaces: []v1alpha1.NetworkInterface{{Type: "public"}},
		},
	}))

	if got := condition(nc, v1alpha1.ConditionTypeValidationSucceeded); got != metav1.ConditionFalse {
		t.Errorf("ValidationSucceeded = %s, want False when no utility or private interface is present", got)
	}
}

func TestReservedLabelKeyIsRejected(t *testing.T) {
	t.Parallel()

	// Overwriting the managed-by label would hide the server from this cluster's garbage collection.
	nc := reconcileClass(t, newAPI(), nodeClassWith(&v1alpha1.UpCloudNodeClassSpec{
		Storage: v1alpha1.StorageSpec{Template: templateUUID},
		Labels:  map[string]string{v1alpha1.LabelKeyManagedBy: "someone-else"},
	}))

	if got := condition(nc, v1alpha1.ConditionTypeValidationSucceeded); got != metav1.ConditionFalse {
		t.Errorf("ValidationSucceeded = %s, want False for a Karpenter-managed label key", got)
	}
}

func TestFullyValidNodeClassGoesReady(t *testing.T) {
	t.Parallel()

	nc := reconcileClass(t, newAPI(), nodeClassWith(&v1alpha1.UpCloudNodeClassSpec{
		Zones:   []string{"de-fra1"},
		Storage: v1alpha1.StorageSpec{Template: templateUUID},
		Network: &v1alpha1.NetworkSpec{
			Interfaces: []v1alpha1.NetworkInterface{{Type: "public"}, {Type: "utility"}},
		},
		Labels: map[string]string{"team": "platform"},
	}))

	for _, cond := range []string{
		v1alpha1.ConditionTypeZonesReady,
		v1alpha1.ConditionTypeStorageTemplateReady,
		v1alpha1.ConditionTypeValidationSucceeded,
	} {
		if got := condition(nc, cond); got != metav1.ConditionTrue {
			t.Errorf("%s = %s, want True", cond, got)
		}
	}
}
