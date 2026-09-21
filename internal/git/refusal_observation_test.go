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

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

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
	scope := deploymentResyncScope()
	result := make(chan ResyncResult, 1)
	l.applyResync(&ResyncRequest{
		GitTargetName:      ledgerTargetName,
		GitTargetNamespace: "default",
		Scope:              &scope,
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

// deploymentResyncScope is the per-type reconcile a refused Deployment arrives on. Every refusal
// in these tests is filed under a cell, because that is what the real path carries and what the
// dedupe is keyed by.
func deploymentResyncScope() ResyncScope {
	return ResyncScopeFor(
		schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, "default")
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

	assert.False(t, w.refusalAlreadyCovered(editingKey(), "observation-1"))
	w.recordRefusalObservation(editingKey(), "observation-1")
	assert.True(t, w.refusalAlreadyCovered(editingKey(), "observation-1"))
	assert.False(t, w.refusalAlreadyCovered(editingKey(), "observation-2"),
		"a different observation is never covered by this one")
	other := refusalKeyFor(itypes.NewResourceReference("other", "team-a"), "deployments")
	assert.False(t, w.refusalAlreadyCovered(other, "observation-1"),
		"the memory is per target, like the rate limit and the consent check")

	// An unidentifiable observation must never be treated as covered, in either direction.
	w.recordRefusalObservation(editingKey(), "")
	assert.False(t, w.refusalAlreadyCovered(editingKey(), ""))
	assert.True(t, w.refusalAlreadyCovered(editingKey(), "observation-1"),
		"and it must not overwrite what is covered")
}

// TestRefusalObservation_RecoveryIsScopedToTheCellThatRecovered is the recovery half, and the
// regression a review found in it.
//
// Without any recovery the fence would be a permanent mute: a user re-making the same live edit
// after the reconciler reverted it produces byte-identical content, so only the acceptance in
// between can tell the second edit from the first. But recovery that clears the whole TARGET is
// the loop coming back by another door: a GitTarget watches several types, a no-op ConfigMap
// resync succeeds every time it runs, and the still-refused Deployment would be re-armed by every
// one of them. A successful evaluation may only speak for what it evaluated.
func TestRefusalObservation_RecoveryIsScopedToTheCellThatRecovered(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{
		OnRefusal: configv1alpha3.RefusalActionPushEmptyCommit,
	})
	loop := newBranchWorkerEventLoop(w, time.Minute)
	t.Cleanup(loop.stopTimers)

	deployments := editingKey()
	configMaps := refusalKeyFor(editingRef(), "configmaps")
	w.recordRefusalObservation(deployments, "observation-1")
	w.recordRefusalObservation(configMaps, "observation-2")

	loop.refusalRecovered(editingRef(), configMaps.cell)

	assert.True(t, w.refusalAlreadyCovered(deployments, "observation-1"),
		"one watched type succeeding is no evidence about another that is still refused")
	assert.False(t, w.refusalAlreadyCovered(configMaps, "observation-2"),
		"the cell that was accepted has nothing outstanding")

	loop.refusalRecovered(editingRef(), deployments.cell)
	assert.False(t, w.refusalAlreadyCovered(deployments, "observation-1"),
		"and once its own cell is accepted, the next refusal there is new again")

	// The ZERO cell is a whole-GitTarget evaluation, which does speak for every cell it holds.
	w.recordRefusalObservation(deployments, "observation-1")
	w.recordRefusalObservation(configMaps, "observation-2")
	loop.refusalRecovered(editingRef(), itypes.CellKey{})
	assert.False(t, w.refusalAlreadyCovered(deployments, "observation-1"))
	assert.False(t, w.refusalAlreadyCovered(configMaps, "observation-2"))
}

// TestRefusalObservation_RecoveryCancelsTheQueuedCommitForThatCell is the other half of the same
// regression.
//
// Dropping the digest is not enough: a commit already queued for that cell finds nothing covering
// it when its timer fires and moves the branch anyway, for a refusal that has since been accepted.
// The obligation has to go with the memory — and only that cell's obligation.
func TestRefusalObservation_RecoveryCancelsTheQueuedCommitForThatCell(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{
		OnRefusal: configv1alpha3.RefusalActionPushEmptyCommit,
	})
	loop := newBranchWorkerEventLoop(w, time.Minute)
	t.Cleanup(loop.stopTimers)

	deployments := editingKey()
	configMaps := refusalKeyFor(editingRef(), "configmaps")
	loop.armTrailingRefusalTouch(deployments, "the deployment was refused", "observation-1", 0)
	loop.armTrailingRefusalTouch(configMaps, "the configmap was refused", "observation-2", 0)
	require.Len(t, loop.refusalPending, 2)

	loop.refusalRecovered(editingRef(), deployments.cell)

	assert.NotContains(t, loop.refusalPending, deployments,
		"a queued commit must not survive the acceptance of the object it was queued for")
	assert.Contains(t, loop.refusalPending, configMaps,
		"and the cell that is still refused keeps its obligation")

	// Nothing was committed for the cancelled entry, so it must not have spent the window either.
	limited, _ := w.refusalRateLimited(editingRef())
	assert.False(t, limited)
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

	loop.armTrailingRefusalTouch(editingKey(), "refused while the window was closed", "observation-1", 0)
	require.Contains(t, loop.refusalPending, editingKey())

	// Whatever else ran in the meantime covered exactly this.
	w.recordRefusalObservation(editingKey(), "observation-1")

	loop.stopRefusalTimer()
	loop.flushPendingRefusalTouch()

	assert.Empty(t, loop.refusalPending, "the entry is consumed either way")
	limited, _ := w.refusalRateLimited(editingRef())
	assert.False(t, limited,
		"a covered entry must not spend the rate-limit window on a commit nobody needed")
}

