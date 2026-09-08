// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

// errAuditTest is the injected fact-store failure used to drive the
// process_error EventList outcome.
var errAuditTest = errors.New("fact store down")

const (
	eventListDurationMetric = "gitopsreverser_audit_eventlist_duration_seconds"
	auditEventsMetric       = "gitopsreverser_audit_events_total"
)

// processErrorEventList decodes but fails processing: the FactRecorder is told
// to error, so an accepted event produces a process_error outcome.
const processErrorEvent = acceptedCreateEvent

// subresourceExecEventList is a one-item EventList for a pods/exec streaming
// request — verb=create with a non-/scale subresource and no resource body.
const subresourceExecEvent = `{"kind":"Event","level":"RequestResponse","auditID":"subres-exec-1",` +
	`"stage":"ResponseComplete","verb":"create","user":{"username":"test-user"},` +
	`"objectRef":{"resource":"pods","namespace":"default","name":"p","apiVersion":"v1","subresource":"exec"},` +
	`"responseStatus":{"code":101}}`

// TestServeHTTP_EventListIngressMetrics asserts the EventList request boundary across the four
// outcome labels.
//
// One instrument covers it: the histogram's own _count series IS the request counter, which is why
// the separate audit_eventlists_total it used to sit beside was removed rather than kept in step.
// The per-item counter went with it — audit_events_total counts the same items once each, with the
// type and verb on them, and TestServeHTTP_AcceptedEventQueuedOutcome below asserts that census.
func TestServeHTTP_EventListIngressMetrics(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		recorderErr bool
		wantOutcome string
		wantStatus  int
	}{
		{
			name:        "processed",
			body:        eventListBody(acceptedCreateEvent),
			wantOutcome: "processed",
			wantStatus:  http.StatusOK,
		},
		{
			name:        "empty event list",
			body:        eventListBody(),
			wantOutcome: "empty",
			wantStatus:  http.StatusOK,
		},
		{
			name:        "decode error",
			body:        "not json",
			wantOutcome: "decode_error",
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "process error",
			body:        eventListBody(processErrorEvent),
			recorderErr: true,
			wantOutcome: "process_error",
			wantStatus:  http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader, err := telemetry.InitTestExporter()
			require.NoError(t, err)

			recorder := &fakeFactSink{}
			if tt.recorderErr {
				recorder.err = errAuditTest
			}
			handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{FactPublisher: recorder}))
			require.NoError(t, err)

			w := serveBody(t, handler, http.MethodPost, defaultRoute, tt.body)
			assert.Equal(t, tt.wantStatus, w.Code)

			match := map[string]string{"outcome": tt.wantOutcome}

			// _count is the request counter. Asserting it here is what makes the removal of the
			// separate counter safe: the number an operator reads is still published, under the
			// series a histogram ships for free.
			durCount, ok := telemetry.CollectHistogramCount(reader, eventListDurationMetric, match)
			require.True(t, ok, "%s should have a sample for %v", eventListDurationMetric, match)
			assert.Equal(t, uint64(1), durCount)
		})
	}
}

// TestServeHTTP_AcceptedEventQueuedOutcome confirms an accepted event records the
// "queued" census outcome on audit_events_total (category="stored").
func TestServeHTTP_AcceptedEventQueuedOutcome(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	recorder := &fakeFactSink{}
	handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{FactPublisher: recorder}))
	require.NoError(t, err)

	w := serveBody(t, handler, http.MethodPost, defaultRoute, eventListBody(acceptedCreateEvent))
	require.Equal(t, http.StatusOK, w.Code)

	queued, ok := telemetry.CollectInt64Sum(reader, auditEventsMetric, map[string]string{
		"outcome": "queued", "category": "stored",
		"resource": "configmaps", "verb": "create",
	})
	require.True(t, ok, "expected a queued outcome sample for the accepted configmaps create")
	assert.Equal(t, int64(1), queued)
}

