# E2E Situation Sketch: Encrypted Secret Readiness Failure

## Purpose

This is a compact sketch of the exact situation in the failing e2e test.

It is meant as a quick orientation document:

- where in the test we are
- which Kubernetes objects already exist
- which internal runtime objects are expected to exist
- what the test is waiting for
- what has not happened yet

## The Failing Test

The failing test is:

- [test/e2e/e2e_test.go:524](test/e2e/e2e_test.go:524)

Name:

- `should commit encrypted Secret manifests when WatchRule includes secrets`

The assertion eventually fails in:

- [test/e2e/e2e_test.go:1817](test/e2e/e2e_test.go:1817)

## Where We Are In The Test Timeline

At the point of failure, the test has done this already:

1. created the test namespace
2. created git credential Secret(s) in that namespace
3. created the `sops-age-key` Secret in that namespace
4. created a healthy `GitProvider`
5. created a new `GitTarget`
6. created a new `WatchRule`

Then the test waits for:

- `GitTarget Ready=True`

That wait fails.

## What Has Already Been Created

### Kubernetes objects that exist before the failure

These should already exist:

- namespace: `<seed>-test-manager`
- `Secret/git-creds-e2e-test-<seed>`
- `Secret/sops-age-key`
- `GitProvider/gitprovider-normal`
- `GitTarget/watchrule-secret-encryption-test-dest`
- `WatchRule/watchrule-secret-encryption-test`

### Controller-runtime / in-memory objects that are expected to exist

By this point, the system expects to have or be able to create:

- a branch worker for:
  - provider namespace = test namespace
  - provider name = `gitprovider-normal`
  - branch = `main`
- a `GitTargetEventStream`
- a snapshot reconciler for the GitTarget path

## What The Test Expects At This Moment

Right after creating the `GitTarget` and `WatchRule`, the test expects:

- `GitProvider` is already `Ready=True`
- `GitTarget` becomes `Ready=True`
- `WatchRule` becomes `Ready=True`

For the `GitTarget`, that implies these underlying conditions should settle to:

- `Validated=True`
- `EncryptionConfigured=True`
- `SnapshotSynced=True`
- `EventStreamLive=True`
- `Ready=True`

## What Actually Happens

The live `GitTarget` status shows:

- `Validated=True`
- `EncryptionConfigured=True`
- `SnapshotSynced=False`
- `EventStreamLive=Unknown`
- `Ready=False`

The blocking condition is:

```yaml
type: SnapshotSynced
status: "False"
reason: SnapshotFailed
message: branch worker not found for provider=<namespace>/gitprovider-normal branch=main
```

So the system has passed:

- provider validation
- branch validation
- conflict checking
- encryption setup

but fails when trying to start the initial snapshot/event-stream path.

## What Has Not Happened Yet

At the moment the failure occurs, the test has **not** yet done any of the following:

- created the watched Secret `test-secret-encryption`
- patched that Secret
- waited for a Git commit containing the encrypted Secret manifest
- verified `.sops.yaml` in the repo path
- decrypted the committed Secret

This is important because it means:

- the failing e2e is not yet exercising the main Secret write path
- the failure happens earlier, in GitTarget startup/lifecycle

## Situation Sketch

### Objects and expected dependencies

```text
GitProvider/gitprovider-normal
  -> should allow branch "main"
  -> should provide repo URL + git credentials

GitTarget/watchrule-secret-encryption-test-dest
  -> references GitProvider/gitprovider-normal
  -> path = e2e/secret-encryption-test
  -> branch = main
  -> encryption secret = sops-age-key
  -> should become:
     Validated=True
     EncryptionConfigured=True
     SnapshotSynced=True
     EventStreamLive=True
     Ready=True

WatchRule/watchrule-secret-encryption-test
  -> references the GitTarget
  -> should become Ready=True
  -> should drive watched resources into that GitTarget path
```

### What the controller seems to need at that point

```text
GitTarget Reconcile
  -> validate provider/branch/conflicts
  -> validate or generate encryption secret
  -> ensure branch worker exists
  -> ensure GitTargetEventStream exists
  -> run initial snapshot sync
  -> switch event stream to live mode
  -> mark Ready=True
```

### Where it currently breaks

```text
GitTarget Reconcile
  -> Validated=True
  -> EncryptionConfigured=True
  -> SnapshotSynced tries to start
  -> branch worker missing
  -> SnapshotSynced=False
  -> Ready=False
```

## Short Interpretation

The failing e2e is currently stuck in this exact state:

- the Kubernetes setup is already correct
- encryption setup is already correct
- no watched Secret has been processed yet
- the GitTarget cannot cross from “configuration is valid” into “initial sync is running/completed”

So this is best understood as:

- a startup lifecycle problem

not:

- a later write/commit/encryption rendering problem

## Most Useful Question To Ask Tomorrow

At this exact point in the e2e, the most useful question is:

- why does a valid `GitTarget` with a valid `GitProvider` and valid encryption config still not have a usable branch worker by the time `SnapshotSynced` starts?

That question is narrower and more actionable than asking whether the batch refactor broke Secret commits.
