// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// refusalTouchInterval is the shortest gap between two empty commits for one GitTarget.
//
// A refusal is not a one-off. The same live edit can be re-attempted, and a controller writing a
// base-owned field can produce one on every reconcile of its own, so without a floor the branch
// would fill with empty commits and every one of them would wake every reconciler watching it.
// The debounce is per target rather than per object because the commit is per branch: two refusals
// a second apart are served by one commit, and the reconcile it triggers covers both.
const refusalTouchInterval = time.Minute

// executeRefusalTouch creates the empty commit. The tree is untouched on purpose, so
// AllowEmptyCommits is what makes it a commit at all.
//
// Nothing here decides WHETHER to do it. That is settled before the write is built, because by the
// time a write reaches the executor it has already been retained, and a write that declined to
// commit here would sit in the retained set advancing the push cooldown for nothing.
func (w *BranchWorker) executeRefusalTouch(
	ctx context.Context,
	worktree *gogit.Worktree,
	pendingWrite PendingWrite,
) (int, plumbing.Hash, error) {
	message, options, _, err := pendingWrite.commitMetadata()
	if err != nil {
		return 0, plumbing.ZeroHash, err
	}
	options.AllowEmptyCommits = true

	hash, err := worktree.Commit(message, options)
	if err != nil {
		return 0, plumbing.ZeroHash, fmt.Errorf("failed to create refusal commit: %w", err)
	}
	log.FromContext(ctx).Info("Empty commit created to trigger a reconcile after a refused write",
		"gitTarget", pendingWrite.GitTargetNamespace+"/"+pendingWrite.GitTargetName,
		"branch", w.Branch, "sha", hash.String())
	return 1, hash, nil
}

// refusalConsent reports whether this GitTarget asked for an empty commit on refusal at all,
// ignoring rate limiting. It is re-read every time, including when a coalesced commit finally
// fires, so a target that was suspended or set back to Ignore in the meantime is honoured.
//
// The GitTarget is read live rather than from the write's resolved metadata because a refusal has
// no write to carry metadata: the plan was aborted before anything was built. A read failure
// returns false, which is the safe direction — an unreadable target is not evidence of consent.
func (w *BranchWorker) refusalConsent(ctx context.Context, target itypes.ResourceReference) bool {
	gitTarget, err := w.getGitTarget(ctx, target.Name, target.Namespace)
	if err != nil {
		w.Log.V(1).Info("Cannot read GitTarget to decide the refusal action; not committing",
			"gitTarget", target.String(), "error", err.Error())
		return false
	}
	if gitTarget.EffectiveOnRefusal() != v1alpha3.RefusalActionPushEmptyCommit {
		return false
	}
	if gitTarget.Spec.Suspend {
		// A suspended target writes nothing, and an empty commit is a write. Driving somebody
		// else's reconciler from a target the operator has deliberately parked would be the one
		// write suspension does not stop.
		return false
	}
	return true
}

// refusalRateLimited reports whether a commit for this target is inside the rate-limit window, and
// stamps the clock when it is not.
//
// The caller must NOT simply drop a rate-limited refusal. Dropping loses it: the reconcile the
// earlier commit triggered may already have finished before this refusal happened, so nothing
// covers it and the live edit stays forever. touchBranchForRefusal coalesces it into a trailing
// commit instead.
func (w *BranchWorker) refusalRateLimited(target itypes.ResourceReference) (bool, time.Duration) {
	w.refusalTouchMu.Lock()
	defer w.refusalTouchMu.Unlock()
	if w.lastRefusalTouch == nil {
		w.lastRefusalTouch = map[string]time.Time{}
	}
	key := target.String()
	if last, seen := w.lastRefusalTouch[key]; seen {
		if elapsed := time.Since(last); elapsed < refusalTouchInterval {
			return true, refusalTouchInterval - elapsed
		}
	}
	w.lastRefusalTouch[key] = time.Now()
	return false, 0
}

