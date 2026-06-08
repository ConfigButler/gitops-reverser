# Type followability naming proposal

> Status: proposal
>
> Companion to [type-followability.md](type-followability.md) and
> [type-followability-implementation.md](type-followability-implementation.md).
> Discovery boundary follow-up:
> [discovery-catalog-typeset-boundary.md](discovery-catalog-typeset-boundary.md).

## Problem

The implementation has mostly collapsed the old split between API discovery,
mapping, rule resolution, and watched-type tables into one shared type decision
surface. The current names still carry some of the old shape:

- `typeset` sounds like a set, but the package now owns records, evaluation,
  live-set retention, lookup, and snapshot construction helpers.
- `WatchedTypeTable` sounds like a resolved lookup table, but it is now a
  per-GitTarget projection of globally followable records through WatchRules.
- `funnel.go` describes the order of checks, not the thing the file owns.
- `model.go` is generic; it hides the fact that the file defines the public
  vocabulary and record shape.

The names should make the three different scopes obvious:

1. **All known cluster types**: every type the registry can explain.
2. **Followable/live cluster types**: the globally safe/actionable subset.
3. **Per-GitTarget watched type set**: the subset selected by that target's
   WatchRules and ClusterWatchRules, including namespace and operation scope.

## Recommended vocabulary

Use `followable`, `live`, and `target` instead of `valid`.

`valid` is not precise enough. A type can be a valid Kubernetes resource while
GitOps Reverser refuses it because policy denies it, identity is ambiguous, or
the required verbs are absent. `followable` names the product decision; `live`
names the operational set that remains actionable during the retention grace.

| Concept | Recommended name | Why |
| --- | --- | --- |
| Every known/explainable type | `allTypeRecords` / `TypeRecords()` | Includes refused and unknown records, so `all` is honest. |
| Globally actionable type set | `followableTypeRecords` / `FollowableTypeRecords()` | Matches the existing `Followability` decision vocabulary. |
| Operational live set | `liveTypeRecords` / `LiveTypeRecords()` | Good if we want `retained` to feel first-class: followable + retained. |
| Per-GitTarget selected set | `TargetTypeSet` | It is really a set per target, limited by watch rules. |
| One member of that set | `TargetType` or `WatchedType` | Keep `WatchedType` if we want continuity with current call sites. |
| Rule match before folding | `typeSelection` or `watchSelection` | A temporary selected record plus namespace/ops. |

Preferred public shape:

```text
TypeRegistry
  AllRecords()        -> []TypeRecord
  FollowableRecords() -> []TypeRecord
  LiveRecords()       -> []TypeRecord (optional alias if retained should be explicit)

TargetTypeSetStore
  TargetTypeSet(gitDest) -> TargetTypeSet
  AllTargetTypeSets()    -> []TargetTypeSet

TargetTypeSet
  GitTarget
  Destination
  Types []WatchedType
  ResolvedAt
```

If we keep only one accessor for the actionable global subset, prefer
`FollowableRecords()`. If the retained state starts appearing in status/UI, add
`LiveRecords()` as a clearer alias and document it as "followable or retained".

## Package name options

### Recommended: `internal/typeinventory`

`typeinventory` says the package owns a durable inventory of Kubernetes resource
types, not merely a set. It fits all current responsibilities: records, decisions,
lookup, live-set retention, and snapshot-backed registries.

Suggested package-level names:

- `typeinventory.Record` instead of `typeset.TypeRecord`
- `typeinventory.Registry`
- `typeinventory.Observation`
- `typeinventory.SnapshotRegistry`
- `typeinventory.Lookup`

Pros:

- Clearer than `typeset` for non-set responsibilities.
- Avoids collision with `APIResourceCatalog`; this is not raw discovery.
- Works for both live cluster and no-cluster analyzer paths.

Cons:

- Longer import name.
- "Inventory" sounds descriptive rather than decisional unless paired with
  `Followability`.

### Alternative: `internal/typecatalog`

Good if we want the package to read as the canonical type catalog, with decisions
included.

Pros:

- Very easy to understand.
- Pairs naturally with `TypeCatalog`, `TypeRecord`, `TypeLookup`.

Cons:

- We already have `APIResourceCatalog` for raw discovery. Two catalogs can blur the
  boundary unless names become `DiscoveryCatalog` and `TypeCatalog`.

### Alternative: `internal/typesurface`

Good if we want to emphasize "the cluster API surface as GitOps Reverser sees it".

Pros:

- Captures observed, degraded, and retained API surface well.
- Less generic than `typeset`.

Cons:

- Slightly abstract.
- "Surface" does not immediately say it contains a registry and decisions.

### Alternative: keep `internal/typeset`

This is acceptable only if we rename the per-GitTarget object away from
`WatchedTypeTable`. Then `typeset` can mean "the global set of type records".

Pros:

- Smallest code churn.
- The user's intuition is right: there is a real set here.

Cons:

- The package has more than set semantics now.
- It remains easy to confuse the global type set with the per-GitTarget selected
  set.

## File name options inside the package

