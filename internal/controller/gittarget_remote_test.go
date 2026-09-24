// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// The two repositories a repoint moves between: a recreated GitProvider mints a new UID, and here
// it names a different URL as well.
var (
	firstRepo  = git.RepoIdentity{ProviderUID: "uid-1", URL: "https://example.invalid/first.git"}
	secondRepo = git.RepoIdentity{ProviderUID: "uid-2", URL: "https://example.invalid/second.git"}
)

func remoteTestTarget() *configbutleraiv1alpha3.GitTarget {
	return &configbutleraiv1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop", Generation: 4},
	}
}

func observation(revision string, at time.Time, by git.ObservationSource, repo git.RepoIdentity) git.RemoteObservation {
	return git.RemoteObservation{Revision: revision, At: at, By: by, Repo: repo}
}

// TestPublishRemote_AbsentUntilSomethingLooks keeps the two "no revision" cases apart. Nothing has
// looked, so there is no stanza at all — which is not the same as a look that found no branch.
func TestPublishRemote_AbsentUntilSomethingLooks(t *testing.T) {
	r := &GitTargetReconciler{}
	target := remoteTestTarget()

	r.publishRemote(target, git.RemoteObservation{}, false, git.RepoIdentity{}, time.Now())

	assert.Nil(t, target.Status.Remote)
}

// TestPublishRemote_AnObservationWithNoRevisionIsStillPublished is the other half: the branch is
// not on the remote, and saying so is the answer rather than the absence of one.
func TestPublishRemote_AnObservationWithNoRevisionIsStillPublished(t *testing.T) {
	r := &GitTargetReconciler{}
	target := remoteTestTarget()
	at := time.Now()

	r.publishRemote(target, observation("", at, git.ObservedByFetch, git.RepoIdentity{}), true, git.RepoIdentity{}, at)

	require.NotNil(t, target.Status.Remote)
	assert.Empty(t, target.Status.Remote.Revision)
	assert.Equal(t, "Fetch", target.Status.Remote.VerifiedBy)
	assert.Equal(t, at.Unix(), target.Status.Remote.LastVerifiedAt.Unix())
}

// TestPublishRemote_TheFloorIsMeasuredFromTheLastWrite is the rate limit, and the distinction is
// the whole point of keeping a ledger.
//
// Measuring against the PUBLISHED OBSERVATION's timestamp does not limit anything: an observation
// made at 12:00:02 can be written at 12:01:00, and a revision proved at 12:01:02 then looks a full
// minute newer than what is published and is written two seconds after it.
func TestPublishRemote_TheFloorIsMeasuredFromTheLastWrite(t *testing.T) {
	r := &GitTargetReconciler{}
	target := remoteTestTarget()
	start := time.Now()

	// Proved at +2s, written at +60s: a reconcile that happened to be late.
	r.publishRemote(target, observation("aaaa", start.Add(2*time.Second), git.ObservedByPush, git.RepoIdentity{}),
		true, git.RepoIdentity{}, start.Add(time.Minute))
	require.Equal(t, "aaaa", target.Status.Remote.Revision)

	// Proved at +62s: newer than the published observation by a minute, two seconds after the write.
	r.publishRemote(target, observation("bbbb", start.Add(62*time.Second), git.ObservedByPush, git.RepoIdentity{}),
		true, git.RepoIdentity{}, start.Add(62*time.Second))

	assert.Equal(t, "aaaa", target.Status.Remote.Revision,
		"two status writes two seconds apart is exactly what the floor exists to prevent")

	// A full interval after the WRITE, it publishes.
	r.publishRemote(target, observation("bbbb", start.Add(62*time.Second), git.ObservedByPush, git.RepoIdentity{}),
		true, git.RepoIdentity{}, start.Add(2*time.Minute))
	assert.Equal(t, "bbbb", target.Status.Remote.Revision)
}

