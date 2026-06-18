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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	"sigs.k8s.io/yaml"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	audit "k8s.io/apiserver/pkg/apis/audit"
)

type recordingAuditEventQueue struct {
	mu     sync.Mutex
	events []auditv1.Event
}

func (q *recordingAuditEventQueue) Enqueue(_ context.Context, event auditv1.Event) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.events = append(q.events, event)
	return nil
}

func (q *recordingAuditEventQueue) auditIDs() []string {
	q.mu.Lock()
	defer q.mu.Unlock()

	ids := make([]string, 0, len(q.events))
	for _, event := range q.events {
		ids = append(ids, string(event.AuditID))
	}
	return ids
}

type errorAuditDebugQueue struct{ err error }

func (q errorAuditDebugQueue) Enqueue(_ context.Context, _ string, _ auditv1.Event) error {
	return q.err
}

type recordingAuditDebugQueue struct {
	sources []string
	events  []auditv1.Event
}

func (q *recordingAuditDebugQueue) Enqueue(_ context.Context, source string, event auditv1.Event) error {
	q.sources = append(q.sources, source)
	q.events = append(q.events, event)
	return nil
}

func (q *recordingAuditDebugQueue) auditIDs() []string {
	ids := make([]string, 0, len(q.events))
	for _, event := range q.events {
		ids = append(ids, string(event.AuditID))
	}
	return ids
}

type recordingByTypeQueue struct {
	mu     sync.Mutex
	err    error
	events []auditv1.Event
}

func (q *recordingByTypeQueue) Enqueue(_ context.Context, event auditv1.Event) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.events = append(q.events, event)
	return q.err
}

func (q *recordingByTypeQueue) auditIDs() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	ids := make([]string, 0, len(q.events))
	for _, event := range q.events {
		ids = append(ids, string(event.AuditID))
	}
	return ids
}

type fakeAuditJoiner struct {
	decision AuditJoinDecision
	err      error
	calls    int
	sources  []AuditSource
	quality  []AuditEventQuality
}

func (j *fakeAuditJoiner) Decide(
	_ context.Context,
	source AuditSource,
	event *auditv1.Event,
	quality AuditEventQuality,
) (AuditJoinDecision, error) {
	j.calls++
	j.sources = append(j.sources, source)
	j.quality = append(j.quality, quality)
	if j.err != nil {
		return AuditJoinDecision{}, j.err
	}
	decision := j.decision
	if decision.Action == AuditJoinActionEmit && decision.Event == nil {
		decision.Event = event
	}
	return decision, nil
}

func eventListFixtureBody(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var list auditv1.EventList
	require.NoError(t, yaml.Unmarshal(raw, &list))
	body, err := json.Marshal(&list)
	require.NoError(t, err)
	return string(body)
}

type orderingAuditJoiner struct {
	firstStarted        chan struct{}
	releaseFirst        chan struct{}
	additionalProcessed chan struct{}
	closeFirstStarted   sync.Once
	closeAdditional     sync.Once
}

func newOrderingAuditJoiner() *orderingAuditJoiner {
	return &orderingAuditJoiner{
		firstStarted:        make(chan struct{}),
		releaseFirst:        make(chan struct{}),
		additionalProcessed: make(chan struct{}),
	}
}

