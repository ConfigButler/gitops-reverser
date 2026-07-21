# Alternative — a per-rule-item namespace, with `*` meaning "all *allowed* namespaces"

> **Status: rejected alternative, retained for design history.** The selected plan is
> [PR 5 deletion safety](pr5-gittarget-deletion-safety.md) followed by
> [PR 6 scope by kind](pr6-cluster-scope-only.md). This alternative would keep
> `ClusterWatchRule.scope: Namespaced` and the former runtime-ceiling machinery; that machinery is
> abandoned. References below to the former “PR 5” describe that historical alternative, not the
> current deletion-safety PR number.
>
> **PR 4 is being implemented as this is written, top-level singular.** The compiled rule already
> carries `SourceNamespace` and [watched_type_resolver.go:301](../../../internal/watch/watched_type_resolver.go#L301)
> already reads it for the watch selection. So this page is a delta against work in flight, and the
> [migration section](#relationship-to-the-in-flight-pr-4) states exactly what it costs to adopt late.
>
> Code references verified against the tree on 2026-07-21.

## The proposal

Move the namespace from the WatchRule spec's top level onto each **rule list item**
([`ResourceRule`](../../../api/v1alpha3/watchrule_types.go#L83)), and give it the same three-valued
shape the other rule-item fields already have:

~~~yaml
apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata:
  name: repo-config
  namespace: tenant-acme
spec:
  targetRef:
    name: acme
  rules:
    - resources: [configmaps]           # namespace omitted → tenant-acme (legacy, unchanged)
    - resources: [secrets]
      namespace: repo-config            # one explicit source namespace
    - resources: [deployments]
      namespace: "*"                    # every namespace the GitTarget's policy ADMITS
~~~

Three meanings, mirroring how `resources`, `apiGroups`, and `apiVersions` already read on the same
item:

| `namespace` on a rule item | Meaning |
|---|---|
| omitted | The WatchRule's own namespace. **Legacy behavior, byte-for-byte.** |
| `"*"` | Every namespace the GitTarget's `allowedSourceNamespaces` **admits** — *not* every namespace that exists. Bounded by policy, deny-by-default when no policy is declared. |
| an explicit name | That one source namespace, subject to the same three-part gate PR 4 already defines. |

`GitTarget.spec.allowedSourceNamespaces` and the ClusterProvider delegation flag are unchanged, and
so is the authorization argument in [PR 4](pr4-source-namespace-field.md#what-the-delegation-flag-means).
This changes the *interface* to the gate, not the gate.

> **Open: singular or plural per item.** `namespace: string` (repeat the item for a second namespace)
> or `namespaces: []string` (one item, several names). Singular keeps the shape identical to the
> current in-flight field and reads cleanest for the common one-namespace case; plural is terser for
> a hand-listed set. This page assumes **singular** and treats `"*"` as the way to say "many",
> because a hand-listed multi-namespace set is the rarer need and `"*"`-bounded-by-policy covers it.
> See [open questions](#open-questions-for-the-reviewer).

## The load-bearing idea: `*` is bounded by the allow-list

The reason this defers the two hard questions the
[cluster-scope-only alternative](alt-clusterwatchrule-cluster-scope-only.md#the-cost-scope-namespaced-is-the-only-expression-of-two-capabilities)
had to answer is a single semantic choice:

> `namespace: "*"` resolves to the **admitted** set — exactly what
> `GitTarget.allowedSourceNamespaces` permits — never to the raw set of namespaces the source
> credential can read.

That has three consequences worth stating separately:

1. **"Follow the whole cluster" is never expressible by a WatchRule author.** A tenant writing `"*"`
   gets what their target's policy admits and nothing more. The catastrophic fail-open reading — a
   rule author quietly watching every namespace — is not a reachable state, because the ceiling is
   the GitTarget's, set by whoever owns the destination.
2. **No policy declared ⇒ `"*"` admits nothing beyond the legacy own-namespace.** `"*"` is not a
   backdoor around deny-by-default. With no `allowedSourceNamespaces` and the delegation flag false,
   an explicit `"*"` is denied with the same terminal condition PR 4 gives a denied name — the
   message naming the fix (declare a policy, set the flag). The omitted case still means own-namespace
   and still needs neither.
3. **Capability A comes back without deleting capability B.** The
   [cluster-scope-only page](alt-clusterwatchrule-cluster-scope-only.md#the-cost-scope-namespaced-is-the-only-expression-of-two-capabilities)
   named two things `scope: Namespaced` uniquely expressed: (A) "watch this type in every namespace"
   and (B) "platform-authored namespaced watching from outside the tenant namespace". `"*"` gives A,
   bounded. B is untouched, because ClusterWatchRule and its cross-namespace `targetRef` remain. So
   neither question has to be answered to ship this.

## Honest accounting: this is additive, not a simplification

The [cluster-scope-only alternative](alt-clusterwatchrule-cluster-scope-only.md) removed PR 5's
runtime ceiling, its fingerprint hazard, and its informer fan-out. **This proposal removes none of
that.** ClusterWatchRule keeps `scope: Namespaced`; the former runtime-ceiling proposal would ship;
and WatchRule *gains* a namespace-expansion path that looks a lot like that proposal's.

So the end state has two ways to watch namespaced resources across namespaces:

| | Authored by | Lives in | Ceiling |
|---|---|---|---|
| WatchRule, `namespace: "*"` or names | the tenant (targetRef is namespace-local) | the tenant's namespace | `GitTarget.allowedSourceNamespaces` |
| ClusterWatchRule, `scope: Namespaced` | the platform (targetRef is cross-namespace) | any namespace | `GitTarget.allowedSourceNamespaces` (PR 5) |

They are not redundant — the authoring locality genuinely differs — but the machinery is doubled, not
shared-down. **The trade this page offers is: keep both capabilities and break nothing, at the cost of
more surface, versus the sibling page's cut both the surface and a capability, and break.** That is the
decision for review; neither is strictly better.

| | [cluster-scope-only](alt-clusterwatchrule-cluster-scope-only.md) | **per-item `*` (this)** | top-level plural `sourceNamespaces` |
|---|---|---|---|
| Breaking change | yes (enum narrows) | no | no |
| Forces the A/B answers | yes | no | no |
| Effect on PR 5 | deletes its guts | keeps it | keeps it |
| Delta vs in-flight PR 4 | pluralize field | move field into rule item + expansion loop | `string` → `[]string` at one existing point |
| "all allowed" available | via WatchRule wildcard/selector | via `"*"` | via `"*"` |
| Per-type namespace granularity | no | **yes** | no |

## What it costs inside PR 4

### 1. Partial authorization within one object

Top-level singular authorizes a WatchRule as a unit: one effective namespace, one gate call, one
`SourceNamespaceAuthorized` condition. Per-item, the gate runs per `(rule item, namespace)`, so one
object can hold an admitted item and a denied item at once — a state the singular field cannot
produce. The `SourceNamespaceAuthorized` condition is per-object and cannot cleanly say "item 2 is
denied". Two semantics, and they must not be blended:

- **A named namespace is *establishing*.** If any named namespace on any item is denied, the **whole
  WatchRule** is `SourceNamespaceAuthorized=False`, `Stalled=True`, no streams — deny-in-whole. The
  message names the offending item and namespace. A rule that silently watches two of the three
  namespaces it asked for is the plural-specific failure this design must refuse; partial success is
  worse than loud failure here.
- **`"*"` is *maintaining* / narrowing.** It never denies. It expands to the admitted set, which may be
  empty, which is a correct outcome and **not** `Stalled` — the same rule
  the former runtime-ceiling design stated for the ceiling.

So a single WatchRule can carry both a *refusal* semantic (a denied name) and a *narrowing* semantic
(`"*"`). When both are present the refusal dominates the object-level condition. This is the existing
[establishing-versus-maintaining contract](pr4-source-namespace-field.md#establishing-versus-maintaining-a-scope)
applied within one object rather than across two rule kinds — implementable, but it is the part most
likely to be gotten subtly wrong, and it needs its own tests.

### 2. `"*"` reintroduces the informer and the sweep hazard — but only opt-in

A name-based item resolves statically: `watchRuleFingerprint`
([watched_type_resolver.go:478-482](../../../internal/watch/watched_type_resolver.go#L478-L482)) keeps
working from spec alone, no source-cluster Namespace informer needed. A `"*"` item against a
**selector** `allowedSourceNamespaces` makes the resolved set depend on source-cluster Namespace
labels — which is the maintaining/sweep hazard [PR 1](pr1-namespace-scoped-resync.md) landed first to
survive, and it drags PR 4's [source-scope service](pr4-source-namespace-field.md#the-source-scope-service--define-this-interface-before-writing-the-gate)
and the source-cluster Namespace informer onto the WatchRule path.

The good news is that this is **opt-in per item**: a WatchRule that never writes `"*"` (or uses `"*"`
against a name-only policy) is fully static and needs none of it. So the dynamic machinery is paid for
only by the rules that ask for dynamism — which is the right place to pay it. A names-only `"*"`
policy is the recommended first cut; the selector-backed `"*"` can follow with the informer.

### 3. The fingerprint must include the resolved set for `"*"` items

Same shape as the former runtime-ceiling design's invalidation rule, now on WatchRule: for a `"*"`
item the resolved namespace set is **not rule state**, so a policy or
Namespace-label change that alters the set must still re-project the watched-type table. Hash the
resolved set into the rule's fingerprint (or carry a source-scope generation into the rebuild
trigger). A name-only item needs nothing new. The silent-failure test from PR 5 applies verbatim: an
unchanged rule object under a changed policy must re-project.

### 4. Expansion in selection

[`collectWatchRuleSelections`](../../../internal/watch/watched_type_resolver.go#L283-L307) currently
appends one selection per matched record at `namespace: rule.SourceNamespace`. Per-item, it appends
one selection **per (matched record × resolved namespace)** — the same expansion PR 5 adds to
`collectClusterWatchRuleSelections`, so the two collectors converge on one shape. Prefer expansion at
this site over filtering at the read site: an expanded selection carries the scope through the plan
hash, the informers, and the resync path for free, while
a read-site filter must be repeated at each and is silently wrong if one is missed.

## Relationship to the in-flight PR 4

The delta depends on whether a release has shipped `sourceNamespace`:

- **Before any release** — pure churn on a preliminary v1alpha3 API that
  [README § compatibility](README.md#compatibility) already accepts without conversion. The field is
  unset everywhere, so nothing migrates. The interface moves from `spec.sourceNamespace` (top level)
  to `spec.rules[].namespace` (per item); the gate, the `SourceNamespaceAuthorized` condition, the
  printer column, and the single gated compile path are all **reused as-is**, just called per item
  instead of per object.
- **After a release** — pluralize/relocate at the next API version with conversion; do not ship the
  singular field into a second release.

The work in flight is not wasted either way. The parts that survive unchanged are the whole authorization
model and status contract; the parts that move are the field's location (spec → rule item) and the
compiled representation (`CompiledRule.SourceNamespace string` → a per-`ResourceRule` namespace plus an
expansion loop). That is a larger rework of the collector and the compiled rule than **top-level plural
`sourceNamespaces []string`** would be — plural keeps `SourceNamespace` on the compiled rule, just as a
slice, and expands at the one point that already exists. If minimizing disruption to the in-flight PR is
the priority, top-level plural delivers the same `"*"`=all-allowed insight at a smaller delta, losing
only the per-type granularity and the visual symmetry with the other rule-item fields.

## What does NOT change

- ClusterWatchRule keeps `scope: Namespaced`; [PR 3](pr3-clusterwatchrule-target-admission.md) and
  the former runtime-ceiling proposal are unaffected. This proposal is orthogonal to them.
- `GitTarget.allowedSourceNamespaces`, the delegation flag, and the in-cluster sign-off argument stand
  verbatim.
- Git placement still follows each mirrored object's **own** namespace, so the write path needs no
  change — a rule item watching `repo-config` still writes under `repo-config/…`
  ([PR 4 Appendix A](pr4-source-namespace-field.md#appendix-a-the-source-objects-namespace-already-names-the-git-folder)).
- The `namespace: ""` collapse rules ([PR 2](pr2-stream-scope-collapse.md)) are unaffected: an omitted
  item namespace resolves to a concrete own-namespace, and `"*"` expands to concrete names — neither
  emits a raw `""` key. Only ClusterWatchRule's cluster-scoped rules still emit `""`.

## Test plan delta (relative to PR 4 as written)

- **Kept:** the two must-have tests
  ([PR 4 § the two tests that must exist](pr4-source-namespace-field.md#the-two-tests-that-must-exist)),
  the fingerprint and selection silent-failure guards, and the Appendix-A Git-path e2e.
- **Changed to per-item:** the gate table becomes per `(item, namespace)`; add the **partial-object**
  case explicitly — one item allowed, one item denied ⇒ whole rule `Stalled`, message names the denied
  item.
- **Added — `"*"` semantics:**
  - `"*"` with no policy and flag false ⇒ denied (deny-by-default), not "all namespaces".
  - `"*"` against a name-only policy ⇒ expands to exactly those names, statically; fingerprint changes
    when the policy changes though the rule object does not.
  - `"*"` against a selector policy, source Namespace access denied, **scope already resolved** ⇒
    `Unknown` / `SourceNamespacePolicyUnavailable`, retained, **not** `Stalled`, **no sweep** — the
    maintaining path.
  - `"*"` against a selector policy that admits nothing ⇒ zero selections for that item, not a failure.
- **Added — mixed within one object:** a rule with one omitted item, one named item, and one `"*"`
  item resolves each independently and the object condition is the correct aggregate (refusal dominates).

## Open questions for the reviewer

1. **Singular `namespace` or plural `namespaces` per item?** Singular + `"*"` is the smaller shape and
   the assumption here. Plural adds a hand-listed multi-namespace set that `"*"`-bounded-by-policy
   largely subsumes.
2. **`"*"` against a *selector* policy in v1, or name-only first?** Name-only defers the source-cluster
   Namespace informer and the whole maintaining/sweep path entirely, exactly as the
   [cluster-scope-only page](alt-clusterwatchrule-cluster-scope-only.md#names-only-plural-is-cheap-a-selector-is-not)
   argues. Recommended: ship name-only `"*"`, add selector `"*"` with the informer as a follow-up.
3. **Additive vs subtractive.** This keeps ClusterWatchRule's namespaced scope and PR 5; the
   [sibling page](alt-clusterwatchrule-cluster-scope-only.md) deletes both but is breaking and forces
   the capability questions. Which cost is the right one for this project is the decision these two
   pages exist to frame.
4. **Deny-in-whole for a denied named item** — confirm this over trim-to-admitted-subset. Trimming is
   friendlier and is a silent narrowing, the failure class this whole folder exists to remove.
5. **Is per-type namespace granularity worth the partial-authorization surface?** If not, top-level
   plural `sourceNamespaces` gives the `"*"` insight without per-item auth complexity, at a smaller
   delta to the in-flight PR.