// TestServeHTTP_UnroutableEventOutcomes confirms an event the shared endpoint could not route is
// counted on audit_events_total under its own outcome, so a producer that stops stamping the
// annotation shows up as a rising drop rate instead of as silence.
func TestServeHTTP_UnroutableEventOutcomes(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		wantOutcome string
	}{
		{"unstamped event", nil, "missing_cluster_annotation"},
		{"the key present but empty", map[string]string{clusterAnnotation: ""}, "missing_cluster_annotation"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader, err := telemetry.InitTestExporter()
			require.NoError(t, err)

			recorder := &fakeFactSink{}
			handler, err := NewAuditHandler(AuditHandlerConfig{
				FactPublisher:           recorder,
				AuditRouteAnnotationKey: clusterAnnotation,
			})
			require.NoError(t, err)

			w := serveBody(t, handler, http.MethodPost, "/audit-webhook",
				eventListBody(annotatedEvent("unroutable", tt.annotations)))
			require.Equal(t, http.StatusOK, w.Code)
			assert.Zero(t, recorder.len())

			dropped, ok := telemetry.CollectInt64Sum(reader, auditEventsMetric, map[string]string{
				"outcome": tt.wantOutcome, "category": "dropped",
				"resource": "configmaps", "verb": "create",
			})
			require.True(t, ok, "expected a %s outcome sample", tt.wantOutcome)
			assert.Equal(t, int64(1), dropped)
		})
	}
}

// TestServeHTTP_NonScaleSubresourceDropped confirms a non-/scale subresource
// (pods/exec) is dropped before recording and recorded on audit_events_total as
// the non_scale_subresource outcome (resource="pods"), so a pods/exec flood is
// distinguishable rather than collapsing into a mirrored pod mutation.
func TestServeHTTP_NonScaleSubresourceDropped(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	recorder := &fakeFactSink{}
	handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{FactPublisher: recorder}))
	require.NoError(t, err)

	w := serveBody(t, handler, http.MethodPost, defaultRoute, eventListBody(subresourceExecEvent))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Zero(t, recorder.len(), "pods/exec must not be recorded")

	exec, ok := telemetry.CollectInt64Sum(reader, auditEventsMetric, map[string]string{
		"outcome": "non_scale_subresource", "category": "dropped",
		"resource": "pods", "verb": "create",
	})
	require.True(t, ok, "expected a non_scale_subresource outcome sample for pods/exec")
	assert.Equal(t, int64(1), exec)
}

// A request refused at the door must still be counted.
//
// This was the ingress gap: method, path and route rejections returned BEFORE any instrument was
// touched, so an apiserver posting audit to the wrong path — or under a route no ClusterProvider
// claims, which is the likeliest audit misconfiguration there is — produced exactly the same
// ingress metrics as an apiserver posting nothing at all.
func TestServeHTTP_RejectedRequestsAreCounted(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		path        string
		wantOutcome string
		wantStatus  int
	}{
		{
			name:        "wrong method",
			method:      http.MethodGet,
			path:        defaultRoute,
			wantOutcome: outcomeBadMethod,
			wantStatus:  http.StatusMethodNotAllowed,
		},
		{
			name:        "wrong path",
			method:      http.MethodPost,
			path:        "/not-the-audit-webhook",
			wantOutcome: outcomeBadPath,
			wantStatus:  http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader, err := telemetry.InitTestExporter()
			require.NoError(t, err)

			handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{FactPublisher: &fakeFactSink{}}))
			require.NoError(t, err)

			w := serveBody(t, handler, tt.method, tt.path, eventListBody(acceptedCreateEvent))
			assert.Equal(t, tt.wantStatus, w.Code)

			count, ok := telemetry.CollectHistogramCount(reader, eventListDurationMetric,
				map[string]string{"outcome": tt.wantOutcome})
			require.True(t, ok, "a refused request must still reach the ingress metric")
			assert.Equal(t, uint64(1), count)
		})
	}
}
