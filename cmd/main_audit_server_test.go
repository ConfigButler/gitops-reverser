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
	assert.Equal(t, "gitopsreverser.audit.events.v1", cfg.auditRedisStream)
	assert.Equal(t, int64(0), cfg.auditRedisMaxLen)
	assert.False(t, cfg.auditRedisTLS)
	assert.False(t, cfg.zapOpts.Development)
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
		"--audit-redis-stream=gitopsreverser.audit.custom",
		"--audit-redis-max-len=1000",
		"--audit-redis-tls",
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
	assert.Equal(t, "gitopsreverser.audit.custom", cfg.auditRedisStream)
	assert.Equal(t, int64(1000), cfg.auditRedisMaxLen)
	assert.True(t, cfg.auditRedisTLS)
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test-invalid", flag.ContinueOnError)
			_, err := parseFlagsWithArgs(fs, tt.args)
			require.Error(t, err)
		})
	}
}

func TestBuildAuditServeMux_RoutesAuditPaths(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	mux := buildAuditServeMux(handler)

	req := httptest.NewRequest(http.MethodPost, "/audit-webhook/cluster-a", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	assert.Equal(t, http.StatusAccepted, w.Code)

	req = httptest.NewRequest(http.MethodPost, "/audit-webhook", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	assert.Equal(t, http.StatusAccepted, w.Code)

	req = httptest.NewRequest(http.MethodPost, "/not-audit", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
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
