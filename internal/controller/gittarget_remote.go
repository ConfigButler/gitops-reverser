// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// observeRemote reads what the data plane last proved about this GitTarget's BRANCH.
//
// The observation is shared: one branch worker serves every GitTarget on its (provider, branch),
// and what it proved — by a push, or by a fetch — is one fact about the branch, available to all
// of them at the moment it was proved, carrying the time it was proved. Sharing it costs nothing
// and re-proving it per target would cost a connection each.
//
// Absent means nothing has looked yet, which is genuinely different from a look that found no
// branch: the first leaves status.remote unset, the second publishes an observation with no
// revision.
func (r *GitTargetReconciler) observeRemote(
	target *configbutleraiv1alpha3.GitTarget,
	providerNS string,
) (git.RemoteObservation, bool) {
	if r.WorkerManager == nil {
		return git.RemoteObservation{}, false
	}
	return r.WorkerManager.RemoteForBranch(git.BranchKey{
		RepoNamespace: providerNS,
		RepoName:      target.Spec.GitProviderRef.Name,
		Branch:        target.Spec.Branch,
	})
}

// publishRemote writes status.remote, subject to the rule that bounds its write rate, and returns
// how long until this target owes another look at its own status — zero when it owes none.
//
// DELIVERY IS NOT PUBLICATION. An observation reaches every GitTarget on the branch the instant it
// is proved, in memory, for nothing. A status write is the opposite kind of thing: fanning one
// push out to ten targets sharing a branch turns it into ten etcd writes, each invalidating the
// cached copy every watcher of the type holds. So the tuple is SAMPLED here rather than published
// on every change, and nothing in the data plane wakes a target because a branch moved.
//
// The bound is a wall clock: at most one remote status write per GitTarget per
// RemotePublicationInterval, measured from the observation currently published rather than from
// the reconcile cadence or from the gap between two observations. When this declines to publish
// something new it says how long is left, and the reconcile requeues on that — so the write lands
// at the deadline instead of waiting for the next steady tick, and a deadline shorter than the
// cadence means what it says.
//
// Two things are still immediate, because they are health rather than throughput: the FIRST
// observation, which is the difference between "nothing has looked" and an answer, and clearing a
// revision that names a repository this target no longer points at.
//
// What this promises, stated plainly: status.remote can lag the latest observation by up to one
// publication interval, so an operator who pushes and immediately reads status may still see the
// previous revision. It promises nothing at all about the branch itself — an unreachable remote
// yields no observation, and the pair (revision, lastVerifiedAt) is what says so.
func publishRemote(
	target *configbutleraiv1alpha3.GitTarget,
	observed git.RemoteObservation,
	seen bool,
	repo git.RepoIdentity,
	now time.Time,
	interval time.Duration,
) time.Duration {
	if !seen {
		// Nothing has looked at this branch since the process started. What is published is the
		// last thing anyone proved about it, and there is nothing better to replace it with.
		return 0
	}
	// The observation names the repository it was proved against; repo is the one the GitTarget's
	// GitProvider names NOW. When they differ, what is published is a revision in a repository
	// this target no longer points at, and it has to be REMOVED rather than replaced: there is
	// nothing to replace it with until a look at the new repository succeeds, and an unreachable
	// new remote would otherwise leave the old one's revision standing indefinitely.
	//
	// It is derived here, on every tick, rather than latched by whoever noticed the repoint. That
	// is what makes it right for the SIBLING targets on the same branch, which each reach this on
	// their own reconcile without anybody having to keep a correction alive for them.
	//
	// Both halves have to be known for the comparison to mean anything: an unreadable GitProvider
	// names no repository, and a worker with no identity of its own records none. Neither is
	// evidence that what is published is about the wrong one.
	if !repo.IsZero() && !observed.Repo.IsZero() && observed.Repo != repo {
		target.Status.Remote = nil
		return 0
	}
	next := &configbutleraiv1alpha3.GitTargetRemoteStatus{
		Revision:       observed.Revision,
		LastVerifiedAt: &metav1.Time{Time: observed.At},
		VerifiedBy:     string(observed.By),
	}
	published := target.Status.Remote
	if !remoteStatusIsNews(published, next) {
		return 0
	}
	if interval <= 0 {
		interval = RemotePublicationInterval
	}
	if published != nil && published.LastVerifiedAt != nil {
		if age := now.Sub(published.LastVerifiedAt.Time); age < interval {
			return interval - age
		}
	}
	target.Status.Remote = next
	return 0
}

// remoteStatusIsNews decides whether an observation says anything the published one does not.
//
// A CHANGED REVISION is news, and so is the same revision proved again later — that is what makes
// lastVerifiedAt a freshness field rather than decoration. What is deliberately NOT news is
// verifiedBy alone: a revision that changes hands from a Push to a Fetch is the same answer proved
// a second way, and the watch plane has always ignored it for that reason. The two layers agree.
//
// News is not permission to write. It is the first of two tests; publishRemote applies the rate
// bound to whatever passes here.
func remoteStatusIsNews(published, next *configbutleraiv1alpha3.GitTargetRemoteStatus) bool {
	if published == nil || published.LastVerifiedAt == nil || next.LastVerifiedAt == nil {
		return true
	}
	if published.Revision != next.Revision {
		return true
	}
	// metav1.Time is second-granular on the wire, so this compares against what was PUBLISHED: a
	// sub-second difference the API server would round away is not movement. It cannot leak a
	// write per tick either, because the rate bound is a whole publication interval wide.
	return next.LastVerifiedAt.Time.After(published.LastVerifiedAt.Time)
}
