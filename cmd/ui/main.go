// Command ui serves the optional management page: pool state, per-node
// consumption and the four actions. It owns no logic of its own - every action
// is an edit to the K8sWorkerPool object, so the controller decides what
// happens exactly as it would for kubectl.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	infrav1 "github.com/k8s-nodescale/vcd-k8s-node-controller/api/v1alpha1"
	"github.com/k8s-nodescale/vcd-k8s-node-controller/internal/mgmt"
)

func main() {
	var addr, prefix, exporterLabels string
	var exporterPort int
	var refresh time.Duration
	flag.StringVar(&addr, "addr", ":8090", "address the UI listens on")
	flag.StringVar(&prefix, "path-prefix", "/nodescale-ui", "path the ingress mounts the page at")
	flag.StringVar(&exporterLabels, "node-exporter-labels", mgmt.DefaultExporterLabels, "label selector of the node-exporter pods")
	flag.IntVar(&exporterPort, "node-exporter-port", mgmt.DefaultExporterPort, "node-exporter port")
	flag.DurationVar(&refresh, "refresh-interval", 15*time.Second, "how often the snapshot is rebuilt")

	var authSecret, authKey, authNamespace string
	var authRefresh, authSessionTTL time.Duration
	var authDisabled bool
	flag.StringVar(&authSecret, "auth-secret", "nodescale-ui-auth", "Secret that holds the accounts")
	flag.StringVar(&authKey, "auth-secret-key", "users.yaml", "key of the accounts file inside the Secret")
	flag.StringVar(&authNamespace, "auth-namespace", podNamespace(), "namespace of the Secret (default: the pod's own)")
	flag.DurationVar(&authRefresh, "auth-refresh", time.Minute, "how often the Secret is re-read")
	flag.DurationVar(&authSessionTTL, "auth-session-ttl", mgmt.DefaultSessionTTL, "how long a sign-in lasts")
	flag.BoolVar(&authDisabled, "auth-disabled", false, "serve without authentication (local use only)")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	log := zap.New(zap.UseFlagOptions(&opts))
	ctrl.SetLogger(log)
	mgmt.SetLogger(func(format string, args ...any) { log.Info(fmt.Sprintf(format, args...)) })
	setupLog := log.WithName("setup")

	cfg, err := ctrl.GetConfig()
	if err != nil {
		setupLog.Error(err, "unable to get kubeconfig")
		os.Exit(1)
	}
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = infrav1.AddToScheme(scheme)
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to create client")
		os.Exit(1)
	}

	collector := &mgmt.Collector{
		Client:         c,
		Metrics:        mgmt.NewMetricsClient(cfg),
		ExporterLabels: exporterLabels,
		ExporterPort:   exporterPort,
		Refresh:        refresh,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go collector.Run(ctx)

	auth := &mgmt.Authenticator{
		Client:     c,
		Namespace:  authNamespace,
		SecretName: authSecret,
		SecretKey:  authKey,
		Refresh:    authRefresh,
		SessionTTL: authSessionTTL,
		Disabled:   authDisabled,
	}
	if authDisabled {
		setupLog.Info("authentication is disabled")
	} else {
		go auth.Run(ctx)
	}

	srv := &mgmt.Server{Client: c, Collector: collector, Auth: auth, PathPrefix: prefix}
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		setupLog.Info("serving the management page", "addr", addr, "prefix", prefix)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			setupLog.Error(err, "listen")
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdown)
}

// podNamespace is the namespace the Secret lives in, which is the pod's own
// unless a flag says otherwise.
func podNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	return "default"
}
