# Interpreting GitOps Reverser Metrics

> Last updated: September 2026, reconciled to the live instrument set in
> [`internal/telemetry/exporter.go`](../internal/telemetry/exporter.go). Names changed in that pass;
> [`UPGRADING.md`](UPGRADING.md) carries the old-to-new table.

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
| `watch_events_total` | counter | `gittarget_namespace`, `gittarget_name`, `group`, `version`, `resource`, `outcome` | The ingest census: every delivered watch event, exactly once. `outcome` is `routed` / `unchanged` / `operation_filtered` / `not_object` / `bookmark` / `shutdown` / `route_failed`. |
| `watch_event_handling_seconds` | histogram | `group`, `version`, `resource` | How long a stream was **busy** on one event, attribution wait included. Occupancy, not queue delay. |
| `watch_sessions_ended_total` | counter | `group`, `version`, `resource`, `reason` | `expired` (cursor out of history, forcing a rebuild) / `error` / `stopped`. |
| `watch_replay_duration_seconds` | histogram | `group`, `version`, `resource` | Time to `initial-events-end`: what a `410` storm charges. |
| `watch_recovery_total` | counter | `gittarget_namespace`, `gittarget_name`, `group`, `resource`, `mode` | One per completed recovery. `mode` is `cursor_resume` / `type_reconcile` / `replay` / `list_fallback`. No `version`: a recovery covers a cell. |
| `watch_types` | gauge | `gittarget_namespace`, `gittarget_name`, `state` | Types this target resolves, by `streaming` / `replaying` / `blocked`. `sum` is the resolved total. |
| `git_documents_total` | counter | `gittarget_namespace`, `gittarget_name`, `group`, `version`, `resource`, `outcome` | The write-boundary census, per **document**. `outcome` is `written` / `deleted_live` / `deleted_sweep` / `unchanged` / `retained`. |
| `git_commits_total` | counter | `provider_namespace`, `provider_name`, `branch`, `author_kind`, `message_source` | Commit batches that **reached the remote**. |
| `git_pushes_total` | counter | `provider_namespace`, `provider_name`, `branch`, `outcome` | One per push cycle: `pushed` or `failed`. |
| `git_push_retries_total` | counter | `provider_namespace`, `provider_name`, `branch`, `reason` | Replay rounds inside a cycle. `reason` is `remote_moved`. |
| `git_push_duration_seconds` | histogram | `provider_namespace`, `provider_name`, `branch` | One cycle end to end, retries included. |
| `git_queue_drops_total` | counter | `provider_namespace`, `provider_name`, `branch`, `kind` | Work a full queue threw away. `kind` is `write` / `attach` / `resync`. Every increment is lost work. |
| `git_queue_depth` | gauge | `provider_namespace`, `provider_name`, `branch` | Pending + in-flight + committed-but-unpushed. Read at scrape time. |
| `placements_total` | counter | `source`, `disposition`, `gittarget_namespace`, `gittarget_name`, `group`, `version`, `resource` | One per new document at a resolved path. |
| `placement_refusals_total` | counter | `reason`, `gittarget_namespace`, `gittarget_name`, `group`, `version`, `resource` | One per new resource the writer declined. Every increment is a resource **absent** from the mirror. |
| `placement_kustomization_entries_total` | counter | `outcome`, `gittarget_namespace`, `gittarget_name` | `added` / `no_change` / `failed`. |
| `git_resync_failures_total` | counter | `gittarget_namespace`, `gittarget_name` | Rule-change resyncs whose apply failed **after** enqueue. |

### Outcomes are classified, not ranked

Several values above are the pipeline working, and a dashboard that paints every non-happy outcome
red trains people to ignore it. Four classes:

| Class | Values | Read as |
| --- | --- | --- |
| **expected** | `routed`, `unchanged`, `operation_filtered`, `bookmark`, `shutdown`, `written`, `deleted_live`, `deleted_sweep`, `retained`, `cached` | the pipeline working |
| **degraded** | `author_kind="unresolved"`, `mode="list_fallback"`, weak attribution tiers | working, on weaker evidence |
| **recoverable** | `git_pushes_total{outcome="failed"}` — the writes are retained and retried | alert on it **sustained with no successes**, never on one occurrence |
| **loss** | `route_failed`, any `git_queue_drops_total`, `placement_refusals_total` | an observed change that did not reach Git |

