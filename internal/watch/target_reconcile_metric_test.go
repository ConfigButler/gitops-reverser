// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"errors"
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

const watchRecoveryMetric = "gitopsreverser_watch_recovery_total"

const resyncFailuresMetric = "gitopsreverser_git_resync_failures_total"

// recordBackgroundResyncFailure must count a fire-and-forget resync that failed at the
// worker, labelled per GitTarget, so a silently-recovered failure is observable.
func TestRecordBackgroundResyncFailure_IncrementsPerGitTarget(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	r := &EventRouter{Log: logr.Discard()}
	gitDest := types.NewResourceReference("my-target", "my-ns")

	r.recordBackgroundResyncFailure(gitDest, resyncFailureError)
	r.recordBackgroundResyncFailure(gitDest, resyncFailureError)

	value, ok := telemetry.CollectInt64Sum(reader, resyncFailuresMetric,
		map[string]string{"gittarget_namespace": "my-ns", "gittarget_name": "my-target", "reason": "error"})
	require.True(t, ok, "expected a git_resync_failures_total sample")
	assert.Equal(t, int64(2), value)
}

// The reason separates a snapshot that did not land from two that are counted elsewhere too: a
// resync refused at intake (git_queue_refusals_total) is gathered again, and a deferred one
// (git_materialization_failures_total) stays pending at the worker.
func TestResyncFailureReason_SeparatesRefusedAndDeferredFromFailed(t *testing.T) {
	assert.Equal(t, resyncFailureRefused, resyncFailureReason(git.ErrFinalizeQueueFull))
	assert.Equal(t, resyncFailureRefused,
		resyncFailureReason(fmt.Errorf("%w: the branch has paused intake", git.ErrFinalizeQueueFull)))
	deferred := fmt.Errorf("%w: refresh remote before resync: %w", git.ErrResyncDeferred, errors.New("dial"))
	assert.Equal(t, resyncFailureDeferred, resyncFailureReason(deferred))
	assert.Equal(t, resyncFailureError, resyncFailureReason(errors.New("write failed")))
}

// recordWatchRecovery must increment the per-GitTarget counter and carry the gittarget_*,
// group/resource and mode labels.
//
// The mode label is the point of the rename: the counter used to be target_reconcile_completed_total
// with a `trigger` label whose documented value (`rule_change`) the code never emitted. It names
// which recovery path ran, and the two the code actually takes are cursor_resume and type_reconcile.
func TestRecordWatchRecovery_IncrementsPerMode(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	m := &Manager{Log: logr.Discard()}
	gitDest := types.NewResourceReference("my-target", "my-ns")
	match := func(mode string) map[string]string {
		return map[string]string{
			"gittarget_namespace": "my-ns", "gittarget_name": "my-target",
			"group": "", "resource": "configmaps", "mode": mode,
		}
	}

	m.recordWatchRecovery(gitDest, "", "configmaps", recoveryModeCursorResume)

	value, ok := telemetry.CollectInt64Sum(reader, watchRecoveryMetric, match(recoveryModeCursorResume))
	require.True(t, ok, "expected a watch_recovery_total sample")
	assert.Equal(t, int64(1), value)

	// A second recovery increments the counter, proving a delta over a baseline distinguishes a
	// fresh recovery from a stale latched value — the property the restart-reconcile e2e gate reads.
	m.recordWatchRecovery(gitDest, "", "configmaps", recoveryModeCursorResume)
	value, ok = telemetry.CollectInt64Sum(reader, watchRecoveryMetric, match(recoveryModeCursorResume))
	require.True(t, ok)
	assert.Equal(t, int64(2), value)

	// A different mode is a distinct series, not folded into the first.
	m.recordWatchRecovery(gitDest, "", "configmaps", recoveryModeTypeReconcile)
	value, ok = telemetry.CollectInt64Sum(reader, watchRecoveryMetric, match(recoveryModeTypeReconcile))
	require.True(t, ok)
	assert.Equal(t, int64(1), value)
}
