// SPDX-License-Identifier: Apache-2.0

package git

// The empty commit that reverts a refused write, from
// docs/design/push-notification-and-reconcile-trigger.md, part 3 option 7.
//
// The commit itself is two lines; every interesting property is in the decision to make one. A
// refusal is not a one-off, the action is opt-in per GitTarget, and an operator who parked a
// target must not have it driving somebody else's reconciler. Those are what these tests pin.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// refusalTouchWorker builds a worker whose fake client holds one GitTarget with the given spec.
func refusalTouchWorker(t *testing.T, spec configv1alpha3.GitTargetSpec) *BranchWorker {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha3.AddToScheme(scheme))

	spec.GitProviderRef.Name = "test-provider"
	spec.Branch = "main"
	if spec.Path == "" {
		spec.Path = "apps"
	}
	target := &configv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "editing", Namespace: "team-a"},
		Spec:       spec,
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(target).Build()

	w := newMetricsTestWorker()
	w.Client = client
	w.Log = logr.Discard()
	w.ctx = context.Background()
	return w
}

func editingRef() itypes.ResourceReference {
	return itypes.NewResourceReference("editing", "team-a")
}

// TestRefusalTouch_OffByDefault. The action deletes objects where the reconciler prunes, so the
// default has to be the one that does nothing.
func TestRefusalTouch_OffByDefault(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{})

	assert.False(t, w.refusalConsent(w.ctx, editingRef()),
		"a GitTarget that did not ask for an empty commit must not get one")
}

// TestRefusalTouch_ExplicitIgnoreIsAlsoOff guards the enum rather than only the zero value.
func TestRefusalTouch_ExplicitIgnoreIsAlsoOff(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{
		OnRefusal: configv1alpha3.RefusalActionIgnore,
	})

	assert.False(t, w.refusalConsent(w.ctx, editingRef()))
}

// TestRefusalTouch_AllowedWhenAskedFor is the positive case.
func TestRefusalTouch_AllowedWhenAskedFor(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{
		OnRefusal: configv1alpha3.RefusalActionPushEmptyCommit,
	})

	assert.True(t, w.refusalConsent(w.ctx, editingRef()))
}

// TestRefusalTouch_SuspendedTargetIsNeverTouched. An empty commit is a write, and it drives
// another controller. Suspension has to stop the one write that reaches outside this operator.
func TestRefusalTouch_SuspendedTargetIsNeverTouched(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{
		OnRefusal: configv1alpha3.RefusalActionPushEmptyCommit,
		Suspend:   true,
	})

	assert.False(t, w.refusalConsent(w.ctx, editingRef()),
		"a suspended target must not drive the reconciler")
}

// TestRefusalTouch_RateLimitedPerTarget. A controller rewriting a base-owned field produces a
// refusal on every one of its reconciles; without a floor the branch fills with empty commits and
// every one of them wakes every reconciler watching it.
func TestRefusalTouch_RateLimitedPerTarget(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{
		OnRefusal: configv1alpha3.RefusalActionPushEmptyCommit,
	})

	limited, _ := w.refusalRateLimited(editingRef())
	require.False(t, limited, "the first refusal commits")

	limited, wait := w.refusalRateLimited(editingRef())
	assert.True(t, limited, "an immediate second one is rate limited")
	assert.Positive(t, wait, "a rate-limited refusal must say when it may be retried")
	assert.LessOrEqual(t, wait, refusalTouchInterval)

	// Age the record past the floor: the next refusal is allowed again.
	w.refusalTouchMu.Lock()
	w.lastRefusalTouch[editingRef().String()] = time.Now().Add(-2 * refusalTouchInterval)
	w.refusalTouchMu.Unlock()

	limited, _ = w.refusalRateLimited(editingRef())
	assert.False(t, limited, "the floor is a rate, not a one-shot")
}

