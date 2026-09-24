// SPDX-License-Identifier: Apache-2.0

package git

// Red-first, per docs/design/push-notification-and-reconcile-trigger.md §1.7: today's behavior is pinned as a
// passing assertion, so the change that removes the head-of-cycle fetch is proved by this
// assertion flipping rather than by argument.
//
// The counter counts CALLS THAT RUN SmartFetch, which is not the same as requests to the Git
// host — one SmartFetch is two or three of those. The ledger in git_roundtrip_ledger_test.go
// measures requests; this measures intent, which is what an operator's query is about.

import (
	"context"
	"errors"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	gitclient "github.com/go-git/go-git/v6/plumbing/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
)

const fetchesTotalMetric = "gitopsreverser_git_fetches_total"

// fetchLabels is the identity a ledger fixture's worker records under, narrowed to one reason.
func fetchLabels(worker *BranchWorker, reason string) map[string]string {
	return map[string]string{
		"provider_namespace": worker.GitProviderNamespace,
		"provider_name":      worker.GitProviderRef,
		"branch":             worker.Branch,
		"reason":             reason,
	}
}

func fetchCount(t *testing.T, reader *sdkmetric.ManualReader, worker *BranchWorker, reason string) int64 {
	t.Helper()
	value, ok := telemetry.CollectInt64Sum(reader, fetchesTotalMetric, fetchLabels(worker, reason))
	if !ok {
		return 0
	}
	return value
}

// TestGitFetchesTotal_SteadyStatePublicationDoesNotFetch is the assertion the flip inverted, and
// the headline claim of the whole design: a healthy target that is publishing does not read the
// remote at all.
//
// The first cycle still fetches, because a new worker has never looked at the remote and its base
// is untrusted by construction. Every cycle after that plans on the tip its own push established.
func TestGitFetchesTotal_SteadyStatePublicationDoesNotFetch(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	f := newLedgerFixture(t, "metric-publication", true)

	f.publish("first")
	assert.Equal(t, int64(1), fetchCount(t, reader, f.worker, fetchReasonPublication),
		"a worker that has never seen the remote fetches once to establish its base")

	f.publish("second")
	f.publish("third")
	assert.Equal(t, int64(1), fetchCount(t, reader, f.worker, fetchReasonPublication),
		"every cycle after that plans on the tip its own push established")

	// And still once per CYCLE rather than per commit, which is the property the guard must not
	// have broken on its way to becoming conditional.
	f.commit(false, "fourth")
	f.commit(true, "fifth")
	f.commit(true, "sixth")
	f.push()
	assert.Equal(t, int64(1), fetchCount(t, reader, f.worker, fetchReasonPublication))

	assert.Zero(t, fetchCount(t, reader, f.worker, fetchReasonRecovery),
		"nothing failed, so nothing had to recover")
}

// TestGitFetchesTotal_DirtyWorktreeFetchesUnderRecovery is the other side of the guard: a base
// that cannot be trusted still fetches, and a worktree left dirty by a failed write is counted
// apart from the ordinary case so it reads as the bug report it is.
func TestGitFetchesTotal_DirtyWorktreeFetchesUnderRecovery(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	f := newLedgerFixture(t, "metric-recovery", true)
	f.publish("first")
	before := fetchCount(t, reader, f.worker, fetchReasonPublication)

	// A write failed part-way and left the worktree dirty. Nothing is retained, so the next
	// cycle's own guard is what has to clean up.
	f.worker.markWorktreeDirty("a write failed part-way")

	f.publish("second")
	assert.Equal(t, int64(1), fetchCount(t, reader, f.worker, fetchReasonRecovery),
		"a dirty worktree must be reset before the next cycle plans on it")
	assert.Equal(t, before, fetchCount(t, reader, f.worker, fetchReasonPublication),
		"and it is not counted as an ordinary publication fetch")
	assert.False(t, f.worker.worktreeDirty(), "the reset cleared it")
}

