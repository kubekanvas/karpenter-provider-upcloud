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

// Package userdata renders a NodeClass's userData with the values that are only known once
// Karpenter has chosen a plan and a zone for a particular NodeClaim.
//
// The provider deliberately does not generate a bootstrap script. Which distribution the node runs,
// and how it joins the cluster, stays the operator's decision — this package only substitutes
// per-node values into whatever script they supply.
//
// Note what is absent: the server's UUID, and therefore its provider ID. UpCloud assigns the UUID
// when CreateServer returns, and userData is part of that same request, so it cannot be templated
// in. A bootstrap script must read it at boot from the metadata service:
//
//	PROVIDER_ID="upcloud:////$(curl -s http://169.254.169.254/metadata/v1/instance_id)"
package userdata

import (
	"fmt"
	"sort"
	"strings"
	"text/template"

	corev1 "k8s.io/api/core/v1"
)

// Options are the per-NodeClaim values exposed to the template.
type Options struct {
	// InstanceType is the UpCloud plan Karpenter selected, e.g. "2xCPU-4GB".
	InstanceType string
	// Zone is the UpCloud zone the server is launched into, e.g. "de-fra1".
	Zone string
	// NodeLabels are the labels Karpenter resolved for the node.
	NodeLabels map[string]string
	// NodeTaints are the taints the node should register with, including the startup taint that
	// keeps pods off it until Karpenter has finished registration.
	NodeTaints []corev1.Taint
}

// templateData is the value handed to the template. It is a distinct type from Options so that the
// CSV helpers are computed once and the template surface stays explicit.
type templateData struct {
	InstanceType  string
	Zone          string
	NodeLabels    map[string]string
	NodeLabelsCSV string
	NodeTaintsCSV string
}

// Render substitutes per-node values into userData.
//
// userData containing no template actions is returned byte-for-byte unchanged, so a script written
// before this existed keeps working and a shell script full of ${...} and $(...) is not disturbed.
func Render(userData string, opts Options) (string, error) {
	if !strings.Contains(userData, "{{") {
		return userData, nil
	}

	tmpl, err := template.New("userdata").
		// Fail loudly on a typo rather than silently substituting "<no value>" into a boot script,
		// where the result is a node that comes up subtly misconfigured.
		Option("missingkey=error").
		Parse(userData)
	if err != nil {
		return "", fmt.Errorf("parsing userData template, %w", err)
	}

	var out strings.Builder
	if err := tmpl.Execute(&out, templateData{
		InstanceType:  opts.InstanceType,
		Zone:          opts.Zone,
		NodeLabels:    opts.NodeLabels,
		NodeLabelsCSV: LabelsCSV(opts.NodeLabels),
		NodeTaintsCSV: TaintsCSV(opts.NodeTaints),
	}); err != nil {
		return "", fmt.Errorf("rendering userData template, %w", err)
	}
	return out.String(), nil
}

// LabelsCSV renders labels as "key=value,key=value", the form kubelet's --node-labels and k3s's
// --node-label accept. Keys are sorted so the same label set always renders identically, which
// keeps the rendered userData stable and avoids spurious drift.
func LabelsCSV(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, labels[k]))
	}
	return strings.Join(parts, ",")
}

// TaintsCSV renders taints as "key=value:Effect,key=value:Effect", the form kubelet's
// --register-with-taints and k3s's --node-taint accept. A taint with no value renders as
// "key=:Effect", which is how the startup taint karpenter.sh/unregistered is expressed.
func TaintsCSV(taints []corev1.Taint) string {
	if len(taints) == 0 {
		return ""
	}
	parts := make([]string, 0, len(taints))
	for _, t := range taints {
		parts = append(parts, fmt.Sprintf("%s=%s:%s", t.Key, t.Value, t.Effect))
	}
	return strings.Join(parts, ",")
}
