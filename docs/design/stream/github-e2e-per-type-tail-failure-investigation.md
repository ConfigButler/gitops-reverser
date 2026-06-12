# GitHub E2E failure investigation: CommitRequest after canonical stream removal

> Status: investigation note, updated 2026-06-12.
> Context: GitHub Actions run
> [27371711951](https://github.com/ConfigButler/gitops-reverser/actions/runs/27371711951),
> attempt 1, commit `0c0a526b6a7368c88bf4a58c20bb4e8b0cd9cb9b`
> (`chore: finish deletion of the single big audit queue`).
> Related: [commitrequest-barrier-timeout-decision.md](../../finished/commitrequest-barrier-timeout-decision.md)
> (Option A), and **[commitrequest-design.md](./commitrequest-design.md)** — the
> CommitRequest design this failure motivated now lives there.
> Scope: the facts and root cause of the full E2E failure seen after the old canonical
> audit queue/consumer path was removed, plus the narrow worker fix it needs. The
> CommitRequest design (how the feature should work) is **not** in this note — it is in
> commitrequest-design.md.

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

## 6. The narrow fix this failure needs

The actionable, worker-local conclusion is simple: a background close — the
`resync-before-apply` finalize, or any other path — must not be able to **strand** a
just-closed live window. Whatever finalizes a window into a pending write must also
schedule its push (`maybeSchedulePush`), so the local event commit reaches the remote
even when the resync that closed it was a no-op. That alone prevents the worst version
of the bug: a user-authored commit that is neither pushed nor still open for the
CommitRequest.

Two diagnostics make this provable in CI rather than inferred: log the **reason** a
window is finalized (timer / finalize-signal / resync-before-apply / buffer-limit /
author-change / shutdown — most of this already exists via `windowFinalizeReason`), and
log the parsed `commitWindow` when a branch worker starts.

A regression test should pin it: a no-op resync that closes an open live window must
not strand its pending write. The fuller test plan — including the intent-durability
case where a cut-off still carries the CommitRequest's message — lives in
commitrequest-design.md §8.

## 7. The design this failure motivated (moved)

This failure raised a deeper question than "how long should the barrier wait?": **what
does a CommitRequest mean now that the single canonical stream has been replaced by one
audit stream per Kubernetes type?** Earlier revisions of this note explored answers
inline — a target-aware watermark barrier, a "tail picture" of per-type
resourceVersions, a baseline finalize algorithm, and a state model. That exploration is
superseded and now lives, reworked, in:

> **[commitrequest-design.md](./commitrequest-design.md)**

In brief, that design drops the watermark barrier entirely (it was best-effort and
could report false-positive success), attaches the CommitRequest's message to the
author's open window as early as possible so any cut-off still commits with the user's
message, resolves the request on push (so `Committed` means "on the remote"), and
renames the `NoOpenWindow` phase to `Rejected` with a structured reason. Treat that
document as the source of truth for CommitRequest behavior; this note is only the
record of the failure that prompted it.
