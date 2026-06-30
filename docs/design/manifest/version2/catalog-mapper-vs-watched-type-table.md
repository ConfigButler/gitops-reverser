# Raw catalog, resolved type surface, GitTarget selection

> Status: design investigation, captured 2026-06-05
>
> Question: should `internal/watch/watched_type_table.go` become the single
> GVK/GVR abstraction, including for CLI/offline tooling, or should
> `internal/watch/catalog_mapper.go` stay separate?
>
> Short answer: neither object is the core abstraction. The core abstraction is a
> resolved type surface behind an interface. `CatalogMapper` and
> `WatchedTypeTable` should both consume that surface.

## Verdict

The right model is layered:

```text
APIResourceCatalog
  raw discovery observations, indexes, freshness, degradation

Resolved type surface
  core interface over trusted type facts:
  exact GVK lookup, exact GVR lookup, one refusal vocabulary

ResourceMapper
  narrow GVK->GVR adapter for manifest analysis, CLI, tests

WatchedTypeTable
  per-GitTarget selected subset plus watch lifecycle
```

This keeps the nasty cluster details at the edge. Discovery wobble, missing
catalog data, disallowed resources, and ambiguous type relationships are
classified before lower-level code acts on them. The lower a caller gets, the more
it should be able to trust what it receives.

So the direction should be:

1. Keep programming to interfaces.
2. Keep `internal/watch` out of CLI and manifest analysis.
3. Keep `WatchedTypeTable` as a GitTarget-specific operational table.
4. Keep `CatalogMapper` as a narrow adapter/interface.
5. Extract the shared type surface into core mapping code.
6. Treat ambiguous GVK->GVR as a hard, observable refusal.

## 1. What is shared

Both the mapper and the watched table are built from the same type facts:

```text
GVK
GVR
namespaced
verbs
preferred
subresource
allowed
```

That shape already exists as `mapping.Entry` in
[`internal/mapping/mapper.go`](../../../../internal/mapping/mapper.go).

The shared abstraction is not "a table" and not "a mapper." It is a resolved view
of API resource discovery:

```text
Given a GVK, either return the one allowed served GVR or a refusal.
Given a GVR, either return the one allowed served type fact or a refusal.
```

Both entry points must apply the same product policy. In particular, exact GVR
lookup must not become a bypass around ambiguous GVK detection.

## 2. What is not shared

`WatchedTypeTable` is not just a generic set of types today. It also carries
GitTarget behavior:

- selected rules and resource scope;
- namespace operation filters;
- pending removals and removal grace;
- sweep safety;
- informer/snapshot/per-type reconcile lifecycle;
- GitTarget destination identity.

Those concerns belong in `internal/watch`. They should not be imported by CLI,
offline analysis, or lower-level manifest code.

The reusable part is the type fact and the resolution contract underneath the
table.

## 3. The interface boundary

The important move is to program against a small core interface rather than against
the live watch implementation.

Sketch:

```go
type TypeSurface interface {
    Ready() MapperReadiness
    Generation() uint64

    ForGVK(ctx context.Context, gvk schema.GroupVersionKind) (Result, error)
    ForGVR(ctx context.Context, gvr schema.GroupVersionResource) (Result, error)
}
```

The concrete implementations can vary:

- live catalog in the controller;
- kubeconfig-backed discovery for CLI;
- static snapshot for tests/offline review;
- structure-only implementation that declines mapping.

Callers should not care which implementation they received. That is the point of
the abstraction.

`ResourceMapper` can remain as the narrower interface for callers that only need
manifest GVK->GVR resolution:

```go
type ResourceMapper interface {
    GVRForGVK(ctx context.Context, gvk schema.GroupVersionKind) (Result, error)
}
```

That narrow adapter is still useful. It keeps manifest analysis from knowing about
watch rules, informers, or discovery internals.

## 4. Ambiguity policy

Product policy:

> A cluster that serves one GVK through multiple GVRs is misconfigured for GitOps
> Reverser. We do not pick a winner. We do not serve that type.

The code must still detect and report this condition, but it should not model it
as a valid operating mode.

This matters for exact GVR lookup.

It is true that `APIResourceCatalog` has a single entry for an exact GVR. But that
does not prove the type is acceptable. If that GVR's GVK is globally ambiguous, the
selected GVR must still be refused.

Correct `ForGVR` behavior:

```text
1. Look up the exact GVR.
2. If missing, return unavailable/degraded/unserved as appropriate.
3. Read the entry's GVK.
4. Resolve that GVK against the full catalog.
5. Accept only if the GVK resolves to exactly this GVR.
6. Otherwise return the same refusal status the GVK path would return.
```

This prevents a GitTarget rule from accidentally selecting one side of an
ambiguous cluster shape and making it look safe.

