# GitTarget and the branch worker: state of affairs

> **Source review**, 2026-10-03 at `373bf8d7` on the step-4 branch. This page distinguishes
> implemented behavior from the remaining plan. It reports source and test coverage inspected;
> it does not claim a fresh runtime validation or current PR/CI status.

## Where the implementation stands

Steps 1 to 4 of the [branch-worker log plan](gittarget-branch-worker-log.md) are built. Keep that
foundation: decided writes take one materialization path, retry uses one schedule, and a refused
replay entry no longer blocks unrelated retained work. The remaining work is admission
correctness, bounded Git calls, and an explicit publication pause that operators can understand.

The simplest recovery stays inside the existing worker. Pause intake for the affected branch
when capacity fills, keep accepted work, retry with backoff, and resume producers after the backlog
settles. Restarting the Pod loses in-memory decisions and save receipts. Redis persistence remains
in the [future HA plan](../future/ha-gittarget-distribution-plan.md).

## What is built

| Capability | Code | Evidence inspected |
|---|---|---|
| All write kinds enter a decided log before materialization | [`branch_log.go`](../../internal/git/branch_log.go) | `decided_write_test.go`, `branch_worker_split_test.go` |
| One checkout materializer and explicit applied-prefix tracking | [`branch_worker.go`](../../internal/git/branch_worker.go) | `dirty_worktree_recovery_test.go`, `branch_worker_split_test.go` |
| One retry deadline, 10s doubling to 5m, including parent recovery | [`retry.go`](../../internal/git/retry.go), [`parent_recovery.go`](../../internal/git/parent_recovery.go) | `retry_test.go`, `parent_recovery_test.go` |
| Failed materialization retains decided writes and their saves | `branch_log.go` | `TestDecidedWrite_SurvivesAFailedRebuildAndLandsThroughTheRetry` |
| Retained-byte threshold closes enqueue admission during retry | `branch_log.go`, `branch_worker.go` | `TestDecidedWrite_AdmissionClosesAtTheBudgetDuringAnOutage` |
| Replay refusal settles only the affected entry | `branch_log.go`, `branch_worker.go` | [`replay_refusal_test.go`](../../internal/git/replay_refusal_test.go) |
| Save outcomes follow remote publication; the controller cannot time out a held save | [`commit_request_attach_loop.go`](../../internal/git/commit_request_attach_loop.go), [`commitrequest_controller.go`](../../internal/controller/commitrequest_controller.go) | Held-request retry and controller safety-bound tests |
| Parent selection and compare-and-swap remain publication guards | [`git_atomic_push.go`](../../internal/git/git_atomic_push.go) | Parent-change tests and `TestGitRoundTripLedger` |
| New watch streams start with a fresh replay; reconnects may resume a cursor | [`target_watch.go`](../../internal/watch/target_watch.go) | `runTargetWatch`, `targetWatchReplayAndStream` |

This is an in-memory execution log. It does not yet reconstruct windows, timers, or outcomes after
process loss, and `PendingWrite` contains live interfaces and process references.

## Review findings and remaining gaps

### 1. A refused UPDATE can be skipped on reconnect

`routeLiveTargetWatchEvent` calls `skipUnchangedLiveUpdate` before routing to the worker. That helper
stores the sanitized-content hash immediately. If enqueue fails, the watch keeps its cursor, but
resuming the same UPDATE sees the stored hash, treats it as unchanged, and can advance the cursor
without delivering the write. The existing worker admission test proves refusal, not redelivery.

Step 5a must commit the dedup baseline only after successful acceptance and pin the entire
producer/cursor path, including overlapping streams. Until then the claim that every refused
live event is redelivered is too strong.

### 2. The retention threshold is not a total memory bound

`syncAdmission` closes intake when a retry is pending and `pendingWritesBytes` reaches the branch
budget. That improves ordinary outage behavior, but:

- Empty request records and refusal touches have zero `ByteSize`; repeated empty saves bypass it.
- The FIFO caps item count (default 1,000), not payload bytes. A snapshot can be large.
- The open window, deferred resyncs, registered requests, and upstream snapshot collection also
  consume memory. An 8 MiB retained threshold does not cap these allocations.
- Admission can reopen when retained bytes fall below the threshold even if publication still
  fails. There is no explicit backlog-settlement latch.

Step 5b needs byte and count accounting at admission, a stated oversized-item path, and room for
ordered lifecycle work. Accepted decisions must not be evicted to make room.

