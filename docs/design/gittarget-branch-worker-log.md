# Branch worker write path as a log with one materializer

> **Plan, partly built on #413**, written 2026-10-02 against `main` at `0daa3711` (#412 merged).
> Steps 1, 1b, 2, 3a and 3b are built; step 3 was split into 3a, 3b and 3c after review.
> It replaces the narrow fixes first considered for gaps 3, 4 and 5 in
> [`gittarget-state-of-affairs.md`](gittarget-state-of-affairs.md#known-gaps-ranked-by-what-a-user-would-hit)
> with one refactor of the write path that closes gap 4 structurally and gives gaps 3 and 5 one
> place to act. It is the "transition boundary" step of
> [`branch-worker-event-model.md`](branch-worker-event-model.md#prepare-the-transition-boundary-for-later-durability),
> limited to the write path. Persistence stays with the
> [HA plan](../future/ha-gittarget-distribution-plan.md). Ships as one PR, one commit per step below.

## Goal

Fewer independent states and fewer execution paths. The worker already holds most of an
event-sourced write path, under other names; several helpers exist only to recreate the parts that
are not named. This plan makes those parts explicit and deletes the helpers. The finished change is
judged by the old machinery actually removed. Temporary growth is fine when
the deletions it enables are concrete, and comments are not trimmed to meet a number.

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

### Step 1: one materializer for the checkout (built)

`refactor(git)`, no behavior change; ledger byte-identical.

`checkoutApplied` counts the retained writes the checkout holds on top of its root. Unknown is a
dirty worktree, which only a reset clears and a push never does; a reset sets it to zero, so with
writes retained it says their commits must be replayed. It replaced `worktreeDirtyState`,
`replayRequiredState` and the `hasPendingCommits` parameter. One `materialize` replaced
`recoverRetainedWrites` and its six call sites, `invalidateAndRefresh`, and `refreshRemoteForResync`.
`ResyncRequest.RefreshRemote` went with it: every resync already fetches.

### Step 1b: clean a partly failed write locally (built)

`perf(git)`. A batch that fails part-way is reset to the commit it started on, locally, with its
leftovers discarded and base trust kept. Only a failed local undo leaves the worktree dirty for a
reset from the remote, so `fetches_total{reason="recovery"}` drops.

### Step 2: a decided window survives a failed materialization (gap 4, built)

`fix(git)`. A closed window enters the log before it is committed, and its save is
`WaitingForPush` from then on. An unreachable remote leaves it for the publication retry; while that
retry is pending, a decision that would need a connection waits for it. Only a failure of the write
itself is terminal. Pinned by `TestDecidedWrite_SurvivesAFailedRebuildAndLandsThroughTheRetry` and
`TestAttach_AnUnreachableRemoteHoldsTheRequest`.

### Step 3a: one write path (built)

`refactor(git)`. Review of step 2 found two defects and a structural gap: two write lifecycles
(`decide → materialize` for windows and saves, `materialize → commit → retain` for the rest),
`pcr.committed` reused to mean "decided", and an executor that re-entered itself through refusal
handling. This step closes them, in [`branch_log.go`](../../internal/git/branch_log.go):

- Every write kind (window, atomic batch, resync, a save's empty record, a refusal's empty commit)
  is decided into the log and committed by `materialize`. `l.commit`, `retain` and the commit guard
  are gone, with the separate resync, atomic and refusal-touch commit paths.
- One place classifies an attempt: `settleCommitted`, `settleFailed` (terminal for that write), or
  `settleUnreachable` (kept for the retry). A write's origin (the resync caller, the atomic request,
  the refusal a touch answers) rides with it so its outcome can be settled there.
- `materialize` is never re-entered. A refusal's empty commit decided while a refused write is being
  settled is appended to the log, and the running pass commits it in order. No push starts inside a
  pass.
- A write committed later than its own decision re-reads its prune policy, as a replay does
  (`TestDecidedWrite_ADeferredDeleteObeysATightenedPrunePolicy`, a review finding).
- Parent recovery never opens an obligation with nothing owed
  (`TestParentRecovery_AFailedEmptySaveOwesNothing`, a review finding).
- `pcr.committed` is gone: `attached` means bound to a write, a window's or a save's own record.
- `rebuildPendingWrites` folded into `replayOntoRemote`. Two replay functions remain on purpose:
  `replayOntoRemote` is the core the push cycle calls under its lock, and
  `refreshRemoteAndRebuildPendingWrites` is the locked entry the loop calls.

Still split: the log is loop state, while `checkoutApplied` lives on the worker because the Git
effect functions (commit, replay, reset, push) and resets outside the loop (path bootstrap) update
it. A resync whose remote cannot be reached still answers its caller and leaves the log, until 3c.

### Step 3b: one retry schedule (built)

`refactor(git)`, no retention change; ledger unchanged. One deadline,
[`retry.go`](../../internal/git/retry.go), replaces `publicationRetry` and parent recovery's
`backoff`, `nextProbeAt` and timer. When it fires with parent recovery open it probes with one
advertisement; otherwise it materializes and publishes. Whoever observes a failed attempt schedules
the next one, once; a new parent latch starts the schedule over, so the first probe is one initial
backoff away, as before. The push timer is only the success cooldown again. `publication_retry.go`,
`deferToRecovery` and the hand-off between the two clocks are gone. `parentProbeHold` stays: it is
how the worker-side base check, outside the loop, knows not to fetch for a parent the probe has not
found yet.

### Step 3c: missing-parent retention and owed snapshots

`fix(git)`. Writes decided while the parent is missing stay in the log instead of being dropped to a
snapshot, resyncs stay in the log when the remote cannot be reached
([Resyncs in the log](#resyncs-in-the-log)), and owed snapshots become log entries that replace
`scopes` and `awaitingPush`. Two decisions come first:

- **Admission.** Protecting every save-bearing entry from eviction cannot also guarantee a bounded
  log under unlimited arrivals. Either admission stops accepting work past a limit (backpressure on
  the watch producers and the save controller), or something with a save can be evicted.
- **Snapshot replacement.** "Same scope and not yet published" is not enough to let a newer resync
  replace an older one: the replacement must keep the writes decided between them and the save
  boundaries they carry.

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

## What is gone so far

| Removed | By |
|---|---|
| `worktreeDirtyState`, `replayRequiredState`, the `hasPendingCommits` parameter | step 1 (one count) |
| `recoverRetainedWrites` and its six call sites, `invalidateAndRefresh`, `refreshRemoteForResync`, `prepareBaseForResync` | steps 1 and 3a |
| `ResyncRequest.RefreshRemote`, end to end | step 1 |
| The fetch that cleaned a partly failed write | step 1b |
| `dropFailedWindow` and the network-failure drop branches | step 2 |
| The second write lifecycle (`l.commit`, `retain`, the commit guard), `pcr.committed`, `rebuildPendingWrites`, executor re-entry | step 3a |
| `publication_retry.go`, `deferToRecovery`, parent recovery's own backoff and timer | step 3b |

Still to remove: parent recovery's `scopes` and `awaitingPush` (3c).

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
