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

// remotePublicationLedger remembers, per GitBranch, what its last status.remote WRITE was: when it
// happened, and which repository the revision it published was proved against.
//
// Both halves are deliberately internal. The write TIME is what a rate limit has to measure
// against — the observation's own timestamp is not, because an observation made just before a
// publication and a fresh one made just after are a second apart while the two writes are not.
// The REPOSITORY is what makes invalidation follow this GitBranch's published state rather than the
// latest observation: the observation can already have been proved against the replacement
// repository while this GitBranch still publishes a revision from the one it has left, and comparing
// the observation against the provider would find them in agreement and leave the wrong revision
// standing.
//
// Losing it costs nothing. An empty ledger publishes the next observation immediately and then
// resumes limiting, which is the right behaviour after a restart: what is published is older than
// the process.
type remotePublicationLedger struct {
	mu      sync.Mutex
	entries map[string]remotePublication
}

type remotePublication struct {
	// uid is the GitBranch object the entry is about. A ledger keyed by namespace/name alone
	// outlives the object that earned it: cleanup needs a reconcile that observes NotFound, and a
	// delete followed quickly by a recreate can be reconciled from the successor directly. The
	// successor would then inherit its predecessor's cooldown and have its FIRST observation
	// suppressed, with nothing published to be a rate against.
	uid  k8stypes.UID
	at   time.Time
	repo git.RepoIdentity
}

// last returns what this GitBranch published, and whether the ledger knows. An entry belonging to
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

// gitBranchRef is the ledger key of a GitBranch.
func gitBranchRef(branch *configbutleraiv1alpha3.GitBranch) types.ResourceReference {
	return types.NewResourceReference(branch.Name, branch.Namespace)
}

// observeRemote reads what the data plane last proved about this GitBranch's repository branch.
//
// Absent means nothing has looked yet, which is genuinely different from a look that found no
// branch: the first leaves status.remote unset, the second publishes an observation with no
// revision.
func (r *GitBranchReconciler) observeRemote(key git.BranchKey) (git.RemoteObservation, bool) {
	if r.WorkerManager == nil {
		return git.RemoteObservation{}, false
	}
	return r.WorkerManager.RemoteForBranch(key)
}

// publishRemote writes status.remote, sampled onto this reconcile and rate-limited.
//
// The branch worker wakes the GitBranch whenever it proves something (GitBranchEvents), and a busy
// branch proves something on every push. A status write per push would invalidate the cached copy
// every watcher of the type holds, so one floor, RemotePublicationInterval, keeps this stanza to at
// most one write a minute. The floor is measured from the last WRITE, kept in the ledger above, not
// from the observation's timestamp.
//
// Two things ignore the floor, because they are correctness rather than throughput: the FIRST
// observation, which is the difference between "nothing has looked" and an answer, and any
// correction that involves the repository changing under the GitBranch.
//
// It returns how long the floor is holding news back, zero when nothing is held. A held answer
// must not wait for whatever reconcile happens to come next: see requeueForRemoteAnswer.
func (r *GitBranchReconciler) publishRemote(
	st *reconcileStatus,
	branch *configbutleraiv1alpha3.GitBranch,
	observed git.RemoteObservation,
	seen bool,
	repo git.RepoIdentity,
	now time.Time,
) time.Duration {
	ref := gitBranchRef(branch)
	published, everPublished := r.remotePublications.last(ref, branch.UID)
	// What this GitBranch PUBLISHED was proved against a repository it no longer points at: its
	// GitProvider was recreated against a different URL.
	staleRepository := everPublished && differentRepository(published.repo, repo)

	if withdrawRemote(staleRepository, seen, observed, repo) {
		// Nothing proved about the repository in use now, so there is nothing to replace what is
		// published with, and it has to go rather than go stale.
		branch.Status.Remote = nil
		st.afterPersist(func() { r.remotePublications.forget(ref) })
		return 0
	}
	if !seen {
		// Nothing has looked at this branch since the process started. What is published is the
		// last thing anyone proved about it, and there is nothing better to replace it with.
		return 0
	}
	next := &configbutleraiv1alpha3.GitBranchRemoteStatus{
		Commit:         observed.Commit,
		LastVerifiedAt: &metav1.Time{Time: observed.At},
		VerifiedBy:     string(observed.By),
		Parent:         parentStatusOf(observed),
	}
	if !remoteStatusIsNews(branch.Status.Remote, next) {
		return 0
	}
	// The floor, skipped for the two corrections above and for the first publication.
	if everPublished && !staleRepository && now.Sub(published.at) < RemotePublicationInterval {
		return published.at.Add(RemotePublicationInterval).Sub(now)
	}
	branch.Status.Remote = next
	// Only once the write has landed, and dated THEN rather than now. A patch refused by the
	// optimistic lock leaves the object holding the winner's older answer, and a ledger that
	// recorded this publication anyway would hold the retry off behind a cooldown for a revision
	// nobody can read. The timestamp is the moment of persistence for the same reason the
	// callback exists: `now` was taken before the gates, the worker wiring and the patch itself,
	// so on a slow reconcile a cooldown dated from it can be half spent before the write lands,
	// and a reconcile queued behind it would publish again straight away.
	st.afterPersist(func() { r.remotePublications.record(ref, branch.UID, r.clockNow(), observed.Repo) })
	return 0
}

