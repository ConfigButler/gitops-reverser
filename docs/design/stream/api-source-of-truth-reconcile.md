# API as source of truth: checkpoint + log per-type reconcile

> Status: **design proposal** — agreed direction, not yet implemented. Supersedes the
> always-open "merged streaming tail" (M13) sketch in
> [../manifest/version2/per-type-reconcile-and-streaming-tail.md](../manifest/version2/per-type-reconcile-and-streaming-tail.md)
> with a watch-free, periodic-reconcile model built on the per-type Redis keyspace.
> Captured: 2026-06-10
> Owner: Simon
> Related:
> [demand-driven-type-materialization-lifecycle.md](demand-driven-type-materialization-lifecycle.md) (**which** types get a checkpoint and the Dormant→Syncing→Synced lifecycle that governs it — the demand layer beneath this doc),
> [audit-log-ingestion-and-ordering.md](audit-log-ingestion-and-ordering.md) (the log producer / ordering / late-lane detail this doc leaves out),
> [per-resource-type-rv-keyed-streams-experiment.md](per-resource-type-rv-keyed-streams-experiment.md) (the write-only prototype this consumes),
> [../manifest/version2/dream.md](../manifest/version2/dream.md) (the origin),
> [../manifest/reconcile-via-watchlist-mark-and-sweep.md](../manifest/reconcile-via-watchlist-mark-and-sweep.md) (the plan/sweep machinery, reused),
> [../manifest/version2/per-type-reconcile-and-streaming-tail.md](../manifest/version2/per-type-reconcile-and-streaming-tail.md) (M10–M12 it builds on).

## 1. One paragraph

