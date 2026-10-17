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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// refusalReapplyInterval is the shortest gap between two Refusal apply requests for one GitTarget.
//
// A refusal is not a one-off. The same live edit can be re-attempted, and a controller writing a
// base-owned field can produce one on every reconcile of its own, so without a floor the applier
// would be asked to reapply on every one of those reconciles. The floor is per target because the
// request is: two refusals a second apart are served by one request, and the apply it causes
// covers both.
const refusalReapplyInterval = time.Minute

// refusalKey identifies ONE unresolved refusal: the GitTarget that owns it, and the watched collection
// it is about.
//
// The collection is in the key because a review found what a target-wide key gets wrong in both
// directions. A target watches several types; a successful ConfigMap resync says nothing about a
// Deployment that is still refused, so clearing the whole target on it re-armed the standing
// refusal and the request loop came back. And a queued request for one collection must survive another
// collection recovering. This is the same granularity the GitPathAccepted condition already uses — the
// watch layer blocks and clears per collection — so the memory and the status now agree about what is
// still refused.
//
// The zero collection is the whole-GitTarget evaluation, which speaks for everything the target holds.
//
// Always build one with newRefusalKey, which drops the collection's selector. A refusal is about
// the Git documents under the structural boundary, which a selector does not narrow (see
// types.CollectionKey), and the rule compiler allows one selector per boundary. Every producer
// must agree on the key: the live path files a refusal under the selector-bearing collection its
// events came from, and when the accepted snapshot that recovers it was keyed differently, its
// queued request fired after recovery and its memory muted the next identical refusal. Kept with
// the selector, a rule that changed selector left the old key behind the same way.
type refusalKey struct {
	target     itypes.ResourceReference
	collection itypes.CollectionKey
}

// newRefusalKey files a refusal for target under collection's structural boundary.
func newRefusalKey(target itypes.ResourceReference, collection itypes.CollectionKey) refusalKey {
	return refusalKey{target: target, collection: refusalBoundary(collection)}
}

// refusalBoundary is collection without its selector: the part of it a refusal is about.
func refusalBoundary(collection itypes.CollectionKey) itypes.CollectionKey {
	collection.LabelSelector = ""
	return collection
}

func (k refusalKey) String() string {
	return k.target.String() + " " + sourceCollectionForLog(k.collection)
}

// refusalConsent reports whether this GitTarget asked for its applier to reapply on refusal,
// ignoring rate limiting, and which incarnation of it asked. It is re-read every time, including
// when a coalesced request finally fires, so a target that was suspended, set back to Ignore, or
// stripped of its applier in the meantime is honoured. serviceAppliers reads consent once more
// before it sends.
//
// The GitTarget is read live rather than from the write's resolved metadata because a refusal has
// no write to carry metadata: the plan was aborted before anything was built. A read failure
// returns false, which is the safe direction — an unreadable target is not evidence of consent.
func (w *BranchWorker) refusalConsent(ctx context.Context, target itypes.ResourceReference) (k8stypes.UID, bool) {
	gitTarget, err := w.getGitTarget(ctx, target.Name, target.Namespace)
	if err != nil {
		w.Log.V(1).Info("Cannot read GitTarget to decide the refusal action; not requesting a reapply",
			"gitTarget", target.String(), "error", err.Error())
		return "", false
	}
	return gitTarget.UID, ConsentsToReapply(gitTarget)
}

// owedRefusal is one refusal its target's applier is owed a Refusal request for: what was refused,
// for the logs, and the incarnation of the GitTarget that consented. A GitTarget deleted and
// recreated under the same name is another incarnation, and inherits nothing owed to the first.
type owedRefusal struct {
	detail string
	uid    k8stypes.UID
}

// ConsentsToReapply reports whether a GitTarget, as it is now, asks for its applier to reapply on a
// refusal. The worker asks before it owes a Refusal request and again before it sends one, and the
// applier dispatcher asks before every attempt to deliver one.
func ConsentsToReapply(gitTarget *v1alpha3.GitTarget) bool {
	if gitTarget.EffectiveOnRefusal() != v1alpha3.RefusalActionReapply || gitTarget.Spec.ApplyTriggerRef == nil {
		return false
	}
	// A suspended target has stopped publishing the live edits a reapply would be weighed against.
	// Driving its applier on behalf of a target the operator has deliberately parked would be
	// acting on a mirror nobody is keeping.
	return !gitTarget.Spec.Suspend
}

