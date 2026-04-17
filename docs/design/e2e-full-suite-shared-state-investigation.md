# E2E Full Suite Shared-State Investigation

## Purpose

This note captures an investigation into an intermittent `task test-e2e-full` failure that appeared
after commit `9c7e0bc3c455f99242480e0577ab2776fae16d60` and was observed concretely in CI for image
`ghcr.io/configbutler/gitops-reverser:ci-621200eefaf185dfe1000526b1e0f53dc3a8d93f`.

The goal of this document is not to claim a final fix. The goal is to preserve the best current
explanation, the strongest evidence, and the exact breadcrumbs that should make the next
investigation much faster if the instability returns.

## Status

### 2026-04-17

Current status:

- the CI is green again
- the failure was observed during `task test-e2e-full`
- targeted local runs of `test-e2e-manager` and `test-e2e-audit-redis` were green when run in isolation
- the strongest remaining explanation is a full-suite shared-state interaction, not a simple single-test regression
- a later repro on then-current `main` showed the same class of failure with a narrower stale path:
  - expected:
    - `e2e/configmap-test/v1/configmaps/<ns>/test-configmap.yaml`
  - actual:
    - `e2e/secret-autogen-test/v1/configmaps/<ns>/test-configmap.yaml`
- that later repro strengthened the hypothesis that earlier secret-manager specs were still eligible to react
  to later ConfigMap events
- a mitigation was then tried locally:
  - the manager secret specs were changed to use a secret-only WatchRule template instead of the broader
    `watchrule.tmpl`
  - after that change, `task test-e2e` passed locally, including the manager smoke spec
    `should create Git commit when ConfigMap is added via WatchRule`
- this is encouraging, but it is still not proof that the broader full-suite instability is permanently fixed

Confidence level:

- high confidence in the symptom
- medium confidence in the root cause
- low confidence that the issue is permanently gone just because CI is green now

## Short Version

The failure does match a shared-state hypothesis, but with an important refinement:

- the most likely leak is not `tilt-playground` directly
- the stronger signal is stale manager-suite rules and targets from earlier specs in the same ordered container
- those stale rules appear to keep writing to the same Git repo when a later spec expects only its own `GitTarget` path to be touched

The clearest evidence is this mismatch from CI:

- expected latest touched path:
  - `e2e/configmap-test/v1/configmaps/<ns>/test-configmap.yaml`
- actual latest touched path:
  - `e2e/v1/configmaps/<ns>/test-configmap.yaml`

That `e2e/` path corresponds to an earlier manager test target, not the current `configmap-test` target.

Later, the same failure pattern reappeared with an even more specific stale path:

- expected latest touched path:
  - `e2e/configmap-test/v1/configmaps/<ns>/test-configmap.yaml`
- actual latest touched path:
  - `e2e/secret-autogen-test/v1/configmaps/<ns>/test-configmap.yaml`

That later variant matters because it points directly at the earlier secret auto-generation manager spec,
which at the time was still using the broad `watchrule.tmpl` fixture that included `configmaps` as well
as `secrets`.

## Triggering Commit

The investigation started from:

- `9c7e0bc3c455f99242480e0577ab2776fae16d60`
- commit subject:
  - `fix: WatchRule leaking resources from other namespaces in live event stream`

That commit changed three relevant areas:

1. `RuleStore.GetMatchingRules()` became namespace-aware for namespaced `WatchRule`s
2. the audit consumer began passing namespace context into the matcher
3. watch-manager startup began bootstrapping the in-memory `RuleStore` from existing rules before initial reconcile

Relevant files:

- [internal/rulestore/store.go](/workspaces/gitops-reverser/internal/rulestore/store.go)
- [internal/queue/redis_audit_consumer.go](/workspaces/gitops-reverser/internal/queue/redis_audit_consumer.go)
- [internal/watch/bootstrap.go](/workspaces/gitops-reverser/internal/watch/bootstrap.go)
- [internal/watch/manager.go](/workspaces/gitops-reverser/internal/watch/manager.go)

