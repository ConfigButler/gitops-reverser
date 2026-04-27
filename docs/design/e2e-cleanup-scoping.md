---
title: e2e cleanup scoping — let the run finish; only Ctrl-C aborts
status: implemented
date: 2026-04-27
relates-to: docs/design/e2e-watchrule-cross-spec-interference.md
---

# e2e cleanup scoping — let the run finish; only Ctrl-C aborts

## Why

The e2e suite originally had one global flag, `e2ePreserveResources`, that decided whether `cleanupWatchRule`, `cleanupGitTarget`, namespace deletions, etc. should actually run or be skipped. Three independent triggers all flipped the same flag:

1. A `ReportAfterEach` that flipped it on the first spec failure.
2. The registered Gomega fail handler and the SIGINT/SIGTERM handler — both fed the same global preservation bit, so a normal assertion failure was effectively treated like a suite-wide abort.
3. The playground bootstrap, which flipped it deliberately to keep the playground live for the developer after the suite finishes.

Once the flag was on, `skipCleanupBecauseResourcesArePreserved` returned `true` for every cleanup call until the process exited. That meant a single failure poisoned the rest of the run: every spec that ran afterward leaked its WatchRules, GitTargets, ConfigMaps, and namespaces. Those leftovers then interfered with subsequent specs through the controller's own informer (most visibly, the signing batch spec — see [e2e-watchrule-cross-spec-interference.md](e2e-watchrule-cross-spec-interference.md)).

## Vision and design choice

The guiding principle is: **each spec should be self-contained, and the suite should finish whenever it can.** A failure in one spec must not prevent the rest from running, and certainly must not prevent them from starting in a clean state. Only an operator hitting Ctrl-C — i.e. an explicit "stop and let me look" — should freeze the cluster mid-run.

Given that, "auto-preserve on spec failure" is in tension with the vision: it gives one developer a debuggable post-mortem while making every other spec in the run unreliable. We removed it. Failed specs still emit diagnostics (events, controller logs) via `dumpFailureDiagnostics` in `AfterEach`, and the run continues.

For deliberate post-mortem investigation, the developer has two clean options:
- Re-run with `-ginkgo.focus="<failing spec>"` and watch it locally.
- Hit Ctrl-C while the suite is running — that flips the suite-wide preservation flag immediately and freezes the cluster.

Neither path requires the suite to compromise its own reliability for the next twenty specs.

## The two-rule model

There are exactly two reasons cleanup ever skips. Anything else cleans up.

| Rule | When set | What it keeps |
|---|---|---|
| `suiteWidePreserve` (atomic.Bool) | Ctrl-C, SIGTERM, BeforeSuite panic | Skip every cleanup until process exit. |
| `preservedNamespaces` (set of strings) | Explicit `preserveNamespace(ns)` call (today: only the playground) | Skip cleanup for resources in any namespace in this set, regardless of spec outcome. |

The cleanup helper:

```go
func skipCleanupBecauseResourcesArePreserved(scope, namespace string) bool {
    if suiteWidePreserve.Load() {
        return logCleanupSkip(scope, e2ePreservationSummary())
    }
    if isPreservedNamespace(namespace) {
        return logCleanupSkip(scope, "preserved namespace "+namespace)
    }
    return false
}
```

That's the entire decision tree. No `CurrentSpecReport()` check, no `ReportAfterEach` wiring, no fail-handler indirection. Spec failures are reported as failures, diagnostics are dumped, the suite continues.

Concrete consequences:

1. **No cascade is even possible.** There is no global flag a single failure can flip.
2. **`AfterAll` works correctly in `Ordered` blocks.** It cleans the namespace at end-of-block, no edge case where one passing spec after a failed spec causes the namespace to either survive or get nuked unexpectedly.
3. **The signing batch spec stops being a secondary failure.** It only fails when *it* is genuinely broken — the signal we actually want.
4. **Ctrl-C still preserves everything**, exactly as a developer would expect.
5. **The playground still works.** It calls `preserveNamespace("tilt-playground")` after a successful bootstrap and the namespace survives the run.

## How the playground exception works in this model

The playground describe block is intentionally different from every other spec in the suite. It runs once, succeeds, and *wants to leave its namespace alive* so the developer can:

- Open Tilt against the cluster and see the live `playground` GitProvider/GitTarget/WatchRule reconciling.
- `kubectl apply` configmaps into `tilt-playground` and watch them appear in the playground Gitea repo.
- Re-run `task test-e2e` later without re-bootstrapping the playground — the second run sees the existing namespace and skips re-creation in `BeforeAll`.

In the original global-flag world, the playground got this behavior by flipping the master switch and preserving everything in the suite. That is *why* the cascade was wide enough to reach the signing namespace: the playground's "preserve me" intent was conflated with "preserve everything for debugging."

In the new model, the playground says exactly what it needs:

```go
// In tilt_playground_e2e_test.go:
preserveNamespace(playgroundNamespace)
```