`route_failed` and a queue drop **overlap**: a full worker queue is one of the ways a route fails, so
one dropped event increments both. They are two views of one event, and summing them double-counts.

### The funnel

Four lines, left to right. They count different units on purpose (an event is not a document, a
document is not a commit), so read it as a funnel and never as a subtraction:

```promql
sum(rate(gitopsreverser_watch_events_total{outcome="routed"}[5m]))
sum(rate(gitopsreverser_git_documents_total{outcome="written"}[5m]))
sum(rate(gitopsreverser_git_commits_total[5m]))
sum(rate(gitopsreverser_git_pushes_total{outcome="pushed"}[5m]))
```

**Is the mirror stopped?** A failed push retains its writes and retries, so a failure beside
successes is contention. A branch failing with nothing landing is an outage:

```promql
sum by (provider_namespace, provider_name, branch) (
  rate(gitopsreverser_git_pushes_total{outcome="failed"}[15m]))
unless
sum by (provider_namespace, provider_name, branch) (
  rate(gitopsreverser_git_pushes_total{outcome="pushed"}[15m])) > 0
```

**Is work being thrown away?** This should be flat zero. It used to be a log line and nothing else:

```promql
sum by (kind) (rate(gitopsreverser_git_queue_drops_total[5m]))
```

**Are streams saturated?** A stream is single-threaded, so time it spends handling one event is
time nothing else on that stream is being read.

Read this as **aggregate busy-seconds per second, per type** — not as a per-stream ratio. The
histogram is labelled by type, and one type can be watched by several streams (one per namespace),
so ten lightly loaded streams also sum to `1`. A value near the number of streams for that type is
saturation; a value near `1` with ten streams is 10% each:

```promql
sum by (group, version, resource) (rate(gitopsreverser_watch_event_handling_seconds_sum[5m]))
```

Divide by the stream count if you want the ratio. Where a type is watched cluster-wide (the common
case) there is one stream, and the aggregate *is* the ratio:

```promql
sum by (group, version, resource) (rate(gitopsreverser_watch_event_handling_seconds_sum[5m]))
  / on (group, version, resource) group_left count by (group, version, resource) (
      gitopsreverser_watch_event_handling_seconds_count)
```

The per-stream number is deliberately not published: it would need the namespace on the histogram,
which multiplies the widest family in the system by every watched namespace.

**Is watch stable, and what are rebuilds costing?**

```promql
sum by (resource, reason) (rate(gitopsreverser_watch_sessions_ended_total[15m]))
histogram_quantile(0.95, sum by (le) (rate(gitopsreverser_watch_replay_duration_seconds_bucket[5m])))
```

`git_commits_total` carries the **`BranchWorker`'s**
`{provider_namespace, provider_name, branch, author_kind, message_source}` identity, not a GitTarget: one worker can
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
`watch_recovery_total` and `git_queue_depth`.

`message_source` says where each commit's message came from: `commit_request` is text a
`CommitRequest` supplied and that was used verbatim, `live` is a live window rendered through the
target's `liveTemplate`, and `reconcile` is a snapshot or resync rendered through
`reconcileTemplate`. It is read from the same decision that renders the message, so it cannot
report a source the renderer did not use.

`commit_request` counts commits that USED a request-supplied message, **not** CommitRequests. A
request that omits `spec.message` takes the target's `liveTemplate`, so it counts as `live` — a
low `commit_request` share does not by itself mean requests are failing.

**How much of the history is people naming their own changes?**

```promql
sum by (message_source) (rate(gitopsreverser_commits_total[15m]))
```

