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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

const (
	eventListMetric         = "gitopsreverser_audit_eventlists_total"
	eventListEventsMetric   = "gitopsreverser_audit_eventlist_events_total"
	eventListDurationMetric = "gitopsreverser_audit_eventlist_duration_seconds"
)

// validCreateEventList is a one-item official EventList that decodes and
// processes cleanly.
const validCreateEventList = `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[` +
	`{"kind":"Event","level":"RequestResponse","auditID":"metric-test-1","stage":"ResponseComplete",` +
	`"verb":"create","user":{"username":"test-user"},` +
	`"objectRef":{"resource":"configmaps","namespace":"default","name":"cm","apiVersion":"v1"},` +
	`"responseStatus":{"code":200},` +
	`"responseObject":{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm","namespace":"default"}}}]}`

// emptyEventList decodes to zero items.
const emptyEventList = `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[]}`

// processErrorEventList decodes but fails processing: an event with an empty
// auditID is rejected by checkEvent.
const processErrorEventList = `{"kind":"EventList","apiVersion":"audit.k8s.io/v1","items":[` +
	`{"kind":"Event","auditID":"","verb":"create","user":{"username":"u"},` +
	`"objectRef":{"resource":"configmaps","apiVersion":"v1"}}]}`

func TestServeHTTP_EventListIngressMetrics(t *testing.T) {
	tests := []struct {
		name            string
		path            string
		body            string
		source          string
		wantOutcome     string
		wantStatus      int
		wantEventSample bool
		wantEventCount  int64
	}{
		{
			name:            "official processed",
			path:            "/audit-webhook",
			body:            validCreateEventList,
			source:          "official",
			wantOutcome:     "processed",
			wantStatus:      http.StatusOK,
			wantEventSample: true,
			wantEventCount:  1,
		},
		{
			name:            "additional processed",
			path:            "/audit-webhook-additional",
			body:            validCreateEventList,
			source:          "additional",
			wantOutcome:     "processed",
			wantStatus:      http.StatusOK,
			wantEventSample: true,
			wantEventCount:  1,
		},
		{
			name:            "empty event list",
			path:            "/audit-webhook",
			body:            emptyEventList,
			source:          "official",
			wantOutcome:     "empty",
			wantStatus:      http.StatusOK,
			wantEventSample: true,
			wantEventCount:  0,
		},
		{
			name:            "decode error",
			path:            "/audit-webhook",
			body:            "not json",
			source:          "official",
			wantOutcome:     "decode_error",
			wantStatus:      http.StatusBadRequest,
			wantEventSample: false,
		},
		{
			name:            "process error",
			path:            "/audit-webhook",
			body:            processErrorEventList,
			source:          "official",
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

			handler, err := NewAuditHandler(AuditHandlerConfig{})
			require.NoError(t, err)

			req := httptest.NewRequest(http.MethodPost, tt.path, bytes.NewReader([]byte(tt.body)))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			assert.Equal(t, tt.wantStatus, w.Code)

			match := map[string]string{"source": tt.source, "outcome": tt.wantOutcome}

			requests, ok := telemetry.CollectInt64Sum(reader, eventListMetric, match)
			require.True(t, ok, "audit_eventlists_total should have a sample for %v", match)
			assert.Equal(t, int64(1), requests)

			durCount, ok := telemetry.CollectHistogramCount(reader, eventListDurationMetric, match)
			require.True(t, ok, "audit_eventlist_duration_seconds should have a sample for %v", match)
			assert.Equal(t, uint64(1), durCount)

			events, ok := telemetry.CollectInt64Sum(reader, eventListEventsMetric, match)
			if tt.wantEventSample {
				require.True(t, ok, "audit_eventlist_events_total should have a sample for %v", match)
				assert.Equal(t, tt.wantEventCount, events)
			} else {
				assert.False(t, ok, "decode_error must not produce an event-item sample")
			}
		})
	}
}
