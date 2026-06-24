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

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFlagsWithArgs_Defaults(t *testing.T) {
	fs := flag.NewFlagSet("test-defaults", flag.ContinueOnError)

	cfg, err := parseFlagsWithArgs(fs, []string{})
	require.NoError(t, err)

	assert.False(t, cfg.metricsInsecure)
	assert.False(t, cfg.admissionWebhookEnabled)
	assert.Equal(t, 9443, cfg.admissionWebhookPort)
	assert.False(t, cfg.auditInsecure)
	assert.Equal(t, "0.0.0.0", cfg.auditListenAddress)
	assert.Equal(t, 9444, cfg.auditPort)
	assert.Equal(t, "/tmp/k8s-audit-server/audit-client-ca", cfg.auditClientCAPath)
	assert.Equal(t, "tls.crt", cfg.auditClientCAName)
	assert.Equal(t, int64(10485760), cfg.auditMaxRequestBodyBytes)
	assert.Equal(t, 15*time.Second, cfg.auditReadTimeout)
	assert.Equal(t, 30*time.Second, cfg.auditWriteTimeout)
	assert.Equal(t, 60*time.Second, cfg.auditIdleTimeout)
	assert.Equal(t, "valkey:6379", cfg.auditRedisAddr)
	assert.Equal(t, int64(0), cfg.auditRedisMaxLen)
	assert.Empty(t, cfg.auditDebugRedisStream)
	assert.Equal(t, int64(0), cfg.auditDebugRedisMaxLen)
	assert.False(t, cfg.auditRedisTLS)
	assert.Equal(t, 5*time.Minute, cfg.auditEventBodyTTL)
	assert.Equal(t, 500*time.Millisecond, cfg.auditEventBodyWait)
	assert.False(t, cfg.zapOpts.Development)
	assert.Equal(t, []string{"secrets"}, cfg.sensitiveResources.Entries())
}

func TestParseFlagsWithArgs_AdmissionWebhookValues(t *testing.T) {
	fs := flag.NewFlagSet("test-admission-webhook", flag.ContinueOnError)
	args := []string{
		"--admission-webhook-enabled",
		"--admission-webhook-port=9445",
		"--admission-webhook-cert-path=/tmp/admission-certs",
		"--admission-webhook-cert-name=cert.pem",
		"--admission-webhook-cert-key=key.pem",
	}

	cfg, err := parseFlagsWithArgs(fs, args)
	require.NoError(t, err)

	assert.True(t, cfg.admissionWebhookEnabled)
	assert.Equal(t, 9445, cfg.admissionWebhookPort)
	assert.Equal(t, "/tmp/admission-certs", cfg.admissionWebhookCertPath)
	assert.Equal(t, "cert.pem", cfg.admissionWebhookCertName)
	assert.Equal(t, "key.pem", cfg.admissionWebhookCertKey)
}

func TestParseFlagsWithArgs_AuditUnsecure(t *testing.T) {
	fs := flag.NewFlagSet("test-audit-insecure", flag.ContinueOnError)
	args := []string{
		"--audit-insecure",
	}

	cfg, err := parseFlagsWithArgs(fs, args)
	require.NoError(t, err)
	assert.True(t, cfg.auditInsecure)
}

func TestParseFlagsWithArgs_CustomAuditValues(t *testing.T) {
	fs := flag.NewFlagSet("test-custom", flag.ContinueOnError)
	args := []string{
		"--audit-listen-address=127.0.0.1",
		"--audit-port=9555",
		"--audit-cert-path=/tmp/audit-certs",
		"--audit-cert-name=cert.pem",
		"--audit-cert-key=key.pem",
		"--audit-client-ca-path=/tmp/audit-ca",
		"--audit-client-ca-name=ca.pem",
		"--audit-max-request-body-bytes=2048",
		"--audit-read-timeout=5s",
		"--audit-write-timeout=8s",
		"--audit-idle-timeout=13s",
		"--audit-redis-addr=127.0.0.1:6379",
		"--audit-redis-username=user",
		"--audit-redis-password=pass",
		"--audit-redis-db=2",
		"--audit-redis-max-len=1000",
		"--audit-debug-redis-stream=gitopsreverser.audit.debug.custom",
		"--audit-debug-redis-max-len=2000",
		"--audit-redis-tls",
		"--audit-event-body-ttl=2m",
		"--audit-event-body-wait=250ms",
	}

	cfg, err := parseFlagsWithArgs(fs, args)
	require.NoError(t, err)

	assert.Equal(t, "127.0.0.1", cfg.auditListenAddress)
	assert.Equal(t, 9555, cfg.auditPort)
	assert.Equal(t, "/tmp/audit-certs", cfg.auditCertPath)
	assert.Equal(t, "cert.pem", cfg.auditCertName)
	assert.Equal(t, "key.pem", cfg.auditCertKey)
	assert.Equal(t, "/tmp/audit-ca", cfg.auditClientCAPath)
	assert.Equal(t, "ca.pem", cfg.auditClientCAName)
	assert.Equal(t, int64(2048), cfg.auditMaxRequestBodyBytes)
	assert.Equal(t, 5*time.Second, cfg.auditReadTimeout)
	assert.Equal(t, 8*time.Second, cfg.auditWriteTimeout)
	assert.Equal(t, 13*time.Second, cfg.auditIdleTimeout)
	assert.Equal(t, "127.0.0.1:6379", cfg.auditRedisAddr)
	assert.Equal(t, "user", cfg.auditRedisUsername)
	assert.Equal(t, "pass", cfg.auditRedisPassword)
	assert.Equal(t, 2, cfg.auditRedisDB)
	assert.Equal(t, int64(1000), cfg.auditRedisMaxLen)
	assert.Equal(t, "gitopsreverser.audit.debug.custom", cfg.auditDebugRedisStream)
	assert.Equal(t, int64(2000), cfg.auditDebugRedisMaxLen)
	assert.True(t, cfg.auditRedisTLS)
	assert.Equal(t, 2*time.Minute, cfg.auditEventBodyTTL)
	assert.Equal(t, 250*time.Millisecond, cfg.auditEventBodyWait)
}

