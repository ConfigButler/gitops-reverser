# Commit Window Cleanup: Drop The Planner, Push Back On One-Shape Atomic

> Status: proposed
> Date: 2026-05-01

This is the post-implementation cleanup to follow the
[simplify2 refactor](/workspaces/gitops-reverser/docs/design/commit-window-simplify2.md).
That refactor collapsed two grouping passes into one. This plan does the next
two things:

- **Q1 — Drop the planner shell.** With no planner-time grouping left, the
  planner is dead naming. Remove it boldly. (Recommended: do it.)
- **Q2 — Should atomic collapse into the open window?** Tempting but wrong.
  This doc walks through why the operational divergence between atomic and
  per-event survives any naïve unification, and recommends a smaller targeted
  cleanup instead.

## Q1: Drop The Planner Shell (bold cleanup)

### What "planner" still does today

After [simplify2](/workspaces/gitops-reverser/docs/design/commit-window-simplify2.md),
[commit_planner.go](/workspaces/gitops-reverser/internal/git/commit_planner.go) holds three things:

- `buildGroupedPendingWrite` — branch-worker helper: resolve provider,
  signer, target metadata, build a `PendingWriteCommit`.
- `buildAtomicPendingWrite` — branch-worker helper for atomic requests.
- `buildCommitPlan` — a 1:1 projection from `[]PendingWrite` to `[]CommitUnit`
  that derives three fields: `MessageKind`, `GroupAuthor`, and a single
  resolved `Target`.

None of these is planning. They are: branch-worker pending-write builders, and
a small adapter that turns `PendingWrite` into the executor's input shape.

### Why this matters

Naming sets reader expectations. Calling this code "the planner" tells future
readers to look for planning logic. There is none. The misnaming is exactly
the kind of leftover that re-grows the very design we just deleted: someone
will eventually decide "the planner needs to handle X" and reintroduce a
second decision pass.

### Bold version: delete the planner type machinery entirely

**Delete:**

