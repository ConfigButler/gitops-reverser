# Interpreting GitOps Reverser Metrics

> Last updated: July 2026 — reconciled to the live instrument set in
> [`internal/telemetry/exporter.go`](../internal/telemetry/exporter.go).

This is the operator's field guide to the metrics GitOps Reverser exports. It explains how to
read each metric family and gives copy-pasteable PromQL for the questions operators actually
ask.

Every metric documented here has a real recording site in the code. If you find a metric in a
dashboard that is not listed here, it was removed — see [Known gaps](#known-gaps-not-yet-emitted)
for the areas that are deliberately not instrumented yet.

---

## Where metrics come from

GitOps Reverser exports Prometheus-format metrics via the controller-runtime metrics server.
The bind address is `servers.metrics.bindAddress` (default `:8080`); metrics are served at
`/metrics`.

All metric names are prefixed `gitopsreverser_`. Throughout this document the prefix is
omitted in prose but kept in queries.

Three instrument shapes appear:

| Shape | Suffix | How to read it |
| --- | --- | --- |
| **Counter** | `_total` | Monotonic. Always wrap in `rate(...[5m])` or `increase(...[1h])` — the raw value is meaningless. |
| **Histogram** | `_seconds` | Exposes `_bucket`, `_sum`, `_count`. Use `histogram_quantile()` for percentiles; `_count` is a free counter of observations. |
| **Gauge** | (none) | Instantaneous value; read directly. |

### Reading a histogram

A histogram named `foo_seconds` produces three series:

- `foo_seconds_bucket{le="..."}` — cumulative count per bucket boundary
- `foo_seconds_count` — total number of observations (use it like a counter)
- `foo_seconds_sum` — sum of all observed values

Percentile:

```promql
histogram_quantile(0.95, sum by (le) (rate(gitopsreverser_foo_seconds_bucket[5m])))
```

Mean:

```promql
rate(gitopsreverser_foo_seconds_sum[5m]) / rate(gitopsreverser_foo_seconds_count[5m])
```

---

## What is instrumented today

Object state is ingested by **watch**; **audit is an optional attribution lookup** that only
names the author of a watch-observed change (see
[architecture.md → Optional Attribution](architecture.md#optional-attribution)). The metric
coverage reflects that split, with one important caveat: the **watch ingestion path itself is
only lightly instrumented today** — most of the live metrics sit at the Git-write and
discovery edges. The deliberately-uncovered areas are listed under
[Known gaps](#known-gaps-not-yet-emitted) so a blank dashboard panel is never mistaken for a
healthy zero.

The live metric families are: **Git write & reconcile**, **Audit attribution**, **API resource
catalog**, and **Secret encryption**.

---

## Git write & reconcile

The path from a watch-observed change to a pushed commit, plus the per-GitTarget reconcile
signals. Background: [architecture.md → Git Write Architecture](architecture.md#git-write-architecture).

| Metric | Type | Labels | Notes |
| --- | --- | --- | --- |
| `commits_total` | counter | `provider_namespace`, `provider_name`, `branch`, `author_kind` | Commit batches pushed. Both the per-event and backfill-resync paths feed this one counter. |
| `git_operations_total` | counter | — | Events that produced Git work in a flush. |
| `objects_written_total` | counter | — | Objects that resulted in a file write in a flush. |
| `resync_sweep_deletes_total` | counter | `group`, `version`, `resource` | Managed documents deleted by mark-and-sweep resyncs. Steady-state watch deletes do not increment this. |
| `branch_worker_queue_depth` | gauge | `provider_namespace`, `provider_name`, `branch` | Pending + in-flight + committed-but-unpushed work; reads 0 only when the worker has fully drained. |
| `target_reconcile_completed_total` | counter | `gittarget_namespace`, `gittarget_name`, `trigger` | One increment per completed watch-recovery pass (streaming-snapshot resync applied, or cursor-backed resume). |
| `resync_background_failures_total` | counter | `gittarget_namespace`, `gittarget_name` | Rule-change resyncs whose apply failed/timed out **after** enqueue (otherwise only logged). |
| `watched_types` | gauge | `gittarget_namespace`, `gittarget_name` | How many concrete types a GitTarget currently watches. |

`commits_total` carries the **`BranchWorker`'s**
`{provider_namespace, provider_name, branch, author_kind}` identity, not a GitTarget: one worker can
serve several GitTargets sharing a provider+branch, coalescing their writes into one commit batch, so
the worker is the honest attribution unit. `author_kind` is `user`, `serviceaccount`, `committer`, or `unresolved`;
reconcile/resync commits and configured-author mode use `committer`.

**`unresolved` is the one to watch.** It means attribution RAN and did not name an actor, so
the commit carries the `unknown (attribution unresolved)` author instead of a person. It is
deliberately not folded into `user` (which would make a lost actor look like a named one, so a
degrading attribution path would read as an improving one) nor into `committer` (which would
hide it among legitimately unattributed reconcile writes). For a live mutation that should be
attributable, a non-zero and growing `unresolved` share means the audit-attribution configuration
or delivery path needs investigation. The namespace/name keys are
**prefixed on purpose** — a Prometheus pod scrape with
`honor_labels=false` overwrites a bare `namespace` attribute with the scraping pod's namespace,
so a per-provider `namespace` selector would silently match nothing. The same reasoning applies to
`target_reconcile_completed_total` and `branch_worker_queue_depth`.

**Commit rate per provider/branch:**

```promql
sum by (provider_namespace, provider_name, branch) (rate(gitopsreverser_commits_total[5m]))
```

**Are real names landing in Git?** A wall of `author_kind="committer"` means the Git history is not
showing human or named service-account authors, even if audit is flowing:

```promql
sum by (author_kind) (rate(gitopsreverser_commits_total[15m]))
```

**Is a branch worker backing up?** A persistently rising gauge indicates a stalled remote:

```promql
gitopsreverser_branch_worker_queue_depth
```

**Did a new pod redo its reconciles after a rollout?** `target_reconcile_completed_total` is a
counter (not a latched gauge) precisely so a fresh pod's series starts at 0; a per-pod
`increase(...) > 0` proves the new pod did its own work rather than inheriting the old pod's
stale series. This is the restart-reconcile guarantee:

```promql
sum by (pod) (increase(gitopsreverser_target_reconcile_completed_total[10m]))
```

**Are background resyncs silently failing?** Should be zero; non-zero means snapshots are not
committing and the folder is relying on steady-state events to catch up:

```promql
sum by (gittarget_namespace, gittarget_name) (
  rate(gitopsreverser_resync_background_failures_total[15m]))
```

**How much is each GitTarget watching?**

```promql
gitopsreverser_watched_types
```

**Are resyncs sweeping resources out of Git?** Non-zero is expected after a resource disappears from
the cluster and a scoped/full resync applies. This is not the steady-state delete path:

```promql
sum by (group, version, resource) (rate(gitopsreverser_resync_sweep_deletes_total[1h]))
```

---

## Audit attribution (optional)

Audit runs when `--author-attribution` is on. The kube-apiserver POSTs audit `EventList` payloads to
`/audit-webhook`; the handler applies an intrinsic accept gate and appends the accepted events'
facts to a per-type fact log — **one append per type per request**, not one per event. The watch side
follows that log into a bounded, TTL'd in-memory index and joins against it. There is **no body join
and no second source** — watch, not audit, carries the object body — so the only audit metrics are
the request boundary and the per-event census.

The log is Redis Streams by default and an in-process ring with
`--author-attribution-transport=memory`, which is why attribution no longer implies Redis. Background:
[architecture.md → Optional Attribution](architecture.md#optional-attribution).

| Metric | Type | Labels |
| --- | --- | --- |
| `audit_eventlists_total` | counter | `outcome` |
| `audit_eventlist_events_total` | counter | `outcome` |
| `audit_eventlist_duration_seconds` | histogram | `outcome` |
| `audit_events_total` | counter | `outcome`, `category`, `group`, `version`, `resource`, `verb` |
| `attribution_resolutions_total` | counter | `tier`, `actor_kind`, `group`, `version`, `resource` |
| `attribution_resolution_wait_seconds` | histogram | `tier`, `event_kind`, `group`, `version`, `resource` |
| `attribution_facts_total` | counter | `op` |
| `attribution_fact_index_entries` | gauge | — |
| `attribution_fact_index_evictions_total` | counter | `reason` |
| `attribution_fact_stream_gaps_total` | counter | `stream` |
| `attribution_fact_stream_decode_errors_total` | counter | `transport` |
| `attribution_fact_follower_errors_total` | counter | `transport` |
| `attribution_fact_follower_last_success_timestamp_seconds` | gauge | — |
| `attribution_collection_without_uidset_total` | counter | `reason` |
| `attribution_transport_info` | gauge (always 1) | `transport` |

**EventList request boundary.** `audit_eventlists_total` and `audit_eventlist_duration_seconds`
count requests at `/audit-webhook`; `audit_eventlist_events_total` counts the decoded event items
inside them. `outcome` is bounded: `processed`, `empty`, `decode_error`, `process_error`. This is
the raw delivery edge — "are EventLists arriving at all?" — before any gate or attribution logic.

**Per-event census.** `audit_events_total` increments exactly once per decoded event. `category`
is the coarse bucket of `outcome` (carried as its own label so the health invariant is a simple
selector):

| `category` | Live `outcome` values | Meaning |
| --- | --- | --- |
| `stored` | `queued` | Accepted; the event's facts reached the fact log. |
| `dropped` | `nil_event`, `stage`, `read_only_or_unknown_verb`, `failed_request`, `dry_run`, `unchanged_resource_version`, `non_scale_subresource`, `no_attribution_fact` | Correctly rejected at the accept gate, or accepted and unable to name an author — not an error. |
| `error` | `write_error` | The transport rejected the append for THIS event's stream, so its fact did not reach the log. Publication is per stream, so a request that fails partway still counts the events whose own stream appended as `queued`; the whole request is failed so the API server retries it, and the landed facts are simply appended again. The one category that should stay zero. |

The full enum lives in [`internal/audit/outcome/outcome.go`](../internal/audit/outcome/outcome.go)
— it is the source of truth.

**`no_attribution_fact` is the one to read carefully.** The event was accepted and could name no
author, so nothing was appended for it and no watch event can ever join it. The usual cause is an
aggregated API: the kube-apiserver proxies the request and never decodes the response, so a CREATE's
`objectRef` carries no name at all. It is `dropped` rather than `error` because nothing failed, and
this is the only place it can be counted — the event never becomes a fact, so no fact-side counter
sees it. A rising share for a type you expect to attribute means commits for that type will be
authored unresolved:

```promql
sum by (group, resource) (rate(gitopsreverser_audit_events_total{outcome="no_attribution_fact"}[5m]))
```

**Is audit attribution alive?** Any positive rate means events are flowing:

```promql
sum(rate(gitopsreverser_audit_events_total[5m]))
```

**The health invariant — fact-store errors must be zero:**

```promql
sum(rate(gitopsreverser_audit_events_total{category="error"}[5m]))
```

**Live audit stream by type — what is actually streaming in.** This is the per-type view of the
audit firehose; a type you expect to attribute but never see here means the audit policy is not
delivering it:

```promql
sum by (group, version, resource) (rate(gitopsreverser_audit_events_total[5m]))
```

**Have we ever seen audit for this type?** Useful for deciding whether attribution is worth
waiting on for a given type — zero over a long window means audit never delivers it:

```promql
sum by (group, resource) (increase(gitopsreverser_audit_events_total[1h])) > 0
```

**What strange or high-volume traffic is the webhook receiving?** Top event shapes by outcome —
surfaces an unexpected flood at a glance:

```promql
topk(15, sum by (resource, verb, outcome) (rate(gitopsreverser_audit_events_total[5m])))
```

A non-`/scale` subresource (`exec`, `status`, `log`, …) cannot describe a top-level object the
Git pipeline mirrors, so it is dropped at the gate as `outcome="non_scale_subresource"`. A
`pods/exec` flood shows up as exactly that (with `resource="pods"`) rather than looking like real
pod mutations:

```promql
topk(10, sum by (resource, verb) (
  rate(gitopsreverser_audit_events_total{outcome="non_scale_subresource"}[5m])))
```

**Are EventLists failing to decode?** Should be zero — non-zero means a sender is posting
something that is not an `audit.k8s.io/v1 EventList`:

```promql
sum(rate(gitopsreverser_audit_eventlists_total{outcome="decode_error"}[5m]))
```

**How long does the webhook take to answer?** p95 of the EventList handling time:

```promql
histogram_quantile(0.95,
  sum by (le) (rate(gitopsreverser_audit_eventlist_duration_seconds_bucket[5m])))
```

**Does audit actually attribute watch events?** Match coverage is the share of resolutions that named an
actor (human or service account) rather than producing an explicit unresolved author:

```promql
sum(rate(gitopsreverser_attribution_resolutions_total{tier!="absent"}[5m]))
/
sum(rate(gitopsreverser_attribution_resolutions_total[5m]))
```

Coverage is `tier!="absent"` and nothing narrower. `tier=~"exact.*"` reads the collection and name
tiers as misses, which they are not — they named an actor.

The two labels answer two different questions. **`tier`** names which evidence produced the author,
and it is ordered, strongest first. **`actor_kind`** names who that evidence named, in the same
vocabulary `commits_total{author_kind}` uses.

| `tier` | Meaning |
| --- | --- |
| `exact` | Exact UID+resourceVersion match: this actor produced this exact version. |
| `collection_uid` | A removal whose UID was in the set the API server said a `deletecollection` deleted. No over-attribution risk: either the object was in that set or it was not. It outranks `latest`, because `latest` names whoever last *wrote* an object while a removal asks who *deleted* it. |
| `latest` | The UID-latest tier: the object's own last fact, keyed by UID alone. A removal consults it, and a match here describing a *write* is held as a fallback while the wait continues for evidence about the deletion. |
| `name` | A match on `(namespace, name)` for a fact carrying neither a UID nor a resourceVersion — the usual shape of an aggregated API's audit event, and of a delete the API server answered with a `Status`. |
| `collection_scope` | A removal matched to a `deletecollection` by scope alone — same type and namespace, the request's selector accepting the object's labels, within the collection window. The weakest evidence the join has, and the only one that can name the wrong actor, which is why it is reached only when every more specific tier missed. |
| `resource_version` | The RV-only escape hatch: a fact that carried a resourceVersion and no UID, matched on that version alone. |
| `absent` | No usable fact matched before the grace window elapsed. The resulting live commit is authored as `unknown (attribution unresolved)`. |

| `actor_kind` | Meaning |
| --- | --- |
| `user` | A human (or any non-service-account subject). |
| `serviceaccount` | A named service account — a controller, an operator, a CI identity. |
| `none` | Nobody was named: nothing matched, or the fact that matched carried no author. |

**Evidence quality, independently of coverage.** A shift from `exact` toward `collection_scope` or
`name` is a quality regression even while coverage holds flat, so it is worth its own panel:

```promql
sum by (tier) (rate(gitopsreverser_attribution_resolutions_total[5m]))
```

> **`result` is gone**, and so are `exact_user`, `exact_serviceaccount`, and `weak`. See
> [`UPGRADING.md`](UPGRADING.md) for the old-to-new mapping. `exact_deletecollection_item` went
> earlier, with the expander and the fact keyspace; `collection_uid` is its closest equivalent and
> `collection_scope` is new capability rather than a rename. See
> [`attribution-fact-stream.md`](finished/attribution-fact-stream.md).

**Is the grace window paying for itself?** `event_kind` is `write` or `removal`, and the split is
the point: a removal holds a fallback and keeps waiting for evidence about the deletion, a write
does not. The removal wait is the number `--author-attribution-grace` is tuned from, and a p95
approaching the configured grace means removals are sitting out the whole window:

```promql
histogram_quantile(0.95,
  sum by (le, tier) (
    rate(gitopsreverser_attribution_resolution_wait_seconds_bucket{event_kind="removal"}[5m])))
```

**Is the fact index healthy?** Facts should be written and later matched; a high rate of
`op="written"` alongside `tier="absent"` points at a timing, type, or audit-route mismatch between
audit and watch. The two ops are **not subtractable**: `written` counts every type, `matched` only
the streams this process follows, and a restart re-files the whole retention window.

```promql
sum by (op) (rate(gitopsreverser_attribution_facts_total[5m]))
```

```promql
gitopsreverser_attribution_fact_index_entries
```

**Is the index dropping facts under load?** The index is bounded per type and in total, and an
attribution lost to a full index has to look different from one that was never published. Any
sustained rate here means a burst is outrunning the caps, and
`--author-attribution-max-facts-per-type` / `--author-attribution-max-facts` are the levers:

```promql
sum by (reason) (rate(gitopsreverser_attribution_fact_index_evictions_total[5m]))
```

`reason` is `per_type` (one type is hotter than its share) or `total` (the whole index is under
pressure, and eviction falls on the type holding the most).

**Is a follower losing facts?** A trim gap means the fact log was trimmed past this process's
position: the facts in the gap are gone for good, and the commits that needed them are authored
unresolved. It is the one loss the transport can see, which is why the transport is a log with
positions rather than fire-and-forget publish and subscribe. This should be **zero**:

```promql
sum by (stream) (rate(gitopsreverser_attribution_fact_stream_gaps_total[5m]))
```

**Is a follower silently skipping facts?** An entry the follower cannot decode is skipped and its
position passed — which is right, since it can never decode and stalling on it would cost every
later fact on that stream — so the facts it carried are gone. This is the loss path with **no other
symptom**: unlike a trim gap it is not detectable after the fact, and unlike a publish failure the
API server does not retry it. Any non-zero rate means a schema or version mismatch on the stream:

```promql
sum by (transport) (rate(gitopsreverser_attribution_fact_stream_decode_errors_total[5m]))
```

**Is the fact follower alive?** The follower retries a transport failure with a backoff rather than
returning, so a wedged one degrades attribution to committer-authored across the board with a rising
unresolved rate as the only symptom. The timestamp matters more than the counter: it is the only
thing that separates "erroring occasionally while making progress" from "has read nothing in ten
minutes", and only the second is an outage. Read it as seconds since the last successful read
(idle rounds count as reads, so a quiet cluster reads as healthy):

```promql
time() - gitopsreverser_attribution_fact_follower_last_success_timestamp_seconds
```

```promql
sum by (transport) (rate(gitopsreverser_attribution_fact_follower_errors_total[5m]))
```

**Which transport is in force?** `gitopsreverser_attribution_transport_info` is an info gauge whose
value is always 1, labelled `redis` or `memory`. It is a legend rather than a threshold, and it
changes how every metric above reads: a burst of unresolved commits after a restart is *expected*
under the in-process transport, which drops every fact with the process, and a *bug* under Redis.

```promql
gitopsreverser_attribution_transport_info
```

**How often does a collection delete fall back to scope matching?** A `deletecollection` fact
carries the UIDs the API server named, when it sent them, and joins by membership. When it cannot,
the join falls back to `collection_scope`, which is correct but weaker — so the fallback is counted
rather than inferred:

```promql
sum by (reason) (rate(gitopsreverser_attribution_collection_without_uidset_total[5m]))
```

`reason` is `uid_cap` (the set was larger than `--author-attribution-collection-uid-cap`) or
`no_uids` (the API server sent a body with no usable UIDs). A body-less response — a truncated,
aggregated, or metadata-only `deletecollection` — produces no fact-level degradation event at all,
because there was never a set to drop; it simply resolves through the scope tier. A production
cluster running `--audit-webhook-truncate-enabled` is the one most likely to be in that case.

---

## API resource catalog

The API resource catalog is GitOps Reverser's single trusted in-memory view of the cluster's
served API surface — every `WatchRule` and `ClusterWatchRule` is resolved against it. The watch
manager refreshes it from Kubernetes discovery on its 30 s reconcile ticker, on every
CRD/APIService change, and on every rule change.

| Metric | Type | Labels |
| --- | --- | --- |
| `api_catalog_resources` | gauge | `state` (`allowed`/`excluded`) |
| `api_catalog_group_versions` | gauge | `state` (`trusted`/`degraded`) |
| `api_catalog_refresh_total` | counter | `outcome` (`changed`/`unchanged`/`error`) |
| `api_catalog_refresh_duration_seconds` | histogram | — |
| `api_catalog_generation` | gauge | — |

`excluded` resources are the default-watch-policy set (pods, events, leases, jobs, …) — served
by the cluster but deliberately never watched. `degraded` group/versions are ones discovery
reported as failed, usually a broken aggregated APIService.

**How many resources is GitOps Reverser actually willing to watch?**

```promql
gitopsreverser_api_catalog_resources{state="allowed"}
```

**Is the 30 s refresh doing real work, or just confirming a stable surface?** A healthy cluster
sits almost entirely on `unchanged`. A steady `changed` rate means part of the API surface is
flapping, and each change re-runs informer reconciliation:

```promql
sum by (outcome) (rate(gitopsreverser_api_catalog_refresh_total[15m]))
```

**Is part of the API surface hidden behind a broken APIService?** Should be zero:

```promql
gitopsreverser_api_catalog_group_versions{state="degraded"} > 0
```

**Discovery latency** — catches a slow or non-aggregated apiserver. Normal is single-digit
milliseconds; the call is two cached GETs on Kubernetes ≥ 1.27:

```promql
histogram_quantile(0.95,
  rate(gitopsreverser_api_catalog_refresh_duration_seconds_bucket[5m]))
```

---

## Secret encryption

Background: [architecture.md → Bootstrap, Encryption, and Signing](architecture.md#bootstrap-encryption-and-signing).
Secrets are never committed in plaintext; these metrics confirm the encryption path is healthy.

| Metric | Type | Notes |
| --- | --- | --- |
| `secret_encryption_attempts_total` | counter | Total encryption attempts. |
| `secret_encryption_success_total` | counter | Successful encryptions. |
| `secret_encryption_failures_total` | counter | Failed encryptions (the write is rejected). |
| `secret_encryption_cache_hits_total` | counter | Reused already-encrypted content. |
| `secret_encryption_marker_skips_total` | counter | Marker-based skips reusing cached content. |

**Encryption failure rate** — should be zero; non-zero means Secret writes are being rejected:

```promql
rate(gitopsreverser_secret_encryption_failures_total[5m])
```

**Cache effectiveness:**

```promql
rate(gitopsreverser_secret_encryption_cache_hits_total[5m])
/
rate(gitopsreverser_secret_encryption_attempts_total[5m])
```

---

## Suggested alerts

| Condition | Meaning |
| --- | --- |
| `rate(gitopsreverser_audit_events_total{category="error"}[10m]) > 0` | Attribution fact-store writes are failing — check Redis. |
| `rate(gitopsreverser_audit_eventlists_total{outcome="decode_error"}[10m]) > 0` | A sender is posting non-EventList payloads to `/audit-webhook`. |
| `rate(gitopsreverser_attribution_fact_stream_decode_errors_total[10m]) > 0` | A schema or version mismatch on the fact stream; facts are being skipped and lost. |
| `time() - gitopsreverser_attribution_fact_follower_last_success_timestamp_seconds > 600` | The fact follower is wedged; attribution is degrading to committer-authored cluster-wide. |
| `rate(gitopsreverser_resync_background_failures_total[15m]) > 0` sustained | Background resyncs are not committing; the folder relies on steady-state events to catch up. |
| `gitopsreverser_api_catalog_group_versions{state="degraded"} > 0` | Part of the API surface is hidden behind a broken APIService. |
| `rate(gitopsreverser_secret_encryption_failures_total[10m]) > 0` | Secret writes are being rejected by the encryption path. |
| `gitopsreverser_branch_worker_queue_depth` rising and not draining | A branch worker is backing up against a stalled remote. |

---

## Known gaps (not yet emitted)

These are real holes, listed so a missing panel is never read as a healthy zero. The plan to close
them — with a reference dashboard and an audit/attribution deep-dive — is
[metrics-observability-plan.md](design/metrics-observability-plan.md) (see also
[architecture.md → Observability](architecture.md#observability)):

- **Watch ingestion** — per-type watch events received, reconnects/restarts, `sendInitialEvents`
  replays, `410 Gone` rebuilds, and cursor-resume vs full-replay. Watch is the object-state source,
  yet it has almost no direct coverage today; this is the biggest gap.
- **Shard processing delay** — how long an event waits between arriving on its watch shard and being
  picked up. The wait histogram above times each resolution in isolation; it cannot see the delay a
  slow resolution imposes on the events queued behind it on the same single-threaded shard, which is
  a failure mode that has already broken a test.
- **Relevance filter** — how many watch events are filtered before Git, and why. The filter
  decisions are scattered along the watch-to-Git path today, so there is no one honest boundary to
  count at yet.
- **Git push health** — push latency and conflict-retry counts. The instruments for these were
  removed because nothing recorded them; re-add them **with** a recording site when the need is
  real, not before.

The attribution relabel and the fact-pipeline loss paths that used to be listed here have **shipped**
— `tier` plus `actor_kind`, `event_kind` on the wait histogram, the stream decode-error counter, and
follower health are all documented above.
Nothing consumes these metrics yet, so the break is taken deliberately in one release rather than
twice; it will carry an [`UPGRADING.md`](UPGRADING.md) table of old name → new name.

---

## Adding a new metric to this document

When you add a metric, add a row here too — **and only after it has a production recording
site**. A defined-but-unrecorded instrument is a contract the code does not honor; it does not
belong in `exporter.go` or in this document. The bar for a row: a reader who has never seen the
metric should learn (1) what it measures, (2) at least one query that answers a real operator
question, and (3) what a bad value looks like. A metric without an interpretation is noise.
