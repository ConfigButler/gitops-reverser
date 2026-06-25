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
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	"go.uber.org/zap/zapcore"
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	configbutleraiv1alpha2 "github.com/ConfigButler/gitops-reverser/api/v1alpha2"
	"github.com/ConfigButler/gitops-reverser/internal/controller"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/queue"
	"github.com/ConfigButler/gitops-reverser/internal/rulestore"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
	"github.com/ConfigButler/gitops-reverser/internal/watch"
	webhookhandler "github.com/ConfigButler/gitops-reverser/internal/webhook"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

const (
	flagParseFailureExitCode       = 2
	defaultAdmissionWebhookPort    = 9443
	defaultAuditPort               = 9444
	defaultAuditMaxBodyBytes       = int64(10 * 1024 * 1024)
	defaultAuditReadTimeout        = 15 * time.Second
	defaultAuditWriteTimeout       = 30 * time.Second
	defaultAuditIdleTimeout        = 60 * time.Second
	defaultAuditShutdownTimeout    = 10 * time.Second
	defaultAuditEventBodyTTL       = 5 * time.Minute
	defaultAuditEventBodyWait      = 500 * time.Millisecond
	defaultBranchBufferMaxBytesStr = "8Mi"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(configbutleraiv1alpha2.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	// Parse flags and configure logger
	cfg := parseFlags()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&cfg.zapOpts)))

	bi := currentBuildInfo()
	setupLog.Info("Starting gitops-reverser",
		"version", bi.Version,
		"gitCommit", bi.CommitWithDirty,
		"buildDate", bi.BuildDate,
		"goVersion", bi.GoVersion)

	setupLog.Info("Endpoint configuration",
		"metricsAddr", cfg.metricsAddr,
		"metricsInsecure", cfg.metricsInsecure,
		"auditAddress", buildAuditServerAddress(cfg.auditListenAddress, cfg.auditPort),
		"auditInsecure", cfg.auditInsecure,
		"admissionWebhookEnabled", cfg.admissionWebhookEnabled,
		"admissionWebhookPort", cfg.admissionWebhookPort)
	setupLog.Info("Sensitive resource policy", "resources", cfg.sensitiveResources.Entries())

	// Initialize metrics
	setupCtx := ctrl.SetupSignalHandler()
	_, err := telemetry.InitOTLPExporter(setupCtx)
	fatalIfErr(err, "unable to initialize metrics exporter")

	// TLS/options
	tlsOpts := buildTLSOptions(cfg.enableHTTP2)

	// Servers and cert watchers
	metricsServerOptions, metricsCertWatcher := buildMetricsServerOptions(
		cfg.metricsAddr, !cfg.metricsInsecure,
		cfg.metricsCertPath, cfg.metricsCertName, cfg.metricsCertKey,
		tlsOpts,
	)

	// Manager
	mgr := newManager(metricsServerOptions, cfg.probeAddr, cfg, tlsOpts)

	// Expose build metadata on the metrics server so an operator can confirm a
	// running pod is the build they expect (also logged at startup above).
	fatalIfErr(mgr.AddMetricsServerExtraHandler("/build-info", buildInfoHandler()),
		"unable to register build-info endpoint")

	// Initialize rule store for watch rules
	ruleStore := rulestore.NewStore()

	// Initialize WorkerManager (manages branch workers)
	workerManager := git.NewWorkerManager(
		mgr.GetClient(),
		ctrl.Log.WithName("worker-manager"),
		cfg.branchBufferMaxBytes,
		cfg.sensitiveResources,
	)
	workerManager.SetSSHHostKeyConfig(cfg.sshHostKeys)
	fatalIfErr(mgr.Add(workerManager), "unable to add worker manager to manager")

	// Watch ingestion manager (placeholder, will get EventRouter set later)
	watchMgr := &watch.Manager{
		Client:             mgr.GetClient(),
		Log:                ctrl.Log.WithName("watch"),
		RuleStore:          ruleStore,
		EventRouter:        nil, // Will be set below
		SensitiveResources: cfg.sensitiveResources,
	}

	// Initialize EventRouter with all dependencies. The streaming-snapshot resync
	// (M8) is driven directly through the worker, so there is no longer a separate
	// reconciler manager / two-snapshot handshake.
	eventRouter := watch.NewEventRouter(
		workerManager,
		watchMgr,
		mgr.GetClient(),
		ctrl.Log.WithName("event-router"),
	)

	// Set EventRouter reference in WatchManager
	watchMgr.EventRouter = eventRouter

	// Inject the live followability registry into the writer, so a GVR-only DELETE
	// event resolves to a manifest moved off its canonical path (M6 in the writer).
	// The registry is a stable pointer the watch manager refreshes in place.
	workerManager.SetMapper(watchMgr.TypeRegistry())

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

	setupLog.Info("watch-first committer-only mode enabled; Redis and audit webhook are not required for state capture")

	// Setup watch manager (must be after controllers are set up)
	fatalIfErr(watchMgr.SetupWithManager(mgr), "unable to setup watch ingestion manager")
	fatalIfErr(mgr.Add(watchMgr), "unable to add watch ingestion manager")

	if err := (&controller.GitProviderReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		SSHHostKeys: cfg.sshHostKeys,
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
	if err := (&controller.CommitRequestReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		APIReader: mgr.GetAPIReader(),
		Finalizer: eventRouter,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "CommitRequest")
		os.Exit(1)
	}
	if cfg.admissionWebhookEnabled {
		setupAdmissionWebhooks(mgr)
	}
	// +kubebuilder:scaffold:builder

	// Cert watchers
	addCertWatchersToManager(mgr, metricsCertWatcher, nil)

	// Health checks: watch-first state capture has no Redis/audit readiness dependency.
	addHealthChecks(mgr, nil, nil, nil)

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
	probeAddr                string
	metricsInsecure          bool
	admissionWebhookEnabled  bool
	admissionWebhookPort     int
	admissionWebhookCertPath string
	admissionWebhookCertName string
	admissionWebhookCertKey  string
	enableHTTP2              bool
	auditListenAddress       string
	auditPort                int
	auditCertPath            string
	auditCertName            string
	auditCertKey             string
	auditClientCAPath        string
	auditClientCAName        string
	auditInsecure            bool
	auditMaxRequestBodyBytes int64
	auditReadTimeout         time.Duration
	auditWriteTimeout        time.Duration
	auditIdleTimeout         time.Duration
	auditRedisAddr           string
	auditRedisUsername       string
	auditRedisPassword       string
	auditRedisDB             int
	auditRedisMaxLen         int64
	auditDebugRedisStream    string
	auditDebugRedisMaxLen    int64
	auditByTypeStreamPrefix  string
	auditByTypeDiag          bool
	auditByTypeDiagMaxLen    int64
	auditByTypeDiagResources string
	watchStateStream         bool
	auditRedisTLS            bool
	auditEventBodyTTL        time.Duration
	auditEventBodyWait       time.Duration
	branchBufferMaxBytes     int64
	sensitiveResources       types.SensitiveResourcePolicy
	sshHostKeys              git.SSHHostKeyConfig
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
	fs.BoolVar(&cfg.admissionWebhookEnabled, "admission-webhook-enabled", false,
		"If set, serve the validating admission webhook endpoint.")
	fs.IntVar(&cfg.admissionWebhookPort, "admission-webhook-port", defaultAdmissionWebhookPort,
		"Port for the validating admission webhook HTTPS server.")
	bindServerCertFlags(
		fs,
		"admission-webhook",
		"validating admission webhook TLS",
		&cfg.admissionWebhookCertPath,
		&cfg.admissionWebhookCertName,
		&cfg.admissionWebhookCertKey,
	)
	bindServerCertFlags(
		fs,
		"metrics",
		"metrics server",
		&cfg.metricsCertPath,
		&cfg.metricsCertName,
		&cfg.metricsCertKey,
	)
	fs.BoolVar(&cfg.enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics server and audit ingress server")
	fs.StringVar(&cfg.auditListenAddress, "audit-listen-address", "0.0.0.0",
		"IP address for the dedicated audit ingress HTTPS server.")
	fs.IntVar(&cfg.auditPort, "audit-port", defaultAuditPort, "Port for the dedicated audit ingress HTTPS server.")
	bindServerCertFlags(fs, "audit", "audit ingress TLS", &cfg.auditCertPath, &cfg.auditCertName, &cfg.auditCertKey)
	fs.StringVar(&cfg.auditClientCAPath, "audit-client-ca-path", "/tmp/k8s-audit-server/audit-client-ca",
		"Directory that contains the audit client CA certificate used to verify kube-apiserver client certificates.")
	fs.StringVar(&cfg.auditClientCAName, "audit-client-ca-name", "tls.crt",
		"File name of the audit client CA certificate used to verify kube-apiserver client certificates.")
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
	fs.StringVar(&cfg.auditRedisAddr, "audit-redis-addr", "valkey:6379",
		"Redis server address (<host>:<port>) for audit event queueing.")
	fs.StringVar(&cfg.auditRedisUsername, "audit-redis-username", "",
		"Optional Redis username for audit event queueing.")
	fs.StringVar(
		&cfg.auditRedisPassword,
		"audit-redis-password",
		os.Getenv("REDIS_PASSWORD"),
		"Redis password for audit event queueing. Prefer setting via REDIS_PASSWORD env var from a Kubernetes Secret.",
	)
	fs.IntVar(&cfg.auditRedisDB, "audit-redis-db", 0,
		"Redis database index for audit event queueing.")
	fs.Int64Var(&cfg.auditRedisMaxLen, "audit-redis-max-len", 0,
		"Approximate max stream length (0 disables trimming).")
	fs.StringVar(&cfg.auditDebugRedisStream, "audit-debug-redis-stream", "",
		"Optional Redis stream name for every decoded audit event before normal audit processing.")
	fs.Int64Var(&cfg.auditDebugRedisMaxLen, "audit-debug-redis-max-len", 0,
		"Approximate max debug stream length (0 disables trimming).")
	fs.StringVar(&cfg.auditByTypeStreamPrefix, "audit-bytype-stream-prefix", queue.DefaultRedisByTypeStreamPrefix,
		"Root key prefix for the per-resource-type experiment "+
			"(<prefix>:<group>:<resource>:audit:stream + :objects:items + :__index__).")
	fs.BoolVar(&cfg.auditByTypeDiag, "audit-bytype-diag", false,
		"Enable the opt-in global <prefix>:diag_all firehose — one annotated record per ingested "+
			"audit event, for investigation. Off by default.")
	fs.Int64Var(&cfg.auditByTypeDiagMaxLen, "audit-bytype-diag-max-len", 0,
		"Approximate max <prefix>:diag_all stream length (0 disables trimming).")
	fs.StringVar(&cfg.auditByTypeDiagResources, "audit-bytype-diag-resources", "",
		"Comma-separated resource names to scope the <prefix>:diag_all firehose to (e.g. "+
			"\"commitrequests,configmaps\"). Empty captures every queued event. Bounds the "+
			"firehose (and its in-lock XADD load) to a few suspect types when investigating.")
	fs.BoolVar(&cfg.watchStateStream, "watch-state-stream", false,
		"Experimental (Phase 1 of watch-first-ingestion-architecture): for every Synced type also hold a "+
			"long-lived WATCH and record its events into a per-type <prefix>:<group>:<resource>:watch:stream, "+
			"ALONGSIDE the audit stream, so the watch-derived desired set can be diffed against the "+
			"audit-derived one. Changes no Git write. Off by default.")
	fs.BoolVar(&cfg.auditRedisTLS, "audit-redis-tls", false,
		"If set, Redis connection for audit queueing uses TLS.")
	fs.DurationVar(&cfg.auditEventBodyTTL, "audit-event-body-ttl", defaultAuditEventBodyTTL,
		"TTL for parked additional audit body contributions.")
	fs.DurationVar(&cfg.auditEventBodyWait, "audit-event-body-wait", defaultAuditEventBodyWait,
		"Grace period for a bodyless official audit event to wait for a matching additional body.")
	branchBufferMaxBytesStr := os.Getenv("BRANCH_BUFFER_MAX_BYTES")
	if branchBufferMaxBytesStr == "" {
		branchBufferMaxBytesStr = defaultBranchBufferMaxBytesStr
	}
	var branchBufferMaxBytesFlag string
	fs.StringVar(&branchBufferMaxBytesFlag, "branch-buffer-max-bytes", branchBufferMaxBytesStr,
		"Maximum in-memory event buffer per branch worker, expressed as a Kubernetes resource quantity "+
			"(e.g. 8Mi, 1Gi). Bounds pod memory under bursty workloads; not user-facing.")
	var additionalSensitiveResources string
	fs.StringVar(
		&additionalSensitiveResources,
		"additional-sensitive-resources",
		"",
		"Comma-separated additional sensitive resources in resource or group/resource form.",
	)
	fs.StringVar(&cfg.sshHostKeys.DefaultKnownHostsConfigMap, "default-known-hosts-configmap", "",
		"Optional install-level ConfigMap (in the controller's namespace) supplying SSH known_hosts "+
			"for Git hosts when neither the credentials Secret nor the GitProvider's knownHostsRef does.")
	fs.BoolVar(&cfg.sshHostKeys.AllowMissingKnownHosts, "insecure-allow-missing-known-hosts", false,
		"INSECURE, dev/throwaway clusters only: permit SSH when no host-key source produced any "+
			"known_hosts at all. A present-but-unparseable known_hosts is always a hard error.")
	cfg.zapOpts = zap.Options{
		// Production mode defaults to JSON encoding, which is easier for log processors to parse.
		Development: false,
		Level:       zapcore.InfoLevel,
	}
	cfg.zapOpts.BindFlags(fs)

	if err := fs.Parse(args); err != nil {
		return appConfig{}, err
	}
	if err := validateAuditConfig(cfg); err != nil {
		return appConfig{}, err
	}
	if err := validateAdmissionWebhookConfig(cfg); err != nil {
		return appConfig{}, err
	}

	bufferQuantity, err := resource.ParseQuantity(branchBufferMaxBytesFlag)
	if err != nil {
		return appConfig{}, fmt.Errorf("invalid --branch-buffer-max-bytes %q: %w", branchBufferMaxBytesFlag, err)
	}
	cfg.branchBufferMaxBytes, _ = bufferQuantity.AsInt64()
	if cfg.branchBufferMaxBytes <= 0 {
		return appConfig{}, fmt.Errorf("--branch-buffer-max-bytes must be > 0, got %s", branchBufferMaxBytesFlag)
	}

	cfg.sensitiveResources, err = types.ParseSensitiveResourcePolicy(additionalSensitiveResources)
	if err != nil {
		return appConfig{}, err
	}

	// The install-level default known-hosts ConfigMap lives in the controller's own namespace,
	// supplied via the downward API. Without it, that resolution layer is simply unavailable.
	cfg.sshHostKeys.ControllerNamespace = os.Getenv("POD_NAMESPACE")

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
	if strings.TrimSpace(cfg.auditRedisAddr) == "" {
		return errors.New("audit-redis-addr is required")
	}
	if !cfg.auditInsecure && strings.TrimSpace(cfg.auditClientCAPath) == "" {
		return errors.New("audit-client-ca-path is required when audit TLS is enabled")
	}
	if cfg.auditRedisDB < 0 {
		return fmt.Errorf("audit-redis-db must be >= 0, got %d", cfg.auditRedisDB)
	}
	if cfg.auditRedisMaxLen < 0 {
		return fmt.Errorf("audit-redis-max-len must be >= 0, got %d", cfg.auditRedisMaxLen)
	}
	if cfg.auditDebugRedisMaxLen < 0 {
		return fmt.Errorf("audit-debug-redis-max-len must be >= 0, got %d", cfg.auditDebugRedisMaxLen)
	}
	if cfg.auditByTypeDiagMaxLen < 0 {
		return fmt.Errorf("audit-bytype-diag-max-len must be >= 0, got %d", cfg.auditByTypeDiagMaxLen)
	}
	if cfg.auditEventBodyTTL <= 0 {
		return fmt.Errorf("audit-event-body-ttl must be > 0, got %s", cfg.auditEventBodyTTL)
	}
	if cfg.auditEventBodyWait < 0 {
		return fmt.Errorf("audit-event-body-wait must be >= 0, got %s", cfg.auditEventBodyWait)
	}
	return nil
}

