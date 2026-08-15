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

package utils_test

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/kubekanvas/karpenter-upcloud/pkg/apis/v1alpha1"
	"github.com/kubekanvas/karpenter-upcloud/pkg/utils"
)

func TestParseInstanceID(t *testing.T) {
	t.Parallel()

	// The four-slash prefix is what the UpCloud cloud-controller-manager writes; a NodeClaim whose
	// providerID uses any other shape would silently fail to match its Node.
	for _, tc := range []struct {
		name       string
		providerID string
		want       string
		wantErr    bool
	}{
		{name: "ccm format", providerID: "upcloud:////00fa1b2c-3d4e-5f60-7182-93a4b5c6d7e8", want: "00fa1b2c-3d4e-5f60-7182-93a4b5c6d7e8"},
		{name: "two slashes is not the ccm format", providerID: "upcloud://00fa1b2c", wantErr: true},
		{name: "other provider", providerID: "aws:///us-east-1a/i-0123", wantErr: true},
		{name: "prefix with no uuid", providerID: "upcloud:////", wantErr: true},
		{name: "empty", providerID: "", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := utils.ParseInstanceID(tc.providerID)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseInstanceID(%q) = %q, want error", tc.providerID, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseInstanceID(%q) returned unexpected error: %v", tc.providerID, err)
			}
			if got != tc.want {
				t.Errorf("ParseInstanceID(%q) = %q, want %q", tc.providerID, got, tc.want)
			}
		})
	}
}

func TestProviderIDRoundTrips(t *testing.T) {
	t.Parallel()

	const uuid = "00fa1b2c-3d4e-5f60-7182-93a4b5c6d7e8"
	got, err := utils.ParseInstanceID(utils.ProviderID(uuid))
	if err != nil {
		t.Fatalf("round trip returned unexpected error: %v", err)
	}
	if got != uuid {
		t.Errorf("round trip = %q, want %q", got, uuid)
	}
}

func TestValidateLabels(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		labels  map[string]string
		wantErr bool
	}{
		{name: "nil", labels: nil},
		{name: "ordinary", labels: map[string]string{"team": "platform", "env": "prod"}},
		{name: "key too short", labels: map[string]string{"a": "b"}, wantErr: true},
		{name: "key too long", labels: map[string]string{strings.Repeat("k", 33): "v"}, wantErr: true},
		{name: "key starting with underscore", labels: map[string]string{"_reserved": "v"}, wantErr: true},
		{name: "non ascii key", labels: map[string]string{"täg": "v"}, wantErr: true},
		{name: "value too long", labels: map[string]string{"key": strings.Repeat("v", 256)}, wantErr: true},
		{name: "reserved key", labels: map[string]string{v1alpha1.LabelKeyNodePool: "default"}, wantErr: true},
		{name: "reserved created-at key", labels: map[string]string{v1alpha1.LabelKeyCreatedAt: "0"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := utils.ValidateLabels(tc.labels)
			if tc.wantErr != (err != nil) {
				t.Fatalf("ValidateLabels(%v) error = %v, wantErr %v", tc.labels, err, tc.wantErr)
			}
		})
	}
}

func TestManagedLabelsAreValidUpCloudKeys(t *testing.T) {
	t.Parallel()

	// Every key the controller writes has to satisfy the same constraints a user's keys do,
	// otherwise UpCloud rejects the create call for every single launch.
	labels := utils.ManagedLabels(
		&v1alpha1.UpCloudNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		&karpv1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "default-abcde",
				Labels: map[string]string{karpv1.NodePoolLabelKey: "default"},
			},
		},
		"my-cluster",
		time.Unix(1700000000, 0),
	)
	for key, value := range labels {
		if len(key) < 2 || len(key) > 32 {
			t.Errorf("managed label key %q is %d characters, UpCloud allows 2-32", key, len(key))
		}
		if len(value) > 255 {
			t.Errorf("managed label %q has a %d character value, UpCloud allows 255", key, len(value))
		}
	}
	for _, key := range []string{
		v1alpha1.LabelKeyManagedBy,
		v1alpha1.LabelKeyNodePool,
		v1alpha1.LabelKeyNodeClaim,
		v1alpha1.LabelKeyNodeClass,
		v1alpha1.LabelKeyCreatedAt,
	} {
		if _, ok := labels[key]; !ok {
			t.Errorf("managed labels are missing %q", key)
		}
	}
	if labels[v1alpha1.LabelKeyManagedBy] != "my-cluster" {
		t.Errorf("managed-by = %q, want the cluster name", labels[v1alpha1.LabelKeyManagedBy])
	}
}

func TestManagedLabelsDoNotLetUserLabelsWin(t *testing.T) {
	t.Parallel()

	// A user label that shadowed managed-by would move the server into another cluster's view.
	labels := utils.ManagedLabels(
		&v1alpha1.UpCloudNodeClass{
			ObjectMeta: metav1.ObjectMeta{Name: "default"},
			Spec: v1alpha1.UpCloudNodeClassSpec{
				Labels: map[string]string{v1alpha1.LabelKeyManagedBy: "someone-elses-cluster"},
			},
		},
		&karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "default-abcde"}},
		"my-cluster",
		time.Now(),
	)
	if labels[v1alpha1.LabelKeyManagedBy] != "my-cluster" {
		t.Errorf("managed-by = %q, want %q", labels[v1alpha1.LabelKeyManagedBy], "my-cluster")
	}
}

func TestCreatedAtRoundTrips(t *testing.T) {
	t.Parallel()

	want := time.Unix(1700000000, 0)
	labels := utils.ManagedLabels(
		&v1alpha1.UpCloudNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		&karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "default-abcde"}},
		"my-cluster",
		want,
	)
	if got := utils.CreatedAt(labels); !got.Equal(want) {
		t.Errorf("CreatedAt = %v, want %v", got, want)
	}
}

func TestCreatedAtRejectsGarbage(t *testing.T) {
	t.Parallel()

	// An unparseable value must read as the zero time so garbage collection treats the server as
	// old rather than as perpetually new.
	for _, labels := range []map[string]string{
		nil,
		{},
		{v1alpha1.LabelKeyCreatedAt: "not-a-number"},
	} {
		if got := utils.CreatedAt(labels); !got.IsZero() {
			t.Errorf("CreatedAt(%v) = %v, want the zero time", labels, got)
		}
	}
}
