// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"errors"
	"fmt"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	gitclient "github.com/go-git/go-git/v6/plumbing/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// RefreshRequest asks a worker to re-prove where its branch is on the remote, unless something
// has proved it recently enough.
//
// It is enqueued from the GitTarget reconcile — a scheduled tick that already happens — and never
// called inline: the reconcile must not block on a network round trip, and the work belongs on
// the one goroutine allowed to touch the repository.
type RefreshRequest struct {
	// Target is the GitTarget this refresh is for, and the one its observation is reported
	// against. A worker serves every target on its (provider, branch); this names the one in
	// hand.
	Target itypes.ResourceReference
	// Path is the GitTarget's spec.path, so a refresh can re-read that folder and republish
	// what its shape resolves to. Empty skips the re-scan.
	Path string
	// MaxAge is how old an observation may be before it is topped up. A younger one makes the
	// refresh a no-op with no connection at all, which is what keeps an actively-publishing
	// branch free: its pushes renew the record continuously.
	MaxAge time.Duration
}

// EnqueueRefresh queues a refresh, dropping it if the queue is full.
//
// The drop is safe in a way it is not for a write: the next reconcile asks again, and a refresh
// must never occupy the slot a write needs. It is still counted, because a refresher that has
// stopped running is otherwise invisible.
func (w *BranchWorker) EnqueueRefresh(req *RefreshRequest) {
	if req == nil {
		return
	}
	w.inflightItems.Add(1)
	select {
	case w.eventQueue <- WorkItem{Refresh: req}:
	default:
		w.inflightItems.Add(-1)
		w.recordQueueDrop(queueDropRefresh)
		w.Log.V(1).Info("Event queue full, refresh dropped; the next reconcile asks again",
			"branch", w.Branch, "gitTarget", req.Target.String())
	}
}

// handleRefreshRequest is the refresher, on the loop goroutine.
//
// It reads Git freely and writes no CONTENT: mirroring what it reads would turn "somebody changed
// the folder" into a write nobody asked for. Reporting what it read is the whole point, including
// the verdict that the folder can no longer be written — see rescanLayoutForTarget for why that
// report is worth the recovery loop it starts.
func (l *branchWorkerEventLoop) handleRefreshRequest(req *RefreshRequest) {
	w := l.w

	// 1. The GitProvider first, because everything below is a statement about a REPOSITORY and
	// this worker may have been handed a different one. spec.url is immutable and repointed by
	// recreating the GitProvider, which the worker — keyed by (provider, branch) — survives, so
	// noteRemoteIdentity is what notices; it drops the checkout's trust and the observation.
	provider, err := w.getGitProvider(w.ctx)
	if err != nil {
		w.Log.V(1).Info("Skipping refresh: the GitProvider cannot be read",
			"branch", w.Branch, "gitTarget", req.Target.String(), "error", err.Error())
		return
	}
	w.noteRemoteIdentity(provider.Spec.URL)

	// 2. Report what is already known BEFORE deciding whether to do any work, and on every exit
	// below. A push reports only against the targets whose writes it carried, so a target that is
	// not writing learns where its branch is exclusively from its own tick.
	//
	// Once the repository has changed under us there is a withdrawal to report instead. Nothing
	// else would correct the published revision if the new remote is unreachable, and that
	// revision names a commit in a repository this target no longer points at.
	observed, known := w.LastRemoteObservation()
	switch {
	case known:
		w.reportRemoteObservation([]itypes.ResourceReference{req.Target}, observed)
	case w.observationWithdrawn():
		w.reportRemoteObservation([]itypes.ResourceReference{req.Target}, WithdrawnObservation())
	}

	// 3. Not idle, so not the target this exists for. A reset here would destroy retained
	// commits, and the worktree may hold a partial write — which is also why this is the one exit
	// that does not re-read the folder: a layout resolved from a half-written tree is worse than
	// a slightly old one. A target mid-cycle is writing, and a write publishes its own layout.
	if len(l.pendingWrites) > 0 || l.openWindow != nil || w.worktreeDirty() || w.replayRequired() {
		w.Log.V(1).Info("Skipping refresh: this branch is mid-cycle",
			"branch", w.Branch, "gitTarget", req.Target.String())
		return
	}

	// 4. Something proved this branch recently enough and nothing local is lagging behind that
	// proof, so there is nothing to ask the remote. This is where "a push is a refresh" is spent:
	// an actively-writing branch renews its observation on every push and never reaches the
	// connection below.
	//
	// The folder is still re-read. That is local work, and it is what keeps a SIBLING honest: the
	// fetch that moved this checkout may have been earned by another target's refresh, which
	// rescanned its own folder and knew nothing about this one.
	if known && req.MaxAge > 0 && observed.Age(time.Now()) < req.MaxAge &&
		!w.checkoutMayLagObservation(provider.Spec.URL, observed.Revision) {
		w.rescanLayoutForTarget(w.ctx, req)
		return
	}

	if err := l.refreshFromRemote(req, provider); err != nil {
		// A refresh that failed proves nothing, so nothing is recorded: the published
		// lastVerifiedAt stops advancing, which is precisely how a refresher that stopped
		// working reports itself.
		w.Log.Error(err, "Refresh failed to prove where the branch is",
			"branch", w.Branch, "gitTarget", req.Target.String())
		return
	}

	// 5. One re-read per tick, on every path that reached the remote, whether or not this tick
	// was the one that fetched.
	w.rescanLayoutForTarget(w.ctx, req)
}

