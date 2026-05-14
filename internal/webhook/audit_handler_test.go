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
	"path/filepath"
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

type errorAuditEventQueue struct{ err error }

func (q errorAuditEventQueue) Enqueue(_ context.Context, _ auditv1.Event) error {
	return q.err
}

type recordingAuditEventQueue struct {
	events []auditv1.Event
}

func (q *recordingAuditEventQueue) Enqueue(_ context.Context, event auditv1.Event) error {
	q.events = append(q.events, event)
	return nil
}

type fakeAuditJoiner struct {
	decision AuditJoinDecision
	err      error
	commits  []AuditJoinResult
	releases []string
}

func (j *fakeAuditJoiner) Decide(
	_ context.Context,
	_ AuditSource,
	event *auditv1.Event,
	_ AuditEventQuality,
) (AuditJoinDecision, error) {
	if j.err != nil {
		return AuditJoinDecision{}, j.err
	}
	decision := j.decision
	if decision.Action == AuditJoinActionEmit && decision.Event == nil {
		decision.Event = event
	}
	if decision.AuditID == "" && event != nil {
		decision.AuditID = string(event.AuditID)
	}
	return decision, nil
}

func (j *fakeAuditJoiner) CommitDecision(_ context.Context, _ string, result AuditJoinResult) error {
	j.commits = append(j.commits, result)
	return nil
}

func (j *fakeAuditJoiner) ReleaseDecision(_ context.Context, auditID string) error {
	j.releases = append(j.releases, auditID)
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
			path:           "/audit-webhook",
			body:           `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","level":"RequestResponse","auditID":"test-id","stage":"ResponseComplete","requestURI":"/api/v1/namespaces/default/configmaps","verb":"create","user":{"username":"test-user"},"objectRef":{"resource":"configmaps","namespace":"default","name":"test-config","apiVersion":"v1"},"responseStatus":{"code":200},"responseObject":{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"test-config","namespace":"default"}}}]}`,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "valid audit event - update deployment",
			method:         http.MethodPost,
			path:           "/audit-webhook",
			body:           `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","level":"RequestResponse","auditID":"test-id","stage":"ResponseComplete","requestURI":"/apis/apps/v1/namespaces/default/deployments/test-deploy","verb":"update","user":{"username":"test-user"},"objectRef":{"resource":"deployments","namespace":"default","name":"test-deploy","apiVersion":"apps/v1"},"responseStatus":{"code":200},"responseObject":{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"test-deploy","namespace":"default"}}}]}`,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "multiple events in batch",
			method:         http.MethodPost,
			path:           "/audit-webhook",
			body:           `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","auditID":"batch-event-1","verb":"create","user":{"username":"test-user"},"objectRef":{"resource":"configmaps","apiVersion":"v1"},"responseObject":{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"batch-event-1","namespace":"default"}}},{"kind":"Event","auditID":"batch-event-2","verb":"update","user":{"username":"test-user"},"objectRef":{"resource":"deployments","apiVersion":"apps/v1"},"responseObject":{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"batch-event-2","namespace":"default"}}}]}`,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "additional audit endpoint is accepted",
			method:         http.MethodPost,
			path:           "/audit-webhook-additional",
			body:           `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","auditID":"new-cluster-test","verb":"create","user":{"username":"test-user"},"objectRef":{"resource":"configmaps","apiVersion":"v1"},"responseObject":{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"new-cluster-test","namespace":"default"}}}]}`,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "invalid method",
			method:         http.MethodGet,
			path:           "/audit-webhook",
			body:           `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","auditID":"invalid-method-test","verb":"create","user":{"username":"test-user"},"objectRef":{"resource":"configmaps","apiVersion":"v1"}}]}`,
			expectedStatus: http.StatusMethodNotAllowed,
		},
		{
			name:           "invalid JSON",
			method:         http.MethodPost,
			path:           "/audit-webhook",
			body:           "invalid json",
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "trailing slash is rejected",
			method:         http.MethodPost,
			path:           "/audit-webhook/",
			body:           `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","auditID":"missing-cluster","verb":"create","user":{"username":"test-user"},"objectRef":{"resource":"configmaps","apiVersion":"v1"}}]}`,
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "extra path segments are rejected",
			method:         http.MethodPost,
			path:           "/audit-webhook/extra",
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

	req := httptest.NewRequest(http.MethodPost, "/audit-webhook", bytes.NewReader([]byte("invalid json")))
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

	req := httptest.NewRequest(http.MethodPost, "/audit-webhook", bytes.NewReader(body))
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

		req := httptest.NewRequest(http.MethodPost, "/audit-webhook", bytes.NewReader(eventJSON))
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
			name: "missing request and response bodies is valid before join or legacy filtering",
			event: audit.Event{
				AuditID: "bodyless-id",
				Verb:    "create",
				ObjectRef: &audit.ObjectReference{
					Subresource: "",
				},
			},
			expectedErr:       "",
			expectedProcessed: true,
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
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook", bytes.NewReader([]byte(oversizedBody)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "request body too large")
}

