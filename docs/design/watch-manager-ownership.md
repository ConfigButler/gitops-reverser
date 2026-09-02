# One owner for the watch plane: triggers in, work coalesced

> **built**: all four steps have shipped; "Implementation order" records what each one deleted, and
> "What shipped" at the end records where the implementation departed from this page and why.
> Index: [`../INDEX.md`](../INDEX.md)
> Date: 2026-08-27.
> Related: [`target-watch-plan.md`](target-watch-plan.md),
> [`data-plane-triggering.md`](data-plane-triggering.md),
> [`watch-and-catalog-architecture.md`](watch-and-catalog-architecture.md)

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

> After the latest trigger, the owner starts one reconciliation after two seconds of
> silence. The reconciliation uses the latest configuration snapshot, has a per-target
> deadline, and preserves newer triggers or failures for later retry.

The word doing the work is **once**, and it means *one settled configuration adjustment*,
not *one function invocation*. A pass can fail, time out, or find that the world moved
under it, and every one of those produces another attempt. What the user is promised is
that a burst of related edits converges to one plan, not that the operator called a
function exactly once.

## The latency budget

"Eventually" is not a design. The bound this page commits to:

**What is promised is when work STARTS.** End-to-end readiness is not on this table and
must not be, because discovery, replay, the Git write and the controller's own status
update all sit downstream of the pass and none of them is bounded by the debounce.

| Event | Bound |
| --- | --- |
| A target goes quiet after any change, including its first | its pass **starts** 2s later |
| A target under continuous change | its pass **starts** at the max wait, about 10s |
| A pass that fails or times out | target stays dirty, retried with backoff |
| Retry backoff | 2s, 5s, 10s, 30s, capped at 1 minute |
| Nothing triggering at all | the existing 30s periodic sweep is the floor |

## Failure isolation is the point, not a detail

Centralizing ownership without a deadline does not fix the availability problem, it
relocates it: instead of one blocked controller worker, there is one blocked owner loop,
and now nothing else in the watch plane progresses either. That is strictly worse.

So the isolation boundary is **per target**:

- Every target's pass runs under **its own context with a deadline**. A discovery call
  that hangs fails that target's pass and leaves it dirty; the loop moves on.
- The loop processes a **bounded number of targets per pass** — one per turn, always the one that
  has been ready longest — so one target that is slow but not hung cannot starve the rest.
- The unbounded discovery call named above is fixed regardless of this design. A
  deadline on the local-cluster path is correct today, and it is a precondition for
  trusting the owner loop tomorrow.
- A target whose pass exceeds its deadline stays dirty and is retried, so a deadline is
  a yield rather than a drop.

**A timeout must never install an empty plan.** A pass that could not gather is not a
pass that found nothing, and the difference is the whole of "What a cell leaving means"
in [target-watch-plan.md](target-watch-plan.md): an ungatherable cell must never present
as an absent one. So a deadline produces exactly this and nothing else:

```text
pass failed
target remains dirty
failure and backoff recorded
owner continues with other targets
```

The failure is recorded where an operator looks, not only in a log: a target whose passes
keep timing out must not read as idle. A permanently unreachable source cluster looks the
same as a healthy quiet one unless the retry state is published.

**A settle window bounds when work STARTS, never when it finishes.** The plan still
depends on discovery and on source-namespace state, and both are I/O. Two seconds of
silence says nothing about how long the pass then takes. That is why the deadline and the
cached snapshots below are part of this design rather than an optimization: without them
the debounce would be a promise the system cannot keep.

Without those three, the honest description of this proposal would be "the same
availability failure, in a different goroutine".

## The dirty set has to be observable from the start

A queue that grows silently is what made the resync storm hard to see, and an owner loop
is a queue. These are part of the first implementation rather than a follow-up:

| Signal | Why |
| --- | --- |
| **Oldest dirty-target age** | the single most useful operational number: it goes up and stays up exactly when something is stuck |
| Dirty target count | depth, for saturation |
| Passes started / completed / failed / timed out | separates "not running" from "running and failing" |
| Pass duration | the input to choosing the deadline |
| Triggers by reason | shows which source is noisy |
| Coalesced trigger count | proves the debounce is doing what it claims |

