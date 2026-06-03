# E2E Speedup Plan

Make `task test-e2e*` faster for the three audiences that pay the cost: local
devs, AI agents, and CI. Phases 0–3 below have **shipped**; this doc keeps the
baseline, the outcomes, and the rationale for what was deliberately *not* done.

The authoritative list of which specs still run `Serial` (and why) is the living
[e2e-serial-registry.md](e2e-serial-registry.md).

## Non-goals

- Rewriting the e2e harness from scratch.
- Reducing the number of e2e specs (no spec was merged or deleted; the Phase 2
  split preserved spec count).

## Baseline (2026-05-28, warm devcontainer, k3d reused)

| | |
|---|---|
| Warm `task prepare-e2e` | **1.4 s** (everything stamp-cached) |
| Smoke suite (`task test-e2e`, 20 of 46 specs) | **418.7 s** (~7 min) |

### Per-spec ranking (smoke filter, warm)

```
  Duration     %  Spec
   128.4 s  30.8  Restart Snapshot Safety keeps the git mirror intact when the controller restarts
    40.9 s   9.8  Commit Window Batching collapses a burst of events into one grouped commit and one push
    34.1 s   8.2  Aggregated API server should install and serve flunders through the aggregation layer
    28.9 s   6.9  Manager should create Git commit when IceCreamOrder CRD is installed via ClusterWatchRule
    25.7 s   6.2  Commit Signing should produce per-event commits verifiable locally and by Gitea
    22.3 s   5.3  Commit Request finalizes a CommitRequest created with metadata.generateName
    21.9 s   5.2  Commit Request finalizes the open commit window on demand and reports the resulting SHA
    15.5 s   3.7  Manager should delete Git file when ConfigMap is deleted via WatchRule
     9.9 s   2.4  Manager should create Git commit when ConfigMap is added via WatchRule
     9.1 s   2.2  Manager should commit encrypted Secret manifests when WatchRule includes secrets
     4.4 s   1.1  Manager should backfill pre-existing ConfigMap when WatchRule is added afterwards
     4.1 s   1.0  Manager should receive audit webhook events from kube-apiserver
     2.1 s   0.5  Manager should handle a normal and healthy GitProvider
     1.9 s   0.5  Manager should run successfully
     1.7 s   0.4  Manager should validate GitProvider with real Gitea repository
     0.1 s   0.0  Manager should ensure the metrics endpoint is serving metrics
     0.1 s   0.0  Manager should expose the controller service
```

**Findings that shaped the plan:** the top 5 specs are ~62 % of smoke wallclock;
`Restart Snapshot Safety` alone is 31 %, of which ~75 s was a passive
negative-assertion wait; namespace teardown is a consistent 20–25 s per spec
(finalizer waits); per-spec setup is ~5–10 s. The structural blockers to
parallelism were the shared `Manager` Describe and cluster-scoped resources.

## What shipped

### Phase 0 — Visibility

Added `-ginkgo.json-report` to the e2e `go test` invocations plus a small parser
(`test/e2e/tools/spec-timings/`) that prints the duration-sorted table above from
structured data. No custom reporter needed.

### Phase 1 — `prepare-e2e` as a real `task` DAG

Replaced the flat `cmds:` chains in `prepare-e2e` with `deps:` so independent
stamps (`_age-key`/`_sops-secret-yaml`, `_image-loaded`, `_install-cleanup`,
`manifests`) run concurrently. The critical path is the strictly-serial Flux
ramp-up (`_cluster-ready → _flux-installed → _flux-setup-ready →
_services-ready → install → _controller-deployed → _webhook-tls-ready →
_aggregated-api-ready`); everything else folds onto it.

Two ordering constraints had to stay sequential: `portforward-ensure` runs
*after* `_webhook-tls-ready` (the k3d server-node restart inside it kills
concurrent port-forwards), and `_install-cleanup` is its own node so
`_sops-secret-yaml` can depend on it rather than racing a cleanup that wipes its
output.

**Measured:** cold `task prepare-e2e` 230 s → 213 s (Flux ramp dominates a cached
image build; with a fresh Go build the parallel branches save more). Warm stays
~1 s.

### Phase 2 — Mechanical split of the `Manager` Describe

Split the 21-spec `Manager` Describe into four files, each its own top-level
Describe with per-file fixtures, replacing the package-global `managerRepo` with
per-Describe locals:

- `controller_basics_e2e_test.go` (no Gitea repo)
- `gitprovider_validation_e2e_test.go`
- `watchrule_configmap_secret_e2e_test.go`
- `crd_lifecycle_e2e_test.go` (owns its own per-file CRD)

Spec count and observable behavior preserved. This phase alone is *slower*
sequentially (more namespace teardowns); it's the prerequisite for Phase 2.5.

