# Audit log ingestion: strong RV ordering (the late lane has been removed)

> **⚠️ Superseded in part (2026-06-18): the per-type late lane is removed.** §1–§11 below
> describe the original design, in which an out-of-order event was diverted to a per-type
> `:audit:late` stream. That lane is **gone**: a diverted event is now rejected from the main
> stream and written to no stream — it is signalled by its `outcome` on
> `gitopsreverser_audit_events_total`, by the `lateNotify` checkpoint nudge, and (when the opt-in
> `diag_all` firehose is enabled) by its record there. The reason: the lane was nominally
> "diagnostic only" but had become a correctness input (`LookupCommitRequestAuthor` scanned it),
> contradicting principle P-IR4. The current model is the **§12 outcome catalog** and
> [audit-diagnostic-streams-plan.md](audit-diagnostic-streams-plan.md); read those for what the
> code does today. The §5–§8 late-lane mechanics are retained as design history.

> Status: **baseline implemented** (§9; single-pod, no Lua). The deferred improvements
> (§8.1 atomic Lua, §8.2 pre-sorter) remain gated on the §7 investigation. The detailed
> producer/ingestion path for the per-type audit log that
> [api-source-of-truth-reconcile.md](../../finished/api-source-of-truth-reconcile.md) consumes. That doc
> holds the bigger picture (checkpoint + log reconcile); this one holds the ordering,
> key, late-lane, and deferred-improvement detail it deliberately leaves out.
> Captured: 2026-06-10
> Owner: Simon
> Related:
> [api-source-of-truth-reconcile.md](../../finished/api-source-of-truth-reconcile.md) (the consumer / bigger picture),
> [per-resource-type-rv-keyed-streams-experiment.md](../../finished/per-resource-type-rv-keyed-streams-experiment.md) (the write-only prototype this refines),
> [../audit-ingestion-decision-record.md](../audit-ingestion-decision-record.md).

## 1. Scope and one paragraph

This is the **producer** path only: how one mutating audit event becomes one entry in a
per-Kubernetes-type Valkey/Redis Stream, **in resourceVersion order**, and what we do with
the events that do not arrive in order. The reconcile that *reads* the log is a separate
concern ([api-source-of-truth-reconcile.md](../../finished/api-source-of-truth-reconcile.md)). The core
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
| **IR1** | **Per-type key schema.** One stream per Kubernetes API *group + plural resource*. The core group renders as `core`. A `/scale` event keys onto the **parent** type's stream at the parent's post-scale RV with `subresource=scale` as an entry field (DEC-A of [canonical-stream-retirement.md](../../finished/canonical-stream-retirement.md)); any other subresource — none is forwarded by the webhook — folds onto the resource segment with a dot, defensively. **No namespace** and **no apiVersion** in the key — apiVersion is stored as an event field. |
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

**Revised by DEC-A ([canonical-stream-retirement.md](../../finished/canonical-stream-retirement.md), stage
C-A, landed 2026-06-11):** a `/scale` event is a mutation of the parent object, so it lands
in the **parent** type's stream — keyed at the parent's post-scale resourceVersion (the
Scale body carries it), ordered among the parent's other writes — with the entry's
`subresource=scale` field as the discriminator. The sibling `deployments.scale` stream no
longer exists. The translation into a parent `spec.replicas` field patch stays a
consume-side concern: the freshness tail builds the FieldPatch event
([auditChangeFromEntry](../../../internal/queue/redis_bytype_queue.go)) and the splice folds
the replicas into the parent's desired object
([foldScaleEntry](../../../internal/queue/redis_type_splice.go)). Scale is the only
subresource the webhook forwards; a hypothetical other subresource still folds onto the
resource segment with a dot, defensively.

## 6. The late lane (IR3, IR4) — diagnostic by design

