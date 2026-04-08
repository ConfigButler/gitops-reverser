# Webhook Audit E2E Remediation Status

## Purpose

This document captures:

1. The original remediation plan for the webhook-audit migration regressions.
2. What was implemented.
3. What validated successfully.
4. What is still failing after the latest clean-cluster `make test-e2e` run.
5. The updated understanding of Secret handling in the audit pipeline.

## Original Problem Statement

The move toward webhook-driven audit ingestion introduced two visible regressions:

1. Audit consumer instability around missing Redis consumer groups (`NOGROUP`).
2. Lost username attribution in Git commits because watch-path events with empty `UserInfo` could win the race against audit-path events that carried the real user.

There was also a strong suspicion that the Valkey persistence explanation was incomplete, and that at least one data-mapping or ingestion-path mismatch existed in the new pipeline.

## Original Plan

The plan we implemented was:

### 1. Make audit authoritative for live mutating events when audit Redis is enabled

- Add an internal runtime switch on the watch side.
- When `--audit-redis-enabled` is true:
  - keep watch informers for snapshot / reconcile behavior
  - stop routing live mutating watch events into `GitTargetEventStream`
- Let the audit consumer be the source of truth for live mutation commits.

### 2. Harden the audit consumer against lost Redis groups

- Ensure the consumer group exists on startup.
- Recreate it if `XREADGROUP` returns `NOGROUP`.
- Recreate it if `XAUTOCLAIM` returns `NOGROUP`.

### 3. Stop destructive stream cleanup in e2e

- Remove stream deletion in the audit producer e2e.
- Use unique object names and filtered assertions instead of deleting the shared stream key.

### 4. Strengthen e2e verification

- Keep the existing `jane@acme.com` author-attribution expectation.
- Strengthen the audit-consumer e2e so it also verifies commit author attribution, not just file creation.

### 5. Preserve watch-based snapshot / reconcile behavior

- Watchers should still drive cluster-state snapshots and rule-change reconciliation.
- The migration should not break snapshot-oriented paths like cluster-scoped CRD export.

## Implemented Changes

### Wiring

- [cmd/main.go](/workspaces/gitops-reverser/cmd/main.go)
  - `watch.Manager` now receives `AuditLiveEventsEnabled: cfg.auditRedisEnabled`.

### Watch Manager

- [internal/watch/manager.go](/workspaces/gitops-reverser/internal/watch/manager.go)
  - Added `AuditLiveEventsEnabled bool`.

- [internal/watch/informers.go](/workspaces/gitops-reverser/internal/watch/informers.go)
  - Watch-path live routing is skipped when audit is authoritative, with two important exceptions:
    - `secrets`
    - cluster-scoped resources
  - This is narrower than the original broad suppression because the broader version broke valid e2e behavior.

### Audit Consumer

- [internal/queue/redis_audit_consumer.go](/workspaces/gitops-reverser/internal/queue/redis_audit_consumer.go)
  - Added `NOGROUP` self-healing for:
    - `XREADGROUP`
    - `XAUTOCLAIM`
  - Added `isNoGroupErr`.

### Audit E2E

- [test/e2e/audit_redis_e2e_test.go](/workspaces/gitops-reverser/test/e2e/audit_redis_e2e_test.go)
  - Removed destructive `DEL` of the shared stream.
  - Added commit-author verification for the audit consumer path.

### Unit Test Coverage

- [internal/watch/informers_test.go](/workspaces/gitops-reverser/internal/watch/informers_test.go)
  - Added a test proving watch live routing is skipped when audit is authoritative.
  - Added a test proving cluster-scoped live routing still works when audit is authoritative.

- [internal/queue/redis_audit_consumer_test.go](/workspaces/gitops-reverser/internal/queue/redis_audit_consumer_test.go)
  - Added tests for consumer group recreation on `NOGROUP`.

### Test Stability

- [internal/git/git_operations_test.go](/workspaces/gitops-reverser/internal/git/git_operations_test.go)
  - Public network tests now skip on DNS / timeout failures rather than failing the whole suite in an offline environment.

## Important Design Correction: Secrets and Audit Policy

An important finding emerged while testing:

- SOPS protects what gets committed to Git.
- SOPS does not protect the raw audit payload sent by kube-apiserver to the audit webhook.
- The Redis producer stores audit payloads as `payload_json`.
- Our sanitizer preserves Secret `data` / `binaryData`.

This means removing `secrets` from the audit exclusion policy would expose Secret contents in the audit webhook / Redis pipeline before SOPS encryption happens.

So the current safe position is:

