// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

const (
	commitsTotalMetric = "gitopsreverser_git_commits_total"
	queueDepthMetric   = "gitopsreverser_git_queue_depth"
	queueDropsMetric   = "gitopsreverser_git_queue_drops_total"
	pushesTotalMetric  = "gitopsreverser_git_pushes_total"
	pushRetriesMetric  = "gitopsreverser_git_push_retries_total"
	pushDurationMetric = "gitopsreverser_git_push_duration_seconds"
)

func newMetricsTestWorker() *BranchWorker {
	return &BranchWorker{
		GitProviderRef:       "test-provider",
		GitProviderNamespace: "test-ns",
		Branch:               "main",
		Log:                  logr.Discard(),
		ctx:                  context.Background(),
		contentWriter:        newContentWriter(itypes.SensitiveResourcePolicy{}),
		eventQueue:           make(chan WorkItem, branchWorkerQueueSize),
		branchBufferMaxBytes: DefaultBranchBufferMaxBytes,
	}
}

func workerLabels() map[string]string {
	return map[string]string{
		"provider_namespace": "test-ns",
		"provider_name":      "test-provider",
		"branch":             "main",
	}
}

// commitAndPush models one full cycle: commits are created locally (tallied) and then land on the
// remote (flushed). Every commits_total assertion goes through it, because that IS the contract
// now — a commit is counted when it reaches the remote, never when it is created.
func commitAndPush(w *BranchWorker, writes []PendingWrite, commitsCreated int) {
	w.recordPendingWritesMetrics(writes, commitsCreated)
	w.publishCommittedTally()
}

// commits_total must be labelled with the recording worker's identity
// {provider_namespace, provider_name, branch}. Without labels the counter is a single global
// series; with them, concurrent workers (and parallel e2e specs, isolated by their per-suite
// GitProvider namespace) count separately.
func TestRecordPendingWritesMetrics_LabelsCommitsByWorkerIdentity(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	w := newMetricsTestWorker()
	commitAndPush(w, nil, 2)

	commits, ok := telemetry.CollectInt64Sum(reader, commitsTotalMetric, workerLabels())
	require.True(t, ok, "expected a commits_total sample labelled by the worker identity")
	assert.Equal(t, int64(2), commits)
}

// A query scoped to a different provider_namespace must not see this worker's
// commits — this is the property that lets parallel e2e specs each assert on only
// their own commits (each suite mints its GitProvider in a unique namespace).
func TestRecordPendingWritesMetrics_CommitsIsolatedByProviderNamespace(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	w := newMetricsTestWorker()
	commitAndPush(w, nil, 1)

	otherNamespace := workerLabels()
	otherNamespace["provider_namespace"] = "other-suite-ns"
	_, ok := telemetry.CollectInt64Sum(reader, commitsTotalMetric, otherNamespace)
	assert.False(t, ok, "a different provider_namespace must not match this worker's commits")
}

func TestRecordPendingWritesMetrics_LabelsCommitsByAuthorKind(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	w := newMetricsTestWorker()
	commitAndPush(w, []PendingWrite{
		{
			Kind:      PendingWriteCommit,
			Events:    []Event{{UserInfo: UserInfo{Username: "alice"}}},
			CommitSHA: plumbing.NewHash("1111111111111111111111111111111111111111"),
		},
		{
			Kind: PendingWriteCommit,
			Events: []Event{{
				UserInfo: UserInfo{Username: "system:serviceaccount:flux-system:kustomize-controller"},
			}},
			CommitSHA: plumbing.NewHash("2222222222222222222222222222222222222222"),
		},
		{
			Kind:      PendingWriteResync,
			Committed: boolPtr(true),
		},
	}, 3)

	labels := workerLabels()
	labels["author_kind"] = authorKindUser
	count, ok := telemetry.CollectInt64Sum(reader, commitsTotalMetric, labels)
	require.True(t, ok)
	assert.Equal(t, int64(1), count)

	labels["author_kind"] = authorKindServiceAccount
	count, ok = telemetry.CollectInt64Sum(reader, commitsTotalMetric, labels)
	require.True(t, ok)
	assert.Equal(t, int64(1), count)

	labels["author_kind"] = authorKindCommitter
	count, ok = telemetry.CollectInt64Sum(reader, commitsTotalMetric, labels)
	require.True(t, ok)
	assert.Equal(t, int64(1), count)
}