// checkoutMayLagObservation reports whether the checkout may be sitting behind the recorded
// observation, which is what would make a fresh observation unsafe to reuse.
//
// An observation is recorded from the ADVERTISEMENT, before the fetch that acts on it, so a fetch
// that failed leaves a fresh observation standing next to a checkout that never moved onto it.
// Reusing it there would rescan the old tree and publish its layout until the observation aged
// out.
//
// Base trust answers that in one atomic read, and is the common case — but it is not the whole
// question. Trust is gained only by a fetch-and-reset or a push, so a target that has never
// published, and one whose branch the remote does not carry, never gain it; keying the interval
// on trust alone sent both to Git on EVERY reconcile, whatever interval was configured. So an
// untrusted base falls through to where the checkout actually is: no checkout cannot lag anything
// (the rescan reads nothing, and the first publication clones), and a HEAD already at the
// observed revision is not behind it.
func (w *BranchWorker) checkoutMayLagObservation(remoteURL, revision string) bool {
	if w.baseTrusted() {
		return false
	}
	repo, err := gogit.PlainOpen(w.repoPathForRemote(remoteURL))
	if errors.Is(err, gogit.ErrRepositoryNotExists) {
		return false
	}
	if err != nil {
		// A checkout too broken to open is treated as possibly lagging: the conservative
		// direction costs one advertisement, the other publishes an unread tree's layout.
		return true
	}
	// An empty revision means the branch is not on the remote, and localHead reads an unborn
	// local branch as the zero hash: both are "no commit", so neither is behind the other.
	observedHead := plumbing.ZeroHash
	if revision != "" {
		observedHead = plumbing.NewHash(revision)
	}
	return localHead(repo) != observedHead
}