// TestPublishRemote_TheFirstObservationIsImmediate. The floor is on the RATE of writes, and the
// first one has no rate: holding it back would leave a target that has just published showing no
// branch at all, which reads as a broken data plane rather than as a sampled field.
func TestPublishRemote_TheFirstObservationIsImmediate(t *testing.T) {
	r := &GitTargetReconciler{}
	target := remoteTestTarget()
	at := time.Now()

	r.publishRemote(
		target,
		observation("aaaa", at, git.ObservedByPush, git.RepoIdentity{}),
		true,
		git.RepoIdentity{},
		at,
	)

	require.NotNil(t, target.Status.Remote)
	assert.Equal(t, "aaaa", target.Status.Remote.Revision)
}

// TestPublishRemote_AConvergedTargetWritesNothing is the trap sameLayout was written to avoid: a
// target whose branch has not moved must not write status once per tick merely to advance a clock,
// however long it has been since the last one.
func TestPublishRemote_AConvergedTargetWritesNothing(t *testing.T) {
	r := &GitTargetReconciler{}
	target := remoteTestTarget()
	at := time.Now()
	r.publishRemote(
		target,
		observation("aaaa", at, git.ObservedByFetch, git.RepoIdentity{}),
		true,
		git.RepoIdentity{},
		at,
	)
	published := target.Status.Remote

	r.publishRemote(target, observation("aaaa", at, git.ObservedByFetch, git.RepoIdentity{}),
		true, git.RepoIdentity{}, at.Add(time.Hour))

	assert.Same(t, published, target.Status.Remote)
}

// TestPublishRemote_AReProvedRevisionRefreshesTheClock. The same revision proved again later IS
// news — that is what makes lastVerifiedAt a freshness field — and like every other kind of news
// it waits for the floor.
func TestPublishRemote_AReProvedRevisionRefreshesTheClock(t *testing.T) {
	r := &GitTargetReconciler{}
	target := remoteTestTarget()
	at := time.Now()
	r.publishRemote(
		target,
		observation("aaaa", at, git.ObservedByFetch, git.RepoIdentity{}),
		true,
		git.RepoIdentity{},
		at,
	)

	r.publishRemote(target, observation("aaaa", at.Add(30*time.Second), git.ObservedByFetch, git.RepoIdentity{}),
		true, git.RepoIdentity{}, at.Add(30*time.Second))
	assert.Equal(t, at.Unix(), target.Status.Remote.LastVerifiedAt.Unix(), "inside the floor")

	r.publishRemote(target, observation("aaaa", at.Add(90*time.Second), git.ObservedByFetch, git.RepoIdentity{}),
		true, git.RepoIdentity{}, at.Add(90*time.Second))
	assert.Equal(t, at.Add(90*time.Second).Unix(), target.Status.Remote.LastVerifiedAt.Unix(),
		"an operator reading 'verified 40 minutes ago' on a target being refreshed would be "+
			"reading a broken refresher")
}

// TestRemoteStatusIsNews_TheSameAnswerProvedASecondWayIsNot. A revision that changes hands from a
// Push to a Fetch is the same answer proved again; the watch plane has always ignored it for that
// reason, and this layer now agrees.
func TestRemoteStatusIsNews_TheSameAnswerProvedASecondWayIsNot(t *testing.T) {
	at := metav1.NewTime(time.Now())
	published := &configbutleraiv1alpha3.GitTargetRemoteStatus{
		Revision: "aaaa", LastVerifiedAt: &at, VerifiedBy: "Push",
	}

	assert.False(t, remoteStatusIsNews(published, &configbutleraiv1alpha3.GitTargetRemoteStatus{
		Revision: "aaaa", LastVerifiedAt: &at, VerifiedBy: "Fetch",
	}))
}

// TestRemoteStatusIsNews_AMissingTimestampIsAlwaysNews. Neither side can be compared without one,
// and the safe direction is to write: a stanza with no clock is the one an operator cannot read an
// age off at all.
func TestRemoteStatusIsNews_AMissingTimestampIsAlwaysNews(t *testing.T) {
	at := metav1.NewTime(time.Now())
	withClock := &configbutleraiv1alpha3.GitTargetRemoteStatus{
		Revision: "aaaa", LastVerifiedAt: &at, VerifiedBy: "Fetch",
	}
	without := &configbutleraiv1alpha3.GitTargetRemoteStatus{Revision: "aaaa", VerifiedBy: "Fetch"}

	assert.True(t, remoteStatusIsNews(without, withClock))
	assert.True(t, remoteStatusIsNews(withClock, without))
}

