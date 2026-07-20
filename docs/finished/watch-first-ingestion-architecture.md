# Watch-first ingestion architecture — retired design record

> **Historical context only.** The current implementation contract is
> [`../architecture.md`](../architecture.md). This short record replaces the superseded rollout plan that
> previously occupied this path, so older links remain useful without presenting abandoned design work as live
> behaviour.

The shipped model is watch-first: one raw watch per `(GitTarget, GVR, scope)` supplies object state. A
watch’s initial replay and any recovery replay establish the desired state; Git is the persisted mirror.
Audit events never create or repair object state. They are optional evidence used only to name a live
mutation’s Git author.

## Current invariants

- A branch worker serializes writes. A commit window belongs to one GitTarget and either one named actor or
  an unnamed author state.
- With attribution disabled, and for replay/resync writes, Git uses the configured committer as author.
- With attribution enabled, a strong audit match names the authenticated actor. A weak, conflicting, late,
  or missing fact produces `unknown (attribution unresolved) <attribution-unresolved@gitops-reverser.invalid>`.
  A late fact never rewrites an already-created commit.
- Redis is optional in configured-author mode. It is required when audit-backed attribution is enabled and
  can also retain watch-resume cursors.
- A `CommitRequest` captures its command submitter at validating admission when that optional Redis-backed
  path is enabled. A request without a captured actor claims no actor; the matching window, not the request,
  determines the final Git author.

For the live details, use the [watch ingestion and reconcile](../architecture.md#watch-ingestion-and-reconcile),
[attribution](../architecture.md#attribution-audit-names-the-author-never-the-state), and
[CommitRequest finalize](../architecture.md#commitrequest-finalize) sections.

## Metrics

Metric names and operational interpretation are maintained in
[`../interpreting-metrics.md`](../interpreting-metrics.md). The telemetry design backlog is
[`../design/metrics-observability-plan.md`](../design/metrics-observability-plan.md); a planned metric is not
evidence that the runtime currently emits it.
