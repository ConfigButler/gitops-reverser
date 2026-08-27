# Status convergence failures on the watch-plane rework

**Failure A solved; Failure B open.** Two reproducible failures on
`feat/target-watch-cell-identity` (PR #315), both in status the branch's own rework produces, plus
an inventory of what is ambient and must not be confused with them. Written so a fresh context can
continue without re-deriving anything.

**Facts are separated from hypotheses.** Nothing below is a conclusion unless it says so. Move
this page to `docs/finished/` when the fix has landed and held.

---

## 1. The short version

The data plane is not implicated in either failure. Files land in Git correctly and on time in
every case examined — what fails is the **status that describes them**.

**A and B share a shape, and probably a defect.** Both are per-cell, revision-gated roll-ups in
which a scope's result is produced but never lands, and in which nothing re-measures afterwards. B
loses a retention count. A loses a render-fidelity scope result — which pins the GitTarget at
not-Ready, which pins **every WatchRule pointing at it** at `Ready=False`. That is why A looked
like a rule-level streams failure for three reproductions running. It never was one.

Neither root cause is closed. What IS closed is where to look, and both roll-ups now say far more
about themselves than they did.

| # | Symptom | Seen | Verdict |
| --- | --- | --- | --- |
| **A** | A `WatchRule` never reaches `Ready=True` within 90s while its streams are demonstrably running | 6× (CI 4×, local 2×) | **SOLVED (§2.12).** A status write that loses an optimistic-lock race is dropped, the reconcile reports success, and nothing re-enqueues it — the winning write is status-only and every `For()` filters those |
| **B** | `status.retention.retainedDocuments` stays at its pre-sweep value after `prune.mode` is widened, on a target whose files were swept | 3× (CI twice, local once) | **Open**, and narrowed to the roll-up's accept path. One hypothesis tried and withdrawn — see §3.3 |
| C | Argo CD `selfHeal` commit count moves 3 → 5 during a `Consistently` | 1× (CI, never re-run) | Unclassified, and probably unrelated |
| D | Encryption-secret recreation spec times out | 1× (CI) | **Ambient, pre-existing** — see §5 |

**A green run is not evidence against A or B, and this page has now been wrong once for forgetting
that.** Run `33113743391` was green on all six e2e legs, Lint and Unit, with a matching 80/80 local
suite, on a commit where both defects were fully intact. A withdrawn fix (§3.3) also passed the
spec it was meant to fix, once, while contradicting the logs. Require a mechanism, not a colour.

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

### 2.4 The diagnostic working, first time out

Run `33116777679`, `E2E (quickstart-install)`, on the commit that added §4.1:

```text
watchrule "quickstart-watchrule-…" condition Ready: status="False" reason="Rechecking"
  message="Waiting for 1 of 5 render scopes to report under their current revision:
           ingresses.networking.k8s.io in …-test-quickstart-framework (revision 5)"
```

**One** scope of five, named, with the revision it owes. Every previous reproduction of A produced
`Expected <string>: False to equal <string>: True` and a round of log archaeology; this one states
the answer in the failure text.

Mining that job log against the named scope narrows A sharply:

- the target planned **all five cells in ONE pass** (`keep=0 start=5`), so the five revisions
  1–5 were issued by a single `Reconcile` and every stream was started with the revision that
  `Reconcile` returned for its cell — there is no earlier incarnation for a tail to come from;
- **every one of the five replayed**, ingresses included (`count:0`);
- **every one of the five resyncs applied**, ingresses at 21:20:12, `committed:false` (correct for
  an empty scope) — and the branch worker is one goroutine, so the `Handling`/`applied` pairs are
  unambiguous;
- the spec failed at 21:21:37, **85 seconds later**. Not slow. Stuck.

So the ingresses report was made, under the revision the gate holds, and the gate did not take it.
By elimination the remaining candidates are exactly `recordScope`'s refusal branches — an unknown
target, a scope absent from the installed set, or a revision mismatch — plus the earlier return on
a zero revision. **All four were silent.** §4.4 makes all four speak; until one of them fires on a
reproduction, A's precise branch is not yet known and must not be guessed.

### 2.5 The second reproduction, and what it excludes

Run `33118310453`, `E2E (full-manager)`, on `cf08e467` — a build that already carried the
refused-revision recording of §4.5:

```text
watchrule "srcns-wildcard-rule" condition Ready: status="False" reason="Rechecking"
  message="Waiting for 1 of 4 render scopes to report under their current revision:
           secrets in 1787866675-test-srcns-wildcard (owes revision 4)"
```

**No refusal clause.** The scope owes revision 4 and has refused nothing, which excludes the
revision-mismatch branch outright — no report ever arrived carrying the wrong revision.

The log corroborates every other link. The plan went `keep=2 start=2` at 21:40:52, so the two
wildcard cells were issued revisions 3 and 4 by that one `Reconcile`; the secrets-wildcard replay
completed (`count:0`, rv 3230); and its resync was handled. So the stream ran, replayed and
enqueued, and its drain reached `MarkTargetRenderFidelityScopeClean`.

Yet the gate holds no result and recorded no refusal. `recordScope` records a refusal on a revision
mismatch, so the only way to reach the gate and leave no trace is **not to reach it at all**:

- `MarkTargetRenderFidelityScopeClean` returns before calling it when `revision == 0` — the
  stream carried no revision, though the gate issued it one; or
- `recordScope` returned at one of its `!found` guards, which record nothing.

Those two are what §4.4's line separates, and it is **not** in this build (`32b75e02` postdates
`cf08e467`). The next reproduction on a build that carries it closes A.

