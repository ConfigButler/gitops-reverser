# Review: the metrics and their labels, before the breaking release

Every instrument GitOps Reverser exports was read at its recording site. For each one, this review
asks whether it answers a question an operator has, and whether its labels let that operator find
the object to fix. This release breaks the API anyway, so a label change costs the least now. Every
finding below was acted on in the same branch.

## Scope

Reviewed on `docs-metrics-architecture-inventory` (PR #450) from `918fa804`: the 65 instruments
registered in [`internal/telemetry/exporter.go`](../../internal/telemetry/exporter.go) (38
counters, 8 histograms, 5 synchronous gauges, 14 observable gauges), their recording sites, and
[`interpreting-metrics.md`](../interpreting-metrics.md). The cardinality model is
[§7.1 of the plan](../design/metrics-observability-plan.md#71-cardinality-budget).

## Verdict

The surface is in good shape. Each stage of the pipeline counts once with a bounded `outcome`,
the loss paths are separate counters, the saturation gauges are read at scrape time, and every
instrument in the guide has an interpretation and a query. None of them is noise: each answers a
question that no condition, Event or log line answers as well.

The labels had one structural flaw. The branch worker's 21 families were labeled by
`{provider_namespace, provider_name, branch}`, a label set from before `GitBranch` existed. It did
not name the object an operator acts on, so a failing push could not be matched to the
`GitBranch` that reports it. The smaller findings follow.

## The branch worker's identity

Every branch-worker series now carries `{gitbranch_namespace, gitbranch_name, provider_namespace,
provider_name}`, set in one place (`BranchWorker.gitBranchAttrs`). The `GitBranch` is the object to
act on; the `GitProvider` groups the branches one credential writes, which is how an expired
credential shows. Neither adds a series: a worker serves one `GitBranch`, and a `GitBranch`'s
provider is immutable. `branch` is dropped because the `GitBranch` already names it.

A first version of this change dropped the provider too and published it on a separate
`git_branch_info` series for a join. That was undone: the join cost every "by provider" query a
`group_left`, and a `group_left` errors when two pods export the info series at once, as during a
rollout. Labels that add no series belong on the series.

`git_branch_targets` maps a `GitTarget` to its `spec.gitBranchRef`, so it names the target even
when the `GitBranch` or its provider does not resolve. Before, it published empty provider labels
in that case.

## Findings

| # | Finding | What was done |
| --- | --- | --- |
| R1 | `audit_events_total` carried no cluster. With several `KubeProvider`s posting audit, "whose audit stopped" could not be asked. | It gains `audit_route`: the route an event was posted under, when a live `KubeProvider` declares it (the reconcile publishes each provider's route into `attribution.RouteHealth`); `unrecognized` for any other route; `none` for a shared-endpoint event naming none. The raw route is producer-chosen, so it is never a label unchecked. The request histogram stays route-free: a histogram pays its bucket count per label. |
| R2 | `git_fetches_total{reason="bootstrap"}` could never emit. Its caller, `BranchWorker.EnsurePathBootstrapped`, had no production caller, and neither did `ensureRepositoryInitialized`: a worker's first contact is the publication cycle's fetch. | The dead entry point, its five helpers and the reason are deleted. `ensureRepositoryInitialized` moved to a test fixture. The template staging it wrapped is live, and its tests now call `ensureBootstrapTemplateInPath` directly. Ledger rows 1 and 2 keep their numbers, which docs cite, and measure the same two requests. |
| R3 | Suspected: `git_resync_failures_total` overlaps `git_commit_failures_total`. Traced: it does not, because a resync's commit failure never reaches `recordCommitFailure`. It did overlap two others: a resync refused at intake is also `git_queue_refusals_total{kind="resync"}`, and a resync deferred while the remote is unreachable stays pending, yet was counted as failed. | `git_resync_failures_total` gains `reason`: `error` and `timeout` lose a snapshot; `refused` and `deferred` lose nothing. A deferred reply wraps the new `git.ErrResyncDeferred`. The alert selects `reason=~"error\|timeout"`. |
| R4 | `attribution_resolution_wait_seconds` carried `tier`, `event_kind` and the type triple: as a histogram, the largest family in §7.1 (7,680 series in the model install). No query used the type on it. | The type is gone from the histogram; `attribution_resolutions_total` keeps it. |
| R5 | `secret_encryptions_total{outcome="failed"}` named no `GitTarget`. | It gains `gittarget_namespace` and `gittarget_name`, from the write batch's own target, which every path sets. |
| R6 | `InitOTLPExporter` set up a Prometheus exporter and printed to stdout. Nothing is pushed over OTLP. | Renamed `InitPrometheusExporter`; the prints are gone, and the no-op shutdown says why it is one. |
| R7 | `appliers_held`, `stale_snapshots_total` and `claimed_resource_reconciled_total` had no `apply_` prefix. | Renamed `apply_appliers_held`, `apply_stale_snapshots_total` and `apply_claimed_resource_reconciled_total`. |
| R8 | New, found tracing R3: nothing in production sent an atomic write request (`CommitModeAtomic`), so `kind="atomic"` on `git_commit_failures_total` and `git_writes_dropped_total`, and `close_reason="atomic_before_apply"`, never emitted. The commit-failure doc also called `atomic` "a snapshot/resync request", which it was not. | The atomic write path is deleted: `handleAtomicRequest`, `buildAtomicPendingWrite`, `PendingWriteAtomic`, `CommitMode`, and every `WriteRequest` field but `Events`. With one value left, `kind` comes off both counters. Its enqueue logs now name the event's GitTarget; they printed an always-empty request field. Tests of shared behavior were moved onto live writes. |

Other changes in the same pass:

- `deadcode -test -tags e2e ./...` found four functions nothing calls, in tests or any binary:
  `git.AuthFromSecretData`, `git.GetCommitSigner`, `giteaclient.Client.RegisterUserKeyAsUser` and
  `test/utils.UncommentCode`. They are deleted, and the sweep now reports nothing.

- `apply_requests_total` and `apply_revision_mismatches_total` had no identity at all. They carry
  the `GitBranch` and provider that asked and the claim that was asked.
- `watch_sessions_ended_total`, `watch_replay_duration_seconds` and `watch_event_handling_seconds`
  gain `source_cluster`: an `expired` storm is one API server's watch cache under pressure.
- The rule "object names are labels in one family only" was already false. It now says what §7.1
  says: configuration objects may be labels, mirrored resources never are.

## Family by family

**Configuration state** (`resource_condition`). The best-designed family: status as a label,
`Unknown` synthesized, deleted objects stop publishing. It keeps its generic `resource_namespace`
and `resource_name` keys, the Flux shape; matching a `GitBranch`'s conditions to its worker series
takes a `label_replace`, which the guide shows.

**Watch ingest.** Labeled by the `GitTarget` and the type, which is what "which tenant stopped
receiving events" needs; the per-type stream instruments now carry the cluster.

**Write boundary and windows.** Correctly per `GitTarget`: a document, a placement, a window and a
save each belong to one target.

**Branch worker and apply.** Useful and well split: pushes, retries, fetches, refusals, losses and
the retained backlog each answer a different question.

**Audit attribution.** The fact-loss trio stays three counters on purpose: their units differ.

**Catalog, watch plane, Secret encryption, receiver.** Right as they are, with R5 applied.

## Consequence to plan for

A dashboard or alert that selects a branch-worker series by `branch`, or names one of the three
renamed apply series, matches nothing after this release.
[`UPGRADING.md`](../UPGRADING.md) lists every series that changed.
