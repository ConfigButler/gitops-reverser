# E2E: assert on metrics, not operator logs

**Status:** IMPLEMENTED 2026-06-12 (see "Implementation outline" — all steps landed).
**Date:** 2026-06-12

## The ask

Several e2e specs prove "something happened" (most importantly: *a commit was
created*) by scraping the operator's pod logs for a substring. That is brittle:
log strings are not a contract, a tail window can miss the line, and — the part
that bites under Ginkgo parallelism — a substring match cannot tell *this* spec's
commit from a commit driven by another spec sharing the controller.

The proposal: assert on the existing counter
[`gitopsreverser_commits_total`](../../internal/telemetry/exporter.go) instead,
adding labels so a spec can isolate *its own* commits when many run at once.

This document does three things: (1) catalogs every log-scrape in the e2e suite
and sorts the real candidates from the false positives, (2) confirms the
metric-assertion plumbing already exists and is in use, and (3) recommends a
concrete label set — which is **not** quite the one the ask guessed at, and the
reason is the interesting part.

## Method

Scanned every `*.go` under [test/e2e/](../../test/e2e/) for log reads
(`controllerLogs(...)`, `kubectl logs`, `showControllerLogs(...)`) and for
`ContainSubstring`/`MatchRegexp` assertions, then traced each to whether it gates
a spec's pass/fail (a real assertion) or merely dumps context for debugging.

## Findings — the catalog

The grep is noisier than the reality. Most `ContainSubstring` hits that *look*
like log scraping are actually reading **git log output**, a **CRD status
condition**, or **`kubectl` output** — those are legitimate and should stay.
Only **three** sites assert on operator pod logs.

### A. True operator-log assertions — the conversion candidates