// TestGitFetchesTotal_ContentionIsCountedSeparately pins the label that makes the steady-state
// claim assertable. A bare total would fall when contention fell, so it could not distinguish
// "we removed the fetch" from "the test was quiet".
func TestGitFetchesTotal_ContentionIsCountedSeparately(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	f := newLedgerFixture(t, "metric-contention", true)
	f.publish("prime")

	f.commit(false, "mine")
	f.contend("OUTSIDE.md", "from-another-writer\n")
	f.push()

	// One rejection is one contention fetch: the rejected push carried the remote's hash, so only
	// the reset that follows has to reach the network. See §2.2.
	assert.Equal(t, int64(1), fetchCount(t, reader, f.worker, fetchReasonContention),
		"a rejection resets to the remote tip, and that reset is a fetch")
	assert.Zero(t, fetchCount(t, reader, f.worker, fetchReasonPushFailureProbe),
		"the push said where the remote was, so nothing had to probe for it")
	assert.Equal(t, int64(1), fetchCount(t, reader, f.worker, fetchReasonPublication),
		"only the worker's first cycle fetched; the second planned on a trusted base")
}

// TestGitFetchesTotal_PushFailureProbeIsNotContention separates the two ways a push can fail.
//
// An auth failure or a dropped connection produces no advertisement, so the worker has to look up
// where the remote is. Counting that as contention would inflate the series an operator reads as
// "other writers are fighting me over this branch" on a target that has no other writers.
func TestGitFetchesTotal_PushFailureProbeIsNotContention(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	f := newLedgerFixture(t, "metric-probe", true)
	f.publish("prime")
	f.commit(false, "mine")

	// A push that dies before the remote says anything, on every attempt.
	original := pushAtomicFn
	pushAtomicFn = func(
		_ context.Context, _ *gogit.Repository, _ plumbing.Hash,
		_ plumbing.ReferenceName, _ []gitclient.Option,
	) (PushOutcome, error) {
		return PushOutcome{}, errors.New("dial tcp: connection reset by peer")
	}
	defer func() { pushAtomicFn = original }()

	require.Error(t, f.worker.pushPendingCommits(f.pending))

	assert.Equal(t, int64(1), fetchCount(t, reader, f.worker, fetchReasonPushFailureProbe),
		"the failure had to ask where the remote was, and that is one fetch")
	assert.Zero(t, fetchCount(t, reader, f.worker, fetchReasonContention),
		"the remote had not moved, so nothing contended and nothing reset")
}

// TestGitFetchesTotal_ProbeThenResetWhenTheRemoteDidMove is the other half: a push that produced
// no advertisement AND a remote that really moved costs one probe plus one reset.
func TestGitFetchesTotal_ProbeThenResetWhenTheRemoteDidMove(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	f := newLedgerFixture(t, "metric-probe-moved", true)
	f.publish("prime")
	f.commit(false, "mine")

	original := pushAtomicFn
	pushes := 0
	pushAtomicFn = func(
		ctx context.Context, repo *gogit.Repository, rootHash plumbing.Hash,
		rootBranch plumbing.ReferenceName, auth []gitclient.Option,
	) (PushOutcome, error) {
		pushes++
		if pushes == 1 {
			return PushOutcome{}, errors.New("dial tcp: connection reset by peer")
		}
		return original(ctx, repo, rootHash, rootBranch, auth)
	}
	defer func() { pushAtomicFn = original }()

	f.contend("OUTSIDE.md", "from-another-writer\n")
	require.NoError(t, f.worker.pushPendingCommits(f.pending))

	assert.Equal(t, int64(1), fetchCount(t, reader, f.worker, fetchReasonPushFailureProbe))
	assert.Equal(t, int64(1), fetchCount(t, reader, f.worker, fetchReasonContention),
		"the probe confirmed movement, so the reset that follows is contention")
}

// TestGitFetchesTotal_ForcedRecheckAndBootstrap covers the two reasons that must keep counting
// after the flip: a worker's first contact with the repository, and somebody asking it to look at
// Git now.
func TestGitFetchesTotal_ForcedRecheckAndBootstrap(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	f := newLedgerFixture(t, "metric-recheck", true)
	f.createLedgerTarget("team-a", nil)

	require.NoError(t, f.worker.EnsurePathBootstrapped("team-a", "target-a", "default"))
	assert.Equal(t, int64(1), fetchCount(t, reader, f.worker, fetchReasonBootstrap),
		"the bootstrap is instrumented at prepareBootstrapRepository. NOTE: its only caller is\n"+
			"EnsurePathBootstrapped, which has no production caller, so this series is test-only today")

	err = f.worker.syncWithRemote(f.worker.ctx, fetchReasonForcedRecheck)
	require.NoError(t, err)
	assert.Equal(t, int64(1), fetchCount(t, reader, f.worker, fetchReasonForcedRecheck),
		"a forced recheck with nothing retained fetches through syncWithRemote")

	f.commit(false, "retained")
	require.NoError(t, f.worker.refreshRemoteAndRebuildPendingWrites(f.worker.ctx, f.pending, fetchReasonForcedRecheck))
	assert.Equal(t, int64(2), fetchCount(t, reader, f.worker, fetchReasonForcedRecheck),
		"and with retained writes it fetches through refreshRemoteAndRebuildPendingWrites")
}

