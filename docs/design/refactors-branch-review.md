# `refactors` branch — review & action checklist

> Review of `main...refactors` (57 files, ~7.5k added / ~2.1k deleted).
> Generated 2026-06-01 from a clean working tree.
> Covers: (1) code-review findings, (2) docs triage, (3) refactor advice on the
> two e2e docs and the stability-enabled follow-ups.

The big test-file split (`e2e_test.go` → four `*_e2e_test.go` files) verified
clean: all 23 Manager specs are re-created, assertion counts match, and
CRD/namespace cleanup was correctly redistributed. Findings concentrate in the
new Phase-3 drain metrics and the new e2e helpers.

## Merge readiness

### Block before merge

- [x] Fix the queue-depth metric publication race (finding 1).
- [x] Fix discovery-lag / empty-plan handling for rule-set snapshots (finding 2).
- [x] Simplify CI Allure handling: keep uploading raw Ginkgo JSON and timing
  summaries, but remove CI-side Allure result conversion, HTML rendering,
  permission fixups, and Allure artifact upload from `.github/workflows/ci.yml`.
  Local report generation via `task allure-e2e-report` is enough when a human
  needs the visual timeline.

### Good enough for follow-up

- [x] Cleanup findings 5-9 (done); finding 10 CI half done, two Go micro-opts
  deferred with rationale (see finding 10).
- [x] Docs archiving/relocation in section 2.
- [x] Local Allure docs polish in section 3.

### Validation already run

- [x] `git diff --check origin/main...HEAD`
- [x] `task lint-actions`
- [x] `go test ./test/e2e/tools/ginkgo-allure ./test/e2e/tools/spec-timings ./internal/git ./internal/watch`
- [x] `go test -run '^$' ./test/e2e`
- [x] `task lint`
- [x] `task test`
- [x] `task test-e2e`

---

## 1. Code review

### Correctness (ranked)

