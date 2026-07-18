# High Availability and Durable Delivery Plan

Status: **proposed** (not started)

## Scope and Definition of Done

This plan updates the previous HA proposal for the current watch-first
architecture. Kubernetes WATCH is the source of mirrored object state. Audit is
optional attribution only; it is not a source of object state or a write queue.

The first release is active/passive HA, not active/active scheduling:

- Run at least two controller Pods.
- One elected Pod owns controllers, target watches, and Git branch workers.
- Other Pods remain ready to serve admission and audit endpoints, and can become
  the active Pod after the leader fails.
- Losing one controller Pod must not silently drop a Kubernetes-to-Git state
  change. The replacement may replay a change or create a no-op Git attempt.

The durability contract is eventual state convergence: after recovery, Git
matches the watched Kubernetes state. It is not a promise of one Git commit for
every Kubernetes mutation, nor an exactly-once event history.

This contract assumes the Kubernetes API, Git remote, and durable queue remain
available. A single Redis or Valkey Pod is therefore not sufficient for an
installation that also needs to survive loss of any one dependency Pod.

## Current State

The repository has useful foundations, but it is not HA today.

- The Helm chart rejects a replica count greater than one. See
  [validate-replica-count.yaml](../../charts/gitops-reverser/templates/validate-replica-count.yaml).
- The watch manager and worker manager declare that they need leader election,
  but the controller-runtime manager does not enable it. See
  [manager.go](../../internal/watch/manager.go) and
  [worker_manager.go](../../internal/git/worker_manager.go).
- Each GitTarget runs per-GVR, per-scope watches with initial-event replay and a
  mark-and-sweep resync. See [target_watch.go](../../internal/watch/target_watch.go).
- A live watch event currently goes through EventRouter and
  GitTargetEventStream into an in-memory BranchWorker FIFO. See
  [event_router.go](../../internal/watch/event_router.go) and
  [git_target_event_stream.go](../../internal/reconcile/git_target_event_stream.go).
- Redis currently stores resume cursors and author-attribution data. A cursor is
  written after the event enters the in-memory FIFO, but before the eventual Git
  push. A Pod crash in that interval can make a replacement resume past work
  that only existed in RAM. This is the blocker for lossless failover.
- BranchWorker keeps open commit windows, local commits, unpushed writes, and
  CommitRequest outcomes in memory. Its local clone is disposable.
- Git pushes already use a remote reference compare-and-swap. PushAtomic remains
  the final protection against a stale owner or an external remote update. See
  [git_atomic_push.go](../../internal/git/git_atomic_push.go).
- The chart has a PDB and preferred Pod anti-affinity, but these only improve
  placement; they do not make the data path durable or coordinate writers.

## Target Architecture: HA v1

HA v1 retains one active data-plane owner for the whole release. This is the
smallest design that satisfies loss of one controller Pod without introducing
distributed watch ownership.

    Kubernetes API WATCH
            |
            v
    active watch manager
            |
            v
    durable branch-shard journal  <---- Redis/Valkey, durable and HA
            |
            v
    active branch worker
            |
            v
    Git remote with compare-and-swap

The standby controller does not run target watches or branch workers. It does
run the non-leader HTTP servers, so admission and audit traffic can use the
Service endpoints on either Pod. Audit writes attribution facts to the shared
store and never directly writes Git.

Controller-runtime leader election owns the active/passive transition. The
leader lock must use a release-scoped Kubernetes Lease in the release namespace.
On lock loss, the old owner stops reading new durable work and cancels its
watches. A stalled old owner may still finish an already-started push; the
remote compare-and-swap rejects it if a newer owner has moved the ref.

## Durable Write Journal

Redis or Valkey becomes mandatory in HA mode. Add a versioned durable journal
under a branch-write-shard key, with Redis Streams used for delivery and
consumer-group recovery.

### Shard identity

The journal and worker key must be:

    BranchWriteShard = canonical remote identity + branch

