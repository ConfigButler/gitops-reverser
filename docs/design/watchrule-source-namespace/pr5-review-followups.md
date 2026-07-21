# PR 5 review follow-ups

> Work list from a review of PR 5 ([#260](https://github.com/ConfigButler/gitops-reverser/pull/260)):
> two behavioral gaps and two documentation corrections. **Every claim below was re-verified against
> the code before being accepted**, and all four hold. Nothing here reopens the
> [deletion-safety decision](pr5-gittarget-deletion-safety.md) — `OnEvent` stays the default, and the
> review reached that conclusion independently.
>
> **Status: all four landed on this branch.** All gates were green when the review ran (`task lint`,
> `task test`, and `task test-e2e` at 73 passed / 22 skipped / 0 failures), so nothing below was
> caught by a gate — which is the point of writing it down. Each section keeps the analysis that
> justified the fix; the **Landed** note at its end records what was actually done and which test
> holds it. After the fixes: 75 passed / 22 skipped / 0 failures, unit coverage 77.8%.

## The two blockers are one defect seen from both sides

PR 5 applies `prune.mode` correctly everywhere it is *read*. Both high findings are about what
happens when the value **changes**: it is captured at a moment and never re-read.

| Operator does | Intent | What should happen | What happens |
|---|---|---|---|
| `OnEvent` → `Always` | "converge this mirror" | the orphans get swept | nothing, until a replay happens for an unrelated reason (**R1**) |
| `Always` → `OnEvent`/`Never` | "stop deleting, now" | queued deletions stop | a locally committed, unpushed write can still replay under `Always` (**R2**) |

So R1 and R2 are opposite halves of one missing rule: **the effective mode must be re-read at the
moment it is acted on, and a change in it must be an event.** The two fixes are deliberately
asymmetric, because the risk is asymmetric — a loosening may take its time but must be triggered; a
tightening must take effect immediately and must trigger nothing.

## Summary

| # | Item | Verdict | Status |
|---|---|---|---|
| R1 | Declaring `Always` does not converge a quiet target | Confirmed | **Fixed** |
| R2 | A tightening is outrun by an already-retained write on rebase replay | Confirmed, **exposure narrowed** | **Fixed** |
| R3 | The retention log does not name the GitTarget the docs promise it names | Confirmed | **Fixed** — with [retention visibility](pr5-retention-visibility.md) Step 0 |
| R4 | The docs claim a failure mode the code does not have | Confirmed | **Fixed** |
| — | Keep the retention metric target-unlabelled for cardinality | **Declined** | Not taken |

## R1 — declaring `Always` does not converge a quiet target

[UPGRADING.md](../../UPGRADING.md) tells an operator that declaring `Always` keeps the old behavior,
and [configuration.md](../../configuration.md) that it gives a "faithful, converged mirror". A
`prune.mode` edit does neither on its own:

- the GitTarget controller's only force flag is `gitPathWasRefused`
  ([`gittarget_controller.go`](../../../internal/controller/gittarget_controller.go));
- an unchanged watch-spec set returns early from `prepareTargetWatchSetReplacementLocked`
  ([`target_watch.go`](../../../internal/watch/target_watch.go)) — `equalTargetWatchSpecs` compares
  GVR, namespace, and operation filter, and the prune mode is (correctly) not among them;
- the **only** production resync enqueue is `enqueueReplayResync`, reached from a completed initial
  replay or the LIST fallback. Nothing else in the operator enqueues a resync.

The part that makes this worse than "converges eventually" is the reconnect path. After the first
session `runTargetWatch` sets `resumeFromCursor = true`, and a cursor resume streams live events
**without** enqueuing anything. So the practical trigger set for a sweep is: a controller restart, a
WatchRule edit that changes this target's watch set, or a cursor expiry. A healthy target with stable
rules whose cursors keep resuming can sit under `Always` indefinitely without ever sweeping the
orphans the operator declared `Always` to remove.

The e2e suite already contains the workaround, which is the strongest evidence the gap is real —
the sweep spec has to churn the rule to make a resync happen at all:

~~~go
By("toggling ConfigMaps off and back on to force a scoped replay resync")
~~~
— [`prune_mode_e2e_test.go`](../../../test/e2e/prune_mode_e2e_test.go)

**Fix.** Edge-triggered force. Remember the last-declared effective mode per `gitDest` beside the
source cluster id (`rememberGitTargetCluster` in
[`materialization.go`](../../../internal/watch/materialization.go) is the shape), and force the watch
set when it has changed *to* a sweeping mode:

~~~go
DeclareForGitTarget(ctx, gitDest, target.SourceCluster(), gitPathWasRefused || pruneModeBecameSweeping)
~~~

`force` already does the right thing end to end: it cancels the prior set, and the first session of
the replacement replays rather than resuming, which is exactly what enqueues the resync.

Two things this must get right, both silent if wrong:

- **Edge, not level.** Forcing whenever the mode *is* `Always` forces on every steady requeue, which
  is a permanent replay loop. Only a *change* may force.
- **Only the loosening direction.** `always → never` needs no replay, and forcing one would tear down
  every stream at the exact moment an operator is trying to stop something from happening. The
  remembered mode must also be dropped in `ForgetGitTargetDeclaration` with the rest of the
  per-target state.

**Considered and not chosen:** putting the mode into the watch-set identity (`equalTargetWatchSpecs`).
It makes both directions do the same expensive thing, including the one that must not churn, and it
puts a policy value into a structure that otherwise describes only *what is being watched* — two
concerns that would then have to be kept in sync forever.

**Worth noting for later:** `ResyncRequest.Heal` exists for precisely this shape — its doc comment
says "a periodic checkpoint re-anchor or a removed-type sweep"
([`types.go`](../../../internal/git/types.go)) — and it currently has **no production producer**;
every call site passes `heal: false`. A heal-shaped "re-list this target's scopes and enqueue a
converging resync" would avoid stream churn altogether. That is a larger change than this PR should
carry; the force flag is the right size now, and the heal path is the better long-term home.

**Test.** e2e: seed an orphan under the default mode, patch `spec.prune.mode: Always`, and observe
the sweep **without touching the WatchRule**. That is the test that would have caught this, and it
also lets the existing sweep spec drop its toggle workaround. Unit: a declare whose mode changed to
`Always` replaces the watch set; an unchanged mode does not; and `always → onEvent` does not.

**Landed.** [`prune_declaration.go`](../../../internal/watch/prune_declaration.go) holds the whole
seam — `pruneModeRequiresReplay` / `rememberGitTargetPruneMode` / `forgetGitTargetPruneMode` — and
`DeclareForGitTarget` now takes the effective mode beside the source cluster. Three details worth
recording:

- The mode is remembered **only after `EnsureGitTargetWatches` succeeds**. A failed declare must
  leave the pending force standing for the next reconcile rather than consuming it on an attempt
  that never reached the data plane, so the value means "what the running watches were built for".
- The `forgetGitTargetPruneMode` rationale in the first draft of this document was wrong, and the
  code comment says so instead: a recreated GitTarget has no watch set to replace, so its first
  declare replays regardless. Forgetting is lifecycle hygiene — an entry per deleted target, and a
  claim about state that no longer exists — not a correctness fix.
- `TestReplaceGitTargetWatches_ForceReplaysAnUnchangedSet` asserts the mechanism itself with an
  explicit negative control (the same specs without the flag reopen nothing), because every other
  test in this area would pass whether or not the flag reached the watch layer.

Held by [`prune_declaration_test.go`](../../../internal/watch/prune_declaration_test.go) — a truth
table over all nine transitions, the two `false` rows carrying the reasons — and by the e2e spec
*"converges an existing orphan when prune.mode is widened, without touching the WatchRule"*, which
seeds a second orphan whose only possible trigger is the patch.

## R2 — a tightening is outrun by an already-retained write

`ResolvedTargetMetadata.PruneMode` is captured at plan time, and its doc comment states outright that
a rebase replay applies the policy in force when the write was planned rather than the current one
([`types.go`](../../../internal/git/types.go)). `executeResyncPendingWrite` passes that stored value
into the planner ([`resync_flush.go`](../../../internal/git/resync_flush.go)).

**The exposure is the retained-write window, not the queue** — worth stating precisely, because it
narrows the finding without dismissing it. A *queued resync request* is safe: `buildResyncPendingWrite`
calls `resolveTargetMetadata` at apply time, so it reads the current spec. What is not safe is a
`PendingWrite`: committed locally, retained until push (`PushCooldown` is 5s, and much longer while
pushes keep failing), and **re-executed** on a push conflict via `pushPendingCommits` →
`rebuildPendingWrites` → `executePendingWrites`
([`branch_worker.go`](../../../internal/git/branch_worker.go)). The replay re-plans against the newly
rebased worktree, so it can compute drops the first apply never made — against a remote that changed
in the meantime — under the superseded policy.

The existing doc comment is half right, and the half it gets right is worth keeping. Its reasoning
holds for the **loosening** direction: a write planned under `OnEvent` must not start sweeping merely
because someone set `Always` afterwards, since that snapshot's retention decision was already taken
against a desired set that is now stale. It does not hold for the **tightening** direction, where
stopping deletions that have not yet landed is the entire purpose of the edit.

**Fix.** Order the modes (`Never` < `OnEvent` < `Always`) and apply the **minimum** of the stored and
the current value at execution time. Minimum delivers both halves at once: a loosening cannot
escalate an already-planned write (what the doc comment wanted), and a tightening applies immediately
(what it missed).

The read-failure case needs a deliberate answer rather than a fallback: if the GitTarget cannot be
read at replay time — deleted, or a cold cache — treat it as the most restrictive. A missed sweep is
redone by the next resync; a sweep authorized by a policy the process could not read is not undoable.
This is the same rule the implementation already adopted for an unrecognized enum value, recorded in
[part 1's implementation notes](pr5-gittarget-deletion-safety.md#the-empty-string-is-unset-not-never):
a policy this build cannot resolve does not authorize deletion.

**The event path has the same shape.** Retained live-event windows carry the same metadata and
`pruneModeForBase` reads the stored value
([`pending_writes.go`](../../../internal/git/pending_writes.go)). Severity is lower — those deletes
are observed evidence, not inference — but `Never` means "do not mirror deletes", and a window that
has not pushed has not mirrored anything yet. Fix both; the resync path is the blocker.

**Test.** A replay-seam regression: retain a resync write planned under `Always`, tighten the
GitTarget to `OnEvent`, force a push conflict so the write replays, and assert the replay retains.
Plus a unit test on the min-of-two helper covering the unreadable-target case, which is the branch
most likely to be written the wrong way round.

**Landed.** `PruneMode.MoreRestrictiveOf` in
[`prune_policy.go`](../../../api/v1alpha3/prune_policy.go) orders the modes;
`tightenPendingPruneModes` in [`branch_worker.go`](../../../internal/git/branch_worker.go) applies
it in `rebuildPendingWrites`, immediately before the replay re-plans. Three decisions the design
above did not settle:

- **The tightening mutates the shared `Targets` map**, so one pass covers both deletion paths: the
  sweep reads its mode through `PendingWrite.Target`, the DELETE writer through `pruneModeForBase`,
  and both read the same map. It also means the tightening survives every subsequent push attempt,
  which is right — a policy decrease is not undone by a later increase.
- **A failed read returns an error rather than guessing**, which the design's "use the more
  restrictive" rule did not cover. Guessing the captured mode could apply a revoked deletion;
  guessing the strictest could silently drop a legitimate one, and under `OnEvent` no later resync
  re-derives an event delete. Returning the error leaves the pending writes retained and the push
  cycle retries them, so neither happens. A **deleted** GitTarget is different — that is a definite
  answer, no policy exists, and it replays under `Never`.
- **One read per GitTarget, not per retained write.** A conflicting push can be replaying many
  windows for one busy target.

Held by [`prune_replay_test.go`](../../../internal/git/prune_replay_test.go): both directions, the
deleted and unreadable targets, the both-paths assertion, the read-count bound, and the ordering
table including the unrecognized value.

## R3 — the retention log does not name the GitTarget

`reportRetainedOrphans` logs `retained`, `pruneMode`, `path`, and `scope`
([`resync_flush.go`](../../../internal/git/resync_flush.go)) — no GitTarget namespace or name. Both
user-facing docs promise otherwise: "logs a throttled line naming the target"
([UPGRADING.md](../../UPGRADING.md)) and "the operator logs a line naming the target"
([configuration.md](../../configuration.md)). `path` is not an identifier: two GitTargets in
different namespaces can write the same `spec.path` on different branches of the same repository.

The caller already holds what is needed — `executeResyncPendingWrite` has the resolved target and
already passes its `PruneMode` down the same call.

This is the same defect as the metric's missing labels, so it is tracked once, in
[retention visibility → Step 0](pr5-retention-visibility.md#step-0-make-both-retention-signals-name-the-gittarget),
rather than in two places.

**One disagreement recorded.** The review asks to leave the metric target-unlabelled for cardinality.
Declined: `gittarget_namespace` / `gittarget_name` is the established label pair for this codebase's
per-target counters (`TargetReconcileCompletedTotal` and its neighbours in
[`telemetry/exporter.go`](../../../internal/telemetry/exporter.go)), cardinality is bounded by the
number of GitTargets rather than by resources, and "which target is retaining" is the operational
question the counter exists to answer. What must stay off the metric is per-path, per-scope, or
per-document labels — that is unbounded, and it is what the log line is for.

**Landed.** Both signals in [`resync_flush.go`](../../../internal/git/resync_flush.go) now carry the
target. Two things came out of it that the finding did not ask for:

- **The throttle key was the defect too.** It was the path alone, so two co-resident targets sharing
  a `spec.path` would have had one silently suppress the other's line for ten minutes. It is now the
  GitTarget plus the path.
- **`applyResyncToWorktree` takes the `ResolvedTargetMetadata` instead of four unpacked fields**
  (cluster id, placement, prune mode, and now the identity). That is what made the target reference
  available without an eighth parameter, and it moved the `OrDefault` normalization to a single
  entry point rather than leaving each reader to remember it.

Held by [`retention_report_test.go`](../../../internal/git/retention_report_test.go), including the
legacy target routed through the real apply so the assertion covers the normalization seam and not
just the log call.

## R4 — the docs claim a failure mode the code does not have

[configuration.md](../../configuration.md) and [UPGRADING.md](../../UPGRADING.md) both say that a
source-cluster outage or a narrowed RBAC grant produces a snapshot smaller than reality. The gather
is fail-closed against both, in [`target_watch.go`](../../../internal/watch/target_watch.go):

- a failed watch open or LIST marks the stream `Blocked` and returns an error — no resync is
  enqueued, so no sweep is planned from a partial snapshot;
- a streaming replay enqueues only after the `initial-events-end` bookmark is folded
  (`foldTargetReplayEvent`). A replay cut off mid-stream enqueues nothing at all.

RBAC also cannot narrow *within* a stream: a denied list/watch fails the whole scope rather than
returning a subset, so there is no smaller-but-accepted snapshot to sweep from.

The honest rationale is the one part 1 already leads with — a snapshot that is **complete but
computed against the wrong scope**: a bad rule scope, version skew, a controller that does not
understand a newer scope field. Outages and authorization failures should be described as the case
the controller currently declines to sweep on, with `OnEvent` as defence in depth for the day that
changes: fail-closed is a property of today's gather, not a guarantee the API makes.

The same overstatement is in part 1's own Purpose section ("narrowed by a bad scope, a temporary
outage, or a controller that does not understand a newer scope field"), so the correction is needed
in three places, not the two the review names.

Docs-only, no code dependency — this can land ahead of R1 and R2.

**Landed.** All three corrected, in the shape the review recommended: the scope rationale keeps the
lead, and the outage/RBAC cases are described as what the controller currently declines to sweep on,
with `OnEvent` as defence in depth because failing closed there is a property of today's gather
rather than a guarantee. Two adjacent inaccuracies went with them — the resync is not "periodic"
(it happens when a stream starts or restarts), and the mutability paragraphs now say what a
`prune.mode` edit actually does in each direction, which is only true because of R1.

## What the review confirmed

Recorded because these are the properties most likely to be broken by a later change, and each is now
a claim a reviewer has checked independently: legacy and empty policy values resolve to `OnEvent`; an
unrecognized value fails closed toward retention; suppression happens during planning, so a retained
deletion never enters actions, ordering, stats, or commits; explicit DELETEs stay functional under the
default and `Never` is genuinely archival; the offline analyzer deliberately keeps its full-convergence
reporting; and the e2e retention assertion is guarded by a positive barrier — the `Always` target's
sweep proves a resync ran — rather than by a bare wait.

## Sequencing

All four landed in [#260](https://github.com/ConfigButler/gitops-reverser/pull/260) itself, together
with the [retention visibility](pr5-retention-visibility.md) work: R1 and R2 are policy-*transition*
defects in the feature the PR exists to add, one of them undermines the migration instruction the
release ships with, and both are small.

The [do-not-release window](pr5-gittarget-deletion-safety.md#release-and-rollback) is unchanged by
anything here: `main` holds PR 4's breaking scope rework without PR 5's deletion safety until #260
merges, so these items were on the critical path rather than beside it.

Validated with the standard sequence — `task lint` (0 issues), `task test` (unit coverage 77.8%, at
the baseline, so `.coverage-baseline` is unchanged), and `task test-e2e` (75 passed, 22 skipped, 0
failures), the e2e legs run sequentially.
