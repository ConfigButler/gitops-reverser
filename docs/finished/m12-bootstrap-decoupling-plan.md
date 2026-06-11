# M12 bootstrap-decoupling: typeset-owned per-type bootstrap

> Status: replacement implementation plan, captured 2026-06-09. This supersedes the
> earlier "whole-target snapshot plus per-type escape hatch" plan. Sibling design:
> [type-lifecycle-events-and-wobble-settling.md](type-lifecycle-events-and-wobble-settling.md);
> flake forensics:
> [../design/manifest/e2e-full-suite-flakiness-findings-2026-06.md](../design/manifest/e2e-full-suite-flakiness-findings-2026-06.md).

## Decision

Make the typeset registry the runtime source of truth for type lifecycle, and make
the unit of bootstrap work one selected type for one GitTarget.

The old model is:

```text
GitTarget Ready waits for one whole-target snapshot
  -> stream every selected type
  -> join every bookmark
  -> one full mark-and-sweep commit
  -> then live events may flow
```

The new model is:

```text
typeset registry owns type verdict + lifecycle
WatchedTypeTable projects registry records per GitTarget
TargetTypeScheduler schedules missing per-type work
  TypeActivated / selected type -> scoped reconcile
  TypeRemoved / unselected type -> scoped sweep
```

No normal bootstrap path should need a whole-GitTarget snapshot. Whole-target
snapshot code may stay temporarily as a repair/test helper, but it is no longer a
runtime dependency.

## Rollout Decision

Land the immediate blocker as a small PR before the scheduler rewrite:

```text
PR 1: remove WaitForCacheSync from informer startup
  -> focused e2e should go green with current whole-target machinery

PR 2+: introduce the per-type scheduler behind the existing path
  -> prove retry, apply-result feedback, and restart-safe sweep
  -> then remove normal whole-target bootstrap/rule-change usage
```

This sequencing does not weaken the destination. It keeps the urgent e2e fix out of
the blast radius of changing bootstrap, status, and convergence semantics at the
same time.

## What We Lose

These components should be deleted from normal runtime, not patched. Delete them
only after the replacement scheduler has the two hard correctness properties below:
mirror-derived per-type sweep and retry/apply-result feedback.

- `GitTargetConditionSnapshotSynced`: remove the condition and all "Blocked until
  SnapshotSynced=True" status wiring. This can be a later cleanup: the first
  scheduler slice may keep the condition as a transitional compatibility signal.
- `GitTargetReconciler.evaluateSnapshotGate`: replace it with target activation:
  ensure the worker, ensure/register the event stream, and ask the per-type scheduler
  to reconcile the target's current selected type set.
- `EventRouter.EmitResyncForGitDest` and `gatherAndEnqueueResync`: remove from the
  GitTarget controller and WatchRule rule-change path. If a full-target repair command
  is kept, move this behind an explicit repair-only API/name so production code cannot
  drift back to it.
- `Manager.snapshotTargetsNeedingDelivery`, `ruleSetSnapshotTarget`,
  `lastDeliveredRuleSetHash`, and `pendingRuleSetHash`: replace the target-level
  "effective plan hash needs a snapshot" cache with per-type sync state.
- `Manager.emitSnapshotForRuleChange`: rule changes enqueue per-type reconciles and
  sweeps; they do not enqueue a whole-target snapshot.
- `StreamClusterSnapshotForGitDest` and `resolveSnapshotGVRs` as runtime bootstrap
  machinery. Do not add `SweepSkip`; that creates another full-resync mode instead of
  deleting the old one.
- `git.ResyncRequest` whole-target mode in normal operation. `ScopeGVR` should become
  the only resync mode used by bootstrap, rule changes, lifecycle recovery, and type
  removal.
- The informer cache-sync handoff assumption in `ReconcileForRuleChange`: no comments
  or logic may rely on `WaitForCacheSync` proving that initial ADDED events were
  buffered before a full snapshot.

## PR 1: Root Cause Fix

