# Configuration Model

This guide explains the real configuration objects that drive gitops-reverser after the install
steps in the [root README](../README.md).

The short version:

- `GitProvider` defines where and how to push
- `GitTarget` defines which branch and path to write into
- `WatchRule` defines which namespaced resources should produce Git writes
- `ClusterWatchRule` does the same for cluster-scoped or cross-namespace watching

The chart's optional `quickstart` values are just a convenience layer that creates starter
instances of those same resources.

## How the objects fit together

![Config basics diagram showing the relationship between GitProvider, GitTarget, and WatchRule](images/config-basics.excalidraw.svg)

The usual flow is:

1. Create a `GitProvider` for repository access and commit behavior.
2. Create a `GitTarget` that points at that provider plus a branch and path.
3. Create one or more `WatchRule` or `ClusterWatchRule` objects that point at that target.

That means one repository connection can back multiple targets, and one target can be fed by
multiple watch rules.

## `GitProvider`

`GitProvider` defines the Git remote, credentials, allowed branches, push strategy, and commit
behavior.

The important fields are:

- `spec.url`: repository URL
- `spec.secretRef.name`: Secret with Git credentials such as SSH or HTTPS auth
- `spec.allowedBranches`: branches this provider is allowed to write
- `spec.push`: batching/push behavior
- `spec.commit`: committer identity, commit templates, and signing

Example:

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: GitProvider
metadata:
  name: example-provider
  namespace: default
spec:
  url: git@github.com:example-org/example-repo.git
  secretRef:
    name: git-creds
  allowedBranches:
    - main
```

### `GitProvider.spec.commit`

`spec.commit` configures how gitops-reverser writes commits:

- `committer`: the operator identity written as the Git committer
- `message`: the subject format for per-event and batch commits
- `signing`: the SSH signing key configuration

If `spec.commit` is omitted, gitops-reverser uses its built-in defaults.

#### Author vs committer

These are different on purpose:

- **Author**: who made the cluster change
- **Committer**: who wrote the Git commit object

For per-event commits, the author comes from the Kubernetes audit event. For batch-style snapshot
commits, the operator is effectively both the author and the committer.

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

#### Commit message templates

Use `spec.commit.message.template` for normal per-event commits and
`spec.commit.message.batchTemplate` for atomic batch or snapshot-style commits.

```yaml
spec:
  commit:
    message:
      template: "[{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Name}}"
      batchTemplate: "reconcile: sync {{.Count}} resources"
```

`template` can use:

- `Operation`
- `Group`
- `Version`
- `Resource`
- `Namespace`
- `Name`
- `APIVersion`
- `Username`
- `GitTarget`

`batchTemplate` can use:

- `Count`
- `GitTarget`

Examples:

```yaml
spec:
  commit:
    message:
      template: "chore: [{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Name}}"
```

```yaml
spec:
  commit:
    message:
      template: "[{{.Operation}}] {{.Resource}}/{{.Name}} ({{.Username}})"
```

```yaml
spec:
  commit:
    message:
      batchTemplate: "snapshot: {{.Count}} resources"
```

#### Commit signing

GitOps Reverser signs commits from `spec.commit.signing`.

The signing `Secret` uses these data keys:

- `signing.key`: PEM-encoded SSH private key
- `passphrase`: optional passphrase for encrypted private keys
- `signing.pub`: optional convenience copy of the public key

The operator publishes the effective public key in `.status.signingPublicKey`.

Let the operator generate the signing key:

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: GitProvider
metadata:
  name: example-provider
  namespace: default
spec:
  url: git@github.com:example-org/example-repo.git
  allowedBranches:
    - main
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

## `GitTarget`

`GitTarget` decides where inside the repository resources are written.

The important fields are:

- `spec.providerRef`: which `GitProvider` backs this target
- `spec.branch`: which allowed branch to write to
- `spec.path`: path inside the repository
- `spec.encryption`: how `Secret` resources should be encrypted before commit

Example:

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: GitTarget
metadata:
  name: example-target
  namespace: default
spec:
  providerRef:
    name: example-provider
  branch: main
  path: live-cluster
```

If you enable `spec.encryption`, that applies to `Secret` resource writes for this target. For SOPS
and age details, see [sops-age-guide.md](sops-age-guide.md).

`spec.providerRef.kind` also allows `GitRepository`, but support for reading from Flux
`GitRepository` is not implemented yet.

## `WatchRule`

`WatchRule` is the normal namespaced watcher. It only watches resources in its own namespace and
writes them to the referenced `GitTarget`.

The important fields are:

- `spec.targetRef.name`: target to write to
- `spec.rules`: one or more resource-match rules

Each entry in `spec.rules` is a logical OR. A resource matching any rule is watched.

Example:

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: WatchRule
metadata:
  name: example-watchrule
  namespace: default
spec:
  targetRef:
    name: example-target
  rules:
    - operations: [CREATE, UPDATE, DELETE]
      apiGroups: [""]
      apiVersions: ["v1"]
      resources: ["configmaps", "secrets"]
```

Use `WatchRule` when the watched resources and the `GitTarget` live in the same namespace.

## `ClusterWatchRule`

`ClusterWatchRule` is the cluster-scoped variant. Use it when you need to watch:

- cluster-scoped resources such as `nodes`, `clusterroles`, or CRDs
- namespaced resources across multiple namespaces

Because it is cluster-scoped, its `targetRef` must include the namespace of the referenced
`GitTarget`.

Example:

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: ClusterWatchRule
metadata:
  name: cluster-audit
spec:
  targetRef:
    name: example-target
    namespace: default
  rules:
    - operations: [CREATE, UPDATE, DELETE]
      apiGroups: ["rbac.authorization.k8s.io"]
      apiVersions: ["v1"]
      resources: ["clusterroles", "clusterrolebindings"]
      scope: Cluster
```

Use this sparingly. It is the more powerful option and usually belongs to cluster-admin-managed
setups.

## Quickstart vs hand-managed resources

Keep using the [root README quickstart](../README.md#quick-start) when you want the fastest install
path. The chart's `quickstart` values create a starter `GitProvider`, `GitTarget`, and `WatchRule`
for you.

Move to hand-managed resources when you want:

- more than one `GitTarget`
- more than one watch rule
- cluster-scoped auditing with `ClusterWatchRule`
- direct control over `GitProvider.spec.commit`
- direct control over encryption settings

The chart value reference for the starter `quickstart` block lives in
[charts/gitops-reverser/README.md](../charts/gitops-reverser/README.md).

## What to read next

- [commit-signing.md](commit-signing.md) for signing behavior on Git hosting platforms
- [github-setup-guide.md](github-setup-guide.md) for GitHub auth setup
- [sops-age-guide.md](sops-age-guide.md) for `GitTarget.spec.encryption`
