/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUTHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/controller"
	"github.com/ConfigButler/gitops-reverser/internal/eventqueue"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/leader"
	"github.com/ConfigButler/gitops-reverser/internal/metrics"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	internalwebhook "github.com/ConfigButler/gitops-reverser/internal/webhook"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(configbutleraiv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
}

// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;patch

// nolint:gocyclo
func main() {
	var webhookPort int
	var metricsPort int
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)

	flag.IntVar(&webhookPort, "webhook-port", 9443, "The port for the webhook server.")
	flag.IntVar(&metricsPort, "metrics-port", 8080, "The port for the metrics server.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Create watchers for webhooks certificates
	var webhookCertWatcher *certwatcher.CertWatcher

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		var err error
		webhookCertWatcher, err = certwatcher.New(
			filepath.Join(webhookCertPath, webhookCertName),
			filepath.Join(webhookCertPath, webhookCertKey),
		)
		if err != nil {
			setupLog.Error(err, "Failed to initialize webhook certificate watcher")
			os.Exit(1)
		}

		webhookTLSOpts = append(webhookTLSOpts, func(config *tls.Config) {
			config.GetCertificate = webhookCertWatcher.GetCertificate
		})
	}

	// Setup separate metrics and health probe server
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	metricsMux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	metricsServer := &http.Server{
		Addr:    ":" + strconv.Itoa(metricsPort),
		Handler: metricsMux,
	}

	go func() {
		setupLog.Info("starting metrics server", "addr", metricsServer.Addr)
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			setupLog.Error(err, "problem running metrics server")
			os.Exit(1)
		}
	}()

	webhookServer := webhook.NewServer(webhook.Options{
		Port:    webhookPort,
		TLSOpts: webhookTLSOpts,
	})

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:           scheme,
		WebhookServer:    webhookServer,
		LeaderElection:   enableLeaderElection,
		LeaderElectionID: "9ed3440e.configbutler.ai",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends.. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controller.GitRepoConfigReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GitRepoConfig")
		os.Exit(1)
	}
	ruleStore := rulestore.NewStore()

	if err := (&controller.WatchRuleReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		RuleStore: ruleStore,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "WatchRule")
		os.Exit(1)
	}
	// nolint:goconst
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		eventQueue := eventqueue.NewQueue()
		mgr.GetWebhookServer().Register("/validate-v1-event", &webhook.Admission{
			Handler: &internalwebhook.EventHandler{
				Client:     mgr.GetClient(),
				RuleStore:  ruleStore,
				EventQueue: eventQueue,
			},
		})

		gitWorker := &git.Worker{
			Client:     mgr.GetClient(),
			Log:        ctrl.Log.WithName("git-worker"),
			EventQueue: eventQueue,
		}
		if err := mgr.Add(gitWorker); err != nil {
			handleErr(err, "unable to add git worker to manager")
		}
	}
	// +kubebuilder:scaffold:builder

	// Add the PodLabeler runnable to the manager.
	// It will only be started for the leader.
	if err := mgr.Add(&leader.PodLabeler{
		Client:    mgr.GetClient(),
		Log:       ctrl.Log.WithName("leader-labeler"),
		PodName:   leader.GetPodName(),
		Namespace: leader.GetPodNamespace(),
	}); err != nil {
		handleErr(err, "unable to add leader labeler to manager")
	}

	if webhookCertWatcher != nil {
		setupLog.Info("Adding webhook certificate watcher to manager")
		if err := mgr.Add(webhookCertWatcher); err != nil {
			handleErr(err, "unable to add webhook certificate watcher to manager")
		}
	}

	// Initialize OTLP exporter
	if shutdown, err := metrics.InitOTLPExporter(context.Background()); err != nil {
		handleErr(err, "unable to initialize OTLP exporter")
	} else {
		defer func() {
			if err := shutdown(context.Background()); err != nil {
				setupLog.Error(err, "failed to shutdown OTLP exporter")
			}
		}()
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		handleErr(err, "problem running manager")
	}
}

func handleErr(err error, msg string) {
	if err != nil {
		setupLog.Error(err, msg)
		os.Exit(1)
	}
}