## Refreshing shared state is not the same job as replanning a target

A target's plan depends on three inputs: the API catalog for its cluster, the
source-namespace scopes, and the compiled rules. Two of those are **shared** across
targets, and today one rule edit re-derives all of them and then walks every target.

The owner keeps the two jobs apart:

- **Refresh shared snapshots** (catalog per cluster, namespace scopes) on their own
  cadence and on their own invalidation, with their own deadlines.
- **Replan one target** against whatever snapshots are current, which is pure work over
  in-memory state.

A rule edit therefore replans one target and rediscovers nothing. A catalog change
refreshes that cluster's snapshot once and then marks the targets on that cluster whose
tables reference the changed types. This is what stops "one edit, N discovery calls" from
coming back through a different door.

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
re-creates what the delete tore down.

This needs a test of its own, and the case to write is **delete and recreate under the
same namespace and name**, with a trigger for the old UID still pending. That is the
sequence where a stale trigger resurrects watches for an object that no longer exists,
and it is not covered by testing delete alone.

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
GitTarget in [`e2e-declare-investigation.md`](../finished/e2e-declare-investigation.md) could
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

## Implementation order

Staged so the first change is reviewable on its own. The full message conversion is the
destination, not the first commit.

### Step 0: bound the discovery call

Independent of everything else, and worth landing first because it is small, it is a real
defect today, and every later stage trusts it.

`clusterDiscovery` copies the REST config and sets `sourceClusterDialTimeout` only when
the cluster is remote. Do it unconditionally. The discovery client is a separate client
from the ones that open watches, which is exactly why the existing comment says a copy is
safe: a deadline on the discovery copy cannot deadline a watch. On the local cluster today
`discoCfg = cfg` with no timeout, so the legacy non-context `ServerGroupsAndResources()`
can hang forever on a controller worker.

**Proves it:** a unit test that the discovery config carries a timeout for a local
cluster, and that the config used for watches does not.

### Step 1: the owner loop

The behavioral change, and the one that removes the erratic replanning.

- one owner loop, sole writer of the watch-plane state it already reaches;
- target-level triggers replacing the six inline `ReconcileForRuleChange` call sites and
  the synchronous `DeclareForGitTarget`;
- the 2s rolling silence window, per target, with a ~10s max wait;
- the dirty sequence, so a trigger arriving mid-pass is never lost;
- a per-target context deadline, with a timeout leaving the target dirty and installing
  nothing;
- retry with the 2s/5s/10s/30s/1m backoff, recorded where an operator can see it;
- **no synchronous watch-manager work on any controller worker**;
- the metrics table above.

**Deletes:** `refreshRunningTargetWatches` and its running-set filter,
`signalCatalogRefresh` and `catalogRefreshCh`, and the six inline call sites.

**Proves it:** one `kubectl apply` of a GitTarget plus four WatchRules produces exactly
one plan pass for that target; a trigger arriving during a pass causes a second pass; a
pass that times out leaves the target dirty and its plan untouched; a delete-and-recreate
under the same name does not resurrect the old target's watches; and an edit to one
target produces no pass for any other.

### Step 2: reports become messages

Once the loop exists, move the writers off the shared maps: `markTargetStreamState`,
`MarkTargetGitPathAccepted` / `MarkTargetGitPathRefused`, `recordRenderFidelityStatus` and
`MarkTargetRetention` post reports instead of taking `targetWatchesMu` and
`targetRetentionMu`. Reads become a published snapshot.

**Deletes:** `targetWatchesMu`, `targetRetentionMu`, and the four
write-once-read-many locks named under "What this deletes".

**Proves it:** the existing readiness, retention and fidelity tests pass unchanged, which
is the point: this stage changes who writes, not what is written.

### Step 3: scope the catalog invalidation

Split shared-snapshot refresh from per-target replanning, so a catalog change refreshes
one cluster and marks only the targets that reference the changed types. This is the last
place a global fan-out survives, and it is worth doing separately because it needs the
type-to-target index that steps 1 and 2 do not.

## What shipped

Three things resolved differently from the page above, and one open question stayed open.

### Reports are a published snapshot, not a second channel

