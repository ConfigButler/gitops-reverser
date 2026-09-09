# Proving the metric surface: a plan for what e2e should assert

> **design**: a plan for unbuilt work. Nothing here binds until scheduled.
> Date: 2026-09-09. Written against `9341d668` on `feat/pipeline-join-and-commitrequest-outcomes`.
> Index: [`../INDEX.md`](../INDEX.md)
> Related: [`metrics-observability-plan.md`](metrics-observability-plan.md) (what the surface is),
> [`../interpreting-metrics.md`](../interpreting-metrics.md) (the instrument inventory)
>
> **The finding that shrinks this plan: run identity already exists.** `testNamespaceFor` is
> `fmt.Sprintf("%d-test-%s", GinkgoRandomSeed(), suite)`, so every e2e namespace carries the run's
> seed, and every instrument labelled `gittarget_namespace` or `provider_namespace` is already
> unique per run. Two consecutive local runs produced `1788938080-test-commit-request` and
> `1788938742-test-commit-request`. A per-run scrape label was on this plan; for most of the
> surface it is not needed, and `waitForCommitInNamespace` already relies on the property.
>
> What remains is narrower and sharper: **identity-free instruments cannot be run-scoped at all**,
> and one assertion added on this branch is circular.

## Why this page exists

Two defects on one branch, with the same shape.

`FinalizeWindowMismatch` was declared, switched on by the controller, surfaced as a condition
reason with its own message, and counted by a new metric. Nothing produced it. It had been dead
since the eager-attach refactor, and every layer a reader inspected looked correct.

The e2e check added to catch that class has the same blind spot. It skips when the counter reports
zero observations, so deleting every recording call leaves it green. **It reads the metric to decide
whether to check the metric.**

Both are cases of a signal that looks wired and is not. That is the failure this page is about.

## Two rules

**A metric assertion must not take its trigger from the metric under test.** The trigger has to come
from outside: whether the specs ran, whether the object reached its terminal state, whether the
commit landed. Anything read from the instrument itself makes the check vacuous exactly when the
instrument is broken, which is the only time it matters.

**No enum value may be reachable only through an injected fake.** Every value in a bounded label set
needs one test that produces it from the real producer. The controller-side tests for
`window_mismatch` passed for months against a fake finalizer supplying an outcome the worker never
emitted. A fake proves the mapping; only the producer proves the value exists.

## What the four layers can prove

Each layer answers a question the one below it cannot. Listed with what it is blind to, because that
is what decides where a given assertion belongs.

| Layer | Question | Blind to |
|---|---|---|
| **L1 contract** | is every enum value produced by real code? | whether it reaches Prometheus |
| **L2 wiring** | given activity, does the series appear? | whether the numbers are right |
| **L3 run** | what happened across this run? | which spec did it |
| **L4 fleet** | how does this run compare to previous ones? | anything within a run |

`window_mismatch` passed L2 and L3 for months while failing L1: the counter existed, incremented on
other outcomes, and reported plausible totals.

## What run identity we already have, and what we do not

`testNamespaceFor` seeds the namespace, so a run is identifiable wherever an object namespace
reaches a label:

- `gittarget_namespace` on `watch_events_total`, `git_documents_total`, `watch_recovery_total`,
  `git_resync_failures_total`, `watch_plan_passes_total`, the three placement counters,
  `watch_types`, `watch_streams_open`, and the new `git_branch_targets`.
- `provider_namespace` on `git_commits_total`, `git_pushes_total`, `git_push_retries_total`,
  `git_push_duration_seconds`, `git_queue_drops_total`, `git_commit_failures_total`,
  `git_queue_depth`. The GitProvider is created in the same seeded namespace as its GitTarget.

So the whole pipeline is run-scopable today by selecting on a namespace prefix, with two caveats
worth stating rather than discovering:

- **`TESTNAMESPACE` overrides it.** `testNamespaceFor` returns that value verbatim when set, which
  collapses run identity by design. Anything relying on the seed must tolerate its absence.
- **Fixed-namespace suites are exempt.** A suite that installs into documented namespaces rather
  than seeded ones has no run axis, and gains one only from the scrape side.

**`commit_requests_total{outcome}` has neither.** It carries no namespace, no GitTarget, no cluster:
that was the deliberate choice to keep it bounded when a CommitRequest is created once per save. The
consequence, which needs stating plainly rather than argued away, is that **it cannot be scoped to a
run, a tenant, or a spec, and no mapping gauge can recover that** because aggregation already
discarded the key.

## Two problems that look like one

They need different fixes, and a run label solves only the first.

| Problem | Symptom | Fix |
|---|---|---|
| Cluster reuse across runs | yesterday's failure fails today's clean run; yesterday's success hides today's missing instrumentation | scope the observation to this run |
| Counter resets within a run | the restart-reconcile spec restarts the controller and counters return to zero | `increase()`, which is reset-aware |