func boolPtr(v bool) *bool {
	return &v
}

// queueDepth must report 0 for a freshly drained worker: empty queue and no retained unpushed
// work.
func TestQueueDepth_DrainedReportsZero(t *testing.T) {
	assert.Equal(t, int64(0), newMetricsTestWorker().queueDepth())
}

// queueDepth must count both accepted-but-unhandled items (in flight) and the retained
// unpushed-work flag, so it reflects work the channel length alone cannot see — including an item
// already dequeued and being processed.
func TestQueueDepth_CountsInflightAndRetained(t *testing.T) {
	w := newMetricsTestWorker()
	w.inflightItems.Store(2)
	w.hasUnpushedWork.Store(true)

	assert.Equal(t, int64(3), w.queueDepth())
}

// queueDepth must still report > 0 for an item dequeued from the channel but still being handled —
// the exact window where len(eventQueue) would read 0 and a drain gate could be falsely satisfied
// mid-commit.
func TestQueueDepth_InflightItemDequeuedButUnhandled(t *testing.T) {
	w := newMetricsTestWorker()
	w.inflightItems.Store(1)
	w.hasUnpushedWork.Store(false)

	assert.Equal(t, int64(1), w.queueDepth())
}

// This is the regression the observable gauge exists for.
//
// The depth used to be PUSHED from the bottom of the worker loop, so a worker that had accepted
// work the loop had not yet got to reported 0 — and it kept reporting 0 for as long as the loop was
// blocked inside one item, which is the stall the gauge is supposed to expose. Read at scrape time
// there is no loop in between: the enqueue alone is enough.
func TestQueueDepthGauge_ReportsWorkTheLoopHasNotReachedYet(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	manager := &WorkerManager{Log: logr.Discard(), workers: map[BranchKey]*BranchWorker{}}
	w := newMetricsTestWorker()
	manager.workers[BranchKey{RepoNamespace: "test-ns", RepoName: "test-provider", Branch: "main"}] = w
	telemetry.SetGaugeSource(telemetry.GaugeGitQueueDepth, manager.queueDepthSamples)
	defer telemetry.SetGaugeSource(telemetry.GaugeGitQueueDepth, nil)

	// Work is accepted. The loop is not running at all, which is the worst case of "has not got to
	// it yet", and no metric call is made anywhere on this path.
	w.enqueueRequest(&WriteRequest{Events: []Event{{}}, CommitMode: CommitModePerEvent})
	w.enqueueRequest(&WriteRequest{Events: []Event{{}}, CommitMode: CommitModePerEvent})

	depth, ok := telemetry.CollectInt64Sum(reader, queueDepthMetric, workerLabels())
	require.True(t, ok, "the scrape must observe the queue even with the loop stopped")
	assert.Equal(t, int64(2), depth)
}

// A full queue drops work, and that has to be countable. It used to be a log line and nothing else:
// the depth gauge said the queue was deep, and nothing said anything had been thrown away.
func TestEnqueueRequest_FullQueueCountsTheDrop(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	w := newMetricsTestWorker()
	w.eventQueue = make(chan WorkItem, 1)
	require.True(t, w.enqueueRequest(&WriteRequest{Events: []Event{{}}, CommitMode: CommitModePerEvent}))
	require.False(t, w.enqueueRequest(&WriteRequest{Events: []Event{{}}, CommitMode: CommitModePerEvent}),
		"the second request must be refused by the full queue")

	labels := workerLabels()
	labels["kind"] = queueDropWrite
	drops, ok := telemetry.CollectInt64Sum(reader, queueDropsMetric, labels)
	require.True(t, ok, "a dropped write must be counted")
	assert.Equal(t, int64(1), drops)
	assert.Equal(t, int64(1), w.inflightItems.Load(), "the refused item must not stay in the inflight count")
}

