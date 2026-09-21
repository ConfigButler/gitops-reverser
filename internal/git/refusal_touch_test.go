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
	"os/exec"
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
	assert.Equal(t, editingRef(), loop.refusalPending)
	assert.Equal(t, "a second refusal", loop.refusalPendingDetail)
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
	loop.armTrailingRefusalTouch(editingRef(), "queued while consent still stood", time.Millisecond)

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

	assert.Empty(t, loop.refusalPending.Name,
		"a target set back to Ignore must not get the commit queued under the old setting")
	assert.Nil(t, loop.refusalTimer, "and nothing must be re-armed for a target that said no")
}

// TestRefusalTouch_SkipsObjectsGitDoesNotManage is the pruning fence.
//
// An empty commit makes the reconciler re-apply. For an object Git does not manage that either
// does nothing (Flux prunes its inventory and Argo CD the resources it tracks, and a live-created
// object is in neither) or prunes it, when the object was managed and has since been removed from
// Git. The second is the reconciler's decision to make on its own schedule, not ours to hurry, so
// both are excluded.
func TestRefusalTouch_SkipsObjectsGitDoesNotManage(t *testing.T) {
	w := refusalTouchWorker(t, configv1alpha3.GitTargetSpec{
		OnRefusal: configv1alpha3.RefusalActionPushEmptyCommit,
	})

	assert.False(t, w.refusedObjectsAreManagedInGit(nil),
		"no events means nothing is known to be managed")
	assert.False(t, w.refusedObjectsAreManagedInGit([]Event{configMapEvent("never-in-git", "alice", "team-a")}),
		"an object with no file in the folder must not trigger a commit")
}

// TestRefusalTouch_CommitsForAnObjectGitManages is the positive half of the fence: an object that
// does have a file is drift the reconciler can correct, which is the case the feature exists for.
func TestRefusalTouch_CommitsForAnObjectGitManages(t *testing.T) {
	f := newLedgerFixture(t, "refusal-fence", true)
	f.createLedgerTarget("team-a", nil)
	f.publish("managed")

	assert.True(t, f.worker.refusedObjectsAreManagedInGit(
		[]Event{configMapEvent("managed", "alice", "team-a")}),
		"an object already written to the folder is drift the reconciler can correct")
	assert.False(t, f.worker.refusedObjectsAreManagedInGit(
		[]Event{configMapEvent("managed", "alice", "team-a"), configMapEvent("absent", "alice", "team-a")}),
		"a batch is only safe when every object in it is managed")
}
