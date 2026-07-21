// SPDX-License-Identifier: Apache-2.0

package outcome

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	auditv1 "k8s.io/apiserver/pkg/apis/audit/v1"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

const auditEventsMetric = "gitopsreverser_audit_events_total"

func TestOutcomeCategory(t *testing.T) {
	cases := map[Outcome]Category{
		Queued:                   Stored,
		Parked:                   Held,
		NotNeeded:                Dropped,
		NilEvent:                 Dropped,
		Stage:                    Dropped,
		ReadOnlyOrUnknownVerb:    Dropped,
		FailedRequest:            Dropped,
		DryRun:                   Dropped,
		UnchangedResourceVersion: Dropped,
		MalformedAdditional:      Dropped,
		NonScaleSubresource:      Dropped,
		ShallowDropped:           Dropped,
		RVLessEmptyHighWater:     Dropped,
		OlderThanHighWater:       Dropped,
		NonNumericRV:             Dropped,
		MissingClusterAnnotation: Dropped,
		WriteError:               Error,
	}
	for o, want := range cases {
		assert.Equalf(t, want, o.Category(), "category of %s", o)
	}

	// An unknown/unmapped outcome is treated as Error so a new outcome added without a
	// category mapping trips the category="error" invariant rather than passing silently.
	assert.Equal(t, Error, Outcome("mystery").Category())
}

func TestRecordNilGuard(t *testing.T) {
	// With telemetry uninitialized (AuditEventsTotal == nil) Record is a no-op and must not panic.
	// This test must stay ahead of the tests below: InitTestExporter wires the global
	// instrument permanently, so the nil branch is only reachable before the first call.
	assert.NotPanics(t, func() {
		Record(context.Background(), nil, Queued)
	})
}

// TestGvrParts_LabelValues pins how an objectRef is split into the group/version/resource
// labels. These land on a counter, so a mis-split silently forks one logical series into
// two (or merges two unrelated ones) and every audit dashboard/alert keyed on them lies.
func TestGvrParts_LabelValues(t *testing.T) {
	tests := []struct {
		name        string
		ref         *auditv1.ObjectReference
		wantGroup   string
		wantVersion string
		wantResrc   string
	}{
		{
			name:        "grouped apiVersion splits on the slash",
			ref:         &auditv1.ObjectReference{APIGroup: "apps", APIVersion: "apps/v1", Resource: "deployments"},
			wantGroup:   "apps",
			wantVersion: "v1",
			wantResrc:   "deployments",
		},
		{
			// Core-group events carry a bare "v1", so the group label is the (empty) APIGroup
			// rather than "unknown" — core resources must not be bucketed with malformed ones.
			name:        "bare apiVersion falls back to APIGroup for the group label",
			ref:         &auditv1.ObjectReference{APIGroup: "", APIVersion: "v1", Resource: "configmaps"},
			wantGroup:   "",
			wantVersion: "v1",
			wantResrc:   "configmaps",
		},
		{
			// APIVersion is authoritative when it carries a group: a disagreeing APIGroup must
			// not win, or the same GVR would be counted under two different group labels.
			name:        "slashed apiVersion wins over a disagreeing APIGroup",
			ref:         &auditv1.ObjectReference{APIGroup: "other", APIVersion: "apps/v1", Resource: "deployments"},
			wantGroup:   "apps",
			wantVersion: "v1",
			wantResrc:   "deployments",
		},
		{
			// An empty APIVersion means no usable identity at all, so the group/resource that
			// *are* present are deliberately discarded — a partial series would be misleading.
			name:        "empty apiVersion discards the rest of the objectRef",
			ref:         &auditv1.ObjectReference{APIGroup: "apps", APIVersion: "", Resource: "deployments"},
			wantGroup:   "unknown",
			wantVersion: "unknown",
			wantResrc:   "unknown",
		},
		{
			name:        "missing resource is labelled unknown",
			ref:         &auditv1.ObjectReference{APIGroup: "apps", APIVersion: "apps/v1", Resource: ""},
			wantGroup:   "apps",
			wantVersion: "v1",
			wantResrc:   "unknown",
		},
		{
			// Cut takes only the first separator, so anything past it stays in the version.
			name:        "extra slashes stay in the version",
			ref:         &auditv1.ObjectReference{APIVersion: "a/b/c", Resource: "things"},
			wantGroup:   "a",
			wantVersion: "b/c",
			wantResrc:   "things",
		},
		{
			name:        "leading slash yields an empty group",
			ref:         &auditv1.ObjectReference{APIGroup: "apps", APIVersion: "/v1", Resource: "things"},
			wantGroup:   "",
			wantVersion: "v1",
			wantResrc:   "things",
		},
		{
			name:        "trailing slash yields an empty version",
			ref:         &auditv1.ObjectReference{APIVersion: "apps/", Resource: "things"},
			wantGroup:   "apps",
			wantVersion: "",
			wantResrc:   "things",
		},
		{
			name:        "nil objectRef",
			ref:         nil,
			wantGroup:   "unknown",
			wantVersion: "unknown",
			wantResrc:   "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			group, version, resource := gvrParts(&auditv1.Event{ObjectRef: tt.ref})
			assert.Equal(t, tt.wantGroup, group, "group")
			assert.Equal(t, tt.wantVersion, version, "version")
			assert.Equal(t, tt.wantResrc, resource, "resource")
		})
	}

	// A nil event is defensive: Record is called from paths that terminate on a nil event
	// (the NilEvent outcome), and it must still produce a full, bounded label set.
	group, version, resource := gvrParts(nil)
	assert.Equal(t, "unknown", group)
	assert.Equal(t, "unknown", version)
	assert.Equal(t, "unknown", resource)
}

