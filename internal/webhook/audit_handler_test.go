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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	"sigs.k8s.io/yaml"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	audit "k8s.io/apiserver/pkg/apis/audit"
)

type fakeAuditEventQueue struct {
	mu       sync.Mutex
	events   []auditv1.Event
	err      error
	calls    int
	clusters []string
}

func (q *fakeAuditEventQueue) Enqueue(_ context.Context, clusterID string, event auditv1.Event) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.calls++
	q.clusters = append(q.clusters, clusterID)
	q.events = append(q.events, event)
	if q.err != nil {
		return q.err
	}
	return nil
}

func TestMain(m *testing.M) {
	// Initialize metrics for tests
	_, err := telemetry.InitOTLPExporter(context.Background())
	if err != nil {
		panic("Failed to initialize metrics: " + err.Error())
	}
	m.Run()
}

func TestAuditHandler_ServeHTTP(t *testing.T) {
	tests := []struct {
		name           string
		method         string
		path           string
		body           string
		expectedStatus int
	}{
		{
			name:           "valid audit event - create configmap",
			method:         http.MethodPost,
			path:           "/audit-webhook/cluster-a",
			body:           `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","level":"RequestResponse","auditID":"test-id","stage":"ResponseComplete","requestURI":"/api/v1/namespaces/default/configmaps","verb":"create","user":{"username":"test-user"},"objectRef":{"resource":"configmaps","namespace":"default","name":"test-config","apiVersion":"v1"},"responseStatus":{"code":200},"responseObject":{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"test-config","namespace":"default"}}}]}`,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "valid audit event - update deployment",
			method:         http.MethodPost,
			path:           "/audit-webhook/cluster-a",
			body:           `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","level":"RequestResponse","auditID":"test-id","stage":"ResponseComplete","requestURI":"/apis/apps/v1/namespaces/default/deployments/test-deploy","verb":"update","user":{"username":"test-user"},"objectRef":{"resource":"deployments","namespace":"default","name":"test-deploy","apiVersion":"apps/v1"},"responseStatus":{"code":200},"responseObject":{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"test-deploy","namespace":"default"}}}]}`,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "multiple events in batch",
			method:         http.MethodPost,
			path:           "/audit-webhook/cluster-a",
			body:           `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","auditID":"batch-event-1","verb":"create","user":{"username":"test-user"},"objectRef":{"resource":"configmaps","apiVersion":"v1"},"responseObject":{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"batch-event-1","namespace":"default"}}},{"kind":"Event","auditID":"batch-event-2","verb":"update","user":{"username":"test-user"},"objectRef":{"resource":"deployments","apiVersion":"apps/v1"},"responseObject":{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"batch-event-2","namespace":"default"}}}]}`,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "newly seen cluster ID is accepted",
			method:         http.MethodPost,
			path:           "/audit-webhook/new-cluster-42",
			body:           `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","auditID":"new-cluster-test","verb":"create","user":{"username":"test-user"},"objectRef":{"resource":"configmaps","apiVersion":"v1"},"responseObject":{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"new-cluster-test","namespace":"default"}}}]}`,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "invalid method",
			method:         http.MethodGet,
			path:           "/audit-webhook/cluster-a",
			body:           `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","auditID":"invalid-method-test","verb":"create","user":{"username":"test-user"},"objectRef":{"resource":"configmaps","apiVersion":"v1"}}]}`,
			expectedStatus: http.StatusMethodNotAllowed,
		},
		{
			name:           "invalid JSON",
			method:         http.MethodPost,
			path:           "/audit-webhook/cluster-a",
			body:           "invalid json",
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "missing cluster ID path",
			method:         http.MethodPost,
			path:           "/audit-webhook",
			body:           `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","auditID":"missing-cluster","verb":"create","user":{"username":"test-user"},"objectRef":{"resource":"configmaps","apiVersion":"v1"}}]}`,
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "extra path segments are rejected",
			method:         http.MethodPost,
			path:           "/audit-webhook/cluster-a/extra",
			body:           `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","auditID":"missing-cluster","verb":"create","user":{"username":"test-user"},"objectRef":{"resource":"configmaps","apiVersion":"v1"}}]}`,
			expectedStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, err := NewAuditHandler(AuditHandlerConfig{
				DumpDir: "/tmp/audit-events",
			})
			require.NoError(t, err)

			// Create request
			req := httptest.NewRequest(tt.method, tt.path, bytes.NewReader([]byte(tt.body)))
			w := httptest.NewRecorder()

			// Call handler
			handler.ServeHTTP(w, req)

			// Check response
			assert.Equal(t, tt.expectedStatus, w.Code)
		})
	}
}

