# Merge Readiness Inventory

Snapshot taken from branch `poc/redis-copy` on 2026-06-18.

This note inventories the large rewrite branch and separates what must be settled before merge from
what can safely become follow-up work. Some details describe the worktree at the time of inventory:
the branch had committed work plus staged and unstaged local edits.

## Branch shape

- `poc/redis-copy` was 122 commits ahead of `origin/main` and 0 behind.
- Compared with `origin/main`, the branch touched roughly 396 paths with about 63k insertions and
  14.5k deletions.
- The biggest areas of churn were `internal/`, `docs/`, `test/e2e/`, API/CRDs, Helm/config, and CI.
- Local edits were present in the worktree. Some files had both staged and unstaged changes.
- A later Markdown inventory found 68 newly added Markdown paths, about 23k lines total. See
  [`markdown-triage-inventory.md`](markdown-triage-inventory.md).

## Executed work

### Core architecture

- Reworked synchronization around demand-driven, per-resource-type materialization.
- Added the `internal/typeset` layer for demand/followability/lifecycle ownership.
- Reworked `internal/watch` around watched type tables, type materialization, object mirrors,
  checkpoints, type lifecycle, watermarks, and per-type tailing.
- Removed or retired older broad reconciler/informer paths in favor of the per-type model.
- Added readiness behavior that depends on live audit ingress and Redis health.

### Queue and audit ingestion

- Replaced the single large audit consumer path with per-type Redis streams and object snapshots.
- Added `RedisByTypeStreamQueue`, per-type stream keys, idstate counters, trimming, delete-type
  cleanup, and splice/fold helpers.
- Added audit event parsing and outcome classification.
- Added demand-gated audit ingestion so only claimed and followable types are mirrored.
- Added late/out-of-order handling, high-water behavior, and materialization nudges.
- Added the unified audit outcome metric:
  `gitopsreverser_audit_events_total{outcome,category,group,version,resource,verb}`.

### Git and manifest writing

- Added manifest-aware editing under `internal/git/manifestedit`.
- Added manifest analysis/reporting under `internal/manifestanalyzer` and `internal/manifestreport`.
- Added file-agnostic placement support and many tests for additions, patching, merge behavior,
  keyed lists, comments, framing, and known edge cases.
- Added reconcile/flush paths for mark-and-sweep style reconciliation.
- Improved commit request attachment/finalization behavior and branch worker behavior.

### CommitRequest workflow

- Added controller-driven, audit-attributed finalization.
- Added a delayed window close via `spec.closeDelaySeconds`.
- Added resolve-on-push so committed requests get the pushed SHA.
- Added terminal `Rejected` outcomes with machine-readable reasons.
- Added eager message attach and more unit/e2e coverage around commit windows.

### API and user-facing behavior

- `GitTarget.spec.path` is now required and immutable.
- `GitTarget.spec.providerRef`, `spec.branch`, and `spec.path` are immutable destination fields.
- `GitProvider.spec.url` is immutable.
- `GitProvider.spec.knownHostsRef` was added.
- `GitProvider.spec.commit.message.snapshotTemplate` became `reconcileTemplate`.
- Flux `GitRepository` enum values were removed from `GitTarget.spec.providerRef`; the reference is
  now only to `configbutler.ai/GitProvider`.
- `GitTarget.status` gained phase/materialization roll-up fields.
- `CommitRequest.status` reports kstatus-compatible **conditions** (`Ready`, `Reconciling`, `Stalled`,
  `Attributed`, `Pushed`) plus `observedGeneration`; the former `phase`/`reason`/`message`/`observedTime`
  fields were removed.

### Git credentials and security

- Added support for Flux- and Argo-shaped Git credential Secrets.
- Added HTTP bearer token support.
- Removed per-Secret `insecure_ignore_host_key`.
- SSH now fails closed unless known hosts material is supplied, except for the explicit
  install-level `--insecure-allow-missing-known-hosts` development flag.