// TestVerb_NilEventAndPassthrough pins the verb label. Unlike gvrParts it has no "unknown"
// sentinel: a nil event yields an empty label, which is the value dashboards must expect.
func TestVerb_NilEventAndPassthrough(t *testing.T) {
	assert.Empty(t, verb(nil), "nil event must not panic and yields an empty verb label")
	assert.Empty(t, verb(&auditv1.Event{}), "an unset verb is passed through as empty, not unknown")
	assert.Equal(t, "create", verb(&auditv1.Event{Verb: "create"}))
}

// TestRecord_EmitsLabelledSample covers the recording branch of Record (telemetry
// initialized) and pins the six labels it stamps, including that category is derived from
// the outcome rather than passed in — an alert on category="error" depends on that.
func TestRecord_EmitsLabelledSample(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	ctx := context.Background()
	Record(ctx, &auditv1.Event{
		Verb:      "update",
		ObjectRef: &auditv1.ObjectReference{APIGroup: "apps", APIVersion: "apps/v1", Resource: "deployments"},
	}, WriteError)

	count, ok := telemetry.CollectInt64Sum(reader, auditEventsMetric, map[string]string{
		"outcome":  string(WriteError),
		"category": string(Error),
		"group":    "apps",
		"version":  "v1",
		"resource": "deployments",
		"verb":     "update",
	})
	require.True(t, ok, "expected a sample for the recorded write_error outcome")
	assert.Equal(t, int64(1), count)
}

// TestRecord_NilEventStillRecords guards the defensive path: the NilEvent outcome is
// recorded with no event at all, and must still emit one bounded, fully-labelled sample
// rather than being dropped from the census.
func TestRecord_NilEventStillRecords(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	Record(context.Background(), nil, NilEvent)

	count, ok := telemetry.CollectInt64Sum(reader, auditEventsMetric, map[string]string{
		"outcome":  string(NilEvent),
		"category": string(Dropped),
		"group":    "unknown",
		"version":  "unknown",
		"resource": "unknown",
		"verb":     "",
	})
	require.True(t, ok, "expected a sample for the nil-event outcome")
	assert.Equal(t, int64(1), count)
}
