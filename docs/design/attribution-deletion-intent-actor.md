# A finalized deletion is attributed to the cleanup controller, not to the deleter

> **design** — built. Index: [`../INDEX.md`](../INDEX.md)
>
> Status: BUILT. The defect was reproduced from a mutation-lab capture
> (`configmap/deletion-intent-actor`) and pinned by a corpus-driven unit test
> ([fact_index_corpus_test.go](../../internal/queue/fact_index_corpus_test.go)); that test now pins
> the fix. Shipped: the sticky uid-keyed removal slot, its consultation ahead of the exact tier for a
> removal, the count-based horizon, and the `sticky_delete` tier on
> `attribution_resolutions_total{tier}`. The measurement and the reasoning below are kept as
> written — they are why the shape is what it is.

A human deletes an object that carries a finalizer. A controller clears the finalizer. The commit
that removes the file from Git is authored by **the controller**, and the human who asked for the
deletion appears nowhere.

```text
onboard: [DELETE] reposelections/attribution-probe (by system:serviceaccount:gitops-api:tenant-operator)
```

The `[CREATE]` on the same object, seconds earlier, names the human correctly. This document says
exactly why, with the measurement, and designs the fix.

## What the deletion-as-intent rule promises

The spec's §2.2 states the intent:

> *The first observation of `deletionTimestamp` (or a `DELETED` event) removes the resource from Git
> and attributes the removal to the actor who **requested** the deletion.*

The first half holds. [`operationForLiveTargetWatchEvent`](../../internal/watch/target_watch.go)
renders any object carrying a `deletionTimestamp` as a `DELETE`, so the file goes at the transition
rather than at the eventual `DELETED`. The second half is the one that does not hold, and the reason
is not in the watch side at all — it is in what the two audit events look like.

## The measurement

The lab drives one ConfigMap with two real identities: the deletion is requested by
`alice@example.com` (impersonated), the finalizer is cleared by a real ServiceAccount token,
`system:serviceaccount:<ns>:finalizer-controller`. A **hold** between the two phases is the knob:
the API server batches audit deliveries (`--audit-webhook-batch-max-wait`, 1s here), so the hold
decides whether both events reach the operator in one batch or two.

Three facts come out of the capture, and only the third is surprising:

1. **The removal verb is a `patch`.** There is exactly one audit `delete` — phase one, the human's —
   and nothing audits the disappearance itself. This was already known
   ([`finalizer-delete`](../../test/mutationlab/corpus/configmap/finalizer-delete), catalog row 8).
2. **The two phases have different actors**, which is the whole premise of the report.
3. **Both audit events carry the same `resourceVersion`.** The human's `delete` returns the object
   with `deletionTimestamp` set, at RV *n*. The controller's finalizer `patch` returns the object as
   it stood — also at RV *n*. The capture's relational tokens make this visible directly: both
   response bodies read `resourceVersion: <rv-2>`, and so does the deletion-pending watch event.

Fact 3 is the defect. It is not a ranking problem, and no tier ordering can fix it.

## Why the join then names the controller

The publish side files a fact under the strongest key it has
([`file`](../../internal/queue/fact_index.go)). Both of these facts carry a uid and a
resourceVersion, so both are filed under **`exact(uid, rv)`** and **`latest(uid)`**. Both index
structures are last-writer-wins ([`putExact`, `putLatest`](../../internal/queue/fact_index_store.go)):

```go
s.exact[key] = entry     // same (uid, rv) → the controller's fact REPLACES the human's
s.latest[uid] = entry    // same uid       → likewise
```

So once the controller's fact is applied, the human's fact is not outranked — **it is gone**. The
watch event that renders as the removal is the deletion-pending `MODIFIED`, whose resourceVersion is
that same colliding value, so it asks the exact tier for exactly the key that was overwritten, gets
the strongest possible match, and never consults a weaker tier or waits for anything.

The corpus-driven test states both sides of the race:

| Facts applied when the removal resolves | Named actor | Tier |
|---|---|---|
| the human's `delete` only (a slow finalizer, separate audit batches) | **the human** | `exact` |
| both, in the order the API server produced them (a fast finalizer, one batch) | **the controller** | `exact` |

Nothing about the join can tell those two apart. Whether a deletion is attributed correctly depends
on how quickly a controller cleans up, which is a property of the cluster's workloads.

**The more finalizers an object carries, the likelier the wrong answer**, exactly as the report
says: each finalizer controller adds another write that can land inside the batch window.

### The second trigger: a hung finalizer plus a restart

A finalizer that hangs — days, when nobody notices — is the *safe* case for as long as the operator
stays up. The file left Git at the transition, attributed to the human, and the eventual finalizer
clear and terminal `DELETED` both fold to no-ops against the already-absent path. Slower is better.

