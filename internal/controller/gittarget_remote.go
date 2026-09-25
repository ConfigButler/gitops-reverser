// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// remotePublicationLedger remembers, per GitTarget, what its last status.remote WRITE was: when it
// happened, and which repository the revision it published was proved against.
//
// Both halves are deliberately internal. The write TIME is what a rate limit has to measure
// against — the observation's own timestamp is not, because an observation made just before a
// publication and a fresh one made just after are a second apart while the two writes are not.
// The REPOSITORY is what makes invalidation follow this target's published state rather than the
// latest shared observation: one branch worker serves every GitTarget on a branch, so a sibling
// can already have proved the replacement repository while this target still publishes a revision
// from the one it has left, and comparing the shared observation against the provider would find
// them in agreement and leave the wrong revision standing.
//
// Losing it costs nothing. An empty ledger publishes the next observation immediately and then
// resumes limiting, which is the right behaviour after a restart: what is published is older than
// the process.
type remotePublicationLedger struct {
	mu      sync.Mutex
	entries map[string]remotePublication
}

type remotePublication struct {
	// uid is the GitTarget object the entry is about. A ledger keyed by namespace/name alone
	// outlives the object that earned it: cleanup needs a reconcile that observes NotFound, and a
	// delete followed quickly by a recreate can be reconciled from the successor directly. The
	// successor would then inherit its predecessor's cooldown and have its FIRST observation
	// suppressed, with nothing published to be a rate against.
	uid  k8stypes.UID
	at   time.Time
	repo git.RepoIdentity
}

// last returns what this GitTarget published, and whether the ledger knows. An entry belonging to
// a predecessor under the same name is not this object's, so it answers "nothing".
func (l *remotePublicationLedger) last(ref types.ResourceReference, uid k8stypes.UID) (remotePublication, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, had := l.entries[ref.Key()]
	if !had || entry.uid != uid {
		return remotePublication{}, false
	}
	return entry, true
}

func (l *remotePublicationLedger) record(
	ref types.ResourceReference,
	uid k8stypes.UID,
	at time.Time,
	repo git.RepoIdentity,
) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entries == nil {
		l.entries = map[string]remotePublication{}
	}
	l.entries[ref.Key()] = remotePublication{uid: uid, at: at, repo: repo}
}

func (l *remotePublicationLedger) forget(ref types.ResourceReference) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, ref.Key())
}

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

