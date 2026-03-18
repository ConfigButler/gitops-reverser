# Event Pipeline Design: From Cluster to Git Commit

## Overview

This document analyses the current event flow from Kubernetes cluster through to
Git commits, identifies the problem with reconcile events producing many individual
commits, and proposes a design that:

- produces a **single Git commit** for an entire reconcile run
- **buffers live events** while a reconcile is running (per GitTarget, not globally)
- keeps the existing per-event commit behaviour for live cluster changes unchanged
- allows the state machine to re-enter `RECONCILING` for planned re-reconciles

---

## 1. Current `git.Event` Interface

**File:** `internal/git/types.go`

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
```

### What is missing today

There is **no way for the BranchWorker or `generateCommitsFromEvents` to tell a
reconcile event apart from a live event**.  `generateCommitsFromEvents`
([internal/git/git.go:715](../internal/git/git.go#L715)) creates **one commit per
event**.  A cluster with 500 resources on first install produces 500 commits.

A second gap: the state machine in `GitTargetEventStream` enters `StartupReconcile`
exactly once (at construction).  After `OnReconciliationComplete()` fires it never
returns to the buffering state.  A planned re-reconcile (provider URL change,
selector change, etc.) would therefore not buffer live events and could interleave
live commits with reconcile writes.

---

## 2. Full Event Pipeline (current)

```
┌──────────────────────────────────────────────────────────────┐
│ Event producers                                              │
│                                                              │
│  A) WatchManager (live)         B) FolderReconciler          │
│     cluster change detected        reconcile() computes diff │
│     → dedup by content hash        → N × EmitCreateEvent /  │
│     → git.Event{Op:…}                EmitDeleteEvent /      │
│                                      EmitReconcileResource   │
└─────────────────────┬─────────────────────────┬─────────────┘
                      │ (live path)              │ (reconcile path, N events)
                      ▼                          ▼
            ┌──────────────────────────────────────────────┐
            │  GitTargetEventStream (per GitTarget)         │
            │                                              │
            │  STARTUP_RECONCILE: buffer live events        │
            │                     forward reconcile events  │
            │  LIVE_PROCESSING:   dedup + forward live      │
            └──────────────────┬───────────────────────────┘
                               │ Enqueue(event)  ← N separate calls
                               ▼
                   ┌───────────────────────┐
                   │  BranchWorker          │
                   │  chan Event (cap 100)  │
                   │                       │
                   │  flush triggers:      │
                   │  ≥20 events / ≥1 MiB │  ← batch may be SPLIT here
                   │  1 min ticker         │
                   └──────────┬────────────┘
                              │
                   ┌──────────▼────────────────────┐
                   │ generateCommitsFromEvents()    │
                   │ for each event:               │
                   │   applyEventToWorktree()      │
                   │   createCommitForEvent()  ← one commit each
                   └───────────────────────────────┘
```

---

## 3. Proposed Design

### 3.1 Replace N individual reconcile events with one `ReconcileBatch`

`FolderReconciler.reconcile()` already has **all events in memory at the same time**
when the diff is computed.  Instead of emitting N individual events one by one, it
emits a single `ReconcileBatch` value that carries all of them.

```go
// ReconcileBatch is a complete set of file changes from one reconcile run.
// It arrives at the BranchWorker as a single unit and is always committed together.
type ReconcileBatch struct {
    // Events is the complete ordered list of file changes for this reconcile.
    Events []Event

    // CommitMessage is the summary commit message, e.g.:
    // "reconcile: sync 312 resources from cluster snapshot"
    CommitMessage string

    // GitTargetName / GitTargetNamespace identify the owning GitTarget.
    GitTargetName      string
    GitTargetNamespace string
}
```

The `EventEmitter` interface used by `FolderReconciler` changes from three
per-resource methods to a single batch method:

```go
// Before (three separate calls, each producing one Event):
type EventEmitter interface {
    EmitCreateEvent(resource, obj) error
    EmitDeleteEvent(resource) error
    EmitReconcileResourceEvent(resource, obj) error
}

