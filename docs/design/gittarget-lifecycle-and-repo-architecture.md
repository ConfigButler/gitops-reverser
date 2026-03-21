# GitTarget Lifecycle And Repo Architecture

## Purpose

This document describes two things:

1. where the local Git working repository is created and managed
2. what the current `GitTarget` lifecycle means, step by step

It also explains:

- what `GitTargetConditionValidated` means
- when the controller moves from one condition to the next
- where the generated SOPS age Secret is created
- how bootstrap files fit into the current design

## High-Level Model

Today the system has three related layers:

1. `GitTarget` controller lifecycle
2. branch worker lifecycle
3. local repository/worktree lifecycle

These are related, but they are not the same thing.

### 1. GitTarget lifecycle

This is the user-visible state machine exposed in `GitTarget.status.conditions`.

The main conditions are defined in:

- [internal/controller/gittarget_controller.go](/workspaces/gitops-reverser/internal/controller/gittarget_controller.go)

Current lifecycle conditions:

- `Ready`
- `Validated`
- `EncryptionConfigured`
- `SnapshotSynced`
- `EventStreamLive`

### 2. Branch worker lifecycle

A branch worker is the in-memory component that serializes writes for one:

- provider namespace
- provider name
- branch

It is managed by:

- [internal/git/worker_manager.go](/workspaces/gitops-reverser/internal/git/worker_manager.go)

### 3. Local repository/worktree lifecycle

Each branch worker uses a local repository checkout under `/tmp`.

This is managed by:

- [internal/git/branch_worker.go](/workspaces/gitops-reverser/internal/git/branch_worker.go)
- [internal/git/git.go](/workspaces/gitops-reverser/internal/git/git.go)

## Where The Local Git Repository Is Created

The local repository path is derived in the branch worker.

Relevant functions:

- [internal/git/branch_worker.go](/workspaces/gitops-reverser/internal/git/branch_worker.go)
  - `repoRootPath()`
  - `repoPathForRemote(remoteURL string)`

### Current path layout

The worker stores repositories under:

```text
/tmp/gitops-reverser-workers/<provider-namespace>/<provider-name>/<branch>/repos/<remote-url-hash>
```

This means:

- one logical worker owns one provider+branch combination
- the actual repo directory is keyed by a hash of the remote URL
- the repo directory is reused across operations for that worker

### Why the remote URL is hashed

The worker uses:

- `repoCacheKey(remoteURL string)`

to build a stable directory name from the remote URL.

That avoids path issues from raw URLs while still keeping one reusable local checkout per remote.

## Where The Local Repository Is Opened Or Initialized

The main entrypoint for repository preparation is:

- [internal/git/git.go](/workspaces/gitops-reverser/internal/git/git.go)
  - `PrepareBranch(...)`

### What `PrepareBranch(...)` does

`PrepareBranch(...)` is responsible for:

- ensuring the parent directory exists
- opening an existing local repository if present
- creating a clean repository if missing or broken
- ensuring the `origin` remote is set correctly
- syncing local branch state against the remote

This function does **not** just write files. It is the core “make sure local repo state is usable” operation.

### Where it is called from

The branch worker calls `PrepareBranch(...)` in three important places:

- `ensureRepositoryInitialized(...)`
- `prepareBootstrapRepository(...)`
- the main write/sync path before commit/push

Relevant locations:

- [internal/git/branch_worker.go](/workspaces/gitops-reverser/internal/git/branch_worker.go)

## What The Branch Worker Owns

The branch worker is the owner of:

- local repository synchronization
- worktree mutation
- bootstrapping files in the worktree
- batching or serializing events for commit/push

Important file:

- [internal/git/branch_worker.go](/workspaces/gitops-reverser/internal/git/branch_worker.go)

### Worker identity

A worker is keyed by:

- provider namespace
- provider name
- branch

This means multiple `GitTarget`s can share a worker if they write to different paths on the same branch.

### Worker manager

Workers are created and looked up by:

- [internal/git/worker_manager.go](/workspaces/gitops-reverser/internal/git/worker_manager.go)

Important functions:

- `EnsureWorker(...)`
- `GetWorkerForTarget(...)`
- `RegisterTarget(...)`
- `UnregisterTarget(...)`

## Exact Current GitTarget Lifecycle

The lifecycle is implemented in:

- [internal/controller/gittarget_controller.go](/workspaces/gitops-reverser/internal/controller/gittarget_controller.go)

The controller processes the gates in this order:

1. `Validated`
2. `EncryptionConfigured`
3. `SnapshotSynced`
4. `EventStreamLive`
5. `Ready`

`Ready=True` only happens after the earlier gates are satisfied.

## What `Validated` Means

`Validated` means:

- the referenced `GitProvider` exists
- the target branch matches the provider’s `allowedBranches`
- there is no path conflict with another `GitTarget` using the same provider+branch+path

This logic lives in:

- `evaluateValidatedGate(...)`
- `validateProviderAndBranch(...)`
- `checkForConflicts(...)`

all in:

- [internal/controller/gittarget_controller.go](/workspaces/gitops-reverser/internal/controller/gittarget_controller.go)

### What `Validated=True` does not mean

It does **not** mean:

- the local repo has been cloned
- a worker definitely exists
- the branch exists remotely
- bootstrap files are present
- snapshot reconciliation has completed

It only means that the target is logically allowed and conflict-free enough to continue.

### When the controller jumps to `Validated=True`

The controller sets `Validated=True` after:

- the `GitProvider` lookup succeeds
- branch pattern validation succeeds
- conflict detection succeeds

The exact status set is:

- `type: Validated`
- `status: True`
- `reason: OK`
- `message: Provider and branch validation passed`

### When `Validated=False` happens

Examples:

- provider not found
- branch does not match allowed patterns
- another earlier `GitTarget` already owns the same provider+branch+path

When `Validated=False`, later lifecycle conditions are explicitly blocked:

- `EncryptionConfigured=Unknown`
- `SnapshotSynced=Unknown`
- `EventStreamLive=Unknown`
- `Ready=False`

## What `EncryptionConfigured` Means

`EncryptionConfigured` means:

- if SOPS age encryption is not enabled, nothing more is required
- if SOPS age encryption is enabled, the encryption Secret exists and can be resolved into usable recipients

This logic lives in:

- `evaluateEncryptionGate(...)`
- `ensureEncryptionSecret(...)`

in:

- [internal/controller/gittarget_controller.go](/workspaces/gitops-reverser/internal/controller/gittarget_controller.go)

### When `EncryptionConfigured=True` happens

There are two main cases:

1. Encryption is not enabled

Then the condition becomes:

- `True`
- `NotRequired`

2. Encryption is enabled and valid

Then the condition becomes:

- `True`
- `OK`
- `Encryption configuration is valid`

### When `EncryptionConfigured=False` happens

Examples:

- referenced secret missing
- invalid secret contents
- invalid age configuration
- `generateWhenMissing` disabled but secret absent

If encryption is not configured correctly:

- `SnapshotSynced` stays not started
- `EventStreamLive` stays not started
- `Ready=False`

## Where The New SOPS Secret Is Created

Yes, this is currently part of the `GitTarget` controller lifecycle.

The generated age Secret is created by:

- `ensureEncryptionSecret(...)`
- `createGeneratedEncryptionSecret(...)`

in:

- [internal/controller/gittarget_controller.go](/workspaces/gitops-reverser/internal/controller/gittarget_controller.go)

### When this happens

It happens during the `EncryptionConfigured` gate, not during bootstrap and not in the branch worker.

So the flow is:

1. `GitTarget` enters encryption evaluation
2. if `generateWhenMissing=true`, controller checks whether the Secret exists
3. if it does not exist, controller generates an age identity
4. controller creates or updates the Kubernetes Secret
5. then the controller resolves encryption config again

### Important consequence

The generated SOPS Secret is:

- a Kubernetes resource concern
- handled by the `GitTarget` controller

It is **not** created by:

- bootstrap file staging
- the branch worker
- the git write path

## What `SnapshotSynced` Means

`SnapshotSynced` means:

- the initial cluster snapshot for this `GitTarget` path has been reconciled against the Git path

This is handled in:

- `evaluateSnapshotGate(...)`

and depends on:

- the event router
- a `GitTargetEventStream`
- a branch worker
- a folder reconciler

Relevant files:

- [internal/controller/gittarget_controller.go](/workspaces/gitops-reverser/internal/controller/gittarget_controller.go)
- [internal/reconcile/git_target_event_stream.go](/workspaces/gitops-reverser/internal/reconcile/git_target_event_stream.go)

