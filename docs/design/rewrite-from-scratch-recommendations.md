# GitOps Reverser Analysis and Rewrite Recommendations

## 1. Executive Summary
The project has strong core ideas and good building blocks, but the orchestration layer is carrying too much complexity for its current reliability level.  
If you rewrite from scratch, I would **keep the Git write engine + CRD model direction**, and **redo ingestion, reconciliation wiring, event identity, and lifecycle management**.

Current baseline I validated:
- `make lint`: pass
- `make test`: pass
- Total unit coverage from `cover.out`: **52.6%**
- `make test-e2e`: not run in this pass

## 2. High-Level Component Overview (Current)

### Control Plane Components
1. **API/CRDs**: `GitProvider`, `GitTarget`, `WatchRule`, `ClusterWatchRule`.
2. **Controllers**: compile rules, validate targets/providers, bootstrap workers/streams.
3. **RuleStore**: in-memory compiled rule cache.
4. **Watch Manager**: dynamic discovery + informers + seed listing.
5. **Webhook Event Handler**: stores user attribution in correlation store.
6. **Correlation Store**: in-memory TTL/LRU bridge between webhook and watch paths.
7. **Event Router + Reconcile Manager**: route events and coordinate startup snapshot state.
8. **Git Worker Manager + Branch Workers**: branch-scoped buffering, commit, push.
9. **Audit Handler**: currently metrics/debug oriented, not main ingest path.

### Current Flow
`Kubernetes API` -> `validating webhook (correlation only)` + `watch informers (event source)` -> `event router` -> `GitTarget event stream` -> `branch worker` -> `git commit/push`

Main wiring is in `cmd/main.go:130`, `cmd/main.go:185`, `cmd/main.go:206`.

## 3. What Works Well (Keep)
1. **Branch-scoped serialization for Git writes** is a good concurrency model (`internal/git/worker_manager.go`, `internal/git/branch_worker.go`).
2. **Low-level atomic push logic** is valuable and reusable (`internal/git/git_atomic_push.go`).
3. **Sanitization and secret encryption hooks** are good foundations (`internal/sanitize/sanitize.go`, `internal/git/content_writer.go`).
4. **CRD separation of concerns** is directionally correct (`api/v1alpha1/gittarget_types.go`, `api/v1alpha1/gitprovider_types.go`, `api/v1alpha1/watchrule_types.go`).
5. **Lifecycle gate concept in `GitTarget`** is a solid UX contract (`internal/controller/gittarget_controller.go:480`).

## 4. Main Risks and Architecture Gaps

| Severity | Finding | Why it matters | Evidence |
|---|---|---|---|
| High | Resource identity keys omit namespace | Cross-namespace collisions in dedupe/reconcile keys | `internal/types/identifier.go:89`, `internal/watch/manager.go:299`, `internal/reconcile/folder_reconciler.go:173`, `internal/reconcile/git_target_event_stream.go:152` |
| High | Snapshot create/reconcile events can be dropped | Reconciler emits events without object; stream skips non-delete events without payload | `internal/reconcile/folder_reconciler.go:140`, `internal/reconcile/git_target_event_stream.go:161` |
| High | Snapshot cluster-state query is over-broad | Lists cluster-wide and accepts all objects for target | `internal/watch/manager.go:767`, `internal/watch/manager.go:800` |
| High | Informer stop lifecycle is incomplete | cancel func stored as no-op; stale informers risk | `internal/watch/manager.go:1277`, `internal/watch/manager.go:1136` |
| High | Namespace removal diffs not handled cleanly | compare logic removes only at GVR level, not per namespace | `internal/watch/manager.go:1110`, `internal/watch/manager.go:1034` |
| High | Event loss under pressure | queue full drops events; routing failures are logged and dropped | `internal/git/branch_worker.go:157`, `internal/watch/event_router.go:239`, `internal/watch/informers.go:129` |
| Medium | Rule expressiveness not fully implemented | wildcard and many non-concrete rules are skipped in watcher planning | `internal/watch/gvr.go:41`, `internal/watch/gvr.go:89`, `internal/watch/manager.go:693` |
| Medium | Ignore-filter not implemented | noisy/system resources are not excluded by policy yet | `internal/watch/resource_filter.go:21` |
| Medium | Security posture is broad | read access is cluster-wide `*/*`; webhook failure policy is ignore | `config/rbac/role.yaml:27`, `config/webhook.yaml:16` |
| Medium | Naming/status drift still present | leftover GitRepoConfig reason strings and TODO readiness checks | `internal/controller/watchrule_controller.go:42`, `internal/controller/watchrule_controller.go:191`, `internal/controller/clusterwatchrule_controller.go:41`, `internal/controller/clusterwatchrule_controller.go:168` |
| Medium | Dual ingest complexity with single-pod limitation | correlation + watcher coupling and no HA | `README.md:43`, `README.md:180`, `docs/future/AUDIT_WEBHOOK_NATS_ARCHITECTURE_PROPOSAL.md:20` |
| Low | Audit path is not primary pipeline yet | currently mainly metrics/logging | `internal/webhook/audit_handler.go:214` |

