// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/go-logr/logr"

	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// The watch plane has ONE owner: the loop in Manager.Start. Controllers submit intent, the owner
// mutates, and every change to one GitTarget's configuration inside a rolling silence window
// becomes a single pass over it. Nothing outside this file applies a plan.
//
// See docs/design/watch-manager-ownership.md for why, and for what this replaced: six inline
// ReconcileForRuleChange call sites on controller workers, a synchronous DeclareForGitTarget, a
// second single-slot coalescing channel beside the trigger queue, and a global fan-out that
// re-planned every GitTarget because one rule changed.

const (
	// settleWindow is the rolling silence window a target must be quiet for before its pass runs.
	// A GitTarget and the WatchRules that point at it are ONE piece of configuration — applied
	// together, delivered as several watch events within a few hundred milliseconds — so the
	// window coalesces them into one pass. Two seconds is long enough to absorb a multi-object
	// apply and short enough that an operator watching `kubectl get watchrule` does not think it
	// hung. Fixed rather than configurable, for the reason PushCooldown gives beside the
	// configurable commit window: the cadence a user cares about is how quickly their config takes
	// effect, not how the controller batches its internal work.
	settleWindow = 2 * time.Second

	// maxSettleWait bounds the window. A rolling window that is reset forever never fires, so a
	// target that has been dirty this long runs no matter how much is still arriving; continuous
	// churn converges at the cap instead of starving.
	maxSettleWait = 10 * time.Second

	// targetPassTimeout is the deadline on ONE target's pass. Centralizing ownership without a
	// deadline would not fix the availability problem, it would relocate it: one blocked owner
	// loop instead of one blocked controller worker, with nothing else in the watch plane
	// progressing either. A pass that exceeds this fails, leaves the target dirty, and INSTALLS
	// NOTHING — an ungatherable plan must never present as an absent one.
	targetPassTimeout = 30 * time.Second

	// sharedRefreshTimeout is the deadline on one shared-snapshot refresh: the per-cluster API
	// catalogs and the source-namespace scopes. Both are I/O, both are shared across targets, and
	// neither may take the loop with it.
	sharedRefreshTimeout = 2 * time.Minute
)

// Trigger reasons, as they appear on the triggers-by-reason metric and in logs. There are three
// because there are three ways intent reaches the owner, and telling them apart is what shows
// which source is noisy.
const (
	// TriggerReasonDeclare is the GitTarget controller submitting a declaration.
	TriggerReasonDeclare = "declare"
	// TriggerReasonRuleChange is a WatchRule or ClusterWatchRule edit, refusal, or deletion.
	TriggerReasonRuleChange = "rule_change"
	// TriggerReasonSharedRefresh is a shared snapshot moving under a target: a CRD installed or
	// removed, an APIService appearing, a source-namespace grant or revocation. It is derived
	// rather than posted — the refresh compares each target's rendered plan across the
	// re-projection and marks only the ones it actually changed.
	TriggerReasonSharedRefresh = "shared_refresh"
	// TriggerReasonPeriodic is the 30s floor, the one path that still marks everything.
	TriggerReasonPeriodic = "periodic"
)

// declareIntent is what only the GitTarget controller knows about a target: its UID and the three
// values captured on Declare. It outlives any single trigger — a rule-side trigger names a target
// and the owner resolves it against this record, because the rule-derived watch table carries no
// UID at all (see resolveGitTargetUID).
type declareIntent struct {
	ref        types.ResourceReference
	clusterID  string
	auditRoute string
	pruneMode  v1alpha3.PruneMode
	// force is sticky: a declare that asks for a fresh recheck keeps asking until a pass actually
	// succeeds, so a failed attempt cannot consume it.
	force bool
}

// dirtyTarget is one GitTarget the owner owes a pass.
type dirtyTarget struct {
	ref types.ResourceReference
	// firstDirty is when this target went from clean to dirty, and is what maxSettleWait bounds.
	firstDirty time.Time
	// lastTrigger is the most recent trigger, and is what settleWindow rolls off.
	lastTrigger time.Time
	// seq increments on every trigger. The owner captures it when a pass starts and clears the
	// dirty state at the end only if it has not moved; a trigger that arrived mid-pass therefore
	// leaves the target dirty and is scheduled again immediately, with no settle window, because
	// the change it carries has already waited one out.
	seq uint64
	// retryAt is set by the backoff ladder after a failed pass. Zero means no backoff.
	retryAt  time.Time
	failures int
	reasons  map[string]struct{}
}

