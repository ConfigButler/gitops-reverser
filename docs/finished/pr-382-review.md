# PR #382 review: API-first publication and timing

Review date: 2026-09-21. Reviewed head: `74558c4229a094e07da6eb6141a3a388302b5030`.
Base: `28b4ab5acaa7398e69248070caaaff8dbee76db4`. The comparison covers 33 changed files,
including the worker, Git transport and reset helpers, request attachment, metrics, tests, and
documentation. This is a review snapshot; the
[API-first publication guide](../api-first-publication.md) explains the structures and stories.

The architecture matches the stated API-first intent. Healthy publication uses a known local
base and validates it during the push. A foreign push triggers fetch and replay of unpublished
captured state. Keeping the fetch on snapshot resync and delaying no-diff request completion
until remote confirmation are necessary parts of that design.

The recommendation is to address finding 1 before relying on the recovery guarantee. The review
also identifies a fetch-metric classification error, an existing retry scheduling gap, and
inaccurate cost predictions. Production code was not changed during this review.

## Findings

### 1. P1: mark retained writes unsafe before a reset can partially succeed

In [branch_worker.go](../../internal/git/branch_worker.go), both
`refreshRemoteAndRebuildPendingWrites` and `runPushCycle` set `replayRequired` only after
`syncToRemoteFn` returns successfully. At the reviewed revision, the relevant calls are at lines
1835 and 1694, with the flag set at lines 1844 and 1703.

A reset can update the local branch reference and then fail while changing the worktree or
index. The caller returns with `baseTrusted=false`, but both `worktreeDirty` and `replayRequired`
can remain false. `recoverRetainedWrites` checks those latter two flags, so it lets the next push
proceed. Local HEAD now equals the remote tip: `PushAtomic` returns success without transferring
the retained write. `pushPending` clears the write and can report its request as committed.

This was reproduced against the real HTTP Git fixture without replacing the reset function:

1. Publish a baseline object and finalize another live window into a retained local commit.
2. Push a competing change to the remote.
3. Make the local manifest directory read-only, leaving `.git` writable.
4. Request refresh and replay. Git moves HEAD and fails while removing a file from the worktree.
5. Restore directory permissions and run the next push.

The observed result was:

```text
reset failed: removeat team-a/default/configmaps/must-survive.yaml: permission denied
worktreeDirty=false
replayRequired=false
next push: remote already up2date
retained writes after push: 0
remote tree: must-survive.yaml absent
```

The permission change is fault injection for a filesystem error during reset. The failure does
not require a Git protocol race. Errors after HEAD moves must be treated as potentially
destructive regardless of their cause.

The destructive-reset error path was already unsafe in the baseline. The new replay flag repairs
failures after a successful reset, but leaves this part of the recovery contract incomplete.
This is a remaining correctness defect, not a claim that the PR introduced every part of it.

Suggested correction: set the replay-required state before calling a potentially destructive
sync/reset while writes are retained, and clear it only after a complete rebuild. A more precise
reset API could report whether mutation began, but the conservative guard is sufficient.

Add regression coverage for both reset callers. After an injected reset failure, verify that no
write is cleared, no request reports a nonexistent remote commit, and recovery publishes every
retained write once the filesystem is usable again.

### 2. P2: a resync fetch is classified as publication when no writes are retained

`prepareBaseForResync` in [resync_flush.go](../../internal/git/resync_flush.go), line 102,
passes `forced_recheck` to `invalidateAndRefresh`. With no retained writes, that helper only
invalidates the base and returns. The later `ensureBaseForCycle` performs the fetch and records
`publication` at worker lines 1532–1536, losing the reason for the invalidation.

The result depends on whether the branch happened to hold unpublished writes. The existing
resync metric test covers only the retained-writes case. A separate overlay test primed a healthy
worker, applied a snapshot with `RefreshRemote=false` and no retained writes, and observed:

```text
publication delta=1
forced_recheck delta=0
```

