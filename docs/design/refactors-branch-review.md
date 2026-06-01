# `refactors` branch — review & action checklist

> Review of `main...refactors` (57 files, ~5k added / ~2k deleted) plus the
> uncommitted Allure/Taskfile working-tree changes. Generated 2026-06-01.
> Covers: (1) code-review findings, (2) docs triage, (3) refactor advice on the
> two e2e docs and the stability-enabled follow-ups.

The big test-file split (`e2e_test.go` → four `*_e2e_test.go` files) verified
clean: all 23 Manager specs are re-created, assertion counts match, and
CRD/namespace cleanup was correctly redistributed. Findings concentrate in the
new Phase-3 drain metrics and the new e2e helpers.

---

## 1. Code review

### Correctness (ranked)

- [ ] **1. Stale queue-depth gauge race — can hang the restart drain gate**
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

- [ ] **2. Discovery-lag causes spurious full re-snapshots**
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

- [ ] **3. Reconcile counter skipped on transient error / fired on no-op**
  — [manager.go:1384-1392](../../internal/watch/manager.go#L1384-L1392).
  `recordTargetReconcileCompleted` fires only after **both** `RequestRepoState`
  and `RequestClusterState` succeed. A transient error on the second `continue`s
  (counter never increments though partial state was enqueued) → restart
  `{pod=…} > 0` gate can time out at 90s on a one-off error. It also fires on a
  no-op snapshot, so `increase(target_reconcile_completed_total)` over-counts
  for non-test consumers.

- [ ] **4. Shutdown leaves the depth gauge non-zero**
  — [branch_worker.go:585](../../internal/git/branch_worker.go#L585),
  [:704](../../internal/git/branch_worker.go#L704).
  On `ctx.Done()` the loop finalizes + pushes and returns **without draining
  items still buffered in `eventQueue`**; each was counted into `inflightItems`
  at enqueue, so the final `recordQueueDepth` publishes a non-zero depth for the
  exiting pod that never clears. Shielded today by the per-pod gate; latent leak
  for any cross-pod use.

### Cleanup / efficiency

- [ ] **5. ~13 inlined `git pull` blocks duplicate `pullLatestRepoState`**
  — [watchrule_configmap_secret_e2e_test.go:174](../../test/e2e/watchrule_configmap_secret_e2e_test.go#L174)
  (+~6 more) and ~7 in `crd_lifecycle_e2e_test.go`. The canonical helper
  (fetch + checkout -B + reset --hard) is already used at
  [:707](../../test/e2e/watchrule_configmap_secret_e2e_test.go#L707) and is more
  robust than a bare `git pull` (which can flake on a diverged branch).
- [ ] **6. `gitPull` / `lastCommitMessageForPath` reinvent existing helpers**
  — [gittarget_isolation_e2e_test.go:187](../../test/e2e/gittarget_isolation_e2e_test.go#L187).
  Third pull implementation (no fetch/reset) + manual `git log` where
  `gitRun(dir, args...)` already exists in helpers.go.
- [ ] **7. Hand-rolled `minInt`** — [e2e_test.go:264](../../test/e2e/e2e_test.go#L264);
  used once, Go 1.26 builtin `min` covers it.
- [ ] **8. Redundant "healthy GitProvider" spec**
  — [watchrule_configmap_secret_e2e_test.go:80](../../test/e2e/watchrule_configmap_secret_e2e_test.go#L80)
  re-creates/re-verifies `gitprovider-normal` already built in `BeforeAll`
  ([:60-70](../../test/e2e/watchrule_configmap_secret_e2e_test.go#L60-L70)).
- [ ] **9. Duplicated endpoint→pod filtering**
  — [controller_basics_e2e_test.go:99](../../test/e2e/controller_basics_e2e_test.go#L99)
  vs [:135](../../test/e2e/controller_basics_e2e_test.go#L135); extract
  `expectServiceRoutesToPod`.
- [ ] **10. Wasted work** — CI "Summarize E2E timing reports" `go run`s
  spec-timings per report in a loop ([ci.yml:388](../../.github/workflows/ci.yml#L388)),
  recompiling N+1× (`go build -o` once instead); `currentRuleSetSnapshots`
  resolves GVRs twice per reconcile ([manager.go:1224](../../internal/watch/manager.go#L1224));
  `syncQueueDepthMetric` records every loop iteration rather than on change.

> Uncommitted working tree (Allure env-var overrides + `specSkippedByFilter` +
> per-process thread label) looks good. Caveat: `specSkippedByFilter`
> ([ginkgo-allure/main.go:263](../../test/e2e/tools/ginkgo-allure/main.go#L263))
> infers "filtered-out" from `LabelFilter!="" && Skipped && EndTime.IsZero() &&
> RunTime==0`; a real `Skip()` recording zero timing would also be dropped. Low
> risk — a heuristic, not a guarantee.

---

## 2. Docs triage

> `docs/finished/` already exists (empty) but isn't in the README — adopt it as
> the "done" folder and document it in [docs/README.md](../README.md).

### Move to `docs/finished/` — shipped/resolved

- [ ] [design/gittarget-isolation-on-rule-change.md](gittarget-isolation-on-rule-change.md) — ✅ shipped (this branch)
- [ ] [design/commit-window-refactor.md](commit-window-refactor.md) — implemented
- [ ] [design/e2e-cleanup-scoping.md](e2e-cleanup-scoping.md) — implemented
- [ ] [design/partial-object-audit-event-handling.md](partial-object-audit-event-handling.md) — implemented
- [ ] [design/path-scoped-bootstrap-template-design.md](path-scoped-bootstrap-template-design.md) — implemented
- [ ] [design/remove-gittarget-admission-webhook.md](remove-gittarget-admission-webhook.md) — webhook file gone → done
- [ ] [design/watch-gvr-resolution-cleanup.md](watch-gvr-resolution-cleanup.md) — `APIResourceCatalog` cleanup landed
- [ ] [design/watchrule-gvr-resolution-plan.md](watchrule-gvr-resolution-plan.md) — implemented
- [ ] [design/aggregated-api-requestheader-ca-miswire-report.md](aggregated-api-requestheader-ca-miswire-report.md) — resolved report
- [ ] [design/audit-consumer-username-attribution-gap.md](audit-consumer-username-attribution-gap.md) — resolved
- [ ] [design/e2e-runtime-state-vs-stamps.md](e2e-runtime-state-vs-stamps.md) — resolved failure mode
- [ ] [design/shallow-audit-event-misclassification.md](shallow-audit-event-misclassification.md) — root cause confirmed
- [ ] [design/e2e-test-review.md](e2e-test-review.md) — point-in-time review
- [ ] [git-author.md](../git-author.md) — OIDC author plan, implemented
- [ ] [future/design-commit-request-api.md](../future/design-commit-request-api.md) — implemented
- [ ] [future/design-rule-change-snapshot-trigger.md](../future/design-rule-change-snapshot-trigger.md) — implemented
- [ ] [future/design-audit-ingestion-hardening.md](../future/design-audit-ingestion-hardening.md) — implemented
- [ ] [future/design-commit-context-api.md](../future/design-commit-context-api.md) — superseded
- [ ] [future/idea-audit-enrichment-side-channel.md](../future/idea-audit-enrichment-side-channel.md) — superseded

### Consider removing / relocating

- [ ] [cozystack-bugreport.md](../cozystack-bugreport.md) — upstream bug report against
  CozyStack, not our docs. If filed upstream, replace with a link. Referenced by
  `initial-seed-via-watch-list.md`, so update that link; `serious-bug/` is the
  safe relocation.
- [ ] [ci/findings.md](../ci/findings.md) — stale (2026-02-13), overlaps
  [e2e-ci-stability-report.md](e2e-ci-stability-report.md). Fold true bits in, drop it.
- [ ] Empty `docs/finished/`, `docs/serious-bug/`, `docs/contributing/` — populate
  (per above) or remove the empty scaffolding.

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

**[e2e-allure-reporting.md](../ci/e2e-allure-reporting.md)** — accurate and
current. Only gap:

- [ ] Document the new per-process **`thread` Allure label** the converter now
  emits (the point of the parallel-insight work).

**[e2e-serial-registry.md](e2e-serial-registry.md)** — the Serial entries are
**correctness-driven (shared state), not flakiness-driven**, so improved
stability doesn't justify de-serializing directly. It *does* unlock the deeper
refactors the registry is implicitly waiting for:

- [ ] **Isolate the audit pipeline → de-serialize 4 of 8 entries.** `Audit Redis
  Queue/Consumer`, `Commit Window Batching`, `Commit Request` are Serial only
  because the webhook→Redis-stream→consumer pipeline is a global singleton.
  Per-test stream keys / consumer groups (same "isolate by name" move as the
  icecream CRD groups) lets all four run parallel. **Highest-leverage remaining
  speedup.**
- [ ] **Fix the discovery-lag re-snapshot (code finding #2) → de-serialize
  `crd_lifecycle`.** That spec is Serial because CRD install/delete forces
  unrelated GitTargets to resnapshot — the `manager.go` empty-plan / swallowed
  resolve-miss behavior. Fix at the source; name-isolated CRD groups then make
  the spec parallel-safe.
- `bi_directional` (exact commit-count) and `restart_snapshot` / `image_refresh`
  (singleton controller) are genuinely serial — leave them.

- [ ] **Confidence-window pass (the speedup plan's open thread).** Now that the
  race fixes landed (`03b95ab`, `0787b1b`), decide whether CI can move back off
  `K3D_AGENT_COUNT=0` / `procs=1` toward parallelism, using the uploaded timing
  summaries + Allure reports.