### 3. Backoff does not suppress every failing Git connection

`decide` exempts resyncs from the ordinary retry hold. A resync can fetch during publication
backoff unless the missing-parent hold stops it. The producer also reconnects after a fixed
two-second delay when admission refuses it. That can repeatedly gather snapshots which the
worker cannot accept.

While intake is paused, producers should wait for branch capacity. While publication is backing
off, accepted resyncs should answer with the known failure and retain their write intent without
spending another connection. The existing single retry schedule remains the recovery driver.

### 4. Git network calls have no explicit operation budget

`listRemoteRefs` and `CheckRepo` use ref listing without the caller's context; `SmartFetchFrom`
uses fetch without it. Push receives a worker context with no operation deadline. A stalled
transport can block the synchronous loop, including its retry, withdrawal, and shutdown handling.

Step 6 must measure HTTP and SSH cancellation and bound both calls and the whole publication
attempt. A wrapper goroutine that returns while Git still owns the checkout is unsafe. Implement
this before calling the pause/resume path complete.

### 5. Publication and saturation have no coherent operator state

Parent recovery has status, and push failures have logs and a counter. General publication failure
has no shared worker observation projected to all its targets. A held save can say `WaitingForPush`
without explaining that the remote rejects pushes or intake has stopped. A pre-push rebuild failure
also misses the push-failure counter. Intentional producer pause currently looks like a watch error.

Step 5c projects the worker's failure and admission state into existing conditions, with stable
messages and diagnostics for backlog and retry. Independent target refusals and source replay stay
visible; a successful push cannot clear them.

### 6. Watch replay recovers a weaker contract than accepted-write replay

Backpressure cannot stop Kubernetes mutations or retain watch history indefinitely. A cursor that
expires forces a fresh snapshot; intermediate versions, deleted short-lived objects, and their
authors cannot be reconstructed. With `prune.mode: onEvent`, an expired DELETE can leave a Git
object retained. A complete scoped snapshot may sweep it under `always`; recovery must not silently
change that policy.

A new process already starts a fresh watch replay even with a stored cursor, so the old assertion
that every restart resumes past volatile work was inaccurate. Initial replay repairs current
content where policy permits. It does not restore accepted windows or save outcomes. A push that
lands immediately before a crash can still be followed by a duplicate empty save after restart.

## What the refactor got right

Keep the separation between deciding, materializing, and publishing. It lets a failed fetch keep a
save obligation without guessing what a later snapshot would have meant. Step 4's refusal isolation
also prevents one invalid folder from blocking another target on the same branch.

The earlier `owedSnapshot` proposal and a separate parent retry timer are unnecessary for the
chosen capacity policy. Keep accepted entries and reject new admission explicitly. The next change
should repair the acceptance boundary, not replace the log or introduce a generic workflow engine.

## Recommended order

1. **Step 5a:** fix producer deduplication after rejected admission and test cursor resume.
2. **Step 6:** bound network calls and test stalls and lost push responses.
3. **Step 5b:** complete capacity accounting and per-worker pause/resume, including producer waits.
4. **Step 5c:** expose publication failure, intake pause, and recovery through status and metrics.
5. **Step 7:** update runtime documentation after each behavior lands and validate the final branch.

The [implementation prompt](gittarget-branch-worker-log.md#prompt-for-the-next-implementation)
starts with the smallest correctness fix. The full acceptance matrix lives beside it. Use the
repository's high-risk validation rule for Git/write-stream implementation changes; this review
itself is documentation-only.

## Document ownership

| Page | Role after this review |
|---|---|
| `gittarget-branch-worker-log.md` | Current implementation steps, pause/resume contract, review findings, and next prompt |
| [`branch-worker-event-model.md`](branch-worker-event-model.md) | Transition semantics and the later boundary for reconstructing execution |
| [`push-cooldown.md`](push-cooldown.md) | Healthy publication cadence; the success cooldown remains unchanged |
| [`commitrequest-design.md`](../spec/commitrequest-design.md) | Current save contract, including retained versus unaccepted requests |
| `ha-gittarget-distribution-plan.md` | Future durable acceptance, publication evidence, storage budgets, and HA |

Parent observation, empty-repository bootstrap policy, configuration freshness, and refusal state
across restart remain separate follow-ups. Two providers naming one remote still create separate
workers; only remote compare-and-swap coordinates their writes. These do not need to be solved to
make an in-process outage explicit and recoverable.
