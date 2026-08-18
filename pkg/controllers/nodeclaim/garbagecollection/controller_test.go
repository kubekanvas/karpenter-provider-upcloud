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

package garbagecollection_test

import (
	"context"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/apis/v1alpha1"
	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/controllers/nodeclaim/garbagecollection"
	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/utils"
)

// stubCloudProvider reports a fixed set of cloud-side NodeClaims and records what was deleted.
type stubCloudProvider struct {
	cloudprovider.CloudProvider

	instances []*karpv1.NodeClaim
	deleted   []string
}

func (s *stubCloudProvider) List(_ context.Context) ([]*karpv1.NodeClaim, error) {
	return s.instances, nil
}

func (s *stubCloudProvider) Delete(_ context.Context, nc *karpv1.NodeClaim) error {
	s.deleted = append(s.deleted, nc.Status.ProviderID)
	return nil
}

func (s *stubCloudProvider) Name() string { return "upcloud" }

func (s *stubCloudProvider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&v1alpha1.UpCloudNodeClass{}}
}

func (s *stubCloudProvider) GetInstanceTypes(_ context.Context, _ *karpv1.NodePool) ([]*cloudprovider.InstanceType, error) {
	return nil, nil
}

func (s *stubCloudProvider) IsDrifted(_ context.Context, _ *karpv1.NodeClaim) (cloudprovider.DriftReason, error) {
	return "", nil
}

func (s *stubCloudProvider) RepairPolicies() []cloudprovider.RepairPolicy { return nil }

func cloudInstance(providerID string, age time.Duration) *karpv1.NodeClaim {
	return &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "cloud-" + providerID,
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-age)},
		},
		Status: karpv1.NodeClaimStatus{ProviderID: providerID},
	}
}

// Both karpenter's v1 types and this provider's v1alpha1 types register themselves into client-go's
// global scheme from an init, so the fake client is built from that rather than a fresh one.
func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	return scheme.Scheme
}

func reconcile(t *testing.T, cp *stubCloudProvider, objs ...client.Object) {
	t.Helper()
	kubeClient := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).Build()
	if _, err := garbagecollection.NewController(kubeClient, cp).Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile returned %v", err)
	}
}

func TestOrphanedServerIsReaped(t *testing.T) {
	t.Parallel()

	// A server with no NodeClaim keeps billing forever; reaping it is the whole point of this
	// controller.
	providerID := utils.ProviderID("00000000-1111-2222-3333-444444444444")
	cp := &stubCloudProvider{instances: []*karpv1.NodeClaim{cloudInstance(providerID, time.Hour)}}

	reconcile(t, cp)

	if len(cp.deleted) != 1 || cp.deleted[0] != providerID {
		t.Errorf("deleted %v, want exactly [%s]", cp.deleted, providerID)
	}
}

func TestJustLaunchedServerIsProtectedByTheGracePeriod(t *testing.T) {
	t.Parallel()

	// This is the window that protects a server whose NodeClaim the controller has not observed
	// yet. It reads the creation time off an UpCloud label, so a regression here deletes live nodes
	// seconds after they launch.
	providerID := utils.ProviderID("00000000-1111-2222-3333-555555555555")
	cp := &stubCloudProvider{instances: []*karpv1.NodeClaim{cloudInstance(providerID, 10*time.Second)}}

	reconcile(t, cp)

	if len(cp.deleted) != 0 {
		t.Errorf("deleted %v, want nothing — the server is inside the grace period", cp.deleted)
	}
}

func TestServerWithALiveNodeClaimIsLeftAlone(t *testing.T) {
	t.Parallel()

	providerID := utils.ProviderID("00000000-1111-2222-3333-666666666666")
	cp := &stubCloudProvider{instances: []*karpv1.NodeClaim{cloudInstance(providerID, time.Hour)}}

	nodeClaim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "default-abcde",
			Labels: map[string]string{karpv1.NodePoolLabelKey: "default"},
		},
		Spec: karpv1.NodeClaimSpec{
			NodeClassRef: &karpv1.NodeClassReference{
				Group: "karpenter.k8s.upcloud",
				Kind:  "UpCloudNodeClass",
				Name:  "default",
			},
		},
		Status: karpv1.NodeClaimStatus{ProviderID: providerID},
	}

	reconcile(t, cp, nodeClaim)

	if len(cp.deleted) != 0 {
		t.Errorf("deleted %v, want nothing — the server has a live NodeClaim", cp.deleted)
	}
}

func TestTerminatingCloudInstanceIsSkipped(t *testing.T) {
	t.Parallel()

	// Karpenter is already tearing this one down; a second Delete would race its own termination.
	providerID := utils.ProviderID("00000000-1111-2222-3333-777777777777")
	terminating := cloudInstance(providerID, time.Hour)
	terminating.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	cp := &stubCloudProvider{instances: []*karpv1.NodeClaim{terminating}}

	reconcile(t, cp)

	if len(cp.deleted) != 0 {
		t.Errorf("deleted %v, want nothing — the instance is already terminating", cp.deleted)
	}
}
