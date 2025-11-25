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

package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ConfigButler/gitops-reverser/internal/metrics"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	audit "k8s.io/apiserver/pkg/apis/audit"
)

func TestMain(m *testing.M) {
	// Initialize metrics for tests
	_, err := metrics.InitOTLPExporter(context.Background())
	if err != nil {
		panic("Failed to initialize metrics: " + err.Error())
	}
	m.Run()
}

func TestAuditHandler_ServeHTTP(t *testing.T) {
	tests := []struct {
		name           string
		method         string
		body           string
		expectedStatus int
	}{
		{
			name:           "valid audit event - create configmap",
			method:         http.MethodPost,
			body:           `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","level":"RequestResponse","auditID":"test-id","stage":"ResponseComplete","requestURI":"/api/v1/namespaces/default/configmaps","verb":"create","user":{"username":"test-user"},"objectRef":{"resource":"configmaps","namespace":"default","name":"test-config","apiVersion":"v1"},"responseStatus":{"code":200}}]}`,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "valid audit event - update deployment",
			method:         http.MethodPost,
			body:           `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","level":"RequestResponse","auditID":"test-id","stage":"ResponseComplete","requestURI":"/apis/apps/v1/namespaces/default/deployments/test-deploy","verb":"update","user":{"username":"test-user"},"objectRef":{"resource":"deployments","namespace":"default","name":"test-deploy","apiVersion":"apps/v1"},"responseStatus":{"code":200}}]}`,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "multiple events in batch",
			method:         http.MethodPost,
			body:           `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"verb":"create","user":{"username":"test-user"},"objectRef":{"resource":"configmaps","apiVersion":"v1"}},{"verb":"update","user":{"username":"test-user"},"objectRef":{"resource":"deployments","apiVersion":"apps/v1"}}]}`,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "invalid method",
			method:         http.MethodGet,
			body:           `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"verb":"create","user":{"username":"test-user"},"objectRef":{"resource":"configmaps","apiVersion":"v1"}}]}`,
			expectedStatus: http.StatusMethodNotAllowed,
		},
		{
			name:           "invalid JSON",
			method:         http.MethodPost,
			body:           "invalid json",
			expectedStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create handler
			handler := &AuditHandler{}

			// Create request
			req := httptest.NewRequest(tt.method, "/audit-webhook", bytes.NewReader([]byte(tt.body)))
			w := httptest.NewRecorder()

			// Call handler
			handler.ServeHTTP(w, req)

			// Check response
			assert.Equal(t, tt.expectedStatus, w.Code)
		})
	}
}

func TestAuditHandler_extractGVR(t *testing.T) {
	handler := &AuditHandler{}

	tests := []struct {
		name      string
		eventJSON string
		expected  string
	}{
		{
			name:      "configmap v1",
			eventJSON: `{"objectRef":{"apiVersion":"v1","resource":"configmaps"}}`,
			expected:  "/v1/configmaps",
		},
		{
			name:      "deployment apps/v1",
			eventJSON: `{"objectRef":{"apiVersion":"apps/v1","resource":"deployments"}}`,
			expected:  "apps/v1/deployments",
		},
		{
			name:      "custom resource",
			eventJSON: `{"objectRef":{"apiVersion":"example.com/v1alpha1","resource":"widgets"}}`,
			expected:  "example.com/v1alpha1/widgets",
		},
		{
			name:      "nil objectRef",
			eventJSON: `{}`,
			expected:  "unknown/unknown/unknown",
		},
		{
			name:      "empty apiVersion",
			eventJSON: `{"objectRef":{}}`,
			expected:  "unknown/unknown/unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var event audit.Event
			err := json.Unmarshal([]byte(tt.eventJSON), &event)
			require.NoError(t, err)

			result := handler.extractGVR(&event)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestAuditHandler_InvalidJSON(t *testing.T) {
	handler := &AuditHandler{}

	req := httptest.NewRequest(http.MethodPost, "/audit-webhook", bytes.NewReader([]byte("invalid json")))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid audit event list JSON")
}