// refreshFromRemote reads the remote's advertisement and, only if the branch has moved away from
// what the worktree holds, fetches and resets onto it.
//
// The split is the point. SmartFetch is two connections — it lists the refs and then fetches
// unconditionally — which is right for the write path, because it is about to reset either way.
// On an idle target the answer is almost always "the branch has not moved", and the advertisement
// alone settles it, so the common case here costs ONE connection.
func (l *branchWorkerEventLoop) refreshFromRemote(
	req *RefreshRequest,
	provider *configv1alpha3.GitProvider,
) error {
	w := l.w
	ctx := w.ctx

	auth, err := getAuthFromSecret(ctx, w.Client, provider, w.sshHostKeys, w.credentialPolicy)
	if err != nil {
		return fmt.Errorf("get auth: %w", err)
	}

	// One connection, and it deliberately does NOT go through the local checkout: "where is this
	// branch on the remote" is a question for the remote, and answering it needs no clone. That
	// is what makes it honest for the two targets a clone-first version skipped silently — one
	// declared but never published to, and one whose GitProvider was recreated against a
	// different repository.
	advertised, err := advertiseRemoteBranch(
		provider.Spec.URL, plumbing.NewBranchReferenceName(w.Branch), auth)
	if err != nil {
		return fmt.Errorf("read the remote advertisement: %w", err)
	}
	revision := ""
	if !advertised.IsZero() {
		revision = advertised.String()
	}
	observed := w.recordRemoteObservation(revision, ObservedByFetch)
	w.reportRemoteObservation([]itypes.ResourceReference{req.Target}, observed)

	// The remote does not carry this branch. That IS the answer, and there is nothing to fetch: a
	// fetch would fall back to the default branch and teach us nothing about a branch nobody has
	// created — the ordinary state of a target that has not written yet. A branch somebody
	// DELETED is caught by the compare-and-swap on the next push.
	if advertised.IsZero() {
		w.Log.V(1).Info("Refresh found no such branch on the remote", "branch", w.Branch)
		return nil
	}

	// Everything below compares the advertisement against the local checkout, so a missing one is
	// simply nothing to compare: the branch's position is already recorded and reported, and the
	// first publication will clone.
	repo, err := gogit.PlainOpen(w.repoPathForRemote(provider.Spec.URL))
	if errors.Is(err, gogit.ErrRepositoryNotExists) {
		w.Log.V(1).Info("Refresh observed the remote; there is no checkout to reset yet",
			"branch", w.Branch, "revision", revision)
		return nil
	}
	if err != nil {
		return fmt.Errorf("open repository: %w", err)
	}

	// The branch is where our checkout already is, so there is nothing to fetch. The caller still
	// re-reads the folder: this checkout may have been moved by a SIBLING target's refresh since
	// this target last looked at it.
	if w.baseTrusted() && advertised == localHead(repo) {
		w.Log.V(1).Info("Refresh confirmed the branch has not moved",
			"branch", w.Branch, "revision", revision)
		return nil
	}

	// It moved (or we cannot claim to know where the checkout is). Fetch and reset onto it.
	w.Log.Info("Refresh found the branch elsewhere; resetting onto it",
		"branch", w.Branch, "revision", revision)
	if err := w.syncWithRemote(ctx, fetchReasonRefresh); err != nil {
		return err
	}
	if synced, ok := w.LastRemoteObservation(); ok {
		w.reportRemoteObservation([]itypes.ResourceReference{req.Target}, synced)
	}
	return nil
}

