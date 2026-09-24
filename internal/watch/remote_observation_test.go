// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestReportRemoteObserved_EnqueuesOnANewRevisionOnly draws the transition test where
// status.remote needs it, which is not where layouts need it. A moved branch is what an operator
// is waiting to see, so it must not wait out the steady requeue; renewing only the clock is not
// news, and enqueueing for it would put a reconcile per refresh per target on a channel that was
// deliberately cleared of high-volume traffic.
func TestReportRemoteObserved_EnqueuesOnANewRevisionOnly(t *testing.T) {
	m := &Manager{}
	events := m.GitPathEvents()
	gitDest := types.NewResourceReference("checkout", "shop")
	at := time.Now()

	m.ReportRemoteObserved(gitDest, git.RemoteObservation{Revision: "aaaa", At: at, By: git.ObservedByPush})
	require.Len(t, events, 1, "the first observation is a change: nothing was known before")

	m.ReportRemoteObserved(gitDest, git.RemoteObservation{
		Revision: "aaaa", At: at.Add(10 * time.Minute), By: git.ObservedByFetch,
	})
	assert.Len(t, events, 1, "a refresh that finds the branch where it was is not news")

	m.ReportRemoteObserved(gitDest, git.RemoteObservation{
		Revision: "bbbb", At: at.Add(11 * time.Minute), By: git.ObservedByFetch,
	})
	assert.Len(t, events, 2, "a branch that moved is exactly what the field is read for")
}

// TestReportRemoteObserved_KeepsTheClockEvenWithoutEnqueueing is the other half of the rule
// above: the projection always carries the latest look, so the target's own steady tick
// republishes a renewed clock without anything having to wake it.
func TestReportRemoteObserved_KeepsTheClockEvenWithoutEnqueueing(t *testing.T) {
	m := &Manager{}
	gitDest := types.NewResourceReference("checkout", "shop")
	at := time.Now()

	m.ReportRemoteObserved(gitDest, git.RemoteObservation{Revision: "aaaa", At: at, By: git.ObservedByPush})
	m.ReportRemoteObserved(gitDest, git.RemoteObservation{
		Revision: "aaaa", At: at.Add(10 * time.Minute), By: git.ObservedByFetch,
	})

	observed, ok := m.RemoteForGitTarget(gitDest)
	require.True(t, ok)
	assert.Equal(t, at.Add(10*time.Minute), observed.At)
	assert.Equal(t, git.ObservedByFetch, observed.By)
}

// TestRemoteForGitTarget_AbsentUntilSomethingLooks keeps "nothing has looked" distinguishable
// from "we looked and the branch is not there", which the controller publishes differently.
func TestRemoteForGitTarget_AbsentUntilSomethingLooks(t *testing.T) {
	m := &Manager{}
	_, ok := m.RemoteForGitTarget(types.NewResourceReference("checkout", "shop"))
	assert.False(t, ok)
}

// TestReportRemoteObserved_AnIdenticalReportPublishesNothing. Every refresh tick re-reports what
// it already knew, for every target on the branch, and every push reports too. Storing a value
// that is already there would clone the whole watch-plane snapshot to change nothing — the trap
// both sibling reporters short-circuit on.
func TestReportRemoteObserved_AnIdenticalReportPublishesNothing(t *testing.T) {
	m := &Manager{}
	gitDest := types.NewResourceReference("checkout", "shop")
	observed := git.RemoteObservation{Revision: "aaaa", At: time.Now(), By: git.ObservedByPush}

	m.ReportRemoteObserved(gitDest, observed)
	published := m.watchPlane()

	m.ReportRemoteObserved(gitDest, observed)

	assert.Same(t, published, m.watchPlane(),
		"a byte-identical report changes nothing a reader could see, so nothing is republished")
}

