// SPDX-License-Identifier: Apache-2.0
//
// Copyright 2025 ConfigButler
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
//

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"os"
	"path/filepath"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	"go.uber.org/zap/zapcore"
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	configbutleraiv1alpha1 "github.com/ConfigButler/gitops-reverser/api/v1alpha1"
	"github.com/ConfigButler/gitops-reverser/internal/controller"
	"github.com/ConfigButler/gitops-reverser/internal/correlation"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/leader"
	"github.com/ConfigButler/gitops-reverser/internal/metrics"
	"github.com/ConfigButler/gitops-reverser/internal/reconcile"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
	webhookhandler "github.com/ConfigButler/gitops-reverser/internal/webhook"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

const (
	// Correlation store configuration.
	correlationMaxEntries = 10000
	correlationTTL        = 5 * time.Minute
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(configbutleraiv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	// Parse flags and configure logger
	cfg := parseFlags()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&cfg.zapOpts)))

	// Log metrics configuration for debugging
	setupLog.Info("Metrics configuration",
		"metrics-bind-address", cfg.metricsAddr,
		"metrics-secure", cfg.secureMetrics)

	// Initialize metrics
	setupCtx := ctrl.SetupSignalHandler()
	_, err := metrics.InitOTLPExporter(setupCtx)
	fatalIfErr(err, "unable to initialize metrics exporter")

	// TLS/options
	tlsOpts := buildTLSOptions(cfg.enableHTTP2)

	// Servers and cert watchers
	webhookServer, webhookCertWatcher := initWebhookServer(
		cfg.webhookCertPath, cfg.webhookCertName, cfg.webhookCertKey, tlsOpts,
	)
	metricsServerOptions, metricsCertWatcher := buildMetricsServerOptions(
		cfg.metricsAddr, cfg.secureMetrics,
		cfg.metricsCertPath, cfg.metricsCertName, cfg.metricsCertKey,
		tlsOpts,
	)

	// Manager
	mgr := newManager(metricsServerOptions, webhookServer, cfg.probeAddr, cfg.enableLeaderElection)

	// Leader labeler (if elected)
	addLeaderPodLabeler(mgr, cfg.enableLeaderElection)

	// Controllers
	fatalIfErr((&controller.GitRepoConfigReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr), "unable to create controller", "controller", "GitRepoConfig")

	// Initialize rule store for watch rules
	ruleStore := rulestore.NewStore()

	// Initialize WorkerManager (manages branch workers)
	workerManager := git.NewWorkerManager(mgr.GetClient(), ctrl.Log.WithName("worker-manager"))
	fatalIfErr(mgr.Add(workerManager), "unable to add worker manager to manager")
	setupLog.Info("WorkerManager initialized and added to manager")

	// Initialize correlation store for webhook→watch enrichment
	correlationStore := correlation.NewStore(correlationTTL, correlationMaxEntries)
	correlationStore.SetEvictionCallback(func() {
		metrics.KVEvictionsTotal.Add(context.Background(), 1)
	})
	setupLog.Info("Correlation store initialized",
		"ttl", correlationTTL,
		"maxEntries", correlationMaxEntries)

	// Create ReconcilerManager (will be set up as ControlEventEmitter)
	reconcilerManager := reconcile.NewReconcilerManager(
		nil, // eventRouter will be set after EventRouter is created
		ctrl.Log.WithName("reconciler-manager"),
	)
	setupLog.Info("ReconcilerManager initialized")

	// Watch ingestion manager (placeholder, will get EventRouter set later)
	watchMgr := &watch.Manager{
		Client:           mgr.GetClient(),
		Log:              ctrl.Log.WithName("watch"),
		RuleStore:        ruleStore,
		EventRouter:      nil, // Will be set below
		CorrelationStore: correlationStore,
	}

	// Initialize EventRouter with all dependencies
	eventRouter := watch.NewEventRouter(
		workerManager,
		reconcilerManager,
		watchMgr,
		mgr.GetClient(),
		ctrl.Log.WithName("event-router"),
	)
	setupLog.Info("EventRouter initialized")

	// Set EventRouter reference in WatchManager
	watchMgr.EventRouter = eventRouter

	// GitDestination controller (with WorkerManager and EventRouter)
	fatalIfErr((&controller.GitDestinationReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		WorkerManager: workerManager,
		EventRouter:   eventRouter,
	}).SetupWithManager(mgr), "unable to create controller", "controller", "GitDestination")

	// WatchRule controller (with WatchManager reference for dynamic reconciliation)
	fatalIfErr((&controller.WatchRuleReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		RuleStore:    ruleStore,
		WatchManager: watchMgr,
	}).SetupWithManager(mgr), "unable to create controller", "controller", "WatchRule")

	// ClusterWatchRule controller (with WatchManager reference for dynamic reconciliation)
	fatalIfErr((&controller.ClusterWatchRuleReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		RuleStore:    ruleStore,
		WatchManager: watchMgr,
	}).SetupWithManager(mgr), "unable to create controller", "controller", "ClusterWatchRule")

	// Webhook handler (correlation storage only - stores ALL resources, watch filters by rules)
	eventHandler := &webhookhandler.EventHandler{
		Client:           mgr.GetClient(),
		CorrelationStore: correlationStore,
	}

	// Create and inject decoder for generic Kubernetes resource handling
	decoder := admission.NewDecoder(scheme)
	fatalIfErr(eventHandler.InjectDecoder(&decoder), "unable to inject decoder into webhook handler")
	setupLog.Info("Generic unstructured decoder injected - ready to handle all Kubernetes resource types")

	// Register event correlation webhook
	validatingWebhook := &admission.Webhook{Handler: eventHandler}
	mgr.GetWebhookServer().Register("/process-validating-webhook", validatingWebhook)
	setupLog.Info("Event correlation webhook handler registered", "path", "/process-validating-webhook")

	// Register GitDestination validator webhook (prevents duplicate destinations)
	fatalIfErr(webhookhandler.SetupGitDestinationValidatorWebhook(mgr),
		"unable to setup GitDestination validator webhook")
	setupLog.Info("GitDestination validator webhook registered - enforcing uniqueness constraint")

	// Register experimental audit webhook for metrics collection
	auditHandler, err := webhookhandler.NewAuditHandler(webhookhandler.AuditHandlerConfig{
		DumpDir: cfg.auditDumpPath,
	})
	fatalIfErr(err, "unable to create audit handler")
	mgr.GetWebhookServer().Register("/audit-webhook", auditHandler)
	if cfg.auditDumpPath != "" {
		setupLog.Info("Experimental audit webhook handler registered with file dumping",
			"http-path", "/audit-webhook",
			"dump-path", cfg.auditDumpPath)
	} else {
		setupLog.Info("Experimental audit webhook handler registered (file dumping disabled)",
			"http-path", "/audit-webhook")
	}

	// NOTE: Old git.Worker has been replaced by WorkerManager + BranchWorker architecture
	// The new system is already initialized above and wired through EventRouter
	setupLog.Info("Using new BranchWorker architecture (per-branch workers)")

	// Setup watch manager (must be after controllers are set up)
	fatalIfErr(watchMgr.SetupWithManager(mgr), "unable to setup watch ingestion manager")
	fatalIfErr(mgr.Add(watchMgr), "unable to add watch ingestion manager")
	setupLog.Info("Watch ingestion manager added (cluster-as-source-of-truth mode)")

	if err := (&controller.GitProviderReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GitProvider")
		os.Exit(1)
	}
	if err := (&controller.GitTargetReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GitTarget")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	// Cert watchers
	addCertWatchersToManager(mgr, metricsCertWatcher, webhookCertWatcher)

	// Health checks
	addHealthChecks(mgr)

	// Start manager
	setupLog.Info("starting manager")
	fatalIfErr(mgr.Start(setupCtx), "problem running manager")
}

