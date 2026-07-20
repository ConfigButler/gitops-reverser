# Idea: Unify `PendingWriteAtomic` and `PendingWriteCommit` into one shape

> Status: deferred — captured for future re-assessment.
> Date: 2026-05-05

## The proposal in one sentence

Collapse `PendingWriteKind` (today's `PendingWriteCommit` vs `PendingWriteAtomic`) so
the branch worker has a single open-window pipeline; the snapshot/reconcile case becomes
"a `WriteRequest` with `CommitMessage` set and a request-boundary finalize trigger",
not a parallel `Kind`-tagged path.

## Why it keeps coming up

`PendingWriteCommit` and `PendingWriteAtomic` carry the same fields and end with the
same three lines (`commitPendingWrites`, `append(pendingWrites, ...)`, `pendingWritesBytes += ...`).
The structural symmetry is real. The user-visible commit shape is `CommitMessage` set vs
unset. The temptation is to make that the dispatch and delete the `Kind` enum.

## Use case walkthrough

The four use cases in [commit-window-refactor.md](../spec/commit-window-refactor.md#use-cases):

1. **Burst collapse.** Per-event-only behavior (atomic is one commit by definition).
   Unification neither helps nor breaks this.
2. **Honest authorship.** Reconcile snapshots are configured-author; live commits carry
   either a resolved audit actor or the explicit unresolved identity. Today this is enforced by
   *type*: the atomic path is `AttributionNotAttempted` and never reads `event.UserInfo`. Under
   unification it becomes a *convention*: producers must retain the correct
   `AttributionOutcome` for snapshot batches. Same observable result, weaker compile-time guarantee.
3. **Target isolation.** Single-target on both paths today. Unaffected.
4. **Safe replay.** Both paths carry resolved metadata. Unaffected.

So one use case is per-event-only, two are unaffected, and one (Honest authorship) is
where the two paths intentionally differ. Whether unification is "fine" depends on whether
that one is comfortable being a convention rather than a type-system invariant.

## Where the two shapes actually diverge

| Concern | Per-event / grouped | Atomic | Survives unification? |
|---|---|---|---|
| Authorship source | resolved actor or explicit outcome | Configured author (`AttributionNotAttempted`) | Yes, by preserving `AttributionOutcome` rather than relying on an empty username |
| Commit message | Template render | Caller-provided string | Yes, via `CommitMessage` non-empty as the dispatch signal |
| Finalize timing | Author/target change, silence, byte cap, shutdown | Request boundary | Yes, by adding "request has `CommitMessage` set" as a finalize trigger |
| Byte-cap behavior | Mid-stream finalize is fine | Must not split one snapshot across commits | Yes, via the rule "finalize the *next* organic window early; don't actively split a request" |
| Cross-request coalescing | Same (author, target) merges | Snapshot must not merge with audit-author events | Yes, the new finalize trigger forces a boundary on `CommitMessage`-bearing requests |
| Target identity source | `event.GitTargetName` | `WriteRequest.GitTargetName` (back-fills events) | Unchanged |

Each row is mechanically resolvable. The "survives" column is only "yes" if the producer
side cooperates — most importantly, if the reconcile producer stops fabricating
`UserInfo: {Username: "gitops-reverser"}` and lets the git-layer fall-through populate
`Author`.

## Reasoning — both directions

**Argument for unification.** One open-window pipeline, one `PendingWrite` shape, one
`commitOptionsFor` builder (instead of three: `forEvent`, `forGroup`, `forBatch`). The
architectural principle becomes clean: producers describe *what* changed and declare
intent (`CommitMessage` set vs unset); the git layer decides *how* it's attributed. The
reconcile producer stops asserting an identity it has no business asserting.

**Argument against unification.** The Honest-authorship guarantee is currently enforced
at the type level. Under unification it becomes a population convention: a producer that
forgets to clear `UserInfo` on snapshot events would silently leak audit-style attribution
onto reconcile commits — no compile-time check would catch it. The two `PendingWriteKind`
values are also the cheapest legible encoding of "request-level intent vs per-event
data": deleting them moves intent from request scope onto the substrate (events).

After [commit-window-cleanup-q1](https://github.com/) (the planner deletion, now landed),
the savings from unification have shrunk: it removes one enum, one dispatch arm in
`handleQueueItem`, and the three `commitOptionsFor*` builders. It adds: a new
finalize trigger, a do-not-split byte-cap rule, an event/window invariant
("`UserInfo` empty == snapshot intent"), and producer-side cleanup.

## Complexity assessment

A rough sketch of the change footprint:

- **Producers** (small): three `UserInfo` fabrication sites in
  folder_reconciler.go deleted; the
  reconcile `WriteRequest` site sets `CommitMessage` instead of `CommitMode`. The audit
  producer in [git_target_event_stream.go](../../internal/reconcile/git_target_event_stream.go)
  is unchanged.
- **Branch worker** (medium): merge `buildAtomicPendingWrite` into the open-window flow;
  add request-boundary finalize trigger; rework byte-cap to "don't split a request".
- **Git layer** (small): collapse three signature builders into one driven by
  `PendingWrite.author()`.
- **Types** (small): delete `PendingWriteKind`, `PendingWriteCommit`, `PendingWriteAtomic`,
  `CommitMode`, `CommitModePerEvent`, `CommitModeAtomic`, `WriteRequest.CommitMode`.
- **Tests** (medium): rewrite anything asserting on `PendingWriteKind == PendingWriteAtomic`
  or `CommitMode == CommitModeAtomic`. Add tests for the new boundary rules (force-finalize
  on `CommitMessage`, byte-cap don't-split, empty-`UserInfo` fall-through). Reconcile tests
  that assert on the fabricated `UserInfo="gitops-reverser"` get rewritten to assert on
  the *commit's* Author/Committer instead — a healthier surface anyway.
- **Behavior change** (one): a multi-event per-event request that exceeds the byte cap
  mid-stream is no longer split. Pathological case (thousand-event request); worth a
  changelog line.
- **Migration**: stageable in roughly 8 PR-shaped steps that each leave the tree green.
  Not a single all-or-nothing change.

Roughly: a couple of weeks of focused work, mostly tests and migration order. Not a
weekend project; not a quarter-long rewrite.

## Recommendation

**Deferred.** The savings are modest after the Q1 planner cleanup, and the strongest
objection (losing the type-system check on snapshot authorship) doesn't dissolve — it
becomes a producer convention enforced by review and tests. Worth revisiting if any of
the following changes:

- A second snapshot-style producer appears and the duplication of "build atomic pending
  write + custom signature builder + custom dispatch" becomes painful.
- A new feature requires the open-window pipeline to also handle the snapshot case (e.g.
  cross-request coalescing rules that would naturally extend to snapshots too).
- The reconciler's fabricated `UserInfo` causes a bug (wrong attribution leaking into a
  commit). At that point removing the fabrication becomes a fix, and unification is the
  cleanest way to do it.

If none of those happen, the two-`Kind` model remains the cheapest honest encoding.