// TestGitFetchesTotal_IdleTargetNeverFetches is the property behind ledger row 11, stated in the
// metric an operator would actually query.
func TestGitFetchesTotal_IdleTargetNeverFetches(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	f := newLedgerFixture(t, "metric-idle", true)
	f.publish("prime")

	before := fetchCount(t, reader, f.worker, fetchReasonPublication)

	loop := newBranchWorkerEventLoop(f.worker, idleLedgerCommitWindow)
	defer loop.stopTimers()
	// Every timer an idle worker can fire, fired, with nothing to do.
	for range idleLedgerWindows {
		loop.finalizeOpenWindow()
		loop.pushPending()
		loop.applyDeferredHeals()
	}

	for _, reason := range []string{
		fetchReasonBootstrap,
		fetchReasonPublication,
		fetchReasonRecovery,
		fetchReasonContention,
		fetchReasonPushFailureProbe,
		fetchReasonForcedRecheck,
	} {
		want := int64(0)
		if reason == fetchReasonPublication {
			want = before
		}
		assert.Equal(t, want, fetchCount(t, reader, f.worker, reason),
			"an idle target must not fetch for reason %q", reason)
	}
}

// TestGitFetchesTotal_ReasonSetMatchesTheDocumentedOne keeps the reason set honest: every
// constant is either produced by a call site above or documented as reserved, and this is the
// list docs/interpreting-metrics.md publishes.
func TestGitFetchesTotal_ReasonSetMatchesTheDocumentedOne(t *testing.T) {
	assert.Equal(t,
		[]string{"bootstrap", "publication", "recovery", "contention", "push_failure_probe", "forced_recheck"},
		[]string{
			fetchReasonBootstrap,
			fetchReasonPublication,
			fetchReasonRecovery,
			fetchReasonContention,
			fetchReasonPushFailureProbe,
			fetchReasonForcedRecheck,
		},
		"the documented reason set and the constants must not drift")
}

// TestGitFetchesTotal_ResyncWithRetainedWritesStillReadsTheRemote is the escape hatch §2.1 names,
// driven through the case that used to slip past it.
//
// A resync keeps its fetch because it does not always reach a push: applyResync retains its write
// only if it committed, so a resync that finds nothing to change never opens a connection and
// would conclude "Git already matches the cluster" against a tree nobody had read. Dropping base
// trust is what was supposed to prevent that — but ensureBaseForCycle consults the flag only when
// nothing is retained, so on a target holding work the invalidation reached nothing at all.
func TestGitFetchesTotal_ResyncWithRetainedWritesStillReadsTheRemote(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	f := newLedgerFixture(t, "metric-resync-retained", true)
	f.worker.mapper = configMapMapper()
	f.createLedgerTarget("live", &configv1alpha3.PrunePolicy{Mode: configv1alpha3.PruneAlways})
	f.publish("prime")

	// A live write is committed and retained: the push cooldown has not elapsed, so it is sitting
	// in pendingWrites when the resync arrives.
	loop := newBranchWorkerEventLoop(f.worker, time.Hour)
	loop.lastPushAt = time.Now()
	defer loop.stopTimers()
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapEvent("retained", "alice", "live")},
		CommitMode: CommitModePerEvent,
	}})
	require.True(t, loop.finalizeOpenWindow())
	require.Len(t, loop.pendingWrites, 1, "the resync must arrive with work retained")

	before := fetchCount(t, reader, f.worker, fetchReasonForcedRecheck)

	req := &ResyncRequest{
		Desired:            []manifestanalyzer.DesiredResource{desiredCM("keep", "blue")},
		ResourceVersion:    "42",
		GitTargetName:      ledgerTargetName,
		GitTargetNamespace: "default",
		Result:             make(chan ResyncResult, 1),
	}
	loop.applyResync(req)
	require.NoError(t, (<-req.Result).Err)

	assert.Greater(t, fetchCount(t, reader, f.worker, fetchReasonForcedRecheck), before,
		"a resync judging the tree must have read the remote, retained writes or not")
}

