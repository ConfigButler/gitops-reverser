# Simplify Commit Window to One Live Window + Ordered Pending Commits

> Status: implemented
> Date: 2026-05-01

## Summary

This simplification has been implemented. The previous design had two layers
deciding commit boundaries:

- The branch worker decides when raw events leave the live buffer.
- The planner later re-splits that drained buffer into commit groups by author/target/path history.

That was the source of the confusion in the earlier [commit-window-refactor.md](/workspaces/gitops-reverser/docs/design/commit-window-refactor.md): the sentence about author changes "not draining immediately" was accurate for the old code, but it meant the live queue was not the real commit-shaping boundary.

Implemented simplification:

- Make the branch worker the only owner of per-event grouping and ordering.
- Keep exactly one open per-event window at a time.
- Finalize that window immediately on author change, target change, atomic arrival, byte-cap trip, shutdown, or `commitWindow=0`.
- Remove planner-time regrouping entirely. One `PendingWrite` always maps to exactly one local commit.

## Target Model

```
event arrival
   └─> open window (invariant: same author + same target)
         └─ finalize triggers ─> ordered []PendingWrite (1:1 with commits)
                                   └─> CommitExecutor ─> push / replay
```

There is **one** queue: `pendingWrites`. There is **one** grouping decision point: append-or-finalize on the open window. There is **no** second pass at plan time.

## Key Changes

### Branch worker ([branch_worker.go](/workspaces/gitops-reverser/internal/git/branch_worker.go:532))

- Replace the raw `buffer []Event` model with one open window accumulator for `CommitModePerEvent`.
- The open window has a strict invariant: all contained events share the same author and the same target identity `(name, namespace)`.
- Process `WriteRequest.Events` as a true event stream, one event at a time, even when a single request contains multiple events.
- Window rules:
  - Same author + same target: append to the open window.
  - Same-path re-edits in the open window: last-write-wins; preserve first-seen path order.
  - Different author: finalize the current window, then start a new one with the incoming event.
  - Different target: finalize the current window, then start a new one with the incoming event.
  - Atomic request: finalize the current window first, then append one atomic pending write.
  - `commitWindow=0`: finalize immediately after each appended event, so it is truly per-event.
  - Byte cap: finalize the current window immediately, including the event that tripped the cap. (See *Edge cases* below for the oversized-single-event case.)
  - Shutdown: finalize the current window, then push.

### Commit-window timer

- The commit-window timer is a **silence detector for the currently open window**.
- Every event appended to the open window resets the timer (existing behavior, preserved).
- A finalize triggered by an author or target change closes the open window and **stops** the timer. If the incoming event opens a new window, the timer is started fresh for that window.
- This avoids an obscure failure where rapid author flips would silently extend the window past the configured silence interval.

### Planner ([commit_planner.go](/workspaces/gitops-reverser/internal/git/commit_planner.go:185))

- `buildCommitPlan` becomes a trivial 1:1 projection from `[]PendingWrite` to `[]CommitUnit`. No regrouping, no event-walking. Inline it at the call site if that keeps things clearer; otherwise keep it as a small ordered map.
- Removed the grouped pending-write branch's call to `groupCommits` and the loop that emitted multiple units per pending write.

### Types and naming

- Renamed `PendingWriteGroupedWindow` to `PendingWriteCommit`, reflecting "one finalized commit-shaped pending write".
- Moved the useful `commitGroup` shape into [open_window.go](/workspaces/gitops-reverser/internal/git/open_window.go) as `openWindow`.
- Updated `CommitModePerEvent` docs: with `commitWindow=0` it finalizes each event immediately; otherwise events coalesce by author, target, and quiet-window boundaries.

## Files And Symbols To Delete

Delete outright:

- Deleted `internal/git/commit_groups.go`: `groupCommits`, `shouldStartNewCommitGroup`, `appendClosedCommitGroup`, and the `seenPathAuthor` machinery are gone.
- Deleted `internal/git/commit_groups_test.go`: the old planner-grouper tests were replaced by event-loop tests.
- Replaced the old `PendingWriteGroupedWindow` planner branch with a trivial `PendingWriteCommit` 1:1 projection.

Keep but repurpose:

- The useful accumulator shape now lives in [open_window.go](/workspaces/gitops-reverser/internal/git/open_window.go) as `openWindow`, `newOpenWindow`, `add`, `orderedEvents`, and `windowPathKey`.

