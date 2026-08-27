---
status: design
date: 2026-08-27
related:
  - target-watch-plan.md
  - data-plane-triggering.md
  - watch-and-catalog-architecture.md
---

# One owner for the watch plane: triggers in, work coalesced

> **design**: open, nothing built.
> Index: [`../INDEX.md`](../INDEX.md)

**The short version.** A rule edit is applied by the controller worker that observed
it, synchronously, and it re-plans every GitTarget in the process rather than the one
the rule names. The watch manager has no owner, so eleven mutexes stand in for one.
This proposes the opposite shape: controllers post a trigger and return, one loop owns
the state and does the work, and repeated triggers for a GitTarget collapse into one
pass over it.

This is a follow-up to [target-watch-plan.md](target-watch-plan.md), not part of it.
That plan made the WORK cheap, per cell. This one is about how often the work is asked
for, and by whom.

## What happens today

Both rule controllers call `Manager.ReconcileForRuleChange` inline, from six call sites
across `watchrule_controller.go`, `clusterwatchrule_controller.go` and
`watchrule_source_namespace.go`. The manager's own loop calls it too, on a 30s ticker
and on an API-surface trigger. That one function then does, in order:

1. `RefreshAPIResourceCatalog`, a discovery call;
2. `refreshSourceNamespaceScopes`, a namespace list;
3. `refreshWatchedTypeTables`;
4. `refreshRunningTargetWatches`, which walks **every** resident table and calls
   `replaceGitTargetWatches` for **every** running GitTarget.

The same reconcile then reads `StreamSummaryForWatchRule` back out and writes it to
status, so the read has to happen after the write, in the same pass, on the same
goroutine.

### The three costs, measured

**The fan-out is global.** One rule edit re-plans every target. In a single e2e run,
1256 plan reconciles were logged across 28 GitTargets, peaking at 78 in one second.
Change 2 of the target-watch plan made each of those nearly free for an unchanged
target, which is why this is a cost rather than an outage: the work is cheap now, but
it is still asked for N times when one target changed.

**It runs on a controller worker, behind network calls.** The two refreshes at the top
are I/O. A controller worker executing them is a worker not reconciling anything else,
and a call that blocks rather than fails takes that worker with it. This is not
hypothetical: the discovery call under `refreshClusterForDeclare` carries a request
timeout only for a REMOTE cluster, because `cluster_context.go` applies it under
`if !cc.isLocalLocked()`. On the local cluster the legacy, non-context
`ServerGroupsAndResources()` runs with no deadline at all.

**There is no owner, so there are eleven mutexes.** `clustersMu`,
`gitTargetClustersMu`, `resourceCatalogMu`, `triggersMu`, `targetWatchesMu`,
`gitPathEventsMu`, `gitTargetUIDsMu`, `gitTargetPruneModesMu`, `targetRetentionMu`,
`declaredGVRsMu`, `sourceNamespaceEventsMu`. They exist because the same state is
written by controller workers on three controllers, by the manager's own loop, by every
target-watch stream goroutine reporting readiness, and by branch-worker drain
goroutines reporting fidelity and retention, while status projections read it
concurrently. Each lock is individually defensible. The number is the smell.

## Half of the answer is already in the tree

`Manager.catalogRefreshCh` is a single-slot channel: `signalCatalogRefresh` does a
non-blocking send into a buffer of one, and the manager loop drains it beside the 30s
ticker. That is exactly a coalescing trigger, and API-surface changes (a CRD installed
or removed) already use it.

Rule changes bypass it and call straight through. So the shape being proposed here is
not new to this codebase; it is already the design for one trigger and not for the
others.

## The proposal

**One loop owns the watch plane. Everything else posts a trigger and returns.**

```text
WatchRule / ClusterWatchRule reconcile ─┐
GitTarget reconcile                     ├─→ trigger set ──→ owner loop ──→ declare
API-surface informer                    │   (coalescing)     (debounced)
periodic ticker                         ─┘
```

Three properties, in the order they matter:

**A trigger names a GitTarget where it can.** A rule edit knows its target, so it marks
that target dirty rather than asking for a global pass. A catalog change and the
periodic tick mark everything. The dirty set is the coalescing: ten edits to one
GitTarget between two passes are one pass over it, and an edit to target A does not
touch target B at all.