func validateAdmissionWebhookConfig(cfg appConfig) error {
	if !cfg.admissionWebhookEnabled {
		return nil
	}
	if cfg.admissionWebhookPort <= 0 {
		return fmt.Errorf("admission-webhook-port must be > 0, got %d", cfg.admissionWebhookPort)
	}
	if strings.TrimSpace(cfg.admissionWebhookCertPath) == "" {
		return errors.New("admission-webhook-cert-path is required when admission webhook is enabled")
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

	// serving is true while the listener socket is bound and accepting. It gates the audit
	// half of the readiness probe (see auditServingReadyCheck): because the kube-apiserver
	// reaches this server through a Service, readiness controls endpoint membership, so the
	// apiserver must not route audit events here until the listener is actually open.
	serving atomic.Bool
}

// Serving reports whether the audit ingress listener is bound and accepting connections.
func (r *auditServerRunnable) Serving() bool {
	return r.serving.Load()
}

type serverTimeouts struct {
	read  time.Duration
	write time.Duration
	idle  time.Duration
}

func (r *auditServerRunnable) Start(ctx context.Context) error {
	setupLog.Info("Starting dedicated audit ingress server", "address", r.server.Addr)

	// Bind the listener explicitly (rather than via ListenAndServe) so the "serving" flag flips
	// only once the socket is actually open — the precise moment the apiserver can be allowed to
	// route audit traffic here. A bind failure surfaces before Serve, so readiness never reports
	// ready for a server that never came up.
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", r.server.Addr)
	if err != nil {
		return fmt.Errorf("audit ingress server failed to bind %q: %w", r.server.Addr, err)
	}
	r.serving.Store(true)
	defer r.serving.Store(false)

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultAuditShutdownTimeout)
		defer cancel()
		if err := r.server.Shutdown(shutdownCtx); err != nil {
			setupLog.Error(err, "Failed to shutdown dedicated audit ingress server")
		}
	}()

	if r.tlsEnabled {
		err = r.server.ServeTLS(listener, "", "")
	} else {
		err = r.server.Serve(listener)
	}
	<-shutdownDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("audit ingress server failed: %w", err)
}

