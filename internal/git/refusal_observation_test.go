// SPDX-License-Identifier: Apache-2.0

package git

// Deduplicating one standing refusal, from the review that found the loop.
//
// The rate limit alone only decides how OFTEN a refusal may commit, never whether the refusal is
// the same one. A forced recheck re-lists from the API server every minute, so a refused live edit
// that nobody corrects is observed again and again, and each observation used to be another
// trigger: an empty commit per interval, for ever, waking every reconciler on the branch each
// time. These pin that a repeat is recognised as a repeat, and — just as important — that a real
// second edit is not mistaken for one.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// standingRefusalFixture is a real HTTP Git remote holding the diamond, and a target that asked
// for the empty commit. The diamond refuses an image bump at the write boundary (two render roots
// reach the shared base), which is the refusal the action exists for.
func standingRefusalFixture(t *testing.T, slug string) *ledgerFixture {
	t.Helper()
	f := newLedgerFixture(t, slug, false)
	seedDiamondInRemote(t, f.sim.RepoURL, "main")
	f.createLedgerTarget("", nil)

	target := &configv1alpha3.GitTarget{}
	require.NoError(t, f.worker.Client.Get(f.worker.ctx,
		client.ObjectKey{Name: ledgerTargetName, Namespace: "default"}, target))
	target.Spec.OnRefusal = configv1alpha3.RefusalActionPushEmptyCommit
	require.NoError(t, f.worker.Client.Update(f.worker.ctx, target))

	f.worker.mapper = deploymentMapper()
	return f
}

// refuseResync runs one refused resync through the event loop and pushes whatever it retained,
// exactly as the loop would. image is the live value being mirrored; two calls with the same image
// are two observations of one unchanged refusal.
func refuseResync(t *testing.T, f *ledgerFixture, l *branchWorkerEventLoop, image string) {
	t.Helper()
	// Rewind the rate-limit stamp rather than sleeping a minute: the question here is what happens
	// once the window is open, and the window itself has its own tests.
	f.worker.refusalTouchMu.Lock()
	if f.worker.lastRefusalTouch == nil {
		f.worker.lastRefusalTouch = map[string]time.Time{}
	}
	f.worker.lastRefusalTouch["default/"+ledgerTargetName] = time.Now().Add(-2 * refusalTouchInterval)
	f.worker.refusalTouchMu.Unlock()
	l.lastPushAt = time.Now()

	event := overridesDeploymentEvent(image, 3)
	result := make(chan ResyncResult, 1)
	l.applyResync(&ResyncRequest{
		GitTargetName:      ledgerTargetName,
		GitTargetNamespace: "default",
		// Held constant on purpose: a re-list gives every snapshot a fresh collection version, so
		// a version is not what tells one observation from another.
		ResourceVersion: "unchanged-123",
		Desired: []manifestanalyzer.DesiredResource{
			{Resource: event.Identifier, Object: event.Object},
		},
		Result: result,
	})
	var refused *manifestanalyzer.AcceptanceRefusedError
	require.ErrorAs(t, (<-result).Err, &refused, "the diamond must refuse this write at the boundary")

	require.NoError(t, f.worker.pushPendingCommits(l.pendingWrites))
	l.pendingWrites = nil
	l.pendingWritesBytes = 0
}

func remoteCommits(t *testing.T, f *ledgerFixture) int {
	t.Helper()
	count, err := strconv.Atoi(gitOut(t, f.repoDir, "rev-list", "--count", "main"))
	require.NoError(t, err)
	return count
}

// TestRefusalTouch_AnUnchangedRefusalIsNotASecondTrigger is the regression a review reproduced:
// three rechecks of one refused edit, each with the window open, produced three commits.
func TestRefusalTouch_AnUnchangedRefusalIsNotASecondTrigger(t *testing.T) {
	f := standingRefusalFixture(t, "standing-refusal-unchanged")
	l := newBranchWorkerEventLoop(f.worker, time.Hour)
	t.Cleanup(l.stopTimers)

	before := remoteCommits(t, f)
	for range 3 {
		refuseResync(t, f, l, "ghcr.io/example/podinfo:9.9.9")
	}

	assert.Equal(t, 1, remoteCommits(t, f)-before,
		"one unchanged refusal earns one commit, however often it is re-observed")
}

