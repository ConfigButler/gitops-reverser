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

// TestPublishRemote_AbsentUntilSomethingLooks keeps the two "no revision" cases apart. Nothing
// has looked, so there is no stanza at all — which is not the same as a look that found no
// branch.
func TestPublishRemote_AbsentUntilSomethingLooks(t *testing.T) {
	target := remoteTestTarget()
	due := publishRemote(target, git.RemoteObservation{}, false, git.RepoIdentity{}, time.Now(), time.Minute)
	assert.Nil(t, target.Status.Remote)
	assert.Zero(t, due, "nothing is owed for an answer nobody has")
}

// TestPublishRemote_AnObservationWithNoRevisionIsStillPublished is the other half: the branch is
// not on the remote, and saying so is the answer rather than the absence of one.
func TestPublishRemote_AnObservationWithNoRevisionIsStillPublished(t *testing.T) {
	target := remoteTestTarget()
	at := time.Now()

	publishRemote(target, git.RemoteObservation{At: at, By: git.ObservedByFetch},
		true, git.RepoIdentity{}, at, time.Minute)

	require.NotNil(t, target.Status.Remote)
	assert.Empty(t, target.Status.Remote.Revision)
	assert.Equal(t, "Fetch", target.Status.Remote.VerifiedBy)
	assert.Equal(t, at.Unix(), target.Status.Remote.LastVerifiedAt.Unix())
}

// TestPublishRemote_TheFirstObservationIsImmediate. The bound is on the RATE of writes, and the
// first one has no rate: holding it back would leave a target that has just published showing no
// branch at all, which reads as a broken data plane rather than as a sampled field.
func TestPublishRemote_TheFirstObservationIsImmediate(t *testing.T) {
	target := remoteTestTarget()
	at := time.Now()

	due := publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: at, By: git.ObservedByPush,
	}, true, git.RepoIdentity{}, at, time.Hour)

	require.NotNil(t, target.Status.Remote)
	assert.Equal(t, "aaaa", target.Status.Remote.Revision)
	assert.Zero(t, due)
}

// TestPublishRemote_ANewRevisionWaitsForThePublicationDeadline is the rule this surface is bounded
// by, and it is a deliberate change of promise.
//
// A revision that skipped the interval meant that a branch ten GitTargets share turned one push
// into ten status patches, each an etcd write invalidating every watcher's cached copy of the
// type. So a new revision is held until the published one is a full interval old — and the caller
// is told how long that is, so the write lands on the deadline rather than on the next steady tick.
func TestPublishRemote_ANewRevisionWaitsForThePublicationDeadline(t *testing.T) {
	target := remoteTestTarget()
	first := time.Now()
	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: first, By: git.ObservedByPush,
	}, true, git.RepoIdentity{}, first, time.Minute)
	published := target.Status.Remote

	due := publishRemote(target, git.RemoteObservation{
		Revision: "bbbb", At: first.Add(2 * time.Second), By: git.ObservedByPush,
	}, true, git.RepoIdentity{}, first.Add(2*time.Second), time.Minute)

	assert.Same(t, published, target.Status.Remote, "a second push two seconds later is not a second write")
	assert.Equal(t, 58*time.Second, due, "and the reconcile is told exactly when it owes one")

	// On the deadline, it publishes.
	due = publishRemote(target, git.RemoteObservation{
		Revision: "bbbb", At: first.Add(2 * time.Second), By: git.ObservedByPush,
	}, true, git.RepoIdentity{}, first.Add(time.Minute), time.Minute)

	require.NotNil(t, target.Status.Remote)
	assert.Equal(t, "bbbb", target.Status.Remote.Revision)
	assert.Zero(t, due)
}

// TestPublishRemote_AConvergedTargetWritesNothing is the trap sameLayout was written to avoid: a
// target whose branch has not moved must not write status once per tick merely to advance a clock,
// however long it has been since the last one.
func TestPublishRemote_AConvergedTargetWritesNothing(t *testing.T) {
	target := remoteTestTarget()
	at := time.Now()
	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: at, By: git.ObservedByFetch,
	}, true, git.RepoIdentity{}, at, time.Minute)
	published := target.Status.Remote

	// An hour of steady ticks, re-delivering the same observation. There is nothing to say.
	due := publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: at, By: git.ObservedByFetch,
	}, true, git.RepoIdentity{}, at.Add(time.Hour), time.Minute)

	assert.Same(t, published, target.Status.Remote)
	assert.Zero(t, due, "nothing is owed when nothing has been proved since")
}

