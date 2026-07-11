# Kubernetes API Priority and Fairness — and How to Tune It for High Concurrency

When you throw a sudden burst of requests at a Kubernetes API server — say, 300 clients all authenticating simultaneously — it doesn't just pass everything through. It applies a two-layer traffic control system: a hard concurrency ceiling called **max-requests-inflight**, and a sophisticated queuing layer called **API Priority and Fairness (APF)**. Understanding both is the key to making high-concurrency workloads reliable without adding more hardware.

---

## The inflight ceiling

Before APF even runs, the API server enforces a raw cap on simultaneous in-flight requests:

| Flag | Default | Controls |
|---|---|---|
| `--max-requests-inflight` | 400 | Non-mutating requests (GET, LIST, WATCH) |
| `--max-mutating-requests-inflight` | 200 | Mutating requests (POST, PUT, PATCH, DELETE) |

Any request that arrives when the ceiling is already hit gets an immediate `HTTP 429 Too Many Requests`. No queuing, no retry — just a rejection. The defaults are conservative: they were set for mid-2010s hardware and have barely moved since.

For a k3d/k3s cluster you can raise them at cluster creation time:

```bash
k3d cluster create my-cluster \
  --k3s-arg '--kube-apiserver-arg=--max-requests-inflight=800@server:0' \
  --k3s-arg '--kube-apiserver-arg=--max-mutating-requests-inflight=400@server:0'
```

Or, for an existing cluster started via a shell script, add those args to the `k3d cluster create` line in [test/e2e/cluster/start-cluster.sh](../../test/e2e/cluster/start-cluster.sh).

Raising the ceiling is a blunt instrument. It lets more requests compete for the same CPU and memory simultaneously, which can actually make things worse under sustained load. It is most useful when you have headroom on the node and bursty (not sustained) traffic.

---

## API Priority and Fairness (APF)

APF is the sophisticated layer that sits *inside* the inflight ceiling. It was promoted to stable in Kubernetes 1.29 and is on by default in all modern clusters. The goal is to prevent one noisy caller from starving every other caller — and to give operators control over how the shared budget is allocated.

APF is configured through two resource types:

- **`PriorityLevelConfiguration`** — defines a traffic class (a bucket of capacity and a queue)
- **`FlowSchema`** — defines which requests match a priority level, and how to group them into flows

### PriorityLevelConfiguration

A priority level can be `Exempt` (bypasses all limits, for system-critical traffic like the kubelet) or `Limited` (gets a share of the inflight budget and a queue).

For `Limited` levels the key fields are:

```yaml
spec:
  type: Limited
  limited:
    nominalConcurrencyShares: 100   # relative share of the inflight budget
    limitResponse:
      type: Queue
      queuing:
        queues: 64          # number of independent queues (more = better fairness)
        handSize: 6         # how many queues each request is assigned to (shuffle sharding)
        queueLengthLimit: 50  # max requests waiting per queue before rejection
```

**`nominalConcurrencyShares` (NCS)** is a proportional share, not an absolute count. If the total inflight ceiling is 400 and the sum of NCS across all active priority levels is 1000, a level with 100 NCS gets `400 × (100/1000) = 40` concurrent slots. This is recalculated dynamically as levels become active and idle.

**`queues` and `handSize`** together implement *shuffle sharding*: each incoming request is hashed into `handSize` candidate queues and placed in the shortest one. This probabilistically isolates a misbehaving caller — they are unlikely to land in the same queues as well-behaved callers. More queues mean better isolation but higher memory overhead.

**`queueLengthLimit`** is the per-queue depth. Once a queue is full, new requests are rejected with `HTTP 429`. The total buffer capacity is `queues × queueLengthLimit`.

### FlowSchema

A FlowSchema matches requests to a priority level and assigns a *flow distinguisher* — the key used to group requests within a priority level's queues.

```yaml
spec:
  matchingPrecedence: 800           # lower = evaluated first
  priorityLevelConfiguration:
    name: my-priority-level
  distinguisherMethod:
    type: ByUser                    # or ByNamespace, or None
  rules:
    - subjects:
        - kind: ServiceAccount
          serviceAccount:
            name: my-sa
            namespace: my-ns
      resourceRules:
        - verbs: ["get", "list"]
          apiGroups: ["mygroup.example.com"]
          resources: ["myresources"]
          namespaces: ["*"]
```

**`matchingPrecedence`** controls evaluation order. Every request is matched against all FlowSchemas sorted by ascending `matchingPrecedence`; the first match wins. The built-in `exempt` schema has precedence 1; `catch-all` has 10000.

**`distinguisherMethod`** determines how flows are separated within the priority level's queues:
- `ByUser`: one flow per authenticated user/SA — fair between callers but concentrates load from a single busy SA into one flow
- `ByNamespace`: one flow per source namespace
- `None`: all matched requests go into a single flow

### The built-in priority levels

Kubernetes ships with several built-in levels you cannot delete (but can reference):

| Level | NCS | Purpose |
|---|---|---|
| `exempt` | — | Bypass all limits; for `system:masters` and health checks |
| `node-high` | 40 | High-priority node requests (e.g. kubelet status updates) |
| `system` | 30 | Other system components |
| `leader-election` | 10 | Controller leader election; small but never queued behind workload |
| `workload-high` | 40 | Workload controllers (Deployments, etc.) |
| `workload-low` | 100 | Generic service account requests |
| `global-default` | 20 | Catch-all for everything else |
| `catch-all` | 5 | Last resort; very small to force explicit classification |

