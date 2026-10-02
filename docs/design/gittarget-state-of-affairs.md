# GitTarget and the branch worker: state of affairs

> **Snapshot**, taken 2026-10-02 at `230d602a` (#411 on `main`), and updated for #412, which fixes
> gaps 1 and 2 below, closes the 0.51 docs gaps, and lands the rewritten
> [`branch-worker-event-model.md`](branch-worker-event-model.md) and HA plan. It describes
> what is built, which page owns which decision, which edge cases are known not to work, and a
> recommended order for what comes next. It decides nothing by itself; each linked page stays the
> owner of its own contract. Every claim about behavior below was checked against the code or a
> named test on that commit.

## Where the release stands

Release PR [#406](https://github.com/ConfigButler/gitops-reverser/pull/406) (`0.51.0`) is open and
carries four merged changes since `0.50.0`:

| PR | What it changed | Breaking |
|---|---|---|
| #403 | A save's delay restarts when it claims a window; #388 restored to the changelog | no |
| #404 | One commit window surface: `spec.commit.window.{idleTimeout,maxDuration}` on `GitTarget`, `spec.window` on `CommitRequest`, `closeDelay` gone | **yes** |
| #407 | `GitTarget.spec.parentBranch`; a target on standby creates its write branch from the parent's current tip, checked at publication; the hardening pass | no (additive) |
| #411 | A `CommitRequest` on a suspended target, or one whose render fidelity is not established, fails instead of reporting `Ready=True` with nothing written | no, but user-visible |

The release PR body does not list #411 yet; release-please adds it on its next run.

## What is built

| Capability | Code | Pinned by |
|---|---|---|
| One commit window per branch worker, with idle and maximum-duration timers; a save's timers replace the target's for the window it attaches to | [`open_window.go`](../../internal/git/open_window.go), [`commit_request_attach_loop.go`](../../internal/git/commit_request_attach_loop.go) | `TestCommitEmpty_*`, `TestAttach_*` |
| `whenNothingToCommit: CommitEmpty` records a save's message in an empty commit; outcomes `Committed` and `AlreadyPresent` are decided by the push | [`branch_worker.go`](../../internal/git/branch_worker.go) (`resolvePushedCommitRequests`) | `TestCommitEmpty_*` |
| The controller never fails a save the worker holds; past its safety window it only asks the worker to withdraw it | [`commitrequest_controller.go`](../../internal/controller/commitrequest_controller.go) | `TestParentRecovery_AHeldCommitRequestIsCommittedAfterALongRecovery` |
| Write gates: suspension and render fidelity are read when the commit is made; a commit made before a gate closed is still pushed | [`write_gate.go`](../../internal/git/write_gate.go) | `TestWriteGate_*` (6), `TestSuspend_*`, `test/e2e/suspend_e2e_test.go` |
| Standby: an absent write branch is created from the parent's tip as advertised on the push session; a no-op write leaves it absent | [`git_atomic_push.go`](../../internal/git/git_atomic_push.go), [`branch_worker.go`](../../internal/git/branch_worker.go) | 20 `TestBranchWorker_*Parent*` tests, `test/e2e/new_branch_parent_e2e_test.go` (3 contexts) |
| A parent change on a live worker takes effect at the push admission boundary (`{name, gen}` snapshot) | [`branch_worker.go`](../../internal/git/branch_worker.go) | `TestBranchWorker_AParentChange*` (8) |
| An empty repository that gains a ref is never orphaned; a dangling `HEAD` is `Missing` and only a repository with no refs is `Unborn` | [`git_atomic_push.go`](../../internal/git/git_atomic_push.go), [`git_smart_fetch.go`](../../internal/git/git_smart_fetch.go) | `TestBranchWorker_AnEmptyRepositoryThatGains*`, `TestBranchWorker_ADanglingRemoteHeadIsMissingNotUnborn` |
| Parent recovery is an obligation, not an observation: retained writes plus per-`(target, collection)` dropped scopes, one probe per worker (10s doubling to 5m) | [`parent_recovery.go`](../../internal/git/parent_recovery.go), [`gittarget_parent_recovery.go`](../../internal/controller/gittarget_parent_recovery.go) | `TestParentRecovery_*` (8), the missing-parent e2e context |
| Cost gate: connections per operation, including the four new standby and probe rows | [`git-roundtrip-ledger.golden`](../../internal/git/testdata/git-roundtrip-ledger.golden) rows 13 to 16 | `TestGitRoundTripLedger` |
| Idle refresh: reads one advertisement, never writes, skips a branch mid-cycle, shares the probe deadline during recovery | [`refresh.go`](../../internal/git/refresh.go) | `TestRefresh_*` (25) |
| `status.remote.parent` (`Found`, `Missing`, `Unborn`) while the write branch is absent; `ParentBranchNotFound` (stalled) and `RecoveringParentBranch` (reconciling) | [`gittarget_types.go`](../../api/v1alpha3/gittarget_types.go), [`gittarget_controller.go`](../../internal/controller/gittarget_controller.go) | `internal/controller/gittarget_parent_branch_test.go` |
| Red status, first slice: refusal messages republish when the offender changes; a scoped refusal clears only on the same scope's success; "Why a `GitTarget` is not committing" in [`configuration.md`](../configuration.md#why-a-gittarget-is-not-committing) | [`git_path_acceptance.go`](../../internal/watch/git_path_acceptance.go) | `TestGitTargetKstatusContract`, the unsupported-folder e2e |

## The documents on this topic

| Page | Status | Owns | Note from this review |
|---|---|---|---|
| [`gittarget-parent-branch.md`](gittarget-parent-branch.md) | built (#407) | The parent-branch contract as built | Accurate against the code and ledger. One open question (parent per `GitProvider.status.branches` entry). |
| [`gittarget-parent-hardening.md`](gittarget-parent-hardening.md) | done (#407) | The races, recovery and cost rules the contract depends on | Every §4 item has a named test and its ledger row. Mostly history now, including agent handoff instructions in §0; a candidate for `docs/finished/` after the release, with §7 kept as the "do not re-raise" list. |
| [`gittarget-parent-empty-repository.md`](gittarget-parent-empty-repository.md) | deferred | Which branch an empty repository's first commit may create | Blocked on go-git discarding the server's unborn `HEAD`. The upstream issue and PR are the only step that can start now, and it runs in the background. |
| [`gittarget-parent-observation.md`](gittarget-parent-observation.md) | deferred | The parent after the write branch exists; ancestry status | Informational only. Step 1 (availability) is the current backlog item in [`TODO.md`](../TODO.md). |
| [`gittarget-red-status-plan.md`](gittarget-red-status-plan.md) | partly built | Why a target is red, and what the message must say | Step 5 (the operator section) shipped and the page does not say so. The step 1 cross-package reason-agreement test was not found. Open question 2 (refusal state across a restart) is still open: `GitPathAccepted` lives in watch-manager memory. |
| [`gittarget-configuration-freshness.md`](gittarget-configuration-freshness.md) | deferred | Whether a target's running watch plan matches its configuration | Deferred with explicit pick-up triggers; neither trigger has fired. |
| [`gittarget-api-wave.md`](gittarget-api-wave.md) | effectively done | The `GitTarget` field boundaries | Every step shipped, was dropped, or was declined. A candidate for `docs/finished/`. |
| [`branch-worker-event-model.md`](branch-worker-event-model.md) | proposal; retry built in #412 | Worker execution semantics: the FIFO inputs, deadlines, saves, the transition boundary, and the operation-deadline fix still to do | Leads with the liveness fix, names both unbounded network calls, and hands the journal to the HA plan. |
| [`push-cooldown.md`](push-cooldown.md) | design | Success cooldown versus failure backoff | §7 option C (failure backoff) is the recommended fix for gap 1 below. |
| [`../spec/commitrequest-design.md`](../spec/commitrequest-design.md) | spec | The save lifecycle | Current as of #412. |
| [`../future/ha-gittarget-distribution-plan.md`](../future/ha-gittarget-distribution-plan.md) | future | The durable journal, publication recovery, outage retention, and the persistence and HA rollout | The single owner of the journal since #412. |
| [`../TODO.md`](../TODO.md) | backlog | Follow-ups | Its "Before the next release" list is closed by #412. |

## Known gaps, ranked by what a user would hit

Each was checked in the code at `230d602a`.

1. **Fixed in #412: a failed push on a quiet branch stranded the work, and any save riding it
   waited forever.**
   `pushPending` stops the push timer on failure and arms nothing; its own doc comment says so
   ([`branch_worker.go`](../../internal/git/branch_worker.go), `pushPending`). The refresher skips
   a branch with retained writes (`refresh.go`, "this branch is mid-cycle"). The controller does
   not time out a save the worker holds, and the worker ignores a withdraw for a request it holds
   (`handleWithdrawCommitRequest`). So after one transient push failure with no further edits,
   the commit stays local and the `CommitRequest` stays `WaitingForPush` until some other write
   arrives. Parent recovery is the only failure path that schedules its own retry.
2. **Fixed in #412: a down remote under steady traffic cost one failed push per commit.**
   `lastPushAt` advances
   only on success, so once the cooldown has passed every new commit attempts the push again.
3. **No Git network call has a deadline.** `remote.List` (`listRemoteRefs`, `CheckRepo`) and
   `repo.Fetch` (`SmartFetchFrom`) take no context; the push uses the worker's context, which has
   no deadline; nothing in `internal/git` sets a timeout. A stalled server can block the whole
   branch loop, and every target on that worker with it. Whether go-git's transports apply a
   default timeout was not established here.
4. **A failed rebuild drops the open window.** When retained writes need a replay and its fetch
   fails, `recoverRetainedWrites` returns an error and the finalize drops the window
   (`dropFailedWindow`). Only a missing parent records the dropped scope and asks for a snapshot,
   so under any other failure those writes wait for the next resync. While the rebuild keeps
   failing, every new window spends a fetch on it.
5. **A publication that keeps failing is not in status.** Since #412 it retries every 5m at most,
   with a save riding it held in `WaitingForPush`, but the `GitTarget` shows nothing: only the
   logs and `gitopsreverser_git_pushes_total{outcome="failed"}` do. A revoked credential or a
   branch-protection refusal therefore looks like a slow save.
6. **Accepted work is volatile.** The FIFO (1,000 items) and retained writes are process memory.
   The watch cursor can advance before a write reaches Git, so a crash in between can skip work on
   restart. The 8 MiB buffer threshold closes a window early; it does not bound retained memory.
7. **A save's identity does not survive a restart.** A push that succeeds immediately before a crash
   loses the worker's receipt, and a resent save can create a second empty commit. The outcome
   cache keeps entries 15 minutes; the admission author store keeps records one hour.
8. **Refusal state does not survive a restart.** After a manager restart a refused folder reads as
   accepted until the next write or resync refuses it again.
9. **An empty repository with a non-default write branch** gets the write branch as its first,
   parentless branch (documented as today's bootstrap contract; deferred policy).
10. **The parent disappears from status once the write branch exists**, so an operator cannot see
   that the parent has moved ahead of an open review branch.
11. **Two `GitProvider`s naming one repository** are two workers and two checkouts on the same
   remote; nothing coordinates them beyond the server's compare-and-swap. The advice remains one
   `GitProvider` per repository.
12. **With `parentBranch` omitted, a push cannot see a switch of the remote's `HEAD`**; the next
    refresh follows it with one fetch. This is a documented choice, not a defect.

Gaps 1 to 5 are liveness and visibility defects in the current worker and need no persistence;
1 and 2 are fixed. Gaps 6 and 7 are the durable-execution problem. Gaps 8 to 12 are separate,
smaller decisions.

## Review of `branch-worker-event-model.md`

An earlier version of this page reviewed the uncommitted rewrite and found six issues: the missing
user-visible `WaitingForPush` consequence, the understated timeout gap, the buried near-term fix,
a dated status line, the dropped input inventory, and no INDEX entry. The version in #412 addresses
all six, and moves the journal, publication recovery and outage retention into the HA plan, so
each topic now has one owner. Its factual claims were rechecked against the code.

## Recommended order

### Before cutting 0.51.0

Both steps are in #412: the docs gaps in `UPGRADING.md`, `configuration.md`, the CRD descriptions
and the CommitRequest spec, and the publication retry with its tests. What is left is merging it
once its e2e legs are green.

### Next, after the release

1. **Operation deadlines (gap 3).** Pass the worker context to `List` and `Fetch` and give each
   remote operation a bounded deadline. Measure first how go-git's HTTP and SSH transports behave
   today, against a server that stalls.
2. **The transition boundary, not the journal**: extract the event model's explicit transitions
   and test them from recorded state, and agree the save contract the HA plan asks for (final-state
   convergence, or also preserved save outcomes). Start the journal only after both.
3. **Gaps 4 and 5**: decide what a failed rebuild owes a dropped window, and whether a publication
   that keeps failing belongs in `GitTarget` status.
4. **Cheap, independent items**: open the go-git unborn-`HEAD` issue and PR; parent observation
   step 1 (availability after the write branch exists, no extra connections); the red-status
   reason-agreement test.
5. **Housekeeping**: move the hardening plan and the API-wave page to `docs/finished/`, and mark
   red-status step 5 as shipped.

### Leave deferred

Configuration freshness, ancestry classification, the empty-repository bootstrap policy, and the
durable journal (the HA plan's phases) all wait on a decision or on evidence that does not exist yet. None
of them is needed to make 0.51.0 better than 0.50.0.
