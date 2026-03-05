# Audit Webhook Only + Durable Redis Queue Plan

## Goal

Move GitOps Reverser to a single ingestion path:

1. Kubernetes Audit Webhook ingress only
2. Durable Redis-backed queue for all incoming audit events
3. Replayable, at-least-once delivery into Git processing

Note: this document uses **Redis** terminology for the queue contract. Your new Valkey instance is treated as a
Redis-compatible runtime.

## Why This Is the Next Step

Current docs and code show:

1. Audit ingress is already wired (`/audit-webhook/{clusterID}`) and stable.
2. Current queueing to Git workers is in-process/in-memory (`BranchWorker` channel), which is not durable.
3. Existing design direction already points to a durable queue layer and HA worker model.

This plan closes the gap by making audit ingress durable first, then cutting over routing and scaling behavior.

## Architecture Target

```text
Kube API Audit Backend
  -> /audit-webhook/{clusterID}
  -> Audit Handler (validate + normalize)
  -> Redis Stream (durable enqueue)
  -> Redis Consumer Group worker(s)
  -> EventRouter / GitTargetEventStream / BranchWorker
  -> Git commit/push
```

## Data Contract

Use one Redis stream initially:

1. Stream: `gitopsreverser.audit.events.v1`
2. Consumer group: `gitopsreverser-writers`
3. Event fields:
   - `event_id` (deterministic idempotency key)
   - `audit_id`
   - `cluster_id`
   - `verb`
   - `api_version`
   - `resource`
   - `namespace`
   - `name`
   - `user`
   - `stage_timestamp`
   - `payload_json` (normalized event for downstream mapping)

## Phased Plan

### Phase 0: Feature-flagged foundation

1. Add `internal/queue/` package with interfaces:
   - `Producer.Enqueue(ctx, event) error`
   - `Consumer.Run(ctx, handler) error`
2. Add queue mode config:
   - `queue.mode=inmemory|redis` (default `inmemory` for safe rollout)
   - `redis.addr`, `redis.username`, `redis.password`, `redis.tls`, `redis.stream`, `redis.group`
3. Add Helm values under `queue` and `redis` while keeping current behavior unchanged by default.

### Phase 1: Audit ingress -> Redis durable enqueue

1. Extend `internal/webhook/audit_handler.go` to publish every accepted audit event to Redis.
2. Keep current file dump behavior as optional debug path.
3. Return `5xx` on enqueue failure so Kubernetes audit backend retries.
4. Preserve request body size guardrails and path contract.

Exit criteria:

1. All accepted audit events are visible in stream (`XADD` count increases).
2. Handler correctness tests pass for success/failure paths.

### Phase 2: Redis consumer -> existing processing pipeline

1. Implement consumer worker runnable in `cmd/main.go` lifecycle.
2. Consume via consumer group (`XREADGROUP`) and `XACK` only after successful handoff to existing event pipeline.
3. On worker crash/stall, reclaim stuck entries via `XAUTOCLAIM`.
4. Start with one consumer process, then allow horizontal scaling.

Exit criteria:

1. No dropped events during pod restart test.
2. Pending Entries List (PEL) returns to steady state after failures.

### Phase 3: Idempotency and ordering controls

1. Add dedupe key in Redis:
   - key: `gitopsreverser:dedupe:<event_id>`
   - set with TTL (for example 24h) before downstream processing.
2. Partition lock per `{provider,branch}` using `SET NX EX` lock keys to avoid concurrent writers.
3. Define lock TTL renewal while worker is active.

Exit criteria:

1. Duplicate deliveries do not produce duplicate Git writes.
2. Parallel workers do not race on same branch writes.

### Phase 4: Audit-only ingestion cutover

1. Add runtime switch to disable validating-webhook correlation path.
2. Keep watch/reconcile pieces required for seed and rule-change reconciliation.
3. Remove dual-path assumptions from docs and operator config defaults.

Exit criteria:

1. Ingestion path for ongoing events is audit-only in production profile.
2. Reconcile startup/rule-change still works.

### Phase 5: Performance and durability validation

1. Add metrics:
   - `audit_ingest_requests_total`
   - `audit_enqueue_latency_seconds`
   - `redis_stream_lag`
   - `redis_pel_entries`
   - `event_end_to_end_latency_seconds`
   - `event_dropped_total`
2. Run repeatable load test profiles (low, nominal, burst).
3. Compare baseline (in-memory path) vs Redis mode on:
   - p50/p95/p99 ingest latency
   - sustained events/sec
   - recovery time after Git outage or pod restart
   - memory footprint under burst load

Exit criteria:

1. Throughput and reliability targets are met or tradeoff is explicitly accepted.
2. Performance report is committed under `docs/ci/`.

## Performance Expectations

Likely impact:

1. Slightly higher per-request latency due to networked enqueue.
2. Significantly better durability and restart recovery.
3. Better burst handling when Git is slow/unavailable.

Practical expectation is improved reliability under failure and load spikes, with small steady-state latency overhead.

## Testing Strategy

1. Unit tests:
   - Redis producer/consumer behavior
   - enqueue failure handling in audit handler
   - idempotency and lock behavior
2. Integration tests:
   - consumer group ack/reclaim flows
   - restart recovery with pending messages
3. E2E:
   - send audit events, verify Git output and Redis ack behavior
   - chaos case: stop worker, produce events, restart worker, verify replay

## Rollout Strategy

1. Stage A: `queue.mode=inmemory` in production, `redis` in non-prod shadow.
2. Stage B: enable `redis` for one environment with one replica.
3. Stage C: scale replicas and validate lock/ordering behavior.
4. Stage D: set audit-only ingestion as default.

## Open Decisions

1. One global stream vs stream-per-cluster/per-target
2. Dedupe TTL duration
3. Exact partition key for writer locks (`repo+branch` vs `target+branch`)
4. Dead-letter policy for poison events

## First Implementation Slice (Recommended)

Deliver first:

1. Redis producer in audit handler (Phase 1)
2. Minimal consumer runnable with `XREADGROUP` + `XACK` (Phase 2)
3. Basic lag/PEL metrics (part of Phase 5 instrumentation)

This gives durable ingress quickly and lets you measure performance early before deeper refactors.
