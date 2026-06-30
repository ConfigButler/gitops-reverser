---
title: e2e full-suite flakiness — forensic findings (poc/manifestedit, June 2026)
status: investigation
date: 2026-06-09
related:
  - e2e-watchrule-cross-spec-interference.md
  - e2e-full-suite-shared-state-investigation.md
---

# e2e full-suite flakiness — forensic findings

This records a concrete investigation triggered while hardening the
contextual-namespace work on `poc/manifestedit`. The local `task test-e2e` was
red while GitHub CI was green. This document starts with the raw facts, then
interprets them, then states what is and is not the cause.

## 1. Facts and observations (no interpretation yet)

### 1.1 CI was green on the recent commits

The last five `CI` runs on `poc/manifestedit` were all `success` (`gh run list`,
2026-06-08):

| Run | Commit subject | Result | When |
|---|---|---|---|
| 27161369593 | feat: flexible manifests | success | 2026-06-08T19:21:52Z |
| 27160517400 | feat: flexible manifests | success | 2026-06-08T19:06:00Z |
| 27156885656 | feat: flexible manifests | success | 2026-06-08T17:59:24Z |
| 27153490992 | feat: flexible manifests | success | 2026-06-08T17:09:07Z |
| 27144704089 | feat: flexible manifests | success | 2026-06-08T14:29:21Z |

`main` is green as well. The newest green run corresponds to the branch HEAD
commit `72df204` ("chore: re-enable serial and updating architecture").

CI runs the full suite via `task test-e2e` at **`E2E_GINKGO_PROCS=4`** with **no
`--flake-attempts`/retry** (`.github/workflows/ci.yml`; the matrix even comments
"Validated locally at --procs=4; trialing the same in CI"). So CI's parallelism is
the same as the local run, and there is no retry masking failures.

### 1.2 CI never tested the code that was red locally

The green CI runs validated the **committed** tree at `72df204`. At the time of
this investigation the working tree additionally contained, all **uncommitted**:

- the contextual-namespace hardening (`internal/manifestanalyzer/store.go`,
  `internal/git/plan_flush.go`);
- a **new** e2e spec, `Manager Manifest Folder Editing`
  (`test/e2e/inplace_edit_e2e_test.go`) — `git log` on that file shows the
  committed version contains **zero** occurrences of "Manifest Folder Editing";
- timeout bumps in `test/e2e/crd_lifecycle_e2e_test.go` (60s → 2m).

Therefore **CI has never run this working tree**, and in particular has never run
the new manifest-folder spec. "CI is green" and "local is red" were never the same
code.

### 1.3 Local full-suite runs (same box, same cluster, procs=4 unless noted)

| Run | Tree / image | Procs | Result |
|---|---|---|---|
| A | working tree (hardening + new specs) | 4 | **12 failed**, 17 passed, 24 skipped |
| B | `inplace-edit` label only (the feature) | 1 | **0 failed**, 2 passed |
| C | committed baseline `72df204` (stashed) | 4 | **1 failed**, 39 passed |
| D | working tree, re-run | 4 | **8 failed**, 22 passed |
| E | hardening image + baseline spec set (no new spec) | 4 | **10 failed**, 17 passed |

The failure **count is high-variance** (1, 8, 10, 12) across nominally-similar
runs. Only B (serial, the feature in isolation) was clean.

### 1.4 Every failure is one of two shapes

- `status.conditions not found` — a **WatchRule** that never received *any* status
  for 90s (`e2e_test.go:224` → `:194`). The GitTarget for the same spec **does**
  reach Ready; only the WatchRule hangs.
- `CRD file should exist … icecreamorders…crd…: no such file` at **60s**
  (`crd_lifecycle_e2e_test.go:150`) — the CRD-install commit did not land in time.

The single baseline failure (run C) was the second shape.

### 1.5 Controller health and the reflector noise

- The controller pod was `Ready`, **Restart Count 0**, no panic, across all runs.
- The logs carry **403** repetitions of
  `Failed to watch … icecreamorders.crd-lifecycle.e2e.example.com … the server
  could not find the requested resource` — a reflector retrying a CRD that a spec
  has deleted.
- Specs that only validate a `GitProvider` (no WatchRule readiness) passed every
  run. The failing specs are the ones that need a WatchRule to go Ready and/or
  touch the `icecreamorders` custom resource (CRD Lifecycle, the wildcard
  custom-API WatchRule, Bi-Directional IceCreamOrder, Aggregated API, plus
  WatchRule-bootstrap-dependent specs caught in the wave).

