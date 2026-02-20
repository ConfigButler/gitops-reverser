# GitTarget Startup State Machine and Status Contract

## Purpose

Define a concrete implementation plan for `GitTarget` startup lifecycle and status design, integrating:

- explicit startup gates (validation, encryption, bootstrap, initial snapshot, stream-live),
- Kubernetes condition best practices,
- current `gitops-reverser` CRD/controller patterns.

This document supersedes the earlier bootstrap-only assessment and aligns with `docs/design/status-design-git-target.md`.

## Current Repo Landscape (What Exists Today)

## Existing CRD status patterns

- `GitTarget.status` currently has:
  - `conditions []metav1.Condition`
  - `lastCommit string`
  - `lastPushTime *metav1.Time`
- `GitProvider`, `WatchRule`, `ClusterWatchRule` status currently use condition-driven readiness and do not expose `phase`.
- Across controllers, the dominant pattern is a single `Ready` condition with reason/message updates.

## Existing startup behavior

Current `GitTarget` reconcile path (`internal/controller/gittarget_controller.go`) effectively does:

1. Validate provider/branch/conflict.
2. Ensure encryption secret (optional auto-create).
3. Register worker and event stream.
4. Signal stream transition via `OnReconciliationComplete()`.

Bootstrap and startup synchronization exist, but they are implicit side effects across controller, worker manager, branch worker, and event stream.

## Key Gaps

1. Startup gates are not represented as first-class conditions.
2. Initial snapshot sync (including stale-file deletion) is not an explicit lifecycle gate in status.
3. Worker manager lock scope includes slow bootstrap work.
4. Status today is not explicit enough for operators to answer "what gate is blocking readiness".

## Naming Alignment With Current CRDs

Use current schema names from `api/v1alpha1`:

- `spec.providerRef` (not `destinationRef`, not `gitRepoConfigRef`)
- `spec.encryption.provider`
- `spec.encryption.secretRef.name`
- `spec.encryption.generateWhenMissing`

Status additions should extend `GitTargetStatus` while preserving existing fields (`lastCommit`, `lastPushTime`) for compatibility.

## Proposed GitTarget Status Contract

## Required top-level fields

- `observedGeneration int64`
- `conditions []metav1.Condition`
- `lastReconcileTime metav1.Time`

Keep existing:

- `lastCommit string`
- `lastPushTime *metav1.Time`

## Optional detail blocks (recommended)

- `workerRef {name, uid}`
- `encryption {provider, secretRefName, recipientsHash, lastValidatedTime}`
- `bootstrap {targetPath, commit, lastAppliedTime}`
- `snapshot {lastCompletedTime, stats{created,updated,deleted}}`
- `stream {registered bool, registeredTime, lastEventTime}`

These are observational only; they must never become desired-state inputs.

## Condition Types and Ready Semantics

## Condition types

Required gate conditions:

- `Ready` (summary)
- `Validated`
- `EncryptionConfigured`
- `Bootstrapped`
- `SnapshotSynced`
- `EventStreamLive`

Optional UX condition:

- `Reconciling`

## Ready semantics

`Ready=True` iff all required gates are true:

- `Validated=True`
- `EncryptionConfigured=True` (or `True` with `NotRequired`)
- `Bootstrapped=True`
- `SnapshotSynced=True`
- `EventStreamLive=True`

## Canonical reason set

- `Validated`: `OK`, `ProviderNotFound`, `BranchNotAllowed`, `TargetConflict`
- `EncryptionConfigured`: `OK`, `NotRequired`, `MissingSecret`, `InvalidConfig`, `SecretCreateDisabled`
- `Bootstrapped`: `BootstrapApplied`, `BootstrapNotNeeded`, `WorkerNotFound`, `BootstrapFailed`
- `SnapshotSynced`: `Running`, `Completed`, `SnapshotFailed`
- `EventStreamLive`: `Registered`, `RegistrationFailed`, `Disconnected`
- `Ready`: `OK`, `ValidationFailed`, `EncryptionNotConfigured`, `BootstrapNotComplete`, `InitialSyncInProgress`, `StreamNotLive`

## Deterministic condition mechanics

Implement one shared upsert helper (recommended: shared utility reusable by all controllers):

- condition set behaves as map keyed by `Type`,
- no duplicate types,
- `LastTransitionTime` changes only when `Status` changes,
- `Reason` and `Message` always refreshed,
- condition-level `ObservedGeneration` set on update,
- unevaluated gates start as `Unknown` with explicit reason (`NotChecked`/`NotStarted`/`Blocked`).

## Proposed Startup Lifecycle (Gate Pipeline)

Each reconcile evaluates gates in order and writes status before returning on block:

1. `Validated`
- Check provider exists, branch allowed, no target conflict.
- If false: `Ready=False`, stop.

2. `EncryptionConfigured`
- If encryption disabled: set `True/NotRequired`.
- If enabled: resolve config, ensure secret, validate recipient derivation input.
- If false: `Ready=False`, stop.

3. `Bootstrapped`
- Ensure worker exists and target path bootstrap is applied/idempotent.
- Capture bootstrap commit when created.
- If false: `Ready=False`, stop.

