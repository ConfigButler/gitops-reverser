# Metrics & Audit Observability — improvement plan

> **partly built**. Index: [`../INDEX.md`](../INDEX.md)
>
> Status: PLAN — revised 2026-07-29, reconciled to the code after the attribution fact-stream
> switchover. **Architecture-led**: [architecture.md](../architecture.md) is the spine; every metric
> below maps to a stage in [Common Flows](../architecture.md#common-flows). The live baseline and the
> documentation bar come from [interpreting-metrics.md](../interpreting-metrics.md). This doc is the
> single canonical metrics plan — it supersedes the per-feature metric notes now in `finished/`, and
> it now **absorbs** the attribution surface that was designed in a separate proposal, since
> consolidated into [the attribution spec](../spec/attribution.md#what-is-observable). This document
> owns the plan; that one owns the shipped surface.

## 1. Why now

The product is one sentence: **watch events arrive, and they are processed into commits.** Everything
worth measuring is either a stage of that pipeline, or a way an event can be lost, delayed, or
attributed to the wrong actor on the way through.

The June 2026 cleanup left a surface that was honest but **lopsided**: it covered the *edges* (Git
write, discovery catalog, Secret encryption) and was dark in the middle. Since then the attribution
stage has been rebuilt and instrumented — the fact keyspace became a per-type fact **stream** that
the watch side follows into an in-process index
([attribution-fact-stream.md](../finished/attribution-fact-stream.md)) — so the middle is no longer
uniformly dark. What is left is a sharper, smaller list:

- **Watch ingestion** is the source of object state
  ([architecture.md → State Ingestion](../architecture.md#state-ingestion-and-not-losing-deletes))
  and still has **no direct metrics at all**. It is now the biggest hole by a wide margin.
- **The delay between an event arriving and being processed** is unmeasured, and it is a proven
  failure mode rather than a theoretical one: a slow resolution head-of-line blocks its shard, which
  is what broke a CommitRequest e2e spec (see
  [the attribution spec → the wait](../spec/attribution.md#the-wait)).
- **Attribution was instrumented but mislabelled**, and Phase 1 has now fixed it: `result` crammed a
  tier and an actor kind into one label, `weak` covered two different kinds of evidence, and the wait
  histogram could not tell a write from a removal — exactly the distinction the removal-wait design
  turns on. The attribution loss paths that were silent (an undecodable stream entry, a wedged
  follower, an accepted event that produces no fact) are counted too.

### The questions the metrics must answer

- Is the operator turning cluster changes into commits right now? (liveness)
- Are watch events arriving, and are they being processed promptly, or queueing? (ingestion health)
- Is audit arriving, is it good, and is it actually putting real names on commits? (the audit lens)
- When attribution *doesn't* land, why — no audit, weak evidence, an unfollowed type, a lost fact?
  (degradation)
- Is any object state being lost or stalled? (correctness / backpressure)

### Breaking changes are cheap right now, and will not stay cheap

**Nothing consumes these metrics yet.** No dashboard ships, no alert rules ship, and no user has been
told to build against the current label names. The cost of renaming a label today is one
[`UPGRADING.md`](../UPGRADING.md) entry; the cost after a release with a published dashboard is a
migration for every consumer. This release has already broken the `result` label anyway — the
`deletecollection` rework removed `exact_deletecollection_item` and added `deletecollection_body_uid`,
`deletecollection_scope`, and `name` — so finishing the job costs the same single migration, and deferring
it costs a second one later on a label that will have been wrong twice.

So this plan takes the label breaks now, deliberately, and writes them down. It does not take them
quietly.

## 2. Principles

1. **Architecture is the spine.** Every metric maps to a named stage of
   [Common Flows](../architecture.md#common-flows). No metric exists without a stage.
2. **Watch is the source of truth → instrument it first.** It currently has the least coverage and
   the most risk.
3. **Audit is optional attribution, never correctness.** Metrics measure attribution *coverage and
   quality*; the invariant "a missing/late fact changes the author, never the state"
   ([architecture.md → Optional Attribution](../architecture.md#optional-attribution)) must be
   *visible*, not just asserted.
4. **Every metric has a recording site and an interpretation.** The thing we deleted in June —
   defined-but-never-recorded instruments — must never come back. A metric ships with its
   [interpreting-metrics.md](../interpreting-metrics.md) row (what it measures, one query, what a bad
   value looks like) in the *same* change.
5. **Every silent drop gets a counter.** If the pipeline discards, skips, or ages out an event or a
   fact, that population is counted where the decision is made. A path that loses data with no
   symptom is the failure mode this plan exists to remove.
6. **Label discipline.** Bounded cardinality only: `group`/`version`/`resource` (tens of
   claimed-and-followable types, not thousands), `verb` (~5), bounded `scope`
   (`namespace`/`cluster`), and frozen enum labels (`outcome`, `tier`, `actor_kind`, `reason`).
   **Never** put an object's `name`/`namespace` in a label. Identity labels stay prefixed
   (`provider_*`, `gittarget_*`) to survive a `honor_labels=false` pod scrape — see the note in
   [exporter.go](../../internal/telemetry/exporter.go).
7. **Degradation is loud.** Unresolved attribution, a wedged fact follower, `410` rebuilds, LIST
   fallback — each has a metric, so running in a degraded shape is a visible state, not a silent one.
8. **A metric answers a question a reader can act on.** A number nobody can act on is a number that
   gets alerted on wrongly; `streams_behind` in §5.4 is the worked example.

## 3. Current state (the map)

The live instrument set is [`exporter.go`](../../internal/telemetry/exporter.go); the reader's guide
to it is [interpreting-metrics.md](../interpreting-metrics.md).

| Pipeline stage (architecture.md) | Live metrics today | Coverage |
|---|---|---|
| Discovery & catalog | `api_catalog_resources`, `_group_versions`, `_refresh_total`, `_refresh_duration_seconds`, `_generation` | ✅ good |
| **Watch ingestion** | — | ❌ **none** |
| **Shard queue / processing delay** | — | ❌ **none** |
| **Relevance filter** | — | ❌ **none** |
| Audit ingress | `audit_eventlists_total`, `_eventlist_events_total`, `_eventlist_duration_seconds`, `audit_events_total{outcome,category,group,version,resource,verb}` | ✅ good |
| Attribution publish & join | `attribution_resolutions_total{tier,actor_kind,…}`, `attribution_resolution_wait_seconds{tier,event_kind,…}`, `attribution_facts_total{op}`, `attribution_fact_index_entries`, `_index_evictions_total{reason}`, `_stream_gaps_total{stream}`, `_stream_decode_errors_total{transport}`, `_fact_follower_errors_total{transport}`, `_fact_follower_last_success_timestamp_seconds`, `_collection_without_uidset_total{reason}`, `attribution_transport_info{transport}` | ✅ good (Phase 1 shipped) |
| Git write | `commits_total{provider_*,branch,author_kind}`, `git_operations_total`, `objects_written_total`, `prune_retained_documents_total`, `branch_worker_queue_depth`, `resync_sweep_deletes_total` | 🟡 no push latency / conflict |
| New-file placement | `placements_total{source,disposition,gittarget_*,group,version,resource}`, `placement_refusals_total{reason,…}`, `placement_kustomization_entries_total{outcome,gittarget_*}` | ✅ good — shipped with the Option C deletion |
| Control plane / reconcile | `target_reconcile_completed_total`, `resync_background_failures_total`, `watched_types` | ✅ good |
| Secret encryption | `secret_encryption_{attempts,success,failures,cache_hits,marker_skips}_total` | ✅ good |

Two rows changed meaning since the last revision of this plan, and any query written against the
old text is wrong:

| The old plan said | The code does |
|---|---|
| `result` includes `conflict` and `expired` | neither value ever existed; `result` itself is now gone, replaced by `tier` plus `actor_kind` (§4.4) |
| `attribution_fact_events_total{op}` includes `expired_unmatched` and `late` | `op` is `written` or `matched`, and nothing else |
| `attribution_fact_index_size` is "facts parked in **Redis**" | the index is in **process memory**; Redis (or an in-process ring) carries the fact *stream*, not the index |

## 4. Target metric model — by pipeline stage

New watch metrics use the shape sketched in
[watch-first-ingestion-architecture.md → Metrics](../finished/watch-first-ingestion-architecture.md),
modernized to the live audit convention: separate `group`, `version`, and `resource` labels instead
of a packed `gvr` string. Keep `version` even though it adds series — Git paths and audit metrics
already treat version as part of resource identity, and a served-version migration should be visible
rather than silently folded into the old series.

### 4.1 Watch ingestion (new — the biggest hole)

| Metric | Type | Labels | Answers |
|---|---|---|---|
| `watch_events_total` | counter | `group`, `version`, `resource`, `scope`, `type` (added/modified/deleted/bookmark), `outcome` (applied/filtered/dropped) | watch volume and where events go |
| `watch_restarts_total` | counter | `group`, `version`, `resource`, `scope`, `reason` (`410_gone`/`disconnect`/`rule_change`) | watch stability / `410` pressure |
| `watch_replay_seconds` | histogram | `group`, `version`, `resource`, `scope` | time to `initial-events-end` — resume cost |
| `watch_replay_objects` | histogram | `group`, `version`, `resource`, `scope` | replay size (how much state is re-walked) |
| `watch_recovery_total` | counter | `group`, `version`, `resource`, `mode` (`cursor_resume`/`replay`/`list_fallback`) | which recovery path fires — cursor effectiveness vs aggregated-API fallback |
| `watch_active` | gauge | `group`, `version`, `resource`, `scope` | open watch goroutines vs claimed set |

`watch_active` needs bookkeeping, not just a recording call. Count bookmarks at the session receive
point, before `targetWatchEventResourceVersion` swallows them into cursor progress.

### 4.2 Processing delay — the head-of-line signal (new)

| Metric | Type | Labels | Answers |
|---|---|---|---|
| `watch_event_queue_seconds` | histogram | `group`, `version`, `resource` | how long an event waited between arriving on its shard and being picked up |

This is the failure that broke an e2e spec, and nothing measures it. It was not a slow resolution: it
was the delay a slow resolution imposed on the events queued *behind* it on the same single-threaded
shard. The wait histogram in §4.4 times each resolution in isolation; the ten-second window delay was
only visible by correlating two log lines by hand.

It is also the pressure signal that makes a separate "resolvers currently blocked" gauge unnecessary
for now — see the deferred list in §5.5.

### 4.3 Relevance filter (new)

| Metric | Type | Labels | Answers |
|---|---|---|---|
| `watch_events_filtered_total` | counter | `group`, `version`, `resource`, `reason` (`sanitized_noop`/`status_only`/`not_followable`/`duplicate`) | is the product-side filter behaving, or masking real changes? |

The reason set is the target shape, not a claim that one chokepoint exists today. The phase that
builds it first locates or consolidates the scattered filter decisions on the watch-to-Git path, then
records the metric at the smallest honest boundary.

### 4.4 Attribution / audit — **the centerpiece** (§5 expands this)

| Metric | Type | Labels | State |
|---|---|---|---|
| `audit_events_total` | counter | `outcome`, `category`, `group`, `version`, `resource`, `verb` | ✅ shipped — `no_attribution_fact` added in the `dropped` category |
| `audit_eventlists_total` / `_eventlist_events_total` / `_eventlist_duration_seconds` | counter/hist | `outcome` | live, unchanged |
| `attribution_resolutions_total` | counter | **`tier`**, **`actor_kind`**, `group`, `version`, `resource` | ✅ shipped — `result` is gone; `tier` gained `delete_sticky`, and the two collection values are named for the `deletecollection` verb |
| `attribution_resolution_wait_seconds` | histogram | **`tier`**, **`event_kind`**, `group`, `version`, `resource` | ✅ shipped — relabelled and split by write/removal |
| `attribution_facts_total` | counter | `op` (`written`/`matched`) | ✅ shipped — renamed from `attribution_fact_events_total` |
| `attribution_fact_index_entries` | gauge | — | ✅ shipped — renamed from `attribution_fact_index_size` |
| `attribution_fact_index_evictions_total` | counter | `reason` (`per_type`/`total`) | live, unchanged — and now the removal pointer's only horizon |
| `attribution_fact_stream_gaps_total` | counter | `stream` | live, unchanged |
| `attribution_collection_without_uidset_total` | counter | `reason` (`uid_cap`/`no_uids`) | ✅ shipped — renamed from `attribution_collection_degraded_total` |
| `attribution_fact_stream_decode_errors_total` | counter | `transport` | ✅ shipped — the one loss path that had no symptom at all |
| `attribution_fact_follower_errors_total` | counter | `transport` | ✅ shipped |
| `attribution_fact_follower_last_success_timestamp_seconds` | gauge | — | ✅ shipped — distinguishes "erroring but progressing" from "has read nothing in ten minutes" |
| `attribution_transport_info` | gauge (always 1) | `transport` (`redis`/`memory`) | ✅ shipped — interpretive metadata; changes how every metric above reads |
| `commits_total` | counter | `provider_*`, `branch`, `author_kind` | live, unchanged — the bottom line |

### 4.5 Git write (new additions to a covered stage)

| Metric | Type | Labels | Answers |
|---|---|---|---|
| `git_push_duration_seconds` | histogram | `provider_*`, `branch` | push latency (re-added with a recording site and doc row) |
| `git_push_conflicts_total` | counter | `provider_*`, `branch` | non-fast-forward → fetch/reset/replay retries ([PushAtomic](../../internal/git/git_atomic_push.go) detects a moved remote; [BranchWorker](../../internal/git/branch_worker.go) fetches, rebuilds, and retries) |
| `placements_total` | counter | `source`, `disposition`, `gittarget_*`, `group`, `version`, `resource` | ✅ shipped — "why did this file land here?", and which (target, type) needs a `placement.byType` line (`source="canonical"`) |
| `placement_refusals_total` | counter | `reason`, `gittarget_*`, `group`, `version`, `resource` | ✅ shipped — which resources are **not** in the mirror, and why. Replaces a log line plus `ResyncStats.PlacementSkipped` |
| `placement_kustomization_entries_total` | counter | `outcome`, `gittarget_*` | ✅ shipped — `failed` is a file committed outside every render: in Git, looks mirrored, applied by nothing |

**Note on `placements_total`, since this plan argued the other way.** An earlier revision of the
priority queue said to argue *against* leading with a `placement_fell_back_total`, because "it happened
somewhere" is not actionable. That objection was to the **labels**, not to the counter: with the
GitTarget and the type key on it, one series reads directly as the `byType` line that is missing. The
per-resource detail (path, name) deliberately stays in the log line.

### 4.6 Catalog, reconcile, secrets

Keep as-is (✅ above). One small add:
`watch_set_changes_total{gittarget_namespace,gittarget_name,op=open/close}` to see watch churn when
rules/CRDs change (pairs with `target_reconcile_completed_total{trigger=rule_change}`).

## 5. Deep dive: audit & attribution observability

This is the subsystem to keep glass-box, and it is the one that changed most. The model is now two
halves that never call each other, meeting only through the keys a fact was filed under
([the attribution spec](../spec/attribution.md)):

```text
kube-apiserver --POST--> /audit-webhook --gate--> append one entry per type to the fact stream
                                                            |  (Redis Streams, or an in-process ring)
                                                            v
                                                     fact follower --> in-process index (bounded, TTL'd)
                                                            |
watch event --> resolver registers waiter keys, looks once, --+
                sleeps up to --author-attribution-grace
                                                            |
                            evidence found -> named actor ; else -> unknown (attribution unresolved)
```

Four lenses, each a dashboard question.

### 5.1 Is audit arriving and well-formed?

`audit_eventlists_total{outcome}` (delivery), `audit_eventlist_duration_seconds` (latency),
`audit_events_total{category}` (per-event fate; `error` must be 0). This is the ingress half, and it
is already complete.

One value was missing from it, and Phase 1 added it.
[`internal/audit/outcome`](../../internal/audit/outcome/outcome.go) is the single bounded vocabulary
for what ingestion did with one event, and an event that is accepted but yields **no attribution
fact** had no terminal value there — it was counted `queued`, which claimed an append that was never
owed. `no_attribution_fact` in the `Dropped` category (not `Error`, so the e2e invariant is intact)
counts that population at the point where the decision is made, while the event's type and verb are
still on the label set. That is where the aggregated-API create shows up: it is rejected before
publication, so no fact-side counter can ever see it.

### 5.2 Is the evidence good enough to name an actor?

`attribution_resolutions_total` is the heart, and its label was wrong.

`result` had seven values and two of them were one tier seen twice:

```text
exact_user  exact_serviceaccount  weak  collection_uid  collection_scope  name  absent
```

`exact` is the only tier that also encodes *who* the actor was, so counting exact resolutions means
summing two series, and the actor kind cannot be asked of any other tier — there is no way to learn
how many `name` or `collection_uid` resolutions named a service account. Meanwhile `commits_total`
already carries `author_kind` with `user`, `serviceaccount`, `committer`, and `unresolved`, so the
two metrics disagreed about the shape of one distinction.

**Shipped:**

| Label | Values |
|---|---|
| `tier` | `exact`, `latest`, `resource_version`, `name`, `delete_sticky`, `deletecollection_body_uid`, `deletecollection_scope`, `absent` |
| `actor_kind` | `user`, `serviceaccount`, `none` |

`weak` split at the same time. It covered both a `latest` (uid) match and the rv-only
escape hatch, which are different evidence: the object's own last write, against a fact that carried
a resourceVersion and no uid. The removal path turns on `latest` specifically, and the measurement
that found the window race had to *infer* "these were `latest` matches held as fallbacks" from a wait
distribution, because the label could not say it.

The tier ladder itself, strongest first, is documented in
[the attribution spec → the tiers](../spec/attribution.md#the-tiers-strongest-first);
the operator-facing reading of each value is in
[interpreting-metrics.md](../interpreting-metrics.md#audit-attribution-optional).

**Match coverage** is the share of resolutions that named an actor rather than producing the explicit
unresolved author. It is `tier != "absent"` — *not* `tier =~ "exact.*"`, which would read the
collection and name tiers as misses.

### 5.3 How long did it wait, and for what kind of event?

`attribution_resolution_wait_seconds` carries `event_kind` (`write` / `removal`). `ExactCapable`
splits every query into a write or a removal, and the wait design differs completely between them: a
removal holds a fallback and keeps waiting for evidence about the deletion, a write does not. The
histogram used to put an absent write and an absent removal in one series, and the removal wait is
the number anyone tuning `--author-attribution-grace` actually needs.

Splitting wait time by outcome is what turned the window race from a mystery into a measurement:
the uid-latest tier at a 6.7 s mean against the exact tier at 0.18 s said immediately that removals
were sitting out their grace, which no aggregate mean would have shown. `event_kind` makes that
reading direct instead of inferred.

### 5.4 Is the fact pipeline itself healthy?

The publish side, the transport, and the follower are three places a fact can be lost. All three are
counted now; the last two were added in Phase 1.

- **Decode errors — the sharpest gap.** Both transports did the same thing with an entry they cannot
  decode: `continue`, then advance the cursor past it. No log, no metric, no retry. A malformed or
  future-schema entry was discarded and the follower moved on as though it had read it.
  `attribution_fact_stream_decode_errors_total` (plus a log line) is the whole fix, and it was first
  in line because it is the one loss path with **no symptom at all**: unlike a trim gap it is not
  detectable after the fact, and unlike a publish failure the API server does not retry it.
- **Follower health.** When the follower fails, `Run` logs and retries with a backoff. Nothing
  counted it. A follower that is flapping, or wedged and retrying forever, degrades attribution to
  committer-authored across the board, with a rising unresolved rate as the only symptom and nothing
  pointing at the cause. The **timestamp matters more than the counter**: a counter says errors are
  happening; only `..._last_success_timestamp_seconds` distinguishes "erroring occasionally while
  making progress" from "has not read anything in ten minutes", and only the second is an outage.
- **Transport identity.** `attribution_transport_info{transport}` is an info gauge, value always 1.
  It is interpretive metadata rather than a signal: the two transports have different failure modes,
  and the same symptom means different things under each. A burst of unresolved commits after a
  restart is *expected* under the in-memory transport, which loses every fact on restart by design,
  and is a *bug* under Redis. Reading any other metric here without knowing which transport is in
  force is reading it without knowing the contract.
- **Already live and worth keeping:** `attribution_fact_stream_gaps_total{stream}` (facts lost for
  good to a trim — should be zero), `attribution_fact_index_evictions_total{reason}` (the caps are
  binding), and `attribution_collection_without_uidset_total{reason}` (the precise collection join
  was unavailable, so the resolution fell to the scope tier).

One live signal is **not** ready to be exported. `behind` on a followed stream is set when the last
read filled its entry budget, meaning more was waiting when the read returned. It is the precondition
for trim-gap detection, not a measure of how far behind the follower is — a stream one entry behind
and a stream a thousand entries behind carry the same value. Exported as `streams_behind` it would
invite an alert on a condition that occurs during any ordinary burst. It needs redefining as real lag
before it can carry that meaning.

### 5.5 Deferred, and what has to be true first

| Deferred | Precondition |
|---|---|
| `fact_index_replay_seconds` | a replay-complete boundary and a readiness barrier exist at all — the follower runs continuously, streams join the subscription set as watches start, and nothing gates serving on a warm index, so a duration recorded today would measure an arbitrary window |
| the stream-scaling set (followed-stream count, per-stream read cost, lag) | the followed-stream count is large enough to be in question, and `behind` is redefined as real lag |
| fact-shape distribution (`uid_rv`/`uid_only`/`rv_only`/`name_only`/`collection`) | a shape taxonomy distinct from the tier taxonomy. Counting facts *by tier* is invalid: a fact with a uid **and** an rv is filed under both `exact` and `latest`, so tiers do not partition facts |
| `resolvers_waiting` | queue delay (§4.2) proves insufficient, **and** it is incremented around the blocking `select` alone — `Await` registers *before* its first lookup on purpose, so a gauge at registration counts resolutions in flight rather than resolvers blocked |
| `fact_index_expired_total` | wanted when tuning the TTL or the caps; low risk, low urgency |

What an earlier draft of this surface got wrong, and why each mistake was invisible, is recorded in
`git log` on the proposal that has since been folded into
[the attribution spec](../spec/attribution.md#what-is-observable); the three things that surface
deliberately cannot answer are listed there.

## 6. The reference dashboard

Grafana, one dashboard, top-down. The **Audit & Attribution** row is the marquee. PromQL is given so
a panel is copy-pasteable. The attribution queries below are against the shipped Phase 1 labels; the
watch and push families they sit beside are not emitted yet.

**Row 0 — SLO header (stat panels):**

- Commit rate: `sum(rate(gitopsreverser_commits_total[5m]))`
- Audit errors (must be 0): `sum(rate(gitopsreverser_audit_events_total{category="error"}[5m]))`
- Attribution match coverage %:
  `sum(rate(gitopsreverser_attribution_resolutions_total{tier!="absent"}[5m])) / sum(rate(gitopsreverser_attribution_resolutions_total[5m]))`
- Push latency p95: `histogram_quantile(0.95, sum by (le)(rate(gitopsreverser_git_push_duration_seconds_bucket[5m])))`
- Max worker queue depth: `max(gitopsreverser_branch_worker_queue_depth)`
- Transport in force: `gitopsreverser_attribution_transport_info` (a legend, not a threshold)

**Row 1 — AUDIT & ATTRIBUTION (marquee):**

- *Live audit stream by type* (timeseries): `sum by (group,version,resource)(rate(gitopsreverser_audit_events_total[1m]))`
- *Audit outcome mix* (stacked): `sum by (category,outcome)(rate(gitopsreverser_audit_events_total[5m]))`
- *Attribution coverage by type* (timeseries):

  ```promql
  sum by (group,version,resource)(rate(gitopsreverser_attribution_resolutions_total{tier!="absent"}[5m]))
    / sum by (group,version,resource)(rate(gitopsreverser_attribution_resolutions_total[5m]))
  ```

- *Evidence mix* (stacked): `sum by (tier)(rate(gitopsreverser_attribution_resolutions_total[5m]))` —
  a shift from `exact` toward `collection_scope` or `name` is a quality regression even while
  coverage holds flat.
- *Actor mix* (pie): `sum by (actor_kind)(rate(gitopsreverser_attribution_resolutions_total[15m]))`
- *Commit author mix* (pie/stacked): `sum by (author_kind)(rate(gitopsreverser_commits_total[15m]))`
- *Removal wait p95* (timeseries) with an `--author-attribution-grace` threshold line — the panel the
  grace window is tuned from:

  ```promql
  histogram_quantile(0.95, sum by (le,tier)(
    rate(gitopsreverser_attribution_resolution_wait_seconds_bucket{event_kind="removal"}[5m])))
  ```

- *Fact pipeline health* (timeseries): `sum by (op)(rate(gitopsreverser_attribution_facts_total[5m]))`,
  alongside `gitopsreverser_attribution_fact_index_entries`
- *Fact loss* (timeseries, should be flat zero):
  `sum(rate(gitopsreverser_attribution_fact_stream_gaps_total[5m]))`,
  `sum(rate(gitopsreverser_attribution_fact_stream_decode_errors_total[5m]))`,
  `sum by (reason)(rate(gitopsreverser_attribution_fact_index_evictions_total[5m]))`
- *Follower liveness* (stat): `time() - gitopsreverser_attribution_fact_follower_last_success_timestamp_seconds`
- *Top dropped audit outcomes* (table): `topk(10, sum by (resource,verb,outcome)(rate(gitopsreverser_audit_events_total{category="dropped"}[5m])))`

**Row 2 — WATCH INGESTION:**

- Events/sec by type: `sum by (group,version,resource,type)(rate(gitopsreverser_watch_events_total[5m]))`
- Queue delay p95 — the head-of-line panel:
  `histogram_quantile(0.95, sum by (le,group,version,resource)(rate(gitopsreverser_watch_event_queue_seconds_bucket[5m])))`
- Restarts / `410` pressure: `sum by (group,version,resource,reason)(rate(gitopsreverser_watch_restarts_total[15m]))`
- Replay p95: `histogram_quantile(0.95, sum by (le,group,version,resource)(rate(gitopsreverser_watch_replay_seconds_bucket[5m])))`
- Recovery mode mix (cursor vs replay vs list): `sum by (mode)(rate(gitopsreverser_watch_recovery_total[15m]))`
- Active watches vs claimed: `sum(gitopsreverser_watch_active)` vs `sum(gitopsreverser_watched_types)`

**Row 3 — GIT WRITE:**

- Commit rate by provider/branch, push latency p95, conflict-retry ratio
  (`rate(git_push_conflicts_total)/rate(commits_total)`), queue depth, objects written, resync sweep
  deletes: `sum by (group,version,resource)(rate(gitopsreverser_resync_sweep_deletes_total[1h]))`.

**Row 4 — DISCOVERY / SECRETS:**

- Allowed resources, degraded group/versions (`> 0` red), refresh outcome mix, encryption failure rate.

> The dashboard ships as JSON under `docs/dashboards/` (or the chart) so it is versioned with the
> code. It is built after the §4.4 relabel lands, so it is never written against a name that is
> already scheduled to change.

## 7. Cardinality & cost

- `group`/`version`/`resource` is bounded by **claimed ∩ followable** types (tens), `verb` ~5,
  `scope` is a bounded enum (`namespace`/`cluster`), and all other labels are frozen enums (`tier` 7,
  `actor_kind` 3, `event_kind` 2, `outcome` ~11, `category` 3, `author_kind` 4, `mode` 3). Worst case
  is a few thousand series total — comfortable for Prometheus.
- The one place to watch is `attribution_resolution_wait_seconds`, which is a histogram carrying
  `tier` × `event_kind` × the type triple. It already carries the type triple today; `event_kind`
  doubles it at most, and removals and writes rarely both occur for every tier.
- **No object identity in labels** (no `name`/`namespace` of watched objects) — that is the only
  thing that would blow up cardinality, and principle 6 forbids it.
- Histograms reuse shared bucket sets (sub-second→minutes), as today.

## 8. Alerts / SLOs

| Alert | Expression (sketch) | Meaning |
|---|---|---|
| Audit fact-store errors | `rate(gitopsreverser_audit_events_total{category="error"}[10m]) > 0` | fact appends are failing — check the transport |
| Fact stream loss | `rate(gitopsreverser_attribution_fact_stream_gaps_total[10m]) > 0` | the stream was trimmed past this process's position; those facts are gone |
| Undecodable fact entries | `rate(gitopsreverser_attribution_fact_stream_decode_errors_total[10m]) > 0` | a schema or version mismatch on the stream; facts are being skipped |
| Fact follower wedged | `(time() - …_fact_follower_last_success_timestamp_seconds > 600) or (…_transport_info == 1 unless on() …_fact_follower_last_success_timestamp_seconds)`, `for: 10m` | attribution is degrading to committer-authored cluster-wide |
| Attribution coverage drop | coverage (`tier!="absent"`) `< 0.5` for 30m while audit is flowing | facts stopped matching watch events |
| Grace window saturating | `attribution_resolution_wait_seconds{tier="absent",event_kind="removal"}` p95 → `--author-attribution-grace` | removals are sitting out the full grace; raise grace, or skip the wait for never-attributed types |
| Shard queue delay | `watch_event_queue_seconds` p95 approaching the grace window | head-of-line blocking; events are queued behind slow resolutions |
| Watch restart storm | `rate(watch_restarts_total{reason="410_gone"}[15m])` spike | RV churn / compaction pressure |
| List fallback in use | `rate(watch_recovery_total{mode="list_fallback"}[1h]) > 0` | an aggregated API isn't honoring streaming list |
| Worker backing up | `branch_worker_queue_depth` rising, not draining | stalled remote |
| Degraded API surface | `api_catalog_group_versions{state="degraded"} > 0` | broken APIService |

The follower row needs both arms, and the second is the one that is easy to leave out. The gauge is
not emitted until the follower's first successful read, so `time() - <gauge>` returns **no series**
for a follower that has been wedged since startup — a transport unreachable at boot, which is
precisely the outage the metric exists for. The `unless` arm fires on the gauge's ABSENCE while
`attribution_transport_info` says a follower is running, and `for: 10m` keeps an ordinary restart's
gap from tripping it. Stamping the gauge at start instead would remove the arm at the cost of
claiming a success that never happened, which is worse: it reads as health for the first ten minutes
of every outage.

Note the first row's metric: `write_error` is a value on `gitopsreverser_audit_events_total`, which
is **per event**. The `audit_eventlist_*` families are request-level and carry a different outcome
set; an alert written against `audit_eventlist_*{outcome="write_error"}` reports zero forever, which
is the worst failure mode a monitoring change can have.

## 9. Implementation phases

Phases 1-3 each ship: recording sites → unit tests (manual-reader assertions) →
`interpreting-metrics.md` rows, validated per
[AGENTS.md](../../AGENTS.md) (`fmt`→`generate`→`manifests`→`vet`→`lint`→`test`→`test-e2e`, e2e
sequential). **No metric merges without its doc row.** The dashboard JSON and the alert RULES are
Phase 4 for one reason: a panel or an alert written against a family that is still being designed is
a query nobody re-checks once it stops matching. The alert *sketches* in §8 are the specification
those rules are written from, not shipped rules.

0. **Attribution join — done.** Structured resolver result, `attribution_resolutions_total`,
   `attribution_resolution_wait_seconds`, `attribution_fact_events_total`,
   `attribution_fact_index_size`, `_index_evictions_total`, `_stream_gaps_total`,
   `_collection_degraded_total`, and the `commits_total{author_kind}` label change all ship today.
   Sites: [author_resolver.go](../../internal/watch/author_resolver.go),
   [fact_index.go](../../internal/queue/fact_index.go),
   [author_fact.go](../../internal/queue/author_fact.go),
   [branch_worker.go](../../internal/git/branch_worker.go).
1. **Attribution surface correction + the silent loss paths — done.** The §4.4 relabel (`tier` +
   `actor_kind`, `weak` → `latest` / `resource_version`, `event_kind` on the wait histogram), the
   four renames, `no_attribution_fact` on `audit_events_total`, the stream decode-error counter, the
   follower error counter and last-success gauge, and `attribution_transport_info` all ship, each
   with its recording site, a manual-reader unit test, and its
   [interpreting-metrics.md](../interpreting-metrics.md) row. The label break is written up in
   [`UPGRADING.md`](../UPGRADING.md). `AttributionResult` in
   [author_fact.go](../../internal/queue/author_fact.go) is the tier vocabulary — the split lives in
   the enum, not at the metric boundary, so the resolver names the tier it actually took. Sites:
   [author_resolver.go](../../internal/watch/author_resolver.go),
   [fact_index.go](../../internal/queue/fact_index.go),
   [fact_stream.go](../../internal/queue/fact_stream.go),
   [outcome.go](../../internal/audit/outcome/outcome.go),
   [audit_handler.go](../../internal/webhook/audit_handler.go).
2. **Watch ingestion + queue delay.** `watch_events_total`, `watch_restarts_total`,
   `watch_replay_seconds`, `watch_replay_objects`, `watch_recovery_total`, `watch_active`, and
   `watch_event_queue_seconds`. Sites:
   [target_watch.go](../../internal/watch/target_watch.go),
   [manager.go](../../internal/watch/manager.go). Ship Row 2.
3. **Relevance filter + git push health.** `watch_events_filtered_total`,
   `git_push_duration_seconds`, `git_push_conflicts_total`. Sites: the filter decision points on the
   watch-to-Git path, [git_atomic_push.go](../../internal/git/git_atomic_push.go), and
   [branch_worker.go](../../internal/git/branch_worker.go). Ship Row 3.
4. **Dashboard, alert rules, and adaptive grace.** Ship the Grafana JSON and the alert rules, and use
   per-type coverage to skip the grace wait for types audit never covers — no point delaying a watch
   event for a fact that never comes. The metric is the prerequisite; the optimization follows it.

## 10. Non-goals / risks

- **Not** cross-pod HA aggregation — single active replica today
  ([architecture.md → Operational Boundaries](../architecture.md#operational-boundaries)); metrics are
  per-pod and that's fine. The fact stream is per-replica-followed by design, so a follower gauge is
  a per-pod statement, not a cluster one.
- **Not** per-mutation history — watch collapses to current state across gaps; metrics count
  observations, not mutations.
- RV-based "watch lag" (how far behind the apiserver a watch is) is attractive but hard to compute
  honestly across types; deferred, not in scope.
- Do **not** reintroduce the retired body-join metrics (`audit_join_*`, `audit_official_gate_wait`,
  `parked`/`shallow_dropped` outcomes) or the v1 keyspace's `exact_deletecollection_item` — they
  belong to architectures that no longer exist.
- Do **not** subtract counters across populations. `written` minus `matched` is not delivery loss:
  `written` counts every fact appended for every type, while the follower files only facts on streams
  **this process follows**, and a restart re-reads the retention window and files the same facts
  again. Two counters over different populations do not subtract; delivery loss is measured where
  delivery happens, which is what §5.4 does.

## References

- [architecture.md](../architecture.md) — leading source of truth (esp. *Common Flows*,
  *Optional Attribution*, *State Ingestion*, *Observability*).
- [interpreting-metrics.md](../interpreting-metrics.md) — the live baseline + the per-metric doc bar.
- [spec/attribution.md](../spec/attribution.md) — the shipped attribution surface behind §4.4 and
  §5, how the two halves work, and the tier ladder the `tier` label names.
- [attribution-fact-stream.md](../finished/attribution-fact-stream.md) — the shipped transport, the
  in-process index, and the follower these metrics watch.
- [watch-first-ingestion-architecture.md](../finished/watch-first-ingestion-architecture.md) — the
  watch-first ingestion design and the earlier metric sketch §4.1 modernizes.
