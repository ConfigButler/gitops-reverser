# Event Batch Refactor: E2E Investigation Handoff

## Summary

The event unification refactor is mostly in place and compiles, but full e2e is still blocked by one `GitTarget` readiness failure.

The failing scenario is:

- encrypted secret flow in [test/e2e/e2e_test.go](/workspaces/gitops-reverser/test/e2e/e2e_test.go)
- test name: `should commit encrypted Secret manifests when WatchRule includes secrets`

The important part is that the failure happens **before** the actual Secret commit verification.

The `GitTarget` never reaches `Ready=True`, so the test times out while waiting for readiness.

## Current Validation State

What passed:

- `make lint`
- `make test`
- `docker info`

What failed:

- `make test-e2e`

What was not run after the full e2e failure:

- `make test-e2e-quickstart-manifest`
- `make test-e2e-quickstart-helm`

## Refactor Scope That Landed

The internal event path was unified around one request type.

Main files touched:

- [internal/git/types.go](/workspaces/gitops-reverser/internal/git/types.go)
- [internal/git/branch_worker.go](/workspaces/gitops-reverser/internal/git/branch_worker.go)
- [internal/git/git.go](/workspaces/gitops-reverser/internal/git/git.go)
- [internal/reconcile/git_target_event_stream.go](/workspaces/gitops-reverser/internal/reconcile/git_target_event_stream.go)
- [internal/reconcile/git_target_event_stream_test.go](/workspaces/gitops-reverser/internal/reconcile/git_target_event_stream_test.go)
- [internal/reconcile/gittarget_lifecycle_integration_test.go](/workspaces/gitops-reverser/internal/reconcile/gittarget_lifecycle_integration_test.go)

The refactor introduced:

- `WriteRequest`
- `CommitMode`
- one branch-worker request pipeline for both live events and reconcile batches
- one git write implementation underneath the old wrappers

This refactor does **not** appear to be the direct cause of the failing e2e assertion.

## Exact Failing Assertion

The failing assertion is in the helper:

- [test/e2e/e2e_test.go:1817](/workspaces/gitops-reverser/test/e2e/e2e_test.go:1817)

The specific test starts here:

- [test/e2e/e2e_test.go:524](/workspaces/gitops-reverser/test/e2e/e2e_test.go:524)

The timeout occurs while waiting for:

- `gittarget/<dest>` to become `Ready=True`

The failing step is the first readiness check after creating:

- the `GitTarget`
- the `WatchRule`

This means the failure occurs before:

- creating the watched Secret
- patching the Secret
- verifying Git contents
- decrypting committed data

## What The Live GitTarget Looks Like While Failing

During a focused repro, the live object looked like this:

- `Validated=True`
- `EncryptionConfigured=True`
- `SnapshotSynced=False`
- `EventStreamLive=Unknown`
- `Ready=False`

Most important condition values:

```yaml
type: SnapshotSynced
status: "False"
reason: SnapshotFailed
message: branch worker not found for provider=<namespace>/gitprovider-normal branch=main
```

```yaml
type: Ready
status: "False"
reason: InitialSyncInProgress
message: SnapshotSynced gate failed: SnapshotFailed
```

So the real blocker is:

- not validation
- not encryption setup
- not the write path
- but the snapshot gate failing because no branch worker exists yet

## When It Happens In The Lifecycle

The observed order is:

1. `GitProvider` becomes `Ready=True`
2. `GitTarget` becomes `Validated=True`
3. `GitTarget` becomes `EncryptionConfigured=True`
4. `GitTarget` enters snapshot setup
5. `ensureEventStream(...)` cannot find a branch worker
6. `SnapshotSynced=False`
7. `Ready=False`

So the failure happens in the transition from:

- validation/encryption

to:

- snapshot/event-stream startup

## Relevant Controller Code

The failure path is in:

- [internal/controller/gittarget_controller.go](/workspaces/gitops-reverser/internal/controller/gittarget_controller.go)

Important functions:

- `evaluateSnapshotGate(...)`
- `ensureEventStream(...)`

The event stream path depends on finding a worker via:

- `WorkerManager.GetWorkerForTarget(...)`

If no worker exists, the snapshot gate fails immediately.

## Relevant Worker Code

Worker lifecycle is managed in:

- [internal/git/worker_manager.go](/workspaces/gitops-reverser/internal/git/worker_manager.go)

Important functions:

- `EnsureWorker(...)`
- `GetWorkerForTarget(...)`
- `RegisterTarget(...)`

Important observation:

- `RegisterTarget(...)` exists, but there is no active call site in the current codebase outside the manager itself

That means worker creation appears to have lost its earlier lifecycle hook.

## Most Likely Root Cause

