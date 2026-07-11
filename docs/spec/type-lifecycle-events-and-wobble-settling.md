# Type lifecycle events, the wobble-settle phase, and consolidation

> **spec** — current behaviour. The code depends on this document; change one, change the other. Index: [`../INDEX.md`](../INDEX.md)

> Status: design direction, captured 2026-06-09. **Implemented 2026-06-09 (M12 first
> slices):** Proposals 1–2 in [internal/typeset/lifecycle.go](../../internal/typeset/lifecycle.go)
> (`LifecycleEvent`/`Observer`/`Subscribe`, `SettleWindow`, settle + flap coalescing in
> `Registry.Update`); M12 per-type reconcile/sweep via
> `manifestanalyzer.BuildScopedPlan`, the `ScopeGVR` resync request, the registry
> subscription + drain goroutine in internal/watch/type_lifecycle.go,
> and `EventRouter.EmitTypeReconcile/SweepForGitDest`. Proposal 3's consolidation is the
> shared `Manager.typeWobbling` predicate. The per-type path is gated on `SnapshotSynced`,
> so bootstrap is unchanged; `Unknown` granularity and bootstrap-decoupling remain open.
> Origin: the per-type reconcile track ([dream.md](type-followability.md),
> `per-type-reconcile-and-streaming-tail.md`)
> and the [e2e flakiness findings](e2e-serial-registry.md).
> Related:
> [discovery-catalog-typeset-boundary.md](type-followability.md),
> [type-followability.md](type-followability.md),
> [catalog-mapper-vs-watched-type-table.md](type-followability.md),
> [api-catalog-watched-type-architecture.md](type-followability.md).

## Why now

Two things came together. First, **M10 landed**: the typeset registry is now the
single decision surface for "what does this GitTarget follow, and is each type
usable right now?", projected per-GitTarget by the resident `WatchedTypeTable`.
That central, checking source was the right move and this design keeps it.

Second, **M12 — per-type reconcile + per-type sweep — is the next real product
move**, and tracing the e2e flakiness made its hard edge concrete: the unit of
work becomes a *type*, and a type's health **changes over time** (a CRD installs,
a group's discovery wobbles, a CRD is deleted). A naive per-type reconcile that
acts on whatever verdict it reads *at that instant* would:

- fire its first reconcile mid-wobble and then immediately have to undo it, or
- treat a transient unserved blink as a removal and sweep the type's KRM.

The registry already refuses to do the destructive half of that (the
`RemovalGrace` + `VerdictRetained` fail-closed). What it does **not** yet give us
is a clean, single signal of *transitions* — "this type just became unhealthy",
"this type became healthy again", "this type is now really gone" — that a per-type
reconcile can subscribe to instead of polling and re-deriving. And the "is this
type usable?" judgment is currently re-derived in **three** layers, which is the
simplification this document is also chasing.

## What we already have (the foundation — keep it)

The typeset registry (`internal/typeset/registry.go`,
[type-followability.md](type-followability.md)) is the single owner of type
identity and the live set. Per known type it computes one `TypeRecord` with a
`Verdict`:

| Verdict | Meaning |
|---|---|
| `Followable` | every check passed; in the live set |
| `Retained` | a *transient* check (served/trusted) fails now, but the type is held through `RemovalGrace` |
| `Refused` | a *permanent* check failed; never followed |
| `Unknown` | the registry could not assess it (catalog unavailable) |

…with a single machine-readable `Reason` for a failure (`not-served`,
`discovery-degraded`, `absence-expired`, `gvk-not-unique`, `missing-verb`, …), a
fixed `RemovalGrace` (60s — product safety, not tuning), a `Generation` (the scan
the records came from), and a `Revision` — a *change-of-decision* counter that
bumps whenever followable membership or the generation moves.

M10 projects this per-GitTarget into the `WatchedTypeTable`, re-resolved on a rules
fingerprint or a catalog `Generation()` change, and already holds a type the
catalog momentarily stops serving rather than dropping it (the pending-removal
fail-closed in `resolveSnapshotGVRs`). GVK↔GVR is treated as **1:1** (a
multi-resource GVK is a refused `TypeConflict`), giving the per-type work an
unambiguous key.

