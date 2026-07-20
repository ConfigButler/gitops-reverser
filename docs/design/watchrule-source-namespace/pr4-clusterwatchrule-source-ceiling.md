# PR 4 — a declared allowedSourceNamespaces bounds ClusterWatchRule too

> Phase 4 of [source-namespace addressing](README.md). **Depends on:** PR 3 (the field) and PR 1
> (stream scoping). **Must ship with PR 3** — see the
> [release gate](README.md#implementation-phases). No new API fields; this changes what an existing
> field governs.

## Why this is launch-set, not follow-up

A multi-tenant deployment needs a ClusterWatchRule per tenant GitTarget **from day one**, because a
ClusterWatchRule is the only way to select cluster-scoped types and every tenant needs their CRDs
mirrored. So the rule kind that can bypass a WatchRule-only allow-list is not a rare edge — it is in
every tenant's baseline configuration.

Without this PR, `allowedSourceNamespaces` is enforced on the rule kind that cannot bypass it and
unenforced on the one that can. The operator's answer to *"will this stream objects from namespaces
outside my allow-list?"* becomes "hand-audit every ClusterWatchRule and hope", and the audit has to
be repeated on every change by anyone with cluster-admin. With this PR the answer is a field you can
read off the GitTarget.

The workaround — carefully writing ClusterWatchRules that contain only `scope: Cluster` rules — does
work today. It just cannot be *verified* from the GitTarget, which is where a tenant boundary should
be legible.

## The invariant

`ClusterWatchRule.targetRef` is a `NamespacedTargetReference` with a **required** namespace
([clusterwatchrule_types.go:22-44](../../../api/v1alpha3/clusterwatchrule_types.go#L22-L44)), so it
reaches across namespaces by design, and `collectClusterWatchRuleSelections` hardcodes
`namespace: ""` for every rule
([watched_type_resolver.go:317-318](../../../internal/watch/watched_type_resolver.go#L317-L318)).
Left alone, a ClusterWatchRule delivers every source namespace into its target's Git destination
regardless of what that target declared.

So the allow-list is stated over the **destination**, and holds for both rule kinds:

> A source namespace may be mirrored into a GitTarget only if it is the requesting WatchRule's own
> namespace, or the GitTarget's declared `allowedSourceNamespaces` admits it.

Because a ClusterWatchRule has no own namespace, the kinds differ only in the legacy carve-out when
the field is **undeclared**:

| `GitTarget.allowedSourceNamespaces` | WatchRule | ClusterWatchRule (`scope: Namespaced` rules) |
|---|---|---|
| Undeclared | Own namespace only (legacy) | All source namespaces (legacy) |
| Declared | Own namespace, plus what the policy admits | Exactly what the policy admits |

One meaning for the field across both kinds — declared means ceiling, absent means no new grant — so
nobody has to remember which rule kind inverts it, and no existing ClusterWatchRule changes behavior
until a target owner declares a policy.

### This is a restriction, so no delegation flag

`allowWatchRuleSourceNamespaceOverride` gates *granting* a WatchRule a foreign namespace. The ceiling
only ever narrows, so it applies whether the flag is true or false. Gating a restriction behind a
delegation flag would mean an admin must grant extra authority in order to reduce scope.

### Three precisions the implementation must not get wrong

- **Cluster-scoped rules are exempt.** A namespace allow-list cannot constrain Nodes, CRDs, or
  ClusterRoles. The ceiling applies per `ClusterResourceRule`, only where `scope: Namespaced`. A
  ClusterWatchRule mixing both scopes keeps its cluster-scoped streams intact — and since the CRD use
  case above *is* the cluster-scoped half, an over-broad implementation that refuses or narrows the
  whole rule breaks the exact configuration this PR exists to serve.
- **Narrowing is not a refusal.** A declared ceiling admitting nothing leaves a namespaced-scope
  ClusterWatchRule selecting no namespaces. That is a correct outcome, not a terminal failure: it
  must not set `Stalled=True`. Report it through the existing `ResourcesResolved` surface and the
  reason below, so a dead rule is still visible.
- **The ceiling does not partition cluster-scoped objects at all.** Stated plainly in the
  [overview](README.md#what-the-ceiling-does-not-do): every tenant selecting CRDs gets every CRD the
  credential can read. If tenants must not see each other's cluster-scoped objects, the answer is one
  ClusterProvider and credential per tenant, not this field.

### Status

The narrowing is a scope change, not an authorization refusal, so it reuses the domain condition from
PR 3 rather than adding a second one. On ClusterWatchRule, `SourceNamespaceAuthorized=True` with
reason `AllSourceNamespaces` when no ceiling applies, and `SourceNamespacesNarrowed` when one does.
The `SourceAuthorized` printer column is added to ClusterWatchRule as well, so the two rule kinds
read the same way in `kubectl get -o wide`.

## Implementation

### 1. Apply the ceiling in selection

`collectClusterWatchRuleSelections`
([watched_type_resolver.go:306-323](../../../internal/watch/watched_type_resolver.go#L306-L323))
hardcodes `namespace: ""`. When the referenced GitTarget declares `allowedSourceNamespaces`, expand
each `scope: Namespaced` rule into **one selection per admitted namespace** instead of a single `""`
selection. Leave `scope: Cluster` rules emitting `""`.

**Prefer expansion over filtering events at the read site.** An expanded selection carries the scope
through the plan hash, the informers, **and** the resync path (`snapshotGVRsFromTable`,
[scope_resolve.go:180](../../../internal/watch/scope_resolve.go#L180)) for free. A read-site filter
must be repeated at each of those and is silently wrong if one is missed — and it would also mean an
unfiltered LIST/WATCH over all namespaces, so the data crosses into the process before being dropped.

Two consequences worth knowing:

- A name-based policy expands statically; a **selector**-based one makes the namespace set depend on
  the source-cluster Namespace informer PR 3 introduces, and can produce many streams on a large
  cluster. That is the cost of the safe direction.
- A declared policy emits no `""` key at all, so the [PR 1](pr1-stream-scope-collapse.md) collapse
  cannot trigger for that target. PR 1 still governs the undeclared case, which remains the common
  one.

### 2. Fingerprint the resolved scope

`clusterWatchRuleFingerprint`
([watched_type_resolver.go:491-500](../../../internal/watch/watched_type_resolver.go#L491-L500))
has **no** `src=` component, on the assumption that a ClusterWatchRule is always all-source-namespaces.
Step 1 falsifies that.

Its input is not on the rule object at all: the effective namespace set comes from the GitTarget's
policy and, for a selector, from live source-cluster Namespace labels. **Hash the resolved set.**
Without this, declaring or tightening `allowedSourceNamespaces` leaves the ClusterWatchRule's streams
running at their old width with no visible symptom — the easiest failure in the workstream to ship,
because the rule object itself did not change and every diff looks correct.

### 3. Reactivity

| Input changes | Required reaction | Status |
|---|---|---|
| GitTarget `allowedSourceNamespaces` | Re-resolve the ceiling and replan the ClusterWatchRule's streams. | Not wired — the ClusterWatchRule controller performs no GitTarget-driven re-resolution. Needs a GitTarget → ClusterWatchRules mapper. |
| Source-cluster Namespace labels | Re-resolve selector-based ceilings. | Extends the per-source-cluster informer PR 3 adds, to map to ClusterWatchRules as well. |
| ClusterProvider `allowedNamespaces` | Already handled by [PR 2](pr2-clusterwatchrule-target-admission.md)'s mapper. | — |

## Test plan

These prove the invariant. Without them the allow-list is enforced only where it cannot be bypassed.

- **`TestCollectClusterWatchRuleSelections_DeclaredCeilingNarrowsClusterWideStream`** — a
  ClusterWatchRule with a `scope: Namespaced` rule against a GitTarget declaring
  `allowedSourceNamespaces: {names: [repo-config]}` emits selections for `repo-config` **only**, and
  emits **no** `""` selection. Assert the absence of the `""` key directly: `""` present alongside
  `repo-config` reads as "we did narrowing" in a diff while behaving as all-namespaces at runtime,
  because `SnapshotNamespaces()` short-circuits on it
  ([watched_type_table.go:78-92](../../../internal/watch/watched_type_table.go#L78-L92)).
- **`TestCollectClusterWatchRuleSelections_UndeclaredCeilingStaysClusterWide`** — the upgrade-safety
  twin. No policy on the GitTarget means today's single `""` selection, unchanged.
- **`TestCollectClusterWatchRuleSelections_CeilingSparesClusterScopedRules`** — a ClusterWatchRule
  mixing `scope: Cluster` (CRDs) and `scope: Namespaced` rules under a declared ceiling keeps the
  cluster-scoped stream at `""` while the namespaced one narrows. This is the day-one multi-tenant
  shape; it guards the over-correction of narrowing or refusing the whole rule.
- **`TestCollectClusterWatchRuleSelections_CeilingAdmittingNothingIsNotStalled`** — a declared policy
  matching no namespace yields no namespaced selections, `SourceNamespaceAuthorized=True` with reason
  `SourceNamespacesNarrowed`, and **not** the Failed trio.
- **`TestClusterWatchRuleFingerprint_ChangesWithResolvedSourceScope`** — two otherwise-identical
  ClusterWatchRules whose GitTargets declare different `allowedSourceNamespaces` fingerprint
  differently, and tightening a policy changes the fingerprint of an unchanged rule object. The rule
  spec is byte-identical across both cases, so nothing else in the suite can catch a missing `src=`
  component.
- **Revocation, envtest** — a running ClusterWatchRule under `allowedSourceNamespaces:
  [repo-config, team-a]`, narrowed to `[repo-config]`, stops the `team-a` stream within a bounded
  time. Not merely re-renders status.
- **Selector reactivity, envtest** — removing the matching label from a source-cluster Namespace
  revokes that namespace's stream for a ClusterWatchRule under a selector ceiling.
- **End-to-end, the one that proves the boundary** — the day-one multi-tenant shape: a
  ClusterWatchRule selecting CRDs (`scope: Cluster`) **and** ConfigMaps (`scope: Namespaced`),
  targeting acme's GitTarget which declares `allowedSourceNamespaces: [repo-config]`. Create a
  ConfigMap in `repo-config` and one in `tenant-zen`, and a CRD. Assert all three: `repo-config/…`
  appears, the CRD appears, and `tenant-zen/…` is **never** written. Assert the negative against a
  real commit — every unit-level assertion above is on the plan, not on what reached Git.

## Done when

- A declared ceiling narrows namespaced ClusterWatchRule streams and leaves cluster-scoped ones
  intact.
- Tightening a ceiling stops the removed namespaces' streams without touching the rule object.
- The e2e above passes, including its negative assertion.
- `task lint`, `task test`, `task test-e2e` pass.
