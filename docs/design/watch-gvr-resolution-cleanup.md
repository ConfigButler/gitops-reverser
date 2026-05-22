# Cleanup Plan: Dead Code After `APIResourceCatalog`

## Context

Commit `aa2e463` introduced `APIResourceCatalog` and `RuleGVRResolver` as the
single source of truth for turning a `WatchRule` into concrete GVRs. It added
~1580 lines and removed ~382. Several pre-catalog mechanisms are now logically
dead: they still compile and are still reachable through a live call chain, so
the `unused` linter stays quiet, but their behaviour is now impossible to reach.

This plan removes that dead code. It is pure cleanup — no behaviour change is
intended. Each item below states why it is dead and what evidence confirms it.

Unrelated correctness nits found during review (discovery refresh has no
debounce/TTL, wrong-scope resources report as `NotServed`, stale
`ResourcesResolved` condition on non-success paths) are **out of scope** here and
tracked separately.

## Removal items

### 1. Delete `internal/watch/discovery.go`

`FilterDiscoverableGVRs` is redundant. `RuleGVRResolver.Resolve` already returns
only entries that pass `Allowed && !Subresource && Supports("list","watch")`
(`rule_gvr_resolver.go` `gvrsForCandidates`) and `filterScope`.
`FilterDiscoverableGVRs` re-applies the identical predicate against the same
catalog, and calls `RefreshAPIResourceCatalog` a second time in one reconcile
(`ReconcileForRuleChange` already refreshed).

Actions:

- Remove `FilterDiscoverableGVRs`.
- In `ReconcileForRuleChange`, drop the `filter` / `m.discoveryFilter`
  indirection and pass `requestedGVRs` straight to `compareGVRs`.
- Remove the `discoveryFilter` field from `Manager` (`manager.go`).
- Relocate the still-used helpers so the file can be deleted:
  - `parseGroupVersion`, `groupVersion`, `key`, `groupVersionSplit`,
    `matchesScope` → `api_resource_catalog.go` (catalog/resolver helpers).
  - `restConfig()` → `manager_catalog.go` (with the rest of the discovery
    wiring).
- Delete `internal/watch/discovery.go`.

Test impact: `discovery_integration_test.go` calls `FilterDiscoverableGVRs`
directly and stubs `discoveryFilter`; `rule_change_snapshot_test.go` stubs
`discoveryFilter` to force `compareGVRs` to see zero GVRs. Both must be reworked
to control `requestedGVRs` instead (empty RuleStore or empty catalog). See
[Test impact](#test-impact).

### 2. Delete `internal/watch/resource_filter.go`

`shouldIgnoreResource` is now a no-op (`return false`). The real exclusion list
moved to `resource_policy.go` (`isDefaultResourceExcluded` / `allowedResource`),
surfaced on `APIResourceEntry.Allowed`.

Actions:

- Remove the dead `shouldIgnoreResource` call sites — `manager.go`
  `listResourcesForGVR` and `informers.go` — both are unreachable branches.
- Delete `internal/watch/resource_filter.go` and
  `internal/watch/resource_filter_test.go` (the test exercises a no-op).

### 3. Remove dead helpers from `internal/watch/gvr.go`

`singleConcrete` and `addGVR` are not used by any production code — the new
`gvrFromCompiledRule` / `gvrFromClusterRule` go through `RuleGVRResolver`. They
are kept compiling only by `helpers_test.go`. `addGVR` also calls the dead
`shouldIgnoreResource` from item 2.

Actions:

- Remove `singleConcrete` and `addGVR` from `gvr.go`.
- Remove their tests from `helpers_test.go`.

### 4. Remove the unavailable-GVR retry machinery from `internal/watch/manager.go`

This subsystem existed to handle "a requested GVR is not discoverable yet (CRD
not installed) — track it and retry discovery." That state can no longer occur.

Evidence it is dead:

- `requestedGVRs` (from `computeRequestedGVRs` → `RuleGVRResolver.Resolve`) only
  ever contains catalog-validated GVRs (`Allowed`, listable, watchable,
  scope-correct).