The strongest current hypothesis is:

- after removing bootstrap as a visible lifecycle phase, we also lost the point where a branch worker was guaranteed to exist before snapshot/event-stream setup

In other words:

- `GitTarget` readiness now depends on a worker
- but no code is reliably materializing that worker before `SnapshotSynced` runs

This explains why the target gets stuck exactly at snapshot startup.

## What Was Tried Already

I added a fallback inside:

- [internal/controller/gittarget_controller.go](/workspaces/gitops-reverser/internal/controller/gittarget_controller.go)

The change attempts to:

- call `WorkerManager.EnsureWorker(...)` from `ensureEventStream(...)`
- then retry `GetWorkerForTarget(...)`

This was intended to lazily create the worker if the snapshot gate runs before worker registration.

### Result

After rebuilding the controller image and rerunning e2e:

- the same readiness failure still occurs
- the live `GitTarget` still reports:
  - `SnapshotFailed`
  - `branch worker not found`

This means one of the following is still true:

- the fallback path is not actually being exercised
- the worker manager is not usable from that path at runtime
- the worker is created but not retained/visible to `GetWorkerForTarget(...)`
- the controller is failing earlier than expected and the old error message is still the one being surfaced

## Important Log Findings

### Focused reduced repro

In a reduced run containing:

- normal GitProvider
- simple WatchRule reconciliation
- encrypted secret test

the live `GitTarget` clearly showed:

- `Validated=True`
- `EncryptionConfigured=True`
- `SnapshotSynced=False` due to missing branch worker

### Full e2e after image rebuild

Full e2e still fails on the same test.

The failing step is still the first readiness wait for:

- `watchrule-secret-encryption-test-dest`

However, the controller logs are noisy and not giving a clean trace for the `GitTarget` reconcile body. The most useful evidence remains the live `GitTarget.status.conditions`.

## Why This Does Not Look Like A Git Write Bug

This is probably not caused by the new unified `WriteRequest` path because:

- the failure happens before the watched Secret is created
- no event commit is attempted yet
- the readiness gate fails before any resource write path is exercised

So the investigation should focus on:

- `GitTarget` startup lifecycle
- worker creation timing
- snapshot gate assumptions

not on:

- commit generation
- atomic/per-event write behavior
- bootstrap file staging

## Strongest Next Steps

### 1. Restore explicit worker materialization before snapshot gate

Most likely fix:

- ensure the `GitTarget` reconcile path explicitly creates the branch worker before `evaluateSnapshotGate(...)`

This is cleaner than relying on event-stream fallback.

Places to inspect:

- [internal/controller/gittarget_controller.go](/workspaces/gitops-reverser/internal/controller/gittarget_controller.go)
- [internal/git/worker_manager.go](/workspaces/gitops-reverser/internal/git/worker_manager.go)

### 2. Confirm how worker creation used to happen

Useful question:

- was worker creation previously coupled to bootstrap, registration, or an earlier ready phase?

If yes, that behavior probably needs to come back in a simpler form.

### 3. Add temporary logging around worker creation

Useful logs:

- before calling `EnsureWorker(...)`
- after `EnsureWorker(...)`
- after `GetWorkerForTarget(...)`
- worker key string being used
- whether `WorkerManager.ctx` is initialized at that point

### 4. Verify manager context assumptions

`EnsureWorker(...)` starts workers using:

- `worker.Start(m.ctx)`

If `m.ctx` is unset or stale, worker startup may not succeed as expected.

That is worth checking directly.

## Secondary Observation

While testing locally, a direct package-level run of:

- `go test ./internal/controller ./internal/git ./internal/reconcile`

failed in the controller suite because `envtest` assets were missing:

- `/usr/local/kubebuilder/bin/etcd` not found

This is unrelated to the e2e blocker, but worth remembering when reproducing outside `make test`.

## Suggested Restart Point For Tomorrow

If starting fresh, I would begin here:

1. Open [internal/controller/gittarget_controller.go](/workspaces/gitops-reverser/internal/controller/gittarget_controller.go)
2. Add explicit worker creation before snapshot evaluation
3. Log the worker key and whether creation succeeds
4. Rerun the focused encrypted-secret scenario first
5. If that passes, rerun `make test-e2e`
6. Only then continue to:
   - `make test-e2e-quickstart-manifest`
   - `make test-e2e-quickstart-helm`

## Short Conclusion

The current blocker is best described as:

- a `GitTarget` lifecycle regression in worker creation timing

not as:

- a batch-write regression

The event refactor itself is mostly fine. The broken part is that the `GitTarget` snapshot gate now runs before a branch worker is guaranteed to exist, and the encrypted-secret e2e test is the first place where that consistently shows up.
