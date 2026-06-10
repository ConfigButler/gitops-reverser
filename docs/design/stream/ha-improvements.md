# HA improvements: small deltas to multi-replica readiness

> Status: **design notes** — not scheduled. A running list of the *small* changes that move
> the checkpoint + log reconcile from "leader-elected single replica" to "multi-replica /
> failover-ready". HA is explicitly out of scope for the first cut (parent R10), so nothing
> here is committed work — but the point of this doc is that **none of it is hard**, because
> the agreed designs already carry the seams.
> Captured: 2026-06-10
> Owner: Simon
> Related:
> [demand-driven-type-materialization-lifecycle.md](demand-driven-type-materialization-lifecycle.md) (the demand/claim layer these notes extend — DEC-L3, DEC-L6, DEC-L8, §9),
> [api-source-of-truth-reconcile.md](api-source-of-truth-reconcile.md) (R10 defers HA; the checkpoint is the failover resume point).

## 1. Why HA is close already

The two designs were drawn with the HA seam in mind, so most of the work is *already done by
construction*:

- **The authoritative state is in Redis, not in memory.** The "is this `Synced`, at what rv"
  truth lives in `:objects:state` (DEC-L6 / L8). The in-memory phase is *control-plane only*
  and is rebuilt from Redis on boot — so a second replica that reads the same keyspace sees
  the same world without re-LISTing.
- **The checkpoint is a standing resume point.** A failover replica reconciles against the
  existing `:objects:items @ rv` (parent §3 / R10); it does not need to re-derive the API.
- **Capture is already decoupled from consume.** The always-on audit intake writes the log;
  the periodic LIST writes the checkpoint; the reconcile only *reads* both. Splitting reader
  from writer across replicas does not change that contract.
- **Demand is a self-healing lease, not a teardown protocol** (DEC-L3). A dead replica's
  claims simply age out — there is no handoff message that a failover could lose.

What remains is small and mechanical, captured below.

## 2. Improvement A — claims as Redis leases with a TTL  *(the main one)*

Today the claim table is in-memory (`map[(GitTargetRef, GVR)] -> renewedAt`) and the sweep GCs
a claim by comparing its renewal time to the previous sweep (L-1, DEC-L5). That works for a
single replica. For HA, **move the claim table into Redis with a TTL**, because a claim is a
*lease* and a TTL is exactly the lease primitive.

The load-bearing distinction:

| | Nature | Persistence | On expiry |
|---|---|---|---|
| **Claim** (demand) | ephemeral, self-renewing | Redis key **with TTL** | auto-drops — that is the withdrawal |
| **Materialization** (`:objects:state`: phase/rv/count) | durable resume point | Redis key **without TTL** | never — dropped *explicitly* on release |

What the TTL buys:

1. **Demand becomes shared state.** Once the GitTarget reconcile (which writes `Declare`) and
   the Materializer (which consumes it) can live on different replicas, demand must be shared.
   Redis is already that store; a claim written by one replica is visible to another's sweep
   with no message passing. This answers the **`Declare` transport** open question (parent §9).
2. **Dead-GitTarget / dead-replica cleanup is automatic.** Stop renewing → the claim keys
   expire → demand vanishes. This is DEC-L3's self-healing lease, enforced by the datastore
   instead of computed by the sweep.
3. **It decouples the release grace from the sweep cadence** — closing the *"renewal cadence
   vs sweep interval"* coupling flagged in parent §9. Grace becomes the TTL: fixed, and on
   Redis's clock, so all replicas agree (no per-replica skew).

What the TTL does **not** do — so we do not over-claim it:

- TTL drops the *claim record*, not the *checkpoint*. Redis will not run release logic on
  expiry, and keyspace-expiry notifications are best-effort. So **the periodic sweep stays**;
  it just gets simpler — "does a live claim exist for this `Synced` type?" → re-anchor vs
  release, instead of a timestamp comparison. The ~1h re-anchor timer is unchanged.
- It introduces **one explicit constant** — `TTL = k × GitTarget-reconcile-interval` (k≈2–3)
  so a single slow reconcile never drops a live claim. This is a *conscious* deviation from
  DEC-L5's "no new constant": we trade the implicit sweep-grace for an explicit lease
  duration, which is the right call for HA (explicit beats implicit, and it is the natural
  home for the §9 coupling).

Concrete shape that maps 1:1 onto the in-memory model (`map[GVR]map[Ref]renewedAt`): **one
sorted-set per type**, member = GitTarget ref, score = renewal deadline (`now + TTL`).

```text
Declare        →  ZADD demand:{gvr} <now+TTL> <ref>          # renew = overwrite score
live claim?    →  ZRANGEBYSCORE demand:{gvr} <now> +inf      # members not yet expired
lazy GC        →  ZREMRANGEBYSCORE demand:{gvr} -inf <now>   # prune expired on read
```

The ZSET *is* the in-memory map with the GC inlined into one command, and it keeps the
per-type grouping the sweep already wants. (Per-key `SET claim:{gvr}:{ref} EX <ttl>` also
works and gives true background expiry, at the cost of key sprawl and still needing the poll;
the ZSET is preferred.)

## 3. Improvement B — boot rebuild restores *both* axes

DEC-L6 already rebuilds the materialization phase from `:objects:state` on boot. With
Improvement A, boot also rebuilds **live demand** from the surviving `demand:{gvr}` ZSETs — so
a replica comes up already knowing both *what exists* (checkpoints) and *what is still wanted*
(claims), and resumes without re-LISTing the world.

## 4. Keeping L-1 ready without doing it now

L-1 stays in-process (the agreed "start in-process" of parent §9) — the in-memory `renewedAt`
+ sweep-grace is already the exact behavioural shadow of a TTL'd lease, so there is no rework,
only a store swap. To make that swap a drop-in later, keep the claim table behind a tiny seam:

```go
type claimStore interface {
    renew(ref GitTargetRef, gvrs []schema.GroupVersionResource, now time.Time)
    liveClaimants(gvr schema.GroupVersionResource, now time.Time) []GitTargetRef
    gc(now time.Time)
}
```

The current in-memory map and a future `redisClaimStore` (ZSET + TTL) are then interchangeable
with **no change to the phase machine or the sweep** — which is the whole reason HA here is a
small delta, not a rewrite.

## 5. Out of scope (still genuinely deferred)

These remain non-trivial and are *not* claimed as "easy" — they belong to a dedicated HA plan:

- **Single-writer ownership of the checkpoint LIST and log trim** across replicas (leader per
  type, or a Redis lock), so two replicas do not re-anchor the same type concurrently.
- **Per-type cursor / consumer-group ownership** for the audit stream under failover (parent
  §9 HA bullet).
- **Never-stop audit intake across failover** (parent R4 + R10).
