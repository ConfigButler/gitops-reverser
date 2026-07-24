# PR 5, part 2 — make retention visible on the GitTarget

> The reporting half of [PR 5](pr5-gittarget-deletion-safety.md), landing in the **same pull request**
> ([#260](https://github.com/ConfigButler/gitops-reverser/pull/260)). Part 1 decides what to keep;
> this decides how an operator finds out. No change to the write path.
>
> **Status: implemented.** The [open questions](#open-questions-for-review) are answered at the end
> of that section, and [what this costs the PR](#what-this-costs-the-open-pr) was accepted rather
> than deferred: the field ships in the same release as `spec.prune.mode`.
>
> A review of #260 found the *log* half of the same defect — it did not name the GitTarget either,
> though two user-facing docs said it did. It is folded into
> [Step 0](#step-0-make-both-retention-signals-name-the-gittarget) rather than tracked separately;
> the rest of that review is in [PR 5 review follow-ups](pr5-review-followups.md).

## The problem

Part 1 made a suppressed sweep the **default**, and a suppressed sweep is by construction invisible.
It produces no plan action, no commit, and no `ResyncStats` entry — that is deliberate, because a
retention must be indistinguishable from the event never arriving. The consequence is that an
operator comparing their cluster to the mirror has nothing to read that distinguishes:

- **converged** — the folder matches the cluster; and
- **deliberately retaining** — the folder holds documents a converged mirror would not, because the
  target's policy kept them.

Those two states look identical from `kubectl get gittarget`, from `git log`, and from the folder
itself. That is a bad property for a safety feature whose entire premise is *"we kept something on
purpose."* A safety mechanism the operator cannot observe is one they cannot trust — and, more
practically, one they cannot audit before flipping a target to `Always`.

### What part 1 shipped, and why it is not enough

The state this plan started from. Every "what it cannot" below is now closed — the log line and the
metric name the target, and the count reaches status — but the table is kept as written because it
is the argument for the shape, not a description of the code.

| Signal | Where | What it answers | What it cannot |
|---|---|---|---|
| Throttled log line | [`resync_flush.go`](../../../internal/git/resync_flush.go), `reportRetainedOrphans` | which folder and scope, how many | **which GitTarget** — it logs `path`, and two targets in different namespaces can share one; needs log access; throttled to one per folder per 10 min; nothing to query |
| `gitopsreverser_prune_retained_documents_total` | [`telemetry/exporter.go`](../../../internal/telemetry/exporter.go) | is *anything* retaining, under which mode | **which GitTarget** — it is labelled by `prune_mode` only |
| `Plan.RetainedOrphans` | [`manifestanalyzer/plan.go`](../../../internal/manifestanalyzer/plan.go) | the count, per resync | never leaves the writer; not carried on `ResyncStats` |

So today the only way to answer *"is target X retaining anything?"* is to grep controller logs and
match the `path` field — which is not even an identifier, since two GitTargets in different
namespaces can write the same `spec.path` on different branches of one repository. For a change that
altered the default deletion behaviour of every existing GitTarget, that is too weak.

Neither signal naming the target is the cheapest defect here, and it is worth fixing whatever else is
decided — see [Step 0](#step-0-make-both-retention-signals-name-the-gittarget). It is also a
correctness issue in the docs, not only a gap: both
[configuration.md](../../configuration.md) and [UPGRADING.md](../../UPGRADING.md) already promise a
log line "naming the target".

## What this is deliberately NOT

**Not a condition.** [Part 1](pr5-gittarget-deletion-safety.md#implementation) is explicit: a sweep
suppressed by policy is healthy, not a failed reconciliation, and no failure condition may be raised
merely because a stale document remains. That still holds. Raising any `False` condition for the
configured behaviour would train operators to ignore the conditions that mean the mirror is genuinely
broken — strictly worse than the invisibility being fixed.

The distinction this plan rests on: **a condition asserts health; an observation reports a fact.**
`status.streams` is already the precedent for the second kind — a bounded roll-up that no condition
reads. Retention belongs in that category.

**Not a per-document list.** The count stays bounded however many documents are retained, for the
same reason `status.streams` is counts and never a per-type list. An operator who needs to know
*which* documents reads the log line or scans the folder; status answers "how many, and under what
policy".

## Proposed API

```yaml
status:
  retention:
    mode: OnEvent          # the EFFECTIVE mode, resolved — answers "why" without a second lookup
    retainedDocuments: 3   # documents a converged mirror would not hold
    observedTime: "2026-07-21T13:20:00Z"
```

`mode` is duplicated from `spec.prune.mode` rather than left for the reader to correlate, because
the interesting value is the **effective** one: a legacy GitTarget has no `spec.prune` at all, so
`spec` alone cannot explain why documents are being kept. This is the one place the omitted-field
default becomes visible without reading the source.

`retainedDocuments: 0` and an **absent** `retention` block mean different things, and both are
needed:

- absent — no resync has reported yet (the target has not replayed, or predates the field);
- `0` — a resync ran and found nothing to retain. This is the "converged" signal, and it is why zero
  must be recorded as actively as a non-zero count.

## How the number gets there

The projection is **pull-based**, which the codebase already establishes: the GitTarget controller
reads data-plane state from the watch `Manager` on each reconcile
([`gittarget_controller.go`](../../../internal/controller/gittarget_controller.go)):

```go
streams        = r.EventRouter.WatchManager.StreamSummaryForGitTarget(gitDest)
gitPath        = r.EventRouter.WatchManager.GitPathAcceptanceForGitTarget(gitDest)
renderFidelity = r.EventRouter.WatchManager.RenderFidelityForGitTarget(gitDest)
```

Retention becomes a fourth reader beside them. The full path:

1. **Carry the count out of the writer.** `ResyncStats` gains `Retained int`, set from
   `Plan.RetainedOrphans` in `applyResyncPlan`. It rides the existing `ResyncResult` reply channel —
   no new transport.
2. **Record it per scope.** [`drainScopedResync`](../../../internal/watch/event_router.go) already
   receives the result, the `targetWatchKey` (GVR + namespace) and the render-fidelity epoch, and
   already calls `MarkTargetGitPathAccepted` and `MarkTargetRenderFidelityScopeClean` there. A
   `MarkTargetRetention(gitDest, key, epoch, stats.Retained)` sits beside them.
3. **Roll up per target.** The `Manager` keeps per-(target, scope) counts and sums them;
   `RetentionForGitTarget(gitDest)` returns the roll-up.
4. **Project onto status.** The controller writes `status.retention` next to `status.streams`.

### The eviction problem, and why it is already solved

A per-scope map raises the obvious question: when a type stops being watched, or a namespace leaves
the target's admitted set, its retained count must **disappear** — otherwise the roll-up only ever
grows and becomes a lie. Pruning that map correctly against a changing watch plan is exactly the kind
of bookkeeping that rots.

It does not need writing. [`RenderFidelityGate`](../../../internal/watch/render_fidelity_gate.go)
solves the identical problem for an identical key, and it does so with an **epoch** rather than with
eviction: records carry the watch epoch they were produced under, the epoch bumps when the watch plan
is reinstalled, and records from an older epoch are ignored — "a stale cancellation tail is ignored by
the gate and cannot reopen a failed target." Retention should reuse `RenderFidelityEpochForGitTarget`
verbatim. A scope that vanishes from the plan takes its count with it at the next epoch, with no
per-key deletion logic to get wrong.

This is the most important reuse decision in the plan: writing a second, independent scope-lifecycle
tracker next to the one that already exists is how the two drift apart.

### The staleness property, stated rather than hidden

Because the projection is pull-based, `status.retention` is only as fresh as the last GitTarget
reconcile. A data-plane fact does not by itself enqueue the GitTarget — the same property that makes
`GitPathAccepted` lag, and the documented cause of a past CI flake in that seam. A retention that
begins just after a reconcile can take until the next periodic requeue (up to ~10 min) to appear.

That is acceptable for an observation and unacceptable for a gate, which is a further reason it must
not become a condition. It should be **documented in the field's own godoc**, so a reader does not
mistake a stale zero for a live one — and it is why `observedTime` is in the shape rather than being
inferred from `lastReconcileTime`.

Optionally, a `0 → n` transition could enqueue the target so the first appearance is prompt while
later updates ride the requeue. That is a refinement, not a requirement, and should be decided
against the flake history rather than added reflexively.

## Step 0: make both retention signals name the GitTarget

Small, independent of the status work, and worth doing first. Both halves live in
`reportRetainedOrphans`, which already receives everything except the target reference —
`executeResyncPendingWrite` holds the resolved target and passes its `PruneMode` down the same call,
so this is one extra parameter, not new plumbing.

**The metric:**

```go
telemetry.PruneRetainedDocumentsTotal.Add(ctx, int64(retained), metric.WithAttributes(
    attribute.String("prune_mode", string(mode)),
    attribute.String("gittarget_namespace", ns),   // new
    attribute.String("gittarget_name", name),      // new
))
```

Cardinality is bounded by the number of GitTargets, not by resources, and the label names follow the
convention `TargetReconcileCompletedTotal` already sets — `gittarget_namespace` / `gittarget_name`
rather than the reserved `namespace` / `name`, because a pod scrape with `honor_labels=false`
overwrites a metric's `namespace` attribute with the scraping pod's own, silently breaking any
per-target selector.

**The log line** gains the same identity as a `gitTarget` field. It logs `retained`, `pruneMode`,
`path`, and `scope` today, and `path` is the one thing an operator cannot map back to an object. The
[#260 review](pr5-review-followups.md#r3--the-retention-log-does-not-name-the-gittarget) asked for
the log field while asking to leave the metric unlabelled for cardinality; that half is declined
here, for the reason above — per-target labels are already this codebase's convention and are what
makes the counter actionable. What stays off the metric is per-path, per-scope, or per-document
labels, which are unbounded and are exactly what the log line is for.

Together these make "which target is retaining" answerable from metrics *and* from logs, even before
status lands — and they make the existing sentence in
[configuration.md](../../configuration.md) and [UPGRADING.md](../../UPGRADING.md) true.

## What this costs the open PR

Worth weighing explicitly, since this is going into #260 rather than following it:

- **The suite must be re-run.** #260 is currently green end to end (65 e2e specs, 0 failures). This
  adds an API field, a controller status write, a new `Manager` surface, and e2e coverage — so
  `task lint`, `task test`, and `task test-e2e` all run again from scratch.
- **The do-not-release window stays open longer.** `main` currently holds PR 4's breaking scope
  rework *without* the deletion safety that makes it non-destructive, and
  [release-please #256](https://github.com/ConfigButler/gitops-reverser/pull/256) is open and would
  ship exactly that state. Every day #260 stays open is a day that window is open.
- **It grows a reviewed PR after review.** #260 has already been read once. Additive status work is
  low-risk, but it is new surface arriving after the fact.

Against that: the field is API surface, and adding `status.retention` in the same release as
`spec.prune.mode` avoids a second status-shape change one release later. If it does not land here it
should land before the release, not after — a released `prune.mode` whose retention cannot be
observed is the version operators will form their first impression on.

**Decided: it landed here.** The suite was re-run in full and is green (75 e2e specs passed, 22
skipped, 0 failures; unit coverage 77.8%, unchanged against the baseline).

## Tests

- A resync that retains N documents surfaces `retainedDocuments: N` and the effective `mode`, for a
  target declaring no `spec.prune` — so the roll-up reports the *effective* mode, not the absent
  stored one.
- A later resync that retains nothing drives it back to `0`. This is the likeliest regression, since
  "record zero as actively as non-zero" is easy to lose.
- A scope that leaves the watch plan drops its contribution at the next epoch, without recreating the
  target.
- Counts from a stale epoch are ignored — the property inherited from `RenderFidelityGate`.
- Unscoped and namespace-scoped resyncs both contribute, mirroring part 1's
  `TestPrune_RetentionIsIdenticalUnderEveryResyncShape`.
- e2e: the `Always` target reports `0` while the co-resident default target reports non-zero for the
  same seeded orphan — reusing part 1's barrier structure, since the `Always` sweep is what proves a
  resync ran at all.

All of the above landed in [`retention_rollup_test.go`](../../../internal/watch/retention_rollup_test.go),
[`gittarget_status_test.go`](../../../internal/controller/gittarget_status_test.go) (the
absent-versus-zero projection), and the e2e spec *"reports retained documents, and convergence, on
GitTarget status"*. Two were added while building:

- **The enqueue is on a change only** — first report and every transition enqueue, an unchanged
  report does not. Both halves are asserted, because the second is what stops a deliberately
  retaining target from re-reconciling on every resync of every scope forever.
- **The e2e reads an absent block as a failure, not as zero.** `retainedDocumentsOf` fails when the
  jsonpath is empty rather than defaulting to `0`; without that, the convergence assertion would
  pass before any resync had reported.

## Open questions for review

1. **Should `Never` also report its suppressed explicit deletes?** Under `Never` the DELETE gate
   simply returns and counts nothing, so `retainedDocuments` would cover only the sweep half. A
   `Never` target could therefore report `0` while actively declining to mirror deletes — arguably
   the more surprising retention of the two. Options: leave it sweep-only and say so in the godoc;
   add a second counter; or make the field mean "documents kept by policy" across both paths, which
   is more honest but needs counting in the event writer too.
2. **Is `0` vs absent worth the extra state?** It costs a pointer field and the "record zero actively"
   requirement. A plain count where zero and unknown collapse is simpler, but then status can never
   say "converged" — which is half the value.
3. **Enqueue on `0 → n`, or accept the requeue lag?** Given the flake history in this projection
   class, accepting the lag and documenting it is the conservative default.
4. **Step 0 only, if the window matters more?** The metric labels are a few lines and close the
   operational question; the status field is the audit question. Shipping only Step 0 in #260 and the
   status field immediately after is a legitimate split if closing the release window is the priority.

### How they were answered

1. **Sweep-only, and the godoc says so.** `retainedDocuments` counts the inferred path alone, so a
   `Never` target can report zero while still declining to mirror deletes. Counting the two together
   would need the event writer to count as well, and — more importantly — would merge a number
   derived from a *snapshot* with one derived from *events*, which respond to different failures. The
   field's `kubectl explain` text carries the caveat rather than leaving it to be discovered.
2. **Yes — a pointer field, with zero recorded as actively as any other count.** Absent and zero are
   the two things an operator most needs told apart, and a plain count cannot say "converged".
   `TestRetentionRollup_ZeroIsRecordedAsActivelyAsAnyOtherCount` exists because that is the half most
   easily lost in a later refactor.
3. **Enqueue on change — the opposite of the conservative default this plan proposed.** Waiting out
   the steady requeue (5 minutes) for the *first appearance* of a retention is too long for a signal
   an operator consults before flipping a target to `Always`, and it would leave the e2e assertion
   with nothing better than a long sleep. The flake history argues against a projection that does
   *not* enqueue, which is the opposite failure. It enqueues on a change of the count or the mode,
   never on an unchanged report, so a steadily retaining target does not re-reconcile forever.
4. **Not split — everything shipped in #260.** See the status note at the top.

## Done when

- `status.retention` reports the effective mode and a bounded retained count for both new and legacy
  GitTargets, and returns to `0` when a resync finds nothing to retain.
- The roll-up is epoch-based and shares the scope lifecycle `RenderFidelityGate` already owns, rather
  than maintaining a second one.
- Both retention signals — the metric and the throttled log line — identify the GitTarget.
- No condition changes state because of retention.
- `task lint`, `task test`, and `task test-e2e` pass.

## Implementation notes

Decisions taken while building this that the plan above did not settle.

### The mode travels with the count, not with the spec

The plan said `mode` is duplicated onto status so a legacy GitTarget's behaviour is explainable
without a second lookup. Building it surfaced a second reason, and it changed where the value comes
from: the controller could have read `EffectivePruneMode()` off the object it is already reconciling,
but then a target patched to `Always` would publish the new mode beside a count the *old* one
produced, until the next resync reported. So `ResyncStats` carries `PruneMode` alongside `Retained`,
and the pair is written and read together. The mode on status is the mode that produced the number.

### Retention is not the render-fidelity gate, only its epoch

The plan's most important reuse decision was to take the epoch from `RenderFidelityGate` rather than
write a second scope-lifecycle tracker, and that held: `MarkTargetRetention` takes the epoch
`enqueueReplayResync` already computes, a newer epoch replaces the whole per-scope map, and an older
one is dropped. What it does **not** do is live inside the gate. The gate decides whether a target
may be written to; retention decides nothing at all. Sharing the epoch is reuse; sharing the
structure would have put an observation inside a gate, which is exactly the confusion this plan
exists to avoid.

### `Retained` is a plan view, and has to be

Every other `ResyncStats` field is counted from what the apply *did* — deliberately, because a
sensitive resource is `PlanSkip` in the plan while `applyUpsert` really does rewrite it, so
plan-derived stats would report a real commit as skipped. `Retained` is the exception and cannot be
anything else: it counts drops the planner did not emit, so there is no action to observe. The
comment at the assignment says so, because "count from the apply, not the plan" is otherwise the
rule in that function.