// TestGitFetchesTotal_RecoveryIsOneSeriesWhetherOrNotWritesAreRetained is the label bug that made
// the `recovery` series unreadable.
//
// A dirty worktree is recovered two different ways: commitPendingWrites resets when nothing is
// retained, and the event loop resets and replays when something is. Those are the same event, and
// which one happens depends only on whether a push was in cooldown at the time. The replay path
// reported them as `forced_recheck` — nobody asked for anything — so half of every recovery
// vanished from the series §12 tells operators to read as a bug report.
func TestGitFetchesTotal_RecoveryIsOneSeriesWhetherOrNotWritesAreRetained(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	f := newLedgerFixture(t, "metric-recovery-retained", true)
	f.createLedgerTarget("team-a", nil)
	f.publish("prime")

	loop := newBranchWorkerEventLoop(f.worker, time.Hour)
	loop.lastPushAt = time.Now()
	defer loop.stopTimers()

	// Retained work, so the loop's replay path is the one that recovers.
	loop.handleQueueItem(WorkItem{Request: &WriteRequest{
		Events:     []Event{configMapEvent("retained", "alice", "team-a")},
		CommitMode: CommitModePerEvent,
	}})
	require.True(t, loop.finalizeOpenWindow())
	require.Len(t, loop.pendingWrites, 1)

	forcedBefore := fetchCount(t, reader, f.worker, fetchReasonForcedRecheck)
	f.worker.markWorktreeDirty("a write failed part-way")
	require.NoError(t, loop.recoverRetainedWrites())

	assert.Equal(t, int64(1), fetchCount(t, reader, f.worker, fetchReasonRecovery),
		"a recovery is a recovery whether or not a push happened to be in cooldown")
	assert.Equal(t, forcedBefore, fetchCount(t, reader, f.worker, fetchReasonForcedRecheck),
		"and nobody asked the worker to re-read Git, so it is not a forced recheck")
	assert.False(t, f.worker.worktreeDirty(), "the reset cleared it")
}

// TestGitFetchesTotal_ResyncWithNoRetainedWritesAlsoReadsTheRemote is the other half of the
// resync guarantee, and it is worth pinning separately because the fetch is not where you would
// look for it.
//
// prepareBaseForResync owns the fetch on this path. With nothing retained, invalidateAndRefresh
// drops base trust and then calls syncWithRemote directly rather than leaving the reset to the
// commit that follows, which is what makes the reason `forced_recheck` instead of `publication`:
// the fetch belongs to the snapshot that asked for it, not to a live publication. Ledger row 10
// measures the same operation as requests on the wire; this pins which series it lands in.
func TestGitFetchesTotal_ResyncWithNoRetainedWritesAlsoReadsTheRemote(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	f := newLedgerFixture(t, "metric-resync-unretained", true)
	f.worker.mapper = configMapMapper()
	f.createLedgerTarget("live", &configv1alpha3.PrunePolicy{Mode: configv1alpha3.PruneAlways})
	f.publish("prime")
	require.True(t, f.worker.baseTrusted(), "the publish leaves the base trusted")

	loop := newBranchWorkerEventLoop(f.worker, time.Hour)
	defer loop.stopTimers()
	require.Empty(t, loop.pendingWrites, "this is the nothing-retained path")

	pubBefore := fetchCount(t, reader, f.worker, fetchReasonPublication)
	forcedBefore := fetchCount(t, reader, f.worker, fetchReasonForcedRecheck)

	req := &ResyncRequest{
		Desired:            []manifestanalyzer.DesiredResource{desiredCM("keep", "blue")},
		ResourceVersion:    "42",
		GitTargetName:      ledgerTargetName,
		GitTargetNamespace: "default",
		Result:             make(chan ResyncResult, 1),
	}
	loop.applyResync(req)
	require.NoError(t, (<-req.Result).Err)

	assert.Equal(t, forcedBefore+1, fetchCount(t, reader, f.worker, fetchReasonForcedRecheck),
		"a resync must read the remote before judging the tree, retained writes or not")
	assert.Equal(t, pubBefore, fetchCount(t, reader, f.worker, fetchReasonPublication),
		"and it must NOT be filed under the series this design asserts at zero on a healthy "+
			"target: a snapshot resync is not a live publication fetching again")
}
