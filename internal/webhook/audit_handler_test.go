// SPDX-License-Identifier: Apache-2.0

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

	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"
	"sigs.k8s.io/yaml"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// TestMain initializes telemetry once for the package. The handler's metric
// calls are nil-guarded, so tests pass without it, but initializing here keeps
// the steady-state code paths exercised. Per-test value assertions use a fresh
// telemetry.InitTestExporter() reader.
func TestMain(m *testing.M) {
	if _, err := telemetry.InitOTLPExporter(context.Background()); err != nil {
		panic("Failed to initialize metrics: " + err.Error())
	}
	os.Exit(m.Run())
}

// fakeFactRecorder is an in-memory AuditFactRecorder. It appends every accepted
// event and can be told to fail with an injectable error.
type fakeFactRecorder struct {
	mu     sync.Mutex
	err    error
	events []auditv1.Event
}

func (r *fakeFactRecorder) RecordFact(_ context.Context, event auditv1.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.events = append(r.events, event)
	return nil
}

func (r *fakeFactRecorder) auditIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.events))
	for _, event := range r.events {
		ids = append(ids, string(event.AuditID))
	}
	return ids
}

func (r *fakeFactRecorder) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// eventListBody wraps zero or more event JSON fragments into an EventList body.
func eventListBody(items ...string) string {
	return `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[` +
		strings.Join(items, ",") + `]}`
}

// eventListFixtureBody reads a YAML EventList fixture and re-encodes it as the
// JSON body the API server would POST.
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

// serveBody runs one POST request through the handler and returns the recorder.
func serveBody(t *testing.T, handler *AuditHandler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// A canonical, accepted create event: ResponseComplete, mutating verb, success
// status, top-level resource, no unchanged-RV. It must reach the FactRecorder.
const acceptedCreateEvent = `{"kind":"Event","level":"RequestResponse","auditID":"create-1",` +
	`"stage":"ResponseComplete","verb":"create","user":{"username":"test-user"},` +
	`"requestURI":"/api/v1/namespaces/default/configmaps",` +
	`"objectRef":{"resource":"configmaps","namespace":"default","name":"cm","apiVersion":"v1"},` +
	`"responseStatus":{"code":200},` +
	`"responseObject":{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm","namespace":"default"}}}`

func TestNewAuditHandler_DefaultsMaxBody(t *testing.T) {
	handler, err := NewAuditHandler(AuditHandlerConfig{})
	require.NoError(t, err)
	assert.Equal(t, DefaultAuditMaxRequestBodyBytes, handler.config.MaxRequestBodyBytes)

	handler, err = NewAuditHandler(AuditHandlerConfig{MaxRequestBodyBytes: 4096})
	require.NoError(t, err)
	assert.Equal(t, int64(4096), handler.config.MaxRequestBodyBytes)
}

// TestAuditHandler_MethodAndPathValidation pins the HTTP-method and path gates:
// only POST to the canonical /audit-webhook is accepted; the removed
// /audit-webhook-additional endpoint, trailing slashes, and extra segments are
// all 400.
func TestAuditHandler_MethodAndPathValidation(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{"valid POST", http.MethodPost, "/audit-webhook", http.StatusOK},
		{"GET rejected", http.MethodGet, "/audit-webhook", http.StatusMethodNotAllowed},
		{"PUT rejected", http.MethodPut, "/audit-webhook", http.StatusMethodNotAllowed},
		{"DELETE rejected", http.MethodDelete, "/audit-webhook", http.StatusMethodNotAllowed},
		{"trailing slash rejected", http.MethodPost, "/audit-webhook/", http.StatusBadRequest},
		{"removed additional endpoint rejected", http.MethodPost, "/audit-webhook-additional", http.StatusBadRequest},
		{"extra segment rejected", http.MethodPost, "/audit-webhook/extra", http.StatusBadRequest},
		{"unrelated path rejected", http.MethodPost, "/wrong", http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, err := NewAuditHandler(AuditHandlerConfig{})
			require.NoError(t, err)

			w := serveBody(t, handler, tt.method, tt.path, eventListBody(acceptedCreateEvent))
			assert.Equal(t, tt.wantStatus, w.Code)
		})
	}
}

func TestValidateAuditWebhookPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"canonical", "/audit-webhook", false},
		{"trailing slash", "/audit-webhook/", true},
		{"removed additional endpoint", "/audit-webhook-additional", true},
		{"extra segment", "/audit-webhook/extra", true},
		{"cluster id segment", "/audit-webhook/cluster-a", true},
		{"unrelated", "/healthz", true},
		{"root", "/", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAuditWebhookPath(tt.path)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestAuditHandler_DecodeErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"not json", "invalid json"},
		{"truncated json", `{"kind":"EventList","items":[`},
		{"wrong type", `["just","an","array"]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &fakeFactRecorder{}
			handler, err := NewAuditHandler(AuditHandlerConfig{FactRecorder: recorder})
			require.NoError(t, err)

			w := serveBody(t, handler, http.MethodPost, "/audit-webhook", tt.body)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Zero(t, recorder.len(), "a decode failure records no facts")
		})
	}
}

func TestAuditHandler_RejectsOversizedBody(t *testing.T) {
	recorder := &fakeFactRecorder{}
	handler, err := NewAuditHandler(AuditHandlerConfig{MaxRequestBodyBytes: 32, FactRecorder: recorder})
	require.NoError(t, err)

	w := serveBody(t, handler, http.MethodPost, "/audit-webhook", eventListBody(acceptedCreateEvent))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "request body too large")
	assert.Zero(t, recorder.len())
}

func TestAuditHandler_EmptyEventListRecordsNothing(t *testing.T) {
	recorder := &fakeFactRecorder{}
	handler, err := NewAuditHandler(AuditHandlerConfig{FactRecorder: recorder})
	require.NoError(t, err)

	w := serveBody(t, handler, http.MethodPost, "/audit-webhook", eventListBody())
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Zero(t, recorder.len(), "an empty event list records no facts")
}

// TestAuditHandler_AcceptedEventRecordsFact is the happy path: a canonical
// mutating event reaches the FactRecorder and the request returns 200.
func TestAuditHandler_AcceptedEventRecordsFact(t *testing.T) {
	recorder := &fakeFactRecorder{}
	handler, err := NewAuditHandler(AuditHandlerConfig{FactRecorder: recorder})
	require.NoError(t, err)

	w := serveBody(t, handler, http.MethodPost, "/audit-webhook", eventListBody(acceptedCreateEvent))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []string{"create-1"}, recorder.auditIDs())
}

// TestAuditHandler_NilRecorderAcceptsWithoutRecording confirms configured-author
// mode: a nil FactRecorder records nothing yet still returns 200.
func TestAuditHandler_NilRecorderAcceptsWithoutRecording(t *testing.T) {
	handler, err := NewAuditHandler(AuditHandlerConfig{}) // FactRecorder nil
	require.NoError(t, err)

	w := serveBody(t, handler, http.MethodPost, "/audit-webhook", eventListBody(acceptedCreateEvent))
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestAuditHandler_RecordsEveryAcceptedEventInBatch confirms the handler walks
// the whole list, recording each accepted event in order.
func TestAuditHandler_RecordsEveryAcceptedEventInBatch(t *testing.T) {
	recorder := &fakeFactRecorder{}
	handler, err := NewAuditHandler(AuditHandlerConfig{FactRecorder: recorder})
	require.NoError(t, err)

	second := `{"kind":"Event","auditID":"update-1","stage":"ResponseComplete","verb":"update",` +
		`"user":{"username":"test-user"},` +
		`"objectRef":{"resource":"deployments","apiGroup":"apps","apiVersion":"apps/v1",` +
		`"namespace":"prod","name":"web"},"responseStatus":{"code":200},` +
		`"responseObject":{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","resourceVersion":"7"}}}`

	w := serveBody(t, handler, http.MethodPost, "/audit-webhook", eventListBody(acceptedCreateEvent, second))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []string{"create-1", "update-1"}, recorder.auditIDs())
}

// TestAuditHandler_RecordFactErrorFailsRequest pins the retry contract: a
// fact-store failure surfaces as 500 so the API server redelivers.
func TestAuditHandler_RecordFactErrorFailsRequest(t *testing.T) {
	recorder := &fakeFactRecorder{err: errors.New("fact store down")}
	handler, err := NewAuditHandler(AuditHandlerConfig{FactRecorder: recorder})
	require.NoError(t, err)

	w := serveBody(t, handler, http.MethodPost, "/audit-webhook", eventListBody(acceptedCreateEvent))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "fact store down")
}

