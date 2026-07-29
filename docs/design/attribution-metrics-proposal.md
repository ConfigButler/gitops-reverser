# Proposal: the attribution metric surface

The attribution metrics have grown one label at a time, and two of them now answer questions nobody
asked while three questions that matter have no metric at all. This proposes a consolidated surface:
what to rename, what to split, and what to add.

## Why now, specifically

**This branch has already broken the `result` label.** `exact_deletecollection_item` is gone,
replaced by `collection_uid` and `collection_scope`, and `name` is new. Anything reading
`result="exact_deletecollection_item"` stops matching in this release whatever else happens.

That turns the usual objection inside out. The reason to leave a metric label alone is that changing
it breaks consumers, and the cost is paid once per break. Since this release breaks `result` already,
finishing the job here costs the same single migration. Deferring it costs a second one later, for a
label that will have been wrong twice.

The second reason is that nothing consumes these yet. There are no dashboards or alerts to rewrite,
so the change is a documentation exercise rather than a coordination one. That will not be true
forever, which is the argument for doing it in the release that is already breaking.

## Principles this applies

1. **A label names one dimension.** If a value encodes two things, it needs two labels.
2. **`result` names the TIER, not the confidence.** The tier ladder is the model; the metric should
   be readable against the code without a translation table.
3. **Every silent drop gets a counter.** A record the system discards without recording it is a bug
   that can only be found by reading a corpus, which is how the last one was found.
4. **Prefer a gauge or histogram that names its unit.**

## Existing metrics: proposed changes

| Now | Proposed | Why |
|---|---|---|
| `attribution_resolutions_total{result=exact_user\|exact_serviceaccount\|weak\|…}` | `attribution_resolutions_total{tier, actor_kind}` | `result` mixes the tier with who the actor was; see below |
| `attribution_resolution_wait_seconds{result,…}` | `attribution_resolution_wait_seconds{tier, event_kind}` | the wait story is about removals, and the metric cannot currently say whether a wait was one |
| `attribution_fact_events_total{op}` | `attribution_facts_total{stage}` | "events" collides with audit events and watch events, the two other things called events here |
| `attribution_fact_index_size` | `attribution_fact_index_entries` | a gauge should name what it counts |
| `attribution_collection_degraded_total{reason}` | `attribution_collection_without_uidset_total{reason}` | "degraded" suggests something broke; the scope join is correct, the precise one was merely unavailable |
| `attribution_fact_index_evictions_total{reason}` | unchanged | already says what it counts |
| `attribution_fact_stream_gaps_total{stream}` | unchanged | already says what it counts |

### `result` becomes `tier` plus `actor_kind`

Today `result` has seven values and two of them are the same tier seen twice:

```text
exact_user  exact_serviceaccount  weak  collection_uid  collection_scope  name  absent
```

`exact` is the only tier that also encodes who the actor was. So counting exact resolutions means
summing two series, and the actor kind cannot be asked of any other tier at all. There is no way to
learn how many `name` or `collection_uid` resolutions named a service account, because that dimension
does not exist there.

The codebase already models this correctly one metric over: `gitopsreverser_commits_total` carries
`author_kind` with `user`, `serviceaccount`, `committer` and `unresolved`. Two metrics currently
disagree about the shape of one distinction.

**Proposed:**

| Label | Values |
|---|---|
| `tier` | `exact`, `latest`, `resource_version`, `name`, `collection_uid`, `collection_scope`, `absent` |
| `actor_kind` | `user`, `serviceaccount`, `none` |

`actor_kind=none` pairs with `tier=absent`, and lines up with `commits_total`'s `unresolved`.

### `weak` is also two tiers

While `result` is being changed: `weak` currently covers a `latest` (uid) match AND the rv-only
hatch, which are different evidence. A `latest` match is the object's own last write; an rv-only
match is a fact that had a resourceVersion and no uid at all. Splitting them into `latest` and
`resource_version` makes the label exactly the tier ladder, one value per rung, which is the point of
principle 2.

This matters more than it looks. The removal path turns on the `latest` tier specifically, and the
measurement that found the window race had to infer "these were `latest` matches held as fallbacks"
from a wait distribution, because the metric could not say it.

### `event_kind` on the wait histogram