### 2.6 The third reproduction — local, fully instrumented, and still silent

`task test-e2e` on `f277f074` reproduced A locally, same spec and same shape:

```text
watchrule "srcns-wildcard-rule" condition Ready: status="False" reason="Rechecking"
  message="Waiting for 1 of 4 render scopes to report under their current revision:
           secrets in 1787867245-test-srcns-wildcard (owes revision 4)"
```

This build carried every diagnostic. What it says:

| Signal | Count | Meaning |
| --- | --- | --- |
| `a render scope result was not applied` | **0** | the gate never refused a report, on any branch, and was never handed a zero revision |
| `superseded by a newer resync` | **0** | restored to Info, so this zero is now meaningful |
| `per-type reconcile failed/refused/timed out` | 23, **none for this target** | all belong to the intentional refusal specs |

And upstream is again clean: the plan went `keep=2 start=2` at 21:53:33 issuing revisions 3 and 4,
the secrets-wildcard replay completed, and its resync was handled.

**The remaining ambiguity is a hole in the instrumentation itself.**
`MarkTargetRenderFidelityScopeClean` logs its refusals and says nothing when it succeeds, so
silence from it means EITHER "never called" OR "called and accepted". Those are opposite
conclusions and the reproduction cannot distinguish them.

That is the same asymmetry this page has now found three times — a component that reports what it
rejects and stays silent about what it takes. §4.4's line closed it for the refusals; the accept
path is closed in the commit that follows this one.

### 2.7 A rule's Ready is a COPY, and that was being read as a live view

Caught live on a passing run, mid-spec:

```text
GitTarget srcns-granted        Ready=True   RenderMatchesLive=True "Every rendered token matches live"
WatchRule srcns-granted-rule   Ready=False  Rechecking  2/2
WatchRule srcns-wildcard-rule  Ready=False  Rechecking  2/2
```

Both rules caught up within seconds, so this instance was the ordinary convergence transient. But
it exposes an assumption this page had been making without checking: **a rule's `Ready` is a copy
of its GitTarget's, not a live view of the gate.** `reconcileWatchRuleViaTarget` reads the target's
STORED `Ready` and folds it in as an independent prerequisite, so a rule reporting `Rechecking`
means only that the target said so *when the rule last reconciled*.

That splits Failure A in two, with completely different searches:

- the GitTarget is genuinely stuck at `Rechecking` for 90s — the gate never converges; or
- the GitTarget converged and the RULE's copy went stale — the rule is not being re-reconciled, or
  is reading a stale cached target.

Every reproduction so far has been read as the first. The evidence is consistent with it — the
rule reconciles every ~10s in the logs, and a rule that re-reads a Ready target goes Ready — but
**it has never been directly confirmed**, because the failure text prints only the rule.