// TestRefusalTouch_UnreadableTargetIsNotConsent. An unreadable GitTarget is missing evidence, not
// evidence of permission, and the action is destructive where the reconciler prunes.
func TestRefusalTouch_UnreadableTargetIsNotConsent(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{
		OnRefusal: configv1alpha3.RefusalActionPushEmptyCommit,
	})

	assert.False(t, w.refusalConsent(w.ctx, itypes.NewResourceReference("absent", "team-a")),
		"a GitTarget that cannot be read must not be treated as opted in")
}

// TestRefusalCommitMessage_ExplainsTheEmptyDiff. The message is the whole user-visible product of
// this feature for anyone reading git log, and an unexplained empty commit looks like a mistake.
func TestRefusalCommitMessage_ExplainsTheEmptyDiff(t *testing.T) {
	message := refusalCommitMessage(editingRef(), "unsupported folder content")

	assert.Contains(t, message, "team-a/editing")
	assert.Contains(t, message, "changes no file")
	assert.Contains(t, message, "unsupported folder content")
}

// TestRefusalTouch_LandsAnEmptyCommitOnTheRemote is the claim the whole option rests on: the
// branch moves, and no file changed. Both halves matter. A commit that changed something would be
// a write nobody asked for; a branch that did not move would trigger nothing, because a new
// revision is the entire mechanism (Argo CD's selfHeal=false skip is keyed on having already
// synced THIS revision, and Flux needs a new artifact revision).
//
// It runs over a real Git server rather than file://, because go-git's in-process receive-pack
// never compares cmd.Old, so a compare-and-swap assertion over file:// passes vacuously.
func TestRefusalTouch_LandsAnEmptyCommitOnTheRemote(t *testing.T) {
	f := newLedgerFixture(t, "refusal-touch", true)
	f.createLedgerTarget("team-a", nil)
	f.publish("before")

	beforeSHA, beforeTree := remoteHeadAndTree(t, f.repoDir, "main")

	write, err := f.worker.buildRefusalTouchWrite(
		f.worker.ctx, itypes.NewResourceReference("target-a", "default"), "unsupported folder content")
	require.NoError(t, err)

	batch := []PendingWrite{*write}
	require.NoError(t, f.worker.commitPendingWrites(batch, false))
	f.pending = append(f.pending, batch...)
	f.push()

	afterSHA, afterTree := remoteHeadAndTree(t, f.repoDir, "main")

	assert.NotEqual(t, beforeSHA, afterSHA, "the branch must move, or nothing is triggered")
	assert.Equal(t, beforeTree, afterTree, "the commit must change no file")
	assert.Equal(t, beforeSHA, parentOf(t, f.repoDir, afterSHA),
		"the empty commit must sit directly on the previous tip")
}

// remoteHeadAndTree reads the bare repository directly, so the assertion is about what the SERVER
// holds rather than about what our worker believes it pushed.
func remoteHeadAndTree(t *testing.T, repoDir, branch string) (string, string) {
	t.Helper()
	return gitOut(t, repoDir, "rev-parse", branch), gitOut(t, repoDir, "rev-parse", branch+"^{tree}")
}

func parentOf(t *testing.T, repoDir, sha string) string {
	t.Helper()
	return gitOut(t, repoDir, "rev-parse", sha+"^")
}

func gitOut(t *testing.T, repoDir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repoDir}, args...)...).CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// TestRefusalTouch_RateLimitedRefusalIsCoalescedNotDropped is the regression for a review finding.
//
// Dropping a rate-limited refusal is a correctness bug, not a missed optimisation. The reconcile
// the previous commit triggered may already have finished by the time the second refusal happens,
// so nothing would ever cover it and the live edit would stay in the cluster for good. The second
// refusal has to leave a trailing commit armed.
func TestRefusalTouch_RateLimitedRefusalIsCoalescedNotDropped(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{
		OnRefusal: configv1alpha3.RefusalActionPushEmptyCommit,
	})
	loop := newBranchWorkerEventLoop(w, time.Minute)
	t.Cleanup(loop.stopTimers)

	// Consume the window, as a first refusal that committed would.
	limited, _ := w.refusalRateLimited(editingRef())
	require.False(t, limited)

	loop.armTrailingRefusalTouch(editingRef(), "a second refusal", time.Minute)

	assert.NotNil(t, loop.refusalTimer, "a coalesced refusal must leave a trailing commit armed")
	require.Contains(t, loop.refusalPending, editingRef().String())
	assert.Equal(t, "a second refusal", loop.refusalPending[editingRef().String()].detail)
}