// readyAt is when this target's pass may start: after the silence window, or at the hard cap,
// whichever comes first — but never before a backoff the last failure asked for.
func (d *dirtyTarget) readyAt() time.Time {
	at := d.lastTrigger.Add(settleWindow)
	if hardCap := d.firstDirty.Add(maxSettleWait); hardCap.Before(at) {
		at = hardCap
	}
	if d.retryAt.After(at) {
		return d.retryAt
	}
	return at
}

// forgetAction is a deletion the owner must apply before it plans anything else.
//
// uid is the INCARNATION the deletion is for, resolved when it was queued rather than taken from
// the caller: both production callers react to a NotFound and so carry no UID at all, and a
// UID-less deletion that matched every incarnation would tear down the successor of a GitTarget
// deleted and recreated under the same namespace and name. Empty means nothing was on record to
// delete, which makes the teardown a no-op anyway.
type forgetAction struct {
	ref types.ResourceReference
	uid string
}

// watchPlaneTriggers is the coalescing intake: the dirty set, the declare records, the pending
// deletions, and the one-slot wake signal that tells the owner to look.
//
// Its mutex is a queue boundary, not shared state. It is held across map writes only — never
// across I/O, a cancellation, a stream start, or another subsystem's lock.
type watchPlaneTriggers struct {
	mu       sync.Mutex
	dirty    map[string]*dirtyTarget
	declares map[string]*declareIntent
	forgets  []forgetAction
	// invalidatedClusters names source clusters whose credentials rotated or were revoked. Every
	// GitTarget mirroring from one has its streams stopped and is replanned.
	invalidatedClusters []string
	wake                chan struct{}
	coalesced           uint64
	// sharedDue asks for a shared-snapshot refresh (the per-cluster catalogs and the
	// source-namespace scopes) on the next loop turn, with the targets it invalidates derived from
	// what actually changed rather than from a global fan-out.
	sharedDue bool
	// sweepDue asks additionally for every declared target to be marked dirty. It is the periodic
	// floor, and the only path that still fans out to everything.
	sweepDue bool
	// sharedRefreshing is true while a shared refresh is in flight on its own goroutine. It is
	// what keeps the loop from starting a second one, and from waiting on the first.
	sharedRefreshing bool
	// sharedFailures counts consecutive failed refreshes and sharedRetryAt is the floor the
	// backoff ladder puts under the next attempt. A failed refresh re-requests itself, and a
	// cluster that refuses the connection outright fails in a millisecond — without a floor those
	// two would spin, hammering discovery as fast as it can say no.
	sharedFailures int
	sharedRetryAt  time.Time
}

func (m *Manager) triggers() *watchPlaneTriggers {
	m.triggerInit.Do(func() {
		m.watchTriggers = &watchPlaneTriggers{
			dirty:    map[string]*dirtyTarget{},
			declares: map[string]*declareIntent{},
			wake:     make(chan struct{}, 1),
		}
	})
	return m.watchTriggers
}

// signal is the non-blocking wake. The buffer of one is the whole coalescing mechanism: a
// thousand triggers between two loop turns leave one token and one dirty entry per target.
func (t *watchPlaneTriggers) signal() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// markDirtyLocked records a trigger against one target. mu must be held.
func (t *watchPlaneTriggers) markDirtyLocked(ref types.ResourceReference, reason string, now time.Time) {
	entry := t.dirty[ref.Key()]
	coalesced := entry != nil
	if !coalesced {
		entry = &dirtyTarget{ref: ref, firstDirty: now, reasons: map[string]struct{}{}}
		t.dirty[ref.Key()] = entry
	} else {
		t.coalesced++
	}
	recordTriggerLocked(reason, coalesced)
	// The reference the owner plans against is the one the declare record holds; a rule-side
	// trigger carries no UID, so it must not be allowed to blank one already known.
	if ref.UID != "" {
		entry.ref = ref
	}
	entry.lastTrigger = now
	entry.seq++
	entry.reasons[reason] = struct{}{}
}