func TestAuditHandler_BodyPresenceControlsDumping(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		expectedStatus int
		expectedDumped []string
	}{
		{
			name:           "events with object bodies are dumped",
			body:           `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","auditID":"bodyful-1","verb":"update","stage":"ResponseComplete","user":{"username":"test-user"},"objectRef":{"resource":"configmaps","namespace":"default","name":"cm-a","apiVersion":"v1"},"responseObject":{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm-a","namespace":"default"}}}]}`,
			expectedStatus: http.StatusOK,
			expectedDumped: []string{"bodyful-1.yaml"},
		},
		{
			name:           "bodyless non-delete events are ignored",
			body:           `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","auditID":"bodyless-update-1","verb":"update","stage":"ResponseComplete","user":{"username":"test-user"},"objectRef":{"resource":"configmaps","namespace":"default","name":"cm-a","apiVersion":"v1"}}]}`,
			expectedStatus: http.StatusOK,
			expectedDumped: nil,
		},
		{
			name:           "bodyless delete events are still dumped",
			body:           `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","auditID":"bodyless-delete-1","verb":"delete","stage":"ResponseComplete","user":{"username":"test-user"},"objectRef":{"resource":"flunders","namespace":"default","name":"flunder-a","apiVersion":"wardle.example.com/v1alpha1"}}]}`,
			expectedStatus: http.StatusOK,
			expectedDumped: []string{"bodyless-delete-1.yaml"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dumpDir := t.TempDir()

			handler, err := NewAuditHandler(AuditHandlerConfig{
				DumpDir: dumpDir,
			})
			require.NoError(t, err)

			req := httptest.NewRequest(http.MethodPost, "/audit-webhook", bytes.NewReader([]byte(tt.body)))
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)

			assert.Equal(t, tt.expectedStatus, w.Code)

			entries, err := os.ReadDir(dumpDir)
			require.NoError(t, err)
			require.Len(t, entries, len(tt.expectedDumped))
			for i, expectedFile := range tt.expectedDumped {
				assert.Equal(t, expectedFile, entries[i].Name())
				assert.FileExists(t, filepath.Join(dumpDir, expectedFile))
			}
		})
	}
}

func TestAuditHandler_EnqueueFailureReturnsInternalServerError(t *testing.T) {
	handler, err := NewAuditHandler(AuditHandlerConfig{
		Queue: errorAuditEventQueue{err: errors.New("queue down")},
	})
	require.NoError(t, err)

	body := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","auditID":"queued-1","verb":"create","stage":"ResponseComplete","user":{"username":"test-user"},"objectRef":{"resource":"secrets","namespace":"default","name":"secret-a","apiVersion":"v1"},"responseObject":{"apiVersion":"v1","kind":"Secret","metadata":{"name":"secret-a","namespace":"default"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "failed to enqueue audit event")
}