The deterministic e2e failure remains real and should be fixed immediately:
`WatchRuleReconciler.reconcileWatchRuleViaTarget` calls
`Manager.ReconcileForRuleChange` synchronously before status is persisted. That path
starts informers and blocks in `startSingleInformer` on `cache.WaitForCacheSync`
while holding `informersMu`. A fresh CRD informer can fail to list with "the server
could not find the requested resource", so the WatchRule never writes conditions and
the spec times out on `status.conditions not found`.

Do this as the first code change:

- In `internal/watch/manager.go`, remove synchronous `cache.WaitForCacheSync` from
  `startSingleInformer`.
- Start informer factories and return. Informer sync is background work; live events
  flow when the informer is actually synced.
- Update `ReconcileForRuleChange` comments so they no longer claim cache sync
  guarantees the initial ADDED buffer.

This fix is independent of the architecture rewrite, but it removes the immediate
deadlock and makes the later per-type path easier to validate.

Expected result: the focused wildcard e2e should pass with the current machinery
still intact. The WatchRule writes status promptly; existing retry paths can gather
and commit once the fresh CRD-backed type is actually served.

## New Component: TargetTypeScheduler

Add one scheduler in `internal/watch`, owned by `Manager`.

Inputs:

- the current `typeset.Registry`;
- the current resident `WatchedTypeTable` values;
- registered GitTarget event streams;
- branch worker availability through `EventRouter`;
- lifecycle events from `typeset.Registry.Subscribe`;
- explicit target/rule-change kicks from GitTarget and WatchRule reconcilers.

State:

- per `(gitDest, gvr)` selected scope hash;
- last successfully applied registry generation/revision for that type;
- last action: `reconcile`, `sweep`, `skip-retained`, `skip-refused`;
- last error and timestamp;
- next retry time and retry count for transient failures;
- in-flight flag so repeated kicks coalesce instead of stampeding the worker.

Lifecycle events are wakeups, not durable truth. On every wakeup the scheduler reads
the registry and target table again and computes the current required work. This keeps
event delivery cheap and makes dropped buffered events recoverable without falling
back to a whole-target snapshot.

## Hard Requirements Before Replacement

The scheduler may run beside the old path before these are done, but it must not
replace whole-target bootstrap/rule-change delivery until both are true:

1. **Mirror-derived per-type sweep.** Rule removals and settled `TypeRemoved` must
   converge after manager restart. The scheduler cannot rely only on in-memory
   "previously selected" state to know what to sweep. It must derive mirrored types
   from the managed Git subtree/manifest store, then sweep only the affected
   `(group, resource)` with `ScopeGVR`.
2. **Retry plus apply-result feedback.** A per-type reconcile is not complete when it
   is enqueued. The scheduler must learn whether the worker applied the scoped resync,
   record success only from the `ResyncResult`, and periodically recompute/retry
   transient failures such as API stream errors, worker-not-ready, or push conflicts.

These two properties are what make the scheduler a convergence mechanism instead of
an event callback. They are also what make content assertions pass, not just readiness
assertions.

## Scheduler Rules

For every target with a live event stream:

- If a selected type is `VerdictFollowable` and either it was never applied, its scope
  changed, or the registry revision/generation advanced, enqueue
  `EventRouter.EmitTypeReconcileForGitDest`.
- If a selected type is `VerdictRetained`, enqueue nothing, keep the previous mirror,
  and record `skip-retained` status. Retained means "do not stream, do not sweep".
- If a previously selected type is no longer selected by rules, enqueue
  `EventRouter.EmitTypeSweepForGitDest` for that GVR. After restart, "previously
  selected" comes from the mirrored managed types, not only scheduler memory.
- If the registry emits settled `TypeRemoved`, enqueue a type sweep for targets that
  previously selected or currently mirror that GVR.
- If a type is `VerdictRefused`, enqueue nothing and surface the reason. Permanent
  refusal should never become a destructive sweep unless the type was previously
  selected and the scheduler has an explicit previous-selection record to sweep.
- If a scoped reconcile/sweep fails, keep the work item pending and retry from a
  periodic recompute. Lifecycle events should speed up convergence, not be the only
  recovery path.

The scheduler must be idempotent: recomputing all target/type pairs after restart is
valid. Missing in-memory state should err toward reconciling followable selected
types, not sweeping unknown historical types.

## Small Scheduler Slice

The first scheduler PR should be intentionally small:

