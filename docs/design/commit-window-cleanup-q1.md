# Commit Window Cleanup Q1: Drop The Planner Shell

> Status: proposed
> Date: 2026-05-01
> Scope: this plan only. Q2 (atomic / per-event abstraction) is intentionally out of scope and will be discussed separately.

After [simplify2](/workspaces/gitops-reverser/docs/design/commit-window-simplify2.md) the "planner" no longer plans. It is a 1:1 projection from `[]PendingWrite` to `[]CommitUnit`. This plan deletes the planner shell so the code matches reality.

## Why

The current types invite confusion:

- `CommitPlan` is `{ Units []CommitUnit }` — a wrapper around a slice. It buys nothing.
- `CommitUnit` is `PendingWrite` minus `Targets`, plus three derived fields (`MessageKind`, `GroupAuthor`, `Target`). All three are deterministic functions of `PendingWrite`.
- `commit_planner.go` mixes branch-worker pending-write builders (which belong to the worker) with a tiny adapter (which can live next to the executor or just be inlined).
- `buildCommitPlan` is a switch that derives those three fields and otherwise copies. There is no planning.

The misnaming actively misleads future readers and invites someone to reintroduce a second decision pass "in the planner".

## Bold Outcome

After this change:

- **`CommitPlan` is gone.**
- **`CommitUnit` is gone.**
- **`buildCommitPlan` is gone.**
- **`executeCommitPlan` is gone.** Its replacement is `executePendingWrites(ctx, repo, []PendingWrite)`.
- **`commit_planner.go` is gone.** Its remaining helpers (`buildGroupedPendingWrite`, `buildAtomicPendingWrite`, `resolveEventsForPendingWrite`, `resolveTargetMetadata`) move to `branch_worker.go` (they are branch-worker helpers, not planning), or to a new `pending_writes.go` next to it.
- The three derivations live as **methods on `PendingWrite`**: `MessageKind()`, `Author()`, `Target()`.
- The executor consumes `[]PendingWrite` directly.

What stays unchanged:

- `PendingWriteCommit` and `PendingWriteAtomic` (and the `PendingWriteKind` enum). The two-kind distinction encodes a real semantic difference; this cleanup is about deleting the planner shell, not unifying the kinds.
- `CommitMessageKind` (the enum). The executor still switches on it.
- All branch-worker behavior, all event-loop behavior, all push/replay behavior.

## Concrete File-By-File Changes

### Delete

- **`internal/git/types.go:162-165`** — the `CommitPlan` struct.
- **`internal/git/types.go:176-185`** — the `CommitUnit` struct.
- **`internal/git/commit_planner.go:185-235`** — `buildCommitPlan`. After this delete the file is just pending-write builders; either rename it or fold them.

### Rename / move