- Added `docs/security-model.md` and upgrade guidance for credentials/providerRef changes.

### CI, test, and docs

- Added Codecov configuration and unit/e2e coverage uploads.
- Added `.coverage-baseline` and a local coverage ratchet through `task cover-check`.
- Reworked e2e task structure and added e2e coverage collection.
- Added or expanded e2e tests for commit requests, status, overlap, scale subresource handling,
  inplace editing, restart reconcile behavior, signing, and GitTarget isolation.
- Added a large set of design, finished, fact, and future docs that now describe the new system.
- Added a Markdown triage checklist for all newly added Markdown files.

## Documentation check

The new Markdown set is useful, but it is not yet tidy enough to ignore during merge prep:

- `docs/UPGRADING.md` is user-facing and currently covers the Git credentials/providerRef break, but
  not the other API/status/template breaks listed below.
- Several active `docs/design/**` files still describe proposed or partially implemented work:
  manifest inventory, GitTarget placement/repository validity, new-file placement, contextual
  namespace/Kustomize support, manifest writer follow-ups, materialization/liveness review work,
  HA notes, late-lane analysis, and signing investigations.
- Many files under `docs/finished/` are correctly historical, but some still contain open questions
  or explicitly deferred work. That is fine if the deferred items are copied into `docs/TODO.md` or
  intentionally left as history.
- The stream docs now lean toward the chosen direction that the per-type late lane is removed, but
  historical sections and some code comments still use late-lane language. That should be scrubbed
  or clearly marked as historical to avoid a future reader treating removed behavior as current.

## Pressing merge blockers

1. **Make diagnostic divert semantics consistent.**
   The current docs and code lean toward the chosen model: the per-type late lane is removed, and a
   diverted event is represented by `audit_events_total`, `lateNotify`, and optional `diag_all`.
   Before merge, remove stale `diag_late`/late-lane behavior from code, tests, comments, and docs, or
   mark historical sections very clearly. `LookupCommitRequestAuthor` should scan only the main
   stream if the removal model stands.

2. **Clean the worktree.**
   Decide what belongs in the branch, commit it, and remove local-only edits such as
   `.claude/settings.local.json` if they are not intentional.

3. **Fix markdown/code hygiene.**
   `git diff --check` reported an extra blank line at EOF in
   `internal/queue/redis_bytype_queue_test.go`.

4. **Regenerate generated artifacts.**
   API and webhook/config changes mean `task generate` and `task manifests` need to be run and any
   resulting CRD/RBAC/generated-code changes reviewed.

5. **Finish migration documentation.**
   `docs/UPGRADING.md` should cover all breaking changes, not only credentials/providerRef:
   required `GitTarget.spec.path`, immutable destination fields, `snapshotTemplate` to
   `reconcileTemplate`, the CommitRequest status move from `phase`/`reason` to conditions
   (`Ready`/`Reconciling`/`Stalled`/`Attributed`/`Pushed`), and other status shape changes.

6. **Triage the new Markdown set.**
   Use [`markdown-triage-inventory.md`](markdown-triage-inventory.md) to mark each new Markdown file
   as Keep, Finished, Future, Summarize, or Remove. This does not require polishing every historical
   note, but it does require avoiding contradictory active docs and moving true follow-ups into
   `docs/TODO.md` or `docs/future/`.

7. **Validate CI workflow changes.**
   The branch edits `.github/workflows/ci.yml`, so `task lint-actions` is required in addition to
   the normal validation suite.

8. **Run the full merge gate.**
   Because this branch changes Go code, CRDs, Helm/config, CI, shell/e2e behavior, and generated
   manifests, the merge gate is:

   ```bash
   task fmt
   task generate
   task manifests
   task vet
   task lint
   task test
   task lint-actions
   docker info
   task test-e2e
   ```

## Loose ends suitable for follow-up