## Concrete CI Failure

The observed failure in CI happened in:

- image / SHA context:
  - `621200eefaf185dfe1000526b1e0f53dc3a8d93f`
- failing spec:
  - `Manager Manager should create Git commit when ConfigMap is added via WatchRule`

The test timed out while waiting for the latest commit to match the expected touched path set.

Expected:

```text
e2e/configmap-test/v1/configmaps/1776422075-test-manager/test-configmap.yaml
```

Actual:

```text
e2e/v1/configmaps/1776422075-test-manager/test-configmap.yaml
```

This is the single most important observation from the failure.

## Why The Path Mismatch Matters

The current spec under test creates a `GitTarget` with path:

- `e2e/configmap-test`

So if the system were behaving correctly, the resulting commit should have touched only:

- `e2e/configmap-test/v1/configmaps/<ns>/<name>.yaml`

Instead, the latest commit touched:

- `e2e/v1/configmaps/<ns>/<name>.yaml`

That shorter path is associated with an earlier manager spec that uses a `GitTarget` path of:

- `e2e`

That means the commit that "won the race" at the end of the assertion window was produced by a
different target than the one created by the current spec.

## Strong Evidence From Controller Logs

The controller logs from the same CI run show that immediately before and during the failing
ConfigMap test, the controller was still performing unrelated writes in the same repository:

- deleting:
  - `e2e/v1/secrets/.../test-secret-autogen.sops.yaml`
  - `e2e/v1/secrets/.../sops-age-key-autogen.sops.yaml`
  - `e2e/secret-encryption-test/.../sops-age-key-autogen.sops.yaml`
  - `e2e/secret-autogen-test/.../sops-age-key-autogen.sops.yaml`
- then creating the ConfigMap
- then creating that same ConfigMap multiple times via repeated per-event writes

This matters because the failing spec expects a clean repo outcome for one target path, but the log
shows active writes from several other previously created paths in the same branch and repository.

The logs strongly suggest:

- earlier specs had not become fully irrelevant by the time this later spec ran
- multiple `GitTarget`s and/or `WatchRule`s were still producing writes to the same branch
- the final latest commit was allowed to come from the wrong target path

## Refined Hypothesis

The refined working hypothesis is:

1. earlier manager specs create additional `WatchRule` and `GitTarget` pairs targeting the same repo and branch
2. some of those rules remain active long enough to overlap with later specs
3. after `9c7e0bc`, startup/bootstrap/reconcile behavior made that overlap easier to observe
4. when the later ConfigMap test creates one object, more than one target path can still react
5. the e2e assertion checks the latest commit only, so if a stale target writes after the intended target, the spec fails

This is a stronger explanation than "the new path assertion was wrong" because the logs show the
system really was making unrelated writes in the same repo during the assertion window.

## What Seems Less Likely Now

The first-pass theory was that `tilt-playground` was the primary cause because it only appears in
the full suite and not in the isolated manager run.

That is still possible as background pressure, but it is no longer the strongest explanation.

Why it got downgraded:

- the concrete path mismatch points directly at another manager spec's `GitTarget` path, not the playground path
- the observed writes in the controller logs are to manager-test paths, not to `tilt-playground` paths
- the failure reads like cross-spec interference inside the ordered manager container more than global cluster pollution from the playground

Refined stance:

- `tilt-playground` may still contribute to "full-suite-only" conditions
- but the highest-value place to inspect is manager-spec lifecycle and target/rule cleanup behavior

## Likely Mechanisms

The following mechanisms could all produce the observed symptom.

### 1. Rule lifetime overlaps between ordered specs

Earlier manager specs create:

- `GitProvider`
- `GitTarget`
- `WatchRule`

Those are later cleaned up, but there may be a window where:

- the next spec has already started
- the old rule is still present, still compiled, or still in a live event path