4. `SnapshotSynced`
- Trigger initial full snapshot reconciliation.
- Must include create/update/delete reconciliation for target path.
- Keep `SnapshotSynced=False, reason=Running` until complete.
- If failed: `SnapshotSynced=False, reason=SnapshotFailed`, stop.

5. `EventStreamLive`
- Register/verify stream only after initial snapshot success.
- Call `OnReconciliationComplete()` only at this point.
- If false: `Ready=False`, stop.

6. `Ready`
- Compute from gate conditions only.

## Status YAML Examples

These are illustrative snapshots of `GitTarget.status` for key situations.

## 1. Validation blocked (missing provider)

```yaml
status:
  observedGeneration: 7
  lastReconcileTime: "2026-02-19T10:10:11Z"
  conditions:
    - type: Validated
      status: "False"
      reason: ProviderNotFound
      message: "Referenced GitProvider 'sut/main-provider' not found"
      observedGeneration: 7
      lastTransitionTime: "2026-02-19T10:10:11Z"
    - type: EncryptionConfigured
      status: Unknown
      reason: Blocked
      message: "Blocked by Validated=False"
      observedGeneration: 7
      lastTransitionTime: "2026-02-19T10:10:11Z"
    - type: Bootstrapped
      status: Unknown
      reason: Blocked
      message: "Blocked by Validated=False"
      observedGeneration: 7
      lastTransitionTime: "2026-02-19T10:10:11Z"
    - type: SnapshotSynced
      status: Unknown
      reason: NotStarted
      message: "Initial snapshot sync has not started"
      observedGeneration: 7
      lastTransitionTime: "2026-02-19T10:10:11Z"
    - type: EventStreamLive
      status: Unknown
      reason: NotStarted
      message: "Event stream activation has not started"
      observedGeneration: 7
      lastTransitionTime: "2026-02-19T10:10:11Z"
    - type: Ready
      status: "False"
      reason: ValidationFailed
      message: "Validated gate failed: ProviderNotFound"
      observedGeneration: 7
      lastTransitionTime: "2026-02-19T10:10:11Z"
```

## 2. Encryption blocked (secret missing + autocreate disabled)

```yaml
status:
  observedGeneration: 8
  lastReconcileTime: "2026-02-19T10:13:45Z"
  conditions:
    - type: Validated
      status: "True"
      reason: OK
      message: "Provider and branch validation passed"
      observedGeneration: 8
      lastTransitionTime: "2026-02-19T10:13:40Z"
    - type: EncryptionConfigured
      status: "False"
      reason: SecretCreateDisabled
      message: "Encryption secret sut/sops-age-key is missing and generateWhenMissing is false"
      observedGeneration: 8
      lastTransitionTime: "2026-02-19T10:13:45Z"
    - type: Bootstrapped
      status: Unknown
      reason: Blocked
      message: "Blocked by EncryptionConfigured=False"
      observedGeneration: 8
      lastTransitionTime: "2026-02-19T10:13:45Z"
    - type: Ready
      status: "False"
      reason: EncryptionNotConfigured
      message: "EncryptionConfigured gate failed: SecretCreateDisabled"
      observedGeneration: 8
      lastTransitionTime: "2026-02-19T10:13:45Z"
```

## 3. Snapshot running (initial create/delete reconciliation in progress)

```yaml
status:
  observedGeneration: 9
  lastReconcileTime: "2026-02-19T10:18:02Z"
  lastCommit: "3f95c85f53e7862d9d124d67f22e8de2919759b8"
  conditions:
    - type: Validated
      status: "True"
      reason: OK
      message: "Provider and branch validation passed"
      observedGeneration: 9
      lastTransitionTime: "2026-02-19T10:17:54Z"
    - type: EncryptionConfigured
      status: "True"
      reason: OK
      message: "Encryption configuration is valid"
      observedGeneration: 9
      lastTransitionTime: "2026-02-19T10:17:55Z"
    - type: Bootstrapped
      status: "True"
      reason: BootstrapApplied
      message: "Bootstrap commit applied for path clusters/prod"
      observedGeneration: 9
      lastTransitionTime: "2026-02-19T10:17:57Z"
    - type: SnapshotSynced
      status: "False"
      reason: Running
      message: "Initial snapshot reconciliation in progress"
      observedGeneration: 9
      lastTransitionTime: "2026-02-19T10:18:02Z"
    - type: EventStreamLive
      status: Unknown
      reason: Blocked
      message: "Blocked until SnapshotSynced=True"
      observedGeneration: 9
      lastTransitionTime: "2026-02-19T10:18:02Z"
    - type: Ready
      status: "False"
      reason: InitialSyncInProgress
      message: "SnapshotSynced gate is still running"
      observedGeneration: 9
      lastTransitionTime: "2026-02-19T10:18:02Z"
  snapshot:
    stats:
      created: 12
      updated: 3
      deleted: 2
```

## 4. Stream registration failed after snapshot success

