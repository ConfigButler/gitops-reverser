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
	mu        sync.Mutex
	err       error
	events    []auditv1.Event
	providers []string
}

func (r *fakeFactRecorder) RecordFact(_ context.Context, providerName string, event auditv1.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.events = append(r.events, event)
	r.providers = append(r.providers, providerName)
	return nil
}

// lastProvider returns the provider name threaded into the most recent RecordFact call.
func (r *fakeFactRecorder) lastProvider() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.providers) == 0 {
		return ""
	}
	return r.providers[len(r.providers)-1]
}

// recordedProviders returns the provider name threaded into each RecordFact call, in order — the
// fan-out a single annotation-routed batch produced.
func (r *fakeFactRecorder) recordedProviders() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.providers...)
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

// defaultRoute is the named audit route a single-cluster install posts to. Audit routes are NAMED,
// and the bare /audit-webhook is the shared, annotation-routed endpoint that is off unless
// AuditRouteAnnotationKey is set — so every test about event classification (rather than routing)
// posts here, exactly as a single-cluster apiserver would.
const defaultRoute = "/audit-webhook/default"

// routedConfig used to fill in the ProviderResolver that defaultRoute was existence-gated on.
// Ingestion no longer reads Kubernetes at all, so it is now identity, kept so the classification
// tests keep reading as "post to a normal route" rather than growing an explanation each.
func routedConfig(config AuditHandlerConfig) AuditHandlerConfig { return config }

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

// TestAuditHandler_NamedDefaultRouteThreadsItsProvider checks that /audit-webhook/default is an
// ordinary named route: it records its facts under the "default" ClusterProvider name.
func TestAuditHandler_NamedDefaultRouteThreadsItsProvider(t *testing.T) {
	recorder := &fakeFactRecorder{}
	handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{FactRecorder: recorder}))
	require.NoError(t, err)

	body := `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[` + acceptedCreateEvent + `]}`
	w := serveBody(t, handler, http.MethodPost, defaultRoute, body)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, []string{"create-1"}, recorder.auditIDs())
	assert.Equal(t, "default", recorder.lastProvider(), "the named default route keys facts by its own name")
}

// TestAuditHandler_NamedRouting checks the /audit-webhook/<route> mapping: the route is taken from
// the path verbatim and keys the facts, with NO check that a ClusterProvider declares it. Ingestion
// is a partition write, not a claim about an object, so a route nobody reads costs one expiring key
// and a provider created later still joins its facts.
func TestAuditHandler_NamedRouting(t *testing.T) {
	body := eventListBody(acceptedCreateEvent)

	t.Run("a named route records under its own name", func(t *testing.T) {
		recorder := &fakeFactRecorder{}
		handler, err := NewAuditHandler(AuditHandlerConfig{FactRecorder: recorder})
		require.NoError(t, err)
		w := serveBody(t, handler, http.MethodPost, "/audit-webhook/prod-eu-1", body)
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "prod-eu-1", recorder.lastProvider())
	})

	t.Run("a route no ClusterProvider declares is stored, not refused", func(t *testing.T) {
		recorder := &fakeFactRecorder{}
		handler, err := NewAuditHandler(AuditHandlerConfig{FactRecorder: recorder})
		require.NoError(t, err)
		w := serveBody(t, handler, http.MethodPost, "/audit-webhook/not-declared-yet", body)
		require.Equal(t, http.StatusOK, w.Code,
			"a 404 here dropped batches in flight while a provider was being created; "+
				"the API server does not retry one")
		assert.Equal(t, "not-declared-yet", recorder.lastProvider())
	})

	t.Run("default is an ordinary route", func(t *testing.T) {
		recorder := &fakeFactRecorder{}
		handler, err := NewAuditHandler(AuditHandlerConfig{FactRecorder: recorder})
		require.NoError(t, err)
		w := serveBody(t, handler, http.MethodPost, defaultRoute, body)
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "default", recorder.lastProvider(), "default has no privileged route")
	})

	t.Run("bare endpoint is 400 while no annotation key is configured", func(t *testing.T) {
		recorder := &fakeFactRecorder{}
		handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{FactRecorder: recorder}))
		require.NoError(t, err)
		w := serveBody(t, handler, http.MethodPost, "/audit-webhook", body)
		assert.Equal(t, http.StatusBadRequest, w.Code,
			"the bare endpoint reads no route of its own, so a producer posting to it is misconfigured")
		assert.Zero(t, recorder.len())
	})
}

