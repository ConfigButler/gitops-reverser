# API catalog and watched type architecture

> Status: architecture proposal, captured 2026-06-08
>
> Question: how should `APIResourceCatalog` and `WatchedTypeTable` relate when
> both currently carry Kubernetes type identity and policy facts?
>
> Short answer: keep `APIResourceCatalog` as the raw discovery cache, introduce a
> resolved type surface as the policy boundary, and make `WatchedTypeTable` a
> GitTarget-specific projection of already-resolved types.

## Problem

The current implementation has useful pieces, but the boundaries are blurry.

`APIResourceCatalog` owns discovery refresh, group/version trust state, indexes,
and raw type facts. `WatchedTypeTable` owns GitTarget selection, namespace
operation filters, conflict visibility, pending removals, and snapshot/informer
safety.

The overlap is that both objects now handle type identity:

```text
APIResourceEntry
  GVK, GVR, namespaced, verbs, preferred, subresource, allowed, policy reason

WatchedType
  GVK, GVR, namespaced, served version, preferred, scope, namespace operations
```

That overlap forces `WatchedTypeTable` to look back into the raw catalog with
`LookupGVR`, then repeat part of the GVK/GVR validity policy locally. It also
means product policy is spread across `APIResourceCatalog`, `CatalogMapper`,
`RuleGVRResolver`, `WatchedTypeTable`, and the git writer's sensitive-resource
policy.

The neat model is to make raw discovery, type policy, and GitTarget selection
three different layers.

## Current Comparison

| Concern | `APIResourceCatalog` today | `WatchedTypeTable` today | Desired owner |
| --- | --- | --- | --- |
| Discovery refresh | Calls Kubernetes discovery and handles partial failure | Does not refresh discovery | `APIResourceCatalog` |
| Raw indexes | Indexes by GVK, GVR, resource, group/resource, group/version | Calls `LookupGVR` while building a target table | `APIResourceCatalog` |
| Readiness and degraded discovery | Tracks ready, generation, degraded group/versions | Stores `ResolvedAt`; blocks gather on blocking misses and pending removals | Catalog for source state; table for resolved snapshot state |
| Type facts | Stores GVK, GVR, namespaced, verbs, preferred, subresource | Repeats GVK, GVR, namespaced, preferred, served version | Resolved type surface should expose accepted type facts |
| Allowed/disallowed policy | Computes `Allowed` and `PolicyReason` during catalog entry creation | Receives misses and avoids unplanned types indirectly | Resolved type surface/type policy |
| GVK ambiguity | Catalog can return multiple entries for a GVK | Detects conflicts only among selected GVRs | Resolved type surface |
| GVR validation | `LookupGVR` returns one raw catalog entry | Treats the returned GVR as enough to build a watched type | Resolved type surface |
| WatchRule expansion | Raw resource/group indexes support wildcard and omitted-group lookups | Consumes resolved selections after expansion | Watch rule resolver, backed by type surface |
| GitTarget scope | None | Owns namespace ops, cluster-wide selection, target identity | `WatchedTypeTable` |
| Pending removals | None | Owns grace period and sweep safety | `WatchedTypeTable` |
| Sensitive resource handling | None | None | Type policy/type surface, consumed by git writer |

The important asymmetry: `APIResourceCatalog` is cluster-global; `WatchedTypeTable`
is GitTarget-specific. Any shared type decision between them belongs in a layer
between them, not inside either one.

## Target Layers

```text
APIResourceCatalog
  raw Kubernetes discovery cache
  indexes, generation, readiness, degraded group/versions

TypePolicy
  GitOps Reverser policy over resource types
  allowed/disallowed, sensitive/encrypted, optional policy reasons

ResolvedTypeSurface
  stable interface over catalog + policy
  exact GVK lookup, exact GVR lookup, candidate listing, one refusal vocabulary

ResourceMapper
  narrow adapter for manifest analysis
  GVK -> GVR only

RuleGVRResolver
  WatchRule selector semantics
  wildcards, omitted groups, preferred version, scope, list/watch support

WatchedTypeTable
  GitTarget-selected operational view
  namespace operations, conflicts/misses for visibility, pending removals
```

## APIResourceCatalog Responsibility

`APIResourceCatalog` should answer: what did Kubernetes discovery currently say,
and how trustworthy is that observation?

It should own:

- discovery refresh;
- preserving previous clean group/versions when discovery is partially degraded;
- generation increments;
- raw indexes for efficient lookups;
- cloning and sorting raw entries;
- catalog metrics facts.

It should not own:

- GitTarget rule semantics;
- snapshot, informer, or sweep behavior;
- sensitive-resource write behavior;
- final "is this type usable by GitOps Reverser?" decisions.

The catalog may still store raw decorations such as preferred version and
subresource shape because those come from discovery or are direct projections of
discovery names. But a caller should not treat a raw catalog entry as a permission
to watch or write that type.

## Resolved Type Surface Responsibility

The missing abstraction is the policy boundary between raw discovery and consumers.

Sketch:

```go
type TypeFact struct {
    GVK schema.GroupVersionKind
    GVR schema.GroupVersionResource

    Namespaced bool
    Verbs []string
    Preferred bool
    Subresource bool

    Allowed bool
    Sensitive bool
    PolicyReason string
}

type TypeResult struct {
    Fact TypeFact
    Status mapping.Status
    Reason string
    Generation uint64
}

type TypeSurface interface {
    Ready() mapping.MapperReadiness
    Generation() uint64

    ForGVK(ctx context.Context, gvk schema.GroupVersionKind) (TypeResult, error)
    ForGVR(ctx context.Context, gvr schema.GroupVersionResource) (TypeResult, error)
}
```

