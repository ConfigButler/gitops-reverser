# TODO

The open work, in one list. Each item is one or two lines with a code pointer or the design page
that argues it. Ranked within each group, most urgent first. **Before the next release** marks what
should land before the next tag is cut (the last release is v0.53.0). Checked against `main` at
`1c4fade4` on 2026-10-24.

## Before the next release

- [ ] **R1: an applier that comes due during a flush records nothing between the mark and its
  joining.** Its `flushEdits` starts when it joins, not at the mark, so an edit decided in between
  is neither published before the apply nor recorded; if the apply overwrites it, Git keeps the edit
  and the cluster runs the old value with no `Repair` pending. Start `flushEdits` for every applier
  when the flush begins, or take a new mark when the due set grows. `flushed()` in
  `internal/git/applier_claim.go`.
- [ ] **R4: a replaced branch worker reads the old worker's settled-snapshot record.**
  `forgetSettledSnapshots` runs only on `GitTarget` teardown (`internal/watch/materialization.go`),
  so after "Branch worker replaced" (`gittarget_controller.go`, the `RequestRecheckForGitTarget`
  path) the new worker's startup converge passes the barrier at once and can apply before the folder
  is rebuilt. Argo CD can then be asked for a path that does not exist.
- [ ] **Override #428's changelog entry.** Its `BREAKING CHANGE` footer describes
  `GitProvider.spec.accessFrom` and "every GitTarget writing one repository branch must name the same
  GitProvider", both replaced before release by `GitBranch` and grants (#429, #441). Add a
  `BEGIN_COMMIT_OVERRIDE` block to #428's PR body, as #419 did, before the release PR is cut.
- [ ] **Remove the "loud rejection" fields.** `RemovedReceiver` (`gitprovider_types.go`),
  `RemovedClusterProviderRef` (`gittarget_types.go`, `fluxclaim_types.go`, `argocdclaim_types.go`),
  `RemovedGitProviderRef`, `RemovedBranch` and `RemovedParentBranch` (`gittarget_types.go`) exist
  only to refuse an old spelling at admission. With no users yet, an API break is a hard cut; drop
  them and their CEL rules, and the matching sentences in [UPGRADING.md](UPGRADING.md).
- [ ] **F-i: CRD text beyond the contract.** `RepairPending` says "an API edit made after its
  flush", and `kubectl explain` never defines "flush" (`fluxclaim_types.go`, `argocdclaim_types.go`).
  Check `applierClaimRef`'s description for rationale too (AGENTS.md, API changes).

## Applier claims and bi-directional editing

- [ ] **Decide Fix 2 for live edits dropped while render fidelity is pending**: hold the events
  (2a, the user stays the author) or re-derive them with a resync (2b, the operator becomes the
  author), or keep the drop and alert on `git_writes_dropped_total`. It is also the cause behind the
  flaky audit-route override e2e spec.
  [live-edit-dropped-while-render-fidelity-pending.md](design/live-edit-dropped-while-render-fidelity-pending.md).
- [ ] **Reopen the render-fidelity gate after a Git repair.** A target stuck on
  `RenderMatchesLive=False` clears only on a re-declaration or a restart. `Invalidate` and
  `RequestRecheckForGitTarget` exist; their only caller is branch-worker replacement
  (`gittarget_controller.go`). Specified in
  [render-fidelity.md](design/support-boundary/render-fidelity.md) §6b; no decision needed.
- [ ] **Watch the held applier**, so a switch-on or a `flux resume` is undone at once instead of
  within the one-minute `heldClaimSteadyInterval`. Also makes ask #31 below exact.
  [applier-claim-held-applier-watch.md](design/applier-claim-held-applier-watch.md).
- [ ] **F-a: a held barrier is invisible.** A `GitTarget` with no planned collection holds its
  applier, and with it every reason of every `GitTarget` on that claim, `ForeignCommit` included,
  with no log, condition or metric. The hold is approved; the silence is the defect.
  (`internal/git/applier_claim.go`, `internal/watch/apply_state.go`)