// TestReportRemoteObserved_ANewRepositoryWakesTheTarget is the correction path. A replacement
// branch worker reports against a different repository, and the target has to reconcile: the
// revision published from the old one describes a repository it no longer points at, and the
// controller is the only thing that can take it out of status.
func TestReportRemoteObserved_ANewRepositoryWakesTheTarget(t *testing.T) {
	m := &Manager{}
	events := m.GitPathEvents()
	gitDest := types.NewResourceReference("checkout", "shop")
	at := time.Now()

	m.ReportRemoteObserved(gitDest, git.RemoteObservation{
		Revision: "aaaa", At: at, By: git.ObservedByPush, Repo: firstRepo,
	})
	require.Len(t, events, 1)

	elsewhere := git.RemoteObservation{
		Revision: "aaaa", At: at.Add(time.Second), By: git.ObservedByFetch, Repo: secondRepo,
	}
	m.ReportRemoteObserved(gitDest, elsewhere)

	observed, ok := m.RemoteForGitTarget(gitDest)
	require.True(t, ok)
	assert.Equal(t, secondRepo, observed.Repo)
	assert.Len(t, events, 2,
		"the same hash in a different repository is a different answer, and only a reconcile publishes it")

	m.ReportRemoteObserved(gitDest, elsewhere)
	assert.Len(t, events, 2, "and re-reporting it is not news again")
}

// TestTearDownGitTarget_DropsTheRemoteObservation. The projection is keyed by namespace/name, and
// a GitTarget deleted and recreated under the same name is a different target that may point at a
// different branch entirely. Leaving the observation behind would hand the new one its
// predecessor's revision before anything had looked on its behalf.
func TestTearDownGitTarget_DropsTheRemoteObservation(t *testing.T) {
	m := &Manager{}
	gitDest := types.NewResourceReference("checkout", "shop")
	m.ReportRemoteObserved(gitDest, git.RemoteObservation{
		Revision: "aaaa", At: time.Now(), By: git.ObservedByPush,
	})
	_, seen := m.RemoteForGitTarget(gitDest)
	require.True(t, seen)

	m.tearDownGitTarget(gitDest)

	_, seen = m.RemoteForGitTarget(gitDest)
	assert.False(t, seen, "a deleted GitTarget leaves nothing behind for its successor to inherit")
}

// TestReportRemoteObserved_WakesOnEveryChangeOfAnswer. "The remote does not carry this branch" has
// no revision, and it means opposite things in two repositories: in the one the target points at
// it is publishable, and from the one it has left it is a stanza that must come out. Comparing
// revisions alone left whichever was published standing until some unrelated reconcile happened by.
func TestReportRemoteObserved_WakesOnEveryChangeOfAnswer(t *testing.T) {
	at := time.Now()
	absentIn := func(repo git.RepoIdentity) git.RemoteObservation {
		return git.RemoteObservation{At: at, By: git.ObservedByFetch, Repo: repo}
	}

	t.Run("an absent branch in another repository", func(t *testing.T) {
		m := &Manager{}
		events := m.GitPathEvents()
		gitDest := types.NewResourceReference("checkout", "shop")

		m.ReportRemoteObserved(gitDest, absentIn(firstRepo))
		require.Len(t, events, 1)

		m.ReportRemoteObserved(gitDest, absentIn(secondRepo))
		assert.Len(t, events, 2, "the stanza has to come out of status, and only a reconcile does that")
	})

	t.Run("and back again", func(t *testing.T) {
		m := &Manager{}
		events := m.GitPathEvents()
		gitDest := types.NewResourceReference("checkout", "shop")

		m.ReportRemoteObserved(gitDest, absentIn(secondRepo))
		require.Len(t, events, 1)

		m.ReportRemoteObserved(gitDest, absentIn(firstRepo))
		assert.Len(t, events, 2,
			"the other repository answered — it does not carry the branch — and that is publishable")
	})
}

// The two repositories a repoint moves between; a recreated GitProvider mints a new UID.
var (
	firstRepo  = git.RepoIdentity{ProviderUID: "uid-1", URL: "https://example.invalid/first.git"}
	secondRepo = git.RepoIdentity{ProviderUID: "uid-2", URL: "https://example.invalid/second.git"}
)
