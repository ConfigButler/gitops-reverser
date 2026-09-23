// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// refreshHarness is a ledger fixture plus a loop to run the refresh on, and a record of every
// observation reported out of it.
type refreshHarness struct {
	*ledgerFixture

	loop     *branchWorkerEventLoop
	target   itypes.ResourceReference
	reported []RemoteObservation
}

func newRefreshHarness(t *testing.T, slug string) *refreshHarness {
	t.Helper()
	h := &refreshHarness{
		ledgerFixture: newLedgerFixture(t, slug, true),
		target:        itypes.NewResourceReference("checkout", "shop"),
	}
	h.loop = newBranchWorkerEventLoop(h.worker, time.Hour)
	h.worker.remoteReporter = func(_ itypes.ResourceReference, observed RemoteObservation) {
		h.reported = append(h.reported, observed)
	}
	return h
}

// refresh runs one refresh tick with the given maximum age and returns what it cost in
// connections to the Git host.
func (h *refreshHarness) refresh(maxAge time.Duration) int64 {
	before := h.mark()
	h.loop.handleRefreshRequest(&RefreshRequest{Target: h.target, MaxAge: maxAge})
	return h.mark().since(before).connections()
}

// TestRefresh_AFreshObservationCostsNothing is decision 2's test, and the property the whole
// design turns on: an actively-publishing branch never schedules a fetch, because its last push
// already proved where the remote is.
//
// The report still happens. A quiet GitTarget sharing a branch with a busy one has nothing to
// fetch, and its status still has to say when the branch was last proved.
func TestRefresh_AFreshObservationCostsNothing(t *testing.T) {
	h := newRefreshHarness(t, "refresh-fresh")
	h.publish("written-by-a-push")

	connections := h.refresh(time.Hour)

	assert.Zero(t, connections, "a push inside the age is the refresh; nothing may be spent on top of it")
	require.Len(t, h.reported, 1, "the target still has to be told what we know")
	assert.Equal(t, ObservedByPush, h.reported[0].By)
}

// TestRefresh_AnUnmovedBranchCostsOneConnection is the refresher's common case, and the reason it
// does not simply call syncWithRemote: that is two connections, and on an idle target the
// advertisement alone settles the question.
func TestRefresh_AnUnmovedBranchCostsOneConnection(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	h := newRefreshHarness(t, "refresh-unmoved")
	h.publish("prime")

	connections := h.refresh(time.Nanosecond) // everything is stale

	assert.Equal(t, int64(1), connections, "one ref advertisement, and nothing else")
	assert.Zero(t, fetchCount(t, reader, h.worker, fetchReasonRefresh),
		"an advertisement that confirms the branch has not moved runs no SmartFetch")
	require.NotEmpty(t, h.reported)
	last := h.reported[len(h.reported)-1]
	assert.Equal(t, ObservedByFetch, last.By, "we went and looked, so the record says so")
	assert.Equal(t, revParseMain(t, h.repoDir), last.Revision)
}

// TestRefresh_AMovedBranchFetchesAndResets is the other arm: somebody else pushed, so the
// advertisement disagrees with the checkout and the refresher pays for the fetch it now needs.
func TestRefresh_AMovedBranchFetchesAndResets(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	h := newRefreshHarness(t, "refresh-moved")
	h.publish("prime")
	h.contend("OUTSIDE.md", "from-another-writer\n")
	moved := revParseMain(t, h.repoDir)

	h.refresh(time.Nanosecond)

	assert.Equal(t, int64(1), fetchCount(t, reader, h.worker, fetchReasonRefresh),
		"the branch moved, so the checkout has to be brought onto it")
	require.NotEmpty(t, h.reported)
	assert.Equal(t, moved, h.reported[len(h.reported)-1].Revision,
		"status must follow the branch somebody else moved")
	assert.True(t, h.worker.baseTrusted(), "the reset leaves the worktree at the remote tip")
}

// TestRefresh_SkipsABranchMidCycle is the guard that does not depend on decision 2 being right.
// A reset here would destroy the local commits behind retained writes.
func TestRefresh_SkipsABranchMidCycle(t *testing.T) {
	h := newRefreshHarness(t, "refresh-mid-cycle")
	h.commit(false, "retained")
	h.loop.pendingWrites = h.pending

	connections := h.refresh(time.Nanosecond)

	assert.Zero(t, connections, "a worker mid-cycle is not the target the refresher exists for")
	assert.Empty(t, h.reported)
	assert.Len(t, h.loop.pendingWrites, 1, "and the retained write is still there")
}

// TestRefresh_WritesNothing is the boundary as a test rather than a rule: a fresh look at Git
// must never cause a publication.
func TestRefresh_WritesNothing(t *testing.T) {
	h := newRefreshHarness(t, "refresh-writes-nothing")
	h.publish("prime")
	h.contend("OUTSIDE.md", "from-another-writer\n")
	moved := revParseMain(t, h.repoDir)

	h.refresh(time.Nanosecond)

	assert.Equal(t, moved, revParseMain(t, h.repoDir),
		"the branch must be exactly where the other writer left it: no commit, empty or otherwise")
	assert.Empty(t, h.loop.pendingWrites, "and nothing may be retained for a later push")
}
