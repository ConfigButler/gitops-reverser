# Using GitOps Reverser with Flux

When GitOps Reverser publishes a folder that Flux applies, GitOps Reverser decides when Flux
applies it. A `FluxTrigger` holds the `Kustomization` **suspended** while it is idle. For each apply,
GitOps Reverser resumes it, waits for that one reconcile to end, and suspends it again. Flux still
fetches, renders, applies, prunes and reports health; it just never applies on its own: not on its
interval, not on a new artifact, not on a controller restart. This page is the complete Flux setup:
what to change on the `GitRepository` and the `Kustomization`, what to declare on the `FluxTrigger`
and the `GitTarget`, where the Git host's webhook goes, how to check it works, what it costs, and
which tests prove each part.

The [bi-directional guide](bi-directional.md) explains the model behind this for any applier, and
the [configuration reference](configuration.md#who-applies-this-folder-specapplytriggerref) has every field
and condition reason. Argo CD gets the same contract with automated sync off: see [the Argo CD
guide](argocd.md).

## What changes

| Before | With GitOps Reverser |
| --- | --- |
| The `Kustomization` applies on its interval, on every new artifact, and on a controller restart | It is suspended, and applies only when GitOps Reverser resumes it for one apply |
| A failed apply is retried after `retryInterval` | The `Kustomization` is suspended again after a failure; GitOps Reverser retries on a quiet branch |
| The Git host's push webhook goes to a Flux `Receiver` | It goes to GitOps Reverser's receiver; the Flux `Receiver` for this repository is removed |
| Nothing ties a folder to its `Kustomization` | A `FluxTrigger` holds it, and the `GitTarget` names the trigger in `spec.applyTriggerRef` |
| `flux get ks` shows it ready | It shows it `Suspended`, which is how it is held |

## Why the `Kustomization` is suspended

Only GitOps Reverser can see the edits that are live and not yet pushed, so only it can tell when
an apply is safe. It starts an apply only on a **quiet branch**: no open commit window and no
pending writes. An apply at any other moment applies something correct at the wrong time:

- **Its own pushes.** Two API edits a few seconds apart produce two commits. An apply between them
  applies the first commit over the second edit, and GitOps Reverser then publishes the revert as a
  third commit.
- **Somebody else's push.** While an API edit is still in an open commit window, a Git-side push
  arrives. Applied straight away, the push's tree reverts the edit, because the edit is not in Git
  yet. GitOps Reverser instead closes the window, replays the edit onto the push, pushes the
  result, and only then applies.

A long interval makes Flux apply less often; it does not make Flux wait. A `Kustomization` that is
not suspended still applies when its interval fires, when kustomize-controller restarts, when a
failed apply retries, when its own spec changes, and when its `GitRepository` gets a new artifact
for any reason, including a fetch another consumer asked for. A suspended `Kustomization` does none
of these: a reconcile reads `spec.suspend` first and returns before it resolves its source.

## A complete example

One folder, `apps/checkout` on branch `main` of `github.com/acme/platform`, published by GitOps
Reverser and applied by the `Kustomization` `flux-system/checkout`. Every object below is checked by
a unit test against the code that validates and drives the applier (see
[what the tests prove](#what-the-tests-prove)), so this example cannot drift from what is accepted.

The Flux side. The `GitRepository` follows exactly the branch the `GitTarget` writes. The
`Kustomization`'s manifest does **not** set `suspend`: the `FluxTrigger` holds that field.

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: platform
  namespace: flux-system
spec:
  url: https://github.com/acme/platform
  ref:
    branch: main
  interval: 1h
  secretRef:
    name: platform-git
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: checkout
  namespace: flux-system
spec:
  sourceRef:
    kind: GitRepository
    name: platform
  path: ./apps/checkout
  prune: true
  interval: 1h
```

The GitOps Reverser side. The `GitProvider` points at the same repository (any URL form of it) and
serves a push receiver; a `FluxTrigger` holds the `Kustomization`, in the cluster the `default`
`ClusterProvider` names, and the `GitTarget` names the trigger:

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
  applyTriggerRef:
    kind: FluxTrigger
    name: checkout
  onRefusal: Reapply
---
apiVersion: configbutler.ai/v1alpha3
kind: FluxTrigger
metadata:
  name: checkout
  namespace: team-a
spec:
  clusterProviderRef:
    name: default
  kustomizationRef:
    namespace: flux-system
    name: checkout
  quietPeriod: 30s
```

No Flux `Receiver` for `acme/platform` appears, on purpose: remove yours if you have one.

## The `GitRepository`

The `GitTarget`'s `ApplyTriggerReady` is `True` only when the `Kustomization`'s source fetches the
branch the `GitTarget` writes. The trigger checks the rules about the source alone when it
reconciles, and the `GitTarget` checks the repository and branch against the source the trigger
reports. All of them are checked again before every apply, so a source changed after the check
is still never resumed for the wrong branch.

| Rule | Reported when broken |
| --- | --- |
| `spec.url` is the `GitProvider`'s repository. The forms compare as one repository: `https://` and `ssh://`, `git@host:org/repo` (SCP style), a trailing `.git` or `/`, letter case, and the default port | `GitTarget` `SourceMismatch`: `the applier of FluxTrigger checkout applies <url>, not <url>` |
| `spec.ref.branch` is exactly the `GitTarget`'s `spec.branch` | `GitTarget` `SourceMismatch`: `the applier of FluxTrigger checkout applies branch "release", not main` |
| `spec.ref` pins nothing stronger: no `commit`, `name`, `semver` or `tag`, even alongside the branch. Flux resolves those before the branch, so the artifact would never follow it | trigger `UnsupportedSource`: `pins spec.ref.tag "v1.0.0", so it never follows a branch` |
| `spec.ref.branch` is set | trigger `UnsupportedSource`: `names no spec.ref.branch` |

A suspended `Kustomization` ignores a new artifact, so the source's own `interval` decides when it
fetches, never when anything applies. Keep it longer than an apply takes, `1h` in the example: an
apply reads the artifact again just before it resumes the `Kustomization`, and a fetch of the
source's own between that read and the reconcile makes Flux apply another commit than the one
GitOps Reverser checked. When the source moved on, that is logged, counted in
`apply_revision_mismatches_total`, and still protected by the apply window, but it is avoidable. An
apply of an *older* revision than the one checked (kustomize-controller's cached source lagging the
fetch) counts as failed, and GitOps Reverser asks again.

**Shared sources.** Several `Kustomization`s may read one `GitRepository`. The trigger lists its
source's other consumers in `status.source.otherConsumers`: the `Kustomization`s no `FluxTrigger`
holds and the `HelmRelease`s whose chart comes from it, directly or through a `HelmChart` that
their `chartRef` names. That list is information only: one source
for `apps` and `infra` is a normal setup, and it never affects `Ready`. Each consumer in it applies
a new artifact the moment it arrives, so:

- While the list is empty and the trigger's own `Kustomization` is held, `status.source.prefetch` is
  `true`: when GitOps Reverser learns of a push while the branch is busy, it has the source fetch at
  once, so the artifact is ready when the apply is due. A consumer counts as held only while its
  `FluxTrigger` reports `Suspended` `True`, and that is checked again on the cluster just before the
  fetch. While the list is not empty, nothing is fetched ahead.
- Appliers on one branch that share a source apply one at a time.

## The `Kustomization`

- `spec.sourceRef` names a `GitRepository`. A `Kustomization` that applies from an `OCIRepository`
  or a `Bucket` makes its trigger report `UnsupportedSource`: GitOps Reverser writes Git, and can only ask a Git source
  to fetch.
- **The manifest omits `spec.suspend`.** Whatever applies the `Kustomization` object (a parent
  `Kustomization`, Helm, flux-operator) must leave the field alone, or it resumes the
  `Kustomization` on its next apply. This is checked: when another field manager applies
  `spec.suspend`, the trigger reports `Configured=False`, `SuspendOwnedElsewhere`, naming the
  manager, and holds nothing. A one-off write such as `flux resume` is no such owner; see
  [overriding by hand](#overriding-by-hand).
- `spec.interval` and `spec.retryInterval` decide nothing while it is suspended.
- `spec.wait`, health checks and `spec.timeout` lengthen each apply, and that is correct: the apply
  can still revert edits until it ends, so GitOps Reverser follows it, and holds its apply window
  open, for as long as it runs.
- `spec.dependsOn`: a dependent waits until its dependency has applied its current generation and is
  ready. A dependency whose apply failed is suspended with its generation unapplied, so its
  dependents wait until GitOps Reverser's retry of it succeeds.
- It may live in any namespace (`flux-system` is the usual one). The reference's namespace defaults
  to the trigger's own. GitOps Reverser resumes only a `Kustomization` whose source fetches this
  `GitProvider`'s repository and branch, so naming one elsewhere grants nothing that write access to
  the repository does not already give.
- Several `GitTarget`s may name one `FluxTrigger`, for example for a `Kustomization` whose `path`
  covers several folders. It is applied once for all of them, each with its own reason, and waits for
  the trigger's `quietPeriod`, because its apply reaches every one of their folders.
- Two `GitProvider`s may name one repository, for example one per team. Each gets its own branch
  worker, and the branch counts as quiet only when both are: a `Kustomization` applies the tip
  whichever of them wrote it.

## The `FluxTrigger` and the `GitTarget`

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: FluxTrigger
metadata:
  name: checkout
  namespace: team-a            # the GitTargets naming it are in this namespace
spec:
  clusterProviderRef:          # the cluster Flux runs in; default: "default"
    name: default
  kustomizationRef:
    namespace: flux-system     # default: the trigger's namespace
    name: checkout
  quietPeriod: 30s             # default 30s, at most 1h
---
kind: GitTarget
spec:
  applyTriggerRef:
    kind: FluxTrigger
    name: checkout
```

`clusterProviderRef` names the cluster that holds the `Kustomization`: omitted, the
`ClusterProvider` named `default`, as for a `GitTarget`, often the operator's own cluster, or
another provider for Flux running in a management cluster with a `Kustomization` per workload
cluster. It and `kustomizationRef` are immutable: delete and recreate the trigger to
hold another `Kustomization`.

`quietPeriod` is how long the branch must stay quiet after GitOps Reverser's own pushes before the
`Kustomization` applies them (`CatchUp`). Every new write restarts it. A suspended `Kustomization`
never applies a stale artifact, so this is about convergence: Flux's inventory learns of an object
only when it applies a commit that holds it, and an object created through the API that Flux never
applied would not be pruned when Git later deletes it.

**Holding the `Kustomization`.** Once `Configured`, the trigger suspends its `Kustomization` with a
server-side apply under the field manager `gitops-reverser`, with a new
`reconcile.fluxcd.io/requestedAt`, and records a `Normal` `Adopted` Event. It is `Suspended=True`,
and `Ready`, once the `Kustomization` has handled that token: a suspended reconcile handles it
without applying, and reconciles of one object never overlap, so by then nothing that was running
before the suspend still is.

**Deleting the trigger hands the `Kustomization` back.** The trigger carries the finalizer
`configbutler.ai/flux-trigger`. Deleting it suspends the `Kustomization` once more if an apply has
it resumed, waits until that is handled, and releases `spec.suspend`, so the `Kustomization` is
resumed and applies on its own again (a `Normal` `Released` Event). That includes a `Kustomization`
somebody had `flux suspend`ed before the trigger adopted it. One whose `spec.suspend` another
manager also applies stays suspended, and the `Released` Event names that manager. A hand-back that
has not finished 10 minutes after the deletion, because kustomize-controller is down for instance,
lets the trigger go anyway with a `Warning` `ReleaseFailed` Event that says where the
`Kustomization` was left. A `GitTarget` that stops naming the trigger releases nothing: the trigger
is what holds the `Kustomization`. With a second `FluxTrigger` in line for the same `Kustomization`,
deleting the holder leaves it suspended for that one rather than releasing it.

The trigger reports `Configured` and `Suspended`, and its `Ready` includes both:

| Reason | Means | Readiness |
| --- | --- | --- |
| `Configured`: `Succeeded` | The `Kustomization` exists, applies a `GitRepository` that follows a branch, and may be patched | ready |
| `Configured`: `ApplierNotFound` | No such `Kustomization` in the applier's cluster, or Flux is not installed there | progressing: Flux objects are often created after the trigger |
| `Configured`: `UnsupportedSource` | The source is not a `GitRepository`, or one of the [`GitRepository` rules](#the-gitrepository) about the source alone is broken | stalled: needs a human |
| `Configured`: `SuspendOwnedElsewhere` | Another field manager applies the `Kustomization`'s `spec.suspend` | stalled: remove the field from the manifest it applies |
| `Configured`: `Forbidden` | The applier cluster's identity may not patch the `Kustomization` or its `GitRepository` | stalled: needs a human |
| `Configured`: `NamespaceNotAuthorized` | The trigger's `clusterProviderRef` names a provider that does not admit its namespace | stalled: needs a human |
| `Configured`: `ClusterProviderNotFound` | The trigger's `clusterProviderRef` names a provider that does not exist | progressing |
| `Configured`: `HeldByAnotherTrigger` | An older `FluxTrigger` names the same `Kustomization` in the same cluster and holds it; this one does nothing, and deleting it releases nothing. It takes over when that one is deleted | stalled: delete one of the two |
| `Suspended`: `Held` | Suspended under GitOps Reverser, and the last suspend is handled | ready |
| `Suspended`: `Fencing` | Just suspended; waiting for the `Kustomization` to handle the suspend | progressing |
| `Ready`: `OperationUnresolved` | An apply has not completed within 5 minutes, or the `Kustomization` was [resumed by hand](#overriding-by-hand), which is recorded `Unresolved` at once | progressing; it clears when the apply does |

The `GitTarget` reports `ApplyTriggerReady`, and its `Ready` includes it: `True` when the trigger is
`Ready` and its `GitRepository` fetches this repository and branch, `SourceMismatch` when it fetches
another, `ApplyTriggerNotFound` when the trigger does not exist, and otherwise the trigger's own
`Ready` reason.

## Permissions

GitOps Reverser looks the `Kustomization` up in the cluster of the trigger's `clusterProviderRef`,
with the identity of that `ClusterProvider`. That identity needs:

| API group | Resource | Verbs |
| --- | --- | --- |
| `kustomize.toolkit.fluxcd.io` | `kustomizations` | `get`, `list`, `patch` |
| `helm.toolkit.fluxcd.io` | `helmreleases` | `list`, to find the `GitRepository`'s consumers; without it the source is never prefetched |
| `source.toolkit.fluxcd.io` | `gitrepositories` | `get`, `patch` |
| `source.toolkit.fluxcd.io` | `helmcharts` | `list`, to find the `HelmRelease`s whose `chartRef` names a `HelmChart` built from the `GitRepository`; without it the source is never prefetched |
| `authorization.k8s.io` | `selfsubjectaccessreviews` | `create`, to check the two above |

For the operator's own cluster, install the chart with `applierAccess.flux: true`, or apply
[`config/rbac/flux-applier-role.yaml`](../config/rbac/flux-applier-role.yaml) and its binding. For
a remote cluster, grant the same to the identity in the `ClusterProvider`'s kubeconfig.

The chart's grant is cluster-wide for convenience. GitOps Reverser only ever patches the
`Kustomization` and `GitRepository` a trigger names, and lists `Kustomization`s and `HelmRelease`s
to find a source's consumers. The access review needs no grant of its own: Kubernetes lets every
authenticated identity create `selfsubjectaccessreviews` (the `system:basic-user` role).

## The Git host's webhook

Point the Git host's push webhook at GitOps Reverser, so a push reaches the cluster in seconds,
after GitOps Reverser has replayed any pending edits onto it.

1. Serve the receiver: set the chart's `receiver.bindAddress` (for example `":9292"`). The chart
   adds a `<release>-receiver` `Service`. The listener is plain HTTP; expose it to your Git host
   with an Ingress or Gateway route that terminates TLS, the way Flux's `webhook-receiver` is
   exposed.
2. Create a Secret with the webhook secret under the key `token`, in the `GitProvider`'s namespace.
   It is the key a Flux `Receiver` Secret uses, so an existing one can be copied over.
3. Set `GitProvider.spec.receiver` with that Secret and the host's `type`: `github`, `gitlab`,
   `gitea`, or `generic-hmac` for a relay of your own. The values are Flux `Receiver` types.
4. Read the path from `status.webhookPath` and register `https://<your route><path>` as a push
   webhook with the same secret:

   ```bash
   kubectl get gitprovider platform -n team-a -o jsonpath='{.status.webhookPath}'
   # /git-push/3f9a…c41e
   ```

   The path is derived from the token, as Flux's `Receiver` path is, so it names no namespace or
   provider. A wrong path and a wrong secret both answer `404`; the operator's log says which.
   Rotating the token moves the path, so register the new one with the new secret.

5. Delete the Flux `Receiver` for this repository and its webhook at the Git host.

A delivery is only a hint. GitOps Reverser reads where the branch is from the remote and
decides from that, so a delivery for one of its own pushes, a duplicate, or a late one applies
nothing it should not. Without a receiver everything still works, later: a foreign push is found by
the next rejected push or by the periodic refresh (`git.refreshInterval`, `10m`, on the 5-minute
reconcile tick).

## What GitOps Reverser asks, and when

| Reason | When | Applied by |
| --- | --- | --- |
| `ForeignCommit` | The branch gained a commit GitOps Reverser did not push. Pending edits are replayed onto it and pushed first | Every `Kustomization` declared on the branch |
| `Refusal` | An edit was refused and the target has `onRefusal: Reapply` | The refusing target's `Kustomization` |
| `Repair` | An apply reverted an API edit after the edit was made (see below) | That `Kustomization` |
| `CatchUp` | The branch has been quiet for `quietPeriod` after GitOps Reverser's own pushes, or since the operator started | Every `Kustomization` declared on the branch |

**One apply is one operation.** On a quiet branch, GitOps Reverser:

1. Makes the `GitRepository`'s artifact hold the commit: when it does not, it stamps
   `reconcile.fluxcd.io/requestedAt` on the `GitRepository` and waits for that fetch. The suspended
   `Kustomization` ignores the new artifact.
2. Reads the artifact again and checks the decision still holds: the commit is still the branch tip,
   and no edit is waiting to be published. When either changed, it asks nothing and decides again at
   the next quiet moment.
3. Records the operation on the trigger's `status.operation`, so a restart finds it.
4. Resumes the `Kustomization`: one server-side apply sets `spec.suspend: false` and a new
   `requestedAt`. That write enqueues a reconcile that reads both, which applies.
5. Waits for the `Kustomization` to report that token handled, which it does when that reconcile
   ends, whether the apply succeeded, failed, or waited on a dependency. `Ready=True` is `Succeeded`;
   `Ready=False` is `Failed`, with Flux's reason.
6. Suspends it again with another token, and the operation completes once that token is handled:
   nothing that started before the suspend is still running.

A `ForeignCommit` or `CatchUp` asks nothing when the `Kustomization` is suspended and has already
applied the commit. A `Refusal` and a `Repair` always run the operation, since they ask for an
apply of an unchanged commit.

**A failed apply is suspended again,** like any other, which is what stops Flux's own
`retryInterval` retry. GitOps Reverser keeps what the `Kustomization` is owed and retries it on its
own backoff (2 seconds doubling to 5 minutes), on a quiet branch, at the tip as it is then. A
request the `Kustomization`'s configuration rejects waits at least 15 seconds, since only a change
to that configuration clears it: a `GitRepository` that fetches another repository or branch, a
source that is not a `GitRepository`, or `spec.suspend` applied by another field manager. An
`ApplyFailed` Event marks the first failure of a run. A resume that no reconcile ran on (somebody
ran `flux suspend` between the resume and Flux's reconcile) counts as failed too, and is asked
again: a handled token alone does not prove Flux applied anything. A request that arrives while the
trigger is still adopting its `Kustomization` (`Suspended=False`, `Fencing`) is not resumed, and is
retried on the same backoff.

**An apply that takes long is followed, not timed out.** After 5 minutes the operation is
`Unresolved`: the `Kustomization` is suspended at once (that stops whatever would follow, not the
apply already running), an `ApplyUnresolved` Event is recorded, and GitOps Reverser keeps following
it until the suspend is handled. Nothing more is asked of that `Kustomization` until then. If an
operation stops being followed (its `GitTarget` was deleted mid-apply, or the process lost track of
it), the `Kustomization` is suspended with a fence before it is let go. The trigger is the backstop:
an operation in flight that nobody follows for a minute is suspended, a `Warning`
`OperationUnfollowed` Event is recorded, and it completes as failed. A resumed `Kustomization` is
never left with nothing watching it.

**An edit made while Flux applies keeps its value.** From the moment an operation is asked for until
10 seconds after it completes, an API edit to a field the `Kustomization` sets can be reverted on the
cluster by that apply, because the commit Flux applies does not hold the edit yet. GitOps Reverser
recognizes that write by its field manager, `kustomize-controller`, and does not mirror it: the
API's value stays in Git, and the withheld write owes a `Repair`, which applies the tip again as
soon as the branch is quiet. Each such write is logged and counted in
`apply_window_collisions_total{outcome="withheld"}`, and the trigger shows `repairOwed` until the
repair succeeds. Only a write proven to be Flux's is withheld: when `kustomize-controller` and
another manager wrote the object in the same second, it is mirrored and counted as `ambiguous`. A
delete is never withheld.

Before each operation, GitOps Reverser reads each `GitTarget` again: it must still be the same
object (a deleted and recreated one inherits nothing), still name this trigger on this branch,
still be admitted by its `ClusterProvider`, and, for a `Refusal`, still consent. When none still
holds the reason, it asks nothing; it does not substitute a weaker request. The operation runs off
the branch worker, so a slow cluster never holds a publication, and a failed one never fails one.

## Reverting refused edits

With `onRefusal: Reapply`, an edit GitOps Reverser cannot write (one that has no home in the folder,
such as a field a kustomize base owns) is reverted by resuming the target's `Kustomization` for one
apply of the tip, once the branch is quiet. A resume alone applies: no new commit is needed. Only
that `Kustomization` applies. If its `GitRepository` has to fetch to reach the tip, the other
consumers GitOps Reverser holds ignore the new artifact; one in `status.source.otherConsumers`
applies it, as it would for any push.

[Reverting a refused edit](configuration.md#reverting-a-refused-edit-speconrefusal) has the
eligibility rules.

## Overriding by hand

- **`flux resume ks checkout`** applies the `Kustomization` at once, unchecked. Within a minute the
  trigger notices, suspends it again with a token, records a `Normal` `ResumedElsewhere` Event, and
  shows the apply as an `Unresolved` operation that completes once nothing it started is running.
  It does so every time: the person who resumes on purpose gets one apply, and a tool that keeps
  resuming gets an Event for each.
- **`flux reconcile ks checkout`** does nothing visible while the `Kustomization` is suspended: Flux
  marks the request handled without applying.
- **`flux suspend ks checkout`** is taken back with a token of GitOps Reverser's.
- To hand the `Kustomization` back to Flux for good, delete the `FluxTrigger`.

## Checking that it works

```bash
# The applier is found, applies a branch, may be patched, and is held; and it applies this branch.
kubectl get fluxtrigger checkout -n team-a
kubectl get gittarget checkout -n team-a \
  -o jsonpath='{.status.conditions[?(@.type=="ApplyTriggerReady")]}{"\n"}'

# The last apply, why, and where it stands; and what else applies the same source.
kubectl get fluxtrigger checkout -n team-a -o jsonpath='{.status.operation}{"\n"}{.status.source}{"\n"}'
# {"appliedRevision":"main@sha1:4f2c1ab9...","commit":"4f2c1ab9...","fenceToken":"...","phase":"Succeeded","reason":"ForeignCommit",...}

# Each apply asked for, each one that could not be asked or did not complete.
kubectl get events -n team-a --field-selector involvedObject.name=checkout,involvedObject.kind=GitTarget

# Flux's own view: suspended, and the last revision it applied.
flux get ks checkout -n flux-system
```

`gitopsreverser_apply_requests_total{reason,outcome}` counts the requests;
[interpreting metrics](interpreting-metrics.md#apply-requests-and-the-push-receiver) reads it.

## Troubleshooting

| Symptom | Cause | Fix |
| --- | --- | --- |
| Trigger `Configured=False`, `ApplierNotFound` | The `Kustomization` does not exist in the trigger's cluster under that namespace and name, or Flux is not installed there | Create it, or recreate the trigger with the right `kustomizationRef`; an omitted namespace is the trigger's |
| Trigger `Configured=False`, `SuspendOwnedElsewhere` | The manifest that applies the `Kustomization` sets `spec.suspend` | Remove the field from that manifest |
| `ApplyTriggerReady=False`, `SourceMismatch`, or trigger `UnsupportedSource` | The message names the broken [`GitRepository` rule](#the-gitrepository) | Fix the `GitRepository`; what was owed is applied afterward |
| Trigger `Configured=False`, `Forbidden` | The identity lacks one of the [permissions](#permissions) | Grant it, or set `applierAccess.flux: true` |
| A Warning `ApplyRequestFailed` Event | An apply could not be asked for; the message says why | GitOps Reverser keeps asking on a quiet branch, at most every five minutes. It is recorded for the first failure of a run |
| A Warning `ApplyFailed` Event | Flux ran the apply and reported it failed | Read the `Kustomization`'s status; GitOps Reverser retries on its backoff |
| A Warning `ApplyUnresolved` Event, trigger `Ready=False`, `OperationUnresolved` | The apply has not completed in 5 minutes | It is suspended and still followed; look at the `Kustomization`'s status and health checks |
| A Warning `OperationUnfollowed` Event | An apply was left in flight with nothing following it | It was suspended again and its operation completed as failed. Once is harmless; a recurring one is a bug worth reporting with the operator's logs |
| A Warning `ReleaseFailed` Event | A deleted trigger could not hand its `Kustomization` back (forbidden, or not done within 10 minutes) | The message says where the `Kustomization` was left; `flux resume` it if it should apply on its own |
| `flux get ks` shows `Suspended` | That is how the trigger holds it | Nothing to fix; see [overriding by hand](#overriding-by-hand) |
| A dependent `Kustomization` waits | Its dependency's last apply failed, and it is suspended with that generation unapplied | Fix the dependency's failure; GitOps Reverser's retry unblocks it |
| A Git-side push takes minutes to reach the cluster | No receiver, so the refresh finds it | Configure [the webhook](#the-git-hosts-webhook) |
| A refused edit is not reverted | The target is suspended, `onRefusal` is not `Reapply`, or the refusal is not eligible | See [reverting a refused edit](configuration.md#reverting-a-refused-edit-speconrefusal) |

## While GitOps Reverser is down

**Flux applies nothing while GitOps Reverser is down.** A Git-side push waits until it is back, or
until somebody runs `flux resume`, after which GitOps Reverser suspends the `Kustomization` again
when it starts. This is the trade Argo CD with automated sync off makes too, and the price of an
applier that applies only when it is safe.

An apply that was running when it stopped is not lost. The operation is recorded on the trigger
before the `Kustomization` is resumed, so the next process follows it to its end by its tokens, with
the apply window open, and suspends the `Kustomization` again. Its first look at the remote owes
every `Kustomization` a `CatchUp` and no `ForeignCommit`: an apply on startup could revert edits
made while it was down, before its watches have caught them.

## What the tests prove

| Covered | Where |
| --- | --- |
| The [complete example](#a-complete-example) decodes strictly, reports `Configured=True`, binds the `GitTarget`, and an apply fetches and resumes the `Kustomization`; the manifest omits `spec.suspend` | [`flux_guide_test.go`](../internal/applier/flux_guide_test.go) |
| Every fact of kustomize-controller v1.9.6 the lifecycle relies on, in a fake that applies them | [`fluxsim`](../internal/applier/fluxsim/fluxsim_test.go) |
| An operation is not finished when the revision is attempted; a token handled while suspended is no apply; a resume alone reverts a refusal; a suspend does not stop a running apply; the artifact is rechecked before the resume; a failed apply is suspended and Flux's requeue applies nothing; a shared source applies only the consumer asked | [`flux_lifecycle_test.go`](../internal/applier/flux_lifecycle_test.go), [`flux_consumers_test.go`](../internal/applier/flux_consumers_test.go) |
| Adoption, an external resume suspended again with an Event each time, `SuspendOwnedElsewhere`, the hand-back on deletion, and the source's other consumers | [`fluxtrigger_hold_test.go`](../internal/controller/fluxtrigger_hold_test.go) |
| `ApplierNotFound`, then `SourceMismatch` for a source on another branch, reported from a running cluster; the `Kustomization` is adopted and stays suspended | [Manager Flux Applier](../test/e2e/flux_applier_e2e_test.go) |
| A `ForeignCommit` for a `SourceMismatch` applier never stamps the other branch's source, records `ApplyRequestFailed`, and is applied once the source is fixed | Manager Flux Applier |
| A Gitea push webhook to the operator's receiver gets a Git-side push applied, with nothing else on the branch; with no receiver and no API edit, the refresh finds one | Manager Flux Applier |
| A failed apply is suspended again and retried by GitOps Reverser; a `flux resume` is suspended again with a `ResumedElsewhere` Event; deleting the trigger hands the `Kustomization` back | Manager Flux Applier |
| A manager restarted during an apply completes that same operation and leaves the `Kustomization` suspended | [Flux Restart Recovery](../test/e2e/flux_restart_recovery_e2e_test.go) |
| Two API edits seconds apart end at the later value with no extra commit, and Flux catches up by itself | [Bi-directional (Flux)](../test/e2e/flux_bi_directional_e2e_test.go) |
| A Git-side push with an API edit on top lands both, and Flux applies the replayed commit | Bi-directional (Flux) |
| A refused edit with `onRefusal: Reapply` is reverted by a resume, with no commit, and settles | [Refusal reapply](../test/e2e/refusal_reapply_e2e_test.go) |
| Every operation checks the target again (incarnation, branch, admission, consent) and the source binding before it resumes, records itself first, and never asks for something weaker | [`dispatcher_test.go`](../internal/applier/dispatcher_test.go) |
| A failed attempt is decided again on a quiet branch; each `Kustomization` keeps its own backoff; a shared one is asked once; appliers sharing a source apply one at a time; a push is prefetched only while the branch is busy | [`apply_attempt_test.go`](../internal/git/apply_attempt_test.go), [`apply_groups_test.go`](../internal/git/apply_groups_test.go) |

`task test-e2e` runs the Manager specs and Flux Restart Recovery, and `task
test-e2e-bi-directional` runs the bi-directional corner. Not covered end to end yet: a remote
source cluster, `Forbidden` against cluster RBAC, and the receiver installed by the Helm chart
rather than the e2e manifests.
