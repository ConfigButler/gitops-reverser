# Using GitOps Reverser with Argo CD

When GitOps Reverser publishes a folder that an Argo CD `Application` syncs, GitOps Reverser takes
full control of when that `Application` syncs. The `Application` has automated sync switched off,
and every sync it runs is one GitOps Reverser started, pinned to a commit it checked. Argo CD still
renders, applies, prunes, runs hooks and reports health; it no longer decides when. This page is the
complete Argo CD setup: what to change on the `Application`, what to declare on the `GitTarget`,
where the Git host's webhook goes, how to check it works, and which tests prove each part.

The [bi-directional guide](bi-directional.md) explains the model behind this for any applier, and
the [configuration reference](configuration.md#who-applies-this-folder-specapplier) has every field
and condition reason. [Using GitOps Reverser with Flux](flux.md) is the same setup for Flux.

## What changes

| Before | With GitOps Reverser |
| --- | --- |
| `spec.syncPolicy.automated` syncs every new commit | Automated sync is off: `automated.enabled: false`, or no `automated` block |
| A sync starts when Argo CD's refresh finds a new commit | A sync starts when GitOps Reverser sets the `Application`'s `operation`, pinned to a commit |
| `selfHeal` reverts live drift | Nothing reverts live drift by itself; GitOps Reverser publishes it to Git |
| The Git host's push webhook goes to Argo CD | It goes to GitOps Reverser's receiver too; Argo CD's may stay |
| Nothing ties a folder to its `Application` | The `GitTarget` names it in `spec.applier.applicationRef` |

Argo CD's timed refresh and its `/api/webhook` can stay as they are. With automated sync off a
refresh only **compares**: it resolves the branch tip and reports `Synced` or `OutOfSync`, and
applies nothing.

## Why automated sync must be off

**Automated sync applies every new commit it finds, whenever its refresh finds it.** Each push
GitOps Reverser makes is a new commit, so two to three minutes after each push (Argo CD's
`timeout.reconciliation`, 120s plus jitter), or seconds after it when Argo CD's webhook is wired,
the whole `Application` is synced over whatever is pending at that moment. `selfHeal: false` only
skips a commit already synced; it does not stop the next one. That brings back the two races GitOps
Reverser exists to close:

- **Its own pushes.** Two API edits a few seconds apart produce two commits. A sync of the first
  commit after the second edit reverts that edit, and GitOps Reverser then publishes the revert as
  a third commit.
- **Somebody else's push.** While an API edit is still in an open commit window, a Git-side push
  arrives. Synced straight away, the push's tree reverts the edit, because the edit is not in Git
  yet. GitOps Reverser instead closes the window, replays the edit onto the push, pushes the
  result, and only then asks Argo CD to sync it.

Both apply something correct, at the wrong moment. Only GitOps Reverser can see the edits that are
live and not yet pushed, so only it can tell when a sync is safe. It starts one only on a **quiet
branch**: no open commit window and no pending writes.

An `Application` with automated sync on is reported `ApplierConfigured=False` with reason
`AutomatedSyncEnabled`, and GitOps Reverser never syncs it. It is a misconfiguration to fix, and
stalls the `GitTarget`: an `Application` that syncs on its own is outside GitOps Reverser's control.

**There is no catch-up.** Flux needs one, because it applies on its own timers and on a restart
from whatever artifact it holds, and a stale artifact would revert edits already in Git. With
automated sync off, Argo CD applies nothing on its own, so nothing it holds can go stale. GitOps
Reverser's own commits are already live (they were read from the cluster), and after its next
refresh the `Application` reports them `Synced`. So `spec.applier.quietPeriod` is rejected with an
`applicationRef`.

## A complete example

One folder, `apps/checkout` on branch `main` of `github.com/acme/platform`, published by GitOps
Reverser and synced by the `Application` `argocd/checkout`. Every object below is checked by a unit
test against the code that validates and drives the applier (see
[what the tests prove](#what-the-tests-prove)), so this example cannot drift from what is accepted.

The Argo CD side. The `Application` follows exactly the branch the `GitTarget` writes, and its
automated sync is switched off while keeping `prune` for the syncs GitOps Reverser starts:

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

The GitOps Reverser side. The `GitProvider` points at the same repository (any URL form of it) and
serves a push receiver; the `GitTarget` names the `Application` as its applier:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: platform-receiver
  namespace: team-a
stringData:
  token: replace-with-a-long-random-string
---
apiVersion: configbutler.ai/v1alpha3
kind: GitProvider
metadata:
  name: platform
  namespace: team-a
spec:
  url: git@github.com:acme/platform.git
  allowedBranches: ["main"]
  secretRef:
    name: platform-git
  receiver:
    type: github
    secretRef:
      name: platform-receiver
---
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: checkout
  namespace: team-a
spec:
  gitProviderRef:
    name: platform
  branch: main
  path: apps/checkout
  applier:
    applicationRef:
      namespace: argocd
      name: checkout
  onRefusal: Reapply
```

`directory.recurse: true` matters: GitOps Reverser writes `<path>/<namespace>/<group>/<plural>/<name>.yaml`,
so a non-recursive directory source sees nothing.

## The `Application`

`ApplierConfigured` is `True` only when the `Application` syncs the branch the `GitTarget` writes,
from a single source, with automated sync off. Each rule is checked when the target reconciles and
again before every sync, so an `Application` changed after the check is still never synced against
the rules.

| Rule | Reported when broken |
| --- | --- |
| `spec.source.repoURL` is the `GitProvider`'s repository. The forms compare as one repository: `https://` and `ssh://`, `git@host:org/repo` (SCP style), a trailing `.git` or `/`, letter case, and the default port | `SourceMismatch`: `syncs <url>, not <url>` |
| `spec.source.targetRevision` is the `GitTarget`'s `spec.branch`, as the name or as `refs/heads/<name>`. `HEAD` (and an empty `targetRevision`, which means `HEAD`), a tag and a commit never follow the branch | `SourceMismatch`: `targets revision "HEAD", not branch main` |
| A single `spec.source`: no `spec.sources`, no `spec.sourceHydrator` | `SourceMismatch`: `syncs only a single-source Application` |
| Automated sync is off: `spec.syncPolicy.automated` is absent, or has `enabled: false`. This is Argo CD's own rule; an `automated` block with no `enabled` is on | `AutomatedSyncEnabled` |

**Prune.** A sync GitOps Reverser starts prunes when `spec.syncPolicy.automated.prune` is `true`,
read from the `automated` block that `enabled: false` switches off, exactly as automated sync would
read it. Without an `automated` block, its syncs do not prune: a file removed from Git leaves its
object running. Keep the block with `enabled: false` and `prune: true` when a deletion in Git should
delete the live object.

`selfHeal` means nothing with automated sync off, and is ignored. `syncOptions` and `retry` apply to
GitOps Reverser's syncs as they would to an automated one.

## The `GitTarget`

```yaml
spec:
  applier:
    applicationRef:
      namespace: argocd        # default: this GitTarget's namespace
      name: checkout
    clusterProviderRef:        # default: this GitTarget's spec.clusterProviderRef
      name: management
```

`clusterProviderRef` is for a hub Argo CD, which keeps its `Application`s in a management cluster and
deploys to another. Name the `ClusterProvider` of the cluster the `Application` lives in; the
`GitTarget`'s own `spec.clusterProviderRef` stays the cluster it mirrors. Leave it out when Argo CD
runs in the source cluster.

`spec.applier` names exactly one of `kustomizationRef` and `applicationRef`, and `quietPeriod` is
rejected with an `applicationRef`.

The target reports `ApplierConfigured`, and `Ready` includes it:

| Reason | Means | Readiness |
| --- | --- | --- |
| `Succeeded` | The `Application` exists, syncs this repository and branch, has automated sync off, and may be updated | ready |
| `ApplierNotFound` | No such `Application` in the applier's cluster, or Argo CD is not installed there | progressing: `Application`s are often created after the `GitTarget` |
| `SourceMismatch` | One of the [`Application` rules](#the-application) about its source is broken | stalled: needs a human |
| `AutomatedSyncEnabled` | Automated sync is on | stalled: needs a human |
| `Forbidden` | The applier cluster's identity may not update the `Application` | stalled: needs a human |
| `NamespaceNotAuthorized`, `ClusterProviderNotFound` | `spec.applier.clusterProviderRef` names a provider that does not admit this namespace, or does not exist | stalled: needs a human |

## Permissions

GitOps Reverser looks the `Application` up in the cluster of `spec.applier.clusterProviderRef`, by
default the **source cluster**, with the identity of that `ClusterProvider`. That identity needs:

| API group | Resource | Verbs |
| --- | --- | --- |
| `argoproj.io` | `applications` | `get`, `update` |
| `authorization.k8s.io` | `selfsubjectaccessreviews` | `create`, to check the one above |

A sync is started by an `update` of the `Application`, the way Argo CD's own automated sync starts
one. For the operator's own cluster, install the chart with `applierAccess.argocd: true`, or apply
[`config/rbac/argocd-applier-role.yaml`](../config/rbac/argocd-applier-role.yaml) and its binding.
For a remote cluster, grant the same to the identity in the `ClusterProvider`'s kubeconfig. A `Role`
in the `Application`'s namespace is enough when you would rather not grant cluster-wide.

Argo CD's own RBAC does not apply: GitOps Reverser writes the `Application` through the Kubernetes
API, as `kubectl` would, not through `argocd-server`.

## The Git host's webhook

Point the Git host's push webhook at GitOps Reverser's receiver, so a Git-side push is synced in
seconds, after GitOps Reverser has replayed any pending edits onto it. The receiver is the same as
for Flux: [the Flux guide](flux.md#the-git-hosts-webhook) has the steps.

Argo CD's own webhook into `/api/webhook` may stay. It only makes Argo CD refresh sooner, and with
automated sync off a refresh only compares. Without GitOps Reverser's receiver, a foreign push is
still found, later: by the next rejected push or by the periodic refresh (`git.refreshInterval`,
`10m`).

## What GitOps Reverser asks, and when

| Reason | When | Asked |
| --- | --- | --- |
| `ForeignCommit` | The branch gained a commit GitOps Reverser did not push. Pending edits are replayed onto it and pushed first | Every applier declared on the branch |
| `Refusal` | An edit was refused and the target has `onRefusal: Reapply` | The refusing target's `Application` |

There is no `CatchUp` for an `Application` ([why](#why-automated-sync-must-be-off)), and every
request starts a sync: there is no "already current" for Argo CD.

**A request sets the `Application`'s `operation` to the sync automated sync would start**, with
these differences:

- `sync.revision` is **pinned to the commit GitOps Reverser checked**, so the sync applies exactly
  that commit, not whatever the branch holds by then.
- `initiatedBy` is `{username: gitops-reverser, automated: true}`. Argo CD's UI and history show
  the sync as GitOps Reverser's, and a [sync window](https://argo-cd.readthedocs.io/en/stable/user-guide/sync_windows/)
  that blocks automated syncs blocks it too.
- `info` carries the request's reason, and a token that tells this sync's result from any other.
- A `retry` with `refresh: true` is copied without it. Argo CD's refresh-on-retry drops the pinned
  revision and syncs the newest one, a commit GitOps Reverser never checked.

`source`, `prune`, `syncOptions` and `retry` (`limit: 5` when the `Application` sets none) are taken
from the `Application`, as automated sync takes them.

**A sync already running is never replaced.** Argo CD refuses a new operation while one is set, and
so does GitOps Reverser: it waits for it to end (at most 5 minutes), then reads the `Application`
again and decides from what it is then. The update carries the `resourceVersion` it read, so a sync
somebody starts in between makes it fail instead of being overwritten. A failed sync waiting for
Argo CD's retry still counts as running.

Before it writes, GitOps Reverser checks that its decision still holds: the commit is still the
branch tip, and no edit is waiting to be published. When either changed, it writes nothing and
decides again at the next quiet moment.

**Hooks run on every sync**, as they would on an automated one: every `ForeignCommit` asks every
applier on the branch, whether or not the push touched its folder.

**An edit made while Argo CD syncs keeps its value.** Until the sync's operation completes
(`Succeeded`, `Failed` or `Error`, at most 5 minutes), GitOps Reverser treats it as running. An API
edit made in those seconds, to a field the `Application` sets, is reverted on the cluster by the
sync, because the commit it applies does not hold the edit yet. GitOps Reverser recognizes that
write by Argo CD's field manager (`argocd-controller`, for a client-side and a server-side apply
alike, or `argocd-application-controller` for a `Replace=true` sync) and does not mirror it: the
API's value stays in Git. Each such write is logged and counted in
`apply_window_collisions_total{outcome="withheld"}`. Unlike Flux, nothing applies the edit back by
itself, since there is no catch-up: the cluster runs the reverted value until the next sync.

Only a write proven to be Argo CD's is withheld. When Argo CD and another manager wrote the object
in the same second, the write could be the edit itself, so it is mirrored as before and counted as
`ambiguous`. A delete is never withheld.

The request runs off the branch worker, so a slow Argo CD never holds a publication. A request that
fails is decided again from scratch, after that `Application`'s own backoff, exactly as for
[Flux](flux.md#what-gitops-reverser-asks-and-when).

## Reverting refused edits

With `onRefusal: Reapply`, an edit GitOps Reverser cannot write (one that has no home in the folder,
such as a field a kustomize base owns) is reverted by asking the target's `Application` to sync the
tip, once the branch is quiet. Only that `Application` is asked.
[Reverting a refused edit](configuration.md#reverting-a-refused-edit-speconrefusal) has the
eligibility rules.

## Checking that it works

```bash
# The Application is found, syncs this repository and branch, has automated sync off, and may be updated.
kubectl get gittarget checkout -n team-a \
  -o jsonpath='{.status.conditions[?(@.type=="ApplierConfigured")]}{"\n"}'

# What GitOps Reverser last asked for, and why.
kubectl get gittarget checkout -n team-a -o jsonpath='{.status.applier}{"\n"}'

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
| `ApplierConfigured=False`, `AutomatedSyncEnabled` | `spec.syncPolicy.automated` is present without `enabled: false` | Set `automated.enabled: false` (keep `prune`), or remove the block |
| `ApplierConfigured=False`, `ApplierNotFound` | The `Application` does not exist in the applier's cluster under that namespace and name, or Argo CD is not installed there | Create it, or fix `applicationRef`; an omitted namespace is the `GitTarget`'s |
| `ApplierConfigured=False`, `SourceMismatch` | The message names the broken [`Application` rule](#the-application) | Fix the `Application`; a request it was holding is delivered afterward |
| `ApplierConfigured=False`, `Forbidden` | The identity lacks one of the [permissions](#permissions) | Grant it, or set `applierAccess.argocd: true` |
| A Warning `ApplyRequestFailed` Event | A request could not be delivered; the message says why, such as a sync somebody else started that did not end within 5 minutes | GitOps Reverser keeps asking on a quiet branch, at most every five minutes |
| A file deleted in Git leaves its object running | The `Application` has no `automated.prune: true` | Add `automated: {enabled: false, prune: true}` |
| A sync is skipped | A sync window denies automated syncs now | Wait for the window, or allow automated syncs in it |

## While GitOps Reverser is down

GitOps Reverser becomes the only path to a deployment. While it is down, a Git-side push reaches
the cluster through a manual sync (`argocd app sync`, or the UI), which is safe then because nothing
is publishing. When it starts again, its first look at the remote owes no `ForeignCommit`: a sync on
startup could revert edits made while it was down, before its watches have caught them.

## What the tests prove

| Covered | Where |
| --- | --- |
| The [complete example](#a-complete-example) decodes strictly, reports `ApplierConfigured=True`, and its request sets a sync pinned to the commit | [`argocd_guide_test.go`](../internal/applier/argocd_guide_test.go) |
| The operation is the one automated sync builds, but for the pinned revision, the initiator and a pinned retry; a running sync is waited for and never replaced; a sync started between the read and the update makes it fail; every `ApplierConfigured` reason; when a sync counts as finished | [`argocd_test.go`](../internal/applier/argocd_test.go) |
| An `Application` is never owed a `CatchUp`, and is asked on a `ForeignCommit` and a `Refusal`; Argo CD's late write over a newer edit is withheld, by its own field managers | [`apply_trigger_test.go`](../internal/git/apply_trigger_test.go), [`apply_window_test.go`](../internal/git/apply_window_test.go) |
| Two API edits seconds apart keep the later value, with no further commit | [Bi-directional (Argo CD)](../test/e2e/argocd_bi_directional_e2e_test.go) |
| A Git-side push is synced by a `ForeignCommit` request, pinned to that commit | Bi-directional (Argo CD) |
| A refused edit with `onRefusal: Reapply` is reverted by an Argo CD sync | Bi-directional (Argo CD) |
| An `Application` with automated sync on is `AutomatedSyncEnabled` and is never synced | Bi-directional (Argo CD) |
| Argo CD's bookkeeping never reaches Git, asserted as zero commits during a sync | Bi-directional (Argo CD) |

`task test-e2e-bi-directional` runs the Argo CD specs. Not covered end to end yet: a hub Argo CD in
another cluster, `Forbidden` against cluster RBAC, and a `Replace=true` sync's field manager, which
is read from Argo CD's source rather than measured.