- `discoverableGVRs` is `FilterDiscoverableGVRs(requestedGVRs)`, which applies
  the same predicate against the same catalog — so
  `discoverableGVRs == requestedGVRs` in every non-race case.
- Therefore `processRequestedGVRs` always takes the `handleAvailableGVR` branch,
  `newlyUnavailable` is always empty, `scheduleRetries` never spawns a goroutine,
  and `unavailableGVRs` / `unavailableGVRsLastTry` stay empty for the process
  lifetime.
- A genuinely unserved resource is now a `ResolveMiss(NotServed)` and never
  becomes a GVR, so the "requested but not discoverable" case is structurally
  impossible.

The retry behaviour is already replaced — and improved:

- CRD and `APIService` trigger informers (`startAPISurfaceTriggerInformers`)
  signal `catalogRefreshCh` on any add/update/delete, driving an event-paced
  `ReconcileForRuleChange`. This picks up a freshly installed CRD faster than
  the old fixed 2s retry poll.
- The 30s `periodicReconcileInterval` ticker is the backstop for missed signals
  and built-in API changes.

Actions — remove from `manager.go`:

- Functions: `updateUnavailableGVRTracking`, `initializeUnavailableMaps`,
  `buildDiscoverableSet`, `processRequestedGVRs`, `handleAvailableGVR`,
  `handleUnavailableGVR`, `handleNewlyUnavailableGVR`,
  `handlePreviouslyUnavailableGVR`, `calculateRetryDelay`,
  `cleanupUnrequestedGVRs`, `isGVRRequested`, `scheduleRetries`,
  `retryDiscoveryAfterDelay`.
- Fields: `unavailableGVRsMu`, `unavailableGVRs`, `unavailableGVRsLastTry`.
- Constants: `crdDiscoveryRetryInterval`, `crdDiscoveryMaxRetries`,
  `maxRetryShift`.
- The call site `m.updateUnavailableGVRTracking(...)` in
  `ReconcileForRuleChange`.

Keep (still core to informer diffing): `compareGVRs`,
`buildDesiredGVRNamespaces`, `findAddedGVRs`, `hasNewNamespaces`,
`findRemovedGVRs`, `getNamespacesForGVRUnlocked`, and
`periodicReconcileInterval`.

Minor residual risk: a CRD object can appear in discovery slightly before its
resource endpoints are fully served. The trigger informer's `UpdateFunc` fires
on the CRD `Established`-condition transition, so the catalog re-refreshes
within seconds; the 30s periodic reconcile is the final backstop. No dedicated
retry timer is needed.

## Test impact

- `discovery_integration_test.go` — calls `FilterDiscoverableGVRs` directly and
  uses the `discoveryFilter` seam. Rework to assert on `ComputeRequestedGVRs`
  output directly, or remove if fully subsumed by `rule_gvr_resolver_test.go`.
- `rule_change_snapshot_test.go` — replace the `discoveryFilter` stub with an
  empty RuleStore (or empty catalog) to produce zero requested GVRs.
- `resource_filter_test.go` — deleted with item 2.
- `helpers_test.go` — drop the `singleConcrete` / `addGVR` tests (item 3).
- No test references the item 4 machinery, so its removal needs no test rework
  beyond confirming the suite still builds.

## Sequencing

1. Item 3 (`gvr.go` helpers) — smallest, self-contained.
2. Item 2 (`resource_filter.go`) — removes the no-op and its call sites.
3. Item 4 (unavailable-GVR machinery) — largest, but isolated; no test rework.
4. Item 1 (`discovery.go`) — last, because it carries the test-seam rework.

Each step should leave `task test` green on its own.

## Validation

`task fmt` → `task generate` → `task manifests` → `task vet` → `task lint` →
`task test` → `task test-e2e` (e2e sequentially; Docker required).

Expected net effect: one focused cleanup commit, several hundred lines removed,
no behaviour change. The quickstart e2e regression fix from `aa2e463` must stay
green throughout.
