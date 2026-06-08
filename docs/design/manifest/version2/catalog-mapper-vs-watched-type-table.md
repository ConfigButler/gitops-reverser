# Raw catalog, resolved type surface, GitTarget selection

> Status: design investigation, captured 2026-06-05 (draft 2 — reframed)
> Question raised by: should we sunset `internal/watch/catalog_mapper.go` and let
> `internal/watch/watched_type_table.go` serve as the single GVK/GVR abstraction,
> including for the CLI? In the end it is "just a set of types with some functions"
> — isn't the abstraction the same?
> Related:
> [../gvk-gvr-mapping-layer.md](../gvk-gvr-mapping-layer.md),
> [per-type-reconcile-and-streaming-tail.md](per-type-reconcile-and-streaming-tail.md),
> [`internal/mapping/mapper.go`](../../../../internal/mapping/mapper.go),
> [`internal/watch/catalog_mapper.go`](../../../../internal/watch/catalog_mapper.go),
> [`internal/watch/watched_type_table.go`](../../../../internal/watch/watched_type_table.go),
> [`internal/watch/rule_gvr_resolver.go`](../../../../internal/watch/rule_gvr_resolver.go),
> [`internal/watch/api_resource_catalog.go`](../../../../internal/watch/api_resource_catalog.go)