// After (one call with the complete diff):
type ReconcileEmitter interface {
    EmitReconcileBatch(batch ReconcileBatch) error
}
```

`FolderReconciler.reconcile()` builds the full event slice from the diff, wraps it
in `ReconcileBatch`, and calls `EmitReconcileBatch` once.

### 3.2 BranchWorker queue accepts both singles and batches

The queue type changes from `chan Event` to `chan WorkItem`, where `WorkItem` is a
small discriminated-union struct:

```go
// WorkItem is either a single live event or a complete reconcile batch.
// Exactly one field is non-nil.
type WorkItem struct {
    Single *Event
    Batch  *ReconcileBatch
}
```

This does not touch the `Event` type at all and keeps the channel strongly typed.
The BranchWorker processes items in arrival order.  When it encounters a `Batch`, it
writes all files without committing, then creates a single commit.  When it
encounters a `Single`, it writes the file and commits immediately (existing
behaviour).

```
BranchWorker receives WorkItem:

  if item.Single != nil:
      applyEventToWorktree(event)
      createCommitForEvent(event)       ← unchanged, one commit per live event

  if item.Batch != nil:
      for each event in batch.Events:
          applyEventToWorktree(event)   ← write file only, no commit
      createCommitForBatch(batch)       ← ONE commit for the whole batch
