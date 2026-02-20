# Plan: GitTarget Status, Conditions, and Lifecycle Implementation

## Outcomes

- `GitTarget.status` is a reliable, observation-only view of reality.
- A small `status.phase` provides quick human scanning.
- `status.conditions` is the automation contract (`kubectl wait --for=condition=Ready=true`).
- Every gate in startup lifecycle is represented as a stateful condition.
- Condition updates are deterministic (no duplicates, no flapping).

## 1. Define the public API contract

### 1.1 Status fields (what you expose)

Define these in CRD Go types (or equivalent).

Top-level:

- `observedGeneration: int64`
- `phase: string` (small stable enum; informational)
- `conditions: []metav1.Condition` (or compatible schema)
- `lastReconcileTime: metav1.Time` (optional but helpful)

Pointers / detail sub-objects (optional but recommended):

- `workerRef: {name, uid}`
- `encryption: {mode, secretRef, recipientsHash, lastValidatedTime}`
- `bootstrap: {targetPath, commit, lastAppliedTime}`
- `snapshot: {lastCompletedTime, lastCommit, stats{created,updated,deleted}}`
- `stream: {status, registeredTime, lastEventTime}`

### 1.2 Phase values (small and stable)

Keep phase values minimal and derived:

- `PENDING`
- `BOOTSTRAPPING`
- `SYNCING`
- `RUNNING`
- Optional: `DEGRADED`, `ERROR` if explicit "can’t proceed" vs "limping" is needed.

Rule: automation should never depend on phase; only on conditions.

### 1.3 Condition types (real gates)

Define these condition types (string constants, CamelCase):

- `Ready` (summary condition)
- `Validated`
- `EncryptionConfigured`
- `Bootstrapped`
- `SnapshotSynced`
- `EventStreamLive`

Optional (UX only, not a gate):

- `Reconciling` (`True` while reconciliation is actively running)

### 1.4 Ready semantics (document it)

Define `Ready` in docs and code as:

`Ready=True` iff:

- `Validated=True`
- `Bootstrapped=True`
- `SnapshotSynced=True`
- `EventStreamLive=True`
- `EncryptionConfigured=True` (or `True` with reason `NotRequired` when encryption is disabled)

## 2. CRD and RBAC wiring

### 2.1 CRD subresource

Enable `/status` subresource:

```yaml
spec:
  versions:
    - subresources:
        status: {}
```

### 2.2 RBAC for controller

Grant controller:

- `get/list/watch` on `gittargets`
- `update/patch` on `gittargets/status`
- Plus whatever it needs to manage worker objects, secrets, etc.

## 3. Implement condition mechanics once (shared utilities)

### 3.1 Condition upsert helper

Implement a single `SetCondition(status *GitTargetStatus, cond metav1.Condition)` that:

- treats conditions as a map keyed by type,
- updates existing entry if present, otherwise inserts,
- updates `LastTransitionTime` only if `Status` changed,
- always sets/updates `Reason` and `Message`,
- sets `ObservedGeneration` inside the condition (recommended).

### 3.2 Standardize reason enums

Create reason constants per condition. Examples:

- `Validated`: `OK`, `ProviderNotFound`, `BranchNotAllowed`, `TargetConflict`
- `EncryptionConfigured`: `OK`, `NotRequired`, `MissingSecret`, `InvalidRecipients`, `SecretCreateDisabled`
- `Bootstrapped`: `BootstrapApplied`, `BootstrapNotNeeded`, `WorkerNotFound`, `BootstrapFailed`
- `SnapshotSynced`: `Completed`, `Running`, `SnapshotFailed`
- `EventStreamLive`: `Registered`, `RegistrationFailed`, `Disconnected`
- `Ready`: `OK`, `ValidationFailed`, `EncryptionNotConfigured`, `BootstrapNotComplete`, `InitialSyncInProgress`, `StreamNotLive`

### 3.3 Avoid flapping rules

Enforce in helper layer:

- no duplicate types,
- no `LastTransitionTime` churn unless status changes,
- if a gate hasn’t been evaluated yet, set to `Unknown` with clear reason (`NotChecked`, `NotStarted`, `Blocked`).

## 4. Reconcile algorithm: gate-by-gate lifecycle

Implement reconciliation as a deterministic pipeline. Each step:

- computes gate result from observed state,
- writes its condition,
- if blocking, stops further progress (but still updates `Ready` and `Phase`).

### 4.1 Step 0: start-of-reconcile bookkeeping

At beginning of each reconcile:

- set `status.lastReconcileTime = now`
- set `status.observedGeneration` only when reconciling current spec successfully (or after all gates are evaluated; pick one strategy and keep it consistent)

Optionally:

- set `Reconciling=True` at start and `False` at end

### 4.2 Step 1: `Validated`

Check:

- Provider exists
- Branch allowed
- No target conflict (uniqueness constraints)

Set:

- `Validated=True/False`

If false:

- set `Ready=False` reason `ValidationFailed` (or more specific reason),
- set phase accordingly,
- return.