> **Revision note (draft 2).** This document originally framed the choice as
> "`CatalogMapper` vs `WatchedTypeTable`" and leaned on an "opposite directions"
> argument. That framing was too defensive and contained an error. Corrections,
> kept visible on purpose:
> - **Direction is demoted.** GVK→GVR vs GVR→GVK is not a deep difference — the
>   catalog indexes both ways from the same facts. The real difference is **scope,
>   lifecycle, and dependency footprint**.
> - **`TypeConflict` *is* `MappingAmbiguous`.** Draft 1 claimed the watched-type
>   table's conflict refusal had no equivalent in `mapping.Status`. Wrong: "one GVK
>   served by more than one resource" is exactly `MappingAmbiguous`
>   ([mapper.go:192-194](../../../../internal/mapping/mapper.go#L192-L194)) and
>   exactly the `len(accs) > 1` conflict
>   ([watched_type_table.go:219](../../../../internal/watch/watched_type_table.go#L219)).
>   Same condition, two names — which is the duplication this doc is really about.
> - **The earlier "route the table through `ResolveGVK`" recommendation was
>   imprecise.** The table starts from a rule-selected GVR and needs an exact
>   GVR→entry lookup, not the GVK→GVR mapper path. The shareable thing is the
>   *reduction*, not the GVK entry point.

## Verdict

The instinct is right: there is one abstraction here, and it should live in the
core. But it is **not** `WatchedTypeTable`, and it is not `CatalogMapper` either.
Both are *consumers* of a smaller thing that is currently smeared across three code
paths. The honest model is three layers, not two rivals:

```text
  APIResourceCatalog                 raw discovery facts + trust state
        |                            (byGVK / byGVR, ready, degraded, generation)
        v
  resolved type surface              the ONE abstraction to name in core:
  (mapping.Entry + one reduction)    "given a GVK or a GVR, the served+allowed
        |                             resolved type, or a refusal status"
        +--------------------+
        v                    v
  ResourceMapper        WatchedTypeTable
  (global, GVK->GVR,    (per-GitTarget, rule-selected subset
   multi-backend,        of the surface + watch lifecycle:
   manifest/CLI/test)    NamespaceOps, PendingRemovals, sweep)
```

So:

1. **Do not push `WatchedTypeTable` into the CLI.** Not because the data is
   different, but because the *object* carries GitTarget-specific operational
   policy (rule selection, namespace ops, removal grace, sweep) and a dependency on
   the watch manager. (§3)
2. **Do not sunset `CatalogMapper`.** It is the rule-agnostic, multi-backend
   GVK→GVR interface the offline tooling needs. (§3)
3. **Extract the resolved type surface into core** — one reduction, one status
   vocabulary, reusing `mapping.Entry` rather than inventing a new record. Both the
   mapper and the table read it. (§4)
4. **Ambiguous GVK→GVR is a hard, deterministic refusal**, not a supported mode.
   (§5) This matches your conviction and is already how both paths behave.

The right reframe of the whole question: stop asking "mapper or table?" and ask
"**raw catalog → resolved type surface → GitTarget selection**" — which makes the
shared abstraction small and the differences obvious.

## 1. You are right that the data is the same

At the level of facts, `CatalogMapper` and `WatchedTypeTable` traffic in identical
material:

```text
GVK <-> GVR   served?   allowed?   namespaced?   verbs?   preferred?   ambiguous?   degraded?
```

That shape already has a name in the codebase: `mapping.Entry`
([mapper.go:143-151](../../../../internal/mapping/mapper.go#L143-L151)), the
catalog-neutral fact record, and `APIResourceEntry` inside the catalog. A
`WatchedType` is `mapping.Entry` plus per-GitTarget scope; a `mapping.Result` is
`mapping.Entry` plus a status. The draft-1 "opposite directions" argument obscured
this: both are projections of the same `Entry`. So the question "isn't the
abstraction the same?" is **yes at the data layer** — and the fix is to *name that
data layer in core*, not to make one projection swallow the other.

## 2. What is genuinely not shared: lifecycle and dependency

The part of `WatchedTypeTable` that is not "just a set of types" is its
**operational lifecycle**, which a stateless resolver has no business carrying:

- `NamespaceOps` — per-namespace operation filters folded from a GitTarget's rules
  ([watched_type_table.go:88](../../../../internal/watch/watched_type_table.go#L88));
- `PendingRemovals` — a grace-timer hold so a discovery wobble never sweeps git
  ([watched_type_table.go:135](../../../../internal/watch/watched_type_table.go#L135));
- `ResolvedAt` + deliberate re-resolution triggers, and the role as the spine the
  per-type reconcile and untracking sweep iterate
  ([per-type-reconcile-and-streaming-tail.md:350](per-type-reconcile-and-streaming-tail.md#L350)).

These are decisions about *what one GitTarget does over time*, not about *what the
cluster serves*. They are correctly in `internal/watch`. The error would be letting
them leak into the core surface — or dragging them into the CLI.

## 3. Why neither projection moves into the other's job

**`WatchedTypeTable` → CLI: no.** Verified in the tree: `internal/git` and
`internal/manifestanalyzer` import `internal/mapping` but never `internal/watch`,
and `internal/mapping` never imports `internal/watch`. Building a `WatchedTypeTable`
needs a live rule set (`RuleStore`), a `RuleGVRResolver`, a `Manager`, and the
informer/grace machinery. A no-cluster CLI has none of that and does not want it.
The CLI also needs to classify GVKs **no rule selects** (the `unwatched` /
`unserved` taxonomy), and the table — being the *selected subset* — has no entry
for those. A subset cannot describe its own complement.

**`CatalogMapper` → deleted: no.** It is already a thin adapter (it delegates to the
shared reduction). Its value is the *interface* with four `MapperSource` backends —
`structure-only`, `static-snapshot`, `kubeconfig`, `live-catalog`
([mapper.go:71-84](../../../../internal/mapping/mapper.go#L71-L84)) — so the CLI,
tests, and the controller swap the discovery source without touching callers.
Deleting it deletes that polymorphism and re-couples the analyzer to the watch
manager.

The boundary the draft-1 doc defended is the right boundary. What it got wrong was
implying the *abstractions underneath* must also stay apart. They should not.

## 4. The one abstraction, stated minimally

Put the resolved type surface in core (`internal/mapping`, where `Entry` and
`ResolveGVK` already live) and reuse what exists:

- **Record: `mapping.Entry`.** Do not introduce a new `ResolvedType` /
  `TypeSurface` record — it would be a near-synonym of `Entry`. The pushback here is
  against adding vocabulary, not against the idea.
- **One reduction, two entry points.** `ResolveGVK(gvk, candidates, state)` already
  reduces a candidate `[]Entry` + trust state into a `Result`-or-refusal, and its
  reduction core (`partitionSubresources`, `allowedEntries`, ambiguity →
  status, [mapper.go:173-199](../../../../internal/mapping/mapper.go#L173-L199)) is
  **direction-agnostic**. Add a sibling exact-GVR reduction that feeds the *same*
  core, so:
  - `ForGVK(gvk)` → resolved GVR or refusal (today's mapper path);
  - `ForGVR(gvr)` → resolved entry or refusal (what the table's fold needs;
    GVR→GVK is single-valued in the catalog, so this is an exact lookup, not an
    ambiguity search — see [watched_type_table.go:262](../../../../internal/watch/watched_type_table.go#L262)).
- **One status vocabulary.** Collapse `ResolveMissReason`
  ([rule_gvr_resolver.go:32-43](../../../../internal/watch/rule_gvr_resolver.go#L32-L43))
  and `mapping.Status` ([mapper.go:120-137](../../../../internal/mapping/mapper.go#L120-L137))
  into one set of names for `served / unserved / ambiguous / disallowed / degraded /
  unavailable`. These are two hand-maintained spellings of the same outcomes today.

Then the consumers shrink to what they actually are:

- `CatalogMapper` = `ForGVK` over the live catalog (already nearly this).
- `RuleGVRResolver` / `buildWatchedTypeTable` = `ForGVR` over the live catalog, plus
  the GitTarget fold and lifecycle. The `lookupServedEntry`-takes-`Entries[0]`
  shortcut ([watched_type_table.go:263](../../../../internal/watch/watched_type_table.go#L263))
  becomes a call into the shared reduction, so the table and the mapper agree by
  construction.

This is the smallest version of "use the same abstraction": one record, one
reduction, two lookups, one status vocabulary — and the watch-specific lifecycle
stays out of it.

## 5. Ambiguous GVK→GVR is a refusal, not a mode

Endorsing the conviction directly: a cluster that serves one kind through more than
one resource is misconfigured, and GitOps Reverser should **not** pick a winner,
infer intent, or route around it. It refuses.

The good news is the product already behaves this way, in both paths — which is why
unifying is safe rather than a behavior change:

- the mapper returns `MappingAmbiguous` and resolves nothing
  ([mapper.go:192-194](../../../../internal/mapping/mapper.go#L192-L194));
- the table records a `TypeConflict` and does not watch the type
  ([watched_type_table.go:219-221](../../../../internal/watch/watched_type_table.go#L219-L221)).

So ambiguity is not a "valid operating mode" the surface must support with
tie-breaks; it is a single refusal status the surface *classifies*. The one
requirement — the critique's caveat, which is correct — is that the refusal stay
**observable**: a clear status and an edge-log/metric, not a silent gap. Fail-closed,
but loudly. The shared status vocabulary (§4) is what makes that one well-named
outcome instead of two.

## 6. What this explicitly rejects

- **Resolve CLI / analyzer GVKs through `WatchedTypeTable`** — it is the selected
  subset and drags `internal/watch` into a no-cluster tool. (§3)
- **Delete `CatalogMapper`** — deletes the multi-backend interface boundary. (§3)
- **Add a new `ResolvedType` / `TypeSurface` record** — `mapping.Entry` already is
  it; adding a synonym grows vocabulary instead of shrinking it. (§4)
- **Add reverse GVR→GVK mapping to the *mapper interface* for delete planning** —
  not needed; delete planning starts from the GitTarget folder's
  `ByResourceIdentity` inventory ([../gvk-gvr-mapping-layer.md](../gvk-gvr-mapping-layer.md)).
  A core `ForGVR` *reduction helper* (§4) is a different thing and is fine; the
  table needs it internally.
- **Treat ambiguity as a tie-breakable mode** — it is a refusal. (§5)

## 7. Recommendation, in priority order

1. **Keep `CatalogMapper` as the mapper interface/adapter.** (no change)
2. **Extract the resolved type surface into `internal/mapping`:** keep `Entry`, keep
   `ResolveGVK`, add a `ForGVR` reduction sharing the same reduction core, and one
   status vocabulary.
3. **Collapse `ResolveMissReason` into that vocabulary** so watch planning and
   manifest classification name outcomes identically.
4. **Make `buildWatchedTypeTable` / `RuleGVRResolver` resolve through the shared
   reduction**, removing the `Entries[0]` shortcut, so live mapper and table agree
   by construction.
5. **Keep all watch lifecycle** (`NamespaceOps`, `PendingRemovals`, sweep,
   re-resolution triggers) in `internal/watch`.
6. **Keep ambiguity as a hard, observable refusal**; do not build tie-breaking.

## 8. Sequencing

1. §7.2 + §7.4 together (surface + table reads it) — in-package first where it
   touches `internal/mapping`, then the watch wiring; both are unit-testable against
   the existing mapper and watched-type-table tests.
2. §7.3 (one vocabulary) as its own change — mechanical but cross-package, so a
   reviewable diff on its own.
3. None of this is on the M12+ per-type-reconcile critical path; the table already
   serves as that spine
   ([per-type-reconcile-and-streaming-tail.md:350](per-type-reconcile-and-streaming-tail.md#L350)).
   It is consolidation, not a new feature.

## 9. Unverified, on purpose

This is a design argument, not a proven refactor. The load-bearing assumption is
that the `ResolveGVK` reduction core is cleanly separable into a direction-agnostic
helper that a `ForGVR` lookup can reuse without disturbing the table's conflict
refusal or grace logic. That should be confirmed with a spike before §7.2 is
treated as settled. If the reduction turns out to entangle the GVK entry point,
the fallback is still valuable: share only the status vocabulary (§7.3) and keep two
thin lookups.

## References

- [gvk-gvr-mapping-layer.md](../gvk-gvr-mapping-layer.md) — the mapper contract and
  `MapperSource` backends.
- [per-type-reconcile-and-streaming-tail.md](per-type-reconcile-and-streaming-tail.md)
  — the table as the M12+ reconcile/sweep spine and the grace-timer hold.
- [`internal/mapping/mapper.go`](../../../../internal/mapping/mapper.go) — `Entry`,
  `ResolveGVK`, `mapping.Status`.
- [`internal/watch/watched_type_table.go`](../../../../internal/watch/watched_type_table.go)
  — `WatchedType`, `WatchedTypeTable`, `TypeConflict`, `PendingRemoval`.
- [`internal/watch/rule_gvr_resolver.go`](../../../../internal/watch/rule_gvr_resolver.go)
  — `RuleGVRResolver`, `ResolveMissReason` (the parallel vocabulary to collapse).