## 5. Keep vs Redo (If Rewriting From Scratch)

| Component | Keep | Redo |
|---|---|---|
| Git commit/push core | Yes | Keep logic, isolate behind cleaner interface |
| CRD intent model | Mostly | Keep resource roles, tighten validation and immutable fields |
| Sanitization/encryption | Yes | Keep with plugin hooks and schema-aware policies |
| Worker partitioning by repo+branch | Yes | Keep, but add durable queue consumer semantics |
| Event identity/key model | No | Redo with canonical key including namespace/scope |
| Watch/informer manager | No | Redo as a separate ingestion adapter with explicit lifecycle |
| Reconcile orchestration | No | Redo to produce full payload events, not identifier-only for creates/updates |
| Correlation store bridge | No | Remove if moving to audit-first ingest |
| Audit handler | Partial | Promote to first-class ingest or remove experimental dead path |
| Ops/security defaults | No | Redo RBAC/webhook defaults and HA posture |

## 6. Recommended Rewrite Architecture

I recommend this target architecture:

1. **Rule Compiler Service**  
Compiles `WatchRule`/`ClusterWatchRule` into deterministic subscription plans per `GitTarget`.

2. **Ingestion Adapter(s)**  
Preferred: audit webhook ingestion from multiple source clusters. Optional: informer adapter for clusters without audit integration.

3. **Durable Event Bus (Valkey)**  
Valkey Streams as the durable queue with consumer groups and partitioning by `{repoURL, branch}` while carrying `clusterID` on every event.

4. **HA Coordination (Valkey)**  
Distributed lease/lock keys in Valkey to enforce single active writer per `{repoURL, branch}` while allowing many replicas.

5. **Normalizer**  
Single path for sanitize, metadata enrichment, canonical keying, dedupe policy.

6. **Reconciliation Service**  
Seed + orphan detection emits full resource events into the same bus.

7. **Git Writer Service**  
Consumes stream partitions, writes/commits/pushes with retry/idempotency.

8. **Status Projector**  
Updates `GitTarget`/`GitProvider` conditions from durable processing results.

9. **Cluster Registry + AuthN/AuthZ**  
Manages onboarded source clusters (`clusterID`, credentials/certs, policy), validates audit ingress identity, and applies per-cluster limits.

This aligns with the HA direction in `README.md:56` (Valkey-based HA).

## 7. Component Interaction and Event Pipeline

### 7.1 Component Map (Target Architecture)

1. **Kubernetes Sources**  
Kubernetes API audit events from multiple source clusters (primary) and optional watch/informer events (fallback adapter).

2. **Ingress/API Layer**  
Stateless controller pods receive events on `/audit-webhook/<cluster-id>`, authenticate source clusters, and convert events into canonical internal events.

3. **Rule Compiler + Resolver**  
Resolves `WatchRule`/`ClusterWatchRule` against `GitTarget`/`GitProvider` and produces deterministic routing metadata.

4. **Normalizer**  
Sanitizes payloads, enriches metadata (user, operation, `clusterID`, resource identity), computes idempotency key.

5. **Valkey Streams Bus**  
Durable event transport; events are appended to stream partitions keyed by `{repoURL, branch}`.

6. **Valkey Lock/Lease Manager**  
Ensures one active writer per partition key for ordered, conflict-minimized Git writes.

7. **Git Writer Workers**  
Consume stream entries, batch commits, push with retry/conflict handling, emit processing results.

8. **Status Projector**  
Projects pipeline outcomes back to `GitTarget`/`GitProvider` conditions and operational metrics.

9. **Reconciliation Producer**  
On startup/rule-change, lists desired resources and publishes seed/orphan events into the same stream path.

10. **Cluster Registry + Policy Layer**  
Stores cluster onboarding metadata and enforces per-cluster auth, routing, and backpressure policies.

### 7.2 Current Event Pipeline (As-Is)

