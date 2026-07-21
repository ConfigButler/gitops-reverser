# Source-namespace addressing and per-target source scope

> **Design in five PRs.** PRs 1–3 are landed. The current branch becomes **PR 4**, the selected
> scope-by-kind design. **PR 5** is the deletion-safety change, implemented in the PR immediately
> after it. **No release may be cut between the two merges** — the first release containing PR 4 also
> contains PR 5. Review findings still open against PR 4 are tracked in
> [PR 4 review follow-ups](pr4-review-followups.md). Index: [INDEX.md](../../INDEX.md).

## Decision

The selected end state is the two-object model:

- **WatchRule** is the namespaced source-resource surface. `spec.rules[].sourceNamespace` is omitted
  for the rule's own namespace, names one authorized source namespace, or is `"*"` for every
  namespace its GitTarget admits — including a live, selector-resolved set.
- **ClusterWatchRule** is the cluster-scoped source-resource surface. It has no scope choice.
- **GitTarget** owns both the destination's source-namespace allow-list and the deletion policy.

This deliberately removes platform-authored namespaced mirroring without a WatchRule in the tenant
namespace. A platform administrator can still manage the manifest, but must put it in that namespace.
If that placement is unacceptable for a deployment, stop before PR 4 and design a separate API for
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
- `ClusterProvider.spec.allowSourceNamespaceOverride` is the platform-admin delegation that
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

### No self-namespace exception

An omitted `rules[].sourceNamespace` resolves to the WatchRule's own namespace. It is allowed without
a source-namespace policy only while the GitTarget declares no policy. Once a policy is declared, that
namespace must be explicitly admitted just like every other source namespace.

A declared policy is also how a destination says **every** source namespace: a present-but-empty
`selector: {}` admits all of them, live, and is the replacement for the removed
`ClusterWatchRule` + `scope: Namespaced` capability.

The policy does not apply to ClusterWatchRule after PR 4, because a cluster-only rule has no namespace
to bound. This makes the audit question straightforward: either read the WatchRule's selected
namespace and target policy, or recognize a ClusterWatchRule as intentionally cluster-global.

## Implementation phases

| # | PR | Scope | Status |
|---|---|---|---|
| 1 | [Namespace-scoped resync](pr1-namespace-scoped-resync.md) | A per-namespace replay cannot sweep another namespace's manifests of the same type. | landed |
| 2 | [Stream-scope collapse](pr2-stream-scope-collapse.md) | A cluster-wide stream cannot silently widen a co-resident named stream. | landed |
| 3 | [ClusterWatchRule target admission](pr3-clusterwatchrule-target-admission.md) | A ClusterWatchRule cannot attach to a GitTarget its ClusterProvider does not admit. | landed |
| 4 | [Scope by kind](pr4-cluster-scope-only.md) | Rework the unshipped source-namespace work for `rules[].sourceNamespace`, narrow ClusterWatchRule to cluster scope, refuse stored namespaced rules, and document the cross-kind migration. | current branch; breaking |
| 5 | [GitTarget deletion safety](pr5-gittarget-deletion-safety.md) | Add `prune.mode` and make resync sweep opt-in (`always`). | next PR; ships in the same release as PR 4 |

The discarded top-level `sourceNamespace` plan is retained as a
[historical implementation baseline](historical-top-level-source-namespace-baseline.md). Its gate,
conditions, bootstrap enforcement, source-scope snapshot, and reactivity work are reusable; its public
field is not. [PR 4's keep/replace map](pr4-cluster-scope-only.md#existing-pr-4-work-to-keep) defines
the exact rework, including the retained ClusterProvider delegation flag; its
[closed decisions](pr4-cluster-scope-only.md#closed-design-decisions) resolve the former alternatives'
open questions.

## Working and release order

Keep the current branch and rework it in place as PR 4. Do not release a version containing the
top-level `WatchRule.spec.sourceNamespace` field.

1. Implement and review PR 4 on the current branch, then merge it. **`main` is now in a
   do-not-release window.**
2. Implement PR 5 in the next PR and merge it. The window closes.
3. Release both together, with the breaking change in the notes.

There is no release in which PR 4 exists without PR 5, so there is no PR-5 rollback floor to fall
back to. Rolling the controller back past that release while migrated manifests exist is
**unsupported**: the older controller both ignores `rules[].sourceNamespace` (resolving a rule to its
own namespace — a narrower desired set) and lacks `prune.mode` (so a resync sweeps). Remove or narrow
the affected WatchRules first.

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
direction. PR 4 is deliberately breaking:

- The unshipped `WatchRule.spec.sourceNamespace` field is replaced with
  `WatchRule.spec.rules[].sourceNamespace`; it never reaches a release.
- `ClusterResourceRule.scope` narrows to `Cluster` only. Both superseded fields stay in the schema for
  one release as **loud rejections** rather than being deleted, because a deleted field is silently
  pruned from a re-applied legacy manifest.
- A legacy namespaced ClusterWatchRule cannot be converted automatically into a WatchRule: the move is
  cross-kind and `sourceNamespace: "*"` requires an explicit target policy where legacy cluster rules
  did not.

There is no migration tool. The breaking change is carried by the release notes and
[UPGRADING.md](../../UPGRADING.md), which must state precisely what a target with no declared policy
admits, because the two halves pull in opposite directions: a WatchRule whose every item watches its
OWN namespace keeps working untouched (reason `LegacySourceNamespace`, no policy and no delegation
flag required), while every wildcard or cross-namespace item is denied. A conversion produces exactly
the second kind, so converting without also declaring `allowedSourceNamespaces` narrows what is
mirrored. Stating the denial unqualified would tell every existing operator their rules break on
upgrade, which is not true and is the more expensive error of the two.

## Deferred work

- Collapsing wildcard stream fan-out. A `"*"` item opens one stream per (type × admitted namespace);
  a cluster-wide stream carrying a namespace **set** in its resync scope would collapse that without
  widening the sweep. Tracked in [docs/TODO.md](../../TODO.md).
- A source-delete volume guard is deferred. An absolute count alone does not protect a small target's
  whole folder, so it is better added later with a fully specified approval model if needed.
- A platform-owned, cross-namespace namespaced-watch API is deferred unless a real deployment needs
  it.
