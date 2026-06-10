# Audit log ingestion: strong RV ordering + a diagnostic late lane

> Status: **baseline implemented** (§9; single-pod, no Lua). The deferred improvements
> (§8.1 atomic Lua, §8.2 pre-sorter) remain gated on the §7 investigation. The detailed
> producer/ingestion path for the per-type audit log that
> [api-source-of-truth-reconcile.md](api-source-of-truth-reconcile.md) consumes. That doc
> holds the bigger picture (checkpoint + log reconcile); this one holds the ordering,
> key, late-lane, and deferred-improvement detail it deliberately leaves out.
> Captured: 2026-06-10
> Owner: Simon
> Related:
> [api-source-of-truth-reconcile.md](api-source-of-truth-reconcile.md) (the consumer / bigger picture),
> [per-resource-type-rv-keyed-streams-experiment.md](per-resource-type-rv-keyed-streams-experiment.md) (the write-only prototype this refines),
> [../audit-ingestion-decision-record.md](../audit-ingestion-decision-record.md).

## 1. Scope and one paragraph

This is the **producer** path only: how one mutating audit event becomes one entry in a
per-Kubernetes-type Valkey/Redis Stream, **in resourceVersion order**, and what we do with
the events that do not arrive in order. The reconcile that *reads* the log is a separate
concern ([api-source-of-truth-reconcile.md](api-source-of-truth-reconcile.md)). The core
stance: the main stream is **strictly RV-ordered and we never knowingly insert an
out-of-order event into it**; anything that would break the order is diverted to a
**purely diagnostic** late lane whose only job is to let us *see* that reordering is
happening. We build the two obvious mitigations — atomic Lua ingestion and a pre-sort
buffer — **only once the late lane proves we need them**, not before.

## 2. Principles

These are convictions, not just choices; everything below obeys them.

- **P1 — A clean, strictly-ordered main stream is worth more than the convenience of
  inserting a known-late event.** We never trade ordering away to avoid sending an event to
  the late lane. Ordering is the property the whole reconcile leans on (replay from a
  checkpoint); we protect it.
- **P2 — The strong key is a feature, not an obstacle to work around.** Valkey rejecting a
  stream ID that is not strictly greater than the current top is exactly the guardrail we
  want. It makes "never insert out of order" impossible to get wrong: we *cannot* push a
  below-high-water event into the main stream even by mistake.
- **P3 — Observe before building.** The late lane exists to *measure*. We do not build the
  pre-sorter or the Lua ingestion script on speculation; we build each only when the
  diagnostics (§8) show a real, quantified need (§7).

## 3. Requirements

| # | Requirement |
|---|---|
| **IR1** | **Per-type key schema.** One stream per Kubernetes API *group + plural resource*. The core group renders as `core`; a subresource is folded onto the resource segment with a dot (`deployments.scale`). **No namespace** and **no apiVersion** in the key — apiVersion is stored as an event field. |
| **IR2** | **Strictly RV-ordered main stream.** Main-stream IDs are `<resourceVersion>-<subseq>`. Write with `XADD <key> <rv>-* …` so Valkey allocates the `subseq` atomically and monotonically within an RV. |
| **IR3** | **Never knowingly insert out of order.** An event whose RV is **strictly below** the stream's high-water RV is never forced into the main stream (P1/P2). An event whose RV is **equal** to the high-water RV *does* go to main — the `subseq` disambiguates it — so only strictly-older events divert. |
| **IR4** | **Diagnostic late lane.** Such an event is diverted to `:audit:late`, with full context, **never dropped and never reordered into main**. The late lane is observability only — no consumer treats it as reconcile input. |
| **IR5** | **RV-less events are explicit.** Events with no usable object RV (some deletes, collection verbs, `Status` bodies) are marked `rvPresent=false` and attached to the high-water mark as a **declared policy placement**, not a claimed RV. Their correctness is backstopped by the checkpoint, not the log (reconcile doc, DEC-5). |
| **IR6** | **Correct RV comparison.** RV is treated as a non-negative decimal integer (the etcd revision; fits uint64). Compare as a **decimal string** (strip leading zeroes → longer is larger → equal length compares lexically), never via Lua `tonumber`. Non-numeric / non-etcd RVs are out of the strong-ordering domain → explicit policy (§6.3), never a crash. |
| **IR7** | **Observable.** Per-type counters — `mainCount`, `lateCount`, `rvMissingCount` — plus `lastRV` / `lastStreamID` / `lastEventAt`. These are the instrument for the investigation (§7) and for gating the deferred improvements (§8). |
| **IR8** | **Best-effort, non-blocking.** Mirroring never fails the audit request, never perturbs the canonical write, and swallows-and-counts its own errors (inherited from the prototype, experiment doc §7). |
| **IR9** | **Simple at one pod; safe enough beyond.** The baseline ingestion (§5) needs no Lua and is correct on one pod. Lua atomic ingestion and the pre-sorter are deferred improvements (§8), each gated on evidence. |