// On shutdown the loop must drain items still buffered on eventQueue so the depth reads 0. Each
// buffered item was counted into inflightItems at enqueue; left undrained, the exiting worker would
// report a non-zero depth that never clears.
func TestHandleShutdown_DrainsBufferedItemsToZeroDepth(t *testing.T) {
	w := newMetricsTestWorker()
	// Two write requests accepted onto the queue but never handled by the loop.
	w.enqueueRequest(&WriteRequest{Events: []Event{{}}, CommitMode: CommitModePerEvent})
	w.enqueueRequest(&WriteRequest{Events: []Event{{}}, CommitMode: CommitModePerEvent})
	require.Equal(t, int64(2), w.inflightItems.Load())

	loop := newBranchWorkerEventLoop(w, time.Second)
	loop.handleShutdown()
	loop.syncUnpushedWorkFlag()

	assert.Equal(t, int64(0), w.inflightItems.Load(), "buffered items must be drained from the inflight count")
	assert.Equal(t, int64(0), w.queueDepth(), "a drained, exiting worker must read depth 0")
}

// A CommitRequest attach still buffered at shutdown is fire-and-forget: it is
// simply drained from the inflight count (the controller re-sends on its next
// poll), so the exiting worker's depth gauge settles to 0.
func TestHandleShutdown_DrainsBufferedAttach(t *testing.T) {
	_, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	w := newMetricsTestWorker()
	w.EnqueueAttach(&AttachCommitRequest{Namespace: "default", Name: "save", Author: "alice"})
	require.Equal(t, int64(1), w.inflightItems.Load())

	loop := newBranchWorkerEventLoop(w, time.Second)
	loop.handleShutdown()

	assert.Equal(t, int64(0), w.inflightItems.Load(),
		"a buffered attach must be drained from the inflight count on shutdown")
}

// The message_source label is what separates a commit a person named through a CommitRequest from
// automatic mirroring, so it is asserted on the COLLECTED samples rather than only on
// messageSource(): a label that never reaches the exporter is not observable.
func TestRecordPendingWritesMetrics_LabelsCommitsByMessageSource(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	w := newMetricsTestWorker()
	commitAndPush(w, []PendingWrite{
		{
			Kind:      PendingWriteCommit,
			Events:    []Event{{UserInfo: UserInfo{Username: "alice"}}},
			CommitSHA: plumbing.NewHash("1111111111111111111111111111111111111111"),
		},
		{
			Kind:          PendingWriteCommit,
			Events:        []Event{{UserInfo: UserInfo{Username: "alice"}}},
			CommitMessage: "fix(api): correct the service port",
			CommitSHA:     plumbing.NewHash("2222222222222222222222222222222222222222"),
		},
		{
			Kind:      PendingWriteResync,
			Committed: boolPtr(true),
		},
	}, 3)

	for source, want := range map[string]int64{
		messageSourceLive:          1,
		messageSourceCommitRequest: 1,
		messageSourceReconcile:     1,
	} {
		labels := workerLabels()
		labels["message_source"] = source
		count, ok := telemetry.CollectInt64Sum(reader, commitsTotalMetric, labels)
		require.True(t, ok, "expected a commits_total sample for message_source=%q", source)
		assert.Equal(t, want, count, "commits_total for message_source=%q", source)
	}
}

// The first two writes above share author_kind=user, so author_kind alone cannot separate them.
// This pins that the two labels are independent: a dashboard splitting one author's commits by
// message source must not see them collapsed into a single series.
func TestRecordPendingWritesMetrics_MessageSourceSplitsOneAuthorKind(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	w := newMetricsTestWorker()
	commitAndPush(w, []PendingWrite{
		{
			Kind:      PendingWriteCommit,
			Events:    []Event{{UserInfo: UserInfo{Username: "alice"}}},
			CommitSHA: plumbing.NewHash("3333333333333333333333333333333333333333"),
		},
		{
			Kind:          PendingWriteCommit,
			Events:        []Event{{UserInfo: UserInfo{Username: "alice"}}},
			CommitMessage: "fix(api): correct the service port",
			CommitSHA:     plumbing.NewHash("4444444444444444444444444444444444444444"),
		},
	}, 2)

	labels := workerLabels()
	labels["author_kind"] = authorKindUser

	total, ok := telemetry.CollectInt64Sum(reader, commitsTotalMetric, labels)
	require.True(t, ok)
	assert.Equal(t, int64(2), total, "both commits share author_kind=user")

	labels["message_source"] = messageSourceLive
	live, ok := telemetry.CollectInt64Sum(reader, commitsTotalMetric, labels)
	require.True(t, ok)
	assert.Equal(t, int64(1), live)

	labels["message_source"] = messageSourceCommitRequest
	request, ok := telemetry.CollectInt64Sum(reader, commitsTotalMetric, labels)
	require.True(t, ok)
	assert.Equal(t, int64(1), request)
}

