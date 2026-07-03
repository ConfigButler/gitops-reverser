// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

const targetReconcileCompletedMetric = "gitopsreverser_target_reconcile_completed_total"

const resyncBackgroundFailuresMetric = "gitopsreverser_resync_background_failures_total"

// recordBackgroundResyncFailure must count a fire-and-forget resync that failed at the
// worker, labelled per GitTarget, so a silently-recovered failure is observable.
func TestRecordBackgroundResyncFailure_IncrementsPerGitTarget(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	r := &EventRouter{Log: logr.Discard()}
	gitDest := types.NewResourceReference("my-target", "my-ns")

	r.recordBackgroundResyncFailure(gitDest)
	r.recordBackgroundResyncFailure(gitDest)

	value, ok := telemetry.CollectInt64Sum(reader, resyncBackgroundFailuresMetric,
		map[string]string{"gittarget_namespace": "my-ns", "gittarget_name": "my-target"})
	require.True(t, ok, "expected a resync_background_failures_total sample")
	assert.Equal(t, int64(2), value)
}

// recordTargetReconcileCompleted must increment the per-GitTarget counter and
// carry the gittarget_* and trigger labels.
func TestRecordTargetReconcileCompleted_IncrementsPerTrigger(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	m := &Manager{Log: logr.Discard()}
	gitDest := types.NewResourceReference("my-target", "my-ns")

	m.recordTargetReconcileCompleted(gitDest, "rule_change")

	value, ok := telemetry.CollectInt64Sum(reader, targetReconcileCompletedMetric,
		map[string]string{"gittarget_namespace": "my-ns", "gittarget_name": "my-target", "trigger": "rule_change"})
	require.True(t, ok, "expected a target_reconcile_completed_total sample")
	assert.Equal(t, int64(1), value)

	// A second pass increments the counter, proving a delta over a baseline
	// distinguishes a fresh reconcile from a stale latched value.
	m.recordTargetReconcileCompleted(gitDest, "rule_change")
	value, ok = telemetry.CollectInt64Sum(reader, targetReconcileCompletedMetric,
		map[string]string{"gittarget_namespace": "my-ns", "gittarget_name": "my-target", "trigger": "rule_change"})
	require.True(t, ok)
	assert.Equal(t, int64(2), value)

	// A different trigger is a distinct series, not folded into the first.
	m.recordTargetReconcileCompleted(gitDest, "startup_replay")
	value, ok = telemetry.CollectInt64Sum(reader, targetReconcileCompletedMetric,
		map[string]string{"gittarget_namespace": "my-ns", "gittarget_name": "my-target", "trigger": "startup_replay"})
	require.True(t, ok)
	assert.Equal(t, int64(1), value)
}