The e2e assertion now prints the referenced GitTarget's own `Ready`, `RenderMatchesLive`,
`StreamsRunning` and `GitPathAccepted` beside the rule's condition, so the next reproduction says
which of the two it is without inference.

### 2.8 What a HEALTHY target looks like mid-plan

Worth knowing before watching a live cluster and misreading it: a target that has just been
(re)planned briefly publishes `RenderMatchesLive=Unknown` naming its pending scopes, then converges
within seconds. Observed repeatedly on passing runs, e.g.

```text
STUCK tilt-playground/playground-target :: Waiting for 3 of 5 render scopes …
  configmaps in tilt-playground (owes revision 1), deployments.apps … (owes revision 4), …
```

— which read `RenderMatchesLive=True, 5/5 streams` moments later. Mixed revisions across scopes are
normal too: a revision is issued per cell when that cell is first planned or restarted, so a
long-lived target accumulates different numbers per scope.

**The failure is not the pending state; it is the failure to leave it.** Any live watch on this
condition must require the state to persist (60s+) before treating it as a reproduction, or it will
fire on every ordinary replan.

### 2.9 SOLVED SHAPE: the gate converges and the GitTarget's status does not follow

Run `33120703394`, `E2E (full-manager)`, on `e3356796` — the first build carrying the accept-path
log. The rule failed with the usual message, and the controller log answers it outright:

```text
22:16:18  accepted  secrets in …-srcns-source     rev 2 -> Unknown
22:16:18  accepted  configmaps in …-srcns-source  rev 1 -> True
22:16:43  accepted  configmaps in …-srcns-wildcard rev 3 -> Unknown
22:16:43  accepted  secrets in …-srcns-wildcard   rev 4 -> True   "Every rendered token matches live"
```

**The gate accepted every report, including the one the condition said was owed, and reached
`True` at 22:16:43.** The spec failed at 22:19:14 with the rule still reporting
`secrets in … (owes revision 4)`.

Zero `not applied` lines. Zero superseded lines. The gate was never stuck.

**So Failure A is not a watch-plane defect at all. It is a status-publication defect.** Every
earlier section of this page read the rule's message as a live view of the gate; it is a copy of
the GitTarget's stored condition (§2.7), and the copy went stale.

Which copy is proven, too:

- the WatchRule reconciled **every 10s** from 22:16:43 to past 22:18:03 and stayed `False`, so the
  rule's own requeue is not at fault;
- `renderAxis` is fed from `RenderFidelityForGitTarget`, which returns `gate.Status(target)` —
  a LIVE read — so **any** GitTarget reconcile after 22:16:43 would have published `True`.

Therefore the GitTarget was **not reconciled at all** for ~2.5 minutes after its gate converged.

### 2.10 What remains for A

The remaining question is narrow and mechanical: why did the GitTarget not reconcile?

1. **A dropped enqueue.** `enqueueGitTargetReconcile` is a non-blocking send into a single
   256-slot channel and is silently dropped when full — the hazard `14eeef46` split stream
   transitions out of, for exactly this reason. Under the load this leg runs (163 WatchRule
   reconciles in the window) a load-bearing fidelity enqueue can be crowded out.
2. **The 5-minute steady requeue.** `gitTargetRequeue` gives a CONVERGED target
   `RequeueSteadyInterval` (5 min) and a non-converged one 10s. A target that converged, then had
   cells added, then lost its enqueue, waits the full five minutes — which matches the observed
   2.5-minute-and-counting staleness better than the 10s loop does.
3. **A publish-ordering artefact.** The two accepts land in the same second and publish the status
   each drain OBSERVED, not the gate's current one, so a stale `Unknown` can be written to
   `watchPlaneState.fidelity` after a fresh `True`. That surface is only used for change detection,
   so it cannot make the condition wrong — but it can suppress or misorder the enqueue that (1)
   depends on.

**Candidate (1) is now excluded, and §2.9 is confirmed a second time.** Run `33123538889`,
`full-manager`, on `43c4740b` — the first build carrying the dropped-enqueue log AND the e2e
assertion that prints the GitTarget's own conditions:

```text
22:57:26  accepted  ingresses.networking.k8s.io …  rev 5 -> True
22:59:17  FAIL  watchrule "watchrule-test" Ready=False Rechecking
          "… ingresses.networking.k8s.io … (owes revision 5)"
          | gittarget "watchrule-test-dest":
              StreamsRunning=True(5/5); GitPathAccepted=True;
              RenderMatchesLive=Unknown(Rechecking: … ingresses … owes revision 5);
              Ready=False(Rechecking: … )
```

- the gate reached `True` for the very scope named, at 22:57:26;
- **zero** dropped-reconcile lines, so the notification was not lost;
- the GitTarget's OWN published condition carries the same stale message two minutes later, which
  is what the new e2e detail proves — the rule is not merely holding a stale copy of a healthy
  target, the target's published status is stale too.

So: the gate converges, the reconcile request is delivered, and the GitTarget's status still does
not follow. Candidates (2) and (3) remain, plus a fourth the evidence now suggests — that the
reconcile happens and publishes the OLD axis, or does not happen despite the event.

`gitTargetRequeue` gives a non-converged target 10s, so even a lost notification should self-correct
within ten seconds; two minutes of staleness fits neither the 10s loop nor a delivered event. The
next step is therefore the one thing still unobserved: what the GitTarget controller publishes on
each reconcile, and the requeue it chooses. That log is added in the commit following this one.

Note that (3) is worth fixing on its own merits regardless, and has been: publishing the status a
drain observed rather than the gate's current status is a race with no upside.

## 2.12 SOLVED: a status write that loses a race is dropped, and nothing comes back

Reproduced locally on `26cd36c3` — the build carrying the publish log — on a 61-scope wildcard
target. Every link is now observed:

```text
23:32:13-17  66 "GitTarget status published" lines for ONE target, one per scope report
             …  Unknown/Rechecking  converged=false  requeue=10s   (×65)
23:32:17     True/RenderMatchesLive  converged=true   requeue=5m0s  ← the LAST one
23:33:41     FAIL: gittarget "…-dest" RenderMatchesLive=Unknown(Rechecking: … owes revision 44)
```

The gate accepted every scope and reached `True`. No dropped reconcile requests. The controller
computed `True` and logged that it published it. **And 84 seconds later the object still read
`Unknown`.**

The cause is in [`reconcileStatus.commit`](../../internal/controller/status.go):

```go
case apierrors.IsConflict(err):
    log.V(1).Info("status write skipped; object changed during reconcile", …)
    return nil          // ← the write is dropped AND the reconcile reports success
```

Dropping the write is right: by then the observation is stale. Dropping the reconcile with it was
not. The comment justified it as *"The write that beat us enqueued us again"* — and that is **false
by construction here**. The winning write is a STATUS-only update, and every `For()` in this
package carries `predicate.GenerationChangedPredicate`, which exists precisely to filter those out
and break the status-write-triggers-reconcile loop.

So the sequence is:

1. a 61-scope target produces ~66 reconciles in four seconds, one per scope report;
2. reconcile *N* computes `Unknown` and wins the write;
3. reconcile *N+1* computes `True`, loses the optimistic lock, and is silently discarded;
4. it returns `converged=true`, so the caller picks **`RequeueAfter: 5m`**;
5. the winning write was status-only, so the predicate re-enqueues nothing;
6. the object holds `Rechecking` for five minutes, and every WatchRule copies it.

That accounts for every observation, including the two that resisted explanation longest: why it is
**load-dependent** (it needs concurrent reconciles to produce a conflict, which is why all eight
focused low-load attempts passed) and why the staleness lasts **minutes rather than the 10s** a
non-converged target would wait.

**Confirmed independently**, same run, different spec and a different scale — `quickstart-install`
on a 5-scope target:

```text
gate: ingresses.networking.k8s.io … rev 5 -> True
12 publishes, the LAST:  True/RenderMatchesLive  converged=true  requeue=5m0s
~100s later:             gittarget … RenderMatchesLive=Unknown(Rechecking: … owes revision 5)
```

