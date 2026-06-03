# Design: rule-change snapshot trigger

> Status: implemented — captured after issues #145/#146-style symptoms surfaced.
> Date: 2026-05-20
> Implemented: 2026-05-21
> Tests that anchor this work:
> - [internal/watch/rule_change_snapshot_test.go](../../internal/watch/rule_change_snapshot_test.go) — four unit tests
> - [test/e2e/e2e_test.go](../../test/e2e/e2e_test.go) — `It("should backfill pre-existing ConfigMap when WatchRule is added afterwards", ...)`

## Implementation summary

The fix landed as an in-memory delivery contract between `WatchManager` and the
`FolderReconciler` registry:

- `WatchManager` now computes a per-GitTarget rule-set hash from the compiled
  `WatchRule` and `ClusterWatchRule` entries that affect that target.
- `ReconcileForRuleChange` treats GVR changes as informer work, not as the only
  snapshot trigger. If the rule-set hash changed, the target is marked pending
  even when the GVR set is unchanged.
- Snapshot delivery only advances `lastDeliveredRuleSetHash` after the target has
  a registered `FolderReconciler` and both repo and cluster state requests were
  handed to the router.
- `ReconcilerManager.CreateReconciler` calls `WatchManager.MaybeReplaySnapshot`
  when a new reconciler appears, so restart-like bootstraps drain pending
  snapshots instead of dropping them.
- `GitTargetReconciler` now creates a `FolderReconciler` even when
  `SnapshotSynced=True`, preserving the "no new initial snapshot" gate while
  still allowing rule-change snapshots to be delivered.

Validation at implementation time:

- `go test ./internal/watch -run 'TestReconcileForRuleChange' -count=1`
- `task lint`
- `task test`
- `task test-e2e` with the new backfill spec included in the full e2e suite

## The problem in one sentence

`WatchManager.ReconcileForRuleChange` emits its snapshot as a fire-and-forget broadcast against the EventRouter, with no notion of "did this snapshot actually reach a `FolderReconciler` for the affected GitTarget?" and no retry — so any time the rule-set changes without changing the GVR set, or the receiver isn't registered yet, the backfill is silently lost.

## Two failure modes, one root cause

Both reduce to: *no delivery contract between snapshot producer and snapshot consumer.*

### A. Rule-add when GVR is already watched

The old `ReconcileForRuleChange` implementation short-circuited when the GVR
set didn't change:

```go
if len(added) == 0 && len(removed) == 0 {
    return nil
}
```

Adding a second rule for an already-watched GVR, or editing a rule's selectors without touching its GVR, takes this path. The snapshot is never even attempted. Resources that match the new rule and already exist in the cluster stay out of git until they're next mutated. Symmetric across `WatchRule` and `ClusterWatchRule`.

### B. Restart with `SnapshotSynced=True`

The `GitTargetReconciler` writes `SnapshotSynced=True` after a successful initial snapshot. On restart this is read back from the object's status, so the old `evaluateSnapshotGate` path took the early-return — it ensured a `GitTargetEventStream` was registered, but it did *not* create a `FolderReconciler`. Meanwhile `WatchManager.Start` ran its own initial `ReconcileForRuleChange`, which *did* emit a snapshot. That snapshot's `RequestClusterState` reached `RouteClusterStateEvent`, which looked up the reconciler, found none, and dropped the event with a V(1) log line. The same dropping happened for `RouteRepoStateEvent`. The user saw: "live updates write fine, but pre-existing resources never appear after a restart."

The 30-second periodic `ReconcileForRuleChange` then hits failure mode A (GVRs stable), so there's no self-heal.

## How the snapshot walk works

User asked specifically: *how do we walk through all resources in all namespaces when a ClusterWatchRule is adjusted?* The full chain:

