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
| 2 | Removal; a collection fact naming its uid is present | returns at once, `deletecollection_body_uid` | none — measured at ~70ms |
| 3 | Removal; a collection fact covers its scope | returns at once, `deletecollection_scope` | none |
| 4 | Removal; only a stale WRITE fact is present, and the delete fact arrives during the grace | waits, then returns the deleter | the audit delivery lag, ~1s. **This is the case the fix exists for** |
| 5 | Removal; only a stale WRITE fact is present, and no delete fact ever arrives | waits the FULL grace, then returns the last writer | the whole grace, for an answer available at t=0. **This is the entire cost** |
| 6 | Removal; nothing in the index at all | waits the full grace, then `absent` | unchanged by the fix — this is what the grace has always done |
| 7 | Create or update; its exact fact is present or arrives | returns at once or on arrival | unchanged |
| 8 | Create or update; no fact ever arrives | waits the full grace, then `absent` | unchanged. Aggregated-API writes live here: the audit event carries no uid and no resourceVersion, so nothing joinable is ever published |

Rows 6 and 8 are worth separating from the argument. They are the pre-existing behaviour of the
grace window, they were not introduced by the removal wait, and no option below removes them.

**Situation 5 is the whole problem.** Everything else either costs nothing or is unchanged.

### What situation 5 actually is

A removal for which no usable audit event reaches us. The dominant cause is not Kubernetes, it is
**the cluster's audit policy** — and that is a much better position to be in, because a policy is a
file someone wrote on purpose.

- **A type the audit policy drops.** This repository's own recommended policy
  ([`policy.yaml`](../../test/e2e/cluster/audit/policy.yaml)) drops `events`, `endpoints`, `nodes`,
  `pods`, `bindings`, `componentstatuses` and every `*/status` as runtime noise. Watch any of those
  and EVERY removal is situation 5, permanently.
- **An audit route nothing posts to** — the resolver already warns about this one separately.
- A delete whose audit event the **accept gate** drops: a failed request, a dry run.
- A **status-only** change that renders as a removal.

> **A correction, because this document said otherwise and so did two others.** A graceful pod
> delete was cited as producing "no audit event at all". It does not: the `DELETE` request is
> audited like any other, and under the deletion-as-intent rule that request is precisely the fact
> the join wants. Pods are missing from OUR facts because the policy above drops them. The
> distinction matters — "Kubernetes cannot tell us" is a wall, while "the policy did not ask" is a
> setting, and a knowable one.

None of these can be distinguished from situation 4 *by looking at the object*. That is the
difficulty: at the moment of resolution, "the fact is late" and "the fact is never coming" look
identical. But they are very distinguishable **by looking at history** — a type whose audit policy
drops it never produces a fact, not once, ever. That is what option F exploits.

### Aggregated types are a special case, and worse than row 8 suggests

Row 8 says an aggregated-API write never resolves. Tracing it through the code, the reason is
sharper than "the body is empty", and it changes what could be done about it.

The kube-apiserver proxies an aggregated request and never sees the object, so the audit event's
`objectRef` carries **no uid and no resourceVersion**, and no name either unless the request URL
supplies one. This is measured, not inferred: corpus `flunder/aggregated-api-write` for the create,
`flunder/aggregated-api-delete` for the single delete, and
`flunder/aggregated-api-deletecollection` for the collection. What each verb then does:

| Verb on an aggregated type | `objectRef` carries | What happened to the fact | Now |
|---|---|---|---|
| create | nothing: no name, uid or rv | **No fact is published at all.** `AuthorFactFromEvent` rejects it at the "no resolvable name" gate | unchanged, and now COUNTED as `no_attribution_fact` on `audit_events_total` |
| update / patch | the NAME, from the URL path | A fact was published and then **dropped by the index**: with no uid and no rv it could never be joined | **resolves**, on the name tier |
| single delete | the NAME, from the URL path | Same: published, then dropped as unjoinable | **resolves**, on the name tier |
| deletecollection | namespace and selector | **Works.** A collection fact joins by SCOPE, which needs no uid | unchanged, and now two-tiered (`deletecollection_body_uid` / `deletecollection_scope`) |

