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
