# Configuration model

This guide explains the configuration objects that drive gitops-reverser after the install steps
in the [quickstart](quickstart.md).

The short version:

- `GitProvider` defines where and how to push
- `GitBranch` holds one branch of that repository, for every target that writes it
- `KubeProvider` defines the Kubernetes source cluster a target mirrors from
- `GitTarget` defines which source cluster to mirror, and which folder on a `GitBranch` to write into
- `WatchRule` defines which namespaced resources should produce Git writes, and in which source
  namespaces
- `ClusterWatchRule` does the same for cluster-scoped resources
- `CommitRequest` optionally saves: it closes its author's commit window on its own timers, with
  its own message if it has one
- `FluxClaim` and `ArgoCDClaim` hold the Flux `Kustomization` or Argo CD `Application` that
  applies a folder back, so GitOps Reverser decides when it applies

The chart's optional `quickstart` values are a convenience layer that creates starter
instances of those same resources.

For a first trial, use the [quickstart](quickstart.md). It runs configured-author: Git writes work without
kube-apiserver audit delivery, and every commit uses the configured committer identity. Add audit
attribution later only when you need named Kubernetes users or service accounts in Git history.

## How the objects fit together

![Config basics: a WatchRule or ClusterWatchRule selects what a GitTarget mirrors; the GitTarget writes its folder through a GitBranch and a GitProvider with a credentials Secret, and reads the cluster through a KubeProvider. The direction is always from the cluster into Git](images/config-basics.excalidraw.svg)

The usual flow is:

1. Create a `GitProvider` for repository access and commit behavior.
2. Create a `GitBranch` naming that provider and the branch to write.
3. Create a `GitTarget` that points at the `GitBranch`, the `KubeProvider` to mirror from, and a
   folder in the repository.
4. Create one or more `WatchRule` or `ClusterWatchRule` objects that point at that target.
5. Create a `CommitRequest` only when you want a save: your writes in one commit, on the request's
   timers rather than the target's, and optionally with your message.
