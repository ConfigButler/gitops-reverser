# Typeset owns discovery grace: thinning the API-resource catalog

**Status: S1–S3 IMPLEMENTED (2026-06-11); S4 open.** As-built deltas from the plan below
(all minor, same shape):

- The catalog kept its `Refresh(disco) (bool, error)` signature; the scan is read back
  via `Scan(sensitive) (typeset.Scan, bool)` — the sensitive policy stays a
  projection-time concern (the direct successor of `Observations(sensitive)`, which is
  deleted along with `catalog_observe.go` and `APIResourceEntry`).
- `typeset.Scan` gained `ScannedGroupVersions` (the GVs the scan returned a — possibly
  empty — resource list for), so "missing from a GV this scan actually listed" starts
  the grace even on an incomplete scan, exactly matching the old per-record behaviour.
- The catalog keeps the last normalized scan as its only cross-scan state — mechanical,
  per §3.1's allowance: it is the change fingerprint (generation bumps only on changed
  facts, so registry revisions don't churn on steady rescans) and the re-derive source
  for `refreshTypeRegistry`'s lazy callers (rule status, watched-type tables).
- `Stats()`/`DegradedGroupVersions()` survive as per-scan facts (gauge/log surfaces
  unchanged); registry tests for the relocated semantics live in
  `internal/typeset/scan_test.go`.

## 1. Steering

Two wobble-hardening directions were on the table in
[startup-robustness-cert-and-crd-wobble.md](../finished/startup-robustness-cert-and-crd-wobble.md) §5:
B1 ("confirm removals over a window **in the catalog**") and B2 ("don't force-release a
recently-served checkpoint **in the materializer**"). A B1 prototype (a
`catalogRemovalConfirmWindow` with per-group/version `missingSince` timestamps inside
`APIResourceCatalog`) was built and then **reverted** on explicit steering:

> The catalog should stay a thin wrapper on top of the Kubernetes API type system — no
> time-sensitive behaviour in there. The layer on top (`internal/typeset/registry.go`)
> already owns exactly that behaviour ("additions fast, removals slow", the 60 s
> `RemovalGrace`, the settle window). Extend the registry instead — including knowledge of
> served versions — and drive the system toward *most components consuming typeset, never
> the catalog*. Typeset is also the place that should be 'nicer' to the e2e CRD-discovery
> wobble.

This document records the target shape, the trade-offs, and a staged plan.

## 2. Where the state lives today

The pipeline is `discovery scan → APIResourceCatalog → Observations → typeset.Registry →
(watched-type table, Materializer, …)`.

The boundary is already fairly clean — outside
[api_resource_catalog.go](../../internal/watch/api_resource_catalog.go),
[manager_catalog.go](../../internal/watch/manager_catalog.go) and
catalog_observe.go, no component reads the
catalog directly; everything consumes the registry (or a projection of it, like the
watched-type table). But the catalog is **not** a thin wrapper: it holds cross-scan state
and makes retention decisions of its own:

| Catalog behaviour today | Nature |
|---|---|
| Retains a group/version whose discovery **errored** (`IsGroupDiscoveryFailedError` → `degraded`, prior entries kept) | retention decision |
| Prunes a group/version omitted by a **complete** (`err == nil`) scan, immediately | removal decision |
| Carries `trusted`/`degraded` per group/version across scans | cross-scan state |
| Carries `generation`, `ready`, and the merged `byGVR` index across scans | cross-scan state |

The registry then applies its *own* time-based judgement on top: `RemovalGrace = 60 s`
(absent → `Retained` → `Refused`), the `SettleWindow` activation debounce, and the
`TypeWobbling` freeze that the Materializer honours.

### 2.1 The exact typeset surface today, and who reads it

The full public surface, grouped by contract (everything else in the package is
unexported):

