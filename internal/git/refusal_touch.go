// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

// refusalKey identifies ONE unresolved refusal: the GitTarget that owns it, and the watched cell
// it is about.
//
// The cell is in the key because a review found what a target-wide key gets wrong in both
// directions. A target watches several types; a successful ConfigMap resync says nothing about a
// Deployment that is still refused, so clearing the whole target on it re-armed the standing
// refusal and the commit loop came back. And a queued commit for one cell must survive another
// cell recovering. This is the same granularity the GitPathAccepted condition already uses — the
// watch layer blocks and clears per cell — so the memory and the status now agree about what is
// still refused.
//
// The zero cell is the whole-GitTarget evaluation, which speaks for everything the target holds.
type refusalKey struct {
	target itypes.ResourceReference
	cell   itypes.CellKey
}

func (k refusalKey) String() string {
	return k.target.String() + " " + sourceCellForLog(k.cell)
}

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
	targetName, targetNamespace, detail string,
	refused *manifestanalyzer.AcceptanceRefusedError,
	observation string,
	cell itypes.CellKey,
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
	key := refusalKey{target: target, cell: cell}

	// An observation this cell's last commit already covered is not a new trigger. Checked here,
	// after consent and before the rate limit, so a recheck of unchanged input neither commits nor
	// arms the trailing timer: arming it would turn the rate limit into a schedule, which is the
	// loop this fence exists to close.
	if l.w.refusalAlreadyCovered(key, observation) {
		l.w.Log.V(1).Info("Refusal unchanged since the last empty commit; not committing again",
			"gitTarget", target.String(), "sourceCell", sourceCellForLog(cell), "branch", l.w.Branch)
		return
	}

	// Inside the rate-limit window, COALESCE rather than drop. Dropping is a correctness bug, not
	// a missed optimisation: the reconcile the previous commit triggered may already have
	// completed, so nothing would ever cover this refusal and the live edit would stay for good.
	// One trailing commit covers every refusal that arrived during the window.
	if limited, wait := l.w.refusalRateLimited(target); limited {
		l.armTrailingRefusalTouch(key, detail, observation, wait)
		return
	}

	l.commitRefusalTouch(key, detail, observation)
}

// refusalObservation digests what was refused: the objects the write would have produced, and the
// issues raised about them. Two rechecks of one standing refusal produce the same digest, and any
// genuinely different edit — a new value, a different object, a second object joining the batch —
// produces a different one.
//
// It digests CONTENT rather than a resourceVersion on purpose. A forced recheck re-lists from the
// API server and every snapshot it takes carries a fresh collection version, so versions would
// make every recheck look new, which is precisely the loop being closed. Sanitized content is also
// exactly what the refused write was going to be, so it is the thing the previous commit's
// reconcile either covered or did not.
//
// The digest is order-independent: the per-object digests are sorted before they are folded in, so
// the same set observed in a different order is the same observation.
func refusalObservation(
	objects []*unstructured.Unstructured, refused *manifestanalyzer.AcceptanceRefusedError,
) string {
	parts := make([]string, 0, len(objects))
	for _, object := range objects {
		if object == nil {
			continue
		}
		encoded, err := json.Marshal(object.Object)
		if err != nil {
			// An object that cannot be encoded cannot be identified, so it is treated as a
			// distinct observation every time: the fence errs towards committing, which costs an
			// empty commit, rather than towards silence, which costs a live edit its revert.
			return ""
		}
		parts = append(parts, fmt.Sprintf("%x", sha256.Sum256(encoded)))
	}
	if refused != nil {
		for _, issue := range refused.Issues {
			parts = append(parts, fmt.Sprintf("issue:%s:%s:%d",
				issue.Kind, issue.Path, issue.DocumentIndex))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(parts, "\n"))))
}

// refusalObservationForEvents digests the objects a live write batch carried. A DELETE carries no
// object; it contributes nothing, which is harmless because a removal never reaches the commit
// anyway (see refusalIsAWriteBoundary).
func refusalObservationForEvents(
	events []Event, refused *manifestanalyzer.AcceptanceRefusedError,
) string {
	objects := make([]*unstructured.Unstructured, 0, len(events))
	for _, event := range events {
		objects = append(objects, event.Object)
	}
	return refusalObservation(objects, refused)
}

// refusalObservationForDesired digests a resync's desired snapshot, which is the same population
// the live paths digest — the objects the refused write plan was built from.
func refusalObservationForDesired(
	desired []manifestanalyzer.DesiredResource, refused *manifestanalyzer.AcceptanceRefusedError,
) string {
	objects := make([]*unstructured.Unstructured, 0, len(desired))
	for _, resource := range desired {
		objects = append(objects, resource.Object)
	}
	return refusalObservation(objects, refused)
}

// refusalAlreadyCovered reports whether an empty commit has already been made for exactly this
// observation, with nothing accepted for the target since.
//
// An empty observation (nothing to digest, or an object that would not encode) is never covered:
// the fence must not silence a refusal it cannot identify.
func (w *BranchWorker) refusalAlreadyCovered(key refusalKey, observation string) bool {
	if observation == "" {
		return false
	}
	w.refusalTouchMu.Lock()
	defer w.refusalTouchMu.Unlock()
	return w.coveredRefusal[key] == observation
}