//nolint:unused // Attribution mode will re-enable the optional audit server wiring.
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
		serverTLS, err = buildAuditServerTLSConfig(cfg, tlsOpts)
		if err != nil {
			return nil, nil, err
		}
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
	mux.Handle("/audit-webhook-additional", handler)
	mux.Handle("/audit-webhook-additional/", handler)
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

func buildAuditServerTLSConfig(cfg appConfig, tlsOpts []func(*tls.Config)) (*tls.Config, error) {
	serverTLS := buildServerTLSConfig(tlsOpts)

	clientCAPool, err := loadCertPoolFromPEMFile(filepath.Join(cfg.auditClientCAPath, cfg.auditClientCAName))
	if err != nil {
		return nil, fmt.Errorf("failed to load audit client CA: %w", err)
	}

	serverTLS.ClientAuth = tls.RequireAndVerifyClientCert
	serverTLS.ClientCAs = clientCAPool

	return serverTLS, nil
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

func loadCertPoolFromPEMFile(path string) (*x509.CertPool, error) {
	pemData, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read certificate bundle %q: %w", path, err)
	}

	pool := x509.NewCertPool()
	if ok := pool.AppendCertsFromPEM(pemData); !ok {
		return nil, fmt.Errorf("parse certificate bundle %q: no certificates found", path)
	}

	return pool, nil
}