// refusalRateLimited reports whether a request for this target is inside the rate-limit window,
// and stamps the clock when it is not.
//
// The caller must NOT simply drop a rate-limited refusal. Dropping loses it: the apply the earlier
// request caused may already have finished before this refusal happened, so nothing covers it and
// the live edit stays forever. reapplyForRefusal coalesces it into a trailing request instead.
func (w *BranchWorker) refusalRateLimited(target itypes.ResourceReference) (bool, time.Duration) {
	w.refusalMu.Lock()
	defer w.refusalMu.Unlock()
	if w.lastReapply == nil {
		w.lastReapply = map[string]time.Time{}
	}
	key := target.String()
	if last, seen := w.lastReapply[key]; seen {
		if elapsed := time.Since(last); elapsed < refusalReapplyInterval {
			return true, refusalReapplyInterval - elapsed
		}
	}
	w.lastReapply[key] = time.Now()
	return false, 0
}

// reapplyForRefusal is the loop-side half: it turns a reported refusal into a Refusal apply request
// owed to the target's applier, sent once the branch is quiet (serviceAppliers).
//
// It runs on the event loop, after the refusal has already been reported, and it is deliberately
// best-effort. A refusal is already surfaced as GitPathAccepted=False; failing to ask for a reapply
// on top of that is a missed recovery, not a second fault, so every failure here is logged and
// swallowed rather than escalated into the caller's error path.
func (l *branchWorkerEventLoop) reapplyForRefusal(
	targetName, targetNamespace, detail string,
	refused *manifestanalyzer.AcceptanceRefusedError,
	observation string,
	collection itypes.CollectionKey,
) {
	if targetName == "" || targetNamespace == "" {
		return
	}
	// Checked BEFORE consent and the rate limit, so a refusal we would never request for does not
	// consume the window and starve one we would.
	if !refusalIsAWriteBoundary(refused) {
		return
	}
	target := itypes.NewResourceReference(targetName, targetNamespace)
	uid, consents := l.w.refusalConsent(l.w.ctx, target)
	if !consents {
		return
	}
	key := newRefusalKey(target, collection)

	// An observation this collection's last request already covered is not a new trigger. Checked
	// here, after consent and before the rate limit, so a recheck of unchanged input neither
	// requests nor arms the trailing timer: arming it would turn the rate limit into a schedule,
	// which is the loop this fence exists to close.
	if l.w.refusalAlreadyCovered(key, observation) {
		l.w.Log.V(1).Info("Refusal unchanged since the last reapply request; not requesting again",
			"gitTarget", target.String(), "sourceCollection", sourceCollectionForLog(collection), "branch", l.w.Branch)
		return
	}

	// Inside the rate-limit window, COALESCE rather than drop. Dropping is a correctness bug, not
	// a missed optimisation: the apply the previous request caused may already have completed, so
	// nothing would ever cover this refusal and the live edit would stay for good. One trailing
	// request covers every refusal that arrived during the window.
	if limited, wait := l.w.refusalRateLimited(target); limited {
		l.armTrailingReapply(key, detail, observation, wait)
		return
	}

	l.oweReapply(key, owedRefusal{detail: detail, uid: uid}, observation)
}

// refusalObservation digests what was refused: the objects the write would have produced, and the
// issues raised about them. Two rechecks of one standing refusal produce the same digest, and any
// genuinely different edit — a new value, a different object, a second object joining the batch —
// produces a different one.
//
// It digests CONTENT rather than a resourceVersion on purpose. A forced recheck re-lists from the
// API server and every snapshot it takes carries a fresh collection version, so versions would
// make every recheck look new, which is precisely the loop being closed. Sanitized content is also
// exactly what the refused write was going to be, so it is the thing the previous request's
// apply either covered or did not.
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
			// reapply request, rather than towards silence, which costs a live edit its revert.
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
// object; it contributes nothing, which is harmless because a removal never reaches the request
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

