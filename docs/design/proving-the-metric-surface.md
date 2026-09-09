# Proving the metric surface: useful labels and trustworthy e2e checks

> **Design, partly built.** Date: 2026-09-09, written against `9341d668`.
> Index: [`../INDEX.md`](../INDEX.md)
> Related: [`metrics-observability-plan.md`](metrics-observability-plan.md) and
> [`../interpreting-metrics.md`](../interpreting-metrics.md).
>
> **Built** (`feat/metric-identity-and-e2e-isolation`): the GitTarget labels on
> `commit_requests_total` and the fallback for an unresolved target; `source_cluster` on
> `git_branch_targets`; the independent-trigger fix, which became a per-target assertion in the
> CommitRequest spec rather than cross-process bookkeeping; and isolation **option B**, measured at
> 27s and 29s on the two runs taken.
>
> **Not built**: step 5 (direct cluster labels on further families), diagnostics export and
> retention, the `e2e_run` scrape label (option C), a pinned-seed regression, and stubbed
> failure-path checks. The audit invariant is unreviewed under the query rules below.
>
> Read the sections on cardinality, where cluster labels belong, and counter observations as
> standing guidance; the options table and implementation order are kept as the record of why.

## Recommendation

Give operators enough identity to locate a failure. Add GitTarget identity to `commit_requests_total`
and source-cluster identity to the target mapping. Keep the number of series proportional to
configuration wherever possible. Use request status and correlated logs for individual saves.

For e2e, prove that instrumentation responds to known work, and isolate observations across runs.
Resetting Prometheus before a run is a supported design option. Prefer that option with a fresh
controller for the dedicated e2e environment, subject to measuring startup cost. Leave the environment
running after completion, including failure, and export diagnostics before their retention expires.

Ship this in small changes. Correct the current e2e checks before describing them as proof of the
metric surface. GitTarget labels are a useful product improvement to prioritize next. A broad
source-cluster rollout requires an instrument inventory and should be a separate change.

## Start from the operator's question

A metric is useful when its labels identify something the reader can investigate or act on.
Saving a small number of series at the cost of hiding which target fails is a poor default for a
controller serving many tenants.

| Question | Appropriate evidence |
|---|---|
| Are saves failing anywhere? | Aggregate outcome counter |
| Which GitTarget's saves fail? | Outcome counter with GitTarget identity |
| Which source cluster has trouble? | Cluster-owned metrics and target-to-cluster mapping |
| Which targets might a failed push affect? | Branch metrics joined to target mapping |
| Did this particular save land with its message? | CommitRequest status, commit SHA, and Git content |
| Why did this request fail? | Conditions and logs correlated by request identity |

An outcome-only counter cannot be joined back to a tenant or target: it has already discarded that
identity. The mapping can enrich an existing key; it cannot reconstruct a missing one.

A source cluster is also not necessarily a tenant. One cluster can serve several tenants, and one
tenant can use several targets. Dashboards should name the actual dimension they display.

## Cardinality policy

Allow bounded outcome enums and configuration identities that answer an operational question.
Keep per-request names, UIDs, commit SHAs, author identities, and arbitrary error text in logs or
object status. Test-run identities belong in the test harness or scrape configuration.

Cardinality depends on distinct label combinations, not the number of events. Five outcomes across
100 GitTargets produce at most 500 target/outcome combinations for one emitting process. Millions
of saves against those same targets reuse those combinations. Scrape-target labels and additional
independent dimensions increase the total.

Configuration identity is a growth bound, not a fixed limit. Short-lived targets create historical
series, and the telemetry SDK can retain counter series after targets disappear. Estimate both
active and retained combinations, including target churn, process restarts, and histogram buckets.
Check SDK aggregation limits and Prometheus resource use at the intended deployment size before
claiming a capacity guarantee. Document the measured target count and retention assumptions.

A label functionally determined by another label does not multiply every combination independently.
Adding `source_cluster` to an immutable GitTarget identity mostly adds label bytes and indexing cost.
Its value still needs to justify the extra public contract and recording-site maintenance.

### CommitRequest outcomes should identify the GitTarget

Propose this label set:

```text
commit_requests_total{outcome,gittarget_namespace,gittarget_name}
```

This lets an operator alert on one target, compare save outcomes across targets, and correlate the
result with the rest of the pipeline. Fleet queries remain straightforward:

```promql
sum by (outcome) (rate(gitopsreverser_commit_requests_total[15m]))
```

A target panel preserves the identity:

```promql
sum by (gittarget_namespace, gittarget_name, outcome) (
  rate(gitopsreverser_commit_requests_total[15m])
)
```

Keep the existing counting semantics: one terminal decision attempt outside the status-write retry
loop. Adding labels does not make this an exact request ledger across lost status writes or restarts.

Resolve identity from the command's configured target. Requests that fail before a target can be
resolved must still count. Use a documented fallback, such as empty target namespace and name, for
unresolvable references; preserve the requested reference in conditions and logs. Do not create
unbounded series from arbitrary nonexistent target names, or make metric recording block terminal
status on an additional failing API lookup. Reuse target identity already resolved by the workflow.

