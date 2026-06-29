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
	flagParseFailureExitCode        = 2
	defaultAdmissionWebhookBindAddr = ":9443"
	defaultAuditBindAddr            = "0.0.0.0:9444"
	defaultAuditMaxBodyBytes        = int64(10 * 1024 * 1024)
	defaultAuditReadTimeout         = 15 * time.Second
	defaultAuditWriteTimeout        = 30 * time.Second
	defaultAuditIdleTimeout         = 60 * time.Second
	defaultAuditShutdownTimeout     = 10 * time.Second
	defaultBranchBufferMaxSizeStr   = "8Mi"
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
		"auditBindAddress", cfg.auditBindAddress,
		"auditInsecure", cfg.auditInsecure,
		"admissionWebhookEnabled", cfg.admissionWebhookEnabled,
		"admissionWebhookBindAddress", cfg.admissionWebhookBindAddress)
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

	// Valkey/Redis is a required dependency: it holds each GitTarget's watch resume cursors so work is
	// re-picked up exactly where it left off after a restart or reconnect, and it underpins HA and the
	// planned durable branch-worker queue. The cursor store is always wired and the readiness gate keeps
	// the pod not-ready until Redis is reachable. Author attribution is a separate optional layer built
	// on the same connection only when enabled (see below) — the store itself never depends on it.
	redisStore, err := queue.NewRedisStore(queue.RedisStoreConfig{
		Addr:       cfg.redisAddr,
		Username:   cfg.redisUsername,
		AuthValue:  cfg.redisPassword,
		DB:         cfg.redisDB,
		TLSEnabled: !cfg.redisInsecure,
	})
	fatalIfErr(err, "unable to build Redis cursor store")
	watchMgr.WatchCursorStore = redisStore

	redisGate := newRedisReadinessGate(redisStore)
	fatalIfErr(mgr.Add(redisGate), "unable to add redis readiness gate")

	// Optional author attribution. When enabled, the attribution index is built on the Redis connection,
	// the audit webhook records minimal facts, and live watch events are author-attributed when a fact
	// matches. When disabled (committer-only), no attribution index exists and commits use the configured
	// committer identity; the cursor store above is unaffected.
	var (
		auditRunnable    *auditServerRunnable
		auditCertWatcher *certwatcher.CertWatcher
		attributionIndex *queue.AttributionIndex
	)
	if cfg.authorAttribution {
		attributionIndex = redisStore.AttributionIndex(cfg.attributionFactTTL)

		auditHandler, err := webhookhandler.NewAuditHandler(webhookhandler.AuditHandlerConfig{
			MaxRequestBodyBytes: cfg.auditMaxRequestBodyBytes,
			FactRecorder:        attributionIndex,
		})
		fatalIfErr(err, "unable to build audit handler")

		var initErr error
		auditRunnable, auditCertWatcher, initErr = initAuditServerRunnable(cfg, tlsOpts, auditHandler)
		fatalIfErr(initErr, "unable to initialize audit ingress server")
		fatalIfErr(mgr.Add(auditRunnable), "unable to add audit ingress server runnable")

		watchMgr.AuthorResolver = watch.NewAuthorResolver(
			attributionIndex,
			cfg.attributionGrace,
			ctrl.Log.WithName("attribution"),
		)
		setupLog.Info("author attribution enabled: matched audit facts name the commit author",
			"redisAddr", cfg.redisAddr, "grace", cfg.attributionGrace.String())
	} else {
		setupLog.Info("committer-only mode: author attribution disabled; commits use the configured "+
			"committer identity", "redisAddr", cfg.redisAddr)
	}

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
	// Command authorship is captured at admission by the validate-operator-types webhook and
	// lives in its own Redis corner (author:v1:command), independent of
	// --author-attribution (which governs mirrored-resource attribution). It is wired
	// whenever the admission server is on; the controller reads the captured submitter
	// back with no wait (docs/design/commitrequest-admission-authorship.md §2, §6).
	//
	// AuthorLookup must be a nil interface — not a non-nil interface wrapping a nil
	// *CommandAuthorStore — when the webhook is off, so the controller's nil check
	// selects finalize-as-committer immediately (AuthorAttributed=False) instead of
	// dereferencing a nil store.
	var commandAuthorStore *queue.CommandAuthorStore
	var commandAuthorLookup controller.CommandAuthorLookup
	if cfg.admissionWebhookEnabled {
		commandAuthorStore = redisStore.CommandAuthorStore()
		commandAuthorLookup = commandAuthorStore
		setupLog.Info("validate-operator-types webhook enabled: command submitters are captured at admission " +
			"and named as the commit author")
	}
	if err := (&controller.CommitRequestReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		APIReader:    mgr.GetAPIReader(),
		Finalizer:    eventRouter,
		AuthorLookup: commandAuthorLookup,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "CommitRequest")
		os.Exit(1)
	}
	if cfg.admissionWebhookEnabled {
		setupAdmissionWebhooks(mgr, commandAuthorStore)
	}
	// +kubebuilder:scaffold:builder

	// Cert watchers (auditCertWatcher is nil in committer-only mode / --audit-insecure).
	addCertWatchersToManager(mgr, metricsCertWatcher, auditCertWatcher)

	// Health checks: readiness reflects the audit ingress preconditions when attribution is on,
	// and is a bare liveness ping otherwise. auditProbe must be a nil interface when disabled.
	var auditProbe auditReadinessProbe
	if auditRunnable != nil {
		auditProbe = auditRunnable
	}
	addHealthChecks(mgr, auditProbe, auditCertWatcher, redisGate)

	// Start manager
	setupLog.Info("starting manager")
	fatalIfErr(mgr.Start(setupCtx), "problem running manager")
}