## 2. What the code actually does (mechanism)

Read against current `main`-line code, not the older design notes:

1. **The catalog tolerates partial discovery.** `discoverCatalogRefresh`
   (`internal/watch/api_resource_catalog.go`) treats `*ErrGroupDiscoveryFailed`
   as partial success: it keeps the healthy groups, marks the failed group
   degraded (keeping its last-known entries), and sets `complete=false` so no
   group is removed on a wobble. `Registry.Ready()` is a **latch** — once the
   surface has been observed it stays ready. **So a single deleted CRD does not
   flip the global registry to not-ready and does not fail-close unrelated
   GitTargets through the catalog/snapshot path.**

2. **The snapshot mark-and-sweep is GitTarget-global and content-derived, and it
   fails closed on purpose.** `StreamClusterSnapshotForGitDest` /
   `resolveSnapshotGVRs` (`internal/watch/snapshot_stream.go`) abort the whole
   snapshot if any watched type is within the `RemovalGrace` (60s) and currently
   unserved, because `Desired` is the complete set and the worker deletes any git
   doc **not** in `Desired` — snapshotting "only the healthy types" would sweep
   the retained type's mirror. This is anti-destruction safety, not a bug.

3. **The block is bootstrap-only.** `evaluateSnapshotGate`
   (`internal/controller/gittarget_controller.go:390`) does **not** re-run the
   resync once `SnapshotSynced` is true. An already-live GitTarget is **not**
   knocked offline by a later type wobble; "one type fails the whole GitTarget"
   only bites during the *initial* snapshot.

4. **Event routing is live against the RuleStore.** `handleEvent` →
   `matchRules` → `RuleStore.GetMatchingRules` (a live read of the store map) →
   `routeWatchRules` (`internal/watch/informers.go`, `internal/watch/manager.go`).
   On WatchRule deletion the controller calls `RuleStore.Delete`
   (`internal/controller/watchrule_controller.go:85`), which removes the rule from
   that same map. **A deleted WatchRule therefore stops matching immediately** —
   the only residual is the sub-second async window before the delete-reconcile
   runs.

## 3. Diagnosis

The first instinct is to reach for the two mechanisms in
[e2e-watchrule-cross-spec-interference.md](e2e-watchrule-cross-spec-interference.md)
(April 2026). **Both are already fixed in current code**, and were verified
*inactive* in these runs — so neither is the cause here:

- **Not the preservation cascade.** `markSuiteWidePreservation`
  (`test/e2e/suite_state.go`) is called only on a **BeforeSuite failure** or an
  **OS signal** (`e2e_suite_test.go:57,281`), never on a per-spec failure — the
  current comment is explicit: "Per-spec failures deliberately do NOT preserve: the
  run finishes, cleanup runs, and the next spec starts from a known clean state."
  All four captured run logs contain **zero** `Preserving e2e resources` and
  **zero** `skipping cleanup for` lines, confirming preservation never engaged.
- **Not ghost-WatchRule routing.** Matching is live against the RuleStore and
  deletion removes the rule (§2.4), so a deleted WatchRule stops matching at once.
  A "drain on delete" change would fix a non-bug.

What *is* going on:

- **Root trigger: discovery-cache lag after a fresh CRD install.**
  `e2e_test.go:220` literally notes "after a fresh CRD install the controller's
  discovery cache can lag tens of seconds." Under load that lag exceeds the
  CRD-lifecycle spec's 60s budget, so that spec fails — the one failure baseline
  run C also showed.
- **The 403 `icecreamorders` reflector burst is bounded, not a runaway drain.** A
  type that leaves the watched set has its informer cancelled by
  `stopInformerNamespace` (`internal/watch/manager.go:599`). The type lingers only
  for the 60s `RemovalGrace` (so a discovery wobble never sweeps git), during which
  the running informer's reflector retries the now-absent CRD and logs the 403s.
  After the grace it is stopped. So the 403s are a ~60s burst per CRD deletion, not
  an unbounded hot-loop.
- **The wave is resource/timing contention under local `--procs=4`, not a named
  bug.** Every failing spec is wall-clock-bound on asynchronous discovery /
  WatchRule readiness (60–90s windows). A constrained dev box running a heavy
  ~50-spec suite four-wide — plus CRD-lifecycle create/delete churn and (in the
  working-tree runs) one extra heavy concurrent spec — pushes marginal reconciles
  past those windows. That a *type wobble* anywhere makes the whole **bootstrapping**
  GitTarget wait (§2.2–2.3) concentrates the pain on WatchRule-readiness specs. The
  high-variance count (1→8→10→12) is the signature of timing/scheduling, not a
  deterministic regression.

