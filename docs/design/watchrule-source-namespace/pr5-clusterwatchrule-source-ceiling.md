# PR 5 — a declared allowedSourceNamespaces bounds ClusterWatchRule too

> Phase 5 of [source-namespace addressing](README.md). **Depends on:**
> [PR 4](pr4-source-namespace-field.md) (the field and the source-scope service),
> [PR 1](pr1-namespace-scoped-resync.md) (per-namespace expansion is unsafe until the sweep is
> namespace-scoped), and [PR 2](pr2-stream-scope-collapse.md). **Must ship with PR 4** — see the
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

> When a GitTarget declares `allowedSourceNamespaces`, the source namespaces mirrored into it are
> **exactly** those the policy admits, for every rule of every kind. When it declares none, each rule
> kind keeps its legacy scope.

There is **no implicit own-namespace exception** — a declared policy is exhaustive, including for a
WatchRule watching the namespace it lives in. The reasoning, and the authoring footgun it accepts,
are in [no self-namespace exception](README.md#no-self-namespace-exception); do not reintroduce the
carve-out here. Because a ClusterWatchRule has no own namespace, the kinds therefore differ only in
the legacy behavior when the field is **undeclared**:

| `GitTarget.allowedSourceNamespaces` | WatchRule | ClusterWatchRule (`scope: Namespaced` rules) |
|---|---|---|
| Undeclared | Own namespace only (legacy) | All source namespaces (legacy) |
| Declared | Exactly what the policy admits | Exactly what the policy admits |

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

The narrowing is a scope change, not an authorization refusal, so it reuses the domain condition
[PR 4](pr4-source-namespace-field.md#status-contract-kstatus-compatible) defines rather than adding a
second one. On ClusterWatchRule, `SourceNamespaceAuthorized=True` with
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
  the source-cluster Namespace informer [PR 4](pr4-source-namespace-field.md) introduces, and can
  produce many streams on a large cluster. That is the cost of the safe direction.
- A declared policy emits no `""` key **for its namespaced selections**, so the
  [PR 2](pr2-stream-scope-collapse.md) collapse cannot trigger between two namespaced streams on that
  target. It does **not** follow that the target emits no `""` key at all: a co-resident
  `scope: Cluster` rule still emits one, so a GVR selected both cluster-scoped and namespaced can
  still collapse. PR 2 governs that case, and the undeclared case, which remains the common one.

### 2. Put the resolved scope into table invalidation

`rulesFingerprint` is computed **only from compiled rules** — it iterates
`SnapshotWatchRules()` and `SnapshotClusterWatchRules()` and hashes their spec fields
([watched_type_resolver.go:463-500](../../../internal/watch/watched_type_resolver.go#L463-L500)) —
and it is what gates the table rebuild
([watched_type_resolver.go:88-96](../../../internal/watch/watched_type_resolver.go#L88-L96)).
`clusterWatchRuleFingerprint` has no `src=` component at all, on the assumption that a
ClusterWatchRule is always all-source-namespaces. Step 1 falsifies that assumption.

The consequence is bigger than a missing hash component, and it is the failure mode to design
against: the new ceiling's inputs — GitTarget policy and source-cluster Namespace labels — are **not
rule state**, so nothing about them reaches the fingerprint. A mapper that requeues the
ClusterWatchRule is therefore not sufficient on its own: reconciliation runs, the fingerprint is
unchanged, the rebuild is skipped, and the resident table keeps the old namespace set. The streams
carry on at their previous width and every diff looks correct, because the rule object genuinely did
not change.

**So carry a resolved-scope version into invalidation.** Either hash the resolved namespace set into
each rule's fingerprint, or add a separate source-scope generation to the rebuild trigger alongside
the rules fingerprint and the catalog generation. Hashing the resolved set is the smaller change and
composes with the existing gate; whichever is chosen, the test in the plan below asserts that an
unchanged rule object with a changed policy re-projects the table.

This is why [PR 4](pr4-source-namespace-field.md)'s source-scope service must expose resolution to
the resolver, not only to the reconciler: the fingerprint is computed in `internal/watch` and needs
the same answer the gate got.

### 2b. Unknown is not empty

An unresolvable selector must **never** be treated as a valid empty allow-list. The distinction is
load-bearing because narrowing has a data-plane consequence: an empty resolved set means "watch
nothing in any namespace", and combined with a resync it means Git content for those namespaces is
no longer in `desired`. A transient source-cluster outage read as "the policy admits nothing" is
therefore not merely a stopped stream — it is the input to a sweep. This is the sharpest reason
[PR 1](pr1-namespace-scoped-resync.md) lands first and why its recommended retain-on-revocation
semantics matter.

Required behavior when the resolved set is unknown — cache not synced, source cluster unreachable,
Namespace access denied for a selector policy:

- **Retain the current resolved scope.** Do not narrow, do not widen, and do not sweep. The last
  known-good scope keeps running.
- **Never synthesize an empty set.** "I could not evaluate" and "it admits nothing" must be different
  values all the way through the resolver, which is what the three-valued result in PR 4's
  source-scope service exists to provide.
- **Report it as non-terminal:** `SourceNamespaceAuthorized=Unknown` with reason
  `CheckingSourceNamespacePolicy` while a retryable error is being retried, and
  `SourceNamespacePolicyUnavailable` (still `Unknown`, still retained, **not** `Stalled`) when source
  Namespace access is denied outright for a selector policy. Exact-name entries in the same policy
  remain resolvable and keep working.

A permanently unavailable selector is a legitimate long-lived `Unknown`. Turning it into `Stalled`
would be a false claim that the operator knows the rule is wrong, and turning it into an empty set
would be destructive.

### 3. Reactivity

| Input changes | Required reaction | Status |
|---|---|---|
| GitTarget `allowedSourceNamespaces` | Re-resolve the ceiling and replan the ClusterWatchRule's streams. | Not wired — the ClusterWatchRule controller performs no GitTarget-driven re-resolution. Needs a GitTarget → ClusterWatchRules mapper. |
| Source-cluster Namespace labels | Re-resolve selector-based ceilings. | Extends the per-source-cluster informer [PR 4](pr4-source-namespace-field.md#reactivity-and-source-cluster-rbac) adds, to map to ClusterWatchRules as well. |
| ClusterProvider `allowedNamespaces` | Already handled by [PR 3](pr3-clusterwatchrule-target-admission.md)'s mapper. | — |

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
- **`TestWatchedTypeTable_RebuildsWhenOnlyThePolicyChanged`** — the invalidation twin of the above,
  one level up: with the rule object untouched, editing `GitTarget.allowedSourceNamespaces` must
  actually re-project the resident table, not merely re-run reconciliation. This is the test that
  catches "the mapper fired but the fingerprint was unchanged, so the rebuild was skipped".
- **`TestCeiling_UnknownScopeRetainsPreviousAndDoesNotSweep`** — with the source-scope service
  reporting *cannot say* (cache unsynced or source unreachable), the resolved namespace set is
  retained, no narrowing occurs, and **no resync/sweep is enqueued**. Assert the absence of the
  sweep, not only the condition — this is the path where a wrong answer deletes Git content.
- **`TestCeiling_ForbiddenSelectorIsUnknownNotStalled`** — a selector policy whose source Namespace
  access is denied reports `SourceNamespaceAuthorized=Unknown` with
  `SourceNamespacePolicyUnavailable`, keeps running at its last known scope, and does **not** set
  `Stalled=True`. Exact-name entries in the same policy keep resolving.
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