// buildRefusalTouchWrite assembles the empty commit's write. It borrows the target's resolved
// commit configuration so the commit is phrased and signed like every other commit this target
// makes; only its tree is empty.
func (w *BranchWorker) buildRefusalTouchWrite(
	ctx context.Context,
	target itypes.ResourceReference,
	detail string,
) (*PendingWrite, error) {
	metadata, err := w.resolveTargetMetadata(ctx, target.Name, target.Namespace)
	if err != nil {
		return nil, err
	}
	provider, err := w.getGitProvider(ctx)
	if err != nil {
		return nil, err
	}
	signer, err := getCommitSigner(ctx, w.Client, provider)
	if err != nil {
		return nil, err
	}
	return &PendingWrite{
		Kind:          PendingWriteRefusalTouch,
		CommitMessage: refusalCommitMessage(target, detail),
		CommitConfig: ResolveCommitConfig(provider.Spec.Commit).
			WithTargetMessage(metadata.CommitMessage),
		Signer:             signer,
		GitTargetName:      target.Name,
		GitTargetNamespace: target.Namespace,
		Targets: map[pendingTargetKey]ResolvedTargetMetadata{
			{Name: target.Name, Namespace: target.Namespace}: metadata,
		},
	}, nil
}

// refusalCommitMessage is the whole user-visible product of this feature when nobody is watching
// conditions, so it says what happened and why the commit is empty rather than looking like a
// stray no-op somebody force-pushed.
func refusalCommitMessage(target itypes.ResourceReference, detail string) string {
	return fmt.Sprintf(
		"chore: reconcile after a refused write to %s\n\n"+
			"This commit changes no file. It exists to move the branch so the GitOps reconciler "+
			"re-applies the desired state, reverting a live edit that has no destination in Git.\n\n"+
			"Refused: %s\n",
		target.String(), detail)
}

// touchBranchForRefusal is the loop-side half: it turns a reported refusal into an empty commit
// and schedules the push.
//
// It runs on the event loop, after the refusal has already been reported, and it is deliberately
// best-effort. A refusal is already surfaced as GitPathAccepted=False; failing to add a commit on
// top of that is a missed acceleration, not a second fault, so every failure here is logged and
// swallowed rather than escalated into the caller's error path.
func (l *branchWorkerEventLoop) touchBranchForRefusal(
	targetName, targetNamespace, detail string, events []Event,
) {
	if targetName == "" || targetNamespace == "" {
		return
	}
	target := itypes.NewResourceReference(targetName, targetNamespace)
	if !l.w.refusalConsent(l.w.ctx, target) {
		return
	}
	// Checked BEFORE the rate limit, so an object we would never commit for does not consume the
	// window and starve one we would.
	if !l.w.refusedObjectsAreManagedInGit(events) {
		return
	}

	// Inside the rate-limit window, COALESCE rather than drop. Dropping is a correctness bug, not
	// a missed optimisation: the reconcile the previous commit triggered may already have
	// completed, so nothing would ever cover this refusal and the live edit would stay for good.
	// One trailing commit covers every refusal that arrived during the window.
	if limited, wait := l.w.refusalRateLimited(target); limited {
		l.armTrailingRefusalTouch(target, detail, wait)
		return
	}

	l.commitRefusalTouch(target, detail)
}

// armTrailingRefusalTouch schedules the coalesced commit. A second refusal inside the same window
// refreshes the detail and keeps the existing deadline rather than pushing it out, so a steady
// stream of refusals still produces a commit every interval instead of starving while they keep
// arriving.
func (l *branchWorkerEventLoop) armTrailingRefusalTouch(
	target itypes.ResourceReference, detail string, wait time.Duration,
) {
	l.refusalPending = target
	l.refusalPendingDetail = detail
	if l.refusalTimer == nil {
		l.refusalTimer = time.NewTimer(wait)
	}
}