When an event's RV is **strictly below** the stream's high-water RV, the strong key (P2)
makes the main-stream `XADD <rv>-*` fail with "equal or smaller" (an *equal* RV does not
fail — Valkey allocates the next `subseq` and it lands in main, §5.1). That rejection is
not an error to paper over — it is the signal we want. (This rejection is what the baseline routes on directly — no
self-comparison; see §9, including the testing note on why miniredis can't stand in for real
Valkey here.) The strictly-older event goes to
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

**INV-1 — first result (2026-06-10, e2e).** First read of a populated keyspace; method,
full numbers, and the per-resource breakdown in
[per-resource-type-rv-keyed-streams-experiment.md](../../finished/per-resource-type-rv-keyed-streams-experiment.md)
§12. Measured on a **clean cluster** (fresh etcd, empty Valkey) so cross-run contamination
is ruled out: late ratio ≈ **3.2%** (49 / 1524 events), **all `older-than-high-water`** (0
`rv-missing-before-high-water`; 0 `non-numeric-rv`) — confirmed genuine within-run reordering.
The RV-gap is **large and heavy-tailed**: min 1, median 152, mean 686, max 2732 — **~52% of
late events are >100 revisions behind** (the earlier non-clean multi-run agreed: 4.3%, median
168, max 4650, 63% >100). The late traffic is dominated by **controller re-apply of old
objects** (k3s bootstrap addons, kubelet service, coredns/metrics-server — gaps >1000) and
**burst patches on test fixtures**; many entries are repeated touches at an unchanged body RV
(benign no-op writes). Per the decision criteria this is **not** the "small, bounded RV-gap"
case that would justify the pre-sorter (§8.2): a sane reorder window catches only the ~10%
with gap ≤10, while the dominant large-gap mass is re-apply delivery the **checkpoint
backstops** anyway (reconcile DEC-5). **Decision on this evidence: do not build the
pre-sorter.** Caveats: the e2e cluster is bursty (fixture churn + k3s bootstrap) — a worst
case for reordering — so re-measure on a steady-state cluster, and assess the >1-pod delta
(§8.1) separately. (Single pod here, so §8.1 is untested by this run.)

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
non-trivial script faithfully. (The baseline already needs that real-Valkey container too: its
strictly-older→late routing depends on the strong-key rejection, which miniredis does not
emulate — see the §9 testing note. Only the baseline's plain `XADD`/`HINCRBY` paths and
error-injection stay on miniredis.)

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
30s. **But first weigh INV-1 below** — the late traffic a window would actually capture, in
the measured run, did not need capturing.

**Measured (INV-1, 2026-06-10): the catchable late traffic is mostly no-op write
amplification, not lost state.** Zooming into the two burst-patch types whose RV-gaps *do*
fit a ~30s window (`core:secrets` and `…icecreamorders` bi-directional fixtures), every late
entry was **`redundant`** — the main stream already held that object at an **equal-or-higher**
RV. The reason is a guarantee, not a guess: **etcd bumps `resourceVersion` on every real
mutation**, so several late events carrying an *identical* body RV (e.g. `bi-bob-order` ×4 all
at `rv=2782`, with main already at 2782) mean **at most one was a real write; the rest are
no-op patches** — a controller re-applying identical content. They get a fresh
`stageTimestamp` but the object's *old* body RV, so they sort "older-than-high-water." A
window would pull these into main, but the splice already has the latest state from main (and
the no-op detection would drop them at the commit boundary anyway). Event-time skew was 0–10s
and delivery latency ~0.01s — so the reorder is a tight concurrent burst, not slow delivery.
**Net: for this workload the window would add latency + per-type buffers to suppress a
diagnostic signal that costs the consumer nothing.** Re-measure on a steady-state cluster
before reconsidering.

