# WatchRule Wildcard Support — Finding & Plan

Status: **implemented** — Phases 0–1 shipped; **not the full happy flow** (Phase 2
snapshot robustness and Phase 3 guardrails are still outstanding — see
[Implementation status](#implementation-status)).

> **Superseded framing:** the finding below stands, but the recommended way to
> resolve it is the unified model in
> [watch-and-catalog-architecture.md](../design/watch-and-catalog-architecture.md). Treat
> this document as the focused finding + a tactical (Phase-0-first) fallback if
> the larger refactor is deferred.

## Implementation status

Wildcards resolve and watch again. The resolver expands `apiGroups: ["*"]`,
`apiVersions: ["*"]`, and `resources: ["*"]` from the API-resource catalog and
feeds the expanded GVR set through both the informer-planning and snapshot paths;
the rule status reports `"wildcard expanded to N GVRs"`. Admission now rejects
subresource entries (`pods/log`, `pods/*`) via an `items` pattern, and the CRD
field docs were corrected to match. Covered by unit tests in the resolver and
manager-snapshot suites and a manager-labeled e2e smoke spec.

**Done:** Phase 0 (docs + admission validation), Phase 1 (resolver expansion
A + B + C, status surfacing F-partial).

**Not done (the "not fully happy flow" caveat):**

- **Phase 2 / item D — snapshot robustness.** *Partially decided (see
  [Item D decision](#item-d-decision-notfound-skips-everything-else-aborts)
  below).* A `list` that returns **NotFound** (the type is no longer served) now
  skips that GVR instead of aborting the whole snapshot; every other `list` error
  still aborts (pinned by `TestSnapshotWildcardResourceAbortsOnAnyListError` and
  `TestSnapshotAbortsOnListError`). This closes the CRD-churn race that wedged
  wildcard targets. The remaining, larger half of D — resilience to *transient*
  failures on *served* types at hundreds-of-GVR scale (retry/partial-snapshot
  strategy) — is still open and is the gating item for declaring wildcard support
  truly "done".
- **Phase 3 — guardrails.** No informer-count cap/observability and no CRD-burst
  debounce yet.
- **HA.** Single-pod only by design; the branch-shard prerequisite in
  [ha-gittarget-distribution-plan.md](ha-gittarget-distribution-plan.md) must
  land before wildcard watches become a multi-pod feature.

The finding and difficulty assessment below are retained as the rationale and the
roadmap for the remaining phases; they describe the pre-implementation state.

## Finding: documented behavior contradicts implemented behavior

The `WatchRule`/`ClusterWatchRule` CRD field docs promise Kubernetes
admission-webhook / RBAC wildcard semantics, but the GVR resolver refuses them.

The field comments in [watchrule_types.go:71-118](../../api/v1alpha2/watchrule_types.go#L71-L118)
(shipped as the CRD OpenAPI schema → `kubectl explain`) state:

- "All fields except Resources are optional and default to matching all when not
  specified."
- `apiGroups`: "Wildcards supported: `*` matches all groups", "`["*"]` or `[]`
  matches all groups".
- `apiVersions`: "If empty, matches all versions", "`*` matches all versions".
- `resources`: "Wildcard semantics follow Kubernetes admission webhook patterns:
  `*` matches all resources", "`pods/*` matches all pod subresources",
  "`pods/log` matches specific subresource".

The resolver's preflight gate
([rule_gvr_resolver.go:108-131](../../internal/watch/rule_gvr_resolver.go#L108-L131))
does the opposite:

| User writes | Field doc promises | Resolver actually does |
|---|---|---|
| `resources: ["*"]` | all resources | `WildcardResource` miss — **refused** |
| `apiGroups: ["*"]` | all groups | `WildcardGroup` miss — **refused** |
| `apiVersions: []` / `["*"]` | all versions | single **preferred** version only |
| `apiGroups: []` (ambiguous resource) | all groups | `Ambiguous` miss — **refused** |
| `resources: ["pods/log"]`, `["pods/*"]` | subresource match | `NotServed` — **refused** |

There is **no admission validation** rejecting these (only `Operations` has an
enum), so a user can `kubectl apply` a `resources: ["*"]` rule, get no error, and
**silently watch nothing** — only a `V(1)` resolve-miss log. See
[watchrule-wildcard-and-resolution-semantics.md](../design/watchrule-wildcard-and-resolution-semantics.md)
for the full current semantics.

### Why it matters

The field docs were written in the Kubernetes admission-webhook / RBAC /
audit-policy idiom (the comment says so verbatim). **That is what users will
expect** — `*` and "empty = all" are universal in those APIs. The current
behavior is a silent, surprising divergence from a well-known contract.

### Why it diverges today

RBAC and audit policy evaluate selectors **lazily, per request**: a concrete GVR
already exists on the incoming request and the rule is just a predicate. GitOps
Reverser must **eagerly materialize** the watch set — an informer per GVR plus
authoritative `list` snapshots — so it needs concrete GVRs *before* any event
exists. The resolver was added to fix a concrete expansion bug
([watchrule-gvr-resolution-plan.md](../finished/watchrule-gvr-resolution-plan.md)) and, in
making expansion safe, narrowed the contract without updating the docs. Notably
the *matching* layer ([manager.go:480-513](../../internal/watch/manager.go#L480-L513))
*does* honor `*` lazily like RBAC — but it only ever runs against GVRs an
informer already delivers, so it cannot rescue a wildcard rule that planned
nothing.

## What already exists (good news for difficulty)

The hard part of dynamic wildcard support — reacting to a changing API surface —
is **already built and running**:

- API-surface trigger informers watch CRDs and APIServices and coalesce
  add/update/delete into `catalogRefreshCh`
  ([manager_catalog.go:278-303](../../internal/watch/manager_catalog.go#L278-L303)).
- The manager loop re-runs `ReconcileForRuleChange` on that signal and every 30s
  ([manager.go:218-245](../../internal/watch/manager.go#L218-L245)).
- `ReconcileForRuleChange` re-resolves desired GVRs, diffs added/removed against
  active informers, and starts/stops informers live
  ([manager.go:751-794](../../internal/watch/manager.go#L751-L794)).
- Per-GitTarget snapshot isolation already scopes which targets re-snapshot on a
  plan change
  ([gittarget-isolation-on-rule-change.md](../finished/gittarget-isolation-on-rule-change.md),
  [rule-set-snapshot-discovery-lag-fix.md](../finished/rule-set-snapshot-discovery-lag-fix.md)).

So "we'd have to redo reconciles when a GVR is added" is **already the steady
state** for explicit rules. Wildcard support mostly means letting the resolver
emit a larger, catalog-derived set; the reconcile/informer/snapshot plumbing
downstream is unchanged. The churn cost is real but **scoped to the targets that
actually use a wildcard** — explicit-rule targets keep full isolation.

The default watch policy is a small **denylist**
([resource_policy.go:30-47](../../internal/watch/resource_policy.go#L30-L47)):
pods, events, endpoints, endpointslices, leases, controllerrevisions, jobs,
cronjobs, flowcontrol. So "watch everything" really means "every served, allowed,
listable, watchable resource minus ~12 noisy kinds" — still large (every CRD
kind in the cluster), which drives the scale concerns below.

## Difficulty assessment

Ordered low → high effort/risk.

### A. Resolver wildcard expansion — **moderate, low risk**

Replace the preflight refusals with catalog enumeration:

- `apiGroups: ["*"]` → enumerate all groups serving the named resource(s).
- `resources: ["*"]` → enumerate all resources in the named group(s) (or all
  groups if combined with group `*`).
- Both `*` → every entry in the catalog.

The catalog already holds the data (`byGVR`, `byGroupVer`, `byResource`); add
enumeration accessors (e.g. `allEntries()`, `entriesForGroup(group)`). Reuse the
existing `Allowed` / `Supports("list","watch")` / scope filters in
`gvrsForCandidates` so policy and listability still apply. Bounded, mechanical.

### B. Empty-vs-wildcard semantics — **small, but a real decision**

Today empty `apiGroups` demands a *unique* group (`Ambiguous` otherwise). A
wildcard must mean "**all** matches", not "exactly one". Decision: keep `[]` =
"resolve to the single served group, else error" (current, safe) and make `["*"]`
= "all served groups". This preserves a useful strict mode while honoring the
documented wildcard. Same split for versions: `[]` = preferred (current),
`["*"]` = all served versions (new).

### C. Subresources — **small; mostly a docs fix, not a feature**

`pods/log` / `pods/*` cannot be `list`/`watch`-ed as a snapshot surface; the
list/watch+mirror model can't represent them. Recommend **dropping the
subresource promise from the field docs** rather than implementing it, and
keeping the `NotServed` classification. (Subresources still flow as audit events
through a different path; this is only about WatchRule planning.)

### D. Snapshot robustness at wildcard scale — **moderate→large, the real risk**

A wildcard target lists *every* allowed GVR. Two existing behaviors get stressed:

- **Partial-view abort.** `RequestClusterState` aborts the whole target snapshot
  if any single GVR `list` fails
  ([manager.go](../../internal/watch/manager.go)) — correct for a *served* type
  that we could not read, because a missing list looks like deletions. The
  **NotFound** case (type no longer served) is now split out and skipped (see
  [Item D decision](#item-d-decision-notfound-skips-everything-else-aborts)).
  Across hundreds of GVRs the probability that *one served type* fails transiently
  is still higher, so a wildcard target could rarely produce a complete snapshot.
  Resolving that needs a per-GVR resilience strategy (retry/backoff or an explicit
  partial-snapshot mode) that still distinguishes "failed to list a served type"
  from "genuinely empty" without mirroring spurious deletions. This is the hardest
  correctness question and the still-open remainder of D.
- **Informer scale / memory.** One informer per GVR across the whole surface is a
  real resource cost; worth a soft cap or opt-in guardrail.

#### Item D decision: NotFound skips, everything else aborts

**Decided (2026-06-04).** The "one GVR failed to list" question splits cleanly on
the *kind* of failure:

- **`NotFound` → skip that GVR, keep snapshotting.** A `list` returning NotFound
  means the type is no longer served — its CRD or aggregated APIService was
  removed between catalog resolution and the list. This is the **same condition
  the resolver already treats as non-blocking** (`ResolveMissNotServed` is omitted
  from `blockingSnapshotMisses`): a type absent from the catalog at resolve time is
  silently skipped today. A type that vanishes in the narrow resolve→list race is
  the identical situation discovered one step later, so it must be handled
  identically. A no-longer-served type has no live resources to mistake for
  deletions, so skipping it cannot mirror a spurious delete.
- **Any other error → abort the whole snapshot (unchanged).** A timeout, 5xx, or
  connection failure on a type that *is* served means we could not read resources
  that may well exist. Treating that as "empty" would wipe their mirrored files,
  so the partial-view abort stays.

This is provenance-independent — it applies whether the GVR came from a wildcard
or a named rule — because the resolver's existing `NotServed` skip is already
provenance-independent. It needs no per-GVR "is this wildcard?" tracking. It is
deliberately the *smaller* half of D: it fixes the CRD-churn race (a deleted CRD
elsewhere in the cluster no longer wedges every wildcard target) without yet
solving resilience to transient failures on served types at scale, which remains
open above.

Implemented in `GetClusterStateForGitDest`
([manager.go](../../internal/watch/manager.go)); pinned by
`TestSnapshotSkipsTypeNoLongerServed` (skip path) alongside the unchanged
`TestSnapshotAbortsOnListError` / `TestSnapshotWildcardResourceAbortsOnAnyListError`
(abort path). Origin: this race was the dominant cause of the red `E2E (full)` runs
analysed in [wildcard-ci-failure-findings.md](../wildcard-ci-failure-findings.md).

### E. Plan-hash churn — **small mechanically, a policy choice**

A wildcard target's effective-watch-plan hash changes on every CRD install/delete
→ it re-snapshots each time. Per-target isolation already contains this to
wildcard targets. Options: accept it (simplest; the user opted into "everything"),
or debounce/coalesce CRD bursts. The existing `catalogRefreshCh` already coalesces
to depth 1, so bursts are partly absorbed. **Probably acceptable as-is for now.**

### F. Status & validation surface — **small**

- `ResolveWatchRuleResources` / `ResolveClusterWatchRuleResources`
  ([manager_catalog.go:230-265](../../internal/watch/manager_catalog.go#L230-L265))
  should report "wildcard expanded to N GVRs" in status so users see the effect.
- Correct the CRD field docs (requires `task generate` + `task manifests`).
- Optionally add a `DiscoveryDegraded`-aware guard: a wildcard expanded while a
  relevant group's discovery is degraded is an incomplete plan and should block
  (consistent with current snapshot-safety rules).

## Proposed phased plan

1. ✅ **Phase 0 — stop the silent lie (smallest, ship independently).** Done: the
   field docs now match reality (subresources rejected, `[]` resolves
   uniquely/preferred) *and* admission validation rejects subresource entries.
2. ✅ **Phase 1 — resolver expansion (A + B + C).** Done: `*` enumeration with the
   all-vs-unique split, subresource promise dropped, expansion surfaced in status
   (F, partial). Gated behind the existing reconcile machinery; no snapshot
   changes. Wildcard targets watch live events correctly.
3. ⬜ **Phase 2 — snapshot robustness (D).** Decide and implement per-GVR list
   resilience so a wildcard target can take a complete, safe cluster snapshot.
   This is the gating item for declaring wildcard support "done".
4. ⬜ **Phase 3 — guardrails (D scale, E debounce).** Informer-count cap/observability
   and optional CRD-burst debounce if churn proves costly in practice.

## Open decisions

- Phase 0: fix docs, or add rejecting validation, or both? (Both is cleanest.)
- D: ~~what is the correct "one GVR failed to list" behavior for a wildcard target
  — skip that GVR for this cycle (risk: looks like deletions) vs abort the whole
  target (risk: wildcard targets rarely snapshot)?~~ **Split and partly decided:**
  a **NotFound** (type no longer served) skips that GVR; every other error still
  aborts — see
  [Item D decision](#item-d-decision-notfound-skips-everything-else-aborts). The
  transient-failure-on-a-served-type half (retry vs partial snapshot at scale) is
  still open.
- Should "watch everything" remain policy-filtered by the default denylist, or
  should wildcard rules be able to opt into the excluded noisy kinds?
- Is the plan-hash churn for wildcard targets acceptable for v1, or is debounce a
  prerequisite?
