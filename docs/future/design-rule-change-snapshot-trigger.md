# Design: rule-change snapshot trigger

> Status: design — captured after issues #145/#146-style symptoms surfaced.
> Date: 2026-05-20
> Failing tests that anchor this work:
> - [internal/watch/rule_change_snapshot_test.go](../../internal/watch/rule_change_snapshot_test.go) — four unit tests
> - [test/e2e/e2e_test.go](../../test/e2e/e2e_test.go) — `PIt("should backfill pre-existing ConfigMap when WatchRule is added afterwards", ...)`

## The problem in one sentence

`WatchManager.ReconcileForRuleChange` emits its snapshot as a fire-and-forget broadcast against the EventRouter, with no notion of "did this snapshot actually reach a `FolderReconciler` for the affected GitTarget?" and no retry — so any time the rule-set changes without changing the GVR set, or the receiver isn't registered yet, the backfill is silently lost.

## Two failure modes, one root cause

Both reduce to: *no delivery contract between snapshot producer and snapshot consumer.*

### A. Rule-add when GVR is already watched

[manager.go:660-664](../../internal/watch/manager.go#L660-L664) short-circuits when the GVR set didn't change:

```go
if len(added) == 0 && len(removed) == 0 {
    return nil
}
```

Adding a second rule for an already-watched GVR, or editing a rule's selectors without touching its GVR, takes this path. The snapshot is never even attempted. Resources that match the new rule and already exist in the cluster stay out of git until they're next mutated. Symmetric across `WatchRule` and `ClusterWatchRule`.

### B. Restart with `SnapshotSynced=True`

The `GitTargetReconciler` writes `SnapshotSynced=True` after a successful initial snapshot. On restart this is read back from the object's status, so [`evaluateSnapshotGate` at gittarget_controller.go:384](../../internal/controller/gittarget_controller.go#L384) takes the early-return — it ensures a `GitTargetEventStream` is registered, but it does *not* create a `FolderReconciler`. Meanwhile `WatchManager.Start` runs its own initial `ReconcileForRuleChange` (`manager.go:99`), which *does* emit a snapshot. That snapshot's `RequestClusterState` reaches [`RouteClusterStateEvent`](../../internal/watch/event_router.go#L254-L263), which looks up the reconciler, finds none, and drops the event with a V(1) log line. The same dropping happens for `RouteRepoStateEvent`. The user sees: "live updates write fine, but pre-existing resources never appear after a restart."

The 30-second periodic `ReconcileForRuleChange` then hits failure mode A (GVRs stable), so there's no self-heal.

## How the walk works today

User asked specifically: *how do we walk through all resources in all namespaces when a ClusterWatchRule is adjusted?* The full chain:

1. **Trigger.** `ClusterWatchRuleReconciler` finishes reconciling and calls `WatchManager.ReconcileForRuleChange(ctx)` ([clusterwatchrule_controller.go:183](../../internal/controller/clusterwatchrule_controller.go#L183)).
2. **GVR diff.** `ReconcileForRuleChange` computes the desired GVR set from the rule store and diffs it against `activeInformers`. The early-return at 660 fires here for failure mode A.
3. **Informer churn.** `startInformersForGVRs(added)` / `stopInformer(removed)` brings the informer set in line with the desired set. New informers run `WaitForCacheSync`, so by the time this returns every existing resource has been observed by the informer and forwarded as an ADDED event.
4. **Stream buffering.** `beginReconciliationForAffectedTargets` transitions every registered `GitTargetEventStream` to `Reconciling` so the informer ADDED events from step 3 are *buffered* instead of producing N individual `[CREATE]` commits.
5. **Snapshot emission.** `emitSnapshotForRuleChange` calls `ProcessControlEvent(RequestRepoState)` and `ProcessControlEvent(RequestClusterState)` for every affected GitTarget. `handleRequestClusterState` calls [`GetClusterStateForGitDest` (manager.go:415)](../../internal/watch/manager.go#L415):
   - Build `gvrMap[GVR] -> {namespaces, clusterWide}` from active rules. ClusterWatchRule entries set `clusterWide=true` ([manager.go:493](../../internal/watch/manager.go#L493)).
   - For each entry, call [`listResourcesForGVR`](../../internal/watch/manager.go#L604): cluster-wide `List` when `clusterWide`, namespaced `List` when WatchRule.
   - Sanitize each item, key it by `ResourceIdentifier`, return as a `ClusterStateEvent`.
6. **Diff and write.** `FolderReconciler.OnClusterState` ([folder_reconciler.go:120](../../internal/reconcile/folder_reconciler.go#L120)) stores the cluster-state half. When the matching `RepoState` half also arrives, `reconcile()` diffs (`findDifferences`) and emits one atomic `WriteRequest` containing `CREATE`/`UPDATE`/`DELETE` per resource.
7. **Flush.** `completeReconciliationForAffectedTargets` transitions streams back to `LiveProcessing`. The buffered events from step 3 flush; they're no-ops at the git layer because the snapshot batch just wrote those files.

So the *walk itself* is functional and correctly cluster-wide for ClusterWatchRule. The hole is steps 4-6: **the broadcast at step 5 has no acknowledgment.** If no receiver is registered the snapshot is computed and thrown away.

### Architectural note worth flagging now

`listResourcesForGVR` doesn't apply rule selectors today — it lists every resource of the GVR and returns them all unfiltered (the rulestore is MVP-simplified, `rulestore/store.go:328` confirms `NamespaceSelector` isn't implemented yet). For *live events*, `matchRules` applies the full predicate including `nsLabels`. The two paths diverge.

This is fine as long as ClusterWatchRule means "all of this GVR, everywhere." The moment `namespaceSelector` / `labelSelector` lands on the rule spec (which the existing CRD comments already hint at), the snapshot walk must apply the same predicates as the live path or the steady state will not match the snapshot state and we'll get spurious deletes on every reconcile. Fix should land in the same change-set that introduces selectors, not retrofitted later.

## Fix shape

The minimum change to close both failure modes is **a delivery contract per GitTarget**.

### Core idea

For each GitTarget, the WatchManager tracks two values:

- `lastDeliveredRuleSetHash` — fingerprint of the rule-set that was last successfully snapshotted *and* delivered to a registered `FolderReconciler`.
- `currentRuleSetHash` — recomputed on every `ReconcileForRuleChange`, derived from the rules in the store affecting this GitTarget.

The trigger logic becomes:

```
for each gitDest in collectAffectedGitTargets():
    if currentRuleSetHash[gitDest] == lastDeliveredRuleSetHash[gitDest]:
        continue  # already in sync
    if no FolderReconciler registered for gitDest:
        mark gitDest as needing snapshot; do not advance lastDelivered
        continue
    emit RequestRepoState + RequestClusterState
    on successful delivery (route function found the receiver):
        lastDeliveredRuleSetHash[gitDest] = currentRuleSetHash[gitDest]
```

And one additional hook: when `EventRouter.RegisterGitTargetEventStream` (or whenever the reconciler is created via `ReconcilerManager.CreateReconciler`) registers a new receiver, ask the WatchManager whether that gitDest has a pending snapshot — if so, drain it.

This single change covers:

- **Failure A.** `compareGVRs` becomes informational only; the snapshot decision is rule-hash-based. `len(added)==0 && len(removed)==0` no longer short-circuits the snapshot path.
- **Failure B.** When `WatchManager.Start` fires the initial snapshot before the GitTargetReconciler has registered a receiver, `lastDelivered` doesn't advance. The GitTargetReconciler subsequently registers, which drains the pending entry, the snapshot fires again with a receiver present, and `lastDelivered` advances.

The early-return on truly-stable state (rule-hash unchanged) is preserved — we keep idempotency for periodic ticks.

### Receiver registration hook

The cleanest place is `ReconcilerManager.CreateReconciler` (called from `GitTargetReconciler.evaluateSnapshotGate`). It already has the gitDest. Adding "on new reconciler creation, notify WatchManager" makes the coordination explicit and avoids implicit timing assumptions.

Sketch:

```go
// In ReconcilerManager.CreateReconciler, after inserting the reconciler:
if m.onReconcilerCreated != nil {
    m.onReconcilerCreated(gitDest)
}
```

And `onReconcilerCreated := watchManager.MaybeReplaySnapshot` is wired at startup.

### Why not just remove the early-return at manager.go:660?

That fixes failure A but not failure B, and it makes every 30-second periodic tick a full cluster-wide list. The hash-based version pays the same cost only when the hash changes, which is the right knob.

### What the rule-set hash actually contains

Per gitDest, in canonical order:

- For each `WatchRule` targeting it: name, namespace, generation (or a hash of `Spec`).
- For each `ClusterWatchRule` targeting it: name, generation (or `Spec` hash).

`generation` is the safer pick — it bumps on every `spec` change and is stable for status updates, so periodic reconciles caused by status-only events won't trigger spurious snapshots. The fingerprint is cheap; computing it on every `ReconcileForRuleChange` is fine.

## Non-goals

- **No removal of the periodic 30-second `ReconcileForRuleChange`.** It stays as a safety net (informer restarts, missed events). Just becomes hash-aware so it's free when there's nothing to do.
- **No rewrite of the snapshot walk.** The walk is correct; only the trigger and delivery contract change.
- **No new CRD fields.** This is internal coordination; the user-visible surface doesn't change.

## Risk / open questions

- **Hash for status-only rule updates.** If a controller patches `.status` on a `ClusterWatchRule`, that doesn't change `metadata.generation` and shouldn't cause a re-snapshot. Using `generation` correctly handles this.
- **Delivery confirmation vs. delivery success.** The route function "found a receiver" doesn't mean the reconciler actually wrote anything to git — the diff might be empty, or a git push could fail later. We're only promising the snapshot was *handed off* to the right receiver; downstream failures are observable through existing git-push error paths and don't need to be folded in here.
- **Concurrent rule changes.** Two `ReconcileForRuleChange` calls landing close together must not race the hash update. A per-gitDest mutex (or the existing `activeInformers` lock) is enough.
- **Selector parity (flagged earlier).** When selectors land, `GetClusterStateForGitDest` must filter through the same predicate the live path uses, or the diff will produce spurious deletes.

## Acceptance criteria

The four failing unit tests in
[internal/watch/rule_change_snapshot_test.go](../../internal/watch/rule_change_snapshot_test.go)
go green:

- `TestReconcileForRuleChange_AddingSecondRuleSameGVR_EmitsSnapshot`
- `TestReconcileForRuleChange_NarrowingSelectorOnExistingRule_EmitsSnapshot`
- `TestReconcileForRuleChange_AddingSecondWatchRuleSameGVR_EmitsSnapshot`
- `TestReconcileForRuleChange_RestartLikeBootstrap_NoSnapshotDrops`

The pending e2e spec
`PIt("should backfill pre-existing ConfigMap when WatchRule is added afterwards", ...)`
flips from `PIt` to `It` and passes against a real k3d cluster.

The `EventRouter.SnapshotDeliveryDrops()` counter stays at 0 in steady state under a representative load test (creating and editing rules, restarting the controller).

## References

- Trigger bug summary: [idea-cross-kind-dependency-watches.md](idea-cross-kind-dependency-watches.md) (related coordination concern around dependency-watches).
- The early-return: [manager.go:660-664](../../internal/watch/manager.go#L660-L664).
- The snapshot consumer that gets dropped: [event_router.go:254-263](../../internal/watch/event_router.go#L254-L263) (post-instrumentation: increments `snapshotDeliveryDrops`).
- The GitTarget gate that skips re-snapshot on restart: [gittarget_controller.go:384](../../internal/controller/gittarget_controller.go#L384).
- The snapshot walk itself: [GetClusterStateForGitDest at manager.go:415](../../internal/watch/manager.go#L415), [listResourcesForGVR at manager.go:604](../../internal/watch/manager.go#L604).
