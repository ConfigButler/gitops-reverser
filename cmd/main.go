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
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	correlationMaxEntries       = 10000
	correlationTTL              = 5 * time.Minute
	flagParseFailureExitCode    = 2
	defaultAuditPort            = 9444
	defaultAuditMaxBodyBytes    = int64(10 * 1024 * 1024)
	defaultAuditReadTimeout     = 15 * time.Second
	defaultAuditWriteTimeout    = 30 * time.Second
	defaultAuditIdleTimeout     = 60 * time.Second
	defaultAuditShutdownTimeout = 10 * time.Second
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
		"metrics-insecure", cfg.metricsInsecure,
		"webhook-insecure", cfg.webhookInsecure,
		"audit-insecure", cfg.auditInsecure)

	// Initialize metrics
	setupCtx := ctrl.SetupSignalHandler()
	_, err := metrics.InitOTLPExporter(setupCtx)
	fatalIfErr(err, "unable to initialize metrics exporter")

	// TLS/options
	tlsOpts := buildTLSOptions(cfg.enableHTTP2)

	// Servers and cert watchers
	webhookServer, webhookCertWatcher := initWebhookServer(
		!cfg.webhookInsecure,
		cfg.webhookCertPath, cfg.webhookCertName, cfg.webhookCertKey, tlsOpts,
	)
	metricsServerOptions, metricsCertWatcher := buildMetricsServerOptions(
		cfg.metricsAddr, !cfg.metricsInsecure,
		cfg.metricsCertPath, cfg.metricsCertName, cfg.metricsCertKey,
		tlsOpts,
	)

	// Manager
	mgr := newManager(metricsServerOptions, webhookServer, cfg.probeAddr)

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

	// Register GitTarget validator webhook (prevents duplicate targets)
	fatalIfErr(webhookhandler.SetupGitTargetValidatorWebhook(mgr),
		"unable to setup GitTarget validator webhook")
	setupLog.Info("GitTarget validator webhook registered - enforcing uniqueness constraint")

	// Register experimental audit webhook for metrics collection
	auditHandler, err := webhookhandler.NewAuditHandler(webhookhandler.AuditHandlerConfig{
		DumpDir:             cfg.auditDumpPath,
		MaxRequestBodyBytes: cfg.auditMaxRequestBodyBytes,
	})
	fatalIfErr(err, "unable to create audit handler")

	var auditCertWatcher *certwatcher.CertWatcher

	auditRunnable, watcher, initErr := initAuditServerRunnable(cfg, tlsOpts, auditHandler)
	fatalIfErr(initErr, "unable to initialize audit ingress server")
	auditCertWatcher = watcher
	fatalIfErr(mgr.Add(auditRunnable), "unable to add audit ingress server runnable")

	if cfg.auditDumpPath != "" {
		setupLog.Info("Audit ingress server configured with file dumping",
			"http-path", "/audit-webhook/{clusterID}",
			"dump-path", cfg.auditDumpPath,
			"address", buildAuditServerAddress(cfg.auditListenAddress, cfg.auditPort))
	} else {
		setupLog.Info("Audit ingress server configured",
			"http-path", "/audit-webhook/{clusterID}",
			"address", buildAuditServerAddress(cfg.auditListenAddress, cfg.auditPort))
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
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		WorkerManager: workerManager,
		EventRouter:   eventRouter,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GitTarget")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	// Cert watchers
	addCertWatchersToManager(mgr, metricsCertWatcher, webhookCertWatcher, auditCertWatcher)

	// Health checks
	addHealthChecks(mgr)

	// Start manager
	setupLog.Info("starting manager")
	fatalIfErr(mgr.Start(setupCtx), "problem running manager")
}

// appConfig holds parsed CLI flags and logging options.
type appConfig struct {
	metricsAddr              string
	metricsCertPath          string
	metricsCertName          string
	metricsCertKey           string
	webhookCertPath          string
	webhookCertName          string
	webhookCertKey           string
	probeAddr                string
	metricsInsecure          bool
	webhookInsecure          bool
	enableHTTP2              bool
	auditDumpPath            string
	auditListenAddress       string
	auditPort                int
	auditCertPath            string
	auditCertName            string
	auditCertKey             string
	auditInsecure            bool
	auditMaxRequestBodyBytes int64
	auditReadTimeout         time.Duration
	auditWriteTimeout        time.Duration
	auditIdleTimeout         time.Duration
	zapOpts                  zap.Options
}

