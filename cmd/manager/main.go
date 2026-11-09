package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud/vcd"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloud/vsphere"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/cloudinit"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/controller"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/kube"
)

var scheme = runtime.NewScheme()

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = infrav1.AddToScheme(scheme)
}

func main() {
	var metricsAddr, probeAddr string
	var enableLeaderElection bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the health probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	cfg, err := ctrl.GetConfig()
	if err != nil {
		setupLog.Error(err, "unable to get kubeconfig")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "vcd-nodescale.k8s-nodescale.github.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	vcdOrg := os.Getenv("VCD_ORG")
	vcdVDC := os.Getenv("VCD_VDC")

	// The platform is a property of the installation, not of the process that
	// can switch at will: the chart sets NODESCALE_PLATFORM, and exactly that
	// one factory is registered. A pool that names another platform is refused
	// by the reconciler, and the other platform needs a second installation.
	platform := strings.ToLower(strings.TrimSpace(os.Getenv("NODESCALE_PLATFORM")))
	if platform == "" {
		// A binary started by hand, outside the chart, keeps the historical
		// behaviour; the chart never leaves it empty.
		platform = cloud.ProviderVCD
	}

	registry := cloud.Registry{}
	var missingCredentials error
	switch platform {
	case cloud.ProviderVCD:
		factory, err := vcd.New(vcd.Config{
			URL:      os.Getenv("VCD_URL"),
			User:     os.Getenv("VCD_USER"),
			Password: os.Getenv("VCD_PASSWORD"),
			Org:      vcdOrg,
			VDC:      vcdVDC,
			Insecure: os.Getenv("VCD_INSECURE") == "true",
		})
		if err != nil {
			setupLog.Error(err, "unable to configure the vCD provider")
			os.Exit(1)
		}
		registry[cloud.ProviderVCD] = factory
		missingCredentials = requireEnv("VCD_URL", "VCD_USER", "VCD_PASSWORD", "VCD_ORG", "VCD_VDC")
	case cloud.ProviderVSphere:
		registry[cloud.ProviderVSphere] = vsphere.New(vsphere.Config{
			URL:        os.Getenv("VSPHERE_URL"),
			User:       os.Getenv("VSPHERE_USER"),
			Password:   os.Getenv("VSPHERE_PASSWORD"),
			FolderRoot: os.Getenv("VSPHERE_FOLDER_ROOT"),
			Insecure:   os.Getenv("VSPHERE_INSECURE") == "true",
		})
		missingCredentials = requireEnv("VSPHERE_URL", "VSPHERE_USER", "VSPHERE_PASSWORD")
	default:
		setupLog.Error(fmt.Errorf("NODESCALE_PLATFORM=%q", platform),
			"NODESCALE_PLATFORM must be vcd or vsphere: an installation serves exactly one platform")
		os.Exit(1)
	}

	// Credentials being absent is not a reason to restart: the Pod starts, the
	// readiness probe reports why, and each affected pool carries a CloudError
	// naming the missing key. A wrong account is therefore a condition on the
	// pool, not a CrashLoopBackOff of the whole controller.
	if missingCredentials != nil {
		setupLog.Error(missingCredentials,
			"the platform credentials are not configured; pools will report CloudError until the Secret is created")
	}

	// The platform is a property of the pool (spec.provider), not of the
	// process: the registry hands out the provider each pool asks for.
	cloudProvider := registry

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		setupLog.Error(err, "unable to create kubernetes clientset")
		os.Exit(1)
	}

	renderer, err := cloudinit.New()
	if err != nil {
		setupLog.Error(err, "unable to load cloud-init templates")
		os.Exit(1)
	}

	if err := (&controller.WorkerPoolReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		Cloud:              cloudProvider,
		Kube:               kube.NewClient(cs),
		Renderer:           renderer,
		Recorder:           mgr.GetEventRecorderFor("k8sworkerpool-controller"),
		Platform:           platform,
		Org:                vcdOrg,
		VDC:                vcdVDC,
		VSphereFolderRoot:  os.Getenv("VSPHERE_FOLDER_ROOT"),
		PodName:            os.Getenv("POD_NAME"),
		PodNamespace:       os.Getenv("POD_NAMESPACE"),
		BootstrapConfigMap: strings.TrimSpace(os.Getenv("NODESCALE_BOOTSTRAP_CONFIGMAP")),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}
	// Readiness also answers "can this installation serve its platform at
	// all": missing credentials keep the Pod NotReady without restarting it,
	// so Helm --wait and kubectl get pods both show the install is not done.
	if err := mgr.AddReadyzCheck("platform-credentials", func(_ *http.Request) error {
		return missingCredentials
	}); err != nil {
		setupLog.Error(err, "unable to set up the platform credentials check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// requireEnv names the variables the installation's credentials Secret did not
// provide. The values are read once at startup and the Secret is not
// hot-reloaded, so the answer stays accurate for the life of the process - a
// restart after fixing the Secret is the documented way to pick it up.
func requireEnv(keys ...string) error {
	var missing []string
	for _, k := range keys {
		if strings.TrimSpace(os.Getenv(k)) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("platform credentials are incomplete: %s is empty (check the credentials Secret named in the chart, in the release namespace)",
		strings.Join(missing, ", "))
}
