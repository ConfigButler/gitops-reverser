// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
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

// TestRefresh_ObservesTheRemoteWithNoCheckoutYet. A GitTarget that has been declared but has not
// published has a worker and no clone, and it still deserves an honest status.remote.
//
// An earlier cut skipped it to avoid "a standing cost for nothing". The cost is the same one every
// idle target pays — a single ref advertisement — and what it buys is not nothing: the question
// "where is my branch" is answered by the REMOTE, so it needs no local checkout, and skipping left
// this target's status either empty or (after a GitProvider was repointed) still describing a
// repository it no longer uses.
func TestRefresh_ObservesTheRemoteWithNoCheckoutYet(t *testing.T) {
	h := newRefreshHarness(t, "refresh-no-checkout")
	// No publish and no bootstrap: nothing has created the on-disk clone.

	connections := h.refresh(time.Nanosecond)

	assert.Equal(t, int64(1), connections, "one advertisement, which needs no clone")
	require.NotEmpty(t, h.reported, "and the target hears where its branch is")
	assert.Equal(t, revParseMain(t, h.repoDir), h.reported[len(h.reported)-1].Revision)
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

// TestRefresh_DoesNotReportThePreviousRepositoryAfterARepoint is the whole hazard of a worker that
// outlives the repository it was built for.
//
// spec.url is immutable, so pointing a GitTarget somewhere else means deleting and recreating the
// GitProvider — and the worker, keyed by (provider name, branch), survives that untouched. Its
// cached observation is then a fact about a repository this target no longer uses, and the refresh
// path is where it would be published: it reports what is known before doing any work.
func TestRefresh_DoesNotReportThePreviousRepositoryAfterARepoint(t *testing.T) {
	first := newRefreshHarness(t, "repoint-first")
	second := newLedgerFixture(t, "repoint-second", true)

	first.publish("written-to-the-first-repository")
	oldRevision := revParseMain(t, first.repoDir)
	require.NotEmpty(t, oldRevision)

	// The GitProvider is recreated pointing at a different repository. Nothing restarts the
	// worker; the only thing that changes is what its provider says.
	var provider configv1alpha3.GitProvider
	require.NoError(t, first.worker.Client.Get(first.worker.ctx,
		client.ObjectKey{Name: first.worker.GitProviderRef, Namespace: "default"}, &provider))
	provider.Spec.URL = second.sim.RepoURL
	require.NoError(t, first.worker.Client.Update(first.worker.ctx, &provider))

	first.reported = nil
	first.refresh(time.Hour) // an age that would reuse the cached observation, if anything did

	require.NotEmpty(t, first.reported, "the target still has to be told something")
	last := first.reported[len(first.reported)-1]
	assert.NotEqual(t, oldRevision, last.Revision,
		"a revision from the repository this target no longer points at must never be published")
	assert.Equal(t, revParseMain(t, second.repoDir), last.Revision,
		"what is published is where the branch is on the repository it points at NOW")
}

// TestRefresh_AnAbsentCheckoutHonoursTheInterval. A GitTarget that has been declared but has not
// published has a worker and no clone, and it is the target most likely to be left idle for a
// long time. Nothing local can be behind an observation when there is nothing local at all, so
// its interval has to be worth the same as everybody else's: keying the cheap path on base trust
// alone sent this target to the remote on EVERY reconcile, whatever the operator configured.
func TestRefresh_AnAbsentCheckoutHonoursTheInterval(t *testing.T) {
	h := newRefreshHarness(t, "refresh-no-checkout-interval")

	require.Equal(t, int64(1), h.refresh(time.Hour),
		"the first tick has nothing recorded, so it asks the remote")

	assert.Zero(t, h.refresh(time.Hour),
		"and the second, inside the interval, must cost nothing: there is no checkout to be stale")
}

// TestRefresh_AnAbsentBranchHonoursTheInterval is the same rule for the other target that never
// gains base trust: the branch is not on the remote, so no fetch ever resets onto it. The
// checkout exists here and is exactly as current as the observation — an unborn branch and "no
// revision" are the same fact — so a tick inside the interval has nothing to ask.
func TestRefresh_AnAbsentBranchHonoursTheInterval(t *testing.T) {
	h := newRefreshHarnessOn(t, "refresh-absent-interval", false)
	require.NoError(t, h.worker.ensureRepositoryInitialized(h.worker.ctx))

	require.Equal(t, int64(1), h.refresh(time.Nanosecond), "a stale observation asks the remote")

	assert.Zero(t, h.refresh(time.Hour),
		"a branch the remote does not carry still gets the interval it was configured with")
}

// TestRefresh_WithdrawsThePublishedObservationAfterAFailedRepoint is the hazard a dropped
// observation does not on its own close.
//
// Dropping it stops the OLD repository's revision being reported again, which is what the worker
// owns. It says nothing about the copy already published — in the watch plane's projection and in
// status.remote — and if the repository this target now points at cannot be read, nothing ever
// replaces it: the operator goes on reading a revision that lives in a repository the target no
// longer uses. So the refresh withdraws it explicitly, and keeps withdrawing until something is
// actually proved, because every GitTarget on this branch has to hear it on its own tick.
func TestRefresh_WithdrawsThePublishedObservationAfterAFailedRepoint(t *testing.T) {
	h := newRefreshHarness(t, "repoint-unreachable")
	h.publish("written-to-the-first-repository")
	require.NotEmpty(t, revParseMain(t, h.repoDir))

	var provider configv1alpha3.GitProvider
	require.NoError(t, h.worker.Client.Get(h.worker.ctx,
		client.ObjectKey{Name: h.worker.GitProviderRef, Namespace: "default"}, &provider))
	// Recreated pointing at a repository nothing answers for.
	provider.Spec.URL = "http://127.0.0.1:1/unreachable.git"
	require.NoError(t, h.worker.Client.Update(h.worker.ctx, &provider))

	h.reported = nil
	h.refresh(time.Hour) // an age that would reuse the cached observation, if anything did

	require.Len(t, h.reported, 1, "the target has to be told, and the failed look proves nothing")
	assert.True(t, h.reported[0].Withdrawn,
		"what is published names a revision in a repository this target no longer points at")
	assert.Empty(t, h.reported[0].Revision, "a withdrawal carries nothing to publish")

	h.reported = nil
	h.refresh(time.Hour)
	require.Len(t, h.reported, 1)
	assert.True(t, h.reported[0].Withdrawn,
		"it stands until something is proved: a sibling target's first tick must hear it too")
}

// TestRefresh_StopsWithdrawingOnceSomethingIsProved is the other side of that latch. A withdrawal
// is a correction, not a state to live in: the first successful look at the new repository
// replaces it with a real observation.
func TestRefresh_StopsWithdrawingOnceSomethingIsProved(t *testing.T) {
	first := newRefreshHarness(t, "repoint-recovers-first")
	second := newLedgerFixture(t, "repoint-recovers-second", true)
	first.publish("written-to-the-first-repository")

	var provider configv1alpha3.GitProvider
	require.NoError(t, first.worker.Client.Get(first.worker.ctx,
		client.ObjectKey{Name: first.worker.GitProviderRef, Namespace: "default"}, &provider))
	provider.Spec.URL = second.sim.RepoURL
	require.NoError(t, first.worker.Client.Update(first.worker.ctx, &provider))

	first.reported = nil
	first.refresh(time.Hour)

	require.NotEmpty(t, first.reported)
	last := first.reported[len(first.reported)-1]
	assert.False(t, last.Withdrawn, "the new repository answered, so there is something to publish")
	assert.Equal(t, revParseMain(t, second.repoDir), last.Revision)
	assert.False(t, first.worker.observationWithdrawn(), "and nothing is left to take back")
}

// TestRefresh_DoesNotRepublishTheLayoutOfARefusedFolder. Both write paths consult the acceptance
// gate before publishing a layout, because a folder the operator has refused to manage is one we
// should be making no claims about. The refresher rescans the same folder from outside a write,
// so it has to hold the same line — while still not RAISING the refusal, which is the boundary
// rescanLayoutForTarget is built on.
func TestRefresh_DoesNotRepublishTheLayoutOfARefusedFolder(t *testing.T) {
	h := newRefreshHarness(t, "refresh-refused-folder")

	var layouts []LayoutReport
	h.worker.layoutReporter = func(_ itypes.ResourceReference, report LayoutReport) {
		layouts = append(layouts, report)
	}

	h.publish("prime")
	// Somebody adds a second copy of a resource the folder already declares: one resource, two
	// files, which is a refusal the gate raises off the structure alone.
	h.contend("team-a/duplicate.yaml", duplicateOfPrime)
	layouts = nil

	h.refresh(time.Nanosecond)

	assert.Empty(t, layouts, "a refused folder's shape must not be republished from a refresh")
}

// TestEnqueueRefresh_CountsTheDropOnAFullQueue. Dropping a refresh is safe — the next reconcile
// asks again — but a worker saturated for long enough stops topping up status.remote and stops
// re-reading the folder, and an uncounted drop makes that invisible.
func TestEnqueueRefresh_CountsTheDropOnAFullQueue(t *testing.T) {
	reader, err := telemetry.InitTestExporter()
	require.NoError(t, err)

	h := newRefreshHarness(t, "refresh-queue-drop")
	for len(h.worker.eventQueue) < cap(h.worker.eventQueue) {
		h.worker.eventQueue <- WorkItem{}
	}

	h.worker.EnqueueRefresh(&RefreshRequest{Target: h.target, Path: h.path, MaxAge: time.Hour})

	labels := map[string]string{
		"provider_namespace": h.worker.GitProviderNamespace,
		"provider_name":      h.worker.GitProviderRef,
		"branch":             h.worker.Branch,
		"kind":               queueDropRefresh,
	}
	drops, ok := telemetry.CollectInt64Sum(reader, queueDropsMetric, labels)
	require.True(t, ok, "a dropped refresh must be counted like any other lost work item")
	assert.Equal(t, int64(1), drops)
}

// duplicateOfPrime is a second copy of the ConfigMap the harness publishes as "prime", in a file
// of its own: one resource in two files, which the acceptance gate refuses off the structure.
const duplicateOfPrime = `apiVersion: v1
kind: ConfigMap
metadata:
  name: prime
  namespace: default
data:
  key: prime
`

// TestEnqueueRefresh_IgnoresANilRequest. The enqueue is called from a reconcile, which is the one
// place a nil could arrive, and dropping it there would leak an inflight item and hold the queue
// depth gauge up forever.
func TestEnqueueRefresh_IgnoresANilRequest(t *testing.T) {
	h := newRefreshHarness(t, "refresh-nil-request")

	h.worker.EnqueueRefresh(nil)

	assert.Zero(t, h.worker.inflightItems.Load(), "nothing was queued, so nothing is in flight")
	assert.Empty(t, h.worker.eventQueue)
}

// TestRefresh_SkipsWhenTheGitProviderCannotBeRead is the first exit, and the order matters: the
// provider is read BEFORE anything is reported, because every statement below it is about a
// repository only the provider can name.
func TestRefresh_SkipsWhenTheGitProviderCannotBeRead(t *testing.T) {
	h := newRefreshHarness(t, "refresh-no-provider")
	h.publish("prime")

	var provider configv1alpha3.GitProvider
	require.NoError(t, h.worker.Client.Get(h.worker.ctx,
		client.ObjectKey{Name: h.worker.GitProviderRef, Namespace: "default"}, &provider))
	require.NoError(t, h.worker.Client.Delete(h.worker.ctx, &provider))

	h.reported = nil
	connections := h.refresh(time.Nanosecond)

	assert.Zero(t, connections, "there is no URL to ask")
	assert.Empty(t, h.reported, "and nothing may be published against a repository we cannot name")
}

// TestRefresh_CannotRescanAFolderWithNoCheckout. The rescan is best-effort by design: it is a
// read that improves what an operator sees, and a target with no clone yet simply has no folder
// to read. It must log and return rather than fail the tick that has just proved where the
// branch is.
func TestRefresh_CannotRescanAFolderWithNoCheckout(t *testing.T) {
	h := newRefreshHarness(t, "refresh-rescan-no-checkout")

	var layouts []LayoutReport
	h.worker.layoutReporter = func(_ itypes.ResourceReference, report LayoutReport) {
		layouts = append(layouts, report)
	}

	h.refresh(time.Nanosecond)

	require.NotEmpty(t, h.reported, "the remote was still observed")
	assert.Empty(t, layouts, "and no layout is claimed for a folder that is not on disk")
}

// TestRefresh_WithNoPathScansNothing. spec.path is what scopes the rescan, and a target without
// one has no folder of its own to resolve.
func TestRefresh_WithNoPathScansNothing(t *testing.T) {
	h := newRefreshHarness(t, "refresh-no-path")

	var layouts []LayoutReport
	h.worker.layoutReporter = func(_ itypes.ResourceReference, report LayoutReport) {
		layouts = append(layouts, report)
	}
	h.publish("prime")
	layouts = nil

	h.loop.handleRefreshRequest(&RefreshRequest{Target: h.target, MaxAge: time.Nanosecond})

	assert.Empty(t, layouts)
}
