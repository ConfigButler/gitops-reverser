# The Argo CD applier: implementation plan

> **plan**: written 2026-10-17, to be built in a fresh session on #419's branch
> (`docs-bi-directional-right`), before it merges. Delete it
> once the PR lands; the lasting design goes into [`apply-trigger.md`](apply-trigger.md).
> Index: [`../INDEX.md`](../INDEX.md).

**The standard: GitOps Reverser takes full control of when Argo CD syncs.** An `Application` named
by a `GitTarget` has automated sync switched off, and every sync it runs is one GitOps Reverser
asked for, pinned to a commit it checked. This is the supported setup for bi-directional editing
with Argo CD, not an option next to automated sync.

Argo CD is the easier half of the apply trigger. Flux has no fetch-only mode: a new artifact applies
at once. With automated sync off, Argo CD's timed refresh and its Git webhook only **compare**, so
they can stay on and do no harm, and nothing syncs but us. There is no stale artifact to catch up,
so Argo CD gets no `CatchUp` (already decided; do not re-raise).

## Start here (new session)

- Read [`apply-trigger.md`](apply-trigger.md) first: the worker owns every obligation per applier,
  the dispatcher makes one attempt and reports back, and quietness is per repository and branch.
  None of that changes.
- The Flux applier is the template to copy: [`internal/applier/flux.go`](../../internal/applier/flux.go)
  (`Validate`, `Request`, `Handled`), [`internal/applier/dispatcher.go`](../../internal/applier/dispatcher.go),
  [`internal/git/apply_trigger.go`](../../internal/git/apply_trigger.go),
  [`internal/git/apply_window.go`](../../internal/git/apply_window.go),
  [`internal/controller/gittarget_applier.go`](../../internal/controller/gittarget_applier.go).
- The Argo CD upstream checkout is in the gitignored `external-sources/argo-cd` (3.5.0 at
  `3cbae653d`). Read it as ground truth; never link to it from a tracked doc.
- The e2e corner that installs Argo CD is `task test-e2e-bi-directional`
  ([`argocd_bi_directional_e2e_test.go`](../../test/e2e/argocd_bi_directional_e2e_test.go)). It
  occasionally dies on an ECR rate limit pulling `argocd-redis`; rerun later, that is not the diff.

## Facts verified in the upstream source

| Fact | Where (external-sources/argo-cd) | Consequence |
| --- | --- | --- |
| Every apply writes as field manager `argocd-controller`, client-side and server-side alike | `gitops-engine/pkg/sync/sync_context.go` passes `serverSideApplyManager` to `ApplyResource`; `gitops-engine/pkg/utils/kube/resource_ops.go:532` sets it as `FieldManager` | The apply window works as for Flux, with `argocd-controller` in place of `kustomize-controller`. `kubectl-client-side-apply` is only the manager Argo CD migrates *from* |
| Autosync builds `Operation{Sync{Revision, Prune: automated.GetPrune(), SyncOptions, Source(s), Revisions}, InitiatedBy{Automated: true}, Retry: spec.syncPolicy.retry or {Limit: 5}}` | `controller/appcontroller.go:2628` | Build ours the same way, with `Revision` pinned to the checked commit |
| Setting an operation refuses while one is set (`ErrAnotherOperationInProgress`) | `util/argo/argo.go` `SetAppOperation` | Never replace a running operation; wait for it |
| Automated sync is on when `syncPolicy.automated` is present and `enabled` is unset or true | `pkg/apis/application/v1alpha1/types.go` `IsAutomatedSyncEnabled` | `AutomatedSyncEnabled` is exactly this rule; `automated: {enabled: false, prune: …}` is the off switch that keeps prune |
| Sync windows are checked for every operation, manual or not, by `InitiatedBy.Automated` | `controller/sync.go` `syncWindowPreventsSync` | A sync we set obeys the project's windows; with `Automated: true` (decision 2) the automated rules hold |

To verify while building: the `Replace=true` and create paths (`UpdateResource`, `CreateResource`,
`ReplaceResource` in `sync_context.go`) may write under a different manager; hooks create objects
(`Job`s) during a sync.

## What to build

### API

- **`spec.applier.applicationRef`** (`meta.NamespacedObjectReference`), the Argo CD `Application`.
  Exactly one of `kustomizationRef` and `applicationRef` (CEL on `GitTargetApplier`); the namespace
  defaults to the `GitTarget`'s, like `kustomizationRef` (the user decided: no namespace gate).
- **`clusterProviderRef`** works unchanged. It is how a hub Argo CD is reached: the `Application`
  lives in the management cluster.
- **`quietPeriod`** means nothing without `CatchUp`: refuse it together with `applicationRef` (CEL),
  rather than accept a knob that does nothing.
- **`onRefusal: Reapply`** works with `applicationRef`. That closes the known gap where Argo CD
  folders lost the automatic refusal revert when `PushEmptyCommit` was removed.

### The applier interface

