/*
Copyright 2019 The Kubernetes Authors.

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

package cmd

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/lithammer/dedent"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	flag "github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/util/sets"
	clientset "k8s.io/client-go/kubernetes"
	kubeadmapi "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm"
	kubeadmscheme "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm/scheme"
	kubeadmapiv1beta1 "k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm/v1beta1"
	"k8s.io/kubernetes/cmd/kubeadm/app/apis/kubeadm/validation"
	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/options"
	phases "k8s.io/kubernetes/cmd/kubeadm/app/cmd/phases/init"
	"k8s.io/kubernetes/cmd/kubeadm/app/cmd/phases/workflow"
	cmdutil "k8s.io/kubernetes/cmd/kubeadm/app/cmd/util"
	kubeadmconstants "k8s.io/kubernetes/cmd/kubeadm/app/constants"
	"k8s.io/kubernetes/cmd/kubeadm/app/features"
	certsphase "k8s.io/kubernetes/cmd/kubeadm/app/phases/certs"
	kubeconfigphase "k8s.io/kubernetes/cmd/kubeadm/app/phases/kubeconfig"
	kubeadmutil "k8s.io/kubernetes/cmd/kubeadm/app/util"
	"k8s.io/kubernetes/cmd/kubeadm/app/util/apiclient"
	configutil "k8s.io/kubernetes/cmd/kubeadm/app/util/config"
	kubeconfigutil "k8s.io/kubernetes/cmd/kubeadm/app/util/kubeconfig"
)

var (
	initDoneTempl = template.Must(template.New("init").Parse(dedent.Dedent(`
		Your Kubernetes control-plane has initialized successfully!

		To start using your cluster, you need to run the following as a regular user:

		  mkdir -p $HOME/.kube
		  sudo cp -i {{.KubeConfigPath}} $HOME/.kube/config
		  sudo chown $(id -u):$(id -g) $HOME/.kube/config

		You should now deploy a pod network to the cluster.
		Run "kubectl apply -f [podnetwork].yaml" with one of the options listed at:
		  https://kubernetes.io/docs/concepts/cluster-administration/addons/

		You can now join any number of machines by running the following on each node
		as root:

		  {{.joinCommand}}

		`)))
)

// initOptions defines all the init options exposed via flags by kubeadm init.
// Please note that this structure includes the public kubeadm config API, but only a subset of the options
// supported by this api will be exposed as a flag.
type initOptions struct {
	cfgPath               string
	skipTokenPrint        bool
	dryRun                bool
	kubeconfigDir         string
	kubeconfigPath        string
	featureGatesString    string
	ignorePreflightErrors []string
	bto                   *options.BootstrapTokenOptions
	externalcfg           *kubeadmapiv1beta1.InitConfiguration
	uploadCerts           bool
}

// initData defines all the runtime information used when running the kubeadm init workflow;
// this data is shared across all the phases that are included in the workflow.
type initData struct {
	cfg                   *kubeadmapi.InitConfiguration
	skipTokenPrint        bool
	dryRun                bool
	kubeconfigDir         string
	kubeconfigPath        string
	ignorePreflightErrors sets.String
	certificatesDir       string
	dryRunDir             string
	externalCA            bool
	client                clientset.Interface
	waiter                apiclient.Waiter
	outputWriter          io.Writer
	uploadCerts           bool
	certificateKey        string
}

// NewCmdInit returns "kubeadm init" command.
// NB. initOptions is exposed as parameter for allowing unit testing of
//     the newInitOptions method, that implements all the command options validation logic
func NewCmdInit(out io.Writer, initOptions *initOptions) *cobra.Command {
	if initOptions == nil {
		initOptions = newInitOptions()
	}
	initRunner := workflow.NewRunner()

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Run this command in order to set up the Kubernetes control plane.",
		Run: func(cmd *cobra.Command, args []string) {
			c, err := initRunner.InitData(args)
			kubeadmutil.CheckErr(err)

			data := c.(*initData)
			fmt.Printf("[init] Using Kubernetes version: %s\n", data.cfg.KubernetesVersion)

			err = initRunner.Run(args)
			kubeadmutil.CheckErr(err)

			err = showJoinCommand(data, out)
			kubeadmutil.CheckErr(err)
		},
		Args: cobra.NoArgs,
	}

	// adds flags to the init command
	// init command local flags could be eventually inherited by the sub-commands automatically generated for phases
	AddInitConfigFlags(cmd.Flags(), initOptions.externalcfg, &initOptions.featureGatesString)
	AddInitOtherFlags(cmd.Flags(), &initOptions.cfgPath, &initOptions.skipTokenPrint, &initOptions.dryRun, &initOptions.uploadCerts, &initOptions.ignorePreflightErrors)
	initOptions.bto.AddTokenFlag(cmd.Flags())
	initOptions.bto.AddTTLFlag(cmd.Flags())
	options.AddImageMetaFlags(cmd.Flags(), &initOptions.externalcfg.ImageRepository)

	// defines additional flag that are not used by the init command but that could be eventually used
	// by the sub-commands automatically generated for phases
	initRunner.SetAdditionalFlags(func(flags *flag.FlagSet) {
		options.AddKubeConfigFlag(flags, &initOptions.kubeconfigPath)
		options.AddKubeConfigDirFlag(flags, &initOptions.kubeconfigDir)
		options.AddControlPlanExtraArgsFlags(flags, &initOptions.externalcfg.APIServer.ExtraArgs, &initOptions.externalcfg.ControllerManager.ExtraArgs, &initOptions.externalcfg.Scheduler.ExtraArgs)
	})

	// initialize the workflow runner with the list of phases
	initRunner.AppendPhase(phases.NewPreflightPhase())
	initRunner.AppendPhase(phases.NewKubeletStartPhase())
	initRunner.AppendPhase(phases.NewCertsPhase())
	initRunner.AppendPhase(phases.NewKubeConfigPhase())
	initRunner.AppendPhase(phases.NewControlPlanePhase())
	initRunner.AppendPhase(phases.NewEtcdPhase())
	initRunner.AppendPhase(phases.NewWaitControlPlanePhase())
	initRunner.AppendPhase(phases.NewUploadConfigPhase())
	initRunner.AppendPhase(phases.NewUploadCertsPhase())
	initRunner.AppendPhase(phases.NewMarkControlPlanePhase())
	initRunner.AppendPhase(phases.NewBootstrapTokenPhase())
	initRunner.AppendPhase(phases.NewAddonPhase())

	// sets the data builder function, that will be used by the runner
	// both when running the entire workflow or single phases
	initRunner.SetDataInitializer(func(cmd *cobra.Command, args []string) (workflow.RunData, error) {
		return newInitData(cmd, args, initOptions, out)
	})

	// binds the Runner to kubeadm init command by altering
	// command help, adding --skip-phases flag and by adding phases subcommands
	initRunner.BindToCommand(cmd)

	return cmd
}

// AddInitConfigFlags adds init flags bound to the config to the specified flagset
func AddInitConfigFlags(flagSet *flag.FlagSet, cfg *kubeadmapiv1beta1.InitConfiguration, featureGatesString *string) {
	flagSet.StringVar(
		&cfg.LocalAPIEndpoint.AdvertiseAddress, options.APIServerAdvertiseAddress, cfg.LocalAPIEndpoint.AdvertiseAddress,
		"The IP address the API Server will advertise it's listening on. If not set the default network interface will be used.",
	)
	flagSet.Int32Var(
		&cfg.LocalAPIEndpoint.BindPort, options.APIServerBindPort, cfg.LocalAPIEndpoint.BindPort,
		"Port for the API Server to bind to.",
	)
	flagSet.StringVar(
		&cfg.Networking.ServiceSubnet, options.NetworkingServiceSubnet, cfg.Networking.ServiceSubnet,
		"Use alternative range of IP address for service VIPs.",
	)
	flagSet.StringVar(
		&cfg.Networking.PodSubnet, options.NetworkingPodSubnet, cfg.Networking.PodSubnet,
		"Specify range of IP addresses for the pod network. If set, the control plane will automatically allocate CIDRs for every node.",
	)
	flagSet.StringVar(
		&cfg.Networking.DNSDomain, options.NetworkingDNSDomain, cfg.Networking.DNSDomain,
		`Use alternative domain for services, e.g. "myorg.internal".`,
	)
	flagSet.StringVar(
		&cfg.KubernetesVersion, options.KubernetesVersion, cfg.KubernetesVersion,
		`Choose a specific Kubernetes version for the control plane.`,
	)
	flagSet.StringVar(
		&cfg.CertificatesDir, options.CertificatesDir, cfg.CertificatesDir,
		`The path where to save and store the certificates.`,
	)
	flagSet.StringSliceVar(
		&cfg.APIServer.CertSANs, options.APIServerCertSANs, cfg.APIServer.CertSANs,
		`Optional extra Subject Alternative Names (SANs) to use for the API Server serving certificate. Can be both IP addresses and DNS names.`,
	)
	flagSet.StringVar(
		&cfg.NodeRegistration.Name, options.NodeName, cfg.NodeRegistration.Name,
		`Specify the node name.`,
	)
	cmdutil.AddCRISocketFlag(flagSet, &cfg.NodeRegistration.CRISocket)
	flagSet.StringVar(featureGatesString, options.FeatureGatesString, *featureGatesString, "A set of key=value pairs that describe feature gates for various features. "+
		"Options are:\n"+strings.Join(features.KnownFeatures(&features.InitFeatureGates), "\n"))
}

// AddInitOtherFlags adds init flags that are not bound to a configuration file to the given flagset
// Note: All flags that are not bound to the cfg object should be allowed in cmd/kubeadm/app/apis/kubeadm/validation/validation.go
func AddInitOtherFlags(
	flagSet *flag.FlagSet,
	cfgPath *string,
	skipTokenPrint, dryRun, uploadCerts *bool,
	ignorePreflightErrors *[]string,
) {
	options.AddConfigFlag(flagSet, cfgPath)
	flagSet.StringSliceVar(
		ignorePreflightErrors, options.IgnorePreflightErrors, *ignorePreflightErrors,
		"A list of checks whose errors will be shown as warnings. Example: 'IsPrivilegedUser,Swap'. Value 'all' ignores errors from all checks.",
	)
	flagSet.BoolVar(
		skipTokenPrint, options.SkipTokenPrint, *skipTokenPrint,
		"Skip printing of the default bootstrap token generated by 'kubeadm init'.",
	)
	flagSet.BoolVar(
		dryRun, options.DryRun, *dryRun,
		"Don't apply any changes; just output what would be done.",
	)
	flagSet.BoolVar(
		uploadCerts, options.UploadCerts, *uploadCerts,
		"Upload certfificates to kubeadm-certs secret.",
	)
}

// newInitOptions returns a struct ready for being used for creating cmd init flags.
func newInitOptions() *initOptions {
	// initialize the public kubeadm config API by applying defaults
	externalcfg := &kubeadmapiv1beta1.InitConfiguration{}
	kubeadmscheme.Scheme.Default(externalcfg)

	// Create the options object for the bootstrap token-related flags, and override the default value for .Description
	bto := options.NewBootstrapTokenOptions()
	bto.Description = "The default bootstrap token generated by 'kubeadm init'."

	return &initOptions{
		externalcfg:    externalcfg,
		bto:            bto,
		kubeconfigDir:  kubeadmconstants.KubernetesDir,
		kubeconfigPath: kubeadmconstants.GetAdminKubeConfigPath(),
		uploadCerts:    false,
	}
}

// newInitData returns a new initData struct to be used for the execution of the kubeadm init workflow.
// This func takes care of validating initOptions passed to the command, and then it converts
// options into the internal InitConfiguration type that is used as input all the phases in the kubeadm init workflow
func newInitData(cmd *cobra.Command, args []string, options *initOptions, out io.Writer) (*initData, error) {
	// Re-apply defaults to the public kubeadm API (this will set only values not exposed/not set as a flags)
	kubeadmscheme.Scheme.Default(options.externalcfg)

	// Validate standalone flags values and/or combination of flags and then assigns
	// validated values to the public kubeadm config API when applicable
	var err error
	if options.externalcfg.FeatureGates, err = features.NewFeatureGate(&features.InitFeatureGates, options.featureGatesString); err != nil {
		return nil, err
	}

	ignorePreflightErrorsSet, err := validation.ValidateIgnorePreflightErrors(options.ignorePreflightErrors)
	if err != nil {
		return nil, err
	}

	if err = validation.ValidateMixedArguments(cmd.Flags()); err != nil {
		return nil, err
	}

	if err = options.bto.ApplyTo(options.externalcfg); err != nil {
		return nil, err
	}

	// Either use the config file if specified, or convert public kubeadm API to the internal InitConfiguration
	// and validates InitConfiguration
	cfg, err := configutil.LoadOrDefaultInitConfiguration(options.cfgPath, options.externalcfg)
	if err != nil {
		return nil, err
	}

	// override node name and CRI socket from the command line options
	if options.externalcfg.NodeRegistration.Name != "" {
		cfg.NodeRegistration.Name = options.externalcfg.NodeRegistration.Name
	}
	if options.externalcfg.NodeRegistration.CRISocket != "" {
		cfg.NodeRegistration.CRISocket = options.externalcfg.NodeRegistration.CRISocket
	}

	if err := configutil.VerifyAPIServerBindAddress(cfg.LocalAPIEndpoint.AdvertiseAddress); err != nil {
		return nil, err
	}
	if err := features.ValidateVersion(features.InitFeatureGates, cfg.FeatureGates, cfg.KubernetesVersion); err != nil {
		return nil, err
	}

	// if dry running creates a temporary folder for saving kubeadm generated files
	dryRunDir := ""
	if options.dryRun {
		if dryRunDir, err = ioutil.TempDir("", "kubeadm-init-dryrun"); err != nil {
			return nil, errors.Wrap(err, "couldn't create a temporary directory")
		}
	}

	// Checks if an external CA is provided by the user.
	externalCA, _ := certsphase.UsingExternalCA(&cfg.ClusterConfiguration)
	if externalCA {
		kubeconfigDir := options.kubeconfigDir
		if options.dryRun {
			kubeconfigDir = dryRunDir
		}
		if err := kubeconfigphase.ValidateKubeconfigsForExternalCA(kubeconfigDir, cfg); err != nil {
			return nil, err
		}
		if options.uploadCerts {
			return nil, errors.New("can't use externalCA mode and upload-certs")
		}
	}

	return &initData{
		cfg:                   cfg,
		certificatesDir:       cfg.CertificatesDir,
		skipTokenPrint:        options.skipTokenPrint,
		dryRun:                options.dryRun,
		dryRunDir:             dryRunDir,
		kubeconfigDir:         options.kubeconfigDir,
		kubeconfigPath:        options.kubeconfigPath,
		ignorePreflightErrors: ignorePreflightErrorsSet,
		externalCA:            externalCA,
		outputWriter:          out,
		uploadCerts:           options.uploadCerts,
	}, nil
}

// UploadCerts returns Uploadcerts flag.
func (d *initData) UploadCerts() bool {
	return d.uploadCerts
}

// CertificateKey returns the key used to encrypt the certs.
func (d *initData) CertificateKey() string {
	return d.certificateKey
}

// SetCertificateKey set the key used to encrypt the certs.
func (d *initData) SetCertificateKey(key string) {
	d.certificateKey = key
}

// Cfg returns initConfiguration.
func (d *initData) Cfg() *kubeadmapi.InitConfiguration {
	return d.cfg
}

// DryRun returns the DryRun flag.
func (d *initData) DryRun() bool {
	return d.dryRun
}

// SkipTokenPrint returns the SkipTokenPrint flag.
func (d *initData) SkipTokenPrint() bool {
	return d.skipTokenPrint
}

// IgnorePreflightErrors returns the IgnorePreflightErrors flag.
func (d *initData) IgnorePreflightErrors() sets.String {
	return d.ignorePreflightErrors
}

// CertificateWriteDir returns the path to the certificate folder or the temporary folder path in case of DryRun.
func (d *initData) CertificateWriteDir() string {
	if d.dryRun {
		return d.dryRunDir
	}
	return d.certificatesDir
}

// CertificateDir returns the CertificateDir as originally specified by the user.
func (d *initData) CertificateDir() string {
	return d.certificatesDir
}

// KubeConfigDir returns the path of the Kubernetes configuration folder or the temporary folder path in case of DryRun.
func (d *initData) KubeConfigDir() string {
	if d.dryRun {
		return d.dryRunDir
	}
	return d.kubeconfigDir
}

// KubeConfigPath returns the path to the kubeconfig file to use for connecting to Kubernetes
func (d *initData) KubeConfigPath() string {
	if d.dryRun {
		d.kubeconfigPath = filepath.Join(d.dryRunDir, kubeadmconstants.AdminKubeConfigFileName)
	}
	return d.kubeconfigPath
}

// ManifestDir returns the path where manifest should be stored or the temporary folder path in case of DryRun.
func (d *initData) ManifestDir() string {
	if d.dryRun {
		return d.dryRunDir
	}
	return kubeadmconstants.GetStaticPodDirectory()
}

// KubeletDir returns path of the kubelet configuration folder or the temporary folder in case of DryRun.
func (d *initData) KubeletDir() string {
	if d.dryRun {
		return d.dryRunDir
	}
	return kubeadmconstants.KubeletRunDirectory
}

// ExternalCA returns true if an external CA is provided by the user.
func (d *initData) ExternalCA() bool {
	return d.externalCA
}

// OutputWriter returns the io.Writer used to write output to by this command.
func (d *initData) OutputWriter() io.Writer {
	return d.outputWriter
}

// Client returns a Kubernetes client to be used by kubeadm.
// This function is implemented as a singleton, thus avoiding to recreate the client when it is used by different phases.
// Important. This function must be called after the admin.conf kubeconfig file is created.
func (d *initData) Client() (clientset.Interface, error) {
	if d.client == nil {
		if d.dryRun {
			// If we're dry-running, we should create a faked client that answers some GETs in order to be able to do the full init flow and just logs the rest of requests
			dryRunGetter := apiclient.NewInitDryRunGetter(d.cfg.NodeRegistration.Name, d.cfg.Networking.ServiceSubnet)
			d.client = apiclient.NewDryRunClient(dryRunGetter, os.Stdout)
		} else {
			// If we're acting for real, we should create a connection to the API server and wait for it to come up
			var err error
			d.client, err = kubeconfigutil.ClientSetFromFile(d.KubeConfigPath())
			if err != nil {
				return nil, err
			}
		}
	}
	return d.client, nil
}

// Tokens returns an array of token strings.
func (d *initData) Tokens() []string {
	tokens := []string{}
	for _, bt := range d.cfg.BootstrapTokens {
		tokens = append(tokens, bt.Token.String())
	}
	return tokens
}

func printJoinCommand(out io.Writer, adminKubeConfigPath, token, key string, skipTokenPrint, uploadCerts bool) error {
	joinCommand, err := cmdutil.GetJoinCommand(adminKubeConfigPath, token, key, skipTokenPrint, uploadCerts)
	if err != nil {
		return err
	}

	ctx := map[string]string{
		"KubeConfigPath": adminKubeConfigPath,
		"joinCommand":    joinCommand,
	}

	return initDoneTempl.Execute(out, ctx)
}

// showJoinCommand prints the join command after all the phases in init have finished
func showJoinCommand(i *initData, out io.Writer) error {
	adminKubeConfigPath := i.KubeConfigPath()

	// Prints the join command, multiple times in case the user has multiple tokens
	for _, token := range i.Tokens() {
		if err := printJoinCommand(out, adminKubeConfigPath, token, i.certificateKey, i.skipTokenPrint, i.uploadCerts); err != nil {
			return errors.Wrap(err, "failed to print join command")
		}
	}

	return nil
}