This would explain why a later ConfigMap causes writes under an older target path.

### 2. In-memory `RuleStore` / reconcile state lags deletion

`9c7e0bc` introduced `bootstrapRuleStore()` at startup and relies on reconcile behavior in:

- [internal/watch/bootstrap.go](/workspaces/gitops-reverser/internal/watch/bootstrap.go)
- [internal/watch/manager.go](/workspaces/gitops-reverser/internal/watch/manager.go)

Even if the API objects are deleted correctly, there may be a timing window where:

- the old compiled rule is still present
- the watch manager still treats it as active
- or a flush/reconcile cycle emits work for an already-stale target

### 3. One repo, one branch, many paths, one latest-commit assertion

The tests deliberately reuse:

- the same repository
- the same branch

across multiple manager specs.

That is reasonable, but it means a spec that asserts "the latest commit touched only X" is fragile if
other legitimate or stale writers can still commit to the same branch.

This does not prove the assertion is wrong. It proves the assertion is sensitive to any overlap.

### 4. Snapshot/reconcile writes and live writes interleave

The logs include both:

- per-event commits
- atomic reconcile commits

If a target transitions through reconcile or buffered-event flush at the same time another test is
waiting on a live-event commit, the latest commit may belong to the wrong mechanism or the wrong target.

## Why Isolated Runs Were Green

During investigation:

- `task test-e2e-manager` passed when run in isolation
- `task test-e2e-audit-redis` passed when run in isolation

This is consistent with a shared-state explanation because isolated runs remove:

- earlier manager spec buildup from the full suite
- later spec interference
- branch noise from unrelated targets in the same repo

So "isolated green, full-suite flaky" is not contradictory here. It supports the idea that the
system is sensitive to test ordering and lifecycle overlap.

## Things The CI Log Proves

The CI log proves all of the following:

1. the ConfigMap event was seen
2. the controller did write ConfigMap commits
3. the repo was not idle during the assertion window
4. the latest commit path was not the one owned by the current spec
5. the failure was not "nothing happened"; it was "the wrong thing won the race"

That last point is especially important.

This was not a dead system.
This was an over-active or cross-wired system.

## Things The Investigation Did Not Prove

The investigation did not fully prove:

- the exact deletion or reconciliation code path that preserved the stale rule
- whether the stale writer came from API object lifetime, in-memory cache lifetime, or buffered event lifetime
- whether the same issue also explains the concurrent signing-suite failure

The signing failure may be related because it also reasons about latest commits on a shared branch,
but this document keeps the manager-path failure as the primary anchor because the path mismatch there
is unusually direct and easy to interpret.

## Best Current Root-Cause Statement

Best current statement:

> A full-suite shared-state interaction allowed a stale or overlapping manager test target to keep
> writing to the same repository branch after a later manager spec had started. The later spec then
> failed because its "latest commit touches only my expected path" assertion observed a commit from
> the older target path (`e2e/...`) instead of the current target path (`e2e/configmap-test/...`).

This statement is intentionally a little cautious. It matches the evidence well without pretending
to have proven the last missing internal transition.

## What Was Tried After This Note

After the later repro on `main`, one concrete mitigation was tried in the manager e2e suite:

- add a secret-only WatchRule template for the secret-manager specs
- stop using the broad `test/e2e/templates/watchrule.tmpl` for:
  - `should commit encrypted Secret manifests when WatchRule includes secrets`
  - `should generate missing SOPS age secret when age.recipients.generateWhenMissing is enabled`

Why that change was chosen:

- `watchrule.tmpl` matches both `configmaps` and `secrets`
- that meant an earlier secret-oriented manager spec could still legally react to a later ConfigMap
  event if cleanup lagged
- the later failure path
  `e2e/secret-autogen-test/v1/configmaps/<ns>/test-configmap.yaml`
  is exactly what that overlap would produce

Observed result of the mitigation:

- local validation passed with:
  - `task fmt`
  - `task generate`
  - `task manifests`
  - `task vet`
  - `task lint`
  - `task test`
  - `task test-e2e`
- the manager smoke spec
  `should create Git commit when ConfigMap is added via WatchRule`
  passed after the change

Current interpretation:

- this mitigation is a strong fit for the newer `secret-autogen-test` variant
- it reduces one clear source of cross-spec overlap in the manager suite
- it should be treated as a tested mitigation, not yet a fully proven final root-cause closure for every
  possible `task test-e2e-full` flake

## Files Most Worth Re-Inspecting If It Returns

Primary files:

- [internal/watch/manager.go](/workspaces/gitops-reverser/internal/watch/manager.go)
- [internal/watch/bootstrap.go](/workspaces/gitops-reverser/internal/watch/bootstrap.go)
- [internal/controller/watchrule_controller.go](/workspaces/gitops-reverser/internal/controller/watchrule_controller.go)
- [internal/controller/gittarget_controller.go](/workspaces/gitops-reverser/internal/controller/gittarget_controller.go)
- [internal/reconcile/git_target_event_stream.go](/workspaces/gitops-reverser/internal/reconcile/git_target_event_stream.go)
- [internal/git/branch_worker.go](/workspaces/gitops-reverser/internal/git/branch_worker.go)
- [test/e2e/e2e_test.go](/workspaces/gitops-reverser/test/e2e/e2e_test.go)
- [test/e2e/repo_assertions_test.go](/workspaces/gitops-reverser/test/e2e/repo_assertions_test.go)

Specific areas to question:

- when a `WatchRule` is deleted, how quickly does it disappear from the active compiled store?
- when a `GitTarget` is deleted, how quickly is its stream fully unregistered?
- can buffered events still flush after the spec has moved on?
- can snapshot/reconcile logic emit writes for a target that is already logically done from the test's point of view?

## What To Capture Next Time

If this resurfaces, the following evidence will be the most valuable.

### 1. Latest commit inventory, not just latest commit path

Capture:

```bash
git -C <checkout> log --oneline -n 20
git -C <checkout> diff-tree --no-commit-id --name-only -r HEAD
git -C <checkout> show --stat --summary HEAD
```

This helps distinguish:

- wrong-path latest commit
- multiple rapid commits from different targets
- reconcile commit vs per-event commit

### 2. Active rules and targets at failure time

Capture:

```bash
kubectl get watchrules --all-namespaces
kubectl get gittargets --all-namespaces
kubectl get gitproviders --all-namespaces
```

Especially look for old manager-spec resources still present in the test namespace.

### 3. Controller logs around the exact failure window

Search for:

- `Starting git commit and push`
- `Starting write request operation`
- `Created commit`
- `Created atomic commit`
- `Reconciliation completed, transitioning to LIVE_PROCESSING`
- `Finished processing buffered events`

### 4. Path ownership map

It will help to quickly map every manager spec to its intended target path:

- `e2e`
- `e2e/secret-encryption-test`
- `e2e/secret-autogen-test`
- `e2e/configmap-test`
- `e2e/delete-test`
- `e2e/icecream-test`

If a later spec fails and the latest commit touches one of the earlier paths, that is a very strong sign of cross-spec leakage.

## Practical Interpretation For Now

Since CI is green again, the right stance is:

- do not panic
- do not forget this
- treat this as a likely instability pattern that may reappear under timing pressure

If it comes back, the current best working assumption should be:

- not "the controller missed the event"
- not "the repo assertion is obviously wrong"
- but "another target probably wrote to the same repo after the expected target"

That framing should save time.

## Summary

The best current explanation is a full-suite shared-state race involving earlier manager-suite
targets, not a simple single-test path bug.

What the evidence says most clearly:

- the expected commit did happen
- but it did not stay the latest commit
- an older target path appears to have remained active long enough to overwrite the test's assumption

If the flake returns, start by proving which target wrote the final commit and whether that target
should still have been active at all.