| Surface | Methods / types | Consumers |
|---|---|---|
| **`Lookup`** (the minimal cross-package contract) | `Ready()`, `ByGVK(gvk) (TypeRecord, bool)` | `internal/git` (`worker_manager.go`, `plan_flush.go` — resolve manifest GVKs on the write path), `internal/manifestanalyzer` (`store.go`, `scan.go`, `analyzer.go`) |
| **Registry queries** | `ByGVR`, `Followable()`, `All()`, `Ready()`, `Generation()`, `Revision()` | `internal/watch` only: `manager_catalog.go` (refresh + refusal logging + status projections), `watched_type_resolver.go` / `watched_type_table.go` (rule matching → per-GitTarget watched-type table), `scope_resolve.go` (`VerdictRetained` = the wobble check) |
| **Registry feed** | `Update(observations, generation)`, `Entry`, `ObservationsFromEntries` | the catalog bridge only (`catalog_observe.go` → `refreshTypeRegistry`) |
| **Lifecycle** | `Subscribe(Observer)`, `LifecycleEvent` (`TypeActivated` / `TypeWobbling` / `TypeRecovered` / `TypeRemoved` / `TypeRefused`) | `internal/watch/type_lifecycle.go` (drain → git actions + Materializer) |
| **Materializer** (demand axis) | `Declare`, `OnLifecycleEvent`, `BeginSync`, `SyncSucceeded`, `SyncFailed`, `RestoreSynced`, `RequestResync`, `Sweep`, `Phase`, `Checkpoint`, `Claimants`, `PendingSyncs`, `Inventory`, `Subscribe` | `internal/watch/materialization.go` (driver, declare, sweep, status roll-up, late-event nudge) |
| **Fixtures / static** | `NewSnapshotRegistry(Snapshot)`, `BuiltinScale`, `SplitFieldPath` | tests, `internal/auditutil/subresource_policy.go` |

Two observations worth pinning:

- **Controllers never touch typeset directly** — `GitTargetReconciler` & co. read
  `watch.Manager` projections (`MaterializationSummaryForGitTarget`,
  `FollowableTypeRecords`, rule resolution). The typeset blast radius of any change here
  is `internal/watch` + the two `Lookup` consumers, nothing wider.
- **Every consumer call is already version-complete.** `ByGVK`/`ByGVR` take a full
  group/version/kind-or-resource and return one `TypeRecord` whose `Identity` carries the
  version. Nobody asks "what versions exist?" today; rule planning matches a rule's
  `apiVersions` selector by iterating records, which already works per version. The only
  *version-less* questions in the codebase are `(group, resource) → GVR` resolutions
  (the per-type stream keys drop the version): the late-event nudge's
  `claimedGVRForGroupResource` (scans Materializer inventory) and the catalog-side
  preferred-version data that rule planning reads indirectly.

