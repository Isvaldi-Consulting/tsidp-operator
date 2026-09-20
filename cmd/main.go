// tsidp-operator reconciles OIDCClient custom resources into OIDC client
// registrations on a tsidp instance, materializing credentials into
// Kubernetes Secrets. It runs as a sidecar next to tsidp in the same pod,
// talking to tsidp's -local-port loopback listener (which grants admin and
// dynamic client registration rights to pod-local callers).
package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	kclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	tsidpv1alpha1 "github.com/isvaldi-consulting/tsidp-operator/api/v1alpha1"
	"github.com/isvaldi-consulting/tsidp-operator/internal/controller"
	"github.com/isvaldi-consulting/tsidp-operator/internal/tsidp"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(tsidpv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		tsidpURL        = flag.String("tsidp-url", "http://127.0.0.1:8080", "base URL of the tsidp instance (its -local-port loopback listener in the same pod)")
		resyncInterval  = flag.Duration("resync-interval", 10*time.Minute, "how often to re-verify registrations against tsidp")
		watchNamespaces = flag.String("watch-namespaces", "", "comma-separated list of namespaces to watch for OIDCClients (empty = all namespaces, which requires cluster-wide RBAC)")
		metricsAddr     = flag.String("metrics-bind-address", ":8081", "metrics endpoint bind address")
		probeAddr       = flag.String("health-probe-bind-address", ":8082", "health probe bind address")
	)
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	idp := &tsidp.Client{
		BaseURL:    *tsidpURL,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}

	// Scope the Secret informer to operator-managed Secrets: without this,
	// Owns(&Secret{}) would list, watch, and hold every Secret in the
	// cluster in memory. With --watch-namespaces, additionally confine
	// every informer to the listed namespaces so the operator can run with
	// namespace-scoped RBAC.
	cacheOpts := cache.Options{
		ByObject: map[kclient.Object]cache.ByObject{
			&corev1.Secret{}: {Label: labels.SelectorFromSet(controller.ManagedSecretLabels)},
		},
	}
	if *watchNamespaces != "" {
		nsMap := map[string]cache.Config{}
		for _, ns := range strings.Split(*watchNamespaces, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				nsMap[ns] = cache.Config{}
			}
		}
		if len(nsMap) == 0 {
			// An empty map would silently mean "watch everything" —
			// the opposite of what a non-empty flag asked for.
			setupLog.Error(nil, "--watch-namespaces was set but contained no usable namespace names")
			os.Exit(1)
		}
		cacheOpts.DefaultNamespaces = nsMap
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Cache:                  cacheOpts,
		Metrics:                metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress: *probeAddr,
		// No leader election: the operator is a sidecar of a replicas:1
		// StatefulSet, so a second replica cannot exist.
	})
	if err != nil {
		setupLog.Error(err, "creating manager")
		os.Exit(1)
	}

	rec := &controller.OIDCClientReconciler{
		Client:         mgr.GetClient(),
		APIReader:      mgr.GetAPIReader(),
		Scheme:         mgr.GetScheme(),
		Recorder:       mgr.GetEventRecorderFor("tsidp-operator"),
		Tsidp:          idp,
		ResyncInterval: *resyncInterval,
	}
	if err := rec.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "setting up OIDCClient controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "adding healthz check")
		os.Exit(1)
	}
	// Readiness gates on tsidp being reachable so the sidecar reports
	// ready only once tsidp has joined the tailnet and is serving.
	if err := mgr.AddReadyzCheck("tsidp", func(req *http.Request) error {
		ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
		defer cancel()
		_, err := idp.Discovery(ctx)
		return err
	}); err != nil {
		setupLog.Error(err, "adding readyz check")
		os.Exit(1)
	}

	setupLog.Info("starting", "tsidpURL", *tsidpURL, "resync", resyncInterval.String())
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited")
		os.Exit(1)
	}
}
