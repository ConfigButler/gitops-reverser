# Start mirroring a team's resources

This guide takes one team from nothing to its resources landing in a folder of the team's
repository, ending at a first commit you can inspect. It is for a platform administrator onboarding
a team on an existing installation, and for the team owner who then decides what to mirror.

It assumes GitOps Reverser is installed. If it is not, follow the [quick start](quickstart.md)
first. You can repeat this guide for every team on the same installation.

## First: does Flux or Argo CD apply the folder?

Ask this before you pick the folder. If Flux or Argo CD applies the folder this team's resources
will be written to, the two tools need coordinating, or each commit GitOps Reverser makes is applied
back over edits still in flight. In that case, finish this guide on a
[scratch branch](#adopt-an-existing-repository-safely) that nothing deploys, then follow
[bi-directional GitOps](bi-directional.md) before you point the team at the deployed folder.

If nothing applies the folder, or Flux and Argo CD apply only other folders, this guide is all you
need.

## The six choices

Onboarding a team means making six choices. Each one is an object, and each object has an owner:

| Choice | Example in this guide | Object | Usually owned by |
| --- | --- | --- | --- |
| The repository and its credential | `git@github.com:acme/platform.git` | `GitProvider` + Secret | Platform |
| The source cluster | The control cluster, which the operator runs in | `KubeProvider` | Platform |
| The branch | `main` | `GitBranch` | Platform for a shared branch, the team for its own |
| The folder | `teams/team-a` | `GitTarget` | Team |
| The namespaces | `team-a` | `WatchRule` and the provider's grant | Team, within the platform's grant |
| The resource types | Deployments, Services, ConfigMaps | `WatchRule` | Team |

![A platform namespace holds the shared GitProvider with its credentials Secret, a KubeProvider and
a GitReceiver; each team namespace has its own WatchRule and GitTarget writing its own branch of that
repository, and a push event is routed by repository and branch to the GitTarget that writes
it](images/config-platform-shared.excalidraw.svg)

The objects that hold credentials (`GitProvider`, `KubeProvider`) stay in the platform's namespace
with their Secrets. The team's objects live in the team's namespace and reach the shared ones by
reference. A shared object admits only the namespaces its **grants** list, so the platform decides
what each team may use, and the team decides what to do with it.

The examples use these names:

- `platform`: the platform team's namespace, holding the repository credential.
- `gitops-reverser`: the chart's release namespace, holding the chart's `local` `KubeProvider`.
- `team-a`: the team's namespace, which is also the namespace it wants mirrored.

## 1. Platform: connect the repository

Skip this step if a `GitProvider` for the repository exists: add the `allowedGitBranches` grant
below to it, and use its name wherever this guide says `platform-repo`. Otherwise create the
credential Secret as in the [quick start](quickstart.md) or the [GitHub guide](github-setup-guide.md),
in `platform`, and a `GitProvider` beside it:

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitProvider
metadata:
  name: platform-repo
  namespace: platform
spec:
  url: git@github.com:acme/platform.git
  secretRef:
    name: git-creds
  allowedGitBranches:
    - consumerNamespaces: {from: Same}
      branchPatterns: [main]
```

`allowedGitBranches` has no default: without a grant, not even the provider's own namespace may use
the credential. This grant lets a `GitBranch` in `platform` hold `main`.

## 2. Platform: hold the branch, and let the team write through it

One `GitBranch` holds a repository branch for everybody who writes it. For a shared branch such as
`main`, the platform owns it and lists who may write through it:

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitBranch
metadata:
  name: main
  namespace: platform
spec:
  gitProviderRef:
    name: platform-repo
  branch: main
  allowedGitTargets:
    - consumerNamespaces: {from: Same}
    - consumerNamespaces: {from: Names, names: [team-a]}
```

Add each new team to the `names` list. Check that the branch is held:

```bash
kubectl -n platform get gitbranch main
```

`READY` is `True` once the credential could open a push session on the repository. `False` with
reason `PushSessionFailed` means the Secret is missing, wrong, or lacks push permission.

**A branch of the team's own** is the alternative: a grant on the `GitProvider` such as
`branchPatterns: ["teams/{consumer.namespace}/*"]` lets the team create its own `GitBranch` and
decide who shares it. [One tenant, end to end](configuration.md#one-tenant-end-to-end) shows that
shape.

## 3. Platform: let the team mirror its namespace

The `KubeProvider` decides which namespaces' `GitTarget`s may read the cluster, and which source
namespaces they may read. The chart's `local` provider is configured through Helm values. A list you
set is the whole policy, so keep the release namespace's own grant if anything there still uses it:

```yaml
# values.yaml
kubeProvider:
  allowedGitTargets:
    - consumerNamespaces: {from: Same}
      sourceNamespaces: ["{consumer.namespace}"]
    - consumerNamespaces: {from: Names, names: [team-a]}
      sourceNamespaces: ["{consumer.namespace}"]
```

```bash
helm upgrade gitops-reverser oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser --reuse-values -f values.yaml
```

`{consumer.namespace}` is the namespace of the `GitTarget` asking, so `team-a` may mirror `team-a`
and nothing else. Two other common shapes:

- **Many teams at once:** `from: Selector` with a namespace label the platform controls, such as
  `platform.example.com/tenant: "true"`. Keep labeling namespaces a platform permission, so a
  team cannot grant itself access.
- **A remote cluster:** a `KubeProvider` of your own with a kubeconfig Secret, carrying the same
  grant in its `spec.allowedGitTargets`. See
  [creating and managing a `KubeProvider`](configuration.md#creating-and-managing-a-kubeprovider).

[The grants](configuration.md#the-grants) explains every shape, and
[RBAC](rbac.md) explains what the operator's own identity can read.

## 4. Platform: let the team create its objects

The chart ships no Role for people who create GitOps Reverser objects. Give the team one in its
namespace, for example:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: gitops-reverser-editor
  namespace: team-a
rules:
  - apiGroups: [configbutler.ai]
    resources: [gittargets, watchrules, commitrequests]
    verbs: [get, list, watch, create, update, patch, delete]
```

Bind it to the team's group with a `RoleBinding`. Add `gitbranches` when the team owns its own
branch.

## 5. Team: choose the folder

A `GitTarget` names the folder, the branch, and the source cluster:

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: apps
  namespace: team-a
spec:
  gitBranchRef:
    name: main
    namespace: platform
  kubeProviderRef:
    name: local
    namespace: gitops-reverser
  path: teams/team-a
```

- `spec.path` is relative to the repository root. Two `GitTarget`s on one branch cannot write the
  same or nested folders; the later one reports `TargetConflict`.
- `spec.gitBranchRef` and `spec.kubeProviderRef` are immutable. To change either, create a new
  `GitTarget` and delete the old one.
- `spec.prune.mode` decides when a document leaves Git. The default, `OnEvent`, removes a document
  when its resource is deleted, and never on a guess. See
  [the deletion policy](#choose-the-deletion-policy).

The [`GitTarget` reference](configuration.md#gittarget) covers commit timing, messages, encryption,
and placement.

## 6. Team: choose what to mirror

A `WatchRule` selects the resource types, and optionally labels, that the target mirrors from the
team's namespace:

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata:
  name: apps
  namespace: team-a
spec:
  gitTargetRef:
    name: apps
  rules:
    - apiGroups: ["apps"]
      resources: [deployments]
    - apiGroups: [""]
      resources: [services, configmaps]
```

Each rule item watches the `WatchRule`'s own namespace unless it names another in
`sourceNamespace`, which the provider's grant must allow. `objectSelector` narrows a rule to objects
with matching labels.

Mirror the resources that express intent, and leave out what controllers generate from them: a
`Deployment` and not its `ReplicaSet`s and `Pod`s, a Flux `HelmRelease` and not the workloads it
renders. GitOps Reverser cannot tell an authored object from a generated one. See
[choosing what to capture](installing-apps-as-krm.md#the-design-decision-capture-intent-not-the-rendered-output).

## 7. Verify the first commit

Check the team's objects:

```bash
kubectl -n team-a get gittarget,watchrule
kubectl -n team-a wait --for=condition=Validated gittarget/apps --timeout=60s
```

`Validated` means the grants admit the target. The rule's `READY` turns `True` once its watches
run. Then pull the branch: the first commit holds the selected resources that already exist in
`team-a`, under `teams/team-a`. Create a ConfigMap and a second commit follows within seconds:

```bash
kubectl -n team-a create configmap onboarding-check --from-literal=check=1
```

Delete it afterward; under the default deletion policy, that removes its file in a third commit.

## Adopt an existing repository safely

When the folder already holds manifests, see what GitOps Reverser does with them before it writes
`main`. Point a preview target at a **scratch branch** that nothing deploys, started from `main`:

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitBranch
metadata:
  name: preview
  namespace: platform
spec:
  gitProviderRef:
    name: platform-repo
  branch: gitops-preview/team-a
  parentBranch: main
  allowedGitTargets:
    - consumerNamespaces: {from: Names, names: [team-a]}
```

The `GitProvider`'s grant must allow `gitops-preview/*` as well. A `GitTarget` naming this branch
compares the cluster with the folder on `main`, and creates `gitops-preview/team-a` only when it has
something to commit. Read those commits as a normal diff: which files changed, which documents were
added, and where new ones were placed. When you are satisfied, delete the preview `GitTarget` and
create the one for `main`. The preview cannot disturb the folder on `main`, because a `GitTarget`
cannot change branch.

What it can edit depends on how the folder is written:

| The folder holds | What GitOps Reverser does |
| --- | --- |
| Plain Kubernetes manifests | Updates existing documents in place, and adds new ones |
| A supported Kustomize layout | Edits source manifests, image and replica declarations, and `configMapGenerator` inputs; adds and removes resources |
| A Flux `HelmRelease` or Argo CD `Application` | Mirrors the declaration, including chart versions and inline values |
| Helm templates, or a layout it cannot edit safely | Refuses the folder before writing anything: `Stalled=True`, with the reason in `GitPathAccepted` |

The [Kustomize support in the target path](configuration.md#kustomize-support-in-the-target-path)
section lists the exact subset. To inspect a repository without a cluster, use the
`manifest-analyzer` CLI.

### Choose the deletion policy

A document can leave Git for two reasons: the resource was deleted, or a fresh snapshot of the
cluster no longer contains it. `spec.prune.mode` treats them separately:

| Mode | A resource is deleted | A document has no resource in the snapshot | Use it for |
| --- | --- | --- | --- |
| `OnEvent` (default) | Removed | Kept | Adopting a folder: files that match nothing in the cluster stay |
| `Always` | Removed | Removed | A folder that should match the cluster exactly |
| `Never` | Kept | Kept | An archive that only ever gains documents |

Start with `OnEvent` when the folder already holds files. `Always` deletes every file the snapshot
does not account for, so a watch rule narrower than you meant becomes deleted manifests.
[The deletion policy reference](configuration.md#deletion-policy-specprunemode) explains the risk in
full.

## When nothing is committed

Start at the `GitTarget`'s `READY` reason, then the `WatchRule`'s:

| Reason | On | Cause |
| --- | --- | --- |
| `BranchNotAllowed` | `GitBranch` | No grant on the `GitProvider` allows the branch for this namespace |
| `PushSessionFailed` | `GitBranch` | The credential cannot push: Secret, host keys, or permissions |
| `BranchConflict` | `GitBranch` | Another `GitBranch` already holds this repository branch. Reference that one |
| `NamespaceNotAuthorized` | `GitTarget` | The `GitBranch` or the `KubeProvider` has no grant admitting the team's namespace |
| `KubeProviderNotFound` | `GitTarget` | `spec.kubeProviderRef` names no provider. Check the namespace |
| `TargetConflict` | `GitTarget` | Another target on the branch writes the same or a nested folder |
| `UnsupportedContent`, or `GitPathAccepted=False` | `GitTarget` | The folder holds something it cannot edit safely. The message names the file |
| `SourceNamespaceNotAllowed` | `WatchRule` | A rule item names a namespace the provider's grant does not allow |

[Why a `GitTarget` is not committing](configuration.md#why-a-gittarget-is-not-committing) walks
through every condition.

## Next steps

- [Record who made each change](attribution-setup-guide.md), so commits name the Kubernetes user
  as author.
- [Group related changes into one commit](demo/commit-window.md) with a `CommitRequest`.
- [Encrypt Secrets with SOPS and age](sops-age-guide.md) before mirroring `Secret`s.
- If Flux or Argo CD applies the folder: [bi-directional GitOps](bi-directional.md).