// publishRemote writes status.remote, sampled onto this reconcile and rate-limited.
//
// DELIVERY IS NOT PUBLICATION. An observation reaches every GitTarget on the branch the instant it
// is proved, in memory, for nothing. A status write is the opposite kind of thing: fanning one
// push out to ten targets sharing a branch turns it into ten etcd writes, each invalidating the
// cached copy every watcher of the type holds. So this publishes what the target holds AT THE
// MOMENT IT RECONCILES, and nothing in the data plane wakes a target because a branch moved.
//
// The cadence is therefore the target's own: a converged one samples every RequeueSteadyInterval,
// a busy one much sooner, and nobody is scheduled a wake-up for a status field. On top of that
// sits one floor, RemotePublicationInterval, so that a target reconciling in a tight loop still
// writes this stanza at most once a minute. The floor is measured from the last WRITE, kept in the
// ledger above, not from the observation's timestamp.
//
// Two things ignore the floor, because they are correctness rather than throughput: the FIRST
// observation, which is the difference between "nothing has looked" and an answer, and any
// correction that involves the repository changing under the target.
func (r *GitTargetReconciler) publishRemote(
	st *reconcileStatus,
	target *configbutleraiv1alpha3.GitTarget,
	observed git.RemoteObservation,
	seen bool,
	repo git.RepoIdentity,
	now time.Time,
) {
	ref := types.NewResourceReference(target.Name, target.Namespace)
	published, everPublished := r.remotePublications.last(ref, target.UID)
	// What this target PUBLISHED was proved against a repository it no longer points at: its
	// GitProvider was recreated against a different URL.
	staleRepository := everPublished && differentRepository(published.repo, repo)

	if withdrawRemote(staleRepository, seen, observed, repo) {
		// Nothing proved about the repository in use now, so there is nothing to replace what is
		// published with, and it has to go rather than go stale.
		target.Status.Remote = nil
		st.afterPersist(func() { r.remotePublications.forget(ref) })
		return
	}
	if !seen {
		// Nothing has looked at this branch since the process started. What is published is the
		// last thing anyone proved about it, and there is nothing better to replace it with.
		return
	}
	next := &configbutleraiv1alpha3.GitTargetRemoteStatus{
		Revision:       observed.Revision,
		LastVerifiedAt: &metav1.Time{Time: observed.At},
		VerifiedBy:     string(observed.By),
	}
	if !remoteStatusIsNews(target.Status.Remote, next) {
		return
	}
	// The floor, skipped for the two corrections above and for the first publication.
	if everPublished && !staleRepository && now.Sub(published.at) < RemotePublicationInterval {
		return
	}
	target.Status.Remote = next
	// Only once the write has landed, and dated THEN rather than now. A patch refused by the
	// optimistic lock leaves the object holding the winner's older answer, and a ledger that
	// recorded this publication anyway would hold the retry off behind a cooldown for a revision
	// nobody can read. The timestamp is the moment of persistence for the same reason the
	// callback exists: `now` was taken before the gates, the worker wiring and the patch itself,
	// so on a slow reconcile a cooldown dated from it can be half spent before the write lands,
	// and a reconcile queued behind it would publish again straight away.
	st.afterPersist(func() { r.remotePublications.record(ref, target.UID, r.clockNow(), observed.Repo) })
}

// withdrawRemote decides whether what is published has to be taken back rather than replaced.
//
// Two questions, and they are asked of different evidence. The ledger knows which repository THIS
// target published, which is what a sibling's observation cannot answer: one worker serves every
// GitTarget on a branch, so a sibling can already have proved the replacement while this target
// still publishes a revision from the repository it has left. The observation answers for the case
// the ledger cannot — a restart, or a target that has not published under this process — because
// it names the repository it was proved against.
//
// Both comparisons need two known halves: an unreadable GitProvider names no repository, and a
// worker with no identity of its own records none. Neither is evidence of a mismatch.
func withdrawRemote(staleRepository, seen bool, observed git.RemoteObservation, repo git.RepoIdentity) bool {
	if staleRepository && (!seen || differentRepository(observed.Repo, repo)) {
		return true
	}
	return seen && differentRepository(observed.Repo, repo)
}

// differentRepository reports a mismatch only when both identities are known.
func differentRepository(a, b git.RepoIdentity) bool {
	return !a.IsZero() && !b.IsZero() && a != b
}

// remoteStatusIsNews decides whether an observation says anything the published one does not.
//
// A CHANGED REVISION is news, and so is the same revision proved again later — that is what makes
// lastVerifiedAt a freshness field rather than decoration. What is deliberately NOT news is
// verifiedBy alone: a revision that changes hands from a Push to a Fetch is the same answer proved
// a second way, and the watch plane has always ignored it for that reason. The two layers agree.
//
// News is not permission to write. It is the first of two tests; publishRemote applies the write
// floor to whatever passes here.
func remoteStatusIsNews(published, next *configbutleraiv1alpha3.GitTargetRemoteStatus) bool {
	if published == nil || published.LastVerifiedAt == nil || next.LastVerifiedAt == nil {
		return true
	}
	if published.Revision != next.Revision {
		return true
	}
	// metav1.Time is second-granular on the wire, so this compares against what was PUBLISHED: a
	// sub-second difference the API server would round away is not movement.
	return next.LastVerifiedAt.Time.After(published.LastVerifiedAt.Time)
}
