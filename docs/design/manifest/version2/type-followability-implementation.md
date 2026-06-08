# Type followability — implementation log

> Companion to [type-followability.md](type-followability.md). This file is the
> running, append-only record of what the implementation actually changed, in the
> order it changed it. The design doc says what we want; this says what we did.

## Goal

Collapse the scattered type-handling logic (catalog scan + GVK→GVR mapper + rule
GVR resolver + watched-type table/resolver + a hardcoded scale switch) into the
three-layer model of the design:

```
Scan ─▶ Observation ─▶ TypeRegistry (the single decision surface) ─▶ TargetView
```

The heart is one `TypeRecord` carrying one `Followability` (verdict + summary +
funnel-ordered checks), with a single reason-code vocabulary used everywhere a
type is turned away.

## Strategy

Land it in green-keeping increments — `task test` (>90% coverage) and
`task test-e2e` must stay green at every committed step. New canonical model lives
in a fresh leaf package `internal/typeset` (no Kubernetes client deps, just
apimachinery `schema`), so both the live cluster path (`internal/watch`) and the
no-cluster analyzer path can share one decision surface.

## Change list

### Stage 1 — `internal/typeset` model + funnel evaluator (additive)

- New package `internal/typeset`. Pure data model + the funnel that turns an
  `Observation` (raw per-type facts) plus policy into a `Followability`.
- Files:
  - `model.go` — `Identity`, `Scope`, `Origin`, `OriginKind`, `Confidence`,
    `TypeRecord`, `Followability`, `Check`, `Verdict`, `Requirement`, `Result`,
    `Reason`, `Subresources`, `StatusFact`, `ScaleBinding`. `TypeRecord.Followable()`.
  - `scale.go` — the built-in scale registry (`BuiltinScale`), the single source
    of `/scale` parent-replica facts for built-ins (folds the old
    `auditutil.BuiltinScaleReplicasPath`).
  - `funnel.go` — `Evaluate(Observation, Policy) Followability`: the funnel-order
    requirement checks and the mechanical verdict derivation.
- Reason vocabulary (kebab-case, stable): `not-served`, `subresource-only`,
  `discovery-degraded`, `catalog-unavailable`, `absence-expired`, `gvk-not-unique`,
  `gvr-not-unique`, `scope-unknown`, `missing-verb`, `origin-unknown`,
  `denied-by-policy`, `sensitive-unsupported`, `scale-path-unresolved`.

- Tests: `model_test.go`, `funnel_test.go`, `scale_test.go` — table-driven, 95.2%
  package coverage. The funnel test mutates one field of a baseline followable
  Deployment observation per case and asserts verdict + summary + the failing check.

### Stage 2 — `typeset.Registry` decision surface (additive)

- `registry.go` — `Registry`: one `TypeRecord` per known type, the lookups
  (`ByGVK`, `ByGVR`, `Followable`, `All`, `Ready`, `Generation`), and the live-set
  `RemovalGrace` (fixed 60s). `Update(observations, generation)` replaces the set and
  applies the grace: additions are immediate; a previously-live type that stops being
  observed is re-judged `retained` within the grace and dropped once it elapses. The
  clock is injectable (`newRegistry`) so the grace is deterministic in tests.
- Identity ambiguity: a GVK served by >1 GVR keeps one refused record per resource
  (each carries `gvk-not-unique`); `ByGVK` returns the deterministic first by GVR.
- Tests: `registry_test.go` — grace retain→drop, reappearance restarts the grace,
  refused (never-live) types drop immediately, ambiguous-GVK refusal. 97% coverage.

### Stage 4 — single source of built-in scale facts

- `auditutil.BuiltinScaleReplicasPath` is now a thin `[]string` adapter over
  `typeset.BuiltinScale` + `typeset.SplitFieldPath`. The hardcoded apps/core switch
  moved into `typeset.BuiltinScale`, so the followability registry (origin/scale
  enrichment) and the audit consumer (`internal/queue` scale write) read one binding
  and can never drift. Callers and their tests are unchanged (path still
  `["spec","replicas"]`, still a fresh owned slice).

### Stage 5 — live Scan → Observation → Registry pipeline in the Manager

- `internal/watch/catalog_observe.go` — `APIResourceCatalog.Observations()` projects
  the catalog scan into one `typeset.Observation` per served top-level type: identity
  (GVK/GVR/scope), discovery verbs, preferred, trust state, resource policy
  (`Allowed`/`PolicyReason` → `denied-by-policy`), GVK identity uniqueness, the core
  Secret sensitivity flag, and folded subresource facts (`/status` presence, `/scale`
  binding from `typeset.BuiltinScale`). Origin is a group-shape heuristic (builtin vs.
  crd, confidence `inferred`) that never returns `unknown` for a served type.