This weakens the PR's central production diagnostic: a growing publication-fetch counter can mean
normal snapshot reconciliation rather than a regression to fetching on every live publication.
The current metrics guide classifies snapshot fetches under `forced_recheck`.

Suggested correction: preserve the fetch reason until the deferred preparation executes, or
perform the resync's preparation through an entry point that records its own reason. Cover both
retained and empty pending-write cases, including a no-op snapshot, without adding a second fetch.

### 3. P2: the recovery path has no scheduled retry on a quiet branch

`pushPending` stops the push timer after either recovery failure or push failure, at lines 1343
and 1353 of the reviewed worker. It keeps pending writes, but schedules no replacement timer.
No commit timer remains after the last window closes, and a finalized `CommitRequest` is excluded
from its attach timer. Repeated controller attaches recognize that request but do not retry its
publication.

A brief outage can therefore leave an otherwise quiet branch with retained work indefinitely.
The request controller eventually reports its `420s` safety timeout. Under sustained arrivals,
the opposite problem is possible: `lastPushAt` advances only on success, so an expired cooldown
does not space failed attempts.

The push-failure scheduling gap predates the PR. The added recovery-failure return follows the
same pattern. It matters to this review because the design describes retained work as retried
and an increasing recovery counter as evidence of a stall. Neither happens automatically once
the branch becomes quiet.

Suggested follow-up: introduce a bounded retry timer for outstanding writes, separate from the
successful-push cooldown. It should stop when no work is retained. Test it through the running
event loop: fail one attempt, restore the dependency, deliver no new events, and require eventual
publication. Also test attempt spacing during an extended failure with continuing arrivals.

This does not require periodic fetches on a healthy idle branch. It is scheduling recovery for
known outstanding work.

### 4. P2: the proposed notification savings use inconsistent request costs