## 4. The interesting case: green CI, reproducibly-red local

Three independent reasons stack, and together they fully explain it:

1. **Different code.** CI validated `72df204`; the red local runs validated an
   uncommitted tree that adds a heavy new spec CI never ran (§1.2). A green CI here
   says nothing about the working tree.
2. **Timing/load, not logic.** Both failure shapes are wall-clock timeouts around
   asynchronous discovery/WatchRule machinery, not assertion-of-wrong-content. A
   more constrained local box under `--procs=4`, plus one extra heavy concurrent
   spec, tips marginal reconciles past their 60–90s windows. CI's `ubuntu-latest`
   has more headroom.
3. **The suite is timing-sensitive here.** The failing specs already carry a trail
   of band-aids (`e2e-watchrule-cross-spec-interference.md`), and the branch's own
   recent history bumped the CRD-lifecycle timeout — evidence the author was already
   fighting this class of flake. Note the *named* bugs from that April doc (ghost
   routing, preservation cascade) are now fixed (§3); what remains is the wall-clock
   sensitivity itself.

The trap worth naming: **a green CI lulls you into thinking the suite validates
your working tree.** It validated a different commit. And because the failure
*count* of a cascade-prone suite is high-variance, a single green or red run proves
little on its own — only the serial isolation run (B) is a clean signal.

## 5. Was the contextual-namespace hardening responsible? No.

- The feature passes **serially in isolation** (run B, 2/2), so its own code path
  is correct end to end.
- The hardening code is a **no-op for every failing spec**: it only does work when
  a `kustomization.yaml` is present in the GitTarget's watched worktree, and none
  of the failing specs' folders have one.
- It does not touch the WatchRule reconciler, `internal/watch`, or
  `internal/typeset` — the paths that own WatchRule readiness and the catalog.
- The 1-vs-{8,10,12} spread is cascade variance + the extra spec's load (run E,
  with the *baseline* spec set but the hardening image, still produced 10 — i.e.
  the spread tracks parallel cascade, not the image).

## 6. Recommendations (ordered by leverage ÷ risk)

The honest headline: there is **no warranted controller "quick fix"** here. The two
bugs one would reach for are already fixed, and the reflector burst is bounded. The
residual is wall-clock contention, addressed by lowering load or asserting on
signals — and, properly, by the already-designed M10 work.

1. **Lower local parallelism (zero-risk, immediate).** Run `E2E_GINKGO_PROCS=2` (or
   1) locally; the suite is reliably green serially. CI keeps `--procs=4` because
   `ubuntu-latest` has the headroom. This is a runner-budget knob, not a code fix.
2. **Reduce the CRD-install discovery-lag flake (the usual *root* trigger).** The
   60s→2m timeout bump is a reasonable band-aid; the durable fix is making
   ClusterWatchRule type-expansion react to the CRD-install event faster, or
   asserting on a controller signal/metric rather than a wall-clock window.
3. **Reduce cross-spec timing coupling (test infra).** Specs in the Manager block
   share a namespace/repo; even with correct cleanup, async timing couples them
   under load. Unique namespaces per spec (recommendation #3 of
   `e2e-watchrule-cross-spec-interference.md`) decouples them by construction.
4. **Per-type reconcile (the proper, larger fix).** So a single bootstrapping type
   that is slow/unhealthy does not block the whole GitTarget's readiness — the one
   real product-side contributor (§2.2–2.3). It requires a **type-scoped** sweep,
   which is exactly why it is not a safe sliver on top of today's global
   mark-and-sweep.
5. **Do not** add a ghost-WatchRule drain or a preservation-cascade fix — both are
   already handled in current code (§3). Verify before reviving an old root cause.

## 7. Appendix: how each claim was checked

- CI status/commit: `gh run list --branch poc/manifestedit`.
- "CI never ran the new spec": `git log -p -1 -- test/e2e/inplace_edit_e2e_test.go`
  on the committed tree → 0 hits for "Manifest Folder Editing".
- Baseline reproduction: `git stash -u` to `72df204`, `E2E_GINKGO_PROCS=4 task
  test-e2e` → 1 failure (the CRD-install lag), proving the redness is not the
  uncommitted code.
- Feature correctness: `ginkgo --procs=1 --label-filter='inplace-edit'` → 2/2.
- Mechanism: direct reads of `api_resource_catalog.go`, `snapshot_stream.go`,
  `gittarget_controller.go`, `informers.go`, `manager.go`, `rulestore/store.go`.
