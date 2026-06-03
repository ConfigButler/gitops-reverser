# High Availability and GitTarget Distribution Plan

Status: **proposed** (not started)

## Goal

Make GitOps Reverser run safely with multiple pods while preserving the most
important write invariant:

> At any moment, only one pod should own work for a given Git branch, and every
> push must still be protected by remote-branch compare-and-swap semantics.

The cluster's Kubernetes state should be observable from multiple
`gitops-reverser` pods, and `GitTarget`s should be distributable across those
pods. The write path must still serialize every write that can touch the same
branch. The safety model is at-least-once delivery plus idempotent replay, not
exactly-once processing.

## Current Shape

The current implementation is intentionally single-active:

- [AuditConsumer](../../internal/queue/redis_audit_consumer.go) uses Redis
  consumer groups, but `NeedLeaderElection()` returns `true`, so only the leader
  drains the canonical audit stream.
- [WorkerManager](../../internal/git/worker_manager.go) also participates in
  leader election. It creates in-process [BranchWorker](../../internal/git/branch_worker.go)
  instances keyed by `(GitProvider namespace, GitProvider name, branch)`.
- [BranchWorker](../../internal/git/branch_worker.go) serializes commits and
  pushes for that branch key inside one pod. This protects the branch locally,
  but does not by itself protect a multi-pod deployment.
- [GitTargetEventStream](../../internal/reconcile/git_target_event_stream.go)
  buffers live events while snapshots are reconciling and deduplicates events
  per target, but its state is in memory and pod-local.
- [watch.Manager](../../internal/watch/manager.go) is also leader-elected, so
  discovery, informer lifecycle, and snapshot emission happen on one active pod.

This means Redis/Valkey already helps with ingress durability and future
failover, but the Git write ownership boundary is still process-local.

## Audit Ingress Fan-In

All pods can safely serve the audit webhook and append to the same canonical
audit stream, as long as webhook handling stays a **producer-only** path:

- each pod receives `/audit-webhook` and `/audit-webhook-additional` traffic;
- each pod performs request decode, validation, audit body joining, and
  canonical event preparation;
- each pod appends accepted events to the shared Redis/Valkey stream with
  [RedisAuditQueue](../../internal/queue/redis_audit_queue.go);
- no ingress pod routes directly to a local `BranchWorker`.

Redis stream append is atomic across producers, so multiple pods can `XADD` to
the same stream. The Redis-backed audit joiner is also compatible with multiple
ingress pods because body parking, decision claims, commit, and release all use
shared Redis keys keyed by audit ID.

This does not create a new perfect ordering guarantee. The canonical stream
orders events by enqueue time, not by Kubernetes resource version or by a global
API-server sequence. With multiple API servers, webhook retries, load-balanced
HTTP requests, and optional additional audit sources, Kubernetes audit delivery
can already arrive slightly out of order. Multiple ingress pods can make that
arrival-order reality more visible, but they are not the fundamental source of
it.

The design response should be:

- treat the canonical audit stream as an at-least-once ingress log, not an
  exactly-once ordered history;
- preserve event metadata such as deterministic event id, audit ID, stage
  timestamp, user, object reference, operation, and object resource version when
  available;
- keep ordering-sensitive Git writes serialized later by branch write shard;
- make replay and duplicate handling idempotent in the shard writer;
- use snapshot/reconcile as the correction path when audit ordering or delivery
  produces an uncertain derived Git view.

So the answer is "yes, all pods can push audit events to the same queue," but
that only solves ingress availability. It does not by itself make consumers,
branch workers, or snapshots active/active-safe.

## Resource-Type Sequencing Queues

An optional layer between audit ingress and branch-shard writes is a set of
small **resource-type queues**:

```text
ResourceTypeQueue = API group + resource type
```