// refusalAlreadyCovered reports whether a reapply has already been requested for exactly this
// observation, with nothing accepted for the target since.
//
// An empty observation (nothing to digest, or an object that would not encode) is never covered:
// the fence must not silence a refusal it cannot identify.
func (w *BranchWorker) refusalAlreadyCovered(key refusalKey, observation string) bool {
	if observation == "" {
		return false
	}
	w.refusalMu.Lock()
	defer w.refusalMu.Unlock()
	return w.coveredRefusal[newRefusalKey(key.target, key.collection)] == observation
}

// recordRefusalObservation remembers what the request just owed covers. An empty observation is
// recorded too, though it covers nothing (refusalAlreadyCovered): the entry is also the record that
// the refusal stands until it recovers (refusalStands).
func (w *BranchWorker) recordRefusalObservation(key refusalKey, observation string) {
	w.refusalMu.Lock()
	defer w.refusalMu.Unlock()
	if w.coveredRefusal == nil {
		w.coveredRefusal = map[refusalKey]string{}
	}
	key = newRefusalKey(key.target, key.collection)
	if _, standing := w.coveredRefusal[key]; standing && observation == "" {
		return // an observation that covers nothing must not replace one that covers something
	}
	w.coveredRefusal[key] = observation
}

// refusalRecovered records that an evaluation for this target and collection was ACCEPTED, so whatever
// refusal was outstanding there is over: the memory of what the last request covered is dropped,
// and any request still queued for it is cancelled.
//
// **Only a resync calls this, and only for its own scope.** That is the rule the GitPathAccepted
// condition already follows, for the same reason: a live write that happens to avoid the offending
// file proves nothing about the rest of the subtree, and one watched type succeeding proves
// nothing about another. Clearing more widely than the evidence supports is what let a no-op
// ConfigMap resync re-arm a standing Deployment refusal, and the request loop with it.
//
// Dropping the memory is what keeps the fence from becoming a permanent mute: a refused live edit
// earns a request, the applier reverts it, and the same edit is made again — sanitized content
// cannot tell the second from the first, so only the acceptance in between can. Cancelling the
// queued request is the other half: an obligation whose object has since been accepted has nothing
// left to ask for, and firing it anyway would move the branch for a refusal that no longer exists.
func (l *branchWorkerEventLoop) refusalRecovered(target itypes.ResourceReference, collection itypes.CollectionKey) {
	if target.Name == "" || target.Namespace == "" {
		return
	}
	collection = refusalBoundary(collection)
	l.w.clearRefusalObservations(target, collection)
	recovered := func(key refusalKey) bool {
		return key.target == target && (collection == (itypes.CollectionKey{}) || key.collection == collection)
	}
	for _, key := range sortedRefusalKeys(l.refusalPending) {
		if recovered(key) {
			delete(l.refusalPending, key)
		}
	}
	// An obligation already owed, waiting for a quiet branch, is cancelled the same way. Keyed by
	// target alone it could not be told apart from a sibling collection's, so it outlived the
	// recovery and asked for an apply for a refusal that no longer existed.
	for key := range l.reapplyOwed {
		if recovered(key) {
			delete(l.reapplyOwed, key)
		}
	}
	l.rearmRefusalTimer()
}

// refusalStands reports whether a request was owed for this refusal and it has not recovered since.
func (w *BranchWorker) refusalStands(key refusalKey) bool {
	w.refusalMu.Lock()
	defer w.refusalMu.Unlock()
	_, standing := w.coveredRefusal[newRefusalKey(key.target, key.collection)]
	return standing
}

// clearRefusalObservations forgets what this collection's last reapply request covered. The ZERO collection is a
// whole-GitTarget evaluation, which does speak for every collection, so it clears all of them.
func (w *BranchWorker) clearRefusalObservations(target itypes.ResourceReference, collection itypes.CollectionKey) {
	w.refusalMu.Lock()
	defer w.refusalMu.Unlock()
	if collection != (itypes.CollectionKey{}) {
		delete(w.coveredRefusal, newRefusalKey(target, collection))
		return
	}
	for key := range w.coveredRefusal {
		if key.target == target {
			delete(w.coveredRefusal, key)
		}
	}
}

// pendingRefusalReapply is one target's coalesced request: what to say, and when it may run.
type pendingRefusalReapply struct {
	key    refusalKey
	detail string
	// observation is what this entry's request will cover. It is carried rather than re-derived
	// because the refusal that queued it is long gone by the time the timer fires.
	observation string
	dueAt       time.Time
}

