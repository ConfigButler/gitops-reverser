# PR 1 — the resync sweep must be scoped by namespace, not only by GVR

> Phase 1 of [source-namespace addressing](README.md). **Depends on:** nothing.
> **Blocked every other PR in this folder.** Bug fix — no API change, no CRD regeneration.
>
> **Status: landed.** This was the prerequisite that makes namespace fan-out safe: until it landed,
> any change that let one GitTarget watch a GVR in more than one namespace would **delete Git
> content**. The rest of this page is the record of what was wrong and what shipped; sections below
> are past-tense by design, so a regression is recognisable against them.

## The defect, as it stood before this PR

A per-namespace replay produced a `desired` set covering **one** namespace, but the resulting
mark-and-sweep was scoped by **(group, resource) only**. Every managed document of that type in every
*other* namespace was therefore absent from `desired`, and a sweep deletes what is absent.

The scope was dropped in one place. `targetWatchSpecs` already built per-namespace watch keys:

~~~go
key := targetWatchKey{GVR: wt.GVR, Namespace: ns}   // namespace is known here
~~~

but `enqueueReplayResync` passed only the GVR onward:

~~~go
resultCh, enqueued, err := m.EventRouter.enqueueScopedResync(
    ctx, gitDest, key.GVR, desired, revision, false)   // key.Namespace dropped
~~~

`enqueueScopedResync` then set `scope := gvr`, and `resyncPlan` built a predicate that never looked
at the namespace:

~~~go
inScope := func(ri types.ResourceIdentifier) bool {
    return ri.Group == gvr.Group && ri.Resource == gvr.Resource
}
~~~

`ri` is a `types.ResourceIdentifier`, which **already carried `Namespace`**. The information was
present at both ends and discarded in the middle.

## What the tree looks like now

The scope travels end to end as one value:

- `git.ResyncScope` carries GVR **and** Namespace, and owns the match predicate
  ([types.go](../../../internal/git/types.go)) — `ResyncRequest` and `PendingWrite` both hold it, so
  the two halves of a scope cannot be separated in transit.
