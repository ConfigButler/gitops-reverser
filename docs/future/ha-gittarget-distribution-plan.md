# High availability and durable delivery plan

Status: **proposed** (not started)

Reviewed 2026-10-03 alongside branch-worker steps 1 to 4 at `373bf8d7`. Redis/Valkey queue work
remains deferred. Complete the
[in-memory pause/resume contract](../design/gittarget-branch-worker-log.md#recovery-contract)
before starting these phases; a growing external queue does not remove the need for capacity
limits, visible failure state, or a recovery schedule.

This is the owning document for the durable journal, publication recovery, retention, and the
persistence/HA rollout. The [branch worker event model](../design/branch-worker-event-model.md)
owns worker transitions, deadline semantics, the publication retry (built in #412), and the
operation-timeout fix, which can ship independently of this plan.

## Scope and definition of done

This plan updates the previous HA proposal for the current watch-first
architecture. Kubernetes WATCH is the source of mirrored object state. Audit is
optional attribution only; it is not a source of object state or a write queue.

The first HA release uses active/passive ownership:

- Run at least two controller Pods.
- One elected Pod owns controllers, target watches, and Git branch workers.
- Other Pods remain ready to serve admission and audit endpoints, and can become
  the active Pod after the leader fails.
- Losing one controller Pod must not silently drop a Kubernetes-to-Git state
  change. The replacement may replay a change or create a no-op Git attempt.

The baseline durability contract is eventual state convergence under the configured write and
prune policies. A missed DELETE cannot be inferred from a snapshot under `prune.mode: onEvent`;
an `always` target permits scoped absence-based pruning. The proposed extension preserves accepted
save obligations, including window membership, attribution, messages, deadlines, and outcomes.
The event model defines those execution semantics; this plan owns how their records survive failure.

Before implementing the extension, agree on retention, target replacement, and the response to
an indeterminate publication result. Existing coalescing still applies, and rebuilding unpublished
work onto a moved remote can change its SHA or leave no diff. This is not a promise of one commit
per Kubernetes mutation or exactly-once publication under arbitrary history loss.

The phases and acceptance criteria below assume the stronger save contract. Agree on that scope
before HA-1. A convergence-only first release needs narrower phases and explicit save limitations;
it cannot claim the save-recovery criteria in this plan. Recovering pending work and terminal
receipts does not require retaining every historical transition forever.

A Git outage is recoverable within the configured storage budget if the remote eventually
returns. The journal must survive the advertised storage failures, and Kubernetes must become
available for source recovery and status projection. A single Redis or Valkey Pod is insufficient
when the installation must also survive loss of that dependency Pod.

## Current state

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
- Redis currently stores resume cursors and author-attribution data. A cursor is written after
  admission to the in-memory FIFO, before Git publication. A new watch stream starts with a fresh
  replay even with a stored cursor; later reconnects may resume it. Startup can repair current
  content under the write/prune policy, but cannot recover lost intermediate events or accepted
  save decisions. Persisted cursors alone do not establish lossless failover.
- BranchWorker keeps open commit windows, local commits, unpushed writes, and
  CommitRequest outcomes in memory. Its local clone is disposable.
- Steps 1 to 4 provide one decided-write log, one materializer, one retry schedule, and isolated
  replay refusals. Admission currently closes on retained payload bytes during a retry. Empty
  records, queued payloads, and producer deduplication still need the fixes in the linked plan;
  this threshold is not a durable retention contract or a total memory ceiling.
- Git pushes already use a remote reference compare-and-swap. PushAtomic remains
  the final protection against a stale owner or an external remote update. See
  [git_atomic_push.go](../../internal/git/git_atomic_push.go).
- The chart has a PDB and preferred Pod anti-affinity, but these only improve
  placement; they do not make the data path durable or coordinate writers.

## Target architecture: HA v1

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
remote compare-and-swap rejects it if a newer owner has moved the ref. It cannot reject an old
owner solely because its lease expired while the ref stayed unchanged. Require an ownership
epoch for journal transitions, reject stale writers there, and recover already-started pushes
through the publication protocol below. A stronger Git fence needs server-side enforcement.

## Durable write journal

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

URL normalization alone cannot prove that SSH and HTTPS URLs, redirects, or host aliases name the
same repository. HA-0 must define the supported identity and alias rules and document any aliases
it cannot unify. Strip credential material before deriving or exposing the identity.

Every GitTarget maps to exactly one branch write shard. Targets sharing a remote
branch use one journal and one worker, even when they have different paths.
Overlapping paths must remain a reconciliation-time and writer-time refusal. Retain provider and
target incarnations alongside the shard key; sharing a destination must not silently combine
incompatible policy or reroute old work to a replacement object with the same name.

Before combining providers, define credential selection and compatibility for branch permissions,
parent choice, and signing policy. Refuse incompatible bindings explicitly. A shared destination
does not authorize one provider's work to use another provider's permissions.

### Journal record

Give every record a payload schema version, branch identity, stable record identity, and branch
sequence. Version decision behavior as well as data layout so an upgrade can interpret pending
work. Carry the originating command or operation ID for deduplication. Include target and provider
incarnations where identity affects routing or policy. Branch sequence is execution order;
source metadata records provenance.

Retain the inputs, decisions, and results needed to reconstruct the workflow:

- Resource observations: sanitized object or field patch, resource UID and delete identity,
  source cluster incarnation, GVR and namespace scope, `resourceVersion`, target UID, path,
  operation, and resolved attribution.
- Snapshots: snapshot ID, start marker, members, completion marker, collection, and resource
  version. Preserve supersession decisions that affect execution or producer receipts.
- Saves: request UID, target UID, attribution decision, message, attachment and timing policies,
  first registration, and each withdrawal's answer.
- Windows: stable window ID, included input references or materialized coalesced content, save
  membership, closing reason, and captured planning policy.
- Operations: stable batch and attempt IDs, repository identity, intended base, resulting commit
  mapping, and observations that support success, failure, or uncertainty.
- Deadlines: purpose, owner, absolute due time, generation, and accepted firing or cancellation.
- Outcomes: per-request terminal result, pending status projection, and projection receipt.

This extends the earlier resource-only journal proposal. Use explicit data records instead of
serializing `WorkItem` or `PendingWrite`: their live interfaces, process pointers, and channels
have no meaning in another process. Keep reconstructable references to credentials and signers.
Historical policy references also need retained contents; a Kubernetes `resourceVersion` alone
cannot retrieve an arbitrary old configuration.

Encrypt sensitive resource payloads before writing them to the journal. Queue encryption needs
a Kubernetes Secret-backed key, a rotation and retention policy, TLS in transit, and tests that
plaintext secret fields are absent from Redis values. Keep private credentials out of records.

### Atomic handoff and acknowledgment

For each watched GVR and scope, persist the journal record and its resume cursor in one
idempotent Redis operation. A transaction or Lua script must make successful admission and cursor
advancement inseparable. The layout must work with Redis Cluster's same-hash-slot constraints.
Every supported producer needs an explicit acceptance result, including save commands.

Separate three durable milestones:

| Milestone | What it proves |
|---|---|
| Accepted | Input is recoverable; its source cursor may advance |
| Processed | Decisions and remaining obligations are recoverable |
| Completed | Publication or an explicit terminal outcome is proved |

Persist the consumed input position, resulting decisions, and new effect obligations together.
Otherwise a crash can consume a command but lose its push, or create an obligation twice.
Stable effect identities permit redelivery. A durable outbox holds external operations and
status updates still requiring execution; writing it does not prove those operations succeeded.

A complete path is:

1. WATCH receives an observation, or the controller submits a save command.
2. Admission durably records it and any source cursor in one idempotent operation.
3. The worker commits its processing position, decisions, and effects together.
4. The executor performs outstanding work and records the result, using the publication
   recovery protocol when the remote outcome is uncertain.
5. A completed input can be acknowledged while the outcome and any unfinished status projection
   remain recoverable. Terminal refusal and no-commit outcomes also need receipts.

A crash before step 2 cannot advance the durable cursor. After step 2, the consumer can reclaim
accepted work. A lost admission acknowledgment returns the same record on retry. A crash after
step 3 resumes the recorded effects without deciding window membership again. A crash after Git
accepted the push requires publication recovery before repeating an operation or reporting success.

Status projection progresses independently after completion. Retain checkpoints, payload
references, pending effects, and receipts needed by every unfinished step. Acknowledging delivery
or deleting a `CommitRequest` must not remove the only deduplication evidence while commands can
still be redelivered. Receipt collection needs a stated projection and redelivery boundary.

The local clone remains disposable. Recovery reconstructs the recorded workflow, uses the
remote as the Git base, and only replans unfinished publication. Raw resource redelivery alone
cannot restore the same window boundaries or attached saves.

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

Tie every member and completion marker to a snapshot ID and its exact scope. An incomplete
snapshot cannot authorize deletion; an explicitly complete empty one can authorize a scoped
sweep. Preserve ordering fences where live writes, overlapping snapshots, or saves prevent
replacement of an earlier queue position. Persist supersession and deferred-heal state when
needed to reproduce the worker's decisions. A successful local resync reply does not acknowledge
remote publication.

## Publication recovery

Git and the journal cannot participate in one ordinary storage transaction. There is always a
boundary between the remote ref update and recording its result. Treat a lost response as an
uncertain outcome, because a timeout can follow a successful server-side update.

The failure sequence is:

1. The worker durably records an intent to publish a batch.
2. Git accepts the push.
3. The worker dies before recording publication evidence.
4. Recovery finds an outstanding intent and must determine whether it already happened.

Neither a durable outbox nor an in-memory deduplication map resolves step 4. The earlier plan
relied on idempotent writes against the current tree for convergence. That does not preserve
empty commits, save boundaries, or the original request SHA.
Replaying several already-published intermediate states can also create redundant history even
when the final tree converges.

Use stable batch, attempt, and request identities. Before pushing, persist the intended commit
mapping and enough recoverable evidence to recognize that attempt. Candidate evidence includes
retained immutable Git artifacts or an agreed operation marker in Git. A marker changes the
repository contract and needs its own design; choosing it is not implicit in this proposal.
Local artifacts identify what was attempted. Publication evidence must come from the remote or
a durable record of its acknowledgment.

On recovery, seek evidence that the intended commits were published, allowing later commits above
them. If a competing update forces a rebuild, record a new attempt and its refreshed mapping.
Only publish the request outcome once evidence supports that outcome. Equal tree content alone
cannot prove that a particular message or empty save was recorded.

Shallow history, branch deletion, force pushes, and unreachable objects can remove the evidence.
Define how long evidence is retained and what happens when the outcome remains indeterminate.
Do not promise exactly-once publication until the protocol handles those cases within a stated
failure model. The existing remote compare-and-swap protects ref updates; it does not atomically
acknowledge the journal or prove a request's historical execution.

## Surviving hours without Git

Use the same pause/recover/resume behavior as the in-memory worker, with durable admission as the
acceptance boundary. Storage changes how long accepted obligations survive and how much backlog
fits; it does not change which outcomes the worker owes.

| Boundary | During an outage | At capacity | After recovery |
|---|---|---|---|
| Current in-memory path | Retain accepted writes while the process lives | Refuse new admission; explicit producer pause is planned | Replay retained decisions, then resume watches |
| Future durable journal | Persist accepted inputs and workflow decisions; page the active working set | Stop durable admission before eviction or storage failure | Recover accepted obligations, then catch up sources |

If Git is unavailable but the durable store has capacity, producers can keep appending without
loading that backlog into the worker's RAM. When the store fills or cannot guarantee a durable
write, stop admission and keep the source cursor unchanged. Report whether Git publication or
journal admission is blocked; they are different dependencies and can recover independently.
Never acknowledge and trim accepted work merely to free capacity for newer observations.

State an outage budget in workload and storage terms. A useful first estimate is admitted bytes
per second multiplied by outage duration, plus snapshot, index, encryption, and retention costs.
Recovery also needs enough publication throughput to drain the backlog while new work arrives.

Use the atomic admission and cursor handoff defined above. Enforce byte quotas and a retention
policy from the first durable implementation. Stop accepting
new payloads when the durable store cannot honor its contract. This does not guarantee recovery
of every later Kubernetes mutation: a prolonged ingestion stop can outlast watch history.

Retained accepted work drains in branch order before a fresh source snapshot can supersede current
state. An expired watch cursor records a continuity gap; the complete scoped snapshot repairs only
what the current write/prune policy permits. Missed intermediate versions and expired delete
events are not recreated. An accepted save keeps its original obligation through this catch-up;
an unaccepted save has no durable receipt. State these limits in status and operator documentation.

Give recovery and lifecycle work a way to run under saturation, with its causal ordering intact.
Reserved capacity must not let a withdrawal overtake an already accepted attach. Bound the active
in-memory working set independently of journal size; keeping every persisted payload in
`pendingWrites` would preserve the current memory problem.

Snapshot compaction must respect save boundaries. Replacing a request-bearing batch with the
latest object state loses its message, attribution, and publication obligation. Define explicitly
which ordinary writes can coalesce, and preserve references needed by retained windows.

The store's persistence, replication, retention, and eviction settings are part of the guarantee.
Naming Redis Streams as the delivery mechanism does not establish the storage failure model.
Sensitive resource payloads must be encrypted before persistence; queue key rotation must retain
access to accepted work for its recovery lifetime.

Report durable backlog bytes, oldest pending work, outstanding saves, current retry deadline,
publication uncertainty, and status-projection lag. These proposed measurements distinguish a
healthy outage backlog from a worker that has stopped making progress.

## Implementation phases

The branch-worker log plan owns admission correctness, operation deadlines, and in-memory recovery
visibility. The event model owns extraction of deterministic worker transitions. These phases own
persistence and HA after that boundary is available. Do not start them as part of finishing the
current log refactor. Each phase needs its own failure tests; adding leadership must not be the
first recovery test of the journal.

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

- publish idempotent records with explicit admission receipts;
- atomically couple watch cursor advancement to durable admission;
- persist worker decisions and effect obligations with their processing position;
- route live observations, snapshots, saves, withdrawals, deadlines, and results through the
  recoverable boundary;
- encode and encrypt sensitive payloads, enforce quotas, and page retained payloads to bound memory;
- create consumer groups, reclaim abandoned work, and preserve terminal receipts and pending
  status projections when acknowledging completed delivery.

Refactor EventRouter and the worker adapters to use this boundary. The open window can remain
an in-memory working view only if recorded decisions or a checkpoint reconstruct its membership,
save attachment, and deadlines. An unacknowledged list of raw resource events is insufficient.
Local commits remain disposable except for artifacts explicitly retained by the publication
recovery protocol.

Persist schema and decision-behavior versions from the first journal implementation. Test
checkpoint positions, compaction, and compatible readers against pending effects and receipts.
Make target replacement and worker retirement explicit transfer or terminal decisions.

Exit criterion: a replacement process with an empty checkout restores pending windows and saves,
overdue deadlines, and unprojected outcomes. Lost acknowledgments find the original records;
a crash after recording a deadline or effect still allows it to run. Ship and exercise this
phase with one active Pod before adding leader failover.

### HA-1a: Prove publication recovery and outage capacity

Implement the chosen evidence protocol before claiming preserved save outcomes across restart.
Inject crashes before pushing, after Git accepts the push, after recording the result, and
before status projection. Exercise both changed trees and empty commits. Reconstructing a
historical result must cause no external action.

Size an outage workload to the stated duration and admission rate. Restart during it, fill the
quota, recover Git, and keep new traffic arriving while the backlog drains. Measure memory,
journal growth, remote attempts, and time to settle saves. Sleeping for hours at low volume
would establish little about capacity.

Exit criterion: the workload fits its storage and memory budgets and preserves the agreed save
contract. Record an explicit outcome for cases whose publication remains indeterminate within
the supported failure model. Keep that limitation visible before enabling HA.

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
- Enforce journal ownership epochs and reject stale writers at the durable transition boundary.
- Project recorded `CommitRequest` outcomes into Kubernetes status by UID. Recover the terminal
  receipt and any pending projection after failover, without deciding the save outcome again.

Exit criterion: failover and supported rolling upgrades preserve the single-active contract,
including already-started pushes, overdue timers, and unprojected outcomes. Readers must remain
compatible with retained schema and behavior versions.

### HA-3: Release the supported Helm mode

Only after HA-1, HA-1a, and HA-2 pass end-to-end fault tests:

- remove the replica-count rejection;
- reject HA configuration without a Redis endpoint, queue-encryption key, and
  TLS unless an explicitly documented trusted development exception is chosen;
- add leader-election and durable-queue values to the chart schema;
- add Lease RBAC and regenerate the chart RBAC artifact;
- use at least two replicas, RollingUpdate with maxUnavailable zero and
  maxSurge one, and a PDB with minAvailable one;
- make hostname anti-affinity and topology spread required for the HA profile;
  offer a zone-spread profile where clusters have multiple zones;
- document a supported Redis or Valkey topology that survives the advertised storage failures.

The existing PDB can remain for single-Pod installs, but it must not be described
as HA by itself.

### HA-4: Fault-injection acceptance suite

Add fault tests for each durable boundary before enabling the supported HA mode:

| Scenario | Required observation |
|---|---|
| Crash before durable admission | No durable cursor skips an absent record |
| Crash after admission or a lost admission acknowledgment | Same accepted record returns in branch order |
| Crash after local commit, before push | Unpublished work and its save remain recoverable |
| Crash with an attached save | Same author decision, window membership, and deadlines return |
| Restart with overdue deadlines and queued writes | The defined processing-time ordering is preserved |
| Crash after push, before recording success | Publication is recovered or explicitly indeterminate |
| Status API fails after publication | Same outcome is projected later without another commit |
| Empty save is redelivered | Original result is recovered; missing evidence remains explicit |
| Snapshot lacks a complete marker | No sweep runs |
| Watch cursor expires | Fresh replay preserves the old journal tail and scoped deletion rules |
| Journal quota fills during an outage | Admission stops explicitly; accepted records remain; recovery retains capacity |
| Store is full when watch history expires | Gap is explicit; fresh snapshot obeys prune policy and preserves accepted saves |
| Git is down while the journal still has capacity | Durable backlog grows while the active worker memory stays bounded |
| Target or provider is recreated | Old work cannot bind to the replacement by name |
| Old owner completes an in-flight push | Replacement recovers the outcome and rejects stale journal transitions |
| Remote branch moves | Compare-and-swap and recorded rebuild attempts preserve the pending work |
| Upgrade or compaction leaves pending work | State, payloads, receipts, and compatible readers remain available |
| Sensitive-resource backlog crosses key rotation | Accepted work remains decryptable without plaintext storage |

Assert final Git state and deletion handling where the scenario establishes publication, and
explicit uncertainty where evidence is missing. No case may invent a success or silently discard
an accepted save. Exercise the actual storage persistence failure model, including lost
acknowledgments, as well as process interruption. Run the normal repository validation sequence
after implementation: `task fmt`, `task generate` where needed, `task vet`, `task lint`, `task test`,
and `task test-e2e`.

## Optional follow-on: active/active branch shards

Do not make this a prerequisite for HA v1. It improves throughput and isolation,
but creates a second distributed-systems problem.

When needed, assign each BranchWriteShard its own Kubernetes Lease. Any Pod may
publish durable records, but only the shard-Lease holder consumes that shard and
pushes its Git branch. The lease coordinates ownership; remote compare-and-swap remains mandatory
with the publication-recovery limits described above.

Tracking and watching can remain leader-owned initially. Distributing target
watches or GVR scopes should happen only after the snapshot markers, replay
watermarks, deduplication, and completeness state are proven durable. A target
with several watched scopes must never sweep Git state until every required
scope has completed its authoritative replay.

## Acceptance criteria for supported HA

- A two-Pod controller deployment survives loss of the active Pod without
  silently skipping Kubernetes state.
- No durable cursor can advance past work that is absent from the journal.
- No completed delivery is acknowledged without recoverable publication evidence or an explicit
  terminal outcome; pending status projection and required deduplication receipts remain durable.
- A stale or partitioned owner cannot overwrite a newer Git ref.
- Replays and queue claims preserve accepted save obligations. Post-push-before-ack crashes use
  the publication protocol; missing evidence produces explicit uncertainty rather than an
  unsupported success or duplicate-publication guarantee.
- Sensitive resource content is never stored as plaintext in the durable queue.
- The chart prevents unsupported HA configurations and documents dependencies
  that must themselves be highly available.
- README and chart documentation remove the single-Pod limitation only after
  the fault-injection suite passes.