// newManager creates a new controller-runtime Manager with common options.
func newManager(
	metricsOptions metricsserver.Options,
	probeAddr string,
	cfg appConfig,
	baseTLS []func(*tls.Config),
) ctrl.Manager {
	var webhookServer ctrlwebhook.Server
	if cfg.admissionWebhookEnabled {
		webhookServer = ctrlwebhook.NewServer(ctrlwebhook.Options{
			Port:     cfg.admissionWebhookPort,
			CertDir:  cfg.admissionWebhookCertPath,
			CertName: cfg.admissionWebhookCertName,
			KeyName:  cfg.admissionWebhookCertKey,
			TLSOpts:  baseTLS,
		})
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsOptions,
		HealthProbeBindAddress: probeAddr,
		WebhookServer:          webhookServer,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}
	return mgr
}

func setupAdmissionWebhooks(mgr ctrl.Manager) {
	mgr.GetWebhookServer().Register(
		webhookhandler.ValidateAdmissionWebhookPath,
		&ctrladmission.Webhook{Handler: webhookhandler.AdmissionAllowHandler{}},
	)
}

// addCertWatchersToManager attaches optional certificate watchers to the manager.
func addCertWatchersToManager(
	mgr ctrl.Manager,
	metricsCertWatcher, auditCertWatcher *certwatcher.CertWatcher,
) {
	watchers := []struct {
		component string
		watcher   *certwatcher.CertWatcher
	}{
		{component: "metrics", watcher: metricsCertWatcher},
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
