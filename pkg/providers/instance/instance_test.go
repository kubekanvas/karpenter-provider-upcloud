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

package instance_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud/request"
	"github.com/patrickmn/go-cache"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/apis/v1alpha1"
	upcloudcache "github.com/kubekanvas/karpenter-provider-upcloud/pkg/cache"
	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/fake"
	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/providers/instance"
)

const testUUID = "00000000-1111-2222-3333-444444444444"

func newProvider(api *fake.UpCloudAPI) *instance.DefaultProvider {
	return instance.NewDefaultProvider(
		"test-cluster",
		api,
		upcloudcache.NewUnavailableOfferings(),
		cache.New(time.Minute, time.Minute),
	)
}

const testZone = "de-fra1"

func testInstanceType(name string, price float64, planStorageGB int) *cloudprovider.InstanceType {
	return &cloudprovider.InstanceType{
		Name: name,
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, name),
			scheduling.NewRequirement(v1alpha1.LabelInstanceStorageSize, corev1.NodeSelectorOpIn, itoa(planStorageGB)),
		),
		Offerings: cloudprovider.Offerings{{
			Requirements: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, testZone),
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
			),
			Price:     price,
			Available: true,
		}},
		Capacity: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("8Gi"),
			corev1.ResourcePods:   resource.MustParse("110"),
		},
		Overhead: &cloudprovider.InstanceTypeOverhead{},
	}
}

func itoa(i int) string { return resource.NewQuantity(int64(i), resource.DecimalSI).String() }

func testNodeClass() *v1alpha1.UpCloudNodeClass {
	return &v1alpha1.UpCloudNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: v1alpha1.UpCloudNodeClassSpec{
			Storage: v1alpha1.StorageSpec{Template: "tmpl-uuid"},
		},
		Status: v1alpha1.UpCloudNodeClassStatus{
			StorageTemplate: &v1alpha1.StorageTemplate{UUID: "tmpl-uuid", Size: 10},
		},
	}
}

func testNodeClaim() *karpv1.NodeClaim {
	return &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "default-abcde"}}
}

// --- Delete state machine ---------------------------------------------------------------------
//
// UpCloud refuses to delete a running server, and transitions it through "maintenance" while it
// stops. Karpenter retries Delete until it reports NodeClaimNotFound, so each state must either make
// progress or say why it cannot.

func TestDeleteStopsARunningServerWithoutDeletingIt(t *testing.T) {
	t.Parallel()

	api := fake.NewUpCloudAPI()
	api.AddServer(testUUID, "2xCPU-4GB", "de-fra1", upcloud.ServerStateStarted, nil)

	err := newProvider(api).Delete(context.Background(), testUUID)
	if err == nil {
		t.Fatal("Delete should report that the server is still stopping, so Karpenter retries")
	}
	if len(api.StopServerCalls) != 1 {
		t.Fatalf("expected exactly one stop call, got %d", len(api.StopServerCalls))
	}
	// A graceful stop leaves the server in maintenance for ~90s per node while the API refuses to
	// delete it, so the stop must be hard.
	if got := api.StopServerCalls[0].StopType; got != request.ServerStopTypeHard {
		t.Errorf("stop type = %q, want %q", got, request.ServerStopTypeHard)
	}
	if len(api.DeleteCalls) != 0 {
		t.Errorf("a running server must not be deleted before it has stopped, got %d delete calls", len(api.DeleteCalls))
	}
}

func TestDeleteWaitsWhileServerIsInMaintenance(t *testing.T) {
	t.Parallel()

	api := fake.NewUpCloudAPI()
	api.AddServer(testUUID, "2xCPU-4GB", "de-fra1", upcloud.ServerStateMaintenance, nil)

	err := newProvider(api).Delete(context.Background(), testUUID)
	if err == nil {
		t.Fatal("Delete should report that the server is in maintenance")
	}
	if len(api.DeleteCalls) != 0 {
		t.Errorf("UpCloud rejects deleting a server in maintenance; got %d delete calls", len(api.DeleteCalls))
	}
	if len(api.StopServerCalls) != 0 {
		t.Errorf("a server already stopping must not be stopped again, got %d stop calls", len(api.StopServerCalls))
	}
}