- [x] **1. Stale queue-depth gauge race — can hang the restart drain gate**
  — [branch_worker.go:315-331](../../internal/git/branch_worker.go#L315-L331).
  `recordQueueDepth()` runs from enqueue goroutines *and* the event loop; it
  does `Load(inflightItems)` → … → `Record()` non-atomically. An enqueue
  goroutine can load `depth=1`, get preempted while the loop drains the item and
  `Record(0)` then idles, and then publish the stale `1`. OTel gauge is
  last-writer-wins and the idle loop never republishes → gauge latches at `1`,
  so the restart-snapshot gate `branch_worker_queue_depth … == 0` times out at
  90s though the worker is drained. **Fix:** publish the gauge *only* from the
  loop goroutine (drop `recordQueueDepth()` from the enqueue paths; the loop
  already republishes each iteration via `syncQueueDepthMetric`).

- [x] **2. Discovery-lag causes spurious full re-snapshots**
  — [manager.go:1248](../../internal/watch/manager.go#L1248),
  [:1264](../../internal/watch/manager.go#L1264),
  [:1280](../../internal/watch/manager.go#L1280),
  [:1189-1191](../../internal/watch/manager.go#L1189-L1191).
  `currentRuleSetSnapshots` hashes the *resolved* GVR plan and swallows the
  resolve error (`gvrs, _ := resolver.Resolve(...)`); an empty-plan target is
  dropped and `snapshotTargetsNeedingDelivery` then deletes its
  `lastDeliveredRuleSetHash`. A transient API-discovery gap makes a target
  vanish and, on recovery, get re-snapshotted (full repo-state diff). New
  regression vs the old rule-text hash; it's the same mechanism that forces
  `crd_lifecycle` Serial. **Fix:** log the resolve miss instead of discarding,
  and don't evict last-delivered state on an *empty* plan (distinguish "no
  rules" from "rules that didn't resolve yet").
  **Fixed (simple version):** `currentRuleSetSnapshots` now keeps targets that
  still have rules even when the resolved entry set is empty, and
  `snapshotTargetsNeedingDelivery` skips those empty plans without selecting or
  pruning them. This preserves the delivered baseline across transient discovery
  gaps. Known accepted tradeoff: `valid -> stable-empty -> valid` user edits do
  not force a recovery snapshot until the richer settled-empty refinement lands.
  Detailed proposal:
  [rule-set-snapshot-discovery-lag-fix.md](rule-set-snapshot-discovery-lag-fix.md).

- [x] **3. Reconcile counter skipped on transient error / fired on no-op**
  — [manager.go:1384-1392](../../internal/watch/manager.go#L1384-L1392).
  `recordTargetReconcileCompleted` fires only after **both** `RequestRepoState`
  and `RequestClusterState` succeed. A transient error on the second `continue`s
  (counter never increments though partial state was enqueued) → restart
  `{pod=…} > 0` gate can time out at 90s on a one-off error. It also fires on a
  no-op snapshot, so `increase(target_reconcile_completed_total)` over-counts
  for non-test consumers.
  **Fixed (transient-error half):** `emitSnapshotForRuleChange` now returns a
  joined error for any target whose repo/cluster-state request failed; the target
  is left pending (not marked delivered, counter not fired) and
  `ReconcileForRuleChange` propagates the error so the controller requeues with
  backoff and retries promptly instead of waiting out the 30s periodic tick.
  The "fires on a no-op snapshot" half is **left intentionally**: the counter's
  documented contract is to count snapshot passes that reached the git write path
  (not commits produced), and a clean post-restart re-snapshot legitimately
  produces an empty diff yet must still increment so the restart gate can observe
  `{pod=<new>} > 0`. Gating it on a non-empty diff would break that load-bearing
  signal.

- [x] **4. Shutdown leaves the depth gauge non-zero**
  — [branch_worker.go:585](../../internal/git/branch_worker.go#L585),
  [:704](../../internal/git/branch_worker.go#L704).
  On `ctx.Done()` the loop finalizes + pushes and returns **without draining
  items still buffered in `eventQueue`**; each was counted into `inflightItems`
  at enqueue, so the final `recordQueueDepth` publishes a non-zero depth for the
  exiting pod that never clears. Shielded today by the per-pod gate; latent leak
  for any cross-pod use.

### Cleanup / efficiency

- [x] **5. ~13 inlined `git pull` blocks duplicate `pullLatestRepoState`**
  — [watchrule_configmap_secret_e2e_test.go:174](../../test/e2e/watchrule_configmap_secret_e2e_test.go#L174)
  (+~6 more) and ~7 in `crd_lifecycle_e2e_test.go`. The canonical helper
  (fetch + checkout -B + reset --hard) is already used at
  [:707](../../test/e2e/watchrule_configmap_secret_e2e_test.go#L707) and is more
  robust than a bare `git pull` (which can flake on a diverged branch).
  **Fixed:** all 6 blocks in `watchrule_configmap_secret_e2e_test.go` and all 10
  in `crd_lifecycle_e2e_test.go` (both the error-checking and ignore-error
  forms) now call `pullLatestRepoState(g, …)`.
- [x] **6. `gitPull` / `lastCommitMessageForPath` reinvent existing helpers**
  — [gittarget_isolation_e2e_test.go:187](../../test/e2e/gittarget_isolation_e2e_test.go#L187).
  Third pull implementation (no fetch/reset) + manual `git log` where
  `gitRun(dir, args...)` already exists in helpers.go.
  **Fixed:** call site uses `pullLatestRepoState`; the local `gitPull` is removed
  and `lastCommitMessageForPath` goes through `gitRun`. Unused `os/exec` import
  dropped.
- [x] **7. Hand-rolled `minInt`** — [e2e_test.go:264](../../test/e2e/e2e_test.go#L264);
  used once, Go 1.26 builtin `min` covers it. **Fixed:** `minInt` deleted, sole
  caller uses builtin `min`.
- [x] **8. Redundant "healthy GitProvider" spec**
  — [watchrule_configmap_secret_e2e_test.go:80](../../test/e2e/watchrule_configmap_secret_e2e_test.go#L80)
  re-creates/re-verifies `gitprovider-normal` already built in `BeforeAll`
  ([:60-70](../../test/e2e/watchrule_configmap_secret_e2e_test.go#L60-L70)).
  **Fixed:** the spec now only re-asserts the BeforeAll-created provider stays
  Ready; the redundant re-creation is gone.
- [x] **9. Duplicated endpoint→pod filtering**
  — [controller_basics_e2e_test.go:99](../../test/e2e/controller_basics_e2e_test.go#L99)
  vs [:135](../../test/e2e/controller_basics_e2e_test.go#L135); extract
  `expectServiceRoutesToPod`. **Fixed:** both blocks call the new
  `expectServiceRoutesToPod(g, service, pod)` helper in `helpers.go`; the
  now-unused `strings` import is dropped.
- [x] **10. Wasted work** — CI "Summarize E2E timing reports" `go run`s
  spec-timings per report in a loop ([ci.yml:388](../../.github/workflows/ci.yml#L388)),
  recompiling N+1× (`go build -o` once instead); `currentRuleSetSnapshots`
  resolves GVRs twice per reconcile ([manager.go:1224](../../internal/watch/manager.go#L1224));
  `syncQueueDepthMetric` records every loop iteration rather than on change.
  **Fixed (CI half):** the summary step now `go build -o /tmp/spec-timings` once
  and runs the binary per report instead of `go run` recompiling each time.
  **Deferred intentionally (the two Go halves):** `syncQueueDepthMetric`'s
  per-iteration `Record` is the load-bearing republish that finding #1's fix
  relies on to keep the OTel gauge from latching a stale depth — switching it to
  record-on-change would reintroduce that race. The "resolves GVRs twice" cost
  spans two *different* methods (plan-hash in `currentRuleSetSnapshots` vs.
  watch/list setup in the snapshot builder), so collapsing it needs a
  per-reconcile resolution cache rather than a local tidy-up; left for a focused
  follow-up.

> The Allure env-var overrides + `specSkippedByFilter` + per-process thread
> label look good. Caveat: `specSkippedByFilter`
> ([ginkgo-allure/main.go:263](../../test/e2e/tools/ginkgo-allure/main.go#L263))
> infers "filtered-out" from `LabelFilter!="" && Skipped && EndTime.IsZero() &&
> RunTime==0`; a real `Skip()` recording zero timing would also be dropped. Low
> risk — a heuristic, not a guarantee.

---

## 2. Docs triage

> `docs/finished/` already exists (empty) but isn't in the README — adopt it as
> the "done" folder and document it in [docs/README.md](../README.md).

### Move to `docs/finished/` — shipped/resolved

- [x] [finished/gittarget-isolation-on-rule-change.md](../finished/gittarget-isolation-on-rule-change.md) — ✅ shipped (this branch)
- [x] [finished/commit-window-refactor.md](../finished/commit-window-refactor.md) — implemented
- [x] [finished/e2e-cleanup-scoping.md](../finished/e2e-cleanup-scoping.md) — implemented
- [x] [finished/partial-object-audit-event-handling.md](../finished/partial-object-audit-event-handling.md) — implemented
- [x] [finished/path-scoped-bootstrap-template-design.md](../finished/path-scoped-bootstrap-template-design.md) — implemented
- [x] [finished/remove-gittarget-admission-webhook.md](../finished/remove-gittarget-admission-webhook.md) — webhook file gone → done
- [x] [finished/watch-gvr-resolution-cleanup.md](../finished/watch-gvr-resolution-cleanup.md) — `APIResourceCatalog` cleanup landed
- [x] [finished/watchrule-gvr-resolution-plan.md](../finished/watchrule-gvr-resolution-plan.md) — implemented
- [x] [finished/aggregated-api-requestheader-ca-miswire-report.md](../finished/aggregated-api-requestheader-ca-miswire-report.md) — resolved report
- [x] [finished/audit-consumer-username-attribution-gap.md](../finished/audit-consumer-username-attribution-gap.md) — resolved
- [x] [finished/e2e-runtime-state-vs-stamps.md](../finished/e2e-runtime-state-vs-stamps.md) — resolved failure mode
- [x] [finished/shallow-audit-event-misclassification.md](../finished/shallow-audit-event-misclassification.md) — root cause confirmed
- [x] [finished/e2e-test-review.md](../finished/e2e-test-review.md) — point-in-time review
- [x] [finished/git-author.md](../finished/git-author.md) — OIDC author plan, implemented
- [x] [finished/design-commit-request-api.md](../finished/design-commit-request-api.md) — implemented
- [x] [finished/design-rule-change-snapshot-trigger.md](../finished/design-rule-change-snapshot-trigger.md) — implemented
- [x] [finished/design-audit-ingestion-hardening.md](../finished/design-audit-ingestion-hardening.md) — implemented
- [x] [finished/design-commit-context-api.md](../finished/design-commit-context-api.md) — superseded
- [x] [finished/idea-audit-enrichment-side-channel.md](../finished/idea-audit-enrichment-side-channel.md) — superseded

### Consider removing / relocating

- [x] [serious-bug/cozystack-bugreport.md](../serious-bug/cozystack-bugreport.md) — upstream bug report against
  CozyStack, not our docs. If filed upstream, replace with a link. Referenced by
  `initial-seed-via-watch-list.md`, so update that link; `serious-bug/` is the
  safe relocation. **Done:** moved to
  [serious-bug/cozystack-bugreport.md](../serious-bug/cozystack-bugreport.md);
  both references in `initial-seed-via-watch-list.md` updated.
- [x] [ci/findings.md](../ci/findings.md) — stale (2026-02-13), overlaps
  [e2e-ci-stability-report.md](e2e-ci-stability-report.md). Fold true bits in, drop it.
  **Deviated from the recommendation:** on inspection the two docs do *not*
  overlap — `ci/findings.md` is devcontainer/CI infrastructure rationale
  (workspace mounts, Go cache volumes, in-container cluster networking), while
  `e2e-ci-stability-report.md` is about test stability and the e2e job's disk
  pressure. Deleting it would lose useful rationale. It *was* stale on specifics
  (referenced Kind + `make setup-cluster`; the project now uses k3d + `task`), so
  it was refreshed in place (k3d node naming, `start-cluster.sh` port handling,
  updated date) and kept.
- [x] Empty `docs/finished/`, `docs/serious-bug/`, `docs/contributing/` — populate
  (per above) or remove the empty scaffolding. **Done:** `finished/` was already
  populated by the section-2 moves and is now documented in
  [docs/README.md](../README.md); `serious-bug/` is populated by the cozystack
  relocation and documented in the README; the empty `docs/contributing/` was
  removed (the contributor guide lives at top-level `CONTRIBUTING.md`).

### Keep on the active design surface

Still-relevant plans/proposals: [e2e-speedup-plan.md](e2e-speedup-plan.md),
[e2e-serial-registry.md](e2e-serial-registry.md),
[e2e-ci-stability-report.md](e2e-ci-stability-report.md),
[e2e-speedup-skipped.md](e2e-speedup-skipped.md),
[audit-metrics-overhaul-plan.md](audit-metrics-overhaul-plan.md) (proposed),
[initial-seed-via-watch-list.md](initial-seed-via-watch-list.md) (proposed) +
[upgrade-finding.md](../upgrade-finding.md), the sensitive-resource plan +
follow-up, and [e2e-watchrule-cross-spec-interference.md](e2e-watchrule-cross-spec-interference.md)
(status: **open**). All `future/idea-*.md` stay. Reference guides
(`kubernetes-*`, `crd-relationships`, `status-conditions-guide`, audit-webhook
designs, lifecycle/architecture) stay. All top-level user guides + `ci/*`
explainers + `demo/*` + `audit-setup/*` stay.

---

## 3. The two e2e docs + stability-enabled refactors

**[e2e-allure-reporting.md](../ci/e2e-allure-reporting.md)** — accurate for the
current branch, but should be updated if CI stops rendering Allure:

- [x] If `.github/workflows/ci.yml` is simplified, rewrite the CI section to say
  CI uploads only raw Ginkgo JSON/timing artifacts; developers can download
  those reports and run `task allure-e2e-report` locally.
- [x] Document the new per-process **`thread` Allure label** the converter now
  emits (the point of the parallel-insight work). **Done:** added an "Allure
  labels" section to `e2e-allure-reporting.md` documenting `thread` plus the
  `suite`/`package`/`parentSuite`/`subSuite`/`tag` labels.

**[e2e-serial-registry.md](e2e-serial-registry.md)** — the Serial entries are
**correctness-driven (shared state), not flakiness-driven**, so improved
stability doesn't justify de-serializing directly. It *does* unlock the deeper
refactors the registry is implicitly waiting for. Both items below were planned
in [e2e-serial-deserialization-plan.md](e2e-serial-deserialization-plan.md) and
then partly executed:

- [~] **Audit pipeline.** The planned heavy refactor (per-test stream/consumer
  isolation, plan option B1) was **not** done — it bends the intentionally
  singleton audit pipeline for test parallelism and isn't worth the
  production-correctness risk. Instead the audit-consumer Serial set was *reduced*:
  `Audit Redis Queue` and `Audit Redis Consumer`
  (`audit_redis_e2e_test.go`, now deleted) were retired — their producer-path and
  basic consumer-commit assertions duplicated the WatchRule suite. The one unique
  assertion (OIDC `user.extra` → commit author) moved to a new **parallel**
  [commit_author_attribution_e2e_test.go](../../test/e2e/commit_author_attribution_e2e_test.go):
  a dedicated 0s-commit-window GitProvider + per-path author read makes it
  concurrency-safe, so it needs no `Serial`. `Commit Window Batching` and
  `Commit Request` **stay Serial** — they assert batching/commit-window semantics
  over the shared stream, which concurrent audit traffic genuinely violates.
- [x] **De-serialize `crd_lifecycle` (was gated on code finding #2).** **Done:**
  with finding #2's fix shipped (a target only resnapshots when its *resolved*
  plan hash changes) and per-file CRD groups keeping the `IceCreamOrder` CRD
  name-isolated, `Manager CRD Lifecycle` dropped `Serial` and now runs `Ordered`
  in the parallel pool. Verified green under `--procs=4` (the only other
  wildcard `ClusterWatchRule`, in `restart_snapshot`, is itself Serial, so no
  concurrent spec matches the icecream group). Registry updated.
- `bi_directional` (exact commit-count) and `restart_snapshot` / `image_refresh`
  (singleton controller) + `aggregated_apiserver` (cluster `APIService`) are
  genuinely serial — leave them.

- [ ] **Confidence-window pass (the speedup plan's open thread).** Now that the
  race fixes landed (`03b95ab`, `0787b1b`), decide whether CI should keep
  `K3D_AGENT_COUNT=0` and the current per-matrix Ginkgo parallelism, or move
  back toward agent-backed clusters. Use the uploaded timing summaries and raw
  Ginkgo JSON as the evidence source; render Allure locally only when the visual
  report is needed.
