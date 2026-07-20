# Alternative — make ClusterWatchRule cluster-scoped only, and pluralize the WatchRule field

> **Status: proposal, for review. Not agreed, not scheduled.** An alternative to
> [PR 5](pr5-clusterwatchrule-source-ceiling.md) and an amendment to
> [PR 4](pr4-source-namespace-field.md), inside the
> [source-namespace addressing](README.md) workstream.
>
> **PR 4 is being implemented as this is written** —
> [`WatchRule.spec.sourceNamespace`](../../../api/v1alpha3/watchrule_types.go#L71) already exists in
> the tree with its full doc comment. This page is therefore written as a *delta against work in
> flight*, not as a greenfield design. The migration section says exactly what it would cost to adopt
> late; the answer is "less than it looks", and the deciding factor is whether the field is plural
> before it ships in a release, not before it is written.
>
> Code references verified against the tree on 2026-07-20.

## The proposal

Two changes, one API-shaped and one field-shaped:

1. **`ClusterWatchRule` selects cluster-scoped types only.** Narrow
   [`ClusterResourceRule.Scope`](../../../api/v1alpha3/clusterwatchrule_types.go#L110) to
   `Cluster`. Namespaced types are selected exclusively by `WatchRule`.
2. **`WatchRule.spec.sourceNamespace` becomes `sourceNamespaces`**, a list of names, since it is now
   the only object that can address namespaced resources at all.

`GitTarget.spec.allowedSourceNamespaces` and the ClusterProvider delegation flag are unchanged, and
so is [PR 4](pr4-source-namespace-field.md)'s gate. What changes is that they become the *only* path
into a target, rather than one of two with a runtime ceiling reconciling them.

## Why: PR 5 re-imposes at runtime a boundary the API can make unrepresentable

[PR 5](pr5-clusterwatchrule-source-ceiling.md) exists for exactly one reason: `ClusterWatchRule`
accepts `scope: Namespaced`, and its `targetRef` is cross-namespace by design
([clusterwatchrule_types.go:22-44](../../../api/v1alpha3/clusterwatchrule_types.go#L22-L44)), so a
rule can deliver every source namespace into a GitTarget that declared an allow-list. PR 5 answers
that by resolving the target's policy inside the watch resolver and expanding a cluster-wide
selection into one selection per admitted namespace.

That is a correct design for the constraint as posed. The observation here is that the constraint is
self-imposed. Removing `Namespaced` from the enum makes the bypass **unrepresentable** rather than
**checked**, and the following all cease to exist:

| PR 5 section | What it costs today | Under this proposal |
|---|---|---|
| §1 apply the ceiling in selection | expand `namespace: ""` per admitted namespace in `collectClusterWatchRuleSelections` ([watched_type_resolver.go:306-323](../../../internal/watch/watched_type_resolver.go#L306-L323)) | gone — cluster-scoped rules legitimately emit `""` |
| §2 resolved scope into table invalidation | `clusterWatchRuleFingerprint` must hash state that **is not rule state** ([watched_type_resolver.go:463-500](../../../internal/watch/watched_type_resolver.go#L463-L500)); an unchanged rule under a changed policy must still re-project the table | gone — a ClusterWatchRule's scope is spec-derived again |
| §2b unknown is not empty | the ClusterWatchRule half of the narrow-then-sweep hazard | gone — nothing narrows |
| §3 reactivity | GitTarget → ClusterWatchRules mapper; informer fan-out to ClusterWatchRules | gone |
| release gate | PR 4 must not ship without PR 5 | survives, but for a smaller reason — see [migration](#if-pr-4-has-already-shipped-singular) |

§2 is the one worth dwelling on, because it is the subtlest defect in the folder and it is created
entirely by the ceiling: the ceiling's inputs (GitTarget policy, source-cluster Namespace labels) are
not rule state, so a mapper that requeues the rule is not sufficient — reconciliation runs, the
fingerprint is unchanged, the rebuild is skipped, and the resident table keeps the old namespace set
while every diff looks correct. Designing that away is worth more than the field it protects.

### It also makes the documented caveat into a definition

[README § what the ceiling does not do](README.md#what-the-ceiling-does-not-do) already concedes that
a namespace allow-list cannot partition cluster-scoped objects, and warns that "my allow-list bounds
what this tenant sees" is false for the cluster-scoped half. Under this proposal that stops being a
caveat attached to a field and becomes the definition of the object:

> **ClusterWatchRule is the cluster-global surface. It is not namespace-bounded, by construction.**
> **WatchRule is the namespaced surface, and every namespace it reaches is named in its spec and
> admitted by its GitTarget.**

Two objects, two sentences, no overlap. The operator's answer to *"will this stream objects from
namespaces outside my allow-list?"* is a property of the kind, not of an audit.

The day-one multi-tenant use case ([README § driving use case](README.md#the-driving-use-case-multi-tenancy-with-crds-on-day-one),
item 4) is unaffected: a tenant still needs a ClusterWatchRule from day one to capture CRDs, and that
is precisely what the narrowed object does. `config/samples/clusterwatchrule.yaml` already uses only
`scope: Cluster`.

## The cost: `scope: Namespaced` is the only expression of two capabilities

This is the part a reviewer should push on, because the capability does not disappear — it
**relocates**, and the proposal is only a win if the new home is the right one.

**Capability A — "watch this type in every namespace."** One object, no enumeration, and it covers
namespaces that do not exist yet. `sourceNamespaces` as a list of *names* does not replace this:
enumeration is not "all", and it goes stale on namespace creation. Expressing "all" again requires a
wildcard entry or a `sourceNamespaceSelector`.

**Capability B — platform-authored namespaced watching for a tenant's target.**
`ClusterWatchRule.targetRef` is cross-namespace, so a platform team can configure a tenant's mirror
without owning an object in the tenant's namespace. `WatchRule.targetRef` is a `LocalTargetReference`
with no namespace field ([watchrule_types.go:24-42](../../../api/v1alpha3/watchrule_types.go#L24-L42)),
so the replacement must live in the tenant's namespace and be authored by whoever can write there.

So the honest framing of this proposal is **not** "cluster-scoped resources belong on a cluster-scoped
object". It is:

> Which object may say *"all namespaces"*, and under whose sign-off?

Today: ClusterWatchRule, under nothing but cluster-admin's ability to create one. Under this proposal:
WatchRule, under `allowedSourceNamespaces` plus the delegation flag — the pair PR 4 already builds,
readable off the GitTarget. That is a better answer. It is not an elimination, and a review that
treats it as one will be surprised later.

**Two questions decide whether the win is large or merely structural:**

1. Does any real configuration need "all namespaces" as a live, self-updating scope? If yes, a
   wildcard or selector must ship in v1 and the machinery does not fully disappear — it moves.
2. Must a platform team be able to author namespaced watching for a tenant target without an object
   in the tenant namespace? If yes, this proposal removes a capability with no replacement, and
   should be rejected or paired with one.

## Names-only plural is cheap; a selector is not

Split the plural question, because the two halves have very different cost.

**`sourceNamespaces: [a, b]`, names only.** The resolved scope stays spec-derived. `watchRuleFingerprint`
([watched_type_resolver.go:478-482](../../../internal/watch/watched_type_resolver.go#L478-L482)) keeps
working with no new invalidation input; no source-cluster Namespace informer is needed for the *rule*
side; and *cannot say* collapses to a pure **establishing** question. That last point cuts into PR 4
as well — the [source-scope service](pr4-source-namespace-field.md#the-source-scope-service--define-this-interface-before-writing-the-gate)
and the three-valued readiness result are dragged in mostly by **selector** policies, not by the field
itself. A names-only v1 on both sides (`sourceNamespaces` and `allowedSourceNamespaces`) would defer
the informer entirely.

**`sourceNamespaceSelector`.** This pulls the whole *maintaining* column of
[establishing versus maintaining](pr4-source-namespace-field.md#establishing-versus-maintaining-a-scope)
onto WatchRule, where today it applies only to PR 5's ceiling. A WatchRule's resolved set can now
narrow, and a narrowed set is the input to a sweep — the destructive path
[PR 1](pr1-namespace-scoped-resync.md) landed first to make survivable. The machinery deleted from
ClusterWatchRule reappears on WatchRule the moment the field is dynamic. Net simplification is still
positive — one path instead of two — but it is not free, and it should not be sold as free.

**Recommended split:** names-only in v1; selector as a follow-up whose entire subject is the
maintaining contract, rather than a subsection of a field addition.

### One consequence of plural to check, not assume

A single WatchRule fanning out over N namespaces needs an identity-complete placement template —
`{name}` plus `{namespace}` or `{namespaceOrCluster}` — or two source namespaces collapse onto one
Git path. That is enforced statically only for core Secrets today
([placement.go:550-552](../../../internal/manifestanalyzer/placement.go#L550-L552),
[gittarget_placement_validation.go:67](../../../internal/controller/gittarget_placement_validation.go#L67));
operator-configured sensitive types rely on write-time guards. The hazard is not created by plural —
two singular WatchRules on one GitTarget already reach it — but plural makes it reachable with one
object and no second author. Worth a static check on any rule that can resolve to more than one
namespace. **Verify against the placement code; do not take this paragraph as settled.**

## Narrowing the enum does not retract stored objects

The step most likely to be skipped, and it is a security step, not a cleanup.

CRD schema validation applies on **write**, not on read. A `ClusterWatchRule` already stored with
`scope: Namespaced` continues to be served, continues to be compiled by
[`bootstrapRuleStore`](../../../internal/watch/bootstrap.go#L49-L68), and continues to emit
`namespace: ""` — so the bypass survives the enum change for every object created before it. The
narrowed enum only prevents *new* ones.

So the change is two-part:

1. narrow the enum (blocks new and blocks updates to existing); **and**
2. make the compile path refuse a `Namespaced` cluster rule explicitly — in the same single gated
   function [PR 4 step 7](pr4-source-namespace-field.md#implementation-steps) routes both the
   reconciler and bootstrap through — with a terminal condition naming the replacement.

Doing (1) without (2) is the same class of defect this folder was written to fix: a scope computed in
one place and not enforced in another. A test asserting that a *pre-existing* `Namespaced` cluster
rule is refused at bootstrap is the one that keeps it honest, because no manifest in the repo can
create the object once the enum lands.

## If PR 4 has already shipped singular

The cost of adopting late is bounded and depends only on whether a release has gone out.

**Before any release carrying `sourceNamespace`** — the pluralization is a rename with no compatibility
surface. This is a preliminary v1alpha3 API and [README § compatibility](README.md#compatibility)
already accepts observable changes with no conversion webhook. The field is new, so it is unset
everywhere; nothing to migrate.

**After a release** — accept both, or accept the churn. `sourceNamespace` and `sourceNamespaces` can
coexist with a CEL rule rejecting both-set and the controller reading singular as a one-element list,
which is ugly but small. Preferred instead: pluralize at the next API version with conversion, and do
not ship singular into a second release.

Either way, **the rest of PR 4 is unaffected**. The gate, the `SourceNamespaceAuthorized` condition,
the printer column, the fingerprint change, the single gated compile path, and the reactivity mappers
all apply per-namespace-in-the-set exactly as written for one namespace. The work in flight is not
wasted by this proposal; the diff is the field's cardinality and the loop around the gate.

The release gate survives in a weaker form: `Namespaced` must be removed from `ClusterWatchRule`
before, or in, the release that first ships `allowedSourceNamespaces`. Otherwise the allow-list is
enforced on the kind that cannot bypass it and unenforced on the kind that can — the same gap
[PR 5](pr5-clusterwatchrule-source-ceiling.md#why-this-is-launch-set-not-follow-up) describes, for the
same reason.

## Labels: two axes, only one is affected

These get conflated, and conflating them muddies both:

- **Labels on namespaces** — `sourceNamespaceSelector`, or the selector half of
  `allowedSourceNamespaces`. Shares the source-cluster Namespace informer, and carries the
  narrow-then-sweep hazard above. Affected by this proposal.
- **Labels on resources** — "mirror only objects carrying `x=y`", a per-rule object filter on
  [`ResourceRule`](../../../api/v1alpha3/watchrule_types.go#L83). Composes with whatever namespace
  scoping exists, needs no authorization gate, has no sweep semantics beyond what a scope change
  already has. **Unaffected by this proposal**, and it should be designed separately so it does not
  get entangled with the authorization work.

So "this fits the label vision better" is true for namespace selectors and neutral for resource
labels.

## Known breakage

Small, from a repo-wide sweep on 2026-07-20 (excluding `external-sources/`):

- [config/samples/clusterwatchrule.yaml](../../../config/samples/clusterwatchrule.yaml) — already
  `scope: Cluster`. No change.
- [test/e2e/unsupported_folder_e2e_test.go:177](../../../test/e2e/unsupported_folder_e2e_test.go#L177)
  — uses `scope: Namespaced` with ConfigMaps, but only because the refusal test needs *some* rule
  kind. Convert to `scope: Cluster` or to a WatchRule.
- [docs/configuration.md](../../configuration.md) — "namespaced resources across multiple namespaces"
  in the `ClusterWatchRule` section becomes false and must change in the same PR.
- The `ResourceScopeNamespaced` constant
  ([clusterwatchrule_types.go:17-18](../../../api/v1alpha3/clusterwatchrule_types.go#L17-L18)) stays:
  WatchRule resolution uses it internally
  ([watched_type_resolver.go:294](../../../internal/watch/watched_type_resolver.go#L294),
  [stream_readiness.go:142](../../../internal/watch/stream_readiness.go#L142),
  [manager_catalog.go:389](../../../internal/watch/manager_catalog.go#L389)). Only the
  `ClusterResourceRule.Scope` **enum** narrows.
- Unit tests constructing `Namespaced` cluster rules across `internal/watch` and
  `internal/controller` — several files; mechanical.

This list is a sweep, not a proof. A reviewer who believes a real deployment depends on capability A
or B above should say so; that is the argument that defeats this page, and no amount of the above
outweighs it.

## Test plan delta

Relative to PR 4 and PR 5 as written:

- **Deleted:** every PR 5 test named for the ceiling — narrowing, sparing cluster-scoped rules,
  `clusterWatchRuleFingerprint` resolved-scope, table rebuild on policy-only change, retain-on-unknown,
  and the establishing/maintaining pair for ClusterWatchRule.
- **Added:** `TestBootstrap_PreExistingNamespacedClusterRuleIsRefused` — a stored ClusterWatchRule with
  `scope: Namespaced` is not compiled and starts no stream, asserted at bootstrap before any reconcile.
  This is the enum-narrowing gap above and it is the single most important new test.
- **Added:** admission rejects `scope: Namespaced` on create and on update, with a message naming
  WatchRule plus `sourceNamespaces` as the replacement.
- **Changed:** every PR 4 gate case becomes a set case. Specifically: an empty set is the legacy
  own-namespace behavior; a partially-admitted set is **denied in whole, not silently trimmed** — a
  rule that quietly watches two of the three namespaces it asked for is the plural-specific failure
  mode, and it needs its own case.
- **Kept as-is:** the two must-have tests from
  [PR 4](pr4-source-namespace-field.md#the-two-tests-that-must-exist), the fingerprint and selection
  silent-failure guards, and the e2e proving Git paths follow the *source object's* namespace
  ([Appendix A](pr4-source-namespace-field.md#appendix-a-the-source-objects-namespace-already-names-the-git-folder)).

## Open questions for the reviewer

1. Capability A — is a live "all namespaces" scope required at launch? If yes, does it come back as a
   wildcard entry, a selector, or not at all?
2. Capability B — must a platform team author namespaced watching for a tenant target from outside the
   tenant namespace?
3. Names-only in v1 on **both** `sourceNamespaces` and `allowedSourceNamespaces`, deferring the
   source-scope service and the Namespace informer wholesale? That is a much larger cut to PR 4 than
   the pluralization itself, and it should be decided on its own merits.
4. Partial admission of a set: deny in whole (recommended) or trim to the admitted subset? Trimming is
   friendlier and is a silent narrowing, which is the failure class this folder exists to remove.
5. Is the enum narrowing acceptable churn on a preliminary v1alpha3, given
   [README § compatibility](README.md#compatibility) already accepts observable changes without
   conversion?

## What this does not change

PR 1, PR 2, and PR 3 are unaffected and remain landed. `GitTarget.spec.allowedSourceNamespaces`, the
delegation flag, and the whole authorization argument in
[PR 4](pr4-source-namespace-field.md#what-the-delegation-flag-means) — including the in-cluster
sign-off and the `IsLocalSource()` trap — stand exactly as written. Git placement still follows the
source object's own namespace, so the write path still needs no change.