// TestAuditHandler_RejectedEventsAreDropped table-drives the intrinsic accept
// gate. A rejected event records no fact and the request still returns 200.
func TestAuditHandler_RejectedEventsAreDropped(t *testing.T) {
	tests := []struct {
		name  string
		event string
		why   string
	}{
		{
			name: "non response-complete stage",
			event: `{"kind":"Event","auditID":"stage-1","stage":"RequestReceived","verb":"create",` +
				`"objectRef":{"resource":"configmaps","apiVersion":"v1"},` +
				`"responseObject":{"metadata":{"resourceVersion":"5"}}}`,
			why: "only ResponseComplete events are facts",
		},
		{
			name: "read-only verb",
			event: `{"kind":"Event","auditID":"get-1","stage":"ResponseComplete","verb":"get",` +
				`"objectRef":{"resource":"configmaps","apiVersion":"v1"},` +
				`"responseObject":{"metadata":{"resourceVersion":"5"}}}`,
			why: "get/list/watch are not mutations",
		},
		{
			name: "failed request (409 conflict)",
			event: `{"kind":"Event","auditID":"conflict-1","stage":"ResponseComplete","verb":"update",` +
				`"objectRef":{"resource":"helmreleases","apiGroup":"helm.toolkit.fluxcd.io","apiVersion":"helm.toolkit.fluxcd.io/v2"},` +
				`"responseStatus":{"code":409},` +
				`"responseObject":{"apiVersion":"v1","kind":"Status","metadata":{"resourceVersion":"5"}}}`,
			why: "a >=300 response never reached etcd",
		},
		{
			name: "dry-run all",
			event: `{"kind":"Event","auditID":"dryrun-1","stage":"ResponseComplete","verb":"patch",` +
				`"requestURI":"/api/v1/namespaces/default/secrets/s?dryRun=All",` +
				`"objectRef":{"resource":"secrets","apiVersion":"v1"},` +
				`"responseStatus":{"code":200},` +
				`"responseObject":{"metadata":{"resourceVersion":"5"}}}`,
			why: "a dry-run was not persisted",
		},
		{
			name: "unchanged resource version",
			event: `{"kind":"Event","auditID":"noop-1","stage":"ResponseComplete","verb":"update",` +
				`"objectRef":{"resource":"configmaps","apiVersion":"v1"},` +
				`"responseStatus":{"code":200},` +
				`"requestObject":{"metadata":{"resourceVersion":"9"}},` +
				`"responseObject":{"metadata":{"resourceVersion":"9"}}}`,
			why: "an unchanged RV did not advance stored state",
		},
		{
			name: "status subresource",
			event: `{"kind":"Event","auditID":"status-1","stage":"ResponseComplete","verb":"update",` +
				`"objectRef":{"resource":"deployments","apiGroup":"apps","apiVersion":"apps/v1","subresource":"status"},` +
				`"responseStatus":{"code":200},` +
				`"responseObject":{"metadata":{"resourceVersion":"5"}}}`,
			why: "status subresource is not attributed",
		},
		{
			name: "exec subresource",
			event: `{"kind":"Event","auditID":"exec-1","stage":"ResponseComplete","verb":"create",` +
				`"objectRef":{"resource":"pods","apiVersion":"v1","subresource":"exec"},` +
				`"responseStatus":{"code":101}}`,
			why: "exec is a streaming subresource, not a mutation",
		},
		{
			name: "log subresource",
			event: `{"kind":"Event","auditID":"log-1","stage":"ResponseComplete","verb":"get",` +
				`"objectRef":{"resource":"pods","apiVersion":"v1","subresource":"log"}}`,
			why: "log is read-only streaming",
		},
		{
			name: "arbitrary non-scale subresource",
			event: `{"kind":"Event","auditID":"proxy-1","stage":"ResponseComplete","verb":"create",` +
				`"objectRef":{"resource":"services","apiVersion":"v1","subresource":"proxy"},` +
				`"responseStatus":{"code":200}}`,
			why: "only /scale subresources forward",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &fakeFactRecorder{}
			handler, err := NewAuditHandler(AuditHandlerConfig{FactRecorder: recorder})
			require.NoError(t, err)

			w := serveBody(t, handler, http.MethodPost, "/audit-webhook", eventListBody(tt.event))
			assert.Equal(t, http.StatusOK, w.Code, tt.why)
			assert.Zero(t, recorder.len(), tt.why)
		})
	}
}

// TestAuditHandler_AcceptedEdgeCases exercises events the gate must accept:
// a mutating /scale subresource, a bodyless delete, and a missing
// responseStatus (treated as success).
func TestAuditHandler_AcceptedEdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		event   string
		auditID string
	}{
		{
			name: "scale subresource forwards",
			event: `{"kind":"Event","auditID":"scale-1","stage":"ResponseComplete","verb":"patch",` +
				`"objectRef":{"resource":"deployments","apiGroup":"apps","apiVersion":"apps/v1","subresource":"scale"},` +
				`"responseStatus":{"code":200},` +
				`"responseObject":{"kind":"Scale","metadata":{"resourceVersion":"5"}}}`,
			auditID: "scale-1",
		},
		{
			name: "bodyless delete is accepted",
			event: `{"kind":"Event","auditID":"delete-1","stage":"ResponseComplete","verb":"delete",` +
				`"objectRef":{"resource":"configmaps","apiVersion":"v1","name":"cm"},` +
				`"responseStatus":{"code":200}}`,
			auditID: "delete-1",
		},
		{
			name: "missing response status is accepted",
			event: `{"kind":"Event","auditID":"nostatus-1","stage":"ResponseComplete","verb":"create",` +
				`"objectRef":{"resource":"configmaps","apiVersion":"v1"},` +
				`"responseObject":{"metadata":{"resourceVersion":"5"}}}`,
			auditID: "nostatus-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &fakeFactRecorder{}
			handler, err := NewAuditHandler(AuditHandlerConfig{FactRecorder: recorder})
			require.NoError(t, err)

			w := serveBody(t, handler, http.MethodPost, "/audit-webhook", eventListBody(tt.event))
			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, []string{tt.auditID}, recorder.auditIDs())
		})
	}
}

