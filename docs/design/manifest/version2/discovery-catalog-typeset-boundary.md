# Discovery catalog and typeset boundary

> Status: proposal, partly landed. Migration step 0 (the registry change signal, Stage 10)
> and steps 1–3 (rule status moved onto the registry's followable set; `RuleGVRResolver` and
> the dead catalog lookup/enumeration APIs deleted, Stage 11) have landed. Steps 4–6 are still
> forward-looking. See
> [type-followability-implementation.md](type-followability-implementation.md).
>
> Companion to [type-followability.md](type-followability.md),
> [type-followability-implementation.md](type-followability-implementation.md), and
> [type-followability-naming-proposal.md](type-followability-naming-proposal.md).

## Question

After the followability work, `internal/typeset` owns the type decision surface:
identity uniqueness, followability checks, the live set, retention grace, and GVK
lookup for the writer/analyzer path. That leaves
[`APIResourceCatalog`](../../../../internal/watch/api_resource_catalog.go) looking
smaller and a bit suspicious.

Should the catalog be folded into `typeset`? Or is it still doing enough to stay?

## Short answer

Do not fold the catalog into `typeset`.

Keep a small discovery-shaped catalog in `internal/watch`, but narrow it to raw
Kubernetes discovery state and rename it when we do the broader naming pass:

```text
DiscoveryCatalog
  Refresh(discovery client)
  Ready()
  Generation()
  Stats()
  DegradedGroupVersions()
  Entries() or Observations(...)

typeset / typeinventory
  Entry
  Observation
  Registry
  TypeRecord
  Followability
  Lookup
```

`typeset` should remain a leaf package. It should not depend on Kubernetes
discovery clients, controller-runtime, dynamic informers, manager logging, or
telemetry. The live manager owns those integrations and feeds neutral entries into
`typeset`.

The cleanup is still worthwhile: several catalog APIs are now only there because
rule status still uses the old `RuleGVRResolver`.

## Current production callers

Search scope: `internal` and `cmd`, excluding `*_test.go`.

| Caller | Uses | Current reason | Move to `typeset`? |
| --- | --- | --- | --- |
| `Manager.RefreshAPIResourceCatalog` | `Refresh`, `Stats`, `logCatalogTransitions` | Pulls Kubernetes discovery, records refresh metrics, rebuilds registry after a successful scan. | No. Discovery and metrics integration stay in watch/manager. |
| `Manager.logCatalogTransitions` | `Ready`, `DegradedGroupVersions` | Edge-triggered logs for first ready catalog and degraded/recovered group versions. | No for degraded GV logs; maybe use registry stats for known/followable counts. |
| `recordCatalogStats` | `CatalogStats` | Emits API catalog gauges. | Partially. Raw discovery counts stay catalog; policy/refusal/followability counts should move to registry/type stats. |
| `Manager.refreshTypeRegistry` | `Ready`, `Observations`, `Generation` | Converts current catalog scan to `typeset.Observation` and publishes `Registry`. | Keep as boundary adapter, but make catalog provide raw entries and let the adapter apply product policy. |
| `RuleGVRResolver` | `entriesForResource`, `entriesForGroup`, `entriesForGroupResource`, `allEntries`, `hasDegradedLookup`, `Ready` | Old WatchRule/ClusterWatchRule status resolver. | Yes. Replace with registry-backed rule projection/status. |
| `Manager.ResolveWatchRuleResources` | `NewRuleGVRResolver` | Status feedback for one namespaced WatchRule. | Yes. Should use the same registry/type selection as actual informers/snapshots. |
| `Manager.ResolveClusterWatchRuleResources` | `NewRuleGVRResolver` | Status feedback for one ClusterWatchRule. | Yes. Same as above. |
| `Manager.ReconcileForRuleChange` | `RefreshAPIResourceCatalog` | Refreshes discovery before computing target type sets. | No. It should keep asking the manager to refresh discovery+registry. |
| `resolveSnapshotGVRs` | `RefreshAPIResourceCatalog`, then registry readiness | Fails closed before snapshotting. | No direct catalog call is fine at manager level; snapshot should keep reading registry after refresh. |

## APIs that were obsolete after rule status moved (done — Stage 11)

These catalog methods had no production caller except the old `RuleGVRResolver`, or no
production caller at all. With rule status moved onto the registry (Stage 11) they are gone:

