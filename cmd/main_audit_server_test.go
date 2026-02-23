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
	"flag"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFlagsWithArgs_Defaults(t *testing.T) {
	fs := flag.NewFlagSet("test-defaults", flag.ContinueOnError)

	cfg, err := parseFlagsWithArgs(fs, []string{})
	require.NoError(t, err)

	assert.False(t, cfg.webhookInsecure)
	assert.False(t, cfg.metricsInsecure)
	assert.False(t, cfg.auditInsecure)
	assert.Equal(t, "0.0.0.0", cfg.auditListenAddress)
	assert.Equal(t, 9444, cfg.auditPort)
	assert.Equal(t, int64(10485760), cfg.auditMaxRequestBodyBytes)
	assert.Equal(t, 15*time.Second, cfg.auditReadTimeout)
	assert.Equal(t, 30*time.Second, cfg.auditWriteTimeout)
	assert.Equal(t, 60*time.Second, cfg.auditIdleTimeout)
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
		"--webhook-cert-path=/tmp/webhook-certs",
		"--audit-listen-address=127.0.0.1",
		"--audit-port=9555",
		"--audit-cert-path=/tmp/audit-certs",
		"--audit-cert-name=cert.pem",
		"--audit-cert-key=key.pem",
		"--audit-max-request-body-bytes=2048",
		"--audit-read-timeout=5s",
		"--audit-write-timeout=8s",
		"--audit-idle-timeout=13s",
	}

	cfg, err := parseFlagsWithArgs(fs, args)
	require.NoError(t, err)

	assert.Equal(t, "127.0.0.1", cfg.auditListenAddress)
	assert.Equal(t, 9555, cfg.auditPort)
	assert.Equal(t, "/tmp/audit-certs", cfg.auditCertPath)
	assert.Equal(t, "cert.pem", cfg.auditCertName)
	assert.Equal(t, "key.pem", cfg.auditCertKey)
	assert.Equal(t, int64(2048), cfg.auditMaxRequestBodyBytes)
	assert.Equal(t, 5*time.Second, cfg.auditReadTimeout)
	assert.Equal(t, 8*time.Second, cfg.auditWriteTimeout)
	assert.Equal(t, 13*time.Second, cfg.auditIdleTimeout)
}

func TestParseFlagsWithArgs_FallsBackToWebhookCertPath(t *testing.T) {
	fs := flag.NewFlagSet("test-fallback", flag.ContinueOnError)
	args := []string{
		"--webhook-cert-path=/tmp/webhook-certs",
	}

	cfg, err := parseFlagsWithArgs(fs, args)
	require.NoError(t, err)
	assert.Equal(t, "/tmp/webhook-certs", cfg.auditCertPath)
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
			name: "invalid read timeout",
			args: []string{"--audit-read-timeout=0s"},
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
