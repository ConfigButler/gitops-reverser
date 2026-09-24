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
	publishRemote(target, git.RemoteObservation{}, false, git.RepoIdentity{}, time.Minute)
	assert.Nil(t, target.Status.Remote)
}

// TestPublishRemote_AnObservationWithNoRevisionIsStillPublished is the other half: the branch is
// not on the remote, and saying so is the answer rather than the absence of one.
func TestPublishRemote_AnObservationWithNoRevisionIsStillPublished(t *testing.T) {
	target := remoteTestTarget()
	at := time.Now()

	publishRemote(target, git.RemoteObservation{At: at, By: git.ObservedByFetch}, true, git.RepoIdentity{}, time.Minute)

	require.NotNil(t, target.Status.Remote)
	assert.Empty(t, target.Status.Remote.Revision)
	assert.Equal(t, "Fetch", target.Status.Remote.VerifiedBy)
	assert.Equal(t, at.Unix(), target.Status.Remote.LastVerifiedAt.Unix())
}

// TestPublishRemote_ANewRevisionAlwaysWrites is the rule the field exists for. A revision we
// pushed ourselves is still news: the push is the event that changes where the branch is, and a
// field lagging it by an interval would be a slower copy of the answer rather than a fresh one.
func TestPublishRemote_ANewRevisionAlwaysWrites(t *testing.T) {
	target := remoteTestTarget()
	first := time.Now()
	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: first, By: git.ObservedByPush,
	}, true, git.RepoIdentity{}, time.Hour)

	publishRemote(target, git.RemoteObservation{
		Revision: "bbbb", At: first.Add(time.Second), By: git.ObservedByPush,
	}, true, git.RepoIdentity{}, time.Hour)

	require.NotNil(t, target.Status.Remote)
	assert.Equal(t, "bbbb", target.Status.Remote.Revision,
		"a second push a second later moves the revision, whatever the quantizer says")
}

// TestPublishRemote_AnUnchangedRevisionIsQuantized is the trap sameLayout was written to avoid,
// tested from the other side: a converged target must not write status once per tick merely to
// advance a clock.
func TestPublishRemote_AnUnchangedRevisionIsQuantized(t *testing.T) {
	target := remoteTestTarget()
	first := time.Now()
	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: first, By: git.ObservedByFetch,
	}, true, git.RepoIdentity{}, 10*time.Minute)
	published := target.Status.Remote

	// One steady tick later, on the same revision. Nothing to say.
	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: first.Add(5 * time.Minute), By: git.ObservedByFetch,
	}, true, git.RepoIdentity{}, 10*time.Minute)
	assert.Same(t, published, target.Status.Remote,
		"an unchanged revision inside the interval leaves the published value untouched")

	// A full interval later, the clock is worth a write: an operator reading "verified 40
	// minutes ago" on a target that is being refreshed would be reading a broken refresher.
	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: first.Add(10 * time.Minute), By: git.ObservedByFetch,
	}, true, git.RepoIdentity{}, 10*time.Minute)
	assert.NotSame(t, published, target.Status.Remote)
	assert.Equal(t, first.Add(10*time.Minute).Unix(), target.Status.Remote.LastVerifiedAt.Unix())
}

// TestPublishRemote_SourceChangeIsNews covers the field that answers "is this revision our own
// work". A branch that moved under us is read off exactly this transition.
func TestPublishRemote_SourceChangeIsNews(t *testing.T) {
	target := remoteTestTarget()
	at := time.Now()
	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: at, By: git.ObservedByPush,
	}, true, git.RepoIdentity{}, time.Hour)

	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: at.Add(time.Second), By: git.ObservedByFetch,
	}, true, git.RepoIdentity{}, time.Hour)

	assert.Equal(t, "Fetch", target.Status.Remote.VerifiedBy)
}