| API | Old state | Fate |
| --- | --- | --- |
| `Entry` | No production caller found. | **Deleted.** |
| `CatalogLookup`, `LookupGVK`, `LookupGVR` | No production caller found; leftover from mapper era. | **Deleted.** `typeset.Registry.ByGVK` is the live lookup surface. |
| `GroupVersionDegraded` | No production caller found. | **Deleted.** A mapper-era leftover with no caller once rule status stopped probing the catalog. |
| `DegradedGroupVersions` | Operator degraded/recovered log + catalog stats. | **Kept** — for the operator-facing degraded-group/version log line and the catalog gauges, *not* for rule status. |
| `entriesForResource`, `entriesForGroup`, `entriesForGroupResource`, `allEntries` | Only `RuleGVRResolver`. | **Deleted.** |
| `hasDegradedLookup` | Only `RuleGVRResolver`. | **Deleted** (not re-homed). Rule status reports only the followable types a rule watches; it raises no per-selector "discovery degraded" diagnostic — see "Rule status resolution" below. |
| `byGVK`, `byResource`, `byGroupRes` indexes | Only supported the obsolete lookups/resolver. | **Deleted.** |

After this cleanup the catalog keeps only its group/version-keyed raw scan (`byGroupVer`), the
derived `byGVR` index it feeds to `typeset`, and group/version trust state.

## What should not move

### Discovery refresh

`typeset` should not call `ServerGroupsAndResources()`. That would pull live
cluster IO and Kubernetes discovery error handling into a package that is currently
usable by:

- the live watch manager,
- the no-cluster manifest analyzer,
- tests and snapshot fixtures,
- the git writer's `typeset.Lookup`.

That leaf shape is valuable. Keep discovery IO in `internal/watch`.

### Partial discovery preservation

The catalog currently preserves the last trusted entries for group/versions that
discovery reports as failed, and separately marks those group/versions degraded.
That is scan mechanics, not product followability. `typeset` should receive this as
entry facts:

```go
typeset.Entry{
    GVK: ...
    GVR: ...
    Degraded: true,
}
```

Then `typeset` can decide `trusted -> discovery-degraded` or `retained`, but it
does not need to know how Kubernetes discovery produced that state.

### Refresh metrics and discovery logs

Metrics like refresh duration, refresh changed/unchanged/error, and degraded
group/version transitions belong near discovery refresh. They are about scanning
the API server, not about type followability.

## What should move

### Rule status resolution — done (Stage 11)

`ResolveWatchRuleResources` and `ResolveClusterWatchRuleResources` no longer use
`RuleGVRResolver`; they report the same decision the actual watch/snapshot path uses,
because they read the same surface.

Before, two paths could disagree:

```text
actual watching:  registry.Followable() -> match rule selectors -> TargetTypeSet -> informers/snapshots
rule status:      APIResourceCatalog -> RuleGVRResolver -> ResolveMiss -> status text
```

That split could lie — a rule could read "resolved" through `RuleGVRResolver` while the
registry refused the same type (identity, origin, scale, sensitivity, a stricter verb
requirement), or the reverse. Now both paths share one matcher:

```text
registry.Followable() -> matchFollowableRecords(rule selector) -> watched types
```

`matchFollowableRecords` (in `internal/watch/watched_type_resolver.go`) is the single
rule-matching surface, used by both the per-GitTarget watched-type tables and rule status,
so the status answer and the active informer/snapshot answer cannot drift.

**Report only what is watched — no refusal taxonomy, no degraded diagnostic.** The status is
deliberately minimal: a rule's `ResourcesResolved` condition reports catalog readiness and how
many distinct followable types the rule currently watches (`"watching N resource type(s)"`),
and nothing more. It does *not* explain why an individual selector matched nothing — absent,
denied-by-policy, verb-poor, ambiguous, and discovery-degraded are all the same to a mirror,
and the application does not surface those distinctions. The only unresolved (`False`) case is
a catalog that has not yet observed discovery (`"API resource catalog is not ready"`).

This **reverses** the earlier proposal in this section, which kept a per-selector
`DiscoveryDegraded` diagnostic and therefore wanted `hasDegradedLookup` re-homed onto the
status projection. That diagnostic is gone — `hasDegradedLookup` was *deleted, not re-homed*.
The full machine-readable "why is this type not followed?" answer still lives on the registry
record (`Manager.TypeRecords()`) for anyone who needs it; it is simply not projected per rule
selector into operator status. The catalog's `DegradedGroupVersions()` stays, but only for the
manager's operator-facing degraded/recovered **log** line and the catalog gauges.

### Policy application inside catalog entries

`APIResourceEntry` currently stores `Allowed` and `PolicyReason`. That is product
policy, not raw Kubernetes discovery.

