// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
)

// --- auditServerRunnable.Serving() ---

func TestAuditServerRunnable_Serving_LifecycleInsecure(t *testing.T) {
	r := &auditServerRunnable{
		server:     buildHTTPServer("127.0.0.1:0", http.NewServeMux(), nil, serverTimeouts{}),
		tlsEnabled: false,
	}

	assert.False(t, r.Serving(), "must not report serving before Start")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	require.Eventually(t, r.Serving, 2*time.Second, 5*time.Millisecond,
		"must report serving once the listener is bound")

	cancel()
	require.NoError(t, <-done, "graceful shutdown returns nil")
	assert.False(t, r.Serving(), "must not report serving after shutdown")
}

func TestAuditServerRunnable_Serving_BindFailureStaysNotServing(t *testing.T) {
	// Occupy a port so the runnable's bind to the same address fails deterministically.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = occupied.Close() }()

	r := &auditServerRunnable{
		server:     buildHTTPServer(occupied.Addr().String(), http.NewServeMux(), nil, serverTimeouts{}),
		tlsEnabled: false,
	}

	err = r.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to bind")
	assert.False(t, r.Serving(), "a server that never bound must not report serving")
}

// --- redisReadinessGate ---

type fakePinger struct {
	failures atomic.Int32 // number of remaining PINGs that should fail
	calls    atomic.Int32
}

func (f *fakePinger) Ping(_ context.Context) error {
	f.calls.Add(1)
	if f.failures.Load() > 0 {
		f.failures.Add(-1)
		return errors.New("dial tcp: connection refused")
	}
	return nil
}

func TestRedisReadinessGate_Err_BeforeAnyAttempt(t *testing.T) {
	g := newRedisReadinessGate(&fakePinger{})
	err := g.Err()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not yet established")
}

func TestRedisReadinessGate_LatchesReadyAfterRetries(t *testing.T) {
	p := &fakePinger{}
	p.failures.Store(2)

	g := newRedisReadinessGate(p)
	g.timeout = 50 * time.Millisecond
	g.interval = time.Millisecond

	require.NoError(t, g.Start(context.Background()))
	assert.NoError(t, g.Err(), "gate must latch ready after the first successful ping")
	assert.Equal(t, int32(3), p.calls.Load(), "two failures then one success")

	// Once latched, it stays ready and never pings again.
	require.NoError(t, g.Err())
	assert.Equal(t, int32(3), p.calls.Load())
}

func TestRedisReadinessGate_NotReadyWhenContextCancelledBeforeSuccess(t *testing.T) {
	p := &fakePinger{}
	p.failures.Store(1 << 30) // effectively always fail

	g := newRedisReadinessGate(p)
	g.timeout = 20 * time.Millisecond
	g.interval = time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	require.NoError(t, g.Start(ctx), "Start returns nil when the manager context is cancelled")
	err := g.Err()
	require.Error(t, err, "gate stays not-ready when it never connected")
	assert.Contains(t, err.Error(), "not yet reachable")
	assert.Contains(t, err.Error(), "connection refused", "surfaces the last ping error")
}

// --- readyz composition ---

func TestAuditServingReadyCheck(t *testing.T) {
	require.NoError(t, auditServingReadyCheck(stubProbe(true))(nil))

	err := auditServingReadyCheck(stubProbe(false))(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not yet serving")
}

func TestAuditCertReadyCheck_NilWatcherIsNoCheck(t *testing.T) {
	assert.Nil(t, auditCertReadyCheck(nil), "insecure mode has no cert sub-check")
}

func TestAuditCertReadyCheck_LoadedCertIsReady(t *testing.T) {
	certPath, keyPath := mustWriteServerCertKey(t)
	watcher, err := certwatcher.New(certPath, keyPath)
	require.NoError(t, err)

	check := auditCertReadyCheck(watcher)
	require.NotNil(t, check)
	assert.NoError(t, check(nil), "a loaded, parseable cert is ready")
}

func TestCombineReadyChecks_FirstFailureWinsAndNilSkipped(t *testing.T) {
	called := 0
	ok := func(_ *http.Request) error { called++; return nil }
	boom := func(_ *http.Request) error { called++; return errors.New("boom") }
	never := func(_ *http.Request) error { t.Fatal("must short-circuit before this check"); return nil }

	// nil checks are skipped; all-pass returns nil.
	require.NoError(t, combineReadyChecks(ok, nil, ok)(nil))
	assert.Equal(t, 2, called)

	// first failure wins and later checks do not run.
	called = 0
	err := combineReadyChecks(ok, boom, never)(nil)
	require.Error(t, err)
	assert.Equal(t, "boom", err.Error())
	assert.Equal(t, 2, called)
}

// --- helpers ---

type stubProbe bool

func (s stubProbe) Serving() bool { return bool(s) }

// mustWriteServerCertKey generates a self-signed server cert + EC key pair, writes both as PEM to
// a temp dir, and returns their paths (cert, key) — enough to construct a real certwatcher.
func mustWriteServerCertKey(t *testing.T) (string, string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "gitops-reverser-audit-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	require.NoError(t, err)

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)

	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	require.NoError(t, os.WriteFile(certPath,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(keyPath,
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600))
	return certPath, keyPath
}
