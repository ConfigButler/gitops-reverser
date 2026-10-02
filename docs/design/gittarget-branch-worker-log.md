# Branch worker write path as a log with one materializer

> **Plan**, written 2026-10-02 against `main` at `0daa3711` (#412 merged). Nothing in it is built.
> It replaces the narrow fixes first considered for gaps 3, 4 and 5 in
> [`gittarget-state-of-affairs.md`](gittarget-state-of-affairs.md#known-gaps-ranked-by-what-a-user-would-hit)
> with one refactor of the write path that closes gap 4 structurally and gives gaps 3 and 5 one
> place to act. It is the "transition boundary" step of
> [`branch-worker-event-model.md`](branch-worker-event-model.md#prepare-the-transition-boundary-for-later-durability),
> limited to the write path. Persistence stays with the
> [HA plan](../future/ha-gittarget-distribution-plan.md). Ships as one PR, one commit per step below.

## Goal

Less code and fewer concepts. The worker already holds most of an event-sourced write path, under
other names; several helpers exist only to recreate the parts that are not named. This plan makes
those parts explicit and deletes the helpers. A step that does not shrink production code halts
the work, and is reported before anything merges.

## What the worker already has

| Event-sourcing role | Present today as | Helpers that compensate for it not being explicit |
|---|---|---|
| Log of decided writes | `pendingWrites`: each entry keeps its events, message, author, policy snapshot and attached save | A decision enters the log only after its local commit succeeds, so a failed commit discards the decision: `dropFailedWindow`, five drop branches, and parent recovery's dropped `scopes`, which re-derive lost decisions through snapshots |
| Projection of the log | The checkout: a root commit plus one commit per entry, which replay already rebuilds from the log | Three flags guess whether the projection is still valid (`worktreeDirty`, `replayRequired`, `rootParentStale()`); six `recoverRetainedWrites` call sites consult them, and `TestEveryLoopCommitPathRecoversADirtyWorktree` exists to catch a seventh that forgets; five functions rebuild the projection in slightly different ways |
| Deadline for an open obligation | Two clocks: `publicationRetry` and parent recovery's probe timer | `deferToRecovery`, the `parentProbeHold` atomic consulted in four places, `awaitingPush` |
| Outcomes | Saves resolve on push | `pcr.committed`, a flag that mirrors "this save rides an entry in the log" |

Gap 4 follows directly from the first row: `finalizeOpenWindowWithReason` runs the rebuild before
it builds the window, and a failed rebuild drops a window whose events, author, message and save
are all still intact.

## Target shape

Three pieces of loop-owned state:

- **`log`**: decided entries that are not yet published. Entry kinds are the existing
  `PendingWrite` kinds (window, atomic, resync, request record, refusal touch) plus one new kind,
  `owedSnapshot{scope}`, a placeholder for work that had to be discarded.
- **`checkout`**: `{root, rootGen, applied}`. The checkout equals `root` plus the commits of
  `log[:applied]`; `applied` unknown means the projection must be rebuilt.
- **`retry`**: the single obligation deadline, `{backoff, due, cause, since, lastErr}`.

Three operations:

- **`decide(entry)`** appends to the log. Write gates and building the write are checked here,
  the decision point the event model names. A network failure cannot fail a decision.
- **`materialize()`** is the only function that changes the checkout for a write. On a valid
  projection and a trusted base it commits the unmaterialized tail with no fetch; otherwise it
  resets to the remote tip and replays the log.
- **`publish()`** pushes the materialized log. A rejection caused by a moved remote invalidates the
  projection, re-materializes and retries (up to three times, as today). Success resolves the saves
  riding the published entries and removes them from the log.

The log and the retry deadline are the state the HA plan's journal would persist. This plan keeps
both in memory.

## Steps

Each step is one commit. Its red tests are written first.

### Step 1: one materializer for the checkout

`refactor(git)`, no behavior change.

- Add `checkout{root, rootGen, applied}`, `materialize(freshBase bool)` and `checkout.invalidate()`.
- Route the six commit paths (finalize, atomic, resync, request record, refusal touch, push) and the
  push's contention replay through `materialize`.
- Delete `recoverRetainedWrites`, `invalidateAndRefresh`, `refreshRemoteAndRebuildPendingWrites`,
  `replayOntoRemote`, `rebuildPendingWrites`, `prepareBaseForResync`, `refreshRemoteForResync`,
  and the `hasPendingCommits` parameter of `ensureBaseForCycle` and `commitPendingWrites`.
- Fold `replayRequiredState`, `rootParentStale()` and `pushCycleRoot*` into `checkout`;
  `worktreeDirtyState` becomes `applied` unknown.
- Delete `TestEveryLoopCommitPathRecoversADirtyWorktree`: only `materialize` can commit, so the
  invariant is structural. Rewrite the dirty-worktree, replay-failure and reset-cleanliness tests
  against `checkout`.

Gate: `git-roundtrip-ledger.golden` byte-identical, every existing behavioral test green.

### Step 2: a decided window survives a failed materialization (gap 4)

`fix(git)`.

- Finalize, atomic, request record and refusal touch call `decide`, then `materialize`. A network
  failure leaves the entry in the log; the retry deadline materializes and publishes it.
- An attached save moves to `WaitingForPush` at decision time. The worker holds it, so the
  controller never fails it while its write can still land
  ([`commitrequest-design.md`](../spec/commitrequest-design.md#lifecycle-and-outcomes)).
- Delete `dropFailedWindow`, the network-failure drop branches, and `pcr.committed` (derived from
  the log).
- An error building or executing a never-materialized entry stays terminal for that entry, as
  today. Gate decisions on the normal path are unchanged.

Red tests: a window whose rebuild fails is committed by the retry with its own author and message,
and its save resolves `Committed`; a new window during the failure spends no fetch.

### Step 3: one retry schedule, and owed snapshots in the log

`refactor(git)`.

- Merge `publicationRetry` and parent recovery's probe schedule into `retry`. When it fires with a
  missing parent it probes with one advertisement (ledger row 15 unchanged); otherwise it
  materializes and publishes. A parent change still makes it due at once.
- Delete `publication_retry.go`, `deferToRecovery`, `parentProbeHold` and its four checks (one
  check remains, in `materialize`), and the separate probe timer.
- Owed snapshots become log entries. An `owedSnapshot{scope}` replaces discarded work in one case
  only: byte-budget overflow of entries with no attached save. It settles when a resync covering
  its scope, decided after it, is published; log order replaces `awaitingPush`.
  `bumpSnapshotRequest` fires when the retry succeeds while owed entries remain.
- A resync is a log entry like any other (see [Resyncs in the log](#resyncs-in-the-log)). A newer
  resync of the same scope replaces an older one that is not yet published, the log's form of the
  coalescing the FIFO already does at enqueue.
- Delete `scopes`, `awaitingPush`, `noteResyncApplied`, `noteRecoveryPublished`,
  `recoveryTargets` and `closeRecoveryIfDone`.
- **Behavior change:** writes decided while the parent is missing stay in the log, bounded by
  `branchBufferMaxBytes`, instead of being dropped to a snapshot. A save on such a target waits in
  `WaitingForPush` instead of failing. `UPGRADING.md` records it.
- `ParentRecovery()` and the `RecoveringParentBranch` status keep their meaning, derived from
  `retry.cause` and the log.
- Rewrite the `TestParentRecovery_*` and `TestPublicationRetry_*` tests against observable
  behavior. The per-worker probe budget, a second outage, and a held save after a long recovery
  stay pinned.

### Step 1b: clean a partly failed write locally

`perf(git)`.

A write that fails part-way leaves staged changes behind. Today that marks the worktree dirty, and
the next commit fetches and resets to the remote tip to discard them. After step 1, `HEAD` is by
construction `root` plus `log[:applied]`, so a local hard reset to `HEAD` discards the same
leftovers without a remote round trip. Only a failed local reset falls back to the fetch.
`fetches_total{reason="recovery"}` drops accordingly, and
[`interpreting-metrics.md`](../interpreting-metrics.md) says so.

### Step 4: a refused replay drops only its own entry

`fix(git)`.

`executePendingWrites` aborts the whole replay at the first error, so one retained write that is
now refused blocks every write behind it indefinitely. During a replay, a refusal settles only its
entry: the refusal is reported, its save fails, and the entry leaves the log. A non-refusal error
still aborts the attempt and keeps everything for the retry. After step 1 this is a small change in
the one replay path.

### Step 5: show a publication that keeps failing (gap 5)

`feat(status)`.

- Each `GitTarget` on the worker reports `Reconciling=True` with the failure message, through the
  readiness path `RecoveringParentBranch` uses, under an existing reason. The message republishes
  only when the error changes; the start of the failure is recorded once; no per-attempt
  timestamp is written. One worker state feeds every target, so no target's status can clear
  another's.
- A held `CommitRequest`'s `WaitingForPush` message carries the same error.
- Count materialization failures, so a failed rebuild is no longer invisible to metrics. Update
  [`interpreting-metrics.md`](../interpreting-metrics.md) and `UPGRADING.md`.

### Step 6: deadlines on Git network calls (gap 3)

`fix(git)`.

- Measure first how go-git v6's HTTP and SSH transports behave against a server that accepts a
  connection and then stalls.
- Pass the context through `listRemoteRefs`, `CheckRepo` and `SmartFetchFrom`. Bound each call,
  and put one deadline over the whole of `publish()` so contention retries do not reset the
  budget. After step 1 that is two call sites, not every handler.
- The loop calls Git synchronously, so a deadline only cancels the call and no goroutine outlives
  it on the checkout. A transport that ignores the context gets a connection-level timeout, never
  a wrapper goroutine.
- A timed-out push is an uncertain outcome: the entries stay in the log and the projection is
  invalidated.

### Step 7: documentation

Update the event model's "Next implementation" and "Problems" sections, gaps 3 to 5 in the
state-of-affairs page, the gate table in
[`commitrequest-design.md`](../spec/commitrequest-design.md#when-the-target-may-not-be-written),
the effective-point comment in `write_gate.go`, [`architecture.md`](../architecture.md), and
`UPGRADING.md`. The PR body carries the argument; this page is updated to "built" or moved to
`docs/finished/`.

## Size estimate

| | Lines |
|---|---|
| Production code removed | about 550: the rebuild variants, three flags, `publication_retry.go`, half of `parent_recovery.go`, the drop paths |
| Production code added | about 300: `log`, `checkout`, `materialize`, `retry` |
| Net production code | about -200 to -300 |
| Test churn | about 1,500 lines across five files, rewritten against behavior instead of flags |

## What stays

The open window and its timers, FIFO admission and resync coalescing at enqueue, deferred heals
(still needed so a heal never closes a sibling target's window), the attach loop, the
refusal-touch schedule, `baseTrusted`, the standby state (`newBranchParent`), and the push's
compare-and-swap with the parent-change admission check.

## Invariants to keep

- `TestGitRoundTripLedger`: existing rows byte-identical; only explained rows added.
- Parent recovery's single probe budget per worker.
- The push admission boundary for a parent change (`admitPushRoot`).
- The write gates' effective points: on the normal path the decision and the local commit happen
  in the same step, as today. Under a failed materialization the gates were read at the decision,
  and a decided entry is materialized later like a retained commit is pushed after a gate closes.
- The controller never fails a request the worker holds.
- Compare-and-swap and refused-upload tests run against `startRealGitServer`, never `file://`.

## Resyncs in the log

A resync arrives on the same FIFO as every other write, so it belongs in the same log. It does two
separate jobs, and only one of them can wait:

- **A write**: make one scope of the folder match a desired set. It is decided into the log in
  arrival order and materialized against a fresh base (the deliberate pre-resync fetch stays, so
  ledger row 10 is unchanged). A committed resync is already retained and replayed today. When it
  cannot be materialized, it stays in the log like a window does. Its age does not matter: writes
  that arrived after it sit after it in the log, and a replay applies them on top.
- **A measurement**: its reply is what marks the scope clean for render fidelity and accepted for
  its Git path (`drainScopedResync` in
  [`event_router.go`](../../internal/watch/event_router.go)). That proof holds only against a fresh
  tree, and the caller waits at most five minutes. So a resync that cannot be materialized now
  still replies with the error, as today, and the scope stays unproven, which is the conservative
  direction. If the deferred write is refused when it finally materializes, step 4 reports that
  refusal on the target's status.

## Decisions

Settled 2026-10-02:

1. **Resyncs stay in the log**, as described in [Resyncs in the log](#resyncs-in-the-log). An owed
   snapshot is needed only for byte-budget overflow.
2. **A partly failed write is cleaned locally** (step 1b).
3. **Push before local e2e.** Commits are pushed once lint and unit tests pass, on a new branch;
   CI runs e2e, and the PR is not ready until e2e is green.

## Out of scope

Persisting the log or the deadlines, HA, an asynchronous executor, the success push cooldown, and
recording the other FIFO inputs (attach, withdraw, refresh) as transitions.