// withdrawRemote decides whether what is published has to be taken back rather than replaced.
//
// Two questions, and they are asked of different evidence. The ledger knows which repository THIS
// GitBranch published, which the latest observation cannot answer: it can already have proved the
// replacement while this GitBranch still publishes a revision from the repository it has left. The
// observation answers for the case the ledger cannot — a restart, or a GitBranch that has not
// published under this process — because it names the repository it was proved against.
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
func remoteStatusIsNews(published, next *configbutleraiv1alpha3.GitBranchRemoteStatus) bool {
	if published == nil || published.LastVerifiedAt == nil || next.LastVerifiedAt == nil {
		return true
	}
	if published.Commit != next.Commit || !equalParentStatus(published.Parent, next.Parent) {
		return true
	}
	// metav1.Time is second-granular on the wire, so this compares against what was PUBLISHED: a
	// sub-second difference the API server would round away is not movement.
	return next.LastVerifiedAt.Time.After(published.LastVerifiedAt.Time)
}

// parentStatusOf is the parent stanza of status.remote: present only while the observation found
// the write branch absent, which is when it says what the branch would be created from.
func parentStatusOf(observed git.RemoteObservation) *configbutleraiv1alpha3.GitBranchParentStatus {
	if observed.Commit != "" || observed.ParentState == "" {
		return nil
	}
	return &configbutleraiv1alpha3.GitBranchParentStatus{
		State:  configbutleraiv1alpha3.GitBranchParentState(observed.ParentState),
		Branch: observed.ParentBranch,
		Commit: observed.ParentCommit,
	}
}

func equalParentStatus(a, b *configbutleraiv1alpha3.GitBranchParentStatus) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// publishPushNotice writes status.lastPushNoticeAt from noticed, the worker's last push notice, and
// returns how long the floor holds a newer one back, zero when nothing is held.
//
// It is published on receipt, apart from status.remote: a delivery that reached the worker reached
// it whether or not the look it caused then read the remote, and the field is how a user tells a
// webhook that arrives from one that does not. A worker that has seen no notice since it started
// knows nothing newer, which is no reason to take back what was published. The floor is the remote
// stanza's interval, measured from the published notice: a busy repository is delivered to on every
// push, and a write per push would invalidate every watcher's cached copy.
func publishPushNotice(branch *configbutleraiv1alpha3.GitBranch, noticed, now time.Time) time.Duration {
	published := branch.Status.LastPushNoticeAt
	// metav1.Time is second-granular on the wire, so a notice is compared as it would be published.
	if noticed.IsZero() || (published != nil && !noticed.Truncate(time.Second).After(published.Time)) {
		return 0
	}
	if published != nil {
		if wait := published.Add(RemotePublicationInterval).Sub(now); wait > 0 {
			return wait
		}
	}
	branch.Status.LastPushNoticeAt = &metav1.Time{Time: noticed}
	return 0
}