// parseFlags parses CLI flags and returns the application configuration.
func parseFlags() appConfig {
	cfg, err := parseFlagsWithArgs(flag.CommandLine, os.Args[1:])
	if err != nil {
		setupLog.Error(err, "unable to parse flags")
		os.Exit(flagParseFailureExitCode)
	}
	return cfg
}

func parseFlagsWithArgs(fs *flag.FlagSet, args []string) (appConfig, error) {
	var cfg appConfig

	fs.StringVar(&cfg.metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	fs.StringVar(&cfg.probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	fs.BoolVar(&cfg.metricsInsecure, "metrics-insecure", false,
		"If set, the metrics endpoint is served via HTTP instead of HTTPS.")
	bindServerCertFlags(fs, "webhook", "webhook", &cfg.webhookCertPath, &cfg.webhookCertName, &cfg.webhookCertKey)
	bindServerCertFlags(
		fs,
		"metrics",
		"metrics server",
		&cfg.metricsCertPath,
		&cfg.metricsCertName,
		&cfg.metricsCertKey,
	)
	fs.BoolVar(&cfg.webhookInsecure, "webhook-insecure", false,
		"If set, webhook server certificate watching and TLS wiring are disabled for local test/play usage.")
	fs.BoolVar(&cfg.enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	fs.StringVar(&cfg.auditDumpPath, "audit-dump-path", "",
		"Directory to write audit events for debugging. If empty, audit event file dumping is disabled.")
	fs.StringVar(&cfg.auditListenAddress, "audit-listen-address", "0.0.0.0",
		"IP address for the dedicated audit ingress HTTPS server.")
	fs.IntVar(&cfg.auditPort, "audit-port", defaultAuditPort, "Port for the dedicated audit ingress HTTPS server.")
	bindServerCertFlags(fs, "audit", "audit ingress TLS", &cfg.auditCertPath, &cfg.auditCertName, &cfg.auditCertKey)
	fs.BoolVar(&cfg.auditInsecure, "audit-insecure", false,
		"If set, the audit ingress endpoint is served via HTTP instead of HTTPS.")
	fs.Int64Var(&cfg.auditMaxRequestBodyBytes, "audit-max-request-body-bytes", defaultAuditMaxBodyBytes,
		"Maximum request body size in bytes accepted by the audit ingress handler.")
	fs.DurationVar(&cfg.auditReadTimeout, "audit-read-timeout", defaultAuditReadTimeout,
		"Read timeout for the dedicated audit ingress HTTPS server.")
	fs.DurationVar(&cfg.auditWriteTimeout, "audit-write-timeout", defaultAuditWriteTimeout,
		"Write timeout for the dedicated audit ingress HTTPS server.")
	fs.DurationVar(&cfg.auditIdleTimeout, "audit-idle-timeout", defaultAuditIdleTimeout,
		"Idle timeout for the dedicated audit ingress HTTPS server.")

	cfg.zapOpts = zap.Options{
		Development: true,
		// Enable more detailed logging for debugging
		Level: zapcore.InfoLevel, // Change to DebugLevel for even more verbose output
	}
	cfg.zapOpts.BindFlags(fs)

	if err := fs.Parse(args); err != nil {
		return appConfig{}, err
	}
	applyAuditCertFallbacks(&cfg)
	if err := validateAuditConfig(cfg); err != nil {
		return appConfig{}, err
	}

	return cfg, nil
}

func bindServerCertFlags(
	fs *flag.FlagSet,
	prefix string,
	component string,
	certPath, certName, certKey *string,
) {
	fs.StringVar(certPath, fmt.Sprintf("%s-cert-path", prefix), "",
		fmt.Sprintf("The directory that contains the %s certificate.", component))
	fs.StringVar(certName, fmt.Sprintf("%s-cert-name", prefix), "tls.crt",
		fmt.Sprintf("The name of the %s certificate file.", component))
	fs.StringVar(certKey, fmt.Sprintf("%s-cert-key", prefix), "tls.key",
		fmt.Sprintf("The name of the %s key file.", component))
}

func applyAuditCertFallbacks(cfg *appConfig) {
	if cfg.auditCertPath == "" {
		cfg.auditCertPath = cfg.webhookCertPath
	}
	if cfg.auditCertName == "" {
		cfg.auditCertName = cfg.webhookCertName
	}
	if cfg.auditCertKey == "" {
		cfg.auditCertKey = cfg.webhookCertKey
	}
}

func validateAuditConfig(cfg appConfig) error {
	if cfg.auditPort <= 0 {
		return fmt.Errorf("audit-port must be > 0, got %d", cfg.auditPort)
	}
	if cfg.auditMaxRequestBodyBytes <= 0 {
		return fmt.Errorf("audit-max-request-body-bytes must be > 0, got %d", cfg.auditMaxRequestBodyBytes)
	}
	if cfg.auditReadTimeout <= 0 {
		return fmt.Errorf("audit-read-timeout must be > 0, got %s", cfg.auditReadTimeout)
	}
	if cfg.auditWriteTimeout <= 0 {
		return fmt.Errorf("audit-write-timeout must be > 0, got %s", cfg.auditWriteTimeout)
	}
	if cfg.auditIdleTimeout <= 0 {
		return fmt.Errorf("audit-idle-timeout must be > 0, got %s", cfg.auditIdleTimeout)
	}
	return nil
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
	tlsEnabled bool,
	certPath, certName, certKey string,
	baseTLS []func(*tls.Config),
) (webhook.Server, *certwatcher.CertWatcher) {
	webhookTLSOpts, webhookCertWatcher, err := buildTLSRuntime(
		tlsEnabled, false, "webhook", certPath, certName, certKey, baseTLS,
	)
	fatalIfErr(err, "failed to initialize webhook TLS runtime")
	if !tlsEnabled {
		setupLog.Info("Webhook insecure mode enabled; skipping webhook certificate watcher wiring")
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
	tlsOpts, metricsCertWatcher, err := buildTLSRuntime(
		secureMetrics, false, "metrics", certPath, certName, certKey, baseTLS,
	)
	fatalIfErr(err, "failed to initialize metrics TLS runtime")

	opts := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/metrics/filters#WithAuthenticationAndAuthorization //nolint:lll // URL
		opts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	return opts, metricsCertWatcher
}

type auditServerRunnable struct {
	server     *http.Server
	tlsEnabled bool
}

type serverTimeouts struct {
	read  time.Duration
	write time.Duration
	idle  time.Duration
}

func (r *auditServerRunnable) Start(ctx context.Context) error {
	setupLog.Info("Starting dedicated audit ingress server", "address", r.server.Addr)

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), defaultAuditShutdownTimeout)
		defer cancel()
		if err := r.server.Shutdown(shutdownCtx); err != nil {
			setupLog.Error(err, "Failed to shutdown dedicated audit ingress server")
		}
	}()

	var err error
	if r.tlsEnabled {
		err = r.server.ListenAndServeTLS("", "")
	} else {
		err = r.server.ListenAndServe()
	}
	<-shutdownDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("audit ingress server failed: %w", err)
}

func initAuditServerRunnable(
	cfg appConfig,
	baseTLS []func(*tls.Config),
	handler http.Handler,
) (*auditServerRunnable, *certwatcher.CertWatcher, error) {
	tlsEnabled := !cfg.auditInsecure
	tlsOpts, certWatcher, err := buildTLSRuntime(
		tlsEnabled, true, "audit ingress", cfg.auditCertPath, cfg.auditCertName, cfg.auditCertKey, baseTLS,
	)
	if err != nil {
		return nil, nil, err
	}

	var serverTLS *tls.Config
	if tlsEnabled {
		serverTLS = buildServerTLSConfig(tlsOpts)
	} else {
		setupLog.Info("Audit ingress TLS disabled; serving plain HTTP for audit ingress")
	}

	mux := buildAuditServeMux(handler)
	server := buildHTTPServer(
		buildAuditServerAddress(cfg.auditListenAddress, cfg.auditPort),
		mux,
		serverTLS,
		serverTimeouts{
			read:  cfg.auditReadTimeout,
			write: cfg.auditWriteTimeout,
			idle:  cfg.auditIdleTimeout,
		},
	)

	return &auditServerRunnable{server: server, tlsEnabled: tlsEnabled}, certWatcher, nil
}

func buildAuditServeMux(handler http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/audit-webhook", handler)
	mux.Handle("/audit-webhook/", handler)
	return mux
}

func buildAuditServerAddress(listenAddress string, port int) string {
	if strings.TrimSpace(listenAddress) == "" {
		return fmt.Sprintf(":%d", port)
	}
	return net.JoinHostPort(listenAddress, strconv.Itoa(port))
}

func buildServerTLSConfig(tlsOpts []func(*tls.Config)) *tls.Config {
	serverTLS := &tls.Config{MinVersion: tls.VersionTLS12}
	for _, opt := range tlsOpts {
		opt(serverTLS)
	}
	return serverTLS
}

func buildTLSRuntime(
	tlsEnabled bool,
	requireCert bool,
	component string,
	certPath, certName, certKey string,
	baseTLS []func(*tls.Config),
) ([]func(*tls.Config), *certwatcher.CertWatcher, error) {
	tlsOpts := append([]func(*tls.Config){}, baseTLS...)
	if !tlsEnabled {
		return tlsOpts, nil, nil
	}

	if strings.TrimSpace(certPath) == "" {
		if requireCert {
			return nil, nil, fmt.Errorf("%s-cert-path is required when %s TLS is enabled", component, component)
		}
		return tlsOpts, nil, nil
	}

	setupLog.Info("Initializing certificate watcher using provided certificates",
		"component", component,
		"cert-path", certPath,
		"cert-name", certName,
		"cert-key", certKey)

	certWatcher, err := newCertWatcher(certPath, certName, certKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to initialize %s certificate watcher: %w", component, err)
	}

	tlsOpts = append(tlsOpts, func(config *tls.Config) {
		config.GetCertificate = certWatcher.GetCertificate
	})
	return tlsOpts, certWatcher, nil
}

func buildHTTPServer(addr string, handler http.Handler, tlsConfig *tls.Config, timeouts serverTimeouts) *http.Server {
	return &http.Server{
		Addr:         addr,
		Handler:      handler,
		TLSConfig:    tlsConfig,
		ReadTimeout:  timeouts.read,
		WriteTimeout: timeouts.write,
		IdleTimeout:  timeouts.idle,
	}
}

func newCertWatcher(certPath, certName, certKey string) (*certwatcher.CertWatcher, error) {
	return certwatcher.New(
		filepath.Join(certPath, certName),
		filepath.Join(certPath, certKey),
	)
}

// newManager creates a new controller-runtime Manager with common options.
func newManager(
	metricsOptions metricsserver.Options,
	webhookServer webhook.Server,
	probeAddr string,
) ctrl.Manager {
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}
	return mgr
}

// addCertWatchersToManager attaches optional certificate watchers to the manager.
func addCertWatchersToManager(
	mgr ctrl.Manager,
	metricsCertWatcher, webhookCertWatcher, auditCertWatcher *certwatcher.CertWatcher,
) {
	watchers := []struct {
		component string
		watcher   *certwatcher.CertWatcher
	}{
		{component: "metrics", watcher: metricsCertWatcher},
		{component: "webhook", watcher: webhookCertWatcher},
		{component: "audit ingress", watcher: auditCertWatcher},
	}

	for _, item := range watchers {
		if item.watcher == nil {
			continue
		}
		setupLog.Info("Adding certificate watcher to manager", "component", item.component)
		fatalIfErr(mgr.Add(item.watcher), "unable to add certificate watcher to manager", "component", item.component)
	}
}

// addHealthChecks registers health and readiness checks.
func addHealthChecks(mgr ctrl.Manager) {
	fatalIfErr(mgr.AddHealthzCheck("healthz", healthz.Ping), "unable to set up health check")
	fatalIfErr(mgr.AddReadyzCheck("readyz", healthz.Ping), "unable to set up ready check")
}