- add the scheduler and recompute loop;
- drive only scoped reconciles for selected `VerdictFollowable` types;
- record worker apply results and retry failures;
- leave whole-target bootstrap/rule-change delivery in place as a fallback;
- avoid GitTarget status API churn except for internal/diagnostic fields needed by
  tests.

The second scheduler PR adds mirror-derived scoped sweep. Only after that should
normal whole-target bootstrap and rule-change snapshots be removed.

## GitTarget Controller

Final target shape: replace the snapshot gate with target activation:

```text
Validated gate
EncryptionConfigured gate
ensure branch worker
ensure/register GitTarget event stream
schedule target's current selected types
mark EventStreamLive based on stream registration
mark Ready when target runtime is live and per-type work is accepted/observable
```

`Ready` should no longer mean "all selected types completed one global snapshot".
That definition is what lets one unhealthy type block the whole target. The status
should instead expose per-type progress and failures:

- selected type count;
- synced type count;
- retained/wobbling type count;
- failed type count with capped examples and reasons;
- last successful type reconcile/sweep time.

If the whole API catalog has never been observed (`registry.Ready()==false`), target
activation should stay blocked. Once the registry is ready, unrelated type failures
must not block target activation.

Do not force this status/API cleanup into PR 1 or the first scheduler slice. Keeping
`SnapshotSynced` as a transitional compatibility signal is acceptable while the
scheduler proves convergence.

## WatchRule and Rule Changes

`ReconcileForRuleChange` should become:

```text
RefreshAPIResourceCatalog
refreshWatchedTypeTables
diff previous TargetTypeSet against current TargetTypeSet
start/stop informers non-blockingly
schedule per-type reconcile for added or scope-changed selected types
schedule per-type sweep for removed selected types
return so the WatchRule controller can write status
```

No whole-target snapshot is emitted for rule changes. A wildcard expansion across
core and custom APIs becomes many independent type reconciles. If one CRD-backed type
is not served yet, that type stays retained or fails its own reconcile; ConfigMaps and
other healthy types still proceed.

During the transition, rule changes may still run the old whole-target snapshot after
non-blocking informer startup. The cutover happens only when scoped reconcile retry
and mirror-derived scoped sweep are both validated.

## Type Lifecycle Consumer

Keep the registry subscription, but change the consumer from "directly do git work"
to "wake the scheduler".

Current direct behavior:

```text
TypeActivated -> fan out EmitTypeReconcileForGitDest
TypeRemoved   -> fan out EmitTypeSweepForGitDest
```

New behavior:

```text
TypeActivated -> scheduler.RecomputeGVR(gvr)
TypeRecovered -> scheduler.RecomputeGVR(gvr)
TypeWobbling  -> scheduler.RecomputeGVR(gvr)
TypeRemoved   -> scheduler.RecomputeGVR(gvr)
TypeRefused   -> scheduler.RecomputeGVR(gvr)
```

The recompute step reads the latest registry verdict and target tables. This makes
`TypeWobbling`, `TypeRecovered`, and `TypeRefused` useful without making every event
handler re-derive type state.

## Git Worker

Keep the existing scoped machinery:

- `EventRouter.EmitTypeReconcileForGitDest`;
- `EventRouter.EmitTypeSweepForGitDest`;
- `git.ResyncRequest.ScopeGVR`;
- `manifestanalyzer.BuildScopedPlan`.

Tighten the model so scoped resync is the runtime default. The whole-target branch in
`resyncPlan` can remain only while repair/test callers still exist. The implementation
should make it obvious when the old whole-target mode has no production callers left,
then delete it.

Do not add `SweepSkip`. It is the wrong abstraction: it preserves a global sweep and
teaches it exceptions. Per-type sweep already gives the correct safety boundary.

## Safety Invariants

- Never sweep a type unless the work item is scoped to that type.
- Never sweep a retained type. `VerdictRetained` means the previous mirror is held.
- Never stream a retained type. It is not served/trusted right now.
- Sweep on CRD/type disappearance only after the registry emits settled `TypeRemoved`
  or after a rule diff proves this GitTarget no longer selects the type.
- A failed per-type stream blocks only that type's reconcile. It must not prevent
  other selected followable types from reconciling.