This is deliberately narrower than "API group." Kubernetes `resourceVersion`
ordering is only meaningful for objects from the same API group and resource
type when served by kube-apiserver, per the Kubernetes
[resource versions](https://kubernetes.io/docs/reference/using-api/api-concepts/#resource-versions)
rules. For example, two `apps/deployments` resource versions can be ordered, but
`apps/deployments` and `apps/replicasets` cannot be ordered just because both
are in the `apps` group. For extension API servers, numeric ordering is only
safe when both resource version strings parse as decimal numbers; otherwise
equality-only comparison is the safe fallback.

The useful shape is:

1. Audit ingress appends to the canonical stream, or directly to a
   resource-type stream after lightweight GVR extraction.
2. A resource-type sequencer consumes events for one group/resource.
3. It holds a short reorder window, sorts comparable arrived events by
   `metadata.resourceVersion`, and coalesces stale updates for the same
   `(namespace, name)`.
4. It fans the resulting event to every active `GitTarget` whose `WatchRule` or
   `ClusterWatchRule` matches that resource type.
5. The fan-out still writes to branch-shard queues, where Git ordering and
   branch ownership are enforced.

This can improve local ordering and reduce redundant writes for noisy resource
types. It also gives a clean fan-out point: one Kubernetes mutation can be
sequenced once, then delivered to many target-specific branch queues.

The reorder window must stay bounded. Resource versions are orderable, but they
are not a promise of contiguous per-type integers that lets a client prove "RV
123 is missing, so wait until it arrives." Gaps may be normal. The sequencer can
delay briefly to let near-simultaneous out-of-order deliveries settle; after the
window expires, it should emit what it has and rely on idempotent replay plus
snapshot/reconcile for correction.

Queue key choice should probably be **group/resource** rather than full GVR.
Different served versions of the same group/resource represent the same
underlying objects, while `WatchRule` planning and routing can still retain the
observed API version in the event payload. If implementation convenience starts
with GVR queues, the design should still normalize high-water marks and
deduplication at the group/resource level where Kubernetes comparison rules
apply.

This layer is not a prerequisite for branch-safe HA. It is a later refinement if
audit ordering noise or fan-out cost becomes visible in practice.

## Desired Model

Move from "one leader owns everything" to "many pods may ingest and route, but
each write shard has exactly one owner."

The first durable shard should be a **branch write shard**:

```text
BranchWriteShard = canonical Git remote identity + branch
```

Using only `GitProvider namespace/name + branch` is not sufficient forever,
because two `GitProvider` objects may point at the same repository and branch.
That collision is already tracked in [TODO.md](../TODO.md). HA should resolve
that at the same time by normalizing branch ownership around the remote identity
that actually receives the push.

Each `GitTarget` maps to exactly one branch write shard, while one branch write
shard may serve many `GitTarget`s that write to different paths.

`GitTarget`s can still be spread across pods. The distribution rule is that
their **write owner** is selected by branch shard, not by target name alone. A
pod may own many branch shards, and a branch shard may own many targets.

## Why Not One Queue Per GitTarget First?

A per-`GitTarget` queue is attractive because it isolates target backlogs and
matches the user's mental model. It is not sufficient as the first HA primitive:
two targets can write different paths on the same branch, and two independent
target queues could then produce two independent push loops.

The safer shape is:

- route events with target identity preserved;
- partition durable write queues by branch write shard;
- optionally add per-target subqueues or priority lanes inside the shard later;
- let exactly one branch owner coalesce, commit, and push all target events for
  that branch.

That gives the desired spread across pods without violating the branch-push
invariant.

## Delivery and Fencing Model

The durable write path should assume **at-least-once** delivery:

- Redis/Valkey streams retain work until it is acknowledged after the Git write
  path reaches a durable terminal point.
- Crash recovery may replay already-seen events.
- Replayed events must be safe through deterministic event ids, content hashes,
  current remote state, and existing `PushAtomic` conflict handling.

The branch-owner lease is an ownership and coordination mechanism, not the final
correctness fence for Git itself. Git remotes do not provide a native fencing
token that can reject a push because a Redis or Kubernetes lease changed while
the network operation was in flight. A lease check immediately before `git push`
would still be a time-of-check/time-of-use race.

Therefore the true write fence remains the remote ref compare-and-swap performed
by [PushAtomic](../../internal/git/git_atomic_push.go): a push is valid only if
the remote branch is still at the expected root. The lease prevents duplicate
work and keeps one intended owner per branch shard; `PushAtomic` protects the
remote branch if a stale owner races during failover or a slow push.

## First Work Item: Queue-Based Branch Ownership

This is the first HA milestone because it moves branch work onto a durable
handoff before informer or snapshot work is spread across pods. HA-0 and HA-1
do **not** by themselves unlock multiple active writer pods; they prepare the
write path while it is still leader-elected. HA-2 is the phase that makes
active writer distribution safe.

### Phase HA-0: Make Same-Branch Ownership Explicit

Introduce a small abstraction around the current branch key:

- compute a canonical `BranchWriteShardID` from resolved `GitProvider` remote
  identity and branch;
- keep the current `BranchWorker` behavior, but key worker creation by
  `BranchWriteShardID`;
- expose the computed shard in logs, metrics, and possibly `GitTarget.status`;
- detect multiple `GitProvider`s that resolve to the same remote + branch and
  make them converge onto the same shard;
- reject or clearly degrade configurations where two `GitTarget`s would write
  overlapping paths on the same shard.

Overlapping-path detection should start as controller reconciliation logic that
sets a degraded status condition before registering the target with the shard.
Admission-time validation is useful for simple literal paths, but it cannot be
the only guard if future target paths become templated or otherwise dynamic.
Runtime conflict detection in the branch owner should remain a defense-in-depth
check before committing a batch.

This can still run single-pod. The purpose is to name the invariant in code
before distributing it.

### Phase HA-1: Per-Shard Redis Work Queues

Split the current single audit-consumer-to-local-worker handoff into durable
per-shard queues:

1. The canonical audit stream remains the ingress queue from kube-apiserver.
2. The active consumer reads audit events, matches them against rules, sanitizes
   the object, and produces one `git.Event` per matched `GitTarget`.
3. Instead of directly calling the local `GitTargetEventStream` / `BranchWorker`,
   the router appends each write event to the Redis stream for that target's
   `BranchWriteShardID`.
4. The still-leader-elected branch owner consumes that shard stream and owns the
   in-memory `GitTargetEventStream`s plus the single `BranchWorker` for the
   shard.

HA-1 is a compatibility and durability step, not active/active writing. It can
coexist with the current direct handoff behind a feature flag or versioned
runtime mode: existing installs keep direct in-process routing, while the new
mode writes to `gitopsreverser.write.shard.v1.*` streams. The `v1` stream name
is intentionally versioned so payload changes can be introduced without
silently confusing old consumers.

Once HA-2 adds shard leases, any active consumer may read audit events, match
them against rules, sanitize the object, and append the resulting write events
to the appropriate shard queue. A branch owner pod then consumes that shard
stream and owns the in-memory `GitTargetEventStream`s plus the single
`BranchWorker` for the shard.

Suggested stream shape:

```text
gitopsreverser.write.shard.v1.{shardID}
```

Payload should include enough context to route without re-reading mutable CRDs:
target namespace/name, target path, provider identity, branch, resource
identifier, operation, user, timestamp, sanitized object payload or tombstone,
and a deterministic event id for deduplication.

This preserves event ordering per branch shard. Once HA-2 enables multiple
active consumers, the same partitioning also keeps ordering stable even if
several pods ingest audit events. It lets a remote outage stall only the
affected shard queue.

### Phase HA-2: Lease Branch Shards

Add ownership leases for branch write shards:

- each shard has one owner pod and a short renewable lease;
- only the lease holder may consume that shard's write stream or attempt to push
  that branch;
- when ownership changes, the new owner claims pending Redis messages, rebuilds
  any required local clone state from the remote branch, and resumes;
- shutdown drains or hands off without acknowledging work that has not reached
  Git.

This is the point where the deployment can safely run multiple active writer
pods. The Redis consumer group alone is not enough: consumer groups prevent two
pods from receiving the same queue entry, but they do not prevent two different
local branch workers from pushing to the same branch after independent routing.
The lease backend choice affects operational behavior and failover latency, but
not the final Git safety property: stale owners must still lose to the remote
ref compare-and-swap.

### Phase HA-3: Durable Per-Target Stream State

Move the correctness state currently held by `GitTargetEventStream` out of the
pod:

- reconciliation state (`RECONCILING` vs `LIVE_PROCESSING`);
- buffered live events during snapshot/reconcile;
- processed content hashes;
- pending snapshot/reconcile delivery markers.

This can be stored in Redis first. Some low-churn status markers may also belong
in `GitTarget.status`, but the hot dedup and buffering path should not depend on
Kubernetes status writes.

Where the snapshot path needs a freshness boundary, prefer a collection
`resourceVersion` watermark over open-ended content-hash buffering: a snapshot is
authoritative as of the collection RV it was listed at, so only live events newer
than that RV (for the same group/resource) need to be replayed on top, and
anything at or below it is already reflected. Content hashes then become a
secondary idempotency guard rather than the primary buffering mechanism. See
HA-4 for the resume side of the same watermark.

This makes branch-owner failover safe during an in-flight snapshot, not merely
during ordinary live-event processing.

## Replicating Kubernetes State Across Pods

After branch ownership is safe, the Kubernetes observation side can evolve in
two layers.

### Phase HA-4: Active/Passive Snapshot State

Keep one active `watch.Manager`, but persist enough state that a newly elected
leader can resume instead of starting from an empty in-memory contract:

- pending and last-delivered rule-set hashes;
- per-target snapshot delivery status;
- tracked GVR completeness and degraded discovery state;
- last-seen resource hashes or resource versions used for deduplication.

Persisting the per-`(group/resource[, namespace])` collection `resourceVersion`
lets a newly elected leader **resume the watch from that point** instead of
relisting cold, and gives snapshot reconciliation a precise watermark rather than
relying only on content hashes. Kubernetes
[consistent reads from the watch cache](https://kubernetes.io/docs/reference/using-api/api-concepts/#resource-versions)
graduated to GA in 1.34 and is stable in 1.35+, so a consistent LIST now returns
a trustworthy collection `resourceVersion` cheaply from cache instead of forcing
a quorum read against etcd. HA can therefore lean on RV-based watermarks and
watch resume more than earlier client guidance allowed. Two constraints still
hold and must be coded for: RV remains comparable only within one group/resource
(never collated across resource types), and a persisted RV that has aged past the
API server's compaction horizon returns `410 Gone` and must fall back to a fresh
relist.

This matches the low-risk path already sketched in
[design-snapshot-engine-evolution.md](design-snapshot-engine-evolution.md#34-multi-pod--ha).

### Phase HA-5: Active/Active Tracking Shards

Only after the state model is durable, shard tracking across pods by
`(GVR, namespace)` or another explicit tracked-set key. Each tracking shard has
its own lease, informer lifecycle, and completeness state.

Snapshots then become a fan-in problem: a single `GitTarget` may need state from
many tracking shards. Snapshot emission should therefore be a queued operation
that waits for all required shards to be synced before producing authoritative
absence/deletion facts.

## Relationship To WatchRule Wildcards

[WatchRule wildcard support](watchrule-wildcard-support-plan.md) increases the
number of GVRs a single target may watch. That stresses informer scale and
snapshot completeness, but it should not be the first distributed-systems
problem solved.

Recommended ordering:

1. Ship HA-0/HA-1 branch-shard queueing first, so wildcard events can be routed
   to a durable per-branch stream instead of a local in-process worker.
2. Then implement wildcard resolver expansion and status visibility.
3. Gate "wildcard support is done" on snapshot robustness, especially per-GVR
   list failure handling.
4. Only later shard the watch/tracking engine across pods.

This lets wildcard work proceed without accidentally creating a world where
multiple pods can push the same branch.

## Failure Cases To Design For

- **Pod dies after dequeue, before push.** Redis pending-entry reclaim must make
  the event visible to the new shard owner.
- **Pod dies after local commit, before push.** The new owner rebuilds from the
  remote and replays retained/pending writes; local commits are disposable.
- **Pod dies after push, before ACK.** Replayed events must be idempotent via
  deterministic event ids, content hashes, and remote-state replay.
- **Lease expires during slow push.** The stale owner may complete wasted work,
  but the remote ref compare-and-swap must prevent it from overwriting a newer
  owner. The owner should keep renewing during long operations to reduce churn,
  but renewal is not the Git fence.
- **Graceful handoff during rolling deploy.** The old owner should stop reading
  new shard work, finish or abandon in-flight work without premature ACKs, and
  let the new owner resume from the shard stream without duplicate divergent
  commits.
- **Remote branch moves externally.** Existing `PushAtomic` conflict handling
  still applies, but replay must draw from the shard queue/state rather than only
  local memory.
- **Two providers point to the same branch.** They must map to one
  `BranchWriteShardID`; otherwise HA is unsafe even if each provider-local
  worker is serialized.
- **Persisted resource version too old (HTTP 410 Gone).** A resume `resourceVersion`
  that has aged past the API server compaction horizon must trigger a fresh relist
  and reconcile rather than a hard failure. Watch bookmarks should be used to keep
  the persisted RV recent and reduce how often this fallback fires.

## Acceptance Criteria

- Multiple pods can receive audit webhook traffic and append canonical audit
  events.
- Official and additional audit payloads may land on different ingress pods and
  still produce one canonical event decision per audit ID.
- Multiple pods can route matched events to write-shard queues.
- For a given canonical remote + branch, exactly one pod owns the branch worker
  and pushes at a time.
- Killing the owner pod during queued, committed-but-unpushed, and post-push
  windows does not lose events or produce duplicate divergent commits.
- Rolling deploys and voluntary lease handoff do not lose events or produce
  duplicate divergent commits.
- Independent branch shards continue processing when one remote or branch is
  slow, broken, or rate-limited.
- The README can remove the blanket "HA is not supported yet" statement only
  after branch-shard ownership and failover are covered by e2e tests.

## Open Decisions

- What is the canonical remote identity: normalized URL, provider UID plus URL,
  resolved host/repo pair, or an explicit `GitProvider.status.remoteID`?
- Should per-shard queues be created lazily per active shard, or should Redis
  store all write events in one stream with `shardID` fields and consumer-group
  partitioning? Redis Cluster topology is a factor: stream-per-shard can
  distribute load naturally, while a single stream is simpler but can become a
  hotspot.
- Should Redis or Kubernetes `Lease` objects own branch-shard coordination?
  Either way, the lease is advisory for Git correctness; the remote ref
  compare-and-swap is the push fence.
- How much of `GitTargetEventStream` state belongs in Redis versus
  `GitTarget.status`?
- Should active/active audit consumers perform full rule matching, or should a
  central matcher fan out to shard queues first?
- Do shard writers need a per-resource monotonicity guard using resource version
  or timestamp, or is snapshot/reconcile correction enough for rare out-of-order
  audit delivery?
- Is a resource-type sequencing layer worth the extra streams and dynamic CRD
  lifecycle handling, or should branch-shard queues absorb audit events directly
  until ordering noise becomes a measured problem?