Step 2 said the data-plane reports would post messages that the owner loop applies. What shipped
splits the state in two, and the split is where the value actually was:

- **The PLAN** — `targetWatches`, the running streams, the cancels — is owned by the loop and
  carries **no mutex at all**. This is the whole of what the page was about: `targetWatchesMu` was
  held across cancelling a stream, across starting one, and across a call into the render-fidelity
  gate's lock, three of the four hazards listed under "Which locks stay". None of them is reachable
  now, because there is no lock to hold.
- **The PROJECTION** — stream readiness, the two write-safety surfaces, the retention roll-up and
  the declare-time captures — is an immutable snapshot behind an atomic pointer
  (`watch_plane_state.go`). Readers take a pointer load and no lock. Writers serialize on one
  `stateMu` that is held across map writes only.

A report channel would have bought one more thing — the owner as the literal sole writer — at the
cost of a queue that has to be drained for a test to observe its own effect, and of report latency
bounded by whatever pass is in flight. The lock it replaces is not a dangerous one by this page's
own criteria: it is never held across I/O, a cancellation, a stream start, or another subsystem's
lock. The six mutexes named for deletion are deleted either way.

`declaredGVRs` and `declaredGVRsMu` were deleted outright rather than converted. Nothing wrote the
map; the only surviving reference was the delete in `ForgetGitTargetDeclaration`.

### Isolation came from taking the I/O off the loop, not from a deadline

The page says a per-target deadline is what stops one blocked target from taking the owner with
it. A deadline BOUNDS a stall; it does not isolate one. A first cut kept two network calls on the
loop — the shared refresh, and a discovery call for a target whose cluster had never been observed
— and a single unreachable source cluster could therefore hold every healthy target, every
teardown and every report for the full timeout. That is the availability failure this page exists
to remove, relocated rather than fixed.

What shipped removes the I/O instead:

- **A pass never dials.** Not even for a cluster nothing has observed yet, which is where it is
  most tempting. The declare capture has already put that cluster in the active set, so the pass
  asks for a shared refresh and fails with "the surface has not been observed yet" until it lands.
  A pass is now pure in-memory work over the tables, and `refreshClusterForDeclare` is deleted.
- **The shared refresh runs on its own goroutine**, one at a time, and the loop does not wait for
  it. A request arriving while one is in flight is held and served by the next.

The per-target deadline stays, as the backstop it should have been: with no I/O in a pass, nothing
should ever reach it. The alternative — per-target workers — was rejected because it would give
`targetWatches` more than one writer again, which is the thing this page is about.

### A stream's lifetime is the manager's, not the pass that started it

The most expensive mistake this change made, and the one only e2e caught. A pass runs under a
deadline, so its context is cancelled the moment it returns — and streams were being started as
children of it. Every stream therefore died the instant its plan finished being applied.

The signature is worth recording, because it reads like health: the plan log says `start:1`, the
stream set holds the cell, and every later pass reports it as `keep:1` and so never restarts it.
Readiness never leaves `Replaying`, the render-fidelity gate never leaves `Rechecking`, and every
WatchRule pointing at that target sits `Ready=False` forever. Nothing logs an error, because
nothing failed.

So the parent of a target watch is `Manager.watchLifetime`, set once by `Start`. Cancelling one
stream is the owner's decision, made per cell through the plan diff — never a side effect of a
context going out of scope. The pass deadline bounds the pass.

### Toggling a rule off and on inside the window is not a replay

The behavioral consequence of coalescing that is worth stating outright, because it is a technique
people use. Applying a rule without a type and then with it again — the "kick it" gesture — used to
tear the stream down and re-establish it, because each apply replanned synchronously. Inside one
settle window it is now a single pass over a plan that never changed: nothing is torn down and
nothing replays.

That is correct rather than regrettable. The pass reads the configuration as it stands, whole, and
a net-zero change is no change. But it is a real difference in what an operator gesture does, and
it is what an e2e prune spec was relying on to force a scoped resync. The supported way to force a
replay is unchanged: widening `prune.mode` into a sweeping mode, which exists precisely because a
policy change that the watch plan cannot see still has to produce one
(see `prune_declaration.go`).

