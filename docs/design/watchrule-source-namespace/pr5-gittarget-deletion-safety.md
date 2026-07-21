# PR 5 — GitTarget deletion safety

> Phase 5 of [source-namespace addressing](README.md). It deliberately contains no source-namespace
> API change. It is implemented in the PR immediately **after**
> [PR 4](pr4-cluster-scope-only.md), and **no release may be cut between the two merges** — the first
> release containing PR 4's breaking API changes also contains this one.
>
> **Status: implemented.** Depends on [PR 1](pr1-namespace-scoped-resync.md) and
> [PR 2](pr2-stream-scope-collapse.md), which identify the resync sweep boundary this PR controls.
> Implementation notes that were decided during the build — and are not restatements of this
> design — are recorded in [Implementation notes](#implementation-notes) at the end.

## Purpose

GitOps Reverser has two distinct deletion paths:

- An explicit source DELETE event deletes one resolved managed document.
- A resync compares a desired snapshot with Git and drops documents that are absent from that
  snapshot (mark-and-sweep).

The first is source-cluster evidence. The second is an inference, and is unsafe when the snapshot
is narrowed by a bad scope, a temporary outage, or a controller that does not understand a newer
scope field. This PR lets each GitTarget decide which paths may remove Git documents.

## API

Add an extensible policy object rather than a bare enum, so a later volume guard can be added
without changing a scalar field into an object:

~~~yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: acme
  namespace: tenant-acme
spec:
  providerRef: { name: acme-git }
  branch: main
  path: tenants/acme
  prune:
    mode: onEvent
~~~

`GitTarget.spec.prune.mode` is an enum with an effective default of `onEvent`:

| Mode | Explicit source DELETE event | Resync mark-and-sweep | Intended use |
|---|---:|---:|---|
| `never` | suppressed | suppressed | archive/tombstone mirror |
| `onEvent` | applied | suppressed | safe default; mirror observed deletes but never infer them |
| `always` | applied | applied | full convergence, including cleanup of stale Git documents |

`onEvent` means a DELETE event, not every watch event. `always` enables both deletion paths.

The CRD default is useful for newly written objects, but it is not the compatibility mechanism.
The controller must use `EffectivePruneMode()` and treat an omitted stored value as `onEvent`; old
GitTargets must become safe without first being edited.

## Why `onEvent` is the default

A scope collapse must stop updates outside the resulting scope, but it must not erase their existing
Git documents. With `onEvent`, a resync emits no managed-drop actions, so the documents remain until
an explicit source DELETE is observed or the target owner deliberately selects `always`.

This is intentionally a behavior change from today's implicit sweep behavior. A target that needs
full desired-state convergence opts in with `prune.mode: always`.

This PR does **not** attempt to limit a large cascade of genuine DELETE events. A future PR may add
for example `maxDeletesPerCommit` to this object if experience shows that control is needed. An
absolute count is not a complete whole-folder safeguard for a small target, so it is better omitted
than presented as one in this first safety release.

## Implementation

1. **API and effective default.** Add `PrunePolicy` and `PruneMode` (`never`, `onEvent`, `always`) to
   `GitTargetSpec`, with schema enum/default and an `EffectivePruneMode()` helper. Regenerate
   deepcopy code and CRDs.
2. **Suppress sweep actions at the planner.** Thread the effective mode into the resync planning
   policy. `BuildScopedPlan` must not emit `PlanDropOrphan` when the mode is `never` or `onEvent`.
   Do not filter the action at apply time: a suppressed drop must not enter the plan, plan hash, or
   commit path.
3. **Gate explicit deletes.** The steady-state delete writer applies delete-document actions only
   for `onEvent` and `always`; `never` leaves the managed document unchanged.
4. **Surface configured retention.** A sweep suppressed by policy is healthy, not a failed
   reconciliation. Emit a rate-limited informational log and make the mode visible in API
   documentation; do not add a failure condition merely because a stale document remains.

The zero value of the internal planner policy must be explicit in every caller. Production resync
code passes the target's effective mode; dry-run and unit-test callers choose deliberately whether
they want to model `always` or `onEvent`.

## Tests

- A legacy GitTarget that omits `prune` has effective mode `onEvent`.
- `onEvent` retains a document when a resync desired set narrows to empty; the generated plan has no
  managed-drop action.
- `onEvent` still mirrors one explicit DELETE event.
- `never` suppresses both paths.
- `always` reproduces today's mark-and-sweep behavior byte-for-byte.
- Full and namespace-scoped resyncs both honor the mode; no alternate sweep path bypasses it.

## Release and rollback

This PR merges immediately after [PR 4](pr4-cluster-scope-only.md) and the two are released together;
`main` is in a do-not-release window between the merges. There is therefore **no released version
containing PR 4 but not this PR**, and no PR-5 rollback floor to fall back to.

Do not claim safety for a rollback past that release once PR-4 manifests have been applied: an older
controller neither understands `prune` nor the rule-item source-namespace field, so it resolves a
narrower desired set *and* sweeps it. Remove or narrow the affected WatchRules first.

Inside the shipping release, `onEvent` is still what makes a scope mistake non-destructive — it is
just no longer a separately released prerequisite.

## Done when

- `prune.mode` defaults effectively to `onEvent` for both new and existing GitTargets.
- Explicit deletes and inferred sweep drops are independently controlled exactly as in the table.
- `task lint`, `task test`, and `task test-e2e` pass, closing the do-not-release window PR 4 opened.

## Reporting what was kept

This page covers the decision — which deletions happen. Making a retention *observable* is planned
separately in [retention visibility](pr5-retention-visibility.md), landing in the same pull request:
a suppressed drop produces no action, no commit, and no stat, so today it is discoverable only from a
throttled log line. That plan proposes a `status.retention` roll-up, and is deliberately still bound
by the rule below — an observation, never a condition.

## Implementation notes

Decisions taken while building this that the design above did not settle. They are recorded because
each one has a failure mode that is invisible if the reasoning is lost.

### The empty string is unset, not `never`

Read literally, `PruneMode("")` answers false to both `AppliesEventDeletes` and `SweepsOrphans` —
which is `never`, the mode that stops mirroring deletes. But an omitted field means `onEvent`. Every
internal carrier of the value (the writer's `ResolvedTargetMetadata`, the per-base lookup, the
write batch) therefore normalizes through `PruneMode.OrDefault` before asking either predicate. An
*unrecognized* value is treated differently and deliberately: it is left alone and retains on both
paths, because a policy this build cannot understand must not authorize deletion.

### The planner models only the inferred path

`manifestanalyzer.Policy` gains a `SweepMode`, not a `PruneMode`: the planner never sees an explicit
DELETE event, so `never` and `onEvent` are the same instruction to it. Keeping the API enum out of
`manifestanalyzer` also preserves that package's freedom from Kubernetes API types, the same rule
`PlacementPolicy` already follows. The translation happens once, in `resyncPlanPolicy`.

`SweepMode`'s zero value **retains**, and every production caller sets it explicitly anyway. The
offline folder scan (`FolderScanPlanPolicy`) deliberately converges: it has no GitTarget to read a
policy from, it writes nothing, and a report that silently omitted a folder's orphans would render a
stale folder identically to a converged one.

### A suppressed drop is counted, not just skipped

`Plan.RetainedOrphans` exists because a suppressed drop leaves no other trace — no action, no
commit, no `ResyncStats` entry. Without the count, nothing downstream could distinguish "this mirror
is converged" from "this mirror is deliberately keeping stale documents". It feeds a throttled
default-verbosity log (one per target folder per 10 minutes, since retention is a steady state and
resyncs fire per type and namespace) and `gitopsreverser_prune_retained_documents_total`.

### Retention is not an empty scope

A retaining policy could have been implemented by passing a scope predicate that matches nothing,
and every drop-suppression test would still pass. It is not, because the two answer different
questions: `inScope` is "is this document any of my business", `Sweep` is "may I delete the ones
that are". Collapsing them would erase the distinction between a document this plan never owned and
one it owned, considered, and kept — which is exactly the fact the operator needs.

### Drift heal is gated too, by construction

The heal resync (`ResyncRequest.Heal`) runs through the same planner, so under `onEvent` the
operator no longer removes a stray managed document a human added to the folder by hand. This
follows from the design rather than extending it, but it is the consequence most likely to surprise:
"the reverser stopped cleaning up junk I put in the folder" is the same mechanism as "the reverser
did not delete my tenant's manifests when the scope collapsed". `always` restores both.