// TestRefusalTouch_AnAcceptedSiblingTypeDoesNotRearmAStandingRefusal is the interaction a review
// found, driven through the real resync handler against a real Git remote.
//
// A GitTarget watches more than one type. The Deployment stays refused; a ConfigMap reconcile for
// the same target succeeds, as an empty or unchanged one does every time it runs. If that success
// is read as recovery for the whole target, the next identical Deployment observation becomes a
// new trigger, and one commit per ConfigMap pass is the loop again wearing a different hat.
func TestRefusalTouch_AnAcceptedSiblingTypeDoesNotRearmAStandingRefusal(t *testing.T) {
	f := standingRefusalFixture(t, "standing-refusal-sibling-type")
	l := newBranchWorkerEventLoop(f.worker, time.Hour)
	t.Cleanup(l.stopTimers)

	before := remoteCommits(t, f)
	refuseResync(t, f, l, "ghcr.io/example/podinfo:9.9.9")
	require.Equal(t, before+1, remoteCommits(t, f), "the first refusal earns its commit")

	// A different watched cell, with nothing to write and nothing to refuse.
	configMaps := ResyncScopeFor(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, "default")
	result := make(chan ResyncResult, 1)
	l.applyResync(&ResyncRequest{
		GitTargetName: ledgerTargetName, GitTargetNamespace: "default",
		Scope: &configMaps, Result: result,
	})
	require.NoError(t, (<-result).Err, "an empty ConfigMap snapshot is accepted")
	require.NoError(t, f.worker.pushPendingCommits(l.pendingWrites))
	l.pendingWrites, l.pendingWritesBytes = nil, 0

	refuseResync(t, f, l, "ghcr.io/example/podinfo:9.9.9")

	assert.Equal(t, before+1, remoteCommits(t, f),
		"a ConfigMap reconcile succeeding says nothing about a Deployment that is still refused")
}