The Kubernetes API is the **ultimate** source of truth; Git is the durable mirror that
follows it so closely it is the source of truth for everything downstream. Today each
GitTarget re-derives the API for itself — a per-reconcile streaming-list gather
([`StreamClusterSnapshotForGitDest`](../../../internal/watch/snapshot_stream.go#L76)) plus
a second always-open informer feed. This design replaces both with a single, standing,
**type-keyed materialization of the API in Redis** and makes GitTarget reconcile a
**consumer** of it. The API is captured **once per type** — and only for the types a GitTarget
actually follows ([demand-driven-type-materialization-lifecycle.md](demand-driven-type-materialization-lifecycle.md))
— by two decoupled writers — an
always-on audit-webhook **log** and a periodic LIST **checkpoint** — and every GitTarget
reconciles by **splicing the checkpoint with the log**, per type, into one commit. No
object watch stays open. Content hashing is dropped: a per-type, RV-anchored ordering
sequence makes "what is newer" exact, and the writer's existing no-op detection makes
"did it change" exact.

## 2. Requirements

These are the requirements this design must satisfy. They are the agreed acceptance
surface; every decision in §4 traces back to one.

| # | Requirement |
|---|---|
| **R1** | **API is the source of truth.** The reconcile's desired state is a pure function of the materialized API (checkpoint + log), never of Git paths or re-derivation from scratch. Git is the mirror. |
| **R2** | **Reconcile per type.** The unit of reconcile is one watched type `(GVK, GVR, scope)`; a GitTarget watching five types has five independent reconciles and one commit per type. |
| **R3** | **No long-lived object watches.** Watches are opened only briefly to fill a checkpoint, then closed. The only always-on intake is the audit-webhook push. (A *discovery* watch for new resource **types** is allowed — it is type-level, not object-level.) |
| **R4** | **The audit intake never stops.** It is the freshness feed; missing events degrades freshness, never correctness (R13). Surviving failover is an HA concern, deferred (R10). |
| **R5** | **The checkpoint is refreshed periodically** by a LIST (or modern WATCH-with-bookmark), e.g. hourly — **not** by folding the audit log into it. The two feeds stay decoupled. |
| **R6** | **Reconcile splices checkpoint + log** to compute current desired state and emits **one** reconcile commit per type. It does **not** replay history as per-event commits, even on first sync of a new GitTarget. |
| **R7** | **Drop content hashing.** Remove the sha256-of-YAML dedup on both the informer edge and the event stream; rely on the ordering sequence (R8) + writer no-op detection. |
| **R8** | **Replayable, RV-anchored ordering.** The per-type log carries a self-assigned, strictly increasing sequence anchored to the resourceVersion, so a reconcile can replay from a checkpoint point with a guarantee, and RV-less events still get a position. |
| **R9** | **Per-type independence.** A wobbly, throttled, or removed type fails *itself*; stable types keep reconciling. |
| **R10** | **HA-ready, not HA-now.** The design must not preclude multiple replicas / failover, but HA is out of scope for this plan. |
| **R11** | **Fail-closed.** Never sweep Git on a stale, unobservable, or partial view. An untrusted absence is never a deletion. |
| **R12** | **Visibility.** An operator can see what a GitTarget follows, per-type sync state, and counts (metrics first, bounded status, optional inventory). |
| **R13** | **Big resource sets are a first-class case** — their own e2e and metrics (checkpoint duration, log lag, commit counts). |
| **R14** | **Coalescing is a follow-up.** Grouping co-arriving changes into one commit is desirable but not required for the first cut. |

## 3. The model: checkpoint + log, spliced per type

Two decoupled capture writers per type, neither holding a watch open, plus a per-type
consumer that splices them. This is a **checkpoint + write-ahead-log** shape.

```mermaid
flowchart TD
    subgraph CAP["Capture — per type (only the audit HTTP intake is always-on)"]
        AW["audit webhook (always-on PUSH)"] -->|every mutating change| LOG[":audit:stream — the log<br/>RV-anchored, replayable"]
        TMR["periodic timer + type-set change"] --> LIST["brief LIST / WATCH+bookmark, then CLOSE"]
        LIST -->|"full replace @ RV R"| CKPT[":objects:items @ :objects:rv=R — the checkpoint"]
    end
    LOG --> SPLICE
    CKPT --> SPLICE
    subgraph CON["Consume — per GitTarget × type (periodic + on audit arrival)"]
        SPLICE["desired = fold( checkpoint@R , log entries with seq > R )"]
        SPLICE --> SCOPE["filter to GitTarget scope"]
        SCOPE --> PLAN["BuildScopedPlan(store, desired, type-scope)"]
        PLAN --> COMMIT["one reconcile commit per type"]
    end
```

The two feeds are deliberately **not** connected (R5). They cover each other's weakness:

| Feed | Strength | Weakness | Covered by |
|---|---|---|---|
| `:audit:stream` (always-on push) | **freshness** (near-real-time) | completeness not guaranteed | the checkpoint heals it within the interval |
| `:objects:items` (periodic LIST) | **correctness** (authoritative full set; catches orphans / missed deletes) | up to one interval stale alone | the log makes the reconcile current |

This is what lets R4 be true without being fatal: an audit gap costs **freshness until the
next checkpoint**, never **correctness** (R13, R11). It is also the HA seam (R10): the
checkpoint is a standing resume point a failover replica can reconcile against.

### 3.1 Relationship to what exists today

The live path is **already** Redis-stream-driven, not informer-driven:
[`AuditConsumer`](../../../internal/queue/redis_audit_consumer.go#L206) `XREADGROUP`s one
canonical stream, matches rules per event, and routes `git.Event`s to the BranchWorker.
This design is **v2 of "audit drives Git"**: shard that single stream into the per-type
`:audit:stream`s the prototype already writes, add the periodic checkpoint for
mark-and-sweep correctness (deletes/orphans no longer depend on a delete event arriving),
and make the unit a per-type splice reconcile. The plan/apply machinery is reused
unchanged (§5).

## 4. Decisions (the choices, with rationale)

### DEC-1 — Capture = periodic checkpoint + always-on log, decoupled  *(satisfies R3, R4, R5)*

**Chosen.** The checkpoint is the only thing that touches the API on a schedule, and it
closes immediately. The log is fed by the existing audit-webhook tap
([`mirrorByType`](../../../internal/webhook/audit_handler.go#L518)). They are never wired
to each other.

*Rejected:* keep `:objects:items` live by folding the log into it — couples the two feeds,
re-introduces a standing consumer, and makes the snapshot only as complete as the audit
policy. *Rejected:* an always-open informer/streaming tail (the old M13) — violates R3 and
owns reconnect / `410 Gone` / fan-out-refcount lifecycle we no longer need.

### DEC-2 — Reconcile = splice(checkpoint, log) per type → one commit  *(satisfies R1, R2, R6)*

**Chosen.** Per `(GitTarget, type)`, desired state is `fold(checkpoint, log-after-R)`,
scoped, then `BuildScopedPlan` → one commit. Pure function of the materialized API (R1).
No history replay (R6).

### DEC-3 — RV-ordered, replayable log (drop millisecond-first ordering)  *(satisfies R8)*

**Chosen.** Re-key the per-type log from today's millisecond-first `<stage_millis>-<rv>`
to **resourceVersion-first**, so the log is ordered by etcd commit order and a reconcile
can replay exactly from a checkpoint (`XRANGE (R +`). This makes "is this event after
checkpoint R?" a precise, delivery-order-independent test, which is what lets DEC-6 drop
content hashing.

The encoding (`<rv>-*` with Valkey-allocated subsequence), the RV-comparison rules, the
diagnostic **late lane** for out-of-order arrivals, RV-less placement, and the deferred
Lua / pre-sorter improvements all live in the dedicated producer design:
**[audit-log-ingestion-and-ordering.md](audit-log-ingestion-and-ordering.md)**. The
load-bearing invariant that doc establishes, and that this reconcile relies on: **the main
stream is strictly RV-ordered — we never knowingly insert an out-of-order event** — so the
splice in §6 can fold by stream position.

### DEC-4 — Checkpoint trigger = demand-gated, periodic **and** event-driven  *(satisfies R2, R5, R9)*

**Chosen.** A checkpoint is built **only for a type a GitTarget claims** — not for every
followable type — and once built is re-anchored on a timer (default ~1h) **and** on a
deliberate type-set change (a claim that adds the type, or a catalog generation bump from a
CRD installed/upgraded). The phase machine that governs this — `Dormant → Requested → Syncing
→ Synced ⇄ Resyncing`, the claim/lease demand model, and the single periodic pass that both
re-anchors the still-wanted and releases the no-longer-wanted — is specified in
**[demand-driven-type-materialization-lifecycle.md](demand-driven-type-materialization-lifecycle.md)**.
This doc assumes a `Synced` checkpoint exists for the type being reconciled.

### DEC-5 — RV-less events: best-effort in the log, correctness from the next checkpoint  *(satisfies R11, R13)*

**Chosen consequence.** RV-bearing events (all creates/updates) replay exactly. RV-less
events (some deletes, collection verbs) get a best-effort placement in the log for
**freshness**; their **correctness** is guaranteed by the next checkpoint — the LIST will
simply not contain a deleted object, and the type-scoped mark-and-sweep removes it. We do
not over-engineer the RV-less path; the checkpoint backstops it. The placement mechanics
are in [audit-log-ingestion-and-ordering.md](audit-log-ingestion-and-ordering.md) (IR5).

### DEC-6 — Drop content hashing  *(satisfies R7)*

**Chosen, and wanted.** Remove `isDuplicateContent`
([informers.go](../../../internal/watch/informers.go)) and `computeEventHash` /
`processedEventHashes`
([git_target_event_stream.go](../../../internal/reconcile/git_target_event_stream.go)).
"Newer?" is answered by the stream position (DEC-3); "changed?" is answered by the writer's
existing no-op detection (`manifestedit.Decide` → `EditNoChange`,
`manifestsAreSemanticallyEqual` in [plan_flush.go](../../../internal/git/plan_flush.go)),
already computed for free at the commit boundary. Measured before/after on a high-churn
type (R13); the cheap fallback if ever needed is a per-identity last-position equality
check (string compare, not a hash).

### DEC-7 — Retire long-lived informers + the RECONCILING handover  *(satisfies R3)*

**Chosen.** With the audit push as the sole live feed and the checkpoint as the periodic
truth, the informer object-watch pipeline
([`startInformersForGVRs`](../../../internal/watch/manager.go) +
[`addHandlers`](../../../internal/watch/informers.go)) and the bootstrap/steady-state
handover buffer (`BeginReconciliation`/`OnReconciliationComplete`) are removed. Deleted-
final-state handling and shared fan-out, which informers gave for free, are now provided by
the checkpoint (catches missed deletes) and Redis fan-out (one capture, N consumers).

### DEC-8 — Coalescing is a follow-up  *(satisfies R14)*

**Chosen.** First cut may reconcile-and-commit per audit-triggered wake-up. Debouncing
co-arriving changes into one commit per window is a later optimization; the BranchWorker
commit window already coalesces, so this is tuning, not new machinery.

## 5. What is reused unchanged

The entire write side stays. **Only the source of `Desired` changes** — from "a live API
stream this reconcile opened" to "the spliced materialization."

- [`BuildScopedPlan`](../../../internal/manifestanalyzer/plan.go#L210) — type-scoped
  mark-and-sweep; a reconcile passes that type's desired set + scope predicate, a pure
  sweep passes an empty desired set (M12).
- [`ResyncRequest{Desired, Revision, ScopeGVR}`](../../../internal/git/types.go#L224) +
  [`EnqueueResync`](../../../internal/git/branch_worker.go#L294) — the BranchWorker entry
  point; `ScopeGVR` already makes a resync per-type.
- The BranchWorker single-writer queue, commit window, and `plan_flush` apply path.

## 6. The splice, specified

Per `(GitTarget, type)` at reconcile time:

```text
R        := :objects:rv                              # checkpoint revision = replay cursor
desired  := decode(:objects:items)                   # the anchor set, pinned at R
for entry in XRANGE :audit:stream (R +:              # log strictly after the checkpoint
    if entry.verb is delete: delete desired[entry.identity]
    else:                    desired[entry.identity] = object(entry)   # last-writer-wins by position
desired  := filter(desired, GitTarget namespaces/scope)
plan     := BuildScopedPlan(store, files, desired, scope(type))
enqueue plan on BranchWorker  →  one commit
```

- The audit entry already carries the full object body (`payload_json`), and the existing
  [`extractObject`](../../../internal/queue/redis_audit_consumer.go#L846) /
  `sanitize` path turns it into a Git-writable object — reused verbatim.
- Idempotent: re-running yields the same `desired` (pure function of checkpoint + log).
- Exact under async delivery: membership in `(R +` is decided by `objectRV`, not arrival
  time (DEC-3).
- Bounded: the log is trimmed to the checkpoint cursor on each re-anchor, so a reconcile
  never scans more than one interval of history.

## 7. Failure / consistency model  *(R11)*

```mermaid
stateDiagram-v2
    [*] --> Fresh: checkpoint synced + log flowing
    Fresh --> Reconcile: fold + commit (per type)
    Fresh --> Hold: checkpoint phase != synced / Redis unreachable
    Fresh --> ReAnchor: log trimmed past cursor
    Hold --> Fresh: capture recovers (NEVER sweeps while held)
    ReAnchor --> Fresh: next checkpoint re-anchors + re-trims
```

- **Checkpoint not `synced`, or Redis unreachable → hold, sweep nothing** (R11). An
  unobservable surface is never a trusted absence — the same guard as M12's degraded-
  catalog hold. (The `Syncing`/`Synced`/`Resyncing`/`Failing` phase vocabulary and its own
  first-sync-hold vs fail-closed-re-anchor handling live in
  [demand-driven-type-materialization-lifecycle.md](demand-driven-type-materialization-lifecycle.md) §7.)
- **Log trimmed past a cursor → wait for / force the next checkpoint** and reconcile from
  it. Bounded by the checkpoint interval, so rare by construction.
- **A type whose checkpoint LIST fails holds itself**; siblings keep reconciling (R9).
- **Consistency pin** is `(commit SHA, :objects:rv, last-applied log position)` per type.
  Cross-type "max position" interpretation is an open question (§9).

## 8. Implementation steps

Ordered; each step is independently shippable and ends green. R0 (the write-only prototype)
has landed.

1. **R1 — Periodic, re-keyed checkpoint.**
   - The *trigger* — when [`mirrorTypeObjects`](../../../internal/watch/type_objects_mirror.go#L59)
     runs for which type, demand-gated and re-anchored on the per-type timer + event triggers —
     is owned by
     [demand-driven-type-materialization-lifecycle.md](demand-driven-type-materialization-lifecycle.md)
     (its L-3/L-4). This step is the *writer mechanics* the checkpoint phase drives.
   - Re-key the log to RV-first and add the diagnostic late lane — the full producer spec
     is [audit-log-ingestion-and-ordering.md](audit-log-ingestion-and-ordering.md) (its
     §9 baseline is the concrete change to
     [`RedisByTypeStreamQueue`](../../../internal/queue/redis_bytype_queue.go)).
   - On each re-anchor, trim `:audit:stream` to the new `:objects:rv` (never below the
     oldest live checkpoint cursor) and update `:objects:state`.
   - *Done when:* checkpoints refresh on schedule and on type-set change; the log is
     RV-ordered and replay-rangeable by `:objects:rv`; still no consumer.

2. **R2 — Splice reconcile (headline).**
   - New per-type consumer that reads `:objects:items` + `XRANGE :audit:stream (R +`,
     folds to `desired`, scopes, calls `BuildScopedPlan`, and `EnqueueResync` with
     `ScopeGVR` — replacing `StreamSnapshotForType` as the desired-set source.
   - Trigger: periodic + on audit arrival for a watched type (wire off the existing
     [`EmitTypeReconcileForGitDest`](../../../internal/watch/event_router.go#L324) seam).
   - Fail-closed per §7.
   - *Done when:* a GitTarget reconciles per type off Redis with no per-reconcile API call;
     N GitTargets fan out from one capture; mark-and-sweep stays within type boundaries;
     multi-type files stay document-correct; a wobbly type does not block siblings (R9).

3. **R3 — Retire the old live + gather paths (DEC-7).**
   - Remove the informer object-watch pipeline and the RECONCILING handover; route the
     per-type audit shards instead of the single canonical stream.
   - Keep `StreamClusterSnapshotForGitDest` *only* as the R1 checkpoint writer.
   - *Done when:* one live path (audit push) remains; no informer object caches; e2e green.

4. **R4 — Commit coalescing (DEC-8 / R14).** Debounce audit-triggered reconciles so
   co-arriving changes group per commit window. *Done when:* a burst on one type yields a
   small, bounded number of commits.

5. **R5 — Drop content hashing (DEC-6 / R7).** Remove both hashes; measure CPU on a
   high-churn type before/after; keep the last-position equality fallback ready. *Done
   when:* no regression on a status-churn workload; CPU measured.

6. **R6 — Visibility (R12).** Surface the keyspace that already exists: `__index__` +
   `:objects:state` (phase/count/rv/updated_at) as per-`(GitTarget, type)` metrics, a
   bounded `GitTargetStatus` roll-up (total/synced/failing, CRD version, last position),
   and an optional queryable inventory. *Done when:* an operator can see what a GitTarget
   follows and each type's sync state without bloating the object.

7. **R13 — Large-set e2e + metrics** (alongside R2/R3): a synthetic cluster-wide type with
   thousands of objects; assert per-type commits, correct sweep, no lost tail event,
   bounded checkpoint duration and log lag.

## 9. Open questions

- **Checkpoint interval.** *Settled: ~1h default.* Open only on whether it is per-type
  tunable. Freshness between checkpoints rides entirely on the log (R4); the interval only
  bounds correctness drift and log size.
- **Cross-type consistency.** Per-type gives one position per type; if any consumer needs a
  folder-wide consistent revision (status, a future audit join), define the "max position
  across types" interpretation.
- **Trigger debounce shape (R14).** Per-type timer vs. audit-arrival-driven vs. both, and
  the debounce window that best matches the commit window.
- **HA (R10, deferred).** Leader-elected single consumer today
  ([`NeedLeaderElection`](../../../internal/queue/redis_audit_consumer.go#L200)). Multi-
  replica fan-out, the never-stop audit intake across failover, and per-type cursor
  ownership are a separate plan; this design must not preclude them.

> Ordering / late-lane / ingestion open questions (the pre-sorter and Lua triggers, the
> non-numeric-RV policy, the §7 investigation) live in
> [audit-log-ingestion-and-ordering.md](audit-log-ingestion-and-ordering.md), not here.