- [ ] **F-e: the apply window compares two clocks.** `sentAt` is the operator's clock,
  `LastFieldManagedAt` the source API server's, at second granularity; a skew of 1s flips the echo
  decision. Add a skewed-clock test, and probably a one-second grace. (`internal/git/apply_window.go`)
- [ ] **F-b, F-c, F-d: edge cases of the snapshot hold.** Live events dropped at the gate while a
  discarded snapshot is held (F-b); an adopt lost while render fidelity is closed, because
  `mirrors()` returns before `noteCreated` (F-c); a `GitTarget` that names a claim after its
  snapshots settled empty never gets its folder (F-d, `resync_flush.go`).
- [ ] **F-h: small leaks and races.** The 5-minute hold can restart; the `Initial` flag races
  settlement; `snapshotHeldSince`, `applierState.named` and dropped collections in
  `settledSnapshots` leak.
- [ ] **F2: accept a Flux fetch whose token was stamped over** when the artifact holds the commit.
  Today `Flux.fetch` waits only for its own token, so a commit another stamp fetched fails after 30s
  (`internal/applier/flux.go`). Also check F3: the hold is checked after the fetch, so a
  `Kustomization` resumed by hand applies the fetch at once.
- [ ] **A2: judge a gone Argo CD token by `status.history`** (our commit,
  `initiatedBy: gitops-reverser`, `deployStartedAt` at or after our request means `Succeeded`).
  Today a manual sync that wipes our token after a success reads as failed and syncs again, so hooks
  run twice (`internal/applier/argocd.go`).
- [ ] **Check Argo CD sync windows in `Configured`.** The guide says a sync window must not block
  GitOps Reverser's syncs; nothing checks it.
- [ ] **Refuse or flag overrides that are not in Git** (Argo `spec.source.kustomize`; Flux
  `spec.images`, `patches`, `targetNamespace`): they change what is applied where the fidelity check
  cannot see. `applicationMismatch` checks only repo and revision. Needs a decision;
  [render-fidelity.md](design/support-boundary/render-fidelity.md) §8.
- [ ] **One holder per applier across `KubeProvider`s** (cluster identity or an object-local stamp).
  [idea-applier-claim-ownership.md](future/idea-applier-claim-ownership.md).
- [ ] **A targeted review of `internal/applier/`.** CodeRabbit skipped #420 (over its 100-file
  limit), so the largest new package has had no bot review.
- [ ] Later: claim an Argo CD `ApplicationSet`
  ([idea-argocd-applicationset-claim.md](future/idea-argocd-applicationset-claim.md)); a webhook
  applier, a third claim kind ([apply-trigger.md](design/apply-trigger.md), "Where this goes next").

## The gitops-api team's asks

Their argument for each is in [gitops-api-open-asks.md](future/gitops-api-open-asks.md).

- [ ] **#32**: a troubleshooting row in [bi-directional-flux.md](bi-directional-flux.md) for a
  controller that replaces the `Kustomization`'s whole spec with an Update. Docs only.
- [ ] **#31**: a metric for an apply the claim did not start; today it is only the
  `ClaimedResourceReconciled` Event, which the API server aggregates and expires.
- [ ] **#34**: an e2e with an in-cluster applier and a remote source cluster, provoking one apply
  window collision.
- [ ] **#23**: verify with their reproduction that #431 fixed deletion-as-intent naming the
  finalizing controller instead of the deleter.
- [ ] **#11**: one YAML encoder for `[CREATE]` and `[UPDATE]` bodies (indent churn).
- [ ] **#28**: `--watch-namespaces`, then an owner selector, so two instances can share one
  config-plane cluster. It is the same mechanism HA needs.
- [ ] **#10**: consider the namespace when placement follows siblings. Cosmetic.

## Attribution