// TestRefusalTouch_TrailingCommitRechecksConsent. A minute is long enough for the target to be
// suspended or set back to Ignore, and acting on consent captured a minute ago is exactly what a
// delayed action gets wrong.
func TestRefusalTouch_TrailingCommitRechecksConsent(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{
		OnRefusal: configv1alpha3.RefusalActionPushEmptyCommit,
	})
	loop := newBranchWorkerEventLoop(w, time.Minute)
	t.Cleanup(loop.stopTimers)
	loop.armTrailingRefusalTouch(editingRef(), "queued while consent still stood", 0)

	setSpec := func(spec configv1alpha3.GitTargetSpec) {
		target := &configv1alpha3.GitTarget{}
		require.NoError(t, w.Client.Get(w.ctx, client.ObjectKey{Name: "editing", Namespace: "team-a"}, target))
		spec.GitProviderRef = target.Spec.GitProviderRef
		spec.Branch, spec.Path = target.Spec.Branch, target.Spec.Path
		target.Spec = spec
		require.NoError(t, w.Client.Update(w.ctx, target))
	}
	setSpec(configv1alpha3.GitTargetSpec{OnRefusal: configv1alpha3.RefusalActionIgnore})

	// The select arm owns the timer (it nils it before dispatching), so the flush is called here
	// the way the loop calls it.
	loop.stopRefusalTimer()
	loop.flushPendingRefusalTouch()

	assert.Empty(t, loop.refusalPending,
		"a target set back to Ignore must not get the commit queued under the old setting")
	assert.Nil(t, loop.refusalTimer, "and nothing must be re-armed for a target that said no")
}

// TestRefusalTouch_OnlyWriteBoundaryRefusalsCommit is the first half of the pruning fence: the
// refusal has to be one where the edit had nowhere to land while the folder itself is accepted.
func TestRefusalTouch_OnlyWriteBoundaryRefusalsCommit(t *testing.T) {
	cases := []struct {
		name string
		kind manifestanalyzer.IssueKind
		want bool
	}{
		{name: "an edit that escapes the write scope", kind: manifestanalyzer.IssueWriteEscapesScope, want: true},
		{name: "an edit with nowhere to land", kind: manifestanalyzer.IssueUnplaceableEdit, want: true},
		{name: "an edit fanning into several files", kind: manifestanalyzer.IssueWriteFanIn, want: true},
		// Excluded even though it is a write-boundary refusal: it is raised for the BATCH and
		// names no path, so nothing can establish that a document exists behind it. A review
		// found it firing for a brand-new resource an images: entry would override.
		{name: "a render the batch refuses", kind: manifestanalyzer.IssueRenderRefused, want: false},
		{name: "a foreign file in the folder", kind: manifestanalyzer.IssueForeignFile, want: false},
		{name: "yaml the folder cannot parse", kind: manifestanalyzer.IssueInvalidYAML, want: false},
		{name: "a kustomization we do not support", kind: manifestanalyzer.IssueUnsupportedKustomize, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refused := &manifestanalyzer.AcceptanceRefusedError{
				Issues: []manifestanalyzer.AcceptanceIssue{
					{Kind: tc.kind, Path: "x.yaml", ExistingDocument: true},
				},
			}
			assert.Equal(t, tc.want, refusalIsAWriteBoundary(refused))
		})
	}
}