// TestRefusalTouch_AnEarlyRefusalStillSeesThePlannedRemoval is the third interaction.
//
// Both flush paths apply their upserts first and abort on the first refusal, so a sweep that would
// have removed a document is never reached — and the refusal, raised before it, used to carry
// evidence saying this flush removes nothing. The commit that evidence earns asks the reconciler
// to re-apply what Git holds, which recreates the object the sweep was about to delete.
//
// The shape: one Deployment whose args a kustomize patch and the live object both changed, which
// cannot be placed, and a second document absent from the snapshot, which PruneAlways sweeps.
func TestRefusalTouch_AnEarlyRefusalStillSeesThePlannedRemoval(t *testing.T) {
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem().Root()
	base := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: default
spec:
  selector:
    matchLabels:
      app: web
  template:
    metadata:
      labels:
        app: web
    spec:
      containers:
        - name: app
          image: ghcr.io/example/app:1.0.0
          args: ["--from-the-base"]
`
	for name, content := range map[string]string{
		"deployment.yaml": base,
		// Absent from the desired snapshot below, so the plan sweeps it.
		"orphan.yaml":        strings.ReplaceAll(base, "web", "orphan"),
		"kustomization.yaml": "resources:\n  - deployment.yaml\n  - orphan.yaml\npatches:\n  - path: patch.yaml\n",
		"patch.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: default
spec:
  template:
    spec:
      containers:
        - name: app
          args: ["--from-the-patch"]
`,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(content), 0o600))
	}

	var live map[string]interface{}
	require.NoError(t, yaml.Unmarshal([]byte(strings.ReplaceAll(base, "--from-the-base", "--from-live")), &live))
	desired := []manifestanalyzer.DesiredResource{{
		Resource: itypes.ResourceIdentifier{
			Group: "apps", Version: "v1", Resource: "deployments", Namespace: "default", Name: "web"},
		Object: &unstructured.Unstructured{Object: live},
	}}

	w := &BranchWorker{contentWriter: newContentWriter(itypes.SensitiveResourcePolicy{}), mapper: deploymentMapper()}
	_, _, err := w.applyResyncToWorktree(t.Context(), worktree, "",
		ResolvedTargetMetadata{PruneMode: configv1alpha3.PruneAlways, Namespace: "default", Name: "target"},
		desired, nil)

	var refused *manifestanalyzer.AcceptanceRefusedError
	require.ErrorAs(t, err, &refused, "the unplaceable edit must refuse the flush")
	require.Contains(t, issueKinds(refused.Issues), manifestanalyzer.IssueUnplaceableEdit)
	assert.False(t, refusalIsAWriteBoundary(refused),
		"the same flush planned to remove the orphan, even though the upsert refused before the sweep ran: %+v",
		refused.Issues)
}

