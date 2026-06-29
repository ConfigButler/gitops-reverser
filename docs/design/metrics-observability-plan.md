# Metrics & Audit Observability — improvement plan

> Status: PROPOSAL — 2026-06-26. **Architecture-led**: [architecture.md](../architecture.md) is the
> spine; every metric below maps to a stage in
> [A Change, End to End](../architecture.md#a-change-end-to-end). The live baseline and the
> documentation bar come from [interpreting-metrics.md](../interpreting-metrics.md). This doc is the
> single canonical metrics plan — it supersedes the per-feature metric notes now in `finished/`.

## 1. Why now

After the June 2026 cleanup the live metric surface is honest but **lopsided**: it covers the *edges*
of the pipeline (Git write, discovery catalog, Secret encryption) and is **dark in the middle** —
the two stages that actually carry the product:

- **Watch ingestion** is the source of object state ([architecture.md → State Ingestion](../architecture.md#state-ingestion-and-not-losing-deletes))
  and has **no direct metrics at all**.
- **Attribution** — naming the author from audit — is the part you most want to watch, and today you
  can only see audit *arriving* (`audit_events_total`), not whether it actually *attributes a commit*.

The goal: make the whole watch-first pipeline observable end-to-end, with **first-class audit /
attribution visibility**, backed by a reference dashboard and a small set of alerts — so there are
serious, honest metrics to show, and the audit subsystem is glass-box.

### The questions the metrics must answer

- Is the operator turning cluster changes into commits right now? (liveness)
- **Is audit arriving, is it good, and is it actually putting real names on commits?** (the audit lens)
- When attribution *doesn't* land, why — no audit, weak match, expired fact, no Redis? (degradation)
- Are watches healthy, or thrashing on `410 Gone` / replays? (ingestion health)
- Is any object state being lost or stalled? (correctness / backpressure)

## 2. Principles

1. **Architecture is the spine.** Every metric maps to a named stage of
   [A Change, End to End](../architecture.md#a-change-end-to-end). No metric exists without a stage.
2. **Watch is the source of truth → instrument it first.** It currently has the least coverage and
   the most risk.
3. **Audit is optional attribution, never correctness.** Metrics measure attribution *coverage and
   quality*; the invariant "a missing/late fact changes the author, never the state"
   ([architecture.md → Optional Attribution](../architecture.md#optional-attribution)) must be
   *visible*, not just asserted.
4. **Every metric has a recording site and an interpretation.** The thing we just deleted —
   defined-but-never-recorded instruments — must never come back. A metric ships with its
   [interpreting-metrics.md](../interpreting-metrics.md) row (what it measures, one query, what a bad
   value looks like) in the *same* change.
5. **Label discipline.** Bounded cardinality only: `group`/`version`/`resource` (tens of
   claimed-and-followable types, not thousands), `verb` (~5), bounded `scope`
   (`namespace`/`cluster`), and frozen enum labels (`outcome`, `result`, `reason`). **Never** put an
   object's `name`/`namespace` in a label. Identity labels stay prefixed (`provider_*`, `gittarget_*`)
   to survive a `honor_labels=false` pod scrape — see the note in
   [exporter.go](../../internal/telemetry/exporter.go).
6. **Degradation is loud.** Committer fallback, absent Redis, `410` rebuilds, LIST fallback — each has
   a metric, so running in a degraded shape is a visible state, not a silent one.

## 3. Current state (the map)

| Pipeline stage (architecture.md) | Live metrics today | Coverage |
|---|---|---|
| Discovery & catalog | `api_catalog_resources`, `_group_versions`, `_refresh_total`, `_refresh_duration_seconds`, `_generation` | ✅ good |
| **Watch ingestion** | — | ❌ **none** |
| **Relevance filter** | — | ❌ **none** |
| **Attribution / audit** | `audit_eventlists_total`, `_eventlist_events_total`, `_eventlist_duration_seconds`, `audit_events_total{outcome,category,group,version,resource,verb}`, `attribution_resolutions_total`, `attribution_resolution_wait_seconds`, `attribution_fact_events_total`, `attribution_fact_index_size` | ✅ coverage and fact-index visibility |
| Git write | `commits_total{provider_*,branch,author_kind}`, `git_operations_total`, `objects_written_total`, `branch_worker_queue_depth`, `resync_sweep_deletes_total` | 🟡 no push latency / conflict |
| Control plane / reconcile | `target_reconcile_completed_total`, `resync_background_failures_total`, `watched_types` | ✅ good |
| Secret encryption | `secret_encryption_{attempts,success,failures,cache_hits,marker_skips}_total` | ✅ good |

## 4. Target metric model — by pipeline stage

New watch metrics use the shape already sketched in
[watch-first-ingestion-architecture.md → Metrics](watch-first-ingestion-architecture.md), but this
plan modernizes type labels to the live audit convention: separate `group`, `version`, and
`resource` labels instead of a packed `gvr` string. Keep `version` on watch metrics even though it
adds some series: Git paths and audit metrics already treat version as part of the resource identity,
and a served-version migration should be visible rather than silently folded into the old series.
Attribution metric names are also made more explicit now, before production users depend on them.

### 4.1 Watch ingestion (new — the biggest hole)

| Metric | Type | Labels | Answers |
|---|---|---|---|
| `watch_events_total` | counter | `group`, `version`, `resource`, `scope`, `type` (added/modified/deleted/bookmark), `outcome` (applied/filtered/dropped) | watch volume and where events go |
| `watch_restarts_total` | counter | `group`, `version`, `resource`, `scope`, `reason` (`410_gone`/`disconnect`/`rule_change`) | watch stability / `410` pressure |
| `watch_replay_seconds` | histogram | `group`, `version`, `resource`, `scope` | time to `initial-events-end` — resume cost |
| `watch_replay_objects` | histogram | `group`, `version`, `resource`, `scope` | replay size (how much state is re-walked) |
| `watch_recovery_total` | counter | `group`, `version`, `resource`, `mode` (`cursor_resume`/`replay`/`list_fallback`) | which recovery path fires — cursor effectiveness vs aggregated-API fallback |
| `watch_active` | gauge | `group`, `version`, `resource`, `scope` | open watch goroutines vs claimed set |

Phase 2 adds the bookkeeping needed for `watch_active`; it is not just a recording call. Count
bookmarks at the session receive point before `targetWatchEventResourceVersion` swallows them into
cursor progress.

### 4.2 Relevance filter (new)

| Metric | Type | Labels | Answers |
|---|---|---|---|
| `watch_events_filtered_total` | counter | `group`, `version`, `resource`, `reason` (`sanitized_noop`/`status_only`/`not_followable`/`duplicate`) | is the product-side filter behaving, or masking real changes? A mis-tuned filter is visible, per [watch-first](watch-first-ingestion-architecture.md). |

The reason set is the target shape, not a claim that one chokepoint exists today. Phase 3 first
locates or consolidates the scattered filter decisions on the watch-to-Git path, then records the
metric at the smallest honest boundary.

### 4.3 Attribution / audit — **the centerpiece** (§5 expands this)

| Metric | Type | Labels | Answers |
|---|---|---|---|
| `audit_events_total` *(have)* | counter | `outcome`, `category`, `group`, `version`, `resource`, `verb` | every audit event by fate (`queued` = attribution fact written) |
| `audit_eventlists_total` / `_eventlist_events_total` / `_eventlist_duration_seconds` *(have)* | counter/hist | `outcome` | the `/audit-webhook` request boundary |
| `attribution_resolutions_total` | counter | `result` (`exact_user`/`exact_serviceaccount`/`weak`/`conflict`/`absent`/`expired`), `group`, `version`, `resource` | **does attribution actually land, per type** |
| `attribution_resolution_wait_seconds` | histogram | `result` | grace-window latency cost (`--author-attribution-grace` tuning) |
| `attribution_fact_events_total` | counter | `op` (`written`/`matched`/`expired_unmatched`/`late`) | fact-index lifecycle — written vs joined vs wasted |
| `attribution_fact_index_size` | gauge | — | facts currently parked in Redis awaiting a join |
| `commits_total` *(have, + intentional label change)* | counter | `provider_*`, `branch`, **`author_kind`** (`user`/`serviceaccount`/`committer`) | **what fraction of commits carry a real name** |

### 4.4 Git write (new additions to a covered stage)

| Metric | Type | Labels | Answers |
|---|---|---|---|
| `git_push_duration_seconds` | histogram | `provider_*`, `branch` | push latency (re-added with a recording site and doc row) |
| `git_push_conflicts_total` | counter | `provider_*`, `branch` | non-fast-forward → fetch/reset/replay retries ([PushAtomic](../../internal/git/git_atomic_push.go) detects a moved remote; [BranchWorker](../../internal/git/branch_worker.go) fetches, rebuilds, and retries) |
| `resync_sweep_deletes_total` | counter | `group`, `version`, `resource` | deletes produced by mark-and-sweep resyncs; steady-state watch deletes use per-event delete-document writes, not sweeps |

### 4.5 Catalog, reconcile, secrets

Keep as-is (✅ above). One small add:
`watch_set_changes_total{gittarget_namespace,gittarget_name,op=open/close}` to see watch churn when
rules/CRDs change (pairs with `target_reconcile_completed_total{trigger=rule_change}`).

## 5. Deep dive: audit & attribution observability

This is the subsystem you want glass-box. The model
([architecture.md → Optional Attribution](../architecture.md#optional-attribution)):

```
kube-apiserver --POST--> /audit-webhook --gate--> write attribution fact (Redis, TTL)
                                                          |
watch event ---------> resolver waits up to --author-attribution-grace --> join by RV/UID
                                                          |
                                  strong match -> user/sa author ; else -> committer
```

Three lenses, each a dashboard question:

**(a) Is audit arriving and well-formed?** — `audit_eventlists_total{outcome}` (delivery),
`audit_eventlist_duration_seconds` (latency), `audit_events_total{category}` (per-event fate; `error`
must be 0). This is the ingress half we already have.

**(b) Is it good enough to attribute?** — `attribution_resolutions_total{result,group,version,resource}`
is the new heart. Phase 1 first changes the `AttributionLookup` / `AuthorResolver` path from a
boolean hit/miss to a structured resolution result, so the metric records facts the code truly knows
instead of inferring them after the fact. It splits every resolved watch event into
`exact_user`, `exact_serviceaccount`, `weak`, `conflict`, `absent`, or `expired` **per type**:

| Result | Meaning | Work needed |
|---|---|---|
| `exact_user` | exact UID+resourceVersion match for a human user | structured result |
| `exact_serviceaccount` | exact UID+resourceVersion match for a service account, named by its own username | structured result |
| `weak` | non-exact match, such as UID-only or RV-only | define and expose match strength |
| `conflict` | multiple candidate authors share a join key | detect collisions while recording or looking up facts |
| `expired` | a fact existed but aged out before the watch event joined it | add tombstone or last-seen evidence; Redis TTL alone is silent |
| `absent` | no matching fact and no evidence that one expired | structured miss result |

Match coverage = `(exact_user + exact_serviceaccount) / all` — the share of events that named an actor
(human or service account) rather than falling back to the committer. Real-name coverage is also shown by
`commits_total{author_kind}` below.

**(c) Are real names actually landing in Git?** — `commits_total{author_kind}`. This is the bottom
line: a wall of `author_kind="committer"` means attribution is effectively off even if audit is
flowing. Pair with `attribution_resolution_wait_seconds` to see whether the grace window is paying
for itself. Adding `author_kind` intentionally changes the existing `commits_total` label contract
before production users depend on it; record it per created commit, so mixed-author batches do not
collapse into a misleading aggregate.

**Fact-index health** — `attribution_fact_events_total{op}` and `attribution_fact_index_size` show the
Redis side: facts written vs actually matched vs `expired_unmatched` (wasted writes) vs `late` (arrived
after the commit shipped — never rewrites, per the invariant, but worth seeing). A high
`expired_unmatched` rate with low coverage is the signature of an audit/watch RV mismatch.

**Live firehose for investigation** — for "watch what's happening right now," metrics aggregate; the
**opt-in `diag_all` stream** from
[audit-diagnostic-streams-plan.md](stream/audit-diagnostic-streams-plan.md) is the complement: a
bounded, default-off Redis stream holding annotated records for queue-reaching audit events, enabled
only while investigating. Keep/finish it as the deep-dive tool behind the dashboard — metrics tell you
*that* attribution dropped; `diag_all` tells you *which event and why* for events it captures, while
pre-queue drops stay covered by `audit_events_total` and the existing raw debug stream.

**Adaptive payoff** — once `attribution_resolutions_total{result="absent",group,version,resource}` and
a per-type last-seen signal exist, the resolver can **skip the grace wait for types audit never
covers** (no point delaying a watch event 3s for a fact that never comes). The metric is the
prerequisite; the optimization is a Phase 4 follow-up.

## 6. The reference dashboard

Grafana, one dashboard, top-down. The **Audit & Attribution** row is the marquee. PromQL is given so a
panel is copy-pasteable.

**Row 0 — SLO header (stat panels):**
- Commit rate: `sum(rate(gitopsreverser_commits_total[5m]))`
- Audit errors (must be 0): `sum(rate(gitopsreverser_audit_events_total{category="error"}[5m]))`
- Attribution match coverage %: `sum(rate(gitopsreverser_attribution_resolutions_total{result=~"exact_.*"}[5m])) / sum(rate(gitopsreverser_attribution_resolutions_total[5m]))`
- Push latency p95: `histogram_quantile(0.95, sum by (le)(rate(gitopsreverser_git_push_duration_seconds_bucket[5m])))`
- Max worker queue depth: `max(gitopsreverser_branch_worker_queue_depth)`

**Row 1 — AUDIT & ATTRIBUTION (marquee):**
- *Live audit stream by type* (timeseries): `sum by (group,version,resource)(rate(gitopsreverser_audit_events_total[1m]))`
- *Audit outcome mix* (stacked): `sum by (category,outcome)(rate(gitopsreverser_audit_events_total[5m]))`
- *Attribution match coverage by type* (timeseries): `sum by (group,version,resource)(rate(gitopsreverser_attribution_resolutions_total{result=~"exact_.*"}[5m])) / sum by (group,version,resource)(rate(gitopsreverser_attribution_resolutions_total[5m]))`
- *Commit author mix* (pie/stacked): `sum by (author_kind)(rate(gitopsreverser_commits_total[15m]))`
- *Grace-window wait p95 by result* (timeseries) with an `--author-attribution-grace` threshold line: `histogram_quantile(0.95, sum by (le,result)(rate(gitopsreverser_attribution_resolution_wait_seconds_bucket[5m])))`
- *Fact-index health* (timeseries): `sum by (op)(rate(gitopsreverser_attribution_fact_events_total[5m]))` + `gitopsreverser_attribution_fact_index_size`
- *Top dropped audit outcomes* (table): `topk(10, sum by (resource,verb,outcome)(rate(gitopsreverser_audit_events_total{category="dropped"}[5m])))`
- *EventList ingress + decode errors* (timeseries): `sum by (outcome)(rate(gitopsreverser_audit_eventlists_total[5m]))`

**Row 2 — WATCH INGESTION:**
- Events/sec by type: `sum by (group,version,resource,type)(rate(gitopsreverser_watch_events_total[5m]))`
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

> The dashboard ships as JSON under `docs/dashboards/` (or the chart) so it is versioned with the code.
> I can generate the Grafana JSON once Phase 1 metrics exist.

## 7. Cardinality & cost

- `group`/`version`/`resource` is bounded by **claimed ∩ followable** types (tens), `verb` ~5,
  `scope` is a bounded enum (`namespace`/`cluster`), and all other labels are frozen enums
  (`result` 7, `outcome` ~10, `category` 4, `author_kind` 3, `mode` 3). Worst case is a few thousand
  series total — comfortable for Prometheus.
- **No object identity in labels** (no `name`/`namespace` of watched objects) — that is the only thing
  that would blow up cardinality, and it is forbidden by principle 5.
- Histograms reuse shared bucket sets (sub-second→minutes), as today.
- `diag_all` is **opt-in and `MAXLEN`-bounded**; off in prod by default.

## 8. Alerts / SLOs

| Alert | Expression (sketch) | Meaning |
|---|---|---|
| Audit fact-store errors | `rate(audit_events_total{outcome="write_error"}[10m]) > 0` | Redis fact writes failing |
| Attribution match coverage drop | match coverage `< 0.5` for 30m while audit flowing | facts stopped matching watch events |
| Grace window saturating | `attribution_resolution_wait_seconds{result=~"absent|expired"}` p95 → `--author-attribution-grace` | misses are waiting the full grace; raise grace or skip never-attributed types |
| Watch restart storm | `rate(watch_restarts_total{reason="410_gone"}[15m])` spike | RV churn / compaction pressure |
| List fallback in use | `rate(watch_recovery_total{mode="list_fallback"}[1h]) > 0` | an aggregated API isn't honoring streaming list |
| Worker backing up | `branch_worker_queue_depth` rising, not draining | stalled remote |
| Degraded API surface | `api_catalog_group_versions{state="degraded"} > 0` | broken APIService |

## 9. Implementation phases

Each phase ships: recording sites → unit tests (manual-reader assertions) → `interpreting-metrics.md`
rows → dashboard panels → alerts, validated per [AGENTS.md](../../AGENTS.md) (`fmt`→`generate`→
`manifests`→`vet`→`lint`→`test`→`test-e2e`, e2e sequential). **No metric merges without its doc row.**

1. **Attribution (implemented).** Structured resolver result,
   `attribution_resolutions_total`, `attribution_resolution_wait_seconds`,
   `attribution_fact_events_total`, `attribution_fact_index_size`, the intentional
   `commits_total{author_kind}` label-contract change, and `resync_sweep_deletes_total`. Recording sites:
   [author_resolver.go](../../internal/watch/author_resolver.go),
   [attribution_index.go](../../internal/queue/attribution_index.go),
   [branch_worker.go](../../internal/git/branch_worker.go), and
   [resync_flush.go](../../internal/git/resync_flush.go). Ship dashboard Row 0 + Row 1. *This alone gives
   serious, demoable audit metrics.*
2. **Watch ingestion.** `watch_events_total`, `watch_restarts_total`, `watch_replay_seconds`,
   `watch_recovery_total`, `watch_active`. Sites:
   [target_watch.go](../../internal/watch/target_watch.go), [manager.go](../../internal/watch/manager.go).
   Ship Row 2.
3. **Relevance filter + git push health.** `watch_events_filtered_total`,
   `git_push_duration_seconds`, `git_push_conflicts_total`. Sites:
   filter decision points on the watch-to-Git path, [git_atomic_push.go](../../internal/git/git_atomic_push.go),
   and [branch_worker.go](../../internal/git/branch_worker.go). Ship Row 3.
4. **Firehose + adaptive grace.** Finish opt-in `diag_all`; use per-type coverage to skip the grace
   wait for never-attributed types; ship the dashboard JSON and alert rules.

## 10. Non-goals / risks

- **Not** cross-pod HA aggregation — single active replica today
  ([architecture.md → Operational Boundaries](../architecture.md#operational-boundaries)); metrics are
  per-pod and that's fine.
- **Not** per-mutation history — watch collapses to current state across gaps; metrics count
  observations, not mutations.
- RV-based "watch lag" (how far behind the apiserver a watch is) is attractive but hard to compute
  honestly across types; deferred, not in scope.
- Keep `diag_all` opt-in/bounded — a full audit firehose in prod would bloat Redis.
- Do **not** reintroduce the retired body-join metrics (`audit_join_*`, `audit_official_gate_wait`,
  `parked`/`shallow_dropped` outcomes) — they belong to an architecture that no longer exists.

## References

- [architecture.md](../architecture.md) — leading source of truth (esp. *A Change, End to End*,
  *Optional Attribution*, *State Ingestion*, *Observability*).
- [interpreting-metrics.md](../interpreting-metrics.md) — the live baseline + the per-metric doc bar.
- [watch-first-ingestion-architecture.md](watch-first-ingestion-architecture.md) — the watch-first
  ingestion design and the earlier metric sketch this plan modernizes.
- [audit-diagnostic-streams-plan.md](stream/audit-diagnostic-streams-plan.md) — the `audit_events_total`
  outcome census and the opt-in `diag_all` firehose.
