# E2E Speedup — Items Considered and Skipped

Companion to [e2e-speedup-plan.md](e2e-speedup-plan.md). This document records
proposals that were evaluated during the 2026-05-28 planning round and
*excluded* from the active plan, with the reasoning. The point is to make
future revisiting cheap: when someone asks "did we look at X?", the answer
and the *why* are here, not lost in chat history.

## 1. Short-circuit `BeforeSuite` to skip warm `task prepare-e2e`

**Proposal.** [test/e2e/e2e_suite_test.go:64](../../test/e2e/e2e_suite_test.go#L64)
unconditionally re-invokes `task prepare-e2e` on every `go test` run. Warm
cost is ~1.4 s. The idea was to `stat` the `prepare-e2e.ready` stamp from Go
and skip the subshell if it's newer than the relevant inputs.

**Why skipped.** It duplicates logic that `task` already owns. The whole
point of the stamp files is that `task` itself decides whether to do work or
no-op. Re-implementing the freshness check in Go gives us two sources of
truth for the same thing, with the obvious failure mode that they drift.

The 1–2 s warm cost is small enough that the better fix — if it ever
matters — is to make `task` cheaper to invoke for an all-no-op DAG (Phase 1
of the active plan already moves in that direction by converting chains into
parallel `deps:`).

**Revisit if:** warm prepare ever grows back past a few seconds in a way
that Phase 1's DAG refactor can't fix.

## 2. CI matrix sharding (split the `full` e2e job by Ginkgo label)

**Proposal.** [.github/workflows/ci.yml:261-313](../../.github/workflows/ci.yml#L261-L313)
runs the e2e suite as a 2-entry matrix `{quickstart, full}`. Splitting
`full` into 4–5 label-scoped shards (`smoke`, `signing`, `audit-consumer`,
`bi-directional`, `image-refresh`, `aggregated-api`) would parallelize
across GitHub runners and drop wallclock to `max(shard)` instead of
`sum(shard)`.

**Why skipped.** Empirically GitHub Actions chokes when this repo asks for
3+ concurrent runners — there are throttles in practice that make the
"free parallelism" assumption false. The shards would queue rather than
actually running in parallel, defeating the wallclock benefit and burning
more CPU minutes per run.

**Revisit if:**

- We move to a GitHub plan / runner pool with real concurrent capacity, or
- We move to a different CI provider where concurrent runners are not
  throttled, or
- We adopt self-hosted runners (see item 3) where we control the
  parallelism budget.

## 3. CI cluster reuse via self-hosted runner or Tekton

**Proposal.** Today every CI job creates a fresh k3d cluster; that boot is
the dominant CI cost. Two flavours of reuse were considered:

- **Self-hosted GitHub runner** holding a long-lived k3d. PRs run against
  namespace-scoped slices.
- **Tekton on a persistent Kubernetes cluster.** Pipelines run as
  CRD-defined Tasks; the host cluster (or warm test fixtures within it)
  survives across pipeline runs.

**Why skipped (for now).** Both require taking on durable operational
responsibility: a VM or cluster we own, secure, upgrade, monitor, and pay
for. Multi-tenant isolation between concurrent PR pipelines (namespaces,
nested k3d, or vCluster) is its own design problem. Tekton additionally
requires running an image registry for CI-built images (`k3d image import`
no longer works once builds live in pods) and maintaining pipeline-as-CRD
definitions alongside GitHub Actions.

The active plan's Phases 1–3 attack the *per-run* cost of the e2e harness.
That's the cheaper lever to pull first. We can re-measure CI wallclock
after they land, and only then decide whether the remaining cold-boot
cost justifies the ops investment.

**Revisit if:** Phases 1–3 of the active plan are complete and CI wallclock
is *still* dominated by cluster boot, *and* the team is willing to take on
the ops burden of a persistent runner or cluster.

## 4. Removing `Ordered` *within* the new Manager files (Ginkgo intra-file parallelism)

**Proposal.** After Phase 2 of the active plan splits the `Manager`
Describe into four files, also drop the `Ordered` decorator inside each
new file so Ginkgo can parallelize specs within a single file.

**Why skipped (for now).** Phase 2 already buys the parallelism that
matters: Ginkgo runs *across* top-level Describes in parallel, so the four
new files can run on four workers. Dropping `Ordered` inside each file
would only help if a single file has many slow specs — which the
post-split layout does not. Group D (IceCreamOrder lifecycle) genuinely
needs `Ordered`. Groups A/B/C have cheap specs (most < 5 s); the gain
from intra-file parallelism is small and the correctness risk (implicit
ordering not caught yet) is real.

**Revisit after:** Phase 2 lands and we have stability data. If one of the
new files has a slow tail of independent specs, drop `Ordered` there
specifically.

## 5. Move build to Tekton / Shipwright for cloud-native CI

**Proposal.** Replace (or supplement) GitHub Actions with Tekton pipelines
and/or Shipwright build CRDs, on the theory that "cloud-native CI" would
naturally make iteration faster.

**Why skipped.** The premise doesn't survive contact with the actual cost
model:

- Iteration speed is dominated by *cluster boot* and *test execution*, not
  by the *build* step. Shipwright only addresses image builds; Tekton is a
  pipeline orchestrator, not a test accelerator.
- Pushing/pulling images through a registry on every build (Tekton can't do
  `k3d image import` the way local `task` can) makes per-iteration cost go
  *up*, not down.
- The legitimate angle — Tekton on a persistent cluster keeping test
  infrastructure warm across pipeline runs — is the same problem as item 3
  with extra moving parts. Self-hosted GitHub runner gets ~80 % of the
  benefit at ~10 % of the complexity.

**Revisit if:** we have a separate reason to want pipeline-as-CRD (preview
environments, dynamic on-demand test infrastructure, multi-product
delivery) — in that case the iteration-speed benefit becomes a bonus rather
than the justification.
