# Demand-gated audit ingestion: only mirror types that are actually wanted

Status: **agreed design / no code yet.** 2026-06-17.

Author note: this grew out of the late-lane diagnostics work
([`late-lane-e2e-2026-06-16-investigation.md`](late-lane-e2e-2026-06-16-investigation.md)
and the 2026-06-16 fresh-run follow-up). Chasing "why is anything in the late lane" surfaced a
bigger, dumber problem underneath it, described in §2.

**Start here (implementing in a fresh context):** read §3 (principles — especially "over-capture is
free, under-capture is rare and self-healed") and §6–§7 (coverage model + the shared-list /
ping-stream mechanism), then §11–§13 (acceptance, code map with line anchors, red-first order).
Background: the demand lifecycle
([`demand-driven-type-materialization-lifecycle.md`](../../finished/demand-driven-type-materialization-lifecycle.md))
and the reconcile model ([`api-source-of-truth-reconcile.md`](../../finished/api-source-of-truth-reconcile.md)),
whose **checkpoint is the correctness authority this design leans on** (§6).

## 1. Scope and one paragraph

Today the audit webhook mirrors **every** audited, body-merged event into a per-resource-type
Redis stream — unconditionally, for every type the cluster emits, whether or not any GitTarget will
ever follow it ([`audit_handler.go` `mirrorByType`](../../../internal/webhook/audit_handler.go#L499)
→ [`RedisByTypeStreamQueue.Enqueue`](../../../internal/queue/redis_bytype_queue.go#L199)). The
**checkpoint/objects** sink is already demand-driven — the materializer only LISTs *claimed* types
(L-3, `mirrorTypeObjects`) — but the **audit-log** sink is not. This doc closes that gap: gate the
per-type audit mirror on a **shared required-types set in Redis**, kept fresh on every ingest pod via
a tiny **ping stream**, so a type is mirrored only while it is wanted and torn down when it is not.
The design is **multi-pod-ready from day one** (any pod may receive any type's audit events), and it
deliberately favours *over*-capture, because the periodic checkpoint is the correctness backstop
(§6).

## 2. Motivation

### 2.1 Mirroring every type is the perfect way to make Redis explode

The per-type stream model is great for a *followed* type and pure overhead for an *unfollowed* one.
We currently pay, for every type the cluster ever audits:

- a `…:audit:stream` (RV-ordered, `MAXLEN`-bounded — but bounded × N types),
- a `…:audit:late` lane (never trimmed),
- a `…:audit:idstate` hash,
- an entry in the `…:__index__` set.

The cost scales with **the cluster's type cardinality and churn**, not with **demand**. That is
backwards. A real cluster has hundreds of CRDs plus the high-churn core/system types
(`events.k8s.io/events`, `discovery.k8s.io/endpointslices`, leases, `controllerrevisions`, …), and a
typical install follows a *handful* of them. Every unfollowed type is a stream we write, index, and
never read.

Measured on a fresh `task test-e2e` run (2026-06-17), which is already **wildcard-heavy** (most types
get claimed), the waste is still plain:

| | types | stream entries |
|---|---:|---:|
| claimed (has `:objects:state`) | 62 | 244 |
| **unclaimed (mirrored for nothing)** | **13** | **220 (~47%)** |

The 13 unclaimed types carrying ~half the stream volume are exactly the high-churn ones nobody
follows — `discovery.k8s.io:endpointslices` (38), `core:namespaces` (34), `events.k8s.io:events`
(24), `monitoring.coreos.com:prometheuses/alertmanagers/...` (18 each), `batch:jobs/cronjobs` (18
each), `apps:controllerrevisions` (18). On a cluster with **targeted** WatchRules instead of
wildcards the ratio inverts: a few claimed types out of hundreds, the rest pure waste. There is no
`MAXLEN` that fixes this — the leak is in the **number of streams**, not their length.

### 2.2 It is also a late-lane noise source

The late-lane investigations keep finding the same shape: the diagnostic noise is concentrated on
types that are either dry-run-only, or **cold/unfollowed**, or being touched by cluster lifecycle
machinery *before any object of that type was ever materialized*:

- `core:namespaces` / `core:configmaps` warmup creates (the e2e `gitops-reverser-audit-warmup`
  primer) — and `namespaces` is **unclaimed** here, so today it gets a stream + a late entry for a
  type nobody follows.
- `wardle.example.com:flunders` namespace-GC `deletecollection`s, all stamped **13:05–13:07**, while
  the first real flunder is not mirrored until **13:12** — i.e. they all land *before* flunders
  materialization starts.

Both classes vanish when the mirror only exists during a type's active materialization window (§5).
What is left in the late lane after that is the genuinely interesting case — a real recent reorder on
an actively-materialized type — which the floor/empty-stream guard in
[`api-source-of-truth-reconcile`](../../finished/api-source-of-truth-reconcile.md)'s late-lane
cleanup then classifies (§8). The two changes are complementary: demand-gating removes the *surface*,
the guard cleans the *residual*.

## 3. Principles

1. **Demand, not cardinality, decides what we mirror.** A type stream exists iff some GitTarget
   currently wants it. (Mirrors the checkpoint sink's existing rule.)
2. **One shared signal in Redis.** The set of required types lives in Redis (`__required__`) and is
   readable by every ingest pod; a tiny ping stream (`__required__:updates`) keeps each pod's local
   copy fresh. There is exactly one definition of "wanted", shared, with no per-pod divergence.
3. **Over-capture is free; under-capture is rare and self-healed.** Mirroring a type slightly *too
   early* only writes entries with `rv ≤ C` that the next trim drops — harmless. Mirroring slightly
   *too late* can miss an event, but **the periodic checkpoint re-LIST captures it at the next
   re-anchor** — so a miss costs *freshness*, never *correctness*. We therefore bias hard toward
   early/over-capture (§6). The checkpoint, not the gate, is the correctness authority.
4. **Best-effort on the hot path.** `Allow` is an in-memory lookup against the locally-cached set; a
   stale cache or a Redis hiccup never fails an audit request and self-corrects on the next ping or
   slow-poll (§7, §10).
5. **Release reclaims space.** Losing demand for a type removes it from `__required__` and a janitor
   deletes its keys, so Redis shrinks when WatchRules narrow — the inverse of the explosion.

## 4. Invariants

- **DG1 (coverage — best-effort, checkpoint-backstopped).** For a type's materialization window with
  first checkpoint revision `C`, the log *should* contain every event with `rv > C`. We make that
  hold in practice by adding the type to `__required__` **early** (at the `Requested` transition,
  well before the LIST that pins `C`) and propagating via the ping stream, so by the time `C` exists
  every pod is already mirroring (§6). When a miss does occur (a pod that has not yet seen the ping
  processes an `rv > C` event), the gap is **bounded by one re-anchor interval and healed by the next
  checkpoint** — never permanent loss. (Contrast DG1's *hard* form, which would require a synchronous
  cross-pod lead before every first LIST; we deliberately do not pay that — see §14.)
- **DG2 (no orphan streams).** A type not in `__required__` ends up with no `…:audit:*` keys and no
  `__index__` membership after a bounded delay — enforced by a janitor that reconciles actual stream
  keys against `__required__` (§7), so the cleanup tolerates lagging writers and dead pods.
- **DG3 (membership ⊆ checkpoint demand).** A type is only ever in `__required__` while it is
  claimed ∩ followable — i.e. a subset of what the checkpoint driver syncs. Over-capture lives
  *within* a type's own window (early add), never across types that are not wanted at all.

## 5. What "wanted" means and when it flips

A type is wanted exactly when it is **claimed ∩ followable** — precisely the set of phases past
`PhaseDormant` in the materializer
([`materializer.go`](../../../internal/typeset/materializer.go#L84-L132)):

| Phase | In `__required__`? | Why |
|---|:--:|---|
| `Dormant` | no | no live claim, or not followable — nothing wants it |
| `Requested` | **yes (added here)** | claimed + followable; added *early*, before the first LIST, to maximise propagation slack (§6) |
| `Syncing` / `Synced` / `Resyncing` / `Failing` | yes | actively materializing or serving a checkpoint |

Membership is added on `Dormant → Requested` and removed on release (`→ Dormant`). The materializer
leaf stays Redis-free: it emits phase events; the watch/driver layer (which already owns Redis)
translates them into `Require`/`Unrequire` writes (§7).

## 6. Coverage model: add early, heal late

The whole correctness argument rests on one asymmetry (Principle 3): **over-capture is free,
under-capture is self-healed.** That lets the gate be a cheap, eventually-consistent shared cache
instead of a synchronously-coordinated lock.

Timeline for one type's first sync, across pods:

```
  type enters Requested        checkpoint LIST taken            later writes
  SADD __required__ + ping          (rv = C)                  (rv > C, captured)
        │   ── ping propagates ──▶        │                     │    │    │
  ──────●───────────●──────────●──────────●───────────●─────────●────●────●────►  time
        │           │          │          │
        │     pod A sees it  pod B sees it │   (all pods mirroring well before C)
        └───────────── slack ─────────────┘
```

- **Add early.** Membership is written when the type enters `Requested`, which is *before* the driver
  runs the LIST (the LIST is downstream of `BeginSync`, which is downstream of `Requested`). The
  natural latency between "claim observed" and "LIST issued" is the propagation slack — no explicit
  sleep or lead-time constant needed. The entries this captures with `rv ≤ C` are redundant and are
  dropped by the normal trim ([`TrimTypeAuditLog`](../../../internal/queue/redis_bytype_queue.go#L250)).
- **Fast propagation via a stream, not pub/sub.** A Redis *stream* is replayable: a pod resumes from
  its last-read ID, so a brief disconnect never silently loses a wakeup (pub/sub would). Each entry is
  a trivial "changed" ping (an epoch counter); pods react by re-reading the *whole* `__required__`
  set, so the update is idempotent and a pod that missed many pings still converges from the latest
  one. `MAXLEN` can be tiny.
- **The rare miss is healed by the checkpoint.** If a pod processes an `rv > C` event before its cache
  reflects the open, that one event is absent from the log. Between `C` and the next checkpoint `C'`
  the mirror is briefly stale; at `C'` (≥ that event's rv) the consistent LIST re-reads the world and
  the state is captured. The log/tail is a *freshness optimisation over* the checkpoint, which is the
  correctness floor (the api-source-of-truth model) — so a gate miss degrades latency, not integrity.

Why this is sound rather than just convenient: the cost of a miss is exactly "a change is reflected
in the GitTarget mirror one re-anchor later than ideal," and re-anchors already run on a timer (and
are nudged by late-lane events). For a GitOps-reversal mirror that is an acceptable, bounded
staleness — and it removes an entire class of cross-pod coordination from the hot path.

## 7. Mechanism — shared required-set + ping stream

```
                 Redis (shared)
   ┌────────────────────────────────────────────┐
   │ <prefix>:__required__          SET of base  │
   │ <prefix>:__required__:updates  ping stream  │
   └───▲───────────────────────────────┬────────┘
       │ SADD/SREM + XADD ping          │ SMEMBERS (seed) + XREAD BLOCK (wakeup) + slow poll
   writer: watch/driver               reader: every ingest pod
   (Require/Unrequire on phase)       (maintains local set; Allow() reads it)
       │                                   │
   janitor: reconcile keys vs set     mirrorByType: Allow(group,resource) before Enqueue
```

**Keys.** Two shared keys under the existing prefix (`gitops-reverser`):
- `…:__required__` — a SET whose members are per-type **base keys** (`<prefix>:<group>:<resource>`,
  same shape as [`__index__`](../../../internal/queue/redis_bytype_queue.go#L60) /
  [`typeBaseKey`](../../../internal/queue/redis_bytype_queue.go#L650)).
- `…:__required__:updates` — a Redis stream; each entry is a small ping (e.g. `epoch=<n>`). Trimmed
  to a tiny `MAXLEN` (only the latest entry matters; older ones are wakeups already consumed).

**Reader (every ingest pod, hot path stays in-memory).**
- On startup: read the stream's current last ID, then `SMEMBERS __required__` to seed a local
  `map[baseKey]struct{}`, then begin `XREAD BLOCK` from that last ID (this ordering can't miss an
  update between seed and subscribe).
- A background goroutine blocks on `XREAD`; on *any* new entry it re-reads `SMEMBERS __required__`
  and swaps the local set. A **slow poll** (re-`SMEMBERS` every ~30s regardless) backstops a missed
  wakeup or a silent period.
- [`mirrorByType`](../../../internal/webhook/audit_handler.go#L499) calls `Allow(group, resource)` —
  an O(1) local lookup — before `Enqueue`. The key is derived exactly like
  [`baseKey`](../../../internal/queue/redis_bytype_queue.go#L632): a `scale` subresource maps to the
  **parent** type (DEC-A), a missing resource (`__unknown__`) is never allowed. On `false` we skip
  `Enqueue` (and can skip the body-join/park work earlier in
  [`processEvent`](../../../internal/webhook/audit_handler.go#L290), since the joined event has no
  consumer but this mirror now that the canonical stream is retired).

**Writer (the materialization owner; single-pod = the one pod, multi-pod = the per-type owner, §9).**
- `Require(gvr)` on `Dormant → Requested`: `SADD __required__ <base>` then `XADD …:updates * epoch <n>`.
  Idempotent — re-adding an already-required type is a no-op `SADD` + a cheap ping (skip the ping if
  the `SADD` added nothing).
- `Unrequire(gvr)` on release: `SREM __required__ <base>` + ping. This stops *new* mirroring across
  pods within a ping (or the slow-poll) — fast. It does **not** delete the type's data keys; the
  janitor does (below), which avoids racing a lagging pod that is still mid-flush.

**Cleanup (janitor, race-tolerant — replaces "delete synchronously on release").** A periodic pass on
the writer reconciles the keyspace against demand: for each base in
[`__index__`](../../../internal/queue/redis_bytype_queue.go#L60) (or scanned stream keys) that is
**absent from `__required__` for a full grace period** and idle (no recent writes), call
`DeleteType(group, resource)`. This:
- reclaims released types (DG2) without depending on precise release timing;
- cleans an orphan a lagging pod may have re-created after `SREM` (deleted next pass);
- cleans entries left by a crashed writer once a new owner runs the janitor (§9).

`DeleteType(group, resource)` is a new
[`RedisByTypeStreamQueue`](../../../internal/queue/redis_bytype_queue.go#L130) method: `DEL`s
`…:audit:stream`, `…:audit:late`, `…:audit:idstate` and `SREM`s the base key from `__index__` (mirror
of [`ensureIndexed`](../../../internal/queue/redis_bytype_queue.go#L603)); the `…:objects:*` keys are
released by the demand lifecycle.

**The gate type.** All of the above sits behind one small `TypeMirrorGate`:
`Allow(group, resource) bool` (reader), `Require(gvr)` / `Unrequire(gvr)` (writer), plus the internal
subscriber goroutine and seed/slow-poll. There is a single Redis-backed implementation — this is the
design from day one, single-pod included (the one pod is both the writer and the sole subscriber, at
negligible cost). No in-memory-only variant is kept.

## 8. Interaction with the late-lane guard

These compose; neither subsumes the other:

- **Demand-gating (this doc)** removes (a) all streams for never-claimed types — the bulk of the
  explosion and incidental noise like the `namespaces` warmup entry, and (b) all *pre-materialization*
  events for claimed types — e.g. the flunders `deletecollection`s that arrive before flunders is in
  `__required__` (anything before the early add is simply not mirrored).
- **The floor / empty-stream guard** (separate proposal) then classifies the *residual* on
  actively-materialized types: events below the checkpoint floor → provably superseded (drop + count),
  RV-less-before-high-water → no-op (drop + count), leaving the late lane holding only genuine recent
  reorders with a `lag` field for triage.

End state: the late lane is empty in a clean run, and any entry in it is actionable — the original
goal.

## 9. Multi-pod operation, ownership, and boot

The design is multi-pod from day one; the open questions are *who writes* `__required__` and how a
restart recovers.

- **Writers are single per type.** Each type's membership is written by that type's materialization
  owner. In single-pod that is trivially the one pod; in multi-pod it rides the **per-type
  single-writer ownership** that HA needs anyway for the LIST/trim
  ([`ha-improvements.md` §5](ha-improvements.md), genuinely deferred). One owner per type means no
  `SADD`/`SREM` fights. The ping stream and the `__required__` set themselves are plain shared
  structures — no per-entry TTL/lease — because a single writer per type is the simpler liveness
  story than N self-expiring leases.
- **Failover rebuilds membership from durable state.** On a new owner taking over (or any boot), it
  reconstructs the set of required types it owns from the durable claim + checkpoint state
  (`:objects:state`, the demand records — [`ha-improvements.md` §3](ha-improvements.md#L86)) and
  re-asserts `Require` for each, then the janitor reclaims anything no longer owned/required. No
  handoff message to lose.
- **Readers seed before serving.** An ingest pod completes its initial `SMEMBERS` seed before the
  audit handler starts accepting events, so a restart does not begin in a blind state. (Even if it
  did, Principle 3 bounds the damage to a checkpoint-healed miss.)
- **Stream stays tiny by nature.** The update rate is bounded by *claim churn* (types entering/leaving
  the followed set — rare, controller-paced), not by event rate. With ping-not-payload entries and a
  small `MAXLEN`, `__required__:updates` cannot become hot.

## 10. Edge cases & failure modes

- **Cache lags a fresh open (the DG1 miss).** A pod processes an `rv > C` event before its ping
  arrives → that event is absent from the log → mirror briefly stale → healed at the next checkpoint
  (§6). Made rare by early-add + stream wakeup; never unbounded.
- **Release race (lagging writer).** After `SREM` + ping, a slow pod may `Enqueue` one more entry for
  the now-unwanted type, re-creating its stream. The janitor's "absent for a grace period + idle"
  rule deletes it on a later pass (DG2). No synchronous delete to race.
- **Writer/owner crash.** `__required__` entries it owned persist until a new owner rebuilds (and may
  re-assert them) and the janitor reclaims any that are no longer wanted. Plain set + janitor, no TTL
  needed.
- **Reader disconnect.** `XREAD` resumes from the last ID (stream replay); the slow poll covers a long
  outage; a full restart re-seeds via `SMEMBERS`.
- **Followable churn / CRD wobble.** A wobble freezes sync but keeps serving the checkpoint
  (`frozen`); the type stays in `__required__` (we still want freshness for a served type) and is
  removed only on actual release.
- **Unknown / no-objectRef events.** Never wanted → never mirrored (today they pollute the
  `__unknown__` bucket).
- **Redis unavailable for the gate read.** Hot path uses the last-known local set, so ingestion keeps
  working on the most recent membership; it converges when Redis returns. A gate write failure is
  logged and retried on the next phase event / janitor pass.

## 11. Acceptance criteria

1. A type never added to `__required__` produces **zero** `…:audit:*` keys and no `__index__`
   membership.
2. **Happy-path coverage:** a freshly claimed type added at `Requested` captures every `rv > C` write
   — integration test: claim → (early add) → race writes around the LIST → assert the tail sees every
   post-`C` write.
3. **Heal-on-miss (soundness of the best-effort posture):** with membership *forced* to arrive late
   (simulate a slow pod) so an `rv > C` event is missed, the next checkpoint re-anchor restores the
   missing state — assert the mirror converges after one re-anchor.
4. **Propagation:** a second subscriber updates its local set within one ping (and, with pings
   suppressed, within the slow-poll interval) of a `Require`/`Unrequire`.
5. **Release + janitor:** `Unrequire` stops new mirroring across pods within a ping; the janitor then
   deletes `…:audit:stream` / `…:audit:late` / `…:audit:idstate` and the `__index__` entry within the
   grace period (DG2).
6. On a clean `task test-e2e` run, `__index__` equals `__required__` (no unclaimed streams); the
   unclaimed-waste row in §2.1 is **0 types / 0 entries**.
7. No regression in tail freshness for claimed types; full suite + e2e green.

## 12. Code map (read these first)

Line anchors correct as of 2026-06-17 (`poc/redis-copy`); re-grep if they drift.

**New — the gate:** `TypeMirrorGate` (suggested `internal/queue` or a new `internal/gate`): the
`__required__` SET + `__required__:updates` stream ops, the subscriber goroutine (seed + `XREAD` +
slow poll), `Allow` / `Require` / `Unrequire`, and the janitor. Model its tests on
[`internal/queue/redis_bytype_queue_test.go`](../../../internal/queue/redis_bytype_queue_test.go)
(miniredis supports streams + sets).

**Reader — audit hot path:** [`internal/webhook/audit_handler.go`](../../../internal/webhook/audit_handler.go)
- `AuditHandlerConfig` [L66](../../../internal/webhook/audit_handler.go#L66); `ByTypeQueue` [L79](../../../internal/webhook/audit_handler.go#L79); `AuditEventQueue` [L83](../../../internal/webhook/audit_handler.go#L83) — add the gate alongside.
- `processEvent` [L290](../../../internal/webhook/audit_handler.go#L290) (early-gate candidate); `mirrorByType` [L499](../../../internal/webhook/audit_handler.go#L499) (**minimal correct gate point, before `Enqueue`**); `extractGVR` [L580](../../../internal/webhook/audit_handler.go#L580).

**Queue — key schema + new cleanup:** [`internal/queue/redis_bytype_queue.go`](../../../internal/queue/redis_bytype_queue.go)
- Suffixes [L57-60](../../../internal/queue/redis_bytype_queue.go#L57); `baseKey` [L632](../../../internal/queue/redis_bytype_queue.go#L632) + `typeBaseKey` [L650](../../../internal/queue/redis_bytype_queue.go#L650) — **gate key must match** (scale→parent, `__unknown__`).
- `Enqueue` [L199](../../../internal/queue/redis_bytype_queue.go#L199); `ensureIndexed` [L603](../../../internal/queue/redis_bytype_queue.go#L603); `TrimTypeAuditLog` [L250](../../../internal/queue/redis_bytype_queue.go#L250).
- **New:** `DeleteType(group, resource)` — `DEL` the three keys + `SREM` from `__index__`.

**Demand leaf — phase machine (stays Redis-free):** [`internal/typeset/materializer.go`](../../../internal/typeset/materializer.go)
- Phases [L59-77](../../../internal/typeset/materializer.go#L59); `Declare` [L157](../../../internal/typeset/materializer.go#L157); `BeginSync` [L240](../../../internal/typeset/materializer.go#L240); `SyncSucceeded` [L285](../../../internal/typeset/materializer.go#L285); `Sweep` [L384](../../../internal/typeset/materializer.go#L384); `releaseLocked` [L503](../../../internal/typeset/materializer.go#L503); `Inventory` [L618](../../../internal/typeset/materializer.go#L618). Emits `MaterializationEvent`s — do **not** add a gate dependency here.

**Driver + tail — where `Require`/`Unrequire` fire:** [`internal/watch/materialization.go`](../../../internal/watch/materialization.go), [`internal/watch/audit_tail.go`](../../../internal/watch/audit_tail.go)
- `Require(gvr)` on the `Requested` phase event (earliest) — at latest at the top of
  `runTypeCheckpointSync` [L504](../../../internal/watch/materialization.go#L504) **before** `BeginSync`
  (L505), so it precedes `mirrorTypeObjects` (the LIST, L508).
- `Unrequire(gvr)` on the release `MaterializationEvent` (the observer that already stops the tail);
  `runTypeAuditTail` [L134](../../../internal/watch/audit_tail.go#L134).
- Existing wiring nearby: `Declare` [L146](../../../internal/watch/materialization.go#L146); `trimTypeAuditLog` [L523](../../../internal/watch/materialization.go#L523); `NudgeTypeResyncForLateEvent` [L70](../../../internal/watch/materialization.go#L70).

**Wiring:** [`cmd/main.go`](../../../cmd/main.go) — `NewRedisByTypeStreamQueue` [L221](../../../cmd/main.go#L221); `AuditLogTrimmer` [L256](../../../cmd/main.go#L256); `NewAuditHandler` [L347](../../../cmd/main.go#L347) + `ByTypeQueue` [L351](../../../cmd/main.go#L351). Construct the gate; reader half → handler config; writer half + janitor → watch `Manager`.

## 13. Implementation order (red-first)

1. **Gate substrate** (`TypeMirrorGate`, Redis-backed): `__required__` SET + `__required__:updates`
   stream; `Require`/`Unrequire` (`SADD`/`SREM` + ping), reader (seed `SMEMBERS` → `XREAD` loop →
   slow poll) maintaining the local set, `Allow`. Unit tests (miniredis): membership, key derivation
   (scale→parent, `__unknown__`), ping→refresh, slow-poll backstop, reconnect/replay. *Red first.*
2. **Coverage tests:** (a) happy-path — early add → every post-`C` write captured; (b) heal-on-miss —
   forced-late membership → missed `rv > C` event restored by the next checkpoint (acceptance #2, #3).
3. **Reader wiring:** `mirrorByType` consults `Allow` before `Enqueue` (then optionally hoist earlier
   in `processEvent`).
4. **Writer wiring:** `Require` on `Requested` (phase-event observer, or top of `runTypeCheckpointSync`
   before `BeginSync`); `Unrequire` on the release observer.
5. **`DeleteType` + janitor:** new queue method (+ test); leader janitor reconciling `__index__` vs
   `__required__` (absent-for-grace + idle → delete), race/crash tolerant.
6. **`cmd/main.go`** construction; boot ordering (reader seeds before the handler serves; writer
   rebuilds `__required__` from durable state).
7. **e2e acceptance (§11 #1, #5, #6):** `__index__` == `__required__`; release → janitor deletes keys.
8. `task fmt → generate → manifests → vet → lint → test → test-e2e` (e2e sequential).

## 14. Alternatives rejected (for the record — do not re-propose without new reasons)

- **Per-type ZSET+TTL leases as the gate signal** ([`ha-improvements.md` §2](ha-improvements.md#L33)
  for the *claim* layer). Rejected for the gate: N self-expiring leases are more moving parts than one
  shared set + a single writer-per-type, and the TTL's only real benefit (auto-cleanup on writer
  death) is covered here by failover-rebuild + janitor. (The claim layer may still use leases; that is
  a separate concern.)
- **Synchronous "open-and-confirm-visible" lead before every first LIST** (block `Open` until all
  pods' caches reflect the open). It would make DG1 a *hard* guarantee, but it puts a cross-pod
  propagation wait on the first-sync path and a constant to tune — and the checkpoint already heals
  the rare miss, so the hard guarantee is not worth its cost (§6, Principle 3).
- **Per-event Redis `SISMEMBER`.** Linearizable and lead-free, but a Redis read on *every* audit event
  — including a read per event for *unwanted* high-churn types (no negative cache possible) — which
  defeats the cost win that motivated gating.

## 15. Open questions

- **Gate package home:** `internal/gate` vs folding into `internal/queue`. Leaning standalone so the
  audit handler depends on the gate, not the whole queue.
- **Early gating point:** gate at `Enqueue` (smallest change) vs hoist into `processEvent` to also
  skip the body-join for unwanted types (cheaper). Do the hoist once the coverage tests are green.
- **Janitor grace + cadence:** concrete values for "absent-for-grace" and the scan interval; pick once
  measured, default to a couple of janitor passes.