// appConfig holds parsed CLI flags and logging options.
type appConfig struct {
	metricsAddr          string
	metricsCertPath      string
	metricsCertName      string
	metricsCertKey       string
	webhookCertPath      string
	webhookCertName      string
	webhookCertKey       string
	enableLeaderElection bool
	probeAddr            string
	secureMetrics        bool
	enableHTTP2          bool
	auditDumpPath        string
	zapOpts              zap.Options
}

// parseFlags parses CLI flags and returns the application configuration.
func parseFlags() appConfig {
	var cfg appConfig

	flag.StringVar(&cfg.metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&cfg.probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&cfg.enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&cfg.secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(
		&cfg.webhookCertPath,
		"webhook-cert-path",
		"",
		"The directory that contains the webhook certificate.",
	)
	flag.StringVar(&cfg.webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&cfg.webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&cfg.metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(
		&cfg.metricsCertName,
		"metrics-cert-name",
		"tls.crt",
		"The name of the metrics server certificate file.",
	)
	flag.StringVar(&cfg.metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&cfg.enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.StringVar(&cfg.auditDumpPath, "audit-dump-path", "",
		"Directory to write audit events for debugging. If empty, audit event file dumping is disabled.")

	cfg.zapOpts = zap.Options{
		Development: true,
		// Enable more detailed logging for debugging
		Level: zapcore.InfoLevel, // Change to DebugLevel for even more verbose output
	}
	cfg.zapOpts.BindFlags(flag.CommandLine)

	flag.Parse()
	return cfg
}

// fatalIfErr logs and exits the process if err is not nil.
func fatalIfErr(err error, msg string, keysAndValues ...any) {
	if err != nil {
		setupLog.Error(err, msg, keysAndValues...)
		os.Exit(1)
	}
}

// buildTLSOptions constructs TLS options, disabling HTTP/2 unless explicitly enabled.
func buildTLSOptions(enableHTTP2 bool) []func(*tls.Config) {
	var tlsOpts []func(*tls.Config)

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, func(c *tls.Config) {
			setupLog.Info("disabling http/2")
			c.NextProtos = []string{"http/1.1"}
		})
	}
	return tlsOpts
}

// initWebhookServer initializes the webhook server and, if configured, a cert watcher.
func initWebhookServer(
	certPath, certName, certKey string,
	baseTLS []func(*tls.Config),
) (webhook.Server, *certwatcher.CertWatcher) {
	webhookTLSOpts := append([]func(*tls.Config){}, baseTLS...)
	var webhookCertWatcher *certwatcher.CertWatcher

	if len(certPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", certPath, //nolint:lll // Structured log with many fields
			"webhook-cert-name", certName, "webhook-cert-key", certKey)

		var err error
		webhookCertWatcher, err = certwatcher.New(
			filepath.Join(certPath, certName),
			filepath.Join(certPath, certKey),
		)
		fatalIfErr(err, "Failed to initialize webhook certificate watcher")

		webhookTLSOpts = append(webhookTLSOpts, func(config *tls.Config) {
			config.GetCertificate = webhookCertWatcher.GetCertificate
		})
	}

	server := webhook.NewServer(webhook.Options{TLSOpts: webhookTLSOpts})
	return server, webhookCertWatcher
}