func (j *orderingAuditJoiner) Decide(
	ctx context.Context,
	source AuditSource,
	event *auditv1.Event,
	_ AuditEventQuality,
) (AuditJoinDecision, error) {
	if source == AuditSourceAdditional {
		j.closeAdditional.Do(func() {
			close(j.additionalProcessed)
		})
		return AuditJoinDecision{Action: AuditJoinActionParked}, nil
	}

	if string(event.AuditID) == "first" {
		j.closeFirstStarted.Do(func() {
			close(j.firstStarted)
		})
		select {
		case <-ctx.Done():
			return AuditJoinDecision{}, ctx.Err()
		case <-j.releaseFirst:
		}
	}

	return AuditJoinDecision{
		Action: AuditJoinActionEmit,
		Event:  event,
		Result: AuditJoinResultAsIs,
		Source: AuditSourceOfficial,
	}, nil
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
			handler, err := NewAuditHandler(AuditHandlerConfig{})
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

func TestAuditHandler_DebugQueueCapturesAllDecodedEventsBeforeProcessing(t *testing.T) {
	debugQueue := &recordingAuditDebugQueue{}
	handler, err := NewAuditHandler(AuditHandlerConfig{DebugQueue: debugQueue})
	require.NoError(t, err)

	body := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[` +
		`{"kind":"Event","auditID":"request-received","verb":"get","stage":"RequestReceived",` +
		`"objectRef":{"resource":"pods","apiVersion":"v1"}},` +
		`{"kind":"Event","auditID":"bodyless-update","verb":"update","stage":"ResponseComplete",` +
		`"objectRef":{"resource":"configmaps","apiVersion":"v1","namespace":"default","name":"cm-a"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook-additional", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []string{"request-received", "bodyless-update"}, debugQueue.auditIDs())
	assert.Equal(t, []string{"additional", "additional"}, debugQueue.sources)
}

func TestAuditHandler_DebugQueueFailureStopsEventProcessing(t *testing.T) {
	queue := &recordingAuditEventQueue{}
	handler, err := NewAuditHandler(AuditHandlerConfig{
		ByTypeQueue: queue,
		DebugQueue:  errorAuditDebugQueue{err: errors.New("debug stream down")},
	})
	require.NoError(t, err)

	body := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[` +
		`{"kind":"Event","auditID":"canonical-event","verb":"create","stage":"ResponseComplete",` +
		`"objectRef":{"resource":"configmaps","apiVersion":"v1","namespace":"default","name":"cm-a"},` +
		`"responseObject":{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm-a"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "debug stream down")
	assert.Empty(t, queue.events)
}

func TestAuditHandler_extractGVR(t *testing.T) {
	handler, err := NewAuditHandler(AuditHandlerConfig{})
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
	handler, err := NewAuditHandler(AuditHandlerConfig{})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/audit-webhook", bytes.NewReader([]byte("invalid json")))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid audit event list")
}

func TestAuditHandler_validateEvent(t *testing.T) {
	handler, err := NewAuditHandler(AuditHandlerConfig{})
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
			name: "status subresource event",
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
			name: "exec subresource event",
			event: audit.Event{
				AuditID: "some-exec",
				Verb:    "create",
				ObjectRef: &audit.ObjectReference{
					Resource:    "pods",
					Subresource: "exec",
				},
			},
			expectedErr:       "",
			expectedProcessed: false,
		},
		{
			name: "scale subresource event forwards",
			event: audit.Event{
				AuditID: "some-scale",
				Verb:    "patch",
				ObjectRef: &audit.ObjectReference{
					Resource:    "deployments",
					Subresource: "scale",
				},
			},
			expectedErr:       "",
			expectedProcessed: true,
		},
		{
			name: "read verb on a subresource is dropped",
			event: audit.Event{
				AuditID: "scale-read",
				Verb:    "get",
				ObjectRef: &audit.ObjectReference{
					Resource:    "deployments",
					Subresource: "scale",
				},
			},
			expectedErr:       "",
			expectedProcessed: false,
		},
		{
			name: "services proxy subresource is dropped as non-scale",
			event: audit.Event{
				AuditID: "svc-proxy",
				Verb:    "create",
				ObjectRef: &audit.ObjectReference{
					Resource:    "services",
					Subresource: "proxy",
				},
			},
			expectedErr:       "",
			expectedProcessed: false,
		},
		{
			name: "an arbitrary mutating subresource is dropped as non-scale",
			event: audit.Event{
				AuditID: "widget-throttle",
				Verb:    "update",
				ObjectRef: &audit.ObjectReference{
					Resource:    "widgets",
					Subresource: "throttle",
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

// TestAuditHandler_ForwardsRealScaleSubresourceRecording drives the real captured
// kube-apiserver recording of a `kubectl scale deployment` through the full webhook
// path — decode, the subresource forwarding gate, ingress classification, and
// enqueue — and asserts the deployments/scale event reaches the canonical stream
// (verb/resource/subresource intact) instead of being dropped as it was before the
// gate change. This is the e2e-shaped proof at unit speed that the recording the
// design is built around is actually forwarded.
func TestAuditHandler_ForwardsRealScaleSubresourceRecording(t *testing.T) {
	recording, err := os.ReadFile("testdata/audit-events/deployment-scale-subresource.json")
	require.NoError(t, err, "the captured scale recording must be readable")

	queue := &recordingAuditEventQueue{}
	handler, err := NewAuditHandler(AuditHandlerConfig{ByTypeQueue: queue})
	require.NoError(t, err)

	body := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[` + string(recording) + `]}`
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Len(t, queue.events, 1, "the real deployments/scale recording must be mirrored to its per-type stream")
	enqueued := queue.events[0]
	require.NotNil(t, enqueued.ObjectRef)
	assert.Equal(t, "deployments", enqueued.ObjectRef.Resource)
	assert.Equal(t, "scale", enqueued.ObjectRef.Subresource)
	assert.Equal(t, "patch", enqueued.Verb)
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
	handler, err := NewAuditHandler(AuditHandlerConfig{MaxRequestBodyBytes: 32})
	require.NoError(t, err)

	oversizedBody := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[]}`
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook", bytes.NewReader([]byte(oversizedBody)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "request body too large")
}

func TestAuditHandler_JoinerParkedSkipsQueue(t *testing.T) {
	queue := &recordingAuditEventQueue{}
	joiner := &fakeAuditJoiner{decision: AuditJoinDecision{Action: AuditJoinActionParked}}
	handler, err := NewAuditHandler(AuditHandlerConfig{
		ByTypeQueue: queue,
		Joiner:      joiner,
	})
	require.NoError(t, err)

	body := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[{"kind":"Event","auditID":"parked-1","verb":"create","stage":"ResponseComplete","user":{"username":"test-user"},"objectRef":{"resource":"flunders","apiGroup":"wardle.example.com","apiVersion":"v1alpha1"},"requestObject":{"kind":"Flunder"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook-additional", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, queue.events)
}

// TestAuditHandler_ShallowOfficialReachesJoinerWithoutRuleKnowledge locks in the
// fix for the aggregated-API audit regression: a shallow official event (an
// aggregated-API resource the kube-apiserver proxies opaquely, so no body) must
// always reach the joiner so it can wait for the proxy-supplied body — the
// ingestion path must not consult WatchRules. flunders are used because that is
// the exact resource the e2e test exercised, but the behaviour is resource
// agnostic.
func TestAuditHandler_ShallowOfficialReachesJoinerWithoutRuleKnowledge(t *testing.T) {
	queue := &recordingAuditEventQueue{}
	joiner := &fakeAuditJoiner{decision: AuditJoinDecision{Action: AuditJoinActionEmit}}
	handler, err := NewAuditHandler(AuditHandlerConfig{ByTypeQueue: queue, Joiner: joiner})
	require.NoError(t, err)

	body := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[` +
		`{"kind":"Event","auditID":"shallow-flunder-1","verb":"create","stage":"ResponseComplete",` +
		`"user":{"username":"test-user"},"objectRef":{"resource":"flunders",` +
		`"apiGroup":"wardle.example.com","apiVersion":"v1alpha1","namespace":"team-a","name":"flunder-a"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 1, joiner.calls, "every shallow official event must reach the joiner's body lookup")
	assert.Equal(t, []AuditSource{AuditSourceOfficial}, joiner.sources)
	assert.Equal(t, []AuditEventQuality{AuditEventQualityIdentityShallow}, joiner.quality)
	assert.Equal(t, []string{"shallow-flunder-1"}, queue.auditIDs())
}

// TestAuditHandler_CompleteOfficialReachesJoinerWithoutRuleKnowledge confirms a
// complete official event (request/response body already inline) flows straight
// through ingestion regardless of WatchRules — the joiner emits it as-is.
func TestAuditHandler_CompleteOfficialReachesJoinerWithoutRuleKnowledge(t *testing.T) {
	queue := &recordingAuditEventQueue{}
	joiner := &fakeAuditJoiner{decision: AuditJoinDecision{Action: AuditJoinActionEmit}}
	handler, err := NewAuditHandler(AuditHandlerConfig{ByTypeQueue: queue, Joiner: joiner})
	require.NoError(t, err)

	body := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[` +
		`{"kind":"Event","auditID":"complete-flunder-1","verb":"create","stage":"ResponseComplete",` +
		`"user":{"username":"test-user"},"objectRef":{"resource":"flunders",` +
		`"apiGroup":"wardle.example.com","apiVersion":"v1alpha1","namespace":"team-a","name":"flunder-a"},` +
		`"requestObject":{"apiVersion":"wardle.example.com/v1alpha1","kind":"Flunder",` +
		`"metadata":{"name":"flunder-a","namespace":"team-a"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 1, joiner.calls)
	assert.Equal(t, []AuditEventQuality{AuditEventQualityComplete}, joiner.quality)
	assert.Equal(t, []string{"complete-flunder-1"}, queue.auditIDs())
}

// TestAuditHandler_AdditionalCompleteEventReachesJoiner confirms an
// additional-source event is parked whenever it is intrinsically useful — a
// mutating verb carrying a body — with no WatchRule lookup.
func TestAuditHandler_AdditionalCompleteEventReachesJoiner(t *testing.T) {
	queue := &recordingAuditEventQueue{}
	joiner := &fakeAuditJoiner{decision: AuditJoinDecision{Action: AuditJoinActionParked}}
	handler, err := NewAuditHandler(AuditHandlerConfig{ByTypeQueue: queue, Joiner: joiner})
	require.NoError(t, err)

	body := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[` +
		`{"kind":"Event","auditID":"additional-flunder-1","verb":"create","stage":"ResponseComplete",` +
		`"user":{"username":"test-user"},"objectRef":{"resource":"flunders",` +
		`"apiGroup":"wardle.example.com","apiVersion":"v1alpha1","namespace":"team-a","name":"flunder-a"},` +
		`"requestObject":{"apiVersion":"wardle.example.com/v1alpha1","kind":"Flunder",` +
		`"metadata":{"name":"flunder-a","namespace":"team-a"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook-additional", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 1, joiner.calls, "a mutating additional event with a body must be parked")
	assert.Equal(t, []AuditSource{AuditSourceAdditional}, joiner.sources)
	assert.Equal(t, []AuditEventQuality{AuditEventQualityComplete}, joiner.quality)
	assert.Empty(t, queue.events, "parked additional bodies do not enter the canonical stream")
}

// TestAuditHandler_AdditionalReadOnlyVerbBypassesJoiner confirms a read-only
// additional event (action is a get) never reaches the joiner — it is no
// mutation, so there is nothing to park.
func TestAuditHandler_AdditionalReadOnlyVerbBypassesJoiner(t *testing.T) {
	queue := &recordingAuditEventQueue{}
	joiner := &fakeAuditJoiner{decision: AuditJoinDecision{Action: AuditJoinActionParked}}
	handler, err := NewAuditHandler(AuditHandlerConfig{ByTypeQueue: queue, Joiner: joiner})
	require.NoError(t, err)

	body := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[` +
		`{"kind":"Event","auditID":"additional-get-1","verb":"get","stage":"ResponseComplete",` +
		`"user":{"username":"test-user"},"objectRef":{"resource":"flunders",` +
		`"apiGroup":"wardle.example.com","apiVersion":"v1alpha1","namespace":"team-a","name":"flunder-a"},` +
		`"responseObject":{"apiVersion":"wardle.example.com/v1alpha1","kind":"Flunder",` +
		`"metadata":{"name":"flunder-a","namespace":"team-a"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook-additional", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Zero(t, joiner.calls, "read-only additional events must not be parked")
	assert.Empty(t, queue.events)
}

// TestAuditHandler_AdditionalShallowEventBypassesJoiner confirms a bodyless
// additional event never reaches the joiner — a shallow additional event has no
// request/response body to contribute.
func TestAuditHandler_AdditionalShallowEventBypassesJoiner(t *testing.T) {
	queue := &recordingAuditEventQueue{}
	joiner := &fakeAuditJoiner{decision: AuditJoinDecision{Action: AuditJoinActionParked}}
	handler, err := NewAuditHandler(AuditHandlerConfig{ByTypeQueue: queue, Joiner: joiner})
	require.NoError(t, err)

	body := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[` +
		`{"kind":"Event","auditID":"additional-shallow-1","verb":"create","stage":"ResponseComplete",` +
		`"user":{"username":"test-user"},"objectRef":{"resource":"flunders",` +
		`"apiGroup":"wardle.example.com","apiVersion":"v1alpha1","namespace":"team-a","name":"flunder-a"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook-additional", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Zero(t, joiner.calls, "shallow additional events must not be parked")
	assert.Empty(t, queue.events)
}

func TestAuditHandler_OfficialCanonicalEventsAreOrderedWhileAdditionalCanPark(t *testing.T) {
	queue := &recordingAuditEventQueue{}
	joiner := newOrderingAuditJoiner()
	handler, err := NewAuditHandler(AuditHandlerConfig{
		ByTypeQueue: queue,
		Joiner:      joiner,
	})
	require.NoError(t, err)

	serve := func(path, auditID string) *httptest.ResponseRecorder {
		t.Helper()
		body := fmt.Sprintf(
			`{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[`+
				`{"kind":"Event","auditID":%q,"verb":"create","stage":"ResponseComplete",`+
				`"user":{"username":"test-user"},"objectRef":{"resource":"flunders",`+
				`"apiGroup":"wardle.example.com","apiVersion":"v1alpha1"}}]}`,
			auditID,
		)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- serve("/audit-webhook", "first")
	}()

	select {
	case <-joiner.firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first official event did not enter the joiner")
	}

	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		secondDone <- serve("/audit-webhook", "second")
	}()

	additionalDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		additionalDone <- serve("/audit-webhook-additional", "first")
	}()

	select {
	case w := <-additionalDone:
		require.Equal(t, http.StatusOK, w.Code)
	case <-time.After(time.Second):
		t.Fatal("additional audit body should be able to park while the official event is waiting")
	}

	select {
	case <-secondDone:
		t.Fatal("second official event overtook the blocked first official event")
	case <-time.After(50 * time.Millisecond):
	}
	assert.Empty(t, queue.auditIDs())

	close(joiner.releaseFirst)

	select {
	case w := <-firstDone:
		require.Equal(t, http.StatusOK, w.Code)
	case <-time.After(time.Second):
		t.Fatal("first official event did not finish")
	}
	select {
	case w := <-secondDone:
		require.Equal(t, http.StatusOK, w.Code)
	case <-time.After(time.Second):
		t.Fatal("second official event did not finish")
	}

	assert.Equal(t, []string{"first", "second"}, queue.auditIDs())
}

func TestAuditHandler_NonResponseCompleteStageBypassesJoiner(t *testing.T) {
	queue := &recordingAuditEventQueue{}
	joiner := &fakeAuditJoiner{decision: AuditJoinDecision{Action: AuditJoinActionEmit, Result: AuditJoinResultAsIs}}
	handler, err := NewAuditHandler(AuditHandlerConfig{
		ByTypeQueue: queue,
		Joiner:      joiner,
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
	require.Len(t, queue.events, 1, "only the ResponseComplete event should be mirrored")
	assert.Equal(t, auditv1.StageResponseComplete, queue.events[0].Stage)
	require.Equal(t, 1, joiner.calls, "only the ResponseComplete event should reach the joiner")
}

// TestAuditHandler_ConflictResponseNeverReachesGit reproduces a real
// production occurrence: a HelmRelease update that the API server rejected with
// a 409 Conflict ("the object has been modified") still produces a complete
// ResponseComplete audit event — but its responseObject is a metav1.Status
// error body, not the HelmRelease. Before the responseStatus gate, that Status
// was extracted and written to Git as the resource's desired state, so the
// committed file briefly held:
//
//	apiVersion: v1
//	kind: Status
//	reason: Conflict
//
// instead of the HelmRelease. A failed request changed nothing in etcd, so the
// event must be dropped at ingress and never reach the joiner or the queue.
func TestAuditHandler_ConflictResponseNeverReachesGit(t *testing.T) {
	queue := &recordingAuditEventQueue{}
	joiner := &fakeAuditJoiner{decision: AuditJoinDecision{Action: AuditJoinActionEmit}}
	handler, err := NewAuditHandler(AuditHandlerConfig{ByTypeQueue: queue, Joiner: joiner})
	require.NoError(t, err)

	// An update to helmreleases.helm.toolkit.fluxcd.io that lost an
	// optimistic-concurrency race: responseStatus.code is 409 and the
	// responseObject is the Status error body the API server returned.
	body := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[` +
		`{"kind":"Event","auditID":"helmrelease-conflict-1","verb":"update","stage":"ResponseComplete",` +
		`"user":{"username":"flux-controller"},"objectRef":{"resource":"helmreleases",` +
		`"apiGroup":"helm.toolkit.fluxcd.io","apiVersion":"v2","namespace":"cozy-system","name":"info-rd"},` +
		`"responseStatus":{"metadata":{},"status":"Failure","reason":"Conflict","code":409,` +
		`"message":"Operation cannot be fulfilled on helmreleases.helm.toolkit.fluxcd.io \"info-rd\": ` +
		`the object has been modified; please apply your changes to the latest version and try again"},` +
		`"responseObject":{"apiVersion":"v1","kind":"Status","metadata":{"name":"info-rd",` +
		`"namespace":"cozy-system"},"status":"Failure","reason":"Conflict",` +
		`"message":"Operation cannot be fulfilled on helmreleases.helm.toolkit.fluxcd.io \"info-rd\": ` +
		`the object has been modified; please apply your changes to the latest version and try again"}}` +
		`]}`
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Zero(t, joiner.calls, "a 409 Conflict event must be dropped before the join pipeline")
	assert.Empty(t, queue.events, "a failed request must never enter the canonical stream")
}

// TestAuditHandler_SuccessfulUpdateStillReachesGit is the companion to the
// conflict test: an identical update that the API server accepted (200, with
// the HelmRelease as responseObject) must still flow through to the queue. The
// responseStatus gate keys on the failure code alone — it must not reject
// healthy mutations.
func TestAuditHandler_SuccessfulUpdateStillReachesGit(t *testing.T) {
	queue := &recordingAuditEventQueue{}
	joiner := &fakeAuditJoiner{decision: AuditJoinDecision{Action: AuditJoinActionEmit}}
	handler, err := NewAuditHandler(AuditHandlerConfig{ByTypeQueue: queue, Joiner: joiner})
	require.NoError(t, err)

	body := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[` +
		`{"kind":"Event","auditID":"helmrelease-ok-1","verb":"update","stage":"ResponseComplete",` +
		`"user":{"username":"flux-controller"},"objectRef":{"resource":"helmreleases",` +
		`"apiGroup":"helm.toolkit.fluxcd.io","apiVersion":"v2","namespace":"cozy-system","name":"info-rd"},` +
		`"responseStatus":{"metadata":{},"code":200},` +
		`"responseObject":{"apiVersion":"helm.toolkit.fluxcd.io/v2","kind":"HelmRelease",` +
		`"metadata":{"name":"info-rd","namespace":"cozy-system"}}}` +
		`]}`
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 1, joiner.calls, "a successful update must still reach the joiner")
	assert.Equal(t, []string{"helmrelease-ok-1"}, queue.auditIDs())
}

func TestAuditHandler_PodExecCreateDoesNotEnterJoinPipeline(t *testing.T) {
	queue := &recordingAuditEventQueue{}
	joiner := &fakeAuditJoiner{decision: AuditJoinDecision{Action: AuditJoinActionEmit}}
	handler, err := NewAuditHandler(AuditHandlerConfig{ByTypeQueue: queue, Joiner: joiner})
	require.NoError(t, err)

	body := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[
{
  "level": "RequestResponse",
  "auditID": "3df193b2-b83e-4375-a0c2-67ee0c045404",
  "stage": "ResponseComplete",
  "requestURI": "/api/v1/namespaces/cozy-kubeovn/pods/ovn-central-7955dc78d8-lvwh4/exec?command=ovsdb-client&command=query&command=unix%3A%2Fvar%2Frun%2Fovn%2Fovnsb_db.sock&command=%5B%22_Server%22%2C%7B%22op%22%3A%22select%22%2C%22table%22%3A%22Database%22%2C%22where%22%3A%5B%5B%22name%22%2C%22%3D%3D%22%2C%22OVN_Southbound%22%5D%5D%2C%22columns%22%3A%5B%22leader%22%2C%22connected%22%2C%22cid%22%2C%22sid%22%2C%22index%22%5D%7D%5D&container=ovn-central&stderr=true&stdout=true",
  "verb": "create",
  "user": {
    "username": "system:serviceaccount:cozy-kubeovn:kube-ovn-plunger",
    "uid": "c0ad9728-52c2-426b-817b-01c5f9e49eb7",
    "groups": [
      "system:serviceaccounts",
      "system:serviceaccounts:cozy-kubeovn",
      "system:authenticated"
    ],
    "extra": {
      "authentication.kubernetes.io/credential-id": [
        "JTI=46b362c9-c8e0-4295-aa6c-431298f08e6b"
      ],
      "authentication.kubernetes.io/node-name": [
        "talos-c1194"
      ],
      "authentication.kubernetes.io/node-uid": [
        "98394f25-77d1-42a4-be30-2b189366cf26"
      ],
      "authentication.kubernetes.io/pod-name": [
        "kube-ovn-plunger-9b759b798-x58r8"
      ],
      "authentication.kubernetes.io/pod-uid": [
        "6a021464-f96a-42be-8249-71b8ad212ece"
      ]
    }
  },
  "sourceIPs": [
    "10.244.0.215"
  ],
  "userAgent": "Go-http-client/1.1",
  "objectRef": {
    "resource": "pods",
    "namespace": "cozy-kubeovn",
    "name": "ovn-central-7955dc78d8-lvwh4",
    "apiVersion": "v1",
    "subresource": "exec"
  },
  "responseStatus": {
    "metadata": {},
    "code": 101
  },
  "requestReceivedTimestamp": "2026-05-22T19:29:23.082656Z",
  "stageTimestamp": "2026-05-22T19:29:23.093528Z",
  "annotations": {
    "authorization.k8s.io/decision": "allow",
    "authorization.k8s.io/reason": "RBAC: allowed by RoleBinding \"kube-ovn-plunger/cozy-kubeovn\" of Role \"kube-ovn-plunger\" to ServiceAccount \"kube-ovn-plunger/cozy-kubeovn\""
  }
}
]}`
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Zero(t, joiner.calls, "pods/exec is a streaming subresource, not a pod create")
	assert.Empty(t, queue.events, "pods/exec must not enter the canonical Git audit stream")
}

// TestClassifyAuditIngress_RejectsFailedRequests pins the intrinsic gate's
// verdict on responseStatus.code: any non-success code (>= 300) is rejected as
// a failed request, while a missing/zero code and 2xx codes pass.
func TestClassifyAuditIngress_RejectsFailedRequests(t *testing.T) {
	newEvent := func(code int32, withStatus bool) *auditv1.Event {
		ev := &auditv1.Event{
			Stage:     auditv1.StageResponseComplete,
			Verb:      "update",
			ObjectRef: &auditv1.ObjectReference{Resource: "helmreleases", Name: "info-rd"},
		}
		if withStatus {
			ev.ResponseStatus = &metav1.Status{Code: code}
		}
		return ev
	}

	tests := []struct {
		name        string
		event       *auditv1.Event
		wantProcess bool
		wantReason  string
	}{
		{name: "409 Conflict rejected", event: newEvent(409, true), wantReason: "failed_request"},
		{name: "403 Forbidden rejected", event: newEvent(403, true), wantReason: "failed_request"},
		{name: "422 Unprocessable rejected", event: newEvent(422, true), wantReason: "failed_request"},
		{name: "500 ServerError rejected", event: newEvent(500, true), wantReason: "failed_request"},
		{name: "200 OK accepted", event: newEvent(200, true), wantProcess: true},
		{name: "201 Created accepted", event: newEvent(201, true), wantProcess: true},
		{name: "missing responseStatus accepted", event: newEvent(0, false), wantProcess: true},
		{name: "zero code accepted", event: newEvent(0, true), wantProcess: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decision := classifyAuditIngress(AuditSourceOfficial, tc.event, AuditEventQualityComplete)
			assert.Equal(t, tc.wantProcess, decision.Process)
			if !tc.wantProcess {
				assert.Equal(t, tc.wantReason, decision.Reason)
			}
		})
	}
}

func TestAuditHandler_FiltersDryRunAndUnchangedRVEvents(t *testing.T) {
	const filteredMetric = "gitopsreverser_audit_events_total"

	tests := []struct {
		name       string
		fixture    string
		wantReason string
		wantMatch  map[string]string
	}{
		{
			name:       "dry-run patch is acknowledged but never mirrored",
			fixture:    "testdata/audit-events/flux-secret-dryrun-patch-eventlist.yaml",
			wantReason: "dry_run",
			wantMatch: map[string]string{
				"outcome": "dry_run", "category": "dropped",
				"group": "", "version": "v1", "resource": "secrets", "verb": "patch",
			},
		},
		{
			name:       "unchanged request and response RV is acknowledged but never mirrored",
			fixture:    "testdata/audit-events/k3s-addon-unchanged-rv-update-eventlist.yaml",
			wantReason: "unchanged_resource_version",
			wantMatch: map[string]string{
				"outcome": "unchanged_resource_version", "category": "dropped",
				"group": "k3s.cattle.io", "version": "v1", "resource": "addons", "verb": "update",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader, err := telemetry.InitTestExporter()
			require.NoError(t, err)

			byType := &recordingByTypeQueue{}
			joiner := &fakeAuditJoiner{decision: AuditJoinDecision{Action: AuditJoinActionEmit}}
			handler, err := NewAuditHandler(AuditHandlerConfig{ByTypeQueue: byType, Joiner: joiner})
			require.NoError(t, err)

			req := httptest.NewRequest(
				http.MethodPost,
				"/audit-webhook",
				bytes.NewReader([]byte(eventListFixtureBody(t, tt.fixture))),
			)
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)

			assert.Equal(t, http.StatusOK, w.Code)
			assert.Zero(t, joiner.calls, "filtered events must not enter the join pipeline")
			assert.Empty(t, byType.events, "filtered events must not enter the per-type mirror")

			got, ok := telemetry.CollectInt64Sum(reader, filteredMetric, tt.wantMatch)
			require.True(t, ok, "expected filtered metric sample for %s", tt.wantReason)
			assert.Equal(t, int64(1), got)
		})
	}
}

func TestAuditHandler_ChangedOrCreatedEventsStillMirror(t *testing.T) {
	tests := []struct {
		name     string
		fixture  string
		wantIDs  []string
		whyAlive string
	}{
		{
			name:     "persisted patch has changed request and response RVs",
			fixture:  "testdata/audit-events/flux-secret-persisted-patch-eventlist.yaml",
			wantIDs:  []string{"persisted-secret-patch"},
			whyAlive: "persisted changed-RV events still enter the join pipeline",
		},
		{
			name:     "create has objectRef RV but no request-body RV",
			fixture:  "testdata/audit-events/aggregated-flunder-create-eventlist.yaml",
			wantIDs:  []string{"aggregated-flunder-create"},
			whyAlive: "create events must not be treated as unchanged from objectRef RV alone",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			byType := &recordingByTypeQueue{}
			joiner := &fakeAuditJoiner{decision: AuditJoinDecision{Action: AuditJoinActionEmit}}
			handler, err := NewAuditHandler(AuditHandlerConfig{ByTypeQueue: byType, Joiner: joiner})
			require.NoError(t, err)

			req := httptest.NewRequest(
				http.MethodPost,
				"/audit-webhook",
				bytes.NewReader([]byte(eventListFixtureBody(t, tt.fixture))),
			)
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)

			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, 1, joiner.calls, tt.whyAlive)
			assert.Equal(t, tt.wantIDs, byType.auditIDs())
		})
	}
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

// TestAuditHandler_ByTypeQueueMirrorsAcceptedEvent confirms that an accepted
// ResponseComplete event is mirrored to the per-resource-type sink.
func TestAuditHandler_ByTypeQueueMirrorsAcceptedEvent(t *testing.T) {
	byType := &recordingByTypeQueue{}
	handler, err := NewAuditHandler(AuditHandlerConfig{ByTypeQueue: byType})
	require.NoError(t, err)

	body := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[` +
		`{"kind":"Event","auditID":"bytype-1","verb":"update","stage":"ResponseComplete",` +
		`"user":{"username":"test-user"},"objectRef":{"resource":"deployments",` +
		`"apiGroup":"apps","apiVersion":"v1","namespace":"prod","name":"web"},` +
		`"responseObject":{"metadata":{"resourceVersion":"42"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []string{"bytype-1"}, byType.auditIDs(),
		"the accepted event must be mirrored to the per-resource-type sink")
}

// TestAuditHandler_ByTypeQueueSkipsNonResponseCompleteStages confirms the mirror is
// only fed StageResponseComplete events.
func TestAuditHandler_ByTypeQueueSkipsNonResponseCompleteStages(t *testing.T) {
	byType := &recordingByTypeQueue{}
	handler, err := NewAuditHandler(AuditHandlerConfig{ByTypeQueue: byType})
	require.NoError(t, err)

	body := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[` +
		`{"kind":"Event","auditID":"req-received-1","verb":"update","stage":"RequestReceived",` +
		`"user":{"username":"test-user"},"objectRef":{"resource":"deployments",` +
		`"apiGroup":"apps","apiVersion":"v1","namespace":"prod","name":"web"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, byType.events, "non-ResponseComplete stages must not be mirrored")
}

// TestAuditHandler_ByTypeMirrorFailureDoesNotFailRequest confirms the mirror is
// best-effort: an error from the per-type sink leaves the request successful.
func TestAuditHandler_ByTypeMirrorFailureDoesNotFailRequest(t *testing.T) {
	byType := &recordingByTypeQueue{err: errors.New("bytype stream down")}
	handler, err := NewAuditHandler(AuditHandlerConfig{ByTypeQueue: byType})
	require.NoError(t, err)

	body := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[` +
		`{"kind":"Event","auditID":"bytype-fail-1","verb":"update","stage":"ResponseComplete",` +
		`"user":{"username":"test-user"},"objectRef":{"resource":"deployments",` +
		`"apiGroup":"apps","apiVersion":"v1","namespace":"prod","name":"web"},` +
		`"responseObject":{"metadata":{"resourceVersion":"42"}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/audit-webhook", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "a mirror failure must not fail the audit request")
}

// TestAuditHandler_DuplicateOfficialDeliveryMirrorsTwice pins the C-C contract
// that replaced the auditID decision key: a webhook retry re-mirrors the event
// at the same resourceVersion, and the RV-keyed per-type stream + idempotent
// splice fold absorb it downstream (zero extra Git effect — see the splice's
// duplicate-delivery test).
func TestAuditHandler_DuplicateOfficialDeliveryMirrorsTwice(t *testing.T) {
	byType := &recordingByTypeQueue{}
	handler, err := NewAuditHandler(AuditHandlerConfig{ByTypeQueue: byType})
	require.NoError(t, err)

	body := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[` +
		`{"kind":"Event","auditID":"dup-1","verb":"update","stage":"ResponseComplete",` +
		`"user":{"username":"test-user"},"objectRef":{"resource":"deployments",` +
		`"apiGroup":"apps","apiVersion":"v1","namespace":"prod","name":"web"},` +
		`"responseObject":{"metadata":{"resourceVersion":"42"}}}]}`

	for range 2 {
		req := httptest.NewRequest(http.MethodPost, "/audit-webhook", bytes.NewReader([]byte(body)))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
	}

	assert.Equal(t, []string{"dup-1", "dup-1"}, byType.auditIDs(),
		"duplicate delivery is no longer suppressed at the webhook; the stream absorbs it")
}
