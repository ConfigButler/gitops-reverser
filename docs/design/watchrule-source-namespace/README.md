# Source-namespace addressing and per-target source scope

> **Design in six PRs.** PRs 1–3 are landed. PR 4 is implemented and green in an open pull request,
> but its top-level `sourceNamespace` API is superseded and must not be released. PR 5 establishes
> deletion safety as a separate rollback-floor release; PR 6 reshapes the unshipped PR-4 work and
> makes ClusterWatchRule cluster-only. Index: [INDEX.md](../../INDEX.md).

## Decision

The selected end state is the two-object model:

- **WatchRule** is the namespaced source-resource surface. `spec.rules[].namespace` is omitted for
  the rule's own namespace, names one authorized source namespace, or is `"*"` for every namespace
  its GitTarget admits.
- **ClusterWatchRule** is the cluster-scoped source-resource surface. It has no `scope` field.
- **GitTarget** owns both the destination's source-namespace allow-list and the deletion policy.

This deliberately removes platform-authored namespaced mirroring without a WatchRule in the tenant
namespace. A platform administrator can still manage the manifest, but must put it in that namespace.
If that placement is unacceptable for a deployment, stop before PR 6 and design a separate API for
that capability.

## The model

Authorization follows the existing references:

~~~text
WatchRule  ──uses──>  GitTarget  ──uses──>  ClusterProvider
    │                     │                       │
    │                     │                       └ permits this target namespace
    │                     └ admits source namespaces for this destination
    └ selects resource types and source namespaces
~~~

- `GitTarget.spec.allowedSourceNamespaces` is a destination-owned, exhaustive allow-list for
  WatchRule source namespaces. A declared policy has no self-namespace exception.
- `ClusterProvider.spec.allowWatchRuleSourceNamespaceOverride` is the platform-admin delegation that
  permits an admitted GitTarget to authorize a WatchRule outside its own namespace.
- `GitTarget.spec.prune.mode` controls deletion: `never`, `onEvent` (default), or `always`.

Cluster-scoped objects have no namespace. A ClusterWatchRule therefore receives every selected
cluster-scoped object its source credential can read; tenant isolation for such objects requires
separate source credentials/ClusterProviders, not a namespace allow-list.

## Why the scope lives on GitTarget

A GitTarget already binds one source cluster to one Git destination, branch, and path. What may reach
that destination is therefore a GitTarget property, not a property of an individual requesting rule.
A declared `allowedSourceNamespaces` policy is exhaustive: it must list the WatchRule's own namespace
as well when that legacy WatchRule is to continue writing to this target.

The policy does not apply to ClusterWatchRule after PR 6, because a cluster-only rule has no namespace
to bound. This makes the audit question straightforward: either read the WatchRule's selected
namespace and target policy, or recognize a ClusterWatchRule as intentionally cluster-global.

## Implementation phases

| # | PR | Scope | Status |
|---|---|---|---|
| 1 | [Namespace-scoped resync](pr1-namespace-scoped-resync.md) | A per-namespace replay cannot sweep another namespace's manifests of the same type. | landed |
| 2 | [Stream-scope collapse](pr2-stream-scope-collapse.md) | A cluster-wide stream cannot silently widen a co-resident named stream. | landed |
| 3 | [ClusterWatchRule target admission](pr3-clusterwatchrule-target-admission.md) | A ClusterWatchRule cannot attach to a GitTarget its ClusterProvider does not admit. | landed |
| 4 | [Original sourceNamespace implementation](pr4-source-namespace-field.md) | Open, green implementation of the authorization model and source-scope service. Its top-level API is superseded; do not release it unchanged. | open, superseded API |
| 5 | [GitTarget deletion safety](pr5-gittarget-deletion-safety.md) | Add `prune.mode` and make resync sweep opt-in (`always`). Release independently as the rollback floor. | planned |
| 6 | [Scope by kind](pr6-cluster-scope-only.md) | Rework PR-4 authorization for `rules[].namespace`, remove ClusterWatchRule `scope`, refuse stored namespaced rules, and migrate cross-kind manifests. | planned, breaking |

## Release order and the open PR 4

**Do not merge or release the open PR 4 unchanged just because it is green.** Its reviewed work is
valuable, but the only user-visible field it adds is known to be the wrong shape and the allow-list is
not a complete boundary while ClusterWatchRule can still select namespaced resources.

Recommended handling:

1. Preserve the current PR 4 branch and test history as the implementation baseline for its gate,
   conditions, bootstrap enforcement, source-scope snapshot, and reactivity wiring.
2. Land and release PR 5 from main independently. Upgrade every controller instance to it; this is
   the minimum safe rollback version for the later API migration.
3. Close the present PR 4 as superseded, or retarget it explicitly as the PR-6 branch. Rebase or
   cherry-pick its reusable implementation after PR 5, then replace the top-level field with rule-item
   namespace resolution and remove ClusterWatchRule `scope` in the same PR.
4. Do not cut a release containing the original top-level `sourceNamespace` field. Release PR 6 only
   after its migration preflight and stored-object refusal are complete.

Merging the current PR 4 only as an unreleased development checkpoint is technically possible, but it
creates a dead public field and makes the eventual review harder. Closing or retargeting it is the
cleaner path.

## Deletion safety

The product has two deletion paths: explicit source DELETE events and inferred mark-and-sweep drops
during resync. Scope mistakes threaten only the latter. PR 5 defaults targets to `prune.mode: onEvent`:
explicit source DELETE events remain mirrored, while a narrowed or incorrect desired set leaves prior
Git documents untouched. `always` restores full desired-state convergence; `never` creates an archive
that does not remove documents.

PR 5 is intentionally narrow. It does not add a deletion-count or percentage guard. If experience
shows that genuine large delete cascades need another control, `prune` is already an object and can
later gain a non-breaking field such as `maxDeletesPerCommit`.

## Compatibility

This is preliminary v1alpha3 API. PR 5 changes the default effective sweep behavior in the safe
direction. PR 6 is deliberately breaking:

- `WatchRule.spec.sourceNamespace` moves to `WatchRule.spec.rules[].namespace` before it has reached
  a release.
- `ClusterResourceRule.scope` and the public `ResourceScope` enum are removed.
- A legacy namespaced ClusterWatchRule cannot be converted automatically into a WatchRule: the move is
  cross-kind and `namespace: "*"` requires an explicit target policy where legacy cluster rules did
  not.

PR 6 supplies a dry-run migration preflight. It must fail rather than silently narrow a target that
has no compatible `allowedSourceNamespaces` policy. A controller rollback is supported only to the
released PR-5 version while PR-6 manifests exist; rolling back farther is unsupported.

## Deferred work

- Selector-backed `rules[].namespace: "*"` is deferred. The first wildcard release supports
  names-only allow-lists; selector fan-out needs independent invalidation and retaining semantics.
- A source-delete volume guard is deferred. An absolute count alone does not protect a small target's
  whole folder, so it is better added later with a fully specified approval model if needed.
- A platform-owned, cross-namespace namespaced-watch API is deferred unless a real deployment needs
  it.
