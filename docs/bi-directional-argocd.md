# Bi-directional GitOps with Argo CD

This page is for a folder that GitOps Reverser writes and an Argo CD `Application` syncs.
`Application`s that sync other folders need nothing from it, and keep automated sync, self-heal and
everything else exactly as they are. [Bi-directional GitOps](bi-directional.md) explains the model;
this page is the complete Argo CD setup: what to change, what to declare, how to check it works,
and which tests prove each part.

The `ArgoCDClaim` holds the `Application`'s automated sync **off**, and switches it off again
whenever somebody switches it on, so every sync it runs is one GitOps Reverser started, pinned to a
commit it checked. Argo CD still renders, applies, prunes, runs hooks and reports health; it no
longer decides when.

## What changes

| Before | With GitOps Reverser |
| --- | --- |
| `spec.syncPolicy.automated` syncs every new commit | Automated sync is off: the applier claim holds `automated.enabled: false`, and hands it back when it goes |
| A sync starts when Argo CD's refresh finds a new commit | A sync starts when GitOps Reverser sets the `Application`'s `operation`, pinned to a commit |
| `selfHeal` reverts live drift | Nothing reverts live drift by itself; GitOps Reverser publishes it to Git |
| The Git host's push webhook goes to Argo CD | It also goes to GitOps Reverser's receiver; Argo CD's may stay |
| Nothing ties a folder to its `Application` | An `ArgoCDClaim` holds it, and the `GitTarget` names the applier claim in `spec.applierClaimRef` |

Argo CD's timed refresh and its `/api/webhook` can stay as they are. With automated sync off, a
refresh only **compares**: it reports `Synced` or `OutOfSync`, and applies nothing.

## Why automated sync must be off

