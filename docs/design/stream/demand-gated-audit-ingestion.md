# Demand-gated audit ingestion: only mirror types that are actually wanted

Status: **proposal / no code change yet.** 2026-06-17.

Author note: this grew out of the late-lane diagnostics work
([`late-lane-e2e-2026-06-16-investigation.md`](late-lane-e2e-2026-06-16-investigation.md)
and the 2026-06-16 fresh-run follow-up). Chasing "why is anything in the late lane" surfaced a
bigger, dumber problem underneath it, described in §2.

**Start here (implementing in a fresh context):** read §6 (the load-bearing correctness rule),
then §12 (code map — every file/symbol to touch, with line anchors) and §13 (red-first
implementation order). Background context: the demand lifecycle
([`demand-driven-type-materialization-lifecycle.md`](../../finished/demand-driven-type-materialization-lifecycle.md))
and the reconcile model ([`api-source-of-truth-reconcile.md`](../../finished/api-source-of-truth-reconcile.md)).

## 1. Scope and one paragraph

Today the audit webhook mirrors **every** audited, body-merged event into a per-resource-type
Redis stream — unconditionally, for every type the cluster emits, whether or not any GitTarget
will ever follow it ([`audit_handler.go` `mirrorByType`](../../../internal/webhook/audit_handler.go#L499)
→ [`RedisByTypeStreamQueue.Enqueue`](../../../internal/queue/redis_bytype_queue.go#L199)). The
**checkpoint/objects** sink is already demand-driven — the materializer only LISTs *claimed*
types (L-3, `mirrorTypeObjects`) — but the **audit-log** sink is not. This doc proposes closing
that gap: gate the per-type audit mirror on the same demand signal the checkpoint already uses,
so a type's stream is created only when its materialization begins, and torn down when its demand
goes away. The one non-obvious part is correctness: the gate must open *before* the checkpoint's
LIST revision, or the log has a hole. §6 is about exactly that.

## 2. Motivation

### 2.1 Mirroring every type is the perfect way to make Redis explode

The per-type stream model is great for a *followed* type and pure overhead for an *unfollowed*
one. We currently pay, for every type the cluster ever audits:

- a `…:audit:stream` (RV-ordered, `MAXLEN`-bounded — but bounded × N types),
- a `…:audit:late` lane (never trimmed),
- a `…:audit:idstate` hash,
- an entry in the `…:__index__` set.

The cost scales with **the cluster's type cardinality and churn**, not with **demand**. That is
backwards. A real cluster has hundreds of CRDs plus the high-churn core/system types
(`events.k8s.io/events`, `discovery.k8s.io/endpointslices`, leases, `controllerrevisions`, …),
and a typical install follows a *handful* of them. Every unfollowed type is a stream we write,
index, and never read.

Measured on a fresh `task test-e2e` run (2026-06-17), which is already **wildcard-heavy** (most
types get claimed), the waste is still plain:

| | types | stream entries |
|---|---:|---:|
| claimed (has `:objects:state`) | 62 | 244 |
| **unclaimed (mirrored for nothing)** | **13** | **220 (~47%)** |

The 13 unclaimed types carrying ~half the stream volume are exactly the high-churn ones nobody
follows — `discovery.k8s.io:endpointslices` (38), `core:namespaces` (34), `events.k8s.io:events`
(24), `monitoring.coreos.com:prometheuses/alertmanagers/...` (18 each), `batch:jobs/cronjobs`
(18 each), `apps:controllerrevisions` (18). On a cluster with **targeted** WatchRules instead of
wildcards the ratio inverts: a few claimed types out of hundreds, the rest pure waste. There is
no `MAXLEN` that fixes this — the leak is in the **number of streams**, not their length.

### 2.2 It is also a late-lane noise source

The late-lane investigations keep finding the same shape: the diagnostic noise is concentrated on
types that are either dry-run-only, or **cold/unfollowed**, or being touched by cluster lifecycle
machinery *before any object of that type was ever materialized*:

- `core:namespaces` / `core:configmaps` warmup creates (the e2e `gitops-reverser-audit-warmup`
  primer) — and `namespaces` is **unclaimed** here, so today it gets a stream + a late entry for a
  type nobody follows.
- `wardle.example.com:flunders` namespace-GC `deletecollection`s, all stamped **13:05–13:07**,
  while the first real flunder is not mirrored until **13:12** — i.e. they all land *before*
  flunders materialization starts.

Both classes vanish if the mirror only exists during a type's active materialization window (§7).
What is left in the late lane after that is the genuinely interesting case — a real recent
reorder on an actively-materialized type — which the floor/empty-stream guard in
[`api-source-of-truth-reconcile`](../../finished/api-source-of-truth-reconcile.md)'s late-lane cleanup then
classifies (§8). The two changes are complementary: demand-gating removes the *surface*, the
guard cleans the *residual*.

## 3. Principles

1. **Demand, not cardinality, decides what we mirror.** A type stream exists iff some GitTarget
   currently wants it. (Mirrors the checkpoint sink's existing rule.)
2. **One demand signal.** The audit-log gate and the checkpoint sink read the *same* claimed ∩
   followable set from the materializer — they can never disagree about whether a type is wanted.
3. **No gap, ever.** Turning the mirror on must not lose any event the checkpoint does not already
   contain. This is the load-bearing invariant (§6).
4. **Best-effort, like all ingestion.** The gate is an in-memory fast check on the audit hot path;
   a stale gate never fails an audit request and self-corrects (§9, §10).
5. **Release reclaims space.** Losing demand for a type tears its streams down, so Redis shrinks
   when WatchRules narrow — the inverse of the explosion.

## 4. Invariants

- **DG1 (coverage, load-bearing).** For every type, the earliest resourceVersion the log captures,
  `L`, must sit **at or just below** the first checkpoint revision `C` of that materialization
  window: `L ≤ C`, and ideally `C − L` small. Two separate requirements:
  - **`L ≤ C` is mandatory (correctness).** If `L > C`, the events in `(C, L)` are in neither the
    checkpoint (which only holds `rv ≤ C`) nor the log (which only holds `rv ≥ L`) → silent loss
    until the next full checkpoint. So the gate must open **before** the LIST that pins `C`.
  - **`C − L` small is the goal (efficiency).** Every captured entry with `rv < C` is redundant
    with the checkpoint and exists only to be trimmed ([`TrimTypeAuditLog`](../../../internal/queue/redis_bytype_queue.go#L250)).
    We want the log to begin *a hair* below the checkpoint, not far below it.

  We get both by opening the gate **a little earlier than the LIST, but no earlier than necessary**:
  early enough guarantees `L ≤ C`; no-earlier-than-needed keeps `C − L` small. §6 makes "a little
  earlier" concrete (the `BeginSync → LIST` seam) and proves the coverage half.
- **DG2 (no orphan streams).** A type with no live claim has no audit stream/late/idstate keys and
  no `__index__` membership after a bounded delay (the release sweep).
- **DG3 (parity with checkpoint demand).** The set of mirrored types ⊆ the set of types the
  checkpoint driver will sync, at all times. (Subset, not equality — see DG1: the mirror opens
  slightly *before* the first checkpoint exists.)

## 5. What "wanted" means

The materializer already owns the demand table and the followability gate
([`materializer.go`](../../../internal/typeset/materializer.go#L84-L132)): a type is mirror-worthy
exactly when it is **claimed ∩ followable**, which is precisely the set of types not in
`PhaseDormant`. The phase machine already computes this:

| Phase | Mirrored? | Why |
|---|:--:|---|
| `Dormant` | no | no live claim, or not followable — nothing wants it |
| `Requested` | wanted, not yet open | claimed + followable, first sync queued; the gate opens when the sync starts (§6), and any event arriving in this brief window has `rv ≤ C` so the checkpoint covers it |
| `Syncing` | **yes** | gate opened at the `BeginSync → LIST` seam; first checkpoint LIST in flight |
| `Synced` / `Resyncing` / `Failing` | yes | serving (or last-served) a checkpoint; gate stays open |

Mirror membership is "any phase past `Dormant`", but the **open call is placed precisely at the
first sync's `BeginSync → LIST` seam** (§6), not literally at the `Dormant → Requested` transition
— that placement is what lands the log floor `L` just below `C` (DG1). The gate is **closed** on
release (last claim ages out → back to `Dormant`, keys deleted).

## 6. The ordering that makes it safe (DG1)

This is the heart of the user's instinct — *"ideally the first log entry is a little below the
last materialization; to get that we start it a bit earlier."* That is exactly right, and it has a
precise place in the code.

Timeline for one type's first sync:

```
        gate opens          checkpoint LIST taken              later writes
   (log floor L ⪅ C)              (rv = C)                   (rv > C, captured)
            │                        │                         │    │    │
  ──────────●────────────────────────●───────────●────────────●────●────●────►  etcd RV / time
            └─ tiny window: writes here          └──── tail folds log entries with rv > C ────┘
               land at rv ≤ C, trimmed
```

- The checkpoint is a consistent LIST at revision `C`; it already reflects every write with
  `rv ≤ C`.
- The tail replays log entries with `rv > C` (exclusive), folding forward from the checkpoint
  ([`auditTailAnchor`](../../../internal/watch/audit_tail.go#L101)).
- So **every event with `rv > C` must be in the log**, and the log may *also* hold some `rv ≤ C`
  entries (the tiny window between gate-open and the LIST) — those are redundant and get trimmed.
  The target is therefore `L ⪅ C`: a hair below, never above.

**Why "a hair before the LIST" both guarantees `L ≤ C` and keeps `C − L` small — with no clock
reasoning.** An event with `rv > C` was committed to etcd *after* `C`, and an audit event reaches
the webhook *after* its own commit (delivery can be delayed, never advanced — that delay is the
entire late-lane phenomenon). The gate is checked at *mirror time*. Since it opened before the LIST
produced `C`, it is already open by the time any `rv > C` event can possibly arrive — so none is
ever dropped (`L ≤ C`). And because we open it only just before the LIST, the only `rv ≤ C` entries
the log captures are those committed in the gate-open→LIST window — a handful — so `C − L` stays
small. ∎ (Note the asymmetry below: a *larger* lead is still safe, just wasteful; opening *after*
the LIST is unsafe.)

**The exact insertion point.** The first-sync driver is
[`runTypeCheckpointSync`](../../../internal/watch/materialization.go#L504):

```go
func (m *Manager) runTypeCheckpointSync(ctx, log, gvr) {
    if !m.materializerInstance().BeginSync(gvr) { return }   // L505  Requested → Syncing
    //  ◀── OPEN THE GATE HERE: gate.Open(gvr) (idempotent; a re-anchor finds it already open)
    rv, err := m.mirrorTypeObjects(ctx, log, gvr)            // L508  the LIST → rv = C
    if err != nil { m.materializerInstance().SyncFailed(gvr); return }
    m.materializerInstance().SyncSucceeded(gvr, rv)          // L514  pin checkpoint C
    m.trimTypeAuditLog(ctx, log, gvr, rv)                    // L515  trim log to C
}
```

Opening between `BeginSync` (L505) and `mirrorTypeObjects` (L508) is the "a bit earlier" the user
means: earlier than the LIST (so `L ≤ C`), but as late as the control flow allows (so `C − L` is
just the gate-open→LIST window). For a re-anchor the gate is already open, so `Open` is a no-op and
the floor does not reset. The `[L, C]` entries are trimmed at L515 by the normal re-anchor
([`TrimTypeAuditLog`](../../../internal/queue/redis_bytype_queue.go#L250)).

**The asymmetry, restated:** opening *too early* costs at most one checkpoint-interval of redundant
captured-then-trimmed entries (cheap, self-cleaning); opening *too late* — after the LIST, or "when
the materializer finished" — costs silent data loss in `(C, L)`. When the exact lead is uncertain,
**err early**.

## 7. Mechanism

A small, concurrency-safe membership oracle sits between the materializer (writer) and the audit
handler (reader):

```
  driver runTypeCheckpointSync ──Open(gvr)──▶                          ◀──Allow(group,resource)── audit handler
   (BeginSync → Open → LIST, §6)              TypeMirrorGate            (mirrorByType, before Enqueue)
  release observer ──Close(gvr)+DeleteType──▶ (map[key]struct{}, RWMutex/sync.Map)
   (MaterializationEvent: release)
```

- **Reader (hot path).** [`mirrorByType`](../../../internal/webhook/audit_handler.go#L499) computes
  the type key from the event's objectRef and consults `Allow` before `Enqueue`. The key derivation
  must match [`baseKey`](../../../internal/queue/redis_bytype_queue.go#L632) /
  [`typeBaseKey`](../../../internal/queue/redis_bytype_queue.go#L650) — notably a `scale`
  subresource gates on the **parent** type (DEC-A), and a missing resource (`__unknown__`) is never
  wanted. `Allow` is an O(1) in-memory lookup; on `false` we skip `Enqueue` entirely (and may skip
  the body-join/park machinery too — the joined event has no consumer but this mirror since the
  canonical stream was retired, so gating *early* in
  [`processEvent`](../../../internal/webhook/audit_handler.go#L290) is a pure win, with the minimal
  correct placement being right before `Enqueue`).
- **Writer — open (control path).** The gate is opened by the **driver**, not the materializer leaf:
  [`runTypeCheckpointSync`](../../../internal/watch/materialization.go#L504) calls `gate.Open(gvr)`
  between [`BeginSync`](../../../internal/typeset/materializer.go#L240) and
  [`mirrorTypeObjects`](../../../internal/watch/materialization.go#L508) (§6). Keeping the leaf
  ([`internal/typeset`](../../../internal/typeset/materializer.go)) free of any Redis/gate dependency
  preserves its purity — it stays the demand source of truth and emits phase events; the
  watch/driver layer (which already owns Redis) translates them into gate calls.
- **Writer — close (control path).** Release is driven by the materializer's lease GC
  ([`Sweep`](../../../internal/typeset/materializer.go#L384) →
  [`releaseLocked`](../../../internal/typeset/materializer.go#L503)), which emits a release
  `MaterializationEvent`. The watch layer already observes that event to stop the per-type tail;
  the same observer calls `gate.Close(gvr)` and `DeleteType(gvr)` (DG2). Tail-stop precedes
  deletion, so nothing races the `DEL`.
- **Wiring.** [`cmd/main.go`](../../../cmd/main.go) constructs the gate, hands the reader half to
  [`NewAuditHandler`](../../../internal/webhook/audit_handler.go#L112)'s config (alongside
  [`ByTypeQueue`](../../../internal/webhook/audit_handler.go#L79), wired at
  [`cmd/main.go:351`](../../../cmd/main.go#L351)) and the writer half to the watch `Manager`
  (alongside the existing [`AuditLogTrimmer`](../../../cmd/main.go#L256) /
  [`SetLateEventNotifier`](../../../cmd/main.go#L236) wiring).

`DeleteType(group, resource)` is a new [`RedisByTypeStreamQueue`](../../../internal/queue/redis_bytype_queue.go#L130)
method that `DEL`s `…:audit:stream`, `…:audit:late`, `…:audit:idstate` and `SREM`s the base key
from [`…:__index__`](../../../internal/queue/redis_bytype_queue.go#L60) (mirror of
[`ensureIndexed`](../../../internal/queue/redis_bytype_queue.go#L603)); the `…:objects:*` keys are
already released by the demand lifecycle. This is what makes Redis shrink (DG2).

## 8. Interaction with the late-lane guard

These compose; neither subsumes the other:

- **Demand-gating (this doc)** removes (a) all streams for never-claimed types — the bulk of the
  explosion and incidental noise like the `namespaces` warmup entry, and (b) all
  *pre-materialization* events for claimed types — e.g. the flunders `deletecollection`s that
  arrive before flunders' gate opens (DG1's "err early" window is small; anything older than the
  gate is simply not mirrored).
- **The floor / empty-stream guard** (separate proposal) then classifies the *residual* on
  actively-materialized types: events below the checkpoint floor → provably superseded (drop +
  count), RV-less-before-high-water → no-op (drop + count), leaving the late lane holding only
  genuine recent reorders with a `lag` field for triage.

End state: the late lane is empty in a clean run, and any entry in it is actionable — which was the
original goal.

## 9. Boot & HA seam

- **Boot.** On startup the materializer leaf replays durable claimed checkpoints (L-5
  `LoadSyncedCheckpoints`); the watch/driver layer must then `gate.Open` every persisted-claimed
  type **before** the audit handler starts accepting events, so a restart does not drop a window.
  (If the handler starts first, the worst case is a brief gap healed by the first post-boot
  checkpoint — acceptable but avoidable by ordering.)
- **HA.** With multiple pods the wanted-set must be shared, or each pod must derive it
  independently. Out of scope here (consistent with the project's current single-writer posture),
  but the seam is clean: the durable `…:objects:state` already records the claimed GVR set, so a
  shared gate can be sourced from Redis later without changing the hot-path reader. See
  [`ha-improvements.md`](ha-improvements.md).

## 10. Edge cases & failure modes

- **Gate opens slightly late (stale reader).** Bounded by in-process visibility (a map write
  before a network LIST). Worst case a handful of `[L,C]`-adjacent entries are missed and healed
  by the next checkpoint — not silent loss of `rv > C` (DG1 holds because the *write* precedes the
  LIST even if a concurrent reader lags by microseconds).
- **Claim flaps (Dormant→Requested→Dormant).** Open then close + delete; a re-claim re-opens and
  re-LISTs fresh. No stale stream survives (DG2).
- **Followable churn / CRD wobble.** A wobble freezes sync but keeps serving the checkpoint
  (`frozen`); the gate stays **open** while frozen (we still want freshness for a served type) and
  only closes on actual release.
- **Unknown / no-objectRef events.** Never wanted → never mirrored (today they pollute the
  `__unknown__` bucket).
- **Mirror failure is still best-effort.** Unchanged from
  [`mirrorByType`](../../../internal/webhook/audit_handler.go#L499): a gate miss or a Redis error
  never fails the audit request.

## 11. Acceptance criteria

1. A type with no live claim produces **zero** `…:audit:*` keys and no `__index__` membership.
2. For a freshly claimed type, no event with `rv > C` (the first checkpoint revision) is ever
   absent from both the checkpoint and the log (DG1) — covered by an integration test that claims a
   type, races writes around the LIST, and asserts the tail sees every post-`C` write.
3. Releasing a type's last claim deletes its `…:audit:stream`, `…:audit:late`, `…:audit:idstate`
   and removes it from `…:__index__` within one release-sweep interval (DG2).
4. On a clean `task test-e2e` run, the set of `__index__` types equals the set of claimed types
   (no unclaimed streams), and the unclaimed-waste row in §2.1 is **0 types / 0 entries**.
5. No regression in tail freshness for claimed types; full suite + e2e green.

## 12. Code map (read these first)

Everything this change touches, grouped by role. Line anchors are correct as of 2026-06-17
(`poc/redis-copy`); re-grep the symbol if they drift.

**Reader — the audit hot path (gate consultation):** [`internal/webhook/audit_handler.go`](../../../internal/webhook/audit_handler.go)
- `AuditHandlerConfig` struct [L66](../../../internal/webhook/audit_handler.go#L66); `ByTypeQueue` field [L79](../../../internal/webhook/audit_handler.go#L79); `AuditEventQueue` interface [L83](../../../internal/webhook/audit_handler.go#L83) — add the gate alongside.
- `NewAuditHandler` [L112](../../../internal/webhook/audit_handler.go#L112); `processEvent` [L290](../../../internal/webhook/audit_handler.go#L290) (early-gate candidate); `eventToMirror` [L447](../../../internal/webhook/audit_handler.go#L447); `mirrorByType` [L499](../../../internal/webhook/audit_handler.go#L499) (**minimal correct gate point, before `Enqueue`**); `extractGVR` [L580](../../../internal/webhook/audit_handler.go#L580).

**Queue — per-type key schema, writes, and new cleanup:** [`internal/queue/redis_bytype_queue.go`](../../../internal/queue/redis_bytype_queue.go)
- Key suffixes [L57-60](../../../internal/queue/redis_bytype_queue.go#L57) (`:audit:stream` / `:audit:late` / `:audit:idstate` / `:__index__`); `baseKey` [L632](../../../internal/queue/redis_bytype_queue.go#L632) + `typeBaseKey` [L650](../../../internal/queue/redis_bytype_queue.go#L650) — **the gate key must match this** (scale→parent, `__unknown__`).
- `Enqueue` [L199](../../../internal/queue/redis_bytype_queue.go#L199); `ensureIndexed` [L603](../../../internal/queue/redis_bytype_queue.go#L603) (mirror for the new delete); `TrimTypeAuditLog` [L250](../../../internal/queue/redis_bytype_queue.go#L250).
- **New:** `DeleteType(group, resource)` — `DEL` the three keys + `SREM` from `:__index__`.

**Demand leaf — phase machine (stays Redis-free):** [`internal/typeset/materializer.go`](../../../internal/typeset/materializer.go)
- Phases [L59-77](../../../internal/typeset/materializer.go#L59); `typeState` [L116](../../../internal/typeset/materializer.go#L116); `Declare` [L157](../../../internal/typeset/materializer.go#L157); `BeginSync` [L240](../../../internal/typeset/materializer.go#L240); `SyncSucceeded` [L285](../../../internal/typeset/materializer.go#L285); `SyncFailed` [L308](../../../internal/typeset/materializer.go#L308); `Sweep` [L384](../../../internal/typeset/materializer.go#L384); `releaseLocked` [L503](../../../internal/typeset/materializer.go#L503); `Inventory` [L618](../../../internal/typeset/materializer.go#L618). Emits `MaterializationEvent`s to observers — do **not** add a gate dependency here.

**Driver + tail — where the gate is opened/closed:** [`internal/watch/materialization.go`](../../../internal/watch/materialization.go), [`internal/watch/audit_tail.go`](../../../internal/watch/audit_tail.go)
- `runTypeCheckpointSync` [L504](../../../internal/watch/materialization.go#L504) — **`gate.Open(gvr)` between `BeginSync` (L505) and `mirrorTypeObjects` (L508)** (§6); `mirrorTypeObjects` is the LIST.
- `Declare` wiring [L146](../../../internal/watch/materialization.go#L146); `trimTypeAuditLog` [L523](../../../internal/watch/materialization.go#L523); `NudgeTypeResyncForLateEvent` [L70](../../../internal/watch/materialization.go#L70).
- Release observer (stops the tail today) — add `gate.Close` + `DeleteType` there; `runTypeAuditTail` [L134](../../../internal/watch/audit_tail.go#L134), `auditTailAnchor` [L101](../../../internal/watch/audit_tail.go#L101).

**Wiring:** [`cmd/main.go`](../../../cmd/main.go) — `NewRedisByTypeStreamQueue` [L221](../../../cmd/main.go#L221); `SetLateEventNotifier` [L236](../../../cmd/main.go#L236); `AuditLogTrimmer` [L256](../../../cmd/main.go#L256); `NewAuditHandler` [L347](../../../cmd/main.go#L347) + `ByTypeQueue` [L351](../../../cmd/main.go#L351). Construct the gate here; reader half → handler config, writer half → watch `Manager`.

**Tests to model after:** [`internal/queue/redis_bytype_queue_test.go`](../../../internal/queue/redis_bytype_queue_test.go) (miniredis), [`internal/typeset/materializer_test.go`](../../../internal/typeset/materializer_test.go), [`internal/watch/materialization_test.go`](../../../internal/watch/materialization_test.go).

## 13. Implementation order (red-first)

1. **`TypeMirrorGate`** (new, `internal/queue` or `internal/watch`): `Allow(group, resource) bool`,
   `Open(gvr)`, `Close(gvr)`, keyed exactly like `typeBaseKey`. Unit test the concurrency + the
   scale→parent / `__unknown__` key derivation. *Red first.*
2. **DG1 integration test (the load-bearing one):** claim a type, open the gate, race writes
   *around* the LIST, assert the tail sees every `rv > C` write; and assert a *closed* gate drops
   events for an unwanted type. This is acceptance criterion #2 — write it before the wiring so the
   ordering bug (open-after-LIST) is caught.
3. **Reader wiring:** `mirrorByType` consults `Allow` before `Enqueue` (then optionally hoist the
   gate earlier in `processEvent` to skip the body-join).
4. **Open wiring:** `gate.Open(gvr)` in `runTypeCheckpointSync` between `BeginSync` and
   `mirrorTypeObjects` (§6). Idempotent for re-anchors.
5. **`DeleteType` + close wiring:** new queue method (+ test), called with `gate.Close` from the
   release observer after tail-stop.
6. **`cmd/main.go`** construction + boot replay ordering (§9): open gates for persisted-claimed
   types before the handler serves.
7. **e2e acceptance (§11 #1, #3, #4):** `__index__` == claimed set; release deletes keys.
8. `task fmt → generate → manifests → vet → lint → test → test-e2e` (e2e sequential).

## 14. Open questions

- **Gate granularity:** key by `(group, resource)` (matches `baseKey`) — confirmed sufficient;
  subresource folds onto the parent. Revisit only if a non-scale subresource ever becomes a sink.
- **Early vs minimal gating point:** skip the body-join for unwanted types (cheaper) vs gate only
  at `Enqueue` (smaller change). Recommend early once DG1 tests are in place.
- **Eager vs lazy delete on release:** delete synchronously on release vs let a janitor sweep
  orphans. Recommend synchronous delete on release + a periodic `__index__`-vs-claims janitor as a
  backstop for crashes.
