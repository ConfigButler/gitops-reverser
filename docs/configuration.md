# Configuration Model

This guide explains the real configuration objects that drive gitops-reverser after the install
steps in the [root README](../README.md).

The short version:

- `GitProvider` defines where and how to push
- `GitTarget` defines which branch and repository path to write into
- `WatchRule` defines which namespaced resources should produce Git writes
- `ClusterWatchRule` does the same for cluster-scoped or cross-namespace watching
- `CommitRequest` optionally asks the operator to close the current commit window now

The chart's optional `quickstart` values are just a convenience layer that creates starter
instances of those same resources.

## Additional sensitive resources

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

## How the objects fit together

![Config basics diagram showing the relationship between GitProvider, GitTarget, and WatchRule](images/config-basics.excalidraw.svg)

The usual flow is:

1. Create a `GitProvider` for repository access and commit behavior.
2. Create a `GitTarget` that points at that provider plus a branch and repository path.
3. Create one or more `WatchRule` or `ClusterWatchRule` objects that point at that target.
4. Create a `CommitRequest` only when you want to flush an open window before the normal timer.

That means one repository connection can back multiple targets, and one target can be fed by
multiple watch rules.

## `GitProvider`

`GitProvider` defines the Git remote, credentials, allowed branches, push strategy, and commit
behavior.

The important fields are:

- `spec.url`: repository URL
- `spec.secretRef.name`: Secret with Git credentials such as SSH or HTTPS auth
- `spec.knownHostsRef`: optional ConfigMap/Secret with SSH `known_hosts` shared across providers
- `spec.allowedBranches`: branches this provider is allowed to write
- `spec.push.commitWindow`: rolling silence window that coalesces events into one commit per author
- `spec.commit`: committer identity, commit templates, and signing

Example:

```yaml
apiVersion: configbutler.ai/v1alpha2
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

### `GitProvider.spec.secretRef` — the credentials Secret

The referenced Secret holds the Git credentials. The examples use the **Kubernetes-native** keys,
which match the built-in Secret types and the tooling around them (`kubectl create secret generic
--type=…`, Sealed Secrets, External Secrets, SOPS):

| Auth | Keys |
|---|---|
| SSH | `ssh-privatekey` (+ optional `ssh-password` passphrase, `known_hosts`) |
| HTTP basic | `username` + `password` |
| HTTP bearer token | `bearerToken` (GitHub fine-grained PAT, GitLab access token; no username) |

#### Reusing a Flux or Argo CD credentials Secret

The credential reader's design is **inspired by both Flux and Argo CD**: it accepts their Secret key
names alongside the native ones, so you can reuse a Git credentials Secret you already have instead of
re-authoring it. The keys read for each auth method:

| Credential | Native key (recommended) | Flux key (also read) | Argo CD key (also read) |
|---|---|---|---|
| SSH private key | `ssh-privatekey` | `identity` | `sshPrivateKey` |
| SSH key passphrase | `ssh-password` | `password` *(when an SSH key is present)* | — *(unsupported by Argo)* |
| SSH host keys | `known_hosts` | `known_hosts` | external ConfigMap → supply via `spec.knownHostsRef` |
| HTTP basic auth | `username` + `password` | `username` + `password` | `username` + `password` |
| HTTP bearer token | `bearerToken` | `bearerToken` | `bearerToken` |

Auth precedence is SSH key → HTTP basic → bearer token. Client certificates (mTLS), custom CA
certificates, and GitHub App credentials are **not supported**.

> **A reused Secret needs write access.** Flux and Argo CD only *clone*, so their Git credentials are
> often read-only (a read-only deploy key, a read-scoped token). GitOps Reverser **pushes** commits,
> so a reused Secret's key or token must have **write** access on the repository — otherwise the
> commits will fail to push.

SSH host keys are resolved in priority order: the credentials Secret's own `known_hosts`, then
`spec.knownHostsRef` (a namespace-local ConfigMap or Secret keyed `known_hosts`, or `ssh_known_hosts`
for data copied out of Argo's `argocd-ssh-known-hosts-cm`), then an install-level default known-hosts
ConfigMap in the controller's namespace (`--default-known-hosts-configmap`). If none yields a valid
host key, SSH fails closed. Host-key rotation is an admin-owned declarative update; verify
fingerprints out of band. The controller flag `--insecure-allow-missing-known-hosts` relaxes this for
throwaway/dev clusters only — it permits SSH when **no** source provided any `known_hosts`; a
`known_hosts` that is present but unparseable is always a hard error.

### `GitProvider.spec.push`

`spec.push.commitWindow` controls how arriving events are grouped into commits. The timer resets
on every event; when it has been silent for the configured duration, the buffered events for a
given (author, gitTarget) are written as one commit. The default is `5s`. Setting `0s` opts into
per-event commits in the steady-state.

```yaml
spec:
  push:
    commitWindow: "5s"