Better boundary:

```text
DiscoveryCatalog.Entry
  GVK
  GVR
  Namespaced
  Verbs
  Preferred
  Subresource
  Degraded

watch/catalog_observe.go adapter
  applies allowedResource(...)
  applies SensitiveResourcePolicy
  builds typeset.Entry

typeset
  evaluates policy/sensitivity/followability
```

This keeps the catalog raw and makes the policy boundary explicit.

### Allowed/excluded catalog metrics

`CatalogStats` currently counts `AllowedResources` and `ExcludedResources`.
Those are no longer pure catalog facts once policy moves out of the raw entry.

Suggested split:

| Metric family | Owner | Examples |
| --- | --- | --- |
| Discovery scan metrics | `DiscoveryCatalog` / manager | refresh outcome, duration, generation, trusted/degraded group versions, served top-level resource count |
| Type decision metrics | `typeset.Registry` / manager | known records, followable/live records, refused records by first failing requirement/reason |

That split names what is actually being measured.

### The registry's own change signal (the gate the table watches) — landed

This was the boundary leak that mattered most in practice, because it caused a real
bug. It is the one part of this proposal already implemented (Stage 10); the rest of
this document is still forward-looking.

The per-GitTarget `WatchedTypeTable` is re-projected only on a deliberate trigger — a
rule-set change or a "did the type surface change?" signal — so the common no-change
reconcile is a cheap compare rather than a rescan. The question is *which* signal
answers "did the type surface change?".

That signal **used to be `registry.Generation()`**, which is just the catalog's
generation passed straight through:

```go
reg.Update(catalog.Observations(...), catalog.Generation())
```

So a consumer of the decision surface (the table) was actually gated on the **scan
layer's** counter — the boundary inversion this document set out to remove: the table
should depend on the registry, not on how Kubernetes discovery counts revisions.

It was not only inelegant — it was **incorrect at the retention-grace boundary.** The
grace is owned by the registry and is time-based, so a type can leave the live set
*without any discovery change*:

```text
t0  discovery serves T           catalog gen = N     T followable
t1  discovery drops T            catalog gen = N+1   T retained (grace running)   <- gen moved, table re-projects, keeps T
t1..t60  discovery stable         catalog gen = N+1   T still retained
t61 grace elapses                catalog gen = N+1   T dropped from Followable()  <- gen did NOT move
```

At `t61` the followable set changed but the catalog generation did not, so a
generation-gated table never re-projected: the dropped type lingered in the table, its
informer kept listing a resource the server no longer served, and the target's
mark-and-sweep snapshot failed closed against a phantom GVR. The old `watchedTypeStore`
removal grace hid this by re-judging absence itself; once the grace moved into the
registry, the gate had to watch a registry-owned signal instead.

**Implemented: the registry exposes `Revision()` — the decision-surface analog of the
catalog's `Refresh() (changed, generation)`.**

```go
// Update bumps an internal revision whenever the *followable membership* changes
// (a type appears, drops after grace, or flips followable<->refused) or the backing
// scan generation moves. It is the registry's "something a consumer cares about
// changed" signal — independent of how discovery counts generations.
func (r *Registry) Update(obs []Observation, generation uint64)
func (r *Registry) Revision() uint64
```

The `WatchedTypeTable` gate is now `(registry.Revision(), rulesFingerprint)` instead of
`(catalog generation, rulesFingerprint)`. The payoff, confirmed in practice:

- **Correctness:** the revision bumps exactly when a retained type leaves the live set,
  so the grace-drop re-projects the table and stops the phantom informer — no separate
  watched-type-layer absence tracking required. (`internal/typeset/registry_test.go`
  `TestRegistry_RevisionBumpsOnGraceDropAtStableGeneration` locks this in.)
- **Clean boundary:** the table depends only on the decision surface. The catalog's
  generation is now a pure scan-layer detail (still passed into `Update` as the
  `ResolvedAt` stamp), and the registry exposes its own change-of-decision signal. Each
  layer reports change in its own terms.
- **No new cost:** the revision also bumps on generation change, so steady-state
  re-projection frequency is unchanged from the generation-gated version; the grace case
  only adds the occasional extra bump it was previously missing.

The companion safety — `resolveSnapshotGVRs` failing closed while a watched type is
`retained` (currently unserved, mid-grace) rather than streaming a phantom GVR — landed
alongside it, re-expressing the old pending-removal fail-closed in the registry's
verdict vocabulary.