Migration work includes the metric inventory, dashboard examples, and an upgrade note. Queries that
expect one series per outcome need aggregation. Do not retain a second fleet-only counter with the
same events, which introduces another recording path to keep consistent.

## Where source-cluster labels belong

Use the identity the instrument's producer owns.

| Instrument family | Recommendation |
|---|---|
| Cluster discovery, catalog, and cluster-owned watch health | Carry `source_cluster` directly |
| Target mapping, `git_branch_targets` | Add `source_cluster` from `GitTarget.SourceCluster()` |
| GitTarget-owned event, document, placement, and request counters | Start with the mapping; evaluate direct labels for common cluster dashboards |
| Shared branch commits, pushes, retries, and queue depth | Keep provider/branch identity; join to potentially affected targets |
| Process-wide queue or transport health | Keep process/transport identity unless the producer has a real cluster partition |

`GitTarget.SourceCluster()` returns the referenced ClusterProvider name, with `default` as the
fallback. Use that existing contract. The internal empty config-plane sentinel is a separate
identity; do not rename it through the target mapping. A provider called `default` can reference a
remote cluster, so its name does not promise physical locality.

Direct `source_cluster` labels are worthwhile when they make a common alert substantially easier,
when a cluster operation has no target, or when historical attribution must survive target deletion.
A current mapping disappears with its target, so it cannot reliably enrich historical counters for
a deleted target. Direct labels retain the identity on each observation.

For an existing immutable target, redundant source identity is usually affordable. Avoid making
operators write joins for every routine question solely to save one label. First add the mapping,
then assess concrete dashboards and measured series cost for each proposed direct addition. Keep
label spelling and identity consistent across producers.

A branch can serve targets from different clusters. Joining its failed pushes to several clusters
means each is potentially affected; it does not allocate the failures between them. Summing the
expanded result can double-count the same push. Provide dashboard examples that preserve this
meaning, and never invent one owning cluster for a shared branch.

## What e2e should prove

Two defects motivated this plan. `FinalizeWindowMismatch` existed in types and controller mappings
without a real worker emission. A later metric check skipped when the counter was absent, so removing
its recording calls would still pass the suite.

Require an independent trigger for each metric assertion: an executed test, a terminal object, or a
verified commit. Use aggregated Ginkgo reports or explicit test observations across parallel
processes. Distinguish specs that were excluded, attempted, and completed the relevant operation.
A missing series must fail a check whose operation completed; it must not select the skip path.

| Layer | Assertion |
|---|---|
| Producer tests | Each supported outcome is produced by the real deciding code |
| Wiring tests | Known work reaches the deployed exporter and Prometheus with the expected labels |
| Run checks | Required signals appeared and no unexpected failures were observed during the run |
| Historical reports | Comparable runs retain approximate distributions and diagnostic artifacts |

Fakes remain useful for mapping and defensive unknown-value tests. They cannot be the only evidence
that a supported outcome occurs. Exercise real producer paths at the cheapest suitable test layer;
every failure mode does not need a separate expensive cluster test.

Namespace-scoped metrics can isolate many specs already: `testNamespaceFor` includes the Ginkgo
seed. That seed is repeatable for reproduction, so it is not a unique invocation ID. `TESTNAMESPACE`
and fixed-namespace suites also defeat namespace-based isolation. Use explicit run boundaries even
when selecting on a test namespace.

## Counter observations need a baseline

Separate detecting an observed failure from estimating how many events occurred.

`increase()` handles observable counter resets and extrapolates to the requested time range. It is
useful for approximate reports. It cannot recover an event before the first sample: a series first
seen at 1 and then remaining at 1 has zero observed increase. A series with too few samples may yield
no result. Treating either case as proof of zero failures is incorrect.

Initialize the bounded failure outcomes where practical, and wait for their baseline scrape before
work begins. After known work completes, wait for a scrape that includes it before asserting. At
controller restarts, account for new series and possible sampling gaps. Preserve missing-data checks
instead of using `or vector(0)` to hide a broken exporter.

With fresh counters and isolated history, a positive `max_over_time` can detect an observed failure,
including a first sample of 1. It does not reconstruct event totals across resets within a series.
Replacement pods commonly create distinct scrape series; evaluate each series before aggregation.
Retain request-status assertions because activity lost entirely between scrapes is invisible to any
PromQL expression.

Review the existing audit invariant under the same rules. Choose its query based on whether it asks
about any observed failure or an approximate total; do not mechanically replace every maximum with
an increase.

## Options for isolating runs

| Option | Benefit | Cost or limitation |
|---|---|---|
| A: retain Prometheus, record run boundaries and baselines | Fast warm startup; earlier runs remain queryable | More handling of resets, first samples, and shared counters |
| B: reset Prometheus and start a fresh controller before each run | Clearer history and counter ownership | Pod startup and scrape readiness add latency |
| C: add an `e2e_run` scrape-target label | Explicit run selection, including fixed namespaces | Configuration rollout and retained series; counters still need baselines |

