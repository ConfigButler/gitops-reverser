# When a removal should stop waiting for its author

> **design**: open question, needs a decision. The behaviour it describes is BUILT and shipped in
> the [attribution fact stream](../finished/attribution-fact-stream.md); what is open is whether the
> wait it introduced should be bounded more tightly, and how. Index: [`../INDEX.md`](../INDEX.md)
>
> The short version: a removal now waits for evidence about the DELETION rather than accepting the
> object's last write, which fixed a real mis-attribution. The cost is concentrated in one case —
> a removal for which no delete fact will ever arrive — and there it spends the whole grace window
> to return the answer it would have returned immediately.

## The question

Attributing a removal to whoever last EDITED the object names an innocent person as the author of a
deletion they did not perform. Not attributing it at all is honest but useless. Waiting for the
right fact is correct and costs commit latency, on a shard that is single-threaded, so the wait
delays whatever is queued behind it.

The wait is already bounded by `--author-attribution-grace` (3s by default, 10s in the e2e suite).
The question is whether a removal should be able to stop sooner than that, and on what evidence.

## First, what actually happens today

It is worth being precise, because the obvious mental model — try the exact key, then the latest
key, then the collection tiers, each with its own wait — is not what the code does and would be
worse if it did.

There is **one** wait, and **every** tier is evaluated on **every** wake:

1. The resolver registers a waiter for all of this event's candidate keys, then reads the index.
2. Any fact applied for any of those keys wakes it.
3. On each wake it re-reads the index and evaluates the whole tier table in one pass, strongest
   first.
4. It returns as soon as the answer is one it is willing to keep, and otherwise keeps waiting.

So the tiers ARE checked in parallel, in the only sense that matters: no tier's wait blocks
another's, and the first satisfying answer wins whichever tier it comes from. The ordering is a
preference among facts that are simultaneously present, not a sequence of attempts.

What changed with the removal-wait fix is step 4, and only for removals: a match on a per-object
tier whose fact is a WRITE no longer counts as satisfying. It is held as a fallback, and the wait
continues.

## The situations

`E` is the event being attributed. "Evidence" means a fact about the deletion: the object's own
delete fact, or a collection fact covering it.

| # | Situation | Today | Cost |
|---|---|---|---|
| 1 | Removal; the object's own delete fact is already in the index | returns at once, `weak` | none |
| 2 | Removal; a collection fact naming its uid is present | returns at once, `collection_uid` | none — measured at ~70ms |
| 3 | Removal; a collection fact covers its scope | returns at once, `collection_scope` | none |
| 4 | Removal; only a stale WRITE fact is present, and the delete fact arrives during the grace | waits, then returns the deleter | the audit delivery lag, ~1s. **This is the case the fix exists for** |
| 5 | Removal; only a stale WRITE fact is present, and no delete fact ever arrives | waits the FULL grace, then returns the last writer | the whole grace, for an answer available at t=0. **This is the entire cost** |
| 6 | Removal; nothing in the index at all | waits the full grace, then `absent` | unchanged by the fix — this is what the grace has always done |
| 7 | Create or update; its exact fact is present or arrives | returns at once or on arrival | unchanged |
| 8 | Create or update; no fact ever arrives | waits the full grace, then `absent` | unchanged. Aggregated-API writes live here: the audit event carries no uid and no resourceVersion, so nothing joinable is ever published |

Rows 6 and 8 are worth separating from the argument. They are the pre-existing behaviour of the
grace window, they were not introduced by the removal wait, and no option below removes them.

**Situation 5 is the whole problem.** Everything else either costs nothing or is unchanged.

### What situation 5 actually is

A removal for which Kubernetes emits no usable audit event:

- a **graceful pod delete**, which produces no audit event at all;
- a **status-only** change that renders as a removal;
- a type the cluster's **audit policy does not capture**;
- a delete whose audit event is **dropped by the accept gate** (a failed request, a dry run);
- an **audit route that nothing posts to**, which the resolver already warns about separately.

None of these can be distinguished from situation 4 by looking at the object. That is the difficulty:
at the moment of resolution, "the fact is late" and "the fact is never coming" look identical.

## The options

### A. Keep the full grace (status quo)

