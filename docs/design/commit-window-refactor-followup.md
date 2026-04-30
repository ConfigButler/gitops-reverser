# Commit Window Refactor — Follow-up Cleanup & Test Plan

> Status: open
> Date: 2026-04-30
> Related: [commit-window-refactor.md](./commit-window-refactor.md)

This document captures the concrete work that's still open after the
commit-window refactor's Phase 1–3 landed. It is a punch list, not a redesign.
Every item names the file and line range where the change applies, and why the
change is worth making.

The refactor's hard parts have landed cleanly: durability shape, the
atomic-loses-work bug, the push-failure taxonomy, the planner/executor split,
and the reconcile API rename. What remains is **(a)** test-only code masquerading
as production surface, and **(b)** missing tests for the refactor's strongest
correctness claims.

---

## Section 1 — Dead code to delete

### 1.1 `WriteEvents` and the parallel execution path in `git.go`

**The biggest single cleanup available.**

[git.go:182-405](../../internal/git/git.go#L182-L405) carries a complete second
execution path that mirrors `BranchWorker.executeCommitPlan` /
`executeCommitUnit`:

| Function | Line | Status |
|---|---|---|
| `WriteEvents` | [git.go:182](../../internal/git/git.go#L182) | callers: tests only |
| `WriteEventsWithContentWriter` | [git.go:193](../../internal/git/git.go#L193) | callers: tests only |
| `tryWriteEventsAttempt` | [git.go:262](../../internal/git/git.go#L262) | only called by `WriteEventsWithContentWriter` |
| `executeCommitPlanWithWriter` | [git.go:308](../../internal/git/git.go#L308) | duplicates `BranchWorker.executeCommitPlan` |
| `executeCommitUnitWithWriter` | [git.go:331](../../internal/git/git.go#L331) | duplicates `BranchWorker.executeCommitUnit` |
| `buildPerEventCommitPlan` | [git.go:382](../../internal/git/git.go#L382) | only called by `WriteEvents` |
| `currentHeadHash` | [git.go:395](../../internal/git/git.go#L395) | only called by `tryWriteEventsAttempt` |

All seven functions exist only to keep `WriteEvents` alive. `WriteEvents` itself
has zero non-test callers.

The cost of leaving this in place is real: the executor logic now exists in
two near-identical implementations. A change to one that doesn't get mirrored
in the other will be invisible to the test suite that exercises the wrong
copy.

**Action:** delete the seven functions above and rewrite their tests
(`git_operations_test.go` — 30 tests, `secret_write_test.go` — 5 tests) to
drive the BranchWorker path via `commitPendingWrites` + `pushPendingCommits`.

### 1.2 `WriteEventsResult` struct

[types.go:75-81](../../internal/git/types.go#L75-L81). Falls out with 1.1.

### 1.3 `renderCommitMessageForGroup`

[commit.go:92](../../internal/git/commit.go#L92). The dispatcher is dead — the
executor already collapses single-event grouped units to
`CommitMessagePerEvent` at plan time
([commit_planner.go:213-215](../../internal/git/commit_planner.go#L213-L215)),
so nothing in production calls this. Only
`TestRenderCommitMessageForGroup_SingleEventFallsBackToEventTemplate`
([commit_test.go:145](../../internal/git/commit_test.go#L145)) uses it.

**Action:** delete the function and the test. The fallback rule is verified
indirectly by the "single-event grouped → per-event template" planner behavior;
once a `TestExecutor_GroupedSingleEvent_UsesPerEventMessageFallback` exists
(see §3.2), the unit-level coverage is stronger anyway.

### 1.4 `GetCommitMessage`

[commit.go:36](../../internal/git/commit.go#L36). Public-looking name, no
non-test callers. Used by ~13 tests in `commit_test.go`.

**Action:** delete and rewrite the tests to call `renderEventCommitMessage`
directly (it's already package-private; the tests are in the same package).

### 1.5 `commitGroups` test helper

[branch_worker.go:709-723](../../internal/git/branch_worker.go#L709-L723). The
comment openly admits "kept as a test-facing helper". Phase 3 of the original
design says: "Remove transitional worker test helpers once tests are updated
to target the new boundaries directly." Four tests in
[branch_worker_split_test.go](../../internal/git/branch_worker_split_test.go)
call it.

The helper's body is now trivial — `buildGroupedPendingWrite` followed by
`commitPendingWrites`. Keeping it hides the actual API under test.

**Action:** inline `buildGroupedPendingWrite` + `commitPendingWrites` at each
call site, then delete the helper.

### 1.6 `commitAndPushWriteRequestForTest` test helper

[branch_worker_test.go:66-84](../../internal/git/branch_worker_test.go#L66-L84).
This is `commitAndPushRequest` reborn under a new name — the design called
that out for deletion. Six large tests
(`TestBranchWorker_CommitAndPushRequest_*`) drive their assertions through it.

**Action:** inline the three-line body (`build*PendingWrite` →
`commitPendingWrites` → `pushPendingCommits`) at each call site. Tests become
slightly longer, but the surface-under-test is what production actually does.

### 1.7 `TryReference`

[git.go:169](../../internal/git/git.go#L169). Public function, zero callers
anywhere in the codebase. Independent of this refactor — easy take.

**Action:** delete.

---

## Section 2 — Cleanup in newly-introduced code

These are not strictly dead code, but they're awkward seams the refactor left
behind.

### 2.1 Fake `WriteRequest` shim in the executor

[commit_executor.go:75-80](../../internal/git/commit_executor.go#L75-L80)
constructs a throwaway `WriteRequest` purely to satisfy
`renderBatchCommitMessage`'s signature. The executor already has all the data
on the `CommitUnit`; the shim exists only because `renderBatchCommitMessage`
in commit.go was written against the old `WriteRequest` shape.

**Action:** change `renderBatchCommitMessage` to take `(events []Event,
override string, gitTarget string, config CommitConfig)` directly. Update its
two callers (executor + `ValidateCommitConfig` in commit.go).

### 2.2 Reconstructed `commitGroup` in the executor

[commit_executor.go:168-179](../../internal/git/commit_executor.go#L168-L179)
rebuilds a `commitGroup` from a `CommitUnit` so it can call
`renderGroupCommitMessage` and `commitOptionsForGroup`. Same shape problem as
2.1: unit → fake legacy representation → render.

**Action:** change `renderGroupCommitMessage` and `commitOptionsForGroup` to
take a `CommitUnit` (or a small struct containing `Author, GitTarget, Events`).
Then `buildCommitGroupForUnit` and the `commitGroup` struct can live entirely
inside the planner where they belong.

### 2.3 Phase 4 ("CommitUnitBuilder") can be closed

The original doc lists optional Phase 4: "Consider whether `commitGroup`
should remain a grouped-planner helper or be replaced by a more general
`CommitUnitBuilder`." After 2.2 lands, `commitGroup` is purely an internal
grouping helper and `CommitUnit` is the unit. They are different concerns;
unifying them would obscure both.

**Action:** mark Phase 4 as "won't do" in the original design doc.

### 2.4 Filename typo

The design doc is `commit-window-refactor.md`. Fixing means a rename and a
breadcrumb update in three places that link to it
(`docs/architecture.md` if applicable; the front-matter `Related:` line in
this doc).

**Action:** rename to `commit-window-refactor.md`. Low priority but trivially
done.

### 2.5 Original design doc's "Phase 3 implemented" claim is inconsistent

[commit-window-refactor.md:654-663](./commit-window-refactor.md#L654) lists
Phase 3 as implemented and includes "Delete the old git.go request-writing
helpers and grouped/atomic compatibility surface that the new planner/executor
flow has replaced." But §1.1 of *this* doc shows that surface is still
present.

**Action:** once §1.1 lands, the original claim becomes true. Until then, the
design doc should reflect reality (e.g. add a note that `WriteEvents`
deletion is deferred while the test suite is migrated).

---

## Section 3 — Missing tests called out by the original design

The original design lists 14 named tests for the new boundaries. Five exist;
nine don't. Listing the gaps in priority order.

### 3.1 (HIGH) `TestBranchWorker_Replay_UsesResolvedMetadata_GitTargetDeletedMidBurst`

The original design calls this "the headline correctness test for resolved
metadata". The strongest correctness argument for the refactor is that
`PendingWrite.Targets` lets replay survive GitTarget mutations
mid-burst — see
[commit-window-refactor.md:163-176](./commit-window-refactor.md#L163-L176).

We have the resolved metadata
([types.go:149-168](../../internal/git/types.go#L149-L168)) and replay uses it
([commit_planner.go:185-235](../../internal/git/commit_planner.go#L185-L235)),
but **no test asserts the invariant**: a GitTarget deleted between commit-time
and push-time → push still succeeds under originally-resolved encryption
config.

**Sketch:**
1. Build a grouped `PendingWrite` against a target with encryption configured.
2. `commitPendingWrites` succeeds (local commit holds resolved metadata).
3. Delete the GitTarget *and* its encryption Secret from the fake client.
4. Trigger a conflict (advance remote externally).
5. `pushPendingCommits` must still succeed and the published file must be
   encrypted under the originally-resolved recipients.

### 3.2 (HIGH) `TestBranchWorker_TransientPushFailure_RetriesSameLocalCommits`

`pushPendingCommits` implements the conflict vs. transient distinction at
[branch_worker.go:826-840](../../internal/git/branch_worker.go#L826-L840), but
only the conflict branch is tested
([branch_worker_split_test.go:230](../../internal/git/branch_worker_split_test.go#L230)).
The transient branch — "remote unchanged → don't rebuild, don't advance
`lastPushAt`, return error" — is dead-reckoning.

**Sketch:** stub `PushAtomic` to return a transient-style error, leave the
remote tip unchanged, assert that:
- `pendingWrites` is unchanged
- the local commit objects are still valid (unchanged hashes)
- `lastPushAt` is unchanged
- `rebuildPendingWrites` was not called (no fetch+reset overhead)

### 3.3 (HIGH) `TestBranchWorker_PushFollowedByFetchFailure_TreatsAsTransient`

[branch_worker.go:834-837](../../internal/git/branch_worker.go#L834-L837) says:
"if push fails *and* the post-failure fetch also fails → return error and let
the next cooldown-driven retry try again." This is the "infinite retry
collapse" guard from
[commit-window-refactor.md:357-366](./commit-window-refactor.md#L357-L366).

**Sketch:** push errors, then `fetchRemoteBranchHash` errors. Assert
`pushPendingCommits` returns the original push error (not a sync error),
state is preserved, no replay attempted.

### 3.4 (MEDIUM) `TestPlanner_ResolvesEncryptionOncePerUniqueTarget`

The original design's most concrete efficiency claim is "encryption is
resolved twice today" — the refactor was supposed to fix that. The fix is
in [commit_planner.go:119-159](../../internal/git/commit_planner.go#L119-L159)
(target resolution is keyed by `pendingTargetKey` and cached per planning
pass).

We have no test that proves the cache works. A regression that re-introduces
per-event resolution would not be caught.

**Sketch:** wrap the fake client with a counting middleware. Build a
`PendingWrite` containing N events all pointing at the same GitTarget. Call
`buildGroupedPendingWrite`. Assert: exactly 1 GitTarget Get, exactly 1
Encryption Secret Get.

### 3.5 (MEDIUM) Planner tests as a discrete layer

`TestPlanner_GroupedWindow_GroupsByAuthorTargetAndCollisionRule`,
`TestPlanner_AtomicRequest_ProducesSingleAtomicPlan`,
`TestPlanner_GroupedWindow_PreservesArrivalOrderAcrossPendingWrites`.

Today the planner's behavior is exercised only end-to-end via
`commitPendingWrites`. That gives coverage but not boundary tests. A planner
test should take `[]PendingWrite` in and assert on the resulting
`[]CommitUnit` — message kind, ordering, target binding — without ever touching
git. These are cheap to write and pin the layer's contract.

### 3.6 (MEDIUM) Executor tests as a discrete layer

`TestExecutor_GroupedSingleEvent_UsesPerEventMessageFallback`,
`TestExecutor_GroupedMultiEvent_UsesGroupTemplate`,
`TestExecutor_AtomicUnit_UsesBatchMessage`,
`TestExecutor_NoOpUnit_SkipsCommit`,
`TestExecutor_AppliesEncryptionFromCommitUnit_NotFromWorker`.

The last one is the most valuable: it pins the invariant that the executor
reads encryption config from the `CommitUnit` (resolved at plan time), never
from mutable worker state. That's the property that makes mid-burst GitTarget
edits race-free.

### 3.7 (LOW) `TestBranchWorker_AtomicAndGroupedInterleaved_PreservesArrivalOrder`

Atomic interleaved with grouped is an explicit design choice
([commit-window-refactor.md:422-439](./commit-window-refactor.md#L422-L439),
"Pick option 1") but isn't directly verified. Existing coverage proves each
in isolation; this test would prove the interleaving order property.

### 3.8 (LOW) `TestBranchWorker_Replay_DropsUnitsThatBecomeNoOpAgainstNewRemoteTree`

The "replay may drop commits that become no-ops" property
([commit-window-refactor.md:381-389](./commit-window-refactor.md#L381-L389))
is implemented (the executor short-circuits when `applyEventToWorktree`
returns no changes), but no test asserts it explicitly under conflict replay.

---

## Section 4 — E2E coverage

The single
[commit_window_batching_e2e_test.go](../../test/e2e/commit_window_batching_e2e_test.go)
exercises the full pipeline (kubectl → audit webhook → Valkey → consumer →
BranchWorker) and asserts the main observable contract: a 4-event burst
within a 3s commit window produces exactly one grouped commit.

This is enough for now. The deep correctness invariants (resolved-metadata
replay, transient-vs-conflict distinction) are unit-test territory — they are
hard to provoke deterministically at e2e scale, and the cost/benefit favors
unit coverage.

**No new e2e tests recommended for this refactor.** Revisit if a regression
surfaces that unit tests can't reproduce.

---

## Section 5 — Suggested order of operations

Each step is small and independently shippable.

1. **Write the three HIGH-priority tests (§3.1, §3.2, §3.3).** They lock in
   the refactor's correctness story before any further code motion. None of
   them require deleting anything yet.
2. **Write `TestPlanner_ResolvesEncryptionOncePerUniqueTarget` (§3.4).** Same
   reason — regression-pin first, refactor second.
3. **Delete §1.7 (`TryReference`).** Trivial, no test changes.
4. **Delete §1.3 (`renderCommitMessageForGroup`) and §1.4 (`GetCommitMessage`).**
   Update the affected `commit_test.go` tests to call package-private renderers
   directly.
5. **Inline §1.5 and §1.6 helpers.** Tests become a handful of lines longer
   each but their surface-under-test is now the public API.
6. **Delete §1.1 (`WriteEvents` and friends) + §1.2 (`WriteEventsResult`).**
   This is the biggest delete; do it last because it requires rewriting 35
   tests in `git_operations_test.go` and `secret_write_test.go`. Drive the
   rewritten tests through `BranchWorker.commitPendingWrites` +
   `pushPendingCommits`.
7. **Apply §2.1 and §2.2 (executor seams).** Now that the legacy
   `executeCommitPlanWithWriter` is gone, the renderers can drop their
   compatibility-shaped signatures.
8. **Write planner/executor unit tests (§3.5, §3.6).** Easy after §2.2 because
   the layers no longer share types with each other awkwardly.
9. **Write the LOW-priority tests (§3.7, §3.8) if there's time.**
10. **Update [commit-window-refactor.md](./commit-window-refactor.md)**:
    rename to fix the typo (§2.4), close out Phase 4 (§2.3), align the
    "implemented" status (§2.5).

After step 10, the refactor is genuinely done.