A `commit_request` share that falls to zero after a rollout **may** mean save requests stopped
reaching an open window — but omitted messages, saves that were no-ops, and pushes that never
landed all read the same way here, because none of them produce a commit carrying a request
message. Confirm against the requests themselves before changing the window: a request that
attached reports `Ready=True`, one that produced a pushed commit also reports `Pushed=True` with
`status.sha`, and one that gave up reports `Stalled=True`. Only if those show requests resolving
without their message reaching a commit is the window worth tuning against `closeDelaySeconds`.

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
gitopsreverser_git_queue_depth
```

**Did a new pod redo its reconciles after a rollout?** `watch_recovery_total` is a
counter (not a latched gauge) precisely so a fresh pod's series starts at 0; a per-pod
`increase(...) > 0` proves the new pod did its own work rather than inheriting the old pod's
stale series. This is the restart-reconcile guarantee:

```promql
sum by (pod) (increase(gitopsreverser_watch_recovery_total[10m]))
```

**Are background resyncs silently failing?** Should be zero; non-zero means snapshots are not
committing and the folder is relying on steady-state events to catch up:

```promql
sum by (gittarget_namespace, gittarget_name) (
  rate(gitopsreverser_git_resync_failures_total[15m]))
```

**How much is each GitTarget watching, and how much of it is actually running?** `sum` is the
resolved-type count; `state="blocked"` is the difference between resolved and running:

```promql
sum by (gittarget_namespace, gittarget_name) (gitopsreverser_watch_types)
gitopsreverser_watch_types{state="blocked"} > 0
```

**Are resyncs sweeping resources out of Git?** Non-zero is expected after a resource disappears from
the cluster and a scoped/full resync applies. This is not the steady-state delete path:

```promql
sum by (group, version, resource) (
  rate(gitopsreverser_git_documents_total{outcome="deleted_sweep"}[1h]))
```

### New-file placement

Placement runs **only** for a resource with no document in Git yet; everything already written is
edited in place, forever. So these counters are sparse by nature — a busy target can go a day without
one — and a zero rate is the steady state, not a broken exporter.

`source` answers "why did it land there?", which is the question a folder cannot answer:

| `source` | Means | Needs attention? |
| --- | --- | --- |
| `by_type` | a `spec.placement.byType` entry named this exact type | no — this is what you asked for |
| `default` | no `byType` entry named the type, so the catch-all `spec.placement.default` answered | **maybe** — a rule you meant to write may not be matching |
| `kustomize_root` | the folder is governed by exactly one supported kustomization, so the file went beside it and joined its `resources:` list | no — the folder's own structure decided |
| `canonical` | nothing else applied, so the built-in `{namespace}/{group}/{resource}/{name}.yaml` path was used | **maybe** — see below |

**Which types are falling back, and in which target?** Each series is a candidate for one
`placement.byType` line. The operator never guesses a hand-authored layout from the folder, so this
is how you learn a layout needs declaring:

```promql
sum by (gittarget_namespace, gittarget_name, group, version, resource) (
  increase(gitopsreverser_placements_total{source="canonical"}[24h]))
```

Canonical is not an error. For a target whose repository the operator bootstrapped, it is the whole
layout and always will be. It is worth acting on when the folder has a convention the operator was not
told about — the file lands somewhere tidy but not where the rest of that type lives.

**Is a bundling policy actually bundling?** `disposition="appended"` proves documents are joining an
existing file rather than each getting their own. It should only ever appear with `source="by_type"`
or `source="default"`; the fallbacks never append:

```promql
sum by (source, disposition) (increase(gitopsreverser_placements_total[24h]))
```

**Are we failing to mirror resources?** Every refusal is a resource that is **not** in Git. The write
is retried on the next event or resync, so a sustained rate is a policy to fix rather than a blip:

```promql
sum by (reason, gittarget_namespace, gittarget_name, resource) (
  increase(gitopsreverser_placement_refusals_total[1h]))
```

| `reason` | What to fix |
| --- | --- |
| `invalid_path` | a declared template that renders outside `spec.path` or without a YAML suffix |
| `sensitive_append` | a template that is not identity-complete, so two Secrets collide on one path |
| `plaintext_onto_encrypted` | a template routing a plaintext resource at a file holding SOPS data |
| `mixed_sensitivity_new_file` | a bundling `default` catching both a sensitive and a plaintext resource |
| `multi_document_target` | the resolved file holds a document the writer cannot account for, so it will not overwrite it |
| `unclassified` | a refusal shape newer than this table — report it |

**The one that looks fine in the folder.** A new file whose `resources:` entry could not be added is
committed and never built by kustomize: it is in Git, it looks mirrored, and nothing applies it. This
should be zero:

```promql
sum by (gittarget_namespace, gittarget_name) (
  increase(gitopsreverser_placement_kustomization_entries_total{outcome="failed"}[1h]))