```text
Kubernetes API
  ├─ Validating Webhook (stores correlation key -> user)
  └─ Dynamic Informers (main event source)
        -> Rule match
        -> Sanitize + dedupe
        -> Correlation lookup
        -> EventRouter
        -> GitTargetEventStream (startup buffer/live mode)
        -> BranchWorker queue
        -> Git commit/push
```

### 7.3 Target Event Pipeline (Valkey + HA)

```text
Source Clusters (N) Kubernetes API
    -> Audit Webhook /audit-webhook/<cluster-id>
    -> Ingress Adapter (authn + cluster policy)
    -> Rule compile/resolve
    -> Normalize + idempotency key (includes clusterID)
    -> Valkey Stream partition {repoURL, branch}
    -> Consumer Group worker candidates
    -> Valkey lease holder elected per partition
    -> Active Git writer processes ordered events
    -> Commit/push + ack
    -> Status/metrics projection
```

### 7.4 Reconciliation Pipeline (Seed and Drift)

```text
GitTarget/Rule change or controller startup
    -> Reconciliation producer computes desired scope
    -> Lists resources and computes orphans
    -> Publishes CREATE/UPDATE/DELETE reconcile events to Valkey Stream
    -> Same writer path as live events
    -> Converged Git state + projected conditions
```

### 7.5 Multi-Cluster Ingestion Model

```text
Cluster A/B/C
   -> audit-webhook/<cluster-id>
   -> validate cluster identity + policy
   -> normalize (clusterID mandatory)
   -> shared Valkey bus
   -> partition by target repo+branch
   -> single writer lease per partition
```

Notes:
1. `clusterID` must be first-class in event identity, metrics, dead-letter entries, and replay tooling.
2. Route policy should allow per-cluster filtering (allowed resources/namespaces) before enqueue.
3. Backpressure and quotas should be enforceable per cluster to prevent one noisy cluster from starving others.

## 8. HA-First Requirements (Valkey-Based)

1. **Replica-safe ingestion**: any pod can receive events and append to Valkey Stream.
2. **Single-writer per branch**: distributed lock key per `{repoURL, branch}` with short TTL + heartbeats.
3. **At-least-once delivery**: consumer groups with pending-entry recovery on crash/restart.
4. **Idempotent processing**: deterministic event key + processed marker in Valkey to handle redelivery safely.
5. **Backpressure handling**: stream length caps and dead-letter stream for poison events.
6. **Failover behavior**: lock expiry enables automatic writer takeover by another replica.
7. **Operational visibility**: metrics for lock ownership, pending backlog, retries, dead-letter counts.
8. **Multi-cluster identity**: all ingest paths require explicit `clusterID` and authenticated source mapping.
9. **Multi-cluster fairness**: enforce per-cluster quotas/rate limits and expose per-cluster lag metrics.

## 9. Concrete Improvements by Component

1. **Identity and dedupe**: replace `ResourceIdentifier.String()` as key source with canonical key that includes namespace.
2. **Cluster-aware identity**: include `clusterID` in canonical event identity, dedupe keys, and audit projections.
3. **Reconcile payloads**: emit full objects for create/update snapshot events.
4. **Informer lifecycle**: explicit per-namespace start/stop contexts; no placeholder cancel funcs.
5. **Delivery guarantees**: remove fire-and-forget drops; use Valkey Streams retry/dead-letter flows.
6. **Rule coverage**: support wildcard expansion via discovery cache.
7. **Security defaults**: reduce RBAC blast radius and move webhook failure policy to fail-open only when explicitly chosen.
8. **Testing**: prioritize watch/reconcile integration paths and raise coverage on low packages (`internal/watch`, `internal/ssh`, `cmd`).
9. **Multi-cluster e2e**: add tests for concurrent audit ingestion from multiple cluster IDs with fairness and isolation checks.

## 10. Suggested Rewrite Plan (Valkey + HA)

1. Build a new internal package boundary around `Event`, `RulePlan`, `BranchWriter` interfaces.
2. Port existing Git writer logic behind the new interfaces first.
3. Implement Valkey Streams producer/consumer with branch partition keys and consumer groups.
4. Implement Valkey lock/lease manager for single active writer per branch.
5. Add cluster onboarding model (`clusterID`, credentials, policy) and ingress auth for `/audit-webhook/<cluster-id>`.
6. Implement reconciliation producer (seed/orphan) publishing into Valkey Streams.
7. Implement ingest adapter (audit first, informer optional) publishing multi-cluster events into Valkey Streams.
8. Cut over `GitTarget` controller to status projection from processing pipeline.
9. Add multi-cluster e2e/load tests (fairness, isolation, replay).
10. Remove correlation store and legacy route paths.