```

Because the entire batch arrives as one `WorkItem`, **there is no flush-tick
splitting problem**.  The BranchWorker does not need to know anything about
reconcile group IDs or counts; it simply processes the item it received.

### 3.3 State machine: `RECONCILING` and `LIVE_PROCESSING`

Rename `StartupReconcile` → `RECONCILING`.  The key addition is that the state
machine can **re-enter `RECONCILING`** from `LIVE_PROCESSING` whenever a new
reconcile is triggered.

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

**`BeginReconciliation()`** is a new method on `GitTargetEventStream`.  It is
called by `GitTargetController` whenever it decides a reconcile is needed (new
target, selector change, provider URL change, etc.).  If the stream is already in
`RECONCILING` the call is a no-op.

**`OnReconciliationComplete()`** works as today: transitions to `LIVE_PROCESSING`
and flushes buffered live events.

Live events buffered during `RECONCILING` still pass through the normal dedup path
when they are flushed — so if the reconcile batch already wrote the same content,
the live event will produce no further commit (the "no-op file write" check in
`handleCreateOrUpdateOperation` takes care of this).

### 3.4 What happens to other GitTargets on the same branch

Each `GitTargetEventStream` has its own independent state machine.  Only the stream
whose reconcile is running enters `RECONCILING`.  Other GitTargets sharing the same
`BranchWorker` continue to enqueue `WorkItem{Single: &event}` items normally.

The BranchWorker serialises all work regardless of source, so a live commit from
GitTarget B may land before or after the reconcile batch from GitTarget A — this is
fine and already the correct semantics.

---

## 4. Updated Event and Batch Interfaces

### `internal/git/types.go` additions

```go
// ReconcileBatch is a complete set of file changes from one reconcile run.
// It is enqueued as a single WorkItem so the BranchWorker always commits it atomically.
type ReconcileBatch struct {
    Events        []Event
    CommitMessage string
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

The `Event` struct itself is **unchanged**.  No batch-related fields are added to it.

### `internal/reconcile/git_target_event_stream.go` changes

```go
// Interface used by FolderReconciler — replaces the three per-resource methods.
type ReconcileEmitter interface {
    EmitReconcileBatch(batch git.ReconcileBatch) error
}

// GitTargetEventStream new/changed public methods:

// BeginReconciliation transitions to RECONCILING (buffering live events).
// Safe to call when already RECONCILING (no-op).
func (s *GitTargetEventStream) BeginReconciliation()

// EmitReconcileBatch forwards a complete reconcile batch to the BranchWorker
// as a single WorkItem. Called while in RECONCILING state.
func (s *GitTargetEventStream) EmitReconcileBatch(batch git.ReconcileBatch) error

// OnReconciliationComplete transitions to LIVE_PROCESSING and flushes buffered live events.
func (s *GitTargetEventStream) OnReconciliationComplete()

// OnWatchEvent handles incoming live events from WatchManager (unchanged signature).
func (s *GitTargetEventStream) OnWatchEvent(event git.Event)
```

### `internal/reconcile/folder_reconciler.go` changes

`FolderReconciler` depends on `ReconcileEmitter` instead of the old `EventEmitter`.
`reconcile()` assembles the full event list and calls `EmitReconcileBatch` once:

```go
func (r *FolderReconciler) reconcile() {
    if !r.clusterStateSeen || !r.gitStateSeen { return }

    toCreate, toDelete, existingInBoth := r.findDifferences(...)

    var events []git.Event
    for _, res := range toCreate {
        events = append(events, git.Event{Op: "CREATE", Identifier: res, Object: r.objectFor(res),
            UserInfo: git.UserInfo{Username: "gitops-reverser"}, Path: r.path})
    }
    for _, res := range toDelete {
        events = append(events, git.Event{Op: "DELETE", Identifier: res,
            UserInfo: git.UserInfo{Username: "gitops-reverser"}, Path: r.path})
    }
    for _, res := range existingInBoth {
        events = append(events, git.Event{Op: "RECONCILE_RESOURCE", Identifier: res,
            Object: r.objectFor(res), UserInfo: git.UserInfo{Username: "gitops-reverser"},
            Path: r.path})
    }

    total := len(events)
    if total == 0 { return }   // nothing to do; cluster and git already match

    batch := git.ReconcileBatch{
        Events:        events,
        CommitMessage: fmt.Sprintf("reconcile: sync %d resources from cluster snapshot", total),
    }
    r.reconcileEmitter.EmitReconcileBatch(batch)
}
```

### `internal/git/branch_worker.go` changes

```go
eventQueue chan WorkItem   // was: chan Event
```

`processEvents` loop handles `WorkItem` instead of `Event`:

```go
case item := <-w.eventQueue:
    if item.Single != nil {
        w.eventBuffer = append(w.eventBuffer, *item.Single)
        // existing count/size flush logic unchanged
    }
    if item.Batch != nil {
        // Flush any buffered live events first (preserve ordering)
        if len(w.eventBuffer) > 0 {
            w.commitAndPush(w.eventBuffer)
            w.eventBuffer = nil
        }
        // Process the entire batch as a unit
        w.commitAndPushBatch(item.Batch)
    }
```

### `internal/git/git.go` changes

Add `WriteEventsAsBatch` (or a flag parameter) that writes all files and creates
exactly one commit:

```go
func generateBatchCommit(
    ctx context.Context,
    writer eventContentWriter,
    repo *git.Repository,
    batch *ReconcileBatch,
) (plumbing.Hash, error) {
    worktree, _ := repo.Worktree()
    for _, event := range batch.Events {
        applyEventToWorktree(ctx, writer, worktree, event)  // write files, no commit
    }
    return worktree.Commit(batch.CommitMessage, &git.CommitOptions{
        Author: &object.Signature{
            Name:  "gitops-reverser",
            Email: "noreply@configbutler.ai",
            When:  time.Now(),
        },
        Committer: &object.Signature{
            Name:  "gitops-reverser",
            Email: "noreply@configbutler.ai",
            When:  time.Now(),
        },
    })
}
```

---

## 5. Full Pipeline (proposed)

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

## 6. Desired Git History

### Today (500 resources, fresh install)

```
abc1234  [CREATE] apps/v1/deployments/default/frontend
abc1235  [CREATE] apps/v1/deployments/default/backend
... (498 more)
```

### After proposed change

```
abc1234  reconcile: sync 500 resources from cluster snapshot
```

Subsequent live changes look identical to today:

```
abc1235  [UPDATE] apps/v1/deployments/default/frontend  (alice)
```

A second planned reconcile (e.g. GitTarget selector updated):

```
abc1236  reconcile: sync 498 resources from cluster snapshot
```

---

## 7. Who Creates and Synchronises Changes

| Actor | Role | Emits |
|---|---|---|
| `WatchManager` | Watches all cluster resources; deduplicates by content hash | `WorkItem{Single: &event}` |
| `FolderReconciler` | Diffs cluster vs Git; assembles full batch | `ReconcileBatch` via `ReconcileEmitter` |
| `GitTargetEventStream` | State machine per GitTarget; routes and buffers | `WorkItem{Single}` or `WorkItem{Batch}` into `BranchWorker` queue |
| `GitTargetController` | Drives lifecycle; calls `BeginReconciliation()` / `OnReconciliationComplete()` | state transitions only |
| `BranchWorker` | One per (provider × branch); serialises all writes | commits + push |

---

## 8. Implementation Actions

Changes are ordered by dependency — each step compiles and tests cleanly on its own.

### Step 1 — `internal/git/types.go`: add `ReconcileBatch` and `WorkItem`

Add the two new types.  `Event` is unchanged.

```go
type ReconcileBatch struct {
    Events             []Event
    CommitMessage      string
    GitTargetName      string
    GitTargetNamespace string
}

type WorkItem struct {
    Single *Event
    Batch  *ReconcileBatch
}
```

### Step 2 — `internal/git/git.go`: add `generateBatchCommit`

Add a new function alongside the existing `generateCommitsFromEvents`.  It applies
all file changes in a `ReconcileBatch` without committing after each one, then
creates a single commit with the batch message.  The existing function is unchanged.

### Step 3 — `internal/git/branch_worker.go`: switch queue to `chan WorkItem`

- Change `eventQueue chan Event` → `eventQueue chan WorkItem`.
- Update `Enqueue(event Event)` → wraps the event in `WorkItem{Single: &event}`.
- Add `EnqueueBatch(batch *ReconcileBatch)` → sends `WorkItem{Batch: batch}`.
- Update `processEvents`: when `item.Batch != nil`, flush the live buffer first,
  then call `commitAndPushBatch` which drives `generateBatchCommit`.

### Step 4 — `internal/reconcile/git_target_event_stream.go`: state machine + batch

- Rename constant `StartupReconcile` → `Reconciling`.
- Add `BeginReconciliation()` method: transitions `LiveProcessing` → `Reconciling`
  (no-op if already `Reconciling`).
- Replace `EventEnqueuer` interface (`Enqueue`) with one that also exposes
  `EnqueueBatch`.
- Add `EmitReconcileBatch(batch git.ReconcileBatch) error`: stamps
  `GitTargetName`/`GitTargetNamespace` onto the batch, calls `EnqueueBatch`.
- Remove `EmitCreateEvent`, `EmitDeleteEvent`, `EmitReconcileResourceEvent` — these
  are replaced by `EmitReconcileBatch`.

### Step 5 — `internal/reconcile/folder_reconciler.go`: emit one batch

- Replace dependency on `EventEmitter` with `ReconcileEmitter`
  (`EmitReconcileBatch`).
- Rewrite `reconcile()` to collect all `toCreate`, `toDelete`, `existingInBoth`
  events into a single `[]git.Event`, set `UserInfo{Username: "gitops-reverser"}`
  on each, and call `EmitReconcileBatch` once.  Return early (no emit) when the
  slice is empty.

### Step 6 — `internal/controller/gittarget_controller.go`: drive state transitions

- Call `stream.BeginReconciliation()` at the start of every reconcile pass that
  determines a snapshot sync is needed (i.e. before starting `FolderReconciler`).
- Ensure `stream.OnReconciliationComplete()` is still called after
  `FolderReconciler` emits its batch — this flushes buffered live events and
  transitions to `LiveProcessing`.

### Step 7 — Update tests

- Unit tests for `FolderReconciler`: expect one `EmitReconcileBatch` call instead of
  N individual emit calls.
- Unit tests for `GitTargetEventStream`: add coverage for `BeginReconciliation()`
  re-entry from `LiveProcessing`.
- Integration tests: assert that a reconcile run with N resources produces exactly
  one Git commit with message matching `reconcile: sync N resources…`.
