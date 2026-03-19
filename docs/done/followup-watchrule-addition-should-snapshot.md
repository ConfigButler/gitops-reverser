# Follow-up: WatchRule Addition Should Trigger a Snapshot, Not Individual Live Events

**Status:** Open
**Depends on:** `bug-snapshot-namespace-scoping.md` (both bugs now fixed)
**Discovered via:** `make test-e2e-bi` — the git log after the fix still shows individual `[CREATE]` commits for secrets instead of one `reconcile: sync N resources…` commit.

---

## Problem

When a `WatchRule` is applied for the first time (or after a controller restart), the controller sets up namespace-scoped informers for the watched namespace. Those informers immediately fire `ADDED` events for every resource that already exists in that namespace. Each event travels the **live event path** and produces its own commit:

```
[CREATE] v1/secrets/bi-controller-sops-<id>
[CREATE] v1/secrets/git-creds-ssh-<run>
[CREATE] v1/secrets/git-creds-invalid-<run>
[CREATE] v1/secrets/git-creds-<run>
[CREATE] v1/secrets/sops-age-key
```

The desired outcome is a single commit that captures the entire initial state of the watched namespace:

```
reconcile: sync 5 resources from cluster snapshot
```

This is the same atomic, auditable commit that the snapshot path already produces for the very first `GitTarget` bootstrap. A `WatchRule` addition is semantically identical: the watch scope just grew, and the new resources should be reconciled as a single unit.

---

## Why It Happens

There are two ways the controller can catch up on existing resources when a new `WatchRule` arrives:

| Path | How it works | Commit output |
|---|---|---|
| **Live event path (current)** | Informer `ADDED` events fire one-by-one as the cache syncs | N individual `[CREATE]` commits |
| **Snapshot path (desired)** | `RequestClusterState` → `GetClusterStateForGitDest` → `ReconcileBatch` | 1 `reconcile: sync N resources…` commit |

The snapshot path already exists and already scopes correctly to the WatchRule's namespace (fixed in `bug-snapshot-namespace-scoping.md`). It just isn't being triggered on rule changes.

The live-event informers **cannot** be the right mechanism for initial population because:
1. The git history becomes unreadable — you can't tell from the log when the watch scope changed vs when a real cluster change happened.
2. If 50 secrets exist when the WatchRule is created, you get 50 commits. There is no way to correlate them back to "WatchRule X was added".
3. Once ADDED events have fired and the informer cache is warm, **any subsequent snapshot will produce a zero-diff** because the live commits already wrote all the files. So the snapshot path is effectively bypassed.

---

## Root Cause

`ReconcileForRuleChange` in `internal/watch/manager.go` reconciles the **informers** (starts/stops watchers) but does not emit a `RequestClusterState` control event for the affected `GitTarget`. The snapshot is therefore never triggered by a rule change.

The `evaluateSnapshotGate` fix (Bug 2 in `bug-snapshot-namespace-scoping.md`) added an early-exit guard so that the snapshot does not re-run on every unrelated `GitTarget` reconcile. The complementary change — triggering the snapshot from the *right* place — was not yet implemented.

---

## Fix Plan

### Step 1 — Emit `RequestClusterState` from `ReconcileForRuleChange`

After `ReconcileForRuleChange` has reconciled the informers, it should emit `RequestClusterState` for every `GitTarget` that is affected by the changed rule set. The affected targets are already known from the RuleStore.

```go
// internal/watch/manager.go — ReconcileForRuleChange (after informer reconcile)

affectedTargets := m.collectAffectedGitTargets()
for _, gitDest := range affectedTargets {
    if err := m.EventRouter.EmitControlEvent(events.ControlEvent{
        Type:    events.RequestClusterState,
        GitDest: gitDest,
    }); err != nil {
        log.Error(err, "failed to emit RequestClusterState for rule change", "gitDest", gitDest)
    }
}
```

`collectAffectedGitTargets` is a small helper that snapshots both WatchRule and ClusterWatchRule stores and returns the unique set of `ResourceReference` values that point to a `GitTarget`.

### Step 2 — Suppress informer ADDED events during snapshot

When a snapshot is in flight the `GitTargetEventStream` is already in `RECONCILING` state (buffering live events). The buffered `ADDED` events will be flushed after `OnReconciliationComplete()` — but since the snapshot already wrote all those files, the live events will produce no-op file writes and therefore no further commits. This is the existing dedup behaviour and no extra work is needed here.

However, if the informer ADDED events are flushed *before* the snapshot batch lands (race condition), they will produce N commits before the snapshot gets to say anything. The safest guard is:

- The `GitTargetEventStream` in `RECONCILING` state **buffers** live events (already the case).
- `BeginReconciliation()` is called by the snapshot emitter before `RequestClusterState` is processed, so ADDED events that arrive during cache sync are buffered.
- After `OnReconciliationComplete()` the buffered events are flushed and the dedup layer discards them (no-op writes).

Verify this ordering in the existing `BeginReconciliation` / `OnReconciliationComplete` tests.

### Step 3 — Update the e2e assertion

The bi-directional e2e test currently asserts that the commit count stays stable after Flux applies resources back. Once this fix is in, the secret-phase of the test should produce **one** `reconcile:` commit rather than five `[CREATE]` commits. Update the expected commit log snapshot in the test accordingly.

---

## Expected Git History After Fix

```
badc99d chore(bootstrap): initialize path bi-directional/.../live
a1b2c3d reconcile: sync 5 resources from cluster snapshot   ← WatchRule added
d4e5f6g [UPDATE] shop.example.com/v1/icecreamorders/bi-alice-order-<id>
```

Instead of the current:

```
badc99d chore(bootstrap): initialize path bi-directional/.../live
90c5268 [CREATE] v1/secrets/bi-controller-sops-<id>
83771a3 [CREATE] v1/secrets/git-creds-<run>
20b7155 [CREATE] v1/secrets/git-creds-invalid-<run>
9ed0925 [CREATE] v1/secrets/git-creds-ssh-<run>
d093af3 [CREATE] v1/secrets/sops-age-key
```

---

## Invariant

After this fix the following invariant should hold:

> Every change to the **watch scope** (WatchRule add/update/delete, controller restart) produces exactly **one** `reconcile: sync N resources from cluster snapshot` commit. Individual `[CREATE]` commits in the history always reflect real-time cluster changes, never initial population.
