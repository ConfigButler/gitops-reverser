# GitHub E2E failure investigation: CommitRequest after canonical stream removal

> Status: investigation note, updated 2026-06-11.
> Context: GitHub Actions run
> [27371711951](https://github.com/ConfigButler/gitops-reverser/actions/runs/27371711951),
> attempt 1, commit `0c0a526b6a7368c88bf4a58c20bb4e8b0cd9cb9b`
> (`chore: finish deletion of the single big audit queue`).
> Related decision:
> [commitrequest-barrier-timeout-decision.md](../../finished/commitrequest-barrier-timeout-decision.md).
> Scope: facts, findings, and advised fixes for the full E2E failure seen after
> the old canonical audit queue/consumer path was removed.

## 1. Executive summary

The referenced GitHub run has two visible attempts:

- **attempt 1 failed** in `E2E (full)`;
- **attempt 2 was cancelled** after a newer run superseded it.

Attempt 1 does **not** show three failed specs. It shows one failed spec:

```text
Commit Request
[It] finalizes the open commit window on demand and reports the resulting SHA

Expected
    <string>: NoOpenWindow
to equal
    <string>: Committed
```

The important correction to the earlier theory is this: the Deployment edit was
not simply lost by the per-type tail. The run logs show the edit reached the
branch worker and was locally committed before the CommitRequest finalized:

```text
19:33:53 git commit created
message="[CREATE] apps/v1/deployments/commit-request-deploy-1781206173"

19:33:55 CommitRequest finalized
phase="NoOpenWindow" barrierReached=true
```

So the sharper failure is:

> A normal event opened a window, but that window was closed before the
> CommitRequest finalize signal arrived, even though the test configured
> `commitWindow=300s`.

The barrier did exactly what Option A says it should do on the happy path:
`barrierReached=true`. Extending the 15 s barrier timeout would not fix this
failure. This is a worker/window-lifetime problem after a successful barrier, not
a barrier-timeout problem.

## 2. Verified GitHub run facts

- Run: `27371711951`
- Attempt with failure: **attempt 1**
- Branch: `poc/redis-copy`
- Commit: `0c0a526b6a7368c88bf4a58c20bb4e8b0cd9cb9b`
- Commit title: `chore: finish deletion of the single big audit queue`
- Previous green full E2E run: `27368636247`
- Previous green commit: `cc426d8f74c7ec28895f2d73612ed10659dfe5a8`
- Full E2E mode: `--procs=4`
- Attempt 1 full E2E result: `43 Passed | 1 Failed | 10 Skipped`
- Failing job: `E2E (full)`
- Passing jobs in the same attempt: lint, unit tests, build, Helm chart,
  devcontainer, quickstart E2E.

Attempt 2 for the same run was cancelled, and its partial logs should not be used
as evidence for the failure.

## 3. The failed sequence

The CommitRequest E2E intentionally uses a long commit window:

```go
const commitWindow = "300s"
```

The spec creates a Deployment, asserts for 10 s that the remote branch is still
absent, then creates a CommitRequest. The expected behavior is:

```text
Deployment event -> open worker window
CommitRequest -> barrier passes -> finalize signal closes that same window
status.phase = Committed
```

The observed behavior was:

```text
19:33:43 create Deployment
19:33:43..19:33:53 remote branch remains absent
19:33:53 worker creates local event commit for the Deployment
19:33:53 create CommitRequest
19:33:55 CommitRequest reports NoOpenWindow, barrierReached=true
```

That sequence means the worker no longer had an `openWindow` when the finalize
signal was processed.

## 4. What this rules out

### 4.1 Not a stale CRD lifecycle assertion in this GitHub attempt

The earlier note discussed a CRD lifecycle log assertion that looked for the
literal substring `git commit` while the resync path logs `git resync commit
created`. That may still be a brittle assertion worth cleaning up, but it is not
the failure in GitHub attempt 1 of run `27371711951`. The run summary has only
the CommitRequest failure.

### 4.2 Not fixed by a longer CommitRequest barrier timeout

The CommitRequest terminal log says `barrierReached=true`. Option A from the
barrier-timeout decision only applies when `DrainTailsToSnapshot` cannot reach
the snapshot in 15 s and returns `false`.

This failure is worse in a different way: the barrier reported success, so the
status carried no degrade message. The problem happened after the barrier's
freshness condition was satisfied.

### 4.3 Not simply "the Deployment audit event was never consumed"

The worker logged a local event commit for the exact Deployment. That proves the
event entered the GitTarget worker path. A pure skip-before-registration loss
would usually leave no such local event commit.

## 5. Most likely product bug

There are two credible mechanisms that can close the window before the
CommitRequest finalize signal.

### 5.1 A background worker item closes the window

`handleResyncRequest` starts with:

```go
l.finalizeOpenWindow()
```

That means a per-type resync or sweep queued after the Deployment event but before
the CommitRequest finalize will close the live event window using the generated
group message. If the resync itself is a no-op, the function does not call
`maybeSchedulePush()` for the just-finalized live window. The local event commit
can therefore exist while the remote branch is still absent, and a later
CommitRequest sees `NoOpenWindow`.

This matches the visible shape especially well:

- the local event commit exists;
- there is no corresponding CommitRequest `Committed` status;
- the branch had remained absent during the test's 10 s check;
- `NoOpenWindow` follows shortly after.

The current logs do not name the reason `finalizeOpenWindow()` ran, so this is a
ranked hypothesis rather than a fully proven cause.

### 5.2 The worker is not honoring the configured `300s` commit window

The local commit appears almost exactly 10 s after the Deployment was submitted,
while the GitProvider was created with `commitWindow=300s`. If no background
resync was involved, then the worker may have started with an unexpected
commit-window value or a stale provider view.

The code currently does not log the parsed commit window when a branch worker
starts, so the GitHub log cannot distinguish this from the resync hypothesis.

## 6. Secondary tail/barrier risk still worth fixing

The earlier global-cursor finding is still valid as a design risk, but it is not
the best explanation for this exact GitHub failure.

The tail stores one cursor per GVR:

```go
m.auditTailCursors[gvr] = auditTailAnchor(checkpointRV)
```

Delivery, however, is per GitTarget:

```go
if m.EventRouter.GetGitTargetEventStream(table.GitDest) == nil {
    continue
}
```

After `apply(ctx, log, gvr, changes)` returns, the global cursor advances even if
a particular GitTarget was skipped. That makes `auditTailCursors[gvr]` a
type-global read/apply marker, not a target-local applied marker. A future
CommitRequest can therefore get `barrierReached=true` even though the target did
not receive all entries the barrier is supposed to protect.

The recent Option-A timeout decision makes this distinction more important:

- timeout (`barrierReached=false`) is visible and bounded;
- false-positive success (`barrierReached=true` for a target that missed delivery)
  is invisible.

So the barrier should eventually read a target-local applied watermark, or the
tail must not advance a value used as "applied" when any target delivery is
skipped.

## 7. Advised fixes

### 7.1 Add reasoned worker diagnostics first

Add low-noise logs around the exact state transitions that matter:

- branch worker start: parsed `commitWindow`, provider name/namespace/branch;
- every `finalizeOpenWindow()` call: reason (`timer`, `finalize-signal`,
  `resync-before-apply`, `atomic-before-apply`, `author-or-target-change`,
  `buffer-limit`, `shutdown`), window author, GitTarget, event count;
- every CommitRequest finalize signal: signal author/target and whether the
  worker had an open window or only pending writes;
- every `handleResyncRequest`: whether it closed a live window and whether the
  resync committed anything.

Without this, CI can prove symptoms but not the closer that stole the window.

### 7.2 Fix `handleResyncRequest` so it cannot strand a just-closed window

At minimum, if `handleResyncRequest` closes an open window, it must schedule or
perform the normal push path even when the resync itself is a no-op:

```go
closedWindow := l.finalizeOpenWindow()
...
if committed {
    ...
    l.maybeSchedulePush()
    return
}
if closedWindow {
    l.maybeSchedulePush()
}
```

That would not make the CommitRequest status become `Committed`, but it prevents
the worst version of the bug: a local user-authored event commit that neither
gets pushed nor remains open for the CommitRequest.

### 7.3 Decide the semantic fix for "save" racing a background close

The stronger fix is to preserve the CommitRequest promise:

> edits made before the CommitRequest should be finalized by that CommitRequest,
> not preempted by a background resync or surprise timer.

Viable directions:

- make background resync avoid closing an open live window unless it can prove
  the close is required;
- add a worker-level "finalize matching pending live commit" path, so a
  CommitRequest can claim a window that was closed into pending writes but not
  pushed yet;
- serialize per-target background resync and CommitRequest finalize with an
  explicit priority/fence so a finalize requested by the user is not overtaken
  by maintenance work.

Do not solve this by treating `NoOpenWindow` as success in the controller. That
would hide cases where nothing was committed at all, and it would lose the
explicit CommitRequest message/SHA contract.

### 7.4 Keep Option A as-is

Do not lengthen `FinalizeBarrierTimeout` for this failure. The observed failure
had `barrierReached=true`, so timeout policy was not the limiting factor.

The right use of the Option-A decision here is diagnostic: when the barrier
times out, status must say so; when the barrier succeeds, we need the downstream
worker invariant to be true.

### 7.5 Make the barrier target-aware

After the immediate worker/window fix, address the tail cursor mismatch:

- treat the current GVR cursor as a read cursor only;
- add per `(GitTarget, GVR)` applied watermarks, or a skipped-target catch-up
  marker;
- make `DrainTailsToSnapshot` wait on target-local applied state for the
  CommitRequest's GitTarget;
- add a regression test where the tail reads an event while a target stream is
  absent, then the target registers and a CommitRequest is created.

The invariant should be:

> an event is "applied" for a CommitRequest only after the target either received
> it on the worker FIFO or completed an authoritative later catch-up covering it.

## 8. Regression tests to add

1. **Worker unit test:** a resync request arrives while a live window is open; the
   resync is a no-op. Assert the window's pending write is pushed or at least has
   a push timer scheduled.
2. **CommitRequest integration/unit test:** a Deployment event opens a window; a
   background resync closes it before finalize. Assert the chosen semantic fix:
   either CommitRequest still reports `Committed`, or the worker prevents the
   background close from stealing the window.
3. **Commit-window config test:** worker creation logs or exposes the parsed
   `commitWindow`; an E2E/helper assertion confirms the CommitRequest suite's
   provider really uses `300s`.
4. **Target-local barrier test:** force skip-before-registration and assert
   `DrainTailsToSnapshot` cannot report success for that GitTarget until catch-up
   has covered the skipped event.

## 9. Proposed order of work

1. Add the worker diagnostics in §7.1.
2. Add the focused `handleResyncRequest` regression test and fix the stranded
   pending-write bug.
3. Decide and implement the CommitRequest semantic fix for background-close races.
4. Add the target-local barrier regression and then implement target-local applied
   watermarks/catch-up.
5. Re-run full E2E with `E2E_GINKGO_PROCS=4` and rerun the GitHub workflow.

## 10. Bottom line

The GitHub failure is not a reason to revisit Option A's 15 s timeout. It is a
reason to tighten the contract between the successful barrier and the branch
worker: once a CommitRequest's barrier passes, the worker must still have a
finalizable user window, or an explicitly handled equivalent, for the edits the
barrier protected.