Current caveat: `WatchedTypeTable` detects conflicts among selected GVRs. That is
narrower than the product policy above. If only one ambiguous GVR is selected, the
table may not currently observe the global ambiguity. A shared type surface should
close that gap.

## 5. Status vocabulary

There should be one core vocabulary for catalog/type lookup outcomes:

```text
Resolved
Unserved
Ambiguous
Disallowed
Subresource
CatalogUnavailable
DiscoveryDegraded
StructureOnly
```

That vocabulary belongs with the resolved type surface.

But watch-rule planning has extra domain concepts that should not be forced into
the core lookup vocabulary:

- wildcard expansion;
- omitted apiGroups matching multiple groups;
- version preference;
- scope filtering;
- list/watch support;
- operation-specific planning messages.

So do not blindly collapse `ResolveMissReason` into `mapping.Status`.

Better:

```text
mapping.Status
  core type lookup outcome

watch.ResolveMiss
  watch-rule planning miss, optionally carrying a mapping.Status plus
  rule-specific detail
```

That keeps lower-level mapping clean while allowing higher-level watch code to
explain rule failures precisely.

## 6. Layer responsibilities

### APIResourceCatalog

Owns raw discovery facts:

- indexes by GVK and GVR;
- catalog readiness;
- degraded group/version state;
- generation.

It can be messy because Kubernetes discovery is messy.

### Resolved type surface

Owns product policy over those facts:

- exact GVK resolution;
- exact GVR validation;
- allowed/disallowed filtering;
- subresource refusal;
- ambiguity refusal;
- trusted absence versus degraded absence.

It returns normalized results. Consumers should not need to interpret catalog
internals.

### ResourceMapper

Owns the manifest-facing interface:

- manifest GVK -> served GVR;
- no dependency on `internal/watch`;
- usable by CLI, tests, controller, and structure-only analysis.

It can be an adapter over the resolved type surface.

### RuleGVRResolver

Owns WatchRule selector expansion:

- resource and group wildcards;
- explicit or preferred versions;
- namespaced versus cluster scope;
- list/watch capability;
- rule-specific miss details.

It should call the resolved type surface when it needs to validate concrete type
facts, but it should keep rule semantics in `internal/watch`.

### WatchedTypeTable

Owns the GitTarget-selected operational view:

- watched types;
- namespace operation filters;
- conflicts and misses for visibility;
- pending removals;
- sweep and informer safety.

It should be built from trusted type-surface results, not by reinterpreting raw
catalog entries.

## 7. What this rejects

Do not move `WatchedTypeTable` into CLI. It is too operational and too coupled to
GitTarget lifecycle.

Do not delete `CatalogMapper`. The interface boundary is valuable, even if the
implementation becomes a thin adapter.

Do not add reverse mapping to delete planning. Delete planning should rely on the
GitTarget folder inventory. If the inventory does not know the resource identity,
we should not invent it at delete time.

Do not let exact GVR lookup bypass global GVK ambiguity.

Do not make lower layers parse string reasons from higher layers. Use typed
statuses and typed misses.

## 8. Recommended refactor path

1. Add a core exact-GVR lookup/validation path beside `ResolveGVK`.
2. Ensure exact GVR validation checks global GVK uniqueness.
3. Keep `ResourceMapper` as the GVK-only interface for manifest analysis.
4. Have the live catalog mapper implement the core surface internally.
5. Have `buildWatchedTypeTable` consume validated surface results instead of raw
   `LookupGVR(...).Entries[0]`.
6. Let `RuleGVRResolver` keep rule-specific misses, but attach or translate from
   core `mapping.Status` where appropriate.
7. Add tests for the important policy case: one GVK served by two GVRs, only one
   selected by the GitTarget, still refused.

## 9. Design principle

The lower layers should be boring.

At the top, the system deals with Kubernetes discovery churn, partial catalogs,
ambiguous clusters, rule syntax, and operator-facing diagnostics.

At the bottom, the writer, analyzer, sweep, and per-type reconcile should operate
on already-classified facts:

```text
this type is resolved and trusted
this type is refused and observable
this surface is degraded, so destructive work must stop
```

That is the reason to push the shared abstraction into core and program toward
interfaces. It simplifies the lower layers without pretending the cluster is
always clean.

## References

- [gvk-gvr-mapping-layer.md](../../../finished/gvk-gvr-mapping-layer.md)
- [`internal/mapping/mapper.go`](../../../../internal/mapping/mapper.go)
- [`internal/watch/catalog_mapper.go`](../../../../internal/watch/catalog_mapper.go)
- [`internal/watch/watched_type_table.go`](../../../../internal/watch/watched_type_table.go)
- [`internal/watch/rule_gvr_resolver.go`](../../../../internal/watch/rule_gvr_resolver.go)
- [`internal/watch/api_resource_catalog.go`](../../../../internal/watch/api_resource_catalog.go)