- [ ] **Option F: stop waiting for a fact a (route, type) has never produced.** A removal on a type
  the audit policy drops spends the full grace for nothing.
  [attribution-removal-wait-options.md](design/attribution-removal-wait-options.md).
- [ ] **Release attribution per object, not per watch (head-of-line).** One slow lookup delays
  every later event on its stream. Before relying on per-object order, check three places: the
  watch cursor may advance only to the oldest unreleased event; the `producerGate` handoff on
  retire and replace, and the snapshot-before-live boundary; and the apply window, which classifies
  an edit by when it reaches the worker.
  [The ordering spec](spec/watch-event-ordering-and-attribution-grace.md#preserve-ordered-release-if-attribution-becomes-concurrent).
- [ ] Two doc sentences: no condition detects an API server's 30s default
  `--audit-webhook-batch-max-wait` (say so in the `KubeProvider` docs), and a relist after a watch
  gap ships its changes as the configured author (say so in the operator docs).
- [ ] Optional: capture `CommitRequest` authors without Redis in single-pod mode, with an in-memory
  `CommandAuthorStore`. The only thing that still forces Redis on one replica. Low priority, and
  only useful until HA.

## Branch worker and CommitRequests

- [ ] `expireWaitingCommitRequests` ranges over a map, so empty commits for saves that expire in
  the same pass land in random order; sort by `seq` (`internal/git/commit_request_attach_loop.go`).
- [ ] `buildRequestRecordWrite` keeps only the username (`UserInfo{Username: pcr.author}`); carry the
  full `UserInfo` so a record commit has the same author identity as a window commit.
- [ ] A record commit skips `requestTemplate`, though its doc comment says it is phrased like every
  other commit; fix the code or the comment.
- [ ] Nits: a doubled doc comment on `commitWindowFor`; "close-delay" in the test names in
  `internal/controller/commitrequest_kstatus_test.go`.
- [ ] **Parent observation, step 1**: keep parent availability and its timestamp after the write
  branch exists, from existing advertisements, with no extra connections.
  [gittarget-parent-observation.md](design/gittarget-parent-observation.md#implementation-and-validation).
- [ ] **Finish making a `GitTarget` turn red quickly and say why no commit was made**: the broader
  reason and status contract table remains.
  [gittarget-red-status-plan.md](design/gittarget-red-status-plan.md).
- [ ] **Transition boundary** in the branch worker, the prerequisite for the durable queue.
  [branch-worker-event-model.md](design/branch-worker-event-model.md).
- [ ] **Make the worker queue durable**, and advance the watch cursor only after a durable worker
  acknowledgment. Today a crash after cursor advancement and before the write lands in Git can skip
  work on restart.
- [ ] **High availability**: leader or ownership coordination so two replicas never write the same
  `GitTarget`, plus the durable queue, plus whether a replica warms its fact index before starting a
  watch it took over. Redis becomes required in this mode.
  [ha-gittarget-distribution-plan.md](future/ha-gittarget-distribution-plan.md).
- [ ] Re-assess unifying the pending-write kinds
  ([idea-unify-pending-write-kinds.md](future/idea-unify-pending-write-kinds.md), now four kinds,
  not two). Deferred.
- [ ] Improve queue and worker observability, with more high-load tests.

## Watch plane

- [ ] **Subscribe the watch plane to `typeset.Registry` lifecycle events.** `Registry.Subscribe`
  has no production observer, so nothing sweeps the manifests of an uninstalled CRD, and since #419
  the operator asks Flux or Argo CD to apply that folder. Its consumer is the `stop` classification
  in [target-watch-plan.md](design/target-watch-plan.md), "What a collection leaving means".
- [ ] **Drop the "TEMPORARY at Info" logs to V(1)** (`gittarget_controller.go`,
  `watch/render_fidelity_gate.go`, `watch/event_router.go`), now that Failure A of
  [watch-plane-status-convergence-failures.md](design/watch-plane-status-convergence-failures.md)
  is fixed.
- [ ] Pin with a test that an unforced retention pass keeps a scope's revision, so only
  `Invalidate` re-issues one (`internal/git/render_fidelity_gate.go`), and close the retention
  revision-mismatch question in
  [watch-plane-status-convergence-failures.md](design/watch-plane-status-convergence-failures.md) §3.
- [ ] Filter more cluster-generated noise, such as `kube-root-ca.crt`; decide which objects and
  whether a user can opt back in (`resource_policy.go` is a type-level denylist only).
- [ ] A manifest whose CRD is missing refuses the whole folder (`IssueUnresolvedKRM`,
  `manifestanalyzer/acceptance.go`): keep that, or defer only that document. The scan already
  records `unresolved-mapping` instead of failing.
- [ ] Decide whether the settle window should be configurable (`settleWindow` 2s, `maxSettleWait`
  10s in `internal/watch/owner.go`). Revisit only on a report that config changes feel slow.
- [ ] Reduce duplication between the `WatchRule` and `ClusterWatchRule` code paths where it makes
  sense.
- [ ] Later: a namespace selector in a `KubeProvider` grant's scope
  ([idea-source-namespace-selector.md](future/idea-source-namespace-selector.md)).

## Support boundary and Git content

- [ ] **Scalar strategic-merge patch authoring**: an env var or a limit edited in one overlay is
  still refused, and since #419 the user sees the edit reverted. Decide "we author patches", then
  about two weeks. [patch-authoring.md](design/support-boundary/patch-authoring.md).
- [ ] Decide how SOPS rules cover sensitive custom resources that are not Secret-shaped (fields such
  as `spec.credentials`): an explicit field policy, or full-file encryption.
- [ ] Should comments survive a SOPS re-encrypt? (e2e Test C in
  [e2e-coverage-gaps-and-improvements-plan.md](design/e2e-coverage-gaps-and-improvements-plan.md)).
- [ ] Decide the three open layout questions in [layout/model.md](layout/model.md#open-questions):
  nested roots per directory, refusing rather than reporting when a governing root disappears, and
  a CRD default for `placement.default`. (The fourth, a shared `GitProvider`, is answered by
  `GitBranch` and grants.)
- [ ] Preserve more user-facing file structure: comments, ordering and other low-noise formatting.
- [ ] **Values Move 2**, the editable values projection
  ([values-file-projection.md](design/support-boundary/values-file-projection.md)).
- [ ] **Least privilege**: never mirror Secrets, permission-aware followability, an RBAC generator
  ([least-privilege-remaining-work.md](future/least-privilege-remaining-work.md)).
- [ ] Support-boundary gaps: the `Generated{path}` claim, Argo `directory.exclude`, paths written by
  ImageUpdateAutomation, the provenance and ownerReference gate, a Helm chart skipped as a unit, the
  Overlay-Diverged trailer.

## E2E and CI

- [ ] **Fix the recurring full-e2e flakes** around WatchRule and snapshot convergence (SOPS
  bootstrap, signing snapshot messages). Notes in the [e2e serial registry](spec/e2e-serial-registry.md).
  Address before the next feature that widens commit-message, snapshot or write-window behavior.
- [ ] **Run image-refresh only when its inputs change**, a path filter in `ci.yml`; it removes a
  14–18 minute leg from most PRs. [e2e-suite-trim.md](design/e2e-suite-trim.md).
- [ ] **F-f: E6 can leak its editing goroutine**: `close(stop)` runs only after the `Eventually`
  succeeds. Use `DeferCleanup` (`test/e2e/bi_directional_echoes_e2e_test.go`).
- [ ] The e2e detail gaps and redundancy tables in [e2e-suite-trim.md](design/e2e-suite-trim.md):
  four specs that look tested and are not, then the moves and merges.
- [ ] `SetDefaultEventuallyTimeout` is global and set in 17 e2e files, so the last `Describe` body
  built wins; use per-spec timeouts (`Eventually(...).WithTimeout`).
- [ ] e2e Test A: a delete missed while the controller is scaled to 0; then Tests B and C.
  [e2e-coverage-gaps-and-improvements-plan.md](design/e2e-coverage-gaps-and-improvements-plan.md).
- [ ] Re-enable `goconst` with a path-scoped exclusion (`test/`, `internal/git/commit.go`) instead
  of the repo-wide disable in [.golangci.yml](../.golangci.yml).
- [ ] Release-image reuse PRs 2, 3 and 5 ([release-image-reuse-plan.md](design/release-image-reuse-plan.md)).
- [ ] Metrics Phase 5: dashboard JSON, recording and alert rules
  ([metrics-observability-plan.md](design/metrics-observability-plan.md) §7).
- [ ] Use [bestpractices.dev](https://www.bestpractices.dev/en) as a maturity checklist, and turn the
  useful items into concrete work.

## Docs and vocabulary

- [ ] **Stale design-doc headers**, one docs PR: [open-asks-priority.md](design/open-asks-priority.md)
  (#339 closed, #23 fixed), [gittarget-api-wave.md](design/gittarget-api-wave.md) (riders shipped),
  [git-write-preflight.md](design/git-write-preflight.md) (superseded by the red-status plan),
  [commitrequest-save-wait-options.md](design/commitrequest-save-wait-options.md) (written against
  `closeDelay`), [gittarget-red-status-plan.md](design/gittarget-red-status-plan.md) (steps 5 and 6
  built), [watch-and-catalog-architecture.md](design/watch-and-catalog-architecture.md)
  (superseded), [target-watch-plan.md](design/target-watch-plan.md) (queue depth is 1000, not 100),
  and the go-git version in [azure-devops-multi-ack.md](design/azure-devops-multi-ack.md) and
  [gittarget-parent-empty-repository.md](design/gittarget-parent-empty-repository.md).
- [ ] **Wrong claims in design bodies**: [support-contract.md](design/support-boundary/support-contract.md)
  calls overlays, values files and ImageUpdateAutomation paths refused;
  [acceptance-precision.md](design/support-boundary/acceptance-precision.md) says everything is
  open; [admission-consent.md](design/support-boundary/admission-consent.md) says the preflight
  "fails closed" (it was decided fail-open, and is not built);
  [multi-source-audit-ingress-hardening.md](design/multi-source-audit-ingress-hardening.md) says the
  server checks the named provider exists; [event-pipeline-overview.md](design/event-pipeline-overview.md)
  has no applier-claim stage.
- [ ] **`(GVR, scope)` is still in [architecture.md](architecture.md)** (nine times);
  [definitions.md](definitions.md) says "resource collection" and retires the old spelling. A
  mechanical reword, as its own docs PR.
- [ ] Settle two words: "replay" in [applier-echoes.md](design/applier-echoes.md) departure 11
  (definitions.md says "initial events"), and "sweep" against "prune" for the act of removing.

## Future directions

- [ ] A `Writable` condition on `GitProvider`. A push session that sends nothing proves the server
  opens one for the credential, not that a write is accepted (branch protection and pre-receive
  hooks apply at ref update), so a green probe is necessary and not sufficient. `False` only on an
  explicit refusal; a failed probe never clears a proven denial. Gating `Ready` on it is a separate
  release. (`GitBranch` now opens a push session before it holds a branch, which covers part of
  this.)
- [ ] Simpler setup flows, including more Git provider bootstrap automation.
- [ ] Constrained reverse actions for simple, known Kustomize-style mutations.
- [ ] Better branching and promotion strategies.

Research:

- Replace the metrics mechanism with the
  [VictoriaMetrics operator](https://docs.victoriametrics.com/helm/victoria-metrics-operator/), so it
  is also Helm and has proper dependencies.
- Read up on how resource versions work and can work in the HA rebuild:
  <https://kubernetes.io/docs/reference/using-api/api-concepts/#resource-versions>.