The table in section 4 of the
[inbound notification design](../design/inbound-push-notification.md#4-what-it-costs) predicts
three HTTP requests for one contended publication with notification, and four for two moves.
Those predictions do not follow from the measured primitive costs on the same page.

With the current implementation, a fetch that transfers the foreign commit costs three HTTP
requests. A subsequent successful push costs two. An early notification can therefore turn the
measured six-request single-rejection case into an estimated five-request case. It avoids the
failed advertisement; it does not remove the object fetch or successful push exchange.

For multiple moves, the result depends on when notifications arrive and how they are coalesced.
There is no justified fixed four-request prediction yet. Revise the estimates or specify and
measure an additional transport optimization that achieves them. The new publication guide uses
the five-request estimate and labels it as unmeasured.

## Test coverage observations

The strongest addition is the smart-HTTP request ledger. It exercises canonical Git's backend,
keeps remote setup outside the measured interval, and pins zero idle traffic independently of
golden regeneration. The typed advertisement error avoids a redundant lookup while preserving
the fallback for failures that did not return an advertised branch hash.

The added tests also cover deleted branches, dirty checkout cleanup, deferred no-diff request
results, repeated request attachment during cooldown, and failed replay before publication.
Those are appropriate boundaries for removing the unconditional fetch.

One named scenario is not exercised as described.
`TestReplayFailure_PartwayThroughTheBatchIsAlsoHeld` in
[replay_failure_test.go](../../internal/git/replay_failure_test.go), lines 192–195, fails all
`GitTarget` reads. That aborts `tightenPendingPruneModes` before the first write executes. It does
not create the mixed batch of fresh and stale commit SHAs described by its comment. Inject the
failure after the first write has been rebuilt, then check both retained writes and their
request outcomes.

The replay tests also call `loop.pushPending()` directly to demonstrate recovery. They prove the
behavior of an attempted retry, but do not prove that a retry is scheduled. Finding 3 needs an
event-loop test for that reason.

The request ledger does not establish wall-clock speed, SSH connection counts, or full-manager
idle behavior through several watch refreshes. A deployed Prometheus assertion for steady-state
publication fetches would complement the unit ledger. The design already identifies that work
as outstanding.

The two rows called “worker start” need a scope correction too. They call
`EnsurePathBootstrapped` directly, but that method has no production caller in this revision.
`BranchWorker.Start` starts the event loop without fetching; the first write or resync prepares
the repository. Consequently, the `bootstrap` fetch series is exercised by the tests but has no
production call path. Rename those rows as bootstrap-helper measurements or drive the real
startup sequence, and document where production initialization is counted.

## Timing assessment

The two main timers serve the API-first design: commit silence keeps history readable, and a
separate cooldown amortizes network publication across local commits. They overlap. There is no
fixed ten-second wait and no initial cooldown before the first push.

The operational qualifications to keep visible are:

- The commit window is rolling, with no maximum age independent of other closing triggers.
- The five-second cooldown limits successful publication cadence, not all Git requests or failed
  attempts. Contention performs up to three immediate push attempts within a cycle.
- Optional attribution can wait up to three seconds per event before it enters the branch queue.
  Serial processing can accumulate delay across missing facts on one watch stream.
- A two-second `CommitRequest` collection delay can expire before an attributed event arrives.
  It is a collection opportunity, not a guarantee about cross-stream delivery.
- The thirty-second watch refresh updates Kubernetes discovery and watch plans. It is not a
  thirty-second Git poll. A refused Git path can nevertheless cause repeated forced fetches on
  the target's ten-second recheck cadence.
- A byte threshold forces finalization but cannot release retained work during a remote outage.
  It is not a hard memory limit; the ingress queue has a separate drop boundary.
- Git operations run synchronously on the branch worker. Timer deadlines do not preempt them.
  Watch-owner and resync-observer timeouts are not a bound on Git publication time.

The [timing guide](../api-first-publication.md#the-other-clocks-around-publication) ties each
clock to its owner, default, and purpose. No new polling interval is needed to preserve the
normal path's correctness.

## Documentation consistency

The PR description still says the trust flag is disabled, healthy publication is unchanged, and
the no-diff request work is unimplemented. Those statements describe an earlier branch state.
Update the description around the final implementation and validation before merge.

The design page also mixes historical and current statements. Its opening says the work is
unbuilt while section 11 says the publication changes are implemented. Earlier sections describe
premature request completion as current behavior even though this PR fixes it. Label historical
reasoning explicitly and separate the implemented worker changes from the proposed receiver.

Use “last known usable base” for the meaning of trust. A remote writer can move the branch at any
time; the trust flag is not a freshness certificate. Also qualify statements that local replay
costs microseconds: the ledger measures HTTP requests and supplies no replay-time benchmark.

The inbound receiver is independent of safely removing the healthy pre-publication fetch. Its
additional value is idle-target freshness and avoiding some contention traffic. Reusing the
manual reconcile annotation would also apply a cluster snapshot, which can undo a foreign Git
edit before its Git-to-cluster reconciler applies it. The design correctly calls for a separate
refresh operation.

## Validation performed

- `go test ./internal/git` passed, including the request ledger and added recovery tests.
- `task test` passed on rerun with total coverage `78.3%`, matching the committed baseline. The
  first run failed an unchanged controller test at `watchrule_controller_test.go:297`: a status
  write lost an optimistic-lock race and the assertion read `Unknown` instead of `False`.
  The clean rerun supports an intermittent failure; it does not establish its root cause.
- Additional Go overlay tests reproduced findings 1 and 2 against the real HTTP fixture. Their
  expected assertions failed: the retained file never reached the remote, and the resync fetch
  incremented the wrong reason. The overlay lived outside the repository and did not alter the
  production or committed test sources.

- `task lint-docs` passed. Direct Vale checks passed on the changed documents. The review record
  also passed explicit markdown and prose checks outside the usual `finished/` exclusions.
- GitHub CI at the reviewed head reported successful lint, unit tests, and five end-to-end jobs:
  `full-core`, `bi-directional`, `source-cluster`, `quickstart-install`, and `image-refresh`.
  `full-manager` was still running at the final observation.

The local end-to-end suite was not run for these documentation-only edits. The remaining CI job
and the reproduced defects prevent treating this review as a clean end-to-end approval.