```

Placement counters carry `gittarget_*` label keys rather than bare `namespace`/`name` for the
pod-scrape reason described above, and they deliberately carry **no path or resource-name label** — both
are unbounded, and both are in the log line at the write site.

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

**EventList request boundary.** `audit_eventlist_duration_seconds` times every request at
`/audit-webhook`, and its `_count` series **is** the request counter: a histogram ships its own
observation count, so the separate `audit_eventlists_total` that used to publish the same numbers
was removed, along with the per-item counter beside it (`audit_events_total` counts the same items
once each, with the type and verb on them).

`outcome` is bounded and covers the door as well as the batch: `bad_method`, `bad_path`,
`bare_endpoint_disabled`, `processed`, `empty`, `decode_error`, `process_error`. The first three are
the important addition: a request refused before decoding used to return before any instrument was
touched, so an apiserver posting audit to a path this operator does not serve looked exactly like an
apiserver posting nothing.

`bare_endpoint_disabled` is named for what it covers, and it is **not** "a route no
`ClusterProvider` claims". A named `/audit-webhook/<route>` is accepted as-is, deliberately: a route
is a partition name, not a claim about an object, and refusing unknown ones dropped audit batches in
flight while a provider was being recreated. The only route-shaped rejection is the bare
`/audit-webhook` endpoint when `--audit-route-annotation-key` is unset.

```promql
sum by (outcome) (rate(gitopsreverser_audit_eventlist_duration_seconds_count[5m]))
```

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
sum(rate(gitopsreverser_audit_eventlist_duration_seconds_count{outcome="decode_error"}[5m]))
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

Three values are named for the **verb that produced the fact** and how it matched:
`delete_sticky`, `deletecollection_body_uid`, `deletecollection_scope`. Those are the tiers only a
removal can reach, and naming the source is deliberate: it says where the evidence came from rather
than what it proves. Two of the three are statements about *this object*; `deletecollection_scope` is
a statement about a request whose scope covered it, and can name the wrong actor. So
`tier=~"delete.*"` reads as "resolved on deletion-specific evidence", never as a guarantee that the
actor named asked for this object's removal.

`latest` and `name` can hold a delete fact too, and are deliberately *not* named for one: either can
equally hold a write, and a value that could mean either must not claim a verb.

| `tier` | Meaning |
| --- | --- |
| `delete_sticky` | The sticky removal pointer: a fact whose own verb is a delete, filed by UID into a slot no later *write* fact may overwrite. Only a removal consults it, and it is asked before `exact`, because a removal's resourceVersion is the one the deletion stamped — the version a finalizer patch's own fact carries too. It is the only tier the fact TTL does not bound: a UID is unique across space and time, so the statement can never be superseded. |
| `exact` | Exact UID+resourceVersion match: this actor produced this exact version. |
| `deletecollection_body_uid` | A removal whose UID was in the set the API server said a `deletecollection` deleted. No over-attribution risk: either the object was in that set or it was not. It outranks `latest`, because `latest` names whoever last *wrote* an object while a removal asks who *deleted* it. |
| `latest` | The UID-latest tier: the object's own last fact, keyed by UID alone. A removal consults it, and a match here describing a *write* is held as a fallback while the wait continues for evidence about the deletion. |
| `name` | A match on `(namespace, name)` for a fact carrying neither a UID nor a resourceVersion — the usual shape of an aggregated API's audit event, and of a delete the API server answered with a `Status`. |
| `deletecollection_scope` | A removal matched to a `deletecollection` by scope alone — same type and namespace, the request's selector accepting the object's labels, within the collection window. The weakest evidence the join has, and the only one that can name the wrong actor, which is why it is reached only when every more specific tier missed. |
| `resource_version` | The RV-only escape hatch: a fact that carried a resourceVersion and no UID, matched on that version alone. |
| `absent` | No usable fact matched before the grace window elapsed. The resulting live commit is authored as `unknown (attribution unresolved)`. |

| `actor_kind` | Meaning |
| --- | --- |
| `user` | A human (or any non-service-account subject). |
| `serviceaccount` | A named service account — a controller, an operator, a CI identity. |
| `none` | Nobody was named, which in practice means nothing matched. |

`actor_kind="none"` and `tier="absent"` go together, and that is an invariant rather than a
coincidence: an audit event whose user cannot be resolved never becomes a fact at all (it is counted
`no_attribution_fact` above), and a fact that names nobody is refused when the follower reads it
(counted as a stream decode error below). Every fact that reaches the index therefore names someone,
which is why coverage can be read off the tier alone.

**Evidence quality, independently of coverage.** A shift from `exact` toward `deletecollection_scope` or
`name` is a quality regression even while coverage holds flat, so it is worth its own panel:

```promql
sum by (tier) (rate(gitopsreverser_attribution_resolutions_total[5m]))
```

> **`result` is gone**, and so are `exact_user`, `exact_serviceaccount`, and `weak`. See
> [`UPGRADING.md`](UPGRADING.md) for the old-to-new mapping. `exact_deletecollection_item` went
> earlier, with the expander and the fact keyspace; `deletecollection_body_uid` is its closest equivalent and
> `deletecollection_scope` is new capability rather than a rename. See
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

This counter is also the removal pointer's only horizon. Every other entry in the index expires on
the fact TTL; a removal pointer is bounded by these caps instead, so a cluster that deletes rarely
keeps its removals for a very long time at no cost, and a busy one keeps the most recent — which are
the ones a replay is likeliest to need. A sustained eviction rate on a type that deletes heavily is
the signal that its pointers are being reclaimed before a replay can use them.

**Is a follower losing facts?** A trim gap means the fact log was trimmed past this process's
position: the facts in the gap are gone for good, and the commits that needed them are authored
unresolved. It is the one loss the transport can see, which is why the transport is a log with
positions rather than fire-and-forget publish and subscribe. This should be **zero**:

```promql
sum by (stream) (rate(gitopsreverser_attribution_fact_stream_gaps_total[5m]))
```

**Is a follower silently skipping facts?** An entry the follower refuses is skipped and its
position passed — which is right, since it can never decode and stalling on it would cost every
later fact on that stream — so the facts it carried are gone. This is the loss path with **no other
symptom**: unlike a trim gap it is not detectable after the fact, and unlike a publish failure the
API server does not retry it.

Two things are refused: a payload that is not valid JSON, and one that is JSON but breaks the fact
contract by naming nobody (`author` must be present and non-empty — a fact exists to name somebody).
Either way the log line names the stream and the entry. Any non-zero rate means something is writing
entries this operator would not write — a version skew, another producer, or a hand-edited stream:

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

**An absent gauge is the worst case, not a healthy one.** The timestamp is not published until the
follower's first successful read, so a follower that has been wedged since startup — a transport
unreachable at boot — has no series at all, and `time() - <gauge>` therefore returns nothing rather
than a large number. An alert must cover that arm explicitly, which is what `attribution_transport_info`
is for: it is published when the follower starts, so a transport running without a last-success
timestamp is exactly the never-succeeded case. Give it a `for: 10m` so an ordinary restart's gap does
not trip it:

```promql
(time() - gitopsreverser_attribution_fact_follower_last_success_timestamp_seconds > 600)
or
(gitopsreverser_attribution_transport_info == 1
   unless on() gitopsreverser_attribution_fact_follower_last_success_timestamp_seconds)
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
the join falls back to `deletecollection_scope`, which is correct but weaker — so the fallback is counted
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
| `api_catalog_resources` | gauge | `source_cluster`, `state` (`allowed`/`excluded`) |
| `api_catalog_group_versions` | gauge | `source_cluster`, `state` (`trusted`/`degraded`) |
| `api_catalog_refresh_total` | counter | `source_cluster`, `outcome` (`changed`/`unchanged`/`error`) |
| `api_catalog_refresh_duration_seconds` | histogram | `source_cluster` |

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