The current GitProvider namespace/name plus branch key is not sufficient:
multiple GitProvider objects can name the same repository and branch. The
canonical remote identity should normalize the resolved Git URL and be hashed
for key and Lease names. It must be exposed in logs, metrics, and target status.

Every GitTarget maps to exactly one branch write shard. Targets sharing a remote
branch use one journal and one worker, even when they have different paths.
Overlapping paths must remain a reconciliation-time and writer-time refusal.

### Journal record

Introduce a versioned record that includes:

- target UID, namespace, and name;
- source cluster, GVR, namespace scope, resource identity, operation, and
  resource version;
- a deterministic idempotency key;
- the sanitized object or delete/field-patch payload;
- author attribution when available;
- the resolved branch-shard identity and target path;
- a kind for live event, snapshot member, snapshot start, or snapshot complete.

The object payload must be encrypted before it is written to Redis when it
contains a sensitive resource. Today plaintext sensitive content is only
transient in the Pod before the Git writer encrypts it. A durable queue changes
that boundary. Queue encryption needs a Kubernetes Secret-backed key, rotation
plan, TLS in transit, and tests proving plaintext secret fields are absent from
Redis values.

### Atomic handoff and acknowledgement

For each watched GVR and scope, persist the journal record and its resume cursor
in one idempotent Redis operation. A Lua script or transaction must make a
successful enqueue and cursor advancement inseparable. The key layout must also
work in Redis Cluster, including its same-hash-slot constraints.

The source watch may advance only after this durable handoff succeeds:

1. WATCH receives an event.
2. The active leader derives a journal record.
3. It atomically records the event and the new cursor.
4. The branch worker consumes the record and may create local commits.
5. It acknowledges the record only after the corresponding state reaches the
   remote Git branch successfully.

If the leader dies before step 3, the cursor is not advanced and the watch
replays. If it dies after step 3 but before step 5, the consumer group reclaims
the unacknowledged record. If it dies after the remote push but before the
acknowledgement, replay is harmless because the write is idempotent against the
current remote tree.

The worker must not rely on its local clone for recovery. A new owner fetches
the remote branch, replays retained journal records, and lets the existing
PushAtomic conflict/rebuild behavior settle external changes.

### Snapshot and replay delivery

Initial events and the list fallback currently produce an in-memory resync
request before storing the cursor. They need the same durable handoff as live
events.

Do not store an unbounded full snapshot in one Redis value. Journal a snapshot
start marker, ordered per-object snapshot members, and a snapshot-complete marker
with the collection resourceVersion. The branch worker applies the scoped
mark-and-sweep only after it has received the complete marker. A failed or
superseded snapshot remains replayable; a new leader can also enqueue a fresh
complete replay after the retained journal tail. HTTP 410 Gone continues to mean
fresh replay, never loss of the old journal tail.

## Implementation Phases

### HA-0: Specify and expose branch ownership

Before changing delivery:

- Add BranchWriteShard resolution from normalized remote URL plus branch.
- Update WorkerManager, EventRouter, metrics, and GitTarget status to use and
  report the shard identity.
- Detect multiple providers naming one remote branch and converge them on the
  same shard.
- Add overlap checks for target paths on a shard.
- Add a feature gate for the durable delivery path. Existing single-Pod installs
  retain the current direct in-memory path until migration is complete.

### HA-1: Add the durable journal in single-active mode

Add a journal package beside the existing Redis store:

- publish idempotent work records;
- create consumer groups, claim abandoned pending records, and acknowledge only
  remote-successful work;
- encode and encrypt sensitive payloads;
- atomically couple cursor advancement to durable publication;
- route both live events and snapshot/replay markers through the journal.

Refactor EventRouter so it publishes durable work instead of directly calling a
local GitTargetEventStream. Refactor BranchWorker so a durable consumer, rather
than its process-local FIFO, owns the acknowledgement lifecycle. The commit
window may remain in memory because unacknowledged records reconstruct it after
failover; local commits must be treated as disposable.