```yaml
status:
  observedGeneration: 9
  lastReconcileTime: "2026-02-19T10:18:17Z"
  conditions:
    - type: Validated
      status: "True"
      reason: OK
      message: "Provider and branch validation passed"
      observedGeneration: 9
      lastTransitionTime: "2026-02-19T10:17:54Z"
    - type: EncryptionConfigured
      status: "True"
      reason: OK
      message: "Encryption configuration is valid"
      observedGeneration: 9
      lastTransitionTime: "2026-02-19T10:17:55Z"
    - type: Bootstrapped
      status: "True"
      reason: BootstrapNotNeeded
      message: "Bootstrap files already present"
      observedGeneration: 9
      lastTransitionTime: "2026-02-19T10:17:57Z"
    - type: SnapshotSynced
      status: "True"
      reason: Completed
      message: "Initial snapshot reconciliation completed"
      observedGeneration: 9
      lastTransitionTime: "2026-02-19T10:18:14Z"
    - type: EventStreamLive
      status: "False"
      reason: RegistrationFailed
      message: "Failed to register GitTargetEventStream for sut/prod-target"
      observedGeneration: 9
      lastTransitionTime: "2026-02-19T10:18:17Z"
    - type: Ready
      status: "False"
      reason: StreamNotLive
      message: "EventStreamLive gate failed: RegistrationFailed"
      observedGeneration: 9
      lastTransitionTime: "2026-02-19T10:18:17Z"
```

## 5. Fully ready

```yaml
status:
  observedGeneration: 10
  lastReconcileTime: "2026-02-19T10:22:01Z"
  lastCommit: "a12f67b7c744eeb5ee0a6d7b0d5ea32b5159cf3f"
  lastPushTime: "2026-02-19T10:21:58Z"
  conditions:
    - type: Validated
      status: "True"
      reason: OK
      message: "Provider and branch validation passed"
      observedGeneration: 10
      lastTransitionTime: "2026-02-19T10:21:40Z"
    - type: EncryptionConfigured
      status: "True"
      reason: NotRequired
      message: "Encryption is not configured for this GitTarget"
      observedGeneration: 10
      lastTransitionTime: "2026-02-19T10:21:40Z"
    - type: Bootstrapped
      status: "True"
      reason: BootstrapNotNeeded
      message: "Bootstrap files already present"
      observedGeneration: 10
      lastTransitionTime: "2026-02-19T10:21:41Z"
    - type: SnapshotSynced
      status: "True"
      reason: Completed
      message: "Initial snapshot reconciliation completed"
      observedGeneration: 10
      lastTransitionTime: "2026-02-19T10:21:47Z"
    - type: EventStreamLive
      status: "True"
      reason: Registered
      message: "GitTarget event stream is live"
      observedGeneration: 10
      lastTransitionTime: "2026-02-19T10:21:48Z"
    - type: Ready
      status: "True"
      reason: OK
      message: "All lifecycle gates satisfied"
      observedGeneration: 10
      lastTransitionTime: "2026-02-19T10:21:48Z"
```

## Phase Field Analysis (Repo-Wide)

## What the repo currently does

- No current CRD status type includes `phase`.
- Existing resources (`GitProvider`, `WatchRule`, `ClusterWatchRule`, `GitTarget`) expose readiness through conditions.
- Automation expectations are already condition-centric (`Ready`).

## Recommendation

Do not add `status.phase` in the first implementation step.

Rationale:

- It is not used elsewhere in the project today.
- It adds one more state surface to maintain and test.
- Conditions already provide machine-readable and human-readable gate state.

If added later for UX, keep it strictly derived from conditions and informational only. Suggested derived values:

- `PENDING`, `BOOTSTRAPPING`, `SYNCING`, `RUNNING`

But phase must never be used as an automation contract.

## Refactor Strategy

1. Split worker registration APIs:
- `EnsureWorker(...)` under short lock.
- `EnsureTargetBootstrapped(...)` outside manager global lock.

2. Introduce GitTarget gate-condition pipeline in reconciler.

3. Add initial snapshot completion barrier before stream live transition.

4. Add shared condition helper and migrate GitTarget to it first.

5. Optionally follow-up by migrating `GitProvider`/`WatchRule`/`ClusterWatchRule` to same helper for consistency.

## Testing Plan

## Unit

- Condition helper upsert behavior (no duplicates, transition-time semantics).
- Gate evaluation table tests for each block point.
- Ready aggregation tests.

## Integration

- Startup progression assertions on conditions:
  - validate -> encryption -> bootstrap -> snapshot -> stream -> ready.
- Snapshot gate explicitly verifies stale-file deletion path.
- `kubectl wait --for=condition=Ready=true gittarget/<name>` compatibility.
- Optional: `kubectl wait --for=condition=SnapshotSynced=true` if exposed.

## Minimal First Delivery

1. Add gate conditions on `GitTarget` (without adding `phase`).
2. Move bootstrap out of `WorkerManager` global lock scope.
3. Add initial snapshot sync gate and block stream-live transition until complete.
4. Keep `Ready` as summary contract.

This delivers the main operational clarity and race/ordering improvements with minimal API churn.