It stops being safe the moment the operator is not running at the transition. A restart, a rollout,
a `GitTarget` created later, a `410` rebuild — after any of them the watch collapses to CURRENT
state, so there is no transition event to observe. The first observation is of an object already
`Terminating`, the file is still in Git, that observation renders as a `DELETE`, and a real commit
lands. The human's fact aged out hours ago, so that commit is authored `unresolved`, or picks up
whichever fact is still inside the TTL — during cleanup, plausibly the finalizer controller's.

A hang does not cause this. It widens the window in which an ordinary restart does, from seconds to
days, which turns an unlucky coincidence into an expected one.

### What the current code does get right

Three things already move in the right direction, and the fix should not disturb them:

- **A removal never settles for a write fact.** [`lookupRemoval`](../../internal/queue/fact_index.go)
  holds a write match as a fallback and keeps waiting for evidence about the deletion; only a fact
  whose own verb is `delete`/`deletecollection` ends the wait. That is already "prefer the actor of
  the transition", expressed at the tier level — it just never gets the chance here, because the
  exact tier answers first.
- **An object's own delete fact wins from more places** than it used to: by uid, and by
  `(namespace, name)` when the delete audit event carried no uid at all.
- **The head-of-line stall that inverted event ordering is fixed at its cause**
  ([attribution-branch-findings.md](attribution-branch-findings.md)), so the reported log's
  "controller's window commits first" no longer has that mechanism behind it.

## The fix: a sticky removal pointer

**A fact about a DELETION must not be replaceable by a fact about a WRITE.**

That single rule closes the defect, and it is the rule the tier ladder already believes in — the
removal path is built entirely around "a fact about the deletion outranks a fact about a write". It
is simply not enforceable today, because the write can overwrite the deletion's *storage* before any
ranking happens.

