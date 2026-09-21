// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"fmt"
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

// refusalTouchAllowed reports whether this GitTarget asked for an empty commit on refusal, and
// whether enough time has passed since the last one.
//
// The GitTarget is read live rather than from the write's resolved metadata because a refusal has
// no write to carry metadata: the plan was aborted before anything was built. A read failure
// returns false, which is the safe direction — an unreadable target is not evidence of consent.
func (w *BranchWorker) refusalTouchAllowed(ctx context.Context, target itypes.ResourceReference) bool {
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

	w.refusalTouchMu.Lock()
	defer w.refusalTouchMu.Unlock()
	if w.lastRefusalTouch == nil {
		w.lastRefusalTouch = map[string]time.Time{}
	}
	key := target.String()
	if last, seen := w.lastRefusalTouch[key]; seen && time.Since(last) < refusalTouchInterval {
		return false
	}
	w.lastRefusalTouch[key] = time.Now()
	return true
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
func (l *branchWorkerEventLoop) touchBranchForRefusal(targetName, targetNamespace, detail string) {
	if targetName == "" || targetNamespace == "" {
		return
	}
	target := itypes.NewResourceReference(targetName, targetNamespace)
	if !l.w.refusalTouchAllowed(l.w.ctx, target) {
		return
	}

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