**Order by RV — never pre-sort on a time field (a correctness trap).** It is tempting to buffer
and sort by the envelope's server-assigned time (`stage_millis` = `StageTimestamp`, the only
trustworthy clock we have). **Do not.** The same no-op-patch mechanism makes *time order and RV
order actively disagree*: a no-op patch carries a **fresh timestamp** but a **stale body RV**,
so the measured low-RV late events were stamped **0.3–9s *later*** than the higher-RV events
they lost to (`rv=2723` after `rv=2959`). Sorting the buffer by `stage_millis` would place that
stale no-op *after* the real write → the RV-folding splice (reconcile §6) would then apply the
**stale body as last-writer, silently overwriting fresh state** — precisely the corruption the
strong RV key (P2) exists to prevent. The principle: **a time field may bound the buffer
*duration* (how long to hold), but RV — the etcd commit order — must remain the *sort key*.**
No envelope time field is more authoritative than RV for ordering; they are strictly less so.

## 9. Baseline ingestion (the first cut, no Lua)

The first implementation, correct on one pod and acceptable beyond, modifies
[`RedisByTypeStreamQueue`](../../../internal/queue/redis_bytype_queue.go) to:

```text
rv := decimal RV from the event body, or "" (IR6 extraction order)
key, lateKey, idstate := baseKey + ":audit:stream", + ":audit:late", + ":audit:idstate"

if rv numeric:
    id, err := XADD key <rv>-* fields(rvPresent=true, placement=resource-version)
    if err == "equal or smaller":                  -- strong key rejected a strictly-older RV
        topRV := first(XINFO STREAM key .last-generated-id)   -- authoritative high-water
        XADD lateKey * fields(reason=older-than-high-water, rv, lastRV=topRV) ; HINCRBY idstate lateCount 1
    else:
        HINCRBY idstate mainCount 1
        HSET idstate lastRV rv lastStreamID id lastEventAt now   -- best-effort observability cache
elif rv == "":                                     -- RV-less (IR5)
    topRV := first(XINFO STREAM key .last-generated-id)         -- authoritative high-water
    if topRV != "":
        XADD key <topRV>-* fields(rvPresent=false, placement=attached-to-last-rv) ; HINCRBY idstate rvMissingCount 1
    else:
        XADD lateKey * fields(reason=rv-missing-before-high-water) ; HINCRBY idstate lateCount 1
else:                                              -- non-numeric (IR6 / §5.3)
    XADD lateKey * fields(reason=non-numeric-rv) ; HINCRBY idstate lateCount 1
```

