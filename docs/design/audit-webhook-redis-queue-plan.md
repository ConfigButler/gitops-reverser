# Audit Webhook + Valkey Queue Plan

## Current status

The audit webhook durable enqueue foundation is now in place.

Implemented:

1. Audit ingress is live on `/audit-webhook/{clusterID}`.
2. Accepted audit events can be enqueued into a Redis-compatible stream backed by Valkey.
3. Redis/Valkey enqueueing is wired into:
   - `cmd/main.go` runtime flags
   - `charts/gitops-reverser` Helm values
   - `config/deployment.yaml` for the local manifest-based e2e install
4. Unit coverage exists for:
   - audit handler enqueue success/failure behavior
   - Redis-backed enqueue behavior
   - audit server flag parsing and validation
5. E2E coverage exists for the audit-to-Valkey enqueue path.

Not implemented yet:

1. A Redis consumer that feeds the existing Git processing pipeline
2. Consumer groups, ack/reclaim handling, or replay workers
3. Deduplication and writer-lock coordination
4. Audit-only ingestion cutover
5. Queue lag / PEL / end-to-end durability metrics

## Current architecture

Today the flow is:

```text
Kube API Audit Backend
  -> /audit-webhook/{clusterID}
  -> Audit Handler
  -> Valkey/Redis stream enqueue (optional, feature-flagged)
  -> stream persistence only
```

Important constraint:

The stream is currently a durability buffer only. There is no in-process or external consumer reading from that stream
back into `EventRouter`, `GitTargetEventStream`, or `BranchWorker`.

## Current implementation details

### Audit handler enqueue integration

The audit handler accepts an `AuditEventQueue` and enqueues each accepted audit event before returning success.

Current behavior:

1. If queueing is disabled, the handler behaves as before.
2. If queueing is enabled and enqueue succeeds, the request continues normally.
3. If queueing is enabled and enqueue fails, the handler returns `500` so the Kubernetes audit backend can retry.
4. Existing request-body guardrails and cluster path validation remain in place.

This gives durable ingress semantics at the HTTP boundary.

### Redis/Valkey stream producer

The repository contains a concrete Redis-backed audit queue implementation under `internal/queue/`.

Current stream contract:

1. Stream: `gitopsreverser.audit.events.v1`
2. Fields written today:
   - `event_id`
   - `audit_id`
   - `cluster_id`
   - `verb`
   - `api_version`
   - `resource`
   - `namespace`
   - `name`
   - `user`
   - `stage_timestamp`
   - `payload_json`
3. `event_id` is a deterministic SHA-256 hash derived from cluster and audit event identity inputs.
4. Optional approximate trimming is supported through `maxLen`.

Follow-up work should preserve this contract unless there is a deliberate migration plan.

### Runtime and deployment wiring

The queue can be enabled at runtime with dedicated audit Redis flags.

Implemented controls:

1. `--audit-redis-enabled`
2. `--audit-redis-addr`
3. `--audit-redis-username`
4. `--audit-redis-password`
5. `--audit-redis-db`
6. `--audit-redis-stream`
7. `--audit-redis-max-len`
8. `--audit-redis-tls`

Deployment status:

1. Helm chart support exists under `queue.redis.*`.
2. Helm defaults remain off, which keeps production behavior conservative.
3. `config/deployment.yaml` currently enables audit enqueueing against the e2e Valkey service.

### E2E environment support

The e2e cluster includes Valkey as a Flux-managed shared service, and the repository contains a dedicated e2e test
that verifies audit events land in the stream after Kubernetes object changes.

This gives confidence in:

1. kube-apiserver audit webhook delivery
2. controller audit endpoint handling
3. Redis-compatible stream writes
4. cluster wiring between the controller and Valkey

## Settings review

The current Redis/Valkey settings are good enough for this slice: they are simple, understandable, and sufficient for
producer-only durable enqueueing.

The main gaps to keep in mind before broader production rollout are:

1. `maxLen` defaults to unbounded retention, which is simple but risky if a consumer is missing or stalled.
2. The connection settings are intentionally minimal; there are no explicit timeout, retry, or pool controls yet.
3. TLS is currently only enabled/disabled; there is no CA or mTLS configuration surface.
4. Startup does not verify Redis reachability, so failures show up on enqueue rather than at boot.

## Scope of this slice

This slice should be understood as:

1. audit webhook durable enqueue foundation
2. Valkey-backed buffering for accepted audit events
3. validated e2e plumbing for audit-to-stream delivery

This slice should not be described as:

1. complete durable queue processing
2. audit-only architecture cutover
3. HA-safe worker replay model

## Remaining work

### Next step 1: Stream consumer into existing processing pipeline

Still needed:

1. consumer runnable lifecycle in `cmd/main.go`
2. consumer group creation and management
3. `XREADGROUP` read loop
4. handoff into the existing Git processing path
5. `XACK` only after safe downstream handoff
6. retry and reclaim behavior such as `XAUTOCLAIM`

Open design question:

We still need to decide the cleanest handoff boundary into the current `EventRouter` and branch-worker architecture so
we do not create a second partially overlapping pipeline.

### Next step 2: Idempotency and writer coordination

Still needed:

1. dedupe key storage for `event_id`
2. TTL policy for dedupe entries
3. branch or target scoped writer locking
4. lock renewal and timeout rules

Risk if consumer work lands before this:

At-least-once delivery would likely produce duplicate downstream writes under retries or failover.

### Next step 3: Audit-only ingestion cutover

Current state:

1. the validating webhook correlation path still exists
2. watch/reconcile behavior remains part of the live ingestion model
3. nothing here removes those assumptions

That means the system has not switched to audit-only operation yet.

### Next step 4: Metrics and durability validation

What exists now:

1. tests prove enqueue success and failure behavior
2. e2e proves stream writes happen

What still needs to be added:

1. enqueue latency metrics
2. stream lag metrics
3. PEL metrics
4. end-to-end replay latency metrics
5. load or outage recovery reports

## Recommended next steps

Recommended follow-up order:

1. Merge this slice as the producer/enqueue foundation.
2. Keep the Flux/Valkey e2e support with it so the test story stays honest.
3. Add a focused follow-up PR for the consumer runnable and downstream handoff.
4. Add idempotency and locking before claiming at-least-once safe Git processing.
5. Revisit audit-only cutover language in README and architecture docs only after the consumer path is real.

## Exit criteria for this slice

This slice is complete when all of the following are true:

1. Audit events are durably appended to Valkey/Redis when queueing is enabled.
2. Enqueue failures surface as `500` responses.
3. The stream contract is documented and stable enough for a future consumer.
4. Unit and e2e coverage keep the enqueue path from regressing.