Twelve reconciles rather than sixty-six, five scopes rather than sixty-one, and the same outcome:
the reconcile that computed `True` lost the write and took the converged cadence with it. The
mechanism does not need a large scope count, only two reconciles close enough together to conflict.

**The fix** records the loss rather than returning it — a conflict is expected and must not become
a reconcile error for every controller using this helper — and the GitTarget reconcile asks
`writeLost()` before choosing its requeue, taking the 10s settle interval instead of the converged
five minutes. The stale-observation protection is untouched.

### 2.13 The failure class

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

## 3. Failure B — named, with a local reproduction

Reproduced **locally** for the first time on `fc04b15b`, which is what settled it. The local
failure carries one field the CI reproductions did not print:

```text
a resync that retains nothing must drive the count back to zero, not leave it stale;
retainedDocuments=1 mode="OnEvent"
```

`mode` is the **pre-widening** value, and that is the whole diagnosis.

### 3.1 The timeline

For `prune-default-target`, all inside 32 seconds:

| Time | Event |
| --- | --- |
| 21:11:03 | `resync retained managed documents … retained:1 pruneMode:"OnEvent"` |
| 21:11:19 | widen patch applied (`prune.mode: Always`) |
| **21:11:25** | plan reconciled — **`restart:1`** — the force replay fires correctly |
| 21:11:25 | replay complete `count:2`; `Handling resync request` → **`Resync request applied`, `committed:true deleted:1`** |
| 21:11:27 → 21:11:57 | `keep:1` only. No restart, no further resync |

The force replay works. The resync applied and committed. And the roll-up never moved.

### 3.2 The proof, by elimination

`MarkTargetRetention` assigns `state.mode = mode.OrDefault()` **unconditionally** on its accepted
path. So a published `mode` still reading `OnEvent` admits only two explanations: no report
arrived, or the report itself carried `OnEvent`.

A report certainly arrived — the resync applied successfully, and `drainScopedResync` calls
`MarkTargetRetention` unconditionally on a successful result. No `retention report dropped` line
fired, so it was not refused.

And the second branch is excluded too: the resync logged `deleted:1`, which only a plan built under
`Always` can produce (§3.3), so the report cannot have carried `OnEvent`.

**Both explanations are excluded, so one of the observations is not what it appears.** That is the
honest state of B, and §3.4 says which link is unobserved. Do not close this by picking whichever
branch is convenient — the last attempt to do that is recorded in §3.3 as a withdrawn fix.

### 3.3 What is NOT the cause — a hypothesis tried and withdrawn

A first reading blamed a stale prune mode: the replay is forced by the owner (which learned the new
mode from the declare) while the branch worker re-derives the mode independently through its
**cached** client in [`resolveTargetMetadata`](../../internal/git/pending_writes.go), so a worker
cache lagging the patch could plan the forced replay under the mode it was forced to replace.

**The evidence refutes it.** `resyncPlanPolicy` maps `onEvent` and `never` alike to
`SweepRetainOrphans`, so a resync planned under anything but `always` emits **no managed drop at
all**. The widening resync logged `deleted:1`, and the spec's `waitForPruneFile(…, false)`
assertions for **both** orphans passed before the failing one. So that resync planned under
`Always`, swept correctly, and its `RetainedOrphans` should have been zero.

A fix that threaded the policy onto the `ResyncRequest` was written, passed the previously failing
spec once locally, and was **reverted**. One green run is not evidence for a mechanism the logs
contradict, and the change carried a real hazard in the safety-critical direction: narrowing the
mode deliberately does NOT force a replay (see `pruneModeRequiresReplay`), so a stream carrying a
snapshotted `always` would sweep under it on any later re-replay — after an operator had tightened
to `never` to stop exactly that. Tightening must stay the cheap, quiet direction.

### 3.4 Where B actually stands

Established:

- the force replay fires (`restart:1`);
- the resync applies, commits, and sweeps under `Always` (`deleted:1`, both files gone);
- `drainScopedResync` therefore reached its success branch, which calls `MarkTargetRetention`
  unconditionally;
- no `retention report dropped` line fired, so nothing was refused;
- and the published roll-up did not move — still `retained=1`, `mode="OnEvent"`.