## Watch plane owner

One loop owns the watch plane: controllers post a trigger naming a GitTarget and return, and the
owner runs one plan pass per target once that target has been quiet for a 2 s rolling silence
window (hard cap ~10 s). Design:
[watch-manager-ownership.md](design/watch-manager-ownership.md).

That loop is a queue, and a queue that grows silently is exactly how a stall gets missed. These
five make it legible.

| Metric | Type | Labels |
| --- | --- | --- |
| `watch_plan_dirty_targets` | gauge | — |
| `watch_plan_oldest_dirty_since_timestamp_seconds` | gauge | — |
| `watch_plan_passes_total` | counter | `outcome` (`completed`/`failed`/`timed_out`), `gittarget_namespace`, `gittarget_name` |
| `watch_plan_pass_duration_seconds` | histogram | — |
| `watch_plan_triggers_total` | counter | `reason` (`declare`/`rule_change`/`shared_refresh`/`periodic`), `coalesced` |

**Is anything stuck?** This is the one to alert on. The oldest dirty target's age climbs and stays
up exactly when a target cannot be planned — a source cluster that stopped answering, a catalog
that never becomes observable — whether the queue is deep or holds a single wedged target. A
healthy install sits under the settle window plus a pass, so seconds:

```promql
time() - gitopsreverser_watch_plan_oldest_dirty_since_timestamp_seconds
```