### Phase 2.5 — Bounded Ginkgo parallelism + Serial registry

Parallelism is driven by the `ginkgo` CLI (`--procs=$E2E_GINKGO_PROCS`, default
`2`), not `go test`. `SynchronizedBeforeSuite` runs `prepare-e2e` + CRD
pre-cleanup once on process #1. Per-file CRD-group isolation
([test/e2e/icecream.go](../../test/e2e/icecream.go)) gives `crd_lifecycle`,
`restart_snapshot`, and `bi_directional` their own API groups so they never
collide by name. The [e2e-serial-registry.md](e2e-serial-registry.md) captures
the specs that still need `Serial`.

**Measured (local, procs=2):** smoke **352 s** vs 418.7 s sequential (~16 %).
**CI runs the `full` leg at `E2E_GINKGO_PROCS=1`** — on stock `ubuntu-latest`
runners the singleton controller is CPU-starved under two concurrent streams plus
its own signing/git work, so a different spec timed out each run. Local/default
stays `procs=2`.

### Phase 3 — Drain-signal metrics for `Restart Snapshot Safety`

Replaced the ~75 s blind negative-assertion wait with two metrics, both gated on
the *new* controller pod (`pod="<new>"`) so a rollout's brief two-series overlap
can't produce a false pass:

- `gitopsreverser_target_reconcile_completed_total{gittarget_namespace,gittarget_name,trigger}`
  — a **counter** (not a latched gauge): on a fresh pod it starts at 0, so
  `> 0` proves *that pod* completed its own post-restart snapshot reconcile.
- `gitopsreverser_branch_worker_queue_depth{provider_namespace,provider_name,branch}`
  — a gauge that reaches 0 only once every accepted item is fully handled and a
  successful push has cleared `pendingWrites` (depth counts in-flight items via
  an `atomic.Int64`, not `len(eventQueue)`, to avoid a mid-commit false drain).

Metrics live in [internal/telemetry/exporter.go](../../internal/telemetry/exporter.go).
Label keys are prefixed (`gittarget_namespace`, not bare `namespace`) because the
pod scrape with `honor_labels: false` would otherwise overwrite a bare
`namespace` and move it to `exported_namespace`.

**Measured:** restart spec ~128 s → ~67 s. Both gates resolve on the first poll
after the rollout settles; the residual time is fixed setup (Prometheus client,
Gitea repo, CRD install, building the mirror, the rollout itself) plus the 15 s
stability window — not the drain wait, which is now ~0 s.

## Considered and skipped

Recorded so revisiting is cheap.

1. **Short-circuit `BeforeSuite` to skip warm `prepare-e2e`.** Re-implementing
   the stamp freshness check in Go duplicates logic `task` already owns and gives
   two sources of truth. The ~1.4 s warm cost is small; Phase 1's DAG is the
   better lever. *Revisit if* warm prepare grows back past a few seconds.

2. **CI matrix sharding by Ginkgo label.** GitHub Actions throttles this repo at
   3+ concurrent runners, so shards queue instead of running in parallel —
   defeating the wallclock benefit while burning more CPU minutes. *Revisit if* we
   get real concurrent runner capacity, a different CI provider, or self-hosted
   runners.

3. **CI cluster reuse (self-hosted runner or Tekton).** Both require durable
   operational ownership (a VM/cluster to secure, upgrade, monitor, pay for) plus
   multi-tenant isolation between concurrent pipelines. Phases 1–3 attack the
   cheaper per-run cost first. *Revisit if* CI wallclock is still dominated by
   cluster boot after Phases 1–3 *and* the team will take on the ops burden.

4. **Removing `Ordered` *within* the new Manager files.** Phase 2 already lets the
   four files run on four workers; intra-file parallelism only helps a file with
   many slow specs, which the post-split layout doesn't have, and adds real
   ordering-correctness risk. *Revisit if* one file grows a slow tail of
   independent specs.

5. **Tekton / Shipwright "cloud-native CI".** Iteration speed is dominated by
   cluster boot and test execution, not the build step; pushing images through a
   registry every build makes per-iteration cost go *up*. The one legitimate
   angle (warm infra on a persistent cluster) is item 3 with extra moving parts.
   *Revisit if* we want pipeline-as-CRD for a separate reason (preview
   environments, on-demand test infra).

## Reproducing the baseline

```bash
task prepare-e2e
mkdir -p /tmp/e2e-baseline
go test -timeout 30m ./test/e2e/ \
  -ginkgo.v \
  -ginkgo.json-report=/tmp/e2e-baseline/full.json
go run ./test/e2e/tools/spec-timings /tmp/e2e-baseline/full.json
```
