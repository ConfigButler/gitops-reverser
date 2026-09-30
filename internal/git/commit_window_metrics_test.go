// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// These tests pin git_commit_windows_total and git_commit_window_duration_seconds: every closed
// window is counted exactly once, at the start of its finalize, with what closed it and whose
// timers were in effect, and its collection time runs from the write that opened it.

const (
	commitWindowsMetric        = "gitopsreverser_git_commit_windows_total"
	commitWindowDurationMetric = "gitopsreverser_git_commit_window_duration_seconds"
)

// windowsClosed reads the window counter for one close reason and timer source on team-a.
func windowsClosed(t *testing.T, reader *sdkmetric.ManualReader, reason, source string) int64 {
	t.Helper()
	value, _ := telemetry.CollectInt64Sum(reader, commitWindowsMetric, map[string]string{
		"gittarget_namespace": "default",
		"gittarget_name":      "team-a",
		"close_reason":        reason,
		"timer_source":        source,
	})
	return value
}

// requireOneObservationPerWindow asserts the invariant the two instruments share: the histogram's
// count equals the counter summed over every close reason and timer source.
func requireOneObservationPerWindow(t *testing.T, reader *sdkmetric.ManualReader, windows int64) {
	t.Helper()
	target := map[string]string{"gittarget_namespace": "default", "gittarget_name": "team-a"}
	counted, _ := telemetry.CollectInt64Sum(reader, commitWindowsMetric, target)
	observed, _ := telemetry.CollectHistogramCount(reader, commitWindowDurationMetric, target)
	require.Equal(t, windows, counted, "windows counted")
	require.Equal(t, uint64(windows), observed, "one duration observation per counted window")
}

func newMetricsLoop(t *testing.T, idle time.Duration) (*BranchWorker, *branchWorkerEventLoop) {
	t.Helper()
	worker, _, _ := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, idle)
	t.Cleanup(loop.stopTimers)
	return worker, loop
}

func TestCommitWindowMetrics_AZeroTimerClosesAndCountsTheWindowAtOnce(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	_, loop := newMetricsLoop(t, 0) // the target's idleTimeout is 0s

	writeTo(loop, "first")

	assert.Nil(t, loop.openWindow)
	assert.Equal(t, int64(1), windowsClosed(t, reader, "idle_timeout", windowTimerSourceTarget))
	requireOneObservationPerWindow(t, reader, 1)
}

func TestCommitWindowMetrics_EachCloseReasonIsCountedUnderItsOwnName(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	_, loop := newMetricsLoop(t, time.Hour)
	loop.lastPushAt = time.Now()

	// max_duration: the target's maximum runs out.
	writeTo(loop, "a")
	loop.openWindow.timers.maxAt = time.Now().Add(-time.Millisecond)
	loop.closeOrArmWindow()

	// identity_change: another author's write arrives while alice's window is open.
	writeTo(loop, "b")
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapTargetEvent("c", "bob", "team-a")},
		CommitMode: CommitModePerEvent,
	}})

	// shutdown: bob's window is open when the worker stops.
	loop.handleShutdown()

	assert.Equal(t, int64(1), windowsClosed(t, reader, "max_duration", windowTimerSourceTarget))
	assert.Equal(t, int64(1), windowsClosed(t, reader, "identity_change", windowTimerSourceTarget))
	assert.Equal(t, int64(1), windowsClosed(t, reader, "shutdown", windowTimerSourceTarget))
	requireOneObservationPerWindow(t, reader, 3)
}

// TestCommitWindowMetrics_AnAttachKeepsTheOpeningTimeAndCountsTheRequestsTimers pins the two
// things an attach must not do: move the moment the window opened, which the attach DOES reset for
// the idle interval, and leave the window counted under the target's timers after it replaced them.
func TestCommitWindowMetrics_AnAttachKeepsTheOpeningTimeAndCountsTheRequestsTimers(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	_, loop := newMetricsLoop(t, time.Hour)
	loop.lastPushAt = time.Now()

	writeTo(loop, "first")
	opened := time.Now().Add(-10 * time.Second) // as if the write had arrived ten seconds ago
	loop.openWindow.openedAt = opened

	serviceAttach(loop, attachReq("alice", time.Hour))
	require.NotNil(t, loop.openWindow.pendingCR)
	assert.Equal(t, opened, loop.openWindow.openedAt, "the attach restarts the timers, not the opening time")
	forceDue(loop)

	assert.Equal(t, int64(1), windowsClosed(t, reader, "max_duration", windowTimerSourceCommitRequest))
	assert.Zero(t, windowsClosed(t, reader, "max_duration", windowTimerSourceTarget))
	requireOneObservationPerWindow(t, reader, 1)
	sum, _ := telemetry.CollectHistogramSum(reader, commitWindowDurationMetric,
		map[string]string{"gittarget_name": "team-a"})
	assert.GreaterOrEqual(t, sum, 10.0, "collection time runs from the write that opened the window")
}

