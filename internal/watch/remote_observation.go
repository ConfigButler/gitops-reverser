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
// Every confirmed look calls it — a successful push as well as a refresh — and the push is the
// busy one. The transition test lives here, as it does for layouts, but it is drawn differently
// and deliberately so: a NEW REVISION is always news, including one we pushed ourselves, because
// status.remote.revision answers "where is my branch" and the push is the event that changes the
// answer. Renewing only the clock is not news, and is left to the target's own steady tick, which
// republishes it as part of the pass it was making anyway.
//
// The enqueue that a new revision triggers puts a per-push series back onto gitPathEvents, which
// 14eeef46 deliberately cleared of high-volume traffic. Two things make that acceptable.
// PushCooldown floors the rate at one push per 5s per branch worker, well under what the buffer
// absorbs. And a dropped enqueue is more benign here than for anything else on that channel: a
// dropped acceptance transition leaves a WRONG condition standing, while a dropped observation
// leaves a TIMESTAMPED one — the operator reads an older lastVerifiedAt and can see exactly how
// old it is, and the next tick corrects it. If the buffer ever becomes a problem, this is the
// first series to move off it, for the same reason it is the safest to drop.
func (m *Manager) ReportRemoteObserved(gitDest types.ResourceReference, observed git.RemoteObservation) {
	newRevision := false
	m.mutateWatchPlane(func(s *watchPlaneState) bool {
		prior, had := s.remotes[gitDest.Key()]
		newRevision = !had || prior.Revision != observed.Revision
		s.remotes[gitDest.Key()] = observed
		return true
	})
	if newRevision {
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
