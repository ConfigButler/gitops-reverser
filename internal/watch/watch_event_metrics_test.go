// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

const watchEventsMetric = "gitopsreverser_watch_events_total"

func ingestMatch(outcome string) map[string]string {
	return map[string]string{
		"gittarget_namespace": "my-ns", "gittarget_name": "my-target",
		"group": "", "version": "v1", "resource": "configmaps", "outcome": outcome,
	}
}

// The ingest census must name the GitTarget and the type, because the question it answers is
// "which tenant stopped receiving events, for which type".
func TestRecordWatchEvent_CountsPerTargetTypeAndOutcome(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	gitDest := types.NewResourceReference("my-target", "my-ns")
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

	recordWatchEvent(t.Context(), gitDest, gvr, watchOutcomeRouted)
	recordWatchEvent(t.Context(), gitDest, gvr, watchOutcomeRouted)
	recordWatchEvent(t.Context(), gitDest, gvr, watchOutcomeUnchanged)

	routed, ok := telemetry.CollectInt64Sum(reader, watchEventsMetric, ingestMatch(watchOutcomeRouted))
	require.True(t, ok)
	assert.Equal(t, int64(2), routed)

	unchanged, ok := telemetry.CollectInt64Sum(reader, watchEventsMetric, ingestMatch(watchOutcomeUnchanged))
	require.True(t, ok)
	assert.Equal(t, int64(1), unchanged)
}

// A filtered event and a lost one must be different series, because they mean opposite things: one
// is the rules working, the other is an observed change that never reached Git. Folding them
// together is exactly what made "nothing is happening" and "delivery is failing" look alike.
func TestRecordWatchEvent_LossIsDistinctFromFiltering(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	gitDest := types.NewResourceReference("my-target", "my-ns")
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

	recordWatchEvent(t.Context(), gitDest, gvr, watchOutcomeOperationFiltered)
	recordWatchEvent(t.Context(), gitDest, gvr, watchOutcomeRouteFailed)

	filtered, ok := telemetry.CollectInt64Sum(reader, watchEventsMetric,
		ingestMatch(watchOutcomeOperationFiltered))
	require.True(t, ok)
	assert.Equal(t, int64(1), filtered)

	lost, ok := telemetry.CollectInt64Sum(reader, watchEventsMetric, ingestMatch(watchOutcomeRouteFailed))
	require.True(t, ok)
	assert.Equal(t, int64(1), lost)
}

// `expired` is the reason worth separating: the cursor fell out of watch history, so the next
// session cannot resume and must replay the whole type. A routine plan teardown must not read the
// same way.
func TestSessionEndReason_SeparatesExpiryFromTeardown(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	assert.Equal(t, sessionEndedStopped, sessionEndReason(cancelled, errors.New("any")),
		"a cancelled context is a teardown however the session returned")
	assert.Equal(t, sessionEndedExpired, sessionEndReason(context.Background(), errTargetWatchExpired))
	assert.Equal(t, sessionEndedError, sessionEndReason(context.Background(), errors.New("boom")))
	assert.Equal(t, sessionEndedStopped, sessionEndReason(context.Background(), nil))
}

// The recovery counter carries group/resource and no version: a recovery covers a CELL, and the
// per-type reconcile path does not know a served version. An empty version label on one arm beside
// a populated one on another would be worse than no label at all.
func TestRecordWatchRecovery_CarriesNoVersionLabel(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	m := &Manager{}
	m.recordWatchRecovery(types.NewResourceReference("my-target", "my-ns"), "", "configmaps",
		recoveryModeCursorResume)

	_, ok := telemetry.CollectInt64Sum(reader, "gitopsreverser_watch_recovery_total",
		map[string]string{"version": "v1"})
	assert.False(t, ok, "the recovery counter must not carry a version label")
}