// armTrailingReapply records one target's coalesced request and arms the shared timer for the
// earliest deadline among all of them.
//
// A second refusal for the SAME target inside its window refreshes the detail and keeps that
// target's deadline rather than pushing it out, so a steady stream still produces a request every
// interval instead of starving while refusals keep arriving. A refusal for a DIFFERENT target
// never displaces one already recorded, which is the bug this map exists for.
func (l *branchWorkerEventLoop) armTrailingReapply(
	key refusalKey, detail, observation string, wait time.Duration,
) {
	if l.refusalPending == nil {
		l.refusalPending = map[refusalKey]pendingRefusalReapply{}
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

// flushPendingReapply owes every coalesced request that has come due.
//
// Consent is re-checked per entry rather than trusted from when the refusal arrived, because a
// minute is long enough for a target to have been suspended or set back to Ignore, and acting on
// stale consent is exactly what a delayed action gets wrong. One target withdrawing consent drops
// only its own entry: every other target's stays, which is the whole point of keeping them apart.
func (l *branchWorkerEventLoop) flushPendingReapply() {
	now := time.Now()
	for _, key := range sortedRefusalKeys(l.refusalPending) {
		entry := l.refusalPending[key]
		if entry.dueAt.After(now) {
			continue
		}
		delete(l.refusalPending, key)

		uid, consents := l.w.refusalConsent(l.w.ctx, entry.key.target)
		if !consents {
			continue
		}
		// Re-checked here as well as on arrival: another request for this collection may have been owed
		// while this entry waited, and if it covered the same observation this one has nothing
		// left to ask for. An entry whose collection RECOVERED in the meantime is not here at all —
		// refusalRecovered removed it.
		if l.w.refusalAlreadyCovered(entry.key, entry.observation) {
			continue
		}
		if limited, wait := l.w.refusalRateLimited(entry.key.target); limited {
			// The window moved under us (another refusal for this target was requested while this one
			// waited). Re-arm rather than request early.
			l.armTrailingReapply(entry.key, entry.detail, entry.observation, wait)
			continue
		}
		l.oweReapply(entry.key, owedRefusal{detail: entry.detail, uid: uid}, entry.observation)
	}
	l.rearmRefusalTimer()
}

// sortedRefusalKeys makes the flush order deterministic, so a test that queues two targets sees
// the same one owed first every run.
func sortedRefusalKeys(pending map[refusalKey]pendingRefusalReapply) []refusalKey {
	keys := make([]refusalKey, 0, len(pending))
	for key := range pending {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	return keys
}

// oweReapply records that the target's applier is owed a Refusal request, which serviceAppliers
// sends once the branch is quiet. Several refusals for one target before then share one request.
func (l *branchWorkerEventLoop) oweReapply(key refusalKey, refusal owedRefusal, observation string) {
	// Recorded as covered now, when the request is owed, rather than when it is delivered: the
	// request is the worker's decision, and a recheck of the same refusal while the branch is still
	// busy must not owe a second one. An attempt that does not deliver gives the refusal back while
	// it stands (restoreAttempt).
	l.w.recordRefusalObservation(key, observation)
	if l.reapplyOwed == nil {
		l.reapplyOwed = map[refusalKey]owedRefusal{}
	}
	l.reapplyOwed[key] = refusal
	l.w.Log.Info("Refused edit owes its applier a reapply, sent once the branch is quiet",
		"gitTarget", key.target.String(), "branch", l.w.Branch, "refused", refusal.detail)
}

// refusalIsAWriteBoundary reports whether this refusal is one where the FOLDER is accepted and
// only the edit had nowhere to land.
//
// **This is the pruning fence, and it replaces an earlier one that got it wrong.** The first
// version stat-ed the object's canonical file under the GitTarget's write path, on the reasoning
// that an object Git does not manage must not be requested for. It disabled the feature for its
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
// Every other refusal means the folder is unusable. Reapplying Git cannot fix a folder, only a
// human can, and the reconciler may be mid-way through its own corrections there, so those are
// left alone. They also reach a different reporter entirely: a folder-level refusal is normally
// found by the per-type reconcile, which blocks the collection through the event router rather than
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
	// is removing a document had one before it. Both would have been requested for. So every issue
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
