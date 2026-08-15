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

package options

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/utils/env"

	"github.com/kubekanvas/karpenter-provider-upcloud/pkg/utils"
)

func init() {
	coreoptions.Injectables = append(coreoptions.Injectables, &Options{})
}

type optionsKey struct{}

type Options struct {
	// ClusterName identifies this cluster's servers in the UpCloud account. It is written to every
	// server as a label and is the filter used to list them, so two clusters sharing an UpCloud
	// account must not share a name.
	ClusterName string
	// ClusterZone is the UpCloud zone used by NodeClasses that do not name their own zones. It is
	// normally the zone the control plane runs in.
	ClusterZone string
	// VMMemoryOverheadPercent is subtracted from a plan's advertised memory to approximate what the
	// guest kernel actually sees, until a real node reports its capacity.
	VMMemoryOverheadPercent float64
	// DisableDryRun skips the UpCloud-side validation the NodeClass controller performs.
	DisableDryRun bool
}

func (o *Options) AddFlags(fs *coreoptions.FlagSet) {
	fs.StringVar(&o.ClusterName, "cluster-name", env.WithDefaultString("CLUSTER_NAME", ""),
		"[REQUIRED] The Kubernetes cluster name, used to label and discover this cluster's UpCloud servers.")
	fs.StringVar(&o.ClusterZone, "cluster-zone", env.WithDefaultString("CLUSTER_ZONE", ""),
		"[REQUIRED] The UpCloud zone used by NodeClasses that do not set spec.zones, e.g. fi-hel1.")
	fs.Float64Var(&o.VMMemoryOverheadPercent, "vm-memory-overhead-percent",
		utils.WithDefaultFloat64("VM_MEMORY_OVERHEAD_PERCENT", 0.075),
		"The fraction of a plan's memory assumed to be lost to virtualisation and kernel overhead when no node of that plan has reported its capacity yet.")
	fs.BoolVarWithEnv(&o.DisableDryRun, "disable-dry-run", "DISABLE_DRY_RUN", false,
		"If true, skip validating UpCloudNodeClasses against the UpCloud API.")
}

func (o *Options) Parse(fs *coreoptions.FlagSet, args ...string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		return fmt.Errorf("parsing flags, %w", err)
	}
	if err := o.Validate(); err != nil {
		return fmt.Errorf("validating options, %w", err)
	}
	return nil
}

func (o *Options) Validate() error {
	var errs []error
	if o.ClusterName == "" {
		errs = append(errs, fmt.Errorf("missing required --cluster-name (or CLUSTER_NAME)"))
	}
	if o.ClusterZone == "" {
		errs = append(errs, fmt.Errorf("missing required --cluster-zone (or CLUSTER_ZONE)"))
	}
	if o.VMMemoryOverheadPercent < 0 || o.VMMemoryOverheadPercent >= 1 {
		errs = append(errs, fmt.Errorf("--vm-memory-overhead-percent must be in [0, 1), got %v", o.VMMemoryOverheadPercent))
	}
	return errors.Join(errs...)
}

func (o *Options) ToContext(ctx context.Context) context.Context {
	return ToContext(ctx, o)
}

func ToContext(ctx context.Context, opts *Options) context.Context {
	return context.WithValue(ctx, optionsKey{}, opts)
}

func FromContext(ctx context.Context) *Options {
	retval := ctx.Value(optionsKey{})
	if retval == nil {
		return nil
	}
	return retval.(*Options)
}