```

A burst (e.g. `kubectl apply -k`, `helm upgrade`, an ArgoCD sync wave) becomes one commit per
author with a summary subject; isolated edits still produce one commit each.

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

There are three templates, one per commit shape:

- `spec.commit.message.eventTemplate`: per-event commits (only used when `commitWindow` is `0s`).
- `spec.commit.message.groupTemplate`: grouped commits produced by the commit window (the
  common case).
- `spec.commit.message.reconcileTemplate`: reconcile commits (the mark-and-sweep reconcile
  path; one commit per synced type).

```yaml
spec:
  commit:
    message:
      eventTemplate: "[{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Name}}"
      groupTemplate: "{{.Author}} on {{.GitTarget}}: {{.Count}} resource(s)"
      reconcileTemplate: "reconciled {{.Count}} {{.Resource}}"
```

`eventTemplate` can use:

- `Operation`
- `Group`
- `Version`
- `Resource`
- `Namespace`
- `Name`
- `APIVersion`
- `Username`
- `GitTarget`

`groupTemplate` can use:

- `Author`
- `GitTarget`
- `Count`
- `Operations` (map of `CREATE`/`UPDATE`/`DELETE` counts)
- `Resources` (slice of `{Group, Version, Resource, Namespace, Name}`)

`reconcileTemplate` can use:

- `Count`
- `GitTarget`
- `Group`
- `Version`
- `Resource`
- `APIVersion`
- `Revision`

`Group`/`Version`/`Resource`/`APIVersion` name the synced type for a per-type reconcile and
`Revision` is the cluster `resourceVersion` the reconcile was pinned to. The default,
`reconciled {{.Count}} {{if .Resource}}{{.Resource}}{{else}}resources{{end}}{{if .Revision}} (last resourceVersion: {{.Revision}}){{end}}`,
renders e.g. `reconciled 6 secrets (last resourceVersion: 1331)`. The type and revision fields are
empty for a whole-target reconcile or a pure sweep, so guard a template that references them
(the default uses `{{if .Resource}}` / `{{if .Revision}}`) to avoid an identity-less subject.

Examples:

```yaml
spec:
  commit:
    message:
      eventTemplate: "chore: [{{.Operation}}] {{.APIVersion}}/{{.Resource}}/{{.Name}}"
```

```yaml
spec:
  commit:
    message:
      eventTemplate: "[{{.Operation}}] {{.Resource}}/{{.Name}} ({{.Username}})"
```

```yaml
spec:
  commit:
    message:
      reconcileTemplate: "reconciled {{.Count}} {{.Resource}}@{{.Revision}}"
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
apiVersion: configbutler.ai/v1alpha2
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
- `spec.path`: required relative path inside the repository; use `.` only when you deliberately
  want the repository root
- `spec.encryption`: how `Secret` resources should be encrypted before commit

Example:

```yaml
apiVersion: configbutler.ai/v1alpha2
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

`spec.path` is required so a target never writes to the repository root by accident. Use a path
such as `live-cluster` for the first install. To deliberately target the repository root, set
`path: "."`. Do not use a leading slash, and do not add a trailing slash.

The target path is authoritative for snapshot reconciliation. A root target can create, update, and
delete managed manifest files at the repository root, so use `.` only for a repository layout that is
dedicated to this target.

If you enable `spec.encryption`, that applies to `Secret` resource writes for this target. For SOPS
and age details, see [sops-age-guide.md](sops-age-guide.md).

`spec.providerRef` references a `GitProvider` in the same namespace as the `GitTarget`. Its `group`
and `kind` default to `configbutler.ai` / `GitProvider`, so in practice you only set `name`.

The most useful status fields are:

- `Ready`: true when the target is valid, the Git path is accepted, and watched streams are running.
- `Reconciling`: true while initial replay, a recheck, or another coarse progress step is in flight.
- `Stalled`: true when the target is blocked until a human fixes configuration, RBAC, or Git path content.
- `Validated` and `EncryptionConfigured`: control-plane details.
- `StreamsRunning`: true when the source watches are past initial replay and routing live events.
- `GitPathAccepted`: true when the target Git path is safe to materialize.
- `status.streams`: bounded counts for tracked, running, replaying, and blocked streams.

Use conditions for automation.

## `WatchRule`

`WatchRule` is the normal namespaced watcher. It only watches resources in its own namespace and
writes them to the referenced `GitTarget`.

Status uses `ResourcesResolved` for selector resolution, `StreamsRunning` for source-watch readiness, and
`GitTargetReady` for the referenced target's write readiness. A rule can have `StreamsRunning=True` and
still remain `Ready=False` when its GitTarget reports `GitPathAccepted=False`.

The important fields are:

- `spec.targetRef.name`: target to write to
- `spec.rules`: one or more resource-match rules

Each entry in `spec.rules` is a logical OR. A resource matching any rule is watched. The rule fields
are:

- `operations`: `CREATE`, `UPDATE`, `DELETE`, or `*`; omitted means all operations.
- `apiGroups`: `""` for the core group, `*` for all groups, or omitted to resolve the named resource
  across the served API surface.
- `apiVersions`: a served version such as `v1`; omitted means the preferred served version.
- `resources`: plural resource names such as `configmaps`, `secrets`, or `*`.

Subresources such as `deployments/scale` are not valid rule resources. GitOps Reverser mirrors
top-level resources; selected subresource effects are handled separately by the controller.

Example:

```yaml
apiVersion: configbutler.ai/v1alpha2
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
apiVersion: configbutler.ai/v1alpha2
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