Recommended file layout if the package becomes `typeinventory`:

| Current file | Recommended file | Why |
| --- | --- | --- |
| `model.go` | `record.go` | Defines `TypeRecord`, identity, origin, scope, and subresource facts. |
| `funnel.go` | `followability.go` | Defines `Observation`, checks, verdict derivation, and summaries. |
| `registry.go` | `registry.go` | Already exact. |
| `observe.go` | `observations.go` | Plural because it builds observations from entries. |
| `lookup.go` | `lookup.go` | Already exact. |
| `scale.go` | `scale_binding.go` | Names the actual domain object, not just the feature. |

If `followability.go` becomes too broad, split it:

- `observation.go`: `Observation` and raw facts.
- `checks.go`: requirement check functions.
- `verdict.go`: verdict derivation and summary rendering.

That split is only worth it if the file grows; today one `followability.go` is
probably clearer.

## Watch-layer rename proposal

The watch package should stop saying "table" for the per-target projection.

| Current name | Proposed name | Notes |
| --- | --- | --- |
| `WatchedTypeTable` | `TargetTypeSet` | The user's suggested "typeset, one per GitTarget" is exactly this object. |
| `watchedTypeStore` | `targetTypeSetStore` | Stores the published per-target projections. |
| `refreshWatchedTypeTables` | `refreshTargetTypeSets` | Reprojects registry records through rules. |
| `resolveWatchedTypeTables` | `buildTargetTypeSets` | It no longer resolves followability; the registry already did. |
| `buildWatchedTypeTable` | `buildTargetTypeSet` | Pure fold of selected records. |
| `watchedTypeTableForGitDest` | `targetTypeSetForGitDest` | Reads one set. |
| `allWatchedTypeTables` | `allTargetTypeSets` | Reads every set. |
| `residentWatchedTypeTables` | `residentTargetTypeSets` | Published in-memory view. |
| `WatchedType` | keep or rename to `TargetType` | Keep if we want continuity; rename if we want the full model to be crisp. |

Recommended package/file names:

- `target_type_set.go`: `TargetTypeSet`, `WatchedType`, folding helpers.
- `target_type_sets.go`: store, refresh, projection from rules.
- `target_type_set_test.go` / `target_type_sets_test.go`.

If we keep `WatchedType`, the relationship reads well:

```text
TypeRegistry.FollowableRecords()
  -> TargetTypeSet.Types []WatchedType
```

The `WatchedType` name is still useful because the value has namespace/operation
scope and is consumed by informers/snapshots. The set itself should carry the
target-specific name.

## Naming schemes

### Scheme A: explicit and conservative

- Package: `internal/typeinventory`
- Global registry: `TypeRegistry`
- Global complete set: `AllRecords`
- Global actionable set: `FollowableRecords`
- Per-target set: `TargetTypeSet`
- Per-target member: `WatchedType`
- Evaluation file: `followability.go`
- Record file: `record.go`

This is the recommended scheme. It keeps `Followability` as the product term and
uses `TargetTypeSet` for the user's "typeset, one per GitTarget" idea.

### Scheme B: shorter, set-oriented

- Package: `internal/typeset`
- Global registry: `Registry`
- Global complete set: `All`
- Global actionable set: `Followable`
- Per-target set: `TargetTypeSet`
- Per-target member: `WatchedType`
- Evaluation file: `followability.go`
- Record file: `record.go`

This minimizes churn. It works if `TargetTypeSet` replaces `WatchedTypeTable`,
because the two levels are no longer both called some form of watched type table.

### Scheme C: catalog-oriented

- Package: `internal/typecatalog`
- Global registry: `Catalog`
- Global complete set: `AllRecords`
- Global actionable set: `LiveRecords`
- Per-target set: `TargetWatchSet`
- Per-target member: `WatchType`
- Evaluation file: `decision.go`
- Record file: `record.go`

This is readable, but it risks confusion with `APIResourceCatalog`. Choose it only
if the raw discovery catalog is renamed to `DiscoveryCatalog`.

## Suggested migration order

1. Rename `WatchedTypeTable` to `TargetTypeSet` first. This gives the biggest
   clarity win with the least package churn.
2. Rename `model.go` to `record.go` and `funnel.go` to `followability.go`.
   These are file-only changes and should be easy to review.
3. Decide whether `internal/typeset` is good enough after the target rename. If it
   still feels overloaded, rename the package to `internal/typeinventory`.
4. Only then consider accessor changes like `All()` -> `AllRecords()` and
   `Followable()` -> `FollowableRecords()`; API churn is easier once the domain
   names are settled.

## Recommendation

Adopt Scheme A unless minimizing churn is more important than clarity:

```text
internal/typeinventory
  record.go
  followability.go
  registry.go
  observations.go
  lookup.go
  scale_binding.go

internal/watch
  target_type_set.go
  target_type_sets.go
```

Use `TargetTypeSet` for the per-GitTarget object. Avoid `validTypes`; use
`allTypeRecords`, `followableTypeRecords`, and optionally `liveTypeRecords` when
the retained state needs to be explicit.