- BranchWorker serialization remains the ordering guarantee for multiple type-scoped
  commits touching the same file.

## Implementation Sequence

1. PR 1: remove `WaitForCacheSync` from informer startup and fix stale
   comments/tests. Run the focused wildcard e2e and the required suite.
2. Add `TargetTypeScheduler` with per-target/per-GVR sync state, periodic recompute,
   apply-result feedback, and retry. Initially schedule scoped reconciles only.
3. Change the lifecycle consumer to wake the scheduler instead of directly enqueueing
   git work.
4. Add mirror-derived per-type sweep: derive mirrored managed GVRs from the GitTarget
   subtree/manifest store and sweep removed/unselected types with `ScopeGVR`.
5. Rewrite rule-change delivery to diff target type sets and schedule scoped reconcile
   or sweep. Keep the old whole-target snapshot fallback until tests prove scoped
   convergence across restart and transient failures.
6. Rewrite GitTarget activation to remove dependence on `evaluateSnapshotGate`.
   `SnapshotSynced` may remain as a transitional condition until status is redesigned.
7. Delete target-level snapshot delivery state: `snapshotTargetsNeedingDelivery`,
   `ruleSetSnapshotTarget`, `lastDeliveredRuleSetHash`, `pendingRuleSetHash`, and
   their tests.
8. Remove production callers of `EmitResyncForGitDest` and
   `StreamClusterSnapshotForGitDest`. Keep them only behind explicit repair/test names
   until all tests are migrated.
9. Update GitTarget status/tests to assert per-type progress; then delete
   `SnapshotSynced`.
10. Update design docs and flake findings to state that bootstrap is per-type and the
    full snapshot path is no longer runtime architecture.

## Files To Modify

- `internal/watch/manager.go`: non-blocking informer start; rule-change path becomes
  type-set diff + scheduler kick; remove target-level snapshot delivery state after
  scoped convergence is proven.
- `internal/watch/type_lifecycle.go`: lifecycle events wake scheduler; remove
  `gitTargetSnapshotSynced` and direct fan-out functions.
- `internal/watch/target_type_scheduler.go` (new): per-target/per-GVR scheduling and
  sync-state owner.
- `internal/watch/snapshot_stream.go`: keep `StreamSnapshotForType`; remove or demote
  `StreamClusterSnapshotForGitDest` and `resolveSnapshotGVRs` from runtime paths.
- `internal/watch/event_router.go`: keep scoped type reconcile/sweep; remove production
  use of whole-target resync.
- `internal/controller/gittarget_controller.go`: replace `evaluateSnapshotGate` with
  target activation and scheduler kick; keep `SnapshotSynced` only as transitional
  compatibility until status is redesigned.
- `internal/git/types.go`, `internal/git/resync_flush.go`: keep `ScopeGVR`; delete
  whole-target resync mode after repair/test callers are gone.
- Tests under `internal/watch`, `internal/controller`, and `internal/git`: replace
  whole-target snapshot expectations with scoped scheduling expectations.
- Docs:
  `docs/finished/type-lifecycle-events-and-wobble-settling.md` and
  `docs/design/e2e-full-suite-flakiness-findings-2026-06.md`.

## Verification

Because this is a Go/runtime change, run the full validation sequence:

1. `task fmt`
2. `task generate`
3. `task manifests` if GitTarget status/API docs change
4. `task vet`
5. `task lint`
6. `task test`
7. Check Docker with `docker info`
8. `task test-e2e`

Focused acceptance before the full e2e:

```bash
task prepare-e2e
CTX=k3d-gitops-reverser-test-e2e \
INSTALL_MODE=config-dir \
NAMESPACE=gitops-reverser \
E2E_AGE_KEY_FILE=.stamps/cluster/k3d-gitops-reverser-test-e2e/age-key.txt \
go run github.com/onsi/ginkgo/v2/ginkgo \
  --procs=1 \
  --focus="should expand wildcard resources across core and custom namespaced APIs" \
  ./test/e2e/
```

Expected outcome: WatchRule status is written quickly; healthy selected types commit
independently; an unhealthy CRD-backed type cannot block ConfigMaps or other stable
types; deletion is still only type-scoped.
