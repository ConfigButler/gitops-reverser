# Audit Webhook Consumer: Current State and Next Steps

## Purpose

This document is the practical starting point for implementing the audit stream consumer.
It maps all existing design documents to what they actually cover, describes the current
code state precisely, and defines the concrete missing work.

---

## Existing documents and what they are good for

| Document | Use it for |
|---|---|
| [audit-webhook-redis-queue-plan.md](audit-webhook-redis-queue-plan.md) | **Start here.** Most authoritative. Defines what is done, what is not, and the open design question on handoff boundary. |
| [valkey-adoption-guide.md](valkey-adoption-guide.md) | Stream consumer patterns: XREADGROUP, XACK, XAUTOCLAIM, lock keys. Good reference when writing consumer code. |
| [multi-cluster-audit-ingestion-implications.md](multi-cluster-audit-ingestion-implications.md) | Future scope only. Do not let it distract from the single-cluster consumer. |
| [best-practices-webhook-ingress.md](best-practices-webhook-ingress.md) | Operational notes on audit ingress (TLS, mTLS, backpressure). Useful for hardening, not for consumer implementation. |
| [../future/AUDIT_WEBHOOK_NATS_ARCHITECTURE_PROPOSAL.md](../future/AUDIT_WEBHOOK_NATS_ARCHITECTURE_PROPOSAL.md) | Historical. NATS was considered and rejected in favour of Valkey. Read only for context on why the current direction was chosen. |
| [../past/audit-webhook-experimental-design.md](../past/audit-webhook-experimental-design.md) | Original metrics-only design. Fully superseded. |

---

## Exact current state in code

### What is done

**Producer: [internal/queue/redis_audit_queue.go](../../internal/queue/redis_audit_queue.go)**
- Writes to Valkey stream `gitopsreverser.audit.events.v1` via `XADD`.
- Fields written: `event_id`, `audit_id`, `cluster_id`, `verb`, `api_version`, `resource`,
  `namespace`, `name`, `user`, `stage_timestamp`, `payload_json` (full audit event JSON).
