# TODO: follow-ups from the #404 review (commit window surface)

PR #404 (`feat/commit-window-surface`) is merged; these need a follow-up PR against main.

## Worth fixing

- [ ] **The controller's safety timeout shrank from 420s to about 124s.**
  - Where: `internal/controller/commitrequest_controller.go` `resolveTimeout()`.
  - Today it is `attachTimeout + maxDuration + 120s`, using the request's own values and counted from
    `CreationTimestamp`.
  - The design (`docs/design/commit-timing-surface.md`, "safety timeout") says it covers the *largest* `attachTimeout`
    and `maxDuration`, plus the push cooldown and retries.
  - Failure: if the remote is down for more than about 2 minutes, or the save waits in `WaitingForWorker`, the save is
    marked `FinalizeFailed` / `Stalled`. The worker keeps the attach, so the commit (or a `CommitEmpty` record) still
    lands, and status contradicts Git.
  - The same happens to a request that is older than the bound when the controller first sees it, for example after a
    restart.
  - Fix: bound it by the schema maximums (5m + 5m + allowance), or start the clock at the worker's registration and
    withdraw the attach when failing closed.
- [ ] **A suspended target makes a `CommitEmpty` save report Ready=True with no commit.**
  - Where: `commit_request_attach_loop.go` `recordCommitRequest` returns `false, nil` on `errTargetSuspended`, and
    `commit_executor.go` `recordEmpty` requires `!target.Suspend`.
  - The design table says "target is suspended → the failure". An attached window that changed nothing resolves
    `AlreadyPresent` the same way.
  - Decide: make the save fail, or change the design table.
- [ ] **A `CommitEmpty` save is recorded while the render-fidelity gate is closed.** (Found by reading the code, not
      reproduced.)
  - Where: `recordCommitRequest` never calls `normalWritesAllowed`.
  - While the gate is closed, the author's events are dropped. The save then times out and records "no writes were
    seen", which is false.
  - The attached-window path fails the request in the same situation.

## Low

- [ ] `expireWaitingCommitRequests` ranges over the `pendingCRs` map, so empty commits for saves that time out in the
      same pass land in random order. Sort them by `seq`.
- [ ] `buildRequestRecordWrite` sets `RequestAuthor: UserInfo{Username: pcr.author}`, which loses the display name and
      email that a window commit carries. Carry the full `UserInfo` on the attach.
- [ ] A record commit skips `requestTemplate`. The doc comment says it is phrased like every other commit. Fix the
      code or the comment.
- [ ] A request whose GitTarget never starts a worker fails after the timeout with the generic "did not resolve within
      the safety window". If the last phase was `WaitingForWorker`, say so in the message.
- [ ] `docs/UPGRADING.md` (the v1alpha3 window entry):
  - Step 1: GitOps-managed GitTargets need their Flux or Argo source suspended (or the field removed from it) until
    step 4. Otherwise the string `commit.window` is re-applied and breaks LIST.
  - The Was/Is table needs a row for the chart value `quickstart.gitTarget.commit.window`, which is now an object.
  - "A removed `closeDelay` is not a hazard" is too strong. It is pruned silently for programmatic clients (see
    `duration_fields_admission_test.go`).
- [ ] Descriptions that still say a save "closes the window now":
  - the CommitRequest godoc and its CRD description
  - `docs/configuration.md` lines 14, 32 and 123
  - the GitTarget `Commit` godoc, which also leaves out the 1m `maxDuration` default.
- [ ] `docs/spec/commitrequest-design.md:126` still says `NoOpenWindow` and "close deadline". It should say `NoWindow`
      and "attach deadline".
- [ ] Nits:
  - `branch_worker.go` `commitWindowFor` has a stale doc comment stacked on top of the real one.
  - The kstatus test names still say "close-delay".