// clusterAnnotation is the audit-event annotation a shared stream stamps with each event's audit
// route, matching --author-attribution-audit-route-annotation-key.
const clusterAnnotation = "example.io/source-cluster"

// annotatedEvent builds an otherwise-acceptable create event carrying an audit-event annotation
// map. A nil map is an event a producer did not stamp at all.
func annotatedEvent(auditID string, annotations map[string]string) string {
	encoded, err := json.Marshal(annotations)
	if err != nil {
		panic(err)
	}
	return `{"kind":"Event","level":"RequestResponse","auditID":"` + auditID + `",` +
		`"stage":"ResponseComplete","verb":"create","user":{"username":"test-user"},` +
		`"requestURI":"/api/v1/namespaces/default/configmaps",` +
		`"annotations":` + string(encoded) + `,` +
		`"objectRef":{"resource":"configmaps","namespace":"default","name":"cm","apiVersion":"v1"},` +
		`"responseStatus":{"code":200},` +
		`"responseObject":{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm","namespace":"default"}}}`
}

// TestAuditHandler_AnnotationRouting pins the shared-stream contract: with an annotation key
// configured the bare endpoint reads the AUDIT ROUTE per event, so one batch fans out to several
// routes, and an event that names none is rejected by itself, never credited to a fallback, while
// the rest of the batch still lands. A route no ClusterProvider has declared is NOT a rejection.
func TestAuditHandler_AnnotationRouting(t *testing.T) {
	newHandler := func(t *testing.T, recorder *fakeFactRecorder) *AuditHandler {
		t.Helper()
		handler, err := NewAuditHandler(AuditHandlerConfig{
			FactRecorder:            recorder,
			AuditRouteAnnotationKey: clusterAnnotation,
		})
		require.NoError(t, err)
		return handler
	}

	t.Run("one batch fans out to several source clusters", func(t *testing.T) {
		recorder := &fakeFactRecorder{}
		handler := newHandler(t, recorder)

		w := serveBody(t, handler, http.MethodPost, "/audit-webhook", eventListBody(
			annotatedEvent("eu", map[string]string{clusterAnnotation: "prod-eu-1"}),
			annotatedEvent("us", map[string]string{clusterAnnotation: "prod-us-1"}),
		))
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, []string{"eu", "us"}, recorder.auditIDs())
		assert.Equal(t, []string{"prod-eu-1", "prod-us-1"}, recorder.recordedProviders())
	})

	t.Run("an unstamped or unknown event is rejected without failing the batch", func(t *testing.T) {
		for _, tt := range []struct {
			name        string
			annotations map[string]string
		}{
			{"no annotation at all", nil},
			{"the key present but empty", map[string]string{clusterAnnotation: ""}},
			{"a different key", map[string]string{"other.io/cluster": "prod-eu-1"}},
		} {
			t.Run(tt.name, func(t *testing.T) {
				recorder := &fakeFactRecorder{}
				handler := newHandler(t, recorder)

				w := serveBody(t, handler, http.MethodPost, "/audit-webhook", eventListBody(
					annotatedEvent("rejected", tt.annotations),
					annotatedEvent("kept", map[string]string{clusterAnnotation: "prod-eu-1"}),
				))
				require.Equal(t, http.StatusOK, w.Code,
					"a heterogeneous stream must not be retried wholesale for one bad event")
				assert.Equal(t, []string{"kept"}, recorder.auditIDs(), "the rejected event produces no fact")
				assert.Equal(t, []string{"prod-eu-1"}, recorder.recordedProviders(),
					"and is never credited to a fallback provider")
			})
		}
	})

	t.Run("a route no ClusterProvider declares is stored like any other", func(t *testing.T) {
		recorder := &fakeFactRecorder{}
		handler := newHandler(t, recorder)

		w := serveBody(t, handler, http.MethodPost, "/audit-webhook", eventListBody(
			annotatedEvent("eu", map[string]string{clusterAnnotation: "never-declared"}),
		))
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, []string{"never-declared"}, recorder.recordedProviders(),
			"the annotation names a partition, not an object that must already exist")
	})

	t.Run("named routes ignore the annotation", func(t *testing.T) {
		recorder := &fakeFactRecorder{}
		handler := newHandler(t, recorder)

		w := serveBody(t, handler, http.MethodPost, "/audit-webhook/prod-us-1", eventListBody(
			annotatedEvent("eu", map[string]string{clusterAnnotation: "prod-eu-1"}),
		))
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, []string{"prod-us-1"}, recorder.recordedProviders(),
			"a named route fixes the source cluster for its whole batch")
	})
}

