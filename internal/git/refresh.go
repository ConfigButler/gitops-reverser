// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"errors"
	"fmt"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	gitclient "github.com/go-git/go-git/v6/plumbing/client"

	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// RefreshRequest asks a worker to re-prove where its branch is on the remote, if nothing has
// proved it recently enough.
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
	// MaxAge is how old an observation may be before it is topped up. An observation younger
	// than this makes the refresh a no-op with no connection at all, which is what keeps an
	// actively-publishing branch free: its pushes renew the record continuously.
	MaxAge time.Duration
}

// EnqueueRefresh queues a refresh, dropping it if the queue is full.
//
// The drop is deliberate and safe in a way it is not for a write: the next reconcile tick asks
// again, and a refresh must never occupy the slot a write needs.
func (w *BranchWorker) EnqueueRefresh(req *RefreshRequest) {
	if req == nil {
		return
	}
	w.inflightItems.Add(1)
	select {
	case w.eventQueue <- WorkItem{Refresh: req}:
	default:
		w.inflightItems.Add(-1)
		w.Log.V(1).Info("Event queue full, refresh dropped; the next reconcile asks again",
			"branch", w.Branch, "gitTarget", req.Target.String())
	}
}

// handleRefreshRequest is the refresher, on the loop goroutine.
//
// It is allowed to read Git freely and must never cause a publication. Reading is what it is for;
// publishing from what it reads would turn "somebody changed the folder" into a write nobody
// asked for.
func (l *branchWorkerEventLoop) handleRefreshRequest(req *RefreshRequest) {
	w := l.w

	// 1. Report what is already known, BEFORE deciding whether to do any work, and on every exit
	// below.
	//
	// This is the only way a quiet target on a shared branch ever hears anything. A worker serves
	// every GitTarget on its (provider, branch), and a push reports only against the targets whose
	// writes it carried — so a target that is not writing learns where its branch is exclusively
	// from its own refresh tick. Skipping the report on the way to skipping the work left it
	// stale for as long as its busy sibling kept the worker occupied.
	observed, known := w.LastRemoteObservation()
	if known {
		w.reportRemoteObservation([]itypes.ResourceReference{req.Target}, observed)
	}

	// 2. Not idle, so not the target this exists for. A reset here would destroy retained
	// commits, and the worktree may hold a partial write, which is also why this is the one exit
	// that does not re-read the folder: a layout resolved from a half-written tree is worse than
	// a slightly old one. A target that is mid-cycle is writing, and a write publishes its own
	// layout as it goes.
	if len(l.pendingWrites) > 0 || l.openWindow != nil || w.worktreeDirty() || w.replayRequired() {
		w.Log.V(1).Info("Skipping refresh: this branch is mid-cycle",
			"branch", w.Branch, "gitTarget", req.Target.String())
		return
	}

	// 3. Something has proved this branch recently enough, so there is nothing to ask the remote.
	// This is where "a push is a refresh" is spent: an actively-writing branch renews its
	// observation on every push and never reaches the connection below.
	//
	// The folder is still re-read. That is local work, and it is what keeps a SIBLING honest: the
	// fetch that moved this checkout may have been earned by another target's refresh, which
	// rescanned its own folder and knew nothing about this one.
	if known && req.MaxAge > 0 && observed.Age(time.Now()) < req.MaxAge {
		w.rescanLayoutForTarget(w.ctx, req)
		return
	}

	if err := l.refreshFromRemote(req); err != nil {
		// A refresh that failed proves nothing, so nothing is recorded: the published
		// lastVerifiedAt stops advancing, which is precisely how a refresher that stopped
		// working reports itself.
		w.Log.Error(err, "Refresh failed to prove where the branch is",
			"branch", w.Branch, "gitTarget", req.Target.String())
		return
	}

	// 4. One re-read per tick, on every path that reached the remote, whether or not this tick
	// was the one that fetched.
	w.rescanLayoutForTarget(w.ctx, req)
}