`preserveNamespace("tilt-playground")` adds that namespace to `preservedNamespaces`. Cleanup helpers consulted with `ns="tilt-playground"` see the match and skip. Cleanup helpers consulted with `ns="1777236647-test-signing"` (or any other test namespace) do *not* see a match, and clean up normally.

This exposed one piece of plumbing the original code didn't have: cleanup helpers must know the namespace they're cleaning. The good news is they almost always already did — `cleanupWatchRule(name, namespace)` and `cleanupGitTarget(name, namespace)` take the namespace as an argument. Cluster-scoped helpers pass `""` and only the suite-wide branch can ever apply to them.

A subtle but important point: the playground namespace marking happens *after* the playground bootstrap step succeeds. If the bootstrap step itself fails, `preserveNamespace` is never reached, the spec fails like any other, and `AfterAll` (or the next run) cleans the half-built playground. There is no path by which a failing playground spec can poison the rest of the suite.

## Implementation as landed

### `test/e2e/suite_state.go`

- `suiteWidePreserve atomic.Bool` (renamed from `e2ePreserveResources`).
- `preservedNamespaces` mutex-guarded `map[string]struct{}` plus `preserveNamespace(ns)` / `isPreservedNamespace(ns)` helpers.
- `skipCleanupBecauseResourcesArePreserved(scope, namespace)` checks suite-wide first, then namespace-scoped, then returns false. No third clause.
- `markSuiteWidePreservation` is the only setter for the suite-wide flag, called from the SIGINT/SIGTERM handler and from a `recover()` deferred at the top of `BeforeSuite` for panics.

### `test/e2e/e2e_suite_test.go`

- The `ReportAfterEach` that flipped the global flag on any spec failure is gone.
- The custom Gomega fail handler (`failAndPreserveResources`) is gone; the suite uses plain `RegisterFailHandler(Fail)`.
- The signal handler still calls `markSuiteWidePreservation`.
- A `defer recover()` at the top of `BeforeSuite` calls `markSuiteWidePreservation("BeforeSuite failed")` if setup code panics, then re-panics. This is a safety net for genuine bugs in setup; it does not catch Gomega failures in `BeforeSuite`, which abort the suite anyway.

### `test/e2e/tilt_playground_e2e_test.go`

- Replaces `markE2EResourcesForPreservation("playground bootstrap complete")` with `preserveNamespace(playgroundNamespace)`.

### Cleanup helpers in `test/e2e/helpers.go`

- All five cleanup helpers (`cleanupGitTarget`, `cleanupWatchRule`, `cleanupClusterWatchRule`, `cleanupNamespace`, `cleanupNamespacedResource`, `cleanupClusterResource`) now pass the namespace through. Cluster-scoped helpers pass `""`.

### Spec-level call sites

- Direct `kubectl ... delete gitprovider/secret` calls in the signing, audit-redis, and manager IceCream specs were replaced with `cleanupNamespacedResource(namespace, kind, name)` so they go through the same preservation path. This is a consistency cleanup, not a behavioral change in the new model — the only effect is that `preserveNamespace` and the suite-wide flag now apply to those resources too, which matches expectations.

### Tests

- `test/e2e/suite_state_test.go` covers the dedup behavior of `markSuiteWidePreservation` and the namespace-scope behavior of `preserveNamespace`.

## Debugging workflow

Without auto-preserve-on-failure, a developer who sees a spec fail and wants to poke at the cluster has two clean paths:

- **In-the-moment.** Hit Ctrl-C the moment a failure scrolls past. The signal handler flips `suiteWidePreserve`, every subsequent cleanup call no-ops, and the cluster is frozen in whatever state it was in. Diagnostics for the specific failed spec were already dumped by `dumpFailureDiagnostics` in `AfterEach`, so you have both the logs and the live cluster.
- **Re-run focused.** `go test -ginkgo.focus="<failing spec>" ./test/e2e/...` (or the equivalent task target) — the failure is reproducible because the spec ran cleanly the first time, and you can hit Ctrl-C if you want the live state.

CI runs always pick up event dumps and controller logs from `dumpFailureDiagnostics`; nothing in this design weakens CI's ability to attribute a failure.

## What this does not solve

This change does not fix the underlying ghost-WatchRule bug in the controller (described in [e2e-watchrule-cross-spec-interference.md](e2e-watchrule-cross-spec-interference.md) §"Open root causes" #1). The manager create/delete specs would still occasionally see ghost commits from prior specs that ran inside the same describe block, because those specs share the same `managerRepo` and namespace. Those need either:

- the controller-side fix (drain event-router state on WatchRule deletion), or
- a unique-namespace-per-spec restructuring of the Manager describe block.

The cleanup-scoping change is orthogonal to both and strictly lowers the suite's failure-amplification factor.

## Migration risk

Low. The change strictly *increases* the amount of cleanup that runs after a failure. A spec that today passes after another spec failed (and so previously inherited the prior spec's leftovers) now starts from a clean namespace. The only way that breaks something is if a spec relied on a leftover from a prior failed spec to pass — which would be a latent test bug we'd want to find anyway.

For the playground, the new namespace-scoped path is strictly narrower than the original global flag, so it cannot accidentally preserve more state than before.