// TestRefusalTouch_RequiresEvidenceTheDocumentExists is the second half, and the one a review
// found missing. The refusal KIND cannot establish that the folder holds the object: a write that
// is REMOVING a document is a write-boundary refusal too, and so is one for an object that was
// never there. Re-applying does nothing for the second and prunes the first, and hurrying somebody
// else's delete is what the fence exists to prevent.
func TestRefusalTouch_RequiresEvidenceTheDocumentExists(t *testing.T) {
	boundary := manifestanalyzer.IssueWriteEscapesScope

	withEvidence := &manifestanalyzer.AcceptanceRefusedError{
		Issues: []manifestanalyzer.AcceptanceIssue{{Kind: boundary, Path: "a.yaml", ExistingDocument: true}},
	}
	assert.True(t, refusalIsAWriteBoundary(withEvidence))

	noEvidence := &manifestanalyzer.AcceptanceRefusedError{
		Issues: []manifestanalyzer.AcceptanceIssue{{Kind: boundary, Path: "a.yaml"}},
	}
	assert.False(t, refusalIsAWriteBoundary(noEvidence),
		"an object the folder does not already hold must not be committed for")

	// A batch is only safe when EVERY issue carries the evidence: one unestablished document is
	// enough to make the commit a guess.
	mixedEvidence := &manifestanalyzer.AcceptanceRefusedError{
		Issues: []manifestanalyzer.AcceptanceIssue{
			{Kind: boundary, Path: "a.yaml", ExistingDocument: true},
			{Kind: boundary, Path: "b.yaml"},
		},
	}
	assert.False(t, refusalIsAWriteBoundary(mixedEvidence))
}

// TestRefusalTouch_MixedRefusalDoesNotCommit. A batch carrying any folder-level issue is a folder
// problem, so it is left alone even though a write-boundary issue rides along with it.
func TestRefusalTouch_MixedRefusalDoesNotCommit(t *testing.T) {
	refused := &manifestanalyzer.AcceptanceRefusedError{
		Issues: []manifestanalyzer.AcceptanceIssue{
			{Kind: manifestanalyzer.IssueUnplaceableEdit, Path: "a.yaml", ExistingDocument: true},
			{Kind: manifestanalyzer.IssueForeignFile, Path: "secrets.txt", ExistingDocument: true},
		},
	}

	assert.False(t, refusalIsAWriteBoundary(refused))
	assert.False(t, refusalIsAWriteBoundary(nil), "a non-refusal never commits")
	assert.False(t, refusalIsAWriteBoundary(&manifestanalyzer.AcceptanceRefusedError{}),
		"a refusal with no issues says nothing about the folder")
}

