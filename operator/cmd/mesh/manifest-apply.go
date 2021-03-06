// Copyright 2019 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mesh

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"istio.io/api/operator/v1alpha1"
	iopv1alpha1 "istio.io/istio/operator/pkg/apis/istio/v1alpha1"
	"istio.io/istio/operator/pkg/helmreconciler"
	"istio.io/istio/operator/pkg/manifest"
	"istio.io/istio/operator/pkg/object"
	"istio.io/istio/operator/pkg/translate"
	"istio.io/istio/operator/pkg/util/clog"
	"istio.io/pkg/log"
)

const (
	// installedSpecCRPrefix is the prefix of any IstioOperator CR stored in the cluster that is a copy of the CR used
	// in the last manifest apply operation.
	installedSpecCRPrefix = "installed-state"
)

type manifestApplyArgs struct {
	// inFilenames is an array of paths to the input IstioOperator CR files.
	inFilenames []string
	// kubeConfigPath is the path to kube config file.
	kubeConfigPath string
	// context is the cluster context in the kube config
	context string
	// readinessTimeout is maximum time to wait for all Istio resources to be ready.
	readinessTimeout time.Duration
	// wait is flag that indicates whether to wait resources ready before exiting.
	wait bool
	// skipConfirmation determines whether the user is prompted for confirmation.
	// If set to true, the user is not prompted and a Yes response is assumed in all cases.
	skipConfirmation bool
	// force proceeds even if there are validation errors
	force bool
	// set is a string with element format "path=value" where path is an IstioOperator path and the value is a
	// value to set the node at that path to.
	set []string
	// charts is a path to a charts and profiles directory in the local filesystem, or URL with a release tgz.
	charts string
}

func addManifestApplyFlags(cmd *cobra.Command, args *manifestApplyArgs) {
	cmd.PersistentFlags().StringSliceVarP(&args.inFilenames, "filename", "f", nil, filenameFlagHelpStr)
	cmd.PersistentFlags().StringVarP(&args.kubeConfigPath, "kubeconfig", "c", "", "Path to kube config")
	cmd.PersistentFlags().StringVar(&args.context, "context", "", "The name of the kubeconfig context to use")
	cmd.PersistentFlags().BoolVarP(&args.skipConfirmation, "skip-confirmation", "y", false, skipConfirmationFlagHelpStr)
	cmd.PersistentFlags().BoolVar(&args.force, "force", false, "Proceed even with validation errors")
	cmd.PersistentFlags().DurationVar(&args.readinessTimeout, "readiness-timeout", 300*time.Second, "Maximum seconds to wait for all Istio resources to be ready."+
		" The --wait flag must be set for this flag to apply")
	cmd.PersistentFlags().BoolVarP(&args.wait, "wait", "w", false, "Wait, if set will wait until all Pods, Services, and minimum number of Pods "+
		"of a Deployment are in a ready state before the command exits. It will wait for a maximum duration of --readiness-timeout seconds")
	cmd.PersistentFlags().StringArrayVarP(&args.set, "set", "s", nil, SetFlagHelpStr)
	cmd.PersistentFlags().StringVarP(&args.charts, "charts", "d", "", chartsFlagHelpStr)
}

func manifestApplyCmd(rootArgs *rootArgs, maArgs *manifestApplyArgs, logOpts *log.Options) *cobra.Command {
	return &cobra.Command{
		Use:   "apply",
		Short: "Applies an Istio manifest, installing or reconfiguring Istio on a cluster.",
		Long:  "The apply subcommand generates an Istio install manifest and applies it to a cluster.",
		// nolint: lll
		Example: `  # Apply a default Istio installation
  istioctl manifest apply

  # Enable grafana dashboard
  istioctl manifest apply --set values.grafana.enabled=true

  # Generate the demo profile and don't wait for confirmation
  istioctl manifest apply --set profile=demo --skip-confirmation

  # To override a setting that includes dots, escape them with a backslash (\).  Your shell may require enclosing quotes.
  istioctl manifest apply --set "values.sidecarInjectorWebhook.injectedAnnotations.container\.apparmor\.security\.beta\.kubernetes\.io/istio-proxy=runtime/default"
`,
		Args: cobra.ExactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runApplyCmd(cmd, rootArgs, maArgs, logOpts)
		}}
}

// InstallCmd in an alias for manifest apply.
func InstallCmd(logOpts *log.Options) *cobra.Command {
	rootArgs := &rootArgs{}
	macArgs := &manifestApplyArgs{}

	mac := &cobra.Command{
		Use:   "install",
		Short: "Applies an Istio manifest, installing or reconfiguring Istio on a cluster.",
		Long:  "The install generates an Istio install manifest and applies it to a cluster.",
		// nolint: lll
		Example: `  # Apply a default Istio installation
  istioctl install

  # Enable grafana dashboard
  istioctl install --set values.grafana.enabled=true

  # Generate the demo profile and don't wait for confirmation
  istioctl install --set profile=demo --skip-confirmation

  # To override a setting that includes dots, escape them with a backslash (\).  Your shell may require enclosing quotes.
  istioctl install --set "values.sidecarInjectorWebhook.injectedAnnotations.container\.apparmor\.security\.beta\.kubernetes\.io/istio-proxy=runtime/default"
`,
		Args: cobra.ExactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runApplyCmd(cmd, rootArgs, macArgs, logOpts)
		}}

	addFlags(mac, rootArgs)
	addManifestApplyFlags(mac, macArgs)
	return mac
}