None of that changes here. This design adds a transition layer on top of it and
then deletes the duplication it makes redundant.

## The gap: rich state, no transitions

Everything above is **state you pull**. A consumer reads the current verdict and
learns "something changed" only from the coarse `Revision` bump; to find out *what*
changed it diffs its own previous projection against the new one. The M10
watched-type store already does exactly this — "a candidate-vs-published diff each
refresh classifies every disappeared type as immediate-removal / indefinite
blocking retention / grace-held pending removal." That diff-and-classify is a
**per-consumer re-derivation of transitions the registry already knows**, because
the registry is the thing that moved the type from one verdict to another.

For M12 this is the wrong shape. A per-type reconcile is *triggered by a
transition* ("became followable" → reconcile it; "absence-expired" → sweep it). If
each consumer re-derives transitions by diffing tables, we have re-implemented the
same edge detection in the watched-type store, the snapshot gate, the informer
lifecycle, and (soon) the per-type reconciler — four places that must agree.

## Proposal 1 — the registry emits per-type lifecycle transitions

Make the registry the **single emitter** of typed transition events, computed where
it already compares prior vs new verdict. No new verdict vocabulary — the events
are transitions *between the existing verdicts*:

| Event | Verdict transition | What it means / who acts |
|---|---|---|
| `TypeActivated` | `*` → `Followable` (settled — see Proposal 2) | type is healthy and stable; M12 schedules its (re)reconcile |
| `TypeWobbling` | `Followable` → `Retained` | transient unserved; **do not** sweep, postpone the type's reconcile, keep informers up |
| `TypeRecovered` | `Retained` → `Followable` | back; resume (collapses into `TypeActivated` after settle) |
| `TypeRemoved` | `Retained` → `Refused`(`absence-expired`) | grace elapsed, genuinely gone; M12's **per-type untracking sweep** for *this type only* |
| `TypeRefused` | `*` → `Refused`(permanent reason) | never watch; drop informers; surface in status |

Each event carries `(GVK, GVR, from, to, Reason, Generation, at)`. The registry
already holds per-entry timestamps for the grace clock, so it has everything to
emit these as a side effect of the verdict recompute it already runs. Delivery
shape is an open question (§Open questions) — a subscribe/observer callback invoked
under the registry's existing single-updater discipline is the leading option; the
`Revision` counter stays as the cheap "did anything change" gate for bulk consumers
that don't want per-type granularity.

The point is not "add an event bus." It is: **the transition is computed once, by
the component that owns the decision, and named** — so every consumer reacts to the
same edge instead of re-detecting it.

## Proposal 2 — the wobble-settle phase (debounce activation)

Separate two timers that are easy to conflate:

- **`RemovalGrace` (existing, 60s) governs REMOVAL.** How long a vanished type is
  *held* before its deletion is honored. It exists so a discovery blink never
  sweeps git. It is deliberately long and is product safety.
- **The settle window (new, short — a few seconds) governs ACTIVATION.** How long a
  type must be *stably* `Followable` before its per-type reconcile is allowed to
  fire. It exists so a flapping or just-appeared type does not drive a per-type
  reconcile (and a potential snapshot/sweep) on a state that is about to change
  again.

`TypeActivated` is emitted only after the type has been continuously `Followable`
for the settle window. A `Followable`→`Retained`→`Followable` flap inside the
window emits **no** `TypeActivated` churn — the reconcile waits for stability. This
is the concrete answer to "should we build a wait-for-the-wobble phase that
prevents the wobble from breaking the first type-specific reconcile": **yes, and it
belongs in the registry, not in each consumer**, so the debounce is implemented and
tested once and M12 simply consumes a stable signal.

A first per-type reconcile therefore only ever runs against a type that has been
stably healthy; a sweep only ever runs on a settled `TypeRemoved`. The wobble can
no longer break either.

## Proposal 3 — the consolidation this unlocks (the cleanup)

The headline payoff is deletion, not addition. "Is this type usable, and what just
changed?" is currently answered in three layers that each re-derive a slice of it:

1. **Catalog** (`api_resource_catalog.go`): marks group/versions degraded, keeps
   last-known on partial discovery.
