# Apply trigger: follow-ups

> **design**: open. Written 2026-10-16, after the Flux applier landed on `docs-bi-directional-right`
> (#419). Index: [`../INDEX.md`](../INDEX.md)
> The design this continues: [`push-notification-and-reconcile-trigger.md`](push-notification-and-reconcile-trigger.md).
> The user-facing model: [`../bi-directional.md`](../bi-directional.md).

The branch makes GitOps Reverser decide when a Flux `Kustomization` applies a folder. This page is
what comes next, in order: the fixes the 2026-10-16 review found, which block the merge; Argo CD;
the breaking change; the smaller loose ends; and the parts of the feature the tests do not reach
yet.

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

## 2. Before merge: what the review found

The review of `21df16be` (`docs/reviews/20261016-review.md`, kept out of the tree) found six
problems. Five hold as reported, and one holds only for its wording. They have one cause: the
worker decides a request once, and nothing after that decision checks it again. The dispatcher
attempts it for up to four minutes after the worker decided, against whatever the cluster, the
`GitTarget` and the branch have become in the meantime. GitOps Reverser now controls when
deployments happen, so each decision has to hold both when it is made and when a patch is sent.

### 2.1 A target the ClusterProvider denies still drives its applier (finding 1)

`appliersOnBranch` lists every `GitTarget` on the provider and branch that names an applier, and
`Dispatcher.resolve` checks nothing more. `SourceDynamicClient` hands out a cluster's client when
**any** target declared that cluster, so a target with `Validated=False/NamespaceNotAuthorized`
rides on another target's declaration. That target can be kept on a running worker by any
legitimate target in its namespace on the same branch. The reviewer reproduced the patch.

The damage is bounded: the request is a reconcile annotation, and once §2.2 is fixed it reaches only
a `Kustomization` that applies this repository and branch. It is still an authorization bypass,
and the fix is cheap. The dispatcher calls `authz.GitTargetAdmitted` for every target in a job,
on every attempt (see §2.4), and drops a target it denies. A client being available is not
permission to use it.

One more gap sits at the same fix site, and the review did not raise it: `kustomizationRef.namespace`
is unrestricted, so an admitted target can name a `Kustomization` in any namespace of the source
cluster, while the operator's `flux-applier` role grants `patch` cluster-wide. A WatchRule needs
`allowAnySourceNamespace` on its ClusterProvider to reach outside its own namespace, and the applier
reference should be held to the same rule.

### 2.2 A `SourceMismatch` applier is still driven (finding 2)

`Flux.Validate` checks the `GitRepository`'s URL and branch, but `Flux.Request` does not, and the
dispatcher never reads the verdict. A target that publishes `main` and names a `Kustomization` whose
source follows `release` reports `ApplierConfigured=False/SourceMismatch`, yet every foreign commit
on `main` still stamps that `release` source. The reviewer reproduced this as well.

`Request` already reads both objects, so it runs `sourceMismatch` on them before it stamps and
returns a `permanentError` on a mismatch. Reading the published condition instead would leave the
race between the check and the patch. This means `Request` needs the expected repository URL and
branch, which the job does not carry today. The dispatcher reads the `GitProvider` when it resolves
the job. This also shapes the `Applier` interface in §3.2: the expected source binding is an input
to `Request`, not only to `Validate`.

### 2.3 A delayed Refusal outlives its consent and its recovery (finding 3)

There are two waiting places for a refusal, and only one of them checks again. `flushPendingReapply`,
which handles the rate-limit trailer, calls `refusalConsent`, as that function's comment promises.
`reapplyOwed` does not. A refusal owed while the branch is busy, behind an open window or a
publication outage, is sent by `serviceRefusals` once the branch is quiet, and the only check left
is whether the target still names an applier. So it goes out after the target is suspended or set to
`Ignore`.

`refusalRecovered` cancels `refusalPending` but leaves `reapplyOwed` alone, because `reapplyOwed` is
keyed by target and has lost the collection it would have to match. A refusal that has recovered,
which means its edit was accepted and is now pending, still asks for an apply once the branch is
quiet. By then the tip carries the accepted edit, so the apply is harmless, but it is still a
request made for a refusal that no longer exists.

Fix both in the worker:

- Key `reapplyOwed` by `refusalKey`, and have `refusalRecovered` delete the matching entries the way
  it already does for `refusalPending`.
- Have `serviceRefusals` call `refusalConsent` before it sends.

The dispatcher has to check consent again on its own attempts (see §2.4). A job's reason is per
applier, but consent is per target: a `Refusal` job can have absorbed a `CatchUp` for a sibling
target (`supersedingReason`). The job therefore has to keep each target's own reason. When the only
target that consented drops out, the job falls back to `CatchUp` for the rest rather than being
dropped.

### 2.4 One rule for all three: check again at the patch

§2.1 to §2.3 need the same check, so it should be written once. Before each attempt, the dispatcher
reads each target in the job again from the cache and keeps only the targets that:

- still name this applier, as `resolve` already checks once,
- `authz.GitTargetAdmitted` admits (§2.1),
- consent to a refusal, for a target that is in the job only for a `Refusal` (§2.3).

Then `Request` checks the source binding (§2.2). A job with no target left is dropped and counted.
This is the last gate before the patch, so the retries go through it too, which is the point the
review makes about "including retries".

### 2.5 A request given up on is lost (finding 4)

After eight failed attempts the dispatcher emits `ApplyRequestFailed` and discards the job, while
the worker cleared its debt when it sent the request. The `DefaultBackoff` comment reads "Nothing is
lost by giving up", but that assumes another request comes, and none has to. A refresh that sees
the same tip owes nothing, so a `ForeignCommit` that failed while the source API was down waits for
the next commit or for Flux's long backstop interval. A failed `Refusal` is worse: `oweReapply`
marks the observation as covered when the request is owed, so the same standing refusal never owes
another request until it recovers.

The review asks to "retain or reconstruct" the request, and I would reconstruct it in the worker
rather than make the dispatcher retry forever. The reason is one the review does not raise: **a
retry is a request sent without the quiet check.** The worker decided it on a quiet branch, and the
dispatcher replays that decision up to four minutes later. Flux cannot pin a commit, so the retry
applies whatever the tip is then, and live edits waiting in an open window by that time are
reverted. That is the hazard this whole design exists to close. Retrying for longer makes the
window longer.

The shape I propose:

- The dispatcher keeps its short retries for a blip of a few seconds, since a request decided a
  moment ago is still sound. It hands a job back to the worker once the job has failed for longer
  than about one `quietPeriod`, or permanently, instead of giving up. "Handing back" is a new call on
  the requester's side of `git.ApplyRequester`, delivered as an event on the worker's loop.
- The worker puts the debt back (`applyDebt.restore`, or `reapplyOwed` for a refusal). It re-sends
  only on a quiet branch, with its own backoff capped at a few minutes so that a permanent failure
  does not produce a tight loop.
- `ApplyRequestFailed` stays, emitted once each time a request goes from delivering to failing,
  not on every attempt.

A `permanentError`, which is a misconfigured `Kustomization` and now also a source mismatch, then
stays owed and is delivered once the configuration is fixed, rather than being forgotten. A
restart loses the hand-back state, which is fine: the first observation owes a `CatchUp`, and that
stamps the source when its artifact is behind.

### 2.6 A Refusal can wake folders that share the GitRepository (finding 5)

This one is a contract problem, and no reason to enforce isolation. When the artifact
is behind the tip, `Request` stamps the `GitRepository`, the new revision wakes every
`Kustomization` on it, and those reapply as well. A `Kustomization` that stamped only itself would be
worse: it reconciles from the old artifact and reverts edits that are already in Git. So the source
has to move, and moving the source wakes everything that consumes it. That is how Flux works, not
something GitOps Reverser can avoid.

The harm is small. The branch is quiet, because a Refusal is sent only then, so the woken siblings
apply a tip with nothing live left out of it. They would have applied the same tip at their own
`CatchUp` one `quietPeriod` later. A `Kustomization` that GitOps Reverser does not know about would
apply it at its next interval. What is wrong is the promise. Five places say a refusal wakes no
other folder:

- the `onRefusal` doc comment in `gittarget_types.go`,
- [`bi-directional.md`](../bi-directional.md),
- [`configuration.md`](../configuration.md),
- [`UPGRADING.md`](../UPGRADING.md),
- §4.5 of the trigger design.

The fix is to narrow all five: only this target's applier is asked, and if its `GitRepository` has
to fetch to reach the tip, every `Kustomization` on that `GitRepository` applies the new revision, as
it would for any push. Give the folder its own `GitRepository` for strict isolation.

### 2.7 Validation can hold the GitTarget controller indefinitely (finding 6)

`applierVerdict` calls `ApplierClients` and `Validate` with the reconcile context, which has no
deadline. The source client has no request timeout either, by design, because it carries the
watches. The GitTarget controller runs one reconcile worker, so an API server that accepts the
connection and never answers stops every GitTarget's reconcile, not only this target's. Wrap both
calls in a bounded context. About 10 seconds is enough: `Validate` makes two GETs and two access
reviews, and a timeout is already `ReasonCheckFailed`, so `keptOrUnknown` keeps the previous verdict.
This is the same fix site as the per-reconcile cost in §6. Do the caching there together with this.

### 2.8 Validation the review ran

The review ran `task lint`, `task test` and `task test-e2e-bi-directional`, and all three were
green. `task test-e2e` failed one spec, audit-route attribution, which passed on its own. That spec
is the known flaky one: it has failed twice in CI on comment-only commits, and both passed on a
rerun. It is not evidence against this branch. Each fix above gets a unit test:

- the reviewer's reproductions for findings 1 to 4 and 6, kept as regression tests,
- a test that a hand-back is re-sent only on a quiet branch.

## 3. Next: the Argo CD applier

### 3.1 What it has to do

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

### 3.2 How I would build it

1. **API.** Add `applicationRef` (`meta.NamespacedObjectReference`) beside `kustomizationRef`, make
   both optional, and add a CEL rule on `GitTargetApplier` requiring exactly one. Document that
   `quietPeriod` is ignored for `applicationRef`. Regenerate, and extend
   [`hack/crdfields/fields.yaml`](../../hack/crdfields/fields.yaml).
2. **An applier interface in `internal/applier`.** Today the dispatcher calls `Flux` directly
   (`attempt` builds `NewFlux`; the controller calls `Flux.Validate`). Introduce:

   ```go
   // Binding is the repository and branch the applier must apply from.
   type Binding struct{ RepositoryURL, Branch string }

   type Applier interface {
       Request(ctx context.Context, want Binding, commit string, reason v1alpha3.ApplyReason) (Outcome, error)
       Validate(ctx context.Context, want Binding) Verdict
   }
   ```

   `Request` takes the binding because it checks it before it writes anything (§2.2). An
   `Application` whose source no longer matches is refused at delivery the same way a
   `Kustomization` is. Then add a constructor that picks the implementation from the `GitTargetApplier`. Add the kind to
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
6. **e2e, the Argo CD corner.** The corner's specs currently prove Argo CD's own behavior
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

### 3.3 Questions to settle before writing it

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

## 4. The breaking change

`onRefusal: PushEmptyCommit` is gone, and `Reapply` needs a Flux applier, so until §3 lands a
folder that Argo CD applies has no automatic revert. GitOps Reverser has no users yet, so there is
nothing to migrate and no reason to hold the merge for Argo CD. The branch merges once §2 is done,
and Argo CD can come in its own PR. Do not bring `PushEmptyCommit` back as a fallback.

One fix in [`UPGRADING.md`](../UPGRADING.md): its entry says to set `onRefusal: Reapply` before
upgrading, which the old CRD rejects. The order is `Ignore`, then upgrade, then set
`spec.applier`, then `Reapply`.

## 5. The webhook applier

After Argo CD. `webhook: {url, secretRef}`, a POST of `{"repository", "branch", "commit",
"reason"}` signed with `X-Signature: sha256=<hex>` (Flux's `generic-hmac` header), behind the same
`Applier` interface. It gets every reason, `CatchUp` included, because nothing says the tool on the
other end does not apply on its own. No in-process plugin, for the reason the trigger design gives.

## 6. Smaller loose ends

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
  the steady tick and on spec changes. Do this together with the deadline in §2.7.
- **`SourceDynamicClient` serves only declared source clusters.** The dispatcher retries until the
  GitTarget's watches have declared the cluster, which covers startup. A target whose declaration
  keeps failing therefore never gets its requests delivered, and only the retry log says why. Count
  it in `apply_requests_total{outcome="failed"}` with a reason, or surface it in `ApplierConfigured`.
  This check answers whether the cluster has a client yet. It does not answer whether this target
  may use that client, which is §2.1.
- **The dispatcher's inbox is capped at 1024 and drops the oldest with a log line only.** That
  can only fill if the loop is wedged, but a drop there is a lost `ForeignCommit`. Count it.
- **A catch-up when nothing was pushed since the last one.** The first observation after a restart
  owes a catch-up, which is right; a refresh that confirms an unchanged tip owes nothing, which is
  also right. Check that a worker replaced because its `GitProvider` was repointed starts with a
  baseline of the new repository and not a `ForeignCommit` against the old one's tip. I believe it
  does (a new worker has a fresh `applyDebt`), and nothing tests it.
- **The trigger design's open question 3**, path scoping for `ForeignCommit`, stays open.

## 7. What the tests do not reach yet

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
   outage, or that it goes out once the push lands. The same test with a `Refusal` owed during the
   outage, and the target set to `Ignore` before the outage ends, covers §2.3.
8. **A foreign commit closing a window a `CommitRequest` is attached to.**
   `closeWindowForForeignCommit` finalizes it early; the request should resolve `Committed` with
   the replayed commit. Untested.
9. **The CEL rule rejecting `Reapply` without an applier** is not exercised by an envtest. A stored
   `PushEmptyCommit` needs no test, because no user has one (§4).
10. **The Helm install.** Local e2e is `INSTALL_MODE=config-dir`, so the chart's receiver flag,
    `Service` and `applierAccess.flux` role are covered by `helm template` tests only. CI's
    quickstart leg installs the chart, but with neither value set.
11. **The race detector in CI.** `applyDebt` is fed from `publishObservation`, which can run off
    the loop goroutine (the first clone), and the dispatcher delivers on goroutines of its own. Both
    are lock-guarded, and their tests pass under `-race -count=3` locally (2026-10-16), but CI does
    not run `-race`, so a later change can regress this unnoticed.