// TestRefusalTouch_ReplayKeepsAcceptedWritesAndTheEmptyDiff is the contention case a review asked
// for, and it lives here rather than in e2e on purpose: the property is about ARRIVAL ORDER
// between our push and somebody else's, and the bi-directional corner does not control that.
// Here the contending commit is made on disk, between our commit and our push, so the rejection is
// deterministic instead of hoped for.
//
// The three things it pins, which is what makes a mixed batch safe:
//
//   - The accepted write survives the replay. A refusal must not cost the writes that were fine.
//   - The empty commit is still empty after being replanned onto a moved tree. Replay re-plans
//     each retained write, and a write with no content must come back with no content rather than
//     picking anything up from the new base.
//   - The other writer's file is still there. We rebase onto their work, never over it.
func TestRefusalTouch_ReplayKeepsAcceptedWritesAndTheEmptyDiff(t *testing.T) {
	f := newLedgerFixture(t, "refusal-replay", true)
	f.createLedgerTarget("team-a", nil)
	f.publish("prime")

	// An ordinary accepted write, committed and retained but not yet pushed.
	f.commit(false, "accepted")
	require.Len(t, f.pending, 1)

	// The refusal's empty commit joins the same retained batch, exactly as the event loop would
	// append it while an earlier write is still waiting for the push cooldown.
	touch, err := f.worker.buildRefusalTouchWrite(
		f.worker.ctx, itypes.NewResourceReference("target-a", "default"), "unsupported folder content")
	require.NoError(t, err)
	batch := []PendingWrite{*touch}
	require.NoError(t, f.worker.commitPendingWrites(batch, true))
	f.pending = append(f.pending, batch...)

	// Somebody else moves the branch before our push, so the compare-and-swap rejects it and the
	// worker replays BOTH retained writes onto their tip.
	f.contend("OUTSIDE.md", "from-another-writer\n")

	f.push()

	head := gitOut(t, f.repoDir, "rev-parse", "main")
	tip, parent := head, gitOut(t, f.repoDir, "rev-parse", head+"^")

	// Identify the commits by their messages rather than trusting the replay's ordering, so a
	// reordering shows up as a failure here instead of silently retargeting the assertions below.
	assert.Contains(t, gitOut(t, f.repoDir, "log", "-1", "--format=%B", tip), "refused write",
		"the tip must be the refusal commit")
	assert.Empty(t, gitOut(t, f.repoDir, "diff-tree", "--no-commit-id", "--name-only", "-r", tip),
		"the refusal commit must still be empty after being replayed onto a moved branch")
	assert.Contains(t, gitOut(t, f.repoDir, "diff-tree", "--no-commit-id", "--name-only", "-r", parent),
		"accepted", "the accepted write must survive the replay")
	assert.Equal(t, "from-another-writer",
		gitOut(t, f.repoDir, "show", "main:OUTSIDE.md"),
		"the other writer's commit must be rebased onto, never over")
}

// TestRefusalTouch_ANewResourceAnEntryWouldOverrideIsNotCommittedFor drives the fence through the
// REAL analyzer rather than a hand-built issue, because that is how a review found the hole.
//
// A brand-new Deployment whose image an existing images: entry would override is refused, and the
// refusal is a write-boundary one. Nothing in Git holds that object, so re-applying would not
// correct it: with pruning on it is removed, and otherwise nothing happens. Committing for it was
// the promise in spec.onRefusal's own documentation being false.
func TestRefusalTouch_ANewResourceAnEntryWouldOverrideIsNotCommittedFor(t *testing.T) {
	writer := newContentWriter(itypes.SensitiveResourcePolicy{})
	worktree := newWorktreeForTest(t)
	root := worktree.Filesystem().Root()

	require.NoError(t, os.WriteFile(filepath.Join(root, "web.yaml"),
		[]byte(sharedImageDeploymentYAML("web")), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "kustomization.yaml"), []byte(
		`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: default
resources:
  - web.yaml
images:
  - name: ghcr.io/example/shared
    newTag: "1.0.0"
`), 0o600))

	_, err := flushEventsForTest(t, writer, worktree, deploymentMapper(),
		newCacheDeploymentEvent("ghcr.io/example/shared:2.0.0"))

	var refused *manifestanalyzer.AcceptanceRefusedError
	require.ErrorAs(t, err, &refused, "the analyzer must still refuse this write")
	assert.False(t, refusalIsAWriteBoundary(refused),
		"an object Git does not hold must not earn a commit, whatever the refusal kind says")
}

// TestRefusalTouch_AnEditToAnExistingDocumentIsCommittedFor is the positive control for the test
// above: the same machinery, an object the folder DOES hold, and the fence opens.
//
// Without this, the fence could pass its negative tests by never opening at all.
func TestRefusalTouch_AnEditToAnExistingDocumentIsCommittedFor(t *testing.T) {
	issue := manifestanalyzer.AcceptanceIssue{
		Kind: manifestanalyzer.IssueWriteEscapesScope,
		Path: "base/deployment.yaml",
	}

	// The shape the e2e produces: a base-owned field, refused because the write would leave the
	// GitTarget's scope, with the document plainly present in the folder.
	issue.ExistingDocument = true
	assert.True(t, refusalIsAWriteBoundary(
		&manifestanalyzer.AcceptanceRefusedError{Issues: []manifestanalyzer.AcceptanceIssue{issue}}))
}