// TestPublishRemote_ARevisionFromAnotherRepositoryIsRemoved. The published revision names a commit
// in a repository this GitTarget no longer points at, and there is nothing to replace it with
// until a look at the new one succeeds. If that repository is unreachable there never will be, so
// the stanza goes rather than going stale.
func TestPublishRemote_ARevisionFromAnotherRepositoryIsRemoved(t *testing.T) {
	r := &GitTargetReconciler{}
	target := remoteTestTarget()
	at := time.Now()
	r.publishRemote(target, observation("aaaa", at, git.ObservedByPush, firstRepo), true, firstRepo, at)
	require.NotNil(t, target.Status.Remote)

	r.publishRemote(target, observation("aaaa", at, git.ObservedByPush, firstRepo), true, secondRepo, at)

	assert.Nil(t, target.Status.Remote,
		"a revision from a repository this target no longer uses must not be left standing")
}

// TestPublishRemote_ASiblingsObservationCannotHideAStaleRepository is the case a comparison
// against the shared observation alone gets wrong.
//
// One worker serves every GitTarget on a branch, so a sibling can already have proved the
// REPLACEMENT repository. The shared observation then agrees with the GitProvider while this
// target is still publishing a revision from the repository it has left, and the floor would hold
// the correction back on top of that. Invalidation follows what THIS target published.
func TestPublishRemote_ASiblingsObservationCannotHideAStaleRepository(t *testing.T) {
	r := &GitTargetReconciler{}
	target := remoteTestTarget()
	at := time.Now()
	r.publishRemote(target, observation("aaaa", at, git.ObservedByPush, firstRepo), true, firstRepo, at)
	require.Equal(t, "aaaa", target.Status.Remote.Revision)

	// A sibling on the same branch has just proved the new repository; one second later, this
	// target reconciles.
	r.publishRemote(target, observation("bbbb", at.Add(time.Second), git.ObservedByFetch, secondRepo),
		true, secondRepo, at.Add(time.Second))

	require.NotNil(t, target.Status.Remote)
	assert.Equal(t, "bbbb", target.Status.Remote.Revision,
		"the repository changed under this target, so the correction does not wait for the floor")
}

// TestPublishRemote_AnUnknownIdentityProvesNothing. The comparison needs both halves. A
// GitProvider that could not be read names no repository, and a worker with no identity of its own
// — the CLI, and tests that never reach a remote — records none. Treating either as a mismatch
// would delete a perfectly good revision every time a provider read blipped.
func TestPublishRemote_AnUnknownIdentityProvesNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		observed git.RepoIdentity
		current  git.RepoIdentity
	}{
		{"the provider could not be read", firstRepo, git.RepoIdentity{}},
		{"the worker has no identity", git.RepoIdentity{}, firstRepo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &GitTargetReconciler{}
			target := remoteTestTarget()
			at := time.Now()

			r.publishRemote(target, observation("aaaa", at, git.ObservedByPush, tc.observed), true, tc.current, at)

			require.NotNil(t, target.Status.Remote)
			assert.Equal(t, "aaaa", target.Status.Remote.Revision)
		})
	}
}

// TestPublishRemote_TheNewRepositoryRepopulatesTheStanza is the other side of the removal: the
// first look at the repository the target points at NOW publishes again, with no latch to clear
// and no floor to wait for, because the ledger was dropped with the revision it described.
func TestPublishRemote_TheNewRepositoryRepopulatesTheStanza(t *testing.T) {
	r := &GitTargetReconciler{}
	target := remoteTestTarget()
	at := time.Now()
	r.publishRemote(target, observation("aaaa", at, git.ObservedByPush, firstRepo), true, secondRepo, at)
	require.Nil(t, target.Status.Remote)

	r.publishRemote(target, observation("bbbb", at.Add(time.Second), git.ObservedByFetch, secondRepo),
		true, secondRepo, at.Add(time.Second))

	require.NotNil(t, target.Status.Remote)
	assert.Equal(t, "bbbb", target.Status.Remote.Revision)
}