// buildMetricsServerOptions configures metrics server options and an optional cert watcher.
func buildMetricsServerOptions(
	metricsAddr string,
	secureMetrics bool,
	certPath, certName, certKey string,
	baseTLS []func(*tls.Config),
) (metricsserver.Options, *certwatcher.CertWatcher) {
	opts := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       baseTLS,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/metrics/filters#WithAuthenticationAndAuthorization //nolint:lll // URL
		opts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	var metricsCertWatcher *certwatcher.CertWatcher
	if len(certPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", certPath, //nolint:lll // Structured log with many fields
			"metrics-cert-name", certName, "metrics-cert-key", certKey)

		var err error
		metricsCertWatcher, err = certwatcher.New(
			filepath.Join(certPath, certName),
			filepath.Join(certPath, certKey),
		)
		fatalIfErr(err, "to initialize metrics certificate watcher", "error", err)

		opts.TLSOpts = append(opts.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	return opts, metricsCertWatcher
}

// newManager creates a new controller-runtime Manager with common options.
func newManager(
	metricsOptions metricsserver.Options,
	webhookServer webhook.Server,
	probeAddr string,
	enableLeaderElection bool,
) ctrl.Manager {
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "9ed3440e.configbutler.ai",
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}
	return mgr
}

// addLeaderPodLabeler adds the leader pod labeler runnable when leader election is enabled.
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch
func addLeaderPodLabeler(mgr ctrl.Manager, enabled bool) {
	if !enabled {
		return
	}

	podName := leader.GetPodName()
	podNamespace := leader.GetPodNamespace()
	if podName != "" && podNamespace != "" {
		setupLog.Info("Adding leader pod labeler", "pod", podName, "namespace", podNamespace)
		podLabeler := &leader.PodLabeler{
			Client:    mgr.GetClient(),
			Log:       ctrl.Log.WithName("leader-labeler"),
			PodName:   podName,
			Namespace: podNamespace,
		}
		fatalIfErr(mgr.Add(podLabeler), "unable to add leader pod labeler")
	} else {
		setupLog.Info("POD_NAME or POD_NAMESPACE not set, skipping leader pod labeler")
	}
}

// addCertWatchersToManager attaches optional certificate watchers to the manager.
func addCertWatchersToManager(mgr ctrl.Manager, metricsCertWatcher, webhookCertWatcher *certwatcher.CertWatcher) {
	if metricsCertWatcher != nil {
		setupLog.Info("Adding metrics certificate watcher to manager")
		fatalIfErr(mgr.Add(metricsCertWatcher), "unable to add metrics certificate watcher to manager")
	}
	if webhookCertWatcher != nil {
		setupLog.Info("Adding webhook certificate watcher to manager")
		fatalIfErr(mgr.Add(webhookCertWatcher), "unable to add webhook certificate watcher to manager")
	}
}

// addHealthChecks registers health and readiness checks.
func addHealthChecks(mgr ctrl.Manager) {
	fatalIfErr(mgr.AddHealthzCheck("healthz", healthz.Ping), "unable to set up health check")
	fatalIfErr(mgr.AddReadyzCheck("readyz", healthz.Ping), "unable to set up ready check")
}
