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
	eventListMetric         = "gitopsreverser_audit_eventlists_total"
	eventListEventsMetric   = "gitopsreverser_audit_eventlist_events_total"
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

// TestServeHTTP_EventListIngressMetrics asserts the three EventList-boundary
// metrics across the four outcome labels. The event-item counter has no sample
// for decode_error, since the item count is only known after a successful decode.
func TestServeHTTP_EventListIngressMetrics(t *testing.T) {
	tests := []struct {
		name            string
		body            string
		recorderErr     bool
		wantOutcome     string
		wantStatus      int
		wantEventSample bool
		wantEventCount  int64
	}{
		{
			name:            "processed",
			body:            eventListBody(acceptedCreateEvent),
			wantOutcome:     "processed",
			wantStatus:      http.StatusOK,
			wantEventSample: true,
			wantEventCount:  1,
		},
		{
			name:            "empty event list",
			body:            eventListBody(),
			wantOutcome:     "empty",
			wantStatus:      http.StatusOK,
			wantEventSample: true,
			wantEventCount:  0,
		},
		{
			name:            "decode error",
			body:            "not json",
			wantOutcome:     "decode_error",
			wantStatus:      http.StatusBadRequest,
			wantEventSample: false,
		},
		{
			name:            "process error",
			body:            eventListBody(processErrorEvent),
			recorderErr:     true,
			wantOutcome:     "process_error",
			wantStatus:      http.StatusInternalServerError,
			wantEventSample: true,
			wantEventCount:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader, err := telemetry.InitTestExporter()
			require.NoError(t, err)

			recorder := &fakeFactRecorder{}
			if tt.recorderErr {
				recorder.err = errAuditTest
			}
			handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{FactRecorder: recorder}))
			require.NoError(t, err)

			w := serveBody(t, handler, http.MethodPost, defaultRoute, tt.body)
			assert.Equal(t, tt.wantStatus, w.Code)

			match := map[string]string{"outcome": tt.wantOutcome}

			requests, ok := telemetry.CollectInt64Sum(reader, eventListMetric, match)
			require.True(t, ok, "%s should have a sample for %v", eventListMetric, match)
			assert.Equal(t, int64(1), requests)

			durCount, ok := telemetry.CollectHistogramCount(reader, eventListDurationMetric, match)
			require.True(t, ok, "%s should have a sample for %v", eventListDurationMetric, match)
			assert.Equal(t, uint64(1), durCount)

			events, ok := telemetry.CollectInt64Sum(reader, eventListEventsMetric, match)
			if tt.wantEventSample {
				require.True(t, ok, "%s should have a sample for %v", eventListEventsMetric, match)
				assert.Equal(t, tt.wantEventCount, events)
			} else {
				assert.False(t, ok, "decode_error must not produce an event-item sample")
			}
		})
	}
}

// TestServeHTTP_AcceptedEventQueuedOutcome confirms an accepted event records the
// "queued" census outcome on audit_events_total (category="stored").
func TestServeHTTP_AcceptedEventQueuedOutcome(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	recorder := &fakeFactRecorder{}
	handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{FactRecorder: recorder}))
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

			recorder := &fakeFactRecorder{}
			handler, err := NewAuditHandler(AuditHandlerConfig{
				FactRecorder:            recorder,
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

	recorder := &fakeFactRecorder{}
	handler, err := NewAuditHandler(routedConfig(AuditHandlerConfig{FactRecorder: recorder}))
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