Automated sync applies every new commit it finds, and each push GitOps Reverser makes is a new
commit. Two to three minutes after each push (Argo CD's `timeout.reconciliation`, 120s plus
jitter), or seconds after it when Argo CD's webhook is wired, the whole `Application` is synced over
whatever is pending at that moment ([why that matters](bi-directional.md#why-the-applier-cannot-decide-for-itself)).
`selfHeal: false` only skips a commit already synced; it does not stop the next one. With
`selfHeal: true`, a live edit is reverted within a second, before it can reach Git at all. The
[support-boundary record](design/support-boundary/argocd-bi-directional.md) has Argo CD's code paths
for each of these, and for the settings that look like a fix and are not.

**The applier claim holds it off.** It applies `spec.syncPolicy.automated.enabled: false` with a
server-side apply under its own field manager, `gitops-reverser`, and touches nothing else of
`automated`, so `prune` and `selfHeal` stay as they are. Switching automated sync on again (Argo
CD's *Enable Auto-Sync*, `kubectl edit`) is undone within a minute, with a
`ClaimedResourceReconciled` Event, and counted in `claimed_resource_reconciled_total`; a sync Argo CD
started in that minute runs to its end, unchecked against the branch.
Deleting the applier claim, or no longer naming it from any `GitTarget`, hands the switch back as it
was: syncing on its own if it did before, manual if it was. The applier claim records which in
`status.previousSuspendState` when it first switches it off: `NotSuspended` for automated sync on,
`Suspended` for off. `kubectl get argocdclaim -o wide` shows it.

**Unless somebody else owns the switch.** Two owners would fight over it, so the applier claim never
writes a switch that is somebody else's, and only checks it:

- An `Application` an `ApplicationSet` generated: the `ApplicationSet` rewrites its spec from its
  template on every reconcile. Set `automated: {enabled: false, prune: true}` in the template.
- A switch another field manager applies with server-side apply (Flux, Helm, an app of apps with
  `ServerSideApply=true`): it sets the switch on every apply. Set `enabled: false` in the manifest it
  applies, or leave `enabled` out of it and the applier claim holds the switch.

While that owner keeps automated sync off, the applier claim is `InControl` and GitOps Reverser syncs as usual.
While it has it on, the applier claim reports `Configured=False` with `OwnedByApplicationSet` or
`AutomatedSyncOwnedElsewhere`, and GitOps Reverser never syncs the `Application`.

**A catch-up is about ownership.** With automated sync off, Argo CD applies nothing on its own.
GitOps Reverser's own commits hold values it read from the cluster, so they are already live, and
after the next refresh the `Application` reports them `Synced`: an API edit asks for no sync. An
object the API creates is different. Argo CD prunes only objects that carry its tracking annotation,
and only a sync stamps it, so an object created through the API in the folder would survive the
deletion of its file in Git. An `Application` therefore gets a `CatchUp` exactly as a Flux
`Kustomization` does: to adopt an object the API created, and to converge, once when the operator
starts and once when a `GitTarget` first names the applier claim ([one sync](#one-sync)).

## Requirements

| Requirement | Why | When it is not met |
| --- | --- | --- |
| Argo CD 3.1 or later | `spec.syncPolicy.automated.enabled` exists from 3.1.0, and is the only way to switch automated sync off and keep `prune` | `ArgoCDVersionNotSupported`: the applier claim checks with a server-side dry run that the API keeps `enabled`, and writes nothing to an `Application` whose API does not |
| Automated sync off: held by the applier claim, or kept off by the `ApplicationSet` or field manager that owns the switch | GitOps Reverser starts every sync, after a flush | `OwnedByApplicationSet` or `AutomatedSyncOwnedElsewhere`; the `Application` is never synced by GitOps Reverser |
| A single Git source, `spec.source`, on the `GitProvider`'s repository | A sync is pinned to one commit of one repository | `UnsupportedSource` for `spec.sources` or `spec.sourceHydrator`; `SourceMismatch` for another repository |
| `targetRevision` is the `GitTarget`'s branch (`main` or `refs/heads/main`) | The tip GitOps Reverser checks is the tip of that branch | `SourceMismatch` for `HEAD`, an empty `targetRevision`, a tag or a commit |
| The source renders the folder GitOps Reverser writes: `directory.recurse: true` for a plain directory, or a kustomization at the path | GitOps Reverser writes `<path>/<namespace>/<group>/<plural>/<name>.yaml` | A non-recursive directory sees no files, and the sync applies nothing |
| Resource tracking by `annotation`, Argo CD's default | GitOps Reverser strips `argocd.argoproj.io/tracking-id`; it cannot strip `app.kubernetes.io/instance`, a label people set on purpose | With `label` or `annotation+label`, the tracking label reaches Git on every object |
| `get`, `update` and `patch` on the `Application`, for the identity of the `KubeProvider` that reaches it | A sync is an update of the `Application`; holding automated sync off is a server-side apply, a patch | `Forbidden` |
| Nothing but GitOps Reverser syncs the `Application` while GitOps Reverser runs | See [what not to do](#what-not-to-do) | Not checked: an unchecked sync can revert live edits |

## A complete example

One folder, `apps/checkout` on branch `main` of `github.com/acme/platform`, published by GitOps
Reverser and synced by the `Application` `argocd/checkout`. Every object below is checked by a unit
test against the code that validates and drives the applier (see
[what the tests prove](#what-the-tests-prove)), so this example cannot drift from what is accepted.

The Argo CD side. The `Application` follows exactly the branch the `GitTarget` writes, and its
automated sync is switched off while keeping `prune` for the syncs GitOps Reverser starts. The
applier claim would switch it off anyway; written in the manifest, it also stays off once the applier claim
hands the switch back:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: checkout
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/acme/platform
    targetRevision: main
    path: apps/checkout
    directory:
      recurse: true
  destination:
    server: https://kubernetes.default.svc
    namespace: checkout
  syncPolicy:
    automated:
      enabled: false
      prune: true
    syncOptions:
      - ServerSideApply=true
```

The GitOps Reverser side. The `GitProvider` points at the same repository (any URL form of it),
which is how the `GitReceiver` the Git host's push webhook calls finds it; an `ArgoCDClaim` holds
the `Application`, in the cluster the chart's `local` `KubeProvider` names (the operator's own,
through `spec.inCluster`), and the `GitTarget` names the applier claim. Nothing may use a provider
without a grant: the `KubeProvider` lets `team-a`'s `GitTarget`s mirror `team-a`
(`allowedGitTargets`), and its applier claims hold an `Application` in `argocd`
(`allowedApplierClaims`); with the Helm chart, set `kubeProvider.allowedGitTargets` and
`kubeProvider.allowedApplierClaims` instead of declaring it. The `GitProvider` lets `team-a` claim
`main`, and the `GitBranch` lets `team-a`'s `GitTarget`s write it. A `KubeProvider` with
`inCluster` that you declare yourself is honored only once it is listed in
`--in-cluster-kube-providers`, which the chart does for its own:

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: KubeProvider
metadata:
  name: local
  namespace: gitops-reverser
spec:
  inCluster: {}
  allowedGitTargets:
    - consumerNamespaces:
        from: Names
        names: [team-a]
      sourceNamespaces: ["{consumer.namespace}"]
  allowedApplierClaims:
    - consumerNamespaces:
        from: Names
        names: [team-a]
      applierNamespaces: [argocd]
---
apiVersion: v1
kind: Secret
metadata:
  name: platform-receiver
  namespace: team-a
stringData:
  token: replace-with-a-long-random-string
---
apiVersion: configbutler.ai/v1alpha3
kind: GitReceiver
metadata:
  name: platform
  namespace: team-a
spec:
  type: github
  secretRef:
    name: platform-receiver
---
apiVersion: configbutler.ai/v1alpha3
kind: GitProvider
metadata:
  name: platform
  namespace: team-a
spec:
  url: git@github.com:acme/platform.git
  allowedGitBranches:
    - consumerNamespaces: {from: Same}
      branchPatterns: [main]
  secretRef:
    name: platform-git
---
apiVersion: configbutler.ai/v1alpha3
kind: GitBranch
metadata:
  name: platform-main
  namespace: team-a
spec:
  gitProviderRef:
    name: platform
  branch: main
  allowedGitTargets:
    - consumerNamespaces: {from: Same}
---
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: checkout
  namespace: team-a
spec:
  gitBranchRef:
    name: platform-main
  kubeProviderRef:
    name: local
    namespace: gitops-reverser
  path: apps/checkout
  applierClaimRef:
    kind: ArgoCDClaim
    name: checkout
  onRefusal: Reapply
---
apiVersion: configbutler.ai/v1alpha3
kind: ArgoCDClaim
metadata:
  name: checkout
  namespace: team-a
spec:
  kubeProviderRef:
    name: local
    namespace: gitops-reverser
  applicationRef:
    namespace: argocd
    name: checkout
```

Point the Git host's push webhook at GitOps Reverser's receiver
([the steps](bi-directional.md#telling-gitops-reverser-that-somebody-else-pushed)), so a Git-side
push is synced in seconds, after any pending edits are replayed onto it.

## The `Application`

The `GitTarget`'s `ApplierClaimReady` is `True` only when the `Application` syncs the branch the
`GitTarget` writes, from a single source, with automated sync off. The applier claim checks these when it
reconciles, for every `GitTarget` naming it, and acts only for one that matches. All of them are
checked again before every sync, so an `Application` changed after the check is still never synced
against them.

| Rule | Reported when broken |
| --- | --- |
| `spec.source.repoURL` is the `GitProvider`'s repository. The forms compare as one repository: `https://` and `ssh://`, `git@host:org/repo` (SCP style), a trailing `.git` or `/`, letter case, and the default port | `SourceMismatch`: `the applier of ArgoCDClaim checkout applies <url>, not <url>`, on the `GitTarget`, and on the applier claim when no `GitTarget` naming it matches |
| `spec.source.targetRevision` is the `spec.branch` of the `GitTarget`'s `GitBranch`, as the name or as `refs/heads/<name>`. `HEAD` (and an empty `targetRevision`), a tag and a commit never follow the branch | `SourceMismatch`: `the applier of ArgoCDClaim checkout applies branch "HEAD", not main`, on the same objects |
| A single `spec.source`: no `spec.sources`, no `spec.sourceHydrator` | applier claim `UnsupportedSource`: `syncs only a single-source Application` |
| Automated sync is off, or the applier claim may switch it off: nobody else owns `spec.syncPolicy.automated.enabled`. Off is Argo CD's own rule: `automated` is absent, or has `enabled: false`; an `automated` block with no `enabled` is on | applier claim `OwnedByApplicationSet` or `AutomatedSyncOwnedElsewhere`, only while automated sync is on |

**Prune.** A sync GitOps Reverser starts prunes when `spec.syncPolicy.automated.prune` is `true`,
read from the `automated` block that `enabled: false` switches off, exactly as automated sync reads
it. Without an `automated` block, its syncs do not prune: a file removed from Git leaves its object
running. `selfHeal` means nothing with automated sync off. `syncOptions` and `retry` apply as they
would to an automated sync.

**Objects another applier owns.** An object whose `argocd.argoproj.io/tracking-id` names another
`Application`, or whose `kustomize.toolkit.fluxcd.io/name` and `kustomize.toolkit.fluxcd.io/namespace`
labels name a Flux `Kustomization`, is not mirrored into the folder: a live change to it is not
mirrored, and a resync neither writes it nor removes its document. Two appliers applying one object
fight each other. See [objects another applier owns](bi-directional.md#objects-another-applier-owns).

## The `ArgoCDClaim`

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: ArgoCDClaim
metadata:
  name: checkout
  namespace: team-a            # the GitTargets naming it are in this namespace
spec:
  kubeProviderRef:             # the cluster the Application lives in; required
    name: management           # namespace omitted: the applier claim's own
  applicationRef:
    namespace: argocd          # default: the applier claim's namespace
    name: checkout
```

`kubeProviderRef` names the cluster that holds the `Application`: the operator's own cluster for an
`inCluster` provider such as the chart's `local`, or a management cluster for a hub Argo CD that
deploys to other clusters. The provider's `spec.allowedApplierClaims` must admit the applier claim's
namespace, its own included only by a grant. The `GitTarget`'s own `spec.kubeProviderRef` stays the
cluster it mirrors. The `Application`'s namespace, such as `argocd`, must be in the
`applierNamespaces` that grant gives; otherwise the applier claim reports
`ApplierNamespaceNotAllowed`. That is a delegation, not owner consent: every namespace the grant
admits may then sync any `Application` in its scope whose source is the repository its
`GitProvider` names, and nothing proves it owns the `Application` or may write to the repository. A
denial hands a held `Application` back. See [the grants](configuration.md#the-grants).
Both references are immutable: delete and recreate the applier claim to hold another `Application`. An
applier claim that holds the switch carries the finalizer `configbutler.ai/argocd-claim`: deleting it,
or no longer naming it from any eligible `GitTarget`, hands the switch back first: as it was
before the applier claim held it, or, for an `Application` an `ApplicationSet` generated since, as the
`ApplicationSet` set it. Where giving the switch up would leave it otherwise, the applier claim sets that
value and leaves it under its field manager, for the switch's owner to change.

### What state the `Application` is in

```mermaid
stateDiagram-v2
    [*] --> OnItsOwn
    OnItsOwn --> Idle: applier claim Configured, switches automated sync off (Adopted)
    Idle --> SyncRequested: GitOps Reverser sets operation, pinned to the commit
    SyncRequested --> Syncing: Argo CD starts the operation
    Syncing --> Idle: Succeeded, Failed or Error
    Idle --> Syncing: somebody else syncs (Sync button, argocd app sync)
    Idle --> Idle: automated sync switched on, and off again (ClaimedResourceReconciled)
    Idle --> OnItsOwn: applier claim deleted or no longer named, switch handed back (Released)

    note right of OnItsOwn
        Argo CD syncs on its own,
        as without GitOps Reverser
    end note
    note right of Idle
        A refresh only compares;
        nothing applies
    end note
```

An applier claim acts only while an eligible `GitTarget` references it: one in the applier claim's namespace that
names it, is admitted by its `KubeProvider`s, and writes the repository and branch the
`Application` syncs. An applier claim nothing eligible references starts no sync and is not in line for
the `Application`; one no `GitTarget` names also reads nothing in the `Application`'s cluster.

The applier claim reports `Configured` and `InControl`, and its `Ready` includes both:

| Reason | Means | Readiness |
| --- | --- | --- |
| `Succeeded` | An eligible `GitTarget` names the applier claim, and the `Application` exists, syncs a single source, has automated sync off or a switch the applier claim may hold, and may be updated and patched | ready |
| `NotReferenced` (`Unknown`) | No `GitTarget` names the applier claim. Nothing is read or written | ready: `Ready=True`, `NotReferenced` |
| `NotBound` (`Unknown`) | No `GitTarget` naming the applier claim is eligible yet, and one cannot be judged yet | progressing |
| `SourceMismatch`, `NamespaceNotAuthorized` or `NotBound` (`False`) | Every `GitTarget` naming the applier claim is refused: it writes another repository or branch, or is not admitted. The message lists each | stalled |
| `ApplierNotFound` | No such `Application` in the applier's cluster, or Argo CD is not installed there | progressing: `Application`s are often created after the applier claim |
| `UnsupportedSource` | The `Application` has several sources, a source hydrator, or no source | stalled |
| `ArgoCDVersionNotSupported` | The `Application`'s API has no `spec.syncPolicy.automated.enabled`: Argo CD before 3.1. Nothing is written | stalled: upgrade Argo CD |
| `OwnedByApplicationSet` | Automated sync is on, and an `ApplicationSet` generated the `Application`, so its template owns the switch | stalled: set `automated.enabled: false` in the template |
| `AutomatedSyncOwnedElsewhere` | Automated sync is on, and another field manager applies the switch | stalled: set `enabled: false` in the manifest it applies, or leave it out |
| `Forbidden` | The applier cluster's identity may not update or patch the `Application` | stalled |
| `NamespaceNotAuthorized` | No grant in the provider's `spec.allowedApplierClaims` admits the applier claim's namespace | stalled |
| `KubeProviderNotFound` | The `kubeProviderRef` names a provider that does not exist | progressing |
| `AuditRouteConflict` | The provider carries an audit route another namespace holds, so it is not used | stalled |
| `ApplierNamespaceNotAllowed` | The `Application`'s namespace is not in the `applierNamespaces` the provider's grants give the applier claim's namespace | stalled |
| `HeldByAnotherClaim` | Another `ArgoCDClaim` in line for the same `Application` in the same cluster, one an eligible `GitTarget` references, holds it (it holds the switch already, or was created first); this one starts no sync, and takes over once that one lets go | stalled: delete one of the two |

`InControl` says whether GitOps Reverser decides, right now, when the `Application` syncs, which
is whether automated sync is off:

| Reason | Means |
| --- | --- |
| `AutomatedSyncOff` (`True`) | Automated sync is off: held off by GitOps Reverser, or kept off by the `ApplicationSet` or field manager that owns the switch (the message says which) |
| `Releasing` (`False`) | The switch is being handed back |
| `Revoking` (`False`) | The switch is being handed back because the `KubeProvider`'s `allowedApplierClaims` no longer admit the applier claim; it finishes even if a grant returns meanwhile |
| `NotReferenced` (`False`), `NotConfigured` (`Unknown`) | Nothing is held: no `GitTarget` names the applier claim, or it is not `Configured` |

A sync that has not completed within 5 minutes also makes the applier claim `Ready=False` with
`OperationUnresolved`, a progressing state that clears when the sync completes.

The `GitTarget` reports `ApplierClaimReady`: `True` when the applier claim is `Ready` and its
`Application` syncs this repository and branch, `SourceMismatch` when it syncs another,
`ApplierClaimNotFound` when the applier claim does not exist, `Unknown` (`NotReferenced`) until the
applier claim has seen that the `GitTarget` names it, and otherwise the applier claim's own `Ready` reason.

## Permissions

GitOps Reverser looks the `Application` up in the cluster of the applier claim's `kubeProviderRef`,
with the identity of that `KubeProvider`. That identity needs:

| API group | Resource | Verbs |
| --- | --- | --- |
| `argoproj.io` | `applications` | `get`, `update`, `patch` |
| `authorization.k8s.io` | `selfsubjectaccessreviews` | `create`, to check the one above |

A sync is started by an `update` of the `Application`, the way Argo CD's own automated sync starts
one, and automated sync is held off with a server-side apply, a `patch`. An applier claim whose switch an
`ApplicationSet` or another field manager owns never patches, and needs no `patch`. For the
operator's own cluster, install the chart with `applierAccess.argocd: true`, or apply
[`config/rbac/argocd-applier-role.yaml`](../config/rbac/argocd-applier-role.yaml) and its binding.
For a remote cluster, grant the same to the identity in the `KubeProvider`'s kubeconfig. A `Role`
in the `Application`'s namespace is enough. Argo CD's own RBAC does not apply: GitOps Reverser
writes the `Application` through the Kubernetes API, as `kubectl` would, not through `argocd-server`.

## One sync

All four [reasons](bi-directional.md#when-gitops-reverser-asks-for-an-apply) reach an
`Application`, `CatchUp` included, as they reach a Flux `Kustomization`. Every request starts a sync;
the branch worker decides beforehand which ones are needed. A `CatchUp` that only converges asks
nothing when the `Application`'s last successful sync applied the tip. A `CatchUp` that adopts syncs
even then, because that sync is what stamps the tracking annotation on the objects the API created.
An `Application` a `GitTarget` names for the first time converges once that `GitTarget`'s initial
snapshots are published, so a commit already on the branch reaches it without waiting for the next
push.

**A request sets the `Application`'s `operation` to the sync automated sync would start**, with
these differences:

- `sync.revision` is **pinned to the commit GitOps Reverser checked**, so the sync applies exactly
  that commit, not whatever the branch holds by then.
- `initiatedBy` is `{username: gitops-reverser, automated: true}`. Argo CD's UI and history show the
  sync as GitOps Reverser's, and [sync windows](#sync-windows-that-block-automated-syncs) treat it
  as an automated sync.
- `info` carries the request's reason, and a token that tells this sync's result from any other.
- A `retry` with `refresh: true` is copied without it, because Argo CD's refresh-on-retry drops the
  pinned revision and syncs the newest one.

`source`, `prune`, `syncOptions` and `retry` (`limit: 5` when the `Application` sets none) are taken
from the `Application`.

```mermaid
stateDiagram-v2
    [*] --> Requested: update sets operation, guarded by resourceVersion
    Requested --> Running: Argo CD moved it into operationState
    Running --> Succeeded: operationState Succeeded
    Running --> Failed: operationState Failed or Error
    Requested --> Unanswered: the update got no answer, or the operator restarted
    Unanswered --> Requested: the token turns up
    Unanswered --> Failed: a guarded refresh write lands first, so the update never can
    Running --> Unresolved: not completed in 5 minutes
    Unresolved --> Succeeded
    Unresolved --> Failed: operationState Failed or Error, or .operation removed by hand
    Failed --> [*]: asked again from scratch, after the backoff
    Succeeded --> [*]
```

`Unanswered` is not shown in `status.operation`, which stays `Requested`: GitOps Reverser writes the
`Application` itself, asking Argo CD for a refresh, under the `resourceVersion` the lost update was
made from. Once that write lands, the lost update can never land, so the operation is `Failed` and
asked for again. No timing assumption about the API server is made.

**A sync already running is never replaced.** Argo CD refuses a new operation while one is set, and
so does GitOps Reverser: it waits for it to end (at most 5 minutes), then decides again from what
the `Application` is then. The update carries the `resourceVersion` it read, so a sync somebody
starts in between makes it fail instead of being overwritten. A failed sync waiting for Argo CD's
retry still counts as running: Argo CD keeps its `operation` set until it completed.

**Hooks run on every sync**, as they would on an automated one: every `ForeignCommit` asks every
applier on the branch, whether or not the push touched its folder, and a `CatchUp` syncs after an API
create in the folder and when a `GitTarget` first names the applier claim.

**The apply window.** Until 10 seconds after the sync's operation completes, however long that
takes, Argo CD's own write to an object nobody edited is the sync landing, and is not mirrored. An
API edit to a field the `Application` sets, one made after the flush included, can be reverted on
the cluster by the sync. GitOps Reverser recognizes that write by Argo CD's field manager
(`argocd-controller`, for a client-side and a server-side apply alike, or
`argocd-application-controller` for a `Replace=true` sync), withholds it from Git, and asks for a
`Repair` sync pinned to the tip that holds the edit, but only when Argo CD's entry is as new as the
edit or newer: undoing an edit can leave an older entry of Argo CD's as the newest, and that undo
is mirrored. When Argo CD and another manager wrote in the same second, the write is mirrored and
counted as `ambiguous`.

A request that fails, and a sync that ends `Failed` or `Error`, is decided again from scratch, after
that `Application`'s own backoff. A sync still running after 5 minutes is `Unresolved`: GitOps
Reverser keeps following it, records an `ApplyUnresolved` Event, and asks the `Application` nothing
more until it completes. Removing `.operation` by hand, the usual way out of a stuck hook, leaves
the operation state `Running` for good, since Argo CD processes an operation only while
`.operation` is set: once `Unresolved`, such a sync of GitOps Reverser's is `Failed` and asked again,
and a request that finds one somebody else started, unchanged for its whole wait, syncs over it.

**Reverting refused edits.** With `onRefusal: Reapply`, the target's `Application` alone is asked to
sync the tip.

## What not to do

Each of these applies Git to the cluster without the check that nothing live is missing from it, or
makes GitOps Reverser mirror something that is not an edit. None is refused by the API; most are
invisible until an edit is lost.

### Syncing by hand, from CI, or by rollback

A sync somebody else starts applies the commit they chose, over whatever is pending at that moment,
and GitOps Reverser publishes the revert as Argo CD's commit. The same holds for `argocd app sync`
in a CI pipeline that runs on push: the push webhook into GitOps Reverser replaces it. A sync in
progress also holds GitOps Reverser's next request, for up to 5 minutes.

`argocd app rollback` applies an older commit over everything since, API edits included, and GitOps
Reverser then publishes the result as a new commit. To undo a change, revert it in Git instead.

A manual sync is the right tool only [while GitOps Reverser is down](#while-gitops-reverser-is-down).

### Sync windows that block automated syncs

GitOps Reverser's syncs carry `automated: true`, so an `AppProject` sync window treats them as
automated syncs: an active `deny` window blocks them, and so does an `allow` window outside its
schedule; `manualSync: true` on the window does not let them through. A blocked sync stays
`Running` (`Sync operation blocked by sync window`) and runs when the window opens, still pinned to
the commit it was asked for, so it reverts any newer API edits outside any apply window. Keep sync
windows off the `AppProject`s of `Application`s that a `GitTarget` names.

### Watching the objects your hooks create

[Hooks](https://argo-cd.readthedocs.io/en/stable/user-guide/resource_hooks/) run on every sync. Argo
CD's default delete policy, `BeforeHookCreation`, deletes a hook's object and creates it again on
each sync, and a hook named with `generateName` is a new object every time. A `WatchRule` covering a
hook's kind (usually `Job`) in the namespace it runs in therefore commits on every sync, and with
the default `spec.prune.mode` can remove the hook's own manifest from Git. Keep hook kinds out of
the folder's `WatchRule`s.

### Switching automated sync back on

The applier claim switches it off again within a minute, but a sync Argo CD starts in that minute applies
the tip over whatever is pending. To have the `Application` sync on its own again, delete the
applier claim, or stop naming it from the `GitTarget`: the hand-back gives the switch up.

### Applying the switch on from a client-side app of apps

A parent `Application` that applies this one with Argo CD's default client-side apply writes the
switch with an update, which the applier claim cannot tell from a click in the UI. With `enabled: true` in
the child's manifest and `selfHeal` on the parent, the two switch it back and forth. Leave `enabled`
out of that manifest, or set it to `false`.

### Generating the `Application` from an `ApplicationSet`

The `ApplicationSet` controller writes its template back over any change to the `Application`s it
generates, so the switch is the template's: the template must carry
`automated: {enabled: false, prune: true}` (or no `automated` block). The applier claim checks it and
never writes it. With automated sync on, the applier claim is `OwnedByApplicationSet`, and its message
says what to change. Pointing an applier claim at the `ApplicationSet` itself is not supported.

### Relying on `Replace=true`

A `Replace=true` sync writes as `argocd-application-controller`. GitOps Reverser counts that as
Argo CD's in the apply window, but no test measures it yet. The default apply, client-side or
`ServerSideApply=true`, writes as `argocd-controller` and is what the end-to-end tests cover.

## Checking that it works

```bash
# The Application is found, its automated sync is held off, and it may be updated; and it syncs this target's branch.
kubectl get argocdclaim checkout -n team-a
kubectl get gittarget checkout -n team-a \
  -o jsonpath='{.status.conditions[?(@.type=="ApplierClaimReady")]}{"\n"}'

# What GitOps Reverser last asked for, why, and where that sync stands.
kubectl get argocdclaim checkout -n team-a -o jsonpath='{.status.operation}{"\n"}'

# The last sync: who started it, which commit, and how it ended. A request asks, it never reports.
kubectl get application checkout -n argocd \
  -o jsonpath='{.status.operationState.operation.initiatedBy}{" "}{.status.operationState.syncResult.revision}{" "}{.status.operationState.phase}{"\n"}'
# {"automated":true,"username":"gitops-reverser"} 4f2c1ab9... Succeeded
```

`gitopsreverser_apply_requests_total{applier_kind="application",reason,outcome}` counts the
requests; [interpreting metrics](interpreting-metrics.md#apply-requests-and-the-push-receiver) reads
it.

## Troubleshooting

| Symptom | Cause | Fix |
| --- | --- | --- |
| `ArgoCDClaim` `Configured=False`, `OwnedByApplicationSet` | An `ApplicationSet` generated the `Application`, and its template has automated sync on | Set `automated: {enabled: false, prune: true}` in the `ApplicationSet`'s template |
| `ArgoCDClaim` `Configured=False`, `AutomatedSyncOwnedElsewhere` | Another field manager applies the switch on; the message names it | Set `enabled: false` in the manifest it applies, or leave `enabled` out of it |
| A Normal `ClaimedResourceReconciled` Event | Somebody switched automated sync on, and the applier claim switched it off again | Delete the applier claim to have the `Application` sync on its own; see [switching it back on](#switching-automated-sync-back-on) |
| `ArgoCDClaim` `Configured=False`, `ArgoCDVersionNotSupported` | Argo CD before 3.1, whose `Application` has no `enabled` | Upgrade Argo CD to 3.1 or later |
| `ArgoCDClaim` `Configured=False`, `ApplierNotFound` | No `Application` with that namespace and name in the applier claim's cluster, or Argo CD is not installed there | Create it, or recreate the applier claim with the right `applicationRef`; an omitted namespace is the applier claim's |
| `ApplierClaimReady=False`, `SourceMismatch`, or applier claim `UnsupportedSource` | The message names the broken [`Application` rule](#the-application) | Fix the `Application`; a sync that was waiting is asked for afterward |
| `ArgoCDClaim` `Configured=False`, `ApplierNamespaceNotAllowed` | The `Application` is in a namespace the grants do not give, such as `argocd` | Add it to `applierNamespaces` in the `KubeProvider`'s `spec.allowedApplierClaims` (Helm: `kubeProvider.allowedApplierClaims`) |
| `ArgoCDClaim` `Configured=Unknown`, `NotBound` | A `GitTarget` names the applier claim, but cannot be judged yet: its `GitProvider` or `KubeProvider` does not exist | Create the missing object; the message names it |
| `ArgoCDClaim` `Configured=False`, `Forbidden` | The identity lacks one of the [permissions](#permissions) | Grant it, or set `applierAccess.argocd: true` |
| A Warning `ApplyRequestFailed` Event | A request could not be delivered, such as a sync somebody else started that did not end within 5 minutes | GitOps Reverser keeps asking after its backoff, at most every five minutes |
| A Warning `ApplyFailed` Event | The sync ended `Failed` or `Error`; the message carries Argo CD's | Fix what the message names; GitOps Reverser asks again after its backoff |
| A Warning `ApplyUnresolved` Event, applier claim `OperationUnresolved` | The sync has not completed in 5 minutes: hooks, waves, or Argo CD's own retries | Nothing is asked of the `Application` until it completes; look at its operation state |
| A file deleted in Git leaves its object running | The `Application` has no `automated.prune: true` | Add `automated: {enabled: false, prune: true}` |
| The last sync stays `Running`: `Sync operation blocked by sync window` | A sync window blocks automated syncs | Remove the window from the `AppProject`; see [sync windows](#sync-windows-that-block-automated-syncs) |
| A commit appears on every sync, adding or removing a `Job` | A `WatchRule` covers the objects a hook creates | Keep hook kinds out of the `WatchRule`s; see [hooks](#watching-the-objects-your-hooks-create) |
| An object in the folder is never mirrored, and the log says `Not mirroring an object another applier owns` | Another `Application` or a Flux `Kustomization` applies it; `apply_ownership_conflicts_total` counts it | Let one applier own it: take it out of the other applier's source, or stop watching it from this `GitTarget` |

## While GitOps Reverser is down

GitOps Reverser becomes the only path to a deployment. While it is down, a Git-side push reaches the
cluster through a manual sync (`argocd app sync`, or the UI), which is safe then because nothing is
publishing. When it starts again, its first look at the remote asks for no `ForeignCommit`. A
`CatchUp` that converges is pending instead, and is asked only once every initial snapshot of the
`GitTarget`s naming the applier claim is published: a sync before that could revert edits made while the
operator was down, before its watches caught them. An `Application` whose last successful sync
applied the tip is asked nothing.

## What the tests prove

| Covered | Where |
| --- | --- |
| The [complete example](#a-complete-example) decodes strictly, reports `Configured=True`, binds the `GitTarget`, and its request sets a sync pinned to the commit | [`argocd_guide_test.go`](../internal/applier/argocd_guide_test.go) |
| The operation is the one automated sync builds, but for the pinned revision, the initiator and a pinned retry; a running sync is waited for and never replaced; a sync started between the read and the update makes it fail; an unanswered update is settled by a guarded write; every `Configured` reason, the `ApplicationSet` and field-manager owners of the switch among them; when a sync counts as finished | [`argocd_test.go`](../internal/applier/argocd_test.go) |
| The applier claim switches automated sync off, keeping `prune`; switches it off again when somebody switches it on; hands it back on deletion and when no longer named, as it stood before (`status.previousSuspendState`) or as an `ApplicationSet` that adopted it set it; leaves it held off for an applier claim in line; never writes a switch an `ApplicationSet` or another field manager owns; never writes one where Argo CD before 3.1 refuses it | [`argocdclaim_hold_test.go`](../internal/controller/argocdclaim_hold_test.go) |
| Holding the switch and handing it back on a real API server: a manual `Application` is manual again, one with automated sync on syncs on its own again, an owner's own `enabled: false` stays, an `ApplicationSet`'s adopted value stays; a schema without `enabled` refuses the apply, and nothing is written | [`argocdclaim_apiserver_test.go`](../internal/controller/argocdclaim_apiserver_test.go) |
| An `Application` converges once a `GitTarget` first names its applier claim, is asked on a `ForeignCommit` and a `Refusal`, and adopts an API create even at the tip; Argo CD's late write over a newer edit is withheld, by its own field managers | [`applier_claim_test.go`](../internal/git/applier_claim_test.go), [`apply_catchup_test.go`](../internal/git/apply_catchup_test.go), [`apply_window_test.go`](../internal/git/apply_window_test.go) |
| An object another applier owns, read from its tracking annotation, is neither mirrored nor swept | [`apply_ownership_test.go`](../internal/git/apply_ownership_test.go) |
| An applier claim nothing names reads nothing and is `Ready`; the binding over several `GitTarget`s; `ApplierNamespaceNotAllowed`; a sync whose `.operation` was removed while it ran ends | [`applierclaim_rule_test.go`](../internal/controller/applierclaim_rule_test.go), [`argocd_test.go`](../internal/applier/argocd_test.go) |
| Two API edits seconds apart keep the later value, with no further commit | [Bi-directional (Argo CD)](../test/e2e/argocd_bi_directional_e2e_test.go) |
| A Git-side push is synced by a `ForeignCommit` request, pinned to that commit | Bi-directional (Argo CD) |
| A refused edit with `onRefusal: Reapply` is reverted by an Argo CD sync | Bi-directional (Argo CD) |
| Automated sync switched on is switched off again, and a Git-side push is then synced by GitOps Reverser; an `Application` an `ApplicationSet` generates with automated sync on is `OwnedByApplicationSet`, is never synced, and its switch is never written | Bi-directional (Argo CD) |
| Argo CD's bookkeeping never reaches Git, asserted as zero commits during a sync | Bi-directional (Argo CD) |
| An API create, and a delete and create with the same content, are adopted, so a Git-side delete prunes them; an edit made during an apply is in Git at once and back on the cluster after the `Repair`; a Git-side push is applied while edits keep arriving, and every edit is kept; a Git-side push survives a resync forced before its apply, and a Git-side addition survives one under `prune.mode: Always`; an API edit asks for no apply; another applier's object is neither mirrored nor adopted; a save made across a Git-side change resolves from the push | [Bi-directional applier echoes (Argo CD)](../test/e2e/bi_directional_echoes_e2e_test.go) |
| A Git-side push made while the previous one is being applied stays in Git and on the cluster | [Bi-directional push race (Argo CD)](../test/e2e/bi_directional_push_race_e2e_test.go) |
| After a restart, an API edit and an API create made while the operator was down end in Git and on the cluster, nothing reverts the edit, a Git-side push made meanwhile is applied, and the create is adopted | [Bi-directional restart (Argo CD)](../test/e2e/bi_directional_restart_e2e_test.go) |

`task test-e2e-bi-directional` runs the Argo CD specs. Not covered end to end yet: a hub Argo CD in
another cluster, `Forbidden` against cluster RBAC, and a `Replace=true` sync's field manager, which
is read from Argo CD's source rather than measured.