- `resyncScopeForWatchKey` is the single watch-key → scope conversion
  ([event_router.go:207-213](../../../internal/watch/event_router.go#L207-L213)), and
  `enqueueReplayResync` passes `resyncScopeForWatchKey(key)`
  ([target_watch.go:585-586](../../../internal/watch/target_watch.go#L585-L586)), so `key.Namespace`
  is preserved rather than dropped.
- `resyncPlan` matches through `scope.Matches`
  ([resync_flush.go:475](../../../internal/git/resync_flush.go#L475)); an empty `Namespace` keeps
  the whole-GVR meaning a genuinely cluster-wide stream needs.
- `resyncHealKey` separates namespaces, so a parked heal for one no longer replaces another's.

## Why it was latent, and live the moment anything else lands

It could not fire before this PR because a GitTarget could only ever watch one namespace per GVR:
`WatchRule.targetRef` is a `LocalTargetReference` with no namespace field, so every WatchRule using a
target lives in that target's namespace, and a ClusterWatchRule's `""` key collapses the whole type
to all-namespaces (which is a correct whole-GVR sweep). One named namespace, or none.

Each of the remaining PRs breaks that invariant, independently — which is why this one went first:

| PR | New source of multi-namespace fan-out on one GVR |
|---|---|
| [PR 2](pr2-stream-scope-collapse.md) | Named and cluster-wide selections become distinct concurrent streams for the same GVR. |
| [PR 4](pr4-source-namespace-field.md) | Two WatchRules in the target's namespace can carry different `sourceNamespace` values. |
| [PR 5](pr5-clusterwatchrule-source-ceiling.md) | A declared ceiling expands one cluster-wide selection into N per-namespace selections. |

So this was not a defect to fix opportunistically alongside the feature. It is the load-bearing floor
under all three, and the failure mode is silent data loss in a tenant's repository — a replay for
`team-a` removing `team-b`'s manifests of the same type.

## The fix that shipped

**Thread the namespace through the scope, and match on it.** Concretely:

1. The scoped-resync request carries a namespace alongside its GVR — `enqueueScopedResync` takes a
   `git.ResyncScope` instead of a bare `gvr`, and carries it into `git.ResyncRequest` and
   `PendingWrite`.
2. `resyncPlan`'s predicate became `scope.Matches`: when the scope names a namespace it requires
   `ri.Namespace == scope.Namespace` in addition to group and resource. An empty scope namespace
   keeps the whole-GVR meaning, which is what a genuinely cluster-wide stream needs.
3. Every other `enqueueScopedResync` caller was audited for the same drop. The `heal: true` path and
   any other scoped-resync producer passes a scope consistent with the `desired` set it built, and
   `resyncHealKey` includes the namespace so two namespaces' parked heals no longer collide.

The invariant now held, and stated in the code comment: **the sweep scope must be exactly the scope
the `desired` set was gathered over.** A `desired` narrower than its sweep scope deletes; a `desired`
wider than its sweep scope silently leaves content unmanaged. This is the rule that was violated.

Having one conversion function (`resyncScopeForWatchKey`) rather than a namespace parameter threaded
by hand is the part that makes it stay fixed: there is no second place for a caller to forget.

### Alternative considered: coalesce into one authoritative snapshot

Rather than making the scope finer, make `desired` wider: gather every watched namespace for a GVR
into a single snapshot and keep sweeping by GVR. This is attractive because it yields one
authoritative picture per type and removes a class of partial-scope reasoning entirely.

It is rejected for this PR because it couples the replay lifecycles of independent streams: one
namespace's watch failing or resuming late would hold up or falsify the whole type's snapshot, and
streams start and stop independently by design. Scope-narrowing is also the smaller, more directly
testable change. Revisit coalescing if per-namespace replay volume becomes the problem.

## Revocation leaves prior content — a decision, not an oversight

Stopping a stream does not remove what it already wrote. When a namespace leaves a watch set — a
[PR 5](pr5-clusterwatchrule-source-ceiling.md) ceiling tightening, a WatchRule deletion, a revoked
label — its manifests remain in Git.

**Recommended: retain, and make it visible.** Deleting a tenant's manifests as a side effect of a
policy edit is destructive, hard to undo in the moment, and easy to trigger by accident (a typo in a
selector). Retention is also the safe direction under the failure mode in
[PR 5](pr5-clusterwatchrule-source-ceiling.md#2b-unknown-is-not-empty): if an unavailable selector were
ever read as an empty allow-list, a sweep-on-revocation would erase the target's entire namespaced
content.

The cost is real and must be documented rather than glossed: after a revocation, Git holds manifests
from a namespace the policy no longer admits, and no automatic process removes them. Removing them is
a deliberate operator action. Whichever way this is settled, it must be settled explicitly and
covered by a test — the failure to avoid is discovering the behavior in production.

## Tests that shipped

- **`TestResync_NamespaceScopedSweepLeavesSiblingNamespacesAlone`** — a GitTarget managing one GVR in
  `team-a` and `team-b`, replaying only `team-a`; `team-b`'s manifests survive untouched. This is the
  test that failed before the fix and is the whole point of the PR.
- **`TestResync_NamespaceScopedSweepStillDropsOrphansInItsOwnNamespace`** — an object removed from
  `team-a` while `team-a` replays is still swept. The fix narrows the sweep; it must not turn it off.
- **`TestResync_ClusterWideScopeStillSweepsEveryNamespace`** — a genuinely cluster-wide stream (empty
  scope namespace) still sweeps every namespace for its type, so PR 2's cluster-wide half is
  unaffected.
- **`TestResyncScopeForWatchKey_CarriesBothHalvesOfTheScope`** — the scope/`desired` agreement
  invariant asserted directly, so a future caller that drops the namespace again fails here rather
  than in a tenant's repo.
- **`TestResyncScope_MatchesRespectsTypeAndNamespace`**,
  **`TestResyncHealKey_SeparatesNamespacesOfTheSameType`**,
  **`TestResyncScope_StringIsNilSafeAndNamesTheNamespace`** — the predicate, the heal-key split, and
  nil-safe formatting.

Verified by reverting the namespace half of `ResyncScope.Matches`:
`TestResync_NamespaceScopedSweepLeavesSiblingNamespacesAlone` and the sibling-namespace row of
`TestResyncScope_MatchesRespectsTypeAndNamespace` both fail without the fix.

## Done — with one item carried forward

- ✅ A scoped resync carries a namespace end to end, and the plan predicate honours it.
- ✅ Multi-namespace replay is proven non-destructive by test.
- ✅ `task lint`, `task test`, `task test-e2e` pass.
- ⏭ **Retention-on-revocation is documented above but not yet enforced by a test.** Nothing in this
  PR can revoke a namespace — no code path removes one from a watch set yet — so the test has no
  subject until [PR 5](pr5-clusterwatchrule-source-ceiling.md) introduces ceiling tightening. It is
  carried as `TestCeiling_UnknownScopeRetainsPreviousAndDoesNotSweep` and the revocation envtest in
  PR 5's plan. Recording it here rather than silently dropping it.
