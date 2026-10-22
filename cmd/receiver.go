// SPDX-License-Identifier: Apache-2.0

// The push receiver's listener: a plain HTTP server for Git hosts' push webhooks. It is its own
// listener because the audit listener requires a client certificate signed by the audit CA, which
// no Git host can present. See docs/configuration.md, "Push receiver settings".

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	webhookhandler "github.com/ConfigButler/gitops-reverser/internal/webhook"
)

const (
	// receiverOff is the --receiver-bind-address value that serves no receiver, the same sentinel
	// --metrics-bind-address uses.
	receiverOff = "0"

	// The receiver's server timeouts. A delivery is one small POST answered without waiting on the
	// remote (the branch worker reads the remote later, on its own loop), so these only have to
	// outlast a slow client sending a body up to the 2 MiB cap, and keep an idle or trickling
	// connection from holding a socket. They are not flags: nothing a Git host does needs them
	// tuned, and every knob is one more thing to get wrong.
	receiverReadHeaderTimeout = 10 * time.Second
	receiverReadTimeout       = 30 * time.Second
	receiverWriteTimeout      = 30 * time.Second
	receiverIdleTimeout       = 60 * time.Second
	receiverShutdownTimeout   = 10 * time.Second
)

// bindReceiverFlags registers the push receiver's flags.
func bindReceiverFlags(fs *flag.FlagSet, cfg *appConfig) {
	fs.StringVar(&cfg.receiverBindAddress, "receiver-bind-address", receiverOff,
		"Address (host:port) the push receiver listens on, over plain HTTP: a Git host's push webhook "+
			"points here plus a GitReceiver's status.webhookPath (/git-push/<digest of the token>), so a "+
			"foreign push is noticed in seconds instead of at the next refresh. Expose it through an "+
			"Ingress or Gateway that terminates TLS. Default 0 serves no receiver; foreign pushes are "+
			"then still found by the next rejected push or --git-refresh-interval. Served by the pod that "+
			"runs the branch workers, because a delivery drives one.")
}

// receiverEnabled reports whether a push receiver listener is configured.
func receiverEnabled(cfg appConfig) bool {
	addr := strings.TrimSpace(cfg.receiverBindAddress)
	return addr != "" && addr != receiverOff
}

// validateReceiverConfig rejects a malformed --receiver-bind-address before the manager starts,
// rather than letting the listener fail to bind after leader election.
func validateReceiverConfig(cfg appConfig) error {
	if !receiverEnabled(cfg) {
		return nil
	}
	if _, _, err := splitBindAddress(cfg.receiverBindAddress); err != nil {
		return fmt.Errorf("invalid receiver-bind-address %q (use host:port, or 0 to serve no receiver): %w",
			cfg.receiverBindAddress, err)
	}
	return nil
}

// setupPushReceiver adds the push receiver listener to the manager when --receiver-bind-address
// is set. notifier is the branch workers' owner (the WorkerManager).
func setupPushReceiver(mgr ctrl.Manager, cfg appConfig, notifier webhookhandler.PushNotifier) {
	if !receiverEnabled(cfg) {
		setupLog.Info("push receiver off (--receiver-bind-address=0): foreign pushes are found by the " +
			"next rejected push or the periodic refresh")
		return
	}
	// A route is found through this index on the cached GitReceivers, so a path nothing publishes
	// costs no API read, and the providers a delivery is for through the second one.
	fatalIfErr(mgr.GetFieldIndexer().IndexField(context.Background(), &configbutleraiv1alpha3.GitReceiver{},
		webhookhandler.GitReceiverWebhookPathIndex, webhookhandler.IndexGitReceiverWebhookPath),
		"unable to index GitReceivers by status.webhookPath")
	fatalIfErr(mgr.GetFieldIndexer().IndexField(context.Background(), &configbutleraiv1alpha3.GitProvider{},
		webhookhandler.GitProviderGitReceiverIndex, webhookhandler.IndexGitProviderGitReceiver),
		"unable to index GitProviders by spec.gitReceiverRef")
	// The manager client reads the kinds from its cache and Secrets straight from the API server
	// (Secrets are excluded from the cache), so a rotated token is used on the next delivery.
	handler, err := webhookhandler.NewReceiverHandler(webhookhandler.ReceiverHandlerConfig{
		Reader:   mgr.GetClient(),
		Notifier: notifier,
	})
	fatalIfErr(err, "unable to build push receiver handler")
	fatalIfErr(mgr.Add(newReceiverServerRunnable(cfg.receiverBindAddress, handler)),
		"unable to add push receiver server runnable")
	setupLog.Info("push receiver enabled", "address", cfg.receiverBindAddress,
		"path", webhookhandler.ReceiverPathPrefix+"<status.webhookPath digest>")
}

// buildReceiverServeMux routes only the receiver prefix to the handler. Everything else is the
// mux's own 404, so the listener answers nothing but /git-push/.
func buildReceiverServeMux(handler http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(webhookhandler.ReceiverPathPrefix, handler)
	return mux
}

// receiverServerRunnable serves the push receiver as a manager Runnable.
type receiverServerRunnable struct {
	server *http.Server

	// boundAddr is the listener's address once it is bound, nil before and after. It is what a
	// test dials when the configured port is 0.
	boundAddr atomic.Pointer[string]
}

func newReceiverServerRunnable(addr string, handler http.Handler) *receiverServerRunnable {
	return &receiverServerRunnable{server: &http.Server{
		Addr:              strings.TrimSpace(addr),
		Handler:           buildReceiverServeMux(handler),
		ReadHeaderTimeout: receiverReadHeaderTimeout,
		ReadTimeout:       receiverReadTimeout,
		WriteTimeout:      receiverWriteTimeout,
		IdleTimeout:       receiverIdleTimeout,
	}}
}

// NeedLeaderElection keeps the receiver with the branch workers: a delivery drives a branch worker,
// and the WorkerManager asks for leader election too. Leader election is not switched on today and
// the chart runs one replica, so this only keeps the two together should it ever be: served on a
// standby, every delivery would find no worker and answer matched=0 for a branch that is written.
func (r *receiverServerRunnable) NeedLeaderElection() bool {
	return true
}

// Start serves until ctx is cancelled, then shuts down gracefully.
func (r *receiverServerRunnable) Start(ctx context.Context) error {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", r.server.Addr)
	if err != nil {
		return fmt.Errorf("push receiver failed to bind %q: %w", r.server.Addr, err)
	}
	bound := listener.Addr().String()
	r.boundAddr.Store(&bound)
	defer r.boundAddr.Store(nil)
	setupLog.Info("Starting push receiver", "address", bound)

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), receiverShutdownTimeout)
		defer cancel()
		if err := r.server.Shutdown(shutdownCtx); err != nil {
			setupLog.Error(err, "Failed to shut down the push receiver")
		}
	}()

	err = r.server.Serve(listener)
	<-shutdownDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("push receiver failed: %w", err)
}