1. **Trigger.** `ClusterWatchRuleReconciler` finishes reconciling and calls `WatchManager.ReconcileForRuleChange(ctx)` ([clusterwatchrule_controller.go:183](../../internal/controller/clusterwatchrule_controller.go#L183)).
2. **GVR diff and rule hash.** `ReconcileForRuleChange` computes the desired GVR set from the rule store and diffs it against `activeInformers`. The implemented path also computes per-target rule-set hashes, so unchanged GVRs no longer suppress a needed snapshot.
3. **Informer churn.** `startInformersForGVRs(added)` / `stopInformer(removed)` brings the informer set in line with the desired set. New informers run `WaitForCacheSync`, so by the time this returns every existing resource has been observed by the informer and forwarded as an ADDED event.
4. **Stream buffering.** `beginReconciliationForTargets` transitions every registered `GitTargetEventStream` for a target needing delivery to `Reconciling` so informer ADDED events from step 3 are *buffered* instead of producing N individual `[CREATE]` commits.
5. **Snapshot emission.** `emitSnapshotForRuleChange` calls `ProcessControlEvent(RequestRepoState)` and `ProcessControlEvent(RequestClusterState)` for every target that needs delivery and has a registered `FolderReconciler`. `handleRequestClusterState` calls [`GetClusterStateForGitDest`](../../internal/watch/manager.go):
   - Build `gvrMap[GVR] -> {namespaces, clusterWide}` from active rules. ClusterWatchRule entries set `clusterWide=true`.
   - For each entry, call `listResourcesForGVR`: cluster-wide `List` when `clusterWide`, namespaced `List` when WatchRule.
   - Sanitize each item, key it by `ResourceIdentifier`, return as a `ClusterStateEvent`.
6. **Diff and write.** `FolderReconciler.OnClusterState` ([folder_reconciler.go:120](../../internal/reconcile/folder_reconciler.go#L120)) stores the cluster-state half. When the matching `RepoState` half also arrives, `reconcile()` diffs (`findDifferences`) and emits one atomic `WriteRequest` containing `CREATE`/`UPDATE`/`DELETE` per resource.
7. **Flush.** `completeReconciliationForTargets` transitions streams back to `LiveProcessing`. The buffered events from step 3 flush; they're no-ops at the git layer because the snapshot batch just wrote those files.

So the *walk itself* is functional and correctly cluster-wide for ClusterWatchRule. The original hole was steps 4-6: **the broadcast at step 5 had no acknowledgment.** The implemented delivery contract now keeps the snapshot pending when no receiver is registered.

### Architectural note worth flagging now

`listResourcesForGVR` doesn't apply rule selectors today — it lists every resource of the GVR and returns them all unfiltered (the rulestore is MVP-simplified, `rulestore/store.go:328` confirms `NamespaceSelector` isn't implemented yet). For *live events*, `matchRules` applies the full predicate including `nsLabels`. The two paths diverge.

This is fine as long as ClusterWatchRule means "all of this GVR, everywhere." The moment `namespaceSelector` / `labelSelector` lands on the rule spec (which the existing CRD comments already hint at), the snapshot walk must apply the same predicates as the live path or the steady state will not match the snapshot state and we'll get spurious deletes on every reconcile. Fix should land in the same change-set that introduces selectors, not retrofitted later.

## Implemented fix shape

The minimum change to close both failure modes is **a delivery contract per GitTarget**.

### Core idea

For each GitTarget, the WatchManager tracks two values:

- `lastDeliveredRuleSetHash` — fingerprint of the rule-set that was last successfully snapshotted *and* delivered to a registered `FolderReconciler`.
- `currentRuleSetHash` — recomputed on every `ReconcileForRuleChange`, derived from the rules in the store affecting this GitTarget.

The trigger logic becomes:

```
for each target in currentRuleSetSnapshots():
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

### What the implemented rule-set hash contains

Per gitDest, in canonical order, the implementation fingerprints the compiled
rule inputs currently held in `RuleStore`:

- For each `WatchRule` targeting it: source name/namespace, resolved target,
  provider, branch, path, and compiled resource rules.
- For each `ClusterWatchRule` targeting it: source name, resolved target,
  provider, branch, path, and compiled cluster resource rules.

This uses the compiled store state rather than fetching the original CR objects
or relying on `metadata.generation`. That keeps the snapshot trigger cheap and
aligned with what live matching actually sees.

## Non-goals

- **No removal of the periodic 30-second `ReconcileForRuleChange`.** It stays as a safety net (informer restarts, missed events). Just becomes hash-aware so it's free when there's nothing to do.
- **No rewrite of the snapshot walk.** The walk is correct; only the trigger and delivery contract change.
- **No new CRD fields.** This is internal coordination; the user-visible surface doesn't change.

## Risk / open questions

- **Hash for status-only rule updates.** The implemented hash is derived from compiled rule store state, not object status. If future work fetches live CR objects for hashing, avoid including `status`, `resourceVersion`, or other status-only metadata.
- **Delivery confirmation vs. delivery success.** The route function "found a receiver" doesn't mean the reconciler actually wrote anything to git — the diff might be empty, or a git push could fail later. We're only promising the snapshot was *handed off* to the right receiver; downstream failures are observable through existing git-push error paths and don't need to be folded in here.
- **Concurrent rule changes.** Two `ReconcileForRuleChange` calls landing close together must not race the hash update. A per-gitDest mutex (or the existing `activeInformers` lock) is enough.
- **Selector parity (flagged earlier).** When selectors land, `GetClusterStateForGitDest` must filter through the same predicate the live path uses, or the diff will produce spurious deletes.

## Downsides and future improvements

- **Hash format is implementation-local.** The current hash is built from
  formatted compiled rule structs. It is fine for an in-process trigger, but a
  future persistent or cross-version delivery contract should use explicit
  canonical JSON or a typed hash builder.
- **Pending delivery state is in memory.** A restart recomputes desired state on
  the next reconciliation, but pending state is not persisted. If this project
  moves toward multi-replica watch managers or stronger crash recovery, this
  contract should move into leader-owned status, a lease-backed store, or a
  durable work queue.
- **Delivery still means "handed to the reconciler."** The implementation does
  not wait for git writes or pushes to succeed before advancing the rule-set
  delivery hash. Downstream failures remain observable through existing git
  error paths. A future version could expose a stronger end-to-end snapshot
  completion condition if users need it.
- **Rule deletion cleanup needs explicit thought.** A hash computed only from
  current rules cannot, by itself, identify a GitTarget that just lost its last
  rule. If deletion should remove previously written files, future work should
  retain tombstones for affected targets or make rule deletion trigger a
  target-scoped cleanup snapshot before forgetting the target.
- **Replay uses the reconciler creation edge.** This solves the known restart
  gap, and periodic reconciliation remains a safety net. If reconciler lifecycle
  grows more complex, consider a small pending-snapshot work queue with retry,
  backoff, and metrics instead of a direct callback.
- **Snapshot selector parity remains important.** When namespace or label
  selectors become authoritative in snapshot listing, the hash must include
  those selector inputs and `GetClusterStateForGitDest` must filter with the
  same predicate as live event matching.

## Acceptance criteria

The four failing unit tests in
[internal/watch/rule_change_snapshot_test.go](../../internal/watch/rule_change_snapshot_test.go)
go green:

- `TestReconcileForRuleChange_AddingSecondRuleSameGVR_EmitsSnapshot`
- `TestReconcileForRuleChange_NarrowingSelectorOnExistingRule_EmitsSnapshot`
- `TestReconcileForRuleChange_AddingSecondWatchRuleSameGVR_EmitsSnapshot`
- `TestReconcileForRuleChange_RestartLikeBootstrap_NoSnapshotDrops`

The e2e spec
`It("should backfill pre-existing ConfigMap when WatchRule is added afterwards", ...)`
passes against a real k3d cluster.

The `EventRouter.SnapshotDeliveryDrops()` counter stays at 0 in steady state under a representative load test (creating and editing rules, restarting the controller).

## References

- Trigger bug summary: [idea-cross-kind-dependency-watches.md](../future/idea-cross-kind-dependency-watches.md) (related coordination concern around dependency-watches).
- The old early-return: `ReconcileForRuleChange` in [manager.go](../../internal/watch/manager.go).
- The snapshot consumer drop instrumentation: `RouteRepoStateEvent` and `RouteClusterStateEvent` in [event_router.go](../../internal/watch/event_router.go).
- The GitTarget restart gate: `evaluateSnapshotGate` in [gittarget_controller.go](../../internal/controller/gittarget_controller.go).
- The snapshot walk itself: `GetClusterStateForGitDest` and `listResourcesForGVR` in [manager.go](../../internal/watch/manager.go).