Ship and exercise this phase with one active Pod first. It removes the existing
crash-loss window independently of multi-Pod scheduling.

### HA-2: Enable active/passive controller failover

Enable controller-runtime leader election in the manager configuration and use a
release-specific Lease ID and namespace. Add explicit configuration for lease
durations rather than relying on undocumented defaults.

- Controllers, WatchManager, journal consumers, and BranchWorkers must require
  leadership.
- Admission, audit, metrics, health checks, and Redis readiness remain
  non-leader services.
- On leadership loss, stop consumption before starting any new Git work; leave
  unacknowledged records for the next leader.
- On startup, claim pending records, rebuild workers from remote Git, then
  establish watches from their durable cursors or fresh replay.
- Persist CommitRequest terminal results in Kubernetes status. A failover must
  not depend on an in-memory outcome map to decide whether a command completed.

### HA-3: Release the supported Helm mode

Only after HA-1 and HA-2 pass end-to-end fault tests:

- remove the replica-count rejection;
- reject HA configuration without a Redis endpoint, queue-encryption key, and
  TLS unless an explicitly documented trusted development exception is chosen;
- add leader-election and durable-queue values to the chart schema;
- add Lease RBAC and regenerate the chart RBAC artifact;
- use at least two replicas, RollingUpdate with maxUnavailable zero and
  maxSurge one, and a PDB with minAvailable one;
- make hostname anti-affinity and topology spread required for the HA profile;
  offer a zone-spread profile where clusters have multiple zones;
- document a supported Redis or Valkey durability topology. It must survive the
  failure model being advertised, not merely pass a ping.

The existing PDB can remain for single-Pod installs, but it must not be described
as HA by itself.

### HA-4: Fault-injection acceptance suite

Add e2e tests for each durable boundary:

- kill the active Pod before durable publication;
- kill it after publication and cursor advancement, before local commit;
- kill it after local commit, before remote push;
- kill it after remote push, before journal acknowledgement;
- force a slow push and leader handoff to exercise a stale writer;
- force remote branch movement and verify replay plus PushAtomic;
- force expired watch cursors and verify replay plus mark-and-sweep;
- roll the deployment with queued, replay, delete, and sensitive-resource work.

Every case must assert final remote Git state, no lost deletion, no divergent
ref, no permanently pending journal record, and a healthy replacement leader.
Run the normal repository validation sequence after implementation: task fmt,
task generate where needed, task vet, task lint, task test, and task test-e2e.

## Optional Follow-on: Active/Active Branch Shards

Do not make this a prerequisite for HA v1. It improves throughput and isolation,
but creates a second distributed-systems problem.

When needed, assign each BranchWriteShard its own Kubernetes Lease. Any Pod may
publish durable records, but only the shard-Lease holder consumes that shard and
pushes its Git branch. The lease is coordination, not the final Git fence; the
remote compare-and-swap remains mandatory.

Tracking and watching can remain leader-owned initially. Distributing target
watches or GVR scopes should happen only after the snapshot markers, replay
watermarks, deduplication, and completeness state are proven durable. A target
with several watched scopes must never sweep Git state until every required
scope has completed its authoritative replay.

## Acceptance Criteria for Supported HA

- A two-Pod controller deployment survives loss of the active Pod without
  silently skipping Kubernetes state.
- No durable cursor can advance past work that is absent from the journal.
- No journal record is acknowledged before its state is recoverable from remote
  Git.
- A stale or partitioned owner cannot overwrite a newer Git ref.
- Replays, queue claims, and post-push-before-ack crashes converge without
  divergent commits.
- Sensitive resource content is never stored as plaintext in the durable queue.
- The chart prevents unsupported HA configurations and documents dependencies
  that must themselves be highly available.
- README and chart documentation remove the single-Pod limitation only after
  the fault-injection suite passes.