func TestAuditHandler_extractGVR(t *testing.T) {
	handler, err := NewAuditHandler(AuditHandlerConfig{
		DumpDir: "/tmp/audit-events",
	})
	require.NoError(t, err)

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
	handler, err := NewAuditHandler(AuditHandlerConfig{
		DumpDir: "/tmp/audit-events",
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/audit-webhook/cluster-a", bytes.NewReader([]byte("invalid json")))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid audit event list")
}

func TestAuditHandler_FileDump(t *testing.T) {
	handler, err := NewAuditHandler(AuditHandlerConfig{
		DumpDir: "/tmp/audit-events",
	})
	require.NoError(t, err)

	// 1. Read the YAML file
	yamlContent, err := os.ReadFile("testdata/audit-events/config-update.yaml")
	require.NoError(t, err)

	// 2. Unmarshal into the v1 Event struct
	// The YAML file includes proper TypeMeta (kind/apiVersion) for Kubernetes consistency
	var event auditv1.Event
	err = yaml.Unmarshal(yamlContent, &event)
	require.NoError(t, err)

	// 3. Create the EventList struct
	eventList := auditv1.EventList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "EventList",
			APIVersion: "audit.k8s.io/v1",
		},
		Items: []auditv1.Event{event},
	}

	// 4. Marshal the whole thing to JSON
	// This guarantees perfect K8s JSON structure
	body, err := json.Marshal(eventList)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/audit-webhook/cluster-a", bytes.NewReader(body))
	w := httptest.NewRecorder()

	// Call handler
	handler.ServeHTTP(w, req)

	// Verify successful processing
	assert.Equal(t, http.StatusOK, w.Code)

	// Check that the file was created and contains valid YAML
	filePath := "/tmp/audit-events/89e50d9e-7963-4836-87ab-a18685930369.yaml"
	fileContent, err := os.ReadFile(filePath)
	require.NoError(t, err, "File should be created successfully")

	// Verify the file contains valid YAML that can be unmarshaled back to audit.Event
	var dumpedEvent audit.Event
	err = yaml.Unmarshal(fileContent, &dumpedEvent)
	require.NoError(t, err, "File content should be valid audit.Event YAML")

	// Verify the auditID matches the actual value from the YAML file
	assert.Equal(t, "89e50d9e-7963-4836-87ab-a18685930369", string(dumpedEvent.AuditID), "AuditID should match")

	// Verify key fields are preserved (from the actual YAML file)
	assert.Equal(t, "patch", dumpedEvent.Verb)
	assert.Equal(t, "system:admin", dumpedEvent.User.Username)
	assert.Equal(t, "configmaps", dumpedEvent.ObjectRef.Resource)

	// Clean up file
	err = os.Remove(filePath)
	require.NoError(t, err, "File cleanup should succeed")

	// Test that events with empty auditID are properly rejected
	t.Run("empty auditID should not create file", func(t *testing.T) {
		os.RemoveAll("/tmp/audit-events")
		handler, err := NewAuditHandler(AuditHandlerConfig{
			DumpDir: "/tmp/audit-events",
		})
		require.NoError(t, err)

		// Create proper event with empty auditID
		event := auditv1.Event{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Event",
				APIVersion: "audit.k8s.io/v1",
			},
			AuditID: "",
			Verb:    "create",
		}
		event.User.Username = "test-user"
		event.ObjectRef = &auditv1.ObjectReference{
			Resource:   "configmaps",
			APIVersion: "v1",
		}

		eventList := auditv1.EventList{
			TypeMeta: metav1.TypeMeta{
				Kind:       "EventList",
				APIVersion: "audit.k8s.io/v1",
			},
			Items: []auditv1.Event{event},
		}

		eventJSON, err := json.Marshal(eventList)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodPost, "/audit-webhook/cluster-a", bytes.NewReader(eventJSON))
		w := httptest.NewRecorder()

		// Call handler
		handler.ServeHTTP(w, req)

		// Verify that empty auditID returns 500 error (from processEvents)
		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Contains(t, w.Body.String(), "invalid audit event: auditID cannot be empty")

		// Verify that no file was created for empty auditID
		emptyAuditIDFile := "/tmp/audit-events/.yaml"
		_, statErr := os.Stat(emptyAuditIDFile)
		assert.True(t, os.IsNotExist(statErr), "File should not be created for empty auditID")
	})
}

