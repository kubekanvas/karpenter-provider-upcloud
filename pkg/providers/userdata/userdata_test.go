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

package userdata_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/providers/userdata"
)

func TestRenderLeavesPlainScriptsUntouched(t *testing.T) {
	t.Parallel()

	// A bootstrap script written before templating existed must keep working byte-for-byte, and a
	// shell script full of ${...} and $(...) must not be disturbed.
	script := `#!/bin/bash
set -euo pipefail
UUID="$(curl -s http://169.254.169.254/metadata/v1/instance_id)"
echo "provider-id: upcloud:////${UUID}"
`
	got, err := userdata.Render(script, userdata.Options{InstanceType: "2xCPU-4GB", Zone: "de-fra1"})
	if err != nil {
		t.Fatalf("Render returned %v", err)
	}
	if got != script {
		t.Errorf("Render altered a script with no template actions:\n%q", got)
	}
}

func TestRenderSubstitutesPerNodeValues(t *testing.T) {
	t.Parallel()

	script := `#!/bin/bash
k3s agent --node-label "{{.NodeLabelsCSV}}" --node-taint "{{.NodeTaintsCSV}}" --plan {{.InstanceType}} --zone {{.Zone}}
`
	got, err := userdata.Render(script, userdata.Options{
		InstanceType: "8xCPU-32GB",
		Zone:         "de-fra1",
		NodeLabels:   map[string]string{"b": "2", "a": "1"},
		NodeTaints:   []corev1.Taint{karpv1.UnregisteredNoExecuteTaint},
	})
	if err != nil {
		t.Fatalf("Render returned %v", err)
	}
	// Labels are sorted so the same set always renders identically and cannot cause spurious drift.
	if !strings.Contains(got, `--node-label "a=1,b=2"`) {
		t.Errorf("labels not rendered in sorted CSV form:\n%s", got)
	}
	if !strings.Contains(got, `--node-taint "karpenter.sh/unregistered=:NoExecute"`) {
		t.Errorf("startup taint not rendered:\n%s", got)
	}
	if !strings.Contains(got, "--plan 8xCPU-32GB") || !strings.Contains(got, "--zone de-fra1") {
		t.Errorf("plan or zone not substituted:\n%s", got)
	}
}

func TestRenderFailsLoudlyOnAnUnknownField(t *testing.T) {
	t.Parallel()

	// Silently substituting "<no value>" into a boot script yields a node that comes up subtly
	// misconfigured, which is far harder to diagnose than a failed launch.
	if _, err := userdata.Render(`echo {{.NoSuchField}}`, userdata.Options{}); err == nil {
		t.Error("Render accepted an unknown template field, want an error")
	}
}

func TestRenderReportsMalformedTemplates(t *testing.T) {
	t.Parallel()

	if _, err := userdata.Render(`echo {{.InstanceType`, userdata.Options{}); err == nil {
		t.Error("Render accepted an unterminated action, want a parse error")
	}
}

func TestCSVHelpersHandleEmptyInput(t *testing.T) {
	t.Parallel()

	if got := userdata.LabelsCSV(nil); got != "" {
		t.Errorf("LabelsCSV(nil) = %q, want empty", got)
	}
	if got := userdata.TaintsCSV(nil); got != "" {
		t.Errorf("TaintsCSV(nil) = %q, want empty", got)
	}
	// A valued taint keeps its value; the startup taint has none.
	got := userdata.TaintsCSV([]corev1.Taint{{Key: "k", Value: "v", Effect: corev1.TaintEffectNoSchedule}})
	if got != "k=v:NoSchedule" {
		t.Errorf("TaintsCSV = %q, want %q", got, "k=v:NoSchedule")
	}
}