**A trigger is never work.** The controller does a non-blocking mark and returns. No
discovery call, no namespace list, no plan replacement on a controller worker. A slow
or hung API call can then only stall the owner loop, which is a bounded, observable
place with one job, rather than a shared worker pool.

**The loop paces itself.** A minimum interval between passes, on the order of seconds,
plus the existing 30s periodic sweep as the floor. Under a storm the loop runs at its
own cadence instead of once per observed event.

### What the loop owns, and what stays shared

The loop owns the mutable plan state: the resident tables, `targetWatches`,
`targetStreamStates`, the declared type sets, the per-target captures. If only the loop
writes them, most of the eleven mutexes collapse into "owned by the loop".

They do not all go. Two categories stay concurrent, and both are reads or
reports from other goroutines:

- **Status projections** the controllers call to publish conditions:
  `StreamSummaryForGitTarget`, `StreamSummaryForWatchRule`,
  `RenderFidelityForGitTarget`, `RetentionForGitTarget`,
  `GitPathAcceptanceForGitTarget`, `SourceClusterReachable`. These need a consistent
  read of a snapshot, which is a much weaker requirement than mutual exclusion around a
  read-modify-write.
- **Reports from the data plane**: a stream marking its cell replaying or streaming, a
  drain goroutine recording a fidelity or retention result. These are writes from
  outside the loop. Either they keep a small lock of their own, or they become messages
  the loop applies, which is the cleaner end state and the larger change.

## The decision this forces

**Status becomes eventually consistent, and the WatchRule reconcile can no longer read
back what it just caused.**

Today the sequence inside one reconcile is: apply the rule change, then read the stream
summary, then write status. With an owner loop, the apply is asynchronous, so the read
observes the state from before it. The rule's `StreamsRunning` would lag by up to one
loop interval.

That is already survivable, and the mechanism is already there:
`RequeueStreamSettleInterval` is 10s and exists for exactly this reason, because
streams do not reach Streaming inside the reconcile that started them either. The
honest framing is that the reconcile has never observed its own effect; it currently
observes an intermediate state that merely looks more immediate.

**A new GitTarget's first declare is the case to check.** Waiting a debounce interval
before the first watch opens is a real regression in cold-start latency. Two answers
are available: let a trigger request an immediate pass when the target has no plan yet,
or keep `DeclareForGitTarget` synchronous for a target the manager has never seen and
route only subsequent changes through the loop. The first is simpler and keeps one
path.

## What this does not change

- **The branch worker's queue.** The coalescing described in
  [target-watch-plan.md](target-watch-plan.md) is on the write path, where the hazard is
  a stale snapshot overtaking a newer write. This page is about the trigger side of the
  watch manager. They are different queues with different failure modes, and neither
  substitutes for the other.
- **The per-cell diff.** The plan classification is what makes an unnecessary pass
  cheap; this makes the pass unnecessary less often. Both are worth having.
- **Leader election.** The manager already runs only on the elected leader
  (`NeedLeaderElection`), so a single owner loop adds no new assumption.

## Open questions

1. **Does a failed pass retry, and with what backoff?** Today a failed declare is
   retried by whatever reconcile comes next, which is accidental but effective. An owner
   loop needs to say what happens: keep the target dirty and retry on the next tick is
   the obvious answer, and it needs a bound so a permanently unreachable source cluster
   does not spin.
2. **Does a delete race a pending trigger?** `ForgetGitTargetDeclaration` cancels and
   drops state. If a trigger for that target is still in the dirty set, the loop must
   not resurrect it. Dropping the dirty mark under the same lock as the forget is
   probably enough, and it needs to be stated.
3. **How is the dirty set observable?** A queue that silently grows is the thing that
   made the resync storm hard to see. A depth gauge and a per-pass count of targets
   re-planned would make this legible from metrics rather than from log volume.
4. **Do data-plane reports become messages to the loop, or keep their own locks?** The
   message form is cleaner and is the larger change; the lock form is a smaller diff
   that leaves the manager partly shared. This can be sequenced: locks first, messages
   later, if at all.
