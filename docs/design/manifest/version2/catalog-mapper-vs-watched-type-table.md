# CatalogMapper vs WatchedTypeTable: one abstraction, or two projections?

> Status: design investigation, captured 2026-06-05
> Question raised by: should we sunset `internal/watch/catalog_mapper.go` and let
> `internal/watch/watched_type_table.go` (which already carries both GVK and GVR
> and has well-tested hysteresis) serve as the single GVK/GVR abstraction —
> including for the CLI?
> Related:
> [../gvk-gvr-mapping-layer.md](../gvk-gvr-mapping-layer.md),
> [per-type-reconcile-and-streaming-tail.md](per-type-reconcile-and-streaming-tail.md),
> [`internal/mapping/mapper.go`](../../../../internal/mapping/mapper.go),
> [`internal/watch/catalog_mapper.go`](../../../../internal/watch/catalog_mapper.go),
> [`internal/watch/watched_type_table.go`](../../../../internal/watch/watched_type_table.go),
> [`internal/watch/rule_gvr_resolver.go`](../../../../internal/watch/rule_gvr_resolver.go),
> [`internal/watch/api_resource_catalog.go`](../../../../internal/watch/api_resource_catalog.go)

## Verdict first

The instinct — *"use the same abstraction"* — is correct, and worth acting on. But
the concrete proposal (retire `CatalogMapper`, resolve through `WatchedTypeTable`
everywhere including the CLI) is the wrong direction, and would regress an
architectural boundary the project deliberately built.

`CatalogMapper` and `WatchedTypeTable` are **not two implementations of one job**.
They are two *projections* of a single shared substrate — the
`APIResourceCatalog` — built for opposite questions, at different layers, with
different dependency footprints:

| | `CatalogMapper` (`mapping.ResourceMapper`) | `WatchedTypeTable` |
|---|---|---|
| Question it answers | "Given **any** GVK, what served GVR — and is that trustworthy?" | "What does **this GitTarget** watch, folded from its rules?" |
| Direction | GVK → GVR | rule shape → GVR → GVK (folded by GVK) |
| Scope | global, rule-agnostic | per-GitTarget, rule-scoped |
| Layer / package | `internal/mapping` interface; impl in `internal/watch` | `internal/watch` only |
| Dependencies | catalog only; **interface has no `internal/watch` dep** | rules + `RuleGVRResolver` + manager + informers + grace timers |
| Backends | 4 (`structure-only`, `static-snapshot`, `kubeconfig`, `live-catalog`) | 1 (live discovery + a live rule set) |
| Lifecycle state | none (stateless read) | `NamespaceOps`, `PendingRemovals` grace, `ResolvedAt` |
| Consumers | `internal/git`, `internal/manifestanalyzer`, CLI, tests | snapshot / informer / plan-hash / per-type reconcile (M12+) |

So: **do not collapse the mapper into the table.** Instead, collapse the *third
thing neither file shows you*: the project currently has **three** reduction code
paths and **two** status vocabularies over the one catalog. That duplication — not
the existence of two projections — is the real "same abstraction" debt. The
recommendation (§7) is to make the catalog's reduction the single authority and
build *both* the mapper and the table on top of it.

## 1. The question, stated precisely