func TestAuditHandler_validateEvent(t *testing.T) {
	handler, err := NewAuditHandler(AuditHandlerConfig{
		DumpDir: "/tmp/audit-events",
	})
	require.NoError(t, err)

	tests := []struct {
		name              string
		event             audit.Event
		expectedErr       string
		expectedProcessed bool
	}{
		{
			name: "valid event",
			event: audit.Event{
				AuditID: "valid-id",
				Verb:    "create",
				ResponseObject: &runtime.Unknown{
					Raw: []byte(`{
						"apiVersion":"v1",
						"kind":"ConfigMap",
						"metadata":{"name":"cm-a","namespace":"default"}
					}`),
				},
				ObjectRef: &audit.ObjectReference{
					Subresource: "",
				},
			},
			expectedErr:       "",
			expectedProcessed: true,
		},
		{
			name: "valid status event",
			event: audit.Event{
				AuditID: "some-status",
				Verb:    "update",
				ResponseObject: &runtime.Unknown{
					Raw: []byte(`{
						"apiVersion":"apps/v1",
						"kind":"Deployment",
						"metadata":{"name":"deploy-a","namespace":"default"}
					}`),
				},
				ObjectRef: &audit.ObjectReference{
					Subresource: "status",
				},
			},
			expectedErr:       "",
			expectedProcessed: false,
		},
		{
			name: "empty auditID",
			event: audit.Event{
				AuditID: "",
				Verb:    "create",
				ObjectRef: &audit.ObjectReference{
					Subresource: "",
				},
			},
			expectedErr: "invalid audit event: auditID cannot be empty",
		},
		{
			name: "missing request and response bodies",
			event: audit.Event{
				AuditID: "bodyless-id",
				Verb:    "create",
				ObjectRef: &audit.ObjectReference{
					Subresource: "",
				},
			},
			expectedErr:       "",
			expectedProcessed: false,
		},
		{
			name: "bodyless delete event still processes",
			event: audit.Event{
				AuditID: "bodyless-delete-id",
				Verb:    "delete",
				ObjectRef: &audit.ObjectReference{
					Resource: "flunders",
					Name:     "flunder-a",
				},
			},
			expectedErr:       "",
			expectedProcessed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			processed, err := handler.checkEvent(&tt.event)
			if tt.expectedErr == "" {
				require.NoError(t, err, "Valid event should not return error")
				require.Equal(t, tt.expectedProcessed, processed)
			} else {
				require.Error(t, err, "Invalid event should return error")
				assert.Contains(t, err.Error(), tt.expectedErr, "Error message should match expected")
			}
		})
	}
}

func TestAuditHandler_ReadYAMLToJSON(t *testing.T) {
	// Read the YAML file
	yamlContent, err := os.ReadFile("testdata/audit-events/config-update.yaml")
	require.NoError(t, err, "Should be able to read YAML file")

	// Convert YAML to JSON
	var yamlData interface{}
	err = yaml.Unmarshal(yamlContent, &yamlData)
	require.NoError(t, err, "Should be able to unmarshal YAML")

	// Convert to JSON
	eventJSON, err := json.Marshal(yamlData)
	require.NoError(t, err, "Should be able to marshal to JSON")

	// Verify the JSON is not empty
	assert.NotEmpty(t, eventJSON, "JSON should not be empty")

	// Verify it contains expected fields
	jsonString := string(eventJSON)
	assert.Contains(t, jsonString, "RequestResponse", "JSON should contain RequestResponse level")
	assert.Contains(t, jsonString, "89e50d9e-7963-4836-87ab-a18685930369", "JSON should contain audit ID")
	assert.Contains(t, jsonString, "patch", "JSON should contain verb")
	assert.Contains(t, jsonString, "test-config3", "JSON should contain configmap name")

	// Log the JSON for verification
	t.Logf("Converted JSON: %s", jsonString)
}

