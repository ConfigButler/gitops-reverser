# Using GitOps Reverser with Flux

When GitOps Reverser publishes a folder that Flux applies, GitOps Reverser decides when Flux
applies it. Flux still fetches, renders, applies, prunes and reports health; it applies when
GitOps Reverser asks, instead of on its own timers or on a Git-host webhook. This page is the
complete Flux setup: what to change on the `GitRepository` and the `Kustomization`, what to declare
on the `GitTarget`, where the Git host's webhook goes, how to check it works, and which tests prove
each part.

The [bi-directional guide](bi-directional.md) explains the model behind this for any applier, and
the [configuration reference](configuration.md#who-applies-this-folder-specapplier) has every field
and condition reason.

## What changes

| Before | With GitOps Reverser |
| --- | --- |
| The `GitRepository` fetches every minute or so | It fetches when GitOps Reverser asks; its `interval` is a 24h backstop |
| The `Kustomization` applies on its interval | It applies when the new artifact arrives, or when GitOps Reverser asks; its `interval` is a 24h backstop |
| The Git host's push webhook goes to a Flux `Receiver` | It goes to GitOps Reverser's receiver; the Flux `Receiver` for this repository is removed |
| Nothing ties a folder to its `Kustomization` | The `GitTarget` names it in `spec.applier.kustomizationRef` |

## Why Flux must not fetch on its own

**A new `GitRepository` artifact makes every `Kustomization` that reads it apply, at once.** Flux
has no mode that fetches without applying, and `suspend: true` on the `Kustomization` also blocks
the requests GitOps Reverser makes. So a source that fetches on its own, on a short interval or on a
webhook into a Flux `Receiver`, is a `Kustomization` that applies on its own. That brings back the
two races GitOps Reverser exists to close:

- **Its own pushes.** Two API edits a few seconds apart produce two commits. A fetch between them
  applies the first commit over the second edit, and GitOps Reverser then publishes the revert as a
  third commit.
- **Somebody else's push.** While an API edit is still in an open commit window, a Git-side push
  arrives. Applied straight away, the push's tree reverts the edit, because the edit is not in Git
  yet. GitOps Reverser instead closes the window, replays the edit onto the push, pushes the
  result, and only then asks Flux to apply.

Both apply something correct; they apply it at the wrong moment. Only GitOps Reverser can see the
edits that are live and not yet pushed, so only it can tell when an apply is safe. It sends a
request only on a **quiet branch**: no open commit window and no pending writes.

The source still stays current. After GitOps Reverser's own pushes, once the branch has been quiet
for `spec.applier.quietPeriod`, it asks the `GitRepository` to fetch if its artifact is behind
(reason `CatchUp`). That matters because Flux still applies on a controller restart and on its long
interval, from whatever artifact it holds, and an artifact left behind would revert edits that are
already in Git.

## A complete example

One folder, `apps/checkout` on branch `main` of `github.com/acme/platform`, published by GitOps
Reverser and applied by the `Kustomization` `flux-system/checkout`. Every object below is checked by
a unit test against the code that validates and drives the applier (see
[what the tests prove](#what-the-tests-prove)), so this example cannot drift from what is accepted.

The Flux side. The `GitRepository` follows exactly the branch the `GitTarget` writes, and both
intervals are long:

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
  interval: 24h
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
  interval: 24h
  retryInterval: 1m
```

The GitOps Reverser side. The `GitProvider` points at the same repository (any URL form of it) and
serves a push receiver; the `GitTarget` names the `Kustomization` as its applier:

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
    kustomizationRef:
      namespace: flux-system
      name: checkout
    quietPeriod: 30s
  onRefusal: Reapply
```

No Flux `Receiver` for `acme/platform` appears, on purpose: remove yours if you have one.

## The `GitRepository`

`ApplierConfigured` is `True` only when the `Kustomization`'s source fetches the branch the
`GitTarget` writes. Each rule below is checked when the target reconciles and again before every
request, so a source changed after the check is still never stamped for the wrong branch.

| Rule | Reported when broken |
| --- | --- |
| `spec.url` is the `GitProvider`'s repository. The forms compare as one repository: `https://` and `ssh://`, `git@host:org/repo` (SCP style), a trailing `.git` or `/`, letter case, and the default port | `SourceMismatch`: `fetches <url>, not <url>` |
| `spec.ref.branch` is exactly the `GitTarget`'s `spec.branch` | `SourceMismatch`: `fetches branch "release", not main` |
| `spec.ref` pins nothing stronger: no `commit`, `name`, `semver` or `tag`, even alongside the branch. Flux resolves those before the branch, so the artifact would never follow it | `SourceMismatch`: `pins spec.ref.tag "v1.0.0", so it never follows branch main` |
| `spec.ref` is set | `SourceMismatch`: `names no spec.ref.branch` |

Set `spec.interval` long, `24h` in the example. It is Flux's backstop for the case where GitOps
Reverser is down; a short one is a source that fetches, and so applies, on its own.

## The `Kustomization`

- `spec.sourceRef` names a `GitRepository`. A `Kustomization` that applies from an `OCIRepository`
  or a `Bucket` reports `SourceMismatch`: GitOps Reverser writes Git, and can only ask a Git source
  to fetch.
- `spec.interval` is long, for the same reason as the source's.
- `spec.suspend` is not `true`. A suspended `Kustomization` ignores the requests.
- `spec.retryInterval` is how soon Flux retries an apply that **failed**, on its own. Keep it, since
  a failed apply otherwise waits out the 24h interval, and know that this one retry is not gated on
  a quiet branch.
- It may live in any namespace (`flux-system` is the usual one). The reference's namespace defaults
  to the `GitTarget`'s own. A request reaches only a `Kustomization` whose source fetches this
  `GitProvider`'s repository and branch, so naming one elsewhere grants nothing that write access to
  the repository does not already give.
- Several `GitTarget`s may name one `Kustomization`, for example one whose `path` covers several
  folders. It is asked once per request, however many of them share it.

## The `GitTarget`

```yaml
spec:
  applier:
    kustomizationRef:
      namespace: flux-system   # default: this GitTarget's namespace
      name: checkout
    quietPeriod: 30s           # default 30s, at most 1h
```

`quietPeriod` is how long the branch must stay quiet after GitOps Reverser's own pushes before Flux
is asked to catch up with them. Every new write restarts it. A short one keeps Flux's artifact
current; a long one makes it less likely that an edit arrives while that catch-up apply runs.

The target reports `ApplierConfigured`, and `Ready` includes it:

| Reason | Means | Readiness |
| --- | --- | --- |
| `Succeeded` | The `Kustomization` exists, applies this repository and branch, and may be patched | ready |
| `ApplierNotFound` | No such `Kustomization` in the source cluster, or Flux is not installed there | progressing: Flux objects are often created after the `GitTarget` |
| `SourceMismatch` | One of the [`GitRepository` rules](#the-gitrepository) is broken, or the source is not a `GitRepository` | stalled: needs a human |
| `Forbidden` | The source-cluster identity may not patch the `Kustomization` or its `GitRepository` | stalled: needs a human |

## Permissions

GitOps Reverser looks the `Kustomization` up in the **source cluster** (the cluster it applies to)
with the identity of the target's `ClusterProvider`. That identity needs:

| API group | Resource | Verbs |
| --- | --- | --- |
| `kustomize.toolkit.fluxcd.io` | `kustomizations` | `get`, `patch` |
| `source.toolkit.fluxcd.io` | `gitrepositories` | `get`, `patch` |
| `authorization.k8s.io` | `selfsubjectaccessreviews` | `create`, to check the two above |

For the operator's own cluster, install the chart with `applierAccess.flux: true`, or apply
[`config/rbac/flux-applier-role.yaml`](../config/rbac/flux-applier-role.yaml) and its binding. For
a remote source cluster, grant the same to the identity in the `ClusterProvider`'s kubeconfig.

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
   # /git-push/team-a/platform
   ```

5. Delete the Flux `Receiver` for this repository and its webhook at the Git host.

A delivery is only a hint. GitOps Reverser reads where the branch is from the remote and
decides from that, so a delivery for one of its own pushes, a duplicate, or a late one asks Flux for
nothing it should not. Without a receiver everything still works, later: a foreign push is found by
the next rejected push or by the periodic refresh (`git.refreshInterval`, `10m`, on the 5-minute
reconcile tick).

## What GitOps Reverser asks, and when

| Reason | When | Asked |
| --- | --- | --- |
| `ForeignCommit` | The branch gained a commit GitOps Reverser did not push. Pending edits are replayed onto it and pushed first | Every `Kustomization` declared on the branch |
| `Refusal` | An edit was refused and the target has `onRefusal: Reapply` | The refusing target's `Kustomization` |
| `CatchUp` | The branch has been quiet for `quietPeriod` after GitOps Reverser's own pushes, or since the operator started | Every `Kustomization` declared on the branch |

A request is what `flux reconcile kustomization --with-source` does, minus the waiting. GitOps
Reverser merge-patches `reconcile.fluxcd.io/requestedAt` on the `GitRepository` when its artifact
does not hold the commit; the new artifact then wakes the `Kustomization`. When the artifact
already holds it, it stamps the `Kustomization` instead, and a `CatchUp` stamps nothing at all.

**It is asynchronous.** The patch returns as soon as the API server accepts it, and nothing waits
for Flux to fetch or apply. It runs off the branch worker, so a slow source cluster never holds a
publication, and a failed request never fails one.

Before every attempt, retries included, GitOps Reverser reads the `GitTarget` again: it must still
name this `Kustomization` on this branch, its `ClusterProvider` must still admit it, and a
`Refusal` must still have consent. A request that fails is retried for about one `quietPeriod`,
because a retry skips the quiet check, and is then handed back to the branch worker, which asks
again once the branch is quiet, waiting longer each time up to five minutes. A failure that cannot
be retried, such as a `SourceMismatch`, is handed back at once and delivered once it is fixed.

## Reverting refused edits

With `onRefusal: Reapply`, an edit GitOps Reverser cannot write (one that has no home in the folder,
such as a field a kustomize base owns) is reverted by asking the target's `Kustomization` to apply
the tip, once the branch is quiet. Only that `Kustomization` is asked. If its `GitRepository` has to
fetch to reach the tip, though, every `Kustomization` on that `GitRepository` applies the new
revision, as it would for any push. Give the folder its own `GitRepository` if the revert must reach
that folder alone.

[Reverting a refused edit](configuration.md#reverting-a-refused-edit-speconrefusal) has the
eligibility rules.

## Checking that it works

```bash
# The applier is found, applies this repository and branch, and may be patched.
kubectl get gittarget checkout -n team-a \
  -o jsonpath='{.status.conditions[?(@.type=="ApplierConfigured")]}{"\n"}'

# What GitOps Reverser last asked Flux for, and why.
kubectl get gittarget checkout -n team-a -o jsonpath='{.status.applier}{"\n"}'
# {"reason":"ForeignCommit","requestedAt":"2026-10-20T10:14:03Z","requestedCommit":"4f2c1ab9..."}

# Each request, and each request that could not be delivered.
kubectl get events -n team-a --field-selector involvedObject.name=checkout,involvedObject.kind=GitTarget

# What Flux actually applied: a request asks, it never reports.
kubectl get kustomization checkout -n flux-system -o jsonpath='{.status.lastAppliedRevision}{"\n"}'
```

`gitopsreverser_apply_requests_total{reason,outcome}` counts the requests;
[interpreting metrics](interpreting-metrics.md#apply-requests-and-the-push-receiver) reads it.

## Troubleshooting

| Symptom | Cause | Fix |
| --- | --- | --- |
| `ApplierConfigured=False`, `ApplierNotFound` | The `Kustomization` does not exist in the source cluster under that namespace and name, or Flux is not installed there | Create it, or fix `kustomizationRef`; an omitted namespace is the `GitTarget`'s |
| `ApplierConfigured=False`, `SourceMismatch` | The message names the broken [`GitRepository` rule](#the-gitrepository) | Fix the `GitRepository`; the request it was holding is delivered afterward |
| `ApplierConfigured=False`, `Forbidden` | The identity lacks one of the [permissions](#permissions) | Grant it, or set `applierAccess.flux: true` |
| A Warning `ApplyRequestFailed` Event | A request could not be delivered; the message says why | GitOps Reverser keeps asking on a quiet branch, at most every five minutes. It is recorded once per failing request |
| Flux applies while edits are in flight | Something else still makes the source fetch: a short `interval`, a Flux `Receiver`, or someone running `flux reconcile` | Lengthen the intervals and remove the Flux `Receiver` |
| A Git-side push takes minutes to reach the cluster | No receiver, so the refresh finds it | Configure [the webhook](#the-git-hosts-webhook) |
| A refused edit is not reverted | The target is suspended, `onRefusal` is not `Reapply`, or the refusal is not eligible | See [reverting a refused edit](configuration.md#reverting-a-refused-edit-speconrefusal) |

## While GitOps Reverser is down

GitOps Reverser becomes the only fast path to a deployment. While it is down, a Git-side push
reaches the cluster through `flux reconcile kustomization <name> --with-source`, which is safe then
because nothing is publishing, or through the 24h backstop. When it starts again, its first look at
the remote owes every `Kustomization` a `CatchUp`, never a `ForeignCommit`: an apply on startup
could revert edits made while it was down, before its watches have caught them.

## What the tests prove

| Covered | Where |
| --- | --- |
| The [complete example](#a-complete-example) decodes strictly, reports `ApplierConfigured=True`, and its request stamps the `GitRepository`; the Flux objects follow this page's advice | [`flux_guide_test.go`](../internal/applier/flux_guide_test.go) |
| `ApplierNotFound`, then `SourceMismatch` for a source on another branch, reported from a running source cluster | [Manager Flux Applier](../test/e2e/flux_applier_e2e_test.go) |
| A `ForeignCommit` for a `SourceMismatch` applier never stamps the other branch's source, records `ApplyRequestFailed`, and is delivered once the source is fixed | Manager Flux Applier |
| A Gitea push webhook to the operator's receiver gets a Git-side push applied, with nothing else on the branch | Manager Flux Applier |
| With no receiver and no API edit, the refresh finds a Git-side push and Flux applies it | Manager Flux Applier |
| Two API edits seconds apart end at the later value with no extra commit, and Flux catches up by itself | [Bi-directional (Flux)](../test/e2e/flux_bi_directional_e2e_test.go) |
| A Git-side push with an API edit on top lands both, and Flux applies the replayed commit | Bi-directional (Flux) |
| A new `GitRepository` revision alone makes the `Kustomization` apply, which is why the source must not fetch on its own | Bi-directional (Flux) |
| A refused edit with `onRefusal: Reapply` is reverted by its `Kustomization`, with no commit, and settles | [Refusal reapply](../test/e2e/refusal_reapply_e2e_test.go) |
| Every attempt checks the target again (admission, branch, consent) and the source binding before it stamps; a failed request goes back to the branch worker, which asks again | [`dispatcher_test.go`](../internal/applier/dispatcher_test.go), [`apply_handback_test.go`](../internal/git/apply_handback_test.go) |

`task test-e2e` runs the Manager specs, and `task test-e2e-bi-directional` runs the bi-directional
corner. Not covered end to end yet: a remote source cluster, `Forbidden` against cluster RBAC, and the
receiver installed by the Helm chart rather than the e2e manifests.
