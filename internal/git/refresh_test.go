// SPDX-License-Identifier: Apache-2.0

package git

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing/object"

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
	// scanVerdicts records every folder-scan verdict reported out of the harness; a nil entry is
	// a folder that passed.
	scanVerdicts []*manifestanalyzer.AcceptanceRefusedError
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
	h.worker.remoteReporter = func(observed RemoteObservation) {
		h.reported = append(h.reported, observed)
	}
	h.worker.scanAcceptance = func(_ itypes.ResourceReference, refused *manifestanalyzer.AcceptanceRefusedError) {
		h.scanVerdicts = append(h.scanVerdicts, refused)
	}
	return h
}

// lastScanVerdict is what the folder scan last published: nil for a folder that can be written.
func (h *refreshHarness) lastScanVerdict(t *testing.T) *manifestanalyzer.AcceptanceRefusedError {
	t.Helper()
	require.NotEmpty(t, h.scanVerdicts, "every scan publishes a verdict, including a clean one")
	return h.scanVerdicts[len(h.scanVerdicts)-1]
}

// removeFromRemote deletes a file from the branch the way the human who fixes a broken folder
// does: another writer's push, which produces no Kubernetes event at all.
func (h *refreshHarness) removeFromRemote(file string) {
	h.t.Helper()
	clientPath := filepath.Join(h.t.TempDir(), "client-remove")
	repo, worktree := initLocalRepo(h.t, clientPath, h.sim.RepoURL, "main")
	require.NoError(h.t, os.Remove(filepath.Join(clientPath, file)))
	_, err := worktree.Add(file)
	require.NoError(h.t, err)
	_, err = worktree.Commit("Client removes "+file, &gogit.CommitOptions{
		Author: &object.Signature{Name: "Client", Email: "client@example.com", When: time.Now()},
	})
	require.NoError(h.t, err)
	require.NoError(h.t, repo.Push(&gogit.PushOptions{
		RefSpecs: []config.RefSpec{config.RefSpec("refs/heads/main:refs/heads/main")},
	}))
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

	require.NotEmpty(t, h.reported)
	assert.Equal(t, ObservedByPush, h.reported[len(h.reported)-1].By,
		"the push proved where the branch is, and delivered it to the branch as it did so")

	h.reported = nil
	connections := h.refresh(time.Hour)

	assert.Zero(t, connections, "a push inside the age is the refresh; nothing may be spent on top of it")
	assert.Empty(t, h.reported,
		"and nothing is re-delivered: the fact reached every target on the branch when it was proved")
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
	assert.Empty(t, h.reported,
		"and it proves nothing, so it delivers nothing: what is already known about the branch "+
			"was delivered when it was proved, and every target on the branch has had it since")
	assert.Len(t, h.loop.pendingWrites, 1, "and the retained write is still there")
}

// TestRefresh_WritesNothing is the boundary as a test rather than a rule: a fresh look at Git
// reports what it read and mirrors none of it.
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

// TestRemoteObservation_CarriesTheRepositoryItWasProvedAgainst. A revision means nothing without
// the repository it is in, and the layer that publishes it — the GitTarget reconcile — holds no
// worker. Stamping the identity on the observation is what lets that layer compare it against the
// repository the GitProvider names NOW, and take back a revision from one the target has left.
func TestRemoteObservation_CarriesTheRepositoryItWasProvedAgainst(t *testing.T) {
	f := newLedgerFixture(t, "observation-identity", true)
	f.publish("prime")

	observed, ok := f.worker.LastRemoteObservation()
	require.True(t, ok)
	require.NotEmpty(t, observed.Revision)
	assert.Equal(t, f.worker.repo, observed.Repo,
		"a push proves where the branch is on THIS worker's repository, and says which one that is")
	assert.Equal(t, f.sim.RepoURL, observed.Repo.URL)
}

