// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// observeRemote reads the observation the data plane last made of this GitTarget's branch.
//
// Absent means nothing has looked yet, which is genuinely different from a look that found no
// branch: the first leaves status.remote unset, the second publishes an observation with no
// revision.
func (r *GitTargetReconciler) observeRemote(
	target *configbutleraiv1alpha3.GitTarget,
) (git.RemoteObservation, bool) {
	if r.EventRouter == nil || r.EventRouter.WatchManager == nil {
		return git.RemoteObservation{}, false
	}
	return r.EventRouter.WatchManager.RemoteForGitTarget(
		types.NewResourceReference(target.Name, target.Namespace))
}

// publishRemote writes status.remote, subject to the rule that keeps its write rate honest.
//
// A converged GitTarget writes nothing on its steady tick, because the status patch no-ops on an
// empty diff. A clock in status ends that unless something governs it, and republishing a
// timestamp nobody reads a decision from — once per target per tick, forever — is the trap
// sameLayout was written to avoid.
//
// So: write whenever the REVISION changes, including one we pushed ourselves, and otherwise only
// when the published timestamp is older than one refresh interval. The revision has to move with
// the fact it dates; a field showing a ten-minute-old revision on a target somebody is actively
// editing is a slower copy of the answer, not a freshness field. The push case is also the cheap
// one: beside a packfile upload and an accepted ref update, one status patch against the local
// API server is noise, and PushCooldown floors it at one push per 5s per branch worker.
func publishRemote(
	target *configbutleraiv1alpha3.GitTarget,
	observed git.RemoteObservation,
	seen bool,
	quantum time.Duration,
) {
	if !seen {
		return
	}
	// A withdrawal is not an observation: the data plane is saying that what is published names a
	// revision in a repository this target no longer points at. It has to be REMOVED rather than
	// replaced, because there is nothing to replace it with until a look at the new repository
	// succeeds — and an unreachable new remote would otherwise leave the old one's revision
	// standing indefinitely.
	if observed.Withdrawn {
		target.Status.Remote = nil
		return
	}
	next := &configbutleraiv1alpha3.GitTargetRemoteStatus{
		Revision:       observed.Revision,
		LastVerifiedAt: &metav1.Time{Time: observed.At},
		VerifiedBy:     string(observed.By),
	}
	if !remoteStatusIsNews(target.Status.Remote, next, quantum) {
		return
	}
	target.Status.Remote = next
}

// remoteStatusIsNews decides whether an observation is worth a status write.
//
// metav1.Time is second-granular on the wire, so the age is compared against what was PUBLISHED
// rather than against an in-memory value: a sub-second difference that the API server would round
// away must not count as movement, or the quantizer would leak one write per tick.
func remoteStatusIsNews(
	published, next *configbutleraiv1alpha3.GitTargetRemoteStatus,
	quantum time.Duration,
) bool {
	if published == nil {
		return true
	}
	if published.Revision != next.Revision || published.VerifiedBy != next.VerifiedBy {
		return true
	}
	if published.LastVerifiedAt == nil || next.LastVerifiedAt == nil {
		return true
	}
	if quantum <= 0 {
		quantum = DefaultGitRefreshInterval
	}
	return next.LastVerifiedAt.Time.Sub(published.LastVerifiedAt.Time) >= quantum
}