- Keep `secrets` excluded in [policy.yaml](/workspaces/gitops-reverser/test/e2e/cluster/audit/policy.yaml).
- Continue handling Secret live mutations via the watch path for now.

That is why the final code keeps a `secrets` exception in [internal/watch/informers.go](/workspaces/gitops-reverser/internal/watch/informers.go).

## Validation Performed

The following validations passed locally:

- `make fmt`
- `make lint`
- `make test`
- `docker info`

Focused package tests also passed:

- `go test ./internal/watch ./internal/queue`

## Clean-Cluster E2E Result

After the audit-policy discussion, the cluster was fully reset with:

```bash
make clean-cluster
```

Then `make test-e2e` was rerun from a clean baseline.

### What Passed in That Clean Run

The following important specs passed:

- Audit Redis producer stream test
- Audit Redis consumer commit test
- Secret encryption via WatchRule
- Secret encryption with generated age recipient
- ConfigMap commit via WatchRule
- ConfigMap deletion via WatchRule
- ClusterWatchRule CRD installation commit

This means the original migration regressions appear fixed:

- `NOGROUP` self-healing is working well enough for the exercised audit path.
- Audit-path author attribution is working.
- The race between watch-path ConfigMap events and audit-path ConfigMap events is no longer breaking the tested commit attribution path.
- The earlier cluster-scoped CRD regression introduced by overly broad watch suppression was fixed by allowing cluster-scoped live routing.

### What Still Fails

The clean-cluster `make test-e2e` run still fails overall.

Current failing spec:

- [test/e2e/e2e_test.go:1011](/workspaces/gitops-reverser/test/e2e/e2e_test.go#L1011)
  - `Manager should create Git commit when IceCreamOrder is added via WatchRule`

Observed failure:

- Expected file never appears:
  - `/workspaces/gitops-reverser/.stamps/repos/e2e-test-1775644840/e2e/icecream-test/shop.example.com/v1/icecreamorders/1775644840-test-manager/alices-order.yaml`

### Important Log Signal

The controller logs show:

- the IceCreamOrder create audit event is received:
  - `gvr: "/v1/icecreamorders"`
- but the commit worker later reports:
  - `No commits created, no need to push it`

That combination suggests the object creation is being observed, but the resulting write is considered a no-op by the time it reaches the Git layer.

## Current Interpretation

The original webhook-audit remediation is only partially complete.

### Fixed

- Audit consumer group recovery
- ConfigMap author attribution through the audit path
- Audit producer e2e isolation
- Audit consumer e2e author verification
- Cluster-scoped CRD export in audit-enabled mode

### Still Broken

- Namespaced custom resource live commit path for `WatchRule` resources like `IceCreamOrder`

## Most Likely Remaining Root Cause

The remaining failure does not look like the original Valkey persistence issue.

It looks more like a deduplication / no-op classification problem for namespaced custom resources:

- the object is seen
- the event is routed or at least processed far enough to generate worker activity
- but no resulting file diff is produced

The strongest candidates are:

1. Sanitized custom-resource content is being judged identical to an already-known state.
2. The write path is building a path or payload that collapses to no diff.
3. Audit/live interaction for namespaced CR instances is still suppressing the effective event somewhere, even though the cluster-scoped CRD path now works.

## Recommended Next Debugging Steps

### 1. Trace `IceCreamOrder` end to end

Instrument or inspect these points for the failing test:

- informer callback receives the CR create
- `handleEvent` match result for `icecreamorders`
- event routing to the `GitTargetEventStream`
- write-request payload generated for the CR
- file path selected for the CR object
- git write result before commit

### 2. Compare successful ConfigMap path vs failing custom-resource path

Specifically compare:

- identifier construction
- sanitized object content
- dedup hash inputs
- generated destination path
- whether metadata needed for CR file generation is missing

### 3. Confirm whether audit path is involved for custom resources

The log line:

- `gvr: "/v1/icecreamorders"`

is suspicious because the expected group is `shop.example.com/v1`.

That may indicate a GVR mapping or audit object-ref translation problem for the custom resource path.

### 4. Inspect no-op commit reasoning

The worker log:

- `No commits created, no need to push it`

should be traced back to the exact file operation result for the `IceCreamOrder` object.

## Current Bottom Line

The answer to "does `make test-e2e` work now?" is:

- No, not fully.

The answer to "did the original webhook-audit remediation help?" is:

- Yes, substantially.

The latest clean-cluster run shows that the original webhook-audit regressions were largely fixed, but a separate remaining regression still blocks the full e2e suite:

- namespaced custom resource export via `WatchRule` in audit-enabled mode.