func TestDeleteRemovesAStoppedServerAndItsStorages(t *testing.T) {
	t.Parallel()

	api := fake.NewUpCloudAPI()
	api.AddServer(testUUID, "2xCPU-4GB", "de-fra1", upcloud.ServerStateStopped, nil)

	if err := newProvider(api).Delete(context.Background(), testUUID); err != nil {
		t.Fatalf("Delete of a stopped server returned %v", err)
	}
	if len(api.DeleteCalls) != 1 {
		t.Fatalf("expected one delete call, got %d", len(api.DeleteCalls))
	}
	// The root disk is created by this controller and has no life of its own; leaving it behind
	// keeps billing after the node is gone.
	if api.DeleteCalls[0].UUID != testUUID {
		t.Errorf("deleted %q, want %q", api.DeleteCalls[0].UUID, testUUID)
	}
}

func TestDeleteReportsNotFoundForAVanishedServer(t *testing.T) {
	t.Parallel()

	api := fake.NewUpCloudAPI() // no servers registered

	err := newProvider(api).Delete(context.Background(), testUUID)
	// Karpenter only stops retrying Delete when it sees this specific error, so a plain error here
	// would loop forever on an already-deleted server.
	if !cloudprovider.IsNodeClaimNotFoundError(err) {
		t.Fatalf("Delete of a missing server returned %v, want a NodeClaimNotFoundError", err)
	}
}

// --- Create -----------------------------------------------------------------------------------

func TestCreateOmitsSourceIPFilteringUnlessRequested(t *testing.T) {
	t.Parallel()

	api := fake.NewUpCloudAPI()
	nodeClass := testNodeClass()
	nodeClass.Spec.Network = &v1alpha1.NetworkSpec{
		Interfaces: []v1alpha1.NetworkInterface{{Type: "public"}, {Type: "utility"}},
	}

	_, err := newProvider(api).Create(context.Background(), nodeClass, testNodeClaim(), nil,
		[]*cloudprovider.InstanceType{testInstanceType("2xCPU-4GB", 0.04, 80)})
	if err != nil {
		t.Fatalf("Create returned %v", err)
	}
	if len(api.CreateServerCalls) != 1 {
		t.Fatalf("expected one create call, got %d", len(api.CreateServerCalls))
	}
	// UpCloud's Boolean distinguishes unset from false, and an explicit false is rejected with
	// SOURCE_IP_FILTERING_INVALID on a public interface, failing every launch.
	for _, iface := range api.CreateServerCalls[0].Networking.Interfaces {
		if iface.SourceIPFiltering != upcloud.Empty {
			t.Errorf("interface %q sent SourceIPFiltering=%v, want it unset", iface.Type, iface.SourceIPFiltering)
		}
	}
}

func TestCreateSizesRootDiskForPlansThatBundleNoStorage(t *testing.T) {
	t.Parallel()

	// Every CLOUDNATIVE-* and GPU-* plan reports storage_size 0 — 102 of UpCloud's 174 plans.
	api := fake.NewUpCloudAPI()
	_, err := newProvider(api).Create(context.Background(), testNodeClass(), testNodeClaim(), nil,
		[]*cloudprovider.InstanceType{testInstanceType("CLOUDNATIVE-2xCPU-4GB", 0.05, 0)})
	if err != nil {
		t.Fatalf("Create returned %v", err)
	}
	got := api.CreateServerCalls[0].StorageDevices[0].Size
	if got != v1alpha1.DefaultRootDiskGB {
		t.Errorf("root disk = %dGB, want the %dGB default (a zero-storage plan must not yield a template-floor disk)",
			got, v1alpha1.DefaultRootDiskGB)
	}
}

func TestCreateHonoursExplicitStorageSize(t *testing.T) {
	t.Parallel()

	api := fake.NewUpCloudAPI()
	nodeClass := testNodeClass()
	size := 120
	nodeClass.Spec.Storage.Size = &size

	if _, err := newProvider(api).Create(context.Background(), nodeClass, testNodeClaim(), nil,
		[]*cloudprovider.InstanceType{testInstanceType("2xCPU-4GB", 0.04, 80)}); err != nil {
		t.Fatalf("Create returned %v", err)
	}
	if got := api.CreateServerCalls[0].StorageDevices[0].Size; got != size {
		t.Errorf("root disk = %dGB, want the explicit %dGB", got, size)
	}
}