// TestAuditRouteForPath covers the path -> (provider, named) mapping directly. The bare path
// names no provider at all: it is the shared, annotation-routed endpoint.
func TestAuditRouteForPath(t *testing.T) {
	name, named := auditRouteForPath("/audit-webhook")
	assert.Empty(t, name, "the bare path resolves no provider by itself")
	assert.False(t, named)

	name, named = auditRouteForPath("/audit-webhook/prod-eu-1")
	assert.Equal(t, "prod-eu-1", name)
	assert.True(t, named, "a segment names a provider")

	name, named = auditRouteForPath(defaultRoute)
	assert.Equal(t, "default", name, "default is an ordinary named route")
	assert.True(t, named)
}

func TestNewAuditHandler_DefaultsMaxBody(t *testing.T) {
	handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{}))
	require.NoError(t, err)
	assert.Equal(t, DefaultAuditMaxRequestBodyBytes, handler.config.MaxRequestBodyBytes)

	handler, err = NewAuditHandler(routedConfig(AuditHandlerConfig{MaxRequestBodyBytes: 4096}))
	require.NoError(t, err)
	assert.Equal(t, int64(4096), handler.config.MaxRequestBodyBytes)
}

// TestAuditHandler_MethodAndPathValidation pins the HTTP-method and path gates:
// only POST to a named /audit-webhook/<name> is accepted; the removed
// /audit-webhook-additional endpoint, trailing slashes, and extra segments are
// all 400, and so is the bare endpoint while annotation routing is off.
func TestAuditHandler_MethodAndPathValidation(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{"valid POST to a named route", http.MethodPost, defaultRoute, http.StatusOK},
		{"GET rejected", http.MethodGet, defaultRoute, http.StatusMethodNotAllowed},
		{"PUT rejected", http.MethodPut, defaultRoute, http.StatusMethodNotAllowed},
		{"DELETE rejected", http.MethodDelete, defaultRoute, http.StatusMethodNotAllowed},
		{"trailing slash rejected", http.MethodPost, "/audit-webhook/", http.StatusBadRequest},
		{"removed additional endpoint rejected", http.MethodPost, "/audit-webhook-additional", http.StatusBadRequest},
		{"two segments rejected", http.MethodPost, "/audit-webhook/a/b", http.StatusBadRequest},
		{"unrelated path rejected", http.MethodPost, "/wrong", http.StatusBadRequest},
		// The bare endpoint only means something with an annotation key configured; this handler has
		// none, so a producer posting there is misconfigured rather than routed to a default.
		{"bare endpoint rejected without an annotation key", http.MethodPost, "/audit-webhook", http.StatusBadRequest},
		// A name with no ClusterProvider behind it is 404 — the gate applies to every name.
		{"any named route is served", http.MethodPost, "/audit-webhook/prod-eu-1", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{}))
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
		{"named provider segment is valid", "/audit-webhook/prod-eu-1", false},
		{"two segments", "/audit-webhook/a/b", true},
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
			handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{FactRecorder: recorder}))
			require.NoError(t, err)

			w := serveBody(t, handler, http.MethodPost, defaultRoute, tt.body)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Zero(t, recorder.len(), "a decode failure records no facts")
		})
	}
}

func TestAuditHandler_RejectsOversizedBody(t *testing.T) {
	recorder := &fakeFactRecorder{}
	handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{MaxRequestBodyBytes: 32, FactRecorder: recorder}))
	require.NoError(t, err)

	w := serveBody(t, handler, http.MethodPost, defaultRoute, eventListBody(acceptedCreateEvent))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "request body too large")
	assert.Zero(t, recorder.len())
}

