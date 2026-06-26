# GitOps Reverser Architecture

> Last updated: June 2026

> **Superseded by watch-first ingestion.** The audit-as-correctness pipeline described in several
> sections below — the per-type Redis audit streams, the demand gate, the checkpoint + log splice, the
> materialization phase machine, the audit body joiner, and the `/audit-webhook-additional` endpoint —
> has been removed. Object state now comes from Kubernetes **watch** (`sendInitialEvents` replay plus a
> mark-and-sweep on re-establishment); audit is an **optional attribution lookup** that only names the
> commit author, and with no Redis the product runs committer-only. The authoritative model is
> [design/watch-first-ingestion-architecture.md](design/watch-first-ingestion-architecture.md). Audit
> sections here are being reconciled to that model; where this document still describes the retired
> pipeline, the design record wins.

GitOps Reverser is a Kubernetes operator that observes cluster mutations and writes the resulting
desired object state to Git. It reverses the traditional GitOps direction: instead of Git driving the
cluster, the Kubernetes API drives Git. The repository becomes a continuously updated mirror of live
cluster state.

This document is the starting point for new contributors. Read the [Ground Rules](#ground-rules) and
[Mental Model](#mental-model) for the shape of the system, then the
[Configuration Model](#configuration-model) and [A Change, End to End](#a-change-end-to-end) for how
you drive it. The later sections give the reference detail behind each piece. It describes the
current code. If a detail here ever disagrees with the source, the source wins. The design records
under [docs/design/](design/) and [docs/finished/](finished/) carry the full reasoning.

***

## Ground Rules

These are the design decisions to keep in your head while reading or changing the code:

**The Kubernetes API is the source of truth.** Git is a materialized mirror of desired state from the
API. State is ingested by **watch** (`sendInitialEvents` replay plus a mark-and-sweep on watch
re-establishment); these paths never treat Git as authority. When a push conflicts with a newer remote
commit, the operator fetches the new remote state. It resets its local clone, then replays retained
writes from the API.

**Writes are serialized per Git branch.** One [BranchWorker](../internal/git/branch_worker.go) owns
each `(GitProvider namespace, GitProvider name, branch)` tuple. Multiple `GitTarget`s may share one
branch. Every write to that branch goes through the worker's single event loop and commit window.

**Watch is the authoritative object-state source.** Each `GitTarget` opens one watch per claimed
`(GVR, scope)`; every Git write derives from persisted state the watch observed. Audit never defines
*what* changed — it only, optionally, explains *who* caused it.

**Audit is an optional attribution lookup.** When Redis is configured, kube-apiserver posts audit
events to `/audit-webhook`; the operator extracts a minimal attribution fact (auditID, user, verb,
resourceVersion, GVR/namespace/name/UID, status, timestamps) into a Redis attribution index keyed for
a join, and a resolver attaches the commit author to a watch event by matching a fact within a bounded
grace window. A missing, late, or absent fact never blocks state capture; it only changes the author.

**Redis/Valkey is optional.** When present it stores two small things: the `(GVR, scope) → RV` watch
resume cursors and the attribution facts — neither is a correctness input. With no Redis the product
runs **committer-only**: it mirrors cluster state to Git authored by the configured committer, and
needs neither Redis nor the audit webhook.

***

## Mental Model

The easy part is "write YAML to Git." The hard parts shape the whole architecture:

* **Ordering.** Two updates to the same object must land in Git in cluster order.
* **Not losing changes.** A dropped or late event must never leave Git *permanently* wrong.
* **Secrets.** Sensitive objects must never touch disk in plaintext.
* **Scale.** A cluster holds thousands of objects across hundreds of types; you cannot keep a live
  informer open on all of them.

The solution, in the vocabulary used throughout this document:

* **Watch is the only object-state source.** Each `GitTarget` opens one Kubernetes **watch** per
  claimed `(GVR, scope)` with `sendInitialEvents=true`. The apiserver delivers events already ordered
  by `resourceVersion` per type, so there is nothing to re-order. Every Git write derives from
  persisted state the watch observed.
* **Watches are per `GitTarget` and scaled by claims.** A watch opens only for the
  claimed ∩ followable `(GVR, scope)` set, so cost scales with what `GitTarget`s actually claim, not
  with cluster type count.
* **Resume and recovery have two modes.** A `(GVR, scope) → resourceVersion` resume cursor in Redis (when
  present) lets a restart resume from the exact point and stream the deltas — including missed
  `DELETED`s. When the exact RV is gone (compaction → `410 Gone`, cold start, or no Redis), the watch
  re-opens with `sendInitialEvents=true` and a **mark-and-sweep** over the replay reconciles any object
  deleted while no watch was running. The sweep fires on watch re-establishment, **never on a timer**.
* **Audit, when configured, only names the author.** It is an optional attribution lookup; a missing or
  late fact costs author fidelity, never correctness, and with no Redis the product commits as the
  configured committer.
* **One `BranchWorker` per Git branch serializes all writes.** Every write to a branch funnels through
  a single worker and a single commit window, which is what keeps concurrent GitTargets and authors
  from racing each other into a corrupt tree.

***

## Configuration Model

You configure GitOps Reverser entirely through five CRDs (group `configbutler.ai`, version
`v1alpha2`). They describe a configurable pipeline. `WatchRule` and `ClusterWatchRule` choose which
Kubernetes resources enter the pipeline. `CommitRequest` can ask for the current window to be saved.
`GitTarget` chooses the branch and path. `GitProvider` supplies the repository, credentials, commit
settings, and push policy.

```mermaid
graph LR
    WR[WatchRule] -->|targetRef| GT[GitTarget]
    CWR[ClusterWatchRule] -->|targetRef| GT
    CR[CommitRequest] -->|targetRef| GT
    GT -->|providerRef| GP[GitProvider]

    style GP fill:#e8f4fd,stroke:#2196f3
    style GT fill:#e8f4fd,stroke:#2196f3
    style WR fill:#fff3e0,stroke:#ff9800
    style CWR fill:#fff3e0,stroke:#ff9800
    style CR fill:#f3e5f5,stroke:#8e24aa
```

| CRD | Scope | One line role |
|---|---|---|
| `WatchRule` | namespaced | which resources in *this* namespace route to a GitTarget |
| `ClusterWatchRule` | cluster | which cluster scoped or cluster wide resources route to a GitTarget |
| `CommitRequest` | namespaced | a one shot "save the open window now" signal |
| `GitTarget` | namespaced | one materialization destination `(provider, branch, path)` |
| `GitProvider` | namespaced | a Git repo + credentials + commit/signing config |

### WatchRule / ClusterWatchRule

* **Sources**: [watchrule_types.go](../api/v1alpha2/watchrule_types.go),
  [clusterwatchrule_types.go](../api/v1alpha2/clusterwatchrule_types.go)
* **Controllers**: [watchrule_controller.go](../internal/controller/watchrule_controller.go),
  [clusterwatchrule_controller.go](../internal/controller/clusterwatchrule_controller.go)

A `WatchRule` selects resources in its own namespace and routes matching events to a same namespace
`GitTarget`. A `ClusterWatchRule` does the same for cluster scoped resources or namespaced resources
across the whole cluster, with an explicit namespace `targetRef`. Both share the rule model:

* `spec.rules[]`: OR resource rules (`MinItems=1`).
* `rules[].operations`: `CREATE` / `UPDATE` / `DELETE` / `*`; omitted means all.
* `rules[].apiGroups`: omitted resolves the named resource across served groups; `""` is the core
  group; `*` is all.
* `rules[].apiVersions`: omitted means the preferred served version.
* `rules[].resources`: plural resource names or `*`.
* `ClusterWatchRule` adds `rules[].scope`: `Cluster` or `Namespaced` (each rule independently scoped).

Subresources are rejected in rule resources. Mirroring operates on top level resources; selected
subresource effects (`/scale`) are translated separately.

### CommitRequest

* **Source**: [api/v1alpha2/commitrequest_types.go](../api/v1alpha2/commitrequest_types.go)
* **Controller**: [internal/controller/commitrequest_controller.go](../internal/controller/commitrequest_controller.go)

A one shot "save now" signal that finalizes the open commit window for a same namespace `GitTarget`
instead of waiting for the silence timer. The **entire spec is immutable**. Key fields:

* `spec.targetRef.name`: target whose open window should be finalized.
* `spec.message`: optional verbatim commit message (1–1024 chars, no control characters).
* `spec.delaySeconds`: optional `0–300s` grace so the author's own in flight changes can join the
  window before it closes.
* `status.phase`: `WaitingForAuditEvent` (initial) → terminal `Committed`, `Rejected`, or `Failed`.
* `status.reason` (when `Rejected`): `NoWindowInGrace`, `WindowMismatch`, or `AlreadyPresent`.
  `Rejected` is a correct, non error outcome.
* `status.branch` / `status.sha`: set when `Committed`.

Why the finalize waits for the request's *own* audit event is explained under
[CommitRequest Finalize](#commitrequest-finalize).

### GitTarget

* **Source**: [api/v1alpha2/gittarget_types.go](../api/v1alpha2/gittarget_types.go)
* **Controller**: [internal/controller/gittarget_controller.go](../internal/controller/gittarget_controller.go)

One materialization destination: `(provider, branch, path)`. Key fields:

* `spec.providerRef`: a `GitProvider` in the same namespace (`group`/`kind` default to
  `configbutler.ai`/`GitProvider`, the only accepted values).
* `spec.branch`: immutable branch, validated against `GitProvider.spec.allowedBranches`.
* `spec.path`: immutable, required path under the repo (`MinLength=1`; `.` means repo root and must be
  chosen explicitly).
* `spec.encryption`: optional SOPS/age encryption settings for sensitive resources.

`providerRef`, `branch`, and `path` are immutable so a target cannot silently orphan an old
materialization. The controller also rejects path overlaps between GitTargets sharing a provider and
branch.

Status splits into two **orthogonal axes** (see
[design/status-design-git-target.md](design/status-design-git-target.md)):

* **Control plane. Is it configured and wired?** `Validated` (provider exists, branch allowed, path
  valid with no overlap), `EncryptionConfigured` (required key material present), and `Ready` (the
  aggregate of those plus a wired branch worker). `Ready` does **not** depend on data plane sync.
* **Data plane. Is desired state actually materializing?** `Synced`, a pure projection of the
  materialization summary, with reasons `OK` / `Initializing` (first checkpoint building) /
  `NotFollowable` (a claimed type is refused) / `SyncFailing` (checkpoint sync failing with no prior
  checkpoint). `status.phase` (`Pending`/`Initializing`/`Synced`/`Degraded`) is an informational
  projection only. Automation gates on conditions, never phase. `status.materialization` is a bounded
  **counts** summary (`claimedTypes`, `syncedTypes`, `pendingTypes`, `failingTypes`,
  `notFollowableTypes`, `observedTime`), not a list by type.

### GitProvider

* **Source**: [api/v1alpha2/gitprovider_types.go](../api/v1alpha2/gitprovider_types.go)
* **Controller**: [internal/controller/gitprovider_controller.go](../internal/controller/gitprovider_controller.go)

Represents a Git repository and the credentials/configuration used to write it. Key fields:

* `spec.url`: immutable repository URL.
* `spec.secretRef`: optional Secret in the same namespace for HTTP/SSH authentication.
* `spec.knownHostsRef`: optional SSH known hosts source.
* `spec.allowedBranches`: glob patterns that gate writable branches.
* `spec.push.commitWindow`: rolling silence window for grouped commits, defaulting to `5s`.
* `spec.commit.committer`: committer identity (defaults to `GitOps Reverser` / `noreply@configbutler.ai`).
* `spec.commit.message`: `eventTemplate` / `reconcileTemplate` / `groupTemplate` Go templates.
* `spec.commit.signing`: SSH signing key reference and optional key generation.
* `status.signingPublicKey`: populated when signing is configured and key material is available.

The controller verifies repository reachability and manages the signing key lifecycle. It generates
an ed25519 keypair when `signing.generateWhenMissing` is set. The portable artifact across GitOps
ecosystems is the credentials Secret, not a foreign repository object. The credentials reader accepts
the Kubernetes native, Flux, and Argo CD Secret key dialects. See
[design/git-credentials-interop.md](design/git-credentials-interop.md). There is no Flux
`GitRepository` provider option.

***

## A Change, End to End

Say a `GitTarget` in namespace `team-a` watches ConfigMaps, and a user runs
`kubectl apply` to edit the ConfigMap `team-a/app-config`. Here is the path that change takes.

```mermaid
flowchart TD
    subgraph K8S["Kubernetes API server"]
        WATCH["WATCH + sendInitialEvents replay<br/>(per claimed GVR + scope)"]
        DISC["Discovery: CRDs / APIServices"]
        AUDIT["/audit-webhook (optional)"]
    end

    subgraph PERGT["Per GitTarget: internal/watch + internal/reconcile"]
        OWN["GitTarget watch set<br/>(claimed ∩ followable (GVR, scope))"]
        RELEVANCE["Relevance filter<br/>(sanitize · followability · no-op diff)"]
        RESOLVE["Resolver<br/>(bounded grace window)"]
        GTES[GitTargetEventStream]
    end

    subgraph ATTR["Attribution (only when Redis present): internal/webhook"]
        AFACTS["Audit fact extractor"]
        AINDEX[("Redis attribution index<br/>(TTL, keyed for join)")]
    end

    subgraph GIT["Git writes: internal/git"]
        BW[BranchWorker]
        PLAN["Manifest aware plan/flush"]
        PUSH["Atomic push"]
    end

    DISC --> OWN
    WATCH --> OWN
    OWN --> RELEVANCE
    RELEVANCE --> RESOLVE
    RESOLVE --> GTES
    GTES --> BW
    BW --> PLAN
    PLAN --> PUSH

    AUDIT -. configured .-> AFACTS
    AFACTS --> AINDEX
    AINDEX -. lookup .-> RESOLVE
```

Following the ConfigMap edit:

1. **Watch delivers it.** The API server applies the change and bumps the object's `resourceVersion`.
   The `GitTarget`'s watch on `core/configmaps` in `team-a` delivers a `MODIFIED` event carrying the
   new object body. (On a cold start or after `410 Gone`, the same object instead arrives as an `ADDED`
   during the `sendInitialEvents` replay.)
2. **Relevance filter.** The event is sanitized (status, managedFields, and volatile metadata
   stripped), checked for followability, and diffed against current Git content. A no-op (e.g. a
   `*/status` bump whose desired-state projection is unchanged) is dropped here.
3. **Resolve the author.** When Redis is present, the resolver waits a bounded grace window
   (`--attribution-grace`, default `3s`) for a matching audit fact in the attribution index, joining by
   resourceVersion/UID. On a strong match the real user or named service account becomes the author;
   otherwise (no Redis, no match, or expiry) the commit is authored by the configured committer.
4. **Route + window.** The event flows into the
   [GitTargetEventStream](../internal/reconcile/git_target_event_stream.go), and the
   [BranchWorker](../internal/git/branch_worker.go) appends it to the open commit window for
   `(author, GitTarget)`.
5. **Commit + push.** When the window closes (5s of silence, a `CommitRequest`, or a buffer limit),
   the manifest aware writer patches `team-a/.../app-config.yaml` in place, commits as the resolved
   author, and pushes via [PushAtomic](../internal/git/git_atomic_push.go) (retrying with fetch/reset/
   replay if the remote moved).

Separately, the audit path (only when Redis is configured): kube-apiserver POSTs audit events to
`/audit-webhook`; [AuditHandler](../internal/webhook/audit_handler.go) extracts a minimal attribution
fact and writes it to the Redis attribution index with a short TTL. That index is read only by the
resolver in step 3; it never creates or repairs object state.

**And if the watch had been lost?** A delete that happened while no watch was running is reconciled on
the next watch (re)connect: a Mode-A resume from the exact RV cursor streams the missed `DELETED`s, and
when the RV is gone a Mode-B `sendInitialEvents` replay plus **mark-and-sweep** reconciles them — see
[Resume and Recovery](#resume-and-recovery). No path silently drops a delete.

***

## What It Writes to Git

A `GitTarget` owns one subtree (`spec.path`) on one branch. New objects are placed at a canonical
REST like path; once a document exists it is edited in place wherever it already lives. A populated
target looks like:

```text
team-a-config/                              # GitTarget spec.path
├── README.md                               # operator-managed bootstrap file
├── .sops.yaml                              # present only when encryption is configured
├── v1/
│   ├── configmaps/team-a/app-config.yaml
│   └── secrets/team-a/db-creds.sops.yaml   # sensitive types are SOPS/age encrypted
└── apps/
    └── v1/deployments/team-a/api.yaml
```

The path shape is `{spec.path}/{group}/{version}/{resource}/{namespace}/{name}.yaml` (the empty core
group is omitted, sensitive resources get a `.sops.yaml` suffix). Details and the placement policy are
in [Git Write Architecture](#git-write-architecture).

***

## State Ingestion and Not Losing Deletes

This is the heart of the system. Object state is ingested by **watch**, and the guarantee is: every
persisted mutation observed while watching reaches Git, and no delete is ever silently dropped across a
gap.

### Watch is already ordered by resource version

The apiserver delivers a watch's events already ordered by `resourceVersion` per type, so there is
**nothing to re-order**: a `MODIFIED` always lands after the create it modifies. Each event carries
GVR, scope, event type (`ADDED` / `MODIFIED` / `DELETED`, plus the transport `BOOKMARK` and `ERROR`),
namespace/name/UID/resourceVersion/generation/deletionTimestamp, and the sanitized object body. The
`initial-events-end` bookmark marks the end of a replay, bookmark RVs advance the resume cursor, and an
`ERROR` such as `410 Gone` triggers a fresh `sendInitialEvents` reconnect.

### Resume and Recovery

Losing a watch (pod eviction, rollout, crash, `410 Gone`) is normal; there are exactly two resume modes.

**Mode A — resume from the exact point (preferred).** A `(GVR, scope) → last resourceVersion` cursor is
persisted to Redis as events arrive and kept fresh with bookmarks even when the type is quiet. On
restart the watch re-opens *from that RV* — a plain delta watch, no replay. If the apiserver still holds
that RV, it streams every event since, **including the `DELETED`s missed while away**, and the operator
simply continues.

**Mode B — replay with mark-and-sweep (the correctness fallback).** If the exact RV is gone (compacted →
`410 Gone`, no cursor, no Redis, or a cold start), a delta is untrustworthy — objects may have been
deleted while away. So the watch opens with `sendInitialEvents=true` and runs a **mark-and-sweep**:
every replayed object is marked `ADDED` up to `initial-events-end`; at the bookmark, any Git file whose
object was *not* marked no longer exists and a `DELETED` is emitted for it (committer-authored); then it
streams live. **This mark-and-sweep is load-bearing and fires only on watch re-establishment, never on a
timer** — there is no periodic LIST or hourly drift sweep.

### Relevance filtering is product code

Watch has no audit policy, so it delivers every persisted `MODIFIED`, including status churn the old
cluster audit policy dropped at the source. The relevance filter reproduces that filter in product code,
on the hot path:

* **Sanitization** ([internal/sanitize](../internal/sanitize/)) strips status, managedFields, and
  volatile metadata before diffing, so runtime churn never masquerades as a desired-state change.
* **Followability** ([internal/typeset](../internal/typeset/)) encodes "controller-owned → don't
  mirror."
* **No-op suppression.** A `*/status` write bumps `resourceVersion` but its sanitized projection equals
  the prior commit; the writer diffs it to a no-op and discards it.

### History granularity

Watch carries only the versions it observes. While connected it sees each `MODIFIED`; across a replay
after `410`, a compaction, or downtime it **collapses every intermediate version into current state**.
Watch-first is therefore a *state mirror with opportunistic per-mutation history*, not a guaranteed
per-mutation change log.

### Observability

Watch volume, drops, restarts, and replay cost are exported by GVR/scope (see the
[watch-first design metrics](design/watch-first-ingestion-architecture.md#metrics)), so a mis-tuned
relevance filter is visible rather than silently dropping intent.

***

## Optional Attribution

* **Handler / fact extractor**: [internal/webhook/audit_handler.go](../internal/webhook/audit_handler.go)
* **Attribution index**: [internal/queue/attribution_index.go](../internal/queue/attribution_index.go)
* **Resolver (grace window join)**: [internal/watch/author_resolver.go](../internal/watch/author_resolver.go)

Attribution runs **only when Redis is configured**. The Kubernetes API server POSTs audit `EventList`
payloads to a **single** HTTP endpoint, `/audit-webhook`; there is no supplementary body endpoint and no
body joiner, because watch — not audit — carries the object body. The handler extracts a minimal
attribution fact and writes it to a Redis attribution index with a short TTL.

| Endpoint | Role |
|---|---|
| `/audit-webhook` | Audit source (kube-apiserver) for the optional attribution index |

Cluster ID path segments are rejected; multi cluster routing is not modeled yet.

### Attribution fact shape

The fact is the smallest thing needed to name an author, not an object log:

| Field | Purpose |
|---|---|
| `auditID` | diagnostics / dedupe |
| `user` / `impersonatedUser` | author candidate (human *or* service account) |
| `verb`, `subresource` | explain the write |
| `responseStatus.code`, `dryRun` | reject failures and non-persistent requests |
| GVR, namespace, name, UID | exact join keys |
| response object resourceVersion | exact watch-event match |
| request/stage timestamps | bounded time matching |

The index is keyed for the join (strongest first: exact `(GVR, ns, name, uid, responseRV)`, then UID,
then RV, then a weak time bucket). Retention is bounded by max audit delay plus the attribution grace
period — minutes, not hours — because watch owns state and old facts are never needed for correctness.

### The resolver and its grace window

A watch event waits a **bounded grace window** (`--attribution-grace`, default `3s`) for a matching fact
to arrive, then ships regardless. On a strong match the real user or named service account becomes the
author; the service-account naming policy (`--attribution-sa-naming`: `name` | `bot`) controls how a
matched service account is named. A weak, conflicting, failed, dry-run, or absent fact resolves to the
committer. A late fact that arrives after a commit has shipped **never rewrites it**. With no Redis the
resolver is skipped entirely and every commit is committer-authored.

***

## Rule and Type Resolution

How a user's `WatchRule` becomes "this GitTarget follows these concrete types in these namespaces."

### RuleStore

* **Source**: [internal/rulestore/store.go](../internal/rulestore/store.go)

An in memory cache populated by the WatchRule and ClusterWatchRule controllers. Compiled rules carry
the full chain from rule to `GitTarget`, `GitProvider`, branch, and path. It is read by the watch
manager (to build watched type tables for GitTargets) and the rule change reconcile path.

### APIResourceCatalog

* **Source**: [internal/watch/api_resource_catalog.go](../internal/watch/api_resource_catalog.go)

A thin normalizer for each scan: it turns one discovery result into a policy annotated `typeset.Scan`
and keeps only mechanical bookkeeping. **All judgement across scans lives in the typeset registry**
(`Registry.UpdateFromScan`): a failed group/version keeps serving last known facts instead of looking
like an empty API surface, and a group/version that vanishes from a complete scan rides a removal
grace rather than being pruned. Both protect against accidental Git deletions on a discovery blink
(see [typeset-owns-discovery-grace.md](design/typeset-owns-discovery-grace.md)). The catalog refreshes
on startup, periodically, and when CRD/APIService trigger informers fire.

### TypeRegistry and Followability

* **Source**: [internal/typeset/](../internal/typeset/)
* **Design**: [design/manifest/version2/type-followability.md](design/manifest/version2/type-followability.md)

`internal/typeset` is the single decision surface for "can this type be followed?" Each `TypeRecord`
carries GVK/GVR identity, scope and preferred version facts, origin classification, subresource facts
(including usable `/scale` bindings), sensitivity policy, and one `Followability` verdict. Checkpoint
planning, manifest analysis, and delete/scale resolution with only GVR all read it. The registry also owns
the second, demand axis via the **Materializer**: a type is materialized only when it is **Followable ∩
claimed**.

### WatchedTypeTable

* **Source**: [internal/watch/watched_type_table.go](../internal/watch/watched_type_table.go)

A projection for each `GitTarget` from the type registry, filtered by that target's rules, recording resolved
GVK/GVR/scope plus namespace and operation coverage. **This is where rule matching effectively
happens:** it resolves the set of `(GVR, scope)` a `GitTarget` claims, so the watch manager opens one
watch per claimed ∩ followable `(GVR, scope)` and scopes each watch's events back to that GitTarget's
namespaces. It feeds two consumers:

* the set of `(GVR, scope)` the GitTarget claims, which the watch manager opens watches for;
* the namespace scope per type, which of a type's objects this GitTarget mirrors.

***

## Watch Ingestion and Reconcile

Desired state comes from one **raw watch per `(GitTarget, GVR, scope)`**. Each event is sanitized,
diffed against current Git content, and applied — there is no separate per-type object store to
reconstruct, because Git already holds current state. The authoritative model is
[design/watch-first-ingestion-architecture.md](design/watch-first-ingestion-architecture.md).

* **Manager**: [internal/watch/manager.go](../internal/watch/manager.go)
* **Watch / replay / resume**: [internal/watch/target_watch.go](../internal/watch/target_watch.go)
* **Author resolver (attribution join + grace window)**: [internal/watch/author_resolver.go](../internal/watch/author_resolver.go)
* **Event router**: [internal/watch/event_router.go](../internal/watch/event_router.go)
* **Worker resync apply (mark-and-sweep)**: [internal/git/resync_flush.go](../internal/git/resync_flush.go)

### The Watch Manager

The watch manager is a controller runtime `Runnable` (`NeedLeaderElection`). It owns **type level**
discovery, the per-GitTarget watch sets, the `(GVR, scope) → RV` resume cursors, and the watched type
tables for GitTargets. Its object-state intake is the watches themselves; its discovery
watches/informers track the API surface (CRDs / APIServices) rather than driving object state.

On `Start` it bootstraps the RuleStore from existing rules, refreshes the API catalog, updates the
TypeRegistry, builds watched type tables, and opens one watch per claimed ∩ followable `(GVR, scope)`,
resuming from the Redis RV cursor when present or replaying from current state when not.

### Opening and resuming watches

On each GitTarget reconcile the controller resolves the GitTarget's claimed ∩ followable `(GVR, scope)`
set. Fully specified GVRs are claimed unconditionally; wildcard rules and rules without a version are
resolved fail closed against discovery. For each `(GVR, scope)` the manager opens one watch:

* **Mode A** resumes from the persisted `(GVR, scope) → resourceVersion` cursor (a delta watch that
  carries any missed `DELETED`s);
* **Mode B** opens with `sendInitialEvents=true` and runs a mark-and-sweep when the exact RV is gone
  (compaction, cold start, or no Redis).

See [Resume and Recovery](#resume-and-recovery). There is **no periodic LIST, no checkpoint, and no
timer-driven drift sweep** — the sweep fires only on watch re-establishment.

### Rule Change Reconcile

A WatchRule / ClusterWatchRule / GitTarget / CRD / APIService change reaches a GitTarget through the
**GitTarget controller**, which `Watches` those objects (generation change predicates) and queues
the affected GitTarget again. On reconcile the GitTarget resolves its claimed `(GVR, scope)` set again;
a type a new rule starts watching gets a new watch opened (with a `sendInitialEvents` replay) and a type
no longer claimed has its watch closed. `Manager.ReconcileForRuleChange` itself refreshes the API
catalog and the watched type tables.

### Mark and Sweep Resync

The BranchWorker applies a reconcile by scanning the GitTarget subtree and building a manifest plan
(this write side is shared with live writes):

* desired resources are upserted through the same content derived path as live writes;
* existing managed documents that are watched but absent from the desired set are deleted;
* untracked, non Kubernetes, unresolved, or unsafe YAML is left alone per analyzer policy;
* nothing is committed if the apply cannot complete safely.

A reconcile can be **type scoped** (`ScopeGVR`): the sweep is restricted to one `(group, resource)`, so
anchoring one type again never disturbs another's manifests. The desired set for the sweep is the
`sendInitialEvents` replay (everything marked `ADDED` up to `initial-events-end`), so a delete is
reconciled only after the replay completes — see [Resume and Recovery](#resume-and-recovery).

***

## Git Write Architecture

### BranchWorker

* **Source**: [internal/git/branch_worker.go](../internal/git/branch_worker.go)
* **Worker manager**: [internal/git/worker_manager.go](../internal/git/worker_manager.go)

`BranchWorker` owns a local clone and a single event loop for its `(provider namespace, provider,
branch)` tuple. Events accumulate in one open commit window, which accepts only one `(author,
GitTarget)` pair at a time:

* same author + same GitTarget: append to the window;
* different author or GitTarget: finalize the current window first;
* repeated writes to the same Git path inside a window use last write wins.

The window finalizes when `spec.push.commitWindow` passes with no new matching event, the retained
buffer reaches `--branch-buffer-max-bytes` (default `8Mi`), a `CommitRequest` finalize deadline matches
the open author and GitTarget, or a resync request that is not a heal or shutdown arrives. Successful local
commits are retained until a fixed push cooldown (`5s`) allows a push, which prevents remote push
storms during bursts. Heal resyncs that arrive during a window are deferred and drained at the next idle
boundary.

### Local Clones and Conflict Retry

Local clones live under `/tmp/gitops-reverser-workers/{namespace}/{provider}/{branch}/repos/{hash}`.
[PushAtomic](../internal/git/git_atomic_push.go) checks the remote ref before pushing. If the remote
diverged it smart fetches the latest tip, hard resets the local clone, replays the retained pending
writes against the fresh tip (refreshing commit hashes), and retries up to the attempt limit. This is
valid because every pending write is rebuilt from sanitized API state. Nothing depends on locally
edited files.

### Manifest Aware Writer

* **Steady state**: [internal/git/plan_flush.go](../internal/git/plan_flush.go)
* **Resync apply**: [internal/git/resync_flush.go](../internal/git/resync_flush.go)
* **YAML editor**: [internal/git/manifestedit/](../internal/git/manifestedit/)
* **Analyzer/planner**: [internal/manifestanalyzer/](../internal/manifestanalyzer/)
* **Object projection**: [internal/manifestreport/](../internal/manifestreport/)

For each commit the writer scans YAML files under the GitTarget path, builds a manifest store without bytes
keyed by resource identity, resolves each event (or each desired resource, for resync) to one action,
hydrates only touched files into buffers for the commit, and flushes only changed or deleted files.

* **Upserts:** if a managed document for the resource already exists, patch it in place (preserving
  siblings in a multi document file); if it is sensitive, encrypt the whole document again at its existing
  path; if no document exists, create a new file at the canonical placement path.
* **Deletes:** use the manifest identity index, so a moved manifest can still be deleted even when it
  is not at the canonical path.
* **Field patches** (currently `/scale` → parent `spec.replicas`) are intentionally narrow: they only
  patch an existing parent manifest and never fabricate a parent object from partial subresource data.

### File Placement

New resources use the canonical REST like path
`{spec.path}/{group}/{version}/{resource}/{namespace}/{name}.yaml`. The core group's empty segment is
omitted (`{spec.path}/v1/configmaps/ns/name.yaml`), and sensitive resources use a `.sops.yaml` suffix.
Existing resources are **match first**: once a document exists in Git, updates and deletes use its
current location instead of recomputing placement. Future placement policy is tracked in
[design/manifest/version2/gittarget-new-file-placement-rules.md](design/manifest/version2/gittarget-new-file-placement-rules.md).

### Bootstrap, Encryption, and Signing

* **Bootstrap** ([bootstrapped_repo_template.go](../internal/git/bootstrapped_repo_template.go)): the
  first write to a GitTarget path stages a `README.md` and, when encryption is configured, a
  `.sops.yaml` with age recipient rules. Existing files are preserved.
* **Encryption** ([encryption.go](../internal/git/encryption.go),
  [sops_encryptor.go](../internal/git/sops_encryptor.go),
  [sensitivity policy](../internal/types/sensitive_resource.go)): core Secrets are sensitive by
  default; `--additional-sensitive-resources` adds more. Sensitive resources are **never written in
  plaintext**. If encryption is required and unavailable, the write fails before any plaintext file is
  created. Encrypted output is cached by metadata + plaintext digest to avoid redundant SOPS work.
* **Signing** ([signing.go](../internal/git/signing.go), [sshsig/](../internal/sshsig/)): commits use
  OpenSSH signatures, with the key read from `GitProvider.spec.commit.signing.secretRef` or generated
  when configured.

***

## CommitRequest Finalize

* **Controller**: [internal/controller/commitrequest_controller.go](../internal/controller/commitrequest_controller.go)
* **Attribution index**: [internal/queue/attribution_index.go](../internal/queue/attribution_index.go)
* **Design**: [design/stream/commitrequest-design.md](design/stream/commitrequest-design.md)

A `CommitRequest` finalizes the open commit window for its GitTarget. With Redis the request is
attributed to its submitter from an audit fact; with no Redis it **finalizes as the committer** with a
status note that finalization happened without end-user attribution — a missing Redis/audit never fails
the request:

1. The controller stamps `WaitingForAuditEvent`. When attribution is available it waits briefly for a
   matching audit fact (bounded, fails *to committer* not closed); with no Redis it proceeds as the
   committer immediately.
2. The controller eagerly **attaches** the request to the worker (`AttachCommitRequest`), anchoring the
   finalize at `receipt + delaySeconds`. The worker binds it to an open window only when author and
   GitTarget match. It **never finalizes another author's window**; a window carries at most one
   request.
3. The window finalizes on the deadline (or when it closes for any other reason). If a finalize closes
   an open window, the worker always schedules a push, so a window closed by an otherwise no op resync
   is not stranded.
4. Outcomes resolve on push: `Committed` carries the pushed `branch`/`sha`; `Rejected` carries a
   structured reason; `Failed` carries a message. There is no watermark barrier in this path.

The CommitRequest submitter is not recoverable from object state alone, so without an audit fact the
finalize is committer-authored.

***

## Controller Wiring

Controllers watch their dependencies so dependents reconcile quickly after spec changes:

* `GitTargetReconciler` watches `GitProvider`, `WatchRule`, `ClusterWatchRule`, and the encryption
  `Secret`; it resolves the GitTarget's claimed `(GVR, scope)` watch set and derives the `Synced`
  condition + materialization summary.
* `WatchRuleReconciler` / `ClusterWatchRuleReconciler` watch `GitTarget` and `GitProvider`, populate
  the RuleStore, and trigger `ReconcileForRuleChange`.
* `GitProviderReconciler` validates reachability and manages the signing key lifecycle.
* `CommitRequestReconciler` runs with `MaxConcurrentReconciles=1` and attributes/attaches as above.

Dependency watches use generation change predicates to avoid queueing again on status only heartbeats.
`GitProvider`, `GitTarget`, and `CommitRequest` carry immutability constraints where a spec change
would orphan a materialized subtree or invalidate an in-flight finalize.

***

## Startup Sequence

Defined in [cmd/main.go](../cmd/main.go):

```mermaid
flowchart TD
    A[Parse flags + logger + build info] --> B[Init telemetry / metrics server]
    B --> C[Create controller runtime manager]
    C --> D[Create RuleStore]
    D --> E[Create WorkerManager + register Runnable]
    E --> F[Create Watch Manager + EventRouter; inject TypeRegistry]
    F --> G[Register WatchRule + ClusterWatchRule controllers]
    G --> H{Redis configured?}
    H -->|yes| I[Create attribution index + audit fact extractor + audit HTTP server]
    H -->|no| J[Committer-only: skip audit webhook + attribution]
    I --> K[Setup + register Watch Manager]
    J --> K
    K --> L[Register GitProvider + GitTarget + CommitRequest controllers]
    L --> M[Add cert watchers + health checks]
    M --> N[mgr.Start]
```

When Redis is configured, the audit HTTP handler is wired with the attribution fact extractor and the
Redis attribution index; the resolver reads that index during the bounded grace window. With no Redis
the audit webhook and attribution are skipped entirely and every commit is committer-authored. `/readyz`
gates on a healthy audit ingress (when enabled) before the pod reports ready.

***

## Operational Boundaries

Current limitations:

* one replica by default; the consumer/watch components declare `NeedLeaderElection` but full HA is
  unfinished (prep in [design/stream/ha-improvements.md](design/stream/ha-improvements.md));
* no pull request creation; the operator writes directly to branches;
* no multi cluster routing or file identity;
* `deletecollection` is reconciled by the watch (each item arrives as its own `DELETED`, or the
  mark-and-sweep reconciles them on replay);
* new file placement is based on the canonical path, not user configurable.

***

## Package Map

| Package | Role |
|---|---|
| [api/v1alpha2/](../api/v1alpha2/) | CRD types |
| [cmd/](../cmd/) | operator entry point and server setup |
| [internal/auditutil/](../internal/auditutil/) | audit identity, objectRef, and subresource helpers feeding attribution facts |
| [internal/controller/](../internal/controller/) | Kubernetes reconcilers |
| [internal/git/](../internal/git/) | branch workers, Git ops, commit/signing/encryption, manifest writer |
| [internal/git/manifestedit/](../internal/git/manifestedit/) | YAML document editor |
| [internal/giteaclient/](../internal/giteaclient/) | Gitea helper client |
| [internal/manifestanalyzer/](../internal/manifestanalyzer/) | manifest inventory, acceptance, and resync planning |
| [internal/manifestreport/](../internal/manifestreport/) | projection of Kubernetes objects into comparable manifest reports |
| [internal/queue/](../internal/queue/) | Redis attribution index (audit facts keyed for the join) and the `(GVR, scope) → RV` resume cursors |
| [internal/reconcile/](../internal/reconcile/) | event stream state for each GitTarget |
| [internal/rulestore/](../internal/rulestore/) | compiled rule cache |
| [internal/sanitize/](../internal/sanitize/) | Kubernetes object sanitization and stable YAML marshal |
| [internal/ssh/](../internal/ssh/) | SSH authentication helpers |
| [internal/sshsig/](../internal/sshsig/) | SSH signature implementation |
| [internal/telemetry/](../internal/telemetry/) | metrics and OTLP setup |
| [internal/types/](../internal/types/) | shared resource identity/reference and sensitivity policy |
| [internal/typeset/](../internal/typeset/) | type followability registry, lookup model, and the relevance filter (controller-owned → don't mirror) |
| [internal/watch/](../internal/watch/) | discovery catalog, watch manager, per-`(GVR, scope)` raw watches with `sendInitialEvents` replay/resume, watched type tables, event router |
| [internal/webhook/](../internal/webhook/) | audit ingress and attribution fact extraction (no body joiner) |

***

## Design Documents

Useful deeper dives live in [docs/design/](design/) and [docs/finished/](finished/).

**Ingestion and reconcile (the current model):**

* [Watch-first ingestion architecture](design/watch-first-ingestion-architecture.md) — the authoritative
  current model: watch is the only object-state source, audit is optional attribution.
* [CommitRequest design](design/stream/commitrequest-design.md)
* [HA improvements (future)](design/stream/ha-improvements.md)

**Historical (the retired audit-as-correctness pipeline), kept for context:**

* [Audit log ingestion and ordering](design/stream/audit-log-ingestion-and-ordering.md)
* [Stream architecture and bootstrap](design/stream/architecture-and-bootstrap.md)
* [API source of truth reconcile](finished/api-source-of-truth-reconcile.md)
* [Demand gated audit ingestion](finished/demand-gated-audit-ingestion.md)
* [Demand driven type materialization lifecycle](finished/demand-driven-type-materialization-lifecycle.md)
* [RV keyed streams experiment for each resource type](finished/per-resource-type-rv-keyed-streams-experiment.md)
* [Reconcile and streaming tail by type](design/manifest/version2/per-type-reconcile-and-streaming-tail.md)
* [Audit diagnostic streams plan](design/stream/audit-diagnostic-streams-plan.md)
* [CommitRequest attribution/divert reliability](finished/commitrequest-attribution-divert-reliability.md)

**Types, discovery, status, and Git write side:**

* [Type followability](design/manifest/version2/type-followability.md)
* [Typeset owns discovery grace](design/typeset-owns-discovery-grace.md)
* [Kubernetes API resource catalog](design/kubernetes-api-resource-catalog.md)
* [GitTarget status design](design/status-design-git-target.md)
* [GitTarget lifecycle and repo architecture](design/gittarget-lifecycle-and-repo-architecture.md)
* [Reconcile via watchlist mark and sweep](design/manifest/reconcile-via-watchlist-mark-and-sweep.md)
* [Git credentials interop](design/git-credentials-interop.md)
* [SOPS/age key management](design/sops-repo-bootstrap-and-key-management-architecture.md)
* [Commit signing](commit-signing.md)
* [Interpreting metrics](interpreting-metrics.md)