So today the "removals" story is split across two layers with different rules: an
**errored** group is retained indefinitely by the catalog, while an **omitted** group is
pruned instantly by the catalog and only then graced for 60 s by the registry. That split
is exactly where the CRD-discovery wobble bites (a complete scan that merely omits a
just-Established CRD's group), and it is why the B1 prototype gravitated into the catalog
— the wrong layer.

## 3. Target shape

### 3.1 The catalog becomes a per-scan normalizer

`APIResourceCatalog` (or its successor — possibly just a free function) does one job:
turn one `ServerGroupsAndResources()` result into a normalized, policy-annotated fact
set:

- the entries served by this scan (GVK/GVR/scope/verbs/preferred/allowed/sensitive),
- which group/versions this scan reported as **failed** (degraded),
- whether the scan was **complete** (`err == nil`).

No `missingSince`, no retention, no merging with the previous scan. Ideally no cross-scan
state at all; if `generation`/`ready` bookkeeping must live somewhere, it is mechanical,
not judgemental.

### 3.2 The registry absorbs all cross-scan judgement

`typeset.Registry.Update` today takes a pre-merged observation set, so the catalog's
merge decisions are invisible to it. In the target shape, `Update` receives the raw scan
facts (entries + failed group/versions + completeness) and applies **one** unified
"additions fast, removals slow" policy:

- **Errored group/version** → its records keep their last-known facts, marked degraded
  (what the catalog's retain-on-error does today, moved over).
- **Omitted by a complete scan** → the records go absent and the *existing*
  `RemovalGrace` machinery judges them: `Retained` for 60 s, then `Refused`. The instant
  prune disappears — the registry's grace finally covers the case it was designed for,
  because the catalog no longer destroys the records before the registry sees them.
- **Incomplete scan** → no removal judgement at all (fail-safe, as today).

The wobble tolerance then comes for free: a complete-scan blink shorter than the grace
never leaves the `Retained` band, so the Materializer's `TypeWobbling` freeze (keep the
checkpoint, keep the tail) is the only consumer-visible effect — no force-release, no
`snapshot cleared`, no lost CR. Tuning happens in exactly one place (grace, settle,
wobble) instead of two.

### 3.3 Served versions: one narrow index, no version plumbing

**Constraint (steering):** versions change rarely, and the cure must not be worse than
the disease — do **not** thread version parameters or version lists through consumer
signatures. The registry already carries the version inside every record's `Identity`
(records are keyed `(GVK, GVR, scope)` — one per version), and as §2.1 shows, every
existing consumer call is already version-complete. So "served versions" is not a new
concept to spread around; it is **one missing index** over records the registry already
holds.

The whole addition:

```go
// ByGroupResource returns the records (one per served version) for a version-less
// (group, resource) pair — the shape per-type stream keys carry. Existing TypeRecord
// fields answer everything else: Identity.GVR.Version, Preferred, Followable().
func (r *Registry) ByGroupResource(group, resource string) []TypeRecord
```

No `VersionInfo` type, no `ServedVersions`/`PreferredGVR` pair, no change to the
`Lookup` interface, no change to `TypeRecord`, no consumer signature changes. A caller
that wants "the GVR to use" picks the `Preferred` record (or the single followable one);
a caller that wants "is any version served" checks `len > 0`. The two known users:

- the late-event nudge's `(group, resource) → GVR` resolution (replaces the
  Materializer-inventory scan in `claimedGVRForGroupResource`);
- whatever S3 needs when the catalog's `byGroupVer`/preferred data stops being readable
  directly.

The index is maintained where `byGVK`/`byGVR` already are (`rebuildIndexesLocked`), so
it costs one map rebuild per Update and nothing at read time. With the grace unified
(3.2), the records it returns are *last-known versions under grace* during a blink —
the wobble-friendly answer — without any consumer being version-aware beyond what it
already is.

If a real multi-version need ever appears (it has not: conversion, storage-version
migration, and version deprecation are all out of scope today), it composes on top of
`ByGroupResource` as a caller-side concern — it does not change this surface.

### 3.4 Consumers use typeset only

End state: the only code that touches the catalog is the refresh path
(`RefreshAPIResourceCatalog` → `Registry.Update`). Everything else — rule planning,
splice scope resolution, the watched-type table, status/metrics surfaces, the late-event
nudge's `(group, resource) → GVR` resolution — reads `typeset` (`Registry` lookups,
`Materializer` inventory). The catalog stops being an API other components may grow
dependencies on.

## 4. Pros and cons

**Pros**

- **One policy, one place.** "Additions fast, removals slow" currently has three
  implementations (catalog retain-on-error, registry grace, materializer freeze) with a
  gap between the first two. Unifying removes the gap that the e2e wobble exploits and
  makes the behaviour testable in one leaf package with an injectable clock.
- **The catalog becomes trivially correct.** A per-scan normalizer needs no locking
  subtleties, no cross-scan invariants, and barely any tests beyond "does it translate a
  scan faithfully".
- **Version lookups stop leaking the catalog.** `ServedVersions`/`PreferredGVR` give the
  remaining version-aware consumers a typeset-native answer, including under grace.
- **Simpler than the B1 catalog prototype.** No new timestamps or windows anywhere: the
  *existing* `RemovalGrace` does the work once it is allowed to see the omission.
- **Leaf-friendly.** `typeset` stays client-free (facts in, verdicts out), so the moved
  logic remains deterministic and unit-testable.

**Cons / risks**

- **`Registry.Update` contract change.** It currently expects the full merged set;
  feeding it raw scans means it must retain last-known facts for degraded/omitted records
  itself. That grows the entry state (it already has `absentSince`; it would also keep
  "last-known facts while degraded") and every `Update` caller/test changes shape.