## 4. Key schema

`<base>` = `gitops-reverser:<group-or-core>:<resource>[.<subresource>]`.

| key | type | contents |
|---|---|---|
| `<base>:audit:stream` | stream | the ordered log — IDs `<rv>-<subseq>` (IR2) |
| `<base>:audit:late` | stream | the diagnostic late lane — Valkey-assigned IDs (`*`) |
| `<base>:audit:idstate` | hash | `lastRV`, `lastStreamID`, `lastEventAt`, `mainCount`, `lateCount`, `rvMissingCount` |
| `gitops-reverser:__index__` | set | every per-type base key, for enumeration without `SCAN` |

Examples:

```text
gitops-reverser:apps:deployments:audit:stream
gitops-reverser:apps:deployments:audit:late
gitops-reverser:apps:deployments:audit:idstate
gitops-reverser:core:configmaps:audit:stream
gitops-reverser:gitops.koudijs.dev:gittargets:audit:stream
```

This is the prototype's schema realized: `:audit:late` and `:audit:state` were already
**reserved** in the experiment doc; `:audit:idstate` is the concrete form of the reserved
state key. The subresource folding and `__index__` set already exist in
[redis_bytype_queue.go](../../../internal/queue/redis_bytype_queue.go) and are kept.

## 5. The stream ID design

### 5.1 `<rv>-*`: resourceVersion is the primary order

