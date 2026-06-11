# Typeset owns discovery grace: thinning the API-resource catalog

**Status: proposal (steering captured 2026-06-11, not yet implemented).**

## 1. Steering

Two wobble-hardening directions were on the table in
[startup-robustness-cert-and-crd-wobble.md](./startup-robustness-cert-and-crd-wobble.md) §5:
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
[catalog_observe.go](../../internal/watch/catalog_observe.go), no component reads the
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

### 3.3 Served versions become a registry concept

Today a registry record is keyed `(GVK, GVR, scope)` — one record per *version*. The
registry cannot answer "which versions of `(group, resource)` are served, and which is
preferred?"; that question still requires the catalog's `byGroupVer`/`Preferred` data.
That forces version-needing consumers (rule planning's `apiVersions` matching, any
`(group, resource) → GVR` resolution like the per-type stream keys, which drop the
version) toward the catalog.

Extend the registry with a per-`(group, resource)` index over its own records:

```go
// sketch
func (r *Registry) ServedVersions(group, resource string) []VersionInfo // version, preferred, followable, verdict
func (r *Registry) PreferredGVR(group, resource string) (schema.GroupVersionResource, bool)
```

Because records already carry per-version identity, this is an index + two lookups, not
new state. With the grace unified (3.2), "served versions" naturally means *last-known
versions under grace* during a blink — which is precisely the wobble-friendly answer a
consumer wants.

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

## 5. Plan

Each stage is independently shippable and e2e-validated; later stages only start when a
stage proves out.

1. **S1 — Registry index for served versions.** Add the per-`(group, resource)` index +
   `ServedVersions`/`PreferredGVR` lookups over existing records. Move the late-event
   nudge's GVR resolution (`claimedGVRForGroupResource`) onto it. No behaviour change.
2. **S2 — Raw-scan `Update`.** Introduce `Registry.UpdateFromScan(entries, failedGVs,
   complete, generation)` that performs the retain-on-error and omission-grace merge
   internally (keeping last-known facts for degraded/retained records). The catalog's
   `Observations()` path becomes a thin call into it. The catalog's *instant prune on
   complete-scan omission is removed* — this is the wobble fix, now in the right layer.
   Port the catalog's retention tests to registry tests with a fake clock.
3. **S3 — Shrink the catalog.** Strip `trusted`/`degraded`/merge state from
   `APIResourceCatalog` until it is a per-scan normalizer; move the `api_catalog_*`
   stats and degraded-transition logging onto registry-derived data. Audit that no
   component outside the refresh path imports the catalog type.
4. **S4 — Re-evaluate the remaining wobble surface.** With omission-grace unified, rerun
   the crd-lifecycle wobble analysis: if a blink can still exceed 60 s under load, decide
   *in typeset* whether the grace for freshly-settled CRDs needs staging (the old B3),
   and whether the materializer's force-release on `Refused` needs its own short
   confirmation (the old B2) — both now single-layer decisions.

## 6. Relationship to existing docs

- Supersedes the **B1-in-the-catalog** candidate of
  [startup-robustness-cert-and-crd-wobble.md](./startup-robustness-cert-and-crd-wobble.md)
  (the mechanism survives, relocated into the registry as S2). B2 remains open as S4.
- Complements [type-followability](./manifest/version2/type-followability.md): the
  registry stays "the single type Lookup + decision surface"; this proposal completes
  that claim by making it the single *grace* surface too.
