# Plan: Unify Event Handling Around One Batch Type

## Summary

Refactor the write pipeline so there is only one queued/writeable event container: a batch type with `[]Event`, used for both live events and reconciliation batches.

A single live event should become a batch with one item.

Bootstrap should stay as file preparation behavior, but we should be explicit that we **do not need a separate init commit**. The tests that matter mostly assert that bootstrap files exist when needed, not that they were created in a dedicated `chore(bootstrap): initialize path {path}` commit.

## Why This Refactor Is Worth Doing

The current code still carries two parallel abstractions:

- a single live event path,
- a multi-event reconcile batch path.

That duplication shows up in:

- queue item types,
- branch worker enqueue APIs,
- branch worker commit/push functions,
- git write entrypoints,
- commit generation helpers,
- tests and mocks.

Unifying around one batch/request type reduces code duplication and makes it easier to add future grouping strategies without creating yet another parallel path.

## Current Code Shape

### Current internal types

Today we still have:

- [`Event`](/workspaces/gitops-reverser/internal/git/types.go)
- [`ReconcileBatch`](/workspaces/gitops-reverser/internal/git/types.go)
- [`WorkItem{Single,Batch}`](/workspaces/gitops-reverser/internal/git/types.go)

And the branch worker still exposes two enqueue APIs:

- [`Enqueue(event Event)`](/workspaces/gitops-reverser/internal/git/branch_worker.go)
- [`EnqueueBatch(batch *ReconcileBatch)`](/workspaces/gitops-reverser/internal/git/branch_worker.go)

### Current duplicated execution paths

In the branch worker:

- [`commitAndPush(events []Event)`](/workspaces/gitops-reverser/internal/git/branch_worker.go)
- [`commitAndPushBatch(batch *ReconcileBatch)`](/workspaces/gitops-reverser/internal/git/branch_worker.go)

In git write logic:

- [`WriteEventsWithContentWriter(...)`](/workspaces/gitops-reverser/internal/git/git.go)
- [`WriteBatchWithContentWriter(...)`](/workspaces/gitops-reverser/internal/git/git.go)
- [`generateCommitsFromEvents(...)`](/workspaces/gitops-reverser/internal/git/git.go)
- [`generateBatchCommit(...)`](/workspaces/gitops-reverser/internal/git/git.go)

The behavior is similar, but the code is split because one path means “many commits from many events” and the other means “one atomic commit for the whole batch”.

## Proposed Design

### 1. Replace `WorkItem{Single,Batch}` with one request type

Create one internal write request type in [`internal/git/types.go`](/workspaces/gitops-reverser/internal/git/types.go), for example:

- `Events []Event`
- `CommitMessage string`
- `GitTargetName`
- `GitTargetNamespace`
- `BootstrapOptions`
- `CommitMode`

Suggested `CommitMode` values:

- `PerEvent`
- `Atomic`

This keeps the semantic difference without keeping two separate transport types.

### 2. Make BranchWorker accept one queue shape

Replace:

- `Enqueue(event Event)`
- `EnqueueBatch(batch *ReconcileBatch)`

with one enqueue method that accepts the unified request type.

Behavior mapping:

- live watch event -> request with `Events` length 1 and `CommitMode=PerEvent`
- flushed live buffer -> request with `Events` length N and `CommitMode=PerEvent`
- reconcile output -> request with `Events` length N and `CommitMode=Atomic`

### 3. Collapse branch worker execution to one commit/push path

Replace:

- `commitAndPush(...)`
- `commitAndPushBatch(...)`

with one function, for example:

- `commitAndPushRequest(req *WriteRequest)`

That function should own:

- GitProvider/auth lookup
- target encryption resolution
- bootstrap option resolution
- writer configuration
- delegation into the unified git write function

### 4. Collapse `git.go` to one real write implementation

Keep a single internal write entrypoint that handles:

- branch checkout/reset
- bootstrap staging
- event application
- commit creation
- push/conflict retry

Recommended shape:

- keep `WriteEvents(...)` temporarily as a thin compatibility wrapper
- internally translate to the unified request type
- route all real work through one implementation

The main simplifications should be:

- merge `tryWriteEventsAttempt` and `tryWriteBatchAttempt`
- merge `generateCommitsFromEvents` and `generateBatchCommit`
- branch only on `CommitMode`

### 5. Preserve current commit semantics

This refactor should not change visible behavior:

- `PerEvent` still means one commit per event
- `Atomic` still means one commit for the whole reconcile batch

So this is a code-structure refactor, not a behavior refactor.

## Bootstrap Behavior Decision

We should keep bootstrap as path-scoped file preparation, but we should **not** add back a separate init commit.

That means:

- bootstrap files can still be written/staged when needed,
- they should be committed together with the first relevant real write,
- there is no need for a dedicated `chore(bootstrap): initialize path {path}` commit.

### Why this is safe

The current code already works this way:

- [`EnsurePathBootstrapped(...)`](/workspaces/gitops-reverser/internal/git/branch_worker.go) prepares missing bootstrap files locally
- [`ensureBootstrapTemplateInPath(...)`](/workspaces/gitops-reverser/internal/git/bootstrapped_repo_template.go) writes and stages them in the worktree
- the actual commit happens later in [`WriteEventsWithContentWriter(...)`](/workspaces/gitops-reverser/internal/git/git.go) or [`WriteBatchWithContentWriter(...)`](/workspaces/gitops-reverser/internal/git/git.go)

### Test impact

The existing tests mostly depend on bootstrap side effects, not on a dedicated bootstrap commit:

- e2e tests check that `.sops.yaml` exists in the repo path when encryption is used
- unit tests check that bootstrap files are prepared and preserved correctly
- current tests do **not** appear to assert the dedicated bootstrap commit message
- current tests do **not** appear to require bootstrap to exist as a separate commit in git history

So the event unification refactor does not need to preserve or restore a standalone bootstrap commit.

## Recommended Sequence

### Step 1. Keep bootstrap behavior as-is

Do not introduce a separate bootstrap commit.

Goal:

- bootstrap files continue to be prepared/staged when needed
- they continue to be committed with the first relevant write
- no extra bootstrap-only git history behavior is introduced

### Step 2. Refactor event handling to one request type

- remove `WorkItem{Single,Batch}`
- remove `Enqueue` vs `EnqueueBatch`
- remove `commitAndPush` vs `commitAndPushBatch`
- remove `WriteEvents...` vs `WriteBatch...` duplication internally

### Step 3. Keep visible behavior unchanged

Only the internal event transport and write plumbing should change.

## Test Plan

- Update unit tests in `internal/git` to validate both `PerEvent` and `Atomic` modes through the unified request path.
- Update branch worker tests to assert live events and reconcile batches both enter the same queue type.
- Update reconcile/event stream tests and mocks to use one enqueue method.
- Keep bootstrap-related assertions focused on file presence and idempotence, not on a dedicated bootstrap commit.
- Run:
  - `make lint`
  - `make test`
  - `make test-e2e`
  - `make test-e2e-quickstart-manifest`
  - `make test-e2e-quickstart-helm`

## Assumptions

- `Event` remains the per-resource payload type.
- The new unified request type is internal only.
- Commit semantics (`PerEvent` vs `Atomic`) must remain unchanged during this refactor.
- We do not need a separate init commit for bootstrap.
