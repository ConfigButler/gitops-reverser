# Watch event ordering under the attribution grace window

> **reference** — durable background. Index: [`../INDEX.md`](../INDEX.md)
>
> Status: **explainer / decision record**
> Date: 2026-06-26
> Related: [Watch-first ingestion architecture](../finished/watch-first-ingestion-architecture.md),
> [`internal/watch/target_watch.go`](../../internal/watch/target_watch.go),
> [`internal/watch/author_resolver.go`](../../internal/watch/author_resolver.go),
> [`internal/reconcile/git_target_event_stream.go`](../../internal/reconcile/git_target_event_stream.go),
> [`internal/git/branch_worker.go`](../../internal/git/branch_worker.go)

## The question

When a live watch event arrives, the resolver waits up to a bounded **grace window**
(`--author-attribution-grace`, default `3s`) for a matching audit fact before the event ships. Does that wait
**reorder** watch events relative to each other? Can the order of commits in Git end up different from
the order the mutations happened in the cluster?

Short answer: **for any single object — and for any single resource type — order is strictly
preserved. The grace window can never make an older mutation overwrite a newer one, and it never
reorders same-object events.** What it *can* change is throughput (it serializes a watch behind the
wait), the *grouping and number* of commits, and the relative commit timing of **unrelated** objects on
different types. None of those affect the materialized Git state.

## The execution model that makes this true

The guarantee falls directly out of how the watch data plane is wired.

**One goroutine per `(GitTarget, GVR, scope)` watch.** `replaceGitTargetWatches` starts one
`go runTargetWatch(...)` per claimed `(GVR, scope)`. Each watch has its own goroutine and its own event
loop ([`targetWatchReplayAndStream`](../../internal/watch/target_watch.go)):

```go
for {
    select {
    case <-ctx.Done():
        return nil
    case ev, ok := <-w.ResultChan():
        // handle ev fully — including the grace-window wait — before the next read
        nextReplaying, err := m.handleTargetWatchSessionEvent(ctx, ..., ev, replaying, &replay)
    }
}
```

**The resolver blocks *inline* in that loop.** On the live path,
`handleTargetWatchSessionEvent → routeLiveTargetWatchEvent → attachAuthor → AuthorResolver.ResolveAuthor`
all run synchronously in the watch goroutine. `ResolveAuthor` polls the attribution index and **sleeps
up to the grace window** before returning. Only after it returns is the event handed to
`RouteToGitTargetEventStream`, and only after *that* returns does the loop read the next event from the
channel.

So a single watch processes its events **strictly one at a time, in arrival order**, and the grace wait
is *head-of-line* on that one watch — it delays the next event but never lets it overtake the current
one.

**The downstream is a synchronous FIFO.**
[`GitTargetEventStream.OnWatchEvent`](../../internal/reconcile/git_target_event_stream.go) is a
pass-through: it stamps the GitTarget identity and calls `branchWorker.Enqueue(event)` with no buffering
or dedup. [`BranchWorker`](../../internal/git/branch_worker.go) is a single buffered `eventQueue`
channel drained by one loop, so **enqueue order is processing order**, and within an open commit window
repeated writes to the same path are last-write-wins.

The apiserver delivers a watch's events **already ordered by `resourceVersion`** for that type. The
chain above never reorders, so that cluster order is preserved all the way into the commit window.

## Why same-object order can never flip

The decisive fact: **an object belongs to exactly one `(GitTarget, GVR, scope)` watch.** All of its
events — every `MODIFIED`, the `DELETED` — flow through the *same* single-threaded goroutine, in RV
order. The grace wait blocks that goroutine, so event *N* (wait included) is fully enqueued before event
*N+1* is even read.

Worked example — two updates to the same ConfigMap, U1 (RV 100) then U2 (RV 101):

1. U1 arrives. The resolver waits for U1's fact (say it is slow and the full 3s elapses).
2. U2 is sitting in the watch channel buffer, **unread** — the goroutine is blocked on U1.
3. U1 resolves (matched or expired) and is enqueued to the BranchWorker.
4. *Now* U2 is read, resolved, and enqueued.

BranchWorker sees `U1, U2`; the window’s last-write-wins yields U2’s content. The newer mutation always
wins, and history shows U1 before U2. U2 can never "jump ahead" of U1, because it is not processed until
U1 is done.