- `event_id` is a deterministic SHA-256 hash built in [`buildEventID`](../../internal/queue/redis_audit_queue.go#L138) — dedup key material is already there.
- Optional approximate trimming via `maxLen`.

**Ingress handler: [internal/webhook/audit_handler.go](../../internal/webhook/audit_handler.go)**
- Receives POST at `/audit-webhook/{clusterID}`.
- Decodes the Kubernetes `audit.k8s.io/v1 EventList`.
- Enqueues to Redis if `Queue != nil`, returns 500 on enqueue failure (correct at-least-once semantics).
- Then: increments metrics, optionally dumps to file, **logs and does nothing else**.
- [Line 229](../../internal/webhook/audit_handler.go#L229): `// For now we hardly do a thing`.

**Wiring: [cmd/main.go](../../cmd/main.go)**
- Redis queue is created when `--audit-redis-enabled` is set ([line 209](../../cmd/main.go#L209)).
- Wired into `AuditHandler` as the `Queue` field.
- No consumer runnable is registered. The stream accumulates indefinitely.

**E2E coverage**
- [cmd/main_audit_server_test.go](../../cmd/main_audit_server_test.go) verifies events land in the stream after Kubernetes object changes.
- The stream write path is tested. The consumer path has no test coverage yet.

### What does not exist

- No XREADGROUP consumer loop anywhere in the codebase.
- No consumer group creation (`XGROUP CREATE`).
- No XACK after processing.
- No XAUTOCLAIM for stuck message recovery.
- No runnable in `cmd/main.go` for the consumer lifecycle.
- No handoff from audit stream into the existing git processing pipeline.

---

## The existing pipeline the consumer must feed into

The git write pipeline already works and should be reused, not replaced.

```
EventRouter.RouteEvent(providerName, providerNamespace, branch, git.Event)
  -> WorkerManager.GetWorkerForTarget(...)
  -> BranchWorker.Enqueue(git.Event)
  -> git write / commit / push
```

- [`EventRouter.RouteEvent`](../../internal/watch/event_router.go#L72) — entry point
- [`WorkerManager.GetWorkerForTarget`](../../internal/git/worker_manager.go) — worker lookup
- [`BranchWorker.Enqueue`](../../internal/git/branch_worker.go#L172) — puts event into the in-memory write channel

A [`git.Event`](../../internal/git/types.go#L119) needs:

```go
type Event struct {
    Object             *unstructured.Unstructured  // sanitized Kubernetes object
    Identifier         types.ResourceIdentifier    // GVK + namespace + name
    Operation          string                      // CREATE, UPDATE, DELETE
    UserInfo           UserInfo                    // Username, UID — THIS is what audit gives us natively
    Path               string                      // from GitTarget.spec.path
    GitTargetName      string
    GitTargetNamespace string
    BootstrapOptions   pathBootstrapOptions
}
```

The `UserInfo.Username` is the primary value that the audit stream provides over the
current validating webhook + in-memory correlation approach. It is available directly
from `auditv1.Event.User.Username` (or `ImpersonatedUser.Username` when set), and is
already extracted into the stream entry's `user` field.

---

## Open design question (from the plan doc)

The plan doc identifies this as the central unresolved question:

> We still need to decide the cleanest handoff boundary into the current `EventRouter`
> and branch-worker architecture so we do not create a second partially overlapping pipeline.

**Recommendation:** Feed the consumer directly into `EventRouter.RouteEvent(...)`.

Reasons:
- `EventRouter` already handles target lookup, worker dispatch, and stream buffering
  during initial reconciliation ([`GitTargetEventStream`](../../internal/reconcile/git_target_event_stream.go)).
- The consumer only needs to build a `git.Event` from the audit payload and call
  `RouteEvent`. No new routing layer needed.
- The existing validating webhook → correlation store → watch path can stay in place
  during the transition and be removed in a follow-up once the audit consumer is proven.

---

## Concrete missing pieces, in order

### ~~Step 1: Build a `git.Event` from a stream entry~~ ✅ Done

Implemented in [`internal/queue/redis_audit_consumer.go`](../../internal/queue/redis_audit_consumer.go).

- `parseAuditEvent` reads `payload_json` from the stream entry and unmarshals the full `auditv1.Event`.
- `verbToOperation` maps HTTP verbs to `OperationType` (create → CREATE, update/patch → UPDATE, delete/deletecollection → DELETE). Read-only verbs are skipped.
- Rule matching calls `ruleStore.GetMatchingRules` and `ruleStore.GetMatchingClusterRules` directly, passing the parsed group/version/resource/operation.
- `splitAPIVersion` splits `"apps/v1"` into `("apps", "v1")` and `"v1"` into `("", "v1")`.
- The existing `RuleStore` API was sufficient — no changes to `rulestore/store.go` were needed.

Routing uses `EventRouter.RouteToGitTargetEventStream` (same path as the watch manager informers), not `RouteEvent` directly. This goes through the `GitTargetEventStream` buffering layer.

### ~~Step 2: Deserialize the object~~ ✅ Done

Implemented in `extractObject` in [`internal/queue/redis_audit_consumer.go`](../../internal/queue/redis_audit_consumer.go).

- For non-DELETE operations: uses `ResponseObject.Raw`.
- For DELETE: uses `RequestObject.Raw`.
- Falls back to a minimal stub (apiVersion + kind + namespace + name) when neither is present.
- Passes the result through [`sanitize.Sanitize`](../../internal/sanitize/sanitize.go#L33).

### ~~Step 3: Consumer runnable in `cmd/main.go`~~ ✅ Done

`AuditConsumer` implements `manager.Runnable` in [`internal/queue/redis_audit_consumer.go`](../../internal/queue/redis_audit_consumer.go).

- `Start` calls `XGROUP CREATE … MKSTREAM` (idempotent via `BUSYGROUP` error check), then enters the read loop.
- `XREADGROUP GROUP … COUNT 50 BLOCK 2s STREAMS … >`.
- `XAUTOCLAIM` ticker fires every 30s, reclaiming entries idle longer than 60s.
- Consumer ID defaults to `$POD_NAME`, falling back to `"gitopsreverser-consumer-0"`.

Wired in [`cmd/main.go`](../../cmd/main.go) inside the `cfg.auditRedisEnabled` block, registered via `mgr.Add`. `NeedLeaderElection() = true`.

### ~~Step 4: XACK only after safe handoff~~ ✅ Done

`XACK` is called in `ackMessage` after `RouteToGitTargetEventStream` returns. Since `BranchWorker.Enqueue` is synchronous (puts the event into the in-memory channel before returning), this gives at-least-once semantics at the stream boundary.

Poison-pill messages (unparseable payload, missing ObjectRef) are also ACKed immediately to prevent the consumer from blocking on them indefinitely.

### ~~Step 5: E2E test for the consumer path~~ ✅ Done

Implemented as `Describe("Audit Redis Consumer", Label("audit-redis"), ...)` in [`test/e2e/audit_redis_e2e_test.go`](../../test/e2e/audit_redis_e2e_test.go).

- BeforeAll creates a namespace, applies git secrets, calls `applySOPSAgeKeyToNamespace` (required for the GitTarget encryption gate), connects to Valkey, and sets up GitProvider + GitTarget + WatchRule targeting `e2e/audit-consumer-test` on `main`.
- The `It` block creates a ConfigMap (name includes `GinkgoRandomSeed` so it is unique per run), waits for the audit stream entry to appear via `XRange`, then does `git pull` in a loop until the file `e2e/audit-consumer-test/v1/configmaps/{ns}/{cm}.yaml` exists with non-zero size.
- The stream is **not** deleted before the test (deleting the stream key also destroys the consumer group; cmName uniqueness is sufficient for isolation).
- Labels: `audit-redis` — runs together with the producer test via `make test-e2e-audit-redis`.

---

## What to leave alone for now

- The validating webhook + correlation store path ([`internal/correlation/store.go`](../../internal/correlation/store.go)).
  Keep it running in parallel. Remove it only after the audit consumer has been validated end-to-end in e2e.
- The `BranchWorker` in-memory queue. Replacing it with a Redis-backed queue is the HA
  follow-on work, not part of this consumer slice.
- Multi-cluster routing. The `clusterID` is already in the stream entry. Wire it into
  log labels and metrics now, but do not implement per-cluster rule filtering yet.
- Idempotency markers (dedupe TTL in Redis). Implement after the consumer works. The
  `event_id` field in the stream entry is already the dedup key.

---

## Files changed

| File | Change |
|---|---|
| [internal/queue/redis_audit_consumer.go](../../internal/queue/redis_audit_consumer.go) | **New.** XREADGROUP loop, XAUTOCLAIM, rule matching, object extraction, routing. |
| [internal/queue/redis_audit_consumer_test.go](../../internal/queue/redis_audit_consumer_test.go) | **New.** 34 unit tests, 91% coverage. |
| [cmd/main.go](../../cmd/main.go) | Wires `AuditConsumer` into the manager when `--audit-redis-enabled`. |
| [test/e2e/audit_redis_e2e_test.go](../../test/e2e/audit_redis_e2e_test.go) | **New `Describe` block.** Consumer e2e: GitProvider + GitTarget + WatchRule setup, audit event → git commit assertion (Step 5). |

All steps are now complete.
