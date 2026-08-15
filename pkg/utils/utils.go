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

package utils

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/UpCloudLtd/upcloud-go-api/v8/upcloud"
	"github.com/awslabs/operatorpkg/serrors"
	"github.com/samber/lo"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/kubekanvas/karpenter-upcloud/pkg/apis/v1alpha1"
)

// ProviderIDPrefix is the prefix the UpCloud cloud-controller-manager writes into
// Node.spec.providerID. Karpenter matches NodeClaims to Nodes on this exact string, so it must stay
// byte-for-byte identical to the CCM's, including the empty region and zone segments.
const ProviderIDPrefix = "upcloud:////"

// UpCloud restricts label keys to 2-32 printable ASCII characters that do not start with an
// underscore, and label values to at most 255 characters.
const (
	maxLabelKeyLength   = 32
	minLabelKeyLength   = 2
	maxLabelValueLength = 255
)

var (
	labelKeyRegex = regexp.MustCompile(`^[\x20-\x7E]+$`)

	// managedLabelKeys are owned by the controller. A NodeClass may not set them, because
	// overwriting any of them would either orphan servers or misattribute them to another cluster.
	managedLabelKeys = map[string]struct{}{
		v1alpha1.LabelKeyManagedBy: {},
		v1alpha1.LabelKeyNodePool:  {},
		v1alpha1.LabelKeyNodeClaim: {},
		v1alpha1.LabelKeyNodeClass: {},
		v1alpha1.LabelKeyCreatedAt: {},
	}
)

// ParseInstanceID extracts the UpCloud server UUID from a providerID.
func ParseInstanceID(providerID string) (string, error) {
	uuid, ok := strings.CutPrefix(providerID, ProviderIDPrefix)
	if !ok || uuid == "" {
		return "", serrors.Wrap(fmt.Errorf("provider id does not match known format"), "provider-id", providerID)
	}
	return uuid, nil
}

// ProviderID renders an UpCloud server UUID as a Kubernetes providerID.
func ProviderID(uuid string) string {
	return ProviderIDPrefix + uuid
}

// ManagedLabels returns the UpCloud labels the controller stamps onto every server it launches.
// LabelKeyManagedBy is what List filters on, so a server missing it is invisible to this
// controller and will never be garbage collected.
//
// createdAt is the launch time to record. Pass the server's existing value when reconciling labels
// on a running server, so that reconciliation does not reset its apparent age and expose it to
// garbage collection.
func ManagedLabels(nodeClass *v1alpha1.UpCloudNodeClass, nodeClaim *karpv1.NodeClaim, clusterName string, createdAt time.Time) map[string]string {
	if createdAt.IsZero() {
		// Only reachable when a server's launch label was missing or unparseable. Any real timestamp
		// beats writing a nonsensical one, and the NodeClaim's existence already proves it is not an
		// orphan.
		createdAt = time.Now()
	}
	managed := map[string]string{
		v1alpha1.LabelKeyManagedBy: clusterName,
		v1alpha1.LabelKeyNodeClass: nodeClass.Name,
		v1alpha1.LabelKeyNodeClaim: nodeClaim.Name,
		v1alpha1.LabelKeyCreatedAt: strconv.FormatInt(createdAt.Unix(), 10),
	}
	if nodePool, ok := nodeClaim.Labels[karpv1.NodePoolLabelKey]; ok {
		managed[v1alpha1.LabelKeyNodePool] = nodePool
	}
	// User labels are applied first so that the managed keys always win.
	return lo.Assign(nodeClass.Spec.Labels, managed)
}

// CreatedAt reads the launch time recorded on a server's labels. A server without the label is
// reported as the zero time, which makes it immediately eligible for garbage collection — correct,
// because only a server this controller launched carries the managed-by label that surfaced it in
// the first place, and every one of those is labeled at launch.
func CreatedAt(labels map[string]string) time.Time {
	raw, ok := labels[v1alpha1.LabelKeyCreatedAt]
	if !ok {
		return time.Time{}
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(seconds, 0)
}

// ValidateLabels checks user-supplied labels against UpCloud's constraints and against the keys the
// controller reserves for itself.
func ValidateLabels(labels map[string]string) error {
	var problems []string
	for key, value := range labels {
		switch {
		case len(key) < minLabelKeyLength || len(key) > maxLabelKeyLength:
			problems = append(problems, fmt.Sprintf("%q: key must be %d-%d characters", key, minLabelKeyLength, maxLabelKeyLength))
		case strings.HasPrefix(key, "_"):
			problems = append(problems, fmt.Sprintf("%q: key must not start with an underscore", key))
		case !labelKeyRegex.MatchString(key):
			problems = append(problems, fmt.Sprintf("%q: key must be printable ASCII", key))
		case len(value) > maxLabelValueLength:
			problems = append(problems, fmt.Sprintf("%q: value must be at most %d characters", key, maxLabelValueLength))
		}
		if _, reserved := managedLabelKeys[key]; reserved {
			problems = append(problems, fmt.Sprintf("%q: key is managed by Karpenter", key))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return serrors.Wrap(fmt.Errorf("labels failed validation requirements"), "labels", strings.Join(problems, ", "))
}

// LabelSliceToMap converts the UpCloud API's label representation into a map.
func LabelSliceToMap(labels upcloud.LabelSlice) map[string]string {
	out := make(map[string]string, len(labels))
	for _, label := range labels {
		out[label.Key] = label.Value
	}
	return out
}

// MapToLabelSlice converts a map into the UpCloud API's label representation, sorted by key so that
// requests are stable and diffable.
func MapToLabelSlice(labels map[string]string) upcloud.LabelSlice {
	out := make(upcloud.LabelSlice, 0, len(labels))
	for _, key := range lo.Keys(labels) {
		out = append(out, upcloud.Label{Key: key, Value: labels[key]})
	}
	sortLabels(out)
	return out
}

func sortLabels(labels upcloud.LabelSlice) {
	// A hand-rolled insertion sort keeps this dependency-free and the slices are tiny (UpCloud caps
	// servers at a handful of labels).
	for i := 1; i < len(labels); i++ {
		for j := i; j > 0 && labels[j].Key < labels[j-1].Key; j-- {
			labels[j], labels[j-1] = labels[j-1], labels[j]
		}
	}
}

// PrettySlice truncates a slice after maxItems so that log lines stay readable.
func PrettySlice[T any](s []T, maxItems int) string {
	var sb strings.Builder
	for i, elem := range s {
		if i > maxItems-1 {
			fmt.Fprintf(&sb, " and %d other(s)", len(s)-i)
			break
		} else if i > 0 {
			fmt.Fprint(&sb, ", ")
		}
		fmt.Fprint(&sb, elem)
	}
	return sb.String()
}

// WithDefaultFloat64 returns the float64 value of the supplied environment variable or, if it is
// absent or unparseable, the supplied default.
func WithDefaultFloat64(key string, def float64) float64 {
	val, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return def
	}
	return f
}