So the observation that creates and updates "measure" while deletes do not was half right, and the
truth was less flattering: for an aggregated type, only the COLLECTION delete was attributable at
all. Everything else either produced no fact or produced one that was discarded on arrival.

**The tier that fixes two of those rows has shipped.** An update or a single delete carries the
object's NAME, and the watch event carries name and namespace too, so a
`(route, group/resource, namespace, name)` tier joins them. It is weaker than uid — a name is reused
after a delete-and-recreate, where a uid is not — so it sits below every other per-object tier, with
the same care the scope tier gets. It is `tier="name"` on `attribution_resolutions_total`, and
`fact_index.go` documents where it ranks and why.

Worth recording honestly: the fact's `name` field was REMOVED from the wire during this work, on the
correct observation that no tier read it. That was true, and it is exactly the field this tier
needed back — it was restored to build it. "No code reads it" and "nothing could ever read it" are
different claims, and only the second one justifies deleting a field.

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
- **Against**: it does not separate situation 4 from 5 at all. A delete of an audit-excluded type
  also comes from a delete request; the request simply is not recorded. The signal is about the
  object, and the question is about the audit pipeline.

### E. Revert the wait; keep only the tier reordering

Removals return on the first match again. Collection deletes whose response body carried uids are
still credited correctly, because that tier now outranks the write fact and both are present at once.

- **For**: no latency cost at all; keeps the measured `deletecollection_body_uid` win.
- **Against**: puts back the mis-attribution for every removal whose delete fact is merely late —
  which, given audit batching, is the common case rather than the corner. It trades a correctness
  property for latency in the one place the product's core claim lives.

### F. A per-(route, type) circuit breaker: stop waiting for a fact that has never once arrived

The resolver already tracks, per audit route, whether attribution has EVER resolved, and warns when
a route has produced a long run of unresolved events. Extend that to `(route, group/resource)` and
use it to decide the wait: a type that has never once produced a fact on this route is not going to
start, so a removal on it should take its fallback immediately.

This is the [circuit breaker](attribution-wait-poll-vs-push.md#option-c-circuit-break-a-route-that-has-never-resolved-anything)
the earlier record already proposed, applied to the case that turns out to need it.

- **For**: it targets situation 5 exactly, and for the dominant cause — a type the audit policy
  drops — it is not a heuristic but a fact about the configuration: the type is excluded, so it
  never publishes, so the counter never moves. The learning is cheap, the mechanism already half
  exists, and the failure mode is safe in the right direction: a type that HAS produced facts keeps
  waiting, so situation 4 is untouched. It also makes a real misconfiguration visible — "you are
  watching a type your audit policy excludes" is exactly the sort of thing an operator wants told.
- **Against**: it needs a warm-up rule, because "never resolved" is also true of a type that has
  simply not been written to yet, and getting that wrong would skip the wait for a type that was
  about to work. It is per process, so it relearns on restart. And it does nothing for the
  intermittent case, where a type produces facts sometimes — for that, C is still the answer.

## Recommendation

**F first, then C.** They are complementary rather than alternatives: F removes the permanent case
(a watched type that is never audited) using a signal that is already almost there, and C removes
the transient case (a fact that would have come by now) using data already in the index. F is the
bigger win, because the audit-policy exclusion makes situation 5 *permanent* for a whole type rather
than occasional, and it also surfaces a misconfiguration nobody currently learns about.

If only one ships, take F.

**C, with A as the fallback if C proves fiddly,** was the earlier recommendation here and is now
second: it prices the transient case well, but it does not notice that a type is structurally
unauditable, which is where the cost actually concentrates. C is the only option that answers the question
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
- **how often a watched type is one the audit policy excludes.** If that is common, option F is the
  whole answer; if it is rare, F is a diagnostic and C is the fix.

A before-and-after against the previous behaviour was attempted and discarded rather than reported:
the two runs' populations differed by more than the change, so the comparison would have been a
workload difference wearing a causal claim's clothes. Deciding between these options on that number
would have been deciding on noise.