Concretely: file a fact whose verb is a removal into a **removal slot** keyed by uid, and let a
later non-removal fact fill the ordinary tiers without touching that slot. `lookupRemoval` consults
the removal slot first. How long the slot lives is its own decision, and not the join TTL — see
[the horizon](#how-long-it-should-live-and-why-that-is-not-the-ttl).

Sketch, in [`file`](../../internal/queue/fact_index.go):

```go
case isRemovalVerb(fact.Verb) && fact.UID != "":
    facts.putRemoval(fact.UID, entry)   // sticky: only another REMOVAL fact may replace it
    // …and the ordinary exact/latest filing as today
```

and in [`lookupRemoval`](../../internal/queue/fact_index.go), before the exact tier is consulted for
a non-exact-capable query.

### Why "sticky" and not "ranked"

A ranking change alone (consult a removal tier before the exact tier) does not help while both facts
live under one key: there would be nothing left to rank. The stickiness is the load-bearing half.
The ordering change is what makes it reachable.

### What it costs

- **One more entry per deleted object**, held until the caps reclaim it rather than until the TTL
  does. The index is capped per type and in total, so the cost is bounded by construction and shows
  up on `attribution_fact_index_evictions_total` if it ever binds.
- **A deliberate asymmetry**: the removal slot is the only structure in the index that a later fact
  may not overwrite. That deserves the comment it will get — the reason is that "who deleted this"
  is a question a later write cannot answer, which is not true of any other tier.

### How long it should live, and why that is not the TTL

The removal pointer should outlive the join TTL, and be bounded by **count rather than time**.

Every other structure in the index must expire for correctness. `exact` and `latest` can be
superseded by a later write. The name tier must expire because a **name** is reused after a
delete-and-recreate, which is exactly why it ranks last. A uid-keyed removal statement has neither
failure mode:

> **A uid is unique across space and time.** "uid X was deleted, and this actor asked for it" can
> never be superseded, because that object can never be written again, deleted again, or recreated
> under the same uid.

So holding it for a day is not less accurate than holding it for ten minutes, only more expensive,
and the question stops being "how long is safe" and becomes "how much memory will we spend". That
argues for a per-type LRU of recent removals, aged out under pressure rather than by a clock: a
cluster that deletes rarely keeps its removals for a very long time at no cost, and a busy one keeps
the most recent, which are the ones a replay is most likely to need. It fits the index's existing
shape — bounded per type and in total, evictions counted — and it needs no new number for an
operator to tune.

**Strictly uid-keyed.** The same stickiness on the name tier would be a defect rather than a fix: a
name reused after a recreate would inherit the previous object's deleter, and a longer horizon makes
that more likely, not less. The TTL is what bounds that risk today and must keep bounding it.

**The longer horizon is a horizon within one process, and the TTL still bounds what a restart can
recover.** The pointer is not persisted: the index is warmed from the fact streams, and both halves
of that are one TTL wide — the transport trims to the retention horizon, and the follower replays
exactly `i.ttl` ([`Run`](../../internal/queue/fact_index.go),
[`FollowFacts`](../../internal/queue/fact_stream.go)). So a pointer for a delete older than the TTL
survives a `410` rebuild, a re-list, and any amount of watch churn, but it does not survive the
operator process going away. The second trigger is therefore *narrowed* rather than closed: it is
closed for a restart that happens after the delete fact was seen and inside the retention window,
and untouched when the operator was not running to see the delete at all. Closing that remainder
needs the pointer to outlive the process, which is a different change — persistence — and is not
proposed here.

### The payoff that is not correctness

A `Terminating` object seen on replay resolves `absent` today, which means it waits out the **whole**
grace window for a fact written days ago that can never arrive — on the shard's single goroutine,
with every later event queued behind it. That is the head-of-line cost
[attribution-branch-findings.md](attribution-branch-findings.md) measured. A removal pointer that
outlives the TTL turns that full-grace wait into an immediate hit, so a cluster carrying objects
stuck `Terminating` stops paying a grace per replayed event — for as long as the process that saw
the delete is still running, which is the horizon the section above states exactly.

### Why not simply refuse to overwrite the exact entry

The narrower move is to make the exact tier **write-once**: `(uid, rv)` asserts "this actor produced
this exact version", and the finalizer patch did not produce that version — it returned it. Keeping
the first fact filed under a key would name the deleter here without touching the read ladder at all,
which is a real attraction.

It is not enough, for two reasons.

**It makes correctness depend on arrival order.** First-writer-wins is still a race, only decided
earlier: it gives the right answer exactly when the deleter's fact is filed first. Within one audit
batch it is, but a fact's arrival order is not guaranteed in general — an HA control plane audits the
`delete` on one API server and the finalizer `patch` on another, and those two audit batches are
independent. "A write may never displace a deletion" needs no ordering assumption, because the
patch's fact can never enter the slot no matter when it lands.

**It does nothing for the replay case.** The exact tier is TTL-bounded and has to stay that way — it
holds one entry per write, not one per delete — so the [second trigger](#the-second-trigger-a-hung-finalizer-plus-a-restart)
and the [head-of-line payoff](#the-payoff-that-is-not-correctness) both need a structure whose
horizon is a count. That structure is the removal slot, and once it exists, the ordering change is
the cheap half.

The narrower move remains worth making on its own merits — a mutating request that returns a
resourceVersion it did not produce should not claim that version's exact key, and a no-op patch does
exactly that today. It is a separate change with a separate argument, and it is not this one.

### The alternative, and why it is second

**File every delete fact under the name tier as well**, so it survives the uid tiers being
overwritten. It is smaller — one extra `putName` — and needs no new structure or lookup order.

It is second because it is indirect: it fixes this defect by making a *weaker* tier hold the answer,
so the removal resolves at `tier="name"` rather than at a tier that says what it is. A reader of the
metric would see a name-tier match and reasonably conclude the audit event carried no uid, which
here would be false. It also leaves the exact tier still answering with the controller for the
window before the name tier is consulted.

## How we will know it worked

The metrics that shipped in the attribution surface make this observable without a repro:

- The evidence mix moves. A finalized deletion that resolves today at `tier="exact"` should resolve
  at `tier="sticky_delete"` instead:
  `sum by (tier) (rate(gitopsreverser_attribution_resolutions_total[5m]))`.
- `commits_total{author_kind}` shifts from `serviceaccount` toward `user` for the affected types —
  the bottom line the report is actually about.
- The removal wait collapses for `Terminating` objects seen on replay, which today spend a full
  grace each:
  `histogram_quantile(0.95, sum by (le) (rate(gitopsreverser_attribution_resolution_wait_seconds_bucket{event_kind="removal"}[5m])))`.
- The corpus test inverts: the "both facts applied" case names the human, and the assertion that
  pinned the defect is now the assertion that pins the fix.

## Reproducing it

```bash
task lab-corpus-update    # captures configmap/deletion-intent-actor (three holds, two actors)
go test ./internal/queue/ -run TestFactIndex_FinalizerRemovalNamesTheDeleter -v
```

The hold is tunable — `LAB_FINALIZER_HOLD=0s` makes the controller win every time, a hold longer
than the audit batch window makes the human win — which is the race itself, made deterministic.

## References

- [attribution-publish-and-join.md](attribution-publish-and-join.md) — what each half does, and the
  tier ladder this fix extends.
- [attribution-removal-wait-options.md](attribution-removal-wait-options.md) — why a removal waits
  for evidence about the deletion rather than accepting the last write.
- [mutation-capture-lab-design.md](../spec/mutation-capture-lab-design.md) — the lab, and why a
  captured shape beats an asserted one.
- [interpreting-metrics.md](../interpreting-metrics.md) — the tier and actor-kind labels the fix is
  measured on.
