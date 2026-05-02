# PR 1: Stop Fabricating Reconcile Authorship

> Status: finished
> Date: 2026-05-01
> Scope: this PR only. Independent of [PR 2](/workspaces/gitops-reverser/docs/design/pr2-collapse-commit-signature-builders.md). No file conflicts. Either PR can land first.
> Verification: implemented in `folder_reconciler.go`; covered by `TestFolderReconciler_EmitsSingleBatch`.

## Summary

The folder reconciler currently fabricates a fake user identity (`UserInfo.Username = "gitops-reverser"`) on every event it produces. This is an architectural leak: the reconciler is asserting an identity it has no business asserting. Authorship attribution is a git-layer concern.

This PR removes the fabrication and converts reconcile snapshots to declared batches (`CommitModeAtomic`), so the git layer's existing batch path attributes the commit to the operator while still resolving the commit subject from the GitProvider `batchTemplate`.

## Architectural Principle

**Producers describe *what* changed and *why*; the git layer decides *how* it gets attributed.**

- The reconciler **describes the change**: events with resource data.
- The reconciler **declares intent**: `WriteRequest.CommitMode = CommitModeAtomic`.
- The reconciler **does not** fabricate `UserInfo`. It has no opinion on attribution.
- The git layer's existing atomic path uses operator-as-author for batch commits (today's `commitOptionsForBatch`). No git-layer change is needed.

## Files Changed

- [internal/reconcile/folder_reconciler.go](/workspaces/gitops-reverser/internal/reconcile/folder_reconciler.go) — three event constructors and one `WriteRequest` constructor.
- Tests: see *Test Plan* below. Today's [folder_reconciler_test.go](/workspaces/gitops-reverser/internal/reconcile/folder_reconciler_test.go) does not assert on the fabricated `UserInfo`, so the test surface is small.

No git-layer changes. No `internal/git/` files modified.

## Concrete Changes

### Delete the three `UserInfo` fabrications

In [folder_reconciler.go:172-198](/workspaces/gitops-reverser/internal/reconcile/folder_reconciler.go#L172-L198), there are three `git.Event{...}` literals, each with `UserInfo: git.UserInfo{Username: "gitops-reverser"}`. Delete that field from each:

- Line 178 (the `toCreate` loop, `Operation: "CREATE"`).
- Line 186 (the `toDelete` loop, `Operation: "DELETE"`).
- Line 196 (the `existingInBoth` loop, `Operation: events.ReconcileResource`).

After deletion, each event has zero-valued `UserInfo` (empty `Username`, empty `UID`).

### Convert the WriteRequest to a declared batch

In [folder_reconciler.go:200-202](/workspaces/gitops-reverser/internal/reconcile/folder_reconciler.go#L200-L202), the request is currently constructed as:

```go
request := git.WriteRequest{
    Events: batchEvents,
}
```

This silently uses `CommitMode = CommitModePerEvent` (the zero value), which is conceptually wrong: a reconcile snapshot is a single logical operation, not a stream of independent edits. Today the per-event path happens to coalesce them into one or more commits because all events share the fabricated `Username="gitops-reverser"` author.

Change to:

```go
request := git.WriteRequest{
    Events:        batchEvents,
    CommitMode:    git.CommitModeAtomic,
}
```

Do not set `CommitMessage` here. Leaving it empty lets the GitProvider's configured `commit.message.batchTemplate` control the subject, including user-supplied templates used by signing/e2e scenarios.

The `fmt` import is already present in the file (verify before adding).

## Behavior Change

Before this PR, reconcile commits show:

```
Author: gitops-reverser <gitops-reverser@cluster.local>
Committer: <operator config>
```

After this PR, reconcile commits show:

```
Author: <operator config>
Committer: <operator config>
```

This is intentional and correct: the operator (via its configured committer identity) is the actual author of a reconcile snapshot. Today's "gitops-reverser" string was a fabrication that happened to resemble a real user.

This is a **visible change in commit history** going forward. Existing commits in any repo are unaffected.

## Test Plan

### Tests to add

In [folder_reconciler_test.go](/workspaces/gitops-reverser/internal/reconcile/folder_reconciler_test.go) or a new test alongside it, assert the externally observable behavior:

1. **`UserInfo` is no longer fabricated.** Capture the `WriteRequest` emitted by a reconcile run with at least one create, one delete, and one existing-in-both resource. Assert that every emitted `Event.UserInfo.Username` is empty.
2. **`CommitMode` is `Atomic`.** Capture the same `WriteRequest`. Assert `request.CommitMode == git.CommitModeAtomic`.
3. **`CommitMessage` remains empty.** Assert `request.CommitMessage == ""` so the GitProvider `batchTemplate` remains authoritative.

These three assertions can live in one test method that exercises the full diff-and-emit flow with mocked dependencies. The existing test infrastructure should be reusable.

### Tests to verify still pass

Run the full reconcile package test suite plus any integration tests that go end-to-end through git:

- `go test ./internal/reconcile/... -count=1`
- `go test ./internal/git/... -count=1` (no changes here, but verify nothing depended on the fabricated UserInfo flowing through)

Look for any test asserting `UserInfo.Username == "gitops-reverser"` or any commit-author assertion that expects "gitops-reverser" in reconcile flows. There should be none in `internal/reconcile/` based on the current grep, but verify.

If an existing integration test asserts that reconcile commits have `Author = gitops-reverser`, it must be updated to assert `Author == <operator>` instead. That is the correct new behavior.

## Acceptance Criteria

- `grep -rn 'Username: "gitops-reverser"' internal/` returns zero matches.
- `grep -rn 'gitops-reverser' internal/reconcile/` returns zero matches in non-test, non-comment code.
- The `WriteRequest` emitted by `FolderReconciler` always has `CommitMode == CommitModeAtomic` and an empty `CommitMessage` whenever `total > 0`.
- All existing tests in `internal/reconcile/...` and `internal/git/...` pass.
- A new test asserts the three properties above (no fabrication, atomic mode, template-resolved message).

## Out Of Scope

- Any change to `internal/git/`. The git layer's atomic path already handles operator-as-author correctly via `commitOptionsForBatch`.
- Changes to other producers. [git_target_event_stream.go](/workspaces/gitops-reverser/internal/reconcile/git_target_event_stream.go) is unchanged — it carries real k8s audit identity through `event.UserInfo` and that's the right behavior.
- Anything related to `PendingWriteKind`, `CommitMode` deletion, or the unified-PendingWrite design (those are tracked separately in [commit-window-cleanup-q2.md](/workspaces/gitops-reverser/docs/design/commit-window-cleanup-q2.md) but are explicitly deferred — do not pull them into this PR).

## Coordination With PR 2

PR 2 collapses the three `commitOptionsFor*` builders into one. The two PRs do not share files and do not conflict. Either can land first. After both land, the behavior is identical to PR 1 alone landing — PR 2 is a behavior-preserving refactor of the signature path.
