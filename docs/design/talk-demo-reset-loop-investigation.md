# Talk Demo Reset Loop Investigation

## Summary

The `test-e2e-talk` scenario currently shows repeated Git worktree resets and repeated initial snapshot reconciliations when the demo target is configured to watch the `vote` namespace plus cluster-scoped resources.

The current cluster state contains about `3105` watched resources, and roughly `3000` of those are `quizsubmissions`. That large object count does not look like the root cause by itself, but it appears to amplify the problem significantly once the reset/reconciliation loop starts.

## What We Observed

- Controller logs repeatedly show:
  - `Preparing branch for operations`
  - `Switching worktree to match remote`
  - `Reset hard to match remote`
  - `Starting reconciliation`
  - `Retrieved cluster state`
- These messages recur in tight loops, often within the same second.
- The live metrics endpoint showed:
  - `gitopsreverser_git_commit_queue_size 5100`
- The talk demo repository ends up receiving many live `quizsubmission` updates, while some pre-existing objects are missing from the initial snapshot export.

## Current High-Level Flow

### Main Control Flow

1. Audit requests enter the webhook handler.
2. Audit events can be enqueued into Redis/Valkey.
3. The watch manager seeds and watches matching Kubernetes resources.
4. Matching resources are routed into a `GitTargetEventStream`.
5. The `GitTargetEventStream` forwards events into a `BranchWorker`.
6. The `BranchWorker` batches events and serializes Git writes for one `(provider, branch)` pair.

### Important Components

- Audit ingestion:
  - [internal/webhook/audit_handler.go](/workspaces/gitops-reverser2/internal/webhook/audit_handler.go)
- Redis/Valkey-backed audit queue:
  - [internal/queue/redis_audit_queue.go](/workspaces/gitops-reverser2/internal/queue/redis_audit_queue.go)
- Watch manager:
  - [internal/watch/manager.go](/workspaces/gitops-reverser2/internal/watch/manager.go)
- Event router:
  - [internal/watch/event_router.go](/workspaces/gitops-reverser2/internal/watch/event_router.go)
- GitTarget startup event stream:
  - [internal/reconcile/git_target_event_stream.go](/workspaces/gitops-reverser2/internal/reconcile/git_target_event_stream.go)
- Per-branch serialized Git writer:
  - [internal/git/branch_worker.go](/workspaces/gitops-reverser2/internal/git/branch_worker.go)
- Worker lifecycle manager:
  - [internal/git/worker_manager.go](/workspaces/gitops-reverser2/internal/git/worker_manager.go)
- GitTarget lifecycle controller:
  - [internal/controller/gittarget_controller.go](/workspaces/gitops-reverser2/internal/controller/gittarget_controller.go)

## Queues and Buffers

### 1. Audit Queue

- Redis/Valkey stream-backed queue
- Used by the audit webhook path
- Source:
  - [internal/queue/redis_audit_queue.go](/workspaces/gitops-reverser2/internal/queue/redis_audit_queue.go)

### 2. GitTarget Startup Buffer

- In-memory buffer inside `GitTargetEventStream`
- Holds events while startup reconciliation is still in progress
- Source:
  - [internal/reconcile/git_target_event_stream.go](/workspaces/gitops-reverser2/internal/reconcile/git_target_event_stream.go)

### 3. Branch Worker Event Queue

