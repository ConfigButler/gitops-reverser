---
status: design
date: 2026-08-27
related:
  - target-watch-plan.md
  - data-plane-triggering.md
  - watch-and-catalog-architecture.md
---

# One owner for the watch plane: triggers in, work coalesced

> **design**: open, nothing built.
> Index: [`../INDEX.md`](../INDEX.md)

**The short version.** A rule edit is applied by the controller worker that observed
it, synchronously, and it re-plans every GitTarget in the process rather than the one
the rule names. The watch manager has no owner, so eleven mutexes stand in for one.
This proposes an actor: controllers submit intent, one loop mutates the watch plane,
the data plane reports back through the same channel, and every change to one GitTarget's
configuration inside a 2s silence window becomes a single pass over it.

This is a follow-up to [target-watch-plan.md](target-watch-plan.md). That plan made the
WORK cheap, per cell. This one is about how often the work is asked for, by whom, and
what happens when one target's work will not finish.

## What happens today

Both rule controllers call `Manager.ReconcileForRuleChange` inline, from six call sites
across `watchrule_controller.go`, `clusterwatchrule_controller.go` and
`watchrule_source_namespace.go`. The manager's own loop calls it too, on a 30s ticker
and on an API-surface trigger. That one function then does, in order:

1. `RefreshAPIResourceCatalog`, a discovery call;
2. `refreshSourceNamespaceScopes`, a namespace list;
3. `refreshWatchedTypeTables`;
4. `refreshRunningTargetWatches`, which walks **every** resident table and calls
   `replaceGitTargetWatches` for **every** running GitTarget.

The same reconcile then reads `StreamSummaryForWatchRule` back out and writes it to
status, so the read has to happen after the write, in the same pass, on the same
goroutine.

### The three costs, measured

**The fan-out is global.** One rule edit re-plans every target. In a single e2e run,
1256 plan reconciles were logged across 28 GitTargets, peaking at 78 in one second.
Change 2 of the target-watch plan made each of those nearly free for an unchanged
target, which is why this is a cost rather than an outage: the work is cheap now, but
it is still asked for N times when one target changed.

**It runs on a controller worker, behind network calls.** The two refreshes at the top
are I/O. A controller worker executing them is a worker not reconciling anything else,
and a call that blocks rather than fails takes that worker with it. This is not
hypothetical: the discovery call under `refreshClusterForDeclare` carries a request
timeout only for a REMOTE cluster, because `cluster_context.go` applies it under
`if !cc.isLocalLocked()`. On the local cluster the legacy, non-context
`ServerGroupsAndResources()` runs with no deadline at all.

**There is no owner, so there are eleven mutexes.** `clustersMu`,
`gitTargetClustersMu`, `resourceCatalogMu`, `triggersMu`, `targetWatchesMu`,
`gitPathEventsMu`, `gitTargetUIDsMu`, `gitTargetPruneModesMu`, `targetRetentionMu`,
`declaredGVRsMu`, `sourceNamespaceEventsMu`. They exist because the same state is
written by controller workers on three controllers, by the manager's own loop, by every
target-watch stream goroutine reporting readiness, and by branch-worker drain
goroutines reporting fidelity and retention, while status projections read it
concurrently. Each lock is individually defensible. The number is the smell.

## Half of the answer is already in the tree

`Manager.catalogRefreshCh` is a single-slot channel: `signalCatalogRefresh` does a
non-blocking send into a buffer of one, and the manager loop drains it beside the 30s
ticker. That is exactly a coalescing trigger, and API-surface changes (a CRD installed
or removed) already use it.

Rule changes bypass it and call straight through. So the shape being proposed here is
not new to this codebase; it is already the design for one trigger and not for the
others.

## The destination is an actor, not a lock refactor

> **Correction.** An earlier draft of this page said the loop owns `targetWatches`
> while also describing stream and worker reports as concurrent writes to the same
> state. Those are two different models and cannot both be the design. The message
> model below is the destination; keeping small locks is a migration step toward it,
> not the end state.

```text
controllers  ──Trigger{target, uid, reason}──┐
streams      ──Report{target, cell, state, revision}──┤
branch workers ──Report{target, fidelity|retention}──┤
                                                     ▼
                                              owner loop
                                   (sole writer of watch-plane state)
                                                     │
                                    publishes immutable status snapshot
                                                     ▼
controllers  ──read latest snapshot──────────────────┘
```

**The owner is the only writer** of `targetWatches`, stream readiness, the declared type
sets and the per-target captures. Nothing else mutates them.

**Cancellation is requested by the owner, and completion is reported back.** The owner
decides that a cell stops; the stream goroutine observes its context, unwinds, and posts
a report. It does not reach back for a manager lock on its way out. That inverts what
change 2 does today, where `set.stop(cell)` runs `cancel()` while holding
`targetWatchesMu` and the woken goroutine then contends for the same mutex.