// TriggerRuleChange marks the GitTarget a rule names as needing a new plan pass.
//
// It replaces the six inline ReconcileForRuleChange call sites. Those did the work — a discovery
// call, a namespace list, a full re-projection, and then a replan of EVERY running GitTarget —
// synchronously, on the controller worker that observed the rule. This posts intent and returns.
//
// It deliberately does NOT also enqueue the GitTarget for reconcile. The GitTarget already learns
// that its plan moved from the pass itself, which enqueues on a render-fidelity change — the one
// moment its answer differs. Enqueueing here too would be a third path to the same channel, firing
// before anything it would report has changed.
func (m *Manager) TriggerRuleChange(gitDest types.ResourceReference) {
	m.trigger(gitDest, TriggerReasonRuleChange)
}

// TriggerAllRuleChange marks every declared GitTarget dirty. It is the deletion path only: a
// WatchRule that is already gone cannot be read for the target it named, so the owner cannot
// narrow the invalidation and does not pretend to.
func (m *Manager) TriggerAllRuleChange() {
	t := m.triggers()
	now := time.Now()
	t.mu.Lock()
	for _, intent := range t.declares {
		t.markDirtyLocked(intent.ref, TriggerReasonRuleChange, now)
	}
	t.mu.Unlock()
	t.signal()
}

func (m *Manager) trigger(gitDest types.ResourceReference, reason string) {
	t := m.triggers()
	t.mu.Lock()
	t.markDirtyLocked(gitDest, reason, time.Now())
	t.mu.Unlock()
	t.signal()
}

// signalSharedRefresh asks for the shared snapshots to be re-derived on the next loop turn. It is
// the whole of what signalCatalogRefresh and catalogRefreshCh used to be — a coalescing trigger
// queue that already carries per-target and global invalidation has no use for a second, cruder
// one beside it.
func (m *Manager) signalSharedRefresh() {
	t := m.triggers()
	t.mu.Lock()
	t.sharedDue = true
	t.mu.Unlock()
	t.signal()
}

// declareIntentFor records the capture-on-Declare values and marks the target dirty. The force
// flag is sticky across attempts: it survives until a pass actually succeeds.
func (m *Manager) declareIntentFor(
	gitDest types.ResourceReference,
	clusterID string,
	auditRoute string,
	pruneMode v1alpha3.PruneMode,
	force bool,
) {
	t := m.triggers()
	now := time.Now()
	next := declareIntent{
		ref:        gitDest,
		clusterID:  clusterID,
		auditRoute: auditRoute,
		pruneMode:  pruneMode,
		force:      force,
	}
	// A GitTarget reconcile is LEVEL-triggered: the controller re-declares on its steady requeue
	// and on every event it watches, with the same values every time. Marking the target dirty for
	// each of those would keep a healthy target permanently owed a pass, which is both a pass per
	// reconcile and — because "the declaration has landed" is a readiness input — a target that
	// never reads converged. So a re-declare that changes nothing and has already landed is a
	// no-op, and the 30s periodic sweep remains the floor under it.
	t.mu.Lock()
	prior := t.declares[gitDest.Key()]
	landed := m.watchPlane().passes[gitDest.Key()].Landed
	if prior != nil {
		next.force = next.force || prior.force
	}
	t.declares[gitDest.Key()] = &next
	if prior == nil || *prior != next || !landed {
		t.markDirtyLocked(gitDest, TriggerReasonDeclare, now)
	}
	t.mu.Unlock()
	t.signal()
}

// forgetIntent drops a deleted GitTarget's declare record and any pending trigger for it, and
// queues the teardown for the owner.
//
// Dropping the trigger happens HERE, synchronously in the caller, rather than on the loop: a
// pending trigger that survived the delete would have the next pass re-create exactly what the
// delete tore down.
func (m *Manager) forgetIntent(gitDest types.ResourceReference) {
	t := m.triggers()
	t.mu.Lock()
	defer t.mu.Unlock()

	// Which incarnation is this deletion for? The declare record is the authority, because it is
	// what a recreation replaces. A caller that names a UID and disagrees with the record is a
	// STALE delete: its object is already gone and a successor has declared, so it must not take
	// the successor's pending pass with it.
	record := t.declares[gitDest.Key()]
	uid := gitDest.UID
	switch {
	case record == nil:
	case gitDest.UID == "":
		// A NotFound cleanup: it names no incarnation, so it means whichever one is on record now.
		uid = record.ref.UID
	case record.ref.UID != gitDest.UID:
		t.forgets = append(t.forgets, forgetAction{ref: gitDest, uid: uid})
		t.signal()
		return
	}

	// Dropping the pending trigger happens HERE, synchronously in the caller, rather than on the
	// loop: a trigger that outlived the delete would have the next pass re-create what the delete
	// tore down.
	delete(t.dirty, gitDest.Key())
	delete(t.declares, gitDest.Key())
	t.forgets = append(t.forgets, forgetAction{ref: gitDest, uid: uid})
	t.signal()
}