The main-vs-late routing is **atomic per `XADD` without Lua**: the strong key makes
`XADD <rv>-*` itself the arbiter (Valkey's native 64-bit ID compare), so we never compare RVs
ourselves and never touch a lossy `tonumber` (IR6). High-water reads — for the RV-less attach
mark and the late-lane diagnostic `lastRV` — use `XINFO STREAM`'s `last-generated-id`, which is
authoritative and survives trimming (§10), **not** the `idstate` cache. `idstate` is therefore
written but never *read for routing*; it is best-effort observability (IR7) and the non-atomic
counters/cache are self-correcting. Folding read-decide-write into one atomic step is exactly
what Lua (§8.1) adds as an *improvement*, not a prerequisite.

> **Testing note (miniredis vs. real Valkey).** The strictly-older→late branch hinges on Valkey
> rejecting a below-high-water `<rv>-*`. **miniredis does not emulate this** — it silently
> *clamps* a partial `<ms>-*` ID whose leading component is below the top (it returns
> `<top>-<seq+1>` instead of the "equal or smaller" error), and only rejects *full* explicit
> IDs. So the ordering/late-routing tests run against a **real Valkey testcontainer**
> ([redis_bytype_queue_test.go](../../../internal/queue/redis_bytype_queue_test.go), one shared
> container for the package); only error-injection and the MaxLen-wiring test stay on miniredis.
> This is the same real-Valkey discipline §8.1 already requires for the deferred Lua.

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

## 12. Event outcomes (the disposition catalog)

Every audit event that is successfully decoded, converted, and validated ends in **exactly one
ingestion outcome**, recorded once on `gitopsreverser_audit_events_total{outcome, category, group,
version, resource, verb}` by the layer that terminates it (the webhook handler for pre-queue
outcomes, the per-type queue inside `Enqueue` for queue-side ones). This is the canonical list of
"where, and why, an event does or does not reach the per-type log." The Git-materialization fate
(read-time tail/splice skips) is deliberately **not** an outcome — those are repeatable per
(GitTarget, GVR) and the checkpoint backstops them. See the design rationale and the diagnostic
streams in [audit-diagnostic-streams-plan.md](audit-diagnostic-streams-plan.md).

`category` is a coarse, alert-friendly grouping derivable from `outcome`: **stored** = in the log;
**held** = kept for later; **dropped** = not in the log (the "Recovered by" column says whether
that is safe); **error** = should never happen (the e2e invariant gates on `category="error" == 0`).

| Outcome | Category | Recorded at | Why | Recovered by |
| --- | --- | --- | --- | --- |
| `queued` | stored | queue `Enqueue` | written to the type stream (numeric in-order, or RV-less pinned to the high-water; a merged official body is also just `queued`) | — |
| `parked` | held | handler (joiner) | an additional body kept until its official event arrives | re-emitted on match |
| `not_needed` | dropped | handler (`mirrorGateAllows`) | type not claimed ∩ followable (demand gate) | — (re-anchored on a later claim) |
| `nil_event` | dropped | handler (`classifyAuditIngress`) | no event | — |
| `stage` | dropped | handler | not the ResponseComplete stage | — |
| `read_only_or_unknown_verb` | dropped | handler | get/list/watch (or an unmapped verb) — no mutation | — |
| `failed_request` | dropped | handler | mutation never reached etcd (responseStatus ≥ 300) | — |
| `dry_run` | dropped | handler | a dry-run request; nothing persisted | — |
| `unchanged_resource_version` | dropped | handler | no state change | — |
| `malformed_additional` | dropped | handler | an additional body without a usable body; the official drives | official event |
| `non_scale_subresource` | dropped | handler (`shouldForwardSubresource`) | only `/scale` is mirrored; status/exec/log/… dropped before Redis | — |
| `shallow_dropped` | dropped | handler (joiner) | identity-shallow official, no body, not deletable | next checkpoint |
| `rvless_empty_highwater` | dropped | queue `Enqueue` | an RV-less event before any high-water exists → no-op | next checkpoint |
| `older_than_high_water` | dropped | queue `Enqueue` | RV below the stream high-water (external batch-delivery reorder) | next checkpoint + `lateNotify` nudge |
| `non_numeric_rv` | dropped | queue `Enqueue` | RV not a uint64 (aggregated apiservers) | next checkpoint |
| `write_error` | **error** | queue `Enqueue` | redis/enqueue failure — the event never reached the log | retry / next checkpoint |

### Divert handling and the diagnostic stream

A diverted event (`older_than_high_water` / `non_numeric_rv`) is rejected from the main stream and
written to **no stream**. There is no per-type late lane (§5–§6 below describe the original
late-lane design, now removed — see the banner at the top). A divert is signalled by its `outcome`
on the counter, by the `lateNotify` nudge (which pulls the type's next checkpoint forward — the
correctness backstop), and by its full record in `diag_all` when that firehose is enabled.

- **`diag_all`** (`<prefix>:diag_all`, **opt-in** via `--audit-bytype-diag`) — one annotated record
  (`record_kind=ingestion`) per event that reaches the queue: the entry payload plus its `outcome`
  and `category`, including the full payload of every divert. The firehose for deep
  ingestion/ordering investigation; bounded by `--audit-bytype-diag-max-len`.

Outcomes inherent to delivery timing (`older_than_high_water`) or internal failure
(`shallow_dropped`, `write_error`) are proven by unit tests; the externally-triggerable ones
(`dry_run`, `unchanged_resource_version`, `non_scale_subresource`, …) by unit tests and the e2e
suite, which also prints the per-outcome breakdown in its teardown.