It is exported as a **timestamp**, not an age, and that is not cosmetic: the age it replaces was
republished once per owner-loop turn, so a wedged pass froze it at whatever it held when the loop
stopped moving. A timestamp stays true with nobody recomputing it.

**Is it not running, or running and failing?** A silent watch plane and a retrying one look
identical on a dirty-count panel alone. `timed_out` is broken out from `failed` because the two
have different causes: a deadline means a pass did not finish, an error means it finished badly:

```promql
sum by (outcome) (rate(gitopsreverser_watch_plan_passes_total[15m]))
```

**Which GitTarget is failing?** The counter is per target, so a single bad tenant is nameable
rather than being averaged into a healthy fleet:

```promql
sum by (gittarget_namespace, gittarget_name) (
  rate(gitopsreverser_watch_plan_passes_total{outcome!="completed"}[15m]))
```

**Is the silence window earning its keep?** One `kubectl apply` of a GitTarget and four WatchRules
should show four coalesced triggers and one pass. A coalescing ratio near zero under a busy config
plane means edits are arriving further apart than the window, which is not a fault — it is the
window doing nothing:

```promql
sum(rate(gitopsreverser_watch_plan_triggers_total{coalesced="true"}[15m]))
  / sum(rate(gitopsreverser_watch_plan_triggers_total[15m]))
```

**Which source is noisy?** `periodic` is the 30 s floor and is expected to dominate a quiet
cluster. A high `shared_refresh` rate means the API surface is flapping; a high `rule_change` rate
means something is rewriting rules:

```promql
sum by (reason) (rate(gitopsreverser_watch_plan_triggers_total[15m]))
```

**How long does a pass take?** The input to choosing the per-target deadline. A pass is in-memory
replanning — discovery is a shared job done once per cluster, not per target — so normal is
sub-millisecond, and anything near a second means a target is doing I/O it should not be:

```promql
histogram_quantile(0.95, rate(gitopsreverser_watch_plan_pass_duration_seconds_bucket[5m]))
```

---

## Secret encryption