// TestRefusalTouch_ASecondRefusedEditEarnsItsOwnCommit is the positive control, and the reason the
// fence keys on the observation rather than simply latching after the first commit.
//
// A second live edit is a second thing to revert. The reconcile the first commit triggered ran
// against the first edit and has nothing to say about this one, so it needs a commit of its own.
func TestRefusalTouch_ASecondRefusedEditEarnsItsOwnCommit(t *testing.T) {
	f := standingRefusalFixture(t, "standing-refusal-changed")
	l := newBranchWorkerEventLoop(f.worker, time.Hour)
	t.Cleanup(l.stopTimers)

	before := remoteCommits(t, f)
	refuseResync(t, f, l, "ghcr.io/example/podinfo:9.9.9")
	refuseResync(t, f, l, "ghcr.io/example/podinfo:9.9.9")
	refuseResync(t, f, l, "ghcr.io/example/podinfo:10.0.0")

	assert.Equal(t, 2, remoteCommits(t, f)-before,
		"a genuinely different refused edit must still move the branch")
}

// TestRefusalObservation_IsContentAddressedAndOrderIndependent pins the digest itself: same
// objects and issues, same observation, whatever order they arrive in.
func TestRefusalObservation_IsContentAddressedAndOrderIndependent(t *testing.T) {
	object := func(name, image string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]interface{}{"name": name, "namespace": "default"},
			"spec":       map[string]interface{}{"image": image},
		}}
	}
	refused := &manifestanalyzer.AcceptanceRefusedError{
		Issues: []manifestanalyzer.AcceptanceIssue{
			{Kind: manifestanalyzer.IssueWriteFanIn, Path: "base/deployment.yaml"},
		},
	}
	web, api := object("web", "v1"), object("api", "v1")

	first := refusalObservation([]*unstructured.Unstructured{web, api}, refused)
	assert.NotEmpty(t, first)
	assert.Equal(t, first, refusalObservation([]*unstructured.Unstructured{api, web}, refused),
		"the same set observed in a different order is the same observation")
	assert.NotEqual(t, first, refusalObservation(
		[]*unstructured.Unstructured{object("web", "v2"), api}, refused),
		"a changed value is a new observation")
	assert.NotEqual(t, first, refusalObservation([]*unstructured.Unstructured{web}, refused),
		"a smaller batch is a new observation")

	// A different refusal about the same objects is also new: the folder changed under the edit,
	// and what the previous commit asked for was answered against the old one.
	elsewhere := &manifestanalyzer.AcceptanceRefusedError{
		Issues: []manifestanalyzer.AcceptanceIssue{
			{Kind: manifestanalyzer.IssueWriteFanIn, Path: "overlays/prod/deployment.yaml"},
		},
	}
	assert.NotEqual(t, first, refusalObservation([]*unstructured.Unstructured{web, api}, elsewhere))

	assert.Empty(t, refusalObservation(nil, nil),
		"nothing to digest is not an observation, and must never count as covered")
}

// TestRefusalObservation_NothingIsCoveredUntilACommitIsMade. The memory is written where the
// commit is, so a build or commit failure leaves the next observation free to try again.
func TestRefusalObservation_NothingIsCoveredUntilACommitIsMade(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{
		OnRefusal: configv1alpha3.RefusalActionPushEmptyCommit,
	})

	assert.False(t, w.refusalAlreadyCovered(editingRef(), "observation-1"))
	w.recordRefusalObservation(editingRef(), "observation-1")
	assert.True(t, w.refusalAlreadyCovered(editingRef(), "observation-1"))
	assert.False(t, w.refusalAlreadyCovered(editingRef(), "observation-2"),
		"a different observation is never covered by this one")
	assert.False(t, w.refusalAlreadyCovered(itypes.NewResourceReference("other", "team-a"), "observation-1"),
		"the memory is per target, like the rate limit and the consent check")

	// An unidentifiable observation must never be treated as covered, in either direction.
	w.recordRefusalObservation(editingRef(), "")
	assert.False(t, w.refusalAlreadyCovered(editingRef(), ""))
	assert.True(t, w.refusalAlreadyCovered(editingRef(), "observation-1"),
		"and it must not overwrite what is covered")
}

// TestRefusalObservation_AnAcceptedWriteMakesTheNextRefusalNew is the recovery half.
//
// Without it the fence would be a permanent mute: a user re-making the same live edit after the
// reconciler reverted it produces byte-identical content, so only the acceptance in between can
// tell the second edit from the first.
func TestRefusalObservation_AnAcceptedWriteMakesTheNextRefusalNew(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{
		OnRefusal: configv1alpha3.RefusalActionPushEmptyCommit,
	})
	w.recordRefusalObservation(editingRef(), "observation-1")
	require.True(t, w.refusalAlreadyCovered(editingRef(), "observation-1"))

	w.clearRefusalObservationFor("editing", "team-a")

	assert.False(t, w.refusalAlreadyCovered(editingRef(), "observation-1"),
		"an accepted write ends the refusal the last commit covered")

	// An unattributable write clears nothing rather than clearing a key no GitTarget reads.
	w.recordRefusalObservation(editingRef(), "observation-1")
	w.clearRefusalObservationFor("editing", "")
	assert.True(t, w.refusalAlreadyCovered(editingRef(), "observation-1"))
}