### What happens in this gate

The controller:

1. ensures there is a `GitTargetEventStream`
2. puts it into reconciling/buffering mode
3. starts initial snapshot reconciliation
4. waits until both cluster state and repo state have been seen
5. records snapshot stats and completion time

### Why this gate matters

It prevents live watch events from racing ahead of the first consistent path sync.

## What `EventStreamLive` Means

`EventStreamLive` means:

- the `GitTargetEventStream` has transitioned from reconciliation/buffering mode into normal live processing

Handled in:

- `evaluateEventStreamGate(...)`

This is the final gate before `Ready=True`.

If it fails:

- `Ready=False`
- reason becomes stream-related, not validation-related

## What `Ready` Means

`Ready=True` means:

- `Validated=True`
- `EncryptionConfigured=True`
- `SnapshotSynced=True`
- `EventStreamLive=True`

and the controller sets:

- `reason: OK`
- `message: All lifecycle gates satisfied`

So `Ready` is now the final summary condition, while the other lifecycle conditions explain which stage is blocking.

## Where Bootstrap Fits In Now

Bootstrap is no longer a user-visible lifecycle condition.

There is no current `Bootstrapped` condition in the `GitTarget` status path.

### What bootstrap means now

Bootstrap is an internal worktree preparation step that makes sure:

- `README.md`
- `.sops.yaml` when needed

exist inside the target path in the local repository worktree.

Relevant code:

- [internal/git/branch_worker.go](/workspaces/gitops-reverser/internal/git/branch_worker.go)
  - `EnsurePathBootstrapped(...)`
  - `prepareBootstrapRepository(...)`
- [internal/git/bootstrapped_repo_template.go](/workspaces/gitops-reverser/internal/git/bootstrapped_repo_template.go)
  - `ensureBootstrapTemplateInPath(...)`

### Important distinction

Bootstrap is now:

- local repo/worktree preparation

not:

- a dedicated `GitTarget` phase
- a separate status gate
- a separate init commit

### Relationship to encryption

Bootstrap and encryption do work together, but indirectly:

- `GitTarget` controller resolves encryption configuration
- branch worker uses resolved recipients to decide whether `.sops.yaml` should be included
- bootstrap file rendering uses those recipients when writing `.sops.yaml`

So:

- the Secret is generated by the controller
- the `.sops.yaml` file is rendered by the git layer

They are related, but they are not created by the same subsystem.

## Current Repo/Bootstrap Flow In Practice

For a path that needs to be written:

1. branch worker prepares or reuses the local repo
2. branch worker calls `PrepareBranch(...)`
3. bootstrap files may be ensured in the local worktree
4. resource files are written or deleted
5. commit generation happens
6. push happens

This means the worktree is the place where bootstrap and resource files come together before commit.

## Important Current Architectural Tension

Right now the lifecycle assumes:

- a branch worker will exist by the time snapshot/event-stream setup runs

But the recent e2e investigation suggests that this assumption is currently broken in at least one path.

That is why the failing `GitTarget` gets stuck at:

- `Validated=True`
- `EncryptionConfigured=True`
- `SnapshotSynced=False`

with:

- `branch worker not found`

So the current architecture is:

- conceptually clear
- but still missing a reliable worker-materialization step before snapshot startup

## Short Answer Summary

### Where is the local Git repo working directory created and managed?

By the branch worker, under:

- `/tmp/gitops-reverser-workers/...`

using:

- `repoRootPath()`
- `repoPathForRemote()`
- `PrepareBranch(...)`

### What does `Validated` mean exactly?

It means:

- provider exists
- branch is allowed
- no conflicting `GitTarget` owns the same provider+branch+path

It does **not** mean repo/worker/bootstrap is ready yet.

### When do we jump to `Validated=True`?

Immediately after provider validation and conflict checks pass, before encryption and before snapshot.

### Where is the new SOPS Secret created?

In the `GitTarget` controller during the `EncryptionConfigured` gate, via:

- `ensureEncryptionSecret(...)`
- `createGeneratedEncryptionSecret(...)`

### Is that tied to bootstrap?

Only indirectly.

- the controller creates the Kubernetes Secret
- the git layer later uses the resolved recipients to render `.sops.yaml`

So bootstrap is no longer a lifecycle phase, but it still depends on encryption being resolved correctly.