For a test, the fix is to let the two halves land in different passes by observing the first one
take effect.

### Gate on the rule, not on its target's roll-up

Both e2e specs that broke were asking the GitTarget "are all your streams running" about a change
to ONE rule. That roll-up answers True for a target that is already mirroring, before the changed
rule has been planned at all, and it is published by a different controller than the one that
compiled the rule — so it can still be describing the previous plan.

A rule's own `StreamsRunning` has neither problem: it is written by the reconcile that compiled the
rule, from the rule's own compiled cells. It is the right gate whenever a spec changes a rule on a
target that is already mirroring, and `waitForWatchRuleStreamsRunning` existed unused for exactly
this.

This is also why the trigger deliberately does NOT enqueue its GitTarget. The GitTarget already
learns that its plan moved from the pass itself, which enqueues on a render-fidelity change — the
one moment its answer differs. A trigger-time enqueue would be a third path to the same channel,
firing before anything it would report has changed.

### A failed shared refresh asks for another

A target pass that fails carries its own retry, in its dirty entry. A shared refresh has none, so
one that could not gather has to re-request itself; otherwise every target keeps planning against
snapshots that may be stale until the 30s sweep, which is the wrong latency for a cluster that has
just become unreachable.

The re-request needs the same backoff ladder a target pass uses, for a reason that is easy to miss:
an unreachable cluster is slow, but a cluster that REFUSES the connection fails in a millisecond,
and a re-request with no floor under it would hammer discovery as fast as it can say no.

### Deletion names an incarnation, and resolves it when it is queued

The page says a trigger carries the UID and the owner drops one whose UID no longer matches. Both
production callers of `ForgetGitTargetDeclaration` react to a **NotFound** and so carry no UID at
all, and the rule-derived `matchesUID` treats an empty UID as matching everything — so a UID-less
deletion matched every incarnation and could tear down the successor of a GitTarget deleted and
recreated under the same namespace and name. It could also drop that successor's pending pass,
because the dirty entry is keyed by name.

So a queued deletion carries the incarnation it is FOR, resolved against the declare record when
it is queued rather than taken from the caller:

- a NotFound cleanup means whichever incarnation is on record at that instant;
- a deletion naming a UID the record has already replaced is stale on arrival: it is queued, but it
  clears neither the declare record nor the dirty entry;
- at apply time it is dropped if either record of the current incarnation — the declare record, or
  the watch-plane UID, which settle at different times — disagrees with it.

### Persistent failure surfaces as `WatchPlanFailing`

Open question 1, answered. `DeclareStatusForGitTarget` publishes whether a declaration has ever
landed, how many consecutive passes have failed, and the last error. The GitTarget controller
projects that as Ready=False / Reconciling=True with reason `WatchPlanFailing`, carrying the
failure count and the message.

It is deliberately a PROGRESSING reason rather than a stall: a pass fails on a source cluster that
is unreachable or a catalog that is not observable yet, and both recover on their own. What it buys
is that such a target does not read as idle. It does not touch `StreamsRunning`, which keeps its
meaning — "the streams are running" — rather than being overloaded with "the plan is fresh".

The distinction that mattered in practice: **pending is "no pass has ever landed", not "the target
is dirty right now"**. Being dirty is the steady state of a system that replans on every rule edit
and sweeps every 30s, and reporting it as unsettled would flap Ready on a healthy mirror. For the
same reason a re-declare that changes nothing and has already landed is a no-op — a GitTarget
reconcile is level-triggered and re-declares identical values on every steady requeue.

### Step 3 needed no type-to-target index

Open question 3, answered: it does not earn its staleness. The shared refresh renders each target's
watch plan to a fingerprint before and after the re-projection and marks dirty only the targets
whose plan actually moved (`watchPlanFingerprint`, `markInvalidatedTargets`). A CRD that appears in
a cluster no rule selects changes no plan and therefore dirties nothing — the property the index was
for — derived from the tables rather than kept beside them.

### Still open

**Does the settle window ever need to be configurable?** It is fixed on purpose. If a large
installation finds 2s wrong, the question is whether it becomes a flag or whether the max wait
absorbs it. Nothing has asked yet.
