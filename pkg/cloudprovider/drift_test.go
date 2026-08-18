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

package cloudprovider_test

import (
	"context"
	"testing"
	"time"

	upcloudapi "github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/patrickmn/go-cache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcp "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/apis/v1alpha1"
	upcloudcache "github.com/kubekanvas/karpenter-provider-upcloud/pkg/cache"
	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/cloudprovider"
	upcloudfake "github.com/kubekanvas/karpenter-provider-upcloud/pkg/fake"
	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/providers/instance"
	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/utils"
)

const driftUUID = "00000000-1111-2222-3333-888888888888"

type noopRecorder struct{}

func (noopRecorder) Publish(...events.Event) {}

// driftFixtures builds a NodeClass/NodePool/NodeClaim trio plus a live server, and returns what
// IsDrifted makes of them. The NodeClass always carries the current hash version, since that is what
// a reconciled NodeClass has; claimHashVersion is what varies.
func driftFixtures(t *testing.T, nodeClassHash, claimHash, claimHashVersion, serverZone string, zones []string) karpcp.DriftReason {
	t.Helper()

	nodeClass := &v1alpha1.UpCloudNodeClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "default",
			Annotations: map[string]string{
				v1alpha1.AnnotationUpCloudNodeClassHash:        nodeClassHash,
				v1alpha1.AnnotationUpCloudNodeClassHashVersion: v1alpha1.UpCloudNodeClassHashVersion,
			},
		},
		Status: v1alpha1.UpCloudNodeClassStatus{},
	}
	for _, z := range zones {
		nodeClass.Status.Zones = append(nodeClass.Status.Zones, v1alpha1.Zone{ID: z})
	}

	nodePool := &karpv1.NodePool{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: karpv1.NodePoolSpec{
			Template: karpv1.NodeClaimTemplate{
				Spec: karpv1.NodeClaimTemplateSpec{
					NodeClassRef: &karpv1.NodeClassReference{
						Group: "karpenter.k8s.upcloud", Kind: "UpCloudNodeClass", Name: "default",
					},
				},
			},
		},
	}

	nodeClaim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "default-abcde",
			Labels: map[string]string{karpv1.NodePoolLabelKey: "default"},
			Annotations: map[string]string{
				v1alpha1.AnnotationUpCloudNodeClassHash:        claimHash,
				v1alpha1.AnnotationUpCloudNodeClassHashVersion: claimHashVersion,
			},
		},
		Status: karpv1.NodeClaimStatus{ProviderID: utils.ProviderID(driftUUID)},
	}

	api := upcloudfake.NewUpCloudAPI()
	api.AddServer(driftUUID, "2xCPU-4GB", serverZone, upcloudapi.ServerStateStarted, nil)

	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(nodeClass, nodePool, nodeClaim).
		Build()

	cp := cloudprovider.New(
		nil, // instance types are not consulted on the drift path
		instance.NewDefaultProvider("test-cluster", api, upcloudcache.NewUnavailableOfferings(),
			cache.New(time.Minute, time.Minute)),
		noopRecorder{},
		kubeClient,
	)

	reason, err := cp.IsDrifted(context.Background(), nodeClaim)
	if err != nil {
		t.Fatalf("IsDrifted returned %v", err)
	}
	return reason
}

func TestMatchingHashIsNotDrift(t *testing.T) {
	t.Parallel()

	got := driftFixtures(t, "abc", "abc", v1alpha1.UpCloudNodeClassHashVersion, "de-fra1", []string{"de-fra1"})
	if got != "" {
		t.Errorf("drift reason = %q, want none", got)
	}
}

func TestChangedHashIsNodeClassDrift(t *testing.T) {
	t.Parallel()

	got := driftFixtures(t, "new", "old", v1alpha1.UpCloudNodeClassHashVersion, "de-fra1", []string{"de-fra1"})
	if got != cloudprovider.NodeClassDrift {
		t.Errorf("drift reason = %q, want %q", got, cloudprovider.NodeClassDrift)
	}
}

func TestHashVersionMismatchIsNotDrift(t *testing.T) {
	t.Parallel()

	// Hashes produced by different algorithm versions are not comparable. Treating the difference as
	// drift would replace every node in the cluster on a controller upgrade.
	got := driftFixtures(t, "new", "old", "v0", "de-fra1", []string{"de-fra1"})
	if got != "" {
		t.Errorf("drift reason = %q, want none across a hash version change", got)
	}
}

func TestServerOutsideTheNodeClassZonesIsZoneDrift(t *testing.T) {
	t.Parallel()

	// Removing a zone from spec.zones is otherwise invisible: the zone lives on the server, not in
	// the launch request's hash.
	got := driftFixtures(t, "abc", "abc", v1alpha1.UpCloudNodeClassHashVersion, "fi-hel1", []string{"de-fra1"})
	if got != cloudprovider.ZoneDrift {
		t.Errorf("drift reason = %q, want %q", got, cloudprovider.ZoneDrift)
	}
}

func TestUnreconciledNodeClassIsNotZoneDrift(t *testing.T) {
	t.Parallel()

	// With no resolved zones yet there is nothing to compare against, and reporting drift would
	// churn nodes while the NodeClass is still being reconciled.
	got := driftFixtures(t, "abc", "abc", v1alpha1.UpCloudNodeClassHashVersion, "fi-hel1", nil)
	if got != "" {
		t.Errorf("drift reason = %q, want none while the NodeClass has no resolved zones", got)
	}
}