// TestRefusalTouch_OneTargetDoesNotDropAnothersPendingCommit is the regression a review found.
//
// One branch worker serves every GitTarget on its (provider, branch), while the rate limit and
// the consent check are both per target. With a single pending slot, B's refusal replaced A's; B
// was then suspended, its consent check dropped it, and A's authorized commit was gone with
// nothing left to report it. Keeping the intent per target is what makes coalesced EXECUTION safe.
func TestRefusalTouch_OneTargetDoesNotDropAnothersPendingCommit(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, configv1alpha3.AddToScheme(scheme))

	newTarget := func(name string, suspend bool) *configv1alpha3.GitTarget {
		target := &configv1alpha3.GitTarget{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"},
			Spec: configv1alpha3.GitTargetSpec{
				Branch: "main", Path: "apps/" + name,
				OnRefusal: configv1alpha3.RefusalActionPushEmptyCommit,
				Suspend:   suspend,
			},
		}
		target.Spec.GitProviderRef.Name = "test-provider"
		return target
	}
	// B is suspended, so its delayed commit must be declined when the timer fires. A is not.
	client := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(newTarget("alpha", false), newTarget("bravo", true)).Build()

	w := newMetricsTestWorker()
	w.Client = client
	w.ctx = context.Background()
	loop := newBranchWorkerEventLoop(w, time.Minute)
	t.Cleanup(loop.stopTimers)

	alpha := itypes.NewResourceReference("alpha", "team-a")
	bravo := itypes.NewResourceReference("bravo", "team-a")

	// A zero wait makes both entries due the moment they are recorded, so the flush below cannot
	// race their deadlines. The deadline itself is covered by its own test.
	loop.armTrailingRefusalTouch(alpha, "alpha was refused", 0)
	loop.armTrailingRefusalTouch(bravo, "bravo was refused", 0)

	require.Len(t, loop.refusalPending, 2,
		"two targets on one worker must hold two pending commits, not one")

	// bravo's consent is withdrawn; alpha's is not, and alpha must survive it.
	loop.stopRefusalTimer()
	loop.flushPendingRefusalTouch()

	assert.Empty(t, loop.refusalPending, "every due entry is consumed exactly once")
	limitedAlpha, _ := w.refusalRateLimited(alpha)
	assert.True(t, limitedAlpha,
		"alpha's commit must have run, consuming its rate-limit window")
	limitedBravo, _ := w.refusalRateLimited(bravo)
	assert.False(t, limitedBravo,
		"bravo's was declined for lack of consent, so it must not have consumed a window")
}

// TestRefusalTouch_TheTimerTracksTheEarliestDeadline. One timer serves every pending target, so it
// has to wake for whichever is due first or a later entry would delay an earlier one.
func TestRefusalTouch_TheTimerTracksTheEarliestDeadline(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{
		OnRefusal: configv1alpha3.RefusalActionPushEmptyCommit,
	})
	loop := newBranchWorkerEventLoop(w, time.Minute)
	t.Cleanup(loop.stopTimers)

	late := itypes.NewResourceReference("late", "team-a")
	soon := itypes.NewResourceReference("soon", "team-a")

	loop.armTrailingRefusalTouch(late, "later", time.Hour)
	loop.armTrailingRefusalTouch(soon, "sooner", 50*time.Millisecond)

	require.Len(t, loop.refusalPending, 2)
	select {
	case <-loop.refusalTimer.C:
	case <-time.After(2 * time.Second):
		t.Fatal("the shared timer must wake for the earliest deadline, not the last one armed")
	}
}