This holds even when attribution is **mixed**. If U1 expires to the committer and U2 matches a human
author, the commit-window author bucket changes between them ([`open_window.go`](../../internal/git/open_window.go)
accepts one `(author, GitTarget)` pair at a time), so U1 finalizes as one commit (committer) and U2 as
the next (the human). That is *two* commits where a single-author burst would have produced one — but
they are still in order (U1 then U2), and the final file is U2.

## What the grace window *can* change (and why it is safe)

**1. Throughput / latency on a single type (the real cost).** Because the wait is head-of-line, one
unmatched event stalls its whole watch for up to the grace window. On a busy, status-churny type whose
events frequently have no audit fact, this serializes the stream and adds up to `grace` of latency per
unmatched event. This is the genuine price of doing the join inline; it is a *throughput* property, not
a correctness or ordering one. It is bounded (never a barrier), and it expires to the committer rather
than blocking state.

**2. Commit grouping and count.** As shown above, per-event author resolution can split what used to be
one commit into several, or coalesce differently than audit-first did. The watch-first design accepts
this explicitly: *more* commits when attribution is mixed, *fewer* when bursts/downtime collapse to
current state.

**3. Relative ordering of *unrelated* objects across different types.** Different `(GVR, scope)` watches
run **concurrently**, each with its own independent grace wait, and they all feed the *same* BranchWorker
FIFO. So if a ConfigMap event waits out its 3s grace while a Deployment event matches immediately, the
Deployment can be enqueued — and committed — **before** the ConfigMap, even if the ConfigMap mutation
happened first in wall-clock. This is safe because:

- they are **different objects on different types → different files in Git**, so there is no
  same-path race and the materialized state is identical regardless of interleaving;
- Kubernetes `resourceVersion` is **only comparable within one group/resource** anyway, so there was
  never a cross-type ordering guarantee to preserve;
- this concurrency already existed in watch-first before attribution (one goroutine per type); the grace
  window only *widens* the timing skew, it does not introduce a new class of reordering.

## Summary table

| Scenario | Order preserved? | Notes |
|---|---|---|
| Two events on the **same object** | **Yes, always** | Same single-threaded watch; serial; RV order kept. Newer never lost. |
| Two events on the **same type**, different objects | **Yes** | Same watch goroutine, FIFO into the worker. |
| Events on **different types** (different watches) | Not guaranteed (and never was) | Concurrent watches + independent grace; different files, so materialized state is unaffected; RV is not cross-type comparable. |
| Mixed attribution (some matched, some committer) | Same-object order preserved | May split into more commits via author bucketing. |
| A late fact arriving **after** a commit shipped | N/A | Never rewrites a shipped commit; the event already committed as committer. |

## Replay vs live

The grace window applies to the **live** path only. During a `sendInitialEvents` replay (Mode B), events
are folded into a single desired set and applied as **one committer-authored resync** (the mark-and-sweep)
with no per-event resolver and no per-event ordering concern — a replay is a snapshot, not an ordered
stream. The transition is clean: the replay resync is enqueued, then live events follow through the
resolver in RV order.

## If head-of-line latency ever becomes a problem (future)

The inline blocking wait is the simplest correct design and it is what ships today. If a deployment with
high-volume, low-attribution types finds the per-event grace latency unacceptable, the resolver could be
made **non-blocking** without sacrificing ordering, by decoupling the wait from the watch goroutine and
re-imposing per-watch order on egress:

- hand each live event to a per-`(GVR, scope)` ordered pipeline that starts the index lookup
  immediately and only *holds* an event until its grace expires **or** all earlier events on that watch
  have shipped (a sequence-numbered reassembly buffer);
- ship in sequence order, so a fast-resolving later event can never overtake a still-waiting earlier one
  on the same watch.

This preserves the exact ordering guarantees above while letting the watch goroutine keep reading. It is
deliberately **not** built yet: it adds a bounded per-watch buffer and reassembly logic, and the simple
inline wait is correct and adequate until measurements say otherwise. **Do not** parallelize attribution
per event without such a reassembly buffer — naive concurrency *would* let a fast-matched later event
overtake a slow earlier one on the same object and break same-object ordering.

## Bottom line

The grace window is a **per-event, bounded wait on a single-threaded watch**. It can delay a stream and
change how commits are grouped or how unrelated objects interleave, but it cannot reorder the events of
one object or one type, and it cannot let an older mutation overwrite a newer one. Same-object Git
history stays in cluster order; the materialized tree is always correct.
