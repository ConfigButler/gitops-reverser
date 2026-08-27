# Status convergence failures on the watch-plane rework

**Root cause named; fix in progress.** Two reproducible failures on
`feat/target-watch-cell-identity` (PR #315), both in status the branch's own rework produces, plus
an inventory of what is ambient and must not be confused with them. Written so a fresh context can
continue without re-deriving anything.

**Facts are separated from hypotheses.** Nothing below is a conclusion unless it says so. Move
this page to `docs/finished/` when the fix has landed and held.

---

## 1. The short version

The data plane is not implicated in either failure. Files land in Git correctly and on time in
every case examined — what fails is the **status that describes them**.

**A and B are one defect, not two.** Both are per-cell, revision-gated roll-ups in which a scope's
result is never recorded, and in which nothing re-measures afterwards. B loses a retention count.
A loses a render-fidelity scope result — which pins the GitTarget at not-Ready, which pins **every
WatchRule pointing at it** at `Ready=False`. That is why A looked like a rule-level streams
failure for three reproductions running. It never was one.

| # | Symptom | Seen | Verdict |
| --- | --- | --- | --- |
| **A** | A `WatchRule` never reaches `Ready=True` within 90s while its streams are demonstrably running | 3× (CI twice, local once) | **Named.** Not a streams failure at all — the GitTarget's render-fidelity gate never converges. Same defect as B |
| **B** | `status.retention.retainedDocuments` stays at its pre-sweep value after `prune.mode` is widened, on a target whose files were swept | 2× (CI) | **Named.** Same defect as A, on the retention roll-up |
| C | Argo CD `selfHeal` commit count moves 3 → 5 during a `Consistently` | 1× (CI, never re-run) | Unclassified, and probably unrelated |
| D | Encryption-secret recreation spec times out | 1× (CI) | **Ambient, pre-existing** — see §5 |

A green run is not evidence against A or B. Run `33113743391` on `773f6cc0` was green on all six
e2e legs, Lint and Unit, and a full local `task test-e2e` was 80/80 on the same commit. Neither
touched the failing path. The defect is intact.

### 1.1 What settled it

Commit `742344ee` made the e2e condition waiter print the condition's `reason` and `message`
instead of only `Expected False to equal True`. The very next failing run said:

```text
watchrule "srcns-wildcard-rule" condition Ready: status="False"
  reason="Rechecking"
  message="Waiting for every render scope to report under its current revision"
```

That is not a stream reason. A stream failure reads `0/1 streams running (configmaps)`. This
string is produced by [`reduceRenderFidelity`](../../internal/git/render_fidelity_gate.go), and it
reaches the WatchRule because `Ready` gates on `GitTargetReady` as an independent prerequisite —
see [`ruleReadiness`](../../internal/controller/stream_status.go), fed from the GitTarget's own
published Ready by [`gitTargetReadyCondition`](../../internal/controller/gittarget_dependency_status.go).

**One printed reason string replaced three rounds of log archaeology.** That is the lesson of this
page, and §4 is the change that generalises it.

---

## 2. Failure A — what actually happens

### 2.1 The observation, restated correctly

`Eventually` at [`e2e_test.go:234`](../../test/e2e/e2e_test.go) times out after 90s waiting for a
WatchRule's `Ready`. Two reproductions, on `srcns-wildcard-rule` and on `signing-per-event-wr`.

The rule's own streams are fine. In run `33103011476`, for `1787855423-test-srcns-config/srcns-granted`:

| Time | Event |
| --- | --- |
| 18:34:14 | plan reconciled, `start:2` — the two `…-source` cells |
| 18:34:14 | both replays complete; both resyncs **`Handling resync request`** then **`Resync request applied`** |
| 18:34:37 | plan reconciled, `keep:2 start:2` — the two `…-wildcard` cells added |
| 18:34:37 | both wildcard replays complete; both resyncs **handled and applied** |
| 18:34:39 → 18:36:05 | `keep:4` steady. No restarts, no failures |
| 18:36:05 | spec fails: `Ready=False`, `reason="Rechecking"` |

So: four cells planned, four streams replayed, **four resyncs applied**. And the render-fidelity
gate still reported that some scope had not reported under its current revision, for 90 seconds.

### 2.2 What is excluded, by evidence rather than by argument

- **A1 and A2 are not the cause.** The stream-readiness diagnostic added in `9e4e3ce9`/`773f6cc0`
  fired **zero times** for `srcns-wildcard-rule` across its 138 mentions in the log. It fired only
  for a *different* rule during ordinary bootstrap, with `plannedCells:[]` — the `plan not
  resident` case it was extended to classify. The rule's expected cells were never in
  disagreement with the plan's.
- **The narrowing reverted in the old §2.6 was correctly reverted**, and would not have helped:
  the cells were never the problem.
- **Not a dropped enqueue.** Zero `Event queue full` lines in the whole run.
- **Not a supersession.** Zero `superseded by a newer resync` lines.
- **Not the retention revision gate.** Zero `retention report dropped` lines.
- **Not a timeout.** Zero. `resyncSignalTimeout` is 5 minutes, so a stuck drain could not have
  logged inside the 90s window either way — but nothing was stuck: every resync applied.

Both diagnostics added in `c24844a1` were live and **neither fired**. By the old §3.4 table that
retires B1 and B2 as mechanisms. The report is not being dropped in transit. It is not being made,
or it is not being counted.

### 2.3 Why nobody could see which scope

[`reduceRenderFidelity`](../../internal/git/render_fidelity_gate.go) computes exactly the fact
needed to diagnose this, and then throws it away:

```go
if clean != len(state.scopes) {
    return RenderFidelityStatus{
        Revision: state.revision, State: RenderFidelityUnknown, Reason: "Rechecking",
        Message:    "Waiting for every render scope to report under its current revision",
        ScopeCount: len(state.scopes), CleanScopes: clean,
    }
}
```

`ScopeCount` and `CleanScopes` are carried on the struct and never rendered.
[`renderAxis`](../../internal/controller/gittarget_controller.go) publishes `renderFidelity.Message`
verbatim, so the GitTarget condition — the one surface an operator or a test can actually read —
says only that *something* is pending, never *what*, never *how many*, and never *under which
revision*. The gate holds a per-scope table of `(revision, finished, clean)` and reports a
constant string.

**This is the defect that made a two-hour bug into a three-day one**, and it is the first thing
§4 fixes. A running system must be able to say what it is waiting for.

### 2.4 The failure class

Every scope in `state.scopes` must reach `finished && clean` for the target to be Ready. A scope
reports exactly once per revision, from
[`drainScopedResync`](../../internal/watch/event_router.go), under the revision its stream was
**started** with. Recovery from a missing report requires the cell's stream to be restarted by a
plan change, because a replay is the only thing that produces a report.

So any scope that ends up holding a revision no running stream will ever report under is stuck
**permanently**, and the target is stuck with it. Three code paths can produce that state, and all
three are silent:

1. **The drain never runs.** `enqueueReplayResync` returns early when the worker's queue was full
   — *before* starting the drain — even though `EnqueueResync` has already delivered
   `ErrFinalizeQueueFull` on the reply channel for the drain to record. The contract comment on
   [`enqueueScopedResync`](../../internal/watch/event_router.go) says the failure "is still
   delivered on resultCh for the drain to record"; the caller in
   [`target_watch.go`](../../internal/watch/target_watch.go) makes that impossible. Nothing marks
   acceptance, fidelity or retention, and nothing re-measures. Latent in the observed runs (no
   queue-full lines), but real, and it is a code *deletion* to fix.
2. **The drain runs and reports, but the gate rejects it.** `recordScope` returns `applied=false`
   when `result.revision != revision`, and the caller discards the result. A scope handed a fresh
   revision without its stream being restarted is in exactly this state for ever.
3. **Supersession skips the reports.** [`handleScopedResyncError`](../../internal/watch/event_router.go)
   returns on `ErrResyncSuperseded` without marking acceptance, **fidelity** or retention, on the
   reasoning that the replacement marks them instead. Its own comment names all three; the earlier
   drafts of this page connected it only to retention.

The common structure is what matters: **three separate mirrors of "the current cell set" must
agree** — the running streams in `targetWatchSet.streams`, the readiness surface in
`watchPlaneState.streams`, and the gate's `scopes` — and the roll-up is *accumulated* by push
rather than *derived*. Any disagreement between the mirrors is unobservable and permanent.

---

## 3. Failure B — the same defect on the retention roll-up

The evidence recorded for B is unchanged and still accurate; only its classification moves.

`Manager GitTarget prune policy` → "converges an existing orphan when `prune.mode` is widened,
without touching the WatchRule",
[`prune_mode_e2e_test.go:280`](../../test/e2e/prune_mode_e2e_test.go):

```text
Timed out after 30.000s.
a resync that retains nothing must drive the count back to zero, not leave it stale
Expected <int>: 1 to equal <int>: 0
```

The force replay fires (`restart:1`), the sweep works — both orphans leave Git, which is why the
file assertions passed — and then the plan reports only `keep`, so nothing re-measures and the
published count sits stale past any test budget. The asymmetry noted at the time (two resync
commits, one handle/apply pair, the second commit re-deleting a file the first removed) is the
same "a result was produced that no roll-up counted" shape as A.

`MarkTargetRetention` has A's problem 2 verbatim: it drops any report whose revision the plan has
moved past, or whose cell the plan no longer holds, and the published count then describes nothing
with nothing scheduled to correct it. Dropping the *stale* report is correct. Leaving the roll-up
permanently unmeasured afterwards is not.

---

## 4. The fix, and why it removes code

The instinct on a status bug is to add a reporter. This one is the opposite: the system already
computes everything needed and declines to publish it, and it maintains three mirrors of one fact.

**4.1 Make the gate say what it is waiting for.** `reduceRenderFidelity` already knows the pending
scopes by name. Render them into the message. The GitTarget condition then reads
`Waiting for 1 of 4 render scopes: secrets in ns-x (revision 3)`, which is visible to
`kubectl get gittarget`, to the WatchRule condition that inherits it, and to the e2e failure text
that already prints `message`. No new logging, no new plumbing.

**4.2 Delete the temporary diagnostics this replaces.** `explainNotRunning`, `notRunningHypothesis`
and `cellNames` in [`stream_readiness.go`](../../internal/watch/stream_readiness.go) exist only to
diagnose Failure A, and they were looking in the wrong place. The supersession log line in
`handleScopedResyncError` was raised V(1) → Info as `TEMPORARY` for the same hunt. Both go back
out once the condition carries the answer. Net change is negative.

**4.3 Always drain.** Remove the early return in `enqueueReplayResync` so the queue-full reply is
read by the drain that the contract already promises will read it.

**4.4 Record what the gate refuses.** A refused report — one carrying a revision the plan has moved
past — was discarded in silence, so a scope waiting for its first replay and a scope discarding a
steady stream of reports were indistinguishable from outside. They have opposite repairs: the first
converges by waiting, the second never does. The scope now remembers the last revision it refused
and the pending message says so. This is the asymmetry that gave B evidence and left A with none;
the retention roll-up already logged its drops.

**4.5 Do not weaken the revision gate.** A stale report from a replaced stream must still be
refused. The repair is that a scope which cannot be reported under its current revision must be
*re-measured*, not that an old measurement may stand in for a new one.

### 4.6 Order of work

1. Make the pending scopes visible in the condition, with unit coverage asserting the message
   names the scope. Land this first — it is what makes the next reproduction self-diagnosing.
2. Remove the temporary diagnostics it replaces.
3. Fix the always-drain defect.
4. Then, with the condition naming the scope, its owed revision and any revision it has refused,
   take the next reproduction and close whichever of §2.4's three paths it points at. The message
   distinguishes them without a log: a scope that owes a revision and has refused none has never
   been replayed; one that has refused a report is being fed by a stream the plan has moved past.

Do not treat a green full run as evidence at any step. Both failures have produced fully green CI
on a commit that failed locally, and vice versa.

---

## 5. Flake inventory — what is ambient, and must not be misattributed

Three separate branches have now been blamed for failures that reproduce on `main`. Check this
table before bisecting.

### 5.1 Ambient, confirmed

| Flake | Signature | Rate | Notes |
| --- | --- | --- | --- |
| **Encryption-secret recreation** | `secrets "recreated-sops-age-key" not found`, 45s, `gittarget_controller_test.go:1140` | ~1 in 11, **on branches and on main** | Unit tests. Root cause and the reason a Secret watch is forbidden are in [`TODO.md`](../TODO.md). Passed on a re-run of the identical code. |
| **Refused-GitTarget recovery** | `GitPathAccepted` projection racy both ways; next requeue up to 10 min | Reproduces locally and deterministically in the `unsupported-folder` refusal spec | Do not chase when the diff is test/docs-only. |
| **`target_watch` forbidden race** | `TestTargetWatchReplayAndStream_FallsBackWhenReplayWatchIsForbidden` | Only under `-race`; CI does not use it | Pre-existing shutdown race. |

### 5.2 Environmental, not code

| Symptom | Cause | Fix |
| --- | --- | --- |
| k3d wedged, Prometheus OOMKilled, nodes NotReady | Host OOM, or a run killed mid-flight | `task clean-cluster`. Do not debug the diff. |
| `prepare-e2e` hangs on `kubectl get ns` while docker looks healthy | k3s server shut itself down on a cloud-controller-manager RBAC race | Same. |
| e2e aborts at flux-operator install | Stale ghcr token (`DENIED` → `docker logout ghcr.io`) or a dead VS Code bridge (`exit status 255` → reload window) | Diagnose by the helper's exit code; 1 = healthy. |
| Fast `failed to get server groups` + zero ginkgo reports | apiserver-discovery flake at bring-up | `gh run rerun --failed`. Do not bisect. |

### 5.3 Traps when reading CI

- **`gh run rerun --failed` may not re-run everything you think.** On run `33082534675` attempt 2,
  only `full-manager` has a new `started_at`; every other job — bi-directional included — carries
  the attempt-1 timestamp and its result is **carried forward**. The UI shows both red. Always
  check `started_at` per attempt before concluding "it failed again".
- **`gh` refuses logs containing terminal escapes.** Use
  `gh api --allow-escape-sequences repos/:owner/:repo/actions/jobs/<id>/logs`. Job logs are not
  retrievable via `gh run view --log` until the whole run completes; the API works sooner.
- **Controller logs are captured in the e2e job log**, so a CI failure can be diagnosed without
  reproducing it. Read them before theorising.
- **`cancel-in-progress: true`** is set per PR. Any push cancels the run in flight.
- **`E2E_LABEL_FILTER` replaces the default filter** — re-AND the exclusions, or a zero-match
  filter skips both suite hooks and passes vacuously.
- **Local `task test-e2e` does not run the bi-directional corner.** It is opt-in:
  `task test-e2e-bi-directional`.

---

## 6. Run inventory

Branch `feat/target-watch-cell-identity`, 2026-08-27.

| Run | Commit | Result |
| --- | --- | --- |
| `33071092006` | `416f8596` | success |
| `33074976097` att.1 | `1eb958a0` | **full-manager: B** |
| `33074976097` att.2 | `1eb958a0` | success |
| `33079663111` | `26683846` | **Unit tests: D**; cancelled by a push |
| `33081182327` / `33082178520` | `58e19599` / `310c3b84` | cancelled by a push |
| `33082534675` att.1 | `5e054717` | **full-manager: A** (wildcard); **bi-directional: C** |
| `33082534675` att.2 | `5e054717` | **full-manager: B**; bi-directional **not re-run** |
| `33087980631` | `c24844a1` | **success — all six e2e legs, Lint, Unit tests** |
| `33100142551` | — | failure |
| `33103011476` | `9e4e3ce9` | **full-manager: A and B**; Unit tests: D. The run that named the cause — its waiter printed `reason="Rechecking"` |
| `33113743391` | `773f6cc0` | **success — all six e2e legs, Lint, Unit tests.** Proves nothing; the failing path was untouched |

Local suites, same code:

| Run | Commit | Result |
| --- | --- | --- |
| `task test-e2e` ×2 | `5e054717` and earlier tree | 80/80 pass |
| `task test-e2e-bi-directional` | `5e054717` | 3/3 pass, including the spec that failed as C |
| `task test-e2e` | `c24844a1` | **75 pass, 1 fail — A** (`signing-per-event-wr`) |
| `task test-e2e` | `773f6cc0` | 80/80 pass |

Related change that landed and is **not** a suspect for either open failure: `14eeef46` split
stream-state transitions off the acceptance channel. Sharing one 256-slot drop-on-full buffer
between rare load-bearing events (acceptance, render-fidelity, retention — up to ~5 min of stale
status when dropped) and high-volume ones (stream transitions — ~10s) is a real design error, and
the fix stands on that reasoning. It does not explain B, which predates it.
