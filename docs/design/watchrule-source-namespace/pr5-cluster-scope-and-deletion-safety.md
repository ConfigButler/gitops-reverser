# PR 5 (replacement) — ClusterWatchRule becomes cluster-scoped, made safe by GitTarget deletion controls

> **Replaces [pr5-clusterwatchrule-source-ceiling.md](pr5-clusterwatchrule-source-ceiling.md).** That
> page bound `allowedSourceNamespaces` to ClusterWatchRule's `scope: Namespaced` streams *at runtime*
> (a ceiling). This page removes the need for a ceiling by removing `scope: Namespaced` from
> ClusterWatchRule entirely — the [two-object model](alt-clusterwatchrule-cluster-scope-only.md) — and
> pairs it with a GitTarget-wide deletion control that makes the breaking change, and every scope
> mistake in this folder, **non-destructive**.
>
> Phase 5 of [source-namespace addressing](README.md). **Depends on:**
> [PR 4](pr4-source-namespace-field.md) (the field, the gate, the source-scope service),
> [PR 1](pr1-namespace-scoped-resync.md) (namespace-scoped resync), and
> [PR 2](pr2-stream-scope-collapse.md). **Breaking change**, accepted on this preliminary v1alpha3 API
> per [README § compatibility](README.md#compatibility). **Still bound by the
> [release gate](README.md#implementation-phases):** until this lands, ClusterWatchRule's namespaced
> streams bypass `allowedSourceNamespaces`, so PR 4's boundary is incomplete without it — do not cut a
> release between PR 4 and this PR.
>
> Code references verified against the tree on 2026-07-21.

## Two moves, in this order

1. **Deletion safety first** — `GitTarget.spec.prune` (`never` | `onEvent` | `always`, default
   `onEvent`) plus a volume guard. This is the foundation: it makes a wrong scope stop *writing*
   without *deleting*, so the breaking change below (and a rollback across it) is a stale-Git
   annoyance rather than tenant data loss.
2. **Scope simplification** — remove the `scope` field from ClusterWatchRule (cluster-scoped by kind),
   refuse any pre-existing namespaced cluster rule, and generalize the WatchRule field from PR 4's
   single `sourceNamespace` to a per-rule-item `namespace` with `"*"` meaning "every namespace the
   GitTarget admits". The `"*"` is the replacement path for the capability being removed from
   ClusterWatchRule.

They are one plan because move 2 is only safe to ship on top of move 1. They may land as two commits;
move 1 must precede move 2 **within the same release**.

---

## Part A — deletion safety

### The two deletion triggers already exist as separate code paths

The write path already draws the boundary this control needs — "Two Paths, One Plan Type"
([reconcile-via-watchlist-mark-and-sweep.md](../../spec/reconcile-via-watchlist-mark-and-sweep.md)):

- **Per-event delete** — [`PlanDelete`](../../../internal/manifestanalyzer/delete_plan.go#L36) resolves
  one real DELETE watch event to one delete-document action and, in its own words, "targets exactly
  ONE identity and **NEVER sweeps**". Applied by
  [`writeBatch.applyDelete`](../../../internal/git/plan_flush.go#L1140).
- **Mark-and-sweep** — [`BuildScopedPlan`](../../../internal/manifestanalyzer/plan.go#L197) (and its
  whole-folder special case [`BuildPlan`](../../../internal/manifestanalyzer/plan.go#L170)) diffs the
  full desired set against Git and emits a **managed drop** for every in-scope document not in desired.
  Applied on resync/heal by [`applyResync`](../../../internal/git/resync_flush.go). This is the path
  that turns a scope collapse into deletion, because "desired" is computed from the (possibly wrong)
  scope.

`prune` gates these two paths independently:

| `prune` | per-event delete (`PlanDelete`) | mark-and-sweep drop (`BuildScopedPlan`) | Git after a genuine delete / after a scope collapse |
|---|---|---|---|
| `never` | suppressed | suppressed | never deleted / never deleted — append-and-tombstone archive |
| **`onEvent`** *(default)* | **applied** | **suppressed** | deleted / **not deleted** — real deletions mirrored, collapse is inert |
| `always` | applied | applied | deleted / **deleted** — today's behavior, faithful and dangerous |

### Why the default is `onEvent`, and why it must not be `always`

The catastrophic deletion is the *inference*, not the *event*. A real DELETE event is a strong signal —
the object left the cluster. The sweep is an inference — "not in my current desired set, so it must be
gone" — and that inference is exactly what is wrong when scope collapsed (config error, a transient
source outage narrowing to empty, or a **rollback** across the scope simplification below). `onEvent`
keeps deletion accuracy for the common case and removes the entire scope-collapse data-loss class.

The default **must** be a sweep-suppressing mode for the safety argument to hold. Defaulting to
`always` — today's implicit behavior — would forfeit the "rollback is survivable" guarantee that is the
whole reason move 1 precedes move 2. Changing the effective default from `always` to `onEvent` is itself
a behavior change for existing GitTargets (they stop pruning stale files on resync); on this
preliminary API with few users it is the intended, safe-direction change, and it is called out in
release notes.

### The volume guard — closing the vector `onEvent` leaves open

`onEvent` blocks the *inference* vector; it does **not** block the *volume* vector. A real
`kubectl delete ns tenant-acme` cascades into a genuine DELETE per object, and `onEvent` faithfully
deletes every file — correct mirroring, but also a whole-folder wipe from real events. So add a guard,
independent of `prune` mode:

> A single commit/flush that would remove more than **N** managed documents (absolute, and/or a ratio
> of the target's tracked documents) refuses the removals, keeps the creates/updates, surfaces a
> `PruneGuardTripped` condition + event on the GitTarget, and requires an explicit override to proceed.

The trichotomy controls *whether* to infer deletions; the guard controls *how many* any one flush may
make, from either path. The guard is the specific answer to "prune a complete folder by mistake" — the
concern that has kept pruning unbuilt until now. Prior art: Flux `Kustomization.spec.prune` and Argo
CD's automated prune are both explicit and treated as dangerous-by-default for this reason (semantics
verifiable in `external-sources/`).

### The reversibility caveat that raises the stakes

"A wrong prune is reversible in Git" is true at the history level but weaker than it looks when the
destination repo is itself a **deploy source** — which is this product's own vision (merge = deploy). A
spurious prune commit can propagate to a live cluster through the downstream Flux/Argo sync *before* a
human reverts it, turning "annoying diff" into "deleted live resources". This is why the control lives
on **GitTarget**, not globally: an audit-mirror destination legitimately wants `always`; a
deploy-source destination wants `onEvent` + guard. The safety/accuracy trade is per-destination.

### Part A implementation

1. **API.** `GitTarget.spec.prune` — a `+kubebuilder:validation:Enum=never;onEvent;always`,
   `+kubebuilder:default=onEvent` string, next to
   [`AllowedSourceNamespaces`](../../../api/v1alpha3/gittarget_types.go#L134). Optional guard fields
   (`pruneGuard.maxDeletions`, `pruneGuard.maxDeletionRatio`). `task generate` + `task manifests`.
2. **Thread the mode into the planner.** `BuildScopedPlan` already takes a
   [`Policy`](../../../internal/manifestanalyzer/plan.go#L197); add the prune mode to `Policy` and
   **suppress the managed-drop emission** when the mode is not `always`. One choke point, because every
   sweep — full, per-type (M12), and resync — funnels through this function. Do **not** filter at
   apply time; a suppressed drop must never enter the plan, so the plan hash, the commit, and the
   resync path all agree.
3. **Gate the per-event path.** `applyDelete` ([plan_flush.go:1140](../../../internal/git/plan_flush.go#L1140))
   skips the removal when the mode is `never`. `onEvent` and `always` apply it.
4. **Volume guard at the flush boundary.** Count `Removed` actions in the batch before commit
   ([plan_flush.go](../../../internal/git/plan_flush.go)); over threshold ⇒ drop the removals from the
   batch, keep the rest, set `PruneGuardTripped`. The guard counts drops from **either** path.
5. **Status/surface.** A `GitTarget` condition/event when the guard trips, and a one-time info log when
   a resync's sweep is suppressed by `onEvent`/`never` (so an operator can see *why* a stale file
   persists). Do not make suppression an error state — it is the configured, healthy behavior.

### Part A test plan

- **Scope-collapse is inert under `onEvent`.** Build a desired set that has narrowed to empty for a
  namespace, run the resync plan, assert **zero** managed-drop actions and that the files remain. This
  is the test that would have caught the rollback data-loss.
- **`onEvent` still mirrors a real delete.** A DELETE event removes exactly its one document.
- **`never` removes nothing** from either path; **`always` reproduces today's sweep** byte-for-byte
  (regression pin: `always` must equal current `BuildPlan` behavior).
- **Volume guard** trips at the threshold from a sweep **and** from an event cascade, keeps
  non-deletions, sets the condition, and honors an override.
- **Default is `onEvent`** on a GitTarget that omits the field (defaulting test).

---

## Part B — scope simplification (the two-object model)

Full rationale and the breaking-change comparison are in
[alt-clusterwatchrule-cluster-scope-only.md](alt-clusterwatchrule-cluster-scope-only.md); this section
is the implementable subset. The end state:

> **ClusterWatchRule is the cluster-global surface. WatchRule is the namespaced surface, and every
> namespace it reaches is named in its spec and admitted by its GitTarget.** Scope is carried by the
> *kind*, never by a per-rule field.

### B0. Why removal deletes the original PR 5 wholesale

`allowedSourceNamespaces` is a *namespace* allow-list; a cluster-only ClusterWatchRule has no namespace
to bound, exactly as [the overview concedes](README.md#what-the-ceiling-does-not-do). So removing the
namespaced scope makes the ceiling inapplicable, and with it go the three hardest mechanisms the
original PR 5 had to build — the per-namespace expansion of `collectClusterWatchRuleSelections`, the
`clusterWatchRuleFingerprint` that had to hash **non-rule state** (the subtlest defect in the folder),
and the "unknown is not empty" retain-on-outage logic for ClusterWatchRule. None of them is written.

### B1. Remove the scope field

Delete `Scope` from [`ClusterResourceRule`](../../../api/v1alpha3/clusterwatchrule_types.go#L110) and
remove the [`ResourceScope` enum](../../../api/v1alpha3/clusterwatchrule_types.go#L9-L19) from the API
surface. The internal `typeset.Scope` stays — discovery still needs it. `task generate` + `task
manifests`.

`collectClusterWatchRuleSelections`
([watched_type_resolver.go:306-323](../../../internal/watch/watched_type_resolver.go#L306-L323)) drops
its `rr.Scope` argument and always matches with `ScopeCluster`; it keeps emitting `namespace: ""`,
which is now correct — those are genuinely cluster-scoped streams.
`clusterWatchRuleFingerprint` drops its `rr.Scope` component
([watched_type_resolver.go:511](../../../internal/watch/watched_type_resolver.go#L511)); it needs no
`src=` component, because a cluster-only rule's scope is fully spec-derived.

### B2. Removing the field does not retract stored objects — refuse them explicitly

CRD validation runs on **write**, not read. A `ClusterWatchRule` already stored with a namespaced rule
stays served and is still compiled by
[`bootstrapRuleStore`](../../../internal/watch/bootstrap.go#L49-L68); once the controller stops reading
`scope`, it would silently reinterpret that rule as cluster-scoped and either match nothing or flip
meaning. So the field removal is paired with a refusal:

> In the single gated compile path [PR 4 step 7](pr4-source-namespace-field.md#implementation-steps)
> routes both the reconciler and bootstrap through, refuse a ClusterWatchRule rule whose resource
> resolves **via discovery** ([`matchesScope`](../../../internal/watch/watched_type_resolver.go#L397))
> to a namespaced type. Terminal condition, message naming the migration: *"watch namespaced resources
> with a WatchRule and rules[].namespace; ClusterWatchRule is cluster-scoped only."*

The must-have test is `TestBootstrap_PreExistingNamespacedClusterRuleIsRefused` — a stored namespaced
cluster rule is not compiled and starts no stream, asserted before any reconcile. No manifest in the
repo can create that object once the field is gone, so this test is the only guard against a regression
in the refusal.

### B3. WatchRule per-item namespace — the replacement for the removed capability

Generalize PR 4's `WatchRule.spec.sourceNamespace` (top-level, singular) to
`WatchRule.spec.rules[].namespace` on [`ResourceRule`](../../../api/v1alpha3/watchrule_types.go#L83):

| `namespace` on a rule item | Meaning |
|---|---|
| omitted | the WatchRule's own namespace (legacy, unchanged) |
| `"*"` | every namespace the GitTarget's `allowedSourceNamespaces` **admits** — not every namespace that exists; deny-by-default with no policy |
| a name | that one source namespace, through PR 4's three-part gate |

This is a **pre-release evolution of PR 4's field**, not a new authorization model — the gate, the
`SourceNamespaceAuthorized` condition, and the source-scope service are reused verbatim, called per
`(rule item, namespace)` instead of per object. The full interface, the partial-authorization handling
(deny-in-whole for a denied *name*, narrow-for-`"*"`), and the `"*"`-selector maintaining path are
specified in [alt-per-item-source-namespace.md](alt-per-item-source-namespace.md); implement that page
here. PR 4 shipped singular into no release, so this is a rename with no conversion surface
([README § compatibility](README.md#compatibility)).

Mechanical deltas from PR 4's singular field:
- `CompiledRule.SourceNamespace` (one value, read at
  [watched_type_resolver.go:301](../../../internal/watch/watched_type_resolver.go#L301)) becomes a
  per-`ResourceRule` namespace; `collectWatchRuleSelections` expands one selection **per (matched
  record × resolved namespace)** — the same expansion the original PR 5 added to the *cluster* rule,
  now on the WatchRule where it belongs.
- `watchRuleFingerprint`
  ([watched_type_resolver.go:478-482](../../../internal/watch/watched_type_resolver.go#L478-L482))
  hashes the per-item namespace, and for a `"*"` item the **resolved** set — the one place the
  fingerprint-not-rule-state care from the original PR 5 survives, now scoped to `"*"` items only.
- Name-only items stay fully static; only `"*"` against a *selector* policy needs the source-cluster
  Namespace informer, so that machinery is opt-in per rule.

### B4. Capability removed with no in-kind replacement — flag for review

ClusterWatchRule's cross-namespace `targetRef` was the only way a **platform** team could author a
tenant's *namespaced* mirror without an object in the tenant namespace. `WatchRule.targetRef` is a
`LocalTargetReference` with no namespace
([watchrule_types.go:24-42](../../../api/v1alpha3/watchrule_types.go#L24-L42)), so the WatchRule must
live in the namespace it watches from. **The two-object model removes platform-external namespaced
authoring with no replacement.** If a real deployment relies on it, this plan is wrong for that
deployment and must be paired with a namespaced-`targetRef` WatchRule (a separate change). This is the
open question most able to defeat Part B — see [open questions](#open-questions).

### B5. Reactivity

- **GitTarget → ClusterWatchRules mapper:** the original PR 5 needed it to re-resolve a runtime
  ceiling. With the ceiling gone, ClusterWatchRule no longer re-resolves on GitTarget policy changes —
  **the mapper is not needed** for ClusterWatchRule. PR 3's ClusterProvider→ClusterWatchRule admission
  mapper is unaffected.
- **WatchRule reactivity:** PR 4's edges carry per-item changes unchanged. A `"*"` item against a
  selector policy needs the source-cluster Namespace informer from PR 4 to map to WatchRules — reused,
  not new.

### Part B test plan

- **`TestBootstrap_PreExistingNamespacedClusterRuleIsRefused`** (B2) — the critical one.
- **Admission/compile refuses** a namespaced-resolving ClusterWatchRule rule with the migration
  message; **cluster-scoped rules are unaffected** (the CRD day-one case still works).
- **`collectClusterWatchRuleSelections`** emits cluster-scoped selections only, still keyed `""`.
- **`clusterWatchRuleFingerprint`** no longer varies with a (removed) scope; two cluster rules
  differing only in resources still fingerprint differently.
- **WatchRule per-item** — the whole
  [alt-per-item test plan](alt-per-item-source-namespace.md#test-plan-delta-relative-to-pr-4-as-written):
  the partial-object deny-in-whole case, `"*"` = deny-by-default with no policy, `"*"` static under a
  name policy, `"*"` retain-on-unknown under a selector policy (with **no sweep** — now doubly assured
  by Part A), and the mixed omitted/name/`"*"` aggregate condition.
- **Deleted:** every original-PR-5 ceiling test (narrowing, spare-cluster-scoped, resolved-scope
  fingerprint, table-rebuild-on-policy-change, retain-on-unknown-for-ClusterWatchRule, the
  establishing/maintaining ClusterWatchRule pair). The mechanisms are gone.

---

## The payoff: the rollback that made this scary is now benign

The [breaking-change comparison](alt-clusterwatchrule-cluster-scope-only.md#comparing-the-approaches--breaking-change-consequences)
named the rollback as the sharpest consequence: roll the controller back past the per-item field and
`namespace: "*"` collapses to own-namespace — a narrowed scope, which is the input to a sweep, which
deletes a tenant's Git content. **Part A removes exactly that edge.** With `prune: onEvent` (the
default), the collapsed scope stops *updating* the dropped namespaces and never *sweeps* them; the
rollback is stale Git, recoverable by rolling forward again, not data loss. The migration mechanics
(cross-kind, no conversion webhook, HA skew) are unchanged — Part A does not make the change less
breaking, it makes getting scope wrong, in either direction, recoverable.

Part A also **de-delicates PR 4**: the [establishing-vs-maintaining
contract](pr4-source-namespace-field.md#establishing-versus-maintaining-a-scope) exists because
narrowing-to-empty feeds a sweep. Under `onEvent` a narrowed set never sweeps, so the three-valued
retain-on-unknown logic drops from "prevents data loss" to "avoids a stopped stream" — still worth
having, no longer load-bearing for correctness.

## Done when

- `GitTarget.spec.prune` defaults to `onEvent`; a resync under a collapsed/empty scope removes nothing;
  a real DELETE event still removes its one document; the volume guard trips and is overridable.
- ClusterWatchRule rejects namespaced rules — new and **pre-existing at bootstrap** — with a message
  naming the WatchRule migration; cluster-scoped rules are unchanged.
- A WatchRule reaches every admitted namespace via `rules[].namespace: "*"`, and a denied *name* fails
  the whole rule loudly; Git paths still follow each object's own namespace.
- The original PR 5's ceiling code and tests are gone, not disabled.
- `task lint`, `task test`, `task test-e2e` pass.

## Open questions

1. **Capability B (B4)** — must a platform team author namespaced watching for a tenant target from
   outside the tenant namespace? If yes, Part B needs a namespaced-`targetRef` WatchRule or must not
   remove the scope.
2. **`prune` default `onEvent` vs `never`** — `onEvent` keeps genuine-delete accuracy; `never` is
   maximally safe but lets Git accumulate tombstones for every real deletion. `onEvent` recommended.
3. **Volume guard shape** — absolute `maxDeletions`, a ratio, or both; and the override mechanism
   (annotation vs a spec field vs a one-shot). Ship the guard with Part A or immediately after.
4. **Singular `namespace` vs plural `namespaces`** per rule item, and **name-only `"*"` first vs
   selector-backed `"*"`** — inherited from
   [alt-per-item open questions](alt-per-item-source-namespace.md#open-questions-for-the-reviewer).

## End-to-end

The day-one multi-tenant shape, re-expressed in the two-object model and proving the deletion safety:

- A **ClusterWatchRule** selecting CRDs (cluster-scoped) and a **WatchRule** in `tenant-acme` selecting
  ConfigMaps with `rules[].namespace: "*"`, against acme's GitTarget declaring
  `allowedSourceNamespaces: [repo-config]` and `prune: onEvent`.
- Create a ConfigMap in `repo-config` and one in `tenant-zen`, and a CRD. Assert: `repo-config/…` and
  the CRD appear; `tenant-zen/…` is **never** written (allow-list holds).
- Then narrow the policy to admit nothing and force a resync. Assert `repo-config/…` is **not deleted**
  (prune `onEvent` suppresses the sweep) — the negative that proves a scope change cannot wipe a
  tenant. Flip the GitTarget to `prune: always` and assert the same narrowing now does remove it, so
  the control is real in both directions.

## What does not change

PR 1, PR 2, PR 3 remain landed and unaffected. `GitTarget.allowedSourceNamespaces`, the delegation
flag, the in-cluster sign-off argument, and the `IsLocalSource()` trap all stand. Git placement still
follows each mirrored object's own namespace — the write path needs no change beyond the `prune` gate.