- **`internal/git/commit_planner.go` → fold into `branch_worker.go`**, OR rename to **`internal/git/pending_writes.go`**. Either is fine; the test for "is this still a planner?" is no. Folding into `branch_worker.go` is more honest because `buildGroupedPendingWrite`, `buildAtomicPendingWrite`, `resolveEventsForPendingWrite`, and `resolveTargetMetadata` are all `*BranchWorker` methods.
- **`executeCommitPlan` → `executePendingWrites`** in [commit_executor.go:32](/workspaces/gitops-reverser/internal/git/commit_executor.go#L32). Signature changes to `(ctx, repo, []PendingWrite)`.
- **`executeCommitUnit` → `executePendingWrite`** in [commit_executor.go:92](/workspaces/gitops-reverser/internal/git/commit_executor.go#L92). Parameter changes from `CommitUnit` to `PendingWrite`.

### Add to `PendingWrite`

Three accessor methods, ideally next to the type in `types.go` or in a small `pending_write.go` file:

```go
// MessageKind is derived from the pending write's shape:
//   - Atomic         → CommitMessageBatch
//   - Commit, 1 evt  → CommitMessagePerEvent
//   - Commit, N evts → CommitMessageGrouped
func (p PendingWrite) MessageKind() CommitMessageKind {
    if p.Kind == PendingWriteAtomic {
        return CommitMessageBatch
    }
    if len(p.Events) == 1 {
        return CommitMessagePerEvent
    }
    return CommitMessageGrouped
}

// Author returns the grouped commit author for a Commit kind. Atomic kind
// returns "" because the executor uses operator authorship for batch commits.
func (p PendingWrite) Author() string {
    if p.Kind == PendingWriteAtomic || len(p.Events) == 0 {
        return ""
    }
    return p.Events[0].UserInfo.Username
}

// Target returns the single resolved target metadata for this pending write.
// For Atomic this is the request-specified target; for Commit it is the
// (already invariant) target of the events.
func (p PendingWrite) Target() ResolvedTargetMetadata {
    name, ns := p.targetIdentity()
    if md := p.findTargetMetadata(name, ns); md.Name != "" {
        return md
    }
    return ResolvedTargetMetadata{Name: name, Namespace: ns}
}

func (p PendingWrite) targetIdentity() (string, string) {
    if p.Kind == PendingWriteAtomic {
        return p.GitTargetName, p.GitTargetNamespace
    }
    if len(p.Events) == 0 {
        return "", ""
    }
    e := p.Events[0]
    return e.GitTargetName, e.GitTargetNamespace
}
```

### Move methods on `CommitUnit` onto `PendingWrite`

- **`(u CommitUnit) path() string`** ([commit_executor.go:51](/workspaces/gitops-reverser/internal/git/commit_executor.go#L51)) → **`(p PendingWrite) path() string`**. Replace `u.Target.Path` with `p.Target().Path`. Replace `u.Events` with `p.Events`.
- **`(u CommitUnit) commitMetadata()`** ([commit_executor.go:63-90](/workspaces/gitops-reverser/internal/git/commit_executor.go#L63-L90)) → **`(p PendingWrite) commitMetadata()`**. The switch becomes `switch p.MessageKind()`. The branches change `u.Events` → `p.Events`, `u.CommitConfig` → `p.CommitConfig`, `u.Signer` → `p.Signer`, `u.CommitMessage` → `p.CommitMessage`, `u.Target.Name` → `p.Target().Name`, and the `renderGroupCommitMessage(u, ...)` / `commitOptionsForGroup(u, ...)` calls take `p` instead.

### Update signatures that took `CommitUnit`

- **`renderGroupCommitMessage(unit CommitUnit, config CommitConfig)`** in [commit.go:73](/workspaces/gitops-reverser/internal/git/commit.go#L73) → **`renderGroupCommitMessage(p PendingWrite, config CommitConfig)`**. Replace `unit.GroupAuthor` with `p.Author()`, `unit.Target.Name` with `p.Target().Name`, `unit.Events` with `p.Events`.
- **`commitOptionsForGroup`** in `commit.go` (same pattern).
- The validation call site at [commit.go:130](/workspaces/gitops-reverser/internal/git/commit.go#L130) that currently builds an empty `CommitUnit{...}` for template-validation purposes builds an empty `PendingWrite{...}` instead.

### Update call sites

- **`internal/git/branch_worker.go:782-787`** — replace the two-line `buildCommitPlan` + `executeCommitPlan` with one call: `commitsCreated, err := w.executePendingWrites(w.ctx, repo, pendingWrites)`.
- **`internal/git/branch_worker.go:879-884`** — same pattern.

### Test migration

- **`internal/git/commit_planner_test.go`** — three tests at lines 155, 184, 207 call `buildCommitPlan` directly. Rewrite them as small `PendingWrite.MessageKind()` / `Author()` / `Target()` tests, or delete if the projection is too trivial to merit a dedicated test. (The interesting behavior moves to the new `executePendingWrites` integration tests, which already exist via the branch-worker tests.)
- **`internal/git/commit_executor_test.go`** — five `CommitUnit{...}` literals at lines 106, 128, 152, 180, 201. Replace with `PendingWrite{Kind: PendingWriteCommit, ...}` literals. The fields map straightforwardly: `MessageKind` (drop, derived), `GroupAuthor` (drop, derived from `Events[0].UserInfo.Username`), `Target` (drop, derived from `Events[0].GitTargetName/Namespace` plus a `Targets` map entry). Where a test wants to set the resolved target directly, populate `Targets`.
- **`internal/git/commit_test.go:135`** — one `CommitUnit{...}` literal. Same replacement.
- All other tests that use `*PendingWrite` are unaffected.

## Suggested Execution Order

This is the order that keeps the tree compiling at every step.

1. **Add the methods** (`MessageKind`, `Author`, `Target`, `path`, `commitMetadata`) to `PendingWrite`. Don't remove anything yet. The new methods coexist with the old `CommitUnit` machinery.
2. **Switch `renderGroupCommitMessage` and `commitOptionsForGroup`** to take `PendingWrite`. Update their two callers (real + the validation call site at commit.go:130).
3. **Switch the executor** internals: `executeCommitUnit` → `executePendingWrite`, parameter → `PendingWrite`. Update its two callers.
4. **Switch `executeCommitPlan` → `executePendingWrites`** with `[]PendingWrite`. Inline the loop; delete the `CommitPlan.Units` access. Update both call sites in `branch_worker.go`.
5. **Delete `buildCommitPlan`**, the `CommitPlan` struct, and the `CommitUnit` struct in one commit. Compiler errors at this point should be limited to test files.
6. **Migrate tests:** rewrite the three planner-projection tests, replace `CommitUnit{...}` literals in executor and commit tests.
7. **Move/rename the file:** fold `commit_planner.go` contents into `branch_worker.go` (preferred), or rename to `pending_writes.go`.

Each step is one commit; between commits the tree compiles and tests pass.

## Out Of Scope

- The two-kind `PendingWrite` (`Atomic` vs `Commit`). Stays as-is. Q2 covers that separately.
- Any change to the event-loop, the open window, push/replay, or the timer.
- Any change to encryption, target resolution, or message templating logic itself.
- `appendPendingWrite` extraction (small symmetry cleanup in the event loop). Captured in Q2.

## Acceptance Criteria

- `grep -rn 'CommitPlan\b\|CommitUnit\b\|buildCommitPlan\|executeCommitPlan' internal/` returns zero matches.
- `internal/git/commit_planner.go` no longer exists.
- All existing tests pass without semantic changes; only literal shapes change.
- `executePendingWrites(ctx, repo, []PendingWrite)` is the single executor entry point.
- `PendingWrite` exposes `MessageKind()`, `Author()`, `Target()`, `path()`, `commitMetadata()`.

## Estimated Cost

One PR. Approximately:

- ~50 lines net deleted in `types.go` + `commit_planner.go`.
- ~30 lines added as `PendingWrite` methods.
- Mechanical updates to `commit_executor.go`, `commit.go`, two call sites in `branch_worker.go`, and three test files.

Single review pass. No behavioral change. No CRD or external API change.