Background: [architecture.md → Bootstrap, Encryption, and Signing](architecture.md#bootstrap-encryption-and-signing).
Secrets are never committed in plaintext; these metrics confirm the encryption path is healthy.

| Metric | Type | Notes |
| --- | --- | --- |
| `secret_encryptions_total` | counter | One per encryption decision, labelled `outcome`: `encrypted`, `failed` (the write is rejected), or `cached` (unchanged content reused). |

**Encryption failure rate** — should be zero; non-zero means Secret writes are being rejected:

```promql
rate(gitopsreverser_secret_encryptions_total{outcome="failed"}[5m])
```

**Where is the encryption path spending its work?** One counter, so this is a share of one total.
The old guidance divided `cache_hits` by `attempts`, which were disjoint populations (the cache is
consulted first and returns early, so a hit never became an attempt) and could exceed 1:

```promql
sum by (outcome) (rate(gitopsreverser_secret_encryptions_total[5m]))
```

---

## Suggested alerts

| Condition | Meaning |
| --- | --- |
| `rate(gitopsreverser_audit_events_total{category="error"}[10m]) > 0` | Attribution fact-store writes are failing — check Redis. |
| `rate(gitopsreverser_audit_eventlist_duration_seconds_count{outcome="decode_error"}[10m]) > 0` | A sender is posting non-EventList payloads to `/audit-webhook`. |
| `rate(gitopsreverser_audit_eventlist_duration_seconds_count{outcome=~"bad_method\|bad_path\|bare_endpoint_disabled"}[15m]) > 0` | An apiserver is posting audit to a path this operator does not serve. |
| `rate(gitopsreverser_attribution_fact_stream_decode_errors_total[10m]) > 0` | A schema or version mismatch on the fact stream; facts are being skipped and lost. |
| `(time() - …_fact_follower_last_success_timestamp_seconds > 600) or (…_transport_info == 1 unless on() …_fact_follower_last_success_timestamp_seconds)`, `for: 10m` | The fact follower is wedged; attribution is degrading to committer-authored cluster-wide. Both arms are needed — see below. |
| `rate(gitopsreverser_git_resync_failures_total[15m]) > 0` sustained | Background resyncs are not committing; the folder relies on steady-state events to catch up. |
| `gitopsreverser_api_catalog_group_versions{state="degraded"} > 0` | Part of the API surface is hidden behind a broken APIService. |
| `time() - gitopsreverser_watch_plan_oldest_dirty_since_timestamp_seconds > 120`, `for: 5m` | A GitTarget cannot be planned — its source cluster or catalog is unreachable — and its mirror is not converging. The `time() -` is not optional: the gauge is a Unix timestamp, so a bare `> 120` is true of every instant since 1970 and would fire on ordinary planning. |
| `rate(gitopsreverser_watch_plan_passes_total{outcome="timed_out"}[15m]) > 0` sustained | Plan passes are hitting their per-target deadline; the target stays dirty and installs nothing. |
| `rate(gitopsreverser_secret_encryptions_total{outcome="failed"}[10m]) > 0` | Secret writes are being rejected by the encryption path. |
| `rate(gitopsreverser_git_queue_drops_total[5m]) > 0` | The queue saturated and work was thrown away. |
| `gitopsreverser_git_queue_depth` rising and not draining | A branch worker is backing up against a stalled remote. |

---

## Known gaps (not yet emitted)

Listed so a missing panel is never read as a healthy zero. The plan for them is
[metrics-observability-plan.md](design/metrics-observability-plan.md).

- **True watch queue delay.** `watch_event_handling_seconds` measures how long a stream was *busy*,
  which is the right saturation signal but not the same as how long an event *waited*. Measuring
  the wait needs an arrival timestamp stamped before the blocking consumer, and events arrive on a
  client-go watch channel this process does not fill, so there is nowhere honest to stamp one.
- **A relevance-filter breakdown beyond the ingest census.** `watch_events_total{outcome}` names
  where an event stopped; it does not attribute an `unchanged` to sanitization versus a genuine
  no-op diff.
- **The reference dashboard and alert rules.** The queries in this document are the specification
  they are written from; the JSON and the rule files are not shipped yet.

The watch ingestion stage, shard occupancy, push health, and the audit ingress rejections that used
to be listed here have **shipped**.

## Adding a new metric to this document

When you add a metric, add a row here too — **and only after it has a production recording
site**. A defined-but-unrecorded instrument is a contract the code does not honor; it does not
belong in `exporter.go` or in this document. The bar for a row: a reader who has never seen the
metric should learn (1) what it measures, (2) at least one query that answers a real operator
question, and (3) what a bad value looks like. A metric without an interpretation is noise.
