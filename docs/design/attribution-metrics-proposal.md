# Proposal: the attribution metric surface

> **design**: the attribution half is **built**. `watch_event_queue_seconds` is not: it moved to
> Phase 2 as a pipeline-wide metric. The Phase 1 surface below is now Phase 1 of
> [`metrics-observability-plan.md`](metrics-observability-plan.md), the canonical plan; this document
> is the reasoning trail behind it. Index: [`../INDEX.md`](../INDEX.md)

Revised after review. An earlier draft proposed thirteen new metric families at once and four of them
would have produced misleading diagnoses. The corrections are spelled out in
[what the first draft got wrong](#what-the-first-draft-got-wrong), because two of the mistakes are
the kind that stay invisible: a series that is always zero, and a gauge that moves in the right
direction while counting the wrong thing.

What survives is a smaller first release that covers normal-operation health and the loss paths
nothing can currently see, plus a deferred set with the preconditions each one needs.

## Why change anything now

**This release has already broken the `result` label.** `exact_deletecollection_item` is gone with the
`deletecollection` rework, replaced by `collection_uid` and `collection_scope`, and `name` is new.
Anything reading the old value stops matching whatever else happens.

The usual reason to leave a label alone is that changing it breaks consumers, and that cost is paid
once per break. Since this release breaks `result` already, finishing the job costs the same single
migration; deferring it costs a second one later, on a label that will have been wrong twice. Nothing
consumes these metrics yet, which will not stay true.

That argument covers the renames. It does not cover new metric families, which is why they are phased.

## Phase 1: the set built first

| Change | Kind | Why it is in the first release |
|---|---|---|
| `result` becomes `tier` plus `actor_kind` | label rework | the break is already happening |
| `weak` splits into `latest` and `resource_version` | label rework | same break, and `latest` is the tier the removal path turns on |
| `event_kind` on the wait histogram | new label | the wait behavior differs entirely between writes and removals |
| follower errors and last-success timestamp | new family | a wedged follower is silent today |
| `no_attribution_fact` outcome on `audit_events_total` | new value, existing counter | the population that produces no fact, counted where the decision is made |
| a stream-entry decode-error counter | new family | undecodable entries are dropped and skipped past with no log and no metric |
| `attribution_transport_info` | new family | changes how every other metric here is read |
| the four low-risk renames | rename | free while the surface is moving |

Everything else waits. The renames are in [the table below](#the-renames).

### `result` becomes `tier` plus `actor_kind`

Today `result` has seven values and two of them are one tier seen twice:

```text
exact_user  exact_serviceaccount  weak  collection_uid  collection_scope  name  absent
```

`exact` is the only tier that also encodes who the actor was, so counting exact resolutions means
summing two series, and the actor kind cannot be asked of any other tier. There is no way to learn
how many `name` or `collection_uid` resolutions named a service account.

`gitopsreverser_commits_total` already carries `author_kind` with `user`, `serviceaccount`,
`committer` and `unresolved`, so two metrics currently disagree about the shape of one distinction.

| Label | Values |
|---|---|
| `tier` | `exact`, `latest`, `resource_version`, `name`, `collection_uid`, `collection_scope`, `absent` |
| `actor_kind` | `user`, `serviceaccount`, `none` |

**`weak` splits at the same time.** It currently covers both a `latest` (uid) match and the rv-only
hatch, which are different evidence: the object's own last write against a fact that had a
resourceVersion and no uid. The removal path turns on `latest` specifically, and the measurement that
found the window race had to infer "these were `latest` matches held as fallbacks" from a wait
distribution because the label could not say it.

### `event_kind` on the wait histogram

`ExactCapable` splits every query into a write or a removal, and the wait design differs completely
between them: a removal holds a fallback and keeps waiting, a write does not. Today the histogram
cannot distinguish an absent write from an absent removal. Adding `event_kind` = `write` / `removal`
makes the removal wait directly queryable, which is the number anyone tuning the grace needs.

### `watch_event_queue_seconds`, moved to Phase 2

This was proposed here and is **not** part of the attribution release. It measures head-of-line
blocking on a watch shard, which is a property of the whole pipeline rather than of attribution, so
the canonical plan took it as the processing-delay stage of §4.2 and scheduled it with the watch
families. The argument for it stands and is kept here: the failure that broke an e2e spec was not a
slow resolution but the delay a slow resolution imposed on the events queued behind it on the same
single-threaded shard, which the wait histogram cannot see because it times each resolution in
isolation. It is also the pressure signal that makes a separate "resolvers waiting" gauge
unnecessary for now.

### Follower health

`attribution_fact_follower_errors_total` plus
`attribution_fact_follower_last_success_timestamp_seconds`.

When the follower fails, `Run` logs and retries with a backoff, and nothing counts it. A follower
that is flapping, or wedged and retrying forever, degrades attribution to committer-authored across
the board, with a rising unresolved rate as the only symptom and nothing pointing at the cause.

The timestamp matters more than the counter. A counter says errors are happening; only the timestamp
distinguishes "erroring occasionally while making progress" from "has not read anything in ten
minutes", and only the second is an outage.

### `no_attribution_fact` on `audit_events_total`

[`internal/audit/outcome`](../../internal/audit/outcome/outcome.go) is already the single bounded
vocabulary for what ingestion did with one event, with a derived `Category` and an e2e invariant that
gates on `category="error"` being zero. An event that is accepted but yields no attribution fact has
no terminal value there today.

Adding one, in the `Dropped` category rather than `Error`, counts that population at the point where
the decision is made and where the event's type and verb are still on the label set. That is where
the aggregated-API create shows up: it is rejected before publication, so no fact-side counter can
ever see it.

### A decode-error counter for stream entries

This is the gap the "every silent drop gets a counter" principle should have caught first and did
not. Both transports do the same thing with an entry they cannot decode:

```go
facts, err := factsFromMessage(messages[j])
if err != nil {
    continue
}
```

and then advance the cursor past it. No log, no metric, no retry. A malformed or future-schema entry
is discarded and the follower moves on as though it had read it.

`attribution_fact_stream_decode_errors_total` is the whole fix. It belongs in the first release
because it is the one loss path with no symptom at all: unlike a trim gap it is not detectable after
the fact, and unlike a publish failure the API server does not retry it.

### `attribution_transport_info{transport}`

An info gauge, value always 1, with `transport="redis"` or `transport="memory"`.

It is in the first release despite being a new family because it is interpretive metadata rather than
a signal: the two transports have different failure modes, and the same symptom means different
things under each. A burst of unresolved commits after a restart is expected under the in-memory
transport, which loses every fact on restart by design, and is a bug under Redis. Reading any of the
other metrics without knowing which is in force is reading them without knowing the contract.

If the first release needs to be smaller still, this is the one to cut.

## What the first draft got wrong

Each of these was checked against the code. They are recorded rather than deleted, because the reason
each was wrong is more useful than the corrected proposal.

### `published - filed` is not delivery loss

The draft proposed a lifecycle counter whose stages could be subtracted: `published` minus `filed`
for delivery loss, `filed` minus `matched` for facts that went unused.

The subtraction is invalid. `published` counts every fact appended by the audit receiver, for every
type. `filed` would count only facts arriving on streams **this process follows**, which is a subset
chosen by which watches are running. Replay compounds it: a restart re-reads the retention window and
files the same facts again, so the second number can exceed the first without anything being wrong.

Two counters over different populations do not subtract. Delivery loss has to be measured where
delivery happens, which is what the follower health signals above do.

### `unfilable` would be a permanently zero series

The draft claimed a stage for facts the index can file under no key, and that it would show "every
aggregated-API create".

It would show neither. The publish gate rejects an event with no resolvable name unless it is a
collection verb, so a fact reaching the index always has a name, a uid, an rv, or is a collection
fact. With the name tier in place every one of those files somewhere. The `default` branch in `file`
is unreachable, and a counter on it would be a flat zero that reads as health.

The aggregated create is the population the draft was reaching for, and it never becomes a fact at
all: it is rejected before publication. Counting it is what the `no_attribution_fact` outcome above
does, on the ingestion side where the event still exists.

### The named failure metric does not exist

The draft said publish failures are already visible as `audit_eventlist_*{outcome="write_error"}`.

`write_error` is a value on `gitopsreverser_audit_events_total`, which is per event. The
`audit_eventlist_*` families are request-level and carry a different outcome set. An alert written
against the name in the draft would report zero forever, which is the worst failure mode a monitoring
change can have.

### `facts_filed_total{tier}` conflates two models

The draft proposed counting facts by the tier they were filed under, to show the publish-side
distribution.

Tiers are resolution outcomes, and they do not partition facts. A fact with a uid and a
resourceVersion is filed under **both** `exact` and `latest`, which the publish-side documentation in
this repository states explicitly. Counting by tier would double-count the most common fact shape and
skew the distribution toward the tier that matters least.

The question underneath it stays interesting: what shape are facts arriving in, and how many carry
only a name. That needs a fact-shape taxonomy (`uid_rv`, `uid_only`, `rv_only`, `name_only`,
`collection`), which is a different label with different values. Deferred rather than renamed,
because it needs designing rather than editing.

### `resolvers_waiting` would count registrations

The draft proposed a gauge incremented when a resolver registers its waiter keys.

`Await` registers **before** its first lookup, deliberately, so that a fact arriving in the gap wakes
a waiter already listening. Most resolutions then return from that first lookup without ever
blocking. A gauge incremented at registration therefore counts resolutions in flight rather than
resolvers blocked, and it would read as pressure on a healthy system.

An earlier revision of this document also claimed the existing `factWaiterRegistry.len()` could be
exported directly. It cannot: `len()` returns the number of candidate KEYS holding a waiter, and one
resolver registers under several, so it over-counts by roughly the tier fan-out.

If it is built later it has to be incremented around the blocking `select` alone.
`watch_event_queue_seconds` measures the same pressure and is scheduled for Phase 2, so this may
never be needed.

### `streams_behind` does not mean what the name says

The draft treated it as a backlog depth and an early-loss indicator.

`behind` is set when **the last read filled its entry budget**, meaning more was waiting when the
read returned. It is the precondition for trim-gap detection rather than a measure of how far behind
the follower is. A stream one entry behind and a stream a thousand entries behind carry the same
value.

Named and interpreted as drafted it would invite an alert on a condition that occurs during any
ordinary burst. It needs redefining, or replacing with a real lag measure, before it can carry that
meaning.

### `fact_index_replay_seconds` cannot show what it was for

The draft proposed measuring replay to show that a restart warms the index before serving.

There is no replay-complete boundary to measure. The follower runs continuously, streams are added to
the subscription set as watches start, and no readiness barrier gates serving on the index being
warm. A duration recorded today would measure an arbitrary window rather than the property the metric
was proposed to prove.

The boundary has to exist first. That is a design change with its own value, and it is the same
question the fact-stream record leaves open about HA handover: whether a replica must warm its index
before starting a watch it has taken over. Build the barrier, then measure it.

## Deferred, and what has to be true first

| Deferred | Precondition |
|---|---|
| `fact_index_replay_seconds` | a replay-complete boundary and a readiness barrier exist |
| the four stream-scaling metrics | the followed-stream count is large enough to be in question, and `behind` is redefined as real lag |
| fact-shape distribution | a shape taxonomy distinct from the tier taxonomy |
| `resolvers_waiting` | queue delay proves insufficient, and it is measured around the blocking select |
| `fact_index_expired_total` | wanted when tuning the TTL or the caps; low risk, low urgency |

The stream-scaling set was designed in an earlier revision of this document and that design stands on
its own merits. What changed is the ordering: it is an investigation suite for a question nobody has
observed a problem with, and building it before the health signals above inverts the priority. The
part worth keeping in view is that a count alone cannot answer whether the stream count is reasonable,
because the same number is fine or fatal depending on what it costs.

## The renames

| Now | Proposed | Why |
|---|---|---|
| `attribution_resolutions_total{result}` | `{tier, actor_kind}` | one label, two dimensions |
| `attribution_resolution_wait_seconds{result}` | `{tier, event_kind}` | same, plus the write and removal split |
| `attribution_fact_events_total{op}` | `attribution_facts_total{op}` | "events" already means audit events and watch events |
| `attribution_fact_index_size` | `attribution_fact_index_entries` | a gauge should name what it counts |
| `attribution_collection_degraded_total{reason}` | `attribution_collection_without_uidset_total{reason}` | nothing broke; the precise join was unavailable |

`attribution_fact_index_evictions_total{reason}` and `attribution_fact_stream_gaps_total{stream}` are
unchanged and stay as they are.

## Reconciling with the canonical plan

**Done.** [`metrics-observability-plan.md`](metrics-observability-plan.md) declares itself the single
canonical metrics plan, and its attribution taxonomy and this proposal could not both be right. Its
version had drifted from the code when the fact-stream work landed:

| It specified | The code has |
|---|---|
| `result` includes `conflict` and `expired` | neither value exists |
| `attribution_fact_events_total{op}` includes `expired_unmatched` and `late` | neither op exists |
| `attribution_fact_index_size` is "facts parked in **Redis**" | the index has been in process memory since the fact-stream work |

So this was never a choice between two live designs. The canonical plan has now absorbed the Phase 1
surface below as its attribution stage (§4.4 and §5 there), records the drift above as a correction,
and links back here as the reasoning trail. A taxonomy that lives in two places diverges again, and
the canonical plan is the one people are told to read; this document keeps the argument, not the
inventory.

Two things the canonical plan took from here and generalized, because they are not attribution's
alone:

- **`watch_event_queue_seconds`** became the processing-delay stage of the whole pipeline rather than
  an attribution metric. It measures head-of-line blocking on a watch shard, which attribution
  happens to be the loudest current cause of.
- **"every silent drop gets a counter"** became a numbered principle there. It is the rule this
  document broke first and then repaired: the decode-error gap below is what a stated principle would
  have caught.

## What to write down

An [`UPGRADING.md`](../UPGRADING.md) entry with a table of old label and metric names against new
ones, so a query can be rewritten mechanically. It should state that `result` is gone rather than
only describing what replaces it.

The break itself needs no defense beyond being written down. **Nothing consumes these metrics yet**:
no dashboard ships, no alert rules ship, and no user has been told to build against these names. The
migration costs one entry today and a consumer migration after the first published dashboard, which
is the whole reason the surface is corrected in the same release that broke `result` anyway.

Worth fixing while in that file: its current attribution entry is headed
`## Unreleased — … (next minor; …)`, which [`AGENTS.md`](../../AGENTS.md) forbids, because by the
time an upgrade guide is read both halves of that heading are false.