func TestAuditHandler_EmptyEventListRecordsNothing(t *testing.T) {
	recorder := &fakeFactRecorder{}
	handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{FactRecorder: recorder}))
	require.NoError(t, err)

	w := serveBody(t, handler, http.MethodPost, defaultRoute, eventListBody())
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Zero(t, recorder.len(), "an empty event list records no facts")
}

// TestAuditHandler_AcceptedEventRecordsFact is the happy path: a canonical
// mutating event reaches the FactRecorder and the request returns 200.
func TestAuditHandler_AcceptedEventRecordsFact(t *testing.T) {
	recorder := &fakeFactRecorder{}
	handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{FactRecorder: recorder}))
	require.NoError(t, err)

	w := serveBody(t, handler, http.MethodPost, defaultRoute, eventListBody(acceptedCreateEvent))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []string{"create-1"}, recorder.auditIDs())
}

// TestAuditHandler_NilRecorderAcceptsWithoutRecording confirms configured-author
// mode: a nil FactRecorder records nothing yet still returns 200.
func TestAuditHandler_NilRecorderAcceptsWithoutRecording(t *testing.T) {
	handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{})) // FactRecorder nil
	require.NoError(t, err)

	w := serveBody(t, handler, http.MethodPost, defaultRoute, eventListBody(acceptedCreateEvent))
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestAuditHandler_RecordsEveryAcceptedEventInBatch confirms the handler walks
// the whole list, recording each accepted event in order.
func TestAuditHandler_RecordsEveryAcceptedEventInBatch(t *testing.T) {
	recorder := &fakeFactRecorder{}
	handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{FactRecorder: recorder}))
	require.NoError(t, err)

	second := `{"kind":"Event","auditID":"update-1","stage":"ResponseComplete","verb":"update",` +
		`"user":{"username":"test-user"},` +
		`"objectRef":{"resource":"deployments","apiGroup":"apps","apiVersion":"apps/v1",` +
		`"namespace":"prod","name":"web"},"responseStatus":{"code":200},` +
		`"responseObject":{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","resourceVersion":"7"}}}`

	w := serveBody(t, handler, http.MethodPost, defaultRoute, eventListBody(acceptedCreateEvent, second))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []string{"create-1", "update-1"}, recorder.auditIDs())
}

// TestAuditHandler_RecordFactErrorFailsRequest pins the retry contract: a
// fact-store failure surfaces as 500 so the API server redelivers.
func TestAuditHandler_RecordFactErrorFailsRequest(t *testing.T) {
	recorder := &fakeFactRecorder{err: errors.New("fact store down")}
	handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{FactRecorder: recorder}))
	require.NoError(t, err)

	w := serveBody(t, handler, http.MethodPost, defaultRoute, eventListBody(acceptedCreateEvent))
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
			handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{FactRecorder: recorder}))
			require.NoError(t, err)

			w := serveBody(t, handler, http.MethodPost, defaultRoute, eventListBody(tt.event))
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
			handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{FactRecorder: recorder}))
			require.NoError(t, err)

			w := serveBody(t, handler, http.MethodPost, defaultRoute, eventListBody(tt.event))
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
	handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{FactRecorder: recorder}))
	require.NoError(t, err)

	second := `{"kind":"Event","auditID":"update-1","stage":"ResponseComplete","verb":"update",` +
		`"objectRef":{"resource":"configmaps","apiVersion":"v1"},` +
		`"responseObject":{"metadata":{"resourceVersion":"5"}}}`

	w := serveBody(t, handler, http.MethodPost, defaultRoute, eventListBody(acceptedCreateEvent, second))
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
	handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{FactRecorder: recorder}))
	require.NoError(t, err)

	w := serveBody(t, handler, http.MethodPost, defaultRoute, eventListBody(string(recording)))
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
			handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{FactRecorder: recorder}))
			require.NoError(t, err)

			w := serveBody(t, handler, http.MethodPost, defaultRoute, eventListFixtureBody(t, tt.fixture))
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
			handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{FactRecorder: recorder}))
			require.NoError(t, err)

			w := serveBody(t, handler, http.MethodPost, defaultRoute, eventListFixtureBody(t, tt.fixture))
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
