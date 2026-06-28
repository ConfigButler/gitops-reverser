# Interpreting GitOps Reverser Metrics

> Last updated: June 2026 — reconciled to the live instrument set in
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
the worker is the honest attribution unit. `author_kind` is `user`, `serviceaccount`, or `committer`;
reconcile/resync commits and unattributed events use `committer`. The namespace/name keys are
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

Audit runs **only when Redis is configured**. The kube-apiserver POSTs audit `EventList`
payloads to `/audit-webhook`; the handler applies an intrinsic accept gate and, for an accepted
event, writes a minimal attribution fact to the Redis index. There is **no body join and no
second source** — watch, not audit, carries the object body — so the only audit metrics are the
request boundary and the per-event census. Background:
[architecture.md → Optional Attribution](architecture.md#optional-attribution).

| Metric | Type | Labels |
| --- | --- | --- |
| `audit_eventlists_total` | counter | `outcome` |
| `audit_eventlist_events_total` | counter | `outcome` |
| `audit_eventlist_duration_seconds` | histogram | `outcome` |
| `audit_events_total` | counter | `outcome`, `category`, `group`, `version`, `resource`, `verb` |
| `attribution_resolutions_total` | counter | `result`, `group`, `version`, `resource` |
| `attribution_resolution_wait_seconds` | histogram | `result` |
| `attribution_fact_events_total` | counter | `op` |
| `attribution_fact_index_size` | gauge | — |

**EventList request boundary.** `audit_eventlists_total` and `audit_eventlist_duration_seconds`
count requests at `/audit-webhook`; `audit_eventlist_events_total` counts the decoded event items
inside them. `outcome` is bounded: `processed`, `empty`, `decode_error`, `process_error`. This is
the raw delivery edge — "are EventLists arriving at all?" — before any gate or attribution logic.

**Per-event census.** `audit_events_total` increments exactly once per decoded event. `category`
is the coarse bucket of `outcome` (carried as its own label so the health invariant is a simple
selector):

| `category` | Live `outcome` values | Meaning |
| --- | --- | --- |
| `stored` | `queued` | Accepted; an attribution fact was written to the index. |
| `dropped` | `nil_event`, `stage`, `read_only_or_unknown_verb`, `failed_request`, `dry_run`, `unchanged_resource_version`, `non_scale_subresource` | Correctly rejected at the accept gate — not an error. |
| `error` | `write_error` | The fact store rejected the write. The one category that should stay zero. |

The full enum lives in [`internal/audit/outcome/outcome.go`](../internal/audit/outcome/outcome.go)
— it is the source of truth.

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
actor (human or service account) rather than falling back to the committer:

```promql
sum(rate(gitopsreverser_attribution_resolutions_total{result=~"exact_.*"}[5m]))
/
sum(rate(gitopsreverser_attribution_resolutions_total[5m]))
```

`result` is bounded:

| `result` | Meaning |
| --- | --- |
| `exact_user` | Exact UID+resourceVersion match for a human user. |
| `exact_serviceaccount` | Exact UID+resourceVersion match for a named service account. |
| `weak` | Non-exact match, such as UID-only or RV-only. |
| `conflict` | Multiple authors wrote facts for the selected join key. |
| `expired` | A fact existed but aged out before the watch event joined it. |
| `absent` | No fact or expiry evidence matched. |

**Is the grace window paying for itself?** Misses waiting near the configured grace window mean the
operator is delaying commits without finding facts:

```promql
histogram_quantile(0.95,
  sum by (le, result) (
    rate(gitopsreverser_attribution_resolution_wait_seconds_bucket{result=~"absent|expired"}[5m])))
```

**Is the Redis fact index healthy?** Facts should be written and later matched; a high `late` or
`expired_unmatched` rate points at timing mismatch between audit and watch:

```promql
sum by (op) (rate(gitopsreverser_attribution_fact_events_total[5m]))
```

```promql
gitopsreverser_attribution_fact_index_size
```

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
- **Git push health** — push latency and conflict-retry counts. The instruments for these were
  removed because nothing recorded them; re-add them **with** a recording site when the need is
  real, not before.

---

## Adding a new metric to this document

When you add a metric, add a row here too — **and only after it has a production recording
site**. A defined-but-unrecorded instrument is a contract the code does not honor; it does not
belong in `exporter.go` or in this document. The bar for a row: a reader who has never seen the
metric should learn (1) what it measures, (2) at least one query that answers a real operator
question, and (3) what a bad value looks like. A metric without an interpretation is noise.
