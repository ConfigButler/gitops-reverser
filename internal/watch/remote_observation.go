// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// ReportRemoteObserved records where a GitTarget's branch was last proved to be. It is installed
// on the WorkerManager (git.RemoteReporter) at startup, and it is the only writer of the remotes
// projection.
//
// Every confirmed look calls it — a successful push as well as a refresh. The transition test
// lives here, as it does for layouts, but it is drawn differently: a NEW REVISION is always news,
// including one we pushed ourselves, because status.remote.revision answers "where is my branch"
// and the push is the event that changes the answer. Renewing only the clock is not news, and is
// left to the target's own steady tick.
//
// That enqueue puts a per-push series back onto gitPathEvents, which 14eeef46 deliberately
// cleared of high-volume traffic. PushCooldown floors it at one push per 5s per branch worker,
// well under what the buffer absorbs — and a dropped enqueue is benign here in a way it is not
// elsewhere on that channel: a dropped acceptance transition leaves a WRONG condition standing,
// a dropped observation only a TIMESTAMPED one that the next tick corrects.
func (m *Manager) ReportRemoteObserved(gitDest types.ResourceReference, observed git.RemoteObservation) {
	newAnswer := false
	m.mutateWatchPlane(func(s *watchPlaneState) bool {
		prior, had := s.remotes[gitDest.Key()]
		// A byte-identical report changes nothing an outside reader could see, and it is the
		// steady state: a refresh re-reports what it already knew on every tick, for every target
		// on the branch. Publishing it would clone the whole watch-plane snapshot to store the
		// value that is already there, which is the trap both sibling reporters avoid.
		if had && prior == observed {
			return false
		}
		// What wakes the controller is a change in the ANSWER to "where is my branch", which the
		// revision alone does not carry. A withdrawal and a branch the remote does not have both
		// have no revision and mean opposite things — one removes the stanza, the other publishes
		// it — so a move between them is news in both directions, and comparing revisions alone
		// left the wrong one standing until some other reconcile happened by.
		//
		// `By` is deliberately not compared: a revision that changes hands from a push to a fetch
		// is the same answer, and the target's own steady tick republishes it.
		newAnswer = !had || prior.Revision != observed.Revision || prior.Withdrawn != observed.Withdrawn
		s.remotes[gitDest.Key()] = observed
		return true
	})
	if newAnswer {
		m.enqueueGitTargetReconcile(gitDest)
	}
}

// RemoteForGitTarget returns the most recent observation of a GitTarget's branch, and whether
// anything has looked at all. Absent leaves status.remote unset, which is different from a
// present observation with no revision: that one means the branch is not on the remote.
func (m *Manager) RemoteForGitTarget(gitDest types.ResourceReference) (git.RemoteObservation, bool) {
	observed, ok := m.watchPlane().remotes[gitDest.Key()]
	return observed, ok
}