| # | Site | Reads | Asserts | Purpose |
|---|------|-------|---------|---------|
| A1 | [crd_lifecycle_e2e_test.go:284-296](../../test/e2e/crd_lifecycle_e2e_test.go#L284-L296) | `controllerLogs(2000)` | `"git commit created"` **OR** `"git resync commit created"` | a commit happened for the CRD instance |
| A2 | [watchrule_configmap_secret_e2e_test.go:514-522](../../test/e2e/watchrule_configmap_secret_e2e_test.go#L514-L522) | `controllerLogs(500)` | `"git commit"` | a commit happened |
| A3 | [controller_basics_e2e_test.go:117-130](../../test/e2e/controller_basics_e2e_test.go#L117-L130) | `kubectl logs` | `"Serving metrics server"` | the metrics server started |

A1 and A2 are the same idea — *did a commit get created* — and are the heart of
the ask. A3 is a one-time readiness check and is already shadowed by a metric
query on the very next lines (`up{job='gitops-reverser'}`), so it is nearly
redundant today.

Note A1's awkwardness: it has to `Or(...)` **two** different log strings because a
commit can come from the per-event path (`"git commit created"`) or the backfill
resync path (`"git resync commit created"`). This is exactly the kind of
implementation-coupling a metric removes — see "Why the metric is also *more
correct*" below.

### B. Diagnostic-only log usage — leave as-is

These dump logs for human debugging; they never gate a result. Keep them.

- [gitprovider_validation_e2e_test.go](../../test/e2e/gitprovider_validation_e2e_test.go) — seven `showControllerLogs(...)` context dumps.
- [e2e_test.go:254](../../test/e2e/e2e_test.go#L254) `showControllerLogs(...)` and [helpers.go:405-415](../../test/e2e/helpers.go#L405-L415) — last-N-lines dump on failure.
- [helpers.go:307-328](../../test/e2e/helpers.go#L307-L328) `controllerLogs(...)` — the shared reader the A-sites call.

### C. Looks like a log scrape, but isn't — leave as-is

These read real state (git history, CRD status, API discovery), not logs. They
are the *robust* assertions and should not change.

| Site | Actually reads |
|------|----------------|
| [crd_lifecycle_e2e_test.go:609](../../test/e2e/crd_lifecycle_e2e_test.go#L609), [:677](../../test/e2e/crd_lifecycle_e2e_test.go#L677) | `git log --oneline` → `"DELETE"` (commit message) |
| [watchrule_configmap_secret_e2e_test.go:803](../../test/e2e/watchrule_configmap_secret_e2e_test.go#L803) | `git log --oneline` → `"DELETE"` |
| [signing_e2e_test.go:482](../../test/e2e/signing_e2e_test.go#L482) | `git log --format=%s` → commit subjects |
| [watchrule_configmap_secret_e2e_test.go:181-182](../../test/e2e/watchrule_configmap_secret_e2e_test.go#L181-L182) | CRD `.status.conditions[...].message` (`"watching N resource type(s)"`) |
| [aggregated_apiserver_e2e_test.go:129-130](../../test/e2e/aggregated_apiserver_e2e_test.go#L129-L130) | `kubectl api-resources` output |

## What happens often

The dominant "did it work?" pattern in the suite is **pull the repo and stat the
expected file** (`pullLatestRepoState` + `os.Stat`/content checks) — robust, and
the overwhelming majority of specs already do this. The log scrape for "a commit
happened" is the *weaker variant*, and it appears in only two spots (A1, A2).

So the honest scope of this work is small and sharp: **two commit-existence log
checks** to convert, plus **one optional** readiness check (A3) to retire. It is
not a sprawling refactor. But the *value* is outsized, because A1/A2 are commit
proofs and commits are the product — making them parallel-safe and
implementation-independent is worth more than the line count suggests.

## The plumbing already exists

There is nothing to build for the test harness. The metric-assertion path is
established and in production use in the suite:

- A shared Prometheus scrapes the controller every **5s** via a ServiceMonitor
  (job `gitops-reverser`): [servicemonitor.yaml](../../test/e2e/setup/manifests/prometheus/servicemonitor.yaml),
  [instance.yaml](../../test/e2e/setup/manifests/prometheus/instance.yaml).
- Helpers already exist:
  [`setupPrometheusClient()`](../../test/e2e/helpers.go#L58),
  [`queryPrometheus(query)`](../../test/e2e/helpers.go#L82),
  [`waitForMetric(...)`](../../test/e2e/helpers.go#L105) /
  [`waitForMetricWithTimeout(...)`](../../test/e2e/helpers.go#L110).
- Precedent: [restart_snapshot_e2e_test.go:185-205](../../test/e2e/restart_snapshot_e2e_test.go#L185)
  already gates on `gitopsreverser_target_reconcile_completed_total` and
  `gitopsreverser_branch_worker_queue_depth`; `controller_basics` and
  `aggregated_apiserver` already query `up{job='gitops-reverser'}`.

A commit-existence check is the same shape as the restart-snapshot reconcile
check that already ships.

## The gap in `gitopsreverser_commits_total`

The metric exists and is incremented at
[branch_worker.go:1226-1228](../../internal/git/branch_worker.go#L1226), but it is
recorded **with no attributes**:

```go
telemetry.CommitsTotal.Add(w.ctx, int64(commitsCreated))
```

With no labels, a PromQL query sees one global series. Two specs committing
concurrently are indistinguishable — exactly the parallelism problem the ask
calls out. The fix is to add labels at this one call site.

### Why the metric is also *more correct*

Both commit paths funnel through this single counter:

- per-event / atomic writes → `executePendingWrite` →
  [`"git commit created"`](../../internal/git/commit_executor.go#L160)
- backfill resync writes → `executeResyncPendingWrite` →
  [`"git resync commit created"`](../../internal/git/resync_flush.go#L226)

…and **both** return their `created` count up into
[`recordPendingWritesMetrics`](../../internal/git/branch_worker.go#L1217), which
adds them to `CommitsTotal`. So the single metric already unifies what A1 has to
express as an `Or(...)` of two log strings. Converting A1 is *strictly simpler*
than the log check it replaces, not just more robust.

## Recommended labels — and why not `gittarget_*`

The ask guessed at GitTarget labels ("we already create a new GitTarget every
time"). That instinct is right about *isolation* but wrong about *which
dimension*, and the doc says so plainly because the ask invited it ("perhaps I'm
wrong").

The commit is produced by a **`BranchWorker`**, whose identity is
`{GitProviderNamespace, GitProviderRef, Branch}`
([branch_worker.go:83](../../internal/git/branch_worker.go#L83)). The worker does
**not** know a single GitTarget at commit time — a branch worker is keyed by
provider+branch, and *multiple* GitTargets that share a provider+branch are served
by the *same* worker, their writes coalesced into one commit batch. A
`gittarget_name` label would therefore be either unavailable or a lie (which of
the N GitTargets sharing the batch?). Threading per-GitTarget attribution down
into the commit batch is possible but invasive and semantically muddy, and is not
warranted for this.

**Recommendation: label `gitopsreverser_commits_total` with
`{provider_namespace, provider_name, branch}`** — the worker's own identity. This:

- is available at the call site with zero plumbing (`w.GitProviderNamespace`,
  `w.GitProviderRef`, `w.Branch`);
- matches the existing
  [`gitopsreverser_branch_worker_queue_depth`](../../internal/git/branch_worker.go#L387)
  labels exactly, so the two branch-worker metrics read consistently;
- **isolates concurrent specs anyway** — because each suite mints its
  GitProvider in a unique namespace
  ([`testNamespaceFor`](../../test/e2e/namespace.go#L43) →
  `"<GinkgoRandomSeed>-test-<suite>"`), so `provider_namespace` is already
  per-suite-unique. A query filtered on the spec's own namespace sees only that
  spec's commits.

In short: you get the GitTarget-level isolation you wanted, via the provider's
namespace, without inventing a label the commit layer can't honestly populate.

> **Reserved-label caveat (load-bearing).** Use the prefixed keys
> `provider_namespace` / `provider_name`, **never** bare `namespace` / `name`. A
> Prometheus pod scrape with `honor_labels=false` overwrites a metric's
> `namespace` attribute with the *scraping target's* namespace, so a
> per-provider `namespace` selector would silently match nothing. This is the
> same reasoning already documented for `TargetReconcileCompletedTotal` in
> [exporter.go](../../internal/telemetry/exporter.go) — follow it.

> **Note on existing label-name drift.** The codebase already has two styles:
> `provider_namespace`/`provider_name`/`branch` (branch-worker metrics) and
> `git_target_namespace`/`git_target` (the audit route metric, see
> [docs/interpreting-metrics.md](../interpreting-metrics.md)). For
> `commits_total`, stay with the **branch-worker** style because that is the
> layer doing the recording.

> **Scrape-lag caveat.** A counter only reflects in PromQL after the next scrape
> (≤5s here). `Eventually(...)` with the existing 2s poll / 30s timeout absorbs
> this; do not assert on the metric synchronously right after the git push.

## How a converted assertion looks

A1/A2 become a one-liner against the spec's own provider namespace. With the
GitProvider in `testNs`, the substring scrape is replaced by a single call to a
helper added next to the other metric helpers in
[helpers.go](../../test/e2e/helpers.go):

```go
// in the spec — replaces the controllerLogs(...) substring scrape
waitForCommitInNamespace(testNs)

// the helper (helpers.go): or vector(0) keeps the query scalar before any series
// exists; the Go-side `> 0` matches the restart-snapshot idiom. It also lazily
// sets up the Prometheus client so a suite need not wire setupPrometheusClient().
func waitForCommitInNamespace(providerNamespace string) {
    ensurePrometheusClient()
    query := fmt.Sprintf(
        `sum(gitopsreverser_commits_total{provider_namespace=%q}) or vector(0)`,
        providerNamespace,
    )
    waitForMetricWithTimeout(query, func(v float64) bool { return v > 0 },
        fmt.Sprintf("a commit was created for GitProvider namespace %q", providerNamespace),
        45*time.Second)
}
```

The metric gate is a *progress* signal; each converted spec keeps its existing
authoritative check (the repo pull + `os.Stat` of the specific committed file)
immediately after, so a counter already-positive from an earlier spec in the same
suite cannot produce a false pass. (A `branch` filter can be added when a spec
commits to several branches under one provider.)

## Implementation outline (for the follow-up, non-docs change)

1. **Metric (one site).** At
   [`recordPendingWritesMetrics`](../../internal/git/branch_worker.go#L1217), add
   `metric.WithAttributes(provider_namespace, provider_name, branch)` to the
   `CommitsTotal.Add`, mirroring `recordQueueDepth`. The caller early-returns when
   `commitsCreated == 0`, so the series first appears on the first commit (the
   e2e assertion uses `sum(...) > 0`, which handles that cleanly — no need to
   pre-instantiate at zero). Unit tests assert the attribute set *and* the
   per-namespace isolation via the manual reader
   ([`InitTestExporter`](../../internal/telemetry/exporter.go#L221) +
   `CollectInt64Sum`).
2. **Docs.** Add a `Labels` column row for `commits_total` in
   [docs/interpreting-metrics.md](../interpreting-metrics.md) (it is currently
   listed label-less at line 326). Labels are a public observability contract —
   document them.
3. **E2E helper.** Add `waitForCommitInNamespace(...)` to
   [helpers.go](../../test/e2e/helpers.go).
4. **Convert A1** ([crd_lifecycle](../../test/e2e/crd_lifecycle_e2e_test.go#L284))
   and **A2** ([watchrule](../../test/e2e/watchrule_configmap_secret_e2e_test.go#L514))
   to the helper. A1 loses its two-string `Or(...)` for free.
5. **A3 (optional):** drop the `"Serving metrics server"` log check in
   [controller_basics](../../test/e2e/controller_basics_e2e_test.go#L117); the
   `up{job='gitops-reverser'}` query immediately below already proves the metrics
   server is up and scraped.
6. **Validate** per [AGENTS.md](../../AGENTS.md): `task lint` → `task test` →
   `task test-e2e` (e2e needs Docker; run sequentially). This step touches Go,
   so it is *not* under the docs-only exception.

## Scope / non-goals

- **In scope (later):** label `commits_total`; convert A1, A2; optionally retire
  A3; document the labels.
- **Out of scope:** the git-log `"DELETE"` checks, `kubectl`/status checks, and
  the diagnostic log dumps (sections B and C) — they read real state and stay.
- **Explicitly not doing:** per-GitTarget commit attribution. The branch-worker
  layer can't populate it honestly, and `provider_namespace` already gives the
  per-spec isolation the ask wants.
- **This document changes no code.** It is the plan for a small, separately
  validated follow-up.
