# PR 2: Collapse Three Commit Signature Builders Into One

> Status: finished
> Date: 2026-05-01
> Scope: this PR only. Independent of [PR 1](/workspaces/gitops-reverser/docs/design/pr1-stop-fabricating-reconcile-authorship.md). No file conflicts. Either PR can land first.
> Verification: implemented in `internal/git/commit.go` and covered by `commit_executor_test.go` plus direct `commitOptionsFor` tests.

## Summary

The git layer has three near-duplicate signature builders — `commitOptionsForEvent`, `commitOptionsForGroup`, and `commitOptionsForBatch` — that differ only in how they pick the Author identity. Replace them with one `commitOptionsFor(pw, ...)` builder driven by the existing `PendingWrite.Author()` method.

This is a **pure refactor**. Behavior is preserved exactly. No commits change. No tests for externally observable behavior need to change.

## Why

Today three functions produce a `git.CommitOptions` with the same Committer (operator config) and the same Signer, differing only in Author:

| Function | Author Name | Author Email |
|---|---|---|
| `commitOptionsForEvent` | `event.UserInfo.Username` | `ConstructSafeEmail(...)` |
| `commitOptionsForGroup` | `pendingWrite.Author()` (= first event UserInfo for non-atomic) | `ConstructSafeEmail(...)` |
| `commitOptionsForBatch` | operator name | operator email |

`PendingWrite.Author()` already encodes the right rule: `""` for atomic, `events[0].UserInfo.Username` otherwise (see [pending_writes.go](/workspaces/gitops-reverser/internal/git/pending_writes.go)). The unified builder uses `Author()` directly: empty falls through to operator-as-both (matches `forBatch`); non-empty becomes the author with operator as committer (matches `forEvent` and `forGroup`).

## Files Changed

- [internal/git/commit.go](/workspaces/gitops-reverser/internal/git/commit.go) — delete the three builders, add one.
- [internal/git/commit_executor.go](/workspaces/gitops-reverser/internal/git/commit_executor.go) — update the three switch arms in `commitMetadata()` to call the unified builder.
- [internal/git/commit_executor_test.go](/workspaces/gitops-reverser/internal/git/commit_executor_test.go) — three existing assertions on `options.Author.Name` keep passing; nothing to change unless tests reference the deleted symbol names directly.

No reconcile-layer changes. No producer changes. No `pending_writes.go` changes (the existing `Author()` method is reused).

## Concrete Changes

### Add the unified builder in `internal/git/commit.go`

Place it next to the existing `operatorSignature` helper:

```go
// commitOptionsFor builds the CommitOptions for a pending write. The committer
// is always the operator. The author is pendingWrite.Author() (the first event's
// UserInfo for non-atomic, "" for atomic). When the author is empty, we mirror
// standard git's no-`--author` semantics: assign the committer signature to
// Author so the commit object has both fields populated with the same identity.
func commitOptionsFor(pw PendingWrite, config CommitConfig, signer git.Signer, when time.Time) *git.CommitOptions {
    committer := operatorSignature(config, when)
    author := pw.Author()
    if author == "" {
        return &git.CommitOptions{
            Author:    committer,
            Committer: committer,
            Signer:    signer,
        }
    }
    return &git.CommitOptions{
        Author: &object.Signature{
            Name:  author,
            Email: ConstructSafeEmail(author, "cluster.local"),
            When:  when,
        },
        Committer: committer,
        Signer:    signer,
    }
}
```

### Delete the three old builders in `internal/git/commit.go`