// rescanLayoutForTarget re-reads the target's folder, publishes what its shape resolves to, and
// publishes whether the folder can be written at all.
//
// The acceptance half is the point. A folder is broken by somebody ELSE's push, which produces no
// Kubernetes event, so without a read nothing notices until the next live edit arrives — and that
// edit is refused and its events are dropped (recordCommitFailure: "lost until the next resync").
// Discovering it by reading costs one local scan on a tick that already happened.
//
// Raising GitPathAccepted=False does more than report: the reconcile reads it back into
// forceRecheck, which re-anchors the target's streams and drives a resync, and a refused target
// then requeues on the fast interval. That is the intended recovery and not a side effect — see
// gitTargetRequeue, which gives a stalled target the fast loop precisely because "someone fixes
// the folder in Git" emits no event to wake it. It cannot loop the branch with commits either:
// spec.onRefusal reverts a refused EDIT, and refusalIsAWriteBoundary admits only the three
// edit-level issue kinds, so a folder refusal — which is the only kind a scan can raise — never
// produces a commit at all.
//
// What this must still never do is write CONTENT. It builds no plan — an empty desired set is the
// sweep-everything signal — and it publishes no layout for a folder it just refused, which is the
// same order both write paths use.
//
// Recovery is symmetric and deliberately narrow: a scan that passes clears only a refusal a SCAN
// raised. A write-boundary refusal is invisible to a structural read, so clearing one here would
// report a target as writable that is not. See Manager.MarkTargetGitPathScanAccepted.
func (w *BranchWorker) rescanLayoutForTarget(ctx context.Context, req *RefreshRequest) {
	// Nothing to read for, either because the target has no folder of its own or because neither
	// projection is wired (the CLI, and tests). The scan is local work, but it is not free, and a
	// verdict nobody receives is not worth taking the repository lock for.
	if req.Path == "" || (w.layoutReporter == nil && w.scanAcceptance == nil) {
		return
	}
	w.repoMu.Lock()
	defer w.repoMu.Unlock()

	provider, err := w.getGitProvider(ctx)
	if err != nil {
		w.Log.V(1).Info("Refresh could not re-read the folder", "error", err.Error())
		return
	}
	repo, err := gogit.PlainOpen(w.repoPathForRemote(provider.Spec.URL))
	if err != nil {
		w.Log.V(1).Info("Refresh could not re-read the folder", "error", err.Error())
		return
	}
	worktree, err := repo.Worktree()
	if err != nil {
		w.Log.V(1).Info("Refresh could not re-read the folder", "error", err.Error())
		return
	}
	root := worktree.Filesystem().Root()
	scoped, err := scanRenderScope(root, req.Path)
	if err != nil {
		w.Log.V(1).Info("Refresh could not re-read the folder", "error", err.Error())
		return
	}
	// The local mapper, deliberately, even for a target mirroring a remote cluster: what is
	// resolved here is the folder's SHAPE, which is decided by the kustomizations in it and not
	// by any cluster's type catalog.
	batch := newWriteBatch(
		ctx, w.contentWriter, w.mapper, scoped.scan, nil, namespacePolicy{}, scoped.writeSubdir)
	batch.target = placementTarget{name: req.Target.Name, namespace: req.Target.Namespace}
	if err := batch.refusal(); err != nil {
		var refused *manifestanalyzer.AcceptanceRefusedError
		if !errors.As(err, &refused) {
			// The gate returns nothing else today. If it ever does, a refusal nobody can classify
			// must not be published as one, and must not pass for acceptance either.
			w.Log.Error(err, "Refresh could not classify what the folder scan returned",
				"branch", w.Branch, "gitTarget", req.Target.String())
			return
		}
		w.Log.Info("Refresh found content in the folder that cannot be written",
			"branch", w.Branch, "gitTarget", req.Target.String(), "detail", refused.Error())
		w.reportScanAcceptance(req.Target, refused)
		return
	}
	w.reportScanAcceptance(req.Target, nil)
	w.reportLayout(ctx, batch, worktreeRevision(worktree))
}

// advertiseRemoteBranch asks the remote where a branch is, and transfers nothing else.
//
// It is built on a storage-less remote, the way CheckRepo is, so it needs no clone on disk. Zero
// means the advertisement did not carry the branch, which includes an empty repository — an
// observation rather than an error: a branch does not exist without a commit.
func advertiseRemoteBranch(
	remoteURL string,
	branch plumbing.ReferenceName,
	auth []gitclient.Option,
) (plumbing.Hash, error) {
	remote := gogit.NewRemote(nil, &config.RemoteConfig{Name: "origin", URLs: []string{remoteURL}})
	refs, err := listRemoteRefs(remote, auth)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	for _, ref := range refs {
		if ref.Type() == plumbing.HashReference && ref.Name() == branch {
			return ref.Hash(), nil
		}
	}
	return plumbing.ZeroHash, nil
}

// localHead is the commit the worktree is on, or the zero hash when the branch is unborn.
func localHead(repo *gogit.Repository) plumbing.Hash {
	head, err := repo.Head()
	if err != nil {
		return plumbing.ZeroHash
	}
	return head.Hash()
}