- `git.ApplierRef` gains a `Kind` (`Kustomization` | `Application`), part of the key.
- The worker skips `CatchUp` for an `Application` applier: `sendDue` never owes it one, and its quiet
  period is not consulted. `ForeignCommit` and `Refusal` work as for Flux.
- The window's field manager comes from the applier's kind (`FluxFieldManager` today, add
  `ArgoCDFieldManager = "argocd-controller"`).
- The dispatcher picks a client by kind behind one small interface (`Validate`, `Request`,
  `Handled`), so `attempt` and `awaitHandled` stay as they are. Metrics already carry
  `applier_kind`; add `application`.

### The Argo CD client (`internal/applier/argocd.go`)

- **Validate** (for `ApplierConfigured`):
  - `ApplierNotFound`: no such `Application`, or the CRD is not served.
  - `SourceMismatch`: `spec.source.repoURL` is not this repository (`repourl.Same`), or
    `targetRevision` is not this branch (a branch name or `refs/heads/<branch>`; `HEAD` and tags
    mismatch). A multi-source `Application` is refused here in v1, with a message that says so.
  - **`AutomatedSyncEnabled`** (new, stalled): `spec.syncPolicy.automated` is present and not
    `enabled: false` (`IsAutomatedSyncEnabled`). The standard is full control, so this is a
    misconfiguration, not a warning.
  - `Forbidden`: a SelfSubjectAccessReview for `patch` on `applications.argoproj.io` fails.
- **Request**:
  1. Read the `Application`. Check the source and the automated-sync rule again (a published
     verdict can be older than the object). Either failing is `Rejected`.
  2. If an operation is set or `status.operationState.phase` is `Running`, wait for it to end,
     bounded by the apply-window timeout, then read again. Never replace it.
  3. Ask `Current()`. False is `Stale`, as for Flux.
  4. Update the `Application` with `operation` set as autosync builds it (see the facts table),
     `Revision` = the commit, `InitiatedBy{Username: "gitops-reverser", Automated: true}`,
     `Prune` per decision 1. Use an update guarded by
     `resourceVersion`, so a sync somebody else started in between makes it fail instead of
     replacing theirs.
- **Handled**: `status.operationState` is for our operation (`initiatedBy.username` and the revision
  match) and has `finishedAt`, whatever the phase. A failed sync still closes the window; Argo CD's
  own retry belongs to the operation.
- There is no `OutcomeCurrent` for Argo CD: every `ForeignCommit` and `Refusal` syncs.

### Controller, RBAC and docs

- `ApplierConfigured` per kind; `applierReadiness` treats `AutomatedSyncEnabled` as stalled.
- RBAC: `config/rbac/argocd-applier-role.yaml` (`get` and `update` on `applications.argoproj.io`)
  and Helm `applierAccess.argocd`, off by default, mirroring the Flux grant.
- Docs: a `docs/argocd.md` guide shaped like [`../flux.md`](../flux.md), with the standard spelled
  out (automated sync off; refresh and the Argo CD webhook may stay). Update the Argo CD section of
  [`../bi-directional.md`](../bi-directional.md), [`../configuration.md`](../configuration.md),
  the support-boundary [`argocd-bi-directional.md`](support-boundary/argocd-bi-directional.md),
  the metric rows in [`../interpreting-metrics.md`](../interpreting-metrics.md), and the "aim beyond
  Flux" section of [`apply-trigger.md`](apply-trigger.md).

## Tests

- **Unit**: the operation equals what autosync would build except the pinned revision and the
  initiator; a running operation is waited for and never replaced; a concurrent operation makes the
  guarded update fail; every `Validate` reason; `Handled` for success, failure and somebody else's
  operation; the worker owes an `Application` no `CatchUp`; the window withholds an
  `argocd-controller` write over a newer edit.
- **e2e, bi-directional corner**: move the existing Argo CD specs to the standard (template
  `Automated: false`, `spec.applier.applicationRef`). Prove:
  - two API edits seconds apart keep the later value, with no further commit;
  - a Git-side push is synced by a `ForeignCommit` request, pinned to that commit;
  - a refused edit with `Reapply` is reverted by an Argo CD sync;
  - an `Application` with automated sync on is `AutomatedSyncEnabled` and is never synced.

## Decided (2026-10-17, do not re-raise)

1. **Prune** comes from `syncPolicy.automated.prune` when `automated` is present with
   `enabled: false` (the off switch that keeps the block). Without the block, the sync does not
   prune, and the guide says so. No `spec.applier.prune` field.
2. **The initiator** is `InitiatedBy{Username: "gitops-reverser", Automated: true}`: we replace
   autosync, so a sync window that blocks automated syncs blocks ours too.
3. **Hooks on every foreign commit** ship as they are. `ForeignCommit` keeps asking every applier on
   the branch; narrowing by changed paths waits for a measured complaint.

## Out of scope

Multi-source `Application`s, `ApplicationSet`s (they generate `Application`s whose spec they own),
Argo CD's `syncPolicy.automated.selfHeal` (meaningless with automated sync off), and Argo CD
sharding or its own HA.
