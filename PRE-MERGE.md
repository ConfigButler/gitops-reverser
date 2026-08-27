# Pre-merge: `feat/target-watch-cell-identity` (PR #315)

Working document for finishing this branch. Written so a fresh context can pick it up
without re-deriving anything. **Facts are separated from hypotheses**, and hypotheses
that are already dead are recorded as dead so nobody chases them twice.

Delete this file (or archive it under `docs/finished/`) as part of the merge. It
describes getting the branch over the line, not the system.

---

## 1. Where the branch stands

`main` + 36 commits, 0 behind. PR #315 open, `MERGEABLE`, not a draft.

### Local gates, run on `1eb958a0` before the fixes in §3

| Gate | Result |
| --- | --- |
| `task lint` | pass — doccheck resolves, 0 Vale errors |
| `task test` | pass — unit coverage 77.3%, baseline 77.3% committed |
| `task test-e2e` | pass — **80 passed, 0 failed**, 23 skipped, 667s |
| `-race` on `internal/watch`, `internal/git`, `reconcile`, `types`, `typeset` | clean |

Re-run after the §3 fixes: `task lint` pass, `task test` pass (77.3%, baseline
unmoved). **`task test-e2e` has not been re-run since those fixes** — that is the one
outstanding gate, and it is a merge blocker under AGENTS.md.

### CI on `1eb958a0`

Run `33074976097`. Attempt 1 failed on **one** leg; attempt 2 was started by `sunib`
at 13:31:30Z. **Attempt 2 passed** — all six e2e legs now green.

> Five of those carry their **attempt-1** passes: `--failed` re-runs only the failure.
> So "all six green" means "full-manager passed on a re-run", not "all six passed
> together on one attempt".

**The failure is intermittent**: original run failed, re-run passed, and a local repro
with the identical 64-spec filter passed. See §2 — it has been fixed on mechanism, not
on statistics.

---

## 2. The CI failure

