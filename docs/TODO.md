# TODO

The open work, in one list. Each item is one or two lines with a code pointer or the design page
that argues it. Ranked within each group, most urgent first. **Before the next release** marks what
should land before the next tag is cut (the last release is v0.55.0). Checked against `main` at
`1bc97904` on 2026-10-24.

## Before the next release

- [ ] **Test the failure boundaries of concurrent commit windows** (stage 3 of
  [concurrent-author-windows.md](design/concurrent-author-windows.md#stages)). Stage 2 ships with no
  flag: a branch worker keeps a window per author whenever their footprints are proven disjoint.
  Before it is released, the reference model (`internal/git/window_model_test.go`) must cover both
  close orders, conflicting writes, delete and recreate, `attach_next`, zero-duration windows, a
  resync, shutdown, failed publication and replay, and buffer pressure, each with several windows
  open. The reviews of stages 1 and 2 add:
  - **gate:** the model retains work across failed pushes and retries before several windows are
    released. Today it publishes at once after every wake (`windowModelRun.wake`) and asserts no
    pending writes after it (`drained`), so decided-but-uncommitted writes with windows open
    (cooldown, failed push, replay) are never modeled;
  - reach floors the model asserts: shutdown, flush and push hint with two or more windows open,
    `attach_next` with others open, a reproof overlap, `path_conflict`; a concurrency-biased profile;
    CI raised to about 20 seeds × 60 steps;
  - model steps: a shutdown mid-run; a timer overdue but not yet delivered (the `!isOpen` guard in
    `attachWaitingCommitRequests` survives its removal); a late stale applier write (the model
    never withholds);
  - the GitTarget-generation and checkout-reset parts of a footprint revision are untested;
  - a counter for window fallbacks (`footprint_fallback` counts the windows it closes, not the
    fallback).

## Applier claims and bi-directional editing

- [ ] **Restart recovery: persist current state.** A graceful stop pushes its windows and writes the
  outcomes of the saves they carried (`CommitRequestsInFlight.Settle`); everything else the branch
  worker keeps in memory is lost on any restart, and a crash, or a stop past its grace period, loses
  all of it. Nothing is persisted. What must survive: the
  open windows with absolute deadlines, author, attribution, message and membership; each attached
  save's `attachDeadline`, `seq` and window; save outcomes until their TTL; the pending writes; the
  withheld-field records (`internal/git/withheld_fields.go`: after a restart the next edit of such an
  object carries the apply's values into Git again, until the `Repair` lands). "Git took the push,
  then the process died" is made safe by writing a push intent {old, new, request→SHA} before every
  `PushAtomic` and comparing it with the remote tip on restart: commits carry no trailers, and SHAs
  are not reproducible. Two restore traps: the startup cold replay (no Redis) closes any restored
  window (`resync_flush.go`), and `finalizeWindow` drops a window while render fidelity is Unknown,
  which it is after a restart. The worker state root `/tmp` is an `emptyDir` a new pod loses.
  [branch-worker-event-model.md](design/branch-worker-event-model.md#saves-lose-execution-identity-across-a-restart).
- [ ] **Gaps in the withheld-field record** (`internal/git/withheld_fields.go`): a field patch in the
  apply window forgets the object's accepted version, so a withhold over it records nothing;
  accepted versions held
  for render fidelity (`recordUnpublishedEdits`) and resyncs with `Initial: false` are untested;
  no counter shows how many fields are withheld.
- [ ] **Reopen the render-fidelity gate after a Git repair, automatically.** Today a target on
  `RenderMatchesLive=False` clears only on an explicit recheck (the
  `reconcile.configbutler.ai/requestedAt` annotation, which restarts every scope; the render
  fidelity e2e spec proves it recovers a repaired folder), a re-declaration or a restart. Doing it on
  an incoming Git revision is more than calling `Invalidate` and `RequestRecheckForGitTarget` from a
  new place: it first needs the retained-intent ordering barrier, so the source refresh cannot
  discard or race an open commit window or pending live edits
  ([render-fidelity.md](design/support-boundary/render-fidelity.md) §6b).
- [ ] **Watch the held applier**, so a switch-on or a `flux resume` is undone at once instead of
  within the one-minute `heldClaimSteadyInterval`. Also makes `apply_claimed_resource_reconciled_total`
  (ask #31) count at the moment of the resume.
  [applier-claim-held-applier-watch.md](design/applier-claim-held-applier-watch.md).
- [ ] **F3: a Flux fetch is made before the hold is checked**, so a `Kustomization` resumed by hand
  applies the fetched artifact at once (`internal/applier/flux.go`, `Request`).
- [ ] **The apply window's remaining clock limits** (`internal/git/apply_window.go`, file comment).
  A server clock ahead by more than the time between an earlier apply and the send dates an undo of
  an edit published before the send after it, so the undo is not mirrored and the next apply reverts
  it. With an edit carried from the window before, the check against the send still mixes the two
  clocks, and a server clock behind can withhold this attempt's own apply and ask for an unneeded
  `Repair`. Only comparing the event's content with Git separates them.
- [ ] **F-h: small leaks and races.** The 5-minute hold can restart; the `Initial` flag races
  settlement; `snapshotHeldSince`, `applierState.named` and dropped collections in
  `settledSnapshots` leak.
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

- [ ] **#34**: an e2e with an in-cluster applier and a remote source cluster, provoking one apply
  window collision.
- [ ] **#10**: consider the namespace when placement follows siblings. Cosmetic.
- **#28** (two instances sharing one cluster) is deferred, not open work: one instance serving many
  namespaces is the tenancy model, and the restart cost behind the ask is the durable queue's.

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
  (grouped window, resync and request record, since #450 removed the atomic kind). Deferred.
- [ ] Improve queue and worker observability, with more high-load tests.

## Watch plane

- [ ] **Subscribe the watch plane to `typeset.Registry` lifecycle events.** `Registry.Subscribe`
  has no production observer, so nothing sweeps the manifests of an uninstalled CRD, and since #419
  the operator asks Flux or Argo CD to apply that folder. Its consumer is the `stop` classification
  in [target-watch-plan.md](design/target-watch-plan.md), "What a collection leaving means".
- [ ] Pin with a test that an unforced retention pass keeps a scope's revision, so only
  `Invalidate` re-issues one (`internal/git/render_fidelity_gate.go`), and close the retention
  revision-mismatch question in
  [watch-plane-status-convergence-failures.md](design/watch-plane-status-convergence-failures.md) §3.
- [ ] Filter more cluster-generated noise beyond `kube-root-ca.crt` (always left out, no opt-in:
  `internal/watch/cluster_generated.go`), such as OpenShift's `openshift-service-ca.crt`, once
  measured on such a cluster.
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
- [ ] e2e for the commit-window stack: two authors editing disjoint files at once, then a
  `path_conflict`; E5 extended to the withheld-field carry; a restart of a source cluster's
  controller with a save in flight; `catchUp: AfterPush` at shutdown.
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
  (#339 closed, #23 fixed),
  [git-write-preflight.md](design/git-write-preflight.md) (superseded by the red-status plan),
  [gittarget-red-status-plan.md](design/gittarget-red-status-plan.md) (steps 5 and 6
  built), [target-watch-plan.md](design/target-watch-plan.md) (queue depth is 1000, not 100),
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