- Prevent same-repository write collisions across multiple `GitProvider` objects.
- Reduce sensitive `payload_json` data persisted in Valkey.
- Filter more cluster-generated noise, such as `kube-root-ca.crt`.
- Decide how SOPS rules should cover sensitive custom resources that are not Secret-shaped.
- Improve output layout control and multi-resource-per-file support.
- Preserve more user-facing file structure, including comments and ordering.
- Handle manifests whose GVK cannot be resolved against the live cluster.
- Resolve the unused `GitTarget.status.lastCommit` field.
- Re-enable `goconst` with path-scoped exclusions.
- Continue improving queue and worker observability under load.
- HA/multi-replica ownership and failover hardening.
- Watch-first checkpoint LIST replacement and related materialization polish.
- Manifest writer follow-ups such as duplicate/orphan pruning and more configurable placement.
- CRD `/scale` remap from custom scale paths.

## Action plan

### Phase 0: Freeze the branch

- Stop adding features on `poc/redis-copy`.
- Treat new discoveries as blockers only if they affect correctness, migration, validation, or
  release safety.
- Keep all polish-only items in `docs/TODO.md` or a follow-up issue.

### Phase 1: Settle the current worktree

- Choose one diagnostic model:
  - current direction: remove the per-type lane and rely on counters, `lateNotify`, and opt-in
    `diag_all`.
  - fallback only if deliberately reversed: keep per-type `:audit:diag_late` plus opt-in
    `diag_all`.
- Make `internal/queue`, queue tests, `cmd/main.go`, Helm values/templates, e2e diagnostics, and
  stream docs match the chosen model.
- Remove accidental local files/edits.
- Fix `git diff --check`.
- Commit the settled changes in one focused stabilization commit.

### Phase 2: Close API, migration, and documentation gaps

- Update `docs/UPGRADING.md` for every breaking user-facing change.
- Confirm README, chart README, `docs/configuration.md`, and samples all show required
  `GitTarget.spec.path` and `reconcileTemplate`.
- Work through [`markdown-triage-inventory.md`](markdown-triage-inventory.md):
  keep canonical/current docs, move completed design notes to `docs/finished/`, move future work to
  `docs/future/` or `docs/TODO.md`, and summarize or remove stale duplicate planning notes.
- Specifically check active design docs that still say proposed, open, investigation, partially
  implemented, deferred, or not scheduled. Either make them clearly historical/future or confirm they
  are intentionally active.
- Run `task generate` and `task manifests`; review CRDs/RBAC/chart/config output.
- Verify Helm values and config deployment flags line up with `cmd/main.go`.

### Phase 3: Run local validation

- Run `task fmt`.
- Run `task vet`.
- Run `task lint`; if it fails, run `task lint-fix`, inspect changes, and rerun `task lint`.
- Run `task test`; commit `.coverage-baseline` if the ratchet raises it.
- Run `task lint-actions`.
- Confirm Docker with `docker info`.
- Run `task test-e2e` sequentially.

### Phase 4: Triage validation results

- If unit tests fail, fix code or tests before touching e2e.
- If e2e fails once on known timing/flakiness signatures, capture artifacts and rerun once.
- If the same e2e failure repeats, treat it as a blocker and fix before merge.
- If coverage rises, commit the new `.coverage-baseline`.

### Phase 5: Merge prep

- Squash or group the final stabilization commits only if it improves reviewability.
- Make the PR description match this inventory: architecture rewrite, breaking changes, validation
  commands, and explicit follow-ups.
- Mark deferred items as follow-up issues, not hidden TODOs.
- Merge when the full gate is green and the worktree is clean.

## Validation already sampled

- `docker info` succeeded, so e2e was not blocked by a missing Docker daemon.
- `go test -count=1 ./internal/queue ./internal/webhook` passed.
- `git diff --check` failed on whitespace, so that remains a pre-merge cleanup item.
- Markdown triage metadata was generated without reading file contents, then a follow-up pass read
  status/headings/open-question markers from the added Markdown set and confirmed documentation
  triage should be part of merge prep.