`Generation()` stays available (still useful as the `ResolvedAt` stamp and for
diagnostics); `Revision()` is the thing the gate watches.

## Proposed final shape

### `internal/watch/discovery_catalog.go`

Raw discovery cache:

```go
type DiscoveryCatalog struct {
    byGroupVersion map[schema.GroupVersion][]DiscoveryEntry
    groupVersion   map[schema.GroupVersion]DiscoveryGroupVersionState
    generation     uint64
    ready          bool
}

type DiscoveryEntry struct {
    GVK         schema.GroupVersionKind
    GVR         schema.GroupVersionResource
    Namespaced  bool
    Verbs       []string
    Preferred   bool
    Subresource bool
    Degraded    bool
}
```

Public-ish surface:

```go
func (c *DiscoveryCatalog) Refresh(d apiResourceDiscovery) (changed bool, err error)
func (c *DiscoveryCatalog) Ready() bool
func (c *DiscoveryCatalog) Generation() uint64
func (c *DiscoveryCatalog) Entries() []DiscoveryEntry
func (c *DiscoveryCatalog) Stats() DiscoveryStats
func (c *DiscoveryCatalog) DegradedGroupVersions() []schema.GroupVersion
```

No GVK/GVR lookup helpers. No resource selector expansion. No product policy.

### `internal/watch/catalog_observe.go`

Boundary adapter:

```go
func observationsFromDiscoveryCatalog(
    catalog *DiscoveryCatalog,
    sensitive types.SensitiveResourcePolicy,
) []typeset.Observation
```

Responsibilities:

- copy raw discovery entries,
- apply default resource policy,
- apply configured sensitive-resource policy,
- call `typeset.ObservationsFromEntries`.

### `internal/typeset`

Decision surface:

```go
Registry.Update([]Observation, generation)
Registry.All()
Registry.Followable()
Registry.ByGVK()
Registry.Revision() // change-of-decision signal the TargetTypeSet gate watches
```

Potential additions for rule status:

```go
type Selector struct {
    APIGroups   []string
    APIVersions []string
    Resources   []string
    Scope       Scope
}

func MatchRecords(records []TypeRecord, selector Selector) MatchResult
```

Whether this helper belongs in `typeset` or `internal/watch` depends on whether
we want `typeset` to know WatchRule selector semantics. The conservative choice is
to keep selector matching in `internal/watch` but make it consume only
`typeset.TypeRecord` values.

## Migration plan

0. ✅ **Done (Stage 10).** Gave `Registry` a `Revision()` and gated the `WatchedTypeTable`
   on it instead of the catalog generation. This was the smallest, highest-value step: it
   fixed the retention-grace lingering-type bug and removed the table's dependency on the
   scan counter. It was independent of the rule-status work below.
1. ✅ **Done (Stage 11).** Moved WatchRule and ClusterWatchRule status onto registry-backed
   matching (`matchFollowableRecords` over `registry.Followable()`). The status now reports
   only what the rule watches — catalog readiness plus a followable-type count — and drops the
   refusal taxonomy entirely. The degraded-group/version diagnostic was *deliberately dropped*,
   not preserved: the application does not surface per-selector discovery-degraded reasons (see
   "Rule status resolution" above). This reverses the earlier sub-proposal to re-home
   `hasDegradedLookup`.
2. ✅ **Done (Stage 11).** Deleted `RuleGVRResolver` (and the `ResolveMiss` reason vocabulary).
3. ✅ **Done (Stage 11).** Deleted the catalog lookup/enumeration APIs and indexes that only
   existed for the old resolver (`Entry`, `CatalogLookup`/`LookupGVK`/`LookupGVR`,
   `GroupVersionDegraded`, `hasDegradedLookup`, `entriesFor*`/`allEntries`, and the
   `byGVK`/`byResource`/`byGroupRes` indexes).
4. Move `Allowed`/`PolicyReason` out of `APIResourceEntry` and into the observation
   adapter.
5. Split catalog stats into raw discovery stats and registry/type decision stats.
6. Rename `APIResourceCatalog` to `DiscoveryCatalog` during the naming pass.

## Recommendation

Keep the boundary:

```text
DiscoveryCatalog: what Kubernetes discovery said and how trustworthy the scan is.
typeset/typeinventory: what GitOps Reverser decides about each type.
TargetTypeSet: what one GitTarget selected from the followable/live records.
```

Most callers should not move to `typeset`; they should move to the manager's
registry-backed view. The one big exception is rule status: it should stop reading
raw catalog entries and start reporting the same registry decisions that drive the
actual watchers.