// A commit that never reaches the remote must not be counted.
//
// This is the defect the tally exists to fix: commits_total was recorded at local commit creation
// while the doc comment claimed it counted pushed commits, so a dead remote and a healthy one drew
// the same graph — the product's headline metric climbing while nothing arrived in Git.
func TestCommitsTotal_NotPublishedUntilThePushLands(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	w := newMetricsTestWorker()
	w.recordPendingWritesMetrics(nil, 3)

	_, ok := telemetry.CollectInt64Sum(reader, commitsTotalMetric, workerLabels())
	require.False(t, ok, "a locally created commit must not be counted before it reaches the remote")

	w.publishCommittedTally()

	commits, ok := telemetry.CollectInt64Sum(reader, commitsTotalMetric, workerLabels())
	require.True(t, ok)
	assert.Equal(t, int64(3), commits)
}

// A failed push holds its tally, so the commits it was carrying are counted by whichever later push
// finally lands them — once, not twice, and never zero.
func TestCommitsTotal_HeldTallyIsPublishedByTheNextSuccessfulPush(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	w := newMetricsTestWorker()
	w.recordPendingWritesMetrics(nil, 2)
	// The push failed: nothing published, and the tally stands.
	w.recordPushOutcome(pushOutcomeFailed, time.Now())
	_, ok := telemetry.CollectInt64Sum(reader, commitsTotalMetric, workerLabels())
	require.False(t, ok)

	// More work is committed locally behind it, then a push lands the lot.
	w.recordPendingWritesMetrics(nil, 1)
	w.publishCommittedTally()

	commits, ok := telemetry.CollectInt64Sum(reader, commitsTotalMetric, workerLabels())
	require.True(t, ok)
	assert.Equal(t, int64(3), commits, "every commit the successful push carried is counted exactly once")

	// And the tally does not replay on the next push.
	w.publishCommittedTally()
	commits, _ = telemetry.CollectInt64Sum(reader, commitsTotalMetric, workerLabels())
	assert.Equal(t, int64(3), commits)
}

// A push cycle ends exactly once, with a duration, whichever way it ended. Without this the only
// trace of a mirror that has stopped advancing is a log line.
func TestRecordPushOutcome_CountsTheCycleAndItsDuration(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	w := newMetricsTestWorker()
	w.recordPushOutcome(pushOutcomePushed, time.Now())
	w.recordPushOutcome(pushOutcomeFailed, time.Now())
	w.recordPushRetry(pushRetryRemoteMoved)

	labels := workerLabels()
	labels["outcome"] = pushOutcomeFailed
	failed, ok := telemetry.CollectInt64Sum(reader, pushesTotalMetric, labels)
	require.True(t, ok, "a push cycle that gave up must be counted")
	assert.Equal(t, int64(1), failed)

	labels["outcome"] = pushOutcomePushed
	pushed, ok := telemetry.CollectInt64Sum(reader, pushesTotalMetric, labels)
	require.True(t, ok)
	assert.Equal(t, int64(1), pushed)

	retryLabels := workerLabels()
	retryLabels["reason"] = pushRetryRemoteMoved
	retries, ok := telemetry.CollectInt64Sum(reader, pushRetriesMetric, retryLabels)
	require.True(t, ok, "a replay round is counted separately: it is not a terminal outcome")
	assert.Equal(t, int64(1), retries)

	durations, ok := telemetry.CollectHistogramCount(reader, pushDurationMetric, workerLabels())
	require.True(t, ok)
	assert.Equal(t, uint64(2), durations, "both cycles are timed, however they ended")
}