// TestPublishRemote_ARestartPublishesAgainstTheObservationAlone. The ledger is in memory, so after
// a restart the only evidence about what is published is the observation itself, which names the
// repository it was proved against.
func TestPublishRemote_ARestartPublishesAgainstTheObservationAlone(t *testing.T) {
	target := remoteTestTarget()
	at := metav1.NewTime(time.Now())
	target.Status.Remote = &configbutleraiv1alpha3.GitTargetRemoteStatus{
		Revision: "aaaa", LastVerifiedAt: &at, VerifiedBy: "Push",
	}
	fresh := &GitTargetReconciler{}

	fresh.publishRemote(target, observation("aaaa", at.Time, git.ObservedByPush, firstRepo), true, secondRepo, at.Time)

	assert.Nil(t, target.Status.Remote, "the observation is about a repository this target has left")
}

// TestObserveRemote_ReadsTheBranchItsGitTargetWrites. The observation is shared by every GitTarget
// on the branch, so it is looked up by (provider namespace, provider, branch) and not by target.
func TestObserveRemote_ReadsTheBranchItsGitTargetWrites(t *testing.T) {
	target := remoteTestTarget()
	target.Spec.GitProviderRef.Name = "repo1"
	target.Spec.Branch = "main"

	_, seen := (&GitTargetReconciler{}).observeRemote(target, "shop")
	assert.False(t, seen, "a reconcile with no worker manager has nothing to read")

	workers := git.NewWorkerManager(nil, logr.Discard(), git.BranchWorkerLimits{}, types.SensitiveResourcePolicy{})
	_, seen = (&GitTargetReconciler{WorkerManager: workers}).observeRemote(target, "shop")
	assert.False(t, seen, "and neither has one whose branch nothing has looked at")
}

// TestRequestRemoteRefresh_DoesNothingWithoutADataPlane covers the two ways the trigger is simply
// not armed: the refresher turned off, and a reconcile running without a worker manager. Neither
// may reach for a worker, because the reconcile runs on every tick of every target.
func TestRequestRemoteRefresh_DoesNothingWithoutADataPlane(t *testing.T) {
	target := remoteTestTarget()

	assert.NotPanics(t, func() {
		(&GitTargetReconciler{GitRefreshInterval: 0}).requestRemoteRefresh(target, "default")
	}, "a zero interval turns the refresher off")

	assert.NotPanics(t, func() {
		(&GitTargetReconciler{GitRefreshInterval: time.Minute}).requestRemoteRefresh(target, "default")
	}, "and a reconcile with no worker manager has nothing to ask")

	workers := git.NewWorkerManager(nil, logr.Discard(), git.BranchWorkerLimits{}, types.SensitiveResourcePolicy{})
	assert.NotPanics(t, func() {
		(&GitTargetReconciler{
			GitRefreshInterval: time.Minute, WorkerManager: workers,
		}).requestRemoteRefresh(target, "default")
	}, "and a target whose branch has no worker yet has nothing to refresh either")
}

// TestPublishRemote_TheRemovalReachesTheAPIAsADelete. Clearing a Go pointer is only half of
// removing a field: the status write is a JSON merge patch computed against the object as read, so
// the fix works only if a field that was there and is now nil is emitted as an explicit null.
func TestPublishRemote_TheRemovalReachesTheAPIAsADelete(t *testing.T) {
	r := &GitTargetReconciler{}
	target := remoteTestTarget()
	target.Status.Conditions = []metav1.Condition{{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready",
		LastTransitionTime: metav1.NewTime(time.Now()), ObservedGeneration: 4,
	}}
	at := time.Now()
	r.publishRemote(target, observation("aaaa", at, git.ObservedByPush, firstRepo), true, firstRepo, at)
	before := target.DeepCopy()

	r.publishRemote(target, observation("aaaa", at, git.ObservedByPush, firstRepo), true, secondRepo, at)

	data, err := client.MergeFrom(before).Data(target)
	require.NoError(t, err)
	assert.JSONEq(t, `{"status":{"remote":null}}`, string(data))
}