Today the prototype keys the log `<stage_millis>-<rv>`
([streamIDCandidates](../../../internal/queue/redis_bytype_queue.go#L187)), so it is
primarily ordered by *webhook delivery time*. That is the wrong order for replay. We
re-key to **`<rv>-<subseq>`**, written as `XADD <key> <rv>-* …`:

- The **`rv`** (the object body's `metadata.resourceVersion`, the etcd revision the write
  committed at) is the primary ordering component — the correct order for one type.
- The **`subseq`** is allocated by Valkey (`-*`). It disambiguates several events that map
  to the same RV (a `deletecollection` can repeat a collection RV) **atomically and
  monotonically**, with no counter state for us to maintain and no multi-writer
  coordination. This is strictly simpler than tracking our own subsequence.

The payoff is exact replay from a checkpoint: `XRANGE <key> (<R> +` returns precisely the
events after checkpoint revision `R`, because an event's RV is fixed at commit time
regardless of when the webhook delivered it.

`stage_millis` stays as a **field** (human scanning, latency metrics), just not as the
ordering key.

### 5.2 RV comparison: decimal string, not `tonumber` (IR6)

A Valkey stream ID component is a **uint64**, so writing `<rv>-*` already requires `rv` to
be a non-negative integer ≤ 2⁶⁴−1 — which every etcd revision is (etcd revisions are
int64). So the *encoding* is fine. The subtlety is the *comparison*: Valkey's embedded Lua
`tonumber` is a 53-bit IEEE double, so it silently mis-compares RVs in the (2⁵³, 2⁶⁴)
range that Valkey itself stores and orders correctly in the ID. We therefore compare RVs
as **decimal strings** (strip leading zeroes; longer string is larger; equal length
compares lexically) anywhere we compare them ourselves, so our comparison matches Valkey's
native 64-bit ID ordering. In practice etcd revisions stay far below 2⁵³, so this is
defensive, not load-bearing — but it is cheap and removes a foot-gun.

> Note the precise reason: it is **not** that RV might exceed int64 (it cannot, and if it
> did it could not be a stream ID at all). It is that *Lua's* number is 53-bit while the
> *stream ID* is 64-bit.

### 5.3 Non-numeric RV: out of the strong-ordering domain

A few API surfaces (aggregated apiservers such as `metrics.k8s.io`) can emit non-numeric
or non-monotonic resourceVersions. Such an RV **cannot** be a stream ID component. These
types are outside the strong-ordering domain by definition; we do not pretend otherwise.
Policy: divert their events to the late lane with `reason=non-numeric-rv` (so they are
still observable) and do not attempt to order them. These are almost never resources a
GitTarget mirrors, so the impact is negligible — but it is a **defined branch, never a
crash** (IR6).

### 5.4 Subresources (IR1)

A subresource event keys onto the parent type's segment with a dot (`deployments.scale`),
exactly as the prototype already does. The `/scale` translation into a parent field-patch
is a reconcile-side concern (already implemented in
[routeScaleFieldPatch](../../../internal/queue/redis_audit_consumer.go#L586)); the log just
records the event under the folded key.

## 6. The late lane (IR3, IR4) — diagnostic by design

When an event's RV is **strictly below** the stream's high-water RV, the strong key (P2)
makes the main-stream `XADD <rv>-*` fail with "equal or smaller" (an *equal* RV does not
fail — Valkey allocates the next `subseq` and it lands in main, §5.1). That rejection is
not an error to paper over — it is the signal we want. (In the shipped baseline the strictly-older case is decided up
front by the §9 decimal-string pre-check; the rejection is kept as the backstop for the
racing-writer case — and because miniredis clamps rather than rejects a partial `<rv>-*`,
see §9.) The strictly-older event goes to
`<base>:audit:late` with `XADD * …` and a full context payload:

```text
reason   = older-than-high-water | rv-missing-before-high-water | non-numeric-rv
rv       = <event-rv>            lastRV = <current high-water>
group resource apiVersion namespace name uid
auditID stage verb user observedAt responseCode payload
```

Two things the late lane is **not**:

- **Not a reconcile input.** The reconcile reads only `<base>:audit:stream` from the
  checkpoint cursor. A late event is, by construction, older than the high-water mark; the
  next checkpoint LIST already incorporates its effect (the object's later state, or its
  absence). So the late lane never needs to drive a commit — the checkpoint backstops it.
- **Not a silent drop.** Every diverted event is fully recorded. The point is to **see**
  it.

This is the whole justification for the late lane: it makes out-of-order delivery a
**visible, countable** phenomenon. As long as it stays near-empty, we know our simple
in-order ingestion is sufficient and we build nothing. The moment it fills, it tells us —
with per-type counts and RV-gap data — exactly what to build and how to size it (§7, §8).

## 7. Investigation: do we actually need a pre-sorter (or atomic ingestion)?

**INV-1 — How often does audit delivery arrive out of RV order, and how far back?**

- **Hypothesis.** With the strong key, slightly-out-of-order webhook delivery will divert
  some events to the late lane. We do **not** yet know the rate, and we will not build the
  pre-sorter or Lua ingestion until the late lane proves the rate is non-trivial. The
  default expectation is that on a single pod, out-of-order delivery is rare and the
  checkpoint backstop makes it harmless.
- **Method.** Run the baseline ingestion (§5, one pod, no pre-sorter, no Lua) on
  representative clusters. Leave it for a sustained window that includes load events
  (rollouts, mass deletes, `deletecollection`, CRD churn). Instrument the late lane.
- **Measurements (per type).**
  - `lateCount : mainCount` ratio — how lopsided is it, and for which types?
  - RV-gap distribution `lastRV − rv` for late events — are they 1–2 revisions behind
    (trivial skew) or far behind (real reordering)?
  - Verb / operation breakdown — is it dominated by deletes (RV-less) vs. genuine updates?
  - Correlation with load events and with webhook delivery latency.
  - **Later, the delta when scaling past one pod** — how much does concurrent ingestion
    *add* to the late rate (this is the signal for atomic ingestion, §8.1).
- **Decision criteria.**
  - Late ratio ~0 and RV-gaps tiny → **do nothing**; the checkpoint backstop suffices.
  - Non-trivial late ratio with small, bounded RV-gaps → **build the pre-sorter** (§8.2),
    sized to the observed reorder window.
  - Late rate that appears or jumps **only when running >1 pod** → the cause is ingestion
    concurrency → **build the Lua atomic ingestion** (§8.1), with or without the pre-sorter.
- **Output.** A recorded measurement that *justifies or refuses* each deferred improvement.
  No improvement ships without this evidence.

## 8. Deferred improvements (build only when §7 proves the need)

Both are listed here so the design is complete and the triggers are explicit. Neither is in
the first cut. We currently run **one pod**, so neither is needed yet.

### 8.1 Atomic Lua / Redis-Function ingestion

**What it adds.** Today's baseline (§5) routes main-vs-late atomically *per `XADD`* (the
strong key does the work) and keeps `idstate` as benign best-effort counters/cache — which
is correct on one pod and tolerable on many (the only races are best-effort counters and a
slightly-stale `lastRV` for RV-less attachment). A Lua script folds *read-decide-write* into
one atomic step, giving: exact counters tied to the routing decision, a non-racy `lastRV`
for RV-less placement, and a single round-trip. Sketch of the atomic decision:

```text
-- KEYS: stream, late, idstate ; ARGV: rv, rvPresent, fields...
lastRV = HGET idstate lastRV
if rvPresent and rv is numeric:
    if decimalCompare(rv, lastRV) >= 0:          -- at or above high-water
        id = XADD stream <rv>-* fields(placement=resource-version)
        HSET idstate lastRV rv lastStreamID id lastEventAt now ; HINCRBY mainCount
        return {main, id}
    else:                                         -- below high-water → late
        XADD late * fields(reason=older-than-high-water, rv, lastRV)
        HINCRBY lateCount ; return {late}
elif not rvPresent:
    if lastRV exists:
        XADD stream <lastRV>-* fields(rvPresent=false, placement=attached-to-last-rv)
        HINCRBY rvMissingCount ; return {main, ...}
    else: XADD late * fields(reason=rv-missing-before-high-water) ; return {late}
else: XADD late * fields(reason=non-numeric-rv) ; return {late}
```

**When to build.** We run >1 pod **and** §7 shows concurrency is materially inflating the
late rate or corrupting counters / RV-less placement. **Tested against a real Valkey
container**, not miniredis — miniredis's `EVAL` coverage is partial and will not exercise a
non-trivial script faithfully (the baseline §5 logic *can* stay on miniredis, since it is
plain `XADD`/`HINCRBY`).

### 8.2 Pre-sort buffer

**What it adds.** A short, bounded **reorder window** (≈30s, tunable) in front of the main
stream: hold each type's incoming events briefly, order them by RV, and forward them in
order, so a slightly-late event makes it *into* the main stream instead of the late lane.
This is the principled way to reduce the late rate **without violating P1** — we are not
inserting a known-out-of-order event; we are delaying *publication* until we are confident
of the order within the window.

**Cost (why it is deferred).** It adds end-to-end latency equal to the window, and it adds
in-flight state (a per-type buffer) that must itself be bounded and crash-safe. We pay that
only when the late lane shows the freshness loss is real and worth the latency.

**When to build.** §7 shows a non-trivial late ratio with RV-gaps that fit a small window.
Size the window to the observed gap distribution (e.g. p99 reorder delay), not a guessed
30s.

## 9. Baseline ingestion (the first cut, no Lua)

The first implementation, correct on one pod and acceptable beyond, modifies
[`RedisByTypeStreamQueue`](../../../internal/queue/redis_bytype_queue.go) to:

```text
rv := decimal RV from the event body, or "" (IR6 extraction order)
key, lateKey, idstate := baseKey + ":audit:stream", + ":audit:late", + ":audit:idstate"

if rv numeric:
    lastRV := HGET idstate lastRV                        -- cached high-water (IR7)
    if lastRV != "" and decimalCompare(rv, lastRV) < 0:  -- strictly older (IR6 — never tonumber)
        XADD lateKey * fields(reason=older-than-high-water, rv, lastRV) ; HINCRBY idstate lateCount 1
    else:                                                 -- at or above high-water → main
        id, err := XADD key <rv>-* fields(rvPresent=true, placement=resource-version)
        if err == "equal or smaller":                     -- backstop: a racing writer moved the top (P2)
            XADD lateKey * fields(reason=older-than-high-water, rv, lastRV) ; HINCRBY idstate lateCount 1
        else:
            HINCRBY idstate mainCount 1
            HSET idstate lastRV rv lastStreamID id lastEventAt now   -- best-effort cache
elif rv == "":                                            -- RV-less (IR5)
    lastRV := HGET idstate lastRV
    if lastRV != "":
        XADD key <lastRV>-* fields(rvPresent=false, placement=attached-to-last-rv) ; HINCRBY idstate rvMissingCount 1
    else:
        XADD lateKey * fields(reason=rv-missing-before-high-water) ; HINCRBY idstate lateCount 1
else:                                                     -- non-numeric (IR6 / §5.3)
    XADD lateKey * fields(reason=non-numeric-rv) ; HINCRBY idstate lateCount 1
```

The main-vs-late decision is **ours**, made by an explicit decimal-string compare of `rv`
against the cached high-water (IR6, §5.2 — never `tonumber`). We do **not** rely on the
`XADD <rv>-*` rejection as the *sole* arbiter, for two reasons: (1) it makes the routing
deterministic and unit-testable without a live Valkey, and (2) **miniredis silently clamps a
partial `<rv>-*` ID whose leading component is below the top** (it returns `<top>-<seq+1>`
rather than the "equal or smaller" rejection real Valkey gives), so a rejection-only baseline
would mis-route strictly-older events under the test double. The strong key is kept as the
**backstop** for the one case the pre-check cannot catch alone: a concurrent writer advancing
the top between our `HGET` and our `XADD` (P2). The cache can only *lag* the true top (it is
written after a main write), so the pre-check never wrongly diverts a current event to late —
a lagging cache merely lets an out-of-order event reach the `XADD`, where the strong key
rejects it. The non-atomic parts (`idstate` counters/cache) are best-effort and
self-correcting; folding read-decide-write into one atomic step is exactly what Lua (§8.1)
adds as an *improvement*, not a prerequisite.

## 10. Operational notes & failure modes

- **Redis/Valkey unreachable** → best-effort: log first error per stream, count, never fail
  the audit request (IR8).
- **Late lane growth** → it is diagnostic, so bound it with an approximate `MaxLen`; trimming
  old late entries loses nothing the counters have not already recorded.
- **Main stream trim** → bound with `MaxLen` **but never below the oldest live checkpoint
  cursor**, or replay breaks. Trimming is tied to the checkpoint re-anchor (reconcile doc).
- **`idstate` divergence** → the stream's true top (`XINFO STREAM` last-generated-id, whose
  first component *is* `lastRV`) is authoritative; `idstate` is a cache and may be rebuilt
  from it. Only ever write `idstate` from the ingestion path.
- **Counters → metrics** → surface `mainCount` / `lateCount` / `rvMissingCount` per type as
  telemetry so the §7 investigation (and ongoing health) is a dashboard, not a manual
  `XLEN`.

## 11. Acceptance criteria

- Events with increasing RVs land in the main stream as `<rv>-<subseq>`; multiple events at
  the same RV get unique IDs via Valkey's `-*` subseq.
- An event whose RV **equals** the high-water RV lands in the main stream with a fresh
  `subseq`; an event whose RV is **strictly below** it is **never** in the main stream — it
  is in the late lane, fully recorded.
- RV-less events are marked `rvPresent=false` and attached to the high-water mark (or sent
  to late before any high-water exists), and are corrected by the next checkpoint.
- Non-numeric RVs are diverted to late with a clear reason, never crashing.
- RV comparison never uses `tonumber`; large-RV-string comparison is correct.
- The late lane is observable per type and is the documented trigger for §8.1 and §8.2.
- Baseline ingestion needs no Lua and is correct on one pod.
