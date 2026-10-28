# Interpreting GitOps Reverser metrics

> Last reconciled: October 2026, against the instruments in
> [`internal/telemetry/exporter.go`](../internal/telemetry/exporter.go) and their recording sites:
> every name, label key and label value below is one the code emits.
> [`UPGRADING.md`](UPGRADING.md) carries the old-to-new table for renamed series.

The operator's field guide to the metrics GitOps Reverser exports: what each family means, and
copy-pasteable PromQL for the questions operators actually ask. Every metric here has a production
recording site; the areas deliberately not instrumented are listed under
[Known gaps](#known-gaps-not-yet-emitted), so a blank panel is never mistaken for a healthy zero.

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
| **Counter** | `_total` | Monotonic. Always wrap in `rate(...[5m])` or `increase(...[1h])`; the raw value is meaningless. |
| **Histogram** | `_seconds` | Exposes `_bucket`, `_sum`, `_count`. Use `histogram_quantile()` for percentiles; `_count` is a free counter of observations. |
| **Gauge** | (none) | Instantaneous value; read directly. |

### Reading a histogram

A histogram named `foo_seconds` produces three series:

- `foo_seconds_bucket{le="..."}`: cumulative count per bucket boundary
- `foo_seconds_count`: total number of observations (use it like a counter)
- `foo_seconds_sum`: sum of all observed values

Percentile:

```promql
histogram_quantile(0.95, sum by (le) (rate(gitopsreverser_foo_seconds_bucket[5m])))
```

Mean:

```promql
rate(gitopsreverser_foo_seconds_sum[5m]) / rate(gitopsreverser_foo_seconds_count[5m])
```

---

## The two paths

Object state is ingested by **watch** and decides what reaches Git. **Audit is an optional
attribution lookup** that only names the author of a watch-observed change: a missing or late fact
changes the author, never the state (see
[architecture.md → Optional Attribution](architecture.md#optional-attribution)). The two never call
each other and meet only at the resolver, so they are documented apart.

| Family | Covers |
| --- | --- |
| [Configuration state](#configuration-state) | whether the objects a human declared were accepted, which is the question that precedes both paths |
| [Pipeline](#pipeline-watch-to-push) | watch ingest, the write boundary, commits, pushes |
| [Audit attribution](#audit-attribution-optional) | the audit door, the fact pipeline, the join |
| [API resource catalog](#api-resource-catalog) | what the operator is willing to watch, per source cluster |
| [Watch plane owner](#watch-plane-owner) | the queue that plans each GitTarget's watches |
| [Secret encryption](#secret-encryption) | the path that keeps Secrets out of Git in plaintext |

---

## Configuration state

Every other family here answers **is the pipeline flowing**. This one answers **was my
configuration accepted**, and it is the question that comes first: a `GitTarget` that has sat at
`Ready=False` for a day is as serious as a stalled push, and until this metric existed only one of
the two could page anybody. The conditions were always on the objects; nothing could watch them
without a kubeconfig.

| Metric | Type | Labels |
| --- | --- | --- |
| `resource_condition` | gauge | `kind`, `resource_namespace`, `resource_name`, `type`, `status`, `reason` |

- `kind` is `GitTarget`, `WatchRule`, `ClusterWatchRule`, `GitProvider`, `GitBranch`,
  `GitReceiver`, `KubeProvider`, `FluxClaim` or `ArgoCDClaim`: the objects a human declares.
- `type` is the kstatus trio: `Ready` (the summary), `Reconciling` (working on it) and `Stalled`
  (given up, and nothing will retry without a human).
- `status` is `True`, `False` or `Unknown`.
- `reason` is the condition's reason as it stands (`ValidationFailed`, `UnsupportedContent`,
  `BranchConflict`), which is what turns a count panel into a first guess. It is the API's own
  string, so a release that renames a reason changes the series a selector matches;
  [`UPGRADING.md`](UPGRADING.md) lists every rename.

**Each condition publishes three series, one per possible status, of which exactly one is `1`.**
So every query below compares `== 1`; a selector without it matches the two zeroes as well. The
alternative shape (one series whose *value* encodes the status) cannot be selected on and cannot
be aggregated, and `count by (status)` over it means nothing.

**`reason` is part of the series identity.** A condition that changes reason changes series, so any
aggregation *over time* has to remove it first; otherwise a transition looks like one series ending
and another starting:

```promql
avg_over_time(sum without (reason) (
  gitopsreverser_resource_condition{type="Ready", status="False"})[1h:1m])
```

**`Unknown` is published, never omitted.** An object that has not been reconciled yet is in a
state, and an absent series is indistinguishable from an operator that is not running.

**A deleted object stops publishing.** The series ends when the object goes, rather than reporting
its last value forever. That matters more than it sounds: a condition gauge that keeps a deleted
object at `Ready=False` manufactures a permanent false positive, and a panel that is permanently
red is a panel nobody reads.

Object names are labels here because these objects are configuration, bounded by how many someone
wrote, as are the `GitTarget`, `GitBranch`, applier claim and `GitReceiver` names other families
carry. The resources being mirrored are data, unbounded, and never a label
([metrics-observability-plan.md](design/metrics-observability-plan.md) §7.1). `CommitRequest` is
excluded on the same reasoning: one is created per save, so it is data-shaped, and its state is
read from `commit_requests_total` in aggregate and from the object's own conditions individually.

**Which configuration objects are not Ready?** The panel to start from:

```promql
gitopsreverser_resource_condition{type="Ready", status="False"} == 1
```

**The fleet, in one panel**: how many objects of each kind are in each state:

```promql
count by (kind, status) (gitopsreverser_resource_condition{type="Ready"} == 1)
```

**Which are permanently wedged?** `Stalled` is the kstatus "will not retry" signal, so this set
needs a human and will not clear on its own. `reason` names which gate refused:

```promql
gitopsreverser_resource_condition{type="Stalled", status="True"} == 1
```

**Which have never been reconciled?** Expected briefly after an apply and after a restart;
persistent means the controller is not picking the object up at all:

```promql
gitopsreverser_resource_condition{type="Ready", status="Unknown"} == 1
```

**Why is one of them unready?** Diagnosis is still `kubectl describe`, but the reason label gets
you to the right object and the right gate first:

```promql
count by (kind, reason) (
  gitopsreverser_resource_condition{type="Ready", status="False"} == 1)
```

### Reading these objects in a Flux dashboard

The shape is `gotk_reconcile_condition`'s, deliberately: the geometry is battle-tested and the
Grafana panels for it already exist. The name is not, because the GitOps Toolkit is defined by
membership under the `fluxcd` organization rather than by behavior, so `gotk_` on our series would
be a false statement about where they came from no matter how closely the conditions match.

For anyone who wants these objects inside Flux-shaped panels, the alias belongs on the consumer's
side, as a recording rule: opt-in, committing us to nobody else's schema:

```yaml
# Optional. Surfaces gitops-reverser config objects in GOTK-shaped dashboards.
- record: gotk_reconcile_condition
  expr: |
    label_replace(label_replace(
      gitopsreverser_resource_condition,
      "name", "$1", "resource_name", "(.*)"),
      "namespace", "$1", "resource_namespace", "(.*)")
```

Operators already running `kube-state-metrics` can get roughly the same series straight from the
CRs with a `CustomResourceStateMetrics` config, and that is a reasonable thing to do. It is not
what ships here: it lags by its own scrape interval, and it can do neither the `Unknown` synthesis
nor the delete, which are the two things that make this metric trustworthy.

---

## Pipeline: watch to push

Everything an observed change passes through on its way to the remote, in order: ingest, the write
boundary, the commit, the push. Background:
[architecture.md → Git Write Architecture](architecture.md#git-write-architecture).

| Metric | Type | Labels | Notes |
| --- | --- | --- | --- |
| `watch_events_total` | counter | `gittarget_namespace`, `gittarget_name`, `group`, `version`, `resource`, `outcome` | The ingest census: every delivered watch event, exactly once. `outcome` is `routed` / `unchanged` / `cluster_generated` (`kube-root-ca.crt`, never mirrored) / `not_object` / `bookmark` / `shutdown` / `stream_error` / `route_failed`. |
| `watch_event_handling_seconds` | histogram | `source_cluster`, `group`, `version`, `resource` | How long a stream was **busy** on one event, attribution wait included. Occupancy, not queue delay. |
| `watch_sessions_ended_total` | counter | `source_cluster`, `group`, `version`, `resource`, `reason` | `closed` (the API server's routine watch timeout: the most common ending on a healthy cluster) / `expired` (cursor out of history, forcing a rebuild) / `error` / `stopped`. |
| `watch_replay_duration_seconds` | histogram | `source_cluster`, `group`, `version`, `resource` | Time to `initial-events-end`: what a `410` storm charges. |
| `watch_recovery_total` | counter | `gittarget_namespace`, `gittarget_name`, `group`, `resource`, `mode` | One per completed recovery. `mode` is `cursor_resume` / `type_reconcile` / `replay` / `list_fallback`. No `version`: a recovery covers a resource collection. |
| `watch_types` | gauge | `source_cluster`, `gittarget_namespace`, `gittarget_name`, `state` | Types this target resolves, by `streaming` / `replaying` / `blocked`. `sum` is the resolved total. |
| `watch_streams_open` | gauge | `source_cluster`, `gittarget_namespace`, `gittarget_name` | Watch **sessions** currently open against the source cluster. Not the same as types: one type across three namespaces is three sessions. |
| `git_documents_total` | counter | `gittarget_namespace`, `gittarget_name`, `group`, `version`, `resource`, `outcome` | The write-boundary census, per **document**. `outcome` is `written` / `deleted_live` / `deleted_sweep` / `unchanged` / `retained` / `refused`. |
| `git_commits_total` | counter | `gitbranch_namespace`, `gitbranch_name`, `provider_namespace`, `provider_name`, `author_kind`, `message_source` | Commit batches that **reached the remote**. |
| `git_pushes_total` | counter | `gitbranch_namespace`, `gitbranch_name`, `provider_namespace`, `provider_name`, `outcome` | One per push cycle: `pushed` or `failed`. |
| `git_push_retries_total` | counter | `gitbranch_namespace`, `gitbranch_name`, `provider_namespace`, `provider_name`, `reason` | Replay rounds inside a cycle. `reason` is `remote_moved`. |
| `git_push_duration_seconds` | histogram | `gitbranch_namespace`, `gitbranch_name`, `provider_namespace`, `provider_name` | One cycle end to end, retries included. |
| `git_fetches_total` | counter | `gitbranch_namespace`, `gitbranch_name`, `provider_namespace`, `provider_name`, `reason` | Every call that reads the remote through a `SmartFetch`. `reason` is `publication` (the head of a publication cycle, and a worker's first contact with the repository) / `recovery` (re-establishing a base that could not be trusted, including a worktree a failed write left dirty that could not be undone locally) / `contention` (the reset onto the new tip after a push was rejected because somebody else moved the branch) / `push_failure_probe` (a push that failed without the remote saying anything, looking up where the branch is) / `forced_recheck` (a full re-read outside the publication cycle: a forced recheck, or the snapshot a resync judges against) / `refresh` (the periodic top-up of an idle branch, or a push receiver delivery on one, and only when the branch had actually moved). See the note below. |
| `git_queue_refusals_total` | counter | `gitbranch_namespace`, `gitbranch_name`, `provider_namespace`, `provider_name`, `kind` | Work refused at enqueue: the queue was full, intake was paused through an outage, or the worker was stopping. `kind` is `write` / `attach` / `resync` / `refresh`. The producer keeps a refused item and offers it again: the watch delivers a write again from its cursor, the controller re-sends a save, a resync is gathered again, and the next reconcile asks for a refresh. What it can cost, when its watch cursor expires before the write comes back, is the write's own commit and, under `prune.mode: OnEvent`, a deleted object's removal from Git. |
| `git_commit_failures_total` | counter | `gitbranch_namespace`, `gitbranch_name`, `provider_namespace`, `provider_name`, `reason` | A live commit window that died between routing and pushing; a resync's failure is `git_resync_failures_total`. `reason` is `refused` (a Git path a human must fix) / `error`. Every increment is a window's events lost until the next resync. An unreachable remote does not count: a decided window waits for the publication retry instead. |
| `git_writes_dropped_total` | counter | `gitbranch_namespace`, `gitbranch_name`, `provider_namespace`, `provider_name`, `reason` | A live event the branch worker dropped because its `GitTarget`'s render fidelity was not established. `reason` is `render_fidelity_diverged`: a rendered value does not match the live object, and every write is dropped until a fresh replay measures the folder clean. Or it is `render_fidelity_pending`: a collection of the `GitTarget` has not finished its replay, which closes every collection of that target until it does. A live event that meets a pending gate is held, not dropped, and committed with its author once the gate opens; it is counted here only when the hold gives up, 10 minutes after the first one was held or once the branch holds more than its retained-byte budget, and the worker then asks for a fresh snapshot, which commits the current state as the operator (a deletion comes back only under prune mode `Always`). Nothing replays a dropped write, so each one is an edit missing from Git until the object changes again or its collection replays. The worker logs the first drop under each gate status at Info, naming the collections the gate waits for, and each hold, release and give-up at Info. |
| `git_commit_windows_total` | counter | `gittarget_namespace`, `gittarget_name`, `close_reason`, `timer_source` | One per closed commit window, whether or not it then produced a commit. `close_reason` is `idle_timeout` / `max_duration` / `attach_next` / `identity_change` / `path_conflict` / `buffer_limit` / `resync_before_apply` / `foreign_commit` (a Git host said the branch may have moved, so the window is published now) / `apply_due` (an apply came due for an applier on the branch, so the window's edits are published before the apply is sent) / `shutdown`; `timer_source` is `target` or `commit_request`. See [tuning commit windows](#tuning-commit-windows). |
| `git_commit_window_duration_seconds` | histogram | `gittarget_namespace`, `gittarget_name` | How long each window collected: from the write that opened it to the start of its finalize. Its count equals `git_commit_windows_total` summed over `close_reason` and `timer_source`. |
| `git_queue_depth` | gauge | `gitbranch_namespace`, `gitbranch_name`, `provider_namespace`, `provider_name` | Pending + in-flight + committed-but-unpushed. Read at scrape time. |
| `git_retained_bytes` | gauge | `gitbranch_namespace`, `gitbranch_name`, `provider_namespace`, `provider_name` | What the worker holds until a push lands, in the units of the retained-byte budget: the open window, the retained writes and waiting saves, each with a 1 KiB per-item charge. Serialized bytes, not heap. |
| `git_retained_writes` | gauge | `gitbranch_namespace`, `gitbranch_name`, `provider_namespace`, `provider_name` | Decided writes waiting for a push. |
| `git_intake_paused` | gauge | `gitbranch_namespace`, `gitbranch_name`, `provider_namespace`, `provider_name` | 1 while the worker refuses new writes, saves and resyncs because an outage filled its budget; 0 otherwise. |
| `git_oldest_retained_write_timestamp_seconds` | gauge | `gitbranch_namespace`, `gitbranch_name`, `provider_namespace`, `provider_name` | When the oldest write still waiting for a push was decided, as a Unix timestamp. No sample while nothing waits. |
| `git_next_retry_timestamp_seconds` | gauge | `gitbranch_namespace`, `gitbranch_name`, `provider_namespace`, `provider_name` | When the next attempt at the pending writes is due, as a Unix timestamp. No sample while no retry is scheduled. |
| `git_materialization_failures_total` | counter | `gitbranch_namespace`, `gitbranch_name`, `provider_namespace`, `provider_name`, `reason` | Attempts to commit decided writes that stopped before any push: `reason` is `unreachable` or `parent_unavailable`. Nothing is lost; the writes wait for the retry. A failure `git_pushes_total` cannot see, because no push cycle started. |
| `git_branch_targets` | gauge | `gitbranch_namespace`, `gitbranch_name`, `gittarget_namespace`, `gittarget_name`, `source_cluster` | Always 1. The **join** between the GitTarget-labeled half of the pipeline and the GitBranch-labeled half: the `GitTarget`'s `spec.gitBranchRef`. Published per configured GitTarget, whether or not its GitBranch exists or its worker runs. |
| `commit_requests_total` | counter | `outcome`, `gittarget_namespace`, `gittarget_name` | One per `CommitRequest` terminal **decision**. `outcome` is `committed` / `no_window` / `window_mismatch` / `already_present` / `save_in_progress` (refused: an earlier save of the same author on the target was in progress) / `author_unresolved` (no audit event named the request's creator, on a cluster that sends them) / `failed`. Requests served on a source cluster count here too. A target that never resolved publishes the empty pair. See the counting note below. |
| `placements_total` | counter | `source`, `disposition`, `gittarget_namespace`, `gittarget_name`, `group`, `version`, `resource` | One per new document at a resolved path. |
| `placement_refusals_total` | counter | `reason`, `gittarget_namespace`, `gittarget_name`, `group`, `version`, `resource` | One per new resource the writer declined. Every increment is a resource **absent** from the mirror. |
| `placement_kustomization_entries_total` | counter | `outcome`, `gittarget_namespace`, `gittarget_name` | `added` / `no_change` / `failed`. |
| `git_resync_failures_total` | counter | `gittarget_namespace`, `gittarget_name`, `reason` | Rule-change resyncs that did not apply. `reason` is `error` (the commit failed) or `timeout` (no answer in time): the snapshot did not land. `refused` (refused at intake; the same refusal is `git_queue_refusals_total{kind="resync"}`, and the stream gathers it again) and `deferred` (the remote could not be reached; it stays pending and applies later, beside `git_materialization_failures_total`) lose nothing. A resync's commit failure is counted here only, never in `git_commit_failures_total`. |

`source_cluster` is the `KubeProvider` the `GitTarget` mirrors from, as `<namespace>/<name>`
(`gitops-reverser/local` for the chart's provider), so two providers with one name in two namespaces
are two values.

### Reading `git_fetches_total`

Nothing else here answers "is my mirror pulling the branch on every write?". `git_pushes_total`
counts cycles and `git_push_retries_total` counts replays, and both read the same whether the
worker fetches constantly or never.

One increment is one `SmartFetch`, which is **two or three requests** to the Git host: it lists
refs in a session of its own and then fetches objects in another. So the counter measures how
often the worker decided to read the remote, which is the thing an operator can act on; it is not
a wire-level request count.

`fetchRemoteBranchHash` is counted, under `push_failure_probe`. It reads like a ref lookup and is
named like one, but it runs the same `SmartFetch` and transfers objects, so leaving it out would
flatten the metric precisely where a push problem is being investigated.

**`contention` and `push_failure_probe` are separate because the two failures mean different
things.** A rejected push already carries the remote's hash, so it costs exactly one fetch: the
reset onto the new tip, counted as `contention`. A push that failed before the remote said
anything (an auth failure, a dropped connection) has to look the hash up first, and that lookup
is the probe. Most probes find the remote unmoved, and then no reset follows and the probe is the
only fetch the failure cost. Folding the two together would make an expired credential read as
other writers fighting you over the branch.

The reasons are split finer than the call sites on purpose. `publication` and `recovery` both come
out of the head of a publication cycle, and separating them is what lets steady-state traffic be
read at a glance:

```promql
sum by (reason) (rate(gitopsreverser_git_fetches_total[5m]))
```

On a healthy publishing target `publication` is **flat at zero** after the worker's first cycle:
the base it plans on is the tip its own last push established. A `publication` rate that tracks
`git_pushes_total` is the pre-change behavior and now means the base is being invalidated every
cycle, so read it together with `recovery` rather than as the normal state.

A climbing `recovery` rate is a worker repeatedly losing confidence in its checkout, which is a bug
report rather than a cost. **`recovery` covers a dirty worktree whichever way it is cleaned up:**
the plain reset when nothing is retained, and the reset-and-replay when something is. Which of the
two happens depends only on whether a push was in cooldown at the time, so splitting them would
hide half of every recovery from the series you are meant to read this way.

A write that fails part-way no longer reaches this series at all in the common case: the worker
undoes the failed batch locally, back to the commit it started on, and the next commit plans on the
checkout without a fetch. Only a failed write that cannot be undone locally leaves the worktree
dirty and costs a `recovery` fetch, so a `recovery` fetch now points at a reset that went wrong, a
parent change, or a replay that did not finish.

`contention` tracks `git_push_retries_total` one-for-one, because both
count a confirmed moved remote. `push_failure_probe` moving on its own is pushes failing for a
reason that is not another writer: check credentials and connectivity, not the branch.
`forced_recheck` moving with nothing else is reconcile traffic.

`refresh` is the price of `--git-refresh-interval`, per branch. Multiply its per-second rate by
the interval in seconds and you get the number of idle branches whose refresh found the branch
somewhere else; if that is close to the number of idle branches you think you have, something
outside Reverser is pushing to them constantly. The advertisement that PRECEDES the fetch is
deliberately not counted here: it runs no `SmartFetch`, and this counter's documented meaning is
every call that does. So on a quiet repository the refresher's traffic shows up nowhere on this
series at all. That is the honest accounting, not an omission: counting the refresher's
advertisements separately would be a new series, not a redefinition of this one. Setting
`--git-refresh-interval=0` returns this reason to a flat zero and the idle target to no Git
traffic whatsoever.

**This is one instrument, not a family.** Seven reasons on one counter answer seven different
operator questions, and the alternative (a metric per call site) would multiply the surface without
telling anyone anything the `reason` label does not. Nothing here should grow an eighth series
without a question it is the only way to answer.

### Sizing the branch worker queue against `git_queue_refusals_total`

`git_queue_refusals_total{kind="write"}` on a healthy branch is a queue too small for its bursts. The
enqueue onto a branch worker is deliberately non-blocking, so a full queue refuses the write rather
than stalling the watch path behind a slow remote. The watch does not advance its cursor past a
refused write: its session ends, it reconnects, and it delivers the write again. So nothing is lost
while that cursor lives, but each refusal costs the stream a reconnect, and a burst that keeps the
queue full keeps the stream reconnecting. If the cursor expires first, the stream rebuilds from a
fresh snapshot, and that recovery follows the target's prune policy. It restores the current
content of every object it lists, but the write's own attributed commit is never made: the change
happened, and the history does not say so. A delete is weaker still: under the default
`prune.mode: OnEvent` a snapshot cannot infer one from absence, so an object deleted meanwhile keeps
its file in Git until `Always` sweeps it or someone removes it; see the [recovery contract][recovery].
Treat any sustained increment on a healthy branch as a signal to raise
`controllerManager.branchWorkerQueueDepth`. During an outage, refusals are the intake pause instead;
see the backpressure paragraph below.

[recovery]: design/gittarget-branch-worker-pending-writes.md#resume-and-the-three-meanings-of-replay

**The depth gauge will not warn you first.** `git_queue_depth` shows the traffic you have, and the
bursts that overrun the queue are usually the traffic you do not: an unpaced `kubectl delete` of a
few hundred objects arrives orders of magnitude faster than the application writes the same objects
did. A workload that sits comfortably at a fraction of the depth can still drop under an
administrative one.

**Size it by burst, not by rate.** A queue slot holds one write request, so what has to fit is the
number of concurrent write requests a bounded burst can produce, roughly
`(concurrent writers) × (GitTargets sharing the branch worker)`. A burst that cannot exceed the
depth is never refused, whatever its arrival shape. Note the second factor: workers are keyed by
repository branch, so pointing a second GitTarget at a branch, from any namespace, halves the
headroom of a worker already in use. The branch series are labeled by the `GitBranch` the worker
writes for and its `GitProvider`, and `GitBranch.status.gitTargetCount` says how many targets
reference it. Confirm
which targets share one worker by joining on `git_branch_targets`: several targets against a
single `git_queue_depth` series is one worker serving all of them.

**What it costs.** On a healthy branch `--branch-buffer-max-size` does **not** cover this queue. That
cap bounds the open commit window plus the writes retained for replay until a push succeeds, and it
is accounted only once an item *leaves* the queue, so queue depth × payload is additional pod
memory, on top of it. During a Git outage the same cap is charged at enqueue and covers the queue
as well; see the backpressure paragraph below.

Budget it as **queue depth × serialized size × ~6**. A queued object is held as an unstructured
map, which measures around six times the bytes it serializes to; the channel itself is negligible
beside that. So a full queue of 1000 at the ~800-byte resources of a small CRD is about 5 MiB per
saturated worker, against a 1Gi default limit, but the multiplier is on the payload, so a folder
mirroring megabyte-sized ConfigMaps or Secrets reaches gigabytes at the same depth. Size against
your own payloads, and against how many workers could saturate at once: the figure is per worker,
and there is one worker per repository branch.

### Outcomes are classified, not ranked

Several values above are the pipeline working, and a dashboard that paints every non-happy outcome
red trains people to ignore it. Four classes:

| Class | Values | Read as |
| --- | --- | --- |
| **expected** | `routed`, `unchanged`, `bookmark`, `shutdown`, `written`, `deleted_live`, `deleted_sweep`, `retained`, `cached` | the pipeline working. `unchanged` is a document considered and found identical; `refused` is a document the writer declined to place, which is loss |
| **degraded** | `author_kind="unresolved"`, `mode="list_fallback"`, `not_object`, `stream_error`, weak attribution tiers | working, on weaker evidence |
| **recoverable** | `git_pushes_total{outcome="failed"}` (the writes are retained and retried); `route_failed` and any `git_queue_refusals_total` (the producer keeps the refused item and offers it again) | alert on it **sustained**: pushes with no successes, refusals on a branch whose intake is not paused. Never on one occurrence |
| **loss** | any `git_commit_failures_total`, `git_writes_dropped_total`, `placement_refusals_total`, `git_documents_total{outcome="refused"}` | an observed change that did not reach Git |

`route_failed` and a queue refusal **overlap**: a refused enqueue is one of the ways a route fails,
so one refused event increments both. They are two views of one event, and summing them
double-counts. A refused event becomes loss only when its watch cursor expires before it comes back:
the fresh snapshot then restores the object's content without that change's own commit, and, under
`prune.mode: OnEvent`, does not remove a deleted object's file.

**Backpressure during an outage looks like loss and is not.** A branch worker keeps every decided
write while its remote cannot be reached, and bounds that at intake: while a failed publication
waits for its retry, work that would take what it holds past the retained-byte budget (queued work,
the open window, retained writes and waiting saves) pauses its intake. It then
refuses new writes, saves and resyncs the way a full queue does, and both series rise. Intake stays
paused until a push lands. The watch does not advance its cursor past a refused event; its stream
waits for the branch to reopen intake and then delivers the event again. Only a cursor that expires
during a long outage turns it into a fresh snapshot, which converges the folder under the target's
prune policy and without that event's own commit. Read these increments beside `git_intake_paused == 1`, or
`git_pushes_total{outcome="failed"}` with no successes: that is an outage holding work back, and
every `GitTarget` on the branch says so on `Ready`. A healthy branch never refuses for this reason.

`placement_refusals_total` and `git_documents_total{outcome="refused"}` **overlap the same way**: a
resource the writer declines to place increments both, so the loss class holds two views of one
refusal rather than two refusals. Read them for different questions (`placement_refusals_total`
carries the `reason` and fires only on the new-file path, `git_documents_total{outcome="refused"}` is
the write-boundary census view) and never add them. They are not quite 1:1 either: a refusal raised
after placement has already resolved reaches the census without a reason, so the census total is the
one to trust for "how many documents were refused".

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
sum by (gitbranch_namespace, gitbranch_name) (
  rate(gitopsreverser_git_pushes_total{outcome="failed"}[15m]))
unless
sum by (gitbranch_namespace, gitbranch_name) (
  rate(gitopsreverser_git_pushes_total{outcome="pushed"}[15m])) > 0
```

**Which GitTargets does that branch serve?** The push-side instruments are labeled by
`{gitbranch_namespace, gitbranch_name}` because one BranchWorker serves one `GitBranch`, which
several GitTargets can share. `git_branch_targets` is the join that names them:

```promql
sum by (gitbranch_namespace, gitbranch_name) (
  rate(gitopsreverser_git_pushes_total{outcome="failed"}[5m])
)
  * on(gitbranch_namespace, gitbranch_name)
  group_right
  gitopsreverser_git_branch_targets
```

`group_right`, not `group_left`: the mapping is the many side (one series per GitTarget), and the
modifier goes on the side with the higher cardinality. `group_left` here works only while exactly
one GitTarget uses the branch and fails the moment a second one does.

**Read the result as "potentially affected", never as "behind".** The join says which GitTargets
write to a branch and nothing about whether any of them had pending work. Two limits follow:

- A `GitBranch` with **no push series at all** drops out of the multiplication, so the target whose
  worker never ran (the one most worth seeing) is the one this query cannot show. Query the
  mapping against `gitopsreverser_git_queue_depth` for that:

  ```promql
  gitopsreverser_git_branch_targets
    unless on(gitbranch_namespace, gitbranch_name) gitopsreverser_git_queue_depth
  ```

- `spec.suspend` stops the write and nothing else, so a suspended target keeps its worker, its
  branch and its series while making no commits by design. Nothing here separates suspended from
  stuck; `resource_condition` does.

**Which `GitProvider` is failing?** A credential that expired fails every branch its
`GitProvider` writes. Every branch-worker series carries the provider beside the `GitBranch`, which
adds no series (a `GitBranch`'s provider cannot change), so the grouping is one selector:

```promql
sum by (provider_namespace, provider_name) (
  rate(gitopsreverser_git_pushes_total{outcome="failed"}[15m]))
```

**What does the `GitBranch` itself say?** Its conditions are on `resource_condition` under the
generic `resource_namespace` and `resource_name` keys, so rename them to match:

```promql
label_replace(label_replace(
  gitopsreverser_resource_condition{kind="GitBranch", type="Ready", status="False"} == 1,
  "gitbranch_namespace", "$1", "resource_namespace", "(.*)"),
  "gitbranch_name", "$1", "resource_name", "(.*)")
```

A `GitBranch` reporting `BranchConflict` holds no branch and runs no worker, so it has conditions
and no branch-worker series.

**How contended is the branch?** A retry is a replay round *inside* a cycle, not a terminal
outcome, which is why it is its own counter rather than a third value on `git_pushes_total`. The
ratio is the contention signal: several writers sharing a branch push the remote head forward under
each other, and every retry rebuilds the pending writes on the new head before pushing again. A
steadily climbing ratio is work being redone, and it is the early warning for the push-latency rise
that follows:

```promql
sum by (gitbranch_namespace, gitbranch_name) (
  rate(gitopsreverser_git_push_retries_total[5m]))
/
sum by (gitbranch_namespace, gitbranch_name) (
  rate(gitopsreverser_git_pushes_total[5m]))
```

**Is work being refused, or thrown away?** All three should be flat zero on a healthy branch. A
queue refusal is a saturated worker or a paused intake, and the producer offers the work again; a
commit failure takes a whole window with it, and `reason="refused"` will not clear until someone
fixes the Git path; a dropped write is an edit that met a closed render-fidelity gate:

```promql
sum by (kind) (rate(gitopsreverser_git_queue_refusals_total[5m]))
sum by (reason) (rate(gitopsreverser_git_commit_failures_total[5m]))
sum by (reason) (rate(gitopsreverser_git_writes_dropped_total[5m]))
```

**How much work is a type's ingestion doing?** A stream is single-threaded, so time it spends
handling one event is time nothing else on that stream is being read. This is **aggregate processing
seconds per second, per type**:

```promql
sum by (group, version, resource) (rate(gitopsreverser_watch_event_handling_seconds_sum[5m]))
```

It does not answer per-stream saturation, and no threshold on it can. One type may be watched by
several streams (one per namespace) whose measurements share these labels, so the individual streams
are not recoverable from the exported series: one stream at 100% and ten streams at 10% both total
`1`. Read it as how much work this type's ingestion is doing, and use it for trend and comparison
between types rather than as a utilization figure.

**Is watch stable, and what are rebuilds costing?**

```promql
sum by (source_cluster, resource, reason) (rate(gitopsreverser_watch_sessions_ended_total[15m]))
histogram_quantile(0.95, sum by (le, source_cluster) (
  rate(gitopsreverser_watch_replay_duration_seconds_bucket[5m])))
```

`source_cluster` is on these three because the API server that closes, expires and replays a watch
is that cluster's: an `expired` storm is one cluster's watch cache under pressure, and summed across
clusters it reads as everybody's.

A steady trickle of `reason="closed"` is health, not a fault: the API server ends every watch on a
randomized timeout and the stream reconnects from its cursor in seconds. Because that reconnect
deliberately moves no condition (a two-second reattach must not flip `StreamsRunning` and make
`kubectl wait --for=condition=Ready` flaky on a healthy cluster), this counter is the **only** place
a reconnect is visible, and so the only way to catch a stream that is reconnecting in a hot loop:

```promql
sum by (resource) (rate(gitopsreverser_watch_sessions_ended_total{reason="closed"}[15m])) > 0.05
```

One close per stream per watch timeout is expected; the threshold above is roughly an order of
magnitude above that. `error` now means a session that ended on a genuine fault, and `expired`
remains the one that costs a full rebuild.

`git_commits_total` carries the **`BranchWorker`'s** `GitBranch` and `GitProvider`, plus
`author_kind` and `message_source`, not a GitTarget: one worker serves every GitTarget on its `GitBranch`, coalescing
their writes into one commit batch, so the worker is the honest attribution unit. `author_kind` is
`user`, `serviceaccount`, `committer`, or `unresolved`; reconcile/resync commits and
configured-author mode use `committer`.

`message_source` is `live`, `reconcile`, or one of three request values: `commit_request` (a save
request's message committed verbatim, no `requestTemplate` configured), `commit_request_framed`
(`requestTemplate` rendered), and `commit_request_fallback` (`requestTemplate` failed to render, so
the message was committed verbatim instead).

The fallback is the one to alert on. It is a **successful** commit (nothing is refused and no
condition moves), so without this counter a `requestTemplate` that has quietly stopped applying
looks exactly like one that was never configured:

```promql
sum by (gitbranch_namespace, gitbranch_name) (
  rate(gitopsreverser_git_commits_total{message_source="commit_request_fallback"}[15m])
) > 0
```

`framed / (framed + fallback)` is the health ratio for the feature.

**`unresolved` is the one to watch.** It means attribution RAN and did not name an actor, so
the commit carries the `unknown (attribution unresolved)` author instead of a person. It is
deliberately not folded into `user` (which would make a lost actor look like a named one, so a
degrading attribution path would read as an improving one) nor into `committer` (which would
hide it among legitimately unattributed reconcile writes). For a live mutation that should be
attributable, a non-zero and growing `unresolved` share means the audit-attribution configuration
or delivery path needs investigation. The namespace/name keys are **prefixed on purpose**: a
Prometheus pod scrape with `honor_labels=false` renames a bare `namespace` label to
`exported_namespace` and puts the scraped pod's namespace in its place, so a per-branch `namespace`
selector would silently match nothing. The same reasoning applies to `watch_recovery_total` and
`git_queue_depth`.

`message_source` says where each commit's message came from: `commit_request` is text a
`CommitRequest` supplied and that was used verbatim, `live` is a live window rendered through the
target's `liveTemplate`, and `reconcile` is a snapshot or resync rendered through
`reconcileTemplate`. It is read from the same decision that renders the message, so it cannot
report a source the renderer did not use.

`commit_request` counts commits that USED a request-supplied message, **not** CommitRequests. A
request that omits `spec.message` takes the target's `liveTemplate`, so it counts as `live`, and a
low `commit_request` share does not by itself mean requests are failing.

**How much of the history is people naming their own changes?**

```promql
sum by (message_source) (rate(gitopsreverser_git_commits_total[15m]))
```

A `commit_request` share that falls to zero after a rollout **may** mean save requests stopped
reaching an open window, but omitted messages, saves that were no-ops, and pushes that never
landed all read the same way here, because none of them produce a commit carrying a request
message. Confirm against the requests themselves before changing the window: a request that
attached reports `Ready=True`, one that produced a pushed commit also reports `Pushed=True` with
`status.commit`, and one that gave up reports `Stalled=True`. Only if those show requests resolving
without their message reaching a commit is the request's `window` worth tuning.

`commit_requests_total` answers the same question in aggregate, which per-object conditions cannot:

```promql
sum by (outcome) (rate(gitopsreverser_commit_requests_total[15m]))
```

**Which target's saves are failing?** The counter carries the GitTarget, so the fleet view above is
an aggregation rather than the only view available:

```promql
sum by (gittarget_namespace, gittarget_name, outcome) (
  rate(gitopsreverser_commit_requests_total[15m])
)
```

An empty `gittarget_name` is a request whose GitTarget could not be read. A target that exists but
has no branch worker yet keeps its labels: the request waits in `WaitingForWorker`, and if the
worker never starts it counts as `failed` under that target. The name it asked for is on the
object's conditions, deliberately not on the series: it is client-supplied text and would grow the
metric without bound.

`window_mismatch` is the value to watch, and the two refusals are deliberately separate. Both mean
`attachTimeout` ran out without a window the request could attach to, but only `window_mismatch`
means one was open meanwhile and belonged to somebody else: the author's edits went into that commit,
under a **generated** message instead of the sentence they typed. Nothing else in this document
goes red for it. `no_window` is the benign half: nothing was pending to save.

The distinction is recorded at expiry from whether a window the request could not attach to was
seen during its wait, so a foreign window that closes before `attachTimeout` runs out still counts. Reading
`no_window` as "users are being refused" over-reports; reading only `committed` under-reports.

**What one increment means.** One terminal DECISION, recorded outside the status-write retry loop.
That is not one per `CommitRequest`: a terminal status that never persisted is re-decided when the
request is redelivered, and a restart re-reconciles anything non-terminal. Read rates and ratios
from it rather than exact request counts.

### Tuning commit windows

A GitTarget's `commit.window` timers decide when its changes become a commit, and a `CommitRequest`
can bring its own. Two metrics show what those timers actually do.

**Which timer ends the windows?** Every closed window is counted once, with what closed it and whose
timers were in effect:

```promql
sum by (gittarget_namespace, gittarget_name, close_reason) (
  rate(gitopsreverser_git_commit_windows_total{timer_source="target"}[1h])
)
```

Filter on `timer_source="target"` when tuning a target's own timers. A window a save attached to
runs on the save's timers, and a save omits `idleTimeout` by default, so its windows would pull the
picture toward `max_duration`.

- `idle_timeout` is the normal ending: the writes went quiet.
- `max_duration` means the maximum was reached first. With the target's timers that usually means
  writes kept arriving closer together than `idleTimeout`; raise `maxDuration` if those changes
  belong in one commit. It does not prove continuous activity: an `idleTimeout` close to
  `maxDuration` ends quiet windows this way too.
- `identity_change` means a write arrived that could not join an open window, nor be proven to change
  other files than it: a different GitTarget on the same branch, or a different author or
  attribution state writing where the worker cannot prove a file of its own (a file with other
  documents, a kustomize folder, a placement template). A high share means commits are being split
  by interleaving rather than by time, and no timer changes that.
- `path_conflict` means another author's write changes a file the window's writes change, so the
  window committed first. Authors editing different files keep a window each.
- `attach_next` is a save with `attach: Next` closing the author's open window before it waits.
- `buffer_limit` means the worker's retained-write budget was reached, and the window was flushed
  to stay within it. Writes committed but not yet pushed count toward that budget, so this reads
  as "the branch is holding too much", not as a measurement of pod memory. See
  [sizing the branch worker queue](#sizing-the-branch-worker-queue-against-git_queue_refusals_total).
- `resync_before_apply` closes the window so a resync can apply after it; `shutdown` is the worker
  stopping.
- `apply_due` closes the window because an apply came due for an applier on the branch: its edits
  are published at once, ahead of the push cooldown, so the apply includes them. A window a
  `CommitRequest` holds is never closed this way; the apply waits for the save. Its share follows the
  apply traffic (Git-side pushes, API creates, repairs), and no timer changes it. `foreign_commit`
  is the same early close for a push a Git host reported.

The count includes windows that then committed nothing, because their writes already matched Git or
their finalize failed. A push that replays commits onto a moved remote closes no window, and neither
does the empty commit a `CommitEmpty` save records when no window reached it.

**How long do changes wait in a window?** This is collection time only, one part of the delay before
a change is in Git: watch or audit delivery comes before it, and the commit, the push cooldown and
the push come after it.

```promql
histogram_quantile(0.95, sum by (le, gittarget_namespace, gittarget_name) (
  rate(gitopsreverser_git_commit_window_duration_seconds_bucket[1h])
))
```

The p95 is an estimate: `histogram_quantile` interpolates linearly inside the bucket that holds it,
so it can only be as sharp as the bucket boundaries around it. A p95 just above the target's
`idleTimeout` is the expected shape for isolated changes. A p95 just above `maxDuration` matches a
`max_duration` share above. A window closes shortly after its deadline, never on it, so each default
timer (a save's `2s`, a target's `5s` and `1m`) and a save's `5m` ceiling has a bucket ending 10%
above it: a healthy `5s` window reads between `5s` and `5.5s`, not in the wide `(5.5, 10]` bucket.
The buckets then continue up to the `24h` a GitTarget may set. A timer set away from the defaults
falls in a wider bucket, and its p95 reads correspondingly coarser.

**Commit rate per GitBranch:**

```promql
sum by (gitbranch_namespace, gitbranch_name) (rate(gitopsreverser_git_commits_total[5m]))
```

**Are real names landing in Git?** A wall of `author_kind="committer"` means the Git history is not
showing human or named service-account authors, even if audit is flowing:

```promql
sum by (author_kind) (rate(gitopsreverser_git_commits_total[15m]))
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
  rate(gitopsreverser_git_resync_failures_total{reason=~"error|timeout"}[15m]))
```

**How much is each GitTarget watching, and how much of it is actually running?** `sum` is the
resolved-type count; `state="blocked"` is the difference between resolved and running:

```promql
sum by (gittarget_namespace, gittarget_name) (gitopsreverser_watch_types)
gitopsreverser_watch_types{state="blocked"} > 0
```

**Does a target's rules resolve to nothing at all?** The most common misconfiguration there is (a
typo in `resources:`, or a type the default watch policy excludes) produces a target that looks
completely healthy on every other panel, because a pipeline with no input has nothing to report:

```promql
sum by (gittarget_namespace, gittarget_name) (gitopsreverser_watch_types) == 0
```

The other half of that confusion is a type the cluster serves and this operator refuses to watch,
which is a policy answer rather than a resolution failure:

```promql
gitopsreverser_api_catalog_resources{state="excluded"}
```

**What is this operator costing my API server?** The question a cluster admin asks before installing
it, and the one `watch_types` cannot answer: a GitTarget watching ConfigMaps in three namespaces
resolves **one type** and opens **three watches**. `watch_streams_open` counts the sessions:

```promql
sum by (source_cluster) (gitopsreverser_watch_streams_open)
```

It counts what the API server itself would count. A session appears when the watch opens (replay
included, since a replaying watch is an open connection) and disappears when it closes, so a
connection attempt that failed and a stream sitting in reconnect backoff are both absent rather than
counted as held. Internal controller and discovery watches are outside it; this is the object-state
watch load only.

**Which GitTarget is opening them?** Useful when one target's rules are wider than intended:

```promql
topk(10, sum by (gittarget_namespace, gittarget_name) (gitopsreverser_watch_streams_open))
```

**Sessions per resolved type** is the fan-out a namespace-scoped rule set produces. A ratio near 1
means most types are watched cluster-wide; a large ratio means many per-namespace watches, which is
the shape that multiplies connections fastest:

```promql
sum by (source_cluster) (gitopsreverser_watch_streams_open)
  / sum by (source_cluster) (gitopsreverser_watch_types)
```

**Are resyncs sweeping resources out of Git?** Non-zero is expected after a resource disappears from
the cluster and a scoped/full resync applies. This is not the steady-state delete path:

```promql
sum by (group, version, resource) (
  rate(gitopsreverser_git_documents_total{outcome="deleted_sweep"}[1h]))
```

### New-file placement

Placement runs **only** for a resource with no document in Git yet; everything already written is
edited in place, forever. So these counters are sparse by nature (a busy target can go a day without
one), and a zero rate is the steady state, not a broken exporter.

`source` answers "why did it land there?", which is the question a folder cannot answer:

| `source` | Means | Needs attention? |
| --- | --- | --- |
| `by_type` | a `spec.placement.byType` entry named this exact type | no: this is what you asked for |
| `default` | no `byType` entry named the type, so the catch-all `spec.placement.default` answered | **maybe**: a rule you meant to write may not be matching |
| `kustomize_root` | the folder is governed by exactly one supported kustomization, so the file went beside it and joined its `resources:` list | no: the folder's own structure decided |
| `builtin` | nothing else applied, so the built-in `{namespace}/{group}/{resource}/{name}.yaml` path was used | **maybe**: see below |

**Which types are falling back, and in which target?** Each series is a candidate for one
`placement.byType` line. The operator never guesses a hand-authored layout from the folder, so this
is how you learn a layout needs declaring:

```promql
sum by (gittarget_namespace, gittarget_name, group, version, resource) (
  increase(gitopsreverser_placements_total{source="builtin"}[24h]))
```

The built-in path is not an error. For a target whose repository the operator bootstrapped, it is the whole
layout and always will be. It is worth acting on when the folder has a convention the operator was not
told about: the file lands somewhere tidy but not where the rest of that type lives.

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
| `unclassified` | a refusal shape newer than this table; report it |

A `{label:key}` template never refuses for a missing label: a resource that does not set it lands
at the built-in `_unlabeled` bucket, or at whatever the template's own `{label:key|fallback}` names
(including one directory up, if that fallback is empty), so it is always mirrored somewhere. See
[the label placement docs](layout/new-file-placement-rules.md#labelkey--the-one-metadata-variable)
if a lot of resources are landing in `_unlabeled` and you would rather they used a declared
fallback or a different variable.

**The one that looks fine in the folder.** A new file whose `resources:` entry could not be added is
committed and never built by kustomize: it is in Git, it looks mirrored, and nothing applies it. This
should be zero:

```promql
sum by (gittarget_namespace, gittarget_name) (
  increase(gitopsreverser_placement_kustomization_entries_total{outcome="failed"}[1h]))
```

Placement counters carry `gittarget_*` label keys rather than bare `namespace`/`name` for the
pod-scrape reason described above, and they deliberately carry **no path or resource-name label**: both
are unbounded, and both are in the log line at the write site.

---

## Apply requests and the push receiver

With `GitTarget.spec.applierClaimRef` set, the branch worker decides when the Flux `Kustomization`
or Argo CD `Application` the applier claim holds applies, and the push receiver tells it that somebody
else pushed. See [who applies this folder](configuration.md#who-applies-this-folder-specapplierclaimref).

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `apply_requests_total` | counter | `gitbranch_namespace`, `gitbranch_name`, `provider_namespace`, `provider_name`, `applier_kind`, `claim_namespace`, `claim_name`, `reason`, `outcome` | One per attempt to one applier, labeled by the `GitBranch` whose worker asked and the applier claim holding the applier it asked. `applier_kind` is `kustomization` (a Flux `Kustomization`) or `application` (an Argo CD `Application`); `reason` is `ForeignCommit` / `Refusal` / `Repair` / `CatchUp`, for either kind (`CatchUp` adopts an object the API created in the folder, or converges once when the worker starts and once when a `GitTarget` first names the applier claim); `outcome` is `sent` (the applier was asked and the ask landed; it says nothing about how the apply went. What needs no apply, a converge of the commit the applier last applied or a branch the remote does not carry, the branch worker never sends, so it is not counted), `failed` (could not ask, including an applier that applies another repository or branch, an `Application` with automated sync on, or one whose running sync did not end within 5 minutes; the branch worker keeps the reason pending and decides the retry after a flush, so a failure that persists keeps counting at its backoff) or `dropped` (no `GitTarget` may still ask for that reason: each was deleted or recreated, moved to another branch or applier, is no longer admitted by its `KubeProvider`, or withdrew its consent to a `Refusal`) or `stale` (asked nothing because the branch tip moved before the ask landed; the worker decides again at once, at the new tip, and frequent stale attempts mean the branch keeps moving within an attempt's lifetime) |
| `apply_revision_mismatches_total` | counter | `gitbranch_namespace`, `gitbranch_name`, `provider_namespace`, `provider_name`, `applier_kind`, `claim_namespace`, `claim_name` | One per apply operation that succeeded on another revision than the commit GitOps Reverser checked. A `Kustomization` applies whatever its `GitRepository` artifact holds when it reconciles; the artifact is read again just before the resume, so this is somebody fetching the source in between (a person, or the source's own interval). The apply window still withholds a late write over a newer edit; a steady rate means something else keeps fetching the source |
| `apply_claimed_resource_reconciled_total` | counter | `applier_kind`, `claim_namespace`, `claim_name` | One per apply an applier claim did not start: a held `Kustomization` resumed (`flux resume`, or a controller writing its spec without `spec.suspend`), or a held `Application`'s automated sync switched on, by somebody else. From then until the claim takes it back, on its next look within a minute, the applier applies whatever its source holds, unchecked against the branch. The claim records a `ClaimedResourceReconciled` Event for each as well, but the API server folds identical Events into one and expires them, so alert on this counter. A `Kustomization` resumed so also shows the apply as an `Unresolved` operation on the claim |
| `apply_window_collisions_total` | counter | `gitbranch_namespace`, `gitbranch_name`, `provider_namespace`, `provider_name`, `outcome` | One per write by the applier, while an apply it was asked for runs, to an object the API side edited after the flush before that request: the apply arriving late over a newer edit. `outcome` is `withheld` (proven to be the applier's write, by its field manager alone, with an entry as new as the edit or newer: `kustomize-controller`, or `argocd-controller` / `argocd-application-controller`; so not mirrored: Git keeps the API's value, and a `Repair` of the applier follows, which restores it on the cluster) or `ambiguous` (the applier and another field manager wrote in the same second, so it was mirrored as ever; if it was the apply, the revert is in Git as the applier's commit). An applier entry older than the edit is not counted: it is the API's undo of an edit, mirrored |
| `apply_echoes_total` | counter | `gitbranch_namespace`, `gitbranch_name`, `provider_namespace`, `provider_name`, `outcome` | One per write by the applier's own field managers alone, judged by the apply window: an apply GitOps Reverser asked for landing, object by object. `outcome` is `not_mirrored` (the applier wrote the object last, at or after the earliest send of the apply windows still open, and nobody edited it in the window: the apply itself, whose content is the commit Git already holds, so it is not mirrored) or `late` (the applier's managers alone wrote the object last with no apply window open: an echo arriving after its window closed, or an apply somebody else started; it is mirrored as an edit). `not_mirrored` rises with every apply and needs no action. A steady `late` rate means applies keep landing outside the windows, and such a write can revert a newer Git-side change to the object: look for a person or a tool applying the folder by hand (`flux reconcile`, a manual sync), or for watch events reaching the worker long after the apply |
| `apply_window_barriers_total` | counter | `gitbranch_namespace`, `gitbranch_name`, `provider_namespace`, `provider_name`, `outcome` | One per apply window whose operation completed: the window waits until the watch has delivered every write its GitTargets' source clusters held at that moment, so the applier's own writes are judged inside it however far behind the watch is (each event waits for its audit fact, and on a cluster that sends none, for the whole `--author-attribution-grace`). `outcome` is `met` (every stream delivered them; the window then closed at the end of its 10-second grace), `timed_out` (the window closed at the 2-minute limit with a stream still behind: a later write of the applier's is mirrored as an edit, and counted as `apply_echoes_total{outcome="late"}`), or `failed` (the source cluster could not be listed, or its resourceVersions are not numbers; the window closed after its grace alone). A steady `timed_out` or `failed` rate means the window cannot see every apply it asked for land |
| `apply_ownership_conflicts_total` | counter | `gitbranch_namespace`, `gitbranch_name`, `provider_namespace`, `provider_name` | One per object another applier owns that the branch worker kept out of a coordinated folder: a live write not mirrored, or a snapshot object neither upserted nor swept. The owner is read from Flux's owner labels (`kustomize.toolkit.fluxcd.io/name` and `/namespace`) or Argo CD's tracking annotation (`argocd.argoproj.io/tracking-id`). Any non-zero rate is a misconfiguration: another `Kustomization` or `Application` applies objects into a namespace the folder mirrors, and two appliers applying one object fight each other. The worker logs each object once, naming both appliers; narrow the `WatchRule`, or move the object to one applier |
| `apply_stale_snapshots_total` | counter | `gitbranch_namespace`, `gitbranch_name`, `provider_namespace`, `provider_name`, `outcome` | One per resync snapshot the branch worker found stale: collected while Git was ahead of the cluster (a commit pushed and not yet applied), collected across an apply, or arriving while Git is ahead. `outcome` is `discarded` (not decided; a fresh snapshot of its `GitTarget` is asked for once Git is no longer ahead, and the `GitTarget`'s live writes are held back by the render-fidelity gate until it lands) or `used` (the `GitTarget`'s snapshots had been held for 5 minutes while an apply kept failing, so this one was decided anyway, and whatever the unapplied commit changed in its objects is reverted, as the API-wins rule says). `discarded` comes in short bursts around applies and needs no action; a discard is not a resync failure and is not counted in `git_resync_failures_total`. Any `used` means an applier is failing for minutes at a time: read the applier claim's `status.operation` and the `ApplyFailed` Events. See [stale snapshots](spec/reconcile-via-watchlist-mark-and-sweep.md#stale-snapshots-are-discarded) |
| `apply_operations_unresolved` | gauge | `gitbranch_namespace`, `gitbranch_name`, `provider_namespace`, `provider_name` | Apply operations on the branch that have not completed 5 minutes after they were requested. Each is still followed, holds its applier's apply window open, and holds back the applier's next request until it completes, so a value that stays above zero is an applier stuck mid-apply: read its status, and the `ApplyUnresolved` Event on the `GitTarget`s. Read at scrape time |
| `apply_appliers_held` | gauge | `gitbranch_namespace`, `gitbranch_name`, `provider_namespace`, `provider_name` | Appliers on the branch with a reason pending that the initial-snapshot barrier holds back: no applier is asked for anything until every `GitTarget` naming its claim has had a snapshot of each collection it watches published, and its folder is on the branch. A short hold follows a start, a newly named claim or a watch change. A value that stays above zero is a `GitTarget` that is not mirrored: one whose `WatchRule` selects nothing holds its applier until it watches something, and one whose folder is missing waits for the fresh snapshot the worker asked for (a suspended `GitTarget` never writes it). The worker logs each hold, its reason and its end at Info ("An apply is held back"). Read at scrape time |
| `receiver_deliveries_total` | counter | `receiver_namespace`, `receiver_name`, `type`, `outcome` | One per verified-or-refused delivery to a `GitReceiver`'s `/git-push/` path. `type` is its `spec.type`; `outcome` is `notified` (handed to at least one branch worker), `unknown_repository` (no `GitProvider`, in any namespace, has the repository the delivery names under any URL it carries), `unknown_branch` (one has, but no branch worker writes that branch for it), `ignored` (a ping, an event other than a push, or a tag or other non-branch ref), `rejected` (a missing or wrong signature or token, or an oversized body) or `invalid` (verified but unparseable, or naming no repository). Every outcome but `rejected` and `invalid` answers `202`. `unknown_repository` and `unknown_branch` are expected on an organization or GitHub App hook, which delivers for every repository and branch. On a repository's own hook, a steady `unknown_repository` rate means the payload names the repository by a URL no provider's `spec.url` matches, such as an SSH alias: the affected `GitBranch` has no `status.lastPushNoticeAt` |

A delivery counted `notified` is only a hint: the worker reads the advertisement, and fetches under
`git_fetches_total{reason="refresh"}` only when the branch actually moved. A `ForeignCommit` request
follows when it did. So on a branch where only GitOps Reverser pushes, `notified` deliveries are
followed by no request at all: a push of the worker's own leaves nothing pending, because the cluster
already runs the edits it holds.

```promql
# Requests that could not be delivered, by applier claim and reason. A failure that persists keeps
# counting, at the worker's backoff of up to a few minutes, until the applier is fixed.
sum by (applier_kind, claim_namespace, claim_name, reason) (
  rate(gitopsreverser_apply_requests_total{outcome="failed"}[15m]))
# A receiver a Git host is calling with the wrong secret.
sum by (receiver_namespace, receiver_name) (
  rate(gitopsreverser_receiver_deliveries_total{outcome="rejected"}[15m])) > 0
# A receiver whose deliveries match no GitProvider's spec.url. Routine on an organization or GitHub
# App hook; on one repository's hook it is a URL spelled another way than the provider's.
sum by (receiver_namespace, receiver_name) (
  increase(gitopsreverser_receiver_deliveries_total{outcome="unknown_repository"}[1h])) > 0
# Edits landing while an apply runs, by GitBranch. Withheld ones are reverted on the cluster until the
# Repair that follows; ambiguous ones may have put the applier's revert in Git.
sum by (gitbranch_namespace, gitbranch_name, outcome) (
  increase(gitopsreverser_apply_window_collisions_total[1h])) > 0
# An applier that somebody else made apply, by claim: each was unchecked against the branch.
sum by (applier_kind, claim_namespace, claim_name) (
  increase(gitopsreverser_apply_claimed_resource_reconciled_total[1h])) > 0
# An applier stuck mid-apply: nothing more is asked of it until its operation completes.
max by (gitbranch_namespace, gitbranch_name) (gitopsreverser_apply_operations_unresolved) > 0
# Applier writes mirrored as edits because no apply window was open. A steady rate can revert
# Git-side changes; something applies the folder outside the windows.
sum by (gitbranch_namespace, gitbranch_name) (
  rate(gitopsreverser_apply_echoes_total{outcome="late"}[1h])) > 0
# Another applier's objects inside a coordinated folder: a misconfiguration, logged per object.
sum by (gitbranch_namespace, gitbranch_name) (
  increase(gitopsreverser_apply_ownership_conflicts_total[1h])) > 0
# Snapshots used although Git was ahead: an applier has been failing for at least 5 minutes.
sum by (gitbranch_namespace, gitbranch_name) (
  increase(gitopsreverser_apply_stale_snapshots_total{outcome="used"}[1h])) > 0
```

## Audit attribution (optional)

Audit runs when `--author-attribution` is on. The kube-apiserver POSTs audit `EventList` payloads to
`/audit-webhook`; the handler applies an intrinsic accept gate and appends the accepted events'
facts to a per-type fact log: **one append per type per request**, not one per event. The watch side
follows that log into a bounded, TTL'd in-memory index and joins against it. There is **no body join
and no second source** (watch, not audit, carries the object body), so the only audit metrics are
the request boundary and the per-event census.

The log is Redis Streams by default and an in-process ring with
`--author-attribution-transport=memory`, so attribution does not require Redis. Background:
[architecture.md → Optional Attribution](architecture.md#optional-attribution).

| Metric | Type | Labels |
| --- | --- | --- |
| `audit_eventlist_duration_seconds` | histogram | `outcome` |
| `audit_events_total` | counter | `audit_route`, `outcome`, `category`, `group`, `version`, `resource`, `verb` |
| `attribution_resolutions_total` | counter | `tier`, `actor_kind`, `group`, `version`, `resource` |
| `attribution_resolution_wait_seconds` | histogram | `tier`, `event_kind` |
| `attribution_facts_total` | counter | `op` |
| `attribution_fact_index_entries` | gauge | none |
| `attribution_fact_index_evictions_total` | counter | `reason` |
| `attribution_fact_stream_gaps_total` | counter | `stream` |
| `attribution_fact_stream_decode_errors_total` | counter | `transport` |
| `attribution_fact_follower_errors_total` | counter | `transport` |
| `attribution_fact_follower_last_success_timestamp_seconds` | gauge | none |
| `attribution_collection_without_uidset_total` | counter | `reason` |
| `attribution_transport_info` | gauge (always 1) | `transport` |

**EventList request boundary.** `audit_eventlist_duration_seconds` times every request at
`/audit-webhook`, and its `_count` series **is** the request counter: a histogram ships its own
observation count, so there is no separate one. The per-event census is `audit_events_total`.

`outcome` is bounded and covers the door as well as the batch: `bad_method`, `bad_path`,
`bare_endpoint_disabled`, `processed`, `empty`, `decode_error`, `process_error`. The first three are
the important addition: a request refused before decoding used to return before any instrument was
touched, so an apiserver posting audit to a path this operator does not serve looked exactly like an
apiserver posting nothing.

`bare_endpoint_disabled` is named for what it covers, and it is **not** "a route no
`KubeProvider` claims". A named `/audit-webhook/<route>` is accepted as-is, deliberately: a route
is a partition name, not a claim about an object, and refusing unknown ones dropped audit batches in
flight while a provider was being recreated. The only route-shaped rejection is the bare
`/audit-webhook` endpoint when `--author-attribution-audit-route-annotation-key` is unset.

```promql
sum by (outcome) (rate(gitopsreverser_audit_eventlist_duration_seconds_count[5m]))
```

**Per-event census.** `audit_events_total` increments exactly once per decoded event. `category`
is the coarse bucket of `outcome` (carried as its own label so the health invariant is a simple
selector). `audit_route` is the route the event was posted under, `/audit-webhook/<route>` or the
shared endpoint's annotation, when some `KubeProvider` declares it; `unrecognized` when none does,
and `none` for an event that names no route. A route is producer-chosen text, so only declared ones
become label values. A fresh pod reads every route as `unrecognized` until its `KubeProvider`s have
reconciled, which takes seconds.

| `category` | Live `outcome` values | Meaning |
| --- | --- | --- |
| `stored` | `queued` | Accepted; the event's facts reached the fact log. |
| `dropped` | `nil_event`, `stage`, `read_only_or_unknown_verb`, `failed_request`, `dry_run`, `unchanged_resource_version`, `non_scale_subresource`, `missing_cluster_annotation`, `no_attribution_fact` | Correctly rejected at the accept gate, or accepted and unable to name an author; not an error. `missing_cluster_annotation` is an event on the shared, annotation-routed endpoint that names no route; it is never credited to a fallback, so a rising rate is a producer that does not stamp the annotation. |
| `error` | `write_error` | The transport rejected the append for THIS event's stream, so its fact did not reach the log. Publication is per stream, so a request that fails partway still counts the events whose own stream appended as `queued`; the whole request is failed so the API server retries it, and the landed facts are appended again. The one category that should stay zero. |

The full enum lives in [`internal/audit/outcome/outcome.go`](../internal/audit/outcome/outcome.go);
it is the source of truth.

**`no_attribution_fact` is the one to read carefully.** The event was accepted and could name no
author, so nothing was appended for it and no watch event can ever join it. The usual cause is an
aggregated API: the kube-apiserver proxies the request and never decodes the response, so a CREATE's
`objectRef` carries no name at all. It is `dropped` rather than `error` because nothing failed, and
this is the only place it can be counted: the event never becomes a fact, so no fact-side counter
sees it. A rising share for a type you expect to attribute means commits for that type will be
authored unresolved:

```promql
sum by (group, resource) (rate(gitopsreverser_audit_events_total{outcome="no_attribution_fact"}[5m]))
```

**Is audit attribution alive?** Any positive rate means events are flowing:

```promql
sum(rate(gitopsreverser_audit_events_total[5m]))
```

**Whose audit stopped?** With several `KubeProvider`s, one cluster's API server can stop posting
while the others carry on, and the total stays positive. Per route:

```promql
sum by (audit_route) (rate(gitopsreverser_audit_events_total[15m])) == 0
```

A route missing from that result entirely has never posted since the pod started; the
`KubeProvider`'s `AuditFactsReceived` condition says which silence it is in. A steady
`audit_route="unrecognized"` rate is an API server posting under a route no `KubeProvider` carries:
its facts are never read.

**The health invariant.** Fact-store errors must be zero:

```promql
sum(rate(gitopsreverser_audit_events_total{category="error"}[5m]))
```

**Live audit stream by type: what is actually streaming in.** This is the per-type view of the
audit firehose; a type you expect to attribute but never see here means the audit policy is not
delivering it:

```promql
sum by (group, version, resource) (rate(gitopsreverser_audit_events_total[5m]))
```

**Have we ever seen audit for this type?** Useful for deciding whether attribution is worth
waiting on for a given type; zero over a long window means audit never delivers it:

```promql
sum by (group, resource) (increase(gitopsreverser_audit_events_total[1h])) > 0
```

**What strange or high-volume traffic is the webhook receiving?** Top event shapes by outcome
surface an unexpected flood at a glance:

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

**Are EventLists failing to decode?** Should be zero; non-zero means a sender is posting
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
tiers as misses, which they are not: they named an actor.

The two labels answer two different questions. **`tier`** names which evidence produced the author,
and it is ordered, strongest first. **`actor_kind`** names who that evidence named, in the same
vocabulary `git_commits_total{author_kind}` uses.

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
| `delete_sticky` | The sticky removal pointer: a fact whose own verb is a delete, filed by UID into a slot no later *write* fact may overwrite. Only a removal consults it. For a terminating object it is asked before `exact`, because the removal's resourceVersion is the one the deletion stamped, which a finalizer patch's own fact carries too. It is the only tier the fact TTL does not bound: a UID is unique across space and time, so the statement can never be superseded. |
| `exact` | Exact UID+resourceVersion match: this actor produced this exact version. For a removal of an object that was not terminating, it is the write that took the object out of view: a label exit. |
| `deletecollection_body_uid` | A removal whose UID was in the set the API server said a `deletecollection` deleted. No over-attribution risk: either the object was in that set or it was not. It outranks `latest`, because `latest` names whoever last *wrote* an object while a removal asks who *deleted* it. |
| `latest` | The UID-latest tier: the object's own last fact, keyed by UID alone. A removal accepts it only when that fact is a delete, which `delete_sticky` normally answers first, so it appears only when the sticky pointer was evicted under the index caps. A *write* here never answers a removal. |
| `name` | A match on `(namespace, name)` for a fact carrying neither a UID nor a resourceVersion: the usual shape of an aggregated API's audit event, and of a delete the API server answered with a `Status`. |
| `deletecollection_scope` | A removal matched to a `deletecollection` by scope alone (same type and namespace, the request's selector accepting the object's labels, within the collection window). The weakest evidence the join has, and the only one that can name the wrong actor, which is why it is reached only when every more specific tier missed. |
| `resource_version` | The RV-only escape hatch: a fact that carried a resourceVersion and no UID, matched on that version alone. |
| `absent` | No usable fact matched before the grace window elapsed. The resulting live commit is authored as `unknown (attribution unresolved)`. |

| `actor_kind` | Meaning |
| --- | --- |
| `user` | A human (or any non-service-account subject). |
| `serviceaccount` | A named service account: a controller, an operator, a CI identity. |
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

**Is the grace window paying for itself?** `event_kind` is `write` or `removal`, and the split is
the point: a removal waits for evidence about what caused it, which may never come (a type the
audit policy drops), while a write waits only for the fact at its own version. The removal wait is
the number `--author-attribution-grace` is tuned from, and a p95 approaching the configured grace
means removals are sitting out the whole window:

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
keeps its removals for a very long time at no cost, and a busy one keeps the most recent, which are
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
position passed (which is right, since it can never decode and stalling on it would cost every
later fact on that stream), so the facts it carried are gone. This is the loss path with **no other
symptom**: unlike a trim gap it is not detectable after the fact, and unlike a publish failure the
API server does not retry it.

Two things are refused: a payload that is not valid JSON, and one that is JSON but breaks the fact
contract by naming nobody (`author` must be present and non-empty: a fact exists to name somebody).
Either way the log line names the stream and the entry. Any non-zero rate means something is writing
entries this operator would not write: a version skew, another producer, or a hand-edited stream:

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
follower's first successful read, so a follower that has been wedged since startup (a transport
unreachable at boot) has no series at all, and `time() - <gauge>` therefore returns nothing rather
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
value is always 1, labeled `redis` or `memory`. It is a legend rather than a threshold, and it
changes how every metric above reads: a burst of unresolved commits after a restart is *expected*
under the in-process transport, which drops every fact with the process, and a *bug* under Redis.

```promql
gitopsreverser_attribution_transport_info
```

**How often does a collection delete fall back to scope matching?** A `deletecollection` fact
carries the UIDs the API server named, when it sent them, and joins by membership. When it cannot,
the join falls back to `deletecollection_scope`, which is correct but weaker, so the fallback is counted
rather than inferred:

```promql
sum by (reason) (rate(gitopsreverser_attribution_collection_without_uidset_total[5m]))
```

`reason` is `uid_cap` (the set was larger than `--author-attribution-collection-uid-cap`) or
`no_uids` (the API server sent a body with no usable UIDs). A body-less response (a truncated,
aggregated, or metadata-only `deletecollection`) produces no fact-level degradation event at all,
because there was never a set to drop; it resolves through the scope tier. A production
cluster running `--audit-webhook-truncate-enabled` is the one most likely to be in that case.

---

## API resource catalog

The API resource catalog is GitOps Reverser's single trusted in-memory view of the cluster's
served API surface: every `WatchRule` and `ClusterWatchRule` is resolved against it. The watch
manager refreshes it from Kubernetes discovery on its 30 s reconcile ticker, on every
CRD/APIService change, and on every rule change.

| Metric | Type | Labels |
| --- | --- | --- |
| `api_catalog_resources` | gauge | `source_cluster`, `state` (`allowed`/`excluded`) |
| `api_catalog_group_versions` | gauge | `source_cluster`, `state` (`trusted`/`degraded`) |
| `api_catalog_refresh_total` | counter | `source_cluster`, `outcome` (`changed`/`unchanged`/`error`) |
| `api_catalog_refresh_duration_seconds` | histogram | `source_cluster` |

`excluded` resources are the default-watch-policy set (pods, events, leases, jobs, …): served
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

**Discovery latency** catches a slow or non-aggregated apiserver. Normal is single-digit
milliseconds; the call is two cached GETs on Kubernetes ≥ 1.27:

```promql
histogram_quantile(0.95,
  rate(gitopsreverser_api_catalog_refresh_duration_seconds_bucket[5m]))
```

---

## Watch plane owner

One loop owns the watch plane: controllers post a request naming a GitTarget and return, and the
owner runs one plan pass per target once that target has been quiet for a 2 s rolling silence
window (hard cap ~10 s). Design:
[watch-manager-ownership.md](design/watch-manager-ownership.md).

That loop is a queue, and a queue that grows silently is exactly how a stall gets missed. These
five make it legible.

| Metric | Type | Labels |
| --- | --- | --- |
| `watch_plan_dirty_targets` | gauge | none |
| `watch_plan_oldest_dirty_since_timestamp_seconds` | gauge | none |
| `watch_plan_passes_total` | counter | `outcome` (`completed`/`failed`/`timed_out`), `gittarget_namespace`, `gittarget_name` |
| `watch_plan_pass_duration_seconds` | histogram | none |
| `watch_plan_triggers_total` | counter | `reason` (`declare`/`rule_change`/`shared_refresh`/`periodic`/`worker_replaced`), `coalesced` |

**Is anything stuck?** This is the one to alert on. The oldest dirty target's age climbs and stays
up exactly when a target cannot be planned (a source cluster that stopped answering, a catalog
that never becomes observable), whether the queue is deep or holds a single wedged target. A
healthy install sits under the settle window plus a pass, so seconds:

```promql
time() - gitopsreverser_watch_plan_oldest_dirty_since_timestamp_seconds
```

It is a **timestamp**, so `time() -` is required: a bare comparison against a duration is true of
every instant since 1970. A timestamp is used rather than a pre-computed age because an age has to
be recomputed to stay true, and the loop that would recompute it is the one that gets stuck.

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
plane means edits are arriving further apart than the window, which is not a fault: it is the
window doing nothing:

```promql
sum(rate(gitopsreverser_watch_plan_triggers_total{coalesced="true"}[15m]))
  / sum(rate(gitopsreverser_watch_plan_triggers_total[15m]))
```

**Which source is noisy?** `periodic` is the 30 s floor and is expected to dominate a quiet
cluster. A high `shared_refresh` rate means the API surface is flapping; a high `rule_change` rate
means something is rewriting rules. `worker_replaced` follows a `GitProvider` recreated for another
repository: the new branch worker has no clone, so every `GitTarget` on the branch re-establishes
its folder:

```promql
sum by (reason) (rate(gitopsreverser_watch_plan_triggers_total[15m]))
```

**How long does a pass take?** The input to choosing the per-target deadline. A pass is in-memory
replanning (discovery is a shared job done once per cluster, not per target), so normal is
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
| `secret_encryptions_total` | counter | One per encryption decision, labeled by the `GitTarget` (`gittarget_namespace`, `gittarget_name`) and `outcome`: `encrypted`, `failed` (the write is rejected), or `cached` (unchanged content reused). |

**Encryption failure rate** should be zero; non-zero means Secret writes are being rejected:

```promql
rate(gitopsreverser_secret_encryptions_total{outcome="failed"}[5m])
```

**Where is the encryption path spending its work?** One counter over one population, so the three
outcomes are shares of one total:

```promql
sum by (outcome) (rate(gitopsreverser_secret_encryptions_total[5m]))
```

---

## Suggested alerts

| Condition | Meaning |
| --- | --- |
| `rate(gitopsreverser_audit_events_total{category="error"}[10m]) > 0` | Attribution fact-store writes are failing; check Redis. |
| `sum by (audit_route) (rate(gitopsreverser_audit_events_total{audit_route!~"unrecognized\|none"}[15m])) == 0`, `for: 15m` | One cluster's API server stopped posting audit while it still did earlier in the pod's life; its commits are authored unresolved. A cluster whose audit policy forwards nothing for 15 minutes reads the same way. |
| `rate(gitopsreverser_audit_eventlist_duration_seconds_count{outcome="decode_error"}[10m]) > 0` | A sender is posting non-EventList payloads to `/audit-webhook`. |
| `rate(gitopsreverser_audit_eventlist_duration_seconds_count{outcome=~"bad_method\|bad_path\|bare_endpoint_disabled"}[15m]) > 0` | An apiserver is posting audit to a path this operator does not serve. |
| `rate(gitopsreverser_attribution_fact_stream_decode_errors_total[10m]) > 0` | A schema or version mismatch on the fact stream; facts are being skipped and lost. |
| `(time() - …_fact_follower_last_success_timestamp_seconds > 600) or (…_transport_info == 1 unless on() …_fact_follower_last_success_timestamp_seconds)`, `for: 10m` | The fact follower is wedged; attribution is degrading to committer-authored cluster-wide. Both arms are needed; see below. |
| `rate(gitopsreverser_git_resync_failures_total{reason=~"error\|timeout"}[15m]) > 0` sustained | Background resyncs are not committing; the folder relies on steady-state events to catch up. |
| `increase(gitopsreverser_apply_ownership_conflicts_total[1h]) > 0` | Another `Kustomization` or `Application` applies objects into a coordinated folder; they are kept out of Git, and the log names both appliers. |
| `increase(gitopsreverser_apply_claimed_resource_reconciled_total[1h]) > 0` | Somebody else made a held applier apply, unchecked against the branch; the labels name the applier claim. A steady rate on one claim is a controller that rewrites the `Kustomization` without `spec.suspend`. |
| `increase(gitopsreverser_apply_stale_snapshots_total{outcome="used"}[1h]) > 0` | An applier has been failing for at least 5 minutes, so a resync decided a snapshot taken while Git was ahead and reverted part of the unapplied commit. |
| `gitopsreverser_api_catalog_group_versions{state="degraded"} > 0` | Part of the API surface is hidden behind a broken APIService. |
| `time() - gitopsreverser_watch_plan_oldest_dirty_since_timestamp_seconds > 120`, `for: 5m` | A GitTarget cannot be planned (its source cluster or catalog is unreachable), and its mirror is not converging. The `time() -` is not optional: the gauge is a Unix timestamp, so a bare `> 120` is true of every instant since 1970 and would fire on ordinary planning. |
| `rate(gitopsreverser_watch_plan_passes_total{outcome="timed_out"}[15m]) > 0` sustained | Plan passes are hitting their per-target deadline; the target stays dirty and installs nothing. |
| `rate(gitopsreverser_secret_encryptions_total{outcome="failed"}[10m]) > 0` | Secret writes are being rejected by the encryption path. |
| `rate(gitopsreverser_git_queue_refusals_total[5m]) > 0 unless on(gitbranch_namespace, gitbranch_name) gitopsreverser_git_intake_paused == 1`, `for: 10m` | A healthy branch keeps refusing work: its queue is too small for its bursts. Producers offer the work again, so this is lag; only a watch cursor that expires first loses commits, and under `OnEvent` deletes. During an outage the intake-paused alert below covers it. |
| `gitopsreverser_git_queue_depth` rising and not draining | A branch worker is backing up against a stalled remote. |
| `time() - gitopsreverser_git_oldest_retained_write_timestamp_seconds > 900`, `for: 5m` | A branch has not published for 15 minutes: its writes, and any save riding them, are held. The `time() -` is not optional, as for the plan-dirty gauge above. |
| `gitopsreverser_git_intake_paused == 1`, `for: 10m` | An outage filled a branch's budget: new changes on its `GitTarget`s are refused until a push lands. |
| `sum by (gitbranch_namespace, gitbranch_name, reason) (increase(gitopsreverser_git_commit_failures_total[15m])) > 0` | Loss: a window died between routing and pushing, and its edits are missing from Git until the next resync. `reason="refused"` will not clear until someone fixes the Git path. |
| `sum by (gitbranch_namespace, gitbranch_name, reason) (increase(gitopsreverser_git_writes_dropped_total[15m])) > 0` | Loss: writes met a closed render-vs-live gate and were dropped, and nothing replays them. `render_fidelity_diverged` goes with `RenderMatchesLive=False` on a `GitTarget`; `render_fidelity_pending` is a hold that gave up. |
| `sum by (gittarget_namespace, gittarget_name, reason) (increase(gitopsreverser_placement_refusals_total[1h])) > 0` | Loss: new resources the writer declined to place are absent from the mirror until the placement policy is fixed. |
| `sum by (gittarget_namespace, gittarget_name) (increase(gitopsreverser_placement_kustomization_entries_total{outcome="failed"}[1h])) > 0` | A new file is in Git without its `resources:` entry, so kustomize never builds it. |
| `max by (gitbranch_namespace, gitbranch_name) (gitopsreverser_apply_operations_unresolved) > 0`, `for: 15m` | An applier is stuck mid-apply, and nothing more is asked of it until the operation completes. |
| `max by (gitbranch_namespace, gitbranch_name) (gitopsreverser_apply_appliers_held) > 0`, `for: 15m` | An applier is held behind the initial-snapshot barrier: a `GitTarget` naming its claim watches nothing, or its folder is missing. |
| `sum by (applier_kind, claim_namespace, claim_name, reason) (rate(gitopsreverser_apply_requests_total{outcome="failed"}[15m])) > 0`, `for: 30m` | Apply requests keep failing for the named applier claim; the worker retries at its backoff, and the cluster runs older content than the branch meanwhile. |
| `sum by (receiver_namespace, receiver_name) (increase(gitopsreverser_receiver_deliveries_total{outcome="rejected"}[15m])) > 0` | A Git host is calling a `GitReceiver` with the wrong secret, so foreign pushes are found only by the periodic refresh. |
| `sum by (gitbranch_namespace, gitbranch_name) (rate(gitopsreverser_git_commits_total{message_source="commit_request_fallback"}[15m])) > 0` | A `requestTemplate` stopped rendering: saves still commit, with the message verbatim. |
| `gitopsreverser_resource_condition{type="Ready", status="False"} == 1`, `for: 15m` | A declared object has not been accepted. The `== 1` is not optional: each condition publishes one series per status, so the selector alone matches the two zeroes too. |
| `gitopsreverser_resource_condition{type="Stalled", status="True"} == 1` | Permanently wedged: `Stalled` is the kstatus "nothing will retry" signal, so this needs a human and will not clear on its own. `reason` names the gate. |
| any `gitopsreverser_*` series with `otel_metric_overflow="true"` | A metric family exceeded its per-instrument cardinality cap, so the labels identifying those series are gone and every query over that family is under-reporting. Raise the cap or cut a label. |
| `sum by (group, resource) (rate(gitopsreverser_attribution_resolutions_total{tier="absent"}[10m])) > 0`, `for: 15m` | Events of this type keep waiting out the whole `--author-attribution-grace` and shipping `unknown (attribution unresolved)`. Each one held its watch stream for the full grace, so every later event of the type, including a save's edit, reached its branch worker that much later. Check the audit policy for the type and the `KubeProvider`'s audit route before anything else. An occasional absence is ordinary; a sustained rate is a configuration problem or a fact that can never arrive. |
| `sum by (gittarget_namespace, gittarget_name) (gitopsreverser_watch_types) == 0`, `for: 10m` | A GitTarget's rules resolve to no watchable type: a typo in `resources:`, or a type the default watch policy excludes. It mirrors nothing while looking healthy everywhere else. The 10 minutes let a freshly declared target settle. |

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
- **CommitRequest timing.** `commit_requests_total` counts how saves end; how long a save waited
  to attach (including the waits that ran out), how long it took end to end, and whether a
  `no_window` save was recorded in an empty commit are not emitted yet.
- **The reference dashboard and alert rules.** The queries in this document are the specification
  they are written from; the JSON and the rule files are not shipped yet.

## Adding a metric

The bar for a row here: a reader who has never seen the metric learns what it measures, one query
that answers a real operator question, and what a bad value looks like. A metric without an
interpretation is noise. The rest of the policy (a recording site before an instrument, and the
doc row in the same change) is
[metrics-observability-plan.md](design/metrics-observability-plan.md) §3.