**Reads are snapshot reads.** The owner publishes an immutable projection after each
pass; controllers read the latest one. A snapshot swap needs a pointer store, not a
critical section around a read-modify-write.

## The unit of change is the config, not the object

A GitTarget and the WatchRules that point at it are **one piece of configuration**. They
are edited together, reviewed together, and applied together. `kubectl apply -f config/`
with a GitTarget and four rules is one intent, and the API server delivers it as five
watch events within a few hundred milliseconds.

Today that is five reconciles, each doing a full global pass. With a per-target settle
window it is one pass, after the edit stops arriving.

**So the debounce is a rolling silence window, not a rate limit.** Each trigger for a
target resets its timer; the pass runs once the target has been quiet for the window.
That is the same mechanism the write path already uses one layer down: `DefaultCommitWindow`
is documented as "the rolling silence window used to coalesce events into one commit",
defaults to 5s, and is user-facing on `GitProvider.spec.push.commitWindow`. This is that
idea applied to configuration rather than to events.

**Two seconds.** Long enough to absorb a multi-object apply, short enough that an
operator watching `kubectl get watchrule` does not think it hung. It is fixed rather
than configurable, for the reason `PushCooldown` gives for being fixed beside the
configurable commit window: the cadence a user cares about is how quickly their config
takes effect, not how the controller batches its internal work.

**A maximum wait bounds it.** A rolling window that is reset forever never fires, so the
pass also runs once a target has been dirty for a hard cap (on the order of 10s) no
matter how much is still arriving. Continuous churn then converges at the cap instead of
starving.

**The window is per target.** Two GitTargets touched by one apply settle independently,
so a busy target cannot delay a quiet one. Deletes batch the same way: `kubectl delete -f`
over the same folder is one settle window, not five passes.

### The window is a heuristic, not a correctness boundary

Kubernetes has no "apply complete" event. The objects in one `kubectl apply` are separate
writes, and admission, cache propagation, controller scheduling and API load can spread
their watch events well past two seconds. So the window is a good bet about human
behavior, never a guarantee that a batch arrived whole.

Nothing may depend on the batch being complete. A trigger that lands after the pass has
run is not an error and not a lost update: it dirties the target again and the next pass
converges. The design has to be correct if every event arrives in its own window, and the
window only has to be *usually* right to be worth having.

### The dirty sequence: a change during a pass is never lost

The race that matters is a trigger arriving while its target's pass is running. Clearing
the dirty mark at the end of a pass would drop it.

So each target carries a **dirty sequence**. The owner captures it when the pass starts,
and at the end clears the dirty state only if the sequence is unchanged. If it moved, the
target stays dirty and is scheduled again immediately, with no settle window, because the
change it carries has already waited one out.

### The pass reads the rule store, not the trigger

A trigger names a target. It does not carry the rule that caused it, and the owner must
not build a plan from whichever rule controller happened to fire first. The pass reads a
coherent snapshot of the rule store and the resident tables, so the plan it builds is the
configuration as it stands at that moment, whole.

This is what makes the batching meaningful rather than cosmetic: five triggers collapsing
into one pass is only useful if that pass sees all five objects.

### This supersedes "never debounce the first declaration"

An earlier revision of this page said a GitTarget the owner has never seen should be
declared immediately, on the grounds that making a cold start wait is a needless
regression. That is now **reversed**, and the reason is the case above.

Applying a GitTarget together with its rules is the normal way this configuration
arrives, and object order within an apply is not guaranteed. If the GitTarget is
declared the instant it lands, it is declared with **no rules yet**: a plan with zero
cells, immediately superseded as each rule arrives. The "responsive" version therefore
manufactures a transient empty plan on every cold start, and then does the real work four
times over.

Transient empty plans are not benign. The empty-plan case is exactly what vacuously
cleared a live-write render divergence in `RenderFidelityGate.Reconcile`, because
restarting every scope holds trivially when there are no scopes; it needed an explicit
non-empty guard. A design that produces an empty plan on every cold start is inviting
that class of bug rather than avoiding it.

The cost of the reversal is bounded and small: a new GitTarget's first watch opens up to
2s later than it might have. The benefit is that the first plan it declares is the one
the user wrote.

## The contract

> A trigger marks a target dirty. The owner waits for two seconds of silence, then
> reconciles the latest coherent configuration once. New triggers during a pass are
> retained and cause another pass. A failed pass stays dirty and retries with backoff.

The word doing the work is **once**, and it means *one settled configuration adjustment*,
not *one function invocation*. A pass can fail, time out, or find that the world moved
under it, and every one of those produces another attempt. What the user is promised is
that a burst of related edits converges to one plan, not that the operator called a
function exactly once.