The `service-accounts` built-in FlowSchema (precedence 9000) sends all service account requests to `workload-low`. This is a large bucket (100 NCS) but uses `ByUser` distinguishing — so all requests from a *single* service account share one flow, and a burst from that SA competes only with itself within a few queues.

---

## What "ByUser" means in practice under load

This is the subtle behaviour that surprises most people.

Suppose your application makes 300 concurrent GET requests from the same service account. With `ByUser` distinguishing, every single one of those 300 requests maps to the *same flow key*. Shuffle sharding spreads them across `handSize` (default 6) candidate queues, but they still all land in the same small region of the queue space. The queue depth limit kicks in quickly, and the rest get rejected.

Compare this to 300 requests each from a *different* user. Those spread across all 64 queues, and each individual user's queue is much less likely to overflow.

The practical implication: if one service account drives high concurrency, `ByUser` works against you. `ByNamespace` or `None` (with a dedicated priority level sized for that traffic) is usually better.

---

## Creating a dedicated lane for a high-concurrency workload

The right pattern for a workload that legitimately needs high concurrency is to pull it out of the shared buckets entirely and give it its own priority level with a matching FlowSchema:

```yaml
apiVersion: flowcontrol.apiserver.k8s.io/v1
kind: PriorityLevelConfiguration
metadata:
  name: quiz-realtime
spec:
  type: Limited
  limited:
    nominalConcurrencyShares: 400
    limitResponse:
      type: Queue
      queuing:
        queues: 16
        handSize: 4
        queueLengthLimit: 200
---
apiVersion: flowcontrol.apiserver.k8s.io/v1
kind: FlowSchema
metadata:
  name: quiz-auth-service
spec:
  matchingPrecedence: 800
  priorityLevelConfiguration:
    name: quiz-realtime
  distinguisherMethod:
    type: ByResource        # spread load by resource type, not by SA identity
  rules:
    - subjects:
        - kind: ServiceAccount
          serviceAccount:
            name: quiz-access
            namespace: vote
      resourceRules:
        - verbs: ["get", "list", "watch", "create"]
          apiGroups: ["examples.configbutler.ai"]
          resources: ["quizsessions", "quizsubmissions"]
          namespaces: ["vote"]
```

Key decisions here:

- **`matchingPrecedence: 800`** ensures this FlowSchema is evaluated before the generic `service-accounts` schema at 9000. Without this, the SA would still fall into `workload-low`.
- **`nominalConcurrencyShares: 400`** gives this level a large share. With a 400-request ceiling and all NCS summing to around 600 (400 + the ~200 from other active levels), this level gets roughly 260 concurrent slots — far more than the default 100 NCS of `workload-low`.
- **`ByResource` distinguishing** means the flow key is the resource type being accessed, not the SA. Concurrent reads and writes spread across different flows instead of piling onto one.
- **`queues: 16, queueLengthLimit: 200`** gives a total buffer of 3200 requests, with 16 isolated regions for shuffle sharding.

---

## Would more nodes help?

Adding **worker nodes** in k3d does not help at all. Worker nodes run workloads but do not run the API server. All of the APF logic lives in the API server process, which runs on the server node(s) only.

Adding **server nodes** (k3d HA mode with multiple server nodes) gives you more API server instances behind the load balancer, which does increase total inflight capacity linearly. However:

- k3d HA is significantly more complex to configure and maintain
- Writes still go through a single etcd leader
- The improvement for *read-heavy* burst traffic is real but rarely worth the operational overhead

For a single-server k3d cluster, the right approach is always: raise the inflight ceiling if the node has headroom, then create a dedicated APF lane for the high-concurrency workload. These are both pure YAML/flag changes that take effect without restarting the cluster.

---

## Observability

APF exposes Prometheus metrics under the `apiserver_flowcontrol_` prefix:

| Metric | What it tells you |
|---|---|
| `apiserver_flowcontrol_rejected_requests_total` | Requests dropped; labeled by `flow_schema`, `priority_level`, `reason` |
| `apiserver_flowcontrol_request_wait_duration_seconds` | Time spent in queue before execution |
| `apiserver_flowcontrol_request_execution_duration_seconds` | Time actually executing (in the API server) |
| `apiserver_flowcontrol_current_inqueue_requests` | Current queue depth per priority level |
| `apiserver_flowcontrol_current_executing_requests` | Current in-flight count per priority level |

The fastest diagnostic: `kubectl get --raw /metrics | grep apiserver_flowcontrol_rejected`. Any non-zero `reason=queue-full` or `reason=concurrency-limit` on a priority level points directly at the bottleneck.

You can also inspect the live configuration at any time:

```bash
kubectl get prioritylevelconfiguration
kubectl get flowschema
kubectl get --raw /debug/api_priority_and_fairness/dump_priority_levels
kubectl get --raw /debug/api_priority_and_fairness/dump_queues
```

The `dump_queues` endpoint shows the current queue depths per priority level in real time — useful for catching thundering-herd events as they happen.
