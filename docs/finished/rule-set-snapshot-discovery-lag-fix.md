# Rule-Set Snapshot Discovery-Lag Fix

> Status: **shipped**. Drives the per-GitTarget effective-watch-plan hash in
> `internal/watch/manager.go`.

## Problem

This fix introduced the per-GitTarget effective-watch-plan hash:

- `currentRuleSetSnapshots()` resolves rules through the API resource catalog.
- It ignores `ResolveMiss` values from `resolver.Resolve(...)`
  (`gvrs, _ := resolver.Resolve(...)`).
- If a target's rules temporarily resolve to zero GVRs, the target is omitted
  entirely ([manager.go:1280-1282](../../internal/watch/manager.go#L1280-L1282)).
- `snapshotTargetsNeedingDelivery()` treats an omitted target as "this target no
  longer has rules" and deletes `lastDeliveredRuleSetHash`
  ([manager.go:1189-1194](../../internal/watch/manager.go#L1189-L1194)).
- When discovery recovers, the same target appears again with the same real
  watch plan but no remembered baseline, so it gets a full rule-change snapshot
  even though the user's rules did not change.

That makes a transient discovery gap look like a real watch-plan change. It can
hide event commits behind later `reconcile: sync ...` commits and is the reason
`crd_lifecycle` still needs `Serial` protection.

## Two facts that shape the fix

Before designing anything, two existing properties narrow what the fix actually
has to do:

1. **Partial snapshots are already refused downstream.**
   `RequestClusterState` aborts whenever `blockingSnapshotMisses(...)` is
   non-empty — "refusing to snapshot a partial cluster view"
   ([manager.go:596-601](../../internal/watch/manager.go#L596-L601)). So a
   half-resolved plan can never *emit* a snapshot. The selection layer does not
   need to be perfect about blocked plans for **correctness** — only to avoid
   noisy select-then-abort retry loops.

2. **"No rules" vs "rules that didn't resolve" needs no miss classification.**
   It is already encoded in the RuleStore. `currentRuleSetSnapshots()` only
   builds a plan for a target that appears in `SnapshotWatchRules()` /
   `SnapshotClusterWatchRules()`. A target with zero rules never gets a plan and
   falls out of `currentKeys` naturally. The bug exists solely because the
   `if len(p.entries) == 0 { continue }` guard drops a target that *still has
   rules* but momentarily resolved to nothing.

Together these mean the headline bug has a small, safe fix, and the richer
classification is a set of optional refinements layered on top.

## Expected states — and when they happen

A target's resolved plan can be in one of four states. The two that produce zero
GVRs (states 3 and 4) look identical to the old code, which is the root cause.

| State | Trigger | Stable? | Action |
|---|---|---|---|
| 1. No rules | The last WatchRule/ClusterWatchRule for the GitTarget was deleted, or its `gitTargetRef` was repointed. | — | Prune remembered state. |
| 2. Resolved plan | One or more rules resolve to served, allowed GVRs. The normal steady state. | yes | Compare hash to last settled; emit a snapshot only if it changed. |
| 3. Stable empty plan | Rules exist but resolve to zero watchable GVRs for a reason that will not change on its own: `NotServed`, `Ambiguous`, `Disallowed`. | yes | No snapshot (nothing to watch). Optionally record a no-watch settled hash (Refinement B). |
| 4. Blocked / uncertain | Rules exist and *would* resolve, but discovery can't answer authoritatively right now: `CatalogUnavailable`, `DiscoveryDegraded`, `WildcardGroup`, `WildcardResource`. | no | Do not snapshot, do not mutate remembered state. Retry next cycle. |

Concrete triggers:

- **State 1 — No rules.** `kubectl delete watchrule`, or editing a rule to point
  at a different GitTarget. The target no longer exists in the RuleStore
  snapshot, so no plan is built for it.
- **State 2 — Resolved plan.** Steady state, every reconcile. A real change is
  e.g. a rule that newly adds `secrets` alongside `configmaps`, or a wildcard
  that begins matching a freshly installed CRD.
- **State 3 — Stable empty.** The user wrote a rule for a CRD that was never
  installed, typo'd a resource name, omitted `apiGroups` in a way that matches
  multiple groups, or named a resource the operator is configured to refuse.
  Re-running discovery returns the same answer, so this is settled.
- **State 4 — Blocked.** **This is the bug's trigger.** A CRD is mid
  install/delete (the `crd_lifecycle` spec), the controller just started and
  discovery is still warming up, or the aggregation layer briefly 503s. Unlike
  state 3, the answer **changes on retry**.

The crucial pair is **3 vs 4**: both yield zero GVRs, but 3 is a settled fact and
4 is a temporary "ask again later." The original code collapsed both into "target
removed," which is what turned a routine CRD install (state 4) into a spurious
full re-snapshot of every unrelated target.

## Fix (ship this)

The headline bug is fixed by a near-trivial change. No new types, no miss
classification, no rename.

1. In `currentRuleSetSnapshots()`, **stop dropping empty-entry plans.** Keep
   every target that has at least one rule, even if it currently resolves to zero
   GVRs. (Delete the `if len(p.entries) == 0 { continue }` guard.)
2. In `snapshotTargetsNeedingDelivery()`, **if a plan has no entries, `continue`
   — skip selection and leave `lastDeliveredRuleSetHash` untouched.** There is
   nothing to snapshot, and the remembered baseline must survive the gap.
3. The existing prune loop now fires only for targets with genuinely zero rules.
   That is already correct — no change needed.

Walk the transient-gap case through it: `valid -> (gap, empty) -> valid`. During
the gap the target is kept but skipped, `lastDeliveredRuleSetHash` stays at the
valid hash; on recovery the hash matches, so no snapshot is emitted. Bug gone.

This alone fixes finding #2 and unblocks de-serializing `crd_lifecycle`.

### What the Fix gives up

Exactly **one** behavioral difference versus a fully classified design: the
`valid -> stable-empty -> valid` *edit* sequence (a user edits a rule to
something non-watchable, then edits it back). The Fix keeps the baseline at the
valid hash the whole time, so on return it sees "unchanged" and skips the
recovery snapshot — even though no live events were watched during the empty
window. Refinement B below addresses exactly this case if you want it.

## Refinement A (optional): skip blocked plans instead of aborting them

The Fix is safe for the **mixed** case (one resolved GVR plus one blocking miss →
non-empty entries) because the downstream abort at
[manager.go:596](../../internal/watch/manager.go#L596) refuses the partial
snapshot. But it does so by *selecting* the target every cycle and aborting it,
which logs an error each reconcile until discovery recovers.

To avoid that noise, capture misses in the plan and add a `blocked` flag:

```go
// fields added to the existing targetWatchPlan, not a parallel type
blocked        bool
blockingMisses []ResolveMiss
```

Set `blocked` when any rule for the target produces a miss in
`blockingSnapshotMisses(...)` (`CatalogUnavailable`, `DiscoveryDegraded`,
`WildcardGroup`, `WildcardResource`). Then in `snapshotTargetsNeedingDelivery()`:

```go
if plan.blocked {
    // Rules exist, but the current resolved plan is not authoritative.
    // Keep lastDeliveredRuleSetHash and pendingRuleSetHash as-is;
    // discovery/catalog recovery will retry.
    continue
}
```

This is a quality-of-noise optimization, not a correctness fix — the Fix is
already safe without it.

## Refinement B (optional): settle a stable-empty hash

This is the *only* mechanism that fixes the `valid -> stable-empty -> valid` edit
case, and the only reason to compute a hash for an empty plan.

Include the **stable** misses (`NotServed`, `Ambiguous`, `Disallowed`) in the
hash so a target can settle into a known "no-watch" plan and later transition
back to a resolved plan:

```
valid hash -> stable empty hash -> valid hash
```

The settled empty hash differs from the valid hash, so the final transition is
detected and the target is re-snapshotted. In `snapshotTargetsNeedingDelivery()`:

```go
if !plan.hasEntries { // hasEntries == len(plan.entries) > 0
    // Stable empty/no-watch plan. No cluster snapshot to emit, but the
    // rule-set state did advance. Mark it settled so a later empty -> resolved
    // transition emits a snapshot.
    lastDeliveredRuleSetHash[key] = plan.hash
    delete(pendingRuleSetHash, key)
    continue
}
```

Do **not** include blocking misses in the hash — a blocked plan is not an
authoritative plan, and a blocked target must hit the Refinement A path
(`continue` without mutating state) before this branch.

When is this worth it? Only when a *served* resource churns during a window in
which the user deliberately edited the rule to be non-watchable and back. Rare,
and the worst case without it is a missed recovery snapshot that self-heals on
the next real rule change. For `NotServed` resources there are by definition no
objects to miss, so it is often moot.

## Implementation notes

- Extend the existing `targetWatchPlan` with the `blocked` / `blockingMisses`
  fields rather than introducing a parallel `ruleSetSnapshotPlan` type. Express
  `hasEntries` as `len(entries) > 0`, not a stored field.
- Reuse `blockingSnapshotMisses(...)` for the classification. If the name reads
  oddly in a planning context, extract a thin alias such as
  `blockingRuleSetPlanMisses(...)`.
- If Refinement B lands, `lastDeliveredRuleSetHash` would be more accurately
  named `lastSettledRuleSetHash`, because stable empty plans are settled without
  a delivered snapshot. Rename only if the patch stays readable; otherwise add a
  comment explaining the broadened meaning.
- Log blocked plans at `V(1)` with `FormatResolveMisses(...)`. Avoid
  default-verbosity logs on every periodic reconcile; catalog transition logs
  already cover the noisy discovery edges.
- Do not mark a blocked plan as pending. Pending is for a snapshot that should be
  emitted once a reconciler exists, not for a plan that is not yet authoritative.

## Test plan

Tests map onto the tiers, so you can land the Fix and its three tests and drop
`Serial` without ever building Refinement A or B. Add them near
[rule_change_snapshot_test.go](../../internal/watch/rule_change_snapshot_test.go):

**Fix:**

- `TestSnapshotTargets_DiscoveryUnavailableKeepsDeliveredHash`: seed a delivered
  configmaps plan, swap the manager catalog to unavailable, call
  `snapshotTargetsNeedingDelivery()`, and assert the target is not selected and
  `lastDeliveredRuleSetHash` is not pruned.
- `TestSnapshotTargets_DiscoveryRecoveryDoesNotResnapshotUnchangedPlan`: after
  the unavailable-catalog pass above, restore the common catalog and assert the
  unchanged target is still not selected.
- `TestSnapshotTargets_RuleRemovalPrunesDeliveredHash`: remove the rule from the
  RuleStore and assert the remembered hash is pruned. This guards against
  confusing "blocked" (state 4) with "deleted" (state 1).

**Refinement A:**

- `TestSnapshotTargets_MixedResolvedAndBlockingMissDoesNotSnapshotPartialPlan`:
  give one target a resolved GVR plus a discovery-degraded lookup. Assert the
  target is not selected and no hash is advanced.
- `TestSnapshotTargets_BlockedTargetDoesNotAffectOtherTargets`: target A is
  blocked by discovery, target B changes from configmaps to configmaps+secrets.
  Assert B is selected and A's remembered state is preserved.

**Refinement B:**

- `TestSnapshotTargets_StableEmptyPlanSettlesAndRecoverySnapshots`: seed a
  delivered configmaps plan, change the rule to a definitely not-served resource,
  assert no snapshot is selected but the settled hash changes, then change back
  to configmaps and assert the target is selected.

The e2e follow-up is to remove `Serial` from `Manager CRD Lifecycle` only after
these unit tests pass and at least one parallel CI confidence window shows that
CRD install/delete no longer drags unrelated GitTargets into snapshots.