// TestPublishRemote_AReProvedRevisionRefreshesTheClockOnTheDeadline. The same revision proved
// again later IS news — that is what makes lastVerifiedAt a freshness field — and like every other
// kind of news it waits for the deadline.
func TestPublishRemote_AReProvedRevisionRefreshesTheClockOnTheDeadline(t *testing.T) {
	target := remoteTestTarget()
	at := time.Now()
	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: at, By: git.ObservedByFetch,
	}, true, git.RepoIdentity{}, at, time.Minute)

	due := publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: at.Add(30 * time.Second), By: git.ObservedByFetch,
	}, true, git.RepoIdentity{}, at.Add(30*time.Second), time.Minute)
	assert.Equal(t, 30*time.Second, due)
	assert.Equal(t, at.Unix(), target.Status.Remote.LastVerifiedAt.Unix())

	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: at.Add(90 * time.Second), By: git.ObservedByFetch,
	}, true, git.RepoIdentity{}, at.Add(90*time.Second), time.Minute)

	assert.Equal(t, at.Add(90*time.Second).Unix(), target.Status.Remote.LastVerifiedAt.Unix(),
		"an operator reading 'verified 40 minutes ago' on a target being refreshed would be "+
			"reading a broken refresher")
}

// TestPublishRemote_TheSameAnswerProvedASecondWayIsNotNews. A revision that changes hands from a
// Push to a Fetch is the same answer proved again; the watch plane has always ignored it for that
// reason, and this layer now agrees. The field still follows, because the re-proof carries a later
// timestamp and that IS news — it just never writes on the strength of verifiedBy alone.
func TestPublishRemote_TheSameAnswerProvedASecondWayIsNotNews(t *testing.T) {
	at := metav1.NewTime(time.Now())
	published := &configbutleraiv1alpha3.GitTargetRemoteStatus{
		Revision: "aaaa", LastVerifiedAt: &at, VerifiedBy: "Push",
	}

	assert.False(t, remoteStatusIsNews(published, &configbutleraiv1alpha3.GitTargetRemoteStatus{
		Revision: "aaaa", LastVerifiedAt: &at, VerifiedBy: "Fetch",
	}))
}

// TestRemoteStatusIsNews_AMissingTimestampIsAlwaysNews. Neither side can be compared without one,
// and the safe direction is to write: a stanza with no clock is the one an operator cannot read
// an age off at all.
func TestRemoteStatusIsNews_AMissingTimestampIsAlwaysNews(t *testing.T) {
	at := metav1.NewTime(time.Now())
	withClock := &configbutleraiv1alpha3.GitTargetRemoteStatus{
		Revision: "aaaa", LastVerifiedAt: &at, VerifiedBy: "Fetch",
	}
	without := &configbutleraiv1alpha3.GitTargetRemoteStatus{Revision: "aaaa", VerifiedBy: "Fetch"}

	assert.True(t, remoteStatusIsNews(without, withClock))
	assert.True(t, remoteStatusIsNews(withClock, without))
}

// TestPublishRemote_AZeroIntervalTakesTheDefault. The interval is a constant today, and a caller
// that passes nothing must not turn the bound off: that is the shape the old quantum had, where a
// disabled refresher meant "write every tick".
func TestPublishRemote_AZeroIntervalTakesTheDefault(t *testing.T) {
	target := remoteTestTarget()
	at := time.Now()
	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: at, By: git.ObservedByFetch,
	}, true, git.RepoIdentity{}, at, 0)

	due := publishRemote(target, git.RemoteObservation{
		Revision: "bbbb", At: at.Add(time.Second), By: git.ObservedByPush,
	}, true, git.RepoIdentity{}, at.Add(time.Second), 0)

	assert.Equal(t, "aaaa", target.Status.Remote.Revision)
	assert.Equal(t, RemotePublicationInterval-time.Second, due)
}

// TestPublishRemote_ARevisionFromAnotherRepositoryIsRemoved. The published revision names a
// commit in a repository this GitTarget no longer points at — its GitProvider was recreated
// against a different URL, and the observation the branch still holds was proved against the old
// one. There is nothing to replace it with until a look at the new repository succeeds, and if
// that repository is unreachable there never will be, so the stanza goes rather than going stale.
func TestPublishRemote_ARevisionFromAnotherRepositoryIsRemoved(t *testing.T) {
	target := remoteTestTarget()
	at := time.Now()
	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: at, By: git.ObservedByPush, Repo: firstRepo,
	}, true, firstRepo, at, time.Hour)
	require.NotNil(t, target.Status.Remote)

	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: at, By: git.ObservedByPush, Repo: firstRepo,
	}, true, secondRepo, at, time.Hour)

	assert.Nil(t, target.Status.Remote,
		"a revision from a repository this target no longer uses must not be left standing")
}

