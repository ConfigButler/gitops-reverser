# Valkey Adoption Guide for GitOps Reverser

This document explains how to adopt Valkey in GitOps Reverser, how to install it, whether it should be bundled into the
Helm chart, and why Valkey Streams are a good fit for the event pipeline.

## 1. Recommendation Summary

1. Use **Valkey Streams + Consumer Groups** as the durable event bus.
2. Use **Valkey lock keys** (`SET key value NX EX`) for single active writer per `{repoURL, branch}` partition.
3. Keep Valkey as an **optional external dependency by default**.
4. Add an **optional Helm subchart** for convenience (disabled by default), not as the only deployment mode.

This aligns with the current project direction in `README.md` (planned HA via Valkey) and with the rewrite goals in
`docs/design/rewrite-from-scratch-recommendations.md`.

## 2. Why Valkey for GitOps Reverser

GitOps Reverser needs:

- Durable buffering during Git or network outages
- At-least-once delivery semantics
- Ordered processing per destination branch
- Replay/recovery after pod restarts
- Horizontal scaling with explicit ownership

Valkey can provide all of this with a small operational footprint compared to larger brokers.

## 3. Valkey Concepts Most Useful for GitOps Reverser

1. **Streams (`XADD`, `XREADGROUP`)**
- Durable append-only log for Kubernetes events.
- Natural fit for event-driven ingestion and replay.

2. **Consumer Groups**
- Multiple workers can process one stream without duplicate active consumption.
- Supports scale-out and worker failover.

3. **Pending Entries List (PEL)**
- Tracks delivered-but-not-acked events.
- Critical for crash recovery and at-least-once guarantees.

4. **`XAUTOCLAIM` / `XCLAIM`**
- Reclaims stuck messages from failed workers.
- Required for robust HA operation.

5. **Ephemeral lock/lease keys (`SET NX EX`)**
- Enforce single active Git writer per partition key (`repo+branch`).
- Prevents concurrent push conflicts.

6. **Lua / Functions**
- Atomic multi-step updates (for example dedupe marker + enqueue + metric counter).

7. **Hashes / Sets**
- Store idempotency markers and short-lived processing metadata.

## 4. Why Streams Over Other Valkey Primitives

| Option | Good For | Limitation for GitOps Reverser |
|---|---|---|
| Pub/Sub | Low-latency fanout | No persistence, no replay, events lost on disconnect |
| Lists (`LPUSH`/`BRPOP`) | Simple queues | No built-in pending/claim model, weaker recovery story |
| Sorted sets as queue | Scheduled tasks | More custom logic for ack/retry/claim |
| Streams | Durable event pipelines | Slightly more operational and implementation complexity |

For this project, **Streams are the best default** because durability + replay + consumer recovery are all first-class.

## 5. Pros and Cons of Adopting Valkey

### Pros

- Durable queueing for API events
- Better HA posture than in-memory per-pod queues
- Clear backpressure and retry model
- Operationally lighter than larger message-broker stacks
- Fits branch-partitioned processing model

### Cons

- New dependency to operate and secure
- Requires careful stream retention and memory management
- At-least-once requires idempotent handlers (duplicates are possible)
- HA deployment (replication/failover) adds platform complexity

## 6. Should Valkey Be Part of the Helm Chart?

### Short answer

**Yes, as an optional subchart; no, as a mandatory bundled component.**

### Why

- Many users already have a cache/queue layer or prefer managed services.
- OSS adoption is smoother when users can start with existing infrastructure.
- A bundled option improves developer experience and quickstart path.

### Recommended chart strategy

1. `valkey.enabled=false` by default
2. If enabled, install a small Valkey instance suitable for dev/small environments
3. Expose `valkey.external.*` settings for production-grade external endpoints
4. Keep one config contract in GitOps Reverser (`VALKEY_ADDR`, auth/TLS, stream names)

## 7. Installation Paths

### 7.1 Local Development (Docker)

```bash
docker run -d --name valkey \
  -p 6379:6379 \
  valkey/valkey:latest

# quick sanity check
docker exec -it valkey valkey-cli PING
```

### 7.2 Kubernetes (Recommended: dedicated release)