// TestAuditHandler_BatchStopsOnFirstRecordError confirms a fact-store failure on
// an earlier event short-circuits the batch: later events are not recorded and
// the whole request is 500.
func TestAuditHandler_BatchStopsOnFirstRecordError(t *testing.T) {
	recorder := &fakeFactRecorder{err: errors.New("fact store down")}
	handler, err := NewAuditHandler(AuditHandlerConfig{FactRecorder: recorder})
	require.NoError(t, err)

	second := `{"kind":"Event","auditID":"update-1","stage":"ResponseComplete","verb":"update",` +
		`"objectRef":{"resource":"configmaps","apiVersion":"v1"},` +
		`"responseObject":{"metadata":{"resourceVersion":"5"}}}`

	w := serveBody(t, handler, http.MethodPost, "/audit-webhook", eventListBody(acceptedCreateEvent, second))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Zero(t, recorder.len(), "no facts persist when the recorder errors")
}

// TestAuditHandler_ForwardsRealScaleSubresourceRecording drives the captured
// `kubectl scale deployment` recording through the full path and asserts the
// deployments/scale event reaches the FactRecorder with verb/resource/subresource
// intact rather than being dropped.
func TestAuditHandler_ForwardsRealScaleSubresourceRecording(t *testing.T) {
	recording, err := os.ReadFile("testdata/audit-events/deployment-scale-subresource.json")
	require.NoError(t, err, "the captured scale recording must be readable")

	recorder := &fakeFactRecorder{}
	handler, err := NewAuditHandler(AuditHandlerConfig{FactRecorder: recorder})
	require.NoError(t, err)

	w := serveBody(t, handler, http.MethodPost, "/audit-webhook", eventListBody(string(recording)))
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 1, recorder.len(), "the real deployments/scale recording must be recorded")

	got := recorder.events[0]
	require.NotNil(t, got.ObjectRef)
	assert.Equal(t, "deployments", got.ObjectRef.Resource)
	assert.Equal(t, "scale", got.ObjectRef.Subresource)
	assert.Equal(t, "patch", got.Verb)
}

// TestAuditHandler_FixtureDryRunAndUnchangedRVDropped drives the real captured
// dry-run and unchanged-RV recordings and confirms neither is recorded.
func TestAuditHandler_FixtureDryRunAndUnchangedRVDropped(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
	}{
		{"dry-run patch", "testdata/audit-events/flux-secret-dryrun-patch-eventlist.yaml"},
		{"unchanged rv update", "testdata/audit-events/k3s-addon-unchanged-rv-update-eventlist.yaml"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &fakeFactRecorder{}
			handler, err := NewAuditHandler(AuditHandlerConfig{FactRecorder: recorder})
			require.NoError(t, err)

			w := serveBody(t, handler, http.MethodPost, "/audit-webhook", eventListFixtureBody(t, tt.fixture))
			assert.Equal(t, http.StatusOK, w.Code)
			assert.Zero(t, recorder.len(), "filtered events must not be recorded")
		})
	}
}

// TestAuditHandler_FixturePersistedAndCreateRecorded drives the real captured
// persisted-patch (changed RVs) and aggregated-create recordings and confirms
// both are recorded.
func TestAuditHandler_FixturePersistedAndCreateRecorded(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		wantID  string
	}{
		{
			name:    "persisted patch has changed RVs",
			fixture: "testdata/audit-events/flux-secret-persisted-patch-eventlist.yaml",
			wantID:  "persisted-secret-patch",
		},
		{
			name:    "create has objectRef RV but no request-body RV",
			fixture: "testdata/audit-events/aggregated-flunder-create-eventlist.yaml",
			wantID:  "aggregated-flunder-create",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &fakeFactRecorder{}
			handler, err := NewAuditHandler(AuditHandlerConfig{FactRecorder: recorder})
			require.NoError(t, err)

			w := serveBody(t, handler, http.MethodPost, "/audit-webhook", eventListFixtureBody(t, tt.fixture))
			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, []string{tt.wantID}, recorder.auditIDs())
		})
	}
}

func TestExtractGVR(t *testing.T) {
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
			var event auditv1.Event
			require.NoError(t, json.Unmarshal([]byte(tt.eventJSON), &event))
			assert.Equal(t, tt.expected, extractGVR(&event))
		})
	}
}