// TestPublishRemote_TheRemovalIsNotBounded. The rate bound exists to stop a target writing status
// to advance a clock. Removing a revision that is about the wrong repository is a correction, not
// a clock, so it must never be held back by it.
func TestPublishRemote_TheRemovalIsNotBounded(t *testing.T) {
	target := remoteTestTarget()
	at := time.Now()
	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: at, By: git.ObservedByFetch, Repo: firstRepo,
	}, true, firstRepo, at, 10*time.Minute)

	due := publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: at.Add(time.Second), By: git.ObservedByFetch, Repo: firstRepo,
	}, true, secondRepo, at.Add(time.Second), 10*time.Minute)

	assert.Nil(t, target.Status.Remote)
	assert.Zero(t, due)
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
			target := remoteTestTarget()
			at := time.Now()
			publishRemote(target, git.RemoteObservation{
				Revision: "aaaa", At: at, By: git.ObservedByPush, Repo: tc.observed,
			}, true, tc.current, at, time.Hour)

			require.NotNil(t, target.Status.Remote)
			assert.Equal(t, "aaaa", target.Status.Remote.Revision)
		})
	}
}

// TestPublishRemote_TheNewRepositoryRepopulatesTheStanza is the other side of the removal: the
// first look at the repository the target points at NOW publishes again, with no latch to clear
// and no deadline to wait for, because there is nothing published to be a rate against.
func TestPublishRemote_TheNewRepositoryRepopulatesTheStanza(t *testing.T) {
	target := remoteTestTarget()
	at := time.Now()
	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: at, By: git.ObservedByPush, Repo: firstRepo,
	}, true, secondRepo, at, time.Hour)
	require.Nil(t, target.Status.Remote)

	publishRemote(target, git.RemoteObservation{
		Revision: "bbbb", At: at.Add(time.Second), By: git.ObservedByFetch, Repo: secondRepo,
	}, true, secondRepo, at.Add(time.Second), time.Hour)

	require.NotNil(t, target.Status.Remote)
	assert.Equal(t, "bbbb", target.Status.Remote.Revision)
}

// TestSoonerRequeue_TakesTheEarlierNonZero. Zero means "nothing due", which is not "due now": a
// requeue of 0 asks controller-runtime for no periodic requeue at all.
func TestSoonerRequeue_TakesTheEarlierNonZero(t *testing.T) {
	assert.Equal(t, time.Minute, soonerRequeue(5*time.Minute, time.Minute))
	assert.Equal(t, time.Minute, soonerRequeue(time.Minute, 5*time.Minute))
	assert.Equal(t, 5*time.Minute, soonerRequeue(5*time.Minute, 0))
	assert.Equal(t, time.Minute, soonerRequeue(0, time.Minute))
	assert.Zero(t, soonerRequeue(0, 0))
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
	r := &GitTargetReconciler{WorkerManager: workers}
	_, seen = r.observeRemote(target, "shop")
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
// the fix works only if a field that was there and is now nil is emitted as an explicit null. A
// patch that simply omitted it would leave the old repository's revision in etcd forever.
func TestPublishRemote_TheRemovalReachesTheAPIAsADelete(t *testing.T) {
	target := remoteTestTarget()
	// A condition, so the patch is computed against a status that outlives the stanza being
	// removed: the whole point is that `remote` alone is deleted.
	target.Status.Conditions = []metav1.Condition{{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready",
		LastTransitionTime: metav1.NewTime(time.Now()), ObservedGeneration: 4,
	}}
	at := time.Now()
	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: at, By: git.ObservedByPush, Repo: firstRepo,
	}, true, firstRepo, at, time.Hour)
	before := target.DeepCopy()

	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: at, By: git.ObservedByPush, Repo: firstRepo,
	}, true, secondRepo, at, time.Hour)

	data, err := client.MergeFrom(before).Data(target)
	require.NoError(t, err)
	assert.JSONEq(t, `{"status":{"remote":null}}`, string(data))
}