`max_over_time` is wrong for both. It preserves a maximum rather than reconstructing increments, so
across a restart it undercounts by the whole post-restart portion. The existing audit invariant uses
it, so this is a pre-existing flaw that the CommitRequest report copied.

`increase()` is correct for both and extrapolates, so counts print as `12.3` rather than `12`. For a
gate on zero that is exact. For a report it is approximate and should say so.

## The plan

### 1. Break the circularity in the CommitRequest outcome check

**Problem.** `reportCommitRequestOutcomes` skips when the counter is zero, so missing instrumentation
is indistinguishable from a shard that never ran the specs.

**Change.** Take the trigger from Ginkgo. `ReportAfterSuite` receives the aggregated report from all
parallel processes, so "did any `commit-request`-labelled spec pass?" is answerable without querying
Prometheus. When they ran, the metric is **required**; when they did not, the check is genuinely not
applicable.

**Acceptance.** Deleting the `recordCommitRequestOutcome` calls fails the suite. Running a shard
whose filter excludes `commit-request` still passes. Both asserted by running them.

### 2. Scope the aggregates to this run

**Problem.** The `[2h]` window reaches into previous runs on a reused cluster.

**Change.** `SynchronizedBeforeSuite` already returns `[]byte` from process 1 to every other process
and currently returns `nil`. Stamp the run's start there and query
`increase(metric[<elapsed>s])`. No infrastructure change, correct on a reused cluster, reset-aware.

**Acceptance.** A run immediately following a failed run passes. Deliberately, this is the assertion
that a stale-history bug would have failed.

### 3. `source_cluster` on the mapping

**Change.** Add it to `git_branch_targets` from the GitTarget's own source-cluster identity.
`spec.clusterProviderRef` is CEL-immutable, so GitTarget to cluster is a stable one-to-one and the
cluster axis becomes derivable for every GitTarget-labelled instrument through one join, rather than
a label added to nine.

**The config-plane sentinel is a separate concept from "unset".** `configPlaneClusterID` is
deliberately the empty string so it cannot collide with a ClusterProvider name, but an empty label
value is indistinguishable from a missing label in PromQL. The label needs an explicit rendering for
the config plane.

**Not on the git-side instruments.** A branch is shared by GitTargets that may mirror different
clusters, so a single `source_cluster` there would have to be invented, which is the same objection
that keeps `gittarget_*` off `git_commits_total`.

**Acceptance.** A remote-source GitTarget and a local one publish distinguishable values, and the
config-plane rendering is selectable.

### 4. Record the two rules where they will be read

At the instruments and at the assertions, not on a page. A rule in `docs/design/` is a rule nobody
reads at the moment they are about to break it.

### 5. Deferred: an `e2e_run` scrape-target label

**Only worth building for identity-free instruments**, which today means `commit_requests_total`
alone. The e2e `ServiceMonitor` is ours, so an `endpoints[].relabelings` entry stamping a run id
labels everything from that target without the application knowing.

**It does not belong in gitops-reverser.** A run id compiled into the operator is a test concern
shipped into the product's metric surface and live in every production deployment. The generic need
behind it is real and already solved by convention: Prometheus supplies `job` and `instance` per
target, and `external_labels` is the standard way to stamp cluster or environment identity. The
generic answer is scrape-side configuration, which is a second reason not to build it into the
operator.

**Reconsider when** a second identity-free instrument appears, or when a fixed-namespace suite needs
run attribution.

## What this plan does not do

- **It does not add `source_cluster` to `commit_requests_total`.** Recorded because the reasoning
  was wrong the first time: that label would be bounded by cluster and outcome, not by save, so it
  is a capacity and product judgment rather than a cardinality error. Keeping the counter fleet-wide
  is a decision to give up tenant attribution on saves, and that limitation belongs in the
  instrument's doc comment.
- **It does not pursue spec-level attribution for identity-free aggregates.** With parallel specs
  sharing one controller, exact attribution to a single spec needs a distinguishing label, a serial
  phase, or correlated logs. Run-level is achievable; spec-level is not, and claiming otherwise
  would be the same kind of overreach as the circular skip.
- **It does not move `CommitRequest` into data planes.** It is a command against a managed
  GitTarget, and the current placement keeps authorization, submitter attribution and status
  ownership together. Remote submission becomes compelling for a "save with only tenant-cluster
  credentials" workflow, and that needs its own design: how a remote request selects an authorized
  management-plane target, how the submitting identity survives the boundary, and how the result
  gets back. Watching the CRD remotely answers none of the three.

## Open questions

- **Does the audit invariant want the same `increase()` correction?** It has the identical
  `max_over_time` flaw. Fixing it is out of scope here and is the same change.
- **Should the seed-in-namespace property be a documented contract?** Several helpers rely on it and
  `TESTNAMESPACE` silently defeats it. Either it is load-bearing and should be stated, or the
  helpers should not depend on it.
- **What is the right config-plane label value?** `local`, `config-plane`, or the operator's own
  cluster id if one is ever introduced. It is a public label value once shipped.
