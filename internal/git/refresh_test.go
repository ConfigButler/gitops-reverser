// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	"github.com/ConfigButler/gitops-reverser/internal/telemetry"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// refreshHarness is a ledger fixture plus a loop to run the refresh on, and a record of every
// observation reported out of it.
type refreshHarness struct {
	*ledgerFixture

	loop     *branchWorkerEventLoop
	target   itypes.ResourceReference
	path     string
	reported []RemoteObservation
}

func newRefreshHarness(t *testing.T, slug string) *refreshHarness {
	t.Helper()
	return newRefreshHarnessOn(t, slug, true)
}

// newRefreshHarnessOn builds the harness on a remote that may or may not already have the branch.
func newRefreshHarnessOn(t *testing.T, slug string, seeded bool) *refreshHarness {
	t.Helper()
	h := &refreshHarness{
		ledgerFixture: newLedgerFixture(t, slug, seeded),
		target:        itypes.NewResourceReference("checkout", "shop"),
		path:          "team-a",
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
	h.loop.handleRefreshRequest(&RefreshRequest{Target: h.target, Path: h.path, MaxAge: maxAge})
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

	h.reported = nil
	connections := h.refresh(time.Nanosecond)

	assert.Zero(t, connections, "a worker mid-cycle is not the target the refresher exists for")
	assert.Len(t, h.reported, 1,
		"but what is already known is still reported: on a shared branch this tick is the only "+
			"way a target that is not writing hears anything")
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

// TestRefresh_RepublishesTheLayoutOfAFolderSomebodyElseChanged is most of what an operator reads
// off a refreshed target: the branch moved, the folder under it is somebody else's now, and what
// its shape implies about placement has to follow.
//
// The refusal side deliberately does NOT follow: see rescanLayoutForTarget.
func TestRefresh_RepublishesTheLayoutOfAFolderSomebodyElseChanged(t *testing.T) {
	h := newRefreshHarness(t, "refresh-layout")

	var layouts []LayoutReport
	h.worker.layoutReporter = func(_ itypes.ResourceReference, report LayoutReport) {
		layouts = append(layouts, report)
	}

	h.publish("prime")
	// Somebody turns the folder into a kustomize root from outside.
	h.contend("team-a/kustomization.yaml",
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n")
	layouts = nil

	h.refresh(time.Nanosecond)

	require.NotEmpty(t, layouts, "the folder changed under us, so its layout must be republished")
	last := layouts[len(layouts)-1]
	assert.Equal(t, manifestanalyzer.LayoutSingleKustomization, last.Reason,
		"the refresh must report the kustomization somebody else added")
}

// TestRefresh_AnAbsentBranchCostsOneConnection is the ordinary state of a target that has not
// written yet, and it must not become a standing per-interval cost. The advertisement is the whole
// answer: a fetch for a branch nobody has created would fall back to the default branch and teach
// us nothing.
func TestRefresh_AnAbsentBranchCostsOneConnection(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	// An unseeded remote: the branch this worker is for does not exist on it. The repository is
	// prepared, as it is for any target the controller has wired, so what is measured is the
	// refresh and not a first clone.
	h := newRefreshHarnessOn(t, "refresh-absent", false)
	require.NoError(t, h.worker.ensureRepositoryInitialized(h.worker.ctx))
	h.reported = nil

	connections := h.refresh(time.Nanosecond)

	assert.Equal(t, int64(1), connections, "one advertisement answers it")
	assert.Zero(t, fetchCount(t, reader, h.worker, fetchReasonRefresh))
	require.NotEmpty(t, h.reported)
	assert.Empty(t, h.reported[len(h.reported)-1].Revision,
		"no revision IS the observation: a branch does not exist without a commit")
}

// TestRefresh_SkipsAWorkerThatHasNeverCloned keeps an ordinary state out of the error log. A
// worker exists for every branch a GitTarget names, including one that has never published, and a
// refresh tick for it must not report a failure once per interval forever.
func TestRefresh_SkipsAWorkerThatHasNeverCloned(t *testing.T) {
	h := newRefreshHarnessOn(t, "refresh-no-checkout", true)
	// No publish, no bootstrap: nothing has created the on-disk clone.

	connections := h.refresh(time.Nanosecond)

	assert.Zero(t, connections, "there is no checkout to refresh, so nothing may be spent")
	assert.Empty(t, h.reported, "and nothing is claimed about a remote nobody has looked at")
}

// TestRefresh_AQuietSiblingIsRescannedWithoutFetching is the shared-branch case, and it is the one
// a per-branch view of freshness gets wrong.
//
// One worker serves every GitTarget on its (provider, branch). When target A's refresh fetches, it
// re-reads A's folder and knows nothing about B's. If B's own tick then skipped the re-read
// because the checkout already matched the remote, B's published layout would describe a folder
// that has since changed — and would stay that way for as long as the branch kept still.
func TestRefresh_AQuietSiblingIsRescannedWithoutFetching(t *testing.T) {
	h := newRefreshHarness(t, "refresh-sibling")

	var layouts []LayoutReport
	h.worker.layoutReporter = func(_ itypes.ResourceReference, report LayoutReport) {
		layouts = append(layouts, report)
	}

	h.publish("prime")
	// Somebody changes the folder's shape from outside, and A's refresh brings the checkout onto
	// it: after this the worktree is current, so nothing will fetch again.
	h.contend("team-a/kustomization.yaml",
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n")
	h.refresh(time.Nanosecond)

	// B's tick. Its observation is fresh — A's refresh renewed it moments ago — so this must cost
	// nothing at the remote and still re-read the folder.
	layouts = nil
	connections := h.refresh(time.Hour)

	assert.Zero(t, connections, "a fresh observation means nothing may be spent at the remote")
	require.NotEmpty(t, layouts, "the quiet sibling must still re-read its own folder")
	assert.Equal(t, manifestanalyzer.LayoutSingleKustomization, layouts[len(layouts)-1].Reason)
}

// TestRefresh_AFailedFetchDoesNotBuyQuietUntilTheObservationAges is the ordering hazard in the
// cheap path. The observation is recorded from the ADVERTISEMENT, before the fetch that acts on
// it, so a fetch that fails leaves a fresh observation standing beside a checkout that never moved
// onto it. Skipping the next tick on the age alone would rescan the old tree and publish its
// layout until the observation aged out.
func TestRefresh_AFailedFetchDoesNotBuyQuietUntilTheObservationAges(t *testing.T) {
	h := newRefreshHarness(t, "refresh-failed-fetch")
	h.publish("prime")

	// The state a failed refresh leaves behind: a fresh observation of a revision the checkout
	// is not at, and a base nobody can vouch for.
	h.worker.recordRemoteObservation("0000000000000000000000000000000000000000", ObservedByFetch)
	h.worker.invalidateBase("a fetch that failed after the advertisement")

	connections := h.refresh(time.Hour)

	assert.Positive(t, connections,
		"an untrusted base must send the tick to the remote, whatever the observation's age says")
}

// TestRemoteObservation_DroppedWhenTheProviderNamesANewRepository. A worker is keyed by
// (provider, branch) while spec.url is immutable and repointed by recreating the GitProvider, so
// one worker can meet a second repository without anything restarting it. An observation is a
// statement about a REPOSITORY; carrying it across would report the old one's revision as this
// GitTarget's remote state, and a fresh enough one would suppress the look that corrects it.
func TestRemoteObservation_DroppedWhenTheProviderNamesANewRepository(t *testing.T) {
	f := newLedgerFixture(t, "observation-repoint", true)
	f.publish("prime")

	before, ok := f.worker.LastRemoteObservation()
	require.True(t, ok)
	require.NotEmpty(t, before.Revision)

	f.worker.noteRemoteIdentity("https://example.invalid/another/repo.git")

	_, known := f.worker.LastRemoteObservation()
	assert.False(t, known, "what was proved about the old repository says nothing about this one")
	assert.False(t, f.worker.baseTrusted(), "and the checkout for it is not established either")
}