// TestPublishRemote_ARevisionFromAnotherRepositoryIsRemoved. The published revision names a
// commit in a repository this GitTarget no longer points at — its GitProvider was recreated
// against a different URL, and the observation still in the projection was proved against the old
// one. There is nothing to replace it with until a look at the new repository succeeds, and if
// that repository is unreachable there never will be, so the stanza goes rather than going stale.
func TestPublishRemote_ARevisionFromAnotherRepositoryIsRemoved(t *testing.T) {
	target := remoteTestTarget()
	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: time.Now(), By: git.ObservedByPush, Repo: firstRepo,
	}, true, firstRepo, time.Hour)
	require.NotNil(t, target.Status.Remote)

	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: time.Now(), By: git.ObservedByPush, Repo: firstRepo,
	}, true, secondRepo, time.Hour)

	assert.Nil(t, target.Status.Remote,
		"a revision from a repository this target no longer uses must not be left standing")
}

// TestPublishRemote_TheRemovalIsNotQuantized. The quantizer exists to stop a converged target
// writing status once a tick to advance a clock. Removing a revision that is about the wrong
// repository is a correction, not a clock, so it must never be held back by it.
func TestPublishRemote_TheRemovalIsNotQuantized(t *testing.T) {
	target := remoteTestTarget()
	at := time.Now()
	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: at, By: git.ObservedByFetch, Repo: firstRepo,
	}, true, firstRepo, 10*time.Minute)

	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: at.Add(time.Second), By: git.ObservedByFetch, Repo: firstRepo,
	}, true, secondRepo, 10*time.Minute)

	assert.Nil(t, target.Status.Remote)
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
			publishRemote(target, git.RemoteObservation{
				Revision: "aaaa", At: time.Now(), By: git.ObservedByPush, Repo: tc.observed,
			}, true, tc.current, time.Hour)

			require.NotNil(t, target.Status.Remote)
			assert.Equal(t, "aaaa", target.Status.Remote.Revision)
		})
	}
}

// TestPublishRemote_TheNewRepositoryRepopulatesTheStanza is the other side of the removal: the
// first look at the repository the target points at NOW publishes again, with no latch to clear.
func TestPublishRemote_TheNewRepositoryRepopulatesTheStanza(t *testing.T) {
	target := remoteTestTarget()
	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: time.Now(), By: git.ObservedByPush, Repo: firstRepo,
	}, true, secondRepo, time.Hour)
	require.Nil(t, target.Status.Remote)

	publishRemote(target, git.RemoteObservation{
		Revision: "bbbb", At: time.Now(), By: git.ObservedByFetch, Repo: secondRepo,
	}, true, secondRepo, time.Hour)

	require.NotNil(t, target.Status.Remote)
	assert.Equal(t, "bbbb", target.Status.Remote.Revision)
}

// TestRemoteStatusIsNews_FallsBackToTheDefaultInterval. The quantum comes from the configured
// refresh interval, and the refresher can be turned off — which must not turn the quantizer into
// "write every tick". A zero falls back to the default rather than to no bound at all.
func TestRemoteStatusIsNews_FallsBackToTheDefaultInterval(t *testing.T) {
	at := metav1.NewTime(time.Now())
	published := &configbutleraiv1alpha3.GitTargetRemoteStatus{
		Revision: "aaaa", LastVerifiedAt: &at, VerifiedBy: "Fetch",
	}
	within := metav1.NewTime(at.Add(DefaultGitRefreshInterval / 2))
	beyond := metav1.NewTime(at.Add(DefaultGitRefreshInterval))

	assert.False(t, remoteStatusIsNews(published, &configbutleraiv1alpha3.GitTargetRemoteStatus{
		Revision: "aaaa", LastVerifiedAt: &within, VerifiedBy: "Fetch",
	}, 0))
	assert.True(t, remoteStatusIsNews(published, &configbutleraiv1alpha3.GitTargetRemoteStatus{
		Revision: "aaaa", LastVerifiedAt: &beyond, VerifiedBy: "Fetch",
	}, 0))
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

	assert.True(t, remoteStatusIsNews(without, withClock, time.Hour))
	assert.True(t, remoteStatusIsNews(withClock, without, time.Hour))
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
	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: time.Now(), By: git.ObservedByPush, Repo: firstRepo,
	}, true, firstRepo, time.Hour)
	before := target.DeepCopy()

	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: time.Now(), By: git.ObservedByPush, Repo: firstRepo,
	}, true, secondRepo, time.Hour)

	data, err := client.MergeFrom(before).Data(target)
	require.NoError(t, err)
	assert.JSONEq(t, `{"status":{"remote":null}}`, string(data))
}