// TestRefresh_AsksNothingWhileItsGitProviderNamesAnotherRepository is the read-path twin of
// TestRepoint_TheWorkerStopsRatherThanWriteWithTheNewProvidersCredentials. The GitProvider is
// repointed under a live worker, and the refresh reads it for the credentials its look would use.
// It must not go looking with them: pointed at its own remote they are the wrong credentials for
// that host, and pointed at the new remote the answer would describe a repository this worker's
// checkout knows nothing about.
//
// So the tick costs nothing and reports nothing, and what was already published is taken back by
// the reconcile from the identity the stored observation carries: see publishRemote.
func TestRefresh_AsksNothingWhileItsGitProviderNamesAnotherRepository(t *testing.T) {
	first := newRefreshHarness(t, "repoint-first")
	second := newLedgerFixture(t, "repoint-second", true)

	first.publish("written-to-the-first-repository")
	require.NotEmpty(t, revParseMain(t, first.repoDir))

	// The GitProvider now names a different repository. Nothing restarts the worker.
	var provider configv1alpha3.GitProvider
	require.NoError(t, first.worker.Client.Get(first.worker.ctx,
		client.ObjectKey{Name: first.worker.GitProviderRef, Namespace: "default"}, &provider))
	provider.Spec.URL = second.sim.RepoURL
	require.NoError(t, first.worker.Client.Update(first.worker.ctx, &provider))

	first.reported = nil
	connections := first.refresh(time.Nanosecond) // an age that would otherwise force the look

	assert.Zero(t, connections, "a worker waiting to be replaced asks no remote anything")
	assert.Empty(t, first.reported,
		"and publishes nothing: the reconcile removes the old revision from the identity it carries")
}

// TestRemoteObservation_KeepsCarryingItsRepositoryAcrossAPush. The stamp is what lets the layer
// that publishes an observation decide whether it still describes the repository the GitTarget
// points at, so it has to survive the producer that writes it most often.
func TestRemoteObservation_KeepsCarryingItsRepositoryAcrossAPush(t *testing.T) {
	f := newLedgerFixture(t, "observation-identity-push", true)
	f.publish("first")
	f.publish("second")

	observed, ok := f.worker.LastRemoteObservation()
	require.True(t, ok)
	assert.Equal(t, ObservedByPush, observed.By)
	assert.Equal(t, f.worker.repo, observed.Repo)
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

// TestRefresh_PublishesARefusalSomebodyElsePushed is the gap this closes. A folder is broken by
// another writer's push, which produces NO Kubernetes event, so nothing noticed until the next
// live edit arrived — and that edit was refused and its events dropped. A read finds it on a tick
// that was happening anyway.
//
// The layout is not republished alongside it, which is the order both write paths use: a folder we
// have just refused is one to make no shape claims about.
func TestRefresh_PublishesARefusalSomebodyElsePushed(t *testing.T) {
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
	h.scanVerdicts = nil

	h.refresh(time.Nanosecond)

	refused := h.lastScanVerdict(t)
	require.NotNil(t, refused, "the scan found content that cannot be written, and must say so")
	assert.Contains(t, refused.Error(), "prime", "the refusal names what it is about")
	assert.Empty(t, layouts, "a refused folder's shape must not be republished from a refresh")
}

// TestRefresh_PublishesTheRecoveryWhenTheFolderIsFixed. The refusal is only half of it: a human
// fixes the folder with another push, which is again no Kubernetes event, so the same read that
// found the breakage has to be what reports it gone.
func TestRefresh_PublishesTheRecoveryWhenTheFolderIsFixed(t *testing.T) {
	h := newRefreshHarness(t, "refresh-refusal-recovers")

	h.publish("prime")
	h.contend("team-a/duplicate.yaml", duplicateOfPrime)
	h.refresh(time.Nanosecond)
	require.NotNil(t, h.lastScanVerdict(t), "precondition: the folder is refused")

	// The human removes the duplicate.
	h.removeFromRemote("team-a/duplicate.yaml")
	h.scanVerdicts = nil

	h.refresh(time.Nanosecond)

	assert.Nil(t, h.lastScanVerdict(t), "the folder can be written again, and only a read can say so")
}

// TestRefresh_WritesNothingWhileTheFolderIsRefused is the boundary, stated against the change that
// made the refresher publish a refusal at all. Raising GitPathAccepted=False drives a recheck and
// a resync, and that recovery machinery is the point — but the refresher itself must not commit,
// not even the empty commit spec.onRefusal produces, which belongs to a path that had work in
// hand.
func TestRefresh_WritesNothingWhileTheFolderIsRefused(t *testing.T) {
	h := newRefreshHarness(t, "refresh-refused-writes-nothing")
	h.publish("prime")
	h.contend("team-a/duplicate.yaml", duplicateOfPrime)
	head := revParseMain(t, h.repoDir)

	h.refresh(time.Nanosecond)

	assert.Equal(t, head, revParseMain(t, h.repoDir),
		"the branch must be exactly where the other writer left it")
	assert.Empty(t, h.loop.pendingWrites, "and nothing may be retained for a later push")
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