- **Genuine removals get slower in one path.** An omitted-by-complete-scan group used to
  vanish from lookups instantly (then grace applied downstream); now its records serve
  last-known facts for up to the grace. That is the *intent* ("removals slow"), but any
  consumer that relied on the instant prune (e.g. a status view wanting "discovery no
  longer lists this") must read the verdict/degraded flag rather than record absence.
- **Migration touches sensitive code.** The catalog feeds everything; moving its merge
  semantics is a behaviour-preserving-then-improving refactor that needs the full e2e
  suite at each stage (this is the same pipeline the M12/R3 work just stabilised).
- **Two sources of "degraded" during the transition.** Until the move completes, the
  catalog's `degraded` flag and the registry's verdicts coexist; metrics/log surfaces
  (`api_catalog_*` gauges, `DegradedGroupVersions` logging) must keep working from
  whichever layer currently owns the fact.

## 5. Plan — the exact interface delta per stage

Each stage is independently shippable and e2e-validated; later stages only start when a
stage proves out.

**What never changes, at any stage:** the `Lookup` interface (`Ready` + `ByGVK`), the
`TypeRecord` shape, the lifecycle vocabulary and `Subscribe`, the whole Materializer
surface, and every consumer outside `internal/watch`. The churn is confined to the
catalog→registry *feed* and the catalog's own innards.

1. **S1 — `Registry.ByGroupResource(group, resource) []TypeRecord`.** One new index in
   `rebuildIndexesLocked` + one query (§3.3). Move the late-event nudge's GVR resolution
   (`watch.Manager.claimedGVRForGroupResource`) onto it. Additive; no behaviour change;
   no other signature touched.
2. **S2 — Raw-scan feed.** The registry gains one method and one input type; the catalog
   bridge stops pre-merging:

   ```go
   // Scan is one normalized discovery result — per-scan facts, no judgement.
   type Scan struct {
       Entries             []Entry              // what this scan serves (existing Entry type)
       FailedGroupVersions []schema.GroupVersion // reported failed (IsGroupDiscoveryFailedError)
       Complete            bool                 // err == nil: omissions are meaningful
       Generation          uint64
   }
   func (r *Registry) UpdateFromScan(scan Scan)
   ```

   Internally it does what the catalog + `Update` together do today, in one place:
   failed group/versions keep their last-known entry facts (marked untrusted/degraded);
   entries omitted by a complete scan go absent and ride the **existing** `RemovalGrace`;
   an incomplete scan removes nothing. `Update(observations, generation)` stays (the
   fixture path `NewSnapshotRegistry` uses it; it may become unexported later). The
   catalog's instant prune on complete-scan omission is **deleted** — this is the wobble
   fix, in the right layer. Port the catalog's retention tests (`PartialRefreshPreserves…`)
   to registry tests with the fake clock.
3. **S3 — Shrink the catalog.** Strip `trusted`/`degraded`/merge state from
   `APIResourceCatalog` until it is a per-scan normalizer (`Refresh` → produce a `Scan`,
   call `UpdateFromScan`); move the `api_catalog_*` stats and degraded-transition logging
   onto registry-derived data (`All()` + verdicts; the `Trusted`/`Degraded` facts are
   already on the records). Audit that no component outside the refresh path imports the
   catalog type — per §2.1 that is already true; the audit pins it.
4. **S4 — Re-evaluate the remaining wobble surface.** With omission-grace unified, rerun
   the crd-lifecycle wobble analysis: if a blink can still exceed 60 s under load, decide
   *in typeset* whether the grace for freshly-settled CRDs needs staging (the old B3),
   and whether the materializer's force-release on `Refused` needs its own short
   confirmation (the old B2) — both now single-layer decisions.

## 6. Relationship to existing docs

- Supersedes the **B1-in-the-catalog** candidate of
  [startup-robustness-cert-and-crd-wobble.md](../finished/startup-robustness-cert-and-crd-wobble.md)
  (the mechanism survives, relocated into the registry as S2). B2 remains open as S4.
- Complements [type-followability](type-followability.md): the
  registry stays "the single type Lookup + decision surface"; this proposal completes
  that claim by making it the single *grace* surface too.
