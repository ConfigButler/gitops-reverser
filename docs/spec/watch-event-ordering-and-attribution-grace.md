# Watch event ordering under the attribution grace window

> **contract:** current watch processing and ordering boundaries. Index: [`../INDEX.md`](../INDEX.md)
>
> Status: **implementation explainer**
> Checked: 2026-09-28
> Related: [Kubernetes watch options](../facts/kubernetes-watch-options.md),
> [Watch-first ingestion architecture](../finished/watch-first-ingestion-architecture.md),
> [Attribution contract](attribution.md)

## The question

When a live watch event arrives, author resolution can wait up to the bounded
`--author-attribution-grace` window (default `3s`) for a matching audit fact. Can that wait
reorder events or change the order of Git commits?

**The wait preserves delivery order within one watch.** Its event loop resolves and routes
one event before reading the next. Different watches run concurrently, and overlapping watches
can observe the same object. The inline wait alone therefore does not establish a global
same-object or same-resource-type ordering guarantee.

## The execution model

For each `GitTarget`, [`targetWatchStreams`](../../internal/watch/target_watch.go) chooses one
served version per resource collection. Its [`CellKey`](../../internal/types/cell.go) contains
group, resource, and namespace selection; the containing target supplies target and source-cluster
context. An all-namespace collection and a named-namespace collection remain separate, so their
object sets can overlap.

Each running watch has its own goroutine. Its event pump handles an event synchronously before
reading another. On the live path, the call chain is:

```text
watch event
  -> routeLiveTargetWatchEvent
  -> attachAuthor
  -> AuthorResolver.ResolveAuthor
  -> RouteToGitTargetEventStream
  -> BranchWorker.Enqueue
```

Operation filtering and unchanged-content checks can discard events before attribution.
For events that need attribution,
[`ResolveAuthor`](../../internal/watch/author_resolver.go) calls the attribution index's
`Await` method inline. It waits for a resolution or the grace deadline, subject to cancellation.
Configured-author mode has no resolver and does not wait.

[`GitTargetEventStream.OnWatchEvent`](../../internal/reconcile/git_target_event_stream.go)
forwards events to the [branch worker](../../internal/git/branch_worker.go). The worker accepts
live writes into its FIFO queue. It preserves enqueue order; it does not merge independent
source streams by `resourceVersion`. A full queue causes the route to fail so that the watch
cursor does not advance past the refused event.

## Two updates on one watch

Suppose one watch delivers updates U1 and U2 for the same ConfigMap, in that order:

1. U1 reaches attribution and waits for a fact.
2. The watch loop has not yet read U2.
3. U1 resolves or expires, then enters the worker queue.
4. The loop reads U2, resolves it, and enqueues it after U1.

The attribution wait cannot make U2 overtake U1 on this path. If both updates are accepted and
applied to the same document, U2 supplies the later content. A commit window can combine both
updates into one commit, so Git need not retain U1 as a separate historical state.

Author resolution can change grouping. The [open commit window](../../internal/git/open_window.go)
groups compatible author and GitTarget work. Different authors can cause consecutive events
to land in separate commits while preserving their processing order.

With attribution enabled, an unresolved event uses the explicit
`unknown (attribution unresolved)` author. With attribution disabled, the configured committer
also authors the change. A fact arriving after a commit is written does not rewrite it.

### Preserve ordered release if attribution becomes concurrent

Do not parallelize per-event attribution without an in-order release buffer. A later event
with a fast lookup must wait for earlier events on its watch. Release an event only after its
own lookup resolves or expires **and** every predecessor has been released. Bound the buffer
and handle cancellation explicitly. The current inline path supplies this ordering without
such a buffer; the requirement does not establish ordering across different watches.

## Overlapping watches are a separate ordering problem

An object does **not** necessarily belong to exactly one watch. For example, a GitTarget can
follow ConfigMaps across all namespaces and also follow ConfigMaps in `team-a`. Both watches
can deliver `team-a/example`. The
[target watch plan](../design/target-watch-plan.md#where-the-bound-is-thin) explicitly records
that nothing orders these overlapping collections against one another.

Each watch preserves its own order, but that does not order their combined arrivals. If
watch A waits on an older event while watch B advances, a newer observation from B can reach
the worker before the older observation from A. A FIFO preserves that arrival order; it cannot
establish which observation is newer by itself. Content deduplication also does not establish
source mutation order.

This is a limitation of the existing design, not proof that every overlap causes stale output.
Correctness across overlapping sources needs a separate ordering or stale-event policy. Label
selectors make the issue more visible because two differently filtered watches can share objects.

## What the grace window changes

**Latency and throughput.** An unmatched event delays later events on its watch, including
those for other objects. The grace bounds each attribution wait; accumulated queueing can make
end-to-end delay longer. Unchanged sanitized updates are skipped before attribution, so they
do not pay this wait. Sustained delays can increase buffering and reconnect pressure.

**Commit grouping.** Waiting changes when events enter the worker, and author changes can split
commit windows. A watch-driven materialized view does not promise one commit per API mutation.

**Interleaving across watches.** Independent waits can change which watch reaches the worker
first. Different objects can share one multi-document YAML file. The single branch worker
serializes their edits; file separation is not the reason that writes avoid concurrent editing.
This serialization does not add source ordering across overlapping watches.

| Scenario | What is guaranteed by the inline wait and FIFO? |
|---|---|
| Two forwarded live events on one watch | The later event cannot overtake the earlier event during attribution and enqueue. |
| Two objects of one type on different watches | No common source order is established. |
| The same object on overlapping watches | Each watch is ordered internally; their merged arrival order can differ from mutation order. |
| Events from different resource types | No common mutation order is established; RVs are not a cross-type clock. |
| Mixed matched and unresolved attribution on one watch | The wait preserves enqueue order but may change commit grouping. |
| A fact arriving after its commit is written | The existing commit remains unchanged. |

## Initial state versus live events

During streaming initial events, the manager collects a desired set until the
`k8s.io/initial-events-end` bookmark. It submits that snapshot as a configured-author resync,
without per-object attribution waits. Initial objects represent current state; their delivery
order does not reconstruct historical mutations.

The accepted resync enters the worker queue before subsequent live events from that watch.
The watch does not wait for the Git write to finish before reading those live events. Queue
order and successful Git completion are distinct facts. Other watches can enqueue work during
this process, so this transition does not resolve overlapping-source ordering.
