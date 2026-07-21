# Alternative — the two-object model: scope is carried by the kind, and the enum is removed

> **Status: proposal, for review. Not agreed, not scheduled.** The crisp, breaking form of the
> WatchRule-side redesign, sibling to the non-breaking
> [alt-per-item-source-namespace.md](alt-per-item-source-namespace.md). Both share the same WatchRule
> interface (a per-rule-item `namespace`); they differ only on ClusterWatchRule, and that difference
> is the whole breaking/non-breaking axis this page exists to compare.
>
> Supersedes this file's earlier "narrow the enum to `Cluster`" framing: narrowing left a vestigial
> single-value `scope: Cluster` field on every rule. The crisper move is to **remove the field
> entirely** — scope is redundant with discovery, so the kind can carry it. See
> [why remove, not narrow](#why-remove-the-enum-not-narrow-it).
>
> **PR 4 is being implemented as this is written, top-level singular.** The compiled rule already
> reads `SourceNamespace` at [watched_type_resolver.go:301](../../../internal/watch/watched_type_resolver.go#L301).
> This page is a delta against that; the [comparison](#comparing-the-approaches--breaking-change-consequences)
> is the point of the page.
>
> Code references verified against the tree on 2026-07-21.

## The model

Two objects, each monomorphic in scope, with scope carried by the **kind** rather than by a field:

- **`WatchRule`** follows **namespaced** resources. Its `ResourceRule` list items gain a `namespace`
  option — omitted means the rule's own namespace (legacy), `"*"` means every namespace the
  GitTarget's `allowedSourceNamespaces` **admits**, an explicit name means that source namespace. This
  is the interface designed in full in
  [alt-per-item-source-namespace.md](alt-per-item-source-namespace.md); it is unchanged here.
- **`ClusterWatchRule`** follows **cluster-scoped** resources. Its `ClusterResourceRule` list items
  **lose** the `Scope` field. Every rule is cluster-scoped, because that is what the kind means.

The [`ResourceScope` enum](../../../api/v1alpha3/clusterwatchrule_types.go#L9-L19) is removed from the
API surface of both kinds — WatchRule never had it, ClusterWatchRule no longer needs it. (The internal
`typeset.Scope` used for discovery matching stays; only the user-facing spec field goes.)

~~~yaml
# namespaced — WatchRule, per-item namespace
kind: WatchRule
metadata: { name: repo-config, namespace: tenant-acme }
spec:
  targetRef: { name: acme }
  rules:
    - resources: [configmaps]                    # own namespace (legacy)
    - resources: [secrets]
      namespace: repo-config                      # one source namespace
    - resources: [deployments]
      namespace: "*"                              # every namespace the target's policy admits
---
# cluster-scoped — ClusterWatchRule, no scope field at all
kind: ClusterWatchRule
metadata: { name: acme-crds }
spec:
  targetRef: { name: acme, namespace: tenant-acme }
  rules:
    - resources: [customresourcedefinitions]      # cluster-scoped, implicitly
      apiGroups: [apiextensions.k8s.io]
~~~

The result is the sentence the whole folder was trying to make legible, now true by construction:

> **ClusterWatchRule is the cluster-global surface. WatchRule is the namespaced surface, and every
> namespace it reaches is named in its spec and admitted by its GitTarget.** Which object watches what
> is answerable from the *kind*, never from an audit of a per-rule scope field.

## Why remove the enum, not narrow it

The [earlier revision of this page] narrowed `Scope` to a single legal value, `Cluster`. That leaves a
field every rule must still spell out (`scope: Cluster`) whose only legal value is the default — pure
boilerplate. Removal is crisper, and it is *safe*, because the field is **redundant with discovery**:

- Discovery records already carry scope: [`TypeRecord.Identity.Scope`](../../../internal/typeset/model.go#L39)
  is `Namespaced` / `ClusterScoped` / `Unknown`.
- [`matchFollowableRecords`](../../../internal/watch/watched_type_resolver.go#L342) already filters the
  declared enum **against** that discovered truth via
  [`matchesScope`](../../../internal/watch/watched_type_resolver.go#L397).

So the operator never needed the user to tell it whether `configmaps` is namespaced — it knows. Under
the two-object model the *kind* supplies the scope filter (WatchRule keeps `ScopeNamespaced` records,
ClusterWatchRule keeps `ScopeCluster` records) and discovery supplies the fact. Nothing is lost by
deleting the field; a class of user error — declaring a scope that contradicts discovery — is deleted
with it.

## This deletes PR 5 outright

[PR 5](pr5-clusterwatchrule-source-ceiling.md) exists solely to make `allowedSourceNamespaces` bind
ClusterWatchRule's `scope: Namespaced` streams. If a ClusterWatchRule can only watch cluster-scoped
resources, a **namespace** allow-list is simply not applicable to it — exactly as the
[overview already concedes](README.md#what-the-ceiling-does-not-do) that the ceiling cannot partition
cluster-scoped objects. So the entire PR disappears, not just its API:

| PR 5 mechanism | Fate under the two-object model |
|---|---|
| §1 expand `namespace: ""` per admitted namespace in `collectClusterWatchRuleSelections` | gone — ClusterWatchRule emits `""` for genuinely cluster-scoped streams, no expansion |
| §2 hash resolved scope into `clusterWatchRuleFingerprint` — state that **is not rule state** | gone — a ClusterWatchRule's scope is spec-derived again. This is the subtlest defect in the folder, and it evaporates |
| §2b "unknown is not empty" (the ClusterWatchRule half of the sweep hazard) | gone — nothing narrows on ClusterWatchRule |
| §3 GitTarget → ClusterWatchRules mapper + informer fan-out | gone |
| the release gate coupling PR 4 to PR 5 | gone — there is no second rule kind to leave unenforced |

The dynamic-scope machinery is not *eliminated* — `WatchRule` still needs it for `"*"` against a
**selector** policy — but it is *consolidated onto one kind* instead of living on both. Under the
as-designed plan it exists twice: PR 4's selector `allowedSourceNamespaces` on WatchRule **and** PR 5's
ceiling on ClusterWatchRule. Here it exists once, on WatchRule, and only for rules that write `"*"`.

## Comparing the approaches — breaking-change consequences

Three approaches are now on the table. They agree on the WatchRule interface (per-item `namespace`,
`"*"` bounded by the allow-list); they differ **only** on ClusterWatchRule, and that is the entire
breaking/non-breaking split.

| | **As-designed** (PR 4 + PR 5) | **Per-item, non-breaking** ([sibling](alt-per-item-source-namespace.md)) | **Two-object** (this page) |
|---|---|---|---|
| ClusterWatchRule namespaced watch | `scope: Namespaced`, bounded at runtime by PR 5 | `scope: Namespaced`, bounded at runtime by PR 5 | **removed** — use WatchRule `namespace: "*"` |
| Breaking change | no (additive) | no (additive) | **yes** |
| PR 5 runtime ceiling needed | yes | yes | **no — deleted** |
| §2 fingerprint-not-rule-state hazard | present (must be built) | present (must be built) | **absent** |
| Dynamic (`"*"`/selector) machinery | two kinds | two kinds | **one kind** (WatchRule) |
| Migration of existing objects | none | none | **manual, cross-kind** |
| Conversion webhook can automate it | n/a | n/a | **no** (cross-*kind*) |
| API legibility ("which object watches what") | audit each rule's scope | audit each rule's scope | **read the kind** |
| Enum boilerplate | `scope:` on every cluster rule | `scope:` on every cluster rule | **none** |

The additive approaches cost **more permanent machinery**; the two-object approach costs a **one-time
breaking migration**. The consequences of that migration, in descending order of sharpness:

### 1. No conversion webhook can migrate it — the move is cross-*kind*

A conversion webhook translates one **kind** between API **versions**. Moving "watch namespaced
resources cluster-wide" from `ClusterWatchRule{scope: Namespaced}` to `WatchRule{namespace: "*"}` is a
move between **kinds**, which no conversion webhook can express. So migration is inherently
out-of-band: a one-shot migration tool, or a human editing manifests. The additive approaches need
**no** migration at all, because the old spelling keeps working. This is the single structural fact
that makes the two-object model "breaking" in a way pluralizing a field never is.

### 2. Rollback is a data-plane hazard, not just a config revert

This is the consequence most likely to be underweighted. After migrating a namespaced ClusterWatchRule
to a WatchRule with `namespace: "*"`, rolling the controller **back** to a version that predates the
per-item field means the old controller does not understand `namespace` — it silently watches the
WatchRule's **own** namespace only. That is a scope *collapse*, and a collapsed scope is the input to
the resync sweep ([PR 1](pr1-namespace-scoped-resync.md) is what makes the sweep namespace-safe, not
sweep-free). So a rollback can **delete a tenant's Git content** for every namespace the `"*"` used to
cover. The additive approaches roll back cleanly, because the old field is still present and still
understood.

### 3. Rolling upgrade is non-atomic (HA skew)

During an HA upgrade the old and new controllers run concurrently. The old one reads
`scope: Namespaced` and watches namespaced; the new one ignores/refuses it. A WatchRule written for the
new controller with `namespace: "*"` is invisible to the old one. So there is a window of divergent
behavior that no amount of care removes — the migration cannot be flipped atomically. The additive
approaches have no skew: both controller versions understand both spellings.

### 4. Stored objects are reinterpreted silently unless explicitly refused

CRD schema validation runs on **write**, not read. An existing `ClusterWatchRule{scope: Namespaced}`
stays stored and served after the field is removed from the schema; the controller simply stops reading
`scope` and now treats every rule as cluster-scoped. A rule selecting `configmaps` would resolve
against **cluster-scoped** discovery records, match nothing, and go quietly dead — or worse, a rule
selecting a name that exists in both scopes would flip meaning. **The field removal must be paired with
an explicit compile-path refusal** (see [next section](#removing-the-field-does-not-retract-stored-objects)).
The additive approaches never reinterpret anything.

### 5. GitOps apply loops break loudly

A user who manages the operator's own CRs through Flux/Argo has `ClusterWatchRule{scope: Namespaced}`
manifests in a Git repo. After the schema change their apply either fails validation (enum value gone)
or silently prunes the field, and their sync goes degraded until they rewrite the manifest as a
WatchRule. That is a visible outage of *their* reconciliation, triggered by upgrading the operator. The
additive approaches leave those manifests valid.

### 6. Documentation, samples, e2e, and tests — mechanical but not zero

Every `scope: Namespaced` in the tree changes, plus the docs sentence promising ClusterWatchRule
watches "namespaced resources across multiple namespaces". Surveyed in
[known breakage](#known-breakage).

### What the breaking cost buys

Set against all six: the additive approaches carry PR 5's runtime ceiling **forever** — including the
§2 fingerprint-not-rule-state hazard, which is the subtlest and most regression-prone thing in the
folder — and they answer "will this stream namespaces outside my allow-list?" only by audit. The
two-object model pays once, at a controlled upgrade, and thereafter the answer is the kind. Whether a
one-time cross-kind migration with a genuine rollback hazard is worth deleting a permanent class of
runtime machinery is the decision this page exists to frame. It is not obviously yes; it is a real
trade, and the rollback hazard (#2) is the reason it is not.

## Removing the field does not retract stored objects

The step most likely to be skipped, and it is a **security/correctness** step, not a cleanup — sharpened
from the enum-narrowing version because now the field is *gone*, not merely restricted:

1. Remove the `Scope` field from the schema (blocks new namespaced cluster rules and blocks updates).
2. **In the single gated compile path** [PR 4 step 7](pr4-source-namespace-field.md#implementation-steps)
   routes both the reconciler and `bootstrapRuleStore` through, refuse any `ClusterWatchRule` rule
   whose resource resolves — **via discovery** — to a namespaced type, with a terminal condition naming
   the migration ("watch namespaced resources with a WatchRule and `namespace`"). Discovery already
   knows the scope ([`matchesScope`](../../../internal/watch/watched_type_resolver.go#L397)), so this
   is a check the resolver can already make.

Doing (1) without (2) is the exact defect this folder was written to remove: a scope decided in one
place and not enforced in another. The must-have test is `TestBootstrap_PreExistingNamespacedClusterRuleIsRefused`
— a stored ClusterWatchRule selecting a namespaced type is not compiled and starts no stream, asserted
at bootstrap before any reconcile. No manifest in the repo can create that object once the field is
gone, so the test is the only thing that can catch a regression in the refusal.

## Capability A/B — the questions this model resolves by relocation

The two capabilities `scope: Namespaced` uniquely expressed do not vanish; they move, and the move must
be deliberate:

- **A — "watch this type in every namespace."** Relocates to WatchRule `namespace: "*"`, **bounded by
  the GitTarget policy** — never "every namespace that exists". A tenant author cannot express
  "follow the whole cluster"; the ceiling is the destination's. This is the safe form of A, and it is
  why the model can ship without answering "should we allow live all-namespaces?" — the answer is
  "yes, bounded, and its liveness equals the allow-list's".
- **B — platform-authored namespaced watching from *outside* the tenant namespace.** `WatchRule.targetRef`
  is a `LocalTargetReference` with no namespace ([watchrule_types.go:24-42](../../../api/v1alpha3/watchrule_types.go#L24-L42)),
  so the WatchRule must live in the namespace it watches from, authored by whoever can write there.
  ClusterWatchRule's cross-namespace `targetRef` was the only way for a platform team to configure a
  tenant's namespaced mirror without an object in the tenant namespace — **and the two-object model
  removes that, with no replacement.** If a real deployment relies on B, this model is wrong for it,
  or must add a WatchRule with a namespaced `targetRef` (itself a change). **This is the argument that
  can defeat the whole page; a reviewer who needs B should say so.**

## Known breakage

From a repo-wide sweep on 2026-07-21 (excluding `external-sources/`):

- [config/samples/clusterwatchrule.yaml](../../../config/samples/clusterwatchrule.yaml) — already
  `scope: Cluster`; drop the now-removed field.
- [test/e2e/unsupported_folder_e2e_test.go:177](../../../test/e2e/unsupported_folder_e2e_test.go#L177)
  — a `scope: Namespaced` ConfigMap ClusterWatchRule used only to exercise the refusal path; convert
  to a WatchRule or a cluster-scoped rule.
- [docs/configuration.md](../../configuration.md) — "namespaced resources across multiple namespaces"
  under `ClusterWatchRule` becomes false; the `ClusterWatchRule` example and the `scope` field docs go.
- The [`ResourceScope` enum + constants](../../../api/v1alpha3/clusterwatchrule_types.go#L9-L19) leave
  the **API**; `typeset.Scope` and `matchesScope` stay (discovery still needs them).
- Unit tests across `internal/watch`, `internal/controller`, `internal/rulestore` that construct
  `Namespaced` cluster rules — mechanical.

This is a sweep, not a proof: a reviewer who believes a deployment depends on capability B above
outweighs all of it.

## Test plan delta

Relative to PR 4 and PR 5 as written:

- **Deleted:** every PR 5 ceiling test — narrowing, sparing cluster-scoped rules, the
  `clusterWatchRuleFingerprint` resolved-scope test, the table-rebuild-on-policy-change test,
  retain-on-unknown, and the establishing/maintaining pair for ClusterWatchRule. The mechanisms they
  guard no longer exist.
- **Added (the critical one):** `TestBootstrap_PreExistingNamespacedClusterRuleIsRefused` — §4 above.
- **Added:** admission/compile refuses a namespaced-resolving ClusterWatchRule rule, message naming the
  WatchRule + `namespace` migration.
- **Inherited from the sibling doc:** the whole WatchRule per-item namespace test plan
  ([alt-per-item § test plan delta](alt-per-item-source-namespace.md#test-plan-delta-relative-to-pr-4-as-written)),
  including the partial-object deny-in-whole case and the `"*"` establishing/maintaining cases.
- **Kept:** PR 4's two must-have tests, the fingerprint/selection silent-failure guards, and the
  Appendix-A Git-path e2e.

## Open questions for the reviewer

1. **Capability B** — must a platform team author namespaced watching for a tenant target from outside
   the tenant namespace? If yes, the two-object model removes it with no replacement and should be
   rejected or paired with a namespaced-`targetRef` WatchRule.
2. **Is the one-time cross-kind migration, with the rollback data-plane hazard (#2), an acceptable
   price** for deleting PR 5's permanent machinery — on a preliminary v1alpha3 whose
   [compatibility policy](README.md#compatibility) already accepts observable changes without
   conversion?
3. **Migration tooling** — ship a one-shot converter (ClusterWatchRule `scope: Namespaced` → WatchRule
   `namespace: "*"`), or document the manual rewrite? A converter cannot be a conversion webhook (#1),
   so it is a standalone tool either way.
4. **Singular `namespace` vs plural `namespaces`** on the WatchRule rule item, and **name-only `"*"`
   first vs selector-backed `"*"`** — both inherited from the
   [sibling doc's open questions](alt-per-item-source-namespace.md#open-questions-for-the-reviewer);
   the selector choice is what determines whether the one remaining dynamic machinery ships now or later.

## What does not change

PR 1, PR 2, PR 3 are unaffected and remain landed. `GitTarget.allowedSourceNamespaces`, the delegation
flag, the in-cluster sign-off argument, and the `IsLocalSource()` trap all stand verbatim. Git
placement still follows each mirrored object's own namespace, so the write path needs no change.