// overlayRefusalFixture is the write-boundary shape the feature exists for, seeded into a real
// HTTP Git remote: a pure overlay whose Deployment comes from ../../base, where an edit to a
// base-owned field has nowhere in the overlay to land. Unlike the diamond, this shape ACCEPTS the
// rendered object unchanged, which is what lets a test drive a refusal and then a recovery.
func overlayRefusalFixture(t *testing.T, slug string) *ledgerFixture {
	t.Helper()
	f := newLedgerFixture(t, slug, false)

	seed := filepath.Join(t.TempDir(), "seed")
	repo, worktree := initLocalRepo(t, seed, f.sim.RepoURL, "main")
	seedOverlayWorktree(t, seed)
	_, err := worktree.Add(".")
	require.NoError(t, err)
	_, err = worktree.Commit("seed overlay", &gogit.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	require.NoError(t, err)
	require.NoError(t, repo.Push(&gogit.PushOptions{
		RefSpecs: []config.RefSpec{"refs/heads/main:refs/heads/main"},
	}))

	f.createLedgerTarget(overlayGitPath, nil)
	target := &configv1alpha3.GitTarget{}
	require.NoError(t, f.worker.Client.Get(f.worker.ctx,
		client.ObjectKey{Name: ledgerTargetName, Namespace: "default"}, target))
	target.Spec.OnRefusal = configv1alpha3.RefusalActionPushEmptyCommit
	require.NoError(t, f.worker.Client.Update(f.worker.ctx, target))
	f.worker.mapper = deploymentMapper()
	return f
}

// TestRefusalTouch_RecoveryCancelsACommitQueuedForTheSameCell is the handler-level regression for
// recovery: the same transition as the focused state test, but driven through the real applyResync
// hook against a real Git remote, so it holds the hook's PLACEMENT and not only the state it
// writes. It is the case a review reproduced.
//
// The sequence is the one an operator actually produces: an edit is refused and earns a commit, a
// second edit inside the rate-limit window queues a trailing commit, and then the object is put
// back to what the folder renders and is accepted. The queued commit is now asking the reconciler
// to re-apply on account of a refusal that no longer exists, so it must not fire.
func TestRefusalTouch_RecoveryCancelsACommitQueuedForTheSameCell(t *testing.T) {
	f := overlayRefusalFixture(t, "recovery-cancels-queued")
	l := newBranchWorkerEventLoop(f.worker, time.Hour)
	t.Cleanup(l.stopTimers)
	l.lastPushAt = time.Now()

	scope := ResyncScopeFor(
		schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, "production")
	key := refusalKey{
		target: itypes.NewResourceReference(ledgerTargetName, "default"),
		cell:   scope.Cell,
	}
	// observe mirrors one per-type reconcile. An empty logLevel is the object exactly as the
	// folder renders it; anything else is a base-owned field edit with nowhere to land.
	observe := func(logLevel string) error {
		event := overlayDeploymentEvent("ghcr.io/example/podinfo:6.4.0")
		if logLevel != "" {
			containers, _, err := unstructured.NestedSlice(
				event.Object.Object, "spec", "template", "spec", "containers")
			require.NoError(t, err)
			containers[0].(map[string]interface{})["env"] = []interface{}{
				map[string]interface{}{"name": "LOG_LEVEL", "value": logLevel},
			}
			require.NoError(t, unstructured.SetNestedSlice(
				event.Object.Object, containers, "spec", "template", "spec", "containers"))
		}
		result := make(chan ResyncResult, 1)
		l.applyResync(&ResyncRequest{
			GitTargetName: ledgerTargetName, GitTargetNamespace: "default", Scope: &scope,
			Desired: []manifestanalyzer.DesiredResource{{Resource: event.Identifier, Object: event.Object}},
			Result:  result,
		})
		err := (<-result).Err
		require.NoError(t, f.worker.pushPendingCommits(l.pendingWrites))
		l.pendingWrites, l.pendingWritesBytes = nil, 0
		return err
	}

	var refused *manifestanalyzer.AcceptanceRefusedError
	require.ErrorAs(t, observe("debug"), &refused, "a base-owned field edit has nowhere to land")
	require.ErrorAs(t, observe("trace"), &refused, "and so does a different value for it")
	require.Contains(t, l.refusalPending, key,
		"the second edit arrived inside the window, so it must be queued behind a trailing commit")

	require.NoError(t, observe(""), "the object is put back to what the folder renders, and accepted")
	require.NotContains(t, l.refusalPending, key,
		"accepting the object cancels the commit that was queued for its refusal")

	// Fire the timer as the loop would, with both clocks advanced past their deadlines.
	before := remoteCommits(t, f)
	f.worker.refusalTouchMu.Lock()
	f.worker.lastRefusalTouch["default/"+ledgerTargetName] = time.Now().Add(-2 * refusalTouchInterval)
	f.worker.refusalTouchMu.Unlock()
	l.stopRefusalTimer()
	l.flushPendingRefusalTouch()
	require.NoError(t, f.worker.pushPendingCommits(l.pendingWrites))

	assert.Equal(t, before, remoteCommits(t, f),
		"a queued refusal must not move the branch after the same object was accepted")
}