6. Only when Flux or Argo CD also applies that same folder back to the cluster: hold its
   `Kustomization` or `Application` with an applier claim, name the applier claim on the `GitTarget`, and
   point the Git host's push webhook at a [`GitReceiver`](#gitreceiver), so GitOps Reverser decides
   when Git is applied. See
   [who applies this folder](#who-applies-this-folder-specapplierclaimref) and
   [bi-directional GitOps](bi-directional.md). Flux and Argo CD applying other folders need nothing.

That means one repository connection can back multiple branches, one branch multiple targets, and
one target can be fed by multiple watch rules.

> **A `KubeProvider` usually exists already.** The chart creates `local` in its release namespace,
> for the operator's own cluster, and a `GitTarget` names it with
> `spec.kubeProviderRef: {name: local, namespace: gitops-reverser}`. Write your own to mirror a
> remote cluster, or to keep a connection and its credential in a team's namespace.

<!-- BEGIN GENERATED: settings-index (task settings-index) -->

## Field overview

Ready-made manifests for all ten objects live in [`config/samples/`](../config/samples/). They
cross-reference the same `example-provider` / `example-target` names, so the set can be applied
together and edited down, rather than assembled from the fragments below.

Every field not marked **required** may be omitted. Sections linked from the tables explain the
fields that need more than a line.

### `GitProvider` (namespaced): where and how to push

| Field | Default | What it does |
|---|---|---|
| `url` | **required** | Repository URL, SSH or HTTPS |
| `allowedGitBranches` | **no `GitBranch`**, its own namespace included | Which namespaces' `GitBranch`es may claim which branches through this provider. Each grant has a `consumerNamespaces` block (`from`: `Same`, `Names`, `Selector`, `All`) and the `branchPatterns` it allows. See [the grants](#the-grants) |
| `secretRef` | anonymous access | Secret holding the write credential. See [the credentials Secret](#gitproviderspecsecretref-the-credentials-secret) |
| `knownHostsRef` | the Secret's own `known_hosts` | Optional ConfigMap or Secret sharing SSH host keys across providers. Host-key verification fails closed either way |
| `commit.committer` | the operator's built-in identity | Name and email written as the commit committer. See [committer identity](#committer-identity) |
| `commit.signing` | off | SSH commit signing. See [commit signing](#commit-signing) |

### `GitBranch` (namespaced): one branch of the repository

Every field but `allowedGitTargets` is immutable: to change one, delete the `GitBranch` and create a new one.

| Field | Default | What it does |
|---|---|---|
| `gitProviderRef` | **required** | The `GitProvider` whose repository and credential write this branch: in this namespace, or `namespace` names another. Its `allowedGitBranches` must admit this namespace and `branch` |
| `branch` | **required** | Branch to write. Only one `GitBranch` holds a repository branch; see [who writes a repository branch](#who-writes-a-repository-branch) |
| `allowedGitTargets` | `[{"consumerNamespaces":{"from":"Same"}}]` | Which namespaces' `GitTarget`s may write through this branch, each grant with a `consumerNamespaces` block. Omitted, this branch's own namespace; `[]` admits nobody. The only mutable field. See [the grants](#the-grants) |
| `parentBranch` | the remote's default branch | Branch a `branch` the remote does not have yet is created from. Only read, so it need not be allowed. See [starting the write branch from another branch](#starting-the-write-branch-from-another-branch-specparentbranch) |

### `GitReceiver` (namespaced): the endpoint a Git host notifies on a push

| Field | Default | What it does |
|---|---|---|
| `type` | **required** | Payload and signature the host sends: `github`, `gitlab`, `gitea`, or `generic-hmac` for a relay of your own. See [`GitReceiver`](#gitreceiver) |
| `secretRef` | **required** | Secret, in this namespace, whose `token` key is the webhook secret. It also derives `status.webhookPath` |

### `KubeProvider` (namespaced): the source cluster

Set exactly one of `secretRef` and `inCluster`. Both are immutable: delete and recreate to repoint.

| Field | Default | What it does |
|---|---|---|
| `secretRef` | none; set `inCluster` instead | Secret in this provider's namespace holding a kubeconfig for the source cluster. `key` defaults to `value`, then `value.yaml`. See [`KubeProvider`](#kubeprovider) |
| `inCluster` | none; set `secretRef` instead | `{}` uses the operator's own identity. Honored only for a provider listed in `--in-cluster-kube-providers`. See [the operator's own identity](#the-operators-own-identity-specincluster) |
| `allowedGitTargets` | `[{"consumerNamespaces":{"from":"Same"},"sourceNamespaces":["{consumer.namespace}"]}]` | Which namespaces' `GitTarget`s may mirror through this provider, and what: each grant has a `consumerNamespaces` block, and `sourceNamespaces`, `clusterResources` or both. Omitted, this provider's own namespace mirrors the namespace of the same name; `[]` admits nobody. See [the grants](#the-grants) |
| `allowedApplierClaims` | `[{"applierNamespaces":["{consumer.namespace}"],"consumerNamespaces":{"from":"Same"}}]` | Which namespaces' applier claims may hold an applier through this provider, and the `applierNamespaces` that applier may live in. Omitted, this provider's own namespace, for appliers in the namespace of the same name; `[]` admits nobody. See [the grants](#the-grants) |
| `client.qps` | `20` | Client rate limit for this source cluster. Refused beside `inCluster` |
| `client.burst` | `30` | Client burst for this source cluster. Refused beside `inCluster` |
| `attribution.auditRoute` | `<namespace>.<name>` | Audit route this provider joins. A route is held by one namespace. See [audit route](#audit-route-and-auditfactsreceived) |
| `commitRequests` | `Ignore` | Whether the `CommitRequest`s created on this cluster are served, and finished ones deleted: `Ignore`, `Reconcile`, `ReconcileAndDeleteFinished`. See [serving them](#serving-the-commitrequests-of-a-source-cluster) |

### `GitTarget` (namespaced): which branch and folder

The destination fields are immutable: to move a target, delete it and create a new one.

| Field | Default | What it does |
|---|---|---|
| `gitBranchRef` | **required** | The `GitBranch` this target writes: its own namespace, or `namespace` names another. Its `allowedGitTargets` must admit this namespace |
| `path` | **required** | Folder within the repository. `.` targets the root; empty is rejected |
| `kubeProviderRef` | **required** | The `KubeProvider` to mirror from: its own namespace, or `namespace` names another. Its `allowedGitTargets` must admit this namespace |
| `commit.window.idleTimeout` | `5s` | Close a commit window after this much silence. `0s` commits every write on its own. See [the commit window](#the-commit-window-speccommitwindow) |
| `commit.window.maxDuration` | `1m` | Close a commit window this long after it opened, however much keeps arriving |
| `commit.message` | the built-in templates | How commits are phrased. See [commit messages](commit-messages.md) |
| `allowedCommitRequests` | `[{"consumerNamespaces":{"from":"Same"}}]` | The `CommitRequest`s that may save this target's windows: `consumerNamespaces` on the cluster GitOps Reverser runs in, `sourceNamespaces` on the one it mirrors. See [who may save](#who-may-save-specallowedcommitrequests) |
| `placement.byType` | the built-in path | Per-type path templates for new documents. See [where new resources are written](#where-new-resources-are-written-specplacement) |
| `placement.default` | the built-in path | Catch-all path template for types with no `byType` entry |
| `placement.useKustomize` | off | Keep the folder a kustomize folder. See [keeping the folder a kustomize folder](#keeping-the-folder-a-kustomize-folder-specplacementusekustomize) |
| `serializeNamespace` | inferred per document | Whether a committed document carries its own `metadata.namespace`. An explicit `false` admits exactly one source namespace. See [whether documents carry their namespace](#whether-documents-carry-their-namespace-specserializenamespace) |
| `prune.mode` | `OnEvent` | Which deletions reach Git. See [deletion policy](#deletion-policy-specprunemode) |
| `applierClaimRef` | the applier applies on its own schedule | The `FluxClaim` or `ArgoCDClaim`, in this namespace, holding the applier of this folder, which GitOps Reverser then asks to apply. See [who applies this folder](#who-applies-this-folder-specapplierclaimref) |
| `onRefusal` | `Ignore` | What happens to an edit with no legal destination. See [reverting a refused edit](#reverting-a-refused-edit-speconrefusal) |
| `encryption` | off | SOPS + age encryption for sensitive resources |
| `suspend` | `false` | Stop writing without deleting the target. See [stopping a target from writing](#stopping-a-target-from-writing-specsuspend) |

### `WatchRule` (namespaced): what to capture

| Field | Default | What it does |
|---|---|---|
| `gitTargetRef` | **required** | The `GitTarget` these rules feed |
| `rules[].resources` | **required** | Plural resource names to watch |
| `rules[].apiGroups` | resolved from the resource name | API groups to match. `[""]` is the core group. Omitted resolves the named resource across the served surface, and selects nothing when more than one group serves that name |
| `rules[].apiVersions` | the preferred served version | API versions to match. `["*"]` watches every served version |
| `rules[].sourceNamespace` | the rule's own namespace | Namespace to watch in the source cluster, or `*`. See [watching a different source namespace](#watching-a-different-source-namespace) |
| `rules[].objectSelector` | every object | Label selector the API server applies to the item's LIST and WATCH. See [selecting objects by label](#selecting-objects-by-label-rulesobjectselector) |

### `ClusterWatchRule` (cluster-scoped): what to capture, cluster-wide

The same shape as a `WatchRule`, for cluster-scoped types. It selects no namespaces, so it has no `sourceNamespace`.

| Field | Default | What it does |
|---|---|---|
| `gitTargetRef` | **required** | The `GitTarget` these rules feed |
| `rules[].resources` | **required** | Plural cluster-scoped resource names to watch |
| `rules[].apiGroups` | resolved from the resource name | API groups to match. Omitted resolves the named resource across the served surface, and selects nothing when more than one group serves that name |
| `rules[].apiVersions` | the preferred served version | API versions to match. `["*"]` watches every served version |
| `rules[].objectSelector` | every object | Label selector the API server applies to the item's LIST and WATCH. See [selecting objects by label](#selecting-objects-by-label-rulesobjectselector) |

### `FluxClaim` (namespaced): the Flux `Kustomization` GitOps Reverser asks to apply

The references are immutable: to hold another `Kustomization`, delete the applier claim and create a new one.

| Field | Default | What it does |
|---|---|---|
| `kubeProviderRef` | **required** | The `KubeProvider` whose cluster holds the `Kustomization`: the applier claim's own namespace, or `namespace` names another. Its `allowedApplierClaims` must admit this namespace |
| `kustomizationRef` | **required** | The `Kustomization` that applies the folders of the `GitTarget`s naming this applier claim. See [who applies this folder](#who-applies-this-folder-specapplierclaimref) |
| `catchUpAfter` | `1m` | How long the `Kustomization` stays behind the branch after a push of GitOps Reverser's own before an apply of the tip is asked for. Such a push needs no apply for the cluster, but leaves `status.lastAppliedRevision` and the `GitRepository`'s artifact at the commit before it. The interval starts when the `Kustomization` is first seen behind, so a burst of saves costs one apply; the apply can still wait for startup, a retry backoff or edits being published, and an edit arriving before the resume defers it by an interval. `0s` never asks for this reason, and drops an apply it asked for that has not started; otherwise `10s` to `24h` |

### `ArgoCDClaim` (namespaced): the Argo CD `Application` GitOps Reverser asks to sync

The references are immutable: to hold another `Application`, delete the applier claim and create a new one.

| Field | Default | What it does |
|---|---|---|
| `kubeProviderRef` | **required** | The `KubeProvider` whose cluster holds the `Application`: the applier claim's own namespace, or `namespace` names another. Its `allowedApplierClaims` must admit this namespace |
| `applicationRef` | **required** | The `Application`, with automated sync off, that syncs the folders of the `GitTarget`s naming this applier claim. See [who applies this folder](#who-applies-this-folder-specapplierclaimref) |
| `catchUpAfter` | `0s` | As a `FluxClaim`'s: how long the `Application`'s last sync stays behind the branch after a push of GitOps Reverser's own before a sync of the tip is asked for. Off by default, since a sync runs the `Application`'s hooks and Argo CD's refresh already compares the live state against the tip. Otherwise `10s` to `24h` |

### `CommitRequest` (namespaced, here or on a source cluster): save now

| Field | Default | What it does |
|---|---|---|
| `gitTargetRef` | **required** | The target whose commit window the request attaches to: in this namespace, or `namespace` names another, whose `allowedCommitRequests` must admit this one. On a source cluster `namespace` is required. See [where a request is created](#where-a-request-is-created) |
| `message` | the target's templates | Commit message, committed verbatim unless `requestTemplate` frames it |
| `window.attach` | `CurrentOrNext` | Which window to attach to: the author's current or next one, or only the next. See [the request window](#the-request-window-specwindow) |
| `window.attachTimeout` | `5s` | How long to wait for a window to attach to |
| `window.idleTimeout` | no idle close | Close the attached window after this much silence |
| `window.maxDuration` | `2s` | Close the attached window this long after the attach |
| `whenNothingToCommit` | `Resolve` | `CommitEmpty` records the message in an empty commit when nothing changed |
| `catchUp` | `AsDefinedOnClaim` | `AfterPush` asks the `GitTarget`'s applier to apply the branch tip right after the commit is pushed, instead of after the applier claim's `catchUpAfter`. The tip may hold later commits, and saves pushed close together can share one apply; the `Applied` condition reports it |

<!-- END GENERATED: settings-index -->

## Sharing connections across namespaces

A platform team usually owns the repository credential, the cluster connections and the endpoint the
Git host calls; application teams decide what to mirror where. The kinds are cut along that line.
What holds a credential (`GitProvider`, `KubeProvider`, `GitReceiver`) sits in its owner's namespace
with its Secret. What a team decides (`GitBranch`, `GitTarget`, `WatchRule`, applier claims) can sit
in the team's namespace, and reaches the shared objects by reference, as far as their grants allow.

![A platform namespace holds the shared GitProvider with its credentials Secret, a KubeProvider and
a GitReceiver; each team namespace has its own WatchRule and GitTarget writing its own branch of that
repository, and a push event is routed by repository and branch to the GitTarget that writes
it](images/config-platform-shared.excalidraw.svg)

### One tenant, end to end

A platform team keeps one repository credential and one cluster connection in its namespace,
`platform`. Namespace `team-a` carries the label `platform.example.com/tenant: "true"`.

1. The platform lets tenants claim `teams/<their namespace>/*` and mirror their own namespace.
2. Team A creates a `GitBranch` for `teams/team-a/live`, and lets `GitTarget`s from its own
   namespace write it.
3. Team A creates a `GitTarget` that references that branch and the platform's `KubeProvider`.
4. The `GitTarget` runs only while **both** grants allow it: the branch's writer grant and the
   provider's source grant.

```yaml
# platform: the credential, and who may claim which branches
apiVersion: configbutler.ai/v1alpha3
kind: GitProvider
metadata: {name: shared-repo, namespace: platform}
spec:
  url: https://git.example.com/platform/config.git
  secretRef: {name: repository-auth}
  allowedGitBranches:
    - consumerNamespaces:
        from: Selector
        selector: {matchLabels: {platform.example.com/tenant: "true"}}
      branchPatterns: ["teams/{consumer.namespace}/*"]
---
# platform: the cluster connection, and who may mirror which namespaces
apiVersion: configbutler.ai/v1alpha3
kind: KubeProvider
metadata: {name: production, namespace: platform}
spec:
  secretRef: {name: production-auth}
  allowedGitTargets:
    - consumerNamespaces:
        from: Selector
        selector: {matchLabels: {platform.example.com/tenant: "true"}}
      sourceNamespaces: ["{consumer.namespace}"]
---
# team-a: the branch it claims, and who may write it
apiVersion: configbutler.ai/v1alpha3
kind: GitBranch
metadata: {name: live, namespace: team-a}
spec:
  gitProviderRef: {name: shared-repo, namespace: platform}
  branch: teams/team-a/live
  allowedGitTargets:
    - consumerNamespaces: {from: Same}
---
# team-a: what to mirror, and where; a WatchRule in team-a selects what
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata: {name: apps, namespace: team-a}
spec:
  gitBranchRef: {name: live}
  kubeProviderRef: {name: production, namespace: platform}
  path: apps
```

| Request | Result | Because |
|---|---|---|
| `team-a`'s `GitBranch` claims `teams/team-a/live` | Allowed | The provider's grant selects `team-a`, and `teams/{consumer.namespace}/*` expands to `teams/team-a/*` |
| `team-a`'s `GitBranch` claims `main` or `teams/team-b/live` | Refused, `BranchNotAllowed` | No grant gives `team-a` either branch |
| `team-a`'s `GitTarget` writes through `team-a/live` | Allowed | `from: Same` on the `GitBranch` |
| `team-b`'s `GitTarget` writes through `team-a/live` | Refused, until team A adds a grant with `consumerNamespaces: {from: Names, names: [team-b]}` | The branch's owner decides who shares it |
| A `WatchRule` in `team-a` watches `team-a` | Allowed | `sourceNamespaces: ["{consumer.namespace}"]` |
| A `WatchRule` in `team-a` watches `kube-system` | Refused, `SourceNamespaceNotAllowed` | The grant's scope is `team-a` only |
| A `GitTarget` in `platform` mirrors through `production` | Refused, `NamespaceNotAuthorized` | No grant admits `platform`; its own namespace is not automatic |

Two expressions name a namespace, and they name different ones:

| Expression | Whose namespace |
|---|---|
| `from: Same` | The object **containing** the grant: for this `GitBranch`, `team-a`; for the providers, `platform` |
| `{consumer.namespace}` in a scope | The **consumer**, the object asking for access: for `team-a`'s `GitBranch`, `team-a` |

`consumerNamespaces: {from: Same}` does not mean "the consumer's own namespace". The block says
which namespaces' consumers the grant admits, and `Same` picks the namespace of the object holding
the grant, as Gateway API's `from: Same` means "the same namespace as the Gateway".

**Platform administrators manage provider grants and authorization labels. Branch owners manage
writer grants on their branches.** That split is why team A can share its branch without asking
the platform.

### The grants

Every grant answers one question: who may use this object, and for what? A grant list lives on the
object that lends something, and is named `allowed` plus the kind that refers to it:

| Grant list | Admits | Scope |
|---|---|---|
| `GitProvider.spec.allowedGitBranches` | `GitBranch`es claiming a branch with its credential | `branchPatterns` (required) |
| `GitBranch.spec.allowedGitTargets` | `GitTarget`s writing a folder into the branch | none |
| `KubeProvider.spec.allowedGitTargets` | `GitTarget`s mirroring with its credential | `sourceNamespaces`, `clusterResources`, or both |
| `KubeProvider.spec.allowedApplierClaims` | `FluxClaim`s and `ArgoCDClaim`s holding an applier with its credential | `applierNamespaces` (required) |

- **An omitted list is the same-namespace default; a written list is the whole policy.** On a
  `GitBranch` and a `KubeProvider` the API server fills an omitted list with one grant for the
  object's own namespace, which you can read back with `kubectl get -o yaml`:

  | You write | The list is |
  |---|---|
  | nothing | `GitBranch`: `[{consumerNamespaces: {from: Same}}]`. `KubeProvider`: `allowedGitTargets: [{consumerNamespaces: {from: Same}, sourceNamespaces: ["{consumer.namespace}"]}]` and `allowedApplierClaims: [{consumerNamespaces: {from: Same}, applierNamespaces: ["{consumer.namespace}"]}]` |
  | a list | exactly that list; the default is not added, so the local namespace is admitted only if the list grants it access |
  | `[]` | nobody, the own namespace included |

  The default grants no cluster-scoped resource, and it reaches only the namespace of the same
  name in the provider's cluster.
- **`allowedGitBranches` has no default.** Which branches a namespace may claim is always written
  as `branchPatterns`, so an omitted or empty list on a `GitProvider` admits no `GitBranch`, its own
  namespace included.
- **The default trusts the object's own namespace.** Whoever may create a `GitTarget` or an applier
  claim beside a `KubeProvider` may use its credential within the default scope. To keep
  administering a provider apart from using it, give the provider a namespace of its own, or write
  `[]` and grant the namespaces you mean.
- **Grants combine by union**, and order never matters.
- **An explicit scope is the whole scope**, never "these plus your own":
  `applierNamespaces: [flux-system]` means `flux-system` only. An object's scope is the union of
  the scopes of every grant that admits it.
- **A scope is a permission, not a selection.** `sourceNamespaces` bounds the namespaces a
  `WatchRule` may name; listing one watches nothing until a rule names it. A request outside the
  union is refused as a whole, never narrowed to fit: a `rules[].sourceNamespace: "*"` item under a
  union without `"*"` is refused, not trimmed to the namespaces listed.
- **Every grant list is `+listType=atomic`**, so one owner applies it whole.
- **References are checked when the referrer reconciles, not at admission.** A refused object is
  accepted by the API server, reports its reason in status, and recovers on its own once a grant
  admits it.

Every grant has two halves: a `consumerNamespaces` block saying which control-cluster namespaces'
consumers it admits, and beside it the scope it gives them. The block is shaped like Gateway API's
`allowedRoutes.namespaces`, with the same `from`:

```yaml
allowedGitTargets:            # on a KubeProvider
  - consumerNamespaces:       # who: control-cluster namespaces
      from: Names
      names: [team-a]
    sourceNamespaces: ["{consumer.namespace}"]   # what: namespaces in this provider's cluster
```

Unlike Gateway API, nothing inside a grant defaults: `consumerNamespaces` is required on every
grant you write, and so is its `from`. A grant without the block, with `consumerNamespaces: null`, or with
`consumerNamespaces: {}` is refused.

| `from` | Admits | Requires |
|---|---|---|
| `Same` | The namespace of the object containing the grant | nothing else |
| `Names` | Exactly these control-cluster namespaces | `names` |
| `Selector` | Control-cluster namespaces whose labels match | `selector` |
| `All` | Every namespace | nothing else |

`names` goes only with `Names`, and `selector` only with `Selector`. "Team B or every tenant" is two
grants. `Same` admits an object's own namespace without writing its name, so a kustomize overlay
that moves the object moves the grant with it; a literal own-namespace name in `names` would not
follow. Admitting everyone takes `from: All`, a word a reviewer sees: `from: Selector` with an empty
`selector: {}` is refused.

| Field | Omitted | `[]` | A value | `"*"` |
|---|---|---|---|---|
| A grant list | No access | No access | Grants, combined by union | (not a value) |
| `consumerNamespaces` | Refused: required | (not a list) | A block with `from` | (not a value) |
| `consumerNamespaces.from` | Refused: required | (not a list) | `Same`, `Names`, `Selector` or `All` | Refused: use `All` |
| `consumerNamespaces.selector` (with `Selector`) | Refused | (not a list) | A label selector on control-cluster Namespaces | `selector: {}` is refused: use `from: All` |
| `consumerNamespaces.names` (with `Names`) | Refused | Refused | Exact control-cluster namespace names; a consumer variable is refused, because `{consumer.namespace}` would admit every consumer (use `from: All`, or `from: Same` for the grant's own namespace) | Refused: use `from: All` |
| `allowedGitBranches[].branchPatterns` | Refused: required | Refused | Branch patterns | Every branch |
| `allowedGitTargets[].sourceNamespaces` | No namespace | Refused | Exactly these source-cluster namespaces, each still named by a `WatchRule` | Every namespace; **required**, in at least one admitting grant, for `rules[].sourceNamespace: "*"`, which is one cluster-wide watch |
| `allowedGitTargets[].clusterResources` | No cluster-scoped resource | Refused | Exactly these, by identity | Every cluster-scoped resource, including CRDs installed later |
| `allowedApplierClaims[].applierNamespaces` | Refused: required | Refused | Exactly these namespaces | Every namespace |

A `KubeProvider` `allowedGitTargets` grant sets `sourceNamespaces`, `clusterResources`, or both;
one that grants neither is refused.

### The consumer variables

The object asking for access is the **consumer**: the `GitBranch` for `allowedGitBranches`, the
`GitTarget` for `allowedGitTargets`, and the applier claim for `allowedApplierClaims`. One grant
serves many consumers, so its scope can name the consumer instead of listing each one:

| Variable | Is | Allowed in | For `team-a`'s `GitBranch` `live` |
|---|---|---|---|
| `{consumer.namespace}` | The consumer's namespace, in the control cluster | every scope and branch pattern | `team-a` |
| `{consumer.name}` | The consumer's name | a branch pattern that also holds `{consumer.namespace}` | `live` |

They are the only variables. Each is replaced with the consumer's value before the scope is
compared, and nothing else about them is special. A grant has no variable for its own namespace:
the grant sits on its object, so that namespace is known when the grant is written, and
`from: Same` admits it.

**Only the namespace bounds anything.** Creating an object in a namespace takes RBAC there, and a
grant's `from` decides which namespaces count. A name is whatever the object's creator types, so a
scope bounded by `{consumer.name}` alone would be no bound: a `GitTarget` named `kube-system` would
reach `kube-system`. That is why `{consumer.name}` is refused in `sourceNamespaces` and
`applierNamespaces`, and allowed in a branch pattern only beside `{consumer.namespace}`, where the
namespace segment does the bounding.

**In `sourceNamespaces` and `applierNamespaces`** an entry is an exact name,
`"{consumer.namespace}"` alone, or `"*"` alone. The variable names a namespace **of the same name in
the provider's cluster**: `sourceNamespaces: ["{consumer.namespace}"]` lets each `GitTarget` mirror the
source namespace that shares its own namespace's name. For a `KubeProvider` that reaches another
cluster, that is a namespace over there that happens to share the name. Like every scope entry it
selects nothing on its own: it permits a `WatchRule` to name that one namespace (an omitted
`sourceNamespace` names it too, being the rule's own namespace), or an applier to live there.

**In a branch pattern** both variables may appear, each as a whole path segment, and the pattern is
matched against the whole branch name:

- **`*` matches any sequence of characters, `/` included, and the empty one.** `feature/*` matches
  `feature/checkout/v2`, and `*` alone is every branch. `main` does not match `main-old`.
- **A variable is a whole path segment:** at the start or after a `/`, and at the end or before a
  `/`. `teams/{consumer.namespace}/*` and `teams/{consumer.namespace}/{consumer.name}` are valid.
  `teams/{consumer.namespace}*` is refused, because for `team-a` it would also match
  `teams/team-ab/live`, and so is `{consumer.namespace}-{consumer.name}`.
- **`{consumer.name}` needs `{consumer.namespace}` in the same pattern.** `env/{consumer.name}` is
  refused: any admitted namespace could claim `env/prod` by naming a `GitBranch` `prod`. Beside the
  namespace the name is a naming convention, not a bound: `teams/{consumer.namespace}/{consumer.name}`
  gives each `GitBranch` the one branch named after it, so a branch says which object claims it, but
  a team that may create `GitBranch`es can still reach every branch under its own prefix.
- **No escaping, `?` or character classes.** Git refuses `*`, `?` and `[` in a branch name, so a
  literal never needs escaping.
- Any other `{...}` is refused when the object is applied, naming it:
  `branch pattern teams/{namespace}/* references unknown variable {namespace}; the variables are
  {consumer.namespace} and {consumer.name}`.

A [placement template](#template-variables) also has a `{namespace}`, and it means something else:
the mirrored resource's namespace, which is `_cluster` for a cluster-scoped resource. A grant
renders nothing, so placement's `|fallback`, sanitizing and empty-segment dropping do not apply.

### Cluster-scoped resources

`clusterResources` bounds what a [`ClusterWatchRule`](#clusterwatchrule) may mirror through a
`GitTarget` the grant admits. It names resource types:

- **A non-core resource is `resource.group`:** `clusterroles.rbac.authorization.k8s.io`,
  `customresourcedefinitions.apiextensions.k8s.io`. **A bare name is the core group only:** `nodes`,
  `namespaces`. A bare `clusterroles` therefore matches nothing.
- **Exact identities or `"*"`,** with no aliases and no partial wildcards. An identity covers every
  served version of its group and resource.
- **It is decided on the rule's text.** A `ClusterWatchRule` item with `"*"` in `apiGroups` or
  `resources` needs `clusterResources: ["*"]`. An item that names its groups and resources is checked
  pair by pair. An item that omits `apiGroups` is checked by the types it would watch: those it
  matches in the source cluster's discovery, at the `apiVersions` it names.
- **One forbidden type refuses the whole `ClusterWatchRule`**, with `ResourcesResolved=False`, reason
  `ClusterResourceNotAllowed`, and a message naming the type. Nothing is filtered silently.
- **Discovery never widens a grant.** Under a narrow grant no wildcard item compiles, and nothing
  outside the grant is watched, so a CRD installed later cannot slip in; the rule is refused when it
  next reconciles. Under `["*"]` it is included: that is what `"*"` grants.

### Refusal messages name the grant

A refusal says what the grants allow and what was asked, so a tenant never needs a broad grant to
find out why something failed:

- `GitBranch`: `namespace "team-a" may claim "teams/team-a/*", "feature/*" through GitProvider
  platform/shared-repo (spec.allowedGitBranches); requested branch "main" is not allowed`
- `GitTarget` on a branch: `GitBranch team-a/live admits team-a, team-b in spec.allowedGitTargets;
  namespace "team-c" is not admitted`
- `WatchRule`: `KubeProvider platform/production lets namespace "team-a" watch "team-a"
  (spec.allowedGitTargets[].sourceNamespaces); spec.rules[0] (...) is not allowed`

The reasons are `NamespaceNotAuthorized` (no grant admits the namespace), `BranchNotAllowed`,
`SourceNamespaceNotAllowed`, `ClusterResourceNotAllowed` and `ApplierNamespaceNotAllowed`.

### When a grant shrinks

A grant shrinks through an edited list, a removed Namespace label, or a deleted object, and every
one of them reaches the objects it admitted through a watch. Once the controller observes it, it
admits no new work under the revoked permission. Work it already held may complete:

- **A `GitBranch` no grant admits any more** gives up its branch the way a deleted one does, but
  keeps its finalizer. Its branch worker closes its intake, pushes or refuses each pending write and
  settles every `CommitRequest`; an event that arrives afterwards is refused. The `GitBranch` reports
  `Stalled=True` with the grant's reason. Nobody holds the branch until a waiting `GitBranch` takes
  it on its own reconcile, which checks its grant first, so a candidate revoked by the same edit
  never holds it. The new holder starts a worker of its own, with none of the old one's work. Writes
  can therefore land after the API server accepted the policy edit: Git cannot take back a push
  already sent.
- **A `GitTarget`, `WatchRule`, `ClusterWatchRule` or applier claim** that loses admission or scope is
  refused on its next reconcile. A write already decided is still committed and pushed.
- **An applier claim its `allowedApplierClaims` refuse** hands its applier back, as one no eligible
  `GitTarget` references does: it stays the holder (`InControl=False`, `Revoking`) while it hands back,
  keeps a `Kustomization` that was suspended before adoption suspended, and gives up after ten
  minutes with a Warning Event if it cannot finish. A regrant during the hand-back lets it finish, and
  the applier claim then adopts the applier anew. A read error never starts a hand-back. Once the
  applier is resumed, Flux or Argo CD may apply Git over live edits not mirrored yet.

Restoring a grant is enough to recover, also after a controller restart: nothing has to be edited on
the objects it admits.

A `GitBranch` is a candidate for its branch only while a grant admits it, and the oldest admitted
candidate holds the branch, by `creationTimestamp` and then UID. Granting a namespace that holds an
old `GitBranch` for a branch someone else holds therefore hands the branch to the old one once it
proves its credential, and the live holder reports `BranchConflict`. **Remove stale `GitBranch`es
before granting.**

### What grants do not replace

- **Every operation runs as the provider's identity, never as the referrer's.** Grants decide who may
  use that identity, and for what; they are not impersonation. A source-namespace grant permits
  mirroring what the provider identity can read in that namespace. An applier-namespace grant
  permits controlling matching appliers there, and the applier's own execution identity bounds what
  it deploys: a Flux `Kustomization`'s `serviceAccountName`, or an Argo CD `AppProject`'s
  destinations.
- **On an `inCluster` provider a scope bypasses live namespace RBAC.** The config plane *is* the
  watched cluster, so the owner of an admitted object acts on the namespaces in its scope through the
  operator's own credential. Grant `sourceNamespaces`, `clusterResources` and `applierNamespaces`
  there on purpose.
- **A namespace grant trusts that namespace's administrators.** Anyone, or any reconciler, allowed
  to create the referring kind there can use the grant. Labels a `Selector` grant matches must be
  controlled by trusted administrators. A `Names` grant also admits a namespace that is deleted and
  recreated with the same name.
- **Branch grants restrict claiming and writing; they do not make branches confidential from each
  other.** A `parentBranch` may name any branch the provider's credential can read, so a permitted
  parent's contents can be copied into the delegated branch. Inside one branch, the folder overlap
  check coordinates writers; it is not a boundary between tenants.

**The limit.** Grants decide who may claim a branch through this provider, and who may write through
this `GitBranch`. They cannot stop a claim on the same repository branch through another provider
whose credential opens a push session. The claim is keyed by the repository and the branch, not the
provider, so a `GitBranch` claiming `main` through a different `GitProvider` for the same URL, with a
copied token, holds it if it is older or first. On Gitea and Forgejo a token that can only read is
enough. Keep tenants from creating providers, as in the next example.

### A platform and its tenants

The model assumes one split, and RBAC enforces it: platform administrators manage providers, their
grants and the namespace labels; branch owners manage writer grants on their branches; tenants
manage the objects that use both. With Flux, objects are applied by a reconciler, so what counts is
what each reconciler's identity may create. The platform's reconciler applies:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: team-a
  labels: {platform.example.com/tenant: "true"}   # the label the grants select on
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: gitops-reverser-tenant}
rules:
  - apiGroups: [configbutler.ai]
    resources: [gitbranches, gittargets, watchrules, fluxclaims, argocdclaims, commitrequests]
    verbs: [get, list, watch, create, update, patch, delete]
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: tenant-reconciler, namespace: team-a}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: tenant-reconciler, namespace: team-a}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: gitops-reverser-tenant}
subjects: [{kind: ServiceAccount, name: tenant-reconciler, namespace: team-a}]
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: {name: team-a, namespace: team-a}
spec:
  serviceAccountName: tenant-reconciler   # team-a's repository is applied as this identity
  sourceRef: {kind: GitRepository, name: team-a}
  path: ./
  interval: 10m
  prune: true
```

It also applies the `GitProvider` and `KubeProvider` above, with their grants, and a `GitBranch` for
`main` in `platform` that the platform keeps for itself (`from: Same` in `allowedGitBranches`). The
Role also needs whatever team A's own workloads require.

| Identity | Manages | Cannot |
|---|---|---|
| The platform's reconciler | `GitProvider`, `KubeProvider` and their grants; Namespaces and their labels; tenant RBAC; platform `GitBranch`es such as `main` | |
| `team-a/tenant-reconciler` | `GitBranch`, `GitTarget`, `WatchRule`, applier claims and `CommitRequest`s in `team-a` | Create or edit a provider; label a Namespace; change RBAC |

The last column is what makes the grants hold: a tenant cannot grant itself a label, and cannot
create the second provider the limit describes. Flux's multi-tenancy lockdown
(`--no-cross-namespace-refs` and `--default-service-account` on kustomize-controller) makes the
tenant identity mandatory, so no tenant `Kustomization` falls back to the controller's own.

### Push events route themselves

The platform team points the Git host's push webhook, or a GitHub App's or an organization's single
webhook, at one [`GitReceiver`](#gitreceiver). A verified push names a repository and a branch, and
wakes the branch worker of every `GitProvider` whose `spec.url` names that repository, whichever
namespace it is in, for that branch only. Nothing references a `GitReceiver` and nothing needs
admitting: a delivery only wakes a worker, which reads the remote with its own credential. A team
that wants an endpoint of its own creates a `GitReceiver` in its own namespace.

Nothing has a default. The operator cannot infer a safe repository, branch, or write credential, nor
which cluster a folder means, so every `GitTarget` names its `GitBranch` and its `KubeProvider`.
Nothing falls back to the provider the chart renders, and that provider admits only the release
namespace's own objects until the installer grants more.

## `GitProvider`

`GitProvider` defines the Git remote, credentials, who may claim which branches, and commit behavior. A
[`GitReceiver`](#gitreceiver) tells it about pushes by matching its `spec.url`; the provider does not
name one.

The important fields are:

- `spec.url`: repository URL
- `spec.secretRef.name`: Secret with Git credentials such as SSH or HTTPS auth
- `spec.knownHostsRef`: optional ConfigMap/Secret with SSH `known_hosts` shared across providers
- `spec.allowedGitBranches`: which namespaces' `GitBranch`es may claim which branches with this
  credential ([the grants](#the-grants)). It has no default: its own namespace needs a grant too
- `spec.commit`: committer identity and signing

How writes are batched and phrased is **not** here: `spec.commit.window` and `spec.commit.message`
belong to the [`GitTarget`](#gittargetspeccommit-how-writes-become-commits) that owns the folder.

Example:

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitProvider
metadata:
  name: example-provider
  namespace: default
spec:
  url: git@github.com:example-org/example-repo.git
  secretRef:
    name: git-creds
  allowedGitBranches:
    - consumerNamespaces: {from: Same}
      branchPatterns: [main]
```

### `GitProvider.spec.secretRef`: the credentials Secret

The referenced Secret holds the Git credentials. The examples use the **Kubernetes-native** keys,
which match the built-in Secret types and the tooling around them (`kubectl create secret generic
--type=…`, Sealed Secrets, External Secrets, SOPS):

| Auth | Keys |
|---|---|
| SSH | `ssh-privatekey` (+ optional `ssh-password` passphrase, `known_hosts`) |
| HTTP basic | `password` (+ optional `username`) |
| HTTP bearer token | `bearerToken` (GitHub fine-grained PAT, GitLab access token; no username) |

**`password` is what selects HTTP basic auth**, and `username` is optional: a Secret with only
`password` authenticates with an empty username, which is what Azure DevOps expects for a Personal
Access Token. ADO ignores the username entirely; measured, any value including none is accepted.
A `username` with no `password` is an error, because that one is a real mistake.

Note that an empty value and an absent key are the same thing to the credential reader, so
`username: ""` behaves exactly like omitting it.

#### Reusing a Flux or Argo CD credentials Secret

The credential reader's design is **inspired by both Flux and Argo CD**: it accepts their Secret key
names alongside the native ones, so you can reuse a Git credentials Secret you already have instead of
re-authoring it. The keys read for each auth method:

| Credential | Native key (recommended) | Flux key (also read) | Argo CD key (also read) |
|---|---|---|---|
| SSH private key | `ssh-privatekey` | `identity` | `sshPrivateKey` |
| SSH key passphrase | `ssh-password` | `password` *(when an SSH key is present)* | *(unsupported by Argo)* |
| SSH host keys | `known_hosts` | `known_hosts` | external ConfigMap → supply via `spec.knownHostsRef` |
| HTTP basic auth | `username` + `password` | `username` + `password` | `username` + `password` |
| HTTP bearer token | `bearerToken` | `bearerToken` | `bearerToken` |

Auth precedence is SSH key → HTTP basic → bearer token. Client certificates (mTLS), custom CA
certificates, and GitHub App credentials are **not supported**.

Credentials are refused with `http://` GitProvider URLs by default. Use HTTPS or SSH for real
repositories. For trusted in-cluster development Git servers that intentionally serve plain HTTP,
start the manager with `--insecure-allow-git-http` or set
`controllerManager.insecureAllowGitHTTP=true` in the Helm chart.

> **A reused Secret needs write access.** Flux and Argo CD only *clone*, so their Git credentials are
> often read-only (a read-only deploy key, a read-scoped token). GitOps Reverser **pushes** commits,
> so a reused Secret's key or token must have **write** access on the repository; otherwise the
> commits will fail to push.

SSH host keys are resolved in priority order: the credentials Secret's own `known_hosts`, then
`spec.knownHostsRef` (a namespace-local ConfigMap or Secret keyed `known_hosts`, or `ssh_known_hosts`
for data copied out of Argo's `argocd-ssh-known-hosts-cm`), then an install-level default known-hosts
ConfigMap in the controller's namespace (`--default-known-hosts-configmap`). If none yields a valid
host key, SSH fails closed. Host-key rotation is an admin-owned declarative update; verify
fingerprints out of band. The controller flag `--insecure-allow-missing-known-hosts` relaxes this for
throwaway/dev clusters only: it permits SSH when **no** source provided any `known_hosts`; a
`known_hosts` that is present but unparseable is always a hard error.

### `GitProvider.spec.commit`

`spec.commit` configures the identity this connection commits under:

- `committer`: the operator identity written as the Git committer
- `signing`: the SSH signing key configuration

If `spec.commit` is omitted, gitops-reverser uses its built-in defaults.

`spec.commit.message` is not here. A commit's wording describes the folder being written, so it is
[`GitTarget.spec.commit.message`](commit-messages.md); setting it on a `GitProvider` is
rejected.

#### Author vs committer

These are different on purpose:

- **Author**: who made the cluster change
- **Committer**: who wrote the Git commit object

For mirrored-resource commits, the author comes from the configured committer identity unless
`attribution.enabled=true` and a matching kube-apiserver audit event names the Kubernetes user or
service account. Snapshot/reconcile commits are operator-authored.

When attribution IS enabled and no matching audit fact arrives, the commit is authored
`unknown (attribution unresolved) <attribution-unresolved@gitops-reverser.invalid>` instead of the
committer. That distinction is the point: a committer-authored commit means attribution was never
attempted, while the sentinel means it was attempted and did not resolve, which is worth investigating.
Such commits also count under `author_kind="unresolved"` in `commits_total`.

That distinction is useful in practice:

- `git log --author=alice` answers "what did Alice change?"
- `git log --committer="GitOps Reverser"` answers "what did the operator write?"

When signing is enabled, Git hosting platforms usually verify the **committer** identity, not the
Kubernetes author.

#### Committer identity

Use `spec.commit.committer` to control the bot identity written as the Git committer:

```yaml
spec:
  commit:
    committer:
      name: GitOps Reverser
      email: 12345678+gitops-reverser-bot@users.noreply.github.com
```

Defaults:

- `name`: `GitOps Reverser`
- `email`: `noreply@configbutler.ai`

If signing is enabled, `spec.commit.committer.email` should be an email that the Git hosting
platform recognizes for the account that owns the signing key.

#### Commit signing

GitOps Reverser signs commits from `spec.commit.signing`.

The signing `Secret` uses these data keys:

- `signing.key`: PEM-encoded SSH private key
- `passphrase`: optional passphrase for encrypted private keys
- `signing.pub`: optional convenience copy of the public key

The operator publishes the effective public key in `.status.signingPublicKey`.

Let the operator generate the signing key:

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitProvider
metadata:
  name: example-provider
  namespace: default
spec:
  url: git@github.com:example-org/example-repo.git
  allowedGitBranches:
    - consumerNamespaces: {from: Same}
      branchPatterns: [main]
  secretRef:
    name: git-creds
  commit:
    committer:
      name: GitOps Reverser
      email: 12345678+gitops-reverser-bot@users.noreply.github.com
    signing:
      secretRef:
        name: gitops-reverser-signing-key
      generateWhenMissing: true
```

Bring your own signing key:

```bash
ssh-keygen -t ed25519 -f /tmp/gitops-reverser-signing -N ""

kubectl create secret generic gitops-reverser-signing-key \
  -n default \
  --from-file=signing.key=/tmp/gitops-reverser-signing \
  --from-file=signing.pub=/tmp/gitops-reverser-signing.pub
```

```yaml
spec:
  commit:
    committer:
      name: GitOps Reverser
      email: 12345678+gitops-reverser-bot@users.noreply.github.com
    signing:
      secretRef:
        name: gitops-reverser-signing-key
```

If you start from the Helm chart `quickstart`, edit the generated `GitProvider` directly when you
want custom `spec.commit` behavior because the starter values do not currently expose those fields.

For the platform-facing behavior behind "valid signature" versus "verified badge", see
[commit-signing.md](commit-signing.md).

## `GitReceiver`

`GitReceiver` is the endpoint a Git host notifies on a push, so a push by somebody else is noticed in
seconds instead of at the next refresh. No `GitProvider` names it: a delivery wakes every
`GitProvider`, in any namespace, whose `spec.url` names the repository the delivery reports. So a
GitHub App or an organization webhook, which has one endpoint for many repositories, needs only one,
and a team that wants its own endpoint creates a `GitReceiver` in its own namespace. The names
follow Flux's `Receiver`, so a Flux user already knows them; the kind is not called `Receiver`
because Flux's sits in the same clusters.

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitReceiver
metadata:
  name: github-app
  namespace: platform
spec:
  type: github              # github | gitlab | gitea | generic-hmac
  secretRef:
    name: github-app-webhook-token   # in this namespace; key: token
status:
  webhookPath: /git-push/3f9a…c41e   # 64 hex characters
```

Point the host's push webhook at the receiver listener plus `status.webhookPath`, with the same
token as the webhook secret. The path is an HMAC of the receiver's namespace and name, keyed by the
token, so it names neither and only whoever holds the token can find it. Rotating the token moves
the path: the old one stops serving at once, and the new one is published at the next reconcile.
Secrets are not watched, so while the Secret or its `token` key is missing, `status.webhookPath` is
empty and the receiver looks again every 30 seconds; once it has a token, every 5 minutes. Each type
verifies the request the way its host signs it:

| `type` | Verifies | Repository read from |
| --- | --- | --- |
| `github` | `X-Hub-Signature-256`, an HMAC of the body | `repository.clone_url`, `ssh_url`, `git_url`, `html_url` |
| `gitlab` | `X-Gitlab-Token`, compared with the token | `project.git_http_url`, `git_ssh_url`, `web_url` |
| `gitea` | `X-Gitea-Signature`, an HMAC of the body | `repository.clone_url`, `ssh_url`, `html_url` |
| `generic-hmac` | `X-Signature: sha256=<hex>`, an HMAC of the body | `repository` in `{"repository": "...", "branch": "main", "after": "<commit>"}` |

`generic-hmac` is for a relay of your own. Its header is the one Flux's `generic-hmac` `Receiver`
checks, and its body must name the repository and the branch.

**How a delivery is routed.** The signature is checked before anything in the body is read, so an
unsigned request cannot probe which repositories or branches are tracked. A verified push names a
branch and a repository, which a host spells several ways. Every spelling is compared with the
`spec.url` of every `GitProvider`, in any namespace, the way
[a branch claim](#who-writes-a-repository-branch) compares repositories: scheme, user, a default
port, a trailing `.git` and letter case are ignored, while the host and the path are not. For each
provider that matches, the branch worker writing that branch is woken, once. A repository that
reaches the operator under another host name, such as an SSH alias, does not match.

**Whether deliveries arrive** is on the `GitBranch`: `status.lastPushNoticeAt` is when a
`GitReceiver` last delivered a push to its branch. It records receipt, so it is set even when the
look the delivery caused could not read the remote; `status.remote` says what that look found. The
first delivery is published at once and later ones at most once a minute, and the field is cleared
when the `GitProvider` is recreated for another repository. Absent after a push has three causes,
which the receiver's `gitopsreverser_receiver_deliveries_total` outcome tells apart:

- The host does not reach the receiver: no delivery is counted, or it is counted as `rejected`.
- The delivery names the repository by a URL other than the provider's `spec.url`, such as an SSH
  alias: `unknown_repository`.
- The repository matches, but no branch worker writes that branch, because no `GitTarget`
  references this `GitBranch` or it does not hold the branch: `unknown_branch`.

Without a receiver, the provider still writes, and a foreign push is still found, by the next
rejected push or by the [periodic refresh](#keeping-an-idle-target-fresh---git-refresh-interval):
the receiver makes it seconds instead of minutes.

A `GitReceiver` reports on `Ready` whether anything serves its path:

| `Ready` | Reason | Meaning |
| --- | --- | --- |
| `True` | `Succeeded` | The token is readable and the listener is on |
| `False` | `SecretNotFound` | `spec.secretRef` names no Secret in the receiver's namespace |
| `False` | `SecretMalformed` | The Secret has no `token` key |
| `False` | `ListenerDisabled` | The operator serves no receiver (`--receiver-bind-address=0`). The path is still published |

**What the host is told.** Every request that reaches no verified receiver gets the same `404`,
whether the path is unknown, from before a token rotation, or its signature is wrong, so the
endpoint reveals nothing about which namespaces or receivers exist. For a published path, the
operator's log says which it was. A verified delivery answers `202 Accepted` with no body, even when
it names a repository or a branch nothing writes, because that is not the host's fault and an error
teaches a host to disable the hook. The answer does not say what was woken: any namespace can hold a
`GitReceiver`, and a count would tell its token holder whether another namespace tracks a
repository. Pushes to
other branches, tag pushes and pings are accepted and ignored. Only a verified body that cannot be
read, or one that names no repository, answers `400`.

The listener handles at most 8 deliveries at once and answers `429` above that, and a body over
2 MiB is refused with `413` before its signature is checked. A push event is a few kilobytes, and a
delivery refused for either reason is still found by the periodic refresh.

A delivery is a hint, never a fact. It makes each woken branch worker read the branch's ref
advertisement (one request) with its own provider's credential, and what the remote says decides
whether the branch gained a foreign commit. The repository the payload names is only compared, never
fetched, so a delivery cannot choose a URL, a credential or a commit. A worker holds at most one
pending look: deliveries that arrive while one waits are folded into it, so a run of them costs one
ref advertisement at a time. That bounds the queue, not the rate: a steady stream of deliveries,
from any namespace's `GitReceiver`, can keep a matching worker reading its remote continuously, an
availability cost that is accepted. A late delivery for one of
GitOps Reverser's own pushes, a duplicate, or a rollback onto one of its older commits are all
judged by the remote's answer, not by the commit the delivery names. Nothing fetches or plans until
there is something to apply, and a delivery never triggers a cluster snapshot.

The receiver needs its own listener, because the Git host cannot present the client certificate
the audit listener requires. See [push receiver settings](#push-receiver-settings).

## `KubeProvider`

`KubeProvider` names the Kubernetes cluster a `GitTarget` mirrors **from**, and the cluster an apply
applier claim reaches its applier in. It is the read-side peer of `GitProvider`: a target has one source
cluster and one Git destination. It is namespaced, and it sets exactly one credential:

- `spec.secretRef`: a Secret in the provider's own namespace holding a kubeconfig for the source
  cluster. `key` is optional; empty reads `value`, then `value.yaml` (Flux's order).
- `spec.inCluster: {}`: the operator's own identity in the cluster it runs in, honored only where
  the installation allows it. See [the operator's own identity](#the-operators-own-identity-specincluster).

For a remote source cluster, create the provider beside its kubeconfig Secret:

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: KubeProvider
metadata:
  name: prod-eu-1
  namespace: platform-connections
spec:
  secretRef:
    name: prod-eu-1-kubeconfig
  allowedGitTargets:
    - consumerNamespaces:
        from: Names
        names: [team-a]
      sourceNamespaces: ["{consumer.namespace}"]
    - consumerNamespaces:
        from: Selector
        selector:
          matchLabels:
            gitops.configbutler.ai/source-access: "true"
      sourceNamespaces: ["{consumer.namespace}"]
  allowedApplierClaims:
    - consumerNamespaces:
        from: Names
        names: [team-a]
      applierNamespaces: [flux-system]
```

The Secret is read in the provider's namespace, so a team's connection keeps its credential with the
team, and the credential never has to live on the source cluster. The operator checks the kubeconfig
before it uses it, without dialing the cluster, and reports the verdict as `Validated`:

| Reason | What the kubeconfig does |
|---|---|
| `KubeConfigSecretNotFound`, `KubeConfigKeyNotFound` | nothing yet: the Secret, or the key in it, does not exist |
| `KubeConfigInvalid` | does not parse as a kubeconfig |
| `KubeConfigExecNotAllowed` | runs an exec auth plugin, a binary in the operator's Pod. Allowed with `--insecure-kubeconfig-exec` |
| `KubeConfigInsecureTLSNotAllowed` | sets `insecure-skip-tls-verify`. Allowed with `--insecure-kubeconfig-tls` |
| `KubeConfigFileReferenceNotAllowed` | names a token, certificate or key by file path, which the operator would read from its own Pod. Always refused: embed the `*-data` form instead |

A `KubeProvider` is used only through its [grants](#the-grants). Omitted, each list is one grant
for the provider's own namespace, reaching the namespace of the same name; `[]` admits nobody:

- **`spec.allowedGitTargets`** admits `GitTarget`s to mirror through it. The union of the admitting
  grants' `sourceNamespaces` bounds which namespaces the `WatchRule`s feeding a target may name, and
  the union of their `clusterResources` what a `ClusterWatchRule` may mirror through it.
- **`spec.allowedApplierClaims`** admits `FluxClaim`s and `ArgoCDClaim`s to hold an applier through
  it, and the union of the admitting grants' `applierNamespaces` bounds where that applier may
  live. A namespace admitted to mirror is not admitted to apply.

Grants are evaluated against namespaces in the **control cluster**, where the referring objects
live. In this example, a `GitTarget` in `team-a`, or in a control-cluster namespace with the shown
label, may mirror its own namespace through `prod-eu-1`, and an applier claim in `team-a` may hold a
`Kustomization` in `flux-system`. Nothing in `platform-connections` may use it: no grant admits it.
A selector is only as strong as the right to set the labels it matches: anyone who can label a
namespace can admit it, so select on labels only the platform can set, or use `Names`. A referrer
the provider does not admit is accepted by the API server and reports `NamespaceNotAuthorized`, and
it recovers on its own once a grant admits it.

Which namespaces are read *from the source cluster* is bounded by the source connection's Kubernetes
RBAC: the credential's own RBAC is the hard maximum, and there is no second allow-list beside it.

### The operator's own identity (`spec.inCluster`)

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: KubeProvider
metadata:
  name: local
  namespace: gitops-reverser
spec:
  inCluster: {}
```

`inCluster` is honored only for a `KubeProvider` listed by exact `namespace/name` in
`--in-cluster-kube-providers`, a comma-separated list with no wildcards, empty by default. With the
chart, add entries to `controllerManager.inClusterKubeProviders`; the provider the chart renders is
listed on its own when it uses `inCluster`. A provider that sets `inCluster` and is not listed
reports `Validated=False` and `Stalled=True` with reason `InClusterNotAuthorized`, and nothing uses
it: a `GitTarget` naming it reports `KubeProviderReady=False`, and the watch side refuses to connect
through it as well.

The list exists because the operator's own identity reads far more than any one team should, and a
namespaced kind can be created by anyone with write access to a namespace. Without the list, writing
`inCluster: {}` would be enough to borrow that identity. The list puts the decision with whoever
installs the operator. Entries are exact names, because whoever can create a `KubeProvider` with a
listed name wields the identity through it. No path leads from an omitted, malformed, or refused
`secretRef` to the operator's identity: that identity is a credential of its own that a provider has
to ask for by name.

`spec.client` is refused beside `inCluster`: the operator's own client is shared by everything that
uses it, so it is not throttled per provider.

### Grants on an `inCluster` provider

A grant on a provider that uses the operator's own identity is a much sharper decision than on a
remote one: there the config plane *is* the watched cluster, so a scope deliberately bypasses live
namespace RBAC and lets the owner of an admitted object act on the namespaces in its scope through
the operator's own credential. A grant is a delegation, not owner consent: it does not prove that a
namespace owns the objects it reaches, or the repository its `GitTarget` writes. That is legitimate
to grant on purpose, which is why the chart's `local` provider grants only its default: objects in
the release namespace, reaching the release namespace. Set `kubeProvider.allowedGitTargets: []` and
`kubeProvider.allowedApplierClaims: []` to admit nobody.

`spec.secretRef` and `spec.inCluster` are immutable, switching from one to the other included, and
so is `GitTarget.spec.kubeProviderRef`: changing either would silently make an existing
materialization mean a different source cluster. To point a provider at another cluster, delete it
and create a new one. Rotate credential *contents* in the referenced Secret instead. `client.qps`
and `client.burst` optionally tune a `secretRef` provider's client; the `KubeProvider` conditions
validate its configuration, while the consuming `GitTarget` reports the live source reachability and
stream state.

### Audit route and `AuditFactsReceived`

`spec.attribution.auditRoute` is the route this cluster's attribution facts arrive on: the
`<route>` segment the apiserver's audit webhook URL ends in, `/audit-webhook/<route>`. It defaults
to `<namespace>.<name>`, so the chart's provider listens on `/audit-webhook/gitops-reverser.local`.
The route is what partitions the facts: two providers carrying the same route read one cluster's
facts, and two carrying different routes can never cross-credit an author.

An apiserver takes one audit webhook backend and therefore posts under one route. A second
`KubeProvider` naming the same cluster, in the same namespace, must be pointed at that route:

```yaml
spec:
  attribution:
    auditRoute: platform-connections.prod-eu-1
```

**A route is held by one namespace.** Providers in the holding namespace may share it, as above;
a provider in another namespace that carries it reports `Stalled=True` with reason
`AuditRouteConflict` and is not used, and the GitTargets and applier claims naming it are refused with the
same reason. Its `Validated` stays what its credential earned. The namespace a route starts with,
the segment before its first dot, holds it when a provider there carries it, so a default route is
always its own provider's. Otherwise the namespace of the oldest provider carrying it holds it
(by `creationTimestamp`, then UID). This is a status condition the reconcile sets, not an admission
refusal, and it clears on its own once the holder lets the route go. Without it, a provider in one
namespace could read the attribution facts of a cluster another namespace connected.

The `AuditFactsReceived` condition reports whether that route has ever delivered, with a default
`FACTSRECEIVED` printer column, and `-o wide` adds the resolved `ROUTE`:

```console
$ kubectl get kubeprovider -A
NAMESPACE              NAME        READY   REASON      FACTSRECEIVED   AGE
gitops-reverser        local       True    Succeeded   True            31m
platform-connections   prod-eu-1   True    Succeeded   Unknown         4m
```

- `True` / `Received`: a fact has arrived, and the message carries when the first one did.
- `Unknown`: none ever has, so every commit mirrored through this provider is authored
  `unknown (attribution unresolved)`. Three silences look identical on the object and need three
  different fixes, so the **reason** says which one it is:

| Reason | What was observed | Where to look |
|---|---|---|
| `TransportUnavailable` | the last append to the fact transport failed | the transport itself, named in the message: with `--author-attribution-transport=redis` (the default) the Redis/Valkey at `--redis-addr`, and with `memory` the operator's logs. No route can be judged while this holds: every route looks silent for the same reason |
| `NoAuditDelivery` | no audit request has ever reached this operator, on any route | the API server's audit webhook backend and its connectivity to the operator. This provider's route is not at fault |
| `RouteUnused` | audit is arriving and no fact has ever been published for this route | this provider's route (`status.auditRoute`) first, then the audit policy: a policy whose level or verbs leave nothing attributable produces no fact even when the route is correct |

The status stays `Unknown` for all three. A failing transport is a fault in the pipeline, not in
this provider, and turning it into a per-object `False` would flip every not-yet-latched
`KubeProvider` at once: one outage, many verdicts. The reason carries the diagnosis instead.

It is a **one-way latch** for a given route: once `True` it stays `True`, across controller restarts
and regardless of how long the route stays quiet afterward. Silence *after* proof is a quiet
cluster; silence *before* it is inconclusive rather than a verdict, which is why the status is
`Unknown` and the reason above says which of the three cases it is. The latch separates the two
without a timer, so there is no `False` state and no grace window.

The latch is keyed to the route it was earned on, recorded in `status.auditRoute`. Because
`spec.attribution.auditRoute` is mutable, repointing a provider at a different route starts the
condition over: proof that one route delivered says nothing about another.

The condition is **not part of `Ready`**: a provider reading a route nobody posts to mirrors
perfectly and loses only the commit author. It is absent entirely when the operator runs with
`--author-attribution=false`.

### Creating and managing a `KubeProvider`

The operator **never creates a `KubeProvider`**, and never re-creates one you delete. A `GitTarget`
whose provider does not exist reports `Validated=False` with reason `KubeProviderNotFound`, and
recovers on its own once the provider appears, so manifests can be applied in any order. That is
deliberate: a source cluster is a connection with credentials and an authorization policy, so it is
yours to declare, review, and roll back like any other resource under GitOps.

There are two supported ways to get one, and both are fully declarative:

- **Commit it yourself.** The objects above are ordinary YAML. Put them in the repository that
  manages this install. This is the recommended path once you are past a first trial.
  [`config/kubeprovider-local.yaml`](../config/kubeprovider-local.yaml) is the plain-manifest form of
  the provider the chart renders.
- **Let the chart render it.** With `kubeProvider.create` (on by default), the chart creates and owns
  a `KubeProvider` named `kubeProvider.name` (`local`) in the release namespace, using `inCluster`
  unless `kubeProvider.secretRef.name` is set, with the grants in `kubeProvider.allowedGitTargets` and
  `kubeProvider.allowedApplierClaims`, empty by default. See
  [charts/gitops-reverser/README.md](../charts/gitops-reverser/README.md). Turn
  `kubeProvider.create` off to manage the object yourself. Helm then deletes the provider it created
  on the next upgrade, so ownership never silently splits between Helm and you. Because a missing
  provider holds its targets unready, plan that switch together with committing your own object.

The chart value is a rendering convenience, not runtime behavior: with it off, nothing in the
operator brings the object back. The chart's optional `quickstart` starter resources mirror through
that provider, so they require `kubeProvider.create`.

Every `GitTarget` names its provider. Omit `namespace` for one in the `GitTarget`'s own namespace:

```yaml
spec:
  kubeProviderRef:
    name: prod-eu-1
    namespace: platform-connections
```

The provider's namespace and name are the source-cluster identity used for watches and in the
`source_cluster` metric label (`platform-connections/prod-eu-1`). A provider deleted and recreated
under the same name is a new connection. Changing a target's source cluster changes what its folder
means, so `kubeProviderRef` is immutable.

## `GitBranch`

`GitBranch` is one branch of the repository a `GitProvider` names. It holds that branch for every
`GitTarget` that references it: one branch worker, one clone, one queue, and the `GitProvider`'s
credential, commit identity and signing key.

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitBranch
metadata:
  name: example-branch
  namespace: default
spec:
  gitProviderRef:
    name: example-provider   # add namespace: for a GitProvider in another namespace
  branch: main               # a grant on the provider must allow it
  parentBranch: develop      # optional; only used to create a missing branch
  allowedGitTargets:         # who may write through it, this namespace included
    - consumerNamespaces: {from: Same}
```

- `spec.gitProviderRef` names the `GitProvider` whose credential writes the branch, in this
  namespace or, with `namespace`, another. The `GitBranch` claims only while a grant in the provider's
  `spec.allowedGitBranches` admits its namespace and a pattern matches `spec.branch`
  (`NamespaceNotAuthorized` or `BranchNotAllowed` otherwise, both `Stalled=True`). That check comes
  before any push session, so a `GitBranch` no grant admits never uses the credential.
- `spec.allowedGitTargets` says which namespaces' `GitTarget`s may write through it. Claiming a branch
  includes the right to share it: sharing `teams/team-a/live` with `team-b` is a `from: Names` grant
  here, and the provider does not change. To keep control of the writers of a branch such as `main`,
  the platform owns its `GitBranch`.
- `spec.parentBranch` is what the branch is created from while the remote does not have it. See
  [starting the write branch from another branch](#starting-the-write-branch-from-another-branch-specparentbranch).
- `spec.gitProviderRef`, `spec.branch` and `spec.parentBranch` are immutable: to write another branch,
  or through another `GitProvider`, create a new `GitBranch` and delete the old one. The reference is
  compared as written, so spelling out the `GitBranch`'s own namespace where it was omitted is a
  change too. `spec.allowedGitTargets` is mutable.

### Who writes a repository branch

Only one `GitBranch` writes a repository branch. The key is the provider's repository URL,
normalized (so `https://github.com/acme/platform.git` and `git@github.com:acme/platform` are one
repository), plus the branch.

1. A `GitBranch` becomes a **candidate** once a grant admits it and its `GitProvider`'s credential
   opens a push (receive-pack) session on the remote. Nothing is pushed: the session is opened and closed. A
   `GitBranch` whose credential cannot open one reports `Ready=False` with reason
   `PushSessionFailed` and claims nothing, so creating it first blocks nobody.
2. Among the candidates for one repository branch, the **oldest** (by `creationTimestamp`) holds it
   and gets the branch worker. Every other one reports `Stalled=True` with reason `BranchConflict`
   and a message naming the holder. Reference the holder from your `GitTarget`s, or delete it first.
3. The holder keeps the branch through a later credential or network failure. It reports
   `Ready=False` (`PushSessionFailed`) and keeps its pending writes, and the branch does not pass to
   another `GitBranch` while it is down. A holder no grant admits any more gives the branch up; see
   [when a grant shrinks](#when-a-grant-shrinks).
4. The claim lives in the operator's memory. After a restart it is decided again by 1 and 2.

What the push session proves depends on the Git host:

| Host | What opening a push session takes |
|---|---|
| GitHub, GitLab | Push permission on the repository |
| Gitea, Forgejo | A valid credential that can read the repository (Gitea's AGit flow accepts pushes to `refs/for/*` from readers; branch writes are checked when they arrive) |
| A host that offers push sessions anonymously | Nothing: the proof is only that the repository is reachable |

Nothing refuses a second `GitBranch` at admission: references are checked in status, so a folder of
manifests can be applied in any order.

### `GitBranch` status

```yaml
status:
  repository: github.com/acme/platform   # normalized: half of the key only one GitBranch holds
  remote:
    commit: 4f1c...
    lastVerifiedAt: "2026-10-21T09:12:00Z"
    verifiedBy: Push
  gitTargetCount: 2
```

`status.remote` is where the branch is on the remote; see
[where the branch is](#where-the-branch-is-and-when-that-was-last-proved-statusremote).
`status.gitTargetCount` is how many `GitTarget`s reference this `GitBranch`, from any namespace. More
than one means they share its branch worker; each still reports its own folder's health on its own
conditions.

`kubectl get gitbranch` shows the provider and its namespace, the branch, the commit the branch is
at, and `Ready`;
`-o wide` adds the reason, the repository, the target count and when the remote was last verified.

| Reason | What it means |
|---|---|
| `GitProviderNotFound` | The `GitProvider` `spec.gitProviderRef` names does not exist (`Stalled=True`) |
| `GitProviderNotReady` | The `GitProvider` exists and is not `Ready` |
| `NamespaceNotAuthorized` | No grant in the provider's `spec.allowedGitBranches` admits this namespace (`Stalled=True`) |
| `BranchNotAllowed` | The grants that admit this namespace give no pattern matching `spec.branch` (`Stalled=True`) |
| `PushSessionFailed` | The credential cannot open a push session; a holder keeps the branch meanwhile |
| `BranchConflict` | Another `GitBranch` holds this repository branch (`Stalled=True`) |
| `ParentBranchNotFound` | Neither the branch nor its parent is on the remote (`Stalled=True`) |
| `RecoveringParentBranch` | The parent is back and the worker is publishing what it held |

### Deleting a `GitBranch`

A `GitBranch` carries a finalizer. Deleting it stops new intake for every `GitTarget` on the
branch, pushes or refuses each pending write, gives each attached `CommitRequest` its outcome, and
only then removes the finalizer and lets another `GitBranch` hold the branch. **Nothing deletes the
branch on the remote.**

Deleting a `GitBranch` that `GitTarget`s still reference is allowed; they report `Validated=False`
with reason `GitBranchNotFound` afterwards. A new `GitBranch` with the same name is a new object and
does not pick up the old one's pending work.

## `GitTarget`

`GitTarget` decides where inside the repository resources are written.

The important fields are:

- `spec.gitBranchRef`: which `GitBranch` this target writes, and so which repository branch and
  credential
- `spec.kubeProviderRef`: which `KubeProvider` supplies resources; required
- `spec.path`: required relative path inside the repository; use `.` only when you deliberately
  want the repository root
- `spec.encryption`: how `Secret` resources should be encrypted before commit
- `spec.placement`: optional policy for where **new** resources are written (see
  [Where new resources are written](#where-new-resources-are-written-specplacement)); omit it and a new
  resource takes the folder's one kustomization root, or the built-in path
- `spec.placement.useKustomize`: whether the operator maintains a `kustomization.yaml` for this
  folder, creating one when the folder has none (see
  [Keeping the folder a kustomize folder](#keeping-the-folder-a-kustomize-folder-specplacementusekustomize))
- `spec.serializeNamespace`: whether written documents carry their own `metadata.namespace` (see
  [Whether documents carry their namespace](#whether-documents-carry-their-namespace-specserializenamespace));
  omit it and each document's namespace is inferred from the folder
- `spec.prune`: which deletion paths may remove documents from this target's folder (see
  [Deletion policy](#deletion-policy-specprunemode)); omit it for the safe default
- `spec.applierClaimRef`: the `FluxClaim` or `ArgoCDClaim` holding the Flux `Kustomization` or
  Argo CD `Application` that applies this folder, so GitOps Reverser decides when it applies (see
  [Who applies this folder](#who-applies-this-folder-specapplierclaimref)); omit it when nothing
  applies the folder back
- `spec.onRefusal`: whether a refused edit asks the applier to reapply Git (see
  [Reverting a refused edit](#reverting-a-refused-edit-speconrefusal))

Example:

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitBranch
metadata:
  name: example-branch
  namespace: default
spec:
  gitProviderRef:
    name: example-provider
  branch: main
---
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: example-target
  namespace: default
spec:
  gitBranchRef:
    name: example-branch
  # The chart's provider. {name: prod-eu-1} names one in this GitTarget's own namespace.
  kubeProviderRef:
    name: local
    namespace: gitops-reverser
  path: live-cluster
```

`spec.path` is required so a target never writes to the repository root by accident. Use a path
such as `live-cluster` for the first install. To deliberately target the repository root, set
`path: "."`. Do not use a leading slash, and do not add a trailing slash.

The target path is authoritative for snapshot reconciliation. A root target can create, update, and
delete managed manifest files at the repository root, so use `.` only for a repository layout that is
dedicated to this target.

If you enable `spec.encryption`, that applies to `Secret` resource writes for this target. For SOPS
and age details, see [sops-age-guide.md](sops-age-guide.md).

`spec.gitBranchRef` references a `GitBranch` by `name`, in the `GitTarget`'s own namespace, and is
required and immutable. Set `namespace` to write through a `GitBranch` in another namespace. Either
way the `GitBranch`'s `spec.allowedGitTargets` must admit this `GitTarget`'s namespace, its own
included, or the `GitTarget` is refused with `Validated=False` (`NamespaceNotAuthorized`). A
`GitBranch` that does not exist is `Validated=False`
(`GitBranchNotFound`), and one that does not hold its branch, or is not `Ready`, keeps the target at
`Ready=False` with reason `GitBranchNotReady` until it does. Either clears on its own once the
`GitBranch` appears or proves itself, so manifests can be applied in any order.

`spec.gitProviderRef`, `spec.branch` and `spec.parentBranch` belong to the `GitBranch`: setting one
on a `GitTarget` is refused at admission with a message naming `spec.gitBranchRef`.

Two `GitTarget`s on one `GitBranch` must not write the same or nested paths: the later-created one
reports `Validated=False` with reason `TargetConflict`. Only an admitted `GitTarget` holds a path this
way. One that its `GitBranch` or its `KubeProvider` does not admit writes nothing and holds no path,
so creating it first cannot block a folder on someone else's branch.

`spec.kubeProviderRef` references a `KubeProvider` by `name`, in the `GitTarget`'s own namespace
unless `namespace` names another, and is required and immutable. The provider's
`spec.allowedGitTargets` must admit this `GitTarget`'s namespace, its own included, or the
`GitTarget` is refused with `Validated=False` (`NamespaceNotAuthorized`). A provider that does not exist is `Validated=False`
(`KubeProviderNotFound`), one whose audit route another namespace holds is refused with
`AuditRouteConflict`, and one that is not `Ready` keeps the target at `KubeProviderReady=False`
(`KubeProviderNotReady`). See [`KubeProvider`](#kubeprovider).

The most useful status fields are:

- `Ready`: true when the target is valid, the Git path is accepted, and watched streams are running.
- `Reconciling`: true while initial replay, a recheck, or another coarse progress step is in flight.
- `Stalled`: true when the target is blocked until a human fixes configuration, RBAC, or Git path content.
- While the branch cannot publish to Git, `Ready=False` with reason `Progressing` says since when,
  what failed, and whether intake of new changes is paused; `Reconciling=True` and `Stalled=False`,
  because the retry needs nobody. Every target on the branch says the same until a push lands, and
  its streams report `BranchIntakePaused` while they wait.
- `Validated` and `EncryptionConfigured`: control-plane details.
- `StreamsRunning`: true when the source watches are past initial replay and routing live events.
- `GitPathAccepted`: true when the target Git path is safe to materialize.
- `status.streams`: bounded counts for tracked, running, replaying, and blocked streams.
- `status.retention`: how many documents `spec.prune.mode` is keeping, and under which mode.
- `LayoutResolved`: what the last scan resolved about the folder's shape, with
  `status.placement` carrying the detail. See below.
- `ApplierClaimReady`: true when the named applier claim is `Ready` and its applier applies this folder's
  repository and branch. Present only when `spec.applierClaimRef` is set. The applier claim's own
  `status.operation` says what was last asked of the applier, and why.

Use conditions for automation.

### Why a `GitTarget` is not committing

Start with the target, then check the rules that feed it:

```bash
kubectl get gittarget -A
kubectl describe gittarget -n <namespace> <name>
kubectl get watchrule,clusterwatchrule -A
```

A red `GitTarget` means the target itself cannot validate, run, or publish. `Ready=False` gives the
summary reason, while the domain conditions name the axis to fix:

| Condition | What to check |
|---|---|
| `Validated=False` | The referenced `GitBranch`, path conflicts, `KubeProvider`, and namespace authorization. |
| `EncryptionConfigured=False` | The age/SOPS Secret and whether key generation is allowed. |
| `GitBranchReady=False` | The `GitBranch`: whether it holds its branch, and its `GitProvider`'s URL, credential Secret, host keys, and push permissions. |
| `KubeProviderReady=False` or `SourceClusterReachable=False` | Source-cluster credentials and access. |
| `GitPathAccepted=False` | Repository files under `spec.path`; the message includes the issue kind, path, and actor when known. |
| `RenderMatchesLive=False` | Fields that no longer match the rendered source, often transformer-owned values. |
| `ApplierClaimReady=False` | The applier claim named in `spec.applierClaimRef`, and its applier: its source, its automated sync (an `Application`), and the permission to write it. Commits still flow; apply requests may not. |

A green `GitTarget` with a red `WatchRule` or `ClusterWatchRule` means the destination may be fine,
but the selected source objects are not being watched or authorized. Inspect the rule's `Ready`,
`GitTargetReady`, `SourceNamespaceAuthorized`, and `StreamsRunning` conditions.

After fixing a Git-side folder problem, request a fresh read of the path:

```bash
kubectl annotate gittarget -n <namespace> <name> \
  reconcile.configbutler.ai/requestedAt="$(date -Iseconds)" --overwrite
```

The kstatus trio is also on Prometheus, so the same answer is alertable without a kubeconfig:
`gitopsreverser_resource_condition{kind="GitTarget", type="Ready", status="False"} == 1` names every
target that has not been accepted, and `reason` says which gate refused. The metric covers `Ready`,
`Reconciling` and `Stalled` for every object on this page except `CommitRequest`; the per-kind axis
conditions above stay on the object, where `kubectl describe` reads them. See
[interpreting-metrics.md](interpreting-metrics.md#configuration-state).

### `GitTarget.spec.commit`: how writes become commits

`spec.commit` says how this target's writes are batched into commits and how those commits are
phrased. Both describe the folder being written rather than the route to the repository, which is
why they sit here and not on the `GitProvider`: two `GitTarget`s sharing one connection can disagree
about both, so an RBAC folder that wants a commit per change and an app folder that wants a burst
coalesced need not be two connections.

```yaml
spec:
  commit:
    window:
      idleTimeout: "5s"
      maxDuration: "1m"
    message:
      liveTemplate: "chore: sync {{.Count}} resource{{if ne .Count 1}}s{{end}}"
```

Configure [automatic messages](commit-messages.md), submit a [save message](#commitrequest),
and interpret its [conditions](#commitrequest).

#### The commit window (`spec.commit.window`)

A commit window collects one author's writes to this target and commits them together. **Only
writes open a window**: an author's first write opens it, and nothing else does. It closes on two
timers, whichever comes first:

- `idleTimeout` closes it after this much silence. Every write it collects restarts it. The
  default is `5s`; `0s` commits every write on its own.
- `maxDuration` closes it this long after it opened, however much keeps arriving, so continuous
  activity cannot postpone a commit indefinitely. The default is `1m`.

`idleTimeout` may not exceed `maxDuration`. A window takes the target's timers when it opens, so
editing them affects windows that open afterward, never one already running. A
[`CommitRequest`](#the-request-window-specwindow) that attaches to a window replaces both timers
for that window.

One branch worker holds one live window, bound to an author and target. Interleaved authors or
targets split a burst. Atomic writes, a resync, buffer limits, shutdown, an `attach: Next` request,
a push notice from the [receiver](#gitreceiver), and the
[flush](bi-directional.md#flush-then-apply) before an apply can close a window early. Push cooldown
is independent: a zero timer does not promise an immediate remote push. See the
[complete applier claim rules](spec/commit-window-refactor.md).

Both values are **Go duration strings with a mandatory unit**, at most `24h`, checked by the API
server: `"750ms"`, `"1.5m"` and `"1m30s"` are accepted, while `"30"`, `".5s"`, `"5 seconds"` and
`"-1s"` are refused at admission with the field named. Nothing downstream re-checks them, because a
malformed value can no longer be stored.

The units are Go's own (`ns`, `us`/`µs`, `ms`, `s`, `m`, `h`) rather than the shorter set Flux
allows, so that every accepted value survives being read and written back by a client: `"0.5ms"`
is re-serialized as `"500µs"`, and a value that cannot be rewritten is one no controller could
ever update.

#### Commit message templates

Three templates decide how a commit is phrased: `liveTemplate` for a live window,
`reconcileTemplate` for an atomic snapshot or resync, and `requestTemplate` for a window a
[save request](#commitrequest) attached to. Each is a Go template over a documented context,
checked when the target reconciles rather than at commit time, and each has a built-in default, so a
target that sets none still writes useful messages.

```yaml
spec:
  commit:
    message:
      liveTemplate: "chore: sync {{.Count}} resource{{if ne .Count 1}}s{{end}}"
```

See [commit messages](commit-messages.md) for the full reference: the precedence rules, every field
of the template context, how to frame a save request's message with what was saved, and the
validation a bad template fails.

### Seeing what a target will do, before it does it

**Point one at a scratch branch.**

```yaml
kind: GitBranch
metadata: {name: gitops-preview}
spec:
  gitProviderRef: {name: homelab}
  branch: gitops-preview          # not main
---
kind: GitTarget
spec:
  gitBranchRef: {name: gitops-preview}
  kubeProviderRef: {name: local, namespace: gitops-reverser}
  path: apps/checkout
```

It commits, and you read the commits: the real files, the real `resources:` registrations, the real
deletes, in a diff you can review. Nothing is simulated, so nothing can be wrong about it. When you
are happy, delete the preview target and declare the real one. `spec.gitBranchRef` is immutable, so
the two are always separate objects, and deleting the preview cannot disturb the real folder.

For inspecting a repository without a cluster at all, use the `manifest-analyzer` CLI.

### Starting the write branch from another branch (`spec.parentBranch`)

`spec.parentBranch` is a `GitBranch` field. A `GitTarget` on a `GitBranch` whose `spec.branch` does
not exist on the remote waits on standby: it compares the cluster against the **parent branch** and
creates nothing while they agree. The first write that has
something to commit creates the branch from the parent's current tip, checked when it pushes, so a
target that waited a week does not publish onto a week-old checkout. A write that finds nothing to
change creates no branch at all.

The parent is the remote's default branch unless you name one:

```yaml
kind: GitBranch
spec:
  gitProviderRef: {name: homelab}
  branch: reverser/prod-edits     # the write branch: created on the first commit
  parentBranch: env/prod          # what it is created from
```

- An explicit parent must exist when the write branch is created. Otherwise the `GitBranch` reports
  `Ready=False` with reason `ParentBranchNotFound`, its `GitTarget`s report `GitBranchNotReady`, and
  nothing is written.
- It recovers on its own once the parent is pushed, with no edit to the cluster and with periodic
  refresh on or off. The branch worker looks for the parent with one ref advertisement after 10s,
  then after twice as long each time, up to every 5 minutes, shared by every target on the branch.
  When the parent is back it publishes the writes it held, with their own authors and messages;
  until those are published the `GitBranch` reports `Ready=False` with reason
  `RecoveringParentBranch`.
- An omitted parent is the remote's default branch, as last discovered. In an empty repository (no
  refs at all, tags included) the first commit starts a branch with no history. A repository that
  is not empty but whose `HEAD` names no branch it carries also reports `ParentBranchNotFound`, and
  writes nothing until `HEAD` is fixed or `parentBranch` is set; that never starts an orphan.
- A detached `HEAD`, or a server that does not say what `HEAD` points at, resolves the way go-git
  resolves it: to `master` when it is at `HEAD`'s commit, otherwise to the alphabetically first
  branch that is. Set `parentBranch` when you need certainty rather than that guess.
- When the remote's `HEAD` switches to another branch, the next discovery (a refresh, or any fetch)
  follows it. A push cannot see `HEAD`, so with periodic refresh off a write branch can still start
  from the previous default branch until then.
- Once the write branch exists, it is used as it is: new commits on the parent do not reach it.
  Delete the write branch after its changes are merged, and the next edit starts a fresh one.
- `parentBranch` is immutable, including adding or removing it: delete and recreate the `GitBranch`
  to change it. It is only read, so the provider's `allowedGitBranches` need not allow it.
- While the write branch is absent, the `GitBranch`'s `status.remote.parent` shows the parent,
  whether it was `Found`, `Missing` or `Unborn`, and the commit the first commit would build on. With
  `parentBranch` omitted, that is how you see which default branch the remote resolved.
- One `GitBranch` holds a repository branch, so every `GitTarget` on it has the same parent.

`spec.suspend` is **not** this. It is the next section.

### Stopping a target from writing (`spec.suspend`)

The panic knob. One field that stops a target writing, without deleting it and without unpicking
the `WatchRule` objects you would have to rebuild afterwards:

```yaml
spec:
  path: apps/checkout
  suspend: true
```

A suspended target keeps its watches, keeps receiving events, and keeps scanning its folder. A valve
that also stopped looking would leave `status.placement` frozen at whatever the folder looked like
the moment you turned it, which is exactly when a stale answer costs the most. It plans no new
commit. `Ready` stays `True` with reason `Suspended`, because not writing is the configured outcome
rather than a fault; every other gate still applies, so a suspended target with a broken
`GitProvider` is still not ready.

`status.retention` is not published while a target is suspended: no resync sweeps, so nothing is
counted, and a published zero would read as "converged" when it means "not measured".

**It takes effect at the next planning boundary, not instantly.** Work already committed locally
when you set it is still pushed, so a target suspended during a push cooldown can publish one more
commit seconds later. That is deliberate: a local commit that is never pushed would sit in the
operator's checkout and surface later, out of order, when you resume. Suspend is a valve on new
work, not an undo.

A `CommitRequest` that ends on a suspended target fails with `FinalizeFailed` and the reason, rather
than resolving `Ready=True` with nothing written. The exception is `WindowMismatch`: a request that
only saw another author's window still reports that. One whose commit was already made locally when
you set `suspend` is pushed with it and resolves normally.

Clearing `suspend` resumes from the cluster's current state on the next resync. The writes
suppressed while it was set are not replayed, so what lands is what the cluster holds then, not a
backlog of the values it passed through.

**Suspend stops writes, not deployments.** A suspended target's applier is still asked to apply a
commit somebody else pushed, because with
[`spec.applierClaimRef`](#who-applies-this-folder-specapplierclaimref) set nothing else asks it,
and a paused mirror must not stall Git-side changes. It requests nothing for a refusal, since that
request exists to act on a live edit the target is no longer publishing.

To re-read the folder now rather than on the periodic cadence, stamp the reconcile-request
annotation with any value that changes:

```bash
kubectl annotate gittarget checkout \
  reconcile.configbutler.ai/requestedAt="$(date -u +%FT%TZ)" --overwrite
```

The spelling is Flux's `reconcile.fluxcd.io/requestedAt` with our own group; the value carries no
meaning, only its change does.

### What the folder resolved to (`status.placement`)

`status.placement` is what the last scan learned about the **folder**, and it is available before
the target has ever written. It answers one question: *why did a write take the shape it did, or why
was it refused?* It deliberately restates nothing the spec already carries:

```yaml
status:
  conditions:
    - type: LayoutResolved
      status: "True"
      reason: SingleKustomization       # SingleKustomization | Ambiguous | None
      message: 'render root "." governs new files; it renders ../../base, which is read-only input'
  placement:
    mode: KustomizeOverlay              # Plain | KustomizeRoot | KustomizeOverlay
    renderRoot: .
    readOnlyBases: ["../../base"]
    resolvedAtCommit: 9f3c1ab
    resolvedAt: "2026-07-30T09:14:22Z"
```

- `mode` is **how this folder is written**, and it is the field that predicts the surprises:

  | | `Plain` | `KustomizeRoot` | `KustomizeOverlay` |
  |---|---|---|---|
  | a new document | written | written **and registered** in `resources:` | same |
  | a delete | file removed | file removed **and its `resources:` entry dropped** | same, unless the object is inherited |
  | deleting an object the folder inherits from its base | n/a | n/a | a **`$patch: delete` is authored into the overlay**; nothing is removed |
  | editing a field the base owns | n/a | n/a | authored into the overlay for `images:`/`replicas:`, **refused** otherwise |
  | can kustomize refuse the write | no | yes, by re-rendering | yes |

  It is absent when the folder covers several render roots, along with `renderRoot`: there is no
  single answer.
- `renderRoot` is the kustomization directory that governs new documents, relative to `spec.path`;
  `.` is the folder itself. Empty for `Plain`, and when the folder has several roots.
- `readOnlyBases` are directories the folder renders but may never write to, spelled the way the
  overlay's own `resources:` spells them. Non-empty exactly when `mode` is `KustomizeOverlay`, and
  an edit landing on a document under one of them is what a `WriteBoundaryRefused` refusal is about.
- `resolvedAtCommit` and `resolvedAt` date **the resolution**, not the last scan. They advance
  when the resolution changes, not on every scan of an unchanged folder, so a timestamp well in the
  past means the folder's shape has been stable, rather than that scanning stopped.

The stanza carries no counters, no `examples`, and no copy of `spec.serializeNamespace` or of the
`byType` map. The rule is that a status field earns its place only if a reader cannot get it from
the spec in the same GET, **and** it varies with this folder. To preview what a target would write,
use a scratch branch (above) rather than a status field.

`LayoutResolved` reports the verdict. `None` (no kustomization governs the folder) is `True` and
perfectly healthy; it is the ordinary case. Only `Ambiguous` is `False`:

**A target must cover exactly one kustomize render root.** A `GitTarget` at `apps/checkout` in a
base-plus-overlays repository covers the base and every overlay, so a new document has no single
root to be placed into, and picking one would hand it to an environment nobody named. Such a target
reports `LayoutResolved=False` with reason `Ambiguous`, naming the roots it covers, and refuses to
place new documents (`GitPathAccepted=False`, reason `AmbiguousLayout`). Existing documents are
unaffected: they are edited where they already live. The fix is to point the target at one leaf,
`apps/checkout/overlays/prod`, and declare the other environments as their own `GitTarget` objects:
one target is one environment is one write partition.

### Where the branch is, and when that was last proved (`status.remote`)

`status.remote` is on the `GitBranch`: what its branch worker last **proved** about the branch on
the Git remote. It is an observation rather than a record of work, so it is there before anything
has been written:

```yaml
status:
  remote:
    commit: ""                          # empty = the branch is not on the remote (yet)
    lastVerifiedAt: "2026-09-23T10:14:02Z"
    verifiedBy: Push                    # Push | Fetch
    parent:                             # only while the branch is not on the remote
      state: Found                      # Found | Missing | Unborn
      branch: main                      # spec.parentBranch, or the remote's default branch
      commit: 9a1e07c3d2...             # set only when Found
```

- `commit` is where the branch is. Empty means the branch is not on the remote at all, which is
  not an error: a branch does not exist without a commit.
- `lastVerifiedAt` answers **"has anything looked"**, which is the question
  `placement.resolvedAtCommit` deliberately does not: that one dates the resolution, so an old
  value there means the folder's shape has been stable.
- `verifiedBy` says what proved it. `Push` means the server accepted a ref update of ours, so this
  commit is Reverser's own work. `Fetch` means it went and looked, and this is what was there.
  A `Fetch` beside a commit none of your publications produced is how a **foreign push** to the
  branch is read off `kubectl`.
- `parent` appears only while the branch is not on the remote: it says what the branch would be
  created from, observed together with the branch's absence. `state` is `Found` (the first commit
  builds on `commit`, the parent's tip), `Missing` (the parent is not on the remote either; the
  `GitBranch` reports `ParentBranchNotFound` and nothing is written) or `Unborn` (an empty repository, with
  no refs at all, and no parent configured; the first commit starts the branch with no history). See
  [starting the write branch from another branch](#starting-the-write-branch-from-another-branch-specparentbranch).

`kubectl get gitbranch` shows `commit` in the `Commit` column, and `-o wide` shows
`lastVerifiedAt` as an age, in the `Verified` column.

Two things renew it. A **push** does, for free: the push session reads the remote's advertisement
and the server names the hash it accepted, on the connection the push was making anyway. So a
target that is publishing never needs anything else. An **idle** target is asked to re-prove it on
its reconcile tick; see
[`--git-refresh-interval`](#keeping-an-idle-target-fresh---git-refresh-interval) below.

**What is proved and what is published are two different rates.** Every push and every fetch
proves where the branch is, and the worker wakes the `GitBranch` each time. Writing status on every
push would invalidate every watcher's cached copy of the type at the pace the branch moves, so the
`GitBranch` writes this stanza at most once a minute, and comes back when that floor lifts. Push and
read status immediately, and you may see the previous commit for up to a minute.

Two corrections skip the once-a-minute floor: the **first** observation, and **taking back** a
commit that names a repository the `GitBranch` no longer points at (a recreated `GitProvider` with
a different URL). And none of this promises
anything about the branch itself: an unreachable remote yields no observation at all, which is
exactly what the pair `commit` + `lastVerifiedAt` is there to say.

`GitProvider.status.lastVerifiedAt` is the same word for the connection rather than the branch:
when the credential and the repository were last proved together. It is never cleared, and a
failure leaves it unchanged, so it tells you when the provider last worked.
A `Ready=False` beside it can come from a later connectivity failure, or from a signing-key or
Secret problem that stops the check from running at all; the condition's reason says which.

### Keeping an idle target fresh (`--git-refresh-interval`)

A target that is writing keeps its own view of Git current. One that has gone quiet used to hold
its previous answer indefinitely, because it is converged and its periodic passes publish status
without touching Git, until somebody annotated it or it wrote again.

`--git-refresh-interval` (Helm: `git.refreshInterval`, default `10m`) bounds that.
On the target's reconcile tick, a branch whose last observation is older than the interval is
re-proved:

| Target state | What it costs |
| --- | --- |
| Actively writing | **Nothing, ever.** Each cycle ends in a push that reads the advertisement, so no refresh is scheduled |
| Refused | Nothing. It is not converged, so it already re-reads every ten seconds |
| Healthy and idle | **One ref advertisement per interval**, plus a fetch only on the intervals where the branch had actually moved |

A refresh republishes `status.remote`, and re-reads the folder to republish `status.placement` and
to answer whether the folder can still be written at all. That last part is why it exists as much
as the first: somebody else's push can leave content Reverser cannot write, and a push emits no
Kubernetes event, so without a read nothing notices until your next live edit. That edit is then
refused, and its events are dropped until a resync recovers them. A refresh that finds such content
sets `GitPathAccepted=False` naming the file and the reason, and a later refresh that finds the
folder fixed clears it again.

Raising that condition puts the target on the ten-second re-check loop, which is the recovery path:
a human fixing a folder in Git produces no event either, so a periodic re-read is the only thing
that can notice. What a refresh still never does is write anything. It plans nothing, commits
nothing and pushes nothing.

That includes [`spec.onRefusal: Reapply`](#reverting-a-refused-edit-speconrefusal), which a folder
refusal never triggers. That setting reverts a refused **edit**, and reapplying Git cannot fix a
folder: only a human can, and the applier may be part-way through its own corrections there.

A refusal a **write** found is not cleared by a refresh. A structural read sees the files; it
cannot see that a particular write may not touch one of them, so only the resync that writes the
affected type clears that kind.

Set `0` to turn it off. Reverser then holds no timer against your Git host at all and an idle
target generates no Git traffic; `status.remote` still works, because pushes still renew it. The
effective granularity is the 5-minute control-plane reconcile, so a value below that means "every
tick". [`gitopsreverser_git_fetches_total{reason="refresh"}`](interpreting-metrics.md#reading-git_fetches_total)
is what it costs.

A refresh that finds a commit GitOps Reverser did not push is a foreign commit like any other: the
target's [applier](#who-applies-this-folder-specapplierclaimref) is asked to apply it after a flush.
That makes the refresh the backstop for deployments when no push webhook reaches the
operator. It is not a latency mechanism: it buys bounded staleness with no inbound surface and no
Git-host configuration. Seconds-fresh is the job of the
[push receiver](#gitreceiver).

### Who applies this folder (`spec.applierClaimRef`)

Set this only when Flux or Argo CD applies the folder this `GitTarget` writes. A folder nothing
applies, and every folder Flux or Argo CD applies that GitOps Reverser does not write, needs no
applier claim: leave the field out, and GitOps Reverser never touches Flux or Argo CD.

![Bi-directional setup: a GitTarget mirrors the cluster into a branch, and an ArgoCDClaim (or a
FluxClaim) holds the Argo CD Application that applies the same folder, so it syncs only when GitOps
Reverser asks; a GitReceiver picks up pushes made in Git](images/config-bi-directional.excalidraw.svg)

When the folder is shared, an **applier claim** holds the object that applies it (its applier), and
GitOps Reverser decides when it applies: a `FluxClaim` holds a Flux `Kustomization`, an
`ArgoCDClaim` an Argo CD `Application`. Naming one makes the folder
[coordinated](definitions.md#applying-back). The `GitTarget` names the applier claim, in its own
namespace.
[Bi-directional GitOps](bi-directional.md) is the model, and the complete setups, with the
lifecycle of an apply, every condition reason and the permissions, are
[bi-directional GitOps with Flux](bi-directional-flux.md) and
[bi-directional GitOps with Argo CD](bi-directional-argocd.md). The reason it has to be this way is
the [applier claim design](design/apply-trigger.md).

**An applier claim acts only while an eligible `GitTarget` references it**: one in its namespace
that names it, is admitted by its `KubeProvider`s, and writes the repository and branch the applier
fetches. Until one does, the applier claim reads and writes nothing in the applier's cluster, and
reports `Configured=Unknown` and `Ready=True`, both `NotReferenced`, so the two may be created in
either order. An applier claim that held its applier hands it back once no eligible `GitTarget`
references it any more.

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: FluxClaim
metadata:
  name: checkout
  namespace: team-a
spec:
  kubeProviderRef:             # the cluster Flux runs in
    name: local
    namespace: gitops-reverser
  kustomizationRef:
    namespace: flux-system     # default: the applier claim's namespace
    name: checkout
---
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: checkout
  namespace: team-a
spec:
  # gitBranchRef, kubeProviderRef, path ...
  applierClaimRef:
    kind: FluxClaim          # or ArgoCDClaim
    name: checkout
```

| Field | Default | What it does |
| --- | --- | --- |
| `FluxClaim.spec.kubeProviderRef`, `ArgoCDClaim.spec.kubeProviderRef` | required | The `KubeProvider` whose cluster holds the applier, in the applier claim's namespace unless `namespace` names another. Immutable |
| `FluxClaim.spec.kustomizationRef` | required | A Flux `Kustomization` whose `sourceRef` is a `GitRepository` that follows a branch. Immutable |
| `ArgoCDClaim.spec.applicationRef` | required | An Argo CD `Application` on a single source whose `targetRevision` names a branch. The applier claim holds its automated sync off, unless an `ApplicationSet` or another field manager owns the switch and keeps it off. Immutable |
| `GitTarget.spec.applierClaimRef` | none | The applier claim, by `kind` and `name`, in the `GitTarget`'s namespace |

**One applier claim, several folders.** Several `GitTarget`s may name one applier claim: their
folders are applied by the same applier, and it is asked once for all of them. Each one's repository
and branch must be the ones the applier applies, or that `GitTarget`'s `ApplierClaimReady` is
`False` with `SourceMismatch`; one that matches is enough for the applier claim to act.

**One applier claim holds an applier, per `KubeProvider`.** A second applier claim of the same kind
in line for the same applier through the same `KubeProvider` reports `HeldByAnotherClaim` and does
nothing. An applier claim no eligible `GitTarget` references is not in line, so it keeps no other
out. GitOps Reverser can't tell that two `KubeProvider`s reach the same cluster: two applier claims
naming one `Kustomization` through two providers would both hold it, each resuming and suspending
it under the other. Name one `KubeProvider` for every applier claim that reaches a given cluster.

**Where the applier is looked up.** In the cluster the applier claim's `spec.kubeProviderRef` names,
with the identity that [`KubeProvider`](#kubeprovider) uses. Name a provider for a management
cluster when Flux or Argo CD runs there; an `inCluster` provider is the operator's own cluster. That
provider's `spec.allowedApplierClaims` must admit the applier claim's namespace, separately from
mirroring, or the applier claim is `Configured=False` with `NamespaceNotAuthorized` (or
`KubeProviderNotFound`). The applier's namespace, such as `flux-system` in the example above, must
be in the `applierNamespaces` those grants give it, or the applier claim is `Configured=False` with
`ApplierNamespaceNotAllowed`. A denial hands a held applier back; see
[when a grant shrinks](#when-a-grant-shrinks). For the operator's own
cluster, the chart grants the applier permissions with `applierAccess.flux: true` and
`applierAccess.argocd: true`.

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: FluxClaim
metadata:
  name: prod-eu-1-apps
  namespace: team-a
spec:
  kubeProviderRef:
    name: management           # Flux runs here, and applies to prod-eu-1
  kustomizationRef:
    namespace: flux-system
    name: prod-eu-1-apps
```

**When a request is made.** After a [flush](bi-directional.md#flush-then-apply): when an apply comes
due, the branch worker closes the open commit window, publishes the writes decided up to that
moment at once, and asks for the tip that publication produced. An edit decided after that is
recorded in the apply window, so an apply that writes over it is withheld from Git and repaired. A
registered `CommitRequest` (a save sets its own commit boundary), a
parent branch being recovered, or a checkout a failed write left dirty holds the request back. No
applier is asked for anything until every initial snapshot of every `GitTarget` naming its applier claim
is published, and nothing is asked while the remote does not carry the branch. The rule is the same
for a `Kustomization` and an `Application`.

| `reason` | When | Sent to |
| --- | --- | --- |
| `ForeignCommit` | The branch gained a commit GitOps Reverser did not push. The pending writes are replayed onto it and pushed first, so the request names that pushed commit | Every applier declared on the branch, once per applier |
| `Refusal` | An eligible refused edit, with `spec.onRefusal: Reapply` | This target's applier |
| `Repair` | An apply reverted an API edit made after the flush; Git kept the edit | That applier |
| `CatchUp` | To adopt: the API created an object in the folder, or a resync added an object to Git. To converge: once when the operator starts, once when a `GitTarget` first names the applier claim, once the applier claim's `catchUpAfter` has passed since a push of GitOps Reverser's own, and after a `CommitRequest` with [`catchUp: AfterPush`](#applying-a-save-at-once) pushed a commit | To adopt, that folder's applier; to converge, every applier declared on the branch at start, the newly named one, the one left behind, or the requesting `GitTarget`'s |

An API edit asks for no apply, and neither does a push of GitOps Reverser's own, at once: the cluster
already runs what they hold. Only the applier's own state is left behind, which `catchUpAfter`
catches up later. A create is different, because an applier prunes only what it applied: an
object the API created (an `ADDED` event whose newest field managers are not an applier's) carries
neither Flux's owner labels nor Argo CD's tracking annotation, so a later Git-side delete of its
file would leave it running. An adopt is therefore applied even at the commit the applier last
applied, since that apply is what stamps the object. A converge asks nothing when the applier's last
successful operation, as `status.operation` records it, applied the tip.

**An object another applier owns is not mirrored into the folder.** When a `GitTarget` names an
applier claim, an object whose Flux owner labels (`kustomize.toolkit.fluxcd.io/name` and
`kustomize.toolkit.fluxcd.io/namespace`) or Argo CD tracking annotation
(`argocd.argoproj.io/tracking-id`) name an applier other than the applier claim's is excluded: a live
change to it is not mirrored, and a resync neither writes it nor removes its document, even under
`prune.mode: Always`. Two appliers applying one object fight each other. Each excluded object is
logged once and counted in `gitopsreverser_apply_ownership_conflicts_total`. A `GitTarget` without
`spec.applierClaimRef` excludes nothing: mirroring another applier's objects one way is what such a
folder can exist for.

**What it reports.** The applier claim reports its applier and the operation it is running. The branch
worker writes `status.operation` and `status.repairPending` at every move of the operation, so they are
current without anything reconciling:

```yaml
kind: FluxClaim
status:
  conditions:
    - type: Configured
      status: "True"
      reason: Succeeded
      message: Kustomization flux-system/checkout applies GitRepository flux-system/platform, ...
    - type: InControl
      status: "True"
      reason: Suspended        # Suspended | Applying | Suspending | Releasing | Revoking | NotReferenced | NotConfigured
    - type: Ready
      status: "True"
  source:
    gitRepository: {namespace: flux-system, name: platform}
    url: https://github.com/acme/platform
    branch: main
    alsoAppliedBy:             # what else applies this GitRepository, held by no FluxClaim
      - {kind: HelmRelease, namespace: platform, name: ingress}
  operation:
    phase: Succeeded           # Requested | Running | Settling | Unresolved | Succeeded | Failed
    reason: ForeignCommit      # ForeignCommit | Refusal | Repair | CatchUp
    commit: 4f2c1ab9...
    token: "2026-10-20T10:14:03.123456789Z"
    suspendToken: "2026-10-20T10:14:20.987654321Z"
    appliedRevision: main@sha1:4f2c1ab9...
    requestedAt: "2026-10-20T10:14:03Z"
    finishedAt: "2026-10-20T10:14:21Z"
  repairPending: false
```

| Field | Means |
| --- | --- |
| `operation.phase` | `Requested`, `Running`, `Settling` (Flux only: applied, not yet seen suspended again) and `Unresolved` (not completed in 5 minutes, still followed) are in flight; `Succeeded` and `Failed` have completed. [The life of an apply](bi-directional.md#the-life-of-an-apply) draws the transitions |
| `operation.token` | Identifies the request in the applier's own status: the `reconcile.fluxcd.io/requestedAt` value of the resume, or the `gitops-reverser request` entry in an Argo CD operation's `info` |
| `operation.suspendToken` | Flux only: the token of the suspend after the apply. Once it is handled, nothing that started before the suspend is still running |
| `operation.appliedRevision` | The revision the applier reports it applied. It can differ from `commit` when the branch moved between the check and the apply |
| `repairPending` | `true` while an apply reverted an API edit made after its flush: Git kept the edit, and the cluster runs the older value until the `Repair` |
| `source.alsoAppliedBy` | Flux only: the `Kustomization`s and `HelmRelease`s that apply the same `GitRepository` and that no `FluxClaim` holds. Information only; it never affects `Ready` |

A `Kustomization` resumed outside GitOps Reverser (`flux resume`) is suspended again and recorded as
an `Unresolved` operation with no `reason` and no `commit`, which completes once the suspend is
handled.

**Conditions.** `Configured` is a gate, and the applier claim's `Ready` aggregates it; either kind is
also `Ready` only once `InControl` is `True`: GitOps Reverser decides when the applier applies
(`Suspended`, or `Applying` while an apply of GitOps Reverser's is in flight; `AutomatedSyncOff`
for an `ArgoCDClaim`). Either kind is `Ready=False` (`OperationUnresolved`) while its operation
is `Unresolved`. An unreferenced applier claim is `Ready=True` (`NotReferenced`). The
`GitTarget`s naming the applier claim are reported as `NotBound` (`Unknown` while one cannot be judged
yet, `False` once each is refused for different reasons), or as the refusal they share,
`SourceMismatch` or `NamespaceNotAuthorized`, every `GitTarget` and its reason in the message. The
other `False` reasons are `ApplierNotFound`, `UnsupportedSource`, `ArgoCDVersionNotSupported`,
`OwnedByApplicationSet` and `AutomatedSyncOwnedElsewhere` (Argo CD), `SuspendOwnedElsewhere` (Flux),
`Forbidden`, `NamespaceNotAuthorized` (the applier claim's own namespace),
`KubeProviderNotFound`, `AuditRouteConflict`, `ApplierNamespaceNotAllowed` and `HeldByAnotherClaim`; each guide's
table says what they mean for its applier and how to fix them
([Flux](bi-directional-flux.md#the-fluxclaim), [Argo CD](bi-directional-argocd.md#the-argocdclaim)).
The `GitTarget` reports `ApplierClaimReady`, and its `Ready` aggregates it: `True` when the applier claim
is `Ready` and its applier applies this target's repository and branch, otherwise the applier claim's own
`Ready` reason, `ApplierClaimNotFound`, `SourceMismatch`, or `Unknown` (`NotReferenced`) until the
applier claim has seen that the `GitTarget` names it. Each delivered request records an
`ApplyRequested` Event on the `GitTarget`, and one that could not be delivered records
`ApplyRequestFailed` once.

The applier is read on the applier claim's own reconcile tick rather than watched, so a cluster without
the Flux or Argo CD CRDs costs nothing: every 15 seconds while it is expected to clear on its own,
such as an applier created after its applier claim, and otherwise every minute for a `FluxClaim` (that
read is what finds a `flux resume`) and every 5 minutes for an `ArgoCDClaim`. A `FluxClaim` that
is `Suspending` is read every 2 seconds. Annotate the applier claim with
`reconcile.configbutler.ai/requestedAt` to have it read now.

**Deleting a `FluxClaim` hands the `Kustomization` back to Flux,** through the finalizer
`configbutler.ai/flux-claim`, unless another `FluxClaim` is in line for it, and so does the last
eligible `GitTarget` that stops naming it. An applier claim that never held its `Kustomization` carries no
finalizer. The [Flux guide](bi-directional-flux.md#how-the-applier-claim-holds-the-kustomization) has the
details.

### Reverting a refused edit (`spec.onRefusal`)

A refusal means the acceptance gate or a write-boundary check found no legal destination for the
write. Nothing is committed for it, `GitPathAccepted` goes `False`, and the live edit stays until
something applies Git over it. `spec.onRefusal` decides whether GitOps Reverser asks for that:

```yaml
spec:
  path: apps/checkout
  applierClaimRef: {kind: FluxClaim, name: checkout}
  onRefusal: Reapply   # default: Ignore
```

| Value | What happens |
| --- | --- |
| `Ignore` (default) | The refusal is reported and nothing else happens |
| `Reapply` | After a flush, this target's [applier](#who-applies-this-folder-specapplierclaimref) is asked to apply the current tip, with reason `Refusal`. The refused edit is reverted when the applier applies it; the request asks, and the applier's own status says whether it did |

`Reapply` requires `spec.applierClaimRef`; a target without one is rejected at admission. The request goes
to this target's applier alone, and it goes out after a flush, so an allowed edit still in a
commit window is published before the apply and is not reverted with the refused one. An
`Application` is asked to sync the tip. When a `Kustomization`'s `GitRepository` has to fetch to
reach the tip, the `Kustomization`s a `FluxClaim` holds stay suspended and ignore the new artifact;
only the objects no applier claim holds, listed in the applier claim's `status.source.alsoAppliedBy`, apply
it, as they would for any push. The
[bi-directional guide](bi-directional.md#choosing-speconrefusal) covers when to choose it.

**It only fires for an edit to a document the folder already holds, and never for a write that
removes one.** That is the pruning guard. Re-applying corrects drift on an object Git manages,
which is the point. For an object Git does not manage, re-applying either does nothing (Flux prunes
from its inventory and Argo CD from the resources it tracks, and a live-created object is in
neither) or, if the object was managed and has since been removed from Git, prunes it. Hurrying
along somebody else's delete is not this operator's call, and the two cases cannot be told apart
from here, so both are excluded.

Establishing that takes more than the refusal's kind, and more than the file. A render refusal fires
for a brand-new resource an `images:` entry would override; removing one document from a file that
holds two leaves the file in place, which looks exactly like an edit. Eligibility is therefore
decided per document, and a write that removes one anywhere is excluded whole.
Folder-level refusals, such as invalid YAML or unsupported content, never trigger it: they need a
correction to the folder.

Three more guards. A suspended target requests nothing. A `GitTarget` that cannot be read is
treated as `Ignore`, since missing evidence is not consent. And requests are coalesced per target:
a controller rewriting a base-owned field refuses on every one of its own reconciles, so refusals
that arrive while a request is outstanding share the next one rather than each making their own.

**A refusal nobody corrects is requested once, not once per recheck.** The target keeps re-reading
its folder and keeps refusing, and an observation of the same objects with the same issues is
already covered by the request made for it. Anything different (a new value, another object, a
different refusal) is a new request. So is the same edit made a second time after the applier
reverted it, because a write of that resource type was accepted in between.

Reapplying does not clear the refusal condition: recovery still requires a successful resync
covering the refused scope. It removes the offending live difference, but it cannot repair an
unsupported folder or give a field a writable destination.

### Deletion policy (`spec.prune.mode`)

A target removes a document from Git for one of two very different reasons, and `spec.prune.mode`
controls them separately:

- an **explicit source DELETE event**: the source cluster told the operator the resource is gone;
- a **resync mark-and-sweep**: a snapshot taken when a watch stream starts or restarts did not
  contain a resource that Git still has a document for, so its absence is *inferred*.

The second is only as trustworthy as the snapshot's **scope**. A snapshot the operator could not
finish is not the risk: a failed list or watch blocks the stream and enqueues no resync at all, and a
replay cut short before its initial-events bookmark enqueues nothing, so a source-cluster outage or
a revoked RBAC grant currently stops a sweep rather than shrinking one.

The risk is a snapshot that is *complete* but gathered against the wrong scope: a watch rule narrower
than you intended, version skew, or an older controller that does not understand a newer scope field.
That snapshot is smaller than reality and indistinguishable from a converged one, and a sweep turns
it into deleted manifests. `OnEvent` is the defense, and it also covers the outage case in depth. Failing
closed there is a property of how the gather works today, and not a guarantee the API makes.

Revoking access to a namespace is neither of those two paths, and removes nothing. When a namespace
leaves a target's watch set (a rule deleted, a `sourceNamespace` narrowed, a label revoked), the
documents already mirrored from it stay in Git, whatever `spec.prune.mode` says, and no automatic
process removes them. That is deliberate: deleting a tenant's manifests as a side effect of a policy
edit is destructive and a typo in a selector would be enough to trigger it. Removing them is an
operator action.

| Mode | Explicit source DELETE | Resync mark-and-sweep | Use it for |
|---|---|---|---|
| `Never` | kept | kept | an archive or tombstone mirror that only ever gains documents |
| `OnEvent` (default) | mirrored | kept | mirroring observed deletes without ever inferring one |
| `Always` | mirrored | swept | full desired-state convergence, including cleaning up stale documents |

```yaml
spec:
  prune:
    mode: Always
```

Omitting `spec.prune` means `OnEvent`. That applies to a `GitTarget` created before this field
existed as well: an upgrade never changes an existing target to a more destructive policy, and you
do not have to edit anything to be safe.

Choose `Always` when the folder is meant to be a faithful, converged mirror and you accept that a
bad watch scope can delete manifests. Choose `Never` when the folder is an audit trail.

#### Seeing what was kept

A retained document is invisible in Git by design: nothing is written, so a retaining mirror and a
converged one look identical in the folder and in `git log`. Three signals report it instead, and
none of them is a failure: retention is the configured outcome, so no condition goes `False` for it.

```console
$ kubectl get gittarget acme -o jsonpath='{.status.retention}'
{"mode":"OnEvent","retainedDocuments":3,"lastChangedAt":"2026-07-21T13:20:00Z"}
```

- `status.retention.retainedDocuments` is how many managed documents a converged mirror would not
  hold. `0` means a resync ran and found nothing to retain; an **absent** `retention` block means no
  resync has reported yet, which is not the same thing. `mode` is the *effective* mode the count was
  produced under, and the only place a `GitTarget` that predates `spec.prune` shows one at all.
- A throttled log line names the target, its path, and the scope (one per target folder per 10
  minutes; the full detail is at `-v1`).
- `gitopsreverser_git_documents_total{outcome="retained"}`, labeled by `gittarget_namespace`,
  `gittarget_name`, and the resource type (`group`, `version`, `resource`). The type is what makes a
  retention actionable: a bare total names nothing to go and look at. It shares one counter with the
  writer's other document outcomes because they are one population (every document the writer
  decided about), so a dashboard reads them as one stacked series.

`status.retention` covers the resync sweep only. Under `Never` a suppressed source DELETE is not
counted, so a `Never` target can report `0` while still declining to mirror deletes.

The count is refreshed when a resync runs, so it lags a change in the cluster until the next one.

`lastChangedAt` records when the count or the effective mode last **changed**. It is not a
freshness signal and must not be read as one: a resync that re-reports the same numbers leaves it
untouched, so an old timestamp is equally consistent with stable retention and with nothing having
measured it since. The field is named for what it does.

For "are resyncs still running", read `gitopsreverser_git_resync_failures_total` and
`gitopsreverser_watch_recovery_total`. A rising
`gitopsreverser_git_documents_total{outcome="retained"}` is positive evidence that retention was
evaluated and found something, but a flat one proves nothing: a healthy resync retaining zero
produces no increment either.

`spec.prune` is mutable (unlike `gitBranchRef` and `path`), so a target can be moved to
`Always` once its watch scope is confirmed, without recreating it. Widening it to `Always` re-lists
the target's watched scopes, so the cleanup runs on the edit instead of waiting for the next replay.
Tightening it applies to the next write and leaves the streams alone, which is what makes it usable
as a stop button.

### Where new resources are written (`spec.placement`)

Placement decides the file path for a resource that has **no document in Git yet**. Once a document
exists, updates and deletes always edit it in place at its current location (found by manifest identity,
not path), so changing placement never moves an existing file; it only affects resources created after
the change.

**There is no re-layout operation today.** The practical consequence is worth stating plainly: a
folder laid out under an older policy keeps that layout for as long as its documents live, however
you change `spec.placement` afterwards. Placement governs the first write and nothing else. This is
a description of current behavior rather than a promise: a deliberate migration that moves documents
in a new commit is a coherent feature and is not ruled out.

To re-place a document by hand today, get it out of the folder and let the mirror write it again.
The writer rebuilds its identity index from the current scan of the folder, so a resource with no
document there is placed as new, under the current policy.

**Check `spec.prune.mode` first.** The route below deletes the cluster object and relies on the
mirror removing its document, which needs `OnEvent` (the default) or `Always`. Under `Never` the
mirror removes nothing, so step 2 never completes and you would be left waiting on a deletion that
is not coming.

1. Delete the object in the cluster.
2. **Wait until the deletion is committed and pushed** and the document is gone from the branch.
3. Recreate the object. It now has no document to update, so the write goes through the current
   placement policy.

Step 2 is not a formality. A delete and a recreate inside one commit window, by the same author,
coalesce by destination path into a single retained entry, so the pair cancels out: the deletion
never reaches Git, the document is still there, and the recreated object is updated in place at its
old location. From the outside that looks exactly like placement being ignored.

#### How a path is chosen (the resolution ladder)

For each new resource the operator walks this order and stops at the first that produces a path:

1. **`spec.placement.byType[<exact type>]`:** an explicit template for that resource's type, if you
   declared one.
2. **`spec.placement.default`:** your explicit catch-all template, if you declared one.
3. **The folder's one kustomize root:** if the whole folder is governed by exactly one supported
   `kustomization.yaml`, the new file goes beside it and is added to its `resources:` list in the same
   commit. This is not a guess about your conventions: a file that kustomization cannot reach would
   never be rendered, so it would be in Git and applied by nothing. Two or more supported
   kustomizations is ambiguous, and the operator declines rather than picking one.
4. **Built-in path:** `{namespace}/{group}/{resource}/{name}.yaml`, namespace first, the group
   omitted for core resources, no version segment, `_cluster/` in place of the namespace for
   cluster-scoped resources (an illegal namespace name, so it can never clash with a real one), and a
   `.sops.yaml` suffix for sensitive resources.

**The operator does not read the rest of your repository to place a file.** Where you keep the other
ConfigMaps does not decide where a new ConfigMap goes: a layout the ladder above cannot derive is one
you declare. So editing the repository never changes where the operator writes next.

#### Knowing when you need a rule

Every placement is counted, labeled with the GitTarget and the resource type, so "does this folder
need a `byType` line?" is a query rather than a folder inspection:

```promql
# types landing on the built-in path, per target: each is a candidate for a byType entry
sum by (gittarget_namespace, gittarget_name, group, version, resource) (
  increase(gitopsreverser_placements_total{source="builtin"}[24h])
)
```

`source="by_type"` is the exact rule you wrote for that type, `source="default"` is your catch-all
answering for a type no `byType` line names, and `source="kustomize_root"` is a folder whose own
structure answered. None needs attention, though a `default` series climbing where you expected
`by_type` is worth a look: it means a rule you thought you had written is not matching. Two
companions matter as much:

- `gitopsreverser_placement_refusals_total{reason}` counts resources the operator **did not write**:
  a template that escapes `spec.path` (`invalid_path`), a sensitive resource whose path is already
  taken (`sensitive_append`), a plaintext resource routed at an encrypted file
  (`plaintext_onto_encrypted`), two resources of mixed sensitivity landing on one new file
  (`mixed_sensitivity_new_file`), and a path whose multi-document file the writer cannot append to
  without dropping a sibling (`multi_document_target`). Any of these means a policy to fix; the write is retried after you
  fix it.
- `gitopsreverser_placement_kustomization_entries_total{outcome="failed"}` counts new files whose
  `resources:` entry could not be added. The file is committed and kustomize will never build it, so
  this is the one that looks fine in the folder and is not.

See [interpreting-metrics.md](interpreting-metrics.md) for the full label sets.

#### Namespaces in a kustomize folder

If the `kustomization.yaml` governing the destination sets `namespace:`, and it names the resource's own
namespace, the new file omits `metadata.namespace`, because the build supplies it and every other document in
that folder omits it too. This applies to a path you declared as much as to the kustomize-root
fallback. When the transformer names a *different* namespace the namespace is written explicitly:
leaving it out would hand the namespace to kustomize and render a different object than the one being
mirrored.

#### Declaring a layout (`byType` / `default`)

Set `spec.placement` when the layout you want is neither of the two things the operator can work out
for itself (a folder's single kustomization root, or the built-in path). For example, a bundle every
ConfigMap joins, or a per-namespace layout:

```yaml
spec:
  placement:
    byType:
      v1/configmaps: "{namespace}/configmaps.yaml"     # bundle every ConfigMap of a namespace into one file
      v1/secrets: "{namespace}/secrets/{name}.yaml"    # one file per Secret
    default: "{namespace}/{group}/{resource}/{name}.yaml"
```

- **`byType`** maps an exact `[group/]version/resource` key (core resources omit the group, e.g.
  `v1/configmaps`; grouped resources include it, e.g. `apps/v1/deployments`) to a path template.
- **`default`** is the template for any type with no `byType` entry. Omit it to fall through to the
  kustomize-root step and then the built-in path.
- Templates are small **brace-variable path templates** (see the table below), validated statically as
  part of the `Validated` gate: an unknown variable, a path that escapes `spec.path` (a leading `/` or
  `..`), or a non-`.yaml`/`.yml` suffix fails the target *before* any write.

#### Template variables

Every value is sanitized for use as a single path segment. An **empty** segment (an omitted variable,
e.g. `{group}` for a core resource) is dropped from the final path, so `{group}/{resource}/{name}.yaml`
renders `configmaps/app.yaml`, not `/configmaps/app.yaml`. Example values are for an `apps/v1` Deployment
named `api` in namespace `team-a`:

| Variable | Renders | Example |
|---|---|---|
| `{name}` | resource name | `api` |
| `{namespace}` | the resource's namespace, or the literal `_cluster` for a cluster-scoped resource | `team-a` (a Node → `_cluster`) |
| `{namespace\|fallback}` | the same, but with your own bucket in place of `_cluster` | `_global` |
| `{resource}` | plural resource name | `deployments` |
| `{group}` | API group; **empty** for core resources | `apps` (a ConfigMap → empty) |
| `{groupPath}` | the API group as a path segment; equivalent to `{group}` today (the empty core-group segment is dropped either way) | `apps` |
| `{version}` | API version | `v1` |
| `{apiVersion}` | manifest `apiVersion`: `group/version`, or `version` alone for core | `apps/v1` (a ConfigMap → `v1`) |
| `{kind}` | manifest kind | `Deployment` |
| `{kindLower}` | manifest kind, lower-cased | `deployment` |
| `{scope}` | `namespaced` or `cluster` (a readable label, not a namespace-position value) | `namespaced` |
| `{sensitiveSuffix}` | `.sops.yaml` for a sensitive resource, `.yaml` otherwise | `.yaml` (a Secret → `.sops.yaml`) |
| `{label:key}` | the value of that label on the resource, or `_unlabeled` if it has none; the key may be prefixed (`{label:app.kubernetes.io/instance}`) | `voter` |
| `{label:key\|fallback}` | the same, but with your own bucket in place of `_unlabeled` | `unassigned` |

> **`{namespace}` always renders something.** For a cluster-scoped resource it is the literal
> `_cluster`, so `{namespace}/{resource}/{name}.yaml` files a ClusterRole at
> `_cluster/clusterroles/admin.yaml` and namespaced and cluster-scoped resources stay cleanly
> separated without a second template. `_cluster` is not a legal namespace name (DNS-1123 forbids
> `_`), so it can never collide with a real one. To name that bucket yourself, write
> `{namespace|_global}` (see [Naming the bucket yourself](#naming-the-bucket-yourself-fallback)).
> `{scope}` is a *descriptor* (`cluster`/`namespaced`), not a substitute, so don't use it as the
> folder for cluster resources.

#### Naming files by kind (`{kindLower}`)

`{kindLower}` gives the `deployment-api.yaml` naming convention, which none of the automatic steps
produce:

```yaml
placement:
  default: "{kindLower}-{name}{sensitiveSuffix}"   # deployment-api.yaml, configmap-cache.yaml
```

This template is **not identity-complete**: it has no `{namespace}`, so a Deployment named `api` in
`team-a` and one in `team-b` render the same path and land as two documents in one file. Use it for a
folder that mirrors one namespace. For a folder that mirrors several, keep the namespace in the path,
for example `"{namespace}/{kindLower}-{name}{sensitiveSuffix}"`. A Secret still needs an
identity-complete `byType` route of its own, as it does with any bundling `default`.

It is a variable rather than a `lower` function on purpose: placement templates are not an expression
language.

#### Placing by label (`{label:key}`)

One variable reads the object's metadata rather than its identity. `{label:team}` renders the value
of the resource's `team` label, and the key may be prefixed: `{label:app.kubernetes.io/instance}` is
one variable, not a variable followed by a directory, because the template scanner takes the whole
`{…}` including the `/` inside it. It works in `byType` and in `default` alike, and a template may
read more than one label.

```yaml
placement:
  byType:
    v1/configmaps: "{label:app.kubernetes.io/instance}/configmaps.yaml"
```

Resources sharing a label value bundle into one file, which is usually the point: every ConfigMap
labeled `app.kubernetes.io/instance: voter` lands in `voter/configmaps.yaml`. The commit that
writes them can name the same label: see [Kind, scope, and labels](commit-messages.md#kind-scope-and-labels).

##### Every resource is placed, labeled or not

There is no "not placed" state to design around. A resource that does not carry the label (or
carries it with an empty value, which Kubernetes permits) renders a bucket instead:

| The resource | `{label:team}` | `{label:team\|unassigned}` | `{label:team\|}` |
|---|---|---|---|
| sets `team: payments` | `payments` | `payments` | `payments` |
| does not set `team` | `_unlabeled` | `unassigned` | nothing; the segment collapses |
| sets `team: ""` | `_unlabeled` | `unassigned` | nothing; the segment collapses |

`_unlabeled` is the built-in bucket, and it is chosen the same way `_cluster` is for a
cluster-scoped `{namespace}`: a label value has to start and end alphanumeric, so no real one can
ever be `_unlabeled` and no real resource can land in that bucket by accident.

##### Choosing your own fallback

`{label:key|fallback}` replaces `_unlabeled` with a bucket you name. A fallback is at most 63
characters of `[A-Za-z0-9._-]`, may not be `.` or `..`, and may be empty. Three consequences are
worth knowing before you pick one:

- **A fallback that is a legal label value shares its bucket with resources labeled it.**
  `{label:team|unassigned}` files unlabeled ConfigMaps exactly where `team: unassigned` ones go, and
  nothing afterwards can tell the two apart. When you need them distinguishable, begin the fallback
  with `_`: a label value may not start with one, so `{label:team|_none}` is a bucket only your
  fallback can reach. That is why a fallback is allowed a leading `_` where a label value is not.
- **An empty fallback is a request to render nothing**, and the empty segment is then dropped, so
  unlabeled resources land one directory up: `{label:team|}/{name}.yaml` puts them at `api.yaml`
  while labeled ones go to `payments/api.yaml`. Write it when you want the labeled resources filed
  into folders and the rest left where they are. Only *whole* segments collapse, so inside a file
  name `{label:team|}-{name}.yaml` renders `-api.yaml` rather than `api.yaml`.
- **A fallback can never add a directory or escape `spec.path`**, because the character set has no
  `/` and `..` is refused. A template that tries is rejected by the GitTarget's `Validated`
  condition with `InvalidConfig`, before anything is written; it is not silently repaired.

##### The destination is sticky

Placement is match-first and runs only for a resource with no document in Git yet, so the rendered
path is a snapshot of the label *at the moment the file was created*. Labeling a resource
afterwards does not move it out of `_unlabeled/`, changing the label does not move it to the new
bucket, and removing the label does not move it into one. Nothing reconciles a file's location
against the label it carries today; if you want a resource moved, move it in Git.

##### Labels the operator strips are rejected up front

The writer removes controller bookkeeping (`kustomize.toolkit.fluxcd.io/*`, `kro.run/*`,
`applyset.kubernetes.io/*`) before a document reaches Git, so those values are already gone by the
time placement runs. A template reading one would not merely be unhelpful; every resource of its
type would render the same fallback forever. The `Validated` gate rejects such a template by name
rather than letting it fail silently per resource. `app.kubernetes.io/instance` is deliberately
**not** stripped (it is indistinguishable from the standard recommended label) and stays usable.

##### Two more limits

- **A label is not identity.** Two resources can carry the same one, so `{label:key}` adds
  discrimination to a path but never counts toward the identity-completeness a sensitive route
  requires: keep `{name}` and a scope variable in those templates.
- **`{annotation:key}` does not exist.** A label value is at most 63 path-safe characters; an
  annotation value is unbounded text (`kubectl.kubernetes.io/last-applied-configuration` is a whole
  JSON document), so there is no truncation rule that would not surprise somebody. Nothing else on
  the object is exposed either; the variables above are the whole language.

#### Naming the bucket yourself (`|fallback`)

Two variables can be **absent** for a resource that is otherwise perfectly placeable, and they are
the same problem wearing two hats: `{label:key}` on a resource that does not carry the label, and
`{namespace}` on a cluster-scoped resource, which has no namespace at all. Neither is a refusal.
Each renders a built-in bucket (`_unlabeled` and `_cluster`), and each accepts the same
`|fallback` suffix to let you name that bucket instead:

```yaml
placement:
  default: "{namespace|_global}/{groupPath}/{resource}/{name}{sensitiveSuffix}"
  byType:
    v1/configmaps: "{label:team|unassigned}/configmaps.yaml"
```

The rules a fallback obeys are the same for both, because both render into one path segment: at
most 63 characters of `[A-Za-z0-9._-]`, never `.` or `..`, and an empty fallback (`{label:team|}`,
`{namespace|}`) renders nothing so the segment collapses. Nothing else is off limits. A template
that breaks those rules is rejected by the `Validated` condition with `InvalidConfig` before
anything is written.

Only these two take a fallback, and a `|` written on any other variable is refused with that
explanation rather than accepted as syntax that can never fire. That is not because the others
always have a value: `{groupPath}` and `{group}` render empty for a core resource. It is because an
empty group is not a resource missing something, it is a resource whose identity has no group
segment, so the segment collapses and the built-in path is what it was always meant to be. Only
`{namespace}` and `{label:key}` have an absence worth naming a bucket for.

##### A fallback that names a real namespace

Nothing stops you writing `{namespace|team-a}`, and it does what it says: cluster-scoped resources
land in `team-a/`, alongside whatever namespace `team-a` writes there. That is a bundling choice,
not a collision, and it is allowed for the same reason bundling is.

It cannot silently merge two resources. Scope is a property of the **type**, not the object, so two
resources rendering the same `{groupPath}/{resource}` are either both cluster-scoped or both
namespaced, and the fallback only ever fires for the first kind. Under
`{namespace|team-a}/{groupPath}/{resource}/{name}.yaml` a ClusterRole named `admin` goes to
`team-a/rbac.authorization.k8s.io/clusterroles/admin.yaml`, and no namespaced resource can render
that path, because no namespaced resource is a ClusterRole.

What a real-namespace fallback does do is put cluster-scoped resources in a folder a human reads as
a namespace. If you would rather they stayed unmistakable, begin the fallback with `_`
(`{namespace|_global}`): no namespace may contain one, so the bucket is yours alone. That is a
readability preference, not a safety rule.

The one case worth thinking about is a template that drops the type variables (a bundle such as
`{namespace|team-a}/all.yaml`), where cluster-scoped resources join the file namespace `team-a`
writes. Documents keep their own identity inside a bundle, and sensitive resources are never
allowed into a shared file, so the fence that matters still holds. For Secrets specifically, a
bundling `default` is rejected outright unless Secrets have their own identity-complete `byType`
route; picking a route that keeps them apart is yours to get right.

#### Sensitivity is a write-safety rule, not a placement setting

Sensitivity is enforced by the operator whatever path is chosen. A `Secret` (and any operator-configured
sensitive type) is always written encrypted, is never appended to an existing file, and is never
co-mingled with a plaintext document. Two consequences for your templates:

- A `byType` route for a sensitive type must be **identity-complete**: it must contain `{name}` and a
  scope such as `{namespace}`, so two of them can never collide onto one file.
- A **bundling `default`** that is not identity-complete (e.g. `"all.yaml"`) is rejected unless every
  sensitive type has its own identity-complete `byType` entry, so a Secret can never fall through into a
  shared file. If an operator-configured sensitive type still reaches such a path at write time, that
  resource is **skipped fail-safe** (logged and counted in the resync summary as `placementSkipped`)
  rather than written unsafely. It is not surfaced as a dedicated status condition today.

### Choosing between the two layout fields

`spec.serializeNamespace` and `spec.placement.useKustomize` answer two independent questions. Both
are optional; a target that sets neither infers the answer per document. Start here, then read the
section for whichever you set.

**Question 1: where does the namespace of a mirrored object live?**

| You want | Set | What the folder looks like |
|---|---|---|
| Each document to say which namespace it is in. Someone can `kubectl apply -f` the folder and land everything where it came from | `serializeNamespace: true` | every namespaced document carries `metadata.namespace` |
| The folder to be installable into a namespace chosen at install time, by a Flux `targetNamespace`, an Argo `destination.namespace`, or a `kustomization.yaml` you wrote | `serializeNamespace: false` | no document carries `metadata.namespace`, and nothing in the folder pins one |
| Neither claim, because the folder is a tree of nested kustomize roots that each supply their own namespace | leave it **unset** | each document is resolved against the root governing its own path |

**Question 2: does the operator maintain this folder's `kustomization.yaml`?**

| You want | Set | What the operator does |
|---|---|---|
| The folder to become a kustomize folder, including from empty | `placement.useKustomize: true` | creates `kustomization.yaml` at `spec.path` if there is none, adopting the files already there, and registers every new document in it |
| Plain files, or a `kustomization.yaml` only you create | leave it **unset** | writes the document; registers it in a root that already governs it, and creates nothing |

Registering a new file with a kustomization that **already** governs it happens either way. That is
not a setting: a file no `resources:` list names is a file kustomize never builds.

**The three combinations in practice:**

| Shape | Spec | Who supplies the namespace |
|---|---|---|
| A mirror you can apply back | `serializeNamespace: true`, no `useKustomize` | the documents |
| A portable artifact | `serializeNamespace: false` **+** `useKustomize: true` | whatever installs the folder |
| An existing kustomize repository | leave both unset | the `kustomization.yaml` files already there |

One combination to avoid: `serializeNamespace: false` on a folder nothing installs. The documents
carry no namespace and nothing supplies one, so `kubectl apply -f` lands them all in `default`.
Nothing can detect that for you, because the installer lives outside the repository. See
[why nothing checks the supplier](#whether-documents-carry-their-namespace-specserializenamespace).

### Whether documents carry their namespace (`spec.serializeNamespace`)

A path decides where a file sits; it cannot decide what is inside it. `spec.serializeNamespace`
does: whether the committed document carries its own `metadata.namespace`.

```yaml
spec:
  path: apps/checkout
  serializeNamespace: false
```

| Value | What is written |
|---|---|
| omitted (default) | inferred per document, which is today's behavior |
| `true` | every namespaced document carries `metadata.namespace` |
| `false` | no document carries it; something outside the folder supplies it |

It governs **every** write the target makes, not only the first one. That is why it sits at the top
level of the spec rather than inside `spec.placement`, which decides where *new* documents go and
never moves one already written. It applies to **namespaced resources only**: a `ClusterRole` has no
namespace, so the field is ignored for it rather than being an error.

**Omitted is not the same as `false`, and it is the right answer more often than either.** Inference
omits `metadata.namespace` only when the kustomization governing that document's path sets
`namespace:` to *this* resource's own namespace, and writes it explicitly otherwise, because
omitting it anywhere else would hand the document to a different namespace than the object it
mirrors. Leave the field unset for a folder that is legitimately non-uniform, such as a tree of
nested kustomize roots each supplying its own namespace: inference resolves every document against
the root governing its own path, which no single folder-wide value can do.

The two explicit values are for the two shapes people declare:

- **`true` for a flat folder applied directly.** Nothing downstream supplies a namespace, so a
  document without one is ambiguous. It also keeps the document portable: it means the same thing
  pasted anywhere.
- **`false` for a folder whose namespace comes from outside it**: a Flux `Kustomization`'s
  `spec.targetNamespace`, an Argo CD `Application`'s `spec.destination.namespace`, or a
  hand-written `kustomization.yaml` in the folder that sets `namespace:`. A root the operator
  creates never sets one.

**Nothing checks that the supplier exists, and nothing can.** For a raw namespace-free folder the
supplier lives in the cluster that *consumes* the repository, and there may be more than one of
them: two deployers may land the same folder in two different namespaces, both correctly. That
portability is the point of the shape, so a rule demanding proof inside the folder would report a
fault against a folder doing exactly what it was built for.

**One thing is checked, and it refuses.** An explicit `serializeNamespace: false` admits exactly
**one source namespace**. A namespace-free document takes its namespace from a single supplier, so
two source namespaces reaching the folder contradict the setting itself, and the failure is silent:
`shop/config` and `billing/config` both resolve to one `config.yaml` whose bytes name no namespace,
so they are not two documents that collide but one document two live objects take turns
overwriting. A second `WatchRule` bringing another source namespace to such a target is refused with
`GitPathAccepted=False`, reason `MultipleSourceNamespaces`. A rule naming `sourceNamespace: "*"` is
refused too, without enumerating anything, because a wildcard cannot be shown to be one namespace.
The set counted is the target's own namespace plus the explicit `rules[].sourceNamespace` of every
`WatchRule` pointing at it. It is unrelated to the `KubeProvider` grants, which answer who may
write here rather than what the folder means.

Inference is never fenced this way. A folder that is truly multi-namespace and namespace-free is
what leaving the field **unset** is for.

**What the render check does with the namespace.** Every write is compared against the live object
it mirrors, and `metadata.namespace` is the one field allowed not to match. It is ignored only when
the folder renders the document with no namespace at all, which is `serializeNamespace: false` with
nothing in the folder supplying one. A `kustomization.yaml` that declares `namespace:` makes a
concrete claim, so it is still compared: a root declaring `namespace: shop` rendering an object that
lives in `billing` is a relocation, and it is refused whatever `serializeNamespace` says.

### Keeping the folder a kustomize folder (`spec.placement.useKustomize`)

```yaml
spec:
  path: apps/checkout
  serializeNamespace: false
  placement:
    useKustomize: true
```

It controls exactly one thing: **what happens when no `kustomization.yaml` governs the path a new
document lands at.**

| | A kustomization governs the path | Nothing governs the path |
|---|---|---|
| omitted / `false` (default) | the new file joins its `resources:` list | the file is written and nothing else is touched |
| `true` | the new file joins its `resources:` list | a `kustomization.yaml` is **created** at `spec.path`, and the file joins it in the same commit |

**Registering a new file with the kustomization that already governs it is not what this flag
controls.** It happens in both rows, because a file no kustomization lists is a file kustomize never
builds. The flag is only about the empty case, which is what makes an empty repository
bootstrappable.

**A created root adopts the whole folder.** Its `resources:` lists every managed document
already in the folder alongside the new one, at the paths those files already have. Nothing is
moved, rewritten or re-encoded. A root naming only the new file would leave every other file
sitting in Git and out of every render: the moment a consumer ran `kustomize build` against the
folder, they would stop being applied, with nothing to show what happened.

**A folder that already has a render root never gains a second one, and a document it would not
render is refused.** If a `byType` or `default` template puts the new document somewhere the
existing root does not govern, the placement is refused with `GitPathAccepted=False`, reason
`UnrenderedPlacement`. Two render roots in one target's folder is the ambiguous case, and an
ambiguous folder stops accepting new documents entirely; committing the file unregistered instead
would leave a document sitting in Git looking mirrored while nothing applies it. The fix is a
template that keeps its documents inside the root, or a target pointed at the folder that root
governs. Without `useKustomize` nothing changes: a target that made no claim about kustomize keeps
its current behavior.

**The created root carries no `namespace:`.** It is an `apiVersion`, a `kind` and the `resources:`
list, and nothing else. `serializeNamespace: false` says the artifact does not encode its deployment
namespace, and creating a root must not re-encode it one file up where an installer cannot override
it, so the namespace still comes from the documents (`serializeNamespace` unset or `true`) or from
whatever installs the folder. The reasoning, and the four other answers that were considered, is in
[`design/created-root-namespace.md`](design/created-root-namespace.md). That last line is the point of the pairing with
[`serializeNamespace: false`](#whether-documents-carry-their-namespace-specserializenamespace): on
an empty folder there is nothing to infer from, and the operator owns the file the omission depends
on, so the omission is provable rather than trusted.

The new document is placed **beside the root**, exactly as it would be beside a root that was
already there, unless a `byType` or `default` template says otherwise. A declared template still
decides the path; the created root lists the document wherever the template put it.

The name says less than the field does, so one reading is worth ruling out: `useKustomize: false`
does not mean "leave kustomize alone". If a folder's `kustomization.yaml` must never be touched, do
not point a `GitTarget` at that folder. A kustomization **above** `spec.path` is never edited (the
ancestor walk stops at the write jail), so rooting the target lower is the way to say it.

### Additional sensitive resources

Core Kubernetes `Secret` resources always use the encrypted Git write path. For a Secret-shaped
custom resource such as CozyStack `tenantsecrets`, add the resource type to the controller startup
values:

```yaml
controllerManager:
  additionalSensitiveResources:
    - core.cozystack.io/tenantsecrets
```

Entries are `resource` for the core API group or `group/resource` for grouped APIs. The match ignores
API version, so a served CRD version change does not change the sensitive classification. The custom
resource still needs a `GitTarget` with `spec.encryption` configured before Git writes can succeed.

### Kustomize support in the target path

A target path may contain `kustomization.yaml` files. The operator retains them as build directives
(it never sweeps them) and understands a deliberately small, round-trippable subset:

- **`namespace:` + `resources:`/`bases`** (local files and directory bases): a namespace-less
  resource file inherits its namespace from the kustomization that references it, and
  `metadata.namespace` is kept out of the file on write.
- **`images:` and `replicas:` overrides**: a live change *produced by* an override entry (an image
  tag, name, or digest pinned by `images:`, or a replica count pinned by `replicas:` (including
  `kubectl scale`) is written back **to that entry**, preserving comments, and the source manifest
  keeps its bytes. Only fields the entry already declares are updated; the operator never adds or
  removes entries. Note that one entry is a shared knob, exactly as in kustomize itself: updating it
  affects every resource in the build whose image matches.
- **`configMapGenerator` with `disableNameSuffixHash: true`** (in `generatorOptions:` or on the
  entry): a live change to the generated ConfigMap is written into the generator's inputs. A
  `files:` key's file gets the live value as its content; an `envs:` key's line and a `literals:`
  key's `KEY=value` item are edited in place, and a removed key's line or item is removed. A new key
  is appended to the entry's `envs:` file when it has exactly one, and otherwise to its `literals:`
  when it has some. Deleting the ConfigMap removes the entry from `configMapGenerator:`, and the
  input files no other entry, resource or patch reads; under `spec.prune.mode: Always` a resync
  sweeps the entry of a generated ConfigMap the cluster no longer has, as it sweeps a document. A
  change with no single input to land in is refused per object: `binaryData`, a label or annotation,
  a new key with several `envs:` files or only `files:` sources, a removed `files:` key, and a
  generator in a read-only base. The input files are build inputs, never foreign content.

Two kustomize shapes beyond that subset are **supported without being authored**:

- A **path-based strategic-merge `patches:` entry** is tolerated as read-only build context: the
  folder is accepted and what it renders is mirrored, but nothing is ever written into a patch
  file, and an edit to a field the patch *owns* is refused per object (not per folder).
- An **overlay that reads a base outside its own folder** (`resources: [../../base]`) is rendered
  by reading that base as read-only context; writes stay inside `spec.path`, and an image/replica
  edit lands on the overlay's own entry.

Everything else outside the modeled subset **refuses the whole target path before anything is
written**: inline or JSON6902 patches and the deprecated `patchesStrategicMerge`/`patchesJson6902`
spellings, `secretGenerator`, a `configMapGenerator` whose name hash is not disabled (the message
says to set `disableNameSuffixHash: true`) or whose `behavior:` is `merge` or `replace`,
`components`, Helm fields, `replacements`, `transformers`,
`namePrefix`/`nameSuffix`, remote bases, and `images:`/`replicas:` values that do not parse (those
would fail `kustomize build` too). A refusal is loud: the target reports `GitPathAccepted=False`,
`Stalled=True`, and `Ready=False` with reason `UnsupportedContent` until the path is cleaned up.

Two situations fall back to plain in-place editing of the source manifest instead of refusing:
a resource file reachable from more than one render root with differing override chains
(ambiguous, because the operator will not guess which chain governs), and a live change an entry cannot
express (for example a removed digest, or two containers demanding different values from one
entry). These fallbacks are recorded as store diagnostics, visible in the analyzer CLI and, for
the running operator, in the logs at debug verbosity (`manifest store diagnostic`).

For design details and the exact boundary, see
[design/support-boundary/finished/images-and-replicas-edit-through.md](design/support-boundary/finished/images-and-replicas-edit-through.md).

## `WatchRule`

`WatchRule` is the **namespaced** watcher: it selects namespaced resources on its `GitTarget`'s
source cluster and writes them to that `GitTarget`. Scope is carried by the rule kind: a `WatchRule`
never selects cluster-scoped types, and a `ClusterWatchRule` never selects namespaced ones.

Status uses `ResourcesResolved` for selector resolution, `StreamsRunning` for source-watch readiness,
`GitTargetReady` for the referenced target's write readiness, and `SourceNamespaceAuthorized` for the
source-namespace gate below. A rule can have `StreamsRunning=True` and still remain `Ready=False`
when its GitTarget reports `GitPathAccepted=False`.

The important fields are:

- `spec.gitTargetRef.name`: target to write to
- `spec.rules`: one or more resource-match rules
- `spec.rules[].sourceNamespace`: the source-cluster namespace that item watches; omitted means the
  rule's own namespace

**The `kube-root-ca.crt` ConfigMap is never mirrored**, whatever a rule selects. The cluster writes
it into every namespace, it holds that one cluster's CA, and the cluster overwrites any other value,
so a copy in Git can be neither edited nor restored. A copy already in Git is treated like any
document the cluster does not hold: prune mode `Always` removes it on the next resync, and under
`OnEvent` you delete it yourself. Each snapshot that leaves it out logs `left out cluster-generated
objects on purpose`, and `watch_events_total{outcome="cluster_generated"}` counts its live events.

### Watching a different source namespace

Set `spec.rules[].sourceNamespace` to mirror a namespace other than the one the `WatchRule` lives in
which is the case a shared config plane needs, where a tenant's configuration namespace and their source
namespace cannot share a name. It sits on the rule item, beside the resource selector it applies to,
so one `WatchRule` can follow different resource types in different namespaces.

| `rules[].sourceNamespace` | Meaning |
|---|---|
| omitted | the `WatchRule`'s own namespace |
| an exact name | one source namespace |
| `"*"` | every namespace the source credential can read, as one cluster-wide watch |

Every item, the rule's own namespace included, is authorized by the grants of the `GitTarget`'s
`KubeProvider`:

1. a grant in its `spec.allowedGitTargets` admits the `GitTarget`'s namespace; and
2. the union of those grants' `sourceNamespaces` holds the namespace the item asks for: its name,
   `"{consumer.namespace}"` for the `GitTarget`'s own (which is the `WatchRule`'s), or `"*"`. An item
   asking for `"*"` needs `"*"` in at least one of them; under a union without it the item is
   refused, never narrowed to the namespaces listed.

There is no third condition on the `GitTarget`. What a target may read from its source cluster is
bounded by that cluster's RBAC for the provider's credential: a request the grants permit still
fails with a clean 403 if the credential cannot read the namespace.

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata:
  name: repo-config
  namespace: tenant-acme
spec:
  gitTargetRef:
    name: acme
  rules:
    - resources: [configmaps]        # omitted → tenant-acme, this rule's own namespace
    - resources: [secrets]
      sourceNamespace: repo-config   # one admitted source namespace
    - resources: [deployments]
      sourceNamespace: "*"           # every namespace the credential can read
```

`"*"` is one cluster-wide list and one cluster-wide watch per matched type, not one of each per
namespace. Connection count is bounded per matched type, but object count, traffic, and memory
still grow with the cluster. Access is bounded by the source credential's RBAC
and by nothing else, which is why it is refused outright unless a grant gives `sourceNamespaces: ["*"]`.

A `"*"` item and a named-namespace item for the same type **overlap**, so one `GitTarget` cannot
hold both: the newer rule is refused (see [overlapping collections](#overlapping-collections)). Keep
the wider scope, or send the narrower one to its own `GitTarget`.

The outcome for all items is aggregated into one `SourceNamespaceAuthorized` condition, also shown by
`kubectl get watchrules -o wide`. A **denied** explicit name refuses the whole `WatchRule`
(`Ready=False`, `Stalled=True`, no streams) rather than silently trimming that item and mirroring
part of what you asked for; the message names the failing item by index and by what it selects.
Authorization is re-evaluated whenever the provider's grants or the namespace's labels change, so
narrowing a grant revokes a running rule rather than only affecting new ones.

This changes only which namespace is **watched**. Git placement always follows each mirrored
object's own namespace, so the rule above writes secrets under `repo-config/…`, not `tenant-acme/…`.

A namespace scope cannot partition **cluster-scoped** objects, which have no namespace. A
`ClusterWatchRule` mirrors only the types the grant's `clusterResources` name (see
[cluster-scoped resources](#cluster-scoped-resources)), and of those, every object its source
credential can read.

Each entry in `spec.rules` is a logical OR. A resource matching any rule is watched. A rule selects
whole resource collections: every selected collection is observed through creates, updates, and
deletions, and the `GitTarget`'s [deletion policy](#deletion-policy-specprunemode) decides whether an
observed removal deletes the Git document. There is no per-rule operation filter. The rule fields are:

- `apiGroups`: `""` for the core group, `*` for all groups, or omitted to resolve the named resource
  across the served API surface. A name served by more than one group is ambiguous when the group is
  omitted, and is watched in none of them: name the group.
- `apiVersions`: a served version such as `v1`; omitted means the preferred served version.
- `resources`: plural resource names such as `configmaps`, `secrets`, or `*`.

Subresources such as `deployments/scale` are not valid rule resources. GitOps Reverser mirrors
top-level resources; selected subresource effects are handled separately by the controller.

Example:

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata:
  name: example-watchrule
  namespace: default
spec:
  gitTargetRef:
    name: example-target
  rules:
    - apiGroups: [""]
      apiVersions: ["v1"]
      resources: ["configmaps", "secrets"]
```

Use `WatchRule` for every **namespaced** resource, whether or not it lives in the `GitTarget`'s own
namespace.

### Selecting objects by label (`rules[].objectSelector`)

`spec.rules[].objectSelector` is a standard Kubernetes label selector (`matchLabels` and
`matchExpressions`). It narrows a rule item to the objects whose labels match, on a `WatchRule` and
on a `ClusterWatchRule`:

```yaml
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    objectSelector:
      matchLabels:
        internal.cozystack.io/tenantresource: "true"
```

**The cluster is the source of truth, and the mirror follows the selection.** The API server applies
the selector to every LIST and WATCH the item opens, with list/watch semantics. An object that gains a
matching label enters the mirror as a create. An object that loses one leaves the mirror exactly as a
deleted object does. The operator does no label matching of its own, and never asks whether an object
that left was deleted or relabeled.

What a removal does to Git is the target's [deletion policy](#deletion-policy-specprunemode):

| Mode | Object deleted or no longer matching | Absent from a complete snapshot |
|---|---|---|
| `Never` | kept | kept |
| `OnEvent` (default) | removed | kept |
| `Always` | removed | removed |

**Under `Always`, a selected mirror owns every document of its type in its namespace scope,** not
only the ones the selector matched. If Git holds Secrets `a` and `b` in `tenant-a` and the selector
returns only `a`, the next complete snapshot removes `b`, even if `b` never matched. Labels in Git,
and whether a document was ever mirrored, do not protect it. To keep documents the selector does not
return, use `OnEvent` or `Never`, or give them a different target path. Under the retaining modes,
each such document counts as [retained](#seeing-what-was-kept): it is absent from the selected
mirror, whether or not the object still exists in the cluster.

Consequences to plan for:

- **Narrowing a selector is a removal.** Editing `team in (a,b)` to `team=a` starts a new
  collection and a complete snapshot, and under `Always` every document it does not return goes.
  Widening adds the newly selected objects without waiting for them to change. A mistaken selector
  and an intentionally smaller mirror look the same to the operator.
- **Removing a document can delete the live object.** When a GitOps tool such as Flux or Argo CD
  applies this folder with pruning on, a document this mirror removes is an object that tool deletes.
- **Enable `Always` only once the cluster is authoritative.** A document committed to Git that has
  not yet been installed, or that another tool has yet to apply, is absent from the snapshot and is
  removed.
- **Selectors match the objects stored in the cluster.** A selector may use labels that the operator strips
  before writing, such as `kustomize.toolkit.fluxcd.io/name`; the labels do not need to survive in
  Git.

The field is the Kubernetes `LabelSelector` type, so the CRD schema checks only its shape. The rule
compiler checks the rest (operators, values, and label syntax, exactly as Kubernetes parses a
selector) and refuses the whole rule before it opens any watch, with `ResourcesResolved=False`,
reason `InvalidObjectSelector`. A valid selector that matches nothing is a valid, empty collection.

**Kustomize layouts.** A removal from a supported Kustomize folder is the same edit whether a
watch event or a snapshot caused it. A document is removed from its own file, keeping the other
documents in that file, and a file that empties loses its `resources:` entry, so the root still
builds. An object the overlay inherits from a read-only base is removed with a `$patch: delete`
the operator writes in the overlay and lists under `patches:`. The operator owns that file: its
first line says so, and when the object is back in the selection the operator removes the file
and its entry. The file's exact contents are the ownership, so an edited patch, or an unrelated
file at the same path, refuses the write (`GitPathAccepted=False`, reason `WriteBoundaryRefused`)
rather than being overwritten or kept. Every such write is re-rendered before commit to prove the
object left, or came back, and that nothing else in the render changed. The
[Kustomize support boundary](#kustomize-support-in-the-target-path) still decides which folders are
writable at all.

**Attribution of a removal.** With [audit attribution](attribution-setup-guide.md) on, an object
leaving a selected collection is attributed only to the write that produced that removal: the label
change. A deletion from a selected collection never carries that evidence, so its removal commit is
authored unresolved; a deletion that waited on a finalizer was already attributed when it was
requested. The document is removed either way. See the
[attribution contract](spec/attribution.md#four-rules-that-are-easy-to-miss).

### Overlapping collections

A `GitTarget` watches each object through one collection. Two items on one `GitTarget` overlap
when they select the same type in the same namespace, or when one of them selects all namespaces.
Only an exact duplicate is allowed: the same type, namespace scope, and selector, written in any
equivalent form (`matchLabels: {team: a}` and
`matchExpressions: [{key: team, operator: In, values: [a]}]` are one selector). It shares one stream.
Any other overlap is refused, whether the selectors are equal or differ: the older rule, then the
rule with the lower name, keeps its collections, and the newer rule is refused as a whole with
`ResourcesResolved=False`, reason `CollectionOverlap`. The message names both rules and both
scopes. A rule whose own items overlap is refused whatever its age. Different namespaces, and
different `GitTarget`s, never overlap. To select several values, use one set-based selector such as
`team in (a,b)`. The check re-runs when rules change or the source cluster starts serving a type. A
collection that leaves the plan stops before an overlapping one that replaces it starts.

## `ClusterWatchRule`

`ClusterWatchRule` is the **cluster-scoped** variant. Use it for cluster-scoped resources such as
`nodes`, `clusterroles`, or CRDs. It has no scope choice and no source-namespace selection: to mirror
namespaced resources across namespaces, use a `WatchRule` with
[`rules[].sourceNamespace`](#watching-a-different-source-namespace).

Because it is cluster-scoped, its `gitTargetRef` must include the namespace of the referenced
`GitTarget`. That namespace must be admitted by the target's `KubeProvider`, and the grants that admit
it must name every type the rule asks for in `clusterResources` (see
[cluster-scoped resources](#cluster-scoped-resources)). The example needs
`clusterResources: [clusterroles.rbac.authorization.k8s.io, clusterrolebindings.rbac.authorization.k8s.io]`,
or `["*"]`.

Example:

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: ClusterWatchRule
metadata:
  name: cluster-rbac
spec:
  gitTargetRef:
    name: example-target
    namespace: default
  rules:
    - apiGroups: ["rbac.authorization.k8s.io"]
      apiVersions: ["v1"]
      resources: ["clusterroles", "clusterrolebindings"]
```

Cluster-scoped objects have no namespace, so no namespace scope bounds a `ClusterWatchRule`: the
types in `clusterResources` do, and for those types its source credential's Kubernetes RBAC. Only a
cluster admin can create one, and no default grant, the chart's included, gives `clusterResources`.
Use it sparingly.

## `CommitRequest`

`CommitRequest` is a one-shot "save" signal for a `GitTarget`. It does not create or change watch
rules. It attaches to its author's commit window, gives that window its message, and decides when
the window closes, instead of the target's timers.

It is the one kind of GitOps Reverser that can also live on a source cluster. A request for a
`GitTarget` that mirrors the cluster GitOps Reverser runs in is created there; a request for a
`GitTarget` that mirrors another cluster is created on that cluster, next to the edits it saves. See
[where a request is created](#where-a-request-is-created).

The important fields are:

- `spec.gitTargetRef`: the target whose window to attach to, by `name`, in the request's own
  namespace unless `namespace` names another; see [who may save](#who-may-save-specallowedcommitrequests)
- `spec.message`: optional literal commit message, preserved verbatim
- `spec.window`: which window to attach to and when it closes; see
  [the request window](#the-request-window-specwindow)
- `spec.whenNothingToCommit`: what to do when the request ends with nothing to commit; see
  [when there is nothing to commit](#when-there-is-nothing-to-commit)
- `spec.catchUp`: whether the applier is asked to apply the save at once; see
  [applying a save at once](#applying-a-save-at-once)

Example:

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: CommitRequest
metadata:
  name: save-now
  namespace: default
spec:
  gitTargetRef:
    name: example-target
  message: |-
    fix(api): correct the service port

    Route traffic to the port exposed by the API container.
  window:
    attach: CurrentOrNext
    attachTimeout: "5s"
    maxDuration: "2s"
```

The entire spec is immutable. Create a new `CommitRequest` for each save attempt.

A present message accepts 1–1024 Unicode characters, including newline. All other ASCII control
characters, including tab, carriage return, and DEL, are rejected, as is whitespace-only text.
Accepted surrounding spaces are preserved. Braces such as `{{.Author}}` remain literal; omission
uses [the live template](commit-messages.md). To frame a save message with what was saved,
configure [`requestTemplate`](commit-messages.md#framing-a-save-message) on the GitTarget. The request is still never
parsed as a template. A rejected request leaves automatic mirroring
available. The submitter chooses any semantic prefix; free-form messages are accepted.

### Where a request is created

The `GitTarget` a request names decides where it belongs, through the `KubeProvider` its
`spec.kubeProviderRef` names:

| The `GitTarget`'s `KubeProvider` | Create the request on | `gitTargetRef.namespace` |
| --- | --- | --- |
| `inCluster` | the cluster GitOps Reverser runs in | optional: omitted, the request's own namespace |
| a kubeconfig, with `spec.commitRequests` set to a `Reconcile` value | the cluster that kubeconfig connects to | required: a namespace of the cluster GitOps Reverser runs in |

- **A request on a source cluster must name the `GitTarget`'s namespace.** That namespace belongs to
  the cluster GitOps Reverser runs in, so there is nothing on the source cluster to default to. A
  request without it is never picked up there, and gets no status.
- **A request on the cluster GitOps Reverser runs in, for a `GitTarget` that mirrors another
  cluster, is refused** with reason `NotAllowed`, when that `KubeProvider` serves no
  `CommitRequest`s: its author would come from this cluster's audit and its edits' authors from the
  other one's. Create it on the other cluster instead. When that `KubeProvider` does serve them, the
  request is left alone, since the kubeconfig may point back at this cluster and then its own watch
  serves the request. If it points elsewhere, nothing serves the request and it gets no status.
- **A request on a source cluster, for a `GitTarget` that mirrors another cluster, is refused** with
  reason `NotAllowed`, and the message names both clusters, when nothing can serve it from there:
  the `GitTarget` mirrors the cluster GitOps Reverser runs in, or its `KubeProvider` serves no
  `CommitRequest`s. Create it on the cluster the `GitTarget` mirrors. A refused request finishes,
  so a `KubeProvider` with `ReconcileAndDeleteFinished` deletes it after the TTL. A kubeconfig that
  points back at the cluster GitOps Reverser runs in sees that cluster's own requests, and leaves
  them to it. A request whose `GitTarget`'s `KubeProvider` does serve `CommitRequest`s is left
  alone, since that `KubeProvider` may connect to the same cluster under another identity.

Only the path the `GitTarget`'s `KubeProvider` picks writes a request's progress, so several
`KubeProvider`s that watch one cluster never fight over a request. Any of them may report
`GitTargetNotFound` for a request whose `GitTarget` does not exist once `attachTimeout` has passed.

### The request window (`spec.window`)

A request never opens a window; only writes do. It waits for one, attaches to at most one, and
replaces that window's timers with its own, shorter or longer:

| Field | Meaning | Default |
|---|---|---|
| `attach` | `CurrentOrNext` attaches to the author's window if one is open when the worker registers the request, and otherwise to the next one a write opens. `Next` first closes the author's open window, under the message it already carries, and attaches to the next one | `CurrentOrNext` |
| `attachTimeout` | stop waiting for a window this long after the worker registers the request | `5s` |
| `idleTimeout` | close the attached window after this much silence; every write restarts it | no idle close |
| `maxDuration` | close the attached window this long after the attach, however much keeps arriving | `2s` |

The defaults are a fixed two-second cutoff after the attach, so an explicit save finishes promptly.
For a save whose writes trickle in, ask for rolling collection, which `maxDuration` still ends:
`idleTimeout: "1s"` with `maxDuration: "10s"`. Durations are at most `5m`, `idleTimeout` may not
exceed `maxDuration`, and `attach: Next` needs a positive `attachTimeout`.

Each value means one thing: `attachTimeout: "0s"` attaches to a window already open, or gives up at
once, and still collects for the full `maxDuration`; `maxDuration: "0s"` finalizes right after the
attach; `idleTimeout: "0s"` closes on the first silence. A request whose `attachTimeout` has run out
never takes a later window: the write that arrives afterward commits on its own, without the
request's message. Author or target changes, the memory limit, a drain before a resync, and
shutdown still close an attached window early, and its message goes with it.

**Pick `attach` by when the request is created.** A save made *after* its writes, like the
[commit-window example](demo/commit-window.md), wants `CurrentOrNext`. A save created *before* its
writes wants `Next`, so it does not sweep up the author's earlier, unrelated edits. `Next`
separates work the worker already collected from work that reaches it afterward. It cannot prove a
write was made after the request: a write still held for its audit fact arrives later and lands in
the next window.

Competing requests from one author are not queued. A window carries one message, so while an
earlier request of the same author on the same `GitTarget` waits for a window or collects one, a
later request is refused at once with [`SaveInProgress`](#its-status). A `Next` request is not
refused by one that is collecting: it closes that window under the earlier request's message, then
waits for its own. A re-sent request is recognized by its identity and restarts nothing: its
deadlines and, for `Next`, its one close are fixed at first registration.

#### Sizing the request window

`attachTimeout` counts from when GitOps Reverser picks the request up, which is after the request's
own audit event arrived. It covers the gap between that and the request's first write reaching the
branch worker. A write is held in the watch path until its audit fact arrives, so the floor is the
spread between the request's audit event and its writes'. The default of `"5s"` clears that on a
loaded or distant cluster; a save created before its writes needs the time to its first write on
top.

The floor assumes the stream is not busy. A watch stream attributes its events in order, so a save's
edit can also queue behind an earlier event of the same type that is still waiting for its fact, for
up to `--author-attribution-grace` each. That is rare when the audit policy covers the type, and the
[suggested alert](interpreting-metrics.md#suggested-alerts) on `tier="absent"` says when it is not.
Widening `attachTimeout` hides that delay rather than removing it.

A request is registered with the branch worker only once its own audit event has named its author,
and its edit is attributed the same way, so a request created just before its edit does not always
reach the worker first. On a `GitTarget` whose windows close on the first silence (`idleTimeout:
"0s"`), such an edit can commit alone before the request is there to attach to it. Make the edit
once the request is registered, when its `Reconciling` reason is `WaitingForWindow` (see
[its status](#its-status)), or give the `GitTarget` an `idleTimeout` that covers the gap.
`Ready=Unknown` alone is too early: it starts at reason `Saving`, before the registration.

`maxDuration`, and `idleTimeout` when set, cover the span of the save's own writes after the
attach. The worst case from registration to the finalize is `attachTimeout` plus `maxDuration`; the
push follows at once, without the push cooldown, with any retries a remote that refuses it needs.

The two directions are not symmetric. Overshooting costs a few seconds of latency on the commit;
undershooting `attachTimeout` resolves the request `Ready=True` with reason `NoWindow` while the
edit commits seconds later under the target's `liveTemplate`, which reads as a save button that did
nothing.

A request cannot rename a finalized commit, including a local commit waiting for push. Between the
finalize and the push the request is still in flight, and GitOps Reverser keeps re-sending its
attach until it reads an outcome; the worker recognizes the re-send as the same request, so a
request never resolves `NoWindow` while its commit waits out the push cooldown. Applying resources
and a request together gives no ordering guarantee between controllers and watch streams. See the
[request contract](spec/commitrequest-design.md).

#### Saving a folder with `kubectl apply`

To save the objects of a folder in one commit with your message, put a `CommitRequest` in the
folder, next to them, and give it a new name for every apply:

```yaml
# save-2026-10-28-checkout-limits.yaml
apiVersion: configbutler.ai/v1alpha3
kind: CommitRequest
metadata:
  name: save-2026-10-28-checkout-limits
spec:
  gitTargetRef:
    name: example-target
  message: "feat: raise the checkout limits"
```

`kubectl apply -f folder/` then commits the folder's changes, authored by you, with the request's
message. kubectl applies the files in name order, so the request can land before or after the
objects: with `attach: CurrentOrNext` the request attaches to the window the objects already opened,
or waits for the one they open. Do not use `attach: Next` here: it would close the objects' window
under the generated message, then wait for a write that never comes. Another unclaimed edit of yours
on the same `GitTarget`, still in its window, shares the commit.

It is one commit only when the window holds every write, and a window is not a transaction. Two
things split it:

- **Time.** With the defaults the window closes 2 seconds after the attach. Writes reach the branch
  worker once their audit facts have named their author, so on a large folder, a loaded API server or
  a source cluster with slow audit delivery, the last writes can arrive later than kubectl sent them.
  A write after the close commits on its own, under the generated message. For such a folder, give
  the request a longer window, `spec.window.maxDuration: "10s"` for example, or rolling collection
  (see [the request window](#the-request-window-specwindow)); the commit lands that much later.
- **Other writes on the branch.** A branch worker holds one window at a time, so a write by another
  author, or for another `GitTarget` on the same branch, closes yours early, with the request's
  message, and your later writes commit under the generated message.

The request then ends `Committed` with the writes its window held.

The name is what makes the next apply a save. Applying the same folder again does this:

- **Nothing changed:** kubectl sends no write for an unchanged object, so there is nothing to save,
  and the finished request keeps its outcome. If the request was already deleted (see
  [finished requests](#finished-requests-are-deleted-after-48-hours)), the apply creates it again,
  and it ends `NoWindow`.
- **Objects changed, same request:** the request is unchanged and finished, so it saves nothing.
  The edits commit on the `GitTarget`'s own timers, under the generated message.
- **Message edited, same name:** the API server refuses the request, because its spec is
  immutable, and kubectl exits with an error after applying the other files. Their edits commit
  under the generated message.

The request itself is never mirrored, even when a `WatchRule` covers its folder or its type.

### Who may save: `spec.allowedCommitRequests`

A `GitTarget` says which `CommitRequest`s may save its windows in `spec.allowedCommitRequests`, a
grant list like the others (see [the grants](#the-grants)). Each grant sets exactly one of two
fields, one for each place a request can be created:

- **`consumerNamespaces`** admits requests on the cluster GitOps Reverser runs in, by namespace,
  with `from: Same | Names | Selector | All`, as in every other grant list. It admits requests only
  for a `GitTarget` whose `KubeProvider` sets `inCluster`.
- **`sourceNamespaces`** admits requests on the cluster the `GitTarget` mirrors, by exact namespace
  name, or `"*"` for every namespace the `GitTarget` may mirror. Variables are refused. A namespace
  the `KubeProvider` does not lend this `GitTarget` in `spec.allowedGitTargets` is never admitted,
  the same bound its `WatchRule`s have. It admits requests only for a `GitTarget` whose
  `KubeProvider` names a kubeconfig.

The list follows the pattern of the other grant lists:

- **Omitted:** the `CommitRequest`s in the `GitTarget`'s own namespace, as one grant with
  `consumerNamespaces: {from: Same}`.
- **`[]`:** no `CommitRequest`.
- **A list:** the `CommitRequest`s at least one grant admits. The list replaces the default, so a
  `GitTarget` that mirrors another cluster writes a `sourceNamespaces` grant.

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: shared
  namespace: platform
spec:
  allowedCommitRequests:
    - consumerNamespaces: {from: Same}
    - consumerNamespaces:
        from: Selector
        selector:
          matchLabels:
            platform.example.com/tenant: "true"
```

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: shop
  namespace: team-shop
spec:
  kubeProviderRef: {name: shop-workspace}
  allowedCommitRequests:
    - sourceNamespaces: ["*"]
```

A request no grant admits fails before it attaches: `Ready=False`, `Stalled=True`, reason
`NotAllowed`. The message says which field applies on the cluster the request was made on.

### Who the save is from

A request's author is found from its own audit event, on the cluster it was created on, the same
way an edit's author is: from the same index, with the same `--author-attribution-grace`. So a save
attaches only to its creator's windows, by the ordinary author rule, and `status.author` names the
creator. The author is recorded in the request's first status write, before it attaches. Create a
request on a source cluster as JSON: `kubectl` and browsers send JSON, and an API server that drops
the audit event of a YAML body loses the save's author without an error.

| The cluster the request was made on | The save |
| --- | --- |
| sends audit events, and one names the creator | attaches to the creator's windows; `status.author` names them |
| sends no audit events at all (its `KubeProvider` never received one) | names nobody, and attaches to a window whose edits name nobody; the commit's author stays unresolved |
| sends audit events, but none names the creator | is refused, `Ready=True` with reason `AuthorUnresolved`: its event went missing, and attaching it would claim the window that should stay unattributed. Save again |
| any, with author attribution off | names nobody, and attaches to the `GitTarget`'s one unnamed window; the committer authors the commit |

Who may save is decided by `allowedCommitRequests`; who authored the commit is decided by audit, or
by nobody.

### What a save holds

A commit window holds one author's edits to one `GitTarget`, wherever that `GitTarget` mirrors them.
An admitted request therefore saves its creator's whole window, edits outside the request's own
namespace included: a grant in `allowedCommitRequests` authorizes exactly that. The request's
namespace decides only whether a grant admits it.

### When there is nothing to commit

`spec.whenNothingToCommit` decides what a request does when it ends with nothing to commit: its
`attachTimeout` ran out with no eligible window, or the window it attached to closed, for any
reason, without changing Git.

- `Resolve` (the default) finishes without a commit.
- `CommitEmpty` records `spec.message` in an empty commit, authored like any other commit by the
  request's submitter. It requires `spec.message`.

| Outcome | `Resolve` | `CommitEmpty` |
|---|---|---|
| The writes changed files | commit, `Committed` | commit, `Committed` |
| The writes already matched Git | no commit, `AlreadyPresent` | empty commit, `AlreadyPresent` |
| No eligible window before `attachTimeout` | no commit, `NoWindow` | empty commit, `NoWindow` |
| Only another author's window was open | no commit, `WindowMismatch` | no commit, `WindowMismatch` |
| An earlier save of the same author on the `GitTarget` was still in progress | `SaveInProgress`, at once | `SaveInProgress`, at once |
| The target is suspended, or its render fidelity is not established | `FinalizeFailed`, the cause | `FinalizeFailed`, the cause |
| The commit or the empty commit failed | `FinalizeFailed` | `FinalizeFailed` |
| The push failed | `WaitingForPush`, with the cause, until a push lands; `FinalizeFailed` if the worker stops first | the same |

The `Ready` reason keeps the cause, and `status.commit` with `Pushed=True` says the empty commit
reached the remote. A hash proves the message is in Git; `NoWindow` says the request saw no writes,
so a write delayed past `attachTimeout` is not in that commit. A request that only saw another
author's window never records one, because the history would then claim more than happened.

A crash between the push and the status write can record the message twice: the worker recognizes a
repeated request only while it runs.

### Its status

Progress and outcome are reported through kstatus-compatible **conditions** (no `phase` string).
`kubectl get commitrequest` shows `Ready`, its `Reason`, the `Author` and the `Commit`; `-o wide`
adds `Pushed` and `Applied`. Automation must stop on either `Ready=True` or `Stalled=True`;
`kubectl wait --for=condition=Ready` alone keeps waiting on terminal failures. `Ready=True` includes
successful no-commit outcomes. Require `Pushed=True` and `status.commit` for evidence that the
request produced a pushed commit:

- **Ready** (summary): `Unknown` while the save runs, from the first status write, which records the
  author. `True` once the request reached a non-error terminal outcome. The `Ready` condition's
  `reason` says which: `Committed` (a commit was pushed; `status.branch`/`status.commit` set), or an
  outcome with nothing to commit: `NoWindow`, `WindowMismatch`, `AlreadyPresent`, or
  `AuthorUnresolved`. With [`CommitEmpty`](#when-there-is-nothing-to-commit), `NoWindow` and
  `AlreadyPresent` can come with a recorded empty commit. `Ready=False` with `Stalled=True` is a
  failure: `NotAllowed` when the `GitTarget` does not admit the request, or the request was made on
  the wrong cluster; `GitTargetNotFound` when the `GitTarget` did not exist within `attachTimeout`;
  `SaveInProgress`; or `FinalizeFailed`.

  `SaveInProgress` means an earlier request of the same author on the same `GitTarget` was still
  waiting for a commit window, or collecting one, when this one arrived. A window carries one
  message, so the two would race for it and one message would be lost without a word; the later
  request is refused at once instead. Save again with a new request once the earlier one is `Ready`.
  A request for the `Next` window is not refused by one that is collecting: it closes that window
  under the earlier request's message first. Saves by different authors do not race for a
  message: a write by another author closes the open window, and its message, before it opens one
  of its own.

  `SaveInProgress` is decided when the branch worker registers the request, which is after its
  author was found, not when it is created. A client that relies on it to keep two of its saves
  apart waits until the `Reconciling` reason is `WaitingForWindow` or `CollectingWindow` before it
  makes the save's edits, not only for `Ready=Unknown`: a request still `Saving` is not registered
  yet, and its edits could reach the window of the save before it.

  `AlreadyPresent` means **the remote confirmed there was nothing to add**, so it is reported after
  the push rather than when the local plan found no difference. That costs a few seconds and buys a
  true answer: a competing Git-side edit can turn the same captured object into a real commit when
  the worker replays onto the moved branch, and such a request resolves `Committed` instead. A
  worker that stops before the push fails the request rather than leaving it to time out.
- **Reconciling** / **Stalled**: the kstatus progress/blocked pair. `Reconciling=True` while the
  request is in progress, with the phase the branch worker reports as its reason: `Saving`
  (claimed, not yet registered), `WaitingForWorker` (the GitTarget has no branch worker yet),
  `WaitingForWindow`, `CollectingWindow`, or `WaitingForPush`. Each phase, and the outcome, is
  written as soon as the branch worker reports it.
  `Stalled=True` when the request failed and needs attention (kstatus reports the object Failed).
- **Pushed**: `True` once the commit, empty or not, is in the remote repository.
- **Applied**: only with [`catchUp: AfterPush`](#applying-a-save-at-once), once `Ready=True`:
  whether the applier caught up with the commit. It never changes `Ready`, so `Ready=True` alone
  does not mean the commit was applied.

A request is served again after GitOps Reverser restarts: the path that owns it finds its own
claim, and resumes with the author it recorded.

### How long a request lives

A `CommitRequest` is a single save, not a standing order: every stage it passes through ends on its
own, and the status says which limit ended it. To save again, create a new request.

| Stage | Ends after | What the request then says |
| --- | --- | --- |
| Picked up, author named, waiting for a branch worker | `attachTimeout` + `maxDuration` + 2 minutes, counted from creation | `Ready=False`, `Stalled=True`, reason `FinalizeFailed`: the `GitTarget` did not start a branch worker, or the request was not acted on, within the safety window |
| Waiting for a window (`WaitingForWindow`) | `attachTimeout` | `Ready=True`, reason `NoWindow` (see [When there is nothing to commit](#when-there-is-nothing-to-commit)) |
| Collecting its window (`CollectingWindow`) | `maxDuration`, at most `5m` | the window closes and is committed |
| Waiting for the push (`WaitingForPush`) | no limit of its own | stays `Ready=Unknown`; `Reconciling` says why the push waits |
| Waiting for its apply, with `catchUp: AfterPush` | 10 minutes from `Ready` | `Applied=Unknown`, reason `TimedOut` (see [Applying a save at once](#applying-a-save-at-once)) |
| Finished | 48 hours, by default | deleted (see [Finished requests are deleted after 48 hours](#finished-requests-are-deleted-after-48-hours)) |

The push is the one wait without a limit, deliberately. Once the window is committed, the commit is
going to the remote: failing the request while the worker still retries, every 10 seconds doubling
to 5 minutes, would report a save as failed that then lands. A worker that stops before the push
fails the request instead. A request that waits there needs the remote fixed, not a new request.

### Applying a save at once

A save is in Git once it is pushed, and the cluster already runs it, so by default nothing applies
it: the applier claim's `catchUpAfter` brings the applier's own state (a `Kustomization`'s
`status.lastAppliedRevision`, its `GitRepository`'s artifact) level later. A request that needs that
state at once, to publish what was applied, sets `spec.catchUp: AfterPush`. Its
`GitTarget`'s applier is then asked to apply the branch tip right after the commit is pushed. The
tip may hold later commits, saves pushed close together can share one apply, and an applier
already at the tip needs none. The request reports the outcome in its `Applied` condition:

| `Applied` | Reason | Means |
| --- | --- | --- |
| `Unknown` | `Applying` | the apply is asked for or runs |
| `Unknown` | `Retrying` | it did not complete and is asked again; the message says why |
| `True` | `Applied` | the applier applied a revision that holds the commit; `status.appliedRevision` names it, as the applier reported it |
| `True` | `NothingToApply`, `NoApplierClaim`, `ApplierGone` | no apply was needed: the request pushed no commit, or the `GitTarget` names no applier claim, or stopped naming the one asked. The cluster already holds what was saved |
| `False` | `CommitDropped` | the branch no longer holds the commit: its history was rewritten after the push. The only `False` |
| `Unknown` | `TimedOut` | the apply did not settle within 10 minutes of `Ready`, so the request stopped following it. The commit stays pushed, and the applier may still apply it; the message says where the apply last stood, and the applier claim's status reports later applies |
| `Unknown` | `Lost` | GitOps Reverser restarted before it learnt how the apply went |

Wait for `Applied=True` for "saved and applied"; `Ready=True` stays "saved". A request follows its
apply for 10 minutes at most: an applier that cannot catch up does not keep it open, and a request
that ends `TimedOut` needs the applier looked at, not a new request. A finished request is deleted
only after its `Applied` condition is settled.

### Finished requests are deleted after 48 hours

Every save leaves one `CommitRequest` behind. When a request finishes (`Ready=True` or
`Stalled=True`), GitOps Reverser writes the time it will be deleted onto it:

```yaml
metadata:
  annotations:
    configbutler.ai/delete-after: "2026-10-01T09:30:00Z"
```

Once that time has passed, the request is deleted. The annotation is the whole contract, so it is
also how you change the outcome for one request:

- Remove the annotation to keep the request.
- Edit the time, in RFC 3339 format, to delete it earlier or later. A value that does not parse
  keeps the request.
- Set it yourself when you create the request to choose the time up front. GitOps Reverser never
  replaces a value that is already there.

The period comes from `--commit-request-ttl` (Helm: `controllerManager.commitRequestTTL`, default
`48h`). Changing it only affects requests that finish afterwards. `0` writes no annotation, so new
requests are kept. A request that finished before this feature existed has no annotation and is
never deleted.

On the cluster GitOps Reverser runs in this always applies. On a source cluster it applies only
when the `KubeProvider` sets `spec.commitRequests: ReconcileAndDeleteFinished`; with `Reconcile`,
nothing is stamped and nothing is deleted, and annotations stamped before a switch back to
`Reconcile` stay without effect.

### Serving the `CommitRequest`s of a source cluster

Three things turn it on, each on purpose:

1. **Install the CRD on the source cluster.** It is one CRD, the release asset
   `commitrequest-crd.yaml`, apart from the other `configbutler.ai` kinds, which a source cluster
   does not need. Install it with `kubectl apply --server-side -f commitrequest-crd.yaml`. GitOps
   Reverser never creates or updates it there.
2. **Set `spec.commitRequests` on the `KubeProvider`.** GitOps Reverser then watches the cluster's
   `CommitRequest`s through that provider, in the namespaces it lends its `GitTarget`s in
   `spec.allowedGitTargets` (one watch across the cluster when a grant lends `"*"`), and writes each
   one's status back through the same credential. A `CommitRequest` is never mirrored, whatever a
   `WatchRule` selects.
3. **Grant it on the `GitTarget`, in `spec.allowedCommitRequests`,** with `sourceNamespaces`. The
   provider switch makes the requests reachable; the `GitTarget`'s owner decides which of them may
   save its windows.

Each value of `spec.commitRequests` makes the cluster a bigger source of `CommitRequest`s, and needs
more of the credential on `commitrequests` in the `configbutler.ai` group, in each lent namespace:

| Value | What GitOps Reverser does | The credential needs |
| --- | --- | --- |
| `Ignore` (default) | nothing | nothing |
| `Reconcile` | serves the requests in the namespaces this `KubeProvider` lends its `GitTarget`s, and writes their status. A finished request stays, and no `delete-after` is stamped on it, because nothing would act on it | `get`, `list`, `watch`; `update` on `commitrequests/status` |
| `ReconcileAndDeleteFinished` | as `Reconcile`, and also stamps `configbutler.ai/delete-after` when a request finishes, and deletes the request once that time has passed | as `Reconcile`, plus `patch` and `delete` |

A `KubeProvider` with `inCluster` takes only `Ignore`: the `CommitRequest`s of the cluster GitOps
Reverser runs in are always served, with the operator's own identity.

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: KubeProvider
metadata:
  name: shop-workspace
  namespace: team-shop
spec:
  secretRef: {name: shop-workspace}
  commitRequests: ReconcileAndDeleteFinished
```

The `KubeProvider` reports how serving goes in its `CommitRequestsServed` condition, which is not
part of `Ready`, and the namespaces it watches in `status.commitRequests.namespaces`. Both are absent
while `spec.commitRequests` is `Ignore`. `kubectl get kubeprovider -o wide` shows the reason in the
`Saves` column:

| Status | Reason | When |
| --- | --- | --- |
| `True` | `Watching` | the message says how many namespaces, or "every namespace" |
| `True` | `NothingToWatch` | no `GitTarget` mirroring through this `KubeProvider` is lent a namespace |
| `False` | `CRDNotInstalled` | the cluster serves no `commitrequests.configbutler.ai`; apply `commitrequest-crd.yaml` |
| `False` | `VersionNotServed` | the CRD is there, but does not serve `v1alpha3`; apply `commitrequest-crd.yaml` of this release |
| `False` | `Forbidden` | `list` or `watch` was denied; the message names the namespaces and the verbs to grant |
| `Unknown` | `Unreachable` | the cluster's API could not be read; the message carries the error |

It is checked on every resync, so a CRD applied by hand clears it without a restart. A finished
request the credential may not delete stays in place; the message says so, and the condition stays
`True`, because serving works.

## Audit ingestion settings

Object state comes from Kubernetes **watch**, not from audit. Audit is an optional attribution lookup:
kube-apiserver posts audit events to a named path, `/audit-webhook/<audit-route>`, where the route is
`KubeProvider.spec.attribution.auditRoute` and defaults to `<namespace>.<name>`. The
operator extracts a minimal attribution fact from each (auditID, user, verb, resourceVersion, GVR, namespace, name, UID,
status, timestamps) and appends it to a per-type **fact log**: one append per type per request, not one
per event. The watch side follows the log for the types it is watching, holds the facts in a bounded,
TTL'd in-memory index, and attaches the commit author to each watch event by matching a fact within a
bounded grace window. Redis also stores per-watch resume cursors, so short reconnects can resume a
normal watch from the last processed resourceVersion when the apiserver can still serve that history.

Named ingress is authenticated to the shared audit CA. It is **not** gated on a `KubeProvider`
carrying that route, and deliberately so: a route is a partition name, not a claim about an object,
and refusing unknown routes dropped audit batches in flight while a provider was being created or
recreated, which the API server does not retry after a 404. A fact filed under a route nobody reads
costs one key that expires on the fact TTL. Ingress also does **not** yet bind a particular client
certificate to a provider. Do not use one shared audit
client credential to attribute several independently administered source clusters. A deployment that
needs that boundary should keep sources isolated until provider-bound ingress authentication is shipped.

### Route a shared audit stream by event annotation

Most audit streams represent one source cluster and must use a named route, including
`/audit-webhook/gitops-reverser.local`. Some control planes emit one shared stream for several logical clusters.
For that shape, the bare `/audit-webhook` endpoint is available only when the configuration model's
annotation key is set:

```yaml
attribution:
  auditRouteAnnotationKey: example.io/source-cluster
```

When this option is set, the receiver reads `example.io/source-cluster` from each event. Its value is
the **audit route** the event belongs to, so events in the same batch may route to different
partitions. A `KubeProvider` joins a route by setting `spec.attribution.auditRoute` to the same
value; it defaults to `<namespace>.<name>`.

**The bare endpoint never guesses a route.** Rejection happens at two levels:

| Situation | Result |
|---|---|
| A request reaches `/audit-webhook` while `auditRouteAnnotationKey` is unset | The whole request is rejected with **400**. The bare endpoint is not enabled, so a producer posting to it is misconfigured. This is the one route-shaped rejection there is, and it is counted as `gitopsreverser_audit_eventlist_duration_seconds_count{outcome="bare_endpoint_disabled"}`. |
| An event carries no annotation | That **event** is rejected: it produces no attribution fact and is never credited to a fallback route. The request still returns 200, so correctly-annotated events in the same batch are kept. |

An annotation naming a route no `KubeProvider` has declared is **not** rejected. The route is a
partition name rather than a claim about an object, so the fact is stored and expires unread if
nothing joins it. That keeps ingestion free of Kubernetes reads, and lets a provider created after
its events started flowing pick them up.

The second row is a per-event rejection rather than a per-request one on purpose. A shared stream is
heterogeneous by definition, so failing the whole batch would discard events that routed correctly and
leave the apiserver retrying a batch that can never succeed. Rejected events are counted and logged, so
a producer that is not stamping the annotation is visible rather than silent. If that count rises,
point the producer at `/audit-webhook/<audit-route>` instead.

Use an annotation that the producing control plane sets consistently as source metadata. This is
routing metadata only: it keeps the audit fact and the watch event in the same source-cluster
partition, so a user from one logical cluster can never be credited for a matching object in another.

Valkey/Redis is **optional**: when `--redis-addr` is set, watch resume cursors are stored so restarts
pick up where they left off; when left empty, watches cold-replay from scratch on restart instead. The
Helm chart defaults to **configured-author** (`attribution.enabled: false`): the audit webhook is
unused and every mirrored-resource commit is authored by the configured committer.

Attribution needs a **fact transport**, which is not the same as needing Redis:

| `--author-attribution-transport` | What carries the facts | `--redis-addr` |
|---|---|---|
| `redis` (default) | Redis streams, one per (audit route, type) | required |
| `memory` | an in-process ring buffer | may be empty |

Choose `redis` for anything you would call production. It is the only transport whose facts survive a
restart, and the only one that can reach a second process, so it is what an eventual HA topology needs.
Choose `memory` for a single-pod install where running a Valkey StatefulSet to name commit authors
is out of proportion to the benefit. The cost is worth stating plainly: in-memory facts do not survive a
restart, so events in flight across one lose their author. That is already true of any restart today,
which is what keeps the difference small.

`memory` is **refused at startup with more than one replica**. The transport only carries facts within
one process, so with two replicas an audit request answered by one pod leaves a watch running on the
other with nothing to join, and every commit through it would be authored `attribution-unresolved` with
nothing saying why. The chart passes `replicaCount` in for this check.

```yaml
queue:
  redis:
    addr: "valkey:6379"
    auth:
      existingSecret: "valkey-auth"
      existingSecretKey: "password"
```

Every key this operator writes is rooted at `queue.redis.keyPrefix` (`--redis-key-prefix`, default
`gitops-reverser`). Give each reverser its own prefix when several share one Redis/Valkey: `--redis-db`
separates only 16 logical databases, and one reverser per tenant or per branch environment passes that
long before it reaches any real Redis limit.

When attribution is enabled, these flags tune the join:

- `--author-attribution-ttl` (default `10m`): how long an attribution fact is retained waiting for the
  matching watch event to join it.

  **Inside that window a fact can name a later write by a different identity. This is a known
  limitation of the weakest join tier, not a desired property.** The join tries the strongest
  evidence first, and its floor is a `(namespace, name)` tier that is last-writer-wins and bounded
  only by this TTL. A write that misses the exact tier can settle for that floor. So an out-of-band
  change, such as a hand-run `kubectl` command against a mirrored object, leaves a fact that a
  later write to the same name can match while the TTL still holds it, and that commit is then
  attributed to whoever made the out-of-band change. It clears itself once the fact expires.

  The floor exists because some writes carry nothing stronger: an aggregated-API write's audit
  record holds a name and neither a uid nor a resourceVersion, and removing the tier would leave
  that whole class unattributed. Narrowing the TTL narrows the exposure, at the cost of losing
  actors whose audit delivery is slow. The full ranking is in
  [`spec/attribution.md`](spec/attribution.md).
- `--author-attribution-grace` (default `3s`): bounded per-event wait for a matching audit fact before a
  watch event ships authored by the `attribution-unresolved` sentinel. Note the delivery floor: the
  apiserver's own `--audit-webhook-batch-max-wait` delays every fact by up to that much, so a grace at or
  below it will lose actors systematically. The wait ends the moment the fact arrives, so a generous
  grace costs latency only when a fact never comes.

  **A removal is the exception, and it is worth budgeting for.** It is credited only to the request
  that caused it: its delete, or, when the object left a rule's `objectSelector`, the write that
  relabeled it. The object's last write never answers it, so an object edited by one person and
  deleted by another is credited to the deleter, never to the editor. Sometimes no such fact ever
  arrives, most often because the cluster's **audit policy excludes the type** (which is true of
  every type in the recommended policy's runtime-noise list). In that case the removal spends the
  whole grace and then ships unresolved. Measured across one e2e run, where the grace is 10s, the
  cost lands on exactly that case and nowhere else: a removal that finds its evidence resolves in
  about 70ms, while one that never does waits the grace. Creates and updates do not consult the
  deletion tiers at all. The watch shard is single-threaded, so the wait also delays the events
  queued behind it on the same `(GitTarget, type, scope)`. Lowering this flag bounds both directly.
  [What attribution gives up, on purpose](spec/attribution.md#what-attribution-gives-up-on-purpose)
  lists every trade of this kind, including the one wrong author it accepts.

  **Watching a type your audit policy excludes costs more than attribution.** Every removal on such
  a type spends the full grace before shipping unresolved, because the fact it is waiting for was
  never recorded. If a watched type's commits are consistently authored
  `unknown (attribution unresolved)`, check the audit policy before anything else: a type in the
  policy's `level: None` list can never be attributed, and no operator-side setting changes that.
- `--author-attribution-max-facts-per-type` (default `4096`) and `--author-attribution-max-facts`
  (default `65536`): how many facts the in-memory index holds, per type and in total, evicted
  oldest-first. Per-type is the fair cap: a burst on one noisy type must not evict every other type's
  facts. Evictions are counted on `attribution_fact_index_evictions_total{reason}`.
- `--author-attribution-collection-window` (default `30s`): how long after a `deletecollection` a
  removal in its scope may still be credited to it. It only has to cover audit batching plus clock skew,
  because the removal is attributed at delete-request time, so finalizers do not stretch it. Raising it
  widens the risk of crediting an unrelated delete to the collection's actor.
- `--author-attribution-collection-uid-cap` (default `10000`): how many object UIDs a `deletecollection`
  fact carries before the set is dropped and the join falls back to scope matching. The fallback is
  already correct, so this only decides how often the precise path is taken; drops are counted on
  `attribution_collection_without_uidset_total{reason}`.

A matched actor is always named by its own username, humans and service accounts alike (e.g.
`system:serviceaccount:flux-system:kustomize-controller`); there is no option to collapse service
accounts to the committer.

```yaml
attribution:
  transport: "redis"
  ttl: "10m"
  grace: "3s"
  maxFactsPerType: 4096
  maxFacts: 65536
  collectionWindow: "30s"
  collectionUIDCap: 10000
```

## Push receiver settings

The [push receiver](#gitreceiver) is its own HTTP listener, separate
from the audit listener. A Git host authenticates with an HMAC or a token over the body and cannot
present the client certificate the audit listener requires, so the two cannot share one.

| Flag | Helm value | Default | What it does |
| --- | --- | --- | --- |
| (none) | `servers.receiver.enabled` | `false` | Serve the receiver; the chart then passes the flag below |
| `--receiver-bind-address` | `servers.receiver.bindAddress` | `0` (off); Helm: `:9292` | `host:port` the receiver listens on. `0` or empty serves no receiver |
| (none) | `servers.receiver.port` | `9292` | The container port; must match the port in `bindAddress` |

The chart creates a `<release>-receiver` Service when the listener is on. Expose it to your
Git host with an Ingress or a Gateway route of your own that terminates TLS, the way Flux's
`webhook-receiver` Service is exposed. Every request still has to carry a valid signature for the
route it names, which is a `GitReceiver`'s `status.webhookPath`; any other path does not serve.

The listener runs in the pod that runs the branch workers. The chart runs exactly one replica (it
refuses `replicaCount` above `1`), so that is the only pod; running more than one needs ownership
coordination first, and a delivery would then have to reach the pod that owns the branch. The
Service targets port `receiver` and is exposed on `service.ports.receiver` (`80` by default).

## Quickstart vs hand-managed resources

Keep using the [quickstart](quickstart.md) when you want the fastest first commit.
The chart's `quickstart` values create a starter `GitProvider`, `GitTarget`, and `WatchRule` for you.

The starter `GitTarget` writes under `live-cluster` by default. Override
`quickstart.gitTarget.path=.` only when you want the starter target to own the repository root.

Move to hand-managed resources when you want:

- more than one `GitTarget`
- more than one watch rule
- cluster-scoped watching with `ClusterWatchRule`
- ad hoc save requests with `CommitRequest`
- direct control over `GitProvider.spec.commit`
- direct control over encryption settings

The chart value reference for the starter `quickstart` block lives in
[charts/gitops-reverser/README.md](../charts/gitops-reverser/README.md).

## What to read next

- [commit-signing.md](commit-signing.md) for signing behavior on Git hosting platforms
- [github-setup-guide.md](github-setup-guide.md) for GitHub auth setup
- [azure-devops-getting-started.md](azure-devops-getting-started.md) for Azure DevOps auth and setup
- [sops-age-guide.md](sops-age-guide.md) for `GitTarget.spec.encryption`
