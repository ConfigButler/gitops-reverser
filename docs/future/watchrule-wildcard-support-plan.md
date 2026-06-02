# WatchRule Wildcard Support — Finding & Plan

Status: **proposed** (not started)

> **Superseded framing:** the finding below stands, but the recommended way to
> resolve it is the unified model in
> [watch-and-catalog-architecture.md](../design/watch-and-catalog-architecture.md). Treat
> this document as the focused finding + a tactical (Phase-0-first) fallback if
> the larger refactor is deferred.

## Finding: documented behavior contradicts implemented behavior

The `WatchRule`/`ClusterWatchRule` CRD field docs promise Kubernetes
admission-webhook / RBAC wildcard semantics, but the GVR resolver refuses them.

The field comments in [watchrule_types.go:71-118](../../api/v1alpha1/watchrule_types.go#L71-L118)
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
  ([manager.go:596-628](../../internal/watch/manager.go#L596-L628)) — correct
  today because a missing list looks like deletions. Across hundreds of GVRs the
  probability that *one* fails is much higher, so a wildcard target could rarely
  produce a complete snapshot. Likely needs a per-GVR resilience strategy that
  still distinguishes "failed to list" from "genuinely empty" without mirroring
  spurious deletions. This is the hardest correctness question.
- **Informer scale / memory.** One informer per GVR across the whole surface is a
  real resource cost; worth a soft cap or opt-in guardrail.

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

1. **Phase 0 — stop the silent lie (smallest, ship independently).** Either make
   the docs match reality (state that `*`/subresources are rejected, `[]`
   resolves uniquely/preferred) *or* add admission validation that rejects `*`
   with a clear message. This removes the "applies cleanly, watches nothing"
   trap regardless of whether full support lands.
2. **Phase 1 — resolver expansion (A + B + C).** Implement `*` enumeration with
   the all-vs-unique split, drop the subresource promise, surface expansion in
   status (F, partial). Gated behind the existing reconcile machinery; no
   snapshot changes yet. Wildcard targets now watch live events correctly.
3. **Phase 2 — snapshot robustness (D).** Decide and implement per-GVR list
   resilience so a wildcard target can take a complete, safe cluster snapshot.
   This is the gating item for declaring wildcard support "done".
4. **Phase 3 — guardrails (D scale, E debounce).** Informer-count cap/observability
   and optional CRD-burst debounce if churn proves costly in practice.

## Open decisions

- Phase 0: fix docs, or add rejecting validation, or both? (Both is cleanest.)
- D: what is the correct "one GVR failed to list" behavior for a wildcard target
  — skip that GVR for this cycle (risk: looks like deletions) vs abort the whole
  target (risk: wildcard targets rarely snapshot)? This needs its own mini-design.
- Should "watch everything" remain policy-filtered by the default denylist, or
  should wildcard rules be able to opt into the excluded noisy kinds?
- Is the plan-hash churn for wildcard targets acceptable for v1, or is debounce a
  prerequisite?