// TestRefusalTouch_RemovingOneDocumentFromASharedFileIsNotCommittedFor is the second regression
// the same review found, and it runs through the real planner rather than a hand-built issue.
//
// A DELETE of one document out of a two-document file leaves the file present, so the old
// file-level evidence answered "this is an edit to a surviving document" and the refusal qualified
// for an empty commit. It is the opposite of an edit: the refusal means Git still holds the
// document, and a commit asking the reconciler to re-apply would put back an object somebody
// deliberately deleted from the cluster.
func TestRefusalTouch_RemovingOneDocumentFromASharedFileIsNotCommittedFor(t *testing.T) {
	writer := newContentWriter(itypes.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem().Root()
	seedDiamond(t, root)

	// A second document joins the shared base file, so removing the first one cannot empty it.
	shared := diamondDeploymentYAML + "---\n" + strings.ReplaceAll(diamondDeploymentYAML, "web", "web2")
	require.NoError(t, os.WriteFile(filepath.Join(root, "base", "deployment.yaml"), []byte(shared), 0o600))

	event := overridesDeploymentEvent("ghcr.io/example/podinfo:1.0.0", 3)
	event.Operation = "DELETE"
	w := &BranchWorker{contentWriter: writer, mapper: deploymentMapper()}
	_, err := w.flushEventsToWorktree(
		t.Context(), worktree, "", []Event{event}, nil, namespacePolicy{}, configv1alpha3.PruneOnEvent)

	var refused *manifestanalyzer.AcceptanceRefusedError
	require.ErrorAs(t, err, &refused, "the diamond must refuse this write at the boundary")
	assert.False(t, refusalIsAWriteBoundary(refused),
		"a removal must not qualify because a sibling document keeps the file alive: %+v", refused.Issues)
}

// TestRefusalTouch_ARemovalAnywhereInTheFlushDisqualifiesIt covers the batch-wide arm of the same
// fence, which the sweep path needs: a mark-and-sweep resync can drop one file's document while
// refusing an edit to another, and the flush is all-or-nothing, so the commit it would earn wakes
// the reconciler over a folder whose deletion did not happen.
func TestRefusalTouch_ARemovalAnywhereInTheFlushDisqualifiesIt(t *testing.T) {
	wb := &writeBatch{
		buffers: map[string]*fileBuffer{},
		contentByPath: map[string][]byte{
			"edited.yaml": []byte("apiVersion: v1\nkind: ConfigMap\n"),
			"swept.yaml":  []byte("apiVersion: v1\nkind: ConfigMap\n"),
		},
	}
	edited := wb.buffer("edited.yaml")
	require.True(t, wb.holdsExistingDocument(edited),
		"an edit to a document the folder holds is what the action exists for")

	wb.recordDocumentRemoval(wb.buffer("swept.yaml"))

	assert.False(t, wb.holdsExistingDocument(edited),
		"a flush that removes a document anywhere earns no commit, whichever file was refused")
}

// TestRefusalTouch_AQueuedCommitIsDroppedOnceSomethingElseCoveredIt closes the gap between arming
// a trailing commit and running it.
//
// A minute passes between the two, and another commit for the same target can be made in it. If
// that commit covered the same observation, this entry is asking for a second one that says
// exactly the same thing to the same reconcilers, so it is dropped rather than run. Consent is
// re-checked at the same point and for the same reason: a delayed action must act on what is true
// when it fires.
func TestRefusalTouch_AQueuedCommitIsDroppedOnceSomethingElseCoveredIt(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{
		OnRefusal: configv1alpha3.RefusalActionPushEmptyCommit,
	})
	loop := newBranchWorkerEventLoop(w, time.Minute)
	t.Cleanup(loop.stopTimers)

	loop.armTrailingRefusalTouch(editingRef(), "refused while the window was closed", "observation-1", 0)
	require.Contains(t, loop.refusalPending, editingRef().String())

	// Whatever else ran in the meantime covered exactly this.
	w.recordRefusalObservation(editingRef(), "observation-1")

	loop.stopRefusalTimer()
	loop.flushPendingRefusalTouch()

	assert.Empty(t, loop.refusalPending, "the entry is consumed either way")
	limited, _ := w.refusalRateLimited(editingRef())
	assert.False(t, limited,
		"a covered entry must not spend the rate-limit window on a commit nobody needed")
}