func TestAuditHandler_JoinerParkedSkipsQueue(t *testing.T) {
	queue := &recordingAuditEventQueue{}
	joiner := &fakeAuditJoiner{decision: AuditJoinDecision{Action: AuditJoinActionParked}}
	handler, err := NewAuditHandler(AuditHandlerConfig{
		Queue:  queue,
		Joiner: joiner,
	})
	require.NoError(t, err)

	body := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","auditID":"parked-1","verb":"create","stage":"ResponseComplete","user":{"username":"test-user"},"objectRef":{"resource":"flunders","apiGroup":"wardle.example.com","apiVersion":"v1alpha1"},"requestObject":{"kind":"Flunder"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook-additional", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, queue.events)
	assert.Empty(t, joiner.commits)
	assert.Empty(t, joiner.releases)
}

func TestAuditHandler_JoinerReleasesDecisionOnEnqueueFailure(t *testing.T) {
	joiner := &fakeAuditJoiner{decision: AuditJoinDecision{
		Action: AuditJoinActionEmit,
		Result: AuditJoinResultMerged,
		Source: AuditSourceOfficial,
	}}
	handler, err := NewAuditHandler(AuditHandlerConfig{
		Queue:  errorAuditEventQueue{err: errors.New("queue down")},
		Joiner: joiner,
	})
	require.NoError(t, err)

	body := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","auditID":"release-1","verb":"create","stage":"ResponseComplete","user":{"username":"test-user"},"objectRef":{"resource":"flunders","apiGroup":"wardle.example.com","apiVersion":"v1alpha1"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, []string{"release-1"}, joiner.releases)
	assert.Empty(t, joiner.commits)
}

func TestAuditHandler_NonResponseCompleteStageBypassesJoiner(t *testing.T) {
	queue := &recordingAuditEventQueue{}
	joiner := &fakeAuditJoiner{decision: AuditJoinDecision{Action: AuditJoinActionEmit, Result: AuditJoinResultAsIs}}
	handler, err := NewAuditHandler(AuditHandlerConfig{
		Queue:  queue,
		Joiner: joiner,
	})
	require.NoError(t, err)

	// Same auditID, two stages. Only ResponseComplete should reach the joiner.
	body := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[` +
		`{"kind":"Event","auditID":"stage-1","verb":"create","stage":"RequestReceived",` +
		`"user":{"username":"u"},"objectRef":{"resource":"configmaps","apiVersion":"v1","namespace":"d","name":"c"},` +
		`"requestObject":{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c"}}},` +
		`{"kind":"Event","auditID":"stage-1","verb":"create","stage":"ResponseComplete",` +
		`"user":{"username":"u"},"objectRef":{"resource":"configmaps","apiVersion":"v1","namespace":"d","name":"c"},` +
		`"responseObject":{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c"}}}` +
		`]}`
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	require.Len(t, queue.events, 1, "only the ResponseComplete event should be enqueued")
	assert.Equal(t, auditv1.StageResponseComplete, queue.events[0].Stage)
	require.Len(t, joiner.commits, 1, "joiner should only have committed for ResponseComplete")
}

func TestAuditSourceFromPath(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		expected    AuditSource
		expectError bool
	}{
		{
			name:     "official endpoint",
			path:     "/audit-webhook",
			expected: AuditSourceOfficial,
		},
		{
			name:     "additional endpoint",
			path:     "/audit-webhook-additional",
			expected: AuditSourceAdditional,
		},
		{
			name:        "trailing slash",
			path:        "/audit-webhook/",
			expectError: true,
		},
		{
			name:        "extra segment",
			path:        "/audit-webhook/extra",
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
			source, err := auditSourceFromPath(tt.path)
			if tt.expectError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected, source)
		})
	}
}