The opened file pair invites a tempting simplification. `WatchedType` carries both
a `GVK` and a `GVR` in a documented 1:1 relationship
([watched_type_table.go:76](../../../../internal/watch/watched_type_table.go#L76)),
and `WatchedTypeTable` has matured into a resilient, hysteresis-aware structure
(`PendingRemovals` under a grace timer,
[watched_type_table.go:135](../../../../internal/watch/watched_type_table.go#L135)).
`CatalogMapper`, by contrast, looks thin — a stateless read over the catalog.

If the table already holds both coordinates and survives discovery wobble, why keep
a second type whose stated future role is "also be implemented in the CLI"
([gvk-gvr-mapping-layer.md → Kubeconfig Discovery Mapper](../gvk-gvr-mapping-layer.md))?

The answer turns on four properties the two files do not advertise on their face.

## 2. They run in opposite directions, and the mapper's direction is deliberate

`CatalogMapper.GVRForGVK` resolves **GVK → GVR** via `catalog.LookupGVK`
([catalog_mapper.go:84](../../../../internal/watch/catalog_mapper.go#L84)). This is
the *document* direction: a manifest declares `apiVersion`/`kind` (a GVK), and the
manifest store must learn the served resource identity to index and classify it
([store.go:401](../../../../internal/manifestanalyzer/store.go#L401)).

`buildWatchedTypeTable` runs the other way: it folds rule-resolved GVRs and then maps
**GVR → GVK** via `lookupServedEntry` → `catalog.LookupGVR`
([watched_type_table.go:263](../../../../internal/watch/watched_type_table.go#L263)).
This is the *watch* direction: rules name resources, the cluster watches GVRs, and
the GVK is recovered for manifest identity.

The mapping-layer design **deliberately excludes** the reverse (GVR → GVK) from the
mapper contract: *"Resource-to-manifest identity is not part of this mapper
contract"* ([gvk-gvr-mapping-layer.md](../gvk-gvr-mapping-layer.md), Interface §).
So the table needs exactly the direction the mapper intentionally does not expose.
"Works in both directions" is a property of a *folded result* (each `WatchedType`
happens to hold both coordinates), not of a general bidirectional resolver. Neither
type is a drop-in for the other's direction.

## 3. The table is rule-scoped; the analyzer must classify what no rule selects

This is the load-bearing objection. `WatchedTypeTable.Types` contains **only** the
types a GitTarget's rules resolved
([watched_type_resolver.go:572-615](../../../../internal/watch/watched_type_resolver.go#L572-L615)
fold rule selections into the table). It is, by construction, the *watched set*.

But the manifest layer's central job is to classify documents the watched set does
**not** contain — that is the entire `unwatched` / `unserved` / `ambiguous` /
`disallowed` taxonomy ([gvk-gvr-mapping-layer.md → Watched Classification](../gvk-gvr-mapping-layer.md)):

```text
document GVK -> mapper -> document GVR      (must work for ANY GVK in the repo)
GitTarget rules -> watched GVR set          (a separate, narrower question)
document GVR in watched set? -> tracked / unwatched / orphan
```

A `WatchedTypeTable` can answer step 2 (it *is* the watched set). It cannot answer
step 1 for a GVK no rule selects — and step 1 over arbitrary GVKs is precisely what
the manifest store and the CLI need. Asking the table to resolve an unwatched GVK is
asking a set to describe its own complement: it has no entry to return.

## 4. The dependency boundary the mapper protects is real and load-bearing

Verified in the tree today:

- `internal/manifestanalyzer` and `internal/git` import `internal/mapping` but
  **never** import `internal/watch`.
- `internal/mapping` **never** imports `internal/watch` (only a doc comment names
  it).

That clean cut is the explicit reason the interface lives in `internal/mapping`
while only the *live* implementation lives in `internal/watch`
([mapper.go:26-30](../../../../internal/mapping/mapper.go#L26-L30)): *"Keeping the
interface free of any controller or discovery dependency preserves the manifest
analyzer's no-cluster promise."*

`WatchedTypeTable` is the opposite of dependency-free. To build one you need a live
rule set (`RuleStore.SnapshotWatchRules`), a `RuleGVRResolver`, a `Manager`, and the
grace/informer machinery around it. Routing CLI or analyzer resolution through the
table would drag the entire watch manager — and a rule set the offline tool does not
have — into a tool whose whole point is to run without a cluster or a controller.
**This is the specific case raised in the question, and it is exactly where the table
substitution breaks hardest.** The CLI does not want the watched set of some
GitTarget; it wants "does this cluster's API surface serve this document's GVK?",
which is the mapper's rule-agnostic question with a swappable backend.

The mapper's four `MapperSource` backends
([mapper.go:71-84](../../../../internal/mapping/mapper.go#L71-L84)) —
`structure-only`, `static-snapshot`, `kubeconfig`, `live-catalog` — are that
swappability. The CLI plugs in `kubeconfig` or `static-snapshot`; the controller
plugs in `live-catalog` (`CatalogMapper`); tests plug in `static-snapshot`; the
no-cluster analyzer keeps `structure-only`. A single concrete `WatchedTypeTable`
has no equivalent polymorphism, and bolting one on would re-implement the mapper
interface under a new name.

## 5. The "hysteresis" is genuinely good — and the mapper already has its equivalent

`PendingRemovals` retains a still-selected type the catalog momentarily stopped
serving, under a grace timer, so a `7 → 0 → 7` discovery wobble never drives a
destructive git sweep
([watched_type_table.go:135](../../../../internal/watch/watched_type_table.go#L135),
[per-type-reconcile-and-streaming-tail.md:613](per-type-reconcile-and-streaming-tail.md)).
This is excellent and correct — but it is a **sweep-safety** mechanism. It protects
a *destructive, stateful, time-extended* action: deleting files from git when a
type appears to vanish.

The mapper faces no such action. Its fail-closed need is a *point-in-time read*:
"don't trust an absence I can't currently observe." It meets that need with the same
catalog trust state, surfaced as status rather than retained as a timer:
`MappingCatalogUnavailable`, `MappingDiscoveryDegraded`, and `Ready()/Degraded`
([mapper.go:120-137](../../../../internal/mapping/mapper.go#L120-L137),
[catalog_mapper.go:58-73](../../../../internal/watch/catalog_mapper.go#L58-L73)).
A caller that sees `Degraded` falls closed on its own action; a caller that needs to
*hold a resource alive over time* (the sweep) uses the grace timer.

So the two are not "one has hysteresis, the other lacks it." They are **the same
underlying catalog trust state expressed for two different action shapes** — a timer
for a destructive time-extended sweep, a status for an instantaneous read. Both are
correct; neither subsumes the other.

## 6. The real duplication: three reductions, two vocabularies, one catalog

Here is where the instinct pays off. Stepping back from the two files in question,
the same catalog is reduced to "served, allowed, unambiguous resource" in **three**
places:

1. `mapping.ResolveGVK` — the shared reduction behind `CatalogMapper` and
   `StaticSnapshotMapper`
   ([mapper.go:173](../../../../internal/mapping/mapper.go#L173)). Output vocabulary:
   `mapping.Status` (`Resolved` / `Unserved` / `Ambiguous` / `Disallowed` /
   `Subresource` / `CatalogUnavailable` / `DiscoveryDegraded` / `StructureOnly`).
2. `RuleGVRResolver.Resolve` — the watch-side rule reduction
   ([rule_gvr_resolver.go:63](../../../../internal/watch/rule_gvr_resolver.go#L63)).
   It applies its **own** scope/version/ambiguity/policy logic
   (`gvrsForCandidates` filters `Allowed`, `Subresource`, `Supports("list","watch")`,
   [rule_gvr_resolver.go:142-164](../../../../internal/watch/rule_gvr_resolver.go#L142-L164))
   and emits a **parallel** vocabulary: `ResolveMissReason` (`NotServed` /
   `Ambiguous` / `Disallowed` / `CatalogUnavailable` / `DiscoveryDegraded`).
3. `buildWatchedTypeTable` / `lookupServedEntry` — takes `lookup.Entries[0]`
   directly ([watched_type_table.go:263](../../../../internal/watch/watched_type_table.go#L263)),
   bypassing `ResolveGVK` entirely and trusting that `RuleGVRResolver` already
   filtered. Its own conflict refusal (one GVK claimed by >1 GVR →
   `TypeConflict`) is a *fourth* outcome category that `mapping.Status` has no name
   for.

`mapping.Status` and `ResolveMissReason` are near-identical taxonomies of catalog
trust outcomes maintained in two packages. *That* is the abstraction that is
duplicated — not the projections. Unifying it is the high-value move the question is
really pointing at.

## 7. Recommendation

**Keep both projections. Unify the resolution layer underneath them.** Concretely,
in priority order:

1. **Do not sunset `CatalogMapper`.** It is the cross-package, rule-agnostic,
   multi-backend GVK→GVR interface that `internal/git`, `internal/manifestanalyzer`,
   and the future CLI depend on. Retiring it re-couples the offline tooling to the
   watch manager and deletes the `kubeconfig` / `static-snapshot` / `structure-only`
   polymorphism the CLI and tests need. This directly answers the question: the CLI
   case is the *strongest* reason to keep the mapper, not a weak one.

2. **Make `buildWatchedTypeTable` resolve through the shared reduction.** Today its
   GVR→GVK leg bypasses `mapping.ResolveGVK`. Route the table's per-type resolution
   through the same reduction (or a shared catalog helper) so the live catalog and
   the table agree on policy, subresource, and ambiguity handling by construction,
   and the `lookupServedEntry`-takes-`[0]` shortcut disappears. This is the literal
   "same abstraction" win for the two files in the question — the table built *on
   top of* the resolver, not beside it.

3. **Collapse `ResolveMissReason` and `mapping.Status` into one vocabulary.** Have
   `RuleGVRResolver` express its outcomes in the `mapping` taxonomy (or a shared
   enum both import) so there is exactly one set of names for "served / unserved /
   ambiguous / disallowed / degraded / unavailable" across watch planning and
   manifest classification. This removes the genuine duplication §6 identifies and
   keeps the two surfaces from drifting.

4. **Optional, later: add a reverse lookup to the contract only if a second consumer
   needs it.** The mapper deliberately omits GVR→GVK. If/when a non-watch consumer
   needs it, add it to the catalog reduction (where `LookupGVR` already lives), not
   to `WatchedTypeTable`. Until then, the table's in-package GVR→GVK leg is fine.

The mental model to adopt:

```text
                 APIResourceCatalog            <- the one source of truth
                 (byGVK / byGVR, trust state, generation, degraded GVs)
                          |
                 one reduction vocabulary       <- the abstraction to unify (§7.2, §7.3)
                 (served? allowed? ambiguous? degraded? unavailable?)
                 /                            \
   ResourceMapper (GVK->GVR)            WatchedTypeTable (rule -> GVR -> GVK)
   global, multi-backend,              per-GitTarget, rule-scoped,
   document/CLI/test facing            sweep/informer/reconcile spine
```

Two projections, one substrate, one reduction. That is the same abstraction used
twice — which is the right shape — rather than one type pressed into two
incompatible jobs.

## 8. What this explicitly rejects, and why

- **"Resolve CLI/analyzer GVKs through `WatchedTypeTable`."** Rejected: the table is
  the watched *set*; the CLI must classify GVKs outside any set, and the table would
  drag `internal/watch` + a live rule set into a no-cluster tool (§3, §4).
- **"Delete `CatalogMapper`, keep only the table."** Rejected: deletes the
  interface boundary and the four-backend polymorphism; re-couples
  `internal/manifestanalyzer` and `internal/git` to `internal/watch` (§4).
- **"Give `WatchedTypeTable` the mapper's status surface so it can stand alone."**
  Rejected as a *substitution* — that is just re-implementing `ResourceMapper` under
  a new name. (Sharing one vocabulary, §7.3, is the supported version of this
  instinct.)

## 9. Suggested sequencing

1. Land §7.2 (table resolves through the shared reduction) — small, in-package,
   removes the `Entries[0]` shortcut, fully unit-testable against the existing
   watched-type-table tests.
2. Land §7.3 (one trust-outcome vocabulary) — mechanical but cross-package; do it as
   its own change so the diff is reviewable.
3. Leave §7.4 unbuilt until a concrete second consumer of GVR→GVK appears outside
   `internal/watch`.

None of this is on the critical path for the M12+ per-type reconcile — the table
already serves as that spine
([per-type-reconcile-and-streaming-tail.md:350](per-type-reconcile-and-streaming-tail.md#L350)).
It is a consolidation that makes the per-type work rest on one resolution authority
instead of three.

## References

- [gvk-gvr-mapping-layer.md](../gvk-gvr-mapping-layer.md) — the mapper contract,
  `MapperSource` backends, and the watched-classification two-step.
- [per-type-reconcile-and-streaming-tail.md](per-type-reconcile-and-streaming-tail.md)
  — the table as the M12+ reconcile/sweep/visibility spine, and the grace-timer hold.
- [`internal/mapping/mapper.go`](../../../../internal/mapping/mapper.go) —
  `ResourceMapper`, `ResolveGVK`, `mapping.Status`.
- [`internal/watch/watched_type_table.go`](../../../../internal/watch/watched_type_table.go)
  — `WatchedType`, `WatchedTypeTable`, `PendingRemoval`, `buildWatchedTypeTable`.
- [`internal/watch/rule_gvr_resolver.go`](../../../../internal/watch/rule_gvr_resolver.go)
  — `RuleGVRResolver`, `ResolveMissReason` (the parallel vocabulary).
