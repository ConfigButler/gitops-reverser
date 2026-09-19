// SPDX-License-Identifier: Apache-2.0

package git

// Red-first, per docs/design/inbound-push-notification.md §12: today's behavior is pinned as a
// passing assertion, so the change that removes the head-of-cycle fetch is proved by this
// assertion flipping rather than by argument.
//
// The counter counts CALLS THAT RUN SmartFetch, which is not the same as requests to the Git
// host — one SmartFetch is two or three of those. The ledger in git_roundtrip_ledger_test.go
// measures requests; this measures intent, which is what an operator's query is about.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

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

// TestGitFetchesTotal_PublicationFetchesEveryCycle is the assertion the flip inverts. Today a
// publication cycle fetches before it plans, once per cycle; after the change it will be zero on
// a healthy steady-state target.
func TestGitFetchesTotal_PublicationFetchesEveryCycle(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	f := newLedgerFixture(t, "metric-publication", true)

	f.publish("first")
	assert.Equal(t, int64(1), fetchCount(t, reader, f.worker, fetchReasonPublication),
		"the head of a publication cycle fetches")

	f.publish("second")
	assert.Equal(t, int64(2), fetchCount(t, reader, f.worker, fetchReasonPublication),
		"and does so once per cycle")

	// Once per CYCLE, not once per commit: the later commits of a cycle build on the local repo.
	f.commit(false, "third")
	f.commit(true, "fourth")
	f.commit(true, "fifth")
	f.push()
	assert.Equal(t, int64(3), fetchCount(t, reader, f.worker, fetchReasonPublication),
		"three commits in one cycle are still one fetch")

	assert.Zero(t, fetchCount(t, reader, f.worker, fetchReasonRecovery),
		"nothing produces the recovery reason yet")
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

	// One rejection is one contention fetch now, not two: the rejected push carried the remote's
	// hash, so only the reset that follows has to reach the network. See §2.2.
	assert.Equal(t, int64(1), fetchCount(t, reader, f.worker, fetchReasonContention),
		"a rejection resets to the remote tip, and that reset is a fetch")
	assert.Equal(t, int64(2), fetchCount(t, reader, f.worker, fetchReasonPublication),
		"the two publication cycles are counted under their own reason, not contention")
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
		"the bootstrap is instrumented at prepareBootstrapRepository, the site with a caller")

	_, err = f.worker.syncWithRemote(f.worker.ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), fetchCount(t, reader, f.worker, fetchReasonForcedRecheck),
		"a forced recheck with nothing retained fetches through syncWithRemote")

	f.commit(false, "retained")
	require.NoError(t, f.worker.refreshRemoteAndRebuildPendingWrites(f.worker.ctx, f.pending))
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
	assert.Equal(t, []string{"bootstrap", "publication", "recovery", "contention", "forced_recheck"},
		[]string{
			fetchReasonBootstrap,
			fetchReasonPublication,
			fetchReasonRecovery,
			fetchReasonContention,
			fetchReasonForcedRecheck,
		},
		"the documented reason set and the constants must not drift")
}
