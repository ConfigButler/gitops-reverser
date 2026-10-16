# Apply trigger: follow-ups

> **design**: open. Written 2026-10-16, after the Flux applier landed on `docs-bi-directional-right`
> (#419). Index: [`../INDEX.md`](../INDEX.md)
> The design this continues: [`push-notification-and-reconcile-trigger.md`](push-notification-and-reconcile-trigger.md).
> The user-facing model: [`../bi-directional.md`](../bi-directional.md).

The branch makes GitOps Reverser decide when a Flux `Kustomization` applies a folder. This page is
what comes next, in order: Argo CD on the same branch, the regression the branch ships until then,
the smaller loose ends, and the parts of the feature the tests do not reach yet.

## 1. Where the branch stands

Built and tested:

- **The decision**, on the branch worker ([`apply_trigger.go`](../../internal/git/apply_trigger.go)).
  `applyDebt` turns every remote observation into what the appliers are owed: a `ForeignCommit`
  for a tip the worker did not push, a `CatchUp` for its own pushes and for its first observation.
  `serviceAppliers` sends requests only on a quiet branch, closes an open window when the branch
  moved under it, and waits `quietPeriod` from the last decided write before a catch-up.
- **The refusal action**, `onRefusal: Reapply`, which owes the refusing target's applier a
  `Refusal` request ([`refusal_reapply.go`](../../internal/git/refusal_reapply.go)).
- **Delivery**, in [`internal/applier`](../../internal/applier/): the Flux client (stamp the
  `GitRepository`, or the `Kustomization` when the artifact already holds the commit), the
  `Validate` behind `ApplierConfigured`, and the dispatcher (per-applier superseding, retry with
  backoff, Events, the ledger behind `status.applier`).
- **The receiver**, `GitProvider.spec.receiver` on `--receiver-bind-address`
  ([`receiver.go`](../../internal/webhook/receiver.go)), which turns a verified delivery into a
  refresh that always reaches the remote.

Not built: the Argo CD applier, the webhook applier. The API accepts `kustomizationRef` only.

## 2. Next: the Argo CD applier

### 2.1 What it has to do

The design is
[§3.6 of the trigger design](push-notification-and-reconcile-trigger.md#36-what-a-request-does).
In short: GitOps Reverser sets `operation.sync` on the `Application` exactly as Argo CD's own
automated sync builds it
([`appcontroller.go`](https://github.com/argoproj/argo-cd/blob/3cbae653d41ced2fcdec5d07c25fff2693c4242e/controller/appcontroller.go#L2628-L2641)),
pinned to the commit the worker checked, and the `Application` runs with
`syncPolicy.automated.enabled: false` (Argo CD 3.1+) so nothing else syncs it.

Three properties make it different from Flux, and each one has to survive into the code:

- **No `CatchUp`.** With automated sync off, Argo CD applies nothing on its own, so an
  `Application` behind GitOps Reverser's own pushes has nothing stale to apply. And every sync runs
  the `Application`'s resource hooks, so syncing for no change would run them for no reason.
  `ForeignCommit` and `Refusal` only.
- **The commit is pinned.** The operation names `revision`, so the sync applies what the worker
  checked, not whatever the tip has become by the time Argo CD gets to it. Flux cannot do this,
  which is why it gets `CatchUp`; Argo CD can, which is why it does not need it.
- **A running operation is waited for, never replaced.** `operation` is a single slot, and
  Argo CD's own `SetAppOperation` refuses to overwrite one. A request that finds it occupied retries
  later; the dispatcher's backoff already covers this.

### 2.2 How I would build it

1. **API.** Add `applicationRef` (`meta.NamespacedObjectReference`) beside `kustomizationRef`, make
   both optional, and add a CEL rule on `GitTargetApplier` requiring exactly one. Document that
   `quietPeriod` is ignored for `applicationRef`. Regenerate, and extend
   [`hack/crdfields/fields.yaml`](../../hack/crdfields/fields.yaml).
2. **An applier interface in `internal/applier`.** Today the dispatcher calls `Flux` directly
   (`attempt` builds `NewFlux`; the controller calls `Flux.Validate`). Introduce:

   ```go
   type Applier interface {
       Request(ctx context.Context, commit string, reason v1alpha3.ApplyReason) (Outcome, error)
       Validate(ctx context.Context, repositoryURL, branch string) Verdict
   }
   ```

   and a constructor that picks the implementation from the `GitTargetApplier`. Add the kind to
   `applierKey`, so a `Kustomization` and an `Application` with one name never supersede each
   other. Keep `applier_kind` on the metric (`kustomization`, `application`).
3. **`argocd.go`.** Read the `Application` (`argoproj.io/v1alpha1`, `applications`) through the
   dynamic client.
   - `Request`: refuse `CatchUp` (outcome `Current`, nothing sent), and otherwise build the
     operation from the `Application`'s own spec: `prune` from `syncPolicy.automated.prune`,
     `syncOptions` from `syncPolicy.syncOptions`, `retry` from `syncPolicy.retry` or `{limit: 5}`,
     `source` from `spec.source` (or `sources` and `revisions` for a multi-source `Application`),
     `initiatedBy.username: gitops-reverser`. Write it with an **update**, not a patch, guarded by
     the `resourceVersion` the read returned. A conflict or an existing `operation` is a retry.
   - `Validate`: `ApplierNotFound`; `SourceMismatch` when no source has a `repoURL` that is the
     same repository (`repourl.Same`) with `targetRevision` equal to the branch (`HEAD` and an empty
     `targetRevision` mean the remote's default branch, which is a mismatch unless that is the
     branch); `AutomatedSyncEnabled` when `syncPolicy.automated` is present without
     `enabled: false`; `Forbidden` from an access review for `update` on `applications`.
   - `AutomatedSyncEnabled` still sends requests: they are correct, only the guarantee is missing.
     It should be a **stall** in `applierReadiness`, because it needs a human, beside
     `SourceMismatch` and `Forbidden`.
4. **The worker.** `serviceCatchUp` asks every applier on the branch. It must skip `applicationRef`
   targets, which means `appliersOnBranch` carries the kind. That is the only worker change.
5. **RBAC.** `config/rbac/argocd-applier-role.yaml` and a binding (`get` and `update` on
   `argoproj.io` `applications`, `create` on `selfsubjectaccessreviews`), included in
   `config/rbac/kustomization.yaml` the way the Flux pair is, and Helm `applierAccess.argoCD`
   beside `applierAccess.flux`.
6. **e2e, the Argo CD corner.** The corner's specs currently prove Argo CD's own behaviour
   (`selfHeal: true` flaps, `selfHeal: false` keeps the edit). Keep the bookkeeping spec and the
   split-ownership spec, and replace the `selfHeal` phases with:
   - the race spec, as in the Flux corner, with `automated.enabled: false` and `applicationRef`;
   - a spec that sets `timeout.reconciliation` short (the corner installs Argo CD, so this is
     `argocd-cm`) and asserts the `Application`'s operation history holds only GitOps Reverser's
     syncs while edits are in flight;
   - the Git-side push with an API edit on top, asserting the synced revision is the replayed tip;
   - the refusal spec against an `Application`.
7. **Docs.** Bring back the Argo CD section of the guide as the trigger design describes it; the
   [docs-first version of the guide](https://github.com/ConfigButler/gitops-reverser/blob/6e700ec8/docs/bi-directional.md#configure-argo-cd)
   on this branch's history is a good starting text. Update `configuration.md`'s applier section,
   the architecture ground rule, and the trigger design's build order.

### 2.3 Questions to settle before writing it

- **`HEAD` as `targetRevision`.** Many `Application`s track `HEAD`. Is that a match for a
  `GitTarget` on the remote's default branch? Resolving it needs the remote's HEAD, which the
  worker's observation has (`ParentBranch` while the branch is absent, not otherwise). Simplest:
  require an explicit branch, and say so in the `SourceMismatch` message.
- **Hooks on `ForeignCommit`.** Every applier on the branch is asked, whether or not the foreign
  commit touched its folder (open question 3 in the design). For Flux that is a no-op apply; for
  Argo CD it runs hooks. If that is a real complaint, the narrowing is the `Application`'s own
  `source.path` compared with the diff, which is the Application's own declaration, not an
  inference.
- **Hub Argo CD.** An `Application` usually lives in the Argo CD cluster, not the cluster it
  deploys to. The current rule (the applier is looked up in the source cluster) makes the common
  Argo CD topology unsupported. It needs a way to say where the `Application` lives: the control
  cluster, or a kubeconfig. Decide this first, because it shapes the API.

## 3. The regression this branch ships until then

`onRefusal: PushEmptyCommit` is gone, and `Reapply` needs a Flux applier. So for a folder Argo CD
applies, a refused edit now stays `OutOfSync` where the empty commit used to revert it. The same is
true for any applier GitOps Reverser cannot name: a hub Argo CD, or a tool with no applier kind.

Two ways to close it:

- **Keep `PushEmptyCommit` as a third value**, documented as the fallback for an applier GitOps
  Reverser cannot name, with the costs it always had (it wakes everything watching the branch, and
  it can revert allowed edits still in a commit window). The machinery is in this branch's history
  (`internal/git/refusal_touch.go` before the rename) and the guards carry over unchanged.
- **Land the Argo CD applier on this branch before it merges**, which is the plan, and note the gap
  in [`UPGRADING.md`](../UPGRADING.md) only if it ships without it.

My recommendation is the second, with the first as the fallback if the hub question in §2.3 turns
out to need its own release.

## 4. The webhook applier

After Argo CD. `webhook: {url, secretRef}`, a POST of `{"repository", "branch", "commit",
"reason"}` signed with `X-Signature: sha256=<hex>` (Flux's `generic-hmac` header), behind the same
`Applier` interface. It gets every reason, `CatchUp` included, because nothing says the tool on the
other end does not apply on its own. No in-process plugin, for the reason the trigger design gives.

## 5. Smaller loose ends

- **The ledger behind `status.applier` is never pruned**
  ([`dispatcher.go`](../../internal/applier/dispatcher.go)). It is keyed by namespace and name, so a
  `GitTarget` deleted and recreated under the same name inherits its predecessor's last request.
  Key it by UID, or forget an entry from the GitTarget controller's deleted-object path, the way
  `remotePublications` does.
- **The receiver listens on the elected replica only.** A standby refuses the connection, so a
  `Service` across replicas loses deliveries to it. With one replica (the default) this is
  invisible. Either bind on every replica and answer `503` until elected, which a Git host retries
  or at least reports, or route to the leader. Open question 2 in the trigger design.
- **`ApplierConfigured` reads the source cluster on every reconcile**: two GETs and two access
  reviews. A target that is not converged reconciles every ten seconds, so a broken target costs
  that much. Cache the verdict per `(cluster, applier, generation)` for a minute, or check only on
  the steady tick and on spec changes.
- **`SourceDynamicClient` serves only declared source clusters.** The dispatcher retries until the
  GitTarget's watches have declared the cluster, which covers startup. A target whose declaration
  keeps failing therefore never gets its requests delivered, and only the retry log says why. Count
  it in `apply_requests_total{outcome="failed"}` with a reason, or surface it in `ApplierConfigured`.
- **The dispatcher's inbox is capped at 1024 and drops the oldest with a log line only.** That
  can only fill if the loop is wedged, but a drop there is a lost `ForeignCommit`. Count it.
- **A catch-up when nothing was pushed since the last one.** The first observation after a restart
  owes a catch-up, which is right; a refresh that confirms an unchanged tip owes nothing, which is
  also right. Check that a worker replaced because its `GitProvider` was repointed starts with a
  baseline of the new repository and not a `ForeignCommit` against the old one's tip. I believe it
  does (a new worker has a fresh `applyDebt`), and nothing tests it.
- **The trigger design's open question 3**, path scoping for `ForeignCommit`, stays open.

## 6. What the tests do not reach yet

Ordered by how much I would worry about each.

1. **The receiver end to end.** The config-dir install runs the listener and a `Service`, but no
   e2e spec sends a delivery. Discovery of a foreign push by a delivery is unit-tested over a real
   Git server (`TestApplyTrigger_APushNoticeFindsAForeignCommit`) and the handler is unit-tested,
   but nothing proves the two are wired together in a deployed operator. A spec should sign a
   `generic-hmac` body with the route's token, POST it to the `Service` from inside the cluster,
   and assert Flux applies a Git-side push within seconds, with no API edit to make it surface.
2. **A foreign push found by the refresher.** The Flux spec finds the foreign push through a
   rejected push, because the e2e refresh rides the five-minute reconcile tick. An idle branch with
   no receiver depends entirely on the refresh path, and nothing exercises it end to end. A
   reconcile-request annotation would not do (it forces a resync, the hazard of §2.2 of the design);
   the test needs a way to fire the refresh, or a shorter tick under test.
3. **The race spec's margin.** It edits twice a few seconds apart and asserts no extra commit for
   30 seconds. It caught the one-wake bug, which is the reason it exists, but it depends on the
   corner's commit window and push cooldown. Write the unit equivalent with an injected clock: a
   window that opens, closes, commits and pushes in one wake, followed by a second write inside
   `quietPeriod`, asserting no request in between. Part of this is
   `TestServiceAppliers_AWriteDecidedAndPushedInOneWakeRestartsTheQuietPeriod`; the second-write
   half is not.
4. **A remote source cluster.** Every test of the applier uses the operator's own cluster. A
   `ClusterProvider` with a kubeconfig, a `Kustomization` in that cluster, and the
   `Forbidden` path when the kubeconfig's identity lacks the grant, are all untested.
5. **`ApplierConfigured` against real RBAC.** e2e covers `True` only. `Forbidden` (drop the
   flux-applier binding), `SourceMismatch` (point the `Kustomization` at another branch) and
   `ApplierNotFound` (name one that does not exist, then create it, and watch the target recover)
   are unit-tested with a fake access check.
6. **Several GitTargets naming one `Kustomization`.** The dispatcher's dedupe is unit-tested; no
   e2e has two folders on one branch applied by one `Kustomization`.
7. **A foreign commit while the remote is unreachable.** The rule is that the request waits with
   the pending writes (§3.4 of the design). Nothing tests that it does not go out during the
   outage, or that it goes out once the push lands.
8. **A foreign commit closing a window a `CommitRequest` is attached to.**
   `closeWindowForForeignCommit` finalizes it early; the request should resolve `Committed` with
   the replayed commit. Untested.
9. **Upgrade with a stored `PushEmptyCommit`.** `EffectiveOnRefusal` reads it as `Ignore` (unit
   level). The CRD rejecting an update until the field changes, and the CEL rule rejecting
   `Reapply` without an applier, are not exercised by an envtest.
10. **The Helm install.** Local e2e is `INSTALL_MODE=config-dir`, so the chart's receiver flag,
    `Service` and `applierAccess.flux` role are covered by `helm template` tests only. CI's
    quickstart leg installs the chart, but with neither value set.
11. **The race detector in CI.** `applyDebt` is fed from `publishObservation`, which can run off
    the loop goroutine (the first clone), and the dispatcher delivers on goroutines of its own. Both
    are lock-guarded, and their tests pass under `-race -count=3` locally (2026-10-16), but CI does
    not run `-race`, so a later change can regress this unnoticed.