func TestAuditHandler_RejectsOversizedBody(t *testing.T) {
	handler, err := NewAuditHandler(AuditHandlerConfig{
		DumpDir:             "/tmp/audit-events",
		MaxRequestBodyBytes: 32,
	})
	require.NoError(t, err)

	oversizedBody := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[]}`
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook/cluster-a", bytes.NewReader([]byte(oversizedBody)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "request body too large")
}

func TestAuditHandler_EnqueuesAcceptedEvents(t *testing.T) {
	queue := &fakeAuditEventQueue{}
	handler, err := NewAuditHandler(AuditHandlerConfig{
		Queue: queue,
	})
	require.NoError(t, err)

	body := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","auditID":"queued-1","verb":"update","stage":"ResponseComplete","user":{"username":"test-user"},"objectRef":{"resource":"configmaps","namespace":"default","name":"cm-a","apiVersion":"v1"},"responseObject":{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm-a","namespace":"default"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook/cluster-a", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 1, queue.calls)
	require.Len(t, queue.events, 1)
	assert.Equal(t, "cluster-a", queue.clusters[0])
	assert.Equal(t, "queued-1", string(queue.events[0].AuditID))
}

func TestAuditHandler_DropsBodylessEventsBeforeQueueing(t *testing.T) {
	queue := &fakeAuditEventQueue{}
	handler, err := NewAuditHandler(AuditHandlerConfig{
		Queue: queue,
	})
	require.NoError(t, err)

	body := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","auditID":"bodyless-1","verb":"update","stage":"ResponseComplete","user":{"username":"test-user"},"objectRef":{"resource":"configmaps","namespace":"default","name":"cm-a","apiVersion":"v1"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook/cluster-a", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 0, queue.calls)
	require.Empty(t, queue.events)
}

func TestAuditHandler_EnqueuesBodylessDeleteEvents(t *testing.T) {
	queue := &fakeAuditEventQueue{}
	handler, err := NewAuditHandler(AuditHandlerConfig{
		Queue: queue,
	})
	require.NoError(t, err)

	body := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","auditID":"bodyless-delete-1","verb":"delete","stage":"ResponseComplete","user":{"username":"test-user"},"objectRef":{"resource":"flunders","namespace":"default","name":"flunder-a","apiVersion":"wardle.example.com/v1alpha1"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook/cluster-a", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 1, queue.calls)
	require.Len(t, queue.events, 1)
	assert.Equal(t, "bodyless-delete-1", string(queue.events[0].AuditID))
}

func TestAuditHandler_EnqueueFailureReturnsInternalServerError(t *testing.T) {
	queue := &fakeAuditEventQueue{err: errors.New("queue down")}
	handler, err := NewAuditHandler(AuditHandlerConfig{
		Queue: queue,
	})
	require.NoError(t, err)

	body := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","auditID":"queued-1","verb":"create","stage":"ResponseComplete","user":{"username":"test-user"},"objectRef":{"resource":"secrets","namespace":"default","name":"secret-a","apiVersion":"v1"},"responseObject":{"apiVersion":"v1","kind":"Secret","metadata":{"name":"secret-a","namespace":"default"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook/cluster-a", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "failed to enqueue audit event")
	require.Equal(t, 1, queue.calls)
}

func TestExtractClusterID(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		expectedID  string
		expectError bool
	}{
		{
			name:       "valid cluster ID",
			path:       "/audit-webhook/cluster-a",
			expectedID: "cluster-a",
		},
		{
			name:       "valid cluster ID with trailing slash",
			path:       "/audit-webhook/cluster-a/",
			expectedID: "cluster-a",
		},
		{
			name:        "missing cluster ID",
			path:        "/audit-webhook",
			expectError: true,
		},
		{
			name:        "extra segment",
			path:        "/audit-webhook/cluster-a/extra",
			expectError: true,
		},
		{
			name:        "invalid prefix",
			path:        "/wrong/cluster-a",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clusterID, err := extractClusterID(tt.path)
			if tt.expectError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expectedID, clusterID)
		})
	}
}

func TestSanitizeClusterIDForMetric(t *testing.T) {
	assert.Equal(t, "cluster-a", sanitizeClusterIDForMetric("cluster-a"))
	assert.Equal(t, "cluster_a", sanitizeClusterIDForMetric("cluster/a"))
	assert.Equal(t, "unknown", sanitizeClusterIDForMetric("   "))

	longID := strings.Repeat("a", MaxClusterIDMetricLabelLength+5)
	assert.Len(t, sanitizeClusterIDForMetric(longID), MaxClusterIDMetricLabelLength)
}