// recordRefusalObservation remembers what the commit just made covers.
func (w *BranchWorker) recordRefusalObservation(key refusalKey, observation string) {
	if observation == "" {
		return
	}
	w.refusalTouchMu.Lock()
	defer w.refusalTouchMu.Unlock()
	if w.coveredRefusal == nil {
		w.coveredRefusal = map[refusalKey]string{}
	}
	w.coveredRefusal[key] = observation
}

// refusalRecovered records that an evaluation for this target and cell was ACCEPTED, so whatever
// refusal was outstanding there is over: the memory of what the last commit covered is dropped,
// and any commit still queued for it is cancelled.
//
// **Only a resync calls this, and only for its own scope.** That is the rule the GitPathAccepted
// condition already follows, for the same reason: a live write that happens to avoid the offending
// file proves nothing about the rest of the subtree, and one watched type succeeding proves
// nothing about another. Clearing more widely than the evidence supports is what let a no-op
// ConfigMap resync re-arm a standing Deployment refusal, and the commit loop with it.
//
// Dropping the memory is what keeps the fence from becoming a permanent mute: a refused live edit
// earns a commit, the reconciler reverts it, and the same edit is made again — sanitized content
// cannot tell the second from the first, so only the acceptance in between can. Cancelling the
// queued commit is the other half: an obligation whose object has since been accepted has nothing
// left to ask for, and firing it anyway would move the branch for a refusal that no longer exists.
func (l *branchWorkerEventLoop) refusalRecovered(target itypes.ResourceReference, cell itypes.CellKey) {
	if target.Name == "" || target.Namespace == "" {
		return
	}
	l.w.clearRefusalObservations(target, cell)
	for _, key := range sortedRefusalKeys(l.refusalPending) {
		if key.target == target && (cell == (itypes.CellKey{}) || key.cell == cell) {
			delete(l.refusalPending, key)
		}
	}
	l.rearmRefusalTimer()
}

// clearRefusalObservations forgets what this cell's last empty commit covered. The ZERO cell is a
// whole-GitTarget evaluation, which does speak for every cell, so it clears all of them.
func (w *BranchWorker) clearRefusalObservations(target itypes.ResourceReference, cell itypes.CellKey) {
	w.refusalTouchMu.Lock()
	defer w.refusalTouchMu.Unlock()
	if cell != (itypes.CellKey{}) {
		delete(w.coveredRefusal, refusalKey{target: target, cell: cell})
		return
	}
	for key := range w.coveredRefusal {
		if key.target == target {
			delete(w.coveredRefusal, key)
		}
	}
}

// pendingRefusalTouch is one target's coalesced commit: what to say, and when it may run.
type pendingRefusalTouch struct {
	key    refusalKey
	detail string
	// observation is what this entry's commit will cover. It is carried rather than re-derived
	// because the refusal that queued it is long gone by the time the timer fires.
	observation string
	dueAt       time.Time
}

// armTrailingRefusalTouch records one target's coalesced commit and arms the shared timer for the
// earliest deadline among all of them.
//
// A second refusal for the SAME target inside its window refreshes the detail and keeps that
// target's deadline rather than pushing it out, so a steady stream still produces a commit every
// interval instead of starving while refusals keep arriving. A refusal for a DIFFERENT target
// never displaces one already recorded, which is the bug this map exists for.
func (l *branchWorkerEventLoop) armTrailingRefusalTouch(
	key refusalKey, detail, observation string, wait time.Duration,
) {
	if l.refusalPending == nil {
		l.refusalPending = map[refusalKey]pendingRefusalTouch{}
	}
	entry, existing := l.refusalPending[key]
	entry.key, entry.detail, entry.observation = key, detail, observation
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

		if !l.w.refusalConsent(l.w.ctx, entry.key.target) {
			continue
		}
		// Re-checked here as well as on arrival: another commit for this cell may have been made
		// while this entry waited, and if it covered the same observation this one has nothing
		// left to ask for. An entry whose cell RECOVERED in the meantime is not here at all —
		// refusalRecovered removed it.
		if l.w.refusalAlreadyCovered(entry.key, entry.observation) {
			continue
		}
		if limited, wait := l.w.refusalRateLimited(entry.key.target); limited {
			// The window moved under us (another refusal for this target committed while this one
			// waited). Re-arm rather than commit early.
			l.armTrailingRefusalTouch(entry.key, entry.detail, entry.observation, wait)
			continue
		}
		l.commitRefusalTouch(entry.key, entry.detail, entry.observation)
	}
	l.rearmRefusalTimer()
}

// sortedRefusalKeys makes the flush order deterministic, so a test that queues two targets sees
// the same one committed first every run.
func sortedRefusalKeys(pending map[refusalKey]pendingRefusalTouch) []refusalKey {
	keys := make([]refusalKey, 0, len(pending))
	for key := range pending {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	return keys
}

func (l *branchWorkerEventLoop) commitRefusalTouch(key refusalKey, detail, observation string) {
	target := key.target
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
	// Recorded once the commit exists, beside the rate-limit stamp and for the same reason: a
	// build or commit failure has produced no trigger, so it must leave the next observation of
	// the same refusal free to make one.
	l.w.recordRefusalObservation(key, observation)
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
