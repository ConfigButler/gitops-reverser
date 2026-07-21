# PR 4 — scope by kind: cluster-only ClusterWatchRule and per-item WatchRule source namespaces

> Phase 4 of [source-namespace addressing](README.md). **Breaking change**, accepted for the
> preliminary v1alpha3 API. Depends on [PR 1](pr1-namespace-scoped-resync.md),
> [PR 2](pr2-stream-scope-collapse.md), and [PR 3](pr3-clusterwatchrule-target-admission.md).
>
> **Release gate.** [PR 5 deletion safety](pr5-gittarget-deletion-safety.md) is implemented in the PR
> immediately after this one. **No release may be cut between the two merges** — the first release
> containing this PR also contains PR 5. See [§9](#9-release-and-rollback).
>
> This PR replaces the unshipped top-level `sourceNamespace` interface described in the
> [historical implementation baseline](historical-top-level-source-namespace-baseline.md). Its
> authorization model, three-valued source-scope service, status contract, and single gated compile
> path are **reused as built**; only the field's location and cardinality change.
>
> Code references verified against the tree on 2026-07-21.

## End state

Scope is carried by the rule kind:

- **WatchRule** selects namespaced source resources. Each `ResourceRule` has an optional
  `sourceNamespace`.
- **ClusterWatchRule** selects cluster-scoped source resources. Its per-item scope choice is gone.

~~~yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata: { name: acme, namespace: tenant-acme }
spec:
  # The destination's exhaustive source-namespace policy. Names and selector are ORed.
  allowedSourceNamespaces:
    names: [tenant-acme, repo-config]
    selector:
      matchLabels:
        gitops.configbutler.ai/mirrorable: "true"
---
apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata: { name: repo-config, namespace: tenant-acme }
spec:
  targetRef: { name: acme }
  rules:
    - resources: [configmaps]              # omitted → this WatchRule's own namespace
    - resources: [secrets]
      sourceNamespace: repo-config         # one admitted source namespace
    - resources: [deployments]
      sourceNamespace: "*"                 # every namespace the target admits, live
---
apiVersion: configbutler.ai/v1alpha3
kind: ClusterWatchRule
metadata: { name: acme-crds }
spec:
  targetRef: { name: acme, namespace: tenant-acme }
  rules:
    - resources: [customresourcedefinitions]
      apiGroups: [apiextensions.k8s.io]
~~~

The resulting audit rule is one question per kind: a WatchRule's source namespaces are exactly what
its GitTarget's policy admits; a ClusterWatchRule is intentionally cluster-global and is bounded only
by its source credential's RBAC.

## Existing PR 4 work to keep

The current implementation has the right authorization and safety boundaries. Rework it in place; do
not replace it with a second implementation. Only its single-namespace API and representation are
discarded.

| Current implementation | PR 4 treatment |
|---|---|
| `ClusterProvider.spec.allowWatchRuleSourceNamespaceOverride` | **Keep the semantics; rename to `allowSourceNamespaceOverride`.** It stays the platform-admin delegation for cross-source-namespace WatchRules, defaults to `false`, and stays deliberately separate from connectivity. |
| `GitTarget.spec.allowedSourceNamespaces` and `NamespaceMatcher` | **Keep unchanged.** It remains the destination-owned, exhaustive policy. Under this design it bounds WatchRules only; ClusterWatchRule has no namespaced selections to bound. |
| `SourceNamespaceAuthorized` condition, kstatus result, and user-facing denial reasons | **Keep.** It becomes the aggregate verdict over all rule items — see the [aggregation order](#5-status-contract-for-per-item-scopes). |
| `internal/authz` three-part gate and exact-name / selector evaluation | **Keep and call per item.** An explicit name can still be admitted by a policy selector; exact names remain usable when source-namespace labels cannot be read. |
| Shared `CompileWatchRule` path and bootstrap enforcement | **Keep.** Both reconcile and bootstrap must continue to use one gated compile path, so a restart cannot open an unauthorized watch window. |
| Source-cluster Namespace snapshot, three-valued result, retention on an unavailable selector, and requeue wiring | **Keep, and now also drive wildcard sets from it.** Refactor the retained state from one namespace to a complete resolved scope for a WatchRule. |
| GitTarget, ClusterProvider, and source-Namespace change mappers | **Keep.** A policy or provider revocation must remove the compiled rule and stop streams promptly. |
| Unit, controller, bootstrap, planning, status, and end-to-end gate tests | **Keep as the regression suite**, changing their fixtures from one top-level namespace to rule-item scopes. |

Replace only these single-scope pieces:

- `WatchRule.spec.sourceNamespace`, `EffectiveSourceNamespace()`, and `OverridesSourceNamespace()`
  become `ResourceRule.sourceNamespace` resolution.
- `CompiledRule.SourceNamespace string` and the `NamespacedName → string` retained-grant map become an
  atomically replaced, whole-WatchRule resolved scope.
- ClusterWatchRule's public scope choice and all namespaced ClusterWatchRule runtime behavior go away.

### Cross-source-namespace delegation stays on ClusterProvider, as `allowSourceNamespaceOverride`

**Keep the delegation boolean, default `false`, and rename it to `allowSourceNamespaceOverride`.**
ClusterProvider is the right object: it is cluster-scoped and platform-admin-owned, so the
administrator who owns the source credential explicitly decides whether owners of admitted GitTargets
may choose source namespaces other than their WatchRule's namespace.

The `WatchRule` prefix in the old name was carrying the disambiguation, and after [§1](#1-clusterwatchrule-becomes-cluster-only)
there is nothing left to disambiguate: ClusterWatchRule no longer has a source-namespace choice, so a
source-namespace override is a WatchRule concept by construction. `allowSourceNamespaceOverride`
reads against the vocabulary the rest of this design already uses — `sourceNamespace`,
`allowedSourceNamespaces`, `SourceNamespaceAuthorized` — and the load-bearing half of the name, the
fact that it delegates a **source namespace** and not a `targetRef`, is retained.

Still do not widen it to `allowCrossNamespaceWatchRules`: that would read as permitting a
cross-namespace `targetRef`, which [decision 8](#closed-design-decisions) explicitly refuses.

The field never reached a release, so this is a plain rename with no deprecation shim — unlike the two
superseded fields in [decision 10](#closed-design-decisions), no stored object can carry the old key,
because the CRD that defines it has not shipped.

~~~yaml
apiVersion: configbutler.ai/v1alpha3
kind: ClusterProvider
metadata:
  name: workspaces
spec:
  # False by default. A platform admin opts in deliberately.
  allowSourceNamespaceOverride: true
---
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: acme
  namespace: tenant-acme
spec:
  allowedSourceNamespaces:
    names: [repo-config, team-payments]
~~~

The provider flag is necessary but never sufficient. A cross-source-namespace request also requires
that the ClusterProvider admits the GitTarget's namespace and that the GitTarget policy admits the
requested source namespace. The credential's Kubernetes RBAC remains the hard maximum.

| WatchRule item request | Provider delegation | GitTarget policy | Outcome |
|---|---|---|---|
| omitted, or equal to the WatchRule namespace | not required | absent | allowed, legacy behavior |
| omitted, or equal to the WatchRule namespace | not required | declared and includes that namespace | allowed |
| omitted, or equal to the WatchRule namespace | not required | declared but does not include that namespace | refused; the policy is exhaustive |
| a different explicit name | required | declared and admits that name, by name **or by selector** | allowed |
| a different explicit name | absent or `false` | any | refused |
| a different explicit name | `true` | absent, or does not admit that name | refused |
| `"*"` | required | absent | refused — deny-by-default; `"*"` is not a backdoor |
| `"*"` | required | declared | expands to exactly the admitted set, by names and/or selector |

`"*"` requires delegation even if the policy happens to list only the WatchRule's own namespace: it
expresses a request to follow the policy's set, and a later policy edit could otherwise widen the
watch without the platform-admin opt-in.

## Closed design decisions

These close the questions from the rejected alternatives. They are part of PR 4's contract, not
implementation choices to revisit while coding.

1. **Use singular `rules[].sourceNamespace`.** Repeat a ResourceRule when two explicitly named
   namespaces need the same resource selector. Do not add `sourceNamespaces: []string`: `"*"` already
   expresses the useful many-namespace case, bounded by the target policy, without a second list
   shape.
2. **Name the field `sourceNamespace`, not `namespace`.** On a namespaced CRD, `rules[].namespace`
   reads ambiguously against `metadata.namespace`. `sourceNamespace` matches the condition
   (`SourceNamespaceAuthorized`), the target field (`allowedSourceNamespaces`), and this folder's
   vocabulary.
3. **Ship the selector-backed wildcard in this release.** A wildcard restricted to names-only policies
   is merely an alias for the list; the dynamism is the whole point, and it is what replaces the
   removed all-namespaces capability. The source-Namespace snapshot, three-valued result, and
   retention path this needs are already built and shipped by the baseline PR — this reuses them
   rather than adding them.
4. **`allowedSourceNamespaces: {selector: {}}` is the "every source namespace" declaration.** A
   present-but-empty selector matches every namespace, and it stays self-updating. See
   [§3](#3-which-namespaces-a-target-admits).
5. **Refuse the whole WatchRule when any explicit named item is denied.** Do not trim the denied item
   and run a partial rule. A `"*"` item resolving to an empty admitted set is different: it is valid,
   produces no selections for that item, and does not stall the WatchRule — but it must be visible.
6. **Keep per-resource-rule namespace granularity.** It is worth the atomic resolved-scope refactor:
   one WatchRule can intentionally follow different resource types in its own namespace, a named
   source namespace, or the target's admitted set. This is clearer than a top-level plural field and
   makes the authorization visible beside the resource selector it applies to.
7. **Adopt the breaking two-object model.** ClusterWatchRule is cluster-scoped only; WatchRule is the
   namespaced surface. The lower permanent runtime complexity is worth the one-time migration on this
   preliminary API.
8. **Do not retain platform-authored namespaced watches from outside the target namespace.** A
   platform administrator may manage the manifest, but it must create the WatchRule in the target
   namespace. Do not add a namespaced `targetRef` or keep namespaced ClusterWatchRule as a workaround
   without a new authorization design.
9. **No migration tool.** The conversion is mechanical and the API is preliminary. The breaking change
   is carried by the release notes and [UPGRADING.md](../../UPGRADING.md) — see [§8](#8-migration).
10. **The one field that SHIPPED stays in the schema for one release as a loud rejection.** Deleting a
    field makes a re-applied legacy manifest *silently pruned*; keeping it makes the apply fail. That
    reasoning applies to `ClusterResourceRule.scope` and only to it — `WatchRule.spec.sourceNamespace`
    never reached a release, so it is deleted outright. See
    [§1](#1-clusterwatchrule-becomes-cluster-only) and
    [§2](#2-watchrule-gains-rulessourcenamespace).
11. **Keep the ClusterProvider delegation boolean, renamed `allowSourceNamespaceOverride`.** `false`
    remains the default and it is required for every cross-source-namespace request, including `"*"`.
    GitTarget policy remains the independent, narrower approval.

## 1. ClusterWatchRule becomes cluster-only

`ClusterResourceRule.scope`
([clusterwatchrule_types.go:111-117](../../../api/v1alpha3/clusterwatchrule_types.go#L111-L117)) stops
being a scope selector.

**Keep the field; narrow it.** Make it `+optional` with `+kubebuilder:default=Cluster` and
`+kubebuilder:validation:Enum=Cluster`, and say in its description that it is deprecated and names its
replacement. Deleting the field instead would be worse in two ways, and both are silent:

- CRD pruning happens on **write**. Once the schema drops the field, re-applying a legacy
  `scope: Namespaced` manifest is accepted with the value pruned away — no error anywhere — and the
  rule quietly stops mirroring namespaced objects.
- A stored pre-release object keeps its value in etcd, but Go code that no longer has the field cannot
  read it, so the controller has nothing to refuse. Inferring the refusal from "this selector resolved
  to a namespaced type" instead is ambiguous for `resources: ["*"]`, which legitimately resolves
  cluster-scoped records — see [the restart fixture](../../../test/e2e/templates/restart/watchrule-wildcard.tmpl).

Retaining a narrowed enum gives an apply-time API-server rejection for the first case and a readable
Go value for the second. Remove the field entirely one release later, or at `v1beta1`.

Three enforcement points, in this order:

1. **Admission** — the narrowed enum rejects `scope: Namespaced` at apply time.
2. **Compile** — in the single gated path shared by bootstrap and reconcile
   ([watchrule_compile.go:61](../../../internal/watch/watchrule_compile.go#L61) and its ClusterWatchRule
   sibling), refuse any ClusterWatchRule holding a stored scope other than `Cluster`. The rule compiles
   **no** stream and the terminal condition says:

   > ClusterWatchRule is cluster-scoped only; watch namespaced resources with a WatchRule and
   > `rules[].sourceNamespace`.

3. **Resolution** — `collectClusterWatchRuleSelections`
   ([watched_type_resolver.go:312-329](../../../internal/watch/watched_type_resolver.go#L312-L329))
   always matches with `ResourceScopeCluster`, so even a pruned or absent value cannot widen a stream.
   Its selections keep using namespace `""`, now only for genuinely cluster-scoped types.

The `ResourceScope` Go type and both constants stay: `matchesScope`
([watched_type_resolver.go:397](../../../internal/watch/watched_type_resolver.go#L397)) still needs them
internally to resolve WatchRule selectors against namespaced records. What goes away is the *public*
choice.

The critical test is `TestBootstrap_PreExistingNamespacedClusterRuleIsRefused`: it must compile no
stream before the first reconciliation can publish status.

This also deletes the abandoned runtime-ceiling design for that kind: no ClusterWatchRule
source-namespace expansion, no `clusterWatchRuleFingerprint` scope component, no ClusterWatchRule
source-policy mapper, no selector-outage retention path for that kind.

## 2. WatchRule gains `rules[].sourceNamespace`

Add `sourceNamespace` to `ResourceRule`
([watchrule_types.go:85](../../../api/v1alpha3/watchrule_types.go#L85)). Validate it structurally as
either the literal `"*"` or a DNS-1123 label — lower-case alphanumerics and `-`, starting and ending
alphanumeric, `MaxLength=63` — so a malformed namespace is rejected at admission rather than resolving
to nothing at compile time.

`ResourceRule` is not shared with `ClusterResourceRule`, so the field cannot leak into the cluster
kind.

| `rules[].sourceNamespace` | Meaning |
|---|---|
| omitted | The WatchRule's own namespace. Legacy behavior, byte-for-byte |
| explicit name | One source namespace, admitted by the target policy and the provider delegation gate |
| `"*"` | Every source namespace `GitTarget.allowedSourceNamespaces` admits — never every namespace that exists |

**Delete `WatchRule.spec.sourceNamespace` outright** — unlike `scope`, which is retained as a
rejection. The retention argument is about STORED objects: an unrecognised top-level field is pruned on
re-apply, and a stored pre-release value that Go can no longer read would make the rule silently watch
its own namespace instead of the one it asked for. Neither hazard exists here, because the field was
never on `main` and so cannot be in any cluster's etcd or in any manifest. Retaining a rejection for a
field nobody can have set would be ceremony, and it would leave a phantom in `kubectl explain` that
readers must be told to ignore.

The Go field, the CEL guard, the `SourceNamespaceFieldRemoved` reason, and the compile-path refusal
all go with it. Pre-release manifests that still set the field — including this repo's own fixtures —
are updated in the same change, which is the whole affected population.

**A denied explicit name denies the whole WatchRule.** The object reports
`SourceNamespaceAuthorized=False`, `Stalled=True`, starts no streams, and its message names the failing
item. Partial success — mirroring two of the three namespaces the rule asked for — is worse than a loud
failure. A wildcard never denies: it narrows to the admitted set, which may be empty.

## 3. Which namespaces a target admits

`GitTarget.spec.allowedSourceNamespaces` is the existing
[`NamespaceMatcher`](../../../api/v1alpha3/namespace_matcher.go) and does not change. What changes is
that a wildcard now resolves through it:

| Policy on the GitTarget | `sourceNamespace: "*"` resolves to |
|---|---|
| undeclared | **denied** — deny-by-default. The message names the fix |
| `{}` (declared, empty) | nothing. An empty declared policy admits nothing, by the matcher's contract |
| `names: [a, b]` | exactly `a` and `b`, statically, with no source-cluster access |
| `selector: {matchLabels: …}` | every source namespace carrying those labels, live |
| `selector: {}` | **every source namespace** — the deliberate "all namespaces" declaration |

The last row is the replacement for today's `ClusterWatchRule` + `scope: Namespaced`, and it is
strictly better: it is declared by the destination owner rather than by the rule author, it is legible
on the GitTarget, and it stays self-updating as namespaces come and go. `LabelSelectorAsSelector`
returns `labels.Everything()` for a present-but-empty selector and `labels.Nothing()` for a nil one,
which is exactly the declared-versus-absent distinction `NamespaceMatcher` is built around — add a test
pinning it, because it currently has none.

Two invariants carry over from the historical baseline unchanged and must not be re-litigated:

- **No self-namespace exception.** Once a policy is declared it is exhaustive, including for an omitted
  item that resolves to the rule's own namespace ([README](README.md#no-self-namespace-exception)).
- **Names stay answerable without source-cluster Namespace access.** `MatchesName` is consulted before
  the selector, so a cluster whose Namespace `list` is `Forbidden` still supports name-based policies —
  including a `"*"` item against a names-only policy. This degradation path is the half most likely to
  regress unnoticed.

### Prior art: how Flux does this

Flux has exactly one implemented namespace ACL: `AccessFrom` in
[fluxcd/pkg](https://github.com/fluxcd/pkg) (`apis/acl/acl_types.go`), evaluated by
`runtime/acl.Authorization.HasAccessToRef`. It is worth reading before implementing this section
because four of its choices are the same as ours, and three of the differences are load-bearing.

**Same mechanism, independently arrived at:**

- Namespace **labels** are the grant, and the selectors are a list evaluated with a logical OR — our
  `names` ∪ `selector`, ORed.
- **An empty selector matches every namespace**, stated in the type's own godoc: *"An empty map of
  MatchLabels matches all namespaces in a cluster."* That is [decision 4](#closed-design-decisions)
  and [§3](#3-which-namespaces-a-target-admits)'s last row, already shipped upstream.
- **Absent ACL denies.** `HasAccessToRef` refuses when the referenced object declares none.
- **"Cannot say" is not "denied."** The Namespace read failure returns a plain error while a real
  mismatch returns a distinctly typed `AccessDeniedError`, and callers branch on `IsAccessDenied`.
  That is [§4.4](#44-unknown-is-not-empty)'s three-valued result in a different vocabulary — worth
  citing when someone proposes collapsing it back to a boolean.
- The **literal `*`** is Flux's token for handing matching over to a selector too:
  `CrossNamespaceObjectReference.name: "*"`, whose `matchLabels` field documents *"MatchLabels
  requires the name to be set to `*`."*

**Where we deliberately differ:**

1. **Direction.** Flux's ACL sits on the object being *referenced* and admits *referrers* by their
   namespace labels. `allowedSourceNamespaces` sits on the destination and admits the namespaces data
   is *read from*. Same deny-unless-granted skeleton, different axis; Flux has no precedent for a
   data-flow-direction policy, so nothing upstream validates the direction for us.
2. **The self-namespace exception.** `HasAccessToRef` returns "allowed" for a same-namespace reference
   *before* it looks at the ACL. We deliberately have none
   ([README](README.md#no-self-namespace-exception)): a declared policy is exhaustive. Flux can afford
   the shortcut because a same-namespace reference grants nothing the referrer's own RBAC did not
   already imply; ours would let a rule keep writing a namespace's content into Git after the
   destination owner tightened the policy to exclude it.
3. **Flux never enumerates.** Every Flux check has one concrete candidate namespace, `Get`s that one
   Namespace, and matches its labels. Nothing upstream turns a selector into a namespace *set*.
   `sourceNamespace: "*"` does, which is why this design needs the source-cluster Namespace snapshot,
   the refresh cadence, and — the part with no upstream analogue to copy —
   [§4.3](#43-invalidation--the-silent-one)'s fingerprint over the resolved set. Treat that as the
   unproven half.
4. **Selector shape.** Flux's `NamespaceSelector` carries `matchLabels` only — no `matchExpressions`,
   no name list. We keep a full `LabelSelector` plus `names`, because names stay answerable when the
   source cluster forbids Namespace `list` ([§3](#3-which-namespaces-a-target-admits)). Flux's shape
   has no such degradation path; it simply fails the check.

**And the caution.** This mechanism is the part of Flux's model that did *not* grow. In the manifests
flux-operator bundles, Flux 2.6.4 carried `accessFrom` on `Bucket`, `GitRepository`, `HelmChart`,
`HelmRepository`, and `ImageRepository`; by 2.9.0 only `HelmRepository` and `ImageRepository` still
have it, and the `HelmRepository` copy is annotated in-schema *"NOT implemented, provisional as of
[fluxcd/flux2#2092](https://github.com/fluxcd/flux2/pull/2092)"*. `OCIRepository` never got one. So
after roughly four years the selector ACL is implemented for exactly one reference —
`ImagePolicy` → `ImageRepository`. There is no design RFC for it either:
[RFC-0001](https://github.com/fluxcd/flux2/blob/main/rfcs/0001-authorization/README.md) is a
memorandum recording the model as of v0.24, and it files cross-namespace references under *security
considerations* with an admission-controller workaround rather than a field.

What Flux shipped instead is a **cluster-wide, platform-admin boolean**: `--no-cross-namespace-refs`
(fluxcd/pkg `runtime/acl/flags.go`), which flux-operator's multitenant profile applies to every
controller alongside `--default-service-account`, and which
[RFC-0012](https://github.com/fluxcd/flux2/blob/main/rfcs/0012-external-artifact/README.md)
recommends third-party controllers expose. Tenancy is then enforced by RBAC and impersonation.

Two things follow for this PR. First, it supports keeping the delegation boolean: an admin-owned
on/off switch is the part of this shape with a proven track record, and ours is strictly better placed
than Flux's — an API field on the cluster-scoped ClusterProvider is per-source-credential and
per-cluster, where a process flag is per-deployment. Second, it argues for keeping the selector half
**narrow**: one policy field, on GitTarget, with no per-kind variants and no second ACL surface
elsewhere in the API. The source credential's RBAC remains the hard ceiling, exactly as it is for Flux.

## 4. Compile, expansion, and invalidation

### 4.1 The resolved scope

`CompiledRule.SourceNamespace string` ([store.go:20-49](../../../internal/rulestore/store.go#L20)) is a
single-namespace representation and cannot be reused. Replace it with a concrete namespace set **per
compiled resource rule**: `CompiledResourceRule.SourceNamespaces []string`, always expanded to names by
compile time. Neither a wildcard nor a policy reference survives into the data plane.

The resolved scope is a **pure function of (rule spec, target policy, source Namespace snapshot),
recomputed on every compile and replaced atomically**. Nothing per-item is persisted across a spec
change, which disposes of the rule-item identity problem: items need no stable API identity because no
state outlives the spec that produced them.

The one exception is the retained grant the establishing/maintaining contract requires
(`sourceNamespaceScope.grants`,
[source_namespace_scope.go](../../../internal/watch/source_namespace_scope.go)). Widen it from
`map[NamespacedName]string` to the whole resolved scope and **key it by the rule's spec hash**:
retention applies only while the spec is unchanged, and a spec edit discards it and re-establishes from
scratch. Keying by item index would let a reorder inherit another item's grant.

### 4.2 Expansion, not filtering

`collectWatchRuleSelections`
([watched_type_resolver.go:284-307](../../../internal/watch/watched_type_resolver.go#L284-L307)) appends
one selection per **(matched record × resolved namespace)**.

Expand here rather than filtering events at the read site. An expanded selection carries the scope
through the plan hash, the informers, **and** the resync path for free; a read-site filter has to be
repeated at each of them and is silently wrong if one is missed — and it would also mean an unfiltered
LIST/WATCH over every namespace, so the data crosses into the process before being dropped.

Neither an omitted item nor a `"*"` item ever emits a raw `""` key: both resolve to concrete names.
Only ClusterWatchRule emits `""` now, so [PR 2](pr2-stream-scope-collapse.md)'s collapse rules are
unaffected.

### 4.3 Invalidation — the silent one

`rulesFingerprint`
([watched_type_resolver.go:469](../../../internal/watch/watched_type_resolver.go#L469)) is computed
**only from compiled rules** and is what gates the table rebuild. A wildcard's inputs — the GitTarget
policy and source-cluster Namespace labels — are **not rule state**, so a mapper that merely requeues
the WatchRule is not sufficient: reconciliation runs, the fingerprint is unchanged, the rebuild is
skipped, and the resident table keeps the old namespace set. Streams carry on at their old width and
every diff looks correct, because the rule object genuinely did not change.

So `watchRuleFingerprint`
([watched_type_resolver.go:490](../../../internal/watch/watched_type_resolver.go#L490)) must hash **each
item's resolved namespace set**, replacing its single `src=` component. Since compilation is what
resolves the set, the fingerprint sees it for free — provided compilation always precedes the rebuild.

### 4.4 Unknown is not empty

An unevaluatable selector must **never** be read as a valid empty allow-list. An empty resolved set
means "watch nothing", and combined with a resync it means Git content for those namespaces leaves the
desired snapshot — a transient outage becomes the input to a sweep. The three-valued result already
built into `authz.SourceScopeResult`
([source_namespace.go](../../../internal/authz/source_namespace.go)) exists for exactly this, and the
[establishing versus maintaining contract](historical-top-level-source-namespace-baseline.md#establishing-versus-maintaining-a-scope)
applies verbatim, now per item:

- **Establishing** (no retained scope for this spec): the item does not compile. A retryable error is
  `Unknown`/`CheckingSourceNamespacePolicy`; a permanent one is
  `False`/`SourceNamespacePolicyUnavailable`/`Stalled=True`.
- **Maintaining** (a retained scope exists for this spec): retain it, keep running, no narrowing, no
  widening, **no sweep**. `Unknown`/`SourceNamespacePolicyUnavailable`, `Stalled=False`.
- **Denial** — the policy evaluated and does not admit — is terminal in both directions and must not
  share a code path with "cannot say".

On a denial or revocation, remove the compiled rule and replan the watch manager **before** publishing
terminal status. On "cannot say", do neither.

### 4.5 The gate becomes per item

`authz.WatchRuleSourceNamespaceAdmitted`
([source_namespace.go:135](../../../internal/authz/source_namespace.go#L135)) currently derives its
candidate from `rule.EffectiveSourceNamespace()`. Split that: the ordering contract (own namespace free
while no policy is declared; a different namespace needs provider admission plus the delegation flag; a
declared policy is exhaustive) is per **candidate namespace** and stays exactly as built. The caller
supplies the candidate, and for `"*"` asks the resolver to enumerate the admitted set instead of testing
one name. `EffectiveSourceNamespace()` and `OverridesSourceNamespace()` move down to the item.

## 5. Status contract for per-item scopes

`SourceNamespaceAuthorized` stays one condition per object, per the
[status contract](historical-top-level-source-namespace-baseline.md#status-contract-kstatus-compatible).
With mixed items it needs a stated aggregation, or two implementations will disagree.

**Reason precedence** (first match wins):

1. any item denied → `False` / `SourceNamespaceNotAllowed` / `Stalled=True`
2. any item permanently unevaluatable while establishing → `False` /
   `SourceNamespacePolicyUnavailable` / `Stalled=True`
3. any item retaining a scope it can no longer re-evaluate → `Unknown` /
   `SourceNamespacePolicyUnavailable` / `Stalled=False`
4. any item still resolving → `Unknown` / `CheckingSourceNamespacePolicy`
5. every item admitted, at least one naming a namespace other than the rule's own → `True` /
   `SourceNamespaceAllowed`
6. every item omitted → `True` / `LegacySourceNamespace`

**Messages name the item**, by index *and* by its resources and requested namespace — an index alone
goes stale the moment somebody reorders the list while reading the message.

**An admitted-but-empty wildcard must be visible.** A rule whose items all resolved to zero namespaces
is not stalled, but it must not read as healthy either: report `True` with reason
`NoAdmittedSourceNamespaces` and let the existing `StreamsRunning` / `ResourcesResolved` surfaces show
the zero. A rule that mirrors nothing while reporting `Ready=True` with no explanation is a silent
no-op.

**`StreamSummaryForWatchRule`
([stream_readiness.go:133](../../../internal/watch/stream_readiness.go#L133)) can no longer be computed
from the spec.** It takes a `configv1alpha3.WatchRule` and rebuilds its keys from
`EffectiveSourceNamespace()`; with a wildcard the resolved set exists only in the compiled rule, so the
roll-up must read the compiled rule (or the resolved scope) instead. Missing this makes a wildcard rule
permanently not-ready while its streams run — the same class of bug the singular field already hit once.

The `SourceAuthorized` printer column stays as shipped.

## 6. Reactivity

| Input changes | Required reaction | Status after the baseline PR |
|---|---|---|
| WatchRule spec | Recompile, re-resolve, re-project | Wired |
| GitTarget `allowedSourceNamespaces` | Recompile its WatchRules **and re-project the table** | Mapper wired; the fingerprint half is [§4.3](#43-invalidation--the-silent-one) |
| ClusterProvider `allowedNamespaces` or the delegation flag | Reconcile affected GitTargets **and their WatchRules** | Wired — the ClusterProvider → WatchRules mapper shipped with the baseline |
| Source-cluster Namespace labels | Re-resolve selector-backed items; grant and revoke | Wired — the per-source-cluster snapshot and its enqueue channel shipped with the baseline; it must now also drive wildcard sets |

The snapshot remains a refresh-cadence re-list, not an informer, and remains armed lazily by a selector
policy actually asking. An install with no selector policies never lists a namespace.

## 7. Wildcard fan-out is an accepted cost

`targetWatchSpecs` opens one stream per `(GVR, namespace)` scope
([target_watch.go:230-243](../../../internal/watch/target_watch.go#L230-L243)) and `ResyncScope` is
single-namespace ([git/types.go:334](../../../internal/git/types.go#L334)). So a `"*"` item over **N**
admitted namespaces and **M** matched types opens **N×M** informers and produces **N×M** resync scopes,
where today's cluster-wide ClusterWatchRule uses M.

This is the price of the safe direction — the alternative is one wide stream filtered at the read site,
which loses the per-namespace resync scope [PR 1](pr1-namespace-scoped-resync.md) landed to create.
Accept it here, state the shape in the field documentation, and follow up: a cluster-wide stream whose
gather carries a namespace **set** would collapse the fan-out without widening the sweep. Tracked in
[docs/TODO.md](../../TODO.md).

The `PendingSample` cap of five entries
([watchrule_types.go:182](../../../api/v1alpha3/watchrule_types.go#L182)) also stops being
representative once totals are N×M. Leave the cap; do not grow the status object.

## 8. Migration

Two capabilities are removed on purpose:

- **Platform-authored namespaced mirroring from outside the tenant namespace.** ClusterWatchRule's
  cross-namespace `targetRef` let a platform team mirror a tenant's namespaced resources with no object
  in that tenant's namespace. A platform administrator may still own the manifest, but it must now live
  in the tenant namespace. If that is unacceptable for a deployment, stop this PR or design a separate
  namespaced-target WatchRule with its own authorization review.
- **Rule-author-declared all-namespace watching.** `scope: Namespaced` let the rule author reach every
  namespace. The replacement is destination-declared: `allowedSourceNamespaces: {selector: {}}` plus
  `sourceNamespace: "*"` — same reach, declared by the GitTarget owner instead of the rule author.

There is **no migration tool**. A conversion webhook cannot perform a cross-kind move, and a dry-run
generator is more surface than this change deserves on a preliminary API with a small install base.
Instead:

- A `feat(api)!:` commit with a `BREAKING CHANGE:` footer, so the removal is unmissable in the generated
  release notes.
- A section in [UPGRADING.md](../../UPGRADING.md), adjacent to the existing v1alpha2 → v1alpha3 rewrite
  instructions, containing:
  1. the two removed capabilities above;
  2. the conversion — a namespaced `ClusterWatchRule` becomes a `WatchRule` **in the tenant namespace**;
     its namespaced items become `sourceNamespace: "*"` (or explicit names); any cluster-scoped items
     stay behind in a revised ClusterWatchRule;
  3. the warning that a target with no policy admits **only** a WatchRule whose every item watches its
     own namespace — every wildcard or cross-namespace item a conversion produces is denied — so
     converting without also declaring `allowedSourceNamespaces` narrows production data; and that
     under PR 5's `OnEvent` default the narrowing leaves stale documents in Git rather than deleting
     them;
  4. a `kubectl get clusterwatchrules -o json | jq …` one-liner that lists every affected object and its
     target.

## 9. Release and rollback

PR 5 is implemented in the PR immediately after this one; both merge before any release is cut.

1. Merge this PR. **`main` is now in a do-not-release window.**
2. Merge [PR 5](pr5-gittarget-deletion-safety.md), which adds `prune.mode` and makes the resync sweep
   opt-in. The window closes.
3. Release both together, with the breaking change in the notes.

**Rolling back the controller past that release is unsupported while migrated manifests exist**, and the
reason belongs in the release notes: the previous controller neither understands
`rules[].sourceNamespace` (so a rule resolves to its own namespace — a *narrower* desired set) nor has
`prune.mode` (so a resync sweeps). Together that deletes the mirrored namespaces' documents. If a
rollback is unavoidable, remove or narrow the affected WatchRules **first**.

The same skew exists inside a rolling upgrade: CRDs are cluster-wide, so an old leader can observe a new
WatchRule, ignore `rules[].sourceNamespace`, and mirror the wrong namespace's content into Git. Complete
the controller rollout before applying migrated manifests.

## Rework sequence

1. **API and generated contract.** Move `sourceNamespace` from `WatchRuleSpec` onto `ResourceRule`,
   retain the two superseded fields as rejections per decision 10, rename
   `allowWatchRuleSourceNamespaceOverride` to `allowSourceNamespaceOverride`, keep
   `allowedSourceNamespaces`, then regenerate the CRDs.
   Rewrite the retained fields' Go/CRD documentation so it no longer says the policy bounds every rule
   kind or that it narrows ClusterWatchRule.
2. **Authorization.** Generalize the existing `internal/authz` one-namespace decision into a per-item
   candidate check plus a wildcard enumeration. Keep its exact-name fast path, selector result states,
   reasons, and provider-delegation check; aggregate per [§5](#5-status-contract-for-per-item-scopes).
3. **Compilation and state.** Refactor `CompileWatchRule`, `CompiledRule`, the RuleStore, the
   source-scope service, and the WatchRule fingerprint/selection collection to operate on the whole
   resolved scope atomically. Keep the current bootstrap path and dependency mappers wired through that
   compiler.
4. **ClusterWatchRule.** Narrow the public scope selector, select only discovery-confirmed
   cluster-scoped types, and add the stored-object refusal to that kind's shared bootstrap/reconcile
   compile path.
5. **Status and roll-up.** Implement the aggregation order and move `StreamSummaryForWatchRule` onto the
   compiled rule.
6. **Documentation and tests.** Replace the top-level-field material listed in
   [§10](#10-docs-that-become-false), and carry the existing authorization cases forward before adding
   rule-item, wildcard, and migration cases.

## 10. Docs that become false

Must change in the same PR:

- [configuration.md](../../configuration.md) line 12 — "`ClusterWatchRule` does the same for
  cluster-scoped or cross-namespace watching";
- [configuration.md](../../configuration.md) lines 784 and 821-835 and 1002 — the `ClusterWatchRule`
  section, its `scope` example, and the multi-namespace use case;
- the `ClusterWatchRule` godoc
  ([clusterwatchrule_types.go:150-163](../../../api/v1alpha3/clusterwatchrule_types.go#L150-L163)) and
  the `WatchRule` godoc
  ([watchrule_types.go:202-215](../../../api/v1alpha3/watchrule_types.go#L202-L215)), both of which
  describe the superseded model, plus the generated CRDs;
- the WatchRule section of [status-conditions-guide.md](../../spec/status-conditions-guide.md) — the new
  reason `NoAdmittedSourceNamespaces` and the aggregation order;
- [architecture.md](../../architecture.md), [security-model.md](../../security-model.md), and
  [rbac.md](../../rbac.md) where they describe cross-namespace ClusterWatchRule watching;
- [UPGRADING.md](../../UPGRADING.md) per [§8](#8-migration), and the
  [INDEX.md](../../INDEX.md) entry for this folder, which still describes the abandoned ClusterWatchRule
  ceiling.

Fixture conversions worth calling out, because they change what the test proves:

- [test/e2e/templates/restart/watchrule-wildcard.tmpl](../../../test/e2e/templates/restart/watchrule-wildcard.tmpl)
  exists to prove the **startup snapshot** honours `apiVersions: ["*"]`, "otherwise a controller restart
  snapshots an empty cluster and deletes the whole tracked tree". Its replacement must still cover that
  regression, not merely compile.
- [unsupported_folder_e2e_test.go:177](../../../test/e2e/unsupported_folder_e2e_test.go#L177), plus the
  ~11 unit-test files referencing the scope enum.

## Tests

**Refusal and admission**

- `TestBootstrap_PreExistingNamespacedClusterRuleIsRefused` — a stored ClusterWatchRule with
  `scope: Namespaced` compiles no stream before the first reconcile can publish status.
- A ClusterWatchRule with `resources: ["*"]` still resolves its cluster-scoped types: the refusal keys
  on the stored scope, not on what the selector happens to resolve.
- Applying `scope: Namespaced` is rejected by the API server (envtest against the generated CRDs) — the
  guard that the field was narrowed rather than deleted.

**Per-item resolution**

- Omitted, explicit, and wildcard items each generate the expected selections; a mixed rule resolves each
  item independently.
- A denied explicit item stops the entire WatchRule and the message names it; an empty wildcard set does
  not stall the rule and reports `NoAdmittedSourceNamespaces`.
- `"*"` with no declared policy is denied, not "all namespaces"; against `{}` it admits nothing; against
  `names` it expands exactly; against `selector: {}` it admits every namespace in the snapshot.
- With source Namespace `list` forbidden, a `"*"` item against a **names-only** policy still resolves,
  while a selector item follows the establishing/maintaining table. Both halves need a case.
- The retained gate suite still proves provider-delegation denial and revocation, target-policy denial
  and revocation, selector uncertainty, and bootstrap refusal before any stream starts.

**Silent-failure guards**

- `TestWatchRuleFingerprint_ChangesWithResolvedSourceScope` — two byte-identical WatchRules whose
  GitTargets declare different policies fingerprint differently, and tightening a policy changes the
  fingerprint of an untouched rule object.
- `TestWatchedTypeTable_RebuildsWhenOnlyThePolicyChanged` — the invalidation twin one level up: the
  resident table re-projects, not merely the reconcile re-runs.
- `TestResolvedScope_UnknownRetainsPreviousAndDoesNotSweep` — assert the **absence of the sweep**, not
  only the condition.
- The stream roll-up reports ready for a wildcard rule whose streams are running — the
  [§5](#5-status-contract-for-per-item-scopes) `StreamSummaryForWatchRule` hazard.

**Reactivity (envtest)**

- Adding the matching label to a source-cluster Namespace grants it to a `"*"` item within a bounded
  time; removing it revokes that namespace's stream rather than only re-rendering status.
- Editing `allowedSourceNamespaces` and flipping the delegation flag each re-reconcile the affected
  WatchRules.

**End-to-end**

- A WatchRule in `tenant-acme` with `sourceNamespace: repo-config` writes under `repo-config/…`, not
  `tenant-acme/…` — the
  [Appendix A](historical-top-level-source-namespace-baseline.md#appendix-a-the-source-objects-namespace-already-names-the-git-folder)
  proof, kept.
- A target whose policy admits `repo-config` never receives an object from `tenant-zen`, asserted
  against a real commit.
- Narrowing a policy under PR 5's `OnEvent` default leaves the prior documents in Git rather than
  sweeping them.

## Done when

- The public `scope` choice is unreachable: rejected at admission, refused at compile, ignored at
  resolution. The top-level `sourceNamespace` is gone from the API entirely.
- `rules[].sourceNamespace` resolves omitted, explicit, and wildcard items, with the wildcard backed by
  both halves of `allowedSourceNamespaces`.
- A policy or source-Namespace-label change re-projects the watched-type table with the rule object
  untouched.
- The release notes and [UPGRADING.md](../../UPGRADING.md) carry the breaking change per
  [§8](#8-migration).
- `task lint`, `task test`, `task test-e2e` pass — and no release is cut until
  [PR 5](pr5-gittarget-deletion-safety.md) has merged.