`ForGVK` and `ForGVR` must share the same product policy:

- no trusted data means `CatalogUnavailable`;
- degraded lookup scope means `DiscoveryDegraded`;
- served but excluded means `Disallowed`;
- subresource-only matches are refused;
- one GVK served by multiple GVRs is `Ambiguous`;
- exact GVR lookup must validate the entry's GVK against the full GVK lookup.

That last point closes the current gap: `LookupGVR` is single-valued because GVR is
unique, but that does not prove the type is safe. If the GVR's GVK is globally
ambiguous, selecting one GVR should still be refused.

## WatchedTypeTable Responsibility

`WatchedTypeTable` should answer: for this GitTarget, which already-accepted
types are operationally active, under which namespaces and operations?

It should own:

- GitTarget destination identity;
- selected namespace and operation filters;
- cluster-wide versus named namespace stream shape;
- resident table generation;
- pending removals and removal grace;
- blocking snapshot safety;
- conflict and miss visibility for target planning.

It should not own:

- raw GVR to GVK lookup;
- global GVK ambiguity policy;
- allowed/disallowed policy;
- sensitive-resource classification;
- Kubernetes discovery freshness.

In the target shape, `buildWatchedTypeTable` receives resolved facts, not raw GVRs
that it must validate against the catalog:

```go
type resolvedSelection struct {
    fact TypeFact
    namespace string
    ops []configv1alpha1.OperationType
}
```

Then the table builder only folds selections by accepted type identity and merges
namespace operation sets. If ambiguity or disallowed state exists, it arrives as a
typed miss from the resolver instead of being rediscovered inside the table.

## Sensitive Resources

The startup flag for additional sensitive resources is currently consumed by the
git writer. That works for encryption, but it keeps an important type policy
outside the type system.

Prefer `Sensitive bool` on `TypeFact`, backed by the existing
`types.SensitiveResourcePolicy`.

This should mean:

- core `v1/secrets` are always sensitive;
- startup additional sensitive resources are classified at type-policy time;
- writer code can still receive the policy directly during migration;
- eventually the selected type fact can carry sensitivity into write planning.

The field should be named `Sensitive`, not `Secret`, because configured resources
may be Secret-shaped CRDs rather than Kubernetes `Secret` objects.

## RuleGVRResolver Role

`RuleGVRResolver` should keep WatchRule-specific syntax:

- omitted apiGroups;
- wildcard groups/resources/versions;
- preferred-version selection when versions are omitted;
- namespaced versus cluster scope;
- list/watch capability checks;
- operator-facing miss detail.

But it should not use raw catalog entries as final answers. It can use catalog or
surface candidate listing for expansion, then validate concrete candidates through
the type surface.

Practical target:

```text
WatchRule selector
  -> candidate raw resources for expansion
  -> TypeSurface.ForGVR for each concrete GVR
  -> resolvedSelection or ResolveMiss
  -> WatchedTypeTable fold
```

`ResolveMiss` can remain a watch-domain type, but it should optionally carry the
core mapping status so UI/status/logging does not need to parse reason strings.

## Refactor Path

1. Add a `TypeFact`/`TypeResult` shape in the mapping layer or a small new core
   package that does not import `internal/watch`.
2. Add exact GVR resolution beside exact GVK resolution and make it validate
   global GVK uniqueness.
3. Move allowed/disallowed and sensitive classification behind a small type-policy
   object used by the live surface and static tests.
4. Keep `CatalogMapper` as a thin `ResourceMapper` adapter over the surface.
5. Change `RuleGVRResolver` so concrete GVRs are accepted only through the surface.
6. Change `WatchedTypeTable` so it folds resolved `TypeFact` selections instead of
   calling `catalog.LookupGVR`.
7. Remove local conflict detection from the table once ambiguity is guaranteed to
   arrive as a typed planning miss.
8. Add regression coverage for the key bug shape: one GVK served by two GVRs, only
   one selected by a GitTarget, still refused.

## Decision

Do not merge `APIResourceCatalog` and `WatchedTypeTable`.

They have different lifetimes and scopes:

- `APIResourceCatalog` is cluster-global and discovery-shaped.
- `WatchedTypeTable` is GitTarget-local and operation-shaped.

The overlap should be removed by inserting a resolved type surface between them.
That surface becomes the single place where GitOps Reverser turns raw discovery
into accepted or refused type facts.

## References

- [catalog-mapper-vs-watched-type-table.md](catalog-mapper-vs-watched-type-table.md)
- [subresource-scope-reduction.md](subresource-scope-reduction.md)
- [gvk-gvr-mapping-layer.md](../gvk-gvr-mapping-layer.md)
- [`internal/watch/api_resource_catalog.go`](../../../../internal/watch/api_resource_catalog.go)
- [`internal/watch/watched_type_table.go`](../../../../internal/watch/watched_type_table.go)
- [`internal/watch/rule_gvr_resolver.go`](../../../../internal/watch/rule_gvr_resolver.go)
- [`internal/watch/catalog_mapper.go`](../../../../internal/watch/catalog_mapper.go)
- [`internal/types/sensitive_resource.go`](../../../../internal/types/sensitive_resource.go)
