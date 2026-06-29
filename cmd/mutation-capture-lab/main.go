/*
SPDX-License-Identifier: Apache-2.0

Copyright 2025 ConfigButler

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

// Command mutation-capture-lab is the mutation-capture lab binary: a minimal set
// of recorders (native watch, audit webhook, validating admission webhook) whose
// output is a versioned corpus of the exact structures Kubernetes emits. It is
// deliberately NOT a second GitOps Reverser; see
// docs/design/mutation-capture-lab-design.md.
//
// It deliberately serves the SAME webhook URLs as the product
// (/validate-all, /audit-webhook, /audit-webhook-additional) so a
// lab deployment can swap the product image without reconfiguring the cluster's
// admission/audit wiring. The /audit-webhook-additional endpoint is the
// integration point the apiservice-audit-proxy posts enriched bodies to; the lab
// records it separately so the corpus shows what that proxy adds — and whether a
// live watch already carries it, which would make the proxy unnecessary.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ConfigButler/gitops-reverser/internal/mutationlab"
	"github.com/ConfigButler/gitops-reverser/internal/mutationlab/labserver"
	"github.com/ConfigButler/gitops-reverser/internal/mutationlab/recorder"
	"github.com/ConfigButler/gitops-reverser/internal/mutationlab/store"
)

const (
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 5 * time.Second
)

type config struct {
	admissionAddr string
	admissionCert string
	auditAddr     string
	auditCert     string
	auditClientCA string
	apiAddr       string
	watchSpec     string
	kubeconfig    string
}

func parseFlags() config {
	var cfg config
	flag.StringVar(&cfg.admissionAddr, "admission-addr", ":9443",
		"Address for the validating admission webhook HTTPS server (/validate-all).")
	flag.StringVar(&cfg.admissionCert, "admission-cert-dir", "",
		"Directory holding tls.crt/tls.key for the admission server. Empty serves plain HTTP (local only).")
	flag.StringVar(&cfg.auditAddr, "audit-addr", ":8444",
		"Address for the audit webhook HTTPS server (/audit-webhook, /audit-webhook-additional).")
	flag.StringVar(&cfg.auditCert, "audit-cert-dir", "",
		"Directory holding tls.crt/tls.key for the audit server. Empty serves plain HTTP (local only).")
	flag.StringVar(&cfg.auditClientCA, "audit-client-ca", "",
		"Optional PEM file; when set the audit server requires and verifies an apiserver client cert.")
	flag.StringVar(&cfg.apiAddr, "api-addr", ":8080",
		"Address for the plain-HTTP records API (/records, /healthz).")
	flag.StringVar(&cfg.watchSpec, "watch-resources", "v1/configmaps",
		"Comma-separated GVRs to watch (group/version/resource or version/resource).")
	flag.StringVar(
		&cfg.kubeconfig,
		"kubeconfig",
		"",
		"Path to a kubeconfig; empty uses the in-cluster config. The watch recorder is skipped if no client is available.",
	)
	flag.Parse()
	return cfg
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg := parseFlags()

	s := store.New()
	admission := recorder.NewAdmission(s, recorder.RejectByLabel)
	conversion := recorder.NewConversion(s)
	auditOfficial := recorder.NewAudit(s, mutationlab.SourceAudit)
	auditAdditional := recorder.NewAudit(s, mutationlab.SourceAuditAdditional)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	watchClient := startWatchRecorder(ctx, logger, s, cfg)

	api := labserver.NewAPI(s)
	if watchClient != nil {
		api = labserver.NewAPI(s, recorder.NewWatchProbe(watchClient))
	}
	servers := buildServers(cfg, admission, conversion, auditOfficial, auditAdditional, api)
	run(ctx, logger, cfg, servers)
}

// server pairs an *http.Server with the cert dir it should serve TLS from ("" =
// plain HTTP) and an optional client-CA file for mTLS.
type server struct {
	name     string
	srv      *http.Server
	certDir  string
	clientCA string
}

func buildServers(
	cfg config,
	admission, conversion, auditOfficial, auditAdditional http.Handler,
	api *labserver.API,
) []server {
	admissionMux := http.NewServeMux()
	admissionMux.Handle("/validate-all", admission)
	// The CRD conversion webhook reuses the admission server's TLS cert and port,
	// so M3 needs no new certificate — just this extra path (see the M3 design).
	admissionMux.Handle("/convert", conversion)

	auditMux := http.NewServeMux()
	auditMux.Handle("/audit-webhook", auditOfficial)
	auditMux.Handle("/audit-webhook-additional", auditAdditional)

	return []server{
		{name: "admission", certDir: cfg.admissionCert, srv: &http.Server{
			Addr: cfg.admissionAddr, Handler: admissionMux, ReadHeaderTimeout: readHeaderTimeout}},
		{name: "audit", certDir: cfg.auditCert, clientCA: cfg.auditClientCA, srv: &http.Server{
			Addr: cfg.auditAddr, Handler: auditMux, ReadHeaderTimeout: readHeaderTimeout}},
		{name: "api", srv: &http.Server{
			Addr: cfg.apiAddr, Handler: api.Routes(), ReadHeaderTimeout: readHeaderTimeout}},
	}
}

func run(ctx context.Context, logger *slog.Logger, cfg config, servers []server) {
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			serve(logger, s)
		}()
	}

	logger.InfoContext(ctx, "mutation-capture-lab serving",
		"admission", cfg.admissionAddr, "audit", cfg.auditAddr, "api", cfg.apiAddr)

	<-ctx.Done()
	logger.InfoContext(ctx, "shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	// Shut the servers down concurrently: with a shared shutdownCtx, a sequential loop
	// lets one slow server consume the whole timeout and leave the rest no time to drain.
	var shutdownWg sync.WaitGroup
	for _, s := range servers {
		shutdownWg.Add(1)
		go func() {
			defer shutdownWg.Done()
			_ = s.srv.Shutdown(shutdownCtx)
		}()
	}
	shutdownWg.Wait()
	wg.Wait()
}

func serve(logger *slog.Logger, s server) {
	var err error
	if s.certDir == "" {
		err = s.srv.ListenAndServe()
	} else {
		s.srv.TLSConfig, err = tlsConfig(s.certDir, s.clientCA)
		if err == nil {
			err = s.srv.ListenAndServeTLS("", "")
		}
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped", "name", s.name, "err", err)
	}
}

func tlsConfig(certDir, clientCAFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(filepath.Join(certDir, "tls.crt"), filepath.Join(certDir, "tls.key"))
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	if clientCAFile != "" {
		pem, err := os.ReadFile(clientCAFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("audit-client-ca: no certificates parsed")
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

func startWatchRecorder(ctx context.Context, logger *slog.Logger, s *store.Store, cfg config) dynamic.Interface {
	gvrs, err := labserver.ParseGVRs(cfg.watchSpec)
	if err != nil {
		logger.ErrorContext(ctx, "invalid --watch-resources; watch recorder disabled", "err", err)
		return nil
	}
	restCfg, err := restConfig(cfg.kubeconfig)
	if err != nil {
		logger.WarnContext(ctx, "no Kubernetes client; watch recorder disabled", "err", err)
		return nil
	}
	client, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		logger.ErrorContext(ctx, "dynamic client; watch recorder disabled", "err", err)
		return nil
	}
	recorder.NewWatch(s, client).Start(ctx, gvrs)
	logger.InfoContext(ctx, "watch recorder started", "resources", cfg.watchSpec)
	return client
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}
