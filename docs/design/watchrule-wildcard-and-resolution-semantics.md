# WatchRule Wildcard & Resolution Semantics

How GitOps Reverser turns a `WatchRule`/`ClusterWatchRule` resource selector into
a concrete set of watchable GVRs, what it deliberately refuses, and why there is
no "watch everything" rule.

Source of truth: `RuleGVRResolver` in
[rule_gvr_resolver.go](../../internal/watch/rule_gvr_resolver.go), backed by the
[APIResourceCatalog](../../internal/watch/api_resource_catalog.go). See also
[watchrule-gvr-resolution-plan.md](../finished/watchrule-gvr-resolution-plan.md).

> **Note:** this documents *current* behavior. The CRD field docs promise
> admission-webhook-style wildcard semantics that this resolver does not yet
> implement — see
> [watchrule-wildcard-support-plan.md](watchrule-wildcard-support-plan.md) for
> that finding and the plan to resolve it.

## Two directions, treated differently

Resource selectors are used in two distinct places, and they handle wildcards
**oppositely**:

| Direction | Code | Question it answers | `*` behavior |
|---|---|---|---|
| **Planning / resolution** | `RuleGVRResolver.Resolve(...)` ([rule_gvr_resolver.go:67](../../internal/watch/rule_gvr_resolver.go#L67)) | "Which concrete GVRs should I set up informers for and snapshot?" | **Refused** (`Wildcard*` miss). |
| **Matching** | `matchesAPIGroups`/`matchesResources` ([manager.go:480-513](../../internal/watch/manager.go#L480-L513)) | "Does this concrete incoming event's GVR match a rule?" | **Honored** (`"*"` matches anything). |

The planning direction is the only one that creates informers and drives
snapshots (`gvrFromCompiledRule → Resolve`,
[gvr.go:79-92](../../internal/watch/gvr.go#L79-L92)). The matching direction only
runs against a GVR an informer **already delivered**
([collectWatchRuleNamespaces](../../internal/watch/manager.go#L416-L424)).

**Consequence:** a `*` rule with no concrete companion rule sets up zero
informers and therefore watches nothing. The matching-side `*` only ever matches
events some *other* concrete rule already brought in. A lone `resources: ["*"]`
rule looks like "watch everything" but is effectively a no-op (it logs a
`WildcardResource` miss at `V(1)`).

## The preflight gate

`Resolve` calls `resolveResource` per resource name, which first runs
`preflightMisses` ([rule_gvr_resolver.go:108-131](../../internal/watch/rule_gvr_resolver.go#L108-L131)).
This gate short-circuits **before the catalog is ever queried**:

| Condition | Result | Meaning |
|---|---|---|
| `resource == ""` | dropped, no miss | Empty resource entry is ignored. |
| `resource == "*"` | `WildcardResource` | `*` in `resources` — expansion unsupported. |
| `hasWildcard(groups)` | `WildcardGroup` | `*` in `apiGroups` — expansion unsupported. |
| `strings.Contains(resource, "/")` | `NotServed` | Subresource (e.g. `pods/status`) planning unsupported. |
| catalog nil or not `Ready()` | `CatalogUnavailable` | Discovery hasn't populated a catalog yet. |
| otherwise | proceed | Look the resource up in the catalog. |

`WildcardResource` vs `WildcardGroup` is purely *which axis* held the `*`.
Resource is checked first, so `apiGroups:["*"], resources:["*"]` reports
`WildcardResource`.

## What the resolver *does* expand from the catalog

Two narrower, **bounded** expansions are supported — neither is an open wildcard:

- **Empty `apiGroups`** (not `*`): `resourceCandidates` looks the resource name
  up across *all* groups via
  [`entriesForResource`](../../internal/watch/api_resource_catalog.go#L286-L290),
  but [`ambiguityMiss`](../../internal/watch/rule_gvr_resolver.go#L141-L152)
  refuses with `Ambiguous` if more than one group serves that name. So it is
  "find this one kind without naming its group," requiring a unique answer — not
  a fan-out.
- **Empty or `*` `apiVersions`**:
  [`choosePreferredVersions`](../../internal/watch/rule_gvr_resolver.go#L212-L235)
  collapses to the single **preferred** version per group-resource.

Note the asymmetry: a version wildcard *is* accepted (it resolves to exactly one
version), but group/resource wildcards are refused. The dividing line is
**boundedness**: does the selector resolve to a fixed, knowable set?

## There is no "watch everything" rule

By design, no single rule can watch the whole cluster:

- `resources: ["*"]` → `WildcardResource`, refused.
- `apiGroups: ["*"]` → `WildcardGroup`, refused.
- Empty `apiGroups` → resolves *one* kind across groups, and only if unambiguous.
- Empty `resources` → the `Resolve` loop iterates `resources`, so it yields zero
  GVRs and watches nothing.

The only way to watch broadly is to **enumerate each concrete resource kind
explicitly**. That list is static: a newly installed CRD is not picked up until a
rule names it (or a wildcard pattern would match it — which is exactly what is
refused). Dynamic "all current and future kinds" is intentionally not
expressible.

## Why this is intentional

1. **Bounded vs unbounded.** A version wildcard collapses to one preferred
   version. A group/resource wildcard expands to an open-ended set whose
   membership depends on what is installed right now. The resolver only expands
   selectors that resolve to a fixed set.
2. **Plan stability.** A wildcard plan's membership would change every time *any*
   CRD is installed or removed, churning the per-GitTarget effective-watch-plan
   hash on unrelated cluster activity — the same spurious re-snapshot problem
   described in
   [rule-set-snapshot-discovery-lag-fix.md](rule-set-snapshot-discovery-lag-fix.md),
   but permanent and amplified. Refusing wildcards keeps watch plans
   deterministic.
3. **Snapshot safety.** Snapshotting `*` would commit the entire cluster (minus
   policy-excluded kinds) to git on every rule-change snapshot — heavy,
   surprising, and far more exposed to the partial-view aborts that
   `RequestClusterState` enforces ([manager.go:596-601](../../internal/watch/manager.go#L596-L601)).
4. **A terse alternative already exists.** Empty `apiGroups` lets a rule name a
   resource without its group as long as the name is unambiguous, so users are
   not pushed toward wildcards just to stay concise.

## Relationship to the snapshot miss classification

`blockingSnapshotMisses` ([manager.go:657-669](../../internal/watch/manager.go#L657-L669))
groups `WildcardGroup`/`WildcardResource` with `CatalogUnavailable`/
`DiscoveryDegraded` as "blocking" — all four prevent emitting a snapshot. But the
two families differ in nature:

- **Transient-blocking** (`CatalogUnavailable`, `DiscoveryDegraded`): the answer
  changes once discovery recovers. Genuinely "ask again later."
- **Stable-blocking** (`WildcardGroup`, `WildcardResource`): a fixed property of
  the rule text. Retrying never changes it; it is effectively a configuration
  error to surface to the user.

This distinction matters for the state model in
[rule-set-snapshot-discovery-lag-fix.md](rule-set-snapshot-discovery-lag-fix.md):
only the transient family should drive "retry next cycle"; the wildcard family
will not resolve on its own.
