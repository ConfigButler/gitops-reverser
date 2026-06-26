# Per-type stream readiness — simple plan

> Status: plan / proposal · Branch: `investigate` · Date: 2026-06-26
>
> Fixes the timing problem documented in
> [watch-replay-watermark-stream-readiness-investigation.md](./watch-replay-watermark-stream-readiness-investigation.md).
> Deliberately simpler than the `Initializing / PartiallyLive / Live` axis sketched
> in [materialization-tail-and-live-readiness-review.md](../../finished/materialization-tail-and-live-readiness-review.md):
> two happy-path states per watched type plus one condition to wait on.
>
> The concrete status objects, fields, printer columns, naming rationale, state
> machine, and the source-code this lets us delete are designed in
> [streaming-readiness-status-machine-design.md](./streaming-readiness-status-machine-design.md).
> Names below follow that doc (`StreamsReady` / `Replaying` / `Streaming` /
> `Blocked`), chosen to fit Kubernetes API conventions.

## 1. Goal

Give every watched type a clear, observable answer to one question: **"is this type's
stream ready, or still replaying?"** Then callers — first of all the e2e suite —
set things up, **wait for `StreamsReady`**, and only then perform the actions whose
live / attributed behaviour they assert. Nothing should expect a write issued in the
same instant a watch is being established to be a live, attributed event; this plan
makes "the streams are ready" a fact you can wait for instead of a guess.

## 2. The states

Per `(GitTarget, type)` — two happy-path states plus an honest dead-end:

- **Replaying** — the `SendInitialEvents` replay is in flight (first replay, or a
  fresh replay after `410 Expired`). A write observed now is folded into the
  **unattributed baseline** — no per-event CREATE, no audit attribution.
- **Streaming** — the watch crossed `k8s.io/initial-events-end`, or resumed from a
  durable cursor with live routing active. A write observed now is a **live,
  per-event, attributable** event.
- **Blocked** — the type cannot be watched (not followable / RBAC) or its watch keeps
  erroring; it can never reach Streaming until the cause clears. Counted honestly, not
  hidden inside Replaying.

```text
  start (no cursor)  ─▶ Replaying ──(initial-events-end)──▶ Streaming
  restart (cursor)   ──────────────────────────────────────▶ Streaming
  Streaming          ──(410 Expired)──▶ Replaying
  Replaying          ──(not followable / watch error)──▶ Blocked ──(recovers)──▶ Replaying
```

**Normal operation does not replay.** A live GitTarget keeps a durable watch cursor
(the last processed resourceVersion, already implemented — `recordTargetWatchCursor` /
`lookupTargetWatchCursor` in
[target_watch.go](../../../internal/watch/target_watch.go)). On restart it *resumes
from the cursor* and goes straight to **Streaming** — it picks up where it left off, no
full replay. Replaying is only the first replay, or recovery after the cursor expires
(`410 Expired`). So in steady state, "wait for `StreamsReady`" is fast.

The controller already knows the transitions today; it just doesn't surface them:

- → Streaming after replay: `target watch replay complete` ([target_watch.go:417](../../../internal/watch/target_watch.go#L417)), then `streamLiveTargetWatchEvents`.
- → Streaming via resume: `recordTargetReconcileCompleted(gitDest, "cursor_resume")` ([target_watch.go:324](../../../internal/watch/target_watch.go#L324)).

## 3. Where the state lives — recommendation

The **source of truth** is the watch manager (it owns the per-`(gitDest, gvr)`
lifecycle). The question is only where to **surface** it.

**Recommendation: per-rule granularity on the (Cluster)WatchRule; a bounded aggregate
on the GitTarget.** Both expose a `StreamsReady` condition.

- **WatchRule / ClusterWatchRule** — `StreamsReady` True ⟺ every type this rule
  resolves is Streaming for its target. This is the object a user authored and reasons
  about, and its `spec.resources` is the natural, explicit enumeration to report on.
- **GitTarget** — one aggregate `StreamsReady` condition plus bounded counts. No
  per-type list.

Why this split (and why not a per-type list on the GitTarget):

- The codebase **already made this call** for the checkpoint roll-up:
  `GitTargetMaterializationStatus` is deliberately "a summary (counts), not a per-type
  list, so it stays bounded regardless of how many types are watched"
  ([gittarget_types.go:128-159](../../../api/v1alpha2/gittarget_types.go#L128-L159)).
  A GitTarget fans in from multiple WatchRules and wildcard expansions, so a per-type
  list there is unbounded by construction.
- The WatchRule is the bounded, user-scoped place. **Caveat:** a wildcard
  ClusterWatchRule can itself resolve to many types, so even there emit the condition +
  counts + a *capped* sample of not-yet-ready types, never a full list.

So the GitTarget answers "is everything I serve ready?"; the WatchRule answers "is *my*
declared set ready?". Both are conditions you can `kubectl wait` on.

## 4. Status surface

Shapes, fields, examples, and printer columns are in the
[design doc](./streaming-readiness-status-machine-design.md) §3. In brief:

- **GitTarget** and **(Cluster)WatchRule** each get a `StreamsReady` condition and a
  bounded `status.streams` roll-up (`summary` / `total` / `ready` / `replaying` /
  `blocked`; the rule adds a capped `pendingSample`).
- `Ready` and `StreamsReady` are independent. **`Ready` must not gate on stream
  readiness** — a large watched set can take a while to replay, and `Ready` keeps
  meaning "admitted and valid," size-independent.
- `phase` is **not** part of this contract and not a printer column (API conventions
  discourage `phase`; automation gates on conditions).

## 5. Wiring

Reuse the path that already drives status — the `Materializer` →
`MaterializationObserver` events that already populate the GitTarget roll-up
([materializer.go](../../../internal/typeset/materializer.go)). Carry a per-type state
(Replaying / Streaming / Blocked) on that event when the watch crosses the watermark,
resumes from a cursor, or errors, and let:

- the GitTarget controller fold it into the aggregate `StreamsReady` condition + counts,
- the WatchRule controller (which already resolves `spec.resources` → GVRs to set
  `Ready`) join those GVRs against the per-type states to set its own `StreamsReady`.

No new transport, no new watch. The data already flows; this names it and surfaces it.

## 6. e2e: every test needs this

The payoff, and **not optional per spec** — the race is general. Any spec that asserts
per-event behaviour, authorship/attribution, or commit ordering can lose it, so **all
e2e tests** adopt the same shape:

1. Create `GitProvider` → `GitTarget` → `(Cluster)WatchRule`.
2. **Wait for `StreamsReady`** before any asserted action:
   - `waitForStreamsReady(name, ns)` → `kubectl wait --for=condition=StreamsReady=true gittarget/<name> --timeout=120s`
     (blocks until ready or the timeout) for "everything this target serves is ready"; or
   - `waitForWatchRuleStreamsReady(name, ns)` for a spec that depends on one rule.
3. Only then create the resources whose live/attributed/ordered behaviour is asserted.

Concretely:

- Add the `waitForStreamsReady` / `waitForWatchRuleStreamsReady` helpers next to the
  existing `waitForGitTargetSynced`
  ([e2e_test.go:235](../../../test/e2e/e2e_test.go#L235)).
- **Audit every spec** and replace the "wait for `Ready`/`Synced` then act" gate with
  "wait for `StreamsReady` then act" wherever the assertion depends on a live per-event
  observation. The two known failures
  ([signing_e2e_test.go:603](../../../test/e2e/signing_e2e_test.go#L603),
  [commit_author_attribution_e2e_test.go:171](../../../test/e2e/commit_author_attribution_e2e_test.go#L171))
  are the first conversions; the comment on `waitForGitTargetSynced` (which already
  warns about the baseline fold) flags the others.
- Retire `Synced` / `waitForGitTargetSynced` (design §6).

## 7. Edge cases

- **Cursor resume:** straight to `Streaming` once the watch is open and routing — no
  `Replaying` flash, no full replay (Section 2). The contract is "post-gate writes are
  live events," not "zero lag."
- **`410 Expired`:** drop the affected type back to `Replaying` (aggregate
  `StreamsReady` → `False`) until the fresh replay re-crosses the watermark.
- **Not followable / watch error:** `Blocked`, surfaced via the `WatchNotPermitted` /
  `WatchError` reason — never silently counted as replaying.
- **Wildcard rules:** condition + counts + capped sample only; never a full list.
- **Multiple WatchRules → one GitTarget:** the GitTarget aggregate is the AND over all
  its tracked types; each WatchRule reports only its own resolved subset.

## 8. Non-goals

- Reworking the baseline resync itself (the baseline is correct; this is about *knowing
  when it ends*).
- Changing `Ready` semantics or gating `Ready` on stream readiness.
- A full per-type status list on any object.
- Per-object (vs per-type) readiness.

## 9. Steps

1. Watch manager: track per-`(gitDest, gvr)` state (Replaying / Streaming / Blocked);
   set Streaming on watermark-crossed and on cursor-resume, Replaying on `410`, Blocked
   on not-followable / watch error.
2. Plumb the state through the existing materialization event to the controllers.
3. GitTarget: derive aggregate `StreamsReady` condition + `streams` counts.
4. (Cluster)WatchRule: derive a `StreamsReady` condition over the rule's resolved types.
5. e2e: add `waitForStreamsReady` / `waitForWatchRuleStreamsReady`; convert **all**
   specs to setup → wait-for-`StreamsReady` → act; start with the two known failures.
6. Regression: the mutationlab `configmap/watch-replay-collapse` row already pins the
   Kubernetes-side fact this plan is designed around.