## `CommitRequest`

`CommitRequest` is a one-shot "save now" signal for a same-namespace `GitTarget`. It does not create
or change watch rules. Instead, it asks the branch worker to finalize a matching open commit window
for the request's author instead of waiting for `GitProvider.spec.push.commitWindow`.

The important fields are:

- `spec.targetRef.name`: target whose open window should be finalized
- `spec.message`: optional verbatim commit message
- `spec.delaySeconds`: optional 0-300 second collect grace after the request is attributed

Example:

```yaml
apiVersion: configbutler.ai/v1alpha2
kind: CommitRequest
metadata:
  name: save-now
  namespace: default
spec:
  targetRef:
    name: example-target
  message: "save default/example-target"
  delaySeconds: 2
```

The entire spec is immutable. Create a new `CommitRequest` for each save attempt.

Status moves from `WaitingForAuditEvent` to one terminal phase:

- `Committed`: a commit was pushed; `status.branch` and `status.sha` are set.
- `Rejected`: the request was handled correctly but produced no commit. `status.reason` is
  `NoWindowInGrace`, `WindowMismatch`, or `AlreadyPresent`.
- `Failed`: the finalize could not complete, for example because the request's own audit event was
  never observed and the operator could not attribute it to an author.

## Audit ingestion settings

Object state comes from Kubernetes **watch**, not from audit. Audit is an optional attribution lookup:
kube-apiserver posts audit events to a single HTTP path, `/audit-webhook`, and the operator extracts a
minimal attribution fact from each (auditID, user, verb, resourceVersion, GVR, namespace, name, UID,
status, timestamps) into a Redis attribution index keyed for the join. A resolver attaches the commit
author to each watch event by matching a fact (by resourceVersion/UID) within a bounded grace window.
The same Redis connection also stores per-watch resume cursors, so short reconnects can resume a normal
watch from the last processed resourceVersion when the apiserver can still serve that history.

Redis stores attribution facts and watch resume cursors. Leave `--audit-redis-addr` (chart
`queue.redis.addr`) empty to run **committer-only** (single-replica): the audit webhook is unused and
every commit is authored by the configured committer.

```yaml
queue:
  redis:
    addr: "valkey:6379"
    auth:
      existingSecret: "valkey-auth"
      existingSecretKey: "password"
```

The attribution flags tune the join:

- `--attribution-ttl` (default `10m`): how long an attribution fact is retained waiting for the
  matching watch event to join it.
- `--attribution-grace` (default `3s`): bounded per-event wait for a matching audit fact before a
  watch event ships authored by the committer.
- `--attribution-sa-naming` (`name` | `bot`): how a matched service account is named — `name` uses the
  service account's own username, `bot` collapses every service account to the committer.

```yaml
attribution:
  ttl: "10m"
  grace: "3s"
  serviceAccountNaming: "name"
```

## Quickstart vs hand-managed resources

Keep using the [root README quickstart](../README.md#quick-start) when you want the fastest install
path. The chart's `quickstart` values create a starter `GitProvider`, `GitTarget`, and `WatchRule`
for you.

The starter `GitTarget` writes under `live-cluster` by default. Override
`quickstart.gitTarget.path=.` only when you want the starter target to own the repository root.

Move to hand-managed resources when you want:

- more than one `GitTarget`
- more than one watch rule
- cluster-scoped auditing with `ClusterWatchRule`
- ad hoc save requests with `CommitRequest`
- direct control over `GitProvider.spec.commit`
- direct control over encryption settings

The chart value reference for the starter `quickstart` block lives in
[charts/gitops-reverser/README.md](../charts/gitops-reverser/README.md).

## What to read next

- [commit-signing.md](commit-signing.md) for signing behavior on Git hosting platforms
- [github-setup-guide.md](github-setup-guide.md) for GitHub auth setup
- [sops-age-guide.md](sops-age-guide.md) for `GitTarget.spec.encryption`