func runApplyCmd(cmd *cobra.Command, rootArgs *rootArgs, maArgs *manifestApplyArgs, logOpts *log.Options) error {
	l := clog.NewConsoleLogger(rootArgs.logToStdErr, cmd.OutOrStdout(), cmd.ErrOrStderr())
	// Warn users if they use `manifest apply` without any config args.
	if len(maArgs.inFilenames) == 0 && len(maArgs.set) == 0 && !rootArgs.dryRun && !maArgs.skipConfirmation {
		if !confirm("This will install the default Istio profile into the cluster. Proceed? (y/N)", cmd.OutOrStdout()) {
			cmd.Print("Cancelled.\n")
			os.Exit(1)
		}
	}
	if err := configLogs(rootArgs.logToStdErr, logOpts); err != nil {
		return fmt.Errorf("could not configure logs: %s", err)
	}
	if err := ApplyManifests(applyInstallFlagAlias(maArgs.set, maArgs.charts), maArgs.inFilenames, maArgs.force, rootArgs.dryRun, rootArgs.verbose,
		maArgs.kubeConfigPath, maArgs.context, maArgs.wait, maArgs.readinessTimeout, l); err != nil {
		return fmt.Errorf("failed to apply manifests: %v", err)
	}

	return nil
}

// ApplyManifests generates manifests from the given input files and --set flag overlays and applies them to the
// cluster. See GenManifests for more description of the manifest generation process.
//  force   validation warnings are written to logger but command is not aborted
//  dryRun  all operations are done but nothing is written
//  verbose full manifests are output
//  wait    block until Services and Deployments are ready, or timeout after waitTimeout
func ApplyManifests(setOverlay []string, inFilenames []string, force bool, dryRun bool, verbose bool,
	kubeConfigPath string, context string, wait bool, waitTimeout time.Duration, l clog.Logger) error {

	ysf, err := yamlFromSetFlags(setOverlay, force, l)
	if err != nil {
		return err
	}

	restConfig, clientSet, err := manifest.InitK8SRestClient(kubeConfigPath, context)
	if err != nil {
		return err
	}
	client, err := client.New(restConfig, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return err
	}
	_, iops, err := GenerateConfig(inFilenames, ysf, force, restConfig, l)
	if err != nil {
		return err
	}

	crName := installedSpecCRPrefix
	if iops.Revision != "" {
		crName += "-" + iops.Revision
	}
	iop, err := translate.IOPStoIOP(iops, crName, iopv1alpha1.Namespace(iops))
	if err != nil {
		return err
	}

	if err := manifest.CreateNamespace(iop.Namespace); err != nil {
		return err
	}

	// Needed in case we are running a test through this path that doesn't start a new process.
	helmreconciler.FlushObjectCaches()
	reconciler, err := helmreconciler.NewHelmReconciler(client, restConfig, iop, &helmreconciler.Options{DryRun: dryRun, Log: l})
	if err != nil {
		return err
	}
	status, err := reconciler.Reconcile()
	if err != nil {
		l.LogAndPrintf("\n\n✘ Errors were logged during apply operation:\n\n%s\n", err)
		return fmt.Errorf("errors occurred during operation")
	}
	if status.Status != v1alpha1.InstallStatus_HEALTHY {
		return fmt.Errorf("errors occurred during operation")
	}

	if wait {
		l.LogAndPrint("Waiting for resources to become ready...")
		objs, err := object.ParseK8sObjectsFromYAMLManifest(reconciler.GetManifests().String())
		if err != nil {
			l.LogAndPrintf("\n\n✘ Errors in manifest:\n%s\n", err)
			return fmt.Errorf("errors during wait")
		}
		if err := manifest.WaitForResources(objs, clientSet, waitTimeout, dryRun, l); err != nil {
			l.LogAndPrintf("\n\n✘ Errors during wait:\n%s\n", err)
			return fmt.Errorf("errors during wait")
		}
	}

	l.LogAndPrint("\n\n✔ Installation complete\n")

	// Save state to cluster in IstioOperator CR.
	iopStr, err := translate.IOPStoIOPstr(iops, crName, iopv1alpha1.Namespace(iops))
	if err != nil {
		return err
	}
	obj, err := object.ParseYAMLToK8sObject([]byte(iopStr))
	if err != nil {
		return err
	}
	if err := reconciler.ProcessObject("", obj.UnstructuredObject()); err != nil {
		return err
	}

	return nil
}