Every removal waits up to `--author-attribution-grace` for delete evidence.

- **For**: simplest; one knob; matches the design's stated principle of waiting before shipping
  rather than rewriting after. An operator who cares lowers the grace, which bounds this and every
  other wait together.
- **Against**: situation 5 pays the maximum for nothing, and it is not rare — a cluster with pod
  churn generates it continuously. The grace is also the wrong lever: lowering it to cut situation 5
  equally cuts situation 4, which is the case worth waiting for.

### B. A separate, shorter deletion-evidence window

Wait for delete evidence only up to `D`, then take the fallback; `D` defaults to something like the
audit delivery floor rather than the grace.

- **For**: recovers most of the latency; keeps situation 4 whenever the batch is on time; small,
  local change.
- **Against**: another flag whose right value is a property of the *cluster's* audit configuration
  (`--audit-webhook-batch-max-wait`), which this operator cannot see. Set too low it silently
  reintroduces the mis-attribution; set too high it buys nothing. It converts a correctness
  property into a tuning exercise, which is how the original bug will come back.

### C. A per-route watermark: stop when the stream has moved past you

Track, per audit route, the newest fact the follower has applied. If that is newer than this event's
own time plus a skew allowance, then any audit event covering this removal has already been
delivered — so no delete fact is coming, and waiting cannot help.

- **For**: no new flag, and it answers the actual question rather than approximating it with a
  timeout. It uses data already in hand: stream positions are millisecond timestamps, and the index
  already applies entries in order. Situation 5 collapses from "the whole grace" to "as soon as the
  next fact on that route arrives", while situation 4 is untouched, because the watermark cannot pass
  the event until its batch has been processed.
- **Against**: it needs the watermark to be per ROUTE rather than per type, because a quiet type's
  stream never advances and would never release the wait. Route-level is the right granularity —
  one audit POST carries many types — but it means a route with no traffic at all still waits the
  full grace, which is correct but worth stating. Also needs care with clock skew between the API
  server's `stageTimestamp` and the operator's clock.

### D. Wait only when the object was deleted with intent

The deletion-as-intent rule already tells us a `deletionTimestamp` was set, which means a delete
REQUEST happened, which means an audit event should exist.

- **For**: targets the wait at removals that provably came from a request.
- **Against**: it does not separate situation 4 from 5 at all. A graceful pod delete also comes from
  a delete request; it just produces no audit event. The signal is about the object, and the
  question is about the audit pipeline.

### E. Revert the wait; keep only the tier reordering

Removals return on the first match again. Collection deletes whose response body carried uids are
still credited correctly, because that tier now outranks the write fact and both are present at once.

- **For**: no latency cost at all; keeps the measured `collection_uid` win.
- **Against**: puts back the mis-attribution for every removal whose delete fact is merely late —
  which, given audit batching, is the common case rather than the corner. It trades a correctness
  property for latency in the one place the product's core claim lives.

## Recommendation

**C, with A as the fallback if C proves fiddly.** C is the only option that answers the question
being asked — "is a fact still coming?" — rather than guessing at it with a timeout, and it removes
the cost from situation 5 without touching situation 4. It also adds no configuration surface, which
matters because option B's knob would have to be set from a value the operator holds in the *API
server's* configuration, not this one's.

B is the pragmatic compromise and should be considered only if C's watermark turns out to be
unreliable in practice. E is a real option, and the honest way to describe it is: it accepts naming
the wrong person some of the time, in exchange for latency.

## What is not yet measured

The cost of situation 5 was measured over a single e2e run: removals that find evidence resolve in
about 70ms, removals that never do average about 3.1s. What that run does NOT establish:

- **how common situation 5 is in a real cluster.** The e2e suite is not a workload; pod churn,
  namespace teardown, and controller-driven deletes would all shift it.
- **the throughput effect.** The shard is single-threaded, so the number that matters to a user is
  commit latency under a burst of removals, not the mean wait per event.
- **whether the watermark in option C advances often enough** on a quiet route to be worth having.

A before-and-after against the previous behaviour was attempted and discarded rather than reported:
the two runs' populations differed by more than the change, so the comparison would have been a
workload difference wearing a causal claim's clothes. Deciding between these options on that number
would have been deciding on noise.