Those cannot all be true at once, which means one of them is not what it appears. The roll-up is
the only unobserved link: `MarkTargetRetention` logs its refusals but says nothing when it accepts,
and `mutateWatchPlane` **discards the entire mutation** when `changed` is false, so an accepted
report and an absent one look identical from outside.

That is the next thing to instrument, and it is the same asymmetry that left A blind: the roll-up
reports what it rejects and stays silent about what it takes.

### 3.5 Why every earlier diagnostic missed it

The `resync retained managed documents` Info line is throttled to once per ten minutes per
`gitTarget@base` by `shouldLogRetention`, and its unthrottled twin is `V(1)`, which CI does not run.
The drop diagnostic could not fire because nothing was dropped. B1 and B2 as originally posed — the
revision gate and the superseded path — are both retired.

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

**4.4 Say when a scope result is refused.** `RecordScope*` answers `applied=false` for three
different reasons and every caller discarded that answer; a fourth path returned even earlier, on a
report carrying no revision. All four now name the cell, the revision reported and the gate's
current message. The retention roll-up has logged its refusals since `c24844a1`, and that asymmetry
is the whole reason B accumulated evidence while A accumulated none.

**4.5 Record what the gate refuses, on the scope itself.** A refused report — one carrying a revision the plan has moved
past — was discarded in silence, so a scope waiting for its first replay and a scope discarding a
steady stream of reports were indistinguishable from outside. They have opposite repairs: the first
converges by waiting, the second never does. The scope now remembers the last revision it refused
and the pending message says so. This is the asymmetry that gave B evidence and left A with none;
the retention roll-up already logged its drops.

**4.6 Instrument what the retention roll-up ACCEPTS.** It logs every refusal and says nothing when
it takes a report, and `mutateWatchPlane` discards the whole mutation when nothing an operator
would see moved — so "accepted and unchanged" and "never reported" are the same silence. Closing
that is what §3.4 needs, and it is the same repair §4.4 made on the fidelity side.

**4.7 Do not weaken the revision gate.** A stale report from a replaced stream must still be
refused. The repair is that a scope which cannot be reported under its current revision must be
*re-measured*, not that an old measurement may stand in for a new one.

### 4.8 Order of work

Steps 1-5 have LANDED. What remains is step 6, and it needs a reproduction rather than a theory.

1. ✅ Pending scopes named in the condition, with the revision each owes (§4.1).
2. ✅ Temporary A1/A2 diagnostics removed; supersession log back to `V(1)` (§4.2).
3. ✅ The drain starts even when the enqueue was dropped (§4.3).
4. ✅ Every fidelity refusal named, on all four branches (§4.4), and recorded on the scope (§4.5).
5. ✅ The retention roll-up says when it accepts a report that publishes nothing (§4.6).
6. ⬜ **Take the next reproduction and read what it says.** Do not guess between the branches; §3.3
   is what guessing cost last time.

Reading the next A reproduction:

| What the condition says | Branch |
| --- | --- |
| `stream carries no revision` | the stream was started without one though the gate issued it one — the plan pass and the gate disagree about the cell |
| a `render scope result was not applied` line with a non-zero `reportedRevision` | the gate holds the scope but would not take the report — one of `recordScope`'s `!found` guards |
| owes revision N, a refusal clause naming a different revision | a stale tail; the live stream's own report is what to look for next |
| a `render scope result accepted` line for the cell, yet the condition still names it | the gate took the report and the scope is STILL pending — the disagreement is inside the gate, or a later `Reconcile` reset it |
| no `accepted` line and no `not applied` line | nothing reached the mark path — the search moves to the drain and to `enqueueReplayResync`, whose `ctx.Done()` guard returns nil as if it had succeeded |

There is a fifth silent branch, and it is deliberately left silent: `MarkTargetRenderFidelity*`
returns without a word when `fidelityGate()` is nil, which is the no-gate-wired legacy path used by
tests. It cannot explain any failure in which OTHER scopes of the same target reported, because the
gate is a single instance created in `NewWorkerManager` and shared by every worker and the watch
manager — if it were nil, no scope would ever report and the target would read vacuously ready.
Do not spend a run on it.

