# Event Pipeline Design: From Cluster to Git Commit

## Status

**Fully implemented and verified** as of March 2026. All items in the original
proposal are shipped. Two follow-on bugs discovered during e2e testing have also
been fixed — see [§10 Bug Fixes](#10-bug-fixes-discovered-during-implementation).

---

## 1. How Kubernetes Informers Work

Before describing our design, it helps to understand what the Kubernetes informer
framework actually does when you start watching a resource type, because it has a
non-obvious property that is central to this design.

### 1.1 Starting a watch: LIST first, then WATCH

When a client-go informer starts it does **not** open a raw watch stream
immediately. It follows a two-phase protocol:

```
Phase 1 — LIST
  GET /apis/v1/secrets?limit=500
  → returns a snapshot of all currently existing secrets
  → response includes a resourceVersion (e.g. "42819")

Phase 2 — WATCH (starts from the LIST's resourceVersion)
  GET /apis/v1/secrets?watch=true&resourceVersion=42819
  → API server streams only events that happened AFTER rv 42819
```

The informer delivers **all items from the LIST as synthetic ADDED events** to
every registered event handler before it transitions to the streaming phase. This
means:

> **Starting an informer on a namespace that already contains 50 secrets will
> cause 50 ADDED events to fire, even though those secrets are not new.**

This is the root cause of the "N individual `[CREATE]` commits" problem this
design solves.

### 1.2 `HasSynced()` — the clean boundary between LIST and WATCH

`cache.WaitForCacheSync(stopCh, informer.HasSynced)` blocks until the informer
reports that its initial LIST has been fully processed. Concretely:

1. The informer's reflector fetches the LIST response.
2. Every item is pushed through the internal `DeltaFIFO` queue.
3. All registered event handlers are called for each item.
4. Only then does `HasSynced()` return `true`.

After `HasSynced()` returns `true`:

- All ADDED events from the initial LIST have already been delivered.
- The informer is now in the WATCH phase, streaming events with
  `resourceVersion > LIST_rv`.
- Any resource that was created **after** the LIST's `resourceVersion` will
  arrive as a fresh ADDED event through the WATCH stream, **not** as part of
  the initial sync burst.

### 1.3 The "constant stream" question — can you tell initial from real?

A common concern: if resources are being created continuously, how does
`HasSynced()` know when the initial burst ends? Could a resource created one
millisecond before the LIST be indistinguishable from one created one millisecond
after?

The answer is: **yes, they are cleanly distinguishable, because of
`resourceVersion`**.

The API server assigns a monotonically increasing `resourceVersion` to every
write. The LIST response captures a consistent snapshot at a specific
`resourceVersion`. The WATCH starts from the next one. There is no ambiguity:

```
rv 42810  secret "old-secret" created    ← appears in LIST (ADDED during sync)
rv 42819  LIST captured at this point    ← HasSynced boundary
rv 42820  secret "new-secret" created    ← appears in WATCH (ADDED post-sync)
```

`cache.WaitForCacheSync` returns after all items up to rv 42819 have been
delivered. The ADDED event for "new-secret" (rv 42820) will only arrive through
the live WATCH stream, after `HasSynced` has already returned `true`.

This property is what makes it safe to call `OnReconciliationComplete()` right
after `WaitForCacheSync` — any resource created after the LIST boundary is
guaranteed to arrive through the live WATCH stream (in LIVE_PROCESSING state),
not to be silently swallowed in the RECONCILING buffer.

---

## 2. Component Responsibilities

Each component in the pipeline has a single, well-bounded responsibility. No
component reaches across its boundary.

### `WatchManager` ([`internal/watch/manager.go`](../internal/watch/manager.go))

**Responsibility:** Translate Kubernetes cluster state into a stream of
`git.Event` values, one per changed resource.

- Owns informer lifecycle (start, stop, namespace scoping).
- Deduplicates events by content hash — status-only changes that produce no
  meaningful YAML diff are dropped before they reach the queue.
- Does **not** know about Git commits, branches, or reconcile batches.
- On a WatchRule change, orchestrates the `BeginReconciliation` → informer
  start → snapshot → `CompleteReconciliation` sequence (§6).
- Calls `GetClusterStateForGitDest` via the EventRouter for snapshots; this
  uses direct API List calls (not the informer cache) and is namespace-scoped
  per WatchRule.

### `EventRouter` ([`internal/watch/event_router.go`](../internal/watch/event_router.go))

**Responsibility:** Dispatch events and control signals to the right recipient
without any business logic.

- Routes live `git.Event` values from WatchManager to the correct
  `GitTargetEventStream` via `RouteToGitTargetEventStream`.
- Routes control events (`RequestClusterState`, `RequestRepoState`) to
  WatchManager and BranchWorker services respectively.
- Owns the registry of `GitTargetEventStream` instances (by GitTarget key).
- Exposes `BeginReconciliationForStream` and `CompleteReconciliationForStream`
  so WatchManager can drive stream state transitions without depending directly
  on `GitTargetEventStream`.
- Does **not** inspect event content or make routing decisions based on
  resource type.

### `GitTargetEventStream` ([`internal/reconcile/git_target_event_stream.go`](../internal/reconcile/git_target_event_stream.go))

**Responsibility:** Enforce ordering between reconcile batches and live events
for one GitTarget, and deduplicate redundant live events.

- Implements the `RECONCILING` / `LIVE_PROCESSING` state machine (§4).
- In `RECONCILING`: buffers live events so they cannot interleave with a
  batch that is being written.
- In `LIVE_PROCESSING`: checks a per-resource event hash before forwarding;
  events whose content has not changed since the last processed event are
  discarded.
- Forwards batches (`EmitReconcileBatch`) and individual events (`OnWatchEvent`)
  to the `BranchWorker` as `WorkItem` values.
- Does **not** know what the events represent at the domain level (secrets,
  deployments, etc.).

### `FolderReconciler` ([`internal/reconcile/folder_reconciler.go`](../internal/reconcile/folder_reconciler.go))

**Responsibility:** Compute the diff between cluster state and Git state and
emit exactly one `ReconcileBatch` describing what must change.

- Holds the last known cluster resource list and Git resource list.
- Updated via `OnClusterState` and `OnRepoState`; calls `reconcile()` on each
  update.
- `reconcile()` computes `toCreate`, `toDelete`, `existingInBoth` and packages
  them into a single `ReconcileBatch` with a human-readable commit message.
- Returns early (no emit) when the diff is empty — cluster and Git already
  agree.
- Does **not** start informers, make API calls, or interact with Git directly.

### `BranchWorker` ([`internal/git/branch_worker.go`](../internal/git/branch_worker.go))

**Responsibility:** Serialise all writes to one Git branch (one per provider ×
branch combination) and push atomically.

- Accepts `WorkItem` values from its queue: either a `Single` (one live event)
  or a `Batch` (a complete reconcile batch).
- For `Single`: buffers events and flushes them (one commit per event) based on
  count, size, or a periodic tick.
- For `Batch`: flushes any live buffer first (preserving ordering), then writes
  all files in the batch and creates exactly **one** commit with the batch's
  commit message.
- Does **not** decide what to write or when — that decision was made upstream.

### `GitTargetController` ([`internal/controller/gittarget_controller.go`](../internal/controller/gittarget_controller.go))

**Responsibility:** Drive the GitTarget startup lifecycle through a set of
ordered gates, and keep the `GitTarget` status conditions accurate.

- Evaluates gates in order: Validated → EncryptionConfigured → Bootstrapped →
  SnapshotSynced → EventStreamLive.
- `evaluateSnapshotGate` runs the initial cluster snapshot **once**, guarded by
  `SnapshotSynced=True`. It never re-runs the snapshot for unrelated triggers
  (e.g. Flux touching the encryption secret).
- `evaluateEventStreamGate` calls `OnReconciliationComplete()` if the stream is
  still in `RECONCILING` after the snapshot lands.
- Does **not** start informers, list resources, or make Git commits.

---

## 3. `git.Event` and Batch Types

**File:** [`internal/git/types.go`](../internal/git/types.go)

```go
type Event struct {
    Object     *unstructured.Unstructured   // nil for DELETE
    Identifier types.ResourceIdentifier
    Operation  string                       // CREATE | UPDATE | DELETE | RECONCILE_RESOURCE
    UserInfo   UserInfo
    Path       string                       // folder prefix from owning GitTarget
    GitTargetName      string               // set by GitTargetEventStream
    GitTargetNamespace string
}

// ReconcileBatch carries all file changes from one reconcile run as a single unit.
// It arrives at BranchWorker as one WorkItem and is always committed together.
type ReconcileBatch struct {
    Events             []Event
    CommitMessage      string               // e.g. "reconcile: sync 42 resources from cluster snapshot"
    GitTargetName      string
    GitTargetNamespace string
}

// WorkItem is the unit of work in the BranchWorker queue.
// Exactly one of Single or Batch is non-nil.
type WorkItem struct {
    Single *Event
    Batch  *ReconcileBatch
}
```

`Event` is unchanged from the original design. `ReconcileBatch` and `WorkItem`
were added alongside it.

---

## 4. State Machine: `RECONCILING` and `LIVE_PROCESSING`

**File:** [`internal/reconcile/git_target_event_stream.go`](../internal/reconcile/git_target_event_stream.go)

```
              ┌──────────────────────────────┐
  (created)   │                              │  BeginReconciliation()
────────────► │       RECONCILING            │ ◄────────────────────────
              │                              │   (from LIVE_PROCESSING)
              │  live events: buffered        │
              │  ReconcileBatch: forwarded   │
              └──────────────┬───────────────┘
                             │
                             │  OnReconciliationComplete()
                             │  flush buffered live events
                             ▼
              ┌──────────────────────────────┐
              │       LIVE_PROCESSING         │
              │                              │
              │  live events: dedup+forward  │
              └──────────────────────────────┘
```

Every `GitTargetEventStream` starts in `RECONCILING` (the controller has not
yet confirmed that the initial snapshot is complete). After
`OnReconciliationComplete()` it enters `LIVE_PROCESSING`. It can re-enter
`RECONCILING` when a planned re-reconcile (WatchRule change, controller restart)
is triggered.

Flushed events pass through the same dedup path as live events. If the
reconcile batch already wrote the same content, the BranchWorker detects no file
change and produces no additional commit.

---

## 5. Full Event Pipeline

```
┌───────────────────────────────────────────────────────────────┐
│ Event producers                                               │
│                                                               │
│  A) WatchManager (live)          B) FolderReconciler          │
│     cluster change detected         reconcile() computes diff │
│     → git.Event                     → ONE ReconcileBatch      │
└──────────────────┬──────────────────────────────┬────────────┘
                   │ OnWatchEvent(event)           │ EmitReconcileBatch(batch)
                   ▼                              ▼
        ┌────────────────────────────────────────────────────┐
        │  GitTargetEventStream (per GitTarget)               │
        │                                                    │
        │  RECONCILING:                                      │
        │    live events  → buffered []Event                 │
        │    ReconcileBatch → WorkItem{Batch} → worker       │
        │                                                    │
        │  LIVE_PROCESSING:                                  │
        │    live events  → dedup → WorkItem{Single} → worker│
        │                                                    │
        │  BeginReconciliation()  →  RECONCILING             │
        │  OnReconciliationComplete() → LIVE_PROCESSING      │
        │    + flush buffered live events as WorkItem{Single} │
        └──────────────────────┬─────────────────────────────┘
                               │ chan WorkItem
                               ▼
                   ┌───────────────────────────┐
                   │  BranchWorker              │
                   │  chan WorkItem (cap 100)   │
                   │                           │
                   │  WorkItem{Single}:         │
                   │    buffer → flush on tick  │
                   │    → one commit per event  │
                   │                           │
                   │  WorkItem{Batch}:          │
                   │    flush live buffer first │
                   │    → generateBatchCommit() │
                   │    → ONE commit            │
                   └──────────┬────────────────┘
                              │ PushAtomic (3 retries)
                              ▼
                         remote Git
```

---

## 6. WatchRule Addition — Grouping the Initial Sync Burst

This is where §1 directly shapes the implementation. When a new `WatchRule` is
applied, the controller must start watching a resource type that may already
have many existing objects. Those objects will fire as ADDED events (§1.1), and
we must group them into one commit rather than N individual `[CREATE]` commits.

`ReconcileForRuleChange` in [`internal/watch/manager.go`](../internal/watch/manager.go)
follows this exact sequence:

```
Step 1 — beginReconciliationForAffectedTargets()
    BeginReconciliation() on each affected GitTargetEventStream.
    Stream enters RECONCILING. Any ADDED event that arrives now is buffered,
    not forwarded to the BranchWorker.

Step 2 — startInformersForGVRs()
    Starts a dynamic informer for each new GVR.
    Calls cache.WaitForCacheSync() — BLOCKS until HasSynced() is true.

    Because of §1.2: by the time WaitForCacheSync returns, every resource
    that existed at LIST time has fired its ADDED event and been buffered
    in the stream. No initial-sync ADDED event can arrive after this point.

Step 3 — clearDeduplicationCacheForGVRs()
    Clears content-hash entries for changed GVRs to prevent false positives.

Step 4 — emitSnapshotForRuleChange()
    Emits RequestClusterState → GetClusterStateForGitDest (direct API List,
    namespace-scoped per §7) → FolderReconciler.reconcile() → EmitReconcileBatch.
    ONE "reconcile: sync N resources from cluster snapshot" commit is produced.

Step 5 — completeReconciliationForAffectedTargets()
    OnReconciliationComplete() on each stream.
    Stream enters LIVE_PROCESSING.
    The buffered ADDED events from step 2 are flushed. The BranchWorker finds
    the files already present (written by the batch in step 4) and produces
    no additional commits.
```

### Why call `OnReconciliationComplete()` here and not later?

The GitTarget controller's normal reconcile loop also calls
`OnReconciliationComplete()` (in `evaluateEventStreamGate`). However, once the
GitTarget is in steady state (Ready=True), its next scheduled reconcile is
`RequeueLongInterval = 10 minutes` away.

If we left the stream in `RECONCILING` for up to 10 minutes, every real cluster
event during that window would be silently buffered and then replayed as a
single delayed burst — breaking the per-event commit invariant for live changes.

Calling `OnReconciliationComplete()` in step 5 is safe because
`cache.WaitForCacheSync` (step 2) guarantees that **no initial-sync ADDED event
can arrive after step 5**. Any resource created after the LIST boundary (§1.3)
is a real new resource and will arrive via the live WATCH stream after the
stream has returned to LIVE_PROCESSING — exactly the correct behaviour.

---

## 7. Namespace Scoping in `GetClusterStateForGitDest`

**File:** [`internal/watch/manager.go`](../internal/watch/manager.go) —
`GetClusterStateForGitDest` and `listResourcesForGVR`

The snapshot uses direct API `List` calls, not the informer cache. Namespace
scope is derived from the WatchRules that match the GitTarget:

| Rule type | List scope |
|---|---|
| `WatchRule` | `dc.Resource(gvr).Namespace(rule.Source.Namespace).List(...)` |
| `ClusterWatchRule` | `dc.Resource(gvr).List(...)` (cluster-wide) |

Multiple WatchRules in different namespaces targeting the same GitTarget are
each listed separately and merged. A `WatchRule` in `test-ns` will never cause
secrets from `cert-manager` or `flux-system` to appear in the snapshot.

---

## 8. Who Drives State Transitions

| Actor | Transition | When |
|---|---|---|
| `GitTargetController.evaluateSnapshotGate` | `BeginReconciliation()` | Initial bootstrap only (guard: `SnapshotSynced≠True`) |
| `GitTargetController.evaluateEventStreamGate` | `OnReconciliationComplete()` | After bootstrap batch lands |
| `WatchManager.beginReconciliationForAffectedTargets` | `BeginReconciliation()` | Before new informers start on a WatchRule change |
| `WatchManager.completeReconciliationForAffectedTargets` | `OnReconciliationComplete()` | After the rule-change snapshot batch is emitted |

The `GitTargetController` no longer re-runs the snapshot on every reconcile
loop. `evaluateSnapshotGate` exits immediately once `SnapshotSynced=True` is
set, preventing spurious re-snapshots from unrelated events.

---

## 9. Git History Examples

### Initial bootstrap or WatchRule addition (snapshot path)

```
abc1234  reconcile: sync 42 resources from cluster snapshot
```

### Single resource change (live path — unchanged)

```
abc1235  [UPDATE] apps/v1/deployments/default/frontend  (alice)
```

### No-op (cluster already matches Git)

No commit is produced. `FolderReconciler.reconcile()` returns early when
`len(toCreate) + len(toDelete) + len(existingInBoth) == 0`.

---

## 10. Bug Fixes Discovered During Implementation

Two bugs were found and fixed during e2e testing. The full analysis is in
[`docs/done/`](done/):

### Bug 1 — Snapshot namespace scoping ([`docs/done/bug-snapshot-namespace-scoping.md`](done/bug-snapshot-namespace-scoping.md))

`GetClusterStateForGitDest` was listing resources cluster-wide. A `WatchRule`
in a test namespace pulled secrets from `cert-manager`, `flux-system`, and
`kube-system` into the snapshot, producing a spurious
`reconcile: sync 27 resources` commit.

**Fix:** Thread a `map[GVR][]string` (GVR → namespaces) through
`GetClusterStateForGitDest` and use `dc.Resource(gvr).Namespace(ns).List(...)`
per namespace (§7).

### Bug 2 — Snapshot re-ran on every GitTarget reconcile ([`docs/done/bug-snapshot-namespace-scoping.md`](done/bug-snapshot-namespace-scoping.md))

`evaluateSnapshotGate` called `StartReconciliation` unconditionally. When Flux
applied a committed secret back to the cluster, `encryptionSecretToGitTargets`
triggered a GitTarget re-reconcile → snapshot re-ran → spurious commit.

**Fix:** Early-exit guard in `evaluateSnapshotGate`:

```go
if meta.IsStatusConditionTrue(target.Status.Conditions, GitTargetConditionSnapshotSynced) {
    return nil, metav1.ConditionTrue, MsgSnapshotCompleted, 0, nil
}
```

### Bug 3 — WatchRule addition produced N individual commits ([`docs/done/followup-watchrule-addition-should-snapshot.md`](done/followup-watchrule-addition-should-snapshot.md))

The old `seedSelectedResources` goroutine emitted individual `[CREATE]` events
via the live path (N commits). Additionally, `OnReconciliationComplete()` was
only called from the controller's scheduled loop (up to 10 minutes later),
leaving the stream stuck in `RECONCILING` and silently buffering real cluster
changes.

**Fix:** Replace `seedSelectedResources` with the sequence in §6, ending with
`completeReconciliationForAffectedTargets()` to immediately return the stream
to `LIVE_PROCESSING` after the snapshot batch lands.