// DeclareStatusForGitTarget reports whether the owner still owes this GitTarget a pass and how
// the last one ended. A target whose passes keep timing out must not read as idle.
func (m *Manager) DeclareStatusForGitTarget(gitDest types.ResourceReference) DeclareStatus {
	t := m.triggers()
	t.mu.Lock()
	intent, declared := t.declares[gitDest.Key()]
	if declared && !matchesUID(intent.ref, gitDest) {
		declared = false
	}
	clusterID := ""
	if declared {
		clusterID = intent.clusterID
	}
	t.mu.Unlock()

	// Pending is "no pass has ever landed for this target", NOT "the target is dirty right now".
	// Being dirty is the steady state of a system that replans on every rule edit and sweeps every
	// 30s; treating it as unsettled would flap Ready on a healthy mirror. What is worth holding a
	// target below Ready is a declaration that has never taken effect.
	pass := m.watchPlane().passes[gitDest.Key()]
	return DeclareStatus{
		Declared:  declared,
		ClusterID: clusterID,
		Pending:   declared && !pass.Landed,
		Failures:  pass.Failures,
		LastError: pass.LastError,
	}
}

// runOwnerLoop is the owner. It is the sole writer of the watch plane: it applies plans, starts
// and cancels streams, and publishes the snapshot every reader sees. It blocks until the context
// is cancelled.
func (m *Manager) runOwnerLoop(ctx context.Context, log logr.Logger) {
	t := m.triggers()

	// The floor. Nothing else in this loop is periodic; every other pass is driven by a trigger.
	periodic := time.NewTicker(periodicReconcileInterval)
	defer periodic.Stop()
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	// The very first turn refreshes the shared snapshots and sweeps, so a restart converges
	// without waiting for the first tick.
	t.mu.Lock()
	t.sharedDue, t.sweepDue = true, true
	t.mu.Unlock()

	timer := time.NewTimer(time.Hour)
	defer timer.Stop()

	for {
		wait := m.ownerTurn(ctx, log)
		if ctx.Err() != nil {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)

		select {
		case <-ctx.Done():
			return
		case <-t.wake:
		case <-timer.C:
		case <-periodic.C:
			t.mu.Lock()
			t.sharedDue, t.sweepDue = true, true
			t.mu.Unlock()
		case <-heartbeat.C:
			m.logOwnerHeartbeat(log)
		}
	}
}

// ownerTurn does one turn of work and returns how long to sleep before the next one, if nothing
// wakes it sooner.
func (m *Manager) ownerTurn(ctx context.Context, log logr.Logger) time.Duration {
	m.applyPendingTeardowns(log)
	m.refreshSharedSnapshotsIfDue(ctx, log)
	m.publishDirtySetDepth()

	// One target per turn, always the one that has been ready longest. A pass is pure in-memory
	// work — every network call the watch plane makes is in the shared refresh above, which runs
	// on its own goroutine — so this is not a throughput bottleneck. It is the bound that stops
	// one slow target from starving the rest, and it keeps the deletions above from waiting
	// behind a queue of passes.
	entry, wait := m.nextReadyTarget()
	if entry == nil {
		// Nothing to plan. Sleep no longer than a backed-off shared refresh is owed, or a failed
		// one would not be reattempted until something else happened to wake the loop.
		return minDuration(wait, m.sharedRefreshWait())
	}
	m.runTargetPass(ctx, log, entry)
	return 0
}