`ExactCapable` splits every query into a write or a removal, and the entire wait design differs
between them: a removal holds a fallback and keeps waiting, a write does not. The histogram cannot
currently distinguish an absent write from an absent removal, which are very different stories.

Adding `event_kind` = `write` / `removal` costs one label and makes the removal wait directly
queryable, which is the number anyone tuning the grace needs.

## New metrics

### 1. The fact lifecycle, including what is discarded

The gap that cost the most. When `file` matches no case it returns no keys, `Apply` records nothing,
and the fact vanishes. So `written` minus `matched` conflates two populations that need different
responses: facts nobody happened to need, and facts nobody could ever have joined.

That second population is not rare or theoretical. It is every aggregated-API create, and before the
name tier it was every delete the API server answered with a `Status`. The window race lived there
for the whole branch, and no counter moved.

**Proposed:** `gitopsreverser_attribution_facts_total{stage}` with stages

| Stage | Meaning |
|---|---|
| `published` | appended to the fact log by the audit receiver |
| `filed` | landed in the index under at least one key |
| `unfilable` | reached the index and could be filed under nothing |
| `matched` | joined by a watch event |

`published` minus `filed` is delivery loss. `filed` minus `matched` is facts that were joinable and
went unused. `unfilable` is the population that can never be attributed, and it should be a named,
graphable number rather than a discovery.

### 2. The publish-side tier distribution

**Proposed:** `gitopsreverser_attribution_facts_filed_total{tier}`, with the same tier values as the
resolution metric.

This answers what shape the facts are arriving in: how many carry a uid, how many only a name.
That ratio is the whole aggregated-API story, and today it can only be learned by reading the index
in a debugger. Together with the resolution metric it also shows the two sides in one query: filed
under `name`, matched at `name`.

### 3. Head-of-line delay on the watch shard

The failure that broke a spec was not a slow resolution. It was the delay a slow resolution imposed
on the events queued behind it, and nothing measures that. The wait histogram times each resolution
in isolation; the ten-second window delay was only visible by correlating two log lines by hand.

**Proposed:** `gitopsreverser_watch_event_queue_seconds{group, version, resource}`, a histogram of
how long a watch event waited between arriving on its shard and being picked up for processing.

That is the honest measure of the blocking, it is cheap (one timestamp per event), and it would alert
on the condition rather than on one of its symptoms. An optional companion,
`gitopsreverser_watch_shard_backlog`, gauges depth if the histogram proves too coarse.

## Questions this surface can answer that today's cannot

| Question | Query |
|---|---|
| How many attributions named a service account, at any tier? | `sum by (actor_kind) (attribution_resolutions_total)` |
| Are removals waiting longer than writes? | `attribution_resolution_wait_seconds{event_kind="removal"}` |
| Are we publishing facts nobody can ever join? | `attribution_facts_total{stage="unfilable"}` |
| Which tier carries the aggregated types? | `attribution_facts_filed_total{tier="name"}` |
| Is a slow resolve delaying other events? | `watch_event_queue_seconds` |
| Is the `latest` tier carrying removals, or the rv hatch? | `attribution_resolutions_total{tier="latest"}` |

## Cost and risk

The code cost is small. `actor_kind` is derived from the author string at read time by a function that
already exists, so nothing new is stored or plumbed. `tier` values already exist as constants and are
being edited in this release regardless. The two new counters are increments at points the code
already passes through. Only the queue histogram needs a new value carried, one timestamp per watch
event.

The risk is the label break, and it is the one thing to be deliberate about. `result` disappears,
replaced by `tier` and `actor_kind`. Renaming a label rather than adding a value means a query
referencing `result` returns nothing rather than returning something wrong, which is the failure mode
to prefer.

## What to write down

An [`UPGRADING.md`](../UPGRADING.md) entry naming the old and new label for each metric, in a table,
so a reader can rewrite a query mechanically. It should state plainly that `result` is gone rather
than describing its replacement only.

Worth noting while editing that file: its current attribution entry is headed
`## Unreleased — … (next minor; …)`, which the house rule in [`AGENTS.md`](../../AGENTS.md)
specifically forbids, because by the time anyone reads an upgrade guide both halves of that heading
are false. Whoever adds the metrics entry is in the right place to fix it.
