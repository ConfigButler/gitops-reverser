# Branch worker write path as a log with one materializer

> **Plan, partly built on #413**, reviewed 2026-10-03 at `373bf8d7`.
> Steps 1 to 4, 5a, and 6 are built; step 3 was split into 3a, 3b, and 3c after review. Steps
> 5a2, 5b, 5c, and 7 remain, in that order. 5a2 is **planned for review**: it waits for approval
> of its decisions before any code changes. Git calls are bounded now, so the pause/resume state
> machine of 5b cannot be defeated by a stalled call.
> The [source review](gittarget-state-of-affairs.md#review-findings-and-remaining-gaps) records
> the remaining defects. This is the write-path part of the "transition boundary" in
> [`branch-worker-event-model.md`](branch-worker-event-model.md#prepare-the-transition-boundary-for-later-durability),
> limited to the write path. Persistence stays with the
> [HA plan](../future/ha-gittarget-distribution-plan.md). Ships as one PR, one commit per step below.

## Decision: pause intake, keep recovery running

When a Git outage fills the branch's capacity, pause new payload admission for that branch.
Keep its worker alive: retain accepted work, service lifecycle commands, and retry on the existing
backoff. Resume intake after the accepted backlog settles. Other branch workers keep running;
targets sharing the affected worker share its publication failure.

Backpressure protects memory. It cannot turn `kube-apiserver` into an indefinitely retained queue,
stop Kubernetes mutations, or preserve every mutation during an outage. A recoverable watch cursor
can redeliver unaccepted events; an expired cursor requires a fresh snapshot. That snapshot recovers
current state under the target's prune policy, with no reconstruction of missed intermediate
versions, authors, or save membership. With the default `prune.mode: onEvent`, a missed delete
cannot be inferred from absence alone; `always` permits a complete scoped snapshot to sweep it.

The near-term design is an in-memory log with explicit recovery. Restarting a Pod discards that
log and its save receipts, so restart is not the recovery mechanism. A durable Redis/Valkey journal
remains future work. Even that journal needs a capacity limit and an explicit admission-stop state.

## Goal

Fewer independent states and fewer execution paths. The worker already holds most of an
event-sourced write path, under other names; several helpers exist only to recreate the parts that
are not named. This plan makes those parts explicit and deletes the helpers. The finished change is
judged by the old machinery removed. Temporary growth is fine when
the deletions it enables are concrete, and comments are not trimmed to meet a number.

## Starting point before step 1

This table records the original machinery; the built steps below describe what replaced it.

| Event-sourcing role | Original representation | Helpers that compensated for it not being explicit |
|---|---|---|
| Log of decided writes | `pendingWrites`: each entry keeps its events, message, author, policy snapshot and attached save | A decision enters the log only after its local commit succeeds, so a failed commit discards the decision: `dropFailedWindow`, five drop branches, and parent recovery's dropped `scopes`, which re-derive lost decisions through snapshots |
| Projection of the log | The checkout: a root commit plus one commit per entry, which replay already rebuilds from the log | Three flags guess whether the projection is still valid (`worktreeDirty`, `replayRequired`, `rootParentStale()`); six `recoverRetainedWrites` call sites consult them, and `TestEveryLoopCommitPathRecoversADirtyWorktree` exists to catch a seventh that forgets; five functions rebuild the projection in slightly different ways |
| Deadline for an open obligation | Two clocks: `publicationRetry` and parent recovery's probe timer | `deferToRecovery`, the `parentProbeHold` atomic consulted in four places, `awaitingPush` |
| Outcomes | Saves resolve on push | `pcr.committed`, a flag that mirrors "this save rides an entry in the log" |

The original window-loss defect followed from the first row: `finalizeOpenWindowWithReason` ran
the rebuild before building the window, so a failed rebuild dropped a window whose events, author,
message, and save were still intact. Step 2 fixes it.

## Target shape

Three pieces of loop-owned state:

- **`log`**: decided entries that are not yet published. Entry kinds are the existing
  `PendingWrite` kinds (window, atomic, resync, request record, refusal touch). Accepted entries
  survive saturation. There is no `owedSnapshot` entry replacing discarded accepted work.
- **`checkout`**: `{root, rootGen, applied}`. The checkout equals `root` plus the commits of
  `log[:applied]`; `applied` unknown means the projection must be rebuilt.
- **`retry`**: the single obligation deadline. Built fields are `backoff`, `due`, and its timer;
  cause, first-failure time, and last error are proposed diagnostic state in step 5.

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

## Recovery contract

These are proposed operational states derived from the log, retry schedule, and admission state.
Do not add a second authoritative phase field or another retry timer.

| State | New payloads | Work the worker still performs | Exit |
|---|---|---|---|
| Running | Accept within capacity | Normal decisions, materialization, and publication | Retryable failure or capacity pressure |
| Retrying | Accept within remaining capacity | Local decisions; Git recovery only when due | Publication succeeds, or capacity fills |
| Paused | Refuse new writes, resyncs, and new saves | Already accepted FIFO work, window closure, lifecycle commands, due recovery | Accepted backlog settles |
| Recovering | Keep intake paused during the attempt | Probe if needed, materialize retained entries, publish | Failure returns to Paused; settled backlog reopens intake |

`Recovering` is the due attempt while paused. A successful advertisement or fetch alone does not
reopen admission: read access does not prove that a branch-protected remote accepts pushes. A
completed publication, or explicit terminal settlement of all accepted obligations, does. Keep
admission closed across partial replay progress and a transient drop below the high-water mark.
Include already admitted FIFO payloads, the open window, and deferred work when deciding whether
that backlog has settled. A healthy FIFO that briefly fills waits for capacity; it must not wait
for a retry deadline that does not exist.

The worker continues consuming accepted FIFO items into decisions, so controls behind them can
run. It does not discard those items or move withdrawals ahead of their attaches. Bound that
accepted set at enqueue; stopping intake after transferring it into the log is too late to make
a strict memory promise. Check due recovery between items so a busy FIFO cannot starve it.

The scope is the existing `(GitProvider namespace, name, branch)` worker. Do not stop the manager,
its status controllers, admission endpoints, or unrelated workers. Worker retirement and Pod
termination keep their explicit shutdown behavior; they cannot be described as pause/resume.

### Resume and the three meanings of replay

1. Replay **accepted decisions** from the retained log onto the remote when the checkout requires
   rebuilding. Preserve order, messages, attribution, and save boundaries. A terminal refusal
   settles only its entry, as step 4 establishes.
2. Resume **unaccepted observations** from the last safely admitted watch cursor after intake
   reopens. While paused, close the affected sessions and wait for branch capacity, with a
   cancellation-safe wakeup and a bounded fallback recheck. Do not reconnect and collect rejected
   snapshots every two seconds. A stale wakeup rechecks capacity before doing work.
3. If the cursor expired, gather a **fresh complete scoped snapshot** through the existing replay
   path, after retained work. An incomplete snapshot never authorizes a sweep. Live work from that
   session follows its snapshot. Existing scope, coalescing fences, and prune policy still apply.

Watch catch-up is a separate observation from Git publication recovery. Admission can reopen while
a scope is still replaying; the existing stream/readiness state must continue showing that scope
as unproven. A published old backlog does not prove the cluster is current. Record cursor expiry
and the history limitation; do not turn a fresh snapshot into a receipt for a missed save.

If a process restarts, `runTargetWatch` starts a fresh replay even with a stored cursor. This can
repair current object content, subject to write gates and pruning. It cannot restore lost decided
writes, intermediate history, or the identity of an empty save already published before the crash.

### Review findings that gate the next implementation

Source review at `373bf8d7` confirms the log path and step 4 refusal isolation. It also finds:

- **Deduplication preceded acceptance** (fixed in step 5a). `skipUnchangedLiveUpdate` stored the
  content hash before enqueue, so a refused UPDATE was skipped on cursor resume, which then
  advanced the cursor.
- **The byte budget is incomplete.** `syncAdmission` uses `pendingWritesBytes` and a pending retry.
  `buildRequestRecordWrite` and `buildRefusalTouchWrite` leave `ByteSize` at zero. Repeated empty
  saves can grow the log without reaching the byte threshold. The FIFO limits item count, while
  individual snapshots and batches can be large. Add accounting for entries and payloads before
  describing retention as bounded; include registered saves and deferred snapshots.
- **Retry pacing has an exception.** `decide` lets resyncs materialize during a pending retry.
  Without the missing-parent hold, those resyncs can spend fetches before the retry deadline.
  A deferred resync can answer its caller with the known retryable failure and remain retained;
  the answer must not require a fresh failing connection.
- **Watch recovery currently polls.** `runTargetWatch` reconnects after its fixed two-second
  backoff. It has no wait for the worker to reopen admission. Queue saturation becomes repeated
  work and a generic watch error, rather than a publication-pause explanation.
- **Git calls were unbounded** (fixed in step 6). **Failure status is incomplete**; step 5c
  addresses it.

These are follow-up requirements, not runtime fixes made by this documentation revision.

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

### Step 2: a decided window survives a failed materialization (built)

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

### Step 3c: missing-parent retention, and admission backpressure (built)

`fix(git)`. Writes, saves and resyncs decided while the parent is missing stay in the log, like any
other a remote failure holds back, and are published when the probe finds the parent. A resync the
remote holds back answers its caller with the error at once, because its caller is waiting to hear
what it found, and stays in its place in the log; when it is applied later, its outcome is reported
the way a live write's is. Two decisions, settled 2026-10-02:

- **Admission backpressure.** Nothing decided is evicted for capacity. While the log holds the retained-byte
  budget and a failed attempt waits for its retry, the worker refuses new writes, saves and resyncs
  at enqueue, through the existing queue-full contract. The intended producer behavior is cursor
  resume for a refused event, controller retry for a save, and recollection for a resync. The review
  above identifies the deduplication hole and the watch-history limit on that intent. A healthy
  branch does not close this admission gate. Its threshold excludes the open window, queued
  payloads, deferred snapshots, and zero-byte entries; it is not a process memory ceiling.
- **No snapshot replacement.** Resyncs replay in arrival order, so the writes decided between two of
  them and every save boundary are kept. The admission threshold limits payload growth; the FIFO
  still coalesces resyncs that are only queued.

With nothing dropped, nothing is owed a snapshot: parent recovery's `scopes`, `awaitingPush`, the
snapshot-request sequence and the controller's snapshot-request tracker are gone. The parent
recovery tests now pin that writes decided across one or two outages are kept and published, and
that a failure leaving nothing in the log opens no obligation.

### Step 4: a refused replay drops only its own entry (built)

`fix(git)`. A replay used to abort at its first error, so one retained write that the moved remote
now refused blocked every write behind it indefinitely. `replayPendingWrites` now undoes a refused
write on its own, goes on, and stamps the entry with its refusal; a replay that completes holds the
others, and `checkoutApplied` counts only those. The loop takes stamped entries out of the log after
either replay (the loop's rebuild in `materializePrefix`, the push cycle's after a rejection) and
settles each as a refusal at first commit is settled: the refusal is reported, its save fails. Any
other failure still abandons the replay and keeps everything, refused write included, for the
retry. A committed resync is marked answered, so a replay that refuses it later reports the refusal
on the target instead of answering its caller twice. Pinned by `replay_refusal_test.go`.

### Step 5a: make refused admission safe (built)

`fix(watch)`. A live event changes the watch's state only once the branch worker accepted it.

- **Deduplication records only accepted content.** `checkLiveContent` reads the per-object content
  hash before routing; `acceptLiveContent` records it after the worker's `Enqueue` returned true.
  A refused UPDATE therefore stays a change when the cursor resume redelivers it. The record is a
  compare-and-swap from the entry the check saw: when an overlapping stream (a cluster-wide and a
  namespaced stream deliver the same object) accepted a different version in between, the entry is
  cleared, because which version the worker took last is unknown. No baseline routes the next
  UPDATE; a wrong baseline would skip one.
- **An event a stopping stream never enqueued records no cursor.** The shutdown arm returned the
  event's resourceVersion, and the cursor moved past an event the worker never saw.
- **A stream resumes from a cursor only after its own replay completed.** `runTargetWatch` used to
  resume after the first session ended, however it ended. When the worker refused that session's
  snapshot (admission backpressure) or the watch failed mid-replay, the reconnect resumed from a
  previous stream's cursor (possibly one the shutdown arm had advanced) and skipped this stream's
  snapshot, its sweep, and its render-fidelity report.
- **`EnqueueRequest` is gone.** It hid its enqueue boolean, and nothing outside `internal/git` tests
  called it; those tests use the unexported `enqueueRequest`.

Pinned by `refused_admission_test.go` (route + cursor through the producer, cancellation, and
resume-after-own-replay; each was reproduced red against the old behavior) and the overlap cases
in `live_content_dedup_test.go`.

Producer inventory, after this step:

| Producer | Worker API | Refusal reaches | Redelivery |
|---|---|---|---|
| Live watch event | `Enqueue` → bool | `routeLiveTargetWatchEvent` error; session ends, cursor not advanced | Cursor resume redelivers the frame |
| Watch replay / LIST fallback snapshot | `EnqueueResync` → bool, plus the reply | `enqueueReplayResync` error; no cursor, stream not marked replayed | The reconnect replays again |
| CommitRequest attach / withdraw | `EnqueueAttach`, `EnqueueWithdraw`, fire-and-forget | Nothing synchronous | The controller re-sends on its next poll |
| Refresh | `EnqueueRefresh`, fire-and-forget | Nothing synchronous | The next `GitTarget` reconcile asks again |

Remaining limits, for 5b:

- Refusal still ends the watch session, and the reconnect retries after the fixed two-second
  backoff. A refused snapshot is now gathered again on every attempt until the worker accepts one,
  where before it resumed from a cursor it had not earned. That is correct, and it costs one LIST
  or replay per attempt while intake is closed. 5b's wait for capacity removes the polling.
- When two overlapping streams accept different versions, the cleared entry lets the next
  `/status`-only UPDATE through once. It is harmless to content, and it can split an open window
  on the author flip, as any unattributed event can. Two streams can still hand the worker two
  versions out of order; deduplication neither causes nor repairs that.
- The cache is in memory. A restart starts every stream with a replay, so a lost cache only routes
  extra UPDATEs.

### Step 5a2: the unchanged filter belongs to its stream (planned, for review)

`refactor(watch)`, with one `fix`. Nothing here is built; the decisions below wait for review.

**What the filter is.** Before routing a live UPDATE, the watch compares its sanitized content
(what would be written to Git) with the content of the last event the worker accepted for that
object. Equal content means the event carries nothing for Git, so it is dropped and counted as
`watch_events_total{outcome="unchanged"}`. CREATE and DELETE always pass. The typical dropped event
is a `/status`-only update.

**Why it stays.** It is a filter against changes that are useless to Git, and two things depend on
it:

1. *Commit windows.* A `/status`-only update arrives with no author, because the audit policy
   drops `/status` writes. Routed, it meets a window opened by a named author, forces an
   identity-change close, and splits a save's collect window into two commits. This broke the
   "one commit" `CommitRequest` e2e specs before the filter existed (`58dd37a8`), and the
   [save-wait guidance](commitrequest-save-wait-options.md) relies on it.
2. *Load.* Status churn on Pods and Deployments would otherwise fill the branch FIFO, and during a
   Git outage close admission sooner.

Git content does not depend on it: a routed no-op finds no diff and commits nothing.

**What changes, and why.**

| Today (after 5a) | Planned |
|---|---|
| One process-wide `sync.Map` (`Manager.liveContentDedup`), keyed by `(GitTarget, GVR, UID)`, shared by every stream that delivers the object | A plain map owned by the stream, keyed by UID; the stream's goroutine is its only reader and writer |
| Recording is a compare-and-swap from the entry the check read; a conflict with another stream clears the entry | Recording is a plain store after the worker accepted the event |
| The memory survives a replay inside the stream, and outlives the stream | Reset whenever a replay of the stream is accepted; dropped with the stream |
| Entries for objects that leave the selection, or for a deleted `GitTarget`, are never removed | Freed when the stream stops |

Arguments:

- **The cross-stream logic exists only because the memory is shared.** Two overlapping collections
  (an all-namespace one and a named-namespace one) deliver the same object independently. With one
  shared entry, each stream's record can overwrite the other's, so step 5a needed the
  compare-and-swap and a conflict rule. Per stream, there is nothing to coordinate: a stream compares
  only with what it delivered and the worker accepted.
- **Per stream filters no worse, and heals better.** Every overlapping stream sees every change to
  the object, because they watch the same object. If one stream lags and the worker receives an older
  version last, today's shared memory can then filter the other stream's next no-op and keep the
  stale version in Git. Per stream, the lagging stream's next event still passes its own filter and
  heals it.
- **The cost is duplicates that were filtered before.** When both streams deliver the same change,
  the second copy used to be filtered against the first; now it reaches the worker. It carries the
  same content and the same resourceVersion, so attribution names the same author and it joins the
  same window: no split, no extra commit, one more FIFO item. Only overlapping collections pay this,
  and only for real changes, not for status churn, which each stream still filters.
- **It fixes a loss the shared memory has today.** A replay inside a stream (after a 410, or for a
  stream replaced on the same collection) writes current state through a resync, which never
  touches the filter's memory. Baseline X, the object changes to Z during the gap, the replay
  writes Z, the object returns to X: the live X matches the stale baseline, is dropped as unchanged,
  and Git keeps Z until the next change or replay. Resetting the stream's memory when its replay is
  accepted (where `markReplayed` runs today) removes the stale baseline. The first no-op after a
  replay then passes once, which costs nothing: no window is open for it to split unless a write
  opened one, and then it carries that write's own fresh content.
- **No locks are needed.** A stream is single-threaded (`routeLiveTargetWatchEvent` runs on its
  goroutine), so the map needs no `sync.Map`, compare-and-swap, or conflict handling.

**What is removed.** `Manager.liveContentDedup`, the `prev`/`hadPrev` fields and the
compare-and-swap in `acceptLiveContent`, and the two overlapping-stream tests that pin the conflict
rule. **What stays.** The check-before-route and record-after-acceptance split from 5a, the DELETE
rule, fail-open on content that cannot be hashed, and the `unchanged` metric outcome.

**Tests.** The 5a producer tests stay as they are. New: a replay inside one stream resets the
memory (the X, Z, X sequence above, red today); two streams each filter their own no-op and each
route a real change; a stopped stream leaves nothing behind.

**Decisions for review.**

1. **Per-stream ownership** as described, including the reset at an accepted replay.
2. **The name.** The concept already has a public name, the `unchanged` outcome of
   `watch_events_total`, and [`definitions.md`](../definitions.md) rule 1 asks for one word per
   concept everywhere. Recommendation: **the unchanged filter** (`unchangedFilter` in code), with
   `liveContentDedup`, `checkLiveContent`, and `acceptLiveContent` renamed to match, and a
   `definitions.md` entry. "Filter spec changes" was proposed; it reads as filtering *out* spec
   changes, which is the opposite of what the filter does (it passes content changes and drops
   the rest), and "spec" is narrower than what is compared: the whole sanitized object, labels and
   annotations included.
3. **Reset or seed at replay.** Recommendation: reset. Seeding the memory with the replay's
   snapshot content would also filter the first no-op after a replay, but it ties the filter to the
   snapshot format for the sake of one event.

### Step 5b: pause and resume under capacity pressure

`fix(git)`. Implement the recovery contract after step 6 bounds synchronous Git work.

- Use byte and count admission budgets covering queued payloads, open/decided work, deferred
  resyncs, and pending saves. Give empty records a nonzero charge. State measured overhead and
  any bounded overshoot; do not advertise the existing 8 MiB threshold as a heap limit.
- Decide the oversized-item path explicitly: reject before acceptance with a capacity diagnostic
  and keep the scope unproven until capacity/configuration changes. Never spin on a snapshot that
  can never fit, partially apply it, or invent permission to sweep. Chunked snapshots remain a
  separate extension if required by measured workloads.
- Keep a small bounded allowance for lifecycle work, with existing FIFO causality intact.
  Outcome reads and duplicate attaches must not allocate a second obligation. Test eventual
  withdrawal progress while intake is closed.
- Pause affected producers and wake them after backlog settlement. Retain accepted work and the
  existing retry schedule. All recovery Git I/O, including a resync's fetch, respects that schedule.
  Preserve ordinary healthy-path refresh and pre-resync fetch costs.

### Step 5c: project publication and intake state

`feat(status)`. One immutable worker observation supplies every target's status and held save.

- With only a retryable publication problem, report `Ready=False`, `Reconciling=True`, and
  `Stalled=False`. Use the existing `Progressing` reason initially; say whether intake is paused,
  what failed, and that retained work remains scheduled. Preserve more specific parent status and
  independent terminal validation/refusal conditions. Intentional producer pause is not a generic
  terminal `WatchError`.
- Keep the first-failure time stable. Update the diagnostic only when the error or operational
  state changes. Expose next retry time in diagnostics/metrics without a status write every tick.
  A sibling target's success cannot clear the shared publication failure, and publication success
  cannot clear an unrelated scope refusal or unfinished watch replay.
- A held save remains `WaitingForPush` with the same cause. An unaccepted save stays under the
  controller's existing safety bound and can fail only after withdrawal proves it is not held.
- Count materialization failures and report retained bytes/count, admission pause, oldest pending
  work, and next retry. Check existing telemetry first. Update
  [`interpreting-metrics.md`](../interpreting-metrics.md) and `UPGRADING.md` when implemented.

### Step 6: deadlines on Git network calls (built)

`fix(git)`. Measured first, against go-git v6.0.0-alpha.5 with a 300 ms context and servers that
accept a connection and then stall at one protocol phase:

| Phase that stalls | HTTP, go-git's context API | HTTP, our code before | SSH, any API |
|---|---|---|---|
| Advertisement (`info/refs`, ls-refs) | Returns at the deadline | Hung: `Remote.List` takes no context | Hung |
| Fetch transfer (`upload-pack`) | Returns at the deadline | Hung: `Repository.Fetch` takes no context | Hung |
| Push (`receive-pack`) | Returns at the deadline | Returned, but the worker's context never expires | Hung |

SSH uses the context only to dial: the SSH handshake (`gossh.NewClientConn`) and every read of the
git protocol ignore it, so a server that never sends its banner, and one that completes the
handshake and then says nothing, both hung past an expired context. That also meant worker shutdown
waited on a stalled SSH call. The measurement and its explanation live with the code, in
[`network_bound.go`](../../internal/git/network_bound.go).

What changed:

- **Context-taking API everywhere.** `listRemoteRefs`, the fetch inside `SmartFetchFrom`,
  `CheckRepo`, and `advertiseRemoteBranch` use `ListContext`/`FetchContext` and take a context; the
  advertisement's three callers (refresh, the parent probe, the push-failure probe) pass theirs.
- **A connection-level bound for SSH.** Each call adds a dialer whose connection closes when the
  call's context ends (`context.AfterFunc`). Closing the socket fails the blocked read on the calling
  goroutine; nothing runs the operation elsewhere, so nothing outlives it with the checkout. go-git
  cancels the context it hands the dialer once the dial returns, so the dialer captures the call's
  own context instead.
- **Two budgets.** `gitCallTimeout` (2 minutes) bounds each advertisement, fetch, or push session,
  inside the library functions, so the `GitProvider` controller's connectivity check is bounded
  too. `gitPublishTimeout` (5 minutes) is one deadline over the whole push cycle, contention retries
  and their replays included. Both are package variables, not flags; a flag waits for a measured
  need.
- **A lost push reply is settled by evidence.** A push that fails without a typed rejection already
  kept its local commits and probed the remote. When the probe finds the branch at our local head,
  the push landed and only its answer was lost: the cycle settles as published instead of replaying,
  which would have planned the writes onto a tree that holds them and landed a save's empty commit
  twice. Only the exact head counts. A remote that moved on past our commits replays as any other
  contention does, because proving ancestry needs history a depth-1 fetch does not carry.

Pinned by [`network_bound_test.go`](../../internal/git/network_bound_test.go): every call above
returns at its bound over HTTP and over both SSH stalls, an SSH stall ends on cancellation, one
deadline covers the whole push cycle, and a push whose reply is swallowed after the server applied
it lands exactly once (reproduced red without the evidence check: it pushed again and the remote
ended at a replayed commit).

Remaining limits:

- Exactly-once is not promised. A lost reply followed by another writer's push replays our writes,
  and an empty save then lands twice.
- The bounds are fixed. A depth-1 fetch that needs more than two minutes fails every attempt.
- Local work inside a cycle (planning, replay, Kubernetes reads for prune policy) counts against
  the cycle's budget but has no bound of its own.

### Step 7: documentation

Update the event model's "Next implementation" and "Problems" sections, the remaining findings in the
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
| Parent recovery's `scopes`, `awaitingPush`, `noteResyncApplied`, `recoveryTargets`, the snapshot-request sequence, the controller's snapshot-request tracker, and the per-scope drop paths | step 3c |

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

1. **Resyncs stay in the log**, as described in [Resyncs in the log](#resyncs-in-the-log). Capacity
   closes admission; it never replaces accepted entries with an owed snapshot.
2. **A partly failed write is cleaned locally** (step 1b).
3. **Follow repository validation and push rules.** This changes the Git write and watch paths,
   so the high-risk exception in `AGENTS.md` requires a local e2e pass before an implementation
   push. Documentation-only changes use `task lint-docs`.

Updated 2026-10-03: pause/resume is per worker, accepted work stays in memory, and watch catch-up
has a weaker guarantee than replaying accepted decisions. Redis remains deferred.

## Acceptance scenarios for the remaining steps

| Scenario | Required observation |
|---|---|
| UPDATE refused, then replayed from the unchanged cursor | Built (5a): it is enqueued; dedup cannot skip it or advance the cursor past it |
| Push outage under continuous events and empty saves | Byte/count budgets stop intake; accepted entries and saves remain intact |
| Saturation across two targets on one branch and a second branch | Shared targets show the pause; the second branch continues |
| Paused producers, with no new Kubernetes edits | One due retry recovers; producers wake without Pod restart |
| Resync arrives during backoff | No early Git fetch; caller receives the failure and accepted intent remains |
| Read access returns but pushes are still rejected | Admission stays paused and backoff continues |
| Cursor expires during pause | Fresh scoped snapshot follows accepted work; no missing-history claim |
| DELETE history expires under each prune mode | Only permitted deletions occur; retained stale objects remain observable |
| One replay entry is refused | Only that entry settles; later work can publish |
| Capacity stays full during withdrawal and shutdown | Controls progress in order; held saves never falsely time out |
| A snapshot exceeds the entire payload budget | Explicit capacity state, no retry storm or partial sweep |
| Remote stalls, or accepts a push and loses its reply | Built (6): deadline returns control; publication evidence governs save outcomes |

## Prompt for the next implementation

Use only after the step 5a2 decisions are approved; record any change to them in the step first.

```text
Continue the branch-worker log plan at step 5a2: the unchanged filter belongs to its stream.
Read the step's decisions as approved, then target_watch.go (checkLiveContent,
acceptLiveContent, routeLiveTargetWatchEvent, runTargetWatch, markReplayed), manager.go,
live_content_dedup_test.go, and refused_admission_test.go.

Write the red test first: a replay inside one stream followed by a live UPDATE back to the
pre-gap content must route. Then move the filter's memory onto the stream, record after
acceptance with a plain store, reset it when the stream's replay is accepted, and delete the
shared map and the compare-and-swap. Apply the approved name everywhere, including
definitions.md. Keep the unchanged metric outcome and the 5a producer tests.

Mark only 5a2 built. Run the AGENTS.md gates; report what changed, validation, and limits.
Step 5b follows; its scope is unchanged.
```

## Out of scope

Persisting the log or the deadlines, HA, an asynchronous executor, the success push cooldown, and
recording the other FIFO inputs (attach, withdraw, refresh) as transitions.
