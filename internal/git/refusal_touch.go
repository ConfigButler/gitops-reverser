// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"

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
	targetName, targetNamespace, detail string, refused *manifestanalyzer.AcceptanceRefusedError,
) {
	if targetName == "" || targetNamespace == "" {
		return
	}
	// Checked BEFORE consent and the rate limit, so a refusal we would never commit for does not
	// consume the window and starve one we would.
	if !refusalIsAWriteBoundary(refused) {
		return
	}
	target := itypes.NewResourceReference(targetName, targetNamespace)
	if !l.w.refusalConsent(l.w.ctx, target) {
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

// pendingRefusalTouch is one target's coalesced commit: what to say, and when it may run.
type pendingRefusalTouch struct {
	target itypes.ResourceReference
	detail string
	dueAt  time.Time
}

// armTrailingRefusalTouch records one target's coalesced commit and arms the shared timer for the
// earliest deadline among all of them.
//
// A second refusal for the SAME target inside its window refreshes the detail and keeps that
// target's deadline rather than pushing it out, so a steady stream still produces a commit every
// interval instead of starving while refusals keep arriving. A refusal for a DIFFERENT target
// never displaces one already recorded, which is the bug this map exists for.
func (l *branchWorkerEventLoop) armTrailingRefusalTouch(
	target itypes.ResourceReference, detail string, wait time.Duration,
) {
	if l.refusalPending == nil {
		l.refusalPending = map[string]pendingRefusalTouch{}
	}
	key := target.String()
	entry, existing := l.refusalPending[key]
	entry.target, entry.detail = target, detail
	if !existing {
		entry.dueAt = time.Now().Add(wait)
	}
	l.refusalPending[key] = entry
	l.rearmRefusalTimer()
}

// rearmRefusalTimer points the one timer at the earliest deadline still outstanding.
func (l *branchWorkerEventLoop) rearmRefusalTimer() {
	l.stopRefusalTimer()
	var earliest time.Time
	for _, entry := range l.refusalPending {
		if earliest.IsZero() || entry.dueAt.Before(earliest) {
			earliest = entry.dueAt
		}
	}
	if earliest.IsZero() {
		return
	}
	l.refusalTimer = time.NewTimer(max(time.Until(earliest), 0))
}

// flushPendingRefusalTouch runs every coalesced commit that has come due.
//
// Consent is re-checked per entry rather than trusted from when the refusal arrived, because a
// minute is long enough for a target to have been suspended or set back to Ignore, and acting on
// stale consent is exactly what a delayed action gets wrong. One target withdrawing consent drops
// only its own entry: every other target's stays, which is the whole point of keeping them apart.
func (l *branchWorkerEventLoop) flushPendingRefusalTouch() {
	now := time.Now()
	for _, key := range sortedRefusalKeys(l.refusalPending) {
		entry := l.refusalPending[key]
		if entry.dueAt.After(now) {
			continue
		}
		delete(l.refusalPending, key)

		if !l.w.refusalConsent(l.w.ctx, entry.target) {
			continue
		}
		if limited, wait := l.w.refusalRateLimited(entry.target); limited {
			// The window moved under us (another refusal for this target committed while this one
			// waited). Re-arm rather than commit early.
			l.armTrailingRefusalTouch(entry.target, entry.detail, wait)
			continue
		}
		l.commitRefusalTouch(entry.target, entry.detail)
	}
	l.rearmRefusalTimer()
}

// sortedRefusalKeys makes the flush order deterministic, so a test that queues two targets sees
// the same one committed first every run.
func sortedRefusalKeys(pending map[string]pendingRefusalTouch) []string {
	keys := make([]string, 0, len(pending))
	for key := range pending {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
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

// refusalIsAWriteBoundary reports whether this refusal is one where the FOLDER is accepted and
// only the edit had nowhere to land.
//
// **This is the pruning fence, and it replaces an earlier one that got it wrong.** The first
// version stat-ed the object's canonical file under the GitTarget's write path, on the reasoning
// that an object Git does not manage must not be committed for. It disabled the feature for its
// main case: a kustomize overlay's base-owned field lives in `../../base`, outside the write
// scope, so the canonical case read as "not managed" and was skipped.
//
// The refusal kind answers the same question better, because the analyzer has already decided it.
// A write-boundary refusal means, in the analyzer's own words, that the edit had nowhere safe to
// land while **the folder itself is accepted**. That carries two things this needs:
//
//   - The folder renders, so the object is one the reconciler produces. Re-applying rewrites it,
//     which reverts the live edit. That is the case the feature exists for.
//   - Nothing about the object is being removed from Git, so re-applying cannot prune it. Hurrying
//     somebody else's delete is what we are avoiding, and a write-boundary refusal cannot cause
//     one.
//
// Every other refusal means the folder is unusable. An empty commit cannot fix a folder, only a
// human can, and the reconciler may be mid-way through its own corrections there, so those are
// left alone. They also reach a different reporter entirely: a folder-level refusal is normally
// found by the per-type reconcile, which blocks the cell through the event router rather than
// coming through here at all.
func refusalIsAWriteBoundary(refused *manifestanalyzer.AcceptanceRefusedError) bool {
	if refused == nil || !refused.AllIssuesOfKinds(
		manifestanalyzer.IssueWriteEscapesScope,
		manifestanalyzer.IssueWriteFanIn,
		manifestanalyzer.IssueUnplaceableEdit,
	) {
		return false
	}
	// The kind is not enough on its own, and a review found the hole: IssueRenderRefused fires for
	// a BRAND-NEW resource whose image an existing images: entry would override, and a write that
	// is removing a document had one before it. Both would have been committed for. So every issue
	// must additionally carry evidence, set only where the pre-write buffer was in hand, that the
	// folder already holds this document and this write is not removing it.
	//
	// IssueRenderRefused is gone from the list entirely rather than relying on that evidence: it
	// is raised for the batch and names no path, so there is nowhere for the evidence to come from.
	for _, issue := range refused.Issues {
		if !issue.ExistingDocument {
			return false
		}
	}
	return true
}
