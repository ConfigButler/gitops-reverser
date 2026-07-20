# PR 1 — the resync sweep must be scoped by namespace, not only by GVR

> Phase 1 of [source-namespace addressing](README.md). **Depends on:** nothing.
> **Blocks: every other PR in this folder.** Bug fix — no API change, no CRD regeneration.
>
> This is the prerequisite that makes namespace fan-out safe. Until it lands, any change that lets
> one GitTarget watch a GVR in more than one namespace **deletes Git content**.

## The defect

A per-namespace replay produces a `desired` set covering **one** namespace, but the resulting
mark-and-sweep is scoped by **(group, resource) only**. Every managed document of that type in every
*other* namespace is therefore absent from `desired`, and a sweep deletes what is absent.

The scope is dropped in one place. `targetWatchSpecs` already builds per-namespace watch keys
([target_watch.go:211-226](../../../internal/watch/target_watch.go#L211-L226)):

~~~go
key := targetWatchKey{GVR: wt.GVR, Namespace: ns}   // namespace is known here
~~~

but `enqueueReplayResync` passes only the GVR onward
([target_watch.go:575-590](../../../internal/watch/target_watch.go#L575-L590)):

~~~go
resultCh, enqueued, err := m.EventRouter.enqueueScopedResync(
    ctx, gitDest, key.GVR, desired, revision, false)   // key.Namespace dropped
~~~

`enqueueScopedResync` then sets `scope := gvr`
([event_router.go:182-196](../../../internal/watch/event_router.go#L182-L196)), and `resyncPlan`
builds a predicate that never looks at the namespace
([resync_flush.go:465-482](../../../internal/git/resync_flush.go#L465-L482)):

~~~go
inScope := func(ri types.ResourceIdentifier) bool {
    return ri.Group == gvr.Group && ri.Resource == gvr.Resource
}
~~~

`ri` is a `types.ResourceIdentifier`, which **already carries `Namespace`**. The information is
present at both ends and discarded in the middle.

## Why it is latent today and live the moment anything else lands

It cannot fire today because a GitTarget can only ever watch one namespace per GVR:
`WatchRule.targetRef` is a `LocalTargetReference` with no namespace field, so every WatchRule using a
target lives in that target's namespace, and a ClusterWatchRule's `""` key collapses the whole type
to all-namespaces (which is a correct whole-GVR sweep). One named namespace, or none.

Each of the remaining PRs breaks that invariant, independently:

| PR | New source of multi-namespace fan-out on one GVR |
|---|---|
| [PR 2](pr2-stream-scope-collapse.md) | Named and cluster-wide selections become distinct concurrent streams for the same GVR. |
| [PR 4](pr4-source-namespace-field.md) | Two WatchRules in the target's namespace can carry different `sourceNamespace` values. |
| [PR 5](pr5-clusterwatchrule-source-ceiling.md) | A declared ceiling expands one cluster-wide selection into N per-namespace selections. |

So this is not a defect to fix opportunistically alongside the feature. It is the load-bearing floor
under all three, and the failure mode is silent data loss in a tenant's repository — a replay for
`team-a` removing `team-b`'s manifests of the same type.

## The fix

**Thread the namespace through the scope, and match on it.** Concretely:

1. Give the scoped-resync request a namespace alongside its GVR — `enqueueScopedResync` takes
   `key targetWatchKey` (or an explicit namespace) instead of a bare `gvr`, and carries it into
   `git.ResyncRequest`.
2. Extend `resyncPlan`'s predicate: when the scope names a namespace, `inScope` requires
   `ri.Namespace == scope.Namespace` in addition to group and resource. An empty scope namespace
   keeps today's whole-GVR meaning, which is what a genuinely cluster-wide stream needs.
3. Audit every other `enqueueScopedResync` caller for the same drop. The `heal: true` path and any
   other scoped-resync producer must pass a namespace consistent with the `desired` set they built,
   or they inherit this bug.

The invariant to hold, and to state in the code comment: **the sweep scope must be exactly the scope
the `desired` set was gathered over.** A `desired` narrower than its sweep scope deletes; a `desired`
wider than its sweep scope silently leaves content unmanaged. This is the rule that was violated.

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

## Tests

- **Multi-namespace replay does not delete siblings.** A GitTarget managing one GVR in `team-a` and
  `team-b`; replay only `team-a`. `team-b`'s manifests must survive untouched. This is the test that
  fails today and is the whole point of the PR.
- **Scoped sweep still works within its namespace.** An object removed from `team-a` while `team-a`
  replays is still swept — the fix must not turn the sweep off, only narrow it.
- **Cluster-wide scope keeps whole-GVR semantics.** A genuinely cluster-wide stream (empty scope
  namespace) still sweeps every namespace for its type, so PR 2's cluster-wide half is unaffected.
- **Scope/desired agreement:** a unit-level assertion that the namespace in the resync request equals
  the namespace of the watch key that produced `desired`. This is the invariant above, asserted
  directly, so a future caller that drops it again fails here rather than in a tenant's repo.
- **Revocation:** whichever semantics are chosen above, assert them — that a namespace removed from
  the watch set leaves its manifests present (recommended), and that no sweep is triggered by the
  removal itself.

## Done when

- A scoped resync carries a namespace end to end, and `inScope` honours it.
- Multi-namespace replay is proven non-destructive by test.
- Retention-on-revocation is decided, documented, and tested.
- `task lint`, `task test`, `task test-e2e` pass.