- In-memory channel per `(provider, branch)`
- Queue size is currently `100`
- Source:
  - [internal/git/branch_worker.go](/workspaces/gitops-reverser2/internal/git/branch_worker.go#L47)
  - [internal/git/branch_worker.go](/workspaces/gitops-reverser2/internal/git/branch_worker.go#L71)

### 4. Branch Worker Batch Buffer

- In-memory slice used before commit/push
- Events are accumulated and flushed on interval or threshold
- Source:
  - [internal/git/branch_worker.go](/workspaces/gitops-reverser2/internal/git/branch_worker.go#L401)

## Main Findings

### Finding 1: Initial Snapshot Reconciliation Restarts Repeatedly

The `GitTarget` reconciler restarts initial snapshot reconciliation on every reconcile pass.

- Source:
  - [internal/controller/gittarget_controller.go](/workspaces/gitops-reverser2/internal/controller/gittarget_controller.go#L477)
  - [internal/controller/gittarget_controller.go](/workspaces/gitops-reverser2/internal/controller/gittarget_controller.go#L499)

What happens now:

- `evaluateSnapshotGate()` creates or reuses a reconciler
- It immediately calls `StartReconciliation()`
- There is no guard for:
  - snapshot already completed
  - same target generation
  - same provider/branch/path inputs

Impact:

- repeated `RequestClusterState`
- repeated `RequestRepoState`
- repeated `Retrieved cluster state`
- repeated Git prep/reset activity downstream

### Finding 2: Bootstrap Git Preparation Also Re-Runs on Every Reconcile

The bootstrap gate always re-enters branch preparation work.

- Source:
  - [internal/controller/gittarget_controller.go](/workspaces/gitops-reverser2/internal/controller/gittarget_controller.go#L428)
  - [internal/controller/gittarget_controller.go](/workspaces/gitops-reverser2/internal/controller/gittarget_controller.go#L445)
  - [internal/git/branch_worker.go](/workspaces/gitops-reverser2/internal/git/branch_worker.go#L207)
  - [internal/git/branch_worker.go](/workspaces/gitops-reverser2/internal/git/branch_worker.go#L229)

What happens now:

- `EnsureWorker()` is called every reconcile
- `EnsureTargetBootstrapped()` is called every reconcile
- bootstrap prep calls `PrepareBranch()`

Impact:

- more `Preparing branch for operations`
- more `Switching worktree to match remote`
- more `Reset hard to match remote`

### Finding 3: Initial Snapshot Events Are Not Hydrated With Objects

The startup reconciliation path identifies resources, but the later event path expects actual object payloads.

- Source:
  - [internal/reconcile/git_target_event_stream.go](/workspaces/gitops-reverser2/internal/reconcile/git_target_event_stream.go#L160)
  - [internal/watch/event_router.go](/workspaces/gitops-reverser2/internal/watch/event_router.go#L163)

What happens now:

- startup reconciliation emits identifier-only events
- the event stream drops non-delete events with `event.Object == nil`
- `ReconcileResource` currently only logs and returns

Impact:

- pre-existing resources from the initial snapshot do not reliably land in Git
- live watched updates can still show up later
- this matches the observed behavior where `quizsubmissions` arrive, but some initial objects are missing

### Finding 4: The Large `quizsubmission` Set Likely Amplifies the Loop

The current talk demo environment contains about `3000` `quizsubmission` resources out of about `3105` total watched resources.

Observed effect:

- every unnecessary snapshot restart rescans thousands of resources
- every unnecessary enqueue adds more pressure to the branch-worker path
- the queue-size metric climbed to `5100`

Current interpretation:

- the large `quizsubmission` set is probably not the original logic bug
- it does appear to make the bug much more visible and much more expensive
- it may also expose secondary issues such as queue pressure, dedupe gaps, and slow drain behavior

### Finding 5: Single-Writer Design Appears Intact

The evidence currently supports a single writer path, not multiple competing writers.

Runtime observations:

- one controller pod is running
- deployment replica count is `1`
- ready replica count is `1`

Design evidence:

- one `BranchWorker` exists per `(provider namespace, provider name, branch)` key
- writes are serialized by `repoMu`
- the worker-manager tests explicitly assert one worker for shared repo+branch

Relevant sources:

- [internal/git/worker_manager.go](/workspaces/gitops-reverser2/internal/git/worker_manager.go#L92)
- [internal/git/branch_worker.go](/workspaces/gitops-reverser2/internal/git/branch_worker.go#L84)
- [internal/git/worker_manager_test.go](/workspaces/gitops-reverser2/internal/git/worker_manager_test.go#L179)

Important limitation:

- the `RepoBranchActiveWorkers` metric is declared but does not appear to be wired up, so we do not currently have a runtime metric proving active worker count.

### Finding 6: The Queue-Size Metric Needs Careful Interpretation

The live metric reported:

- `gitopsreverser_git_commit_queue_size 5100`

However:

- I found production increments for this metric in watch/seed code
- I did not find corresponding production decrements in the writer path

Relevant sources:

- [internal/watch/manager.go](/workspaces/gitops-reverser2/internal/watch/manager.go#L608)
- [internal/watch/informers.go](/workspaces/gitops-reverser2/internal/watch/informers.go)

Interpretation:

- the metric is still a strong sign of churn
- it should not currently be treated as an exact live queue depth without fixing its accounting

## Why The Current Logs Make Sense

The repeated log sequence:

- `Reset hard to match remote`
- `Starting reconciliation`
- `Retrieved cluster state`
- `Preparing branch for operations`
- `Reset hard to match remote`

is consistent with this loop:

1. `GitTarget` reconciles again
2. snapshot gate starts reconciliation again
3. bootstrap/status path re-enters branch preparation again
4. snapshot asks for cluster state again
5. branch prep resets the local worktree again
6. the large `quizsubmission` population makes each loop expensive

## Fix Plan

### 1. Make Snapshot Startup One-Shot or Input-Aware

Goal:

- do not call `StartReconciliation()` on every reconcile

Expected approach:

- persist enough status to know whether snapshot already completed for the current target inputs
- re-run only when a relevant input changes:
  - generation
  - provider
  - branch
  - path
  - possibly watch configuration generation

### 2. Stop Re-Running Bootstrap Preparation on Status-Only Reconciles

Goal:

- avoid repeated `PrepareBranch()` calls when bootstrap is already satisfied

Expected approach:

- make bootstrap idempotence visible in status or in-memory state
- skip `EnsureTargetBootstrapped()` unless bootstrap inputs changed

### 3. Hydrate Initial Snapshot Events With Real Objects

Goal:

- ensure pre-existing cluster resources are written into Git during startup sync

Expected approach:

- either return full objects from the cluster-state path
- or implement `ReconcileResource` so it fetches and routes the actual object

### 4. Re-Test With the Large `quizsubmission` Set

Goal:

- validate behavior under the current realistic talk-demo scale

Specific check:

- keep the current `~3000` `quizsubmissions`
- verify the reset loop stops
- verify queue pressure falls
- verify initial objects and live `quizsubmissions` both land in Git

### 5. Improve Observability

Goal:

- make the next investigation cheaper

Expected follow-ups:

- wire up active worker metrics
- fix queue-size accounting
- add targeted logs around:
  - snapshot start
  - snapshot completion
  - bootstrap skip vs bootstrap execute
  - worker creation / reuse

## Recommended Validation After Fix

1. Run `make lint`
2. Run the targeted talk flow
3. Check controller logs for disappearance of the reset loop
4. Check that the initial snapshot writes pre-existing objects
5. Check that live `quizsubmission` traffic still flows
6. Confirm queue metrics behave plausibly under the `3000`-resource load