### 4.3 Step 2: `EncryptionConfigured`

Resolve encryption mode:

- If encryption disabled: set `EncryptionConfigured=True` reason `NotRequired`
- If enabled:
  - resolve encryption config,
  - ensure secret exists (or create if allowed),
  - validate recipients/key derivation inputs,
  - store `recipientsHash` and `secretRef` in status.

Set:

- `EncryptionConfigured=True/False`

If false: block and return.

### 4.4 Step 3: `Bootstrapped`

Ensure:

- worker exists/bound for `(provider, branch)` and store `workerRef`,
- target path bootstrap applied/idempotently verified,
- if bootstrap commit is needed, ensure it happened and store `bootstrap.commit`.

Set:

- `Bootstrapped=True/False`

If false: block and return.

### 4.5 Step 4: `SnapshotSynced` (initial snapshot)

Critical “clean slate” step:

- trigger snapshot reconciliation for whole folder (creates/updates/deletes),
- wait for completion indication (e.g. reconciliation ID or observed commit),
- record results in `status.snapshot.*` including stats and timestamps.

Set:

- while running: `SnapshotSynced=False` reason `Running`
- on success: `SnapshotSynced=True` reason `Completed`
- on failure: `SnapshotSynced=False` reason `SnapshotFailed`

Block progression until `SnapshotSynced=True`.

### 4.6 Step 5: `EventStreamLive`

Once initial snapshot sync is done:

- register event stream,
- confirm it is live (at least “registered”; optionally track last heartbeat/event timestamp).

Set:

- `EventStreamLive=True/False`

If false: keep `Ready=False` until live.

### 4.7 Step 6: `Ready` (summary)

Compute `Ready` from gate conditions:

- if all required gates are true: `Ready=True` reason `OK`
- else: `Ready=False` with reason/message pointing to first failing gate

Finally set:

- `status.phase` (derived; see next section)

## 5. Phase derivation rules

Make phase a pure function of conditions (not an independent state machine).

Example derivation:

- If `Ready=True` -> `RUNNING`
- Else if `SnapshotSynced` is `False/Unknown` and `Bootstrapped=True` -> `SYNCING`
- Else if any of `Validated`, `EncryptionConfigured`, `Bootstrapped` is `False/Unknown` -> `BOOTSTRAPPING`
- Else -> `PENDING`

Optional:

- If any gate is false with non-retryable reason (e.g. policy denied) -> `ERROR`
- If `Ready=False` but gates mostly true and stream intermittently down -> `DEGRADED`

Keep it minimal unless extra phases are truly needed.

## 6. Example status progression (golden path)

New `GitTarget`:

- `phase=PENDING`
- most gates `Unknown`, `Ready=False`

After validation:

- `Validated=True`
- `phase=BOOTSTRAPPING`

After encryption:

- `EncryptionConfigured=True` (or `True/NotRequired`)

After bootstrap:

- `Bootstrapped=True`

During initial snapshot:

- `phase=SYNCING`
- `SnapshotSynced=False` reason `Running`

After snapshot complete:

- `SnapshotSynced=True`

Stream registered:

- `EventStreamLive=True`

Fully ready:

- `Ready=True`
- `phase=RUNNING`

## 7. Testing plan

### 7.1 Unit tests for condition helper

- upsert inserts new types
- upsert updates existing type without duplication
- `LastTransitionTime` changes only when `Status` changes
- ordering does not matter

### 7.2 Reconcile gate tests (table-driven)

Write scenarios and assert conditions + phase:

- provider missing -> `Validated=False`, `Ready=False`, `phase=BOOTSTRAPPING`
- encryption secret missing and `autocreate=false` -> `EncryptionConfigured=False`
- worker missing -> `Bootstrapped=False`
- snapshot running -> `SnapshotSynced=False` reason `Running`, `phase=SYNCING`
- stream registration fails -> `EventStreamLive=False`, `Ready=False`
- happy path -> all true, `Ready=True`, `phase=RUNNING`

### 7.3 Integration tests for `kubectl wait` compatibility

- `kubectl wait --for=condition=Ready=true gittarget/<name>`
- `kubectl wait --for=condition=SnapshotSynced=true gittarget/<name>` (if automation should wait for initial sync separately)

## 8. Operational conventions (docs to ship)

Document:

- condition types and what `True/False/Unknown` means for each,
- reason enums emitted (for alerting),
- ready semantics as summary gate,
- phase as informational only,
- “conditions are a map keyed by type” behavior.

## 9. Implementation checklist

- [ ] Add status subresource to CRD
- [ ] Add controller RBAC for `/status`
- [ ] Implement `SetCondition` helper and reason constants
- [ ] Add gate constants/types
- [ ] Implement reconcile pipeline steps 1–6
- [ ] Implement phase derivation function
- [ ] Add unit tests for condition helper
- [ ] Add reconcile tests (blocking and happy path)
- [ ] Add integration tests for `kubectl wait`
- [ ] Document status/conditions contract