`E2E (full-manager)`, attempt 1, [`prune_mode_e2e_test.go:280`](test/e2e/prune_mode_e2e_test.go#L280):

> Manager GitTarget prune policy — **converges an existing orphan when `prune.mode` is
> widened, without touching the WatchRule**

Ran 64 of 103 specs in 565s: 63 passed, 1 failed.

### 2.1 Facts

- **The sweep worked.** Both `waitForPruneFile(..., false)` assertions passed — the
  late orphan and the original orphan were removed from Git. The failure is downstream
  of the sweep, in status only.
- **The stale value is `status.retention.retainedDocuments`.** It stayed at `1` instead
  of reaching `0`, for the spec's full 30s budget. `status.retention.mode` reaching
  `Always` was not what failed.
- **It is new to the last two commits.** All six e2e legs passed on `416f8596`, the
  commit immediately before `cdab0c33` and `1eb958a0`.
- **It is not slowness.** Measured across three local full runs, that value converges in
  **~0.1s** — it is already `0` before the check first reads it. CI did not see it move
  for 30 seconds.
- **The target is converged (Ready) at that point**, so its own requeue is ~5 minutes,
  not 10 seconds. Whatever the cause, a missed update sits stale far past any test
  budget. This is why the symptom is 30s-of-nothing rather than a near-miss.
- **`retainedDocumentsOf` fails on an ABSENT retention block** rather than reading it as
  zero ([prune_mode_e2e_test.go:292](test/e2e/prune_mode_e2e_test.go#L292)). The failure
  was a stale `1`, not an absent block, so a roll-up *was* published — with the old
  count.

### 2.2 Hypothesis A — shared event channel (**accepted and FIXED**)

The original theory: `1eb958a0` routed stream-state transitions through
`enqueueStreamStateChange`, which calls `enqueueGitTargetReconcile` and so shares
`gitPathEventsCh` — one 256-slot, drop-on-full channel — with acceptance,
render-fidelity and retention. Mixing high-frequency best-effort events into a buffer
carrying low-frequency load-bearing ones.

What is true: the sharing is real. `enqueueStreamStateChange` is the **seventh**
producer into that single channel ([gitpath_events.go:33](internal/watch/gitpath_events.go#L33)),
and the six pre-existing ones are acceptance, render-fidelity, retention, cluster
context, the plan pass, and the stream-state path itself.

What kills it as stated: **a Go buffered channel drops, it does not evict.**
`select { case ch <- evt: default: }` discards the *arriving* event when the buffer is
full; it can never displace an event already queued. "A burst of stream events evicts
the retention event" is not a mechanism that exists. The reachable version needs the
retention transition to fire while 256 events are already pending — and `source.Channel`
drains into a workqueue that dedupes by object key, so all of these collapse to one item
per GitTarget. That matches the original instinct that the transition count does not
plausibly fill 256.

**Resolved: the channel was split, and the wording above was the only thing wrong.**
The mechanism is *crowding out*, not eviction — the expensive event arrives to find the
buffer full of cheap ones and is itself the value dropped. Same outcome, and the
distinction is what tells you what to measure if this is ever suspected again: the
buffer depth at the moment the expensive event arrives, not some displacement event.

The cost asymmetry is the real argument, and it stands independently of reproduction:

| Event class | Volume | Cost of a drop |
| --- | --- | --- |
| acceptance / render-fidelity / retention | low | up to ~5 min of stale status |
| stream transitions | per cell, per plan change, plus a flap per distinct error message | up to ~10 s |

The GitTarget controller now takes its own `StreamStateEvents()` subscription, and
`enqueueStreamStateChange` no longer touches the acceptance channel at all. That
restores the pre-change loss characteristics for the load-bearing events while keeping
the latency win. A unit test asserts the separation directly: a stream transition must
not appear on the acceptance channel.

"Cannot reproduce on an idle laptop" is not evidence against a drop-under-load bug —
CI is the loaded environment, and the observed signature (a count frozen at its old
value for 30s while the files it counts were already swept, on a target whose requeue
is five minutes) is what a dropped `enqueueGitTargetReconcile` looks like and what
nothing else in that path produces.

### 2.3 Hypothesis B — the retention revision gate (**untested, still open**)

This one is in code the branch changed, and it explains a stale count with no
notification involved at all.

`MarkTargetRetention` **drops any report whose revision does not match the scope's
currently installed revision** ([retention_rollup.go:92](internal/watch/retention_rollup.go#L92)):

```go
scope, selected := state.scopes[cell]
if !selected || revision != scope.revision {
    return false
}
```

And `retainTargetRetentionScopes` deliberately **keeps the previous count** when a
stream is restarted, rather than zeroing a scope nobody re-measured
([retention_rollup.go:120](internal/watch/retention_rollup.go#L120)):

> A restarted stream reports under a new revision. Its previous count stands until the
> replacement reports, which is a truer answer than zeroing a scope nobody re-measured.

Now trace the spec's one action. Widening `prune.mode` sets `force` via
`pruneModeRequiresReplay`; `diffTargetWatchPlans(previous, desired, force)` classifies
**every** desired cell as `restart`; `RenderFidelityGate.Reconcile` issues **fresh**
revisions for every restarted scope; `retainTargetRetentionScopes` installs those fresh
revisions **while keeping `retained: 1`**.

That is precisely the observed state: sweep succeeds, count stays at 1. It is what this
path produces whenever the replacement stream's report is rejected on a revision
mismatch — a **dropped report**, not a delayed notification. Consistent with "did not
move for 30 seconds" in a way that a slow event is not.

**Still worth checking**, and not excluded by the fix in §2.2 — the two are
independent, and this one would produce the same signature without any notification
being involved. **What to check.** A stream captures its revision at start and reports it when the
replay result is ready ([target_watch.go](internal/watch/target_watch.go), `revision`
field). The 30s periodic sweep runs a pass — and therefore
`retainTargetRetentionScopes` — for every declared target. So: can a pass that lands
*while a replay is in flight* install a revision that differs from the one the in-flight
replay captured? If yes, its report is dropped on arrival and the count stays stale
until something else re-measures it, which for a converged target is ~5 minutes. CI is
slower than local, which would widen exactly that window and explains why three local
runs never showed it.

### 2.4 How to settle it

1. Read the **controller logs** from the failed leg, not the spec output —
   `kubectl logs` of the manager, looking for a retention report arriving and being
   dropped. (Repo lesson: diagnose e2e from controller logs before theorizing.)
2. If B holds, the fix is on the producer side of the revision gate, not on the channel.
3. If both re-runs come back green, **do not call it a flake on that basis alone** —
   the timing evidence in §2.1 is against ambient flakiness, and the failure is new to
   two specific commits.

### 2.5 Ruled out, do not re-derive

- **Not a known-flaky spec.** This is not the `unsupported-folder` refusal spec, nor the
  `recreated-sops-age-key` create-then-read race, nor the `-race`-only
  `TestTargetWatchReplayAndStream_FallsBackWhenReplayWatchIsForbidden`.
- **Not cluster poisoning.** The leg ran 63 specs to completion in 565s; a wedged k3d
  cluster fails at bring-up, not 64 specs deep.
- **Not the sweep, the Git write path, or `prune.mode` handling.** Both file assertions
  passed and `status.retention.mode` reached `Always`.

---

## 3. Review findings — status

From the full-branch review. Everything below is **applied on the working tree**, lint
and unit green; nothing is committed yet.

| # | Finding | Status |
| --- | --- | --- |
| 1 | Per-target pass deadline was never enforced | **fixed** |
| 2 | `E2E-DECLARE-INVESTIGATION.md` left at repo root | **fixed** |
| 3 | Shipped design docs in `design/`, contradicting INDEX's own rule | **fixed** (annotated) |
| 4 | Open follow-ups tracked nowhere | **fixed** |
| 5 | Unrelated docs bundled into the PR | **dropped — not worth splitting** |
| 6 | `recordPassOutcome` republished on every pass | **fixed** |
| 7 | `markTargetStreamState` comment overstated the code | **fixed** (comment) |
| 8 | Readiness gate gap when `Declared` is false | **withdrawn — unreachable** |

### 1. The pass deadline bounded nothing

`targetPassTimeout` (30s) was documented as *"A pass that exceeds this fails, leaves the
target dirty, and INSTALLS NOTHING"*, but nothing on the pass path read the context:
`applyTargetPlan` → `ensureGitTargetWatches` → `replaceGitTargetWatches` never called
`ctx.Err()`, and the ctx it passed on is discarded by `streamParent` in production.

Consequences: the owner loop would block indefinitely on any step that stalled — the
availability failure `watch-manager-ownership.md` exists to remove — and the `timed_out`
pass outcome could never be emitted, while
[interpreting-metrics.md:689](docs/interpreting-metrics.md#L689) tells operators to
alert on it.

Fixed by checking the deadline at each step boundary via a new `passDeadline` helper
that wraps `ctx.Err()` with `%w` and names the step it ran out before. `isDeadlineExceeded`
now uses `errors.Is` instead of `strings.Contains` on the rendered message. Covered by
`TestOwner_APassThatIsOutOfTimeStopsAndSaysSo`.

### 2. Root-level investigation doc

`git mv E2E-DECLARE-INVESTIGATION.md docs/finished/e2e-declare-investigation.md`, its
two relative links repointed, its inbound link from `watch-manager-ownership.md`
updated, and a line added so its preserved "the branch is NOT mergeable" header is not
read as current.

### 3. INDEX contradicted itself

INDEX defines `design/` as "we are still deciding" and `finished/` as "binds? **no**",
yet labelled `watch-manager-ownership.md` `**built.**` — the only such entry in the
file — while 17 Go files cite it and `target-watch-plan.md` by path as binding
rationale.

Resolved by stating the exception rather than moving files: a shipped `design/` page
stays put when Go source cites it by path, because `finished/` declares itself
non-binding and these still bind. `target-watch-plan.md` was also missing from INDEX
entirely and now has an entry.

### 4. Untracked follow-ups

Two entries added to `docs/TODO.md`:

- **`typeset.Registry.Subscribe` has no production observer.** Events are computed on
  every `Update` and dispatched to nobody since the Materializer was deleted. It is the
  producer of the only signal separating a settled `TypeRemoved` from a discovery
  wobble — and mistaking the second for the first deletes a user's manifests. Its
  intended consumer is the `stop` classification in `target-watch-plan.md`.
- **Whether the settle window should be configurable**, with the shipped behavioural
  consequence noted (toggling a rule off and on inside the window is no longer a replay).

### 5. Unrelated docs — open

`ae8bdfb8` adds `helm-light-support-boundary.md` (316 lines) and
`direction-and-configuration-surface.md` (332 lines): strategy pages unrelated to the
watch plane, riding on this PR. **Decided: leave them.** They are additive
documentation that harms nothing, and splitting a commit out of a 36-commit branch to
relocate two pages is not worth the churn.

### 6, 7, 8

- `recordPassOutcome` returned `true` unconditionally, so every pass cloned and
  republished the whole snapshot — once per target per 30s sweep, forever — to advance
  `LastAttempt`/`LastSuccess`, which **nothing ever read as a time** (only
  `LastSuccess.IsZero()`). Both fields replaced by a `Landed bool`; republish is now
  conditional. Covered by `TestRecordPassOutcome_ASteadyStatePassRepublishesNothing`.
- `markTargetStreamState` said "on the TRANSITION only" while `setStreamState` compares
  the whole status including `message`. The code is right — the message is published on
  the rule's condition, so a new failure reason has moved something a reader sees — so
  the comment was corrected rather than the behaviour.
- The readiness-gate gap is **withdrawn**: `observeDataPlane` calls
  `DeclareForGitTarget` immediately before `DeclareStatusForGitTarget`, and
  `declareIntentFor` records the intent unconditionally, so `Declared` is true by
  construction at that call site.

---

## 4. What is left

1. **Re-run `task test-e2e`** after the channel split and the §3 fixes. Note what a
   green run does and does not prove: the failure was intermittent, so one pass is not
   evidence the fix worked. The argument for the fix is mechanical.
2. **Consider §2.3** — the retention revision gate is independent of the channel fix
   and would produce the same signature. Not blocking, but not excluded either.
3. **Update the PR body**: it claims `task test-e2e` "80 passed, 0 failed … green on
   this commit", which was true locally and is not the CI story. Add the channel split
   and the §3 fixes.
4. **Delete this file** as part of the merge.

### Notes for whoever picks this up

- Run the e2e commands **sequentially, never in parallel** (AGENTS.md).
- Read the Ginkgo JSON report rather than a truncated background log.
- `E2E_LABEL_FILTER` **replaces** the default filter — re-AND the exclusions, or a
  zero-match filter will skip both suite hooks and pass vacuously.