## The latency budget

"Eventually" is not a design. The bound this page commits to:

| Event | Bound |
| --- | --- |
| A target goes quiet after any change, including its first | its pass starts **2s** later |
| A target under continuous change | its pass starts at the **max wait**, about 10s |
| A pass that fails | target stays dirty, retried with backoff |
| Nothing triggering at all | the existing 30s periodic sweep is the floor |
| Status freshness after a change | the settle window plus the controller's requeue |

## Failure isolation is the point, not a detail

Centralizing ownership without a deadline does not fix the availability problem, it
relocates it: instead of one blocked controller worker, there is one blocked owner loop,
and now nothing else in the watch plane progresses either. That is strictly worse.

So the isolation boundary is **per target**:

- Every target's pass runs under **its own context with a deadline**. A discovery call
  that hangs fails that target's pass and leaves it dirty; the loop moves on.
- The loop processes a **bounded number of targets per pass**, round-robin over the
  dirty set, so one target that is slow but not hung cannot starve the rest.
- The unbounded discovery call named above is fixed regardless of this design. A
  deadline on the local-cluster path is correct today, and it is a precondition for
  trusting the owner loop tomorrow.
- A target whose pass exceeds its deadline stays dirty and is retried, so a deadline is
  a yield rather than a drop.

**A settle window bounds when work STARTS, never when it finishes.** The plan still
depends on discovery and on source-namespace state, and both are I/O. Two seconds of
silence says nothing about how long the pass then takes. That is why the deadline and the
cached snapshots below are part of this design rather than an optimization: without them
the debounce would be a promise the system cannot keep.

Without those three, the honest description of this proposal would be "the same
availability failure, in a different goroutine".

## Invalidation is scoped to what changed

A trigger says what changed, and the owner translates that into the smallest dirty set:

| Trigger | Dirties |
| --- | --- |
| A WatchRule or ClusterWatchRule edit | the target that rule names |
| A source-namespace scope change | the targets whose rules select that namespace |
| A catalog or API-surface change | the targets on the affected cluster whose tables reference the changed types |
| The periodic tick | everything, as the floor |

The middle rows are what keep the fan-out honest. "A CRD appeared" is not
a reason to re-plan a target on another cluster, or one whose watched types do not
mention it. Today every one of these paths is the same global pass.

## A trigger carries identity, not just a key

A dirty key alone is unsafe. A GitTarget can be deleted and recreated under the same
namespace and name, and a queued trigger for the old one would resurrect state under the
new one, or re-open watches for a target that is gone.

So a trigger carries the **UID**, and the owner drops a trigger whose UID no longer
matches what it holds. There is precedent in the tree: `forgetGitTargetUID` already
deletes only when the remembered UID matches the one it was handed.

One wrinkle to respect. The rule-derived reference carries no UID at all: the watch
table is built from rules, and `resolveGitTargetUID` exists precisely because
"the data-plane gitDest comes from the rule-derived watch table and has none". So the
UID has to be attached where it is known, by the GitTarget controller, and rule-side
triggers name a target that the owner resolves against its own record.

**Deletion invalidates before it forgets.** `ForgetGitTargetDeclaration` must drop any
pending trigger for that target in the same step that drops the state, or the next pass
re-creates what the delete just tore down.

## Status is eventually consistent, and says so

Today the sequence inside one reconcile is: apply the rule change, read the stream
summary, write status. With an owner loop the apply is asynchronous, so that read
observes the state from before it, and `StreamsRunning` lags by up to one interval.

That is acceptable, and it should be stated in the status contract rather than left for
someone to discover. The mechanism already exists: `RequeueStreamSettleInterval` is 10s
and exists for exactly this reason, because streams do not reach Streaming inside the
reconcile that started them either. The reconcile has never observed its own effect; it
currently observes an intermediate state that merely looks more immediate.

## Which locks stay, and which are the dangerous ones

Removing every mutex is not the goal, and a count is not a design. A lock is fine when
it guards a pointer swap of an immutable snapshot, or a small report queue. The
dangerous locks are the ones **held across** work that can block or call into another
component:

- across a discovery call or any network I/O;
- across a context cancellation, which wakes goroutines that then want the same lock;
- across stream startup;
- across a call into another subsystem's lock, which is what creates an order to get
  wrong.

`targetWatchesMu` is currently held across three of those four. That is the lock this
design is about, and it is a better target than the total count.

## What this deletes

The point of the design is that the system gets **smaller**, and that there is one way to
ask the watch plane to do something instead of four. This is the inventory.

### Four trigger mechanisms become one

Today a change reaches the watch plane by one of four routes, each with its own
semantics:

| Today | After |
| --- | --- |
| `ReconcileForRuleChange` called inline from six controller call sites | a trigger post |
| `DeclareForGitTarget` called synchronously from the GitTarget reconcile | a trigger post |
| `catalogRefreshCh`, a single-slot channel drained by the manager loop | the same trigger queue |
| the 30s periodic ticker | kept, as the floor |

`signalCatalogRefresh` and `catalogRefreshCh` go: a coalescing trigger queue that already
carries per-target and global invalidation has no use for a second, cruder one beside it.
The exported `ReconcileForRuleChange` on the `WatchManager` interface
(`internal/controller/constants.go`) narrows to a trigger call, and the six call sites
stop doing work.

### The global fan-out goes

`refreshRunningTargetWatches` is deleted outright. The owner walks its dirty set; there is
no reason to walk every resident table because one rule changed.

Its filter goes with it, and that is worth naming separately: it re-plans only targets
already present in `m.targetWatches`, which means **a target whose first declare never
completed is never picked up again**. That property is a trap. It is the reason a stuck
GitTarget in [`E2E-DECLARE-INVESTIGATION.md`](../../E2E-DECLARE-INVESTIGATION.md) could
never recover on its own. A dirty set marked on intent rather than on success does not
have it.

### Roughly half the mutexes go

Not because a count is a goal, but because each one disappears for a stated reason.

**Deleted, replaced by the owner being the sole writer:**

- `targetWatchesMu`, which today guards four maps at once: `targetWatches`,
  `targetStreamStates`, `targetGitPathAcceptance` and `targetRenderFidelity`. Its five
  outside writers all become reports: `markTargetStreamState` (stream goroutines),
  `MarkTargetGitPathAccepted` / `MarkTargetGitPathRefused` and `recordRenderFidelityStatus`
  (drain goroutines).
- `targetRetentionMu`, whose only outside writer is `MarkTargetRetention` from the same
  drain path.

**Deleted, replaced by a published snapshot:** `gitTargetUIDsMu`, `gitTargetPruneModesMu`,
`gitTargetClustersMu` and `declaredGVRsMu` guard state written only at declare time and
read by the data plane. A value written by one goroutine and read by many is a snapshot,
not a critical section.

**Kept, and worth saying why:**

- `gitPathEventsMu` and `sourceNamespaceEventsMu` guard channel handoffs to
  controller-runtime event sources. That is a queue boundary, not shared state.
- `triggersMu` guards the API-surface informer set, which has its own lifecycle.
- `RenderFidelityGate.mu` stays because the gate is genuinely shared: branch workers call
  `AllowsWrites` on their own goroutines, from another package, on the write path. It is
  not watch-plane state that an owner loop can absorb.

### Behavior that stops being accidental

Not a deletion, but the same simplification. Today a failed declare is retried by whatever
reconcile happens next, which works and is nobody's decision. After this it is a dirty
target with a backoff, which is a decision, written down, and observable.

### What must NOT collapse into this

One way of doing things is the goal, so the exceptions have to be explicit. The branch
worker's queue and its commit window are a different mechanism at a different layer, on
the write path, with a different hazard (a stale snapshot overtaking a newer write). They
are deliberately not merged into the trigger queue, and
[target-watch-plan.md](target-watch-plan.md) carries their reasoning.

## What this does not change

- **The branch worker's queue.** The coalescing described in
  [target-watch-plan.md](target-watch-plan.md) is on the write path, where the hazard is
  a stale snapshot overtaking a newer write. This page is about the trigger side of the
  watch manager. They are different queues with different failure modes, and neither
  substitutes for the other.
- **The per-cell diff.** The plan classification is what makes an unnecessary pass
  cheap; this makes the pass unnecessary less often. Both are worth having.
- **Leader election.** The manager already runs only on the elected leader
  (`NeedLeaderElection`), so a single owner loop adds no new assumption.

## Open questions

1. **What bounds the retry of a permanently failing target?** Staying dirty and
   retrying with backoff is right for a transient failure. A source cluster that is gone
   for a day should not be re-attempted every interval forever, and whatever it does
   instead has to be visible in status rather than only in logs.
2. **What does a repeated per-target deadline mean for status?** A target whose pass
   times out has no fresh plan, which is not the same as a target with an empty one.
   That distinction is the same "unobservable is not empty" rule the sweep already
   rests on, and it needs a condition rather than silence.
3. **How is the dirty set observable?** A queue that silently grows is what made the
   resync storm hard to see. A depth gauge and a per-pass count of targets re-planned
   would make this legible from metrics rather than from log volume.
4. **How far does the migration go?** Locks-first gets the fan-out and the isolation
   with a small diff and leaves the manager partly shared. The full message model
   removes the shared writers. The second is the destination; whether it is worth doing
   in one step is open.