2. **Registry**: turns that into `Verdict` + `Reason` + the grace.
3. **Watched-type store** (M10): a candidate-vs-published diff that re-classifies
   every disappeared type into immediate-removal / indefinite-blocking /
   pending-removal.

Layer 1 and 2 are the right boundary and stay (see
[discovery-catalog-typeset-boundary.md](type-followability.md)).
**Layer 3's re-classification is the duplication to remove**: once the registry
emits transitions, the watched-type store stops diffing and re-classifying — it
*subscribes* to its types' transitions and updates membership directly. Concretely,
this design expects to collapse:

- the candidate-vs-published **diff/classify** step → replaced by reacting to
  `TypeWobbling` / `TypeRemoved` / `TypeActivated`;
- `resolveSnapshotGVRs`'s ad-hoc `retainedWatchedTypes` fail-closed scan → replaced
  by a single "are all my types reconcile-eligible?" read off the lifecycle, with
  the *same* fail-closed result;
- the scattered consumers that gate rebuilds on the coarse `Revision` bump and then
  re-derive *what* changed → replaced by the named transition;
- the bespoke pending-removal timers in the watched-type store, folded back onto the
  registry's one grace clock (the store should not own a second copy of the grace).

Net: one owner of "type health over time," one grace clock, one settle clock, one
place that names a transition — and three downstream re-derivations deleted. That is
the "serious simplification" worth taking *with* this change rather than after it,
because adding events without removing the diffs would be strictly worse.

## How M11 and M12 consume it

- **M11 (visibility):** the bounded `GitTargetStatus` roll-up becomes a projection
  of current lifecycle states + last-transition `Reason`s (which it wanted anyway):
  watched/resolved/failing counts, capped failing-type names with their reason, CRD
  versions. No separate computation.
- **M12 (per-type reconcile + sweep):** driven directly by the events.
  `TypeActivated` → reconcile that type's snapshot into git; `TypeRemoved` → sweep
  *only that type's* documents. The settle phase gates the first reconcile; the
  per-type sweep stays type-scoped and fires only on a settled removal — preserving
  the anti-sweep invariant the global mark-and-sweep guards today. The "one slow or
  unhealthy type blocks the whole GitTarget at bootstrap" failure mode disappears
  because each type activates independently.

## Safety invariants (unchanged)

- **Never sweep on a partial/reduced view.** A type is swept only on a *settled*
  `TypeRemoved` (`absence-expired`), never on `Retained`/`Unknown`/wobble.
- **`RemovalGrace` still gates deletion.** The settle window only gates activation;
  it never shortens removal.
- **GVK↔GVR is 1:1.** A multi-resource GVK stays a refused `TypeConflict`.
- **`Unknown` (catalog unavailable) is global, not per-type.** When the registry
  cannot assess the surface at all, no per-type reconcile should proceed — this stays
  a fail-closed whole-surface gate (§Open questions refines it).

## Sequencing and open questions

This is the substrate M12 should be built **on**, so it lands as M12's first slice
(the event + settle layer) before the per-type sweep, not as a separate milestone.
M11 can read the lifecycle states even before the events exist (it only needs
current state), so it is not blocked.

Open questions to settle before coding:

1. **Delivery shape.** Observer callback under the registry's single-updater lock,
   a buffered channel drained by a consumer goroutine, or a "transitions since
   revision N" pull API? The callback keeps the one-writer discipline; a channel
   risks reordering vs the `Generation`.
2. **Settle window value and scope.** One global value, or per-type/per-kind? Start
   with one small fixed constant (mirror `RemovalGrace`'s "safety, not tuning"
   stance) and revisit only with evidence.
3. **Flap coalescing.** Rapid `Followable`↔`Retained` flapping should coalesce to
   at most one pending `TypeActivated`; define the coalescing rule explicitly so two
   consumers cannot disagree on how many events fired.
4. **`Unknown` granularity.** Is "catalog unavailable" always whole-surface, or can
   it be scoped to the degraded group/version so unrelated types keep reconciling?
   (This is the same question the e2e cascade raised: one group's discovery failure
   should not stall unrelated types.)
5. **Idempotency for restart.** On controller restart there is no "prior verdict" to
   diff from; define the cold-start emission (treat first observation as the initial
   state, emit `TypeActivated` only after the settle window, never a spurious
   `TypeRemoved`).
