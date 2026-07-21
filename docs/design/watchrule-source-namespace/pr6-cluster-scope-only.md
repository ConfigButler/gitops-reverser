# PR 6 — scope by kind: cluster-only ClusterWatchRule and per-item WatchRule namespaces

> Phase 6 of [source-namespace addressing](README.md). **Breaking change**, accepted for the
> preliminary v1alpha3 API. Depends on [PR 1](pr1-namespace-scoped-resync.md),
> [PR 2](pr2-stream-scope-collapse.md), [PR 3](pr3-clusterwatchrule-target-admission.md), and the
> released [PR 5 deletion-safety rollback floor](pr5-gittarget-deletion-safety.md).
>
> This PR supersedes the unshipped top-level `sourceNamespace` interface described by
> [PR 4](pr4-source-namespace-field.md). It reuses that PR's authorization, status, bootstrap-gate,
> and source-scope work, but does not release its API shape.

## End state

Scope is carried by the rule kind:

- **WatchRule** selects namespaced source resources. Each `ResourceRule` has an optional
  `namespace` field.
- **ClusterWatchRule** selects cluster-scoped source resources. It has no user-facing `scope` field.

~~~yaml
kind: WatchRule
metadata: { name: repo-config, namespace: tenant-acme }
spec:
  targetRef: { name: acme }
  rules:
    - resources: [configmaps]             # own namespace
    - resources: [secrets]
      namespace: repo-config               # one admitted source namespace
    - resources: [deployments]
      namespace: "*"                       # every namespace the target admits
---
kind: ClusterWatchRule
metadata: { name: acme-crds }
spec:
  targetRef: { name: acme, namespace: tenant-acme }
  rules:
    - resources: [customresourcedefinitions]
      apiGroups: [apiextensions.k8s.io]
~~~

The resulting audit rule is simple: a namespace allow-list applies to WatchRules; a
ClusterWatchRule is intentionally cluster-global and is bounded by its source credential's RBAC.

## 1. ClusterWatchRule becomes cluster-only

Remove `ResourceScope` and `ClusterResourceRule.scope` from the API. Discovery continues to know a
type's internal scope; the resolver always selects cluster-scoped records for ClusterWatchRule. Its
selections continue to use namespace `""`, now only for genuinely cluster-scoped streams.

Removing the schema field is insufficient. Existing objects may be read after a schema change without
the old field available to controller code. In the single compile path shared by bootstrap and
reconcile, refuse any ClusterWatchRule selector that resolves to a namespaced type. The terminal
condition must say:

> ClusterWatchRule is cluster-scoped only; watch namespaced resources with a WatchRule and
> `rules[].namespace`.

The critical test is `TestBootstrap_PreExistingNamespacedClusterRuleIsRefused`: it must compile no
stream before the first reconciliation can publish status.

This removes the abandoned runtime-ceiling design entirely: no ClusterWatchRule source-namespace
expansion, resolved-scope fingerprint, source-policy mapper, or selector-outage retention path is
implemented for that kind.

## 2. WatchRule namespace moves onto ResourceRule

Delete `WatchRule.spec.sourceNamespace`. Add `ResourceRule.namespace` with structural validation for
either `"*"` or a Kubernetes Namespace name (maximum 63 characters and DNS-label syntax).

| `rules[].namespace` | Meaning |
|---|---|
| omitted | WatchRule's own namespace; legacy behavior |
| explicit name | One source namespace, through PR 4's three-part gate |
| `"*"` | All source namespaces that `GitTarget.allowedSourceNamespaces` admits |

Any denied explicit name denies the whole WatchRule. The object reports
`SourceNamespaceAuthorized=False`, `Stalled=True`, and starts no streams; it must name the failing
rule item in its message. A wildcard narrows to the admitted set, which may be empty without being a
terminal refusal.

The first release supports wildcard expansion against a **names-only** allow-list. An explicit
namespace may still use the selector behavior implemented by PR 4, but a wildcard with a selector
policy is rejected as unsupported until its dynamic invalidation and retaining contract receives a
dedicated follow-up. This keeps the initial wildcard set spec-derived and avoids shipping source
Namespace fan-out as a side effect of the API migration.

## 3. Compile and invalidation model

PR 4's current `CompiledRule.SourceNamespace string` and retained-grant map are single-namespace
representations. They cannot be reused verbatim. Replace them with an atomically replaced resolved
scope for the whole WatchRule, containing each item and its concrete namespace set. Because rule
items have no stable API identity, key it to the complete current rule spec rather than an index that
could survive a reorder incorrectly.

`collectWatchRuleSelections` expands every matched resource record by that item's resolved namespace
set. The WatchRule fingerprint contains each item namespace and, for `"*"`, the resolved set. An
explicit-name edit or a target policy edit must rebuild the table; a stale selected namespace is a
silent incorrect watch.

## 4. Deliberate capability removal

ClusterWatchRule's cross-namespace target reference previously let a platform team author a
namespaced mirror without an object in the tenant namespace. This model removes that placement
capability. A platform administrator may still manage WatchRule manifests, but must place them in the
tenant namespace. If that is unacceptable for a deployment, stop this PR or design a separate
namespaced-target WatchRule with its own authorization review.

## 5. Migration and rollout

This is a cross-kind migration; a conversion webhook cannot turn a ClusterWatchRule into a WatchRule.
Provide a dry-run migration command or documented preflight that, for each namespaced
ClusterWatchRule:

1. identifies the target and selected resource rules;
2. requires an explicit compatible `allowedSourceNamespaces` policy; and
3. produces a namespaced WatchRule manifest only when the target policy preserves the intended scope.

It must refuse a target without a policy. Legacy `scope: Namespaced` meant all source namespaces;
`namespace: "*"` with no policy admits none, so silently generating the replacement would narrow
production data.

Release order:

1. PR 5 is already released and every controller runs it.
2. Upgrade to this controller and wait until no PR-5 controller remains active before applying
   migrated WatchRules or removing old ClusterWatchRules.
3. Run the preflight, add/verify policies, then apply the generated WatchRules and remove old rules.
4. Roll back only to PR 5 while migrated manifests exist. It understands `prune.mode: onEvent`, so a
   scope collapse leaves stale Git rather than sweeping it.

## Tests

- Existing namespaced ClusterWatchRules are refused at bootstrap and reconcile; cluster-scoped rules
  remain unaffected.
- ClusterWatchRule selection and fingerprint are cluster-scope-only.
- Omitted, explicit, and wildcard rule-item namespaces each generate the expected selections.
- A denied explicit item stops the entire WatchRule; an empty wildcard set does not stall it.
- A wildcard with no policy or a selector policy fails loudly; names-only policy expansion is exact.
- The resolved WatchRule scope changes its fingerprint and reprojects the table after every relevant
  rule or policy edit.
- An end-to-end migration verifies that a target cannot receive objects from a namespace outside its
  declared policy and that an empty policy change does not delete prior documents under PR 5's
  `onEvent` mode.

## Done when

- The public `scope` enum and the top-level `sourceNamespace` field are gone from generated CRDs.
- Pre-existing namespaced ClusterWatchRules fail closed at bootstrap.
- The current PR-4 gate is reworked for rule-item scopes rather than shipped alongside it.
- The migration preflight and full validation suite pass.