```bash
helm repo add bitnami https://charts.bitnami.com/bitnami
helm repo update

# Example install (adjust values for your environment)
helm install valkey bitnami/valkey \
  -n valkey-system --create-namespace
```

After installation, point GitOps Reverser to the service DNS in that namespace.

### 7.3 Production HA Baseline

Use an HA-capable Valkey topology (replication + failover), with:

- Auth enabled
- TLS enabled where possible
- Persistent volumes enabled
- Resource requests/limits set
- Metrics enabled and scraped
- Backup/restore tested

## 8. Example Helm Values Strategy for GitOps Reverser

Example design (for a future chart update):

```yaml
# values.yaml (proposed)
valkey:
  enabled: false
  external:
    address: "valkey-primary.valkey-system.svc.cluster.local:6379"
    username: ""
    passwordSecretRef:
      name: "valkey-auth"
      key: "password"
    tls:
      enabled: false
      caSecretRef:
        name: ""
        key: "ca.crt"

pipeline:
  streamName: "gitopsreverser.events"
  consumerGroup: "git-writers"
  lockPrefix: "gitopsreverser:lock"
  dedupePrefix: "gitopsreverser:dedupe"
```

## 9. Example Event Flow with Streams

Producer:

```bash
valkey-cli XADD gitopsreverser.events '*' \
  event_id "01HRXYZ..." \
  repo "https://git.example/org/repo.git" \
  branch "main" \
  op "UPDATE" \
  gvr "apps/v1/deployments" \
  ns "default" \
  name "web"
```

Consumer group bootstrap (run once):

```bash
valkey-cli XGROUP CREATE gitopsreverser.events git-writers '$' MKSTREAM
```

Worker read and ack:

```bash
valkey-cli XREADGROUP GROUP git-writers worker-1 COUNT 50 BLOCK 5000 STREAMS gitopsreverser.events '>'
# process events
valkey-cli XACK gitopsreverser.events git-writers <event-stream-id>
```

Recover stuck events from dead workers:

```bash
valkey-cli XAUTOCLAIM gitopsreverser.events git-writers worker-2 60000 0-0 COUNT 100
```

Lease lock for a partition (`repo+branch`):

```bash
valkey-cli SET gitopsreverser:lock:<partition-hash> worker-1 NX EX 30
# renew periodically while active
valkey-cli EXPIRE gitopsreverser:lock:<partition-hash> 30
```

## 10. Mapping to GitOps Reverser Event Pipeline

Target flow:

1. Audit/Webhook ingress receives Kubernetes event
2. Rule compiler resolves matching `GitTarget`
3. Event normalizer generates deterministic idempotency key
4. Event appended to Valkey stream partition
5. Worker from consumer group acquires partition lease
6. Worker sanitizes/writes/commits/pushes to Git
7. Worker acks stream entry
8. Status projector updates CR conditions and metrics

## 11. Risks and Mitigations

1. **Duplicate delivery** (normal in at-least-once)
- Mitigation: deterministic idempotency keys + dedupe TTL store.

2. **Unbounded stream growth**
- Mitigation: retention policy (`MAXLEN`/time-based trim), dead-letter stream, monitoring.

3. **Lock split-brain**
- Mitigation: short TTL + frequent renew + fencing token/version checks.

4. **Valkey outage**
- Mitigation: HA topology, pod disruption controls, alerts, tested failover runbooks.

## 12. Phased Adoption Plan

1. Add Valkey client abstraction and feature flag (`pipeline.mode=inmemory|valkey`).
2. Implement producer path to stream and consumer group worker.
3. Add lock manager per `{repoURL,branch}` and idempotency markers.
4. Add stream metrics (lag, PEL, ack rate, reclaim count).
5. Run shadow mode in e2e (produce to Valkey, keep current writer authoritative).
6. Cut over writes to Valkey consumers.
7. Add optional Helm subchart and external Valkey config values.
8. Document HA runbook and SLO/alerts.

## 13. Final Decision Guidance

If your primary near-term goal is **single-cluster simplicity**, keep current mode and make Valkey optional.
If your goal is **multi-pod reliability and replayable event processing**, adopt Valkey Streams now.

For this project, the practical middle ground is:

- **Adopt Valkey Streams in the architecture**
- **Keep chart integration optional by default**
- **Support external managed/self-hosted Valkey as first-class**