// flushPendingRefusalTouch runs the coalesced commit when its timer fires.
//
// Consent is re-checked here rather than trusted from when the refusal arrived, because a minute
// is long enough for the target to have been suspended or set back to Ignore, and acting on stale
// consent is exactly the kind of thing a delayed action gets wrong.
func (l *branchWorkerEventLoop) flushPendingRefusalTouch() {
	target, detail := l.refusalPending, l.refusalPendingDetail
	l.refusalPending, l.refusalPendingDetail = itypes.ResourceReference{}, ""
	if target.Name == "" || target.Namespace == "" {
		return
	}
	if !l.w.refusalConsent(l.w.ctx, target) {
		return
	}
	if limited, wait := l.w.refusalRateLimited(target); limited {
		// The window moved under us (another refusal committed while this one waited). Re-arm
		// rather than commit early.
		l.armTrailingRefusalTouch(target, detail, wait)
		return
	}
	l.commitRefusalTouch(target, detail)
}

func (l *branchWorkerEventLoop) commitRefusalTouch(target itypes.ResourceReference, detail string) {
	pendingWrite, err := l.w.buildRefusalTouchWrite(l.w.ctx, target, detail)
	if err != nil {
		l.w.Log.Error(err, "Cannot build the empty commit for a refused write",
			"gitTarget", target.String())
		return
	}

	// Single-element batch for the same reason every other commit site uses one: the executor
	// stamps CommitSHA back into the slice it is given, and the retained write is what the push
	// counts from.
	batch := []PendingWrite{*pendingWrite}
	if err := l.w.commitPendingWrites(batch, len(l.pendingWrites) > 0); err != nil {
		l.w.Log.Error(err, "The empty commit for a refused write failed",
			"gitTarget", target.String())
		return
	}
	l.pendingWrites = append(l.pendingWrites, batch...)
	l.pendingWritesBytes += batch[0].ByteSize
	l.maybeSchedulePush()
}

// refusedObjectsAreManagedInGit reports whether every refused object already has a file in the
// GitTarget's folder.
//
// **This is the pruning fence, and it is the reason the action is narrower than it first looked.**
// An empty commit makes the reconciler re-apply, and what re-applying does depends entirely on
// whether the object is one Git manages:
//
//   - Already in Git: re-applying rewrites it, which reverts the refused live edit. This is the
//     case the whole feature is for.
//   - Never managed: Flux prunes from its inventory and Argo CD from the resources it tracks, so a
//     live-created object belongs to neither. Re-applying does nothing to it, and the commit was
//     pointless noise on the branch.
//   - Previously managed and since removed from Git: re-applying PRUNES it. The reconciler was
//     going to do that on its own schedule, and making it happen sooner is us taking responsibility
//     for the timing of somebody else's delete. That is not ours to take.
//
// The second and third cases are indistinguishable from here, and only one of them is harmless, so
// both are excluded. The check errs toward NOT committing: the file path it tests is the canonical
// one, so an object placed somewhere else by a placement template reads as absent and is skipped.
// A missed commit costs the freshness this feature adds; a wrong one costs somebody an object.
func (w *BranchWorker) refusedObjectsAreManagedInGit(events []Event) bool {
	if len(events) == 0 {
		return false
	}
	provider, err := w.getGitProvider(w.ctx)
	if err != nil {
		w.Log.V(1).Info("Cannot resolve the repository to check Git-managed status; not committing",
			"error", err.Error())
		return false
	}
	repoPath := w.repoPathForRemote(provider.Spec.URL)

	for _, event := range events {
		relative := windowPathKey(event, w.contentWriter)
		if relative == "" {
			return false
		}
		if _, statErr := os.Stat(filepath.Join(repoPath, relative)); statErr != nil {
			w.Log.V(1).Info("A refused object is not managed in Git; not committing",
				"path", relative, "reason", "re-applying would prune it or do nothing")
			return false
		}
	}
	return true
}