- `internal/watch/manager_catalog.go` — the Manager holds a `*typeset.Registry`
  (`typeRegistryInstance`), `refreshTypeRegistry()` republishes it from the scan after
  every `RefreshAPIResourceCatalog`, and `FollowableTypeRecords()` / `TypeRecords()`
  expose the live set and the full inventory. The "API resource catalog ready" log
  line now reports `followableTypes` / `knownTypes`, so the pipeline is exercised on
  the real cluster during e2e.
- Tests: `catalog_observe_test.go` — followable built-in (Deployment, with a usable
  scale binding), followable CRD, policy-denied built-in (Pod), missing-verb built-in
  (Node), sensitive-but-supported (Secret), subresources excluded from the registry,
  ambiguous-GVK refusal, and the Manager refresh populating the registry at the
  catalog generation.

### Stage 6 — CatalogMapper deleted; the live mapper is registry-backed

The user asked to remove `CatalogMapper` outright and accept the behavior change,
simplifying where a choice is forced (no detailed per-GitTarget report for a
misconfigured cluster type — yet).

- **Deleted** `internal/watch/catalog_mapper.go` (the `CatalogMapper` struct, the old
  `Manager.Mapper()`, and the `mappingEntries`/`verbSlice`/`lookupState` helpers).
- **New** `internal/watch/registry_mapper.go` — `registryMapper` implements
  `mapping.ResourceMapper` over `typeset.Registry`. `Manager.Mapper()` returns it.
  `GVRForGVK` now answers from the single decision surface:
  - known + followable/retained → `Resolved` (GVR, scope→Namespaced, verbs, preferred);
  - known + refused → first-failing requirement maps to a status: `identity` →
    `Ambiguous`, `policy` → `Disallowed`, anything else → `Unserved` (the simplification
    — the analyzer already lumps the non-policy refusals into one "unresolved" issue,
    so collapsing verb/scope/origin/scale refusals to `Unserved` loses no decision);
  - unknown kind → `CatalogUnavailable` (registry not ready), `DiscoveryDegraded` (its
    group/version is degraded), else `Unserved`. Trust state still comes from the
    catalog via the new `APIResourceCatalog.GroupVersionDegraded`.
- `refreshTypeRegistry` now publishes only once `catalog.Ready()`, so an unready
  catalog keeps the registry (and therefore the mapper) reporting `CatalogUnavailable`
  rather than an empty-but-ready scan.
- The mapper reduction in `internal/mapping` is untouched — the no-cluster analyzer
  still uses the static-snapshot/structure-only implementations. Only the *live*
  implementation moved onto the registry.
- Tests: `catalog_mapper_test.go` → `registry_mapper_test.go` (Resolved, Unserved,
  CatalogUnavailable, Disallowed, **Ambiguous** — the new global identity rule —
  DiscoveryDegraded, context-cancelled, nil-registry, and Manager hand-out).