### Retain history

Record start and end timestamps once in the synchronized suite lifecycle and distribute them to all
processes. Capture pre-work baselines and use the same boundaries in reports. Restrict queries to
the intended controller scrape targets. An elapsed-time range alone does not solve first-sample
loss or events emitted by leftover work from an earlier run.

This option is appropriate when warm startup speed matters more than implementation simplicity.
Namespace selectors remain useful for separating concurrent specs inside the run.

### Reset before the run, preserve afterward

Prefer this option for the dedicated e2e environment if a startup measurement shows acceptable cost.
Implement it once per invocation under the existing exclusive run lock, outside cached Task stamps.
A cached preparation task must not silently skip the reset.

1. Export any previous diagnostics that must survive the next reset, then complete prior-run resource
   cleanup and stop the old controller so it cannot keep emitting old work.
2. Replace the dedicated Prometheus pod with fresh data storage. The current manifest has no storage
   specification, so the Operator defaults to `emptyDir`. Verify this assumption in the setup check;
   replacing a pod with persistent storage would preserve its data.
3. Start a fresh controller. Resetting Prometheus alone leaves old application counter values ready
   to reappear on the next scrape.
4. Wait for exporter readiness, target discovery, and required baseline scrapes. Reconnect any
   port-forward affected by pod replacement. Only then start test activity.
5. Run the specs and collect final observations, including diagnostic collection on failure.
6. Leave Prometheus and the controller available for inspection after success or failure. Export
   reports and failure artifacts without deleting the live data.

Reset only at the next invocation's start. Do not add an end-of-run cleanup. Current retention is
two hours, so leaving Prometheus running alone does not preserve evidence indefinitely. Export
selected query results and logs, or a TSDB snapshot copied outside the pod, for later investigation.
Snapshot support needs an explicit setup decision because the admin API is disabled by default.

A smaller database may reduce query work, but startup has a cost. Measure total preparation and run
time on fresh and reused clusters before claiming a speed improvement. This option isolates runs;
parallel specs sharing the same target still require other evidence for exact attribution.

### Label scrapes by run

Stamp the run ID through the e2e ServiceMonitor's target relabeling, then wait until Prometheus has
adopted the configuration and scraped the new label before starting work. Apply it consistently to
replacement controller pods. Keep the label out of application code.

A new scrape label does not reset application counters. Capture baselines or restart the producer,
and handle first samples as in the other options. Prometheus `external_labels` concern communication
with external systems; they do not add run selectors to ordinary local queries.

Use this option when preserving several runs in one Prometheus is an explicit requirement. It is
not necessary to introduce it solely to distinguish one dedicated run after option B resets history.

## Implementation order and acceptance

1. Fix the current circular skip and report wording. Make missing instrumentation fail after a known
   successful operation, and allow excluded specs to remain inapplicable. Wait for final scrapes.
2. Implement one isolation option and its diagnostics lifecycle. Exercise consecutive runs, reuse
   of the same Ginkgo seed, fixed namespaces, and a controller restart within a run. An earlier
   failure must not fail the next clean run. A newly observed failure must fail its own run.
3. Add GitTarget identity to CommitRequest outcomes, preserving the fallback for unresolvable
   targets. Prove two targets are distinguishable, repeated saves reuse series, and terminal
   decisions still count once outside status conflict retries. Test the real exporter labels.
4. Extend the target mapping with `source_cluster`. Test local and remote provider identities,
   deletion, and multiple targets from different clusters sharing one branch.
5. Evaluate broader direct cluster labels against actual dashboard queries and a measured series
   budget. Record the selected families, the historical-query benefit, and the compatibility impact.

Add regression scenarios where one failure occurs before the first scrape, only one sample exists,
and the producer restarts before the final query. Missing evidence must remain visible. Verify that
removing recording calls makes the relevant wiring test fail. Check that diagnostics remain readable
after a failed run and exported artifacts remain usable after the next reset.

## Merge boundary

The branch's metrics and worker fix are useful independently of this larger plan. Merge them once
known defects in the current assertions are resolved and the required checks pass. If isolation is
not implemented in that change, keep the aggregate report explicitly diagnostic and avoid a gate
that attributes retained history to the current run.

GitTarget outcome labels are the first follow-up product change; they need not wait for a broad
cluster-label redesign. Keep that redesign separate so a useful observability increment does not
turn into a rewrite of every instrument. This document does not itself establish merge readiness
for any later branch head.

## Separate product question: commands in data planes

Keep CommitRequest in the configuration plane for this work. Remote submission deserves its own
proposal when users need to save using only tenant-cluster credentials. That proposal must specify
authorized target selection, submitter identity across the boundary, and delivery of status back to
the requester. Metrics do not require changing command placement.