Update / migrate (not delete):

- Kept `buildGroupedPendingWrite` as the finalize helper used by the open-window path and existing direct commit/executor tests.
- Updated [branch_worker_split_test.go](/workspaces/gitops-reverser/internal/git/branch_worker_split_test.go) with event-loop tests for eager finalize behavior.
- Updated [commit_test.go](/workspaces/gitops-reverser/internal/git/commit_test.go) to remove the `groupCommits` dependency.
- Updated [branch_worker_loop_test.go](/workspaces/gitops-reverser/internal/git/branch_worker_loop_test.go) for the renamed pending kind and open-window byte accounting.

## Edge Cases

- **Oversized single event vs. byte cap.** If one event by itself exceeds `--branch-buffer-max-bytes`, the open window finalizes with that one event and the resulting pending write can exceed the cap. The cap is enforced after append; it bounds retained memory across many events, not any single event's size.
- **Atomic interleave.** `Atomic` requests finalize the open window first, then append the atomic `PendingWrite`. This preserves arrival order.
- **Timer state on author/target flip.** Stop the timer when finalizing; start a fresh one when a new window opens. Do not let the old timer fire into a new open window.

## Consequences And Current Mis-Specifications

Problems fixed:

- The doc's "single queue" mental model did not match reality; the queue held raw events, and commit boundaries were invented later.
- The old grouped pending write was not actually one commit-shaped durability unit; it could expand into multiple commits during planning.
- Target isolation was guaranteed after planning, not at the live-window boundary.
- `commitWindow=0` was described as per-event, but a multi-event `WriteRequest` could still drain as one grouped pending write.

Deliberate behavior change with this simplification:

- Drop the special "same path was previously committed by a different author in the same flush" split rule.
- Example: `alice(F), bob(F), alice(X), alice(F)` becomes 3 commits, not 4.
- New result:
  - `alice(F)`
  - `bob(F)`
  - `alice(X, F)`
- Tradeoff:
  - Simpler and far more intuitive model.
  - Slightly less fine-grained history for cross-author same-path ping-pong inside one quiet window.
  - Authorship stays honest, arrival order stays honest, and the final tree semantics stay correct.

## Test Plan

Event-loop tests added:

- Author change finalizes immediately and starts a new open window.
- Target change finalizes immediately and starts a new open window.
- Same-author same-target repeated path edits collapse to one finalized window with latest content and first-seen resource order.
- `commitWindow=0` with multiple events in one `WriteRequest` produces one pending write per event.
- Atomic request finalizes the open window first and preserves pending-write order.
- Byte cap trip during append finalizes the current window including the tripping event.
- Author change while a commit-window timer is armed: the timer does not fire into the new window.

Replay determinism coverage:

- After a conflict resync, retained `[]PendingWrite` rebuilds as the **same number of commits in the same order** as the original local commit chain. No re-grouping, no re-splitting.

Regression test for the intentional behavior change:

- `alice(F), bob(F), alice(X), alice(F)` yields 3 commits, with the final Alice commit containing both `X` and `F` in first-seen path order (`X`, then `F`).

Executor tests stay mostly unchanged. The grouped-message-kind decision moves to the finalize step (one event in the finalized window → per-event message kind; multiple → grouped), not a second grouping pass.

## Assumptions

- Push cooldown, replay-on-conflict, and resolved target metadata retention are unchanged.
- The external `CommitModePerEvent` / `CommitModeAtomic` API shape is unchanged; this is an internal simplification, not an API redesign.
- "Simplify to one queue" is more important than preserving the cross-author path-collision split rule. If preserving that rule were mandatory, this simplification should not be done because that rule inherently reintroduces a second grouping decision point.

## Documentation Follow-Ups

[commit-window-refactor.md](/workspaces/gitops-reverser/docs/design/commit-window-refactor.md) has been updated:

- Replaced "author changes split commit groups only when the buffer later drains" with eager author/target finalize semantics.
- Removed the cross-author same-path collision rule from the grouping rules section.
- Redrew the flow diagram as: input event -> open window -> finalize triggers -> ordered pending writes -> commit execution -> push/replay.
- Updated the "Core Types" section to reflect `PendingWriteCommit` and the 1:1 PendingWrite-to-commit invariant.