func TestCreatePicksTheCheapestAvailableOffering(t *testing.T) {
	t.Parallel()

	api := fake.NewUpCloudAPI()
	cheap := testInstanceType("2xCPU-4GB", 0.04, 80)
	pricey := testInstanceType("8xCPU-32GB", 0.31, 160)

	if _, err := newProvider(api).Create(context.Background(), testNodeClass(), testNodeClaim(), nil,
		[]*cloudprovider.InstanceType{pricey, cheap}); err != nil {
		t.Fatalf("Create returned %v", err)
	}
	if got := api.CreateServerCalls[0].Plan; got != cheap.Name {
		t.Errorf("launched plan %q, want the cheaper %q", got, cheap.Name)
	}
}

func TestCreateToleratesUnknownRequirementKeysOnTheNodeClaim(t *testing.T) {
	t.Parallel()

	// Karpenter stamps <group>/<nodeclass-kind> onto every NodeClaim template without registering it
	// as a well-known label. Filtering instance types through Requirements.Compatible rejected every
	// one of them and surfaced as insufficient capacity on a cluster with ample capacity.
	api := fake.NewUpCloudAPI()
	nodeClaim := testNodeClaim()
	nodeClaim.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{{
		Key:      "karpenter.k8s.upcloud/upcloudnodeclass",
		Operator: corev1.NodeSelectorOpIn,
		Values:   []string{"default"},
	}}

	if _, err := newProvider(api).Create(context.Background(), testNodeClass(), nodeClaim, nil,
		[]*cloudprovider.InstanceType{testInstanceType("2xCPU-4GB", 0.04, 80)}); err != nil {
		t.Fatalf("Create returned %v, want a successful launch", err)
	}
}

func TestCreateReportsInsufficientCapacityDistinctly(t *testing.T) {
	t.Parallel()

	api := fake.NewUpCloudAPI()
	api.CreateServerErr = fake.Problem(400, upcloud.ErrCodeServerResourcesUnavailable)

	_, err := newProvider(api).Create(context.Background(), testNodeClass(), testNodeClaim(), nil,
		[]*cloudprovider.InstanceType{testInstanceType("2xCPU-4GB", 0.04, 80)})
	// Karpenter reacts to ICE by trying a different offering; conflating it with a generic failure
	// would make a bad template UUID look like a capacity shortage, and vice versa.
	var ice *cloudprovider.InsufficientCapacityError
	if !errors.As(err, &ice) {
		t.Fatalf("Create returned %v, want an InsufficientCapacityError", err)
	}
}

// --- List -------------------------------------------------------------------------------------

func TestListFiltersByClusterScopedLabel(t *testing.T) {
	t.Parallel()

	api := fake.NewUpCloudAPI()
	api.AddServer(testUUID, "2xCPU-4GB", "de-fra1", upcloud.ServerStateStarted, map[string]string{
		v1alpha1.LabelKeyManagedBy: "test-cluster",
	})

	instances, err := newProvider(api).List(context.Background())
	if err != nil {
		t.Fatalf("List returned %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("List returned %d instances, want 1", len(instances))
	}
	// Without a cluster-scoped filter, two clusters sharing an UpCloud account would garbage collect
	// each other's nodes.
	if len(api.ListFilterCalls) != 1 || len(api.ListFilterCalls[0]) != 1 {
		t.Fatalf("expected one server-side label filter, got %v", api.ListFilterCalls)
	}
	filter, ok := api.ListFilterCalls[0][0].(request.FilterLabel)
	if !ok {
		t.Fatalf("filter is %T, want request.FilterLabel", api.ListFilterCalls[0][0])
	}
	if filter.Key != v1alpha1.LabelKeyManagedBy || filter.Value != "test-cluster" {
		t.Errorf("filter = %s=%s, want %s=test-cluster", filter.Key, filter.Value, v1alpha1.LabelKeyManagedBy)
	}
}