// appConfig holds parsed CLI flags and logging options.
type appConfig struct {
	metricsAddr                 string
	metricsCertPath             string
	metricsCertName             string
	metricsCertKey              string
	probeAddr                   string
	metricsInsecure             bool
	admissionWebhookEnabled     bool
	admissionWebhookBindAddress string
	admissionWebhookCertPath    string
	admissionWebhookCertName    string
	admissionWebhookCertKey     string
	enableHTTP2                 bool
	auditBindAddress            string
	auditCertPath               string
	auditCertName               string
	auditCertKey                string
	auditClientCAPath           string
	auditClientCAName           string
	auditInsecure               bool
	auditMaxRequestBodyBytes    int64
	auditReadTimeout            time.Duration
	auditWriteTimeout           time.Duration
	auditIdleTimeout            time.Duration
	redisAddr                   string
	redisUsername               string
	redisPassword               string
	redisDB                     int
	redisInsecure               bool
	authorAttribution           bool
	attributionFactTTL          time.Duration
	attributionGrace            time.Duration
	branchBufferMaxBytes        int64
	sensitiveResources          types.SensitiveResourcePolicy
	sshHostKeys                 git.SSHHostKeyConfig
	zapOpts                     zap.Options
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
		"Serve the metrics endpoint over plain HTTP instead of HTTPS (default false; HTTPS).")
	fs.BoolVar(&cfg.admissionWebhookEnabled, "admission-webhook", false,
		"Serve the validating admission webhook endpoint (default false; off). When true, the webhook "+
			"server binds --admission-webhook-bind-address and requires --admission-webhook-cert-path.")
	fs.StringVar(&cfg.admissionWebhookBindAddress, "admission-webhook-bind-address", defaultAdmissionWebhookBindAddr,
		"Address (host:port) the validating admission webhook HTTPS server binds to; "+
			"an empty host (\":9443\") binds all interfaces.")
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
	fs.StringVar(&cfg.auditBindAddress, "audit-bind-address", defaultAuditBindAddr,
		"Address (host:port) the dedicated audit ingress HTTPS server binds to; "+
			"an empty host (\":9444\") binds all interfaces.")
	bindServerCertFlags(fs, "audit", "audit ingress TLS", &cfg.auditCertPath, &cfg.auditCertName, &cfg.auditCertKey)
	fs.StringVar(&cfg.auditClientCAPath, "audit-client-ca-path", "/tmp/k8s-audit-server/audit-client-ca",
		"Directory that contains the audit client CA certificate used to verify kube-apiserver client certificates.")
	fs.StringVar(&cfg.auditClientCAName, "audit-client-ca-name", "tls.crt",
		"File name of the audit client CA certificate used to verify kube-apiserver client certificates.")
	fs.BoolVar(&cfg.auditInsecure, "audit-insecure", false,
		"Serve the audit ingress endpoint over plain HTTP instead of HTTPS (default false; HTTPS).")
	fs.Int64Var(&cfg.auditMaxRequestBodyBytes, "audit-max-request-body-bytes", defaultAuditMaxBodyBytes,
		"Maximum request body accepted by the audit ingress handler, in bytes (default 10485760, i.e. 10Mi).")
	fs.DurationVar(&cfg.auditReadTimeout, "audit-read-timeout", defaultAuditReadTimeout,
		"Read timeout for the audit ingress HTTPS server (duration string; default 15s).")
	fs.DurationVar(&cfg.auditWriteTimeout, "audit-write-timeout", defaultAuditWriteTimeout,
		"Write timeout for the audit ingress HTTPS server (duration string; default 30s).")
	fs.DurationVar(&cfg.auditIdleTimeout, "audit-idle-timeout", defaultAuditIdleTimeout,
		"Idle timeout for the audit ingress HTTPS server (duration string; default 60s).")
	fs.StringVar(&cfg.redisAddr, "redis-addr", "valkey:6379",
		"Redis/Valkey address (host:port). Required — it holds each GitTarget's watch resume cursors "+
			"(state continuity), and, when author attribution is enabled, the attribution facts.")
	fs.BoolVar(&cfg.authorAttribution, "author-attribution", true,
		"Name the real actor (human or service account) who caused each change as the Git commit author, "+
			"resolved from matching audit facts; this runs the audit webhook ingress (default true). When "+
			"false, every commit is authored by the configured committer identity (committer-only mode).")
	fs.StringVar(&cfg.redisUsername, "redis-username", "", "Optional Redis username.")
	fs.StringVar(
		&cfg.redisPassword,
		"redis-password",
		os.Getenv("REDIS_PASSWORD"),
		"Redis password. Prefer setting via REDIS_PASSWORD env var from a Secret.",
	)
	fs.IntVar(&cfg.redisDB, "redis-db", 0, "Redis database index (default 0).")
	fs.BoolVar(&cfg.redisInsecure, "redis-insecure", false,
		"Connect to Redis over plain TCP instead of TLS (default false; TLS). Redis carries each "+
			"GitTarget's watch cursors and, when attribution is on, the audit facts — prefer TLS. Set "+
			"this only for a trusted in-cluster Redis/Valkey that does not serve TLS.")
	fs.DurationVar(&cfg.attributionFactTTL, "author-attribution-ttl", queue.DefaultAttributionFactTTL,
		"How long an attribution fact is retained waiting for the matching watch event to join it "+
			"(duration string; default 10m).")
	fs.DurationVar(&cfg.attributionGrace, "author-attribution-grace", watch.DefaultAttributionGraceWindow,
		"Bounded per-event wait for a matching audit fact to arrive before a watch event ships as the "+
			"configured committer (duration string; default 3s). Larger values raise attribution hit-rate "+
			"at the cost of commit latency.")
	branchBufferMaxSizeStr := os.Getenv("BRANCH_BUFFER_MAX_SIZE")
	if branchBufferMaxSizeStr == "" {
		branchBufferMaxSizeStr = defaultBranchBufferMaxSizeStr
	}
	var branchBufferMaxSizeFlag string
	fs.StringVar(&branchBufferMaxSizeFlag, "branch-buffer-max-size", branchBufferMaxSizeStr,
		"Maximum in-memory event buffer per branch worker, as a Kubernetes resource quantity "+
			"(e.g. 8Mi, 1Gi; default 8Mi). Bounds pod memory under bursty workloads; not user-facing.")
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

	bufferQuantity, err := resource.ParseQuantity(branchBufferMaxSizeFlag)
	if err != nil {
		return appConfig{}, fmt.Errorf("invalid --branch-buffer-max-size %q: %w", branchBufferMaxSizeFlag, err)
	}
	cfg.branchBufferMaxBytes, _ = bufferQuantity.AsInt64()
	if cfg.branchBufferMaxBytes <= 0 {
		return appConfig{}, fmt.Errorf("--branch-buffer-max-size must be > 0, got %s", branchBufferMaxSizeFlag)
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
	if cfg.attributionGrace < 0 {
		return fmt.Errorf("author-attribution-grace must be >= 0, got %s", cfg.attributionGrace)
	}
	if cfg.attributionFactTTL <= 0 {
		return fmt.Errorf("author-attribution-ttl must be > 0, got %s", cfg.attributionFactTTL)
	}
	// Redis/Valkey is required in every mode: it holds each GitTarget's watch resume cursors. This is
	// independent of author attribution, which only adds commit-author naming on top.
	if strings.TrimSpace(cfg.redisAddr) == "" {
		return errors.New("redis-addr is required: Valkey/Redis holds each GitTarget's watch resume cursors")
	}
	if cfg.redisDB < 0 {
		return fmt.Errorf("redis-db must be >= 0, got %d", cfg.redisDB)
	}
	if !cfg.authorAttribution {
		// Committer-only mode: the audit ingress server is not started, so its server/TLS settings
		// are irrelevant. Redis is still required and was validated above.
		return nil
	}
	if _, _, err := splitBindAddress(cfg.auditBindAddress); err != nil {
		return fmt.Errorf("invalid audit-bind-address %q: %w", cfg.auditBindAddress, err)
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
	if !cfg.auditInsecure && strings.TrimSpace(cfg.auditClientCAPath) == "" {
		return errors.New("audit-client-ca-path is required when audit TLS is enabled")
	}
	return nil
}

func validateAdmissionWebhookConfig(cfg appConfig) error {
	if !cfg.admissionWebhookEnabled {
		return nil
	}
	if _, _, err := splitBindAddress(cfg.admissionWebhookBindAddress); err != nil {
		return fmt.Errorf("invalid admission-webhook-bind-address %q: %w", cfg.admissionWebhookBindAddress, err)
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
		cfg.auditBindAddress,
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

// splitBindAddress parses a host:port bind address into its host and numeric
// port. An empty host (e.g. ":9443") is valid and binds all interfaces.
func splitBindAddress(addr string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return "", 0, fmt.Errorf("must be host:port: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("port %q must be a number", portStr)
	}
	if port <= 0 {
		return "", 0, fmt.Errorf("port must be > 0, got %d", port)
	}
	return host, port, nil
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
		// The bind address was validated in validateAdmissionWebhookConfig; controller-runtime's
		// webhook server takes Host and Port separately, so split it back here.
		host, port, err := splitBindAddress(cfg.admissionWebhookBindAddress)
		if err != nil {
			setupLog.Error(err, "invalid --admission-webhook-bind-address")
			os.Exit(1)
		}
		webhookServer = ctrlwebhook.NewServer(ctrlwebhook.Options{
			Host:     host,
			Port:     port,
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

// setupAdmissionWebhooks registers both handlers on the one admission server: the
// always-allow observer (a future-policy extension point) and the validate-operator-types
// handler that captures the submitter of our own command kinds into commandAuthorStore.
func setupAdmissionWebhooks(mgr ctrl.Manager, commandAuthorStore *queue.CommandAuthorStore) {
	mgr.GetWebhookServer().Register(
		webhookhandler.ValidateAllPath,
		&ctrladmission.Webhook{Handler: webhookhandler.AdmissionAllowHandler{}},
	)
	mgr.GetWebhookServer().Register(
		webhookhandler.ValidateOperatorTypesPath,
		&ctrladmission.Webhook{Handler: &webhookhandler.ValidateOperatorTypesHandler{Store: commandAuthorStore}},
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