- The `CommitPlan` struct in [types.go:163-165](/workspaces/gitops-reverser/internal/git/types.go#L163-L165). It is `{ Units []CommitUnit }` — a slice with a wrapper. The wrapper buys nothing.
- The `CommitUnit` struct in [types.go:177-185](/workspaces/gitops-reverser/internal/git/types.go#L177-L185). It is `PendingWrite` minus `Targets` plus three derived fields. Replace with methods on `PendingWrite`.
- `buildCommitPlan` in [commit_planner.go:185-235](/workspaces/gitops-reverser/internal/git/commit_planner.go#L185-L235). Replaced by inline use of the new `PendingWrite` accessors at the executor call site.

**Add to `PendingWrite`** (or a small adapter file next to it):

```go
func (p PendingWrite) MessageKind() CommitMessageKind {
    if p.Kind == PendingWriteAtomic { return CommitMessageBatch }
    if len(p.Events) == 1            { return CommitMessagePerEvent }
    return CommitMessageGrouped
}

func (p PendingWrite) Author() string {
    if p.Kind == PendingWriteAtomic { return "" } // operator authorship handled by executor
    return p.Events[0].UserInfo.Username
}

func (p PendingWrite) Target() ResolvedTargetMetadata {
    name, ns := p.targetIdentity()
    if md := p.findTargetMetadata(name, ns); md.Name != "" { return md }
    return ResolvedTargetMetadata{Name: name, Namespace: ns}
}

func (p PendingWrite) targetIdentity() (string, string) {
    if p.Kind == PendingWriteAtomic { return p.GitTargetName, p.GitTargetNamespace }
    e := p.Events[0]
    return e.GitTargetName, e.GitTargetNamespace
}
```

**Change `executeCommitPlan`** in [commit_executor.go:32](/workspaces/gitops-reverser/internal/git/commit_executor.go#L32) to take `[]PendingWrite` directly. Rename to `executePendingWrites`. The body walks pending writes and asks each one for its `MessageKind`, `Author`, `Target`, etc.

**Rename the file:**

- `internal/git/commit_planner.go` → `internal/git/pending_writes.go` (or move `buildGroupedPendingWrite` and `buildAtomicPendingWrite` into `branch_worker.go` directly, since those are branch-worker helpers, and delete the file).

**Update call sites:**

- [branch_worker.go:782](/workspaces/gitops-reverser/internal/git/branch_worker.go#L782) and [branch_worker.go:879](/workspaces/gitops-reverser/internal/git/branch_worker.go#L879) drop the `buildCommitPlan` step and call `executePendingWrites` directly.

**Update tests:**

- Any test asserting on `CommitPlan{Units: ...}` switches to asserting on `[]PendingWrite` plus the derived getters.
- Any test calling `buildCommitPlan` directly (planner unit tests) becomes either deleted (the projection is too trivial to test) or rewritten as small `PendingWrite.MessageKind()` etc. tests.

### What stays

- `CommitMessageKind` (the enum) stays — the executor still needs to switch on it.
- `PendingWriteCommit` and `PendingWriteAtomic` stay — see Q2.
- The shape of executor logic stays — it just consumes `PendingWrite` directly.

### Estimated cost

About one type definition deleted, one struct deleted, one file renamed or merged, three small accessor methods added, ~50 lines net removed in the type/projection code, plus mechanical test updates. Single PR scope.

## Q2: Should Atomic Collapse Into The Open Window?

**Recommendation: no.** This section walks through why, against the
[use cases in commit-window-refactor.md](/workspaces/gitops-reverser/docs/design/commit-window-refactor.md#use-cases),
and proposes a smaller cleanup that captures the structural symmetry without
inheriting the costs.

### The proposal restated

> Per-event and atomic could share the same abstraction. They only "break" the
> commit window, but the window is meant to be small anyway. MessageKind and
> authorship can be properties on the event.

If we take this seriously, the model becomes:

- One `PendingWrite` shape (no `Kind` tag).
- `WriteRequest.CommitMode` either disappears or becomes a hint encoded in
  events.
- Atomic events stream into the open window like any other events; the window
  finalizes on the same triggers plus "atomic request boundary".
- Each event carries its own `Author` and `MessageKind` (and the atomic case
  carries `CommitMessage` as well).

### Re-read of the use cases

Walking the four named use cases in the existing design doc:

1. **Burst collapse.** Per-event-only property. Atomic is already one commit
   by definition; nothing to collapse. Unification doesn't help here.
2. **Honest authorship.** This is the explicit sentence: *"Atomic reconcile
   writes are authored by the operator because they represent a controller
   snapshot, not a single human action."* This is a deliberate semantic
   distinction, not an implementation accident. It is what the use case is
   *about*.
3. **Target isolation.** Both kinds are single-target today. Unification
   neither helps nor breaks this.
4. **Safe replay.** Both kinds carry resolved metadata. Unification neither
   helps nor breaks this.

So: 1 use case is per-event-only, 2 are unaffected, and 1 (Honest authorship)
is *the* place where the two modes intentionally differ. The named use cases
do not motivate the unification; one of them actively argues against it.

### Six places where the operational behaviors actually diverge

A unified abstraction has to either preserve each of these or accept the
regression. None of these is a naming issue.

| Concern | Per-event | Atomic | Survives unification? |
|---|---|---|---|
| **Authorship source** | `event.UserInfo.Username` (audit data, per event) | Operator (request-level, ignores event UserInfo) | Only by adding either an event-level override or a window-level override |
| **Commit message** | Template rendered from event data | Caller-provided free-form string | Only if events carry the string (redundantly per event) or the window does |
| **Finalize timing** | author/target change, silence, byte cap, `commitWindow=0`, shutdown | End of `WriteRequest` (must finalize before the next request, and not on silence) | Only by adding a "request boundary" finalize trigger |
| **Byte-cap behavior** | Mid-stream finalize is fine; events split into multiple windows | Mid-stream finalize would split one atomic burst into multiple commits — violates the use case | Only by giving atomic a "do-not-split" exemption, which is `Kind=Atomic` in disguise |
| **Same-author coalescing** | Same author + same target events from different requests *should* merge into one window | Atomic from request A must not merge with same-operator events from request B (or with audit events that happen to be operator-authored) | Only by adding "request boundary" semantics that prevent merging — again, `Kind=Atomic` in disguise |
| **Target identity source** | Each event's `GitTargetName` (events know their target) | `WriteRequest.GitTargetName` (caller asserts; events are filled in from it) | Only if events are pre-populated by the request-builder, which currently happens inside `buildAtomicPendingWrite` |

### What unification actually saves

If we paid all those costs, what we'd remove is:

- The `PendingWriteKind` enum (one constant).
- The two-branch dispatch in `buildCommitPlan` (already a small switch — and Q1 deletes the planner anyway, so this is moot).
- The two-branch dispatch in `handleQueueItem` ([branch_worker.go:538](/workspaces/gitops-reverser/internal/git/branch_worker.go#L538)).

What we'd add:

- `Author`, `MessageKind`, and `CommitMessage` fields on the open window
  (or events).
- A "request boundary finalize" trigger.
- A "do-not-split" / "do-not-coalesce-across-requests" flag on the open window.
- A new failure mode where a mis-populated event (e.g., reconciler forgets to
  set `Author=operator`) silently leaks audit-style attribution onto a batch
  commit.

The extra fields plus the extra flag plus the new failure mode add up to more
complexity than the dispatch they replace. The two `PendingWriteKind` values
are the cheapest, most legible encoding of a real semantic distinction the
design intentionally has.

### Direct response to the user's framing

> *"They could fit the same abstraction very well actually."*

The data shapes can. The semantic distinctions cannot, and the use cases
require the semantic distinctions.

> *"I don't see any difference in how the events are handled."*

There are six differences in the table above. The most user-visible one is
authorship: for an atomic reconcile, the *operator* must be the commit author,
not whatever user was set on the event. Today the type system enforces this
because the atomic path never reads `event.UserInfo`. Under unification this
becomes an event-population convention that the producer must get right, with
no compile-time check.

> *"They 'break' the commit window, but you shouldn't make the commit window
> too big."*

True about the commit window's intended size, but irrelevant to atomic.
Atomic is not "events that arrived in a small enough window to be one
commit"; it is "events that **must** be one commit regardless of arrival
shape, because they represent a single controller decision". The window's
size has nothing to do with that guarantee.

> *"MessageKind and authorship could just be a property of the event."*

They could, mechanically. But:

- It moves the kind from the *intent* (request-level: "this is a reconcile
  snapshot") to the *substrate* (per-event: "I claim to be a batch event").
  The intent belongs at the level of the caller's decision, not on each
  event the caller happens to bundle.
- It makes the producer responsible for population correctness with no type
  check. Today, an atomic request type-statically becomes a `Batch` commit.
  Under the proposal, an atomic request whose events have wrong fields
  produces wrong commits silently.
- It does not actually eliminate the distinction; it relocates it. The open
  window now needs to either take its kind from the events (and refuse to
  mix kinds), or carry its own kind. Either way, two-shape behavior survives.

### What I would do instead: extract the shared tail

The structural symmetry the user spotted is real but small, and it lives in
[branch_worker.go:538-559](/workspaces/gitops-reverser/internal/git/branch_worker.go#L538-L559)
(atomic) vs [branch_worker.go:644-654](/workspaces/gitops-reverser/internal/git/branch_worker.go#L644-L654)
(inside `finalizeOpenWindow`). Both end with the same three lines:

```go
commitPendingWrites([]PendingWrite{*pw}, len(l.pendingWrites) > 0)
l.pendingWrites = append(l.pendingWrites, *pw)
l.pendingWritesBytes += pw.ByteSize
```

Extract:

```go
func (l *branchWorkerEventLoop) appendPendingWrite(pw *PendingWrite) error {
    if err := l.w.commitPendingWrites([]PendingWrite{*pw}, len(l.pendingWrites) > 0); err != nil {
        return err
    }
    l.pendingWrites = append(l.pendingWrites, *pw)
    l.pendingWritesBytes += pw.ByteSize
    return nil
}
```

Then both paths read symmetrically:

- **Atomic:** `finalizeOpenWindow` → `buildAtomicPendingWrite` → `appendPendingWrite` → `maybeSchedulePush`.
- **Window finalize:** `buildGroupedPendingWrite(window.orderedEvents())` → `appendPendingWrite` → (no schedule, the caller does that).

The symmetry is now visible at the call site. The two `PendingWriteKind` values stay because they encode a real distinction; the *handler code* stops repeating itself.

This is the cleanup that costs nothing and keeps the design honest.

## Combined Plan

**Do:**

1. (Q1) Delete `CommitPlan` and `CommitUnit`. Add `MessageKind`, `Author`, `Target` accessor methods to `PendingWrite`. Rename `executeCommitPlan` → `executePendingWrites`; have it consume `[]PendingWrite`. Rename `commit_planner.go` to `pending_writes.go` (or fold `build*PendingWrite` into `branch_worker.go` and delete the file).
2. (Q2 small cleanup) Extract `appendPendingWrite` on the event loop. Use it from both the atomic branch and `finalizeOpenWindow`.

**Don't:**

3. (Q2 large change) Don't merge `PendingWriteAtomic` and `PendingWriteCommit` into one shape. The semantic distinctions in the use cases (specifically Honest authorship) and the operational distinctions (byte-cap, request-boundary finalize, authorship source) all survive unification. The two-kind encoding is the cheapest legible representation of a real difference.

## Test Impact

- Q1: planner-projection tests in [commit_planner_test.go](/workspaces/gitops-reverser/internal/git/commit_planner_test.go) become tests of `PendingWrite.MessageKind()` / `Author()` / `Target()`, or are deleted as too-trivial. Existing executor tests keep working — they switch input from `CommitPlan` to `[]PendingWrite`.
- Q2: `appendPendingWrite` extraction is covered by the existing branch-worker tests. No new test surface needed.

## Open Question Worth Confirming

The two missing tests called out at the end of the simplify2 review are still
worth adding before this cleanup, so the cleanup runs against a stable
behavioral baseline:

- Multi-PendingWrite replay-on-conflict determinism (current
  `TestPushPendingCommits_ReplaysOnConflict` only replays one).
- Regression test for the intentional 4-→-3 commit behavior change
  (`alice(F), bob(F), alice(X), alice(F)`).

Land those first, then do this cleanup against a green baseline.