func TestCommitWindowMetrics_AttachNextCountsTheWindowItCloses(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	_, loop := newMetricsLoop(t, time.Hour)
	loop.lastPushAt = time.Now()

	writeTo(loop, "earlier")
	next := attachReq("alice", time.Hour)
	next.Attach = "Next"
	serviceAttach(loop, next)

	assert.Equal(t, int64(1), windowsClosed(t, reader, "attach_next", windowTimerSourceTarget))
	requireOneObservationPerWindow(t, reader, 1)
}

// TestCommitWindowMetrics_WindowsThatCommitNothingAreCountedToo pins that the count is of windows,
// not of commits: one whose writes already matched Git, and one whose finalize fails, are both in
// the distribution. Counting only the windows that became commits would hide exactly the ones an
// operator is looking for.
func TestCommitWindowMetrics_WindowsThatCommitNothingAreCountedToo(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	worker, loop := newMetricsLoop(t, time.Hour)

	writeTo(loop, "present")
	require.True(t, loop.finalizeOpenWindow())
	loop.pushPending()

	// The same object again: a window with no diff.
	writeTo(loop, "present")
	require.True(t, loop.finalizeOpenWindow())

	// A window whose finalize is refused: the render fidelity gate closes while it is open.
	writeTo(loop, "blocked")
	gate := NewRenderFidelityGate()
	restartAll(gate, types.NewResourceReference("team-a", "default"), fidelityScope("", "configmaps"))
	worker.renderFidelityGate = gate
	require.False(t, loop.finalizeOpenWindow(), "the window is dropped, not committed")

	assert.Equal(t, int64(3), windowsClosed(t, reader, "unspecified", windowTimerSourceTarget))
	requireOneObservationPerWindow(t, reader, 3)
}

// TestCommitWindowMetrics_APushReplayClosesNoWindow pins that rebuilding commits is not collecting:
// a push that replays its writes onto a moved remote re-runs their commits and observes nothing.
func TestCommitWindowMetrics_APushReplayClosesNoWindow(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	worker, serverRepo, remoteURL := setupCommitPushSplitWorker(t)
	createPlainGitTarget(t, worker, "team-a", "team-a")
	loop := newBranchWorkerEventLoop(worker, time.Hour)
	loop.lastPushAt = time.Now()
	defer loop.stopTimers()

	writeTo(loop, "held")
	require.True(t, loop.finalizeOpenWindow())
	before, err := serverRepo.Reference("refs/heads/main", true)
	require.NoError(t, err)

	pushCompetingCommit(t, remoteURL)
	loop.pushPending()
	require.Empty(t, loop.pendingWrites, "the replayed push went through")
	after, err := serverRepo.Reference("refs/heads/main", true)
	require.NoError(t, err)
	require.NotEqual(t, before.Hash(), after.Hash())

	requireOneObservationPerWindow(t, reader, 1)
}

// TestCommitWindowMetrics_ACommitEmptyRecordIsNotAWindow pins that the empty commit a CommitEmpty
// request records when no window reached it is not counted: nothing collected, so there is no
// window to time.
func TestCommitWindowMetrics_ACommitEmptyRecordIsNotAWindow(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)
	_, loop := newMetricsLoop(t, time.Hour)
	loop.lastPushAt = time.Now() // hold the push, so the record can be seen

	serviceAttach(loop, commitEmptyReq("alice", "save: nothing changed"))
	forceDue(loop)
	loop.serviceCommitRequests()
	require.Len(t, loop.pendingWrites, 1, "the record was made")

	_, counted := telemetry.CollectInt64Sum(reader, commitWindowsMetric, map[string]string{})
	assert.False(t, counted, "no window was closed")
}
