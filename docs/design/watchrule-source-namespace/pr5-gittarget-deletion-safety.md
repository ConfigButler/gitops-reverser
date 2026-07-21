# PR 5 — GitTarget deletion safety

> Phase 5 of [source-namespace addressing](README.md). This is an independently releasable
> **rollback-floor** release: it deliberately contains no source-namespace API change. It must be
> released, and all controller instances upgraded to it, before [PR 6](pr6-cluster-scope-only.md)
> introduces the breaking WatchRule and ClusterWatchRule API changes.
>
> **Status: proposal, not started.** Depends on [PR 1](pr1-namespace-scoped-resync.md) and
> [PR 2](pr2-stream-scope-collapse.md), which identify the resync sweep boundary this PR controls.

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

Release this PR by itself. Upgrade the CRD and controller, then wait until every controller pod runs
this version. That version is the rollback floor for PR 6: a rollback from PR 6 to PR 5 still
understands `prune.mode: onEvent`, so it cannot turn a newer namespace scope into a sweep.

Do not claim safety for a rollback past PR 5 after PR-6 manifests have been applied: an older
controller neither understands `prune` nor the new rule-item namespace field.

## Done when

- `prune.mode` defaults effectively to `onEvent` for both new and existing GitTargets.
- Explicit deletes and inferred sweep drops are independently controlled exactly as in the table.
- The release has passed `task lint`, `task test`, and `task test-e2e` and has become the required
  rollback floor before PR 6 starts its manifest migration.