- `commitOptionsForEvent` at [commit.go:148-158](/workspaces/gitops-reverser/internal/git/commit.go#L148-L158).
- `commitOptionsForBatch` at [commit.go:160-167](/workspaces/gitops-reverser/internal/git/commit.go#L160-L167).
- `commitOptionsForGroup` at [commit.go:174-189](/workspaces/gitops-reverser/internal/git/commit.go#L174-L189).

### Update `commitMetadata` in `internal/git/commit_executor.go`

In [commit_executor.go:67-90](/workspaces/gitops-reverser/internal/git/commit_executor.go#L67-L90), the switch has three arms each calling its kind-specific builder. Update each to call the unified builder:

```go
func (p PendingWrite) commitMetadata() (string, *gogit.CommitOptions, error) {
    when := time.Now()
    switch p.MessageKind() {
    case CommitMessagePerEvent:
        if len(p.Events) != 1 {
            return "", nil, errors.New("per-event commit unit requires exactly one event")
        }
        message, err := renderEventCommitMessage(p.Events[0], p.CommitConfig)
        if err != nil {
            return "", nil, err
        }
        return message, commitOptionsFor(p, p.CommitConfig, p.Signer, when), nil
    case CommitMessageBatch:
        message, err := renderBatchCommitMessage(p.Events, p.CommitMessage, p.Target().Name, p.CommitConfig)
        if err != nil {
            return "", nil, err
        }
        return message, commitOptionsFor(p, p.CommitConfig, p.Signer, when), nil
    case CommitMessageGrouped:
        message, err := renderGroupCommitMessage(p, p.CommitConfig)
        if err != nil {
            return "", nil, err
        }
        return message, commitOptionsFor(p, p.CommitConfig, p.Signer, when), nil
    default:
        return "", nil, fmt.Errorf("unsupported commit message kind %q", p.MessageKind())
    }
}
```

The message-rendering branches stay separate (they call three different render functions). Only the signature path is unified.

The kind-specific signature path is now visibly redundant (all three arms call the same builder). A follow-up could simplify the switch further, but that's out of scope for this PR.

## Behavior Preservation

Walk through each case to prove the unified builder matches today:

**Per-event (one audit event, `UserInfo.Username = "alice"`):**
- Today: `commitOptionsForEvent(event, ...)` → Author={alice, safeEmail}, Committer=operator.
- After: `pw.Author() = "alice"` → unified builder takes the non-empty branch → Author={alice, safeEmail}, Committer=operator. ✅

**Grouped (multi-event audit window, all `UserInfo.Username = "alice"`):**
- Today: `commitOptionsForGroup(pw, ...)` → uses `pw.Author()` = "alice" → Author={alice, safeEmail}, Committer=operator.
- After: same `pw.Author()` returns "alice" → same Author, same Committer. ✅

**Batch (atomic snapshot, regardless of event UserInfo content):**
- Today: `commitOptionsForBatch(...)` → Author=operator, Committer=operator.
- After: `pw.Author()` returns `""` for `Kind == PendingWriteAtomic` (unchanged from cleanup-q1) → unified builder takes the empty branch → Author=committer=operator. ✅

This holds whether or not [PR 1](/workspaces/gitops-reverser/docs/design/pr1-stop-fabricating-reconcile-authorship.md) has landed. PR 1 changes the *event UserInfo content* for reconcile (empty instead of "gitops-reverser"), but `pw.Author()` for atomic ignores event UserInfo entirely (returns empty based on `Kind`). So PR 2 is independent of PR 1.

## Test Plan

### Tests to verify still pass

The existing assertions in [commit_executor_test.go](/workspaces/gitops-reverser/internal/git/commit_executor_test.go) cover all three signature paths:

- Line 115-116: per-event → `options.Author.Name == "alice"`, `options.Committer.Name == DefaultCommitterName`.
- Line 136-137: grouped → same shape.
- Line 159-160: batch → `options.Author.Name == DefaultCommitterName`, `options.Committer.Name == DefaultCommitterName`.

All three must still pass after this PR. If they do, behavior preservation is proven for the three documented paths.

Run:

- `go test ./internal/git/... -count=1`
- `go test ./internal/reconcile/... -count=1` (verify no regressions in producer-side flows)
- `go test ./... -count=1` for the full suite if practical

### Tests to add (small)

Add one positive test for the new builder's empty-author fall-through that doesn't depend on going through the executor:

```go
func TestCommitOptionsFor_EmptyAuthorFallsThroughToCommitter(t *testing.T) {
    config := ResolveCommitConfig(nil) // produces operator defaults
    pw := PendingWrite{
        Kind: PendingWriteAtomic,
        // No events set → pw.Author() returns ""
    }
    when := time.Now()
    opts := commitOptionsFor(pw, config, nil, when)

    require.NotNil(t, opts.Author)
    require.NotNil(t, opts.Committer)
    assert.Equal(t, opts.Committer.Name, opts.Author.Name,
        "empty author should fall through to committer")
    assert.Equal(t, opts.Committer.Email, opts.Author.Email)
    assert.Equal(t, DefaultCommitterName, opts.Author.Name)
}
```

Optional second test that asserts the non-empty branch for completeness:

```go
func TestCommitOptionsFor_NonEmptyAuthorIsHonored(t *testing.T) {
    config := ResolveCommitConfig(nil)
    pw := PendingWrite{
        Kind:   PendingWriteCommit,
        Events: []Event{{UserInfo: UserInfo{Username: "alice"}}},
    }
    when := time.Now()
    opts := commitOptionsFor(pw, config, nil, when)

    assert.Equal(t, "alice", opts.Author.Name)
    assert.Equal(t, DefaultCommitterName, opts.Committer.Name)
    assert.NotEqual(t, opts.Author.Name, opts.Committer.Name)
}
```

These are optional because the existing executor tests already cover the same paths end-to-end. Add them if the unified builder feels under-tested in isolation.

## Acceptance Criteria

- `grep -rn 'commitOptionsForEvent\|commitOptionsForGroup\|commitOptionsForBatch' internal/` returns zero matches.
- One new function `commitOptionsFor(pw PendingWrite, ...)` in `internal/git/commit.go`.
- The three switch arms in [commit_executor.go:67-90](/workspaces/gitops-reverser/internal/git/commit_executor.go#L67-L90) all call the unified builder (the kind-specific signature dispatch is gone).
- All existing tests in `internal/git/...` pass without modification.
- Existing assertions on `options.Author.Name` and `options.Committer.Name` at [commit_executor_test.go:115,116,136,137,159,160](/workspaces/gitops-reverser/internal/git/commit_executor_test.go#L115) still pass.
- (Optional but recommended) A new test for the unified builder's empty-author fall-through.

## Out Of Scope

- Anything outside `internal/git/`. No producer changes.
- Deleting `PendingWriteKind`, `PendingWriteAtomic`, `PendingWriteCommit`. The existing two-kind enum stays. `PendingWrite.Author()` still uses `Kind` internally to decide whether to read event UserInfo.
- Deleting `CommitMode`, `CommitModeAtomic`, `CommitModePerEvent`. Out of scope.
- Collapsing the message-rendering switch arms (they call three different render functions and each has its own template logic). Out of scope.
- Any change to event-loop, push, replay, or window logic.

## Coordination With PR 1

PR 1 stops fabricating `UserInfo` in the reconciler. The two PRs do not share files and do not conflict. Either can land first.

- **If PR 2 lands first**: the unified builder produces operator-as-author for atomic (today's `commitOptionsForBatch` behavior preserved) regardless of what `UserInfo` content the producer fabricates. Reconcile commits continue to show `Author = gitops-reverser` until PR 1 lands. No behavior regression.
- **If PR 1 lands first**: reconcile commits switch from `Author = gitops-reverser` to `Author = operator` (PR 1's intentional behavior change). The three old signature builders still exist; the unified one isn't there yet. No regression in PR 2's domain.
- **After both land**: behavior is identical to PR 1's outcome. PR 2's signature collapse is invisible from outside the package.
