# Review: the metrics and their labels, before the breaking release

Every instrument GitOps Reverser exports was read at its recording site. For each one, this review
asks whether it answers a question an operator has, and whether its labels let that operator find
the object to fix. This release breaks the API anyway, so a label change costs the least now.

## Scope

Reviewed on `docs-metrics-architecture-inventory` (PR #450) at `918fa804`: the 66 instruments
registered in [`internal/telemetry/exporter.go`](../../internal/telemetry/exporter.go) (38
counters, 8 histograms, 5 synchronous gauges, 15 observable gauges, counting the one this review
adds), their recording sites, and [`interpreting-metrics.md`](../interpreting-metrics.md). The
cardinality model is [§7.1 of the plan](../design/metrics-observability-plan.md#71-cardinality-budget).

## Verdict

The surface is in good shape. Each stage of the pipeline counts once with a bounded `outcome`,
the loss paths are separate counters, the saturation gauges are read at scrape time, and every
instrument in the guide has an interpretation and a query. None of them is noise: each answers a
question that no condition, Event or log line answers as well.

The labels had one structural flaw, and this branch fixes it. The branch worker's 21 families were
labeled by `{provider_namespace, provider_name, branch}`. That label set predates `GitBranch` and no
longer names an object an operator can act on. Two of the three keys are a `GitBranch`'s immutable
spec, and the third is the branch name. You could not match a failing push to the `GitBranch` that
reports it, and `kubectl get gitbranch` had to be done by hand. Smaller gaps follow below.

## What this branch changes

| Change | Instruments | Why |
| --- | --- | --- |
| `gitbranch_namespace`, `gitbranch_name` replace `provider_namespace`, `provider_name`, `branch` | every branch-worker series: the 9 `git_*` counters and histogram and 6 `git_*` gauges the worker records, `apply_window_collisions_total`, `apply_echoes_total`, `apply_ownership_conflicts_total`, `stale_snapshots_total`, `apply_operations_unresolved`, `appliers_held` | One worker serves one `GitBranch`, so the series and the object share a name. One choke point (`BranchWorker.gitBranchAttrs`) labels all of them. |
| The join maps a `GitTarget` to its `GitBranch` | `git_branch_targets` | It is now `spec.gitBranchRef` alone, so a `GitTarget` whose `GitBranch` does not exist yet, or whose provider does not resolve, is still named. Before, it published empty provider labels in that case. |
| New `git_branch_info{gitbranch_*, provider_namespace, provider_name, branch}` | one series per `GitBranch` | Keeps "which provider is failing" answerable (an expired credential fails every branch it writes) without five identity keys on 21 families. Published for every `GitBranch`, the ones that hold no branch included. |
| `gitbranch_*`, `claim_namespace`, `claim_name` added | `apply_requests_total`, `apply_revision_mismatches_total` | These had no identity at all, so "which applier keeps failing" was a log search. The worker that asked and the claim it asked are both real owners, and the claim matches `claimed_resource_reconciled_total`. |
| `source_cluster` added | `watch_sessions_ended_total`, `watch_replay_duration_seconds`, `watch_event_handling_seconds` | An `expired` storm is one API server's watch cache under pressure. Summed across `KubeProvider`s it reads as everybody's. Same value `watch_types` carries; not the `GitTarget`, because two of the three are histograms. |
| Rule rewritten | `architecture.md`, `interpreting-metrics.md`, `resource_condition.go` | "Object names are labels in one family only" was already false (`gittarget_*`, `claim_*`, `receiver_*`). The rule is now what §7.1 says: configuration objects may be labels, mirrored resources never are. |

`resource_condition` keeps its generic `resource_namespace` and `resource_name` keys: one family
covers nine kinds and copies the Flux shape. Matching it to a branch-worker series takes a
`label_replace`, which the guide shows.

## Family by family

**Configuration state** (`resource_condition`). The best-designed family: status as a label,
`Unknown` synthesized, deleted objects stop publishing. No change.

**Watch ingest** (`watch_events_total`, `watch_recovery_total`, `watch_types`,
`watch_streams_open`). Right labels: the `GitTarget` and the type, which is what "which tenant
stopped receiving events" needs. The three per-type stream instruments lacked the cluster; fixed
above.

**Write boundary and windows** (`git_documents_total`, `placement*`,
`git_commit_windows_total`, `git_commit_window_duration_seconds`, `commit_requests_total`,
`git_resync_failures_total`). Correctly per `GitTarget`: a document, a placement, a window and a save
each belong to one target. No change.

**Branch worker** (`git_commits_total` through `git_next_retry_timestamp_seconds`). Useful and
well-split: pushes, retries, fetches, refusals, losses and the retained backlog each answer a
different question. Relabeled by `GitBranch` above.

**Apply requests and receiver**. The window and echo families were branch-worker series and moved
with them. The two request counters gained an identity. `receiver_deliveries_total` is right as
is: the `GitReceiver` is the object a wrong secret is fixed on.

**Audit attribution**. The counters and the fact-loss trio are sound, and their units are kept
apart on purpose. See R1 and R4 below.

**Catalog, watch plane, Secret encryption**. Right as they are, apart from R6.

## Recommended, not done

Ranked by what each costs a reader who needs it.

| # | Finding | Recommendation |
| --- | --- | --- |
| R1 | `audit_events_total` and `audit_eventlist_duration_seconds` carry no cluster. With several `KubeProvider`s posting audit, "whose audit stopped" cannot be asked. | Label by the `KubeProvider` the audit route resolves to, with one fixed value for a route nothing resolves. Never the raw route: it is client-supplied and unbounded. Additive, so it can follow this release. |
| R2 | `git_fetches_total{reason="bootstrap"}` can never emit. Its only caller, `BranchWorker.EnsurePathBootstrapped`, has no production caller; only tests reach it. The guide already says the series stays at zero. | Delete the dead path and the reason value together. A label value documented as always zero is noise. |
| R3 | `git_resync_failures_total` appears to count a resync whose atomic write failed with an error that `git_commit_failures_total{kind="atomic", reason="error"}` also counts. Its timeout arm is its own. Not traced end to end. | Confirm, then document the overlap the way `route_failed` and `placement_refusals_total` are documented: two views of one failure, never summed. |
| R4 | `attribution_resolution_wait_seconds` carries `tier`, `event_kind` and the type triple. As a histogram it is the most expensive family in §7.1 (7,680 series in the model install). No documented query uses the type on it, and `attribution_resolutions_total` already has it. | Drop `group`, `version`, `resource` from the histogram. Deferred once before; this release is the moment, because it removes labels. |
| R5 | `secret_encryptions_total{outcome="failed"}` names no `GitTarget`, so "whose Secrets are rejected" is a log search. | Add `gittarget_*` if `recordSecretEncryption`'s callers can pass the target without new plumbing. |
| R6 | `InitOTLPExporter` sets up a Prometheus exporter and prints to stdout with `fmt.Println`. Nothing is pushed over OTLP. | Rename it to what it does and log through the logger. Internal only. |
| R7 | The apply family is named inconsistently: `appliers_held`, `stale_snapshots_total` and `claimed_resource_reconciled_total` have no `apply_` prefix, so `{__name__=~"gitopsreverser_apply_.*"}` misses them. | Optional rename, only in a breaking release. Low value; listed so it is decided rather than forgotten. |

## Consequence to plan for

Dashboards and alerts keyed on `provider_namespace`, `provider_name` or `branch` on a branch-worker
series match nothing after this release. That is why #428 and the `GitBranch` work kept those
labels, for the gitops-api dashboards. Each such query moves to `gitbranch_namespace` and
`gitbranch_name`, or joins `git_branch_info` where it groups by provider.
[`UPGRADING.md`](../UPGRADING.md) lists every series that changed.