func TestParseFlagsWithArgs_AdditionalSensitiveResources(t *testing.T) {
	fs := flag.NewFlagSet("test-sensitive-resources", flag.ContinueOnError)
	args := []string{
		"--additional-sensitive-resources=core.cozystack.io/tenantsecrets,credentials",
	}

	cfg, err := parseFlagsWithArgs(fs, args)
	require.NoError(t, err)
	assert.Equal(
		t,
		[]string{"core.cozystack.io/tenantsecrets", "credentials", "secrets"},
		cfg.sensitiveResources.Entries(),
	)
}

func TestParseFlagsWithArgs_InvalidAuditSettings(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "invalid port",
			args: []string{"--audit-port=0"},
		},
		{
			name: "invalid body size",
			args: []string{"--audit-max-request-body-bytes=0"},
		},
		{
			name: "missing audit client ca path",
			args: []string{"--audit-client-ca-path=", "--audit-insecure=false"},
		},
		{
			name: "invalid read timeout",
			args: []string{"--audit-read-timeout=0s"},
		},
		{
			name: "empty redis address",
			args: []string{"--audit-redis-addr="},
		},
		{
			name: "invalid redis db",
			args: []string{"--audit-redis-db=-1"},
		},
		{
			name: "invalid redis max len",
			args: []string{"--audit-redis-max-len=-1"},
		},
		{
			name: "invalid audit debug redis max len",
			args: []string{"--audit-debug-redis-max-len=-1"},
		},
		{
			name: "invalid audit event body ttl",
			args: []string{"--audit-event-body-ttl=0s"},
		},
		{
			name: "invalid audit event body wait",
			args: []string{"--audit-event-body-wait=-1s"},
		},
		{
			name: "invalid sensitive resource",
			args: []string{"--additional-sensitive-resources=example.io/v1/credentials"},
		},
		{
			name: "invalid admission webhook port",
			args: []string{"--admission-webhook-enabled", "--admission-webhook-port=0"},
		},
		{
			name: "missing admission webhook cert path",
			args: []string{"--admission-webhook-enabled", "--admission-webhook-cert-path="},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test-invalid", flag.ContinueOnError)
			_, err := parseFlagsWithArgs(fs, tt.args)
			require.Error(t, err)
		})
	}
}

// TestBuildAuditServeMux_DelegatesAuditPathsToHandler asserts mux-level wiring only.
// The mux registers trailing-slash patterns so that any path under /audit-webhook/ or
// /audit-webhook-additional/ is delegated to the AuditHandler — which is then responsible
// for rejecting unknown subpaths (e.g. cluster-ID segments) with HTTP 400. See
// TestAuditHandler_ServeHTTP and TestAuditSourceFromPath in internal/webhook for the
// actual rejection assertions.
func TestBuildAuditServeMux_DelegatesAuditPathsToHandler(t *testing.T) {
	const delegated = http.StatusAccepted
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(delegated)
	})

	mux := buildAuditServeMux(handler)

	cases := []struct {
		name string
		path string
		want int
	}{
		{"official endpoint is delegated", "/audit-webhook", delegated},
		{"additional endpoint is delegated", "/audit-webhook-additional", delegated},
		{
			"subpath under /audit-webhook/ is delegated (handler then rejects)",
			"/audit-webhook/cluster-a",
			delegated,
		},
		{"unrelated path is not registered", "/not-audit", http.StatusNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			assert.Equal(t, tc.want, w.Code)
		})
	}
}

func TestBuildAuditServerAddress(t *testing.T) {
	assert.Equal(t, "0.0.0.0:9444", buildAuditServerAddress("0.0.0.0", 9444))
	assert.Equal(t, ":9444", buildAuditServerAddress("", 9444))
}

func TestBuildAuditServerTLSConfig_RequiresClientCertificates(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	caPath := filepath.Join(tempDir, "tls.crt")
	require.NoError(t, os.WriteFile(caPath, []byte(mustMakeTestRootCA(t)), 0o600))

	cfg := appConfig{
		auditClientCAPath: tempDir,
		auditClientCAName: "tls.crt",
	}

	serverTLS, err := buildAuditServerTLSConfig(cfg, []func(*tls.Config){
		func(c *tls.Config) {
			c.MinVersion = tls.VersionTLS13
		},
	})
	require.NoError(t, err)
	require.NotNil(t, serverTLS.ClientCAs)

	assert.Equal(t, tls.RequireAndVerifyClientCert, serverTLS.ClientAuth)
	assert.Equal(t, uint16(tls.VersionTLS13), serverTLS.MinVersion)
	expectedPool, err := loadCertPoolFromPEMFile(caPath)
	require.NoError(t, err)
	assert.True(t, expectedPool.Equal(serverTLS.ClientCAs))
}

func TestLoadCertPoolFromPEMFile_InvalidPEM(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	caPath := filepath.Join(tempDir, "invalid.pem")
	require.NoError(t, os.WriteFile(caPath, []byte("not-a-cert"), 0o600))

	_, err := loadCertPoolFromPEMFile(caPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no certificates found")
}

func mustMakeTestRootCA(t *testing.T) string {
	t.Helper()

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "gitops-reverser-test-root",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, privateKey.Public(), privateKey)
	require.NoError(t, err)

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes}))
}