Reading the next B reproduction:

| What the log says | Meaning |
| --- | --- |
| `retention report accepted but published nothing` | the report landed and matched the previous values — so the count was already what the sweep produced, and the bug is upstream of the roll-up |
| `retention report dropped` | the revision gate or the plan membership refused it |
| `superseded by a newer resync` | the coalescing path skipped this scope's reports; check that the REPLACEMENT then reported |
| neither, count still stale | `MarkTargetRetention` was never called; the search moves to the drain |

The supersession line is at Info **while B is open**. It was lowered to `V(1)` once, on the
assumption the fidelity condition had made every lost report visible; it had not — that path skips
the retention report too, and lowering it re-blinded the path in the very local reproduction that
followed. Zero occurrences of it in a run built after that lowering therefore proves nothing.

Do not treat a green full run as evidence at any step. Both failures have produced fully green CI
on a commit that failed locally, and vice versa — and §3.3 records a fix that passed the very spec
it targeted while contradicting the logs.

---

## 5. Flake inventory — what is ambient, and must not be misattributed

Three separate branches have now been blamed for failures that reproduce on `main`. Check this
table before bisecting.

### 5.1 Ambient, confirmed

| Flake | Signature | Rate | Notes |
| --- | --- | --- | --- |
| **Encryption-secret recreation** | `secrets "recreated-sops-age-key" not found`, 45s, `gittarget_controller_test.go:1140` | ~1 in 11 historically; **3 of 4 CI runs on 2026-08-27** | Unit tests. See [`TODO.md`](../TODO.md). Passes on a re-run of identical code. |
| **Refused-GitTarget recovery** | `GitPathAccepted` projection racy both ways; next requeue up to 10 min | Reproduces locally and deterministically in the `unsupported-folder` refusal spec | Do not chase when the diff is test/docs-only. |
| **`target_watch` forbidden race** | `TestTargetWatchReplayAndStream_FallsBackWhenReplayWatchIsForbidden` | Only under `-race`; CI does not use it | Pre-existing shutdown race. |

**The encryption-secret flake's rate is much worse than recorded.** On 2026-08-27 it failed the
Unit job in runs `33116777679`, `33119960052` and `33120703394`, passing only `33118310453` — three
in four, against a documented ~1 in 11. It is not caused by this branch: the failing path touches
neither the watch plane nor anything this branch changes, `task test` passes locally on the same
commits, and the controller log shows the secret being generated (`Generated missing encryption
secret with age key … default/recreated-sops-age-key`) while the test's `Get` never observes it —
the create-then-read race exactly as described. But at this rate **it alone will keep CI from going
green**, so it needs its own fix before this branch can merge on a green run rather than on a
re-run.

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
| `33116777679` | `fc04b15b` | **quickstart-install: A**, and the new condition message named the stuck scope outright; Unit tests: D (ambient). Remaining legs cancelled by a push |
| `33118310453` | `cf08e467` | Lint, Unit, quickstart-install, bi-directional, source-cluster, image-refresh green |

Local suites, same code:

| Run | Commit | Result |
| --- | --- | --- |
| `task test-e2e` ×2 | `5e054717` and earlier tree | 80/80 pass |
| `task test-e2e-bi-directional` | `5e054717` | 3/3 pass, including the spec that failed as C |
| `task test-e2e` | `c24844a1` | **75 pass, 1 fail — A** (`signing-per-event-wr`) |
| `task test-e2e` | `773f6cc0` | 80/80 pass |
| `task test-e2e` | `fc04b15b` | **79 pass, 1 fail — B**, the first local reproduction of B |
| `task test-e2e` | `cf08e467` | 80/80 pass — on the since-reverted fix, so it validates nothing |

Related change that landed and is **not** a suspect for either open failure: `14eeef46` split
stream-state transitions off the acceptance channel. Sharing one 256-slot drop-on-full buffer
between rare load-bearing events (acceptance, render-fidelity, retention — up to ~5 min of stale
status when dropped) and high-volume ones (stream transitions — ~10s) is a real design error, and
the fix stands on that reasoning. It does not explain B, which predates it.
