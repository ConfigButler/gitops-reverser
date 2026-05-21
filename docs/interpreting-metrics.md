# Interpreting GitOps Reverser Metrics

> Last updated: May 2026

This is the operator's field guide to the metrics GitOps Reverser exports. It explains how to
read each metric family and gives copy-pasteable PromQL for the questions operators actually
ask.

It is a living document. New metrics should arrive here with at least one example query — a
metric nobody knows how to read is not observable.

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

## Audit join pipeline

This is the most timing-sensitive subsystem, so it gets the most coverage. Background:
[architecture.md → Audit Ingestion Pipeline](architecture.md#audit-ingestion-pipeline).

The pipeline joins two event sources per `auditID`: the **official** kube-apiserver audit event
(authoritative for *who/when*) and an **additional** body contribution from a proxy
(authoritative for *what*, on aggregated-API paths). They race. The healthy case is the
additional body arriving first and parking; when the official wins the race it waits up to
`--audit-event-body-wait` (default `500ms`) for the body before dropping.

### The metrics

| Metric | Type | Labels |
| --- | --- | --- |
| `audit_events_received_total` | counter | `source`, `gvr`, `action`, `user` |
| `audit_event_quality_total` | counter | `source`, `quality`, `gvr`, `action` |
| `audit_join_parked_total` | counter | `parked_kind` |
| `audit_join_emitted_total` | counter | `source`, `result` (`as_is`, `merged`) |
| `audit_join_duplicate_dropped_total` | counter | `reason` |
| `audit_shallow_dropped_total` | counter | `gvr`, `action` |
| `audit_join_body_late_total` | counter | `gvr`, `action` |
| `audit_join_skew_seconds` | histogram | `arrival` (`body_first`/`official_first`), `outcome` (`merged`/`timed_out`) |
| `audit_official_gate_wait_seconds` | histogram | — |

`audit_join_skew_seconds` is the centerpiece for timing health. Every official↔additional pair
produces one observation:

- **`arrival="body_first"`** — the additional body was already parked when the official arrived.
  The value is the proxy's *lead time*: how long the body sat parked. Always `outcome="merged"`.
- **`arrival="official_first"`** — the official arrived first and waited on the canonical gate.
  The value is the wait duration. `outcome="merged"` if the body arrived in time,
  `outcome="timed_out"` if the grace period expired (that event is also counted in
  `audit_shallow_dropped_total`).

### Query cookbook

**Is the join working at all?** Rate of canonical emissions, split by how they resolved:

```promql
sum by (result) (rate(gitopsreverser_audit_join_emitted_total[5m]))
```

`result="merged"` means an official was completed by a parked/awaited body; `as_is` means the
official already carried its own body.

**How often does the race go the "wrong" way?** Fraction of joins where the official arrived
before its body:

```promql
sum(rate(gitopsreverser_audit_join_skew_seconds_count{arrival="official_first"}[5m]))
/
sum(rate(gitopsreverser_audit_join_skew_seconds_count[5m]))
```

Near `0` is healthy. Climbing toward `1` means the proxy is consistently behind the apiserver.

**Is `--audit-event-body-wait=500ms` enough?** p95 of the time officials spend waiting:

```promql
histogram_quantile(0.95,
  sum by (le) (rate(gitopsreverser_audit_join_skew_seconds_bucket{arrival="official_first"}[5m])))
```

If this creeps toward the configured `bodyWait`, raise the flag **before** drops start — this
is the early-warning signal. If it sits near zero, the wait budget has plenty of headroom.

**Did waiting actually pay off?** Merged-after-wait vs timed-out:

```promql
sum by (outcome) (rate(gitopsreverser_audit_join_skew_seconds_count{arrival="official_first"}[5m]))
```

`outcome="timed_out"` here equals `rate(audit_shallow_dropped_total[5m])` — two views of the
same failure.

**What's the margin against `--audit-event-body-ttl` (5m)?** p99 of the proxy's lead time:

```promql
histogram_quantile(0.99,
  sum by (le) (rate(gitopsreverser_audit_join_skew_seconds_bucket{arrival="body_first"}[5m])))
```

Parked bodies expire at `bodyTTL`. If p99 lead time approaches it, bodies are parking far too
early (or the official stream has stalled) and orphan expiry is imminent.

**Are shallow events being lost?** Non-zero means misconfiguration — no proxy, or an audit
policy that omits bodies:

```promql
sum by (gvr, action) (rate(gitopsreverser_audit_shallow_dropped_total[5m]))
```

**Is the canonical gate causing backpressure?** A shallow official holds the in-pod gate for up
to `bodyWait`; later officials queue behind it. p95 of that queueing delay:

```promql
histogram_quantile(0.95,
  sum by (le) (rate(gitopsreverser_audit_official_gate_wait_seconds_bucket[5m])))
```

Sub-millisecond is normal. Sustained values near `bodyWait` mean officials are serializing
behind shallow events — consider whether the audit policy should be supplying bodies directly.

**Is a webhook retry storm happening?** Duplicate drops should be rare:

```promql
sum by (reason) (rate(gitopsreverser_audit_join_duplicate_dropped_total[5m]))
```

### Suggested alerts

| Condition | Meaning |
| --- | --- |
| `rate(audit_shallow_dropped_total[10m]) > 0` | Bodies are being lost — install the proxy or fix the audit policy. |
| `histogram_quantile(0.95, ...skew_seconds_bucket{arrival="official_first"}...) > 0.4` (with `bodyWait=500ms`) | Grace period about to be exhausted; raise `--audit-event-body-wait`. |
| `rate(audit_join_body_late_total[15m]) > 0` sustained | Additional bodies arriving after the decision — proxy slower than `bodyTTL`. |
| `rate(audit_join_duplicate_dropped_total[5m])` spike | Likely webhook retry storm. |

---

## Git write pipeline

Metrics for the path from matched event to pushed commit. Background:
[architecture.md → Git Operations](architecture.md#git-operations).

| Metric | Type | Notes |
| --- | --- | --- |
| `git_operations_total` | counter | Git operations attempted. |
| `commits_total` | counter | Commit batches pushed. |
| `commit_bytes_total` | counter | Approximate bytes written across commits. |
| `objects_scanned_total` | counter | Objects seen by list/informer paths. |
| `objects_written_total` | counter | Objects that resulted in a file write. |
| `files_deleted_total` | counter | Files removed during orphan cleanup. |
| `rebase_retries_total` | counter | Non-fast-forward push retries (conflict → fetch + replay). |
| `ownership_conflicts_total` | counter | Marker/lease ownership conflicts. |
| `lease_acquire_failures_total` | counter | Failures to acquire/renew a lease. |
| `marker_conflicts_total` | counter | Repository marker conflicts. |
| `watch_duplicates_skipped_total` | counter | Watch events skipped by content-hash dedup. |
| `git_push_duration_seconds` | histogram | End-to-end push latency. |
| `git_commit_queue_size` | gauge | Pending commits queued. |
| `repo_branch_active_workers` | gauge | Active `BranchWorker` goroutines. |
| `repo_branch_queue_depth` | gauge | Per-`(provider,branch)` pending-event depth. |

**Push latency p95:**

```promql
histogram_quantile(0.95, sum by (le) (rate(gitopsreverser_git_push_duration_seconds_bucket[5m])))
```

**Conflict rate** — how often pushes hit a diverged remote and replayed:

```promql
rate(gitopsreverser_rebase_retries_total[5m]) / rate(gitopsreverser_commits_total[5m])
```

A steadily rising ratio means remotes are being written by something other than this operator,
or multiple branches are contending.

**Is a branch worker backing up?** A persistently rising gauge indicates a stalled remote:

```promql
gitopsreverser_repo_branch_queue_depth
```

---

## Secret encryption

Background: [architecture.md → Encryption](architecture.md#encryption). Secrets are never
committed in plaintext; these metrics confirm the encryption path is healthy.

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

## Adding a new metric to this document

When you add a metric, add a row here too. The bar: a reader who has never seen the metric
should learn (1) what it measures, (2) at least one query that answers a real operator
question, and (3) what a bad value looks like. A metric without an interpretation is noise.
