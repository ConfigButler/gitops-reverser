// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
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
	publishRemote(target, git.RemoteObservation{}, false, time.Minute)
	assert.Nil(t, target.Status.Remote)
}

// TestPublishRemote_AnObservationWithNoRevisionIsStillPublished is the other half: the branch is
// not on the remote, and saying so is the answer rather than the absence of one.
func TestPublishRemote_AnObservationWithNoRevisionIsStillPublished(t *testing.T) {
	target := remoteTestTarget()
	at := time.Now()

	publishRemote(target, git.RemoteObservation{At: at, By: git.ObservedByFetch}, true, time.Minute)

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
	}, true, time.Hour)

	publishRemote(target, git.RemoteObservation{
		Revision: "bbbb", At: first.Add(time.Second), By: git.ObservedByPush,
	}, true, time.Hour)

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
	}, true, 10*time.Minute)
	published := target.Status.Remote

	// One steady tick later, on the same revision. Nothing to say.
	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: first.Add(5 * time.Minute), By: git.ObservedByFetch,
	}, true, 10*time.Minute)
	assert.Same(t, published, target.Status.Remote,
		"an unchanged revision inside the interval leaves the published value untouched")

	// A full interval later, the clock is worth a write: an operator reading "verified 40
	// minutes ago" on a target that is being refreshed would be reading a broken refresher.
	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: first.Add(10 * time.Minute), By: git.ObservedByFetch,
	}, true, 10*time.Minute)
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
	}, true, time.Hour)

	publishRemote(target, git.RemoteObservation{
		Revision: "aaaa", At: at.Add(time.Second), By: git.ObservedByFetch,
	}, true, time.Hour)

	assert.Equal(t, "Fetch", target.Status.Remote.VerifiedBy)
}