**Verb requirement relaxed (deliberate deviation from the design doc).** The doc lists
`verbs` as get/list/watch/**patch**. Once the registry backs the *live* mapper, a
patch requirement would refuse read-only-but-mirrorable types (and broke the
list/watch-only test fixtures). GitOps Reverser mirrors cluster→Git, which is a read
path, so [`requiredVerbs`](../../../../internal/typeset/funnel.go) is now
**get/list/watch**. The one write-back (a `/scale` replica assignment) is gated on the
scale subresource's own verbs via the `scale` requirement, not on the parent carrying
`patch`. This also keeps the funnel coherent with the live watch resolver (which only
ever required list+watch).

### Stage 7 — `internal/mapping` deleted; the registry is the only Lookup

The whole `mapping.ResourceMapper` / `mapping.Result` contract is gone. It had two
jobs: a status reporter (its 8-way vocabulary — not needed, the registry answers one
followability question) and a *source abstraction* that let the offline analyzer and
the live worker share one engine. `typeset` now does the second job too, so the
package was deleted.

- **`typeset.Lookup`** ([lookup.go](../../../../internal/typeset/lookup.go)) — the
  minimal surface every consumer reads: `Ready() bool` + `ByGVK(gvk) (TypeRecord,
  bool)`. `*typeset.Registry` satisfies it. The three former mapper modes are now
  three registry constructors: live (`Manager.TypeRegistry()`), snapshot
  (`NewSnapshotRegistry`, for fixtures/CLI), and structure-only (an un-`Update`d
  `NewRegistry()`, whose `Ready()==false` is exactly "no API source — don't judge").
- **`typeset.ObservationsFromEntries`** ([observe.go](../../../../internal/typeset/observe.go))
  — the scan reduction (identity uniqueness, origin, scale, sensitivity, policy) moved
  out of `internal/watch` into `typeset`, operating on a neutral `typeset.Entry`. The
  catalog converts its discovery entries to `Entry`; the snapshot builds `Entry`
  fixtures. One reduction, two sources.
- **`internal/manifestanalyzer`** now consumes a `typeset.Lookup`. `DocumentModel.
  Mapping` is a 3-value `MappingOutcome` (`Followable` / `NotFollowable` /
  `NoSource`) instead of `mapping.Status`. Acceptance collapses every not-followable
  cause into one `IssueUnresolvedKRM` refusal (the `IssueUnwatchedAPIKRM` policy
  distinction is dropped — the user asked not to over-report *why* a type is refused).
  `hasAPISource` / `plan.go` follow the 3-state.
- **`internal/git`** worker threads a `typeset.Lookup` (the field/method keep the
  `mapper`/`SetMapper` names); `cmd/main.go` injects `watchMgr.TypeRegistry()`.
- **`internal/watch`** gained `Manager.logTypeRefusals` — the single central place
  refusals are logged (one V(1) line per refused type, edge-triggered by GVK+summary
  so a stable refusal logs once). The full machine-readable "why" stays on the
  registry record (`TypeRecords()`), not in logs.
- **Deleted:** `internal/mapping/` (mapper.go, static_snapshot.go, structure_only.go
  + tests). All fixtures across `manifestanalyzer` and `git` tests moved to
  `typeset.Snapshot`/`typeset.Entry`; a snapshot entry with no Verbs is assumed
  followable-verbed (a fixture convenience), and the status-table tests collapsed to
  the 3 outcomes.

### Stage 8 — close the GVR→Kind direction of the bijection (review follow-up)

A review noted `gvr-not-unique` was modelled but never produced: `observationFromEntry`
hardcoded `GVRUnique: true`. Verdict: not a real-world bug (a GVR is
group/version/**resource**, and discovery keeps a resource name unique per
group/version, so `GVR → Kind` is structurally 1:1 from a live cluster — and the
catalog dedupes `byGVR` before observations are even built), but a genuine
model-consistency gap: the funnel advertised a `gvr-not-unique` reason and
`GVRUnique`/`GVRConflictDetail` fields that were dead, and a non-discovery `Lookup`
source (a snapshot fixture) could be handed a duplicate GVR and would silently pick a
winner instead of refusing both.

Fix in [observe.go](../../../../internal/typeset/observe.go): identity uniqueness is
now computed in **both** directions from the entries — `distinctGVRsByGVK` (GVK→
distinct resources, `gvk-not-unique`) and `distinctGVKsByGVR` (GVR→distinct kinds,
`gvr-not-unique`), both keyed on distinct values so an exact duplicate (same GVR+GVK)
collapses rather than being mistaken for a conflict. The live path is unchanged
(catalog `byGVR` is deduped, so `GVRUnique` stays true — correct, real discovery is
1:1); the snapshot/non-discovery path now refuses a GVR served by two Kinds with
`gvr-not-unique`. Regression tests: `TestObservationsFromEntries_AmbiguousGVR` and
`_ExactDuplicateIsNotAConflict`.

### Stage 9 — sensitivity is a policy input on the Entry, not inferred (review follow-up)

A review asked: shouldn't `Sensitive` be modelled on `typeset.Entry` already, since it
is known at startup? It was right. `typeset` hardcoded `coreSecret(group, resource)`,
which **ignored the operator-configured `SensitiveResourcePolicy`** (the flag-driven
"additional sensitive resources") — so `TypeRecord.Sensitive` was wrong for anything
sensitive beyond core Secrets. (Nothing reads `TypeRecord.Sensitive` yet, so this was a
model-correctness fix, not a behavior change.)

- `typeset.Entry` gained a `Sensitive bool` input (next to `Allowed`/`PolicyReason`);
  `observationFromEntry` now reads `e.Sensitive` and the hardcoded `coreSecret` helper
  is gone. `typeset` no longer knows the word "secrets" — sensitivity is policy, applied
  by the entry builder.
- `APIResourceCatalog.Observations(sensitive types.SensitiveResourcePolicy)` applies the
  configured policy per entry (`sensitive.IsSensitive(group, resource)`), exactly as the
  allow/deny policy is applied. The watch `Manager` gained a `SensitiveResources` field,
  set from `cfg.sensitiveResources` in `cmd/main.go` (the same value the worker already
  gets); `refreshTypeRegistry` passes it. The zero value still treats core Secrets as
  sensitive.
- Test: `TestObservations_AppliesConfiguredSensitivePolicy` proves an operator-marked
  type (configmaps) comes back `Sensitive`, core Secrets stay sensitive, and an unlisted
  type does not.

### Still NOT migrated (the remaining follow-up)

- **`WatchedTypeTable` identity/conflict + the live-set grace
  (snapshot/informer/plan-hash).** The table flags a `TypeConflict` only among the GVRs
  a GitTarget's *rules* selected (local scope); the registry decides identity
  uniqueness *globally* (a kind served by >1 GVR anywhere is refused) — and the live
  mapper now already enforces that global rule. The remaining swap moves the 60-second
  removal grace out of `watchedTypeStore` into the registry and re-expresses the
  pending-removal *fail-closed* snapshot path (the safety that stops a transient
  discovery wobble from sweeping Git) in terms of the registry's `retained`/`refused`
  states. That is the next milestone: make `BuildTargetView` consume
  `registry.Followable()` and delete the duplicated identity/grace logic in
  `rule_gvr_resolver.go` / `watched_type_table.go` / `watched_type_resolver.go`.

## Files touched

New:

- `internal/typeset/model.go`, `funnel.go`, `scale.go`, `registry.go`, `observe.go`,
  `lookup.go` (+ tests).
- `internal/watch/catalog_observe.go` (+ `catalog_observe_test.go`).
- `docs/design/manifest/version2/type-followability-implementation.md` (this log).

Deleted:

- `internal/mapping/` (mapper.go, static_snapshot.go, structure_only.go + tests) — the
  registry is now the only `Lookup`.
- `internal/watch/catalog_mapper.go` and `registry_mapper.go` (+ tests) — the worker
  reads `Manager.TypeRegistry()` directly.

Modified:

- `internal/auditutil/subresource_policy.go` — `BuiltinScaleReplicasPath` now adapts
  `typeset.BuiltinScale`.
- `internal/watch/manager.go` — `typeRegistry` + `typeRefusalsLogged` fields.
- `internal/watch/manager_catalog.go` — `typeRegistryInstance`, `refreshTypeRegistry`
  (gated on `catalog.Ready()`), `TypeRegistry`, `FollowableTypeRecords`, `TypeRecords`,
  `logTypeRefusals`, ready-line `followableTypes`/`knownTypes`.
- `internal/watch/api_resource_catalog.go` — added `GroupVersionDegraded`.
- `internal/typeset/funnel.go` — `requiredVerbs` relaxed to get/list/watch.
- `internal/manifestanalyzer/` — `store.go` (`MappingOutcome`, `typeset.Lookup`
  signatures, `resolveMapping`/`resolvedIdentity`), `acceptance.go`, `plan.go`,
  `scan.go`, `analyzer.go` migrated off `mapping`.
- `internal/git/worker_manager.go`, `branch_worker.go`, `plan_flush.go` — `mapper`
  field/param is now `typeset.Lookup`.
- `cmd/main.go` — `SetMapper(watchMgr.TypeRegistry())`.

## Validation

- `task fmt` / `task generate` / `task manifests` — clean, no generated diffs (no API
  types changed).
- `task vet` / `task lint` — clean (the lint pass fixed: cyclop on `reasonPhrase`,
  gochecknoglobals on the verb list, gocognit on the scale test, nonamedreturns on
  `splitSubresource`, unparam on `newRegistry`, and the `exhaustive` map/switch checks
  — resolved with a phrase map + a boolean helper).
- `task test` — all packages pass; `internal/typeset` 97.9%, `internal/watch` 83.0%
  (whole-package; the new code paths are covered).
- `task test-e2e` — **44 Passed, 0 Failed, 8 Skipped — Test Suite Passed**, re-run
  green at each behavior-changing step: the registry as additive inventory (Stage 5),
  the live mapper switched onto it (Stage 6), and `internal/mapping` deleted with the
  worker + analyzer reading the registry directly (Stage 7). The worker resolves
  manifest GVKs through the registry on the real k3d cluster, so e2e exercises the
  whole pipeline end to end.