// sharedRefreshWait is how long until a backed-off shared refresh may be reattempted, or a long
// sleep when none is owed.
func (m *Manager) sharedRefreshWait() time.Duration {
	t := m.triggers()
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.sharedDue && !t.sweepDue {
		return time.Hour
	}
	if wait := time.Until(t.sharedRetryAt); wait > 0 {
		return wait
	}
	return time.Hour
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// applyPendingTeardowns tears down every GitTarget whose deletion has been queued, and stops the
// streams of every target on a cluster whose credentials moved. It runs before any planning, so a
// delete followed by a recreation under the same name tears down the old incarnation before the
// new one declares.
func (m *Manager) applyPendingTeardowns(log logr.Logger) {
	t := m.triggers()
	t.mu.Lock()
	pending := t.forgets
	invalidated := t.invalidatedClusters
	t.forgets, t.invalidatedClusters = nil, nil
	t.mu.Unlock()

	for _, clusterID := range invalidated {
		m.applyClusterInvalidation(clusterID)
	}

	for _, action := range pending {
		if current, stale := m.staleTeardown(action); stale {
			log.V(1).Info("dropping a stale GitTarget teardown; the name has been recreated",
				"gitDest", action.ref.String(), "staleUID", action.uid, "currentUID", current)
			continue
		}
		m.tearDownGitTarget(action.ref)
	}
}

// matchesUID reports whether two references name the same INCARNATION of a GitTarget. An empty UID
// on either side matches, because the rule-derived paths carry none — a WatchRule names a target
// by namespace and name only. Deletion deliberately does NOT use it: see forgetAction.
func matchesUID(a, b types.ResourceReference) bool {
	return a.UID == "" || b.UID == "" || a.UID == b.UID
}

// staleTeardown reports whether a queued deletion belongs to an incarnation that has since been
// replaced, and what replaced it. Applying such a deletion would tear down its successor.
//
// Two records can name the current incarnation and they settle at different times: a recreation
// declares immediately, while the watch-plane UID only moves once that declaration's pass has run.
// Either disagreeing with the queued deletion is enough to drop it.
func (m *Manager) staleTeardown(action forgetAction) (string, bool) {
	t := m.triggers()
	t.mu.Lock()
	record := t.declares[action.ref.Key()]
	t.mu.Unlock()
	if record != nil && record.ref.UID != action.uid {
		return record.ref.UID, true
	}
	if uid := m.watchPlane().uids[action.ref.Key()]; uid != "" && action.uid != "" && uid != action.uid {
		return uid, true
	}
	return "", false
}

// refreshSharedSnapshotsIfDue re-derives the state that is SHARED across targets — every active
// cluster's API catalog and the source-namespace scopes — and then marks dirty only the targets
// the refresh actually invalidated.
//
// Keeping this apart from replanning a target is the point. A target's plan depends on three
// inputs, two of them shared, and one rule edit used to re-derive all three and then walk every
// target. Here a rule edit replans one target and rediscovers nothing.
//
// It runs on its OWN goroutine, and the loop does not wait for it. This is the only I/O the watch
// plane does, so leaving it inline would mean an unreachable source cluster stalling every healthy
// target, every teardown and every report for the full refresh timeout — the availability failure
// this design exists to remove, relocated rather than fixed. One refresh runs at a time; a request
// arriving while one is in flight is held and served by the next.
func (m *Manager) refreshSharedSnapshotsIfDue(ctx context.Context, log logr.Logger) {
	t := m.triggers()
	t.mu.Lock()
	if t.sharedRefreshing || (!t.sharedDue && !t.sweepDue) || time.Now().Before(t.sharedRetryAt) {
		t.mu.Unlock()
		return
	}
	sweep := t.sweepDue
	t.sharedDue, t.sweepDue = false, false
	t.sharedRefreshing = true
	t.mu.Unlock()

	go func() {
		defer func() {
			t.mu.Lock()
			t.sharedRefreshing = false
			t.mu.Unlock()
			// Wake the loop: the refresh has just marked its dirty set, and the targets it marked
			// should start their settle window from now rather than from the next tick.
			t.signal()
		}()
		gathered := m.refreshSharedSnapshots(ctx, log, sweep)
		if !gathered {
			// A refresh that could not gather leaves every target planning against snapshots that
			// may be stale, and unlike a target pass it has no dirty entry of its own to carry the
			// retry. So it asks for another rather than waiting out the 30s sweep: a cluster that
			// just became unreachable is exactly when the plan is most likely to be wrong.
			//
			// On the same ladder a target pass uses, and the floor is not decoration. An
			// unreachable cluster is slow, but one that REFUSES the connection fails in a
			// millisecond — a re-request with nothing under it would hammer discovery as fast as
			// it can say no.
			t.mu.Lock()
			t.sharedDue = true
			t.sweepDue = t.sweepDue || sweep
			t.sharedFailures++
			t.sharedRetryAt = time.Now().Add(retryDelay(t.sharedFailures))
			t.mu.Unlock()
			return
		}
		t.mu.Lock()
		t.sharedFailures, t.sharedRetryAt = 0, time.Time{}
		t.mu.Unlock()
	}()
}

// refreshSharedSnapshots is the refresh itself, under its own deadline. It reports whether the
// shared state it publishes can be trusted.
func (m *Manager) refreshSharedSnapshots(ctx context.Context, log logr.Logger, sweep bool) bool {
	refreshCtx, cancel := context.WithTimeout(ctx, sharedRefreshTimeout)
	defer cancel()

	gathered := true
	if err := m.refreshAPIResourceCatalog(refreshCtx); err != nil {
		log.Error(err, "API resource catalog refresh failed; it will be retried")
		gathered = false
	}
	// Which targets did that actually invalidate? Comparing each target's rendered watch plan
	// across the re-projection answers it without a type-to-target index and without the
	// staleness one would bring: a CRD that appeared in a cluster no target's rules select
	// changes no plan, so it dirties nothing.
	before := m.watchPlanFingerprints()
	m.refreshWatchedTypeTables()
	m.markInvalidatedTargets(before, m.watchPlanFingerprints(), sweep)
	return gathered
}

// markInvalidatedTargets marks the declared targets a shared refresh actually invalidated: the
// ones whose rendered watch plan differs across the re-projection, plus everything when the
// periodic floor asked for a sweep.
//
// This is what keeps the fan-out honest. "A CRD appeared" is not a reason to replan a target on
// another cluster, or one whose watched types do not mention it — and the plan diff says so
// without a type-to-target index and without the staleness one would bring.
func (m *Manager) markInvalidatedTargets(before, after map[string]uint64, sweep bool) {
	t := m.triggers()
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	for key, intent := range t.declares {
		switch {
		case sweep:
			t.markDirtyLocked(intent.ref, TriggerReasonPeriodic, now)
		case before[key] != after[key]:
			t.markDirtyLocked(intent.ref, TriggerReasonSharedRefresh, now)
		}
	}
}

// watchPlanFingerprints renders every resident table's declared stream set, so two projections
// can be compared per target. It reads the published tables without refreshing them.
func (m *Manager) watchPlanFingerprints() map[string]uint64 {
	tables := m.residentWatchedTypeTables()
	out := make(map[string]uint64, len(tables))
	for _, table := range tables {
		out[table.GitDest.Key()] = watchPlanFingerprint(table)
	}
	return out
}

// nextReadyTarget returns the dirty target whose pass may start and has been waiting longest,
// or nil and how long until the earliest one is ready.
func (m *Manager) nextReadyTarget() (*dirtyTarget, time.Duration) {
	t := m.triggers()
	now := time.Now()

	t.mu.Lock()
	defer t.mu.Unlock()

	chosen := (*dirtyTarget)(nil)
	soonest := time.Hour
	// Sorted, so which target runs first is deterministic when several are ready at once.
	for _, key := range sortedKeys(t.dirty) {
		entry := t.dirty[key]
		at := entry.readyAt()
		if !at.After(now) {
			if chosen == nil || entry.firstDirty.Before(chosen.firstDirty) {
				chosen = entry
			}
			continue
		}
		if d := at.Sub(now); d < soonest {
			soonest = d
		}
	}
	if chosen != nil {
		// A copy: the entry stays in the map (so a trigger arriving mid-pass finds it and bumps
		// its sequence) and the pass plans against a value nothing else mutates.
		snapshot := *chosen
		return &snapshot, 0
	}
	return nil, soonest
}

func sortedKeys[V any](in map[string]V) []string {
	out := make([]string, 0, len(in))
	for k := range in {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// runTargetPass plans ONE GitTarget, under its own deadline, and settles its dirty state.
//
// A pass that fails, times out, or finds that the world moved under it produces another attempt:
// what the contract promises is that a burst of related edits converges to one plan, not that the
// operator called a function exactly once.
func (m *Manager) runTargetPass(ctx context.Context, log logr.Logger, entry *dirtyTarget) {
	t := m.triggers()
	key := entry.ref.Key()

	t.mu.Lock()
	intent := t.declares[key]
	var planned declareIntent
	if intent != nil {
		planned = *intent
	}
	t.mu.Unlock()

	if intent == nil {
		// A trigger for a GitTarget that has never declared. There is no UID, no source cluster
		// and no audit route to plan against, and inventing them is exactly the resurrection the
		// UID check exists to prevent. It converges when the GitTarget controller declares.
		log.V(1).Info("dropping a trigger for an undeclared GitTarget", "gitDest", entry.ref.String())
		m.clearDirty(entry, nil)
		return
	}

	passCtx, cancel := context.WithTimeout(ctx, targetPassTimeout)
	defer cancel()

	started := time.Now()
	err := m.applyTargetPlan(passCtx, planned)
	m.recordPassOutcome(planned.ref, started, err)

	if err != nil {
		log.Error(err, "GitTarget plan pass failed; the target stays dirty",
			"gitDest", planned.ref.String(), "reasons", sortedKeys(entry.reasons))
	}
	m.clearDirty(entry, err)
}

// applyTargetPlan is the pass itself: capture what the declare knows, make sure the cluster has
// been observed, and bring the running watch set into line with what the tables say.
//
// It reads the rule store and the resident tables rather than anything the trigger carried. A
// trigger names a target; it does not carry the rule that caused it, and a plan built from
// whichever rule controller happened to fire first would not be the configuration as it stands.
func (m *Manager) applyTargetPlan(ctx context.Context, intent declareIntent) error {
	force := intent.force || m.pruneModeRequiresReplay(intent.ref, intent.pruneMode)
	m.rememberDeclareCapture(intent.ref, intent.clusterID, intent.auditRoute)

	// A pass never dials anything, not even for a cluster nothing has observed yet. Discovery is
	// the SHARED job: the capture above has just put this target's source cluster into the active
	// set, so asking for a refresh is enough, and the pass below fails with "the surface has not
	// been observed yet" until it lands. Doing that dial here instead would put the one unbounded
	// thing the watch plane touches back on the loop that owns every other target.
	if !m.registryForGitTarget(intent.ref).Ready() {
		m.signalSharedRefresh()
	}

	if err := m.ensureGitTargetWatches(ctx, intent.ref, force); err != nil {
		return err
	}
	// Only once the watches are actually in place: a failed pass must leave the pending force
	// standing for the next one rather than consuming it.
	m.rememberGitTargetPruneMode(intent.ref, intent.pruneMode)
	m.clearDeclareForce(intent.ref)
	return nil
}

// clearDirty settles a target after its pass. The dirty sequence is what makes a change arriving
// mid-pass impossible to lose: if it moved, the target stays dirty and is scheduled again with no
// settle window, because the change it carries has already waited one out.
func (m *Manager) clearDirty(entry *dirtyTarget, passErr error) {
	t := m.triggers()
	now := time.Now()

	t.mu.Lock()
	defer t.mu.Unlock()
	current := t.dirty[entry.ref.Key()]
	if current == nil {
		// Deleted while its pass ran. Nothing to settle, and nothing to reschedule.
		return
	}
	if passErr == nil && current.seq == entry.seq {
		delete(t.dirty, entry.ref.Key())
		return
	}
	if passErr != nil {
		current.failures++
		current.retryAt = now.Add(retryDelay(current.failures))
		return
	}
	// The world moved under the pass. Re-run immediately: this change has already settled once.
	current.firstDirty = now
	current.lastTrigger = now.Add(-settleWindow)
	current.retryAt = time.Time{}
	current.failures = 0
	t.signal()
}

// retryDelay picks the backoff step for the n-th consecutive failure, capped at the last rung.
//
// A failed pass used to be retried by whatever reconcile happened next, which worked and was
// nobody's decision. This is a decision, and it is written down.
func retryDelay(failures int) time.Duration {
	ladder := [...]time.Duration{
		2 * time.Second,
		5 * time.Second,
		10 * time.Second,
		30 * time.Second,
		time.Minute,
	}
	if failures < 1 {
		failures = 1
	}
	if failures > len(ladder) {
		failures = len(ladder)
	}
	return ladder[failures-1]
}