// refreshFromRemote reads the remote's advertisement and, only if the branch has moved away from
// what the worktree holds, fetches and resets onto it.
//
// The split is the point. SmartFetch is two connections — it lists the refs and then fetches
// unconditionally — and for the write path that is right, because it is about to reset either
// way. On an idle target the overwhelmingly common answer is "the branch has not moved", and the
// advertisement alone settles it, so the common case here costs ONE connection. The scarce
// resource is the round trip; local recomputation is free and never an excuse for a fetch.
func (l *branchWorkerEventLoop) refreshFromRemote(req *RefreshRequest) error {
	w := l.w
	ctx := w.ctx

	provider, err := w.getGitProvider(ctx)
	if err != nil {
		return fmt.Errorf("get GitProvider: %w", err)
	}
	auth, err := getAuthFromSecret(ctx, w.Client, provider, w.sshHostKeys, w.credentialPolicy)
	if err != nil {
		return fmt.Errorf("get auth: %w", err)
	}
	repo, err := gogit.PlainOpen(w.repoPathForRemote(provider.Spec.URL))
	if errors.Is(err, gogit.ErrRepositoryNotExists) {
		// A worker exists for this branch but has never cloned: nothing has been published yet.
		// There is no checkout to refresh and nothing to report, and the first publication reads
		// the remote anyway. It is an ordinary state, so it must not be logged as a failure once
		// per tick for the lifetime of an unused target.
		w.Log.V(1).Info("Skipping refresh: no checkout yet", "branch", w.Branch)
		return nil
	}
	if err != nil {
		return fmt.Errorf("open repository: %w", err)
	}

	// One connection: what does the remote say this branch is at?
	advertised, err := advertiseRemoteBranch(repo, plumbing.NewBranchReferenceName(w.Branch), auth)
	if err != nil {
		return fmt.Errorf("read the remote advertisement: %w", err)
	}
	revision := ""
	if !advertised.IsZero() {
		revision = advertised.String()
	}
	observed := w.recordRemoteObservation(revision, ObservedByFetch)
	w.reportRemoteObservation([]itypes.ResourceReference{req.Target}, observed)

	// The remote does not carry this branch. That IS the answer, and there is nothing to
	// fetch: a fetch would fall back to the default branch and teach us nothing about a branch
	// nobody has created. It is the ordinary state of a target that has not written yet, so
	// paying a second connection for it every interval would be a standing cost for nothing. A
	// branch somebody DELETED is handled where it has to be, by the compare-and-swap on the next
	// push.
	if advertised.IsZero() {
		w.Log.V(1).Info("Refresh found no such branch on the remote", "branch", w.Branch)
		return nil
	}

	// The branch is where our checkout already is, so there is nothing to fetch. The caller
	// still re-reads the folder: this worker's checkout may have been moved by a SIBLING
	// target's refresh since this target last looked at it.
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

// rescanLayoutForTarget re-reads the target's folder and republishes what its shape resolves to.
//
// It is structure-only, and the boundary is the point: a fresh look at Git may change what an
// operator READS and nothing else. Publishing placement is inside that line and checkably so —
// LayoutResolved writes no part of the kstatus trio, and the projection republishes only on a
// transition — while re-running the ACCEPTANCE gate would be outside it. GitPathAccepted=False is
// not only a report: the reconcile reads it back into forceRecheck, which re-anchors the target's
// streams and drives a resync, and a refused target then requeues every ten seconds. A refresher
// allowed to raise it would turn "somebody pushed a folder we cannot write" into a cluster
// snapshot every ten seconds, from a code path whose whole justification is that it publishes
// nothing. So acceptance stays where it is: raised by a write that was actually refused, cleared
// by a resync that actually succeeded.
//
// Nothing here builds a plan. An empty desired set is the sweep-everything signal, and this must
// never reach it.
func (w *BranchWorker) rescanLayoutForTarget(ctx context.Context, req *RefreshRequest) {
	if req.Path == "" || w.layoutReporter == nil {
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
	w.reportLayout(ctx, batch, worktreeRevision(worktree))
}

// advertiseRemoteBranch asks the remote where a branch is, and transfers nothing else.
//
// Zero means the advertisement did not carry the branch, which includes an empty repository. That
// is an observation rather than an error: a branch does not exist without a commit.
func advertiseRemoteBranch(
	repo *gogit.Repository,
	branch plumbing.ReferenceName,
	auth []gitclient.Option,
) (plumbing.Hash, error) {
	remote, err := repo.Remote("origin")
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("get remote origin: %w", err)
	}
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
