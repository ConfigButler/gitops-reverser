# Configuration model

This guide explains the real configuration objects that drive gitops-reverser after the install
steps in the [root README](../README.md).

The short version:

- `GitProvider` defines where and how to push
- `ClusterProvider` defines the Kubernetes source cluster a target mirrors from
- `GitTarget` defines which branch and repository path to write into
- `WatchRule` defines which namespaced resources should produce Git writes, and in which source
  namespaces
- `ClusterWatchRule` does the same for cluster-scoped resources
- `CommitRequest` optionally asks the operator to close the current commit window now

The chart's optional `quickstart` values are just a convenience layer that creates starter
instances of those same resources.

For a first trial, use the root README quick start. It runs configured-author: Git writes work without
kube-apiserver audit delivery, and every commit uses the configured committer identity. Add audit
attribution later only when you need named Kubernetes users or service accounts in Git history.

## How the objects fit together

![Config basics diagram showing the relationship between GitProvider, GitTarget, and WatchRule](images/config-basics.excalidraw.svg)

The usual flow is:

1. Create a `GitProvider` for repository access and commit behavior.
2. Create a `ClusterProvider` for the source cluster, including the `default` provider when a target
   omits its source reference.
3. Create a `GitTarget` that points at the Git provider, source cluster, branch, and repository path.
4. Create one or more `WatchRule` or `ClusterWatchRule` objects that point at that target.
5. Create a `CommitRequest` only when you want to flush an open window before the normal timer.

That means one repository connection can back multiple targets, and one target can be fed by
multiple watch rules.

## Why the two provider types have different scopes

`GitProvider` and `ClusterProvider` are both named connections, but their scope follows what they
identify and who normally owns their credentials. API symmetry was not a goal. A Git
destination is normally a team's write boundary, so a namespaced `GitProvider` keeps the repository
credential and its consumers together. A source cluster is a shared physical identity: its client,
discovery surface, watch state, and attribution partition must mean the same thing to every target
that uses it. That makes `ClusterProvider` cluster-scoped.

| Object | Scope | What it represents | Why |
|---|---|---|---|
| `GitProvider` | Namespace | A Git destination and the credentials allowed to write it | A repository destination is normally owned by one team. Keeping the provider and its Secret in that team's namespace makes the ownership boundary direct. |
| `ClusterProvider` | Cluster | One physical Kubernetes source cluster | A source cluster can feed targets in several namespaces, while its connection, watch state, and attribution identity must stay the same everywhere. |

There is no default `GitProvider`: the operator cannot infer a safe repository, branch, or write
credential. `GitTarget.spec.clusterProviderRef` instead defaults to the conventionally opinionated
name `default`. That is a convenient, concrete reference. It does not claim that `default` is always
the local cluster.

`ClusterProvider.spec.allowedNamespaces` is the control-cluster authorization boundary for that
shared source connection: it determines which namespaces may contain `GitTarget`s that reference
the provider. It does not select namespaces in the source cluster or grant permissions there. If a
platform later needs a shared, platform-owned Git destination, that should be a separate
cluster-scoped Git-destination concept with an explicit ownership model, rather than changing the
meaning of the namespaced `GitProvider`.

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
apiVersion: configbutler.ai/v1alpha3
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

### `GitProvider.spec.secretRef`: the credentials Secret

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
| SSH key passphrase | `ssh-password` | `password` *(when an SSH key is present)* | *(unsupported by Argo)* |
| SSH host keys | `known_hosts` | `known_hosts` | external ConfigMap → supply via `spec.knownHostsRef` |
| HTTP basic auth | `username` + `password` | `username` + `password` | `username` + `password` |
| HTTP bearer token | `bearerToken` | `bearerToken` | `bearerToken` |

Auth precedence is SSH key → HTTP basic → bearer token. Client certificates (mTLS), custom CA
certificates, and GitHub App credentials are **not supported**.

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

`Username` is empty whenever no actor was named, both in configured-author mode and when
attribution ran and did not resolve. The `attribution-unresolved` sentinel is scoped to the Git
**author header** and deliberately does not reach templates or message bodies, so a template
rendering `{{.Username}}` never has to special-case it. Use `git log` (or
`author_kind="unresolved"`) to tell the two apart.

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
apiVersion: configbutler.ai/v1alpha3
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

## `ClusterProvider`

`ClusterProvider` names the Kubernetes cluster a `GitTarget` mirrors **from**. It is the read-side
peer of `GitProvider`: a target has one source cluster and one Git destination.

`default` is the conventionally opinionated provider name, not an operator-generated object and not
a synonym for the local cluster. Its only special behavior is that a `GitTarget` which omits
`spec.clusterProviderRef` references a user-created `ClusterProvider` named `default`. That provider
may omit `spec.kubeConfig` to use the operator's in-cluster configuration, or set it to mirror a
remote cluster.

For a remote source cluster, create a provider with a kubeconfig Secret. The Secret is resolved from
the operator's namespace; it is connection material for the operator, not a per-target setting.

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: ClusterProvider
metadata:
  name: prod-eu-1
spec:
  kubeConfig:
    secretRef:
      name: default-source-kubeconfig
  allowedNamespaces:
    names: [team-a]
    selector:
      matchLabels:
        gitops.configbutler.ai/source-access: "true"
```

`allowedNamespaces` is evaluated against namespaces in the **control cluster**, where
`GitTarget`s live. In this example, a `GitTarget` in `team-a`, or in a control-cluster namespace
with the shown label, may reference `prod-eu-1`. `names` and `selector` are ORed, and an omitted
policy admits no control-cluster namespace.

Which namespaces are read *from the source cluster* is bounded by
[`GitTarget.spec.allowedSourceNamespaces`](#bounding-which-source-namespaces-reach-a-target) when
that target declares one, and by the source connection's Kubernetes RBAC in every case: the
credential's own RBAC is always the hard maximum. A `WatchRule` may name a source namespace other
than its own only when this provider also sets:

```yaml
  # Deny-by-default. While false, a WatchRule mirroring through this provider may
  # watch only its OWN namespace, whatever any GitTarget policy says.
  allowSourceNamespaceOverride: true
```

That flag delegates the *choice* of source namespace to the `GitTarget`s this provider admits; it
grants nothing on its own, since the target must still admit the namespace. It is required for
**every** cross-source-namespace request, including `sourceNamespace: "*"`. Setting it on an
**in-cluster** provider is a much sharper decision than on a remote one: there the config plane *is*
the watched cluster, so it deliberately bypasses live namespace RBAC and lets the owner of an
admitted `GitTarget` mirror another namespace's objects into a Git destination they control. That is
legitimate to grant on purpose, which is why it is explicit and defaults to false.

`spec.kubeConfig` and `GitTarget.spec.clusterProviderRef` are immutable: changing either would silently
make an existing materialization mean a different source cluster. Rotate credential *contents* in the
referenced Secret instead. `qps` and `burst` optionally tune a remote provider's client; the
`ClusterProvider` conditions validate its configuration, while the consuming `GitTarget` reports the
live source reachability and stream state.

### Creating and managing the `default` provider

The operator **never creates a `ClusterProvider`**, and never re-creates one you delete. If a
`GitTarget` references a provider that does not exist (including `default`), the target is held
unready through the ordinary "provider not found" path. That is deliberate: a source cluster is a
connection with credentials and an authorization policy, so it is yours to declare, review, and roll
back like any other resource under GitOps.

There are two supported ways to get one, and both are fully declarative:

- **Commit it yourself.** The object above is ordinary YAML. Put it in the repository that manages
  this install. This is the recommended path once you are past a first trial.
- **Let the chart render it.** The chart can create and own a `ClusterProvider` named `default`,
  including its `allowedNamespaces`, from a single value. See
  [charts/gitops-reverser/README.md](../charts/gitops-reverser/README.md). Turn that value off to
  manage the object yourself. Helm then deletes the provider it created on the next upgrade, so
  ownership never silently splits between Helm and you. Because a missing provider holds its targets
  unready, plan that switch together with committing your own object.

The chart value is a rendering convenience, not runtime behavior: with it off, nothing in the
operator brings the object back.

The chart renders the `default` provider by default, including when its optional `quickstart` starter
resources are enabled. It gives the starter `GitTarget` a declared in-cluster source without adding a
source reference to its manifest. Turn `clusterProvider.createDefault` off only when you manage that
provider yourself.

Use another provider name when a target needs a different source cluster:

```yaml
spec:
  clusterProviderRef:
    name: prod-eu-1
```

The provider name is deliberately stable. It is the source-cluster identity used for watches and,
when audit attribution is enabled, for joining an audit event to the corresponding watch event.
Changing a target's source cluster changes what its folder means, so `clusterProviderRef` is
immutable.

## `GitTarget`

`GitTarget` decides where inside the repository resources are written.

The important fields are:

- `spec.providerRef`: which `GitProvider` backs this target
- `spec.clusterProviderRef`: which `ClusterProvider` supplies resources; omit it to reference the
  user-created `default` provider
- `spec.branch`: which allowed branch to write to
- `spec.path`: required relative path inside the repository; use `.` only when you deliberately
  want the repository root
- `spec.encryption`: how `Secret` resources should be encrypted before commit
- `spec.placement`: optional policy for where **new** resources are written (see
  [Where new resources are written](#where-new-resources-are-written-specplacement)); omit it to follow
  the repository's existing layout
- `spec.prune`: which deletion paths may remove documents from this target's folder (see
  [Deletion policy](#deletion-policy-specprunemode)); omit it for the safe default

Example:

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: example-target
  namespace: default
spec:
  providerRef:
    name: example-provider
  # Omit clusterProviderRef to reference the user-created ClusterProvider named "default".
  # clusterProviderRef: {name: prod-eu-1} selects a different source provider.
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

`spec.clusterProviderRef` references a cluster-scoped `ClusterProvider`. It defaults to
`{name: default}` when omitted. That is intentionally different from `providerRef`: a source cluster
is a shared physical identity, while a Git destination and its credential normally belong to the
target's namespace. The default name can represent either an in-cluster or remote source according
to the `ClusterProvider` the user created.

The most useful status fields are:

- `Ready`: true when the target is valid, the Git path is accepted, and watched streams are running.
- `Reconciling`: true while initial replay, a recheck, or another coarse progress step is in flight.
- `Stalled`: true when the target is blocked until a human fixes configuration, RBAC, or Git path content.
- `Validated` and `EncryptionConfigured`: control-plane details.
- `StreamsRunning`: true when the source watches are past initial replay and routing live events.
- `GitPathAccepted`: true when the target Git path is safe to materialize.
- `status.streams`: bounded counts for tracked, running, replaying, and blocked streams.
- `status.retention`: how many documents `spec.prune.mode` is keeping, and under which mode.

Use conditions for automation.

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
{"mode":"OnEvent","retainedDocuments":3,"observedTime":"2026-07-21T13:20:00Z"}
```

- `status.retention.retainedDocuments` is how many managed documents a converged mirror would not
  hold. `0` means a resync ran and found nothing to retain; an **absent** `retention` block means no
  resync has reported yet, which is not the same thing. `mode` is the *effective* mode the count was
  produced under, and the only place a `GitTarget` that predates `spec.prune` shows one at all.
- A throttled log line names the target, its path, and the scope (one per target folder per 10
  minutes; the full detail is at `-v1`).
- `gitopsreverser_prune_retained_documents_total`, labeled by `gittarget_namespace`,
  `gittarget_name`, and `prune_mode`.

`status.retention` covers the resync sweep only. Under `Never` a suppressed source DELETE is not
counted, so a `Never` target can report `0` while still declining to mirror deletes.

The count is refreshed when a resync runs, so it lags a change in the cluster until the next one.
Read `observedTime` before treating a `0` as live.

`spec.prune` is mutable (unlike `providerRef`, `branch`, and `path`), so a target can be moved to
`Always` once its watch scope is confirmed, without recreating it. Widening it to `Always` re-lists
the target's watched scopes, so the cleanup runs on the edit instead of waiting for the next replay.
Tightening it applies to the next write and leaves the streams alone, which is what makes it usable
as a stop button.

### Where new resources are written (`spec.placement`)

Placement decides the file path for a resource that has **no document in Git yet**. Once a document
exists, updates and deletes always edit it in place at its current location (found by manifest identity,
not path), so changing placement never moves an existing file; it only affects resources created after
the change.

#### How a path is chosen (the resolution ladder)

For each new resource the operator walks this order and stops at the first that produces a path:

1. **`spec.placement.byType[<exact type>]`:** an explicit template for that resource's type, if you
   declared one.
2. **`spec.placement.default`:** your explicit catch-all template, if you declared one.
3. **Sibling inference:** follow the layout the repository already uses for resources like this one
   (described next).
4. **Built-in canonical path:** `{namespace}/{group}/{resource}/{name}.yaml`, namespace first, the group
   omitted for core resources, no version segment, `_cluster/` in place of the namespace for
   cluster-scoped resources (an illegal namespace name, so it can never clash with a real one), and a
   `.sops.yaml` suffix for sensitive resources.

If you set **no** `spec.placement`, only steps 3 and 4 run, which is why pointing a target at an
existing repository "just continues" that repo's conventions, and a brand-new empty repo gets the tidy
canonical layout.

#### Following the existing layout (sibling inference)

This is the part that looks like magic but isn't: the operator never reverse-engineers a template. It
reads the files already in the target and makes two **observed** decisions for the new resource. *Which
directory* (the nearest cohort of resources like it: same type, then same type in any namespace) and
*one-file-or-bundle* (does that cohort keep one resource per file, or share a multi-document file?).

Worked example. A target at `spec.path: clusters/prod` already looks like:

```text
clusters/prod/
  all.yaml                       # 9 ConfigMaps in one multi-document file (a "bundle")
  team-a/secrets/db.sops.yaml    # one Secret, encrypted, one file per Secret
```

- A new **ConfigMap** `cache` arrives: its type-cohort (ConfigMaps) lives entirely in the `all.yaml`
  bundle → the new document is **appended to `all.yaml`**. No new file, no canonical tree is created.
- A new **Secret** `api-token` arrives: it is sensitive, so plaintext siblings are ignored; the only
  encrypted cohort is `team-a/secrets/` (one-per-file) → a new encrypted file
  **`team-a/secrets/api-token.sops.yaml`**.
- A new ConfigMap in a **brand-new namespace** `billing`: the ConfigMap cohort is still the `all.yaml`
  bundle, which is namespace-agnostic, so it is **appended to `all.yaml`** too, and the new namespace needs
  no new segment.

The boundaries that keep it predictable:

- A **sensitive** resource never infers from (or is appended into) a plaintext file; it only follows
  encrypted siblings, otherwise it uses the secure canonical path.
- A resource in a **namespace the target has never written before** only joins an existing cohort when
  that cohort has *proven* it is namespace-agnostic by already holding more than one namespace. One
  directory holding one namespace looks identical to a per-namespace layout whose second namespace has
  not arrived yet, so it is not treated as shared. The resource takes the canonical path, which
  carries its own namespace segment. Guessing here would file one namespace's objects under another's
  folder.
- When a type genuinely lives in two layouts at once, the tie-break is deterministic (the cohort with the
  most members wins, then the lexically smallest path), and it is never a coin-flip.
- Inference can only **continue** a layout that already exists. It cannot invent a greenfield one. "I
  want all ConfigMaps bundled even though none exist yet" is a job for `byType` below.

The full ladder, tie-break rules, and edge cases are in
[design/manifest/version2/gittarget-new-file-placement-rules.md](spec/gittarget-new-file-placement-rules.md);
the vision behind it is [design/manifest/file-agnostic-placement.md](spec/gittarget-new-file-placement-rules.md).

#### Declaring a layout (`byType` / `default`)

Set `spec.placement` when you want to **prescribe** a layout rather than follow the repo (for example a
greenfield repo, or a convention inference can't reach):

```yaml
spec:
  placement:
    byType:
      v1/configmaps: "{namespace}/configmaps.yaml"     # bundle every ConfigMap of a namespace into one file
      v1/secrets: "{namespace}/secrets/{name}.yaml"    # one file per Secret
    default: "{namespaceOrCluster}/{group}/{resource}/{name}.yaml"
```

- **`byType`** maps an exact `[group/]version/resource` key (core resources omit the group, e.g.
  `v1/configmaps`; grouped resources include it, e.g. `apps/v1/deployments`) to a path template.
- **`default`** is the template for any type with no `byType` entry. Omit it to fall through to sibling
  inference and then the built-in path.
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
| `{namespace}` | the resource's namespace; **empty** for a cluster-scoped resource | `team-a` |
| `{namespaceOrCluster}` | the namespace, or the literal `_cluster` for a cluster-scoped resource | `team-a` (a Node → `_cluster`) |
| `{resource}` | plural resource name | `deployments` |
| `{group}` | API group; **empty** for core resources | `apps` (a ConfigMap → empty) |
| `{groupPath}` | the API group as a path segment; equivalent to `{group}` today (the empty core-group segment is dropped either way) | `apps` |
| `{version}` | API version | `v1` |
| `{apiVersion}` | manifest `apiVersion`: `group/version`, or just `version` for core | `apps/v1` (a ConfigMap → `v1`) |
| `{kind}` | manifest kind | `Deployment` |
| `{scope}` | `namespaced` or `cluster` (a readable label, not a namespace-position value) | `namespaced` |
| `{sensitiveSuffix}` | `.sops.yaml` for a sensitive resource, `.yaml` otherwise | `.yaml` (a Secret → `.sops.yaml`) |

> **`{namespace}` vs `{namespaceOrCluster}`, the one to get right.** For a cluster-scoped resource
> `{namespace}` is **empty**, so its whole path segment vanishes: a template `{namespace}/{resource}/{name}.yaml`
> renders `clusterroles/admin.yaml` for a ClusterRole (no scope folder at all). Use `{namespaceOrCluster}`
> when a single template must also place cluster-scoped resources; it keeps a stable `_cluster/` segment
> (`_cluster/clusterroles/admin.yaml`) so namespaced and cluster-scoped resources stay cleanly separated.
> `{scope}` is a *descriptor* (`cluster`/`namespaced`), not a substitute, so don't use it as the folder for
> cluster resources.

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

Two kustomize shapes beyond that subset are **supported without being authored**:

- A **path-based strategic-merge `patches:` entry** is tolerated as read-only build context: the
  folder is accepted and what it renders is mirrored, but nothing is ever written into a patch
  file, and an edit to a field the patch *owns* is refused per object (not per folder).
- An **overlay that reads a base outside its own folder** (`resources: [../../base]`) is rendered
  by reading that base as read-only context; writes stay inside `spec.path`, and an image/replica
  edit lands on the overlay's own entry.

Everything else outside the modeled subset **refuses the whole target path before anything is
written**: inline or JSON6902 patches and the deprecated `patchesStrategicMerge`/`patchesJson6902`
spellings, generators, `components`, Helm fields, `replacements`, `transformers`,
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

- `spec.targetRef.name`: target to write to
- `spec.rules`: one or more resource-match rules
- `spec.rules[].sourceNamespace`: the source-cluster namespace that item watches; omitted means the
  rule's own namespace

### Watching a different source namespace

Set `spec.rules[].sourceNamespace` to mirror a namespace other than the one the `WatchRule` lives in
which is the case a shared config plane needs, where a tenant's configuration namespace and their source
namespace cannot share a name. It sits on the rule item, beside the resource selector it applies to,
so one `WatchRule` can follow different resource types in different namespaces.

| `rules[].sourceNamespace` | Meaning |
|---|---|
| omitted | the `WatchRule`'s own namespace: legacy behavior, byte for byte |
| an exact name | one source namespace |
| `"*"` | every namespace `GitTarget.spec.allowedSourceNamespaces` admits, resolved live |

Naming anything other than the rule's own namespace, **including `"*"`**, is authorized by three
things, all of which must hold:

1. the `GitTarget`'s namespace is admitted by its `ClusterProvider`'s `allowedNamespaces`;
2. that `ClusterProvider` sets `allowSourceNamespaceOverride: true`; and
3. the `GitTarget`'s `allowedSourceNamespaces` admits the namespace.

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata:
  name: repo-config
  namespace: tenant-acme
spec:
  targetRef:
    name: acme
  rules:
    - resources: [configmaps]        # omitted → tenant-acme, this rule's own namespace
    - resources: [secrets]
      sourceNamespace: repo-config   # one admitted source namespace
    - resources: [deployments]
      sourceNamespace: "*"           # every namespace the target admits, live
```

`"*"` never means "every namespace that exists": it expands to exactly what the target's policy
admits, so a target that declares no policy **denies** it. Each `"*"` item opens one watch stream per
(matched type × admitted namespace). That is deliberate, because it keeps every replay scoped to a single
namespace, but a real fan-out on a broad policy.

The outcome for all items is aggregated into one `SourceNamespaceAuthorized` condition, also shown by
`kubectl get watchrules -o wide`. A **denied** explicit name refuses the whole `WatchRule`
(`Ready=False`, `Stalled=True`, no streams) rather than silently trimming that item and mirroring
part of what you asked for; the message names the failing item by index and by what it selects. A
`"*"` that currently admits nothing is not a refusal: the rule stays `Ready` with reason
`NoAdmittedSourceNamespaces`, so a no-op rule is visible instead of looking healthy. Authorization is
re-evaluated on every reconcile, so tightening a policy revokes a running rule rather than only
affecting new ones.

This changes only which namespace is **watched**. Git placement always follows each mirrored
object's own namespace, so the rule above writes secrets under `repo-config/…`, not `tenant-acme/…`.

### Bounding which source namespaces reach a target

`GitTarget.spec.allowedSourceNamespaces` bounds which source-cluster namespaces may be mirrored into
that target by its `WatchRule`s:

```yaml
spec:
  allowedSourceNamespaces:
    names: [repo-config]
    selector:
      matchLabels:
        gitops.configbutler.ai/mirrorable: "true"
```

`names` and `selector` are ORed, and the selector matches labels on `Namespace`s in the **source**
cluster, so evaluating it needs `namespaces` `get`/`list`/`watch` for that cluster's credential.
Exact `names` keep working without that access, which is a deliberate degradation path.

It is also what `sourceNamespace: "*"` resolves *through*:

| Policy on the `GitTarget` | `sourceNamespace: "*"` resolves to |
|---|---|
| undeclared | **denied**, deny-by-default; the message names the fix |
| `{}` (declared, empty) | nothing |
| `names: [a, b]` | exactly `a` and `b`, statically, with no source-cluster access |
| `selector: {matchLabels: …}` | every source namespace carrying those labels, live |
| `selector: {}` | **every source namespace**: the deliberate "all namespaces" declaration |

That last row is how a destination owner says *every* source namespace, and it stays self-updating as
namespaces come and go. It is the replacement for the removed cluster-wide namespaced
`ClusterWatchRule`, and it is declared by the destination owner rather than by the rule author.

Two things about this field are easy to get wrong:

- **Omitted and empty differ.** Omitted declares no policy and a `WatchRule` keeps its own namespace.
  A declared-but-empty policy (`{}`) admits **nothing**.
- **A declared policy is exhaustive, with no self-namespace exception.** It must admit every namespace
  that may reach the target, *including* a co-resident legacy `WatchRule`'s own namespace. Adding a
  policy for one override therefore denies those rules until their namespace is admitted, loudly and with
  a message naming the fix, but it will happen.

A namespace allow-list cannot partition **cluster-scoped** objects, which have no namespace. A
`ClusterWatchRule` receives every such object its source credential can read, and this field is
neither consulted nor a bound for it. If a tenant must not see another tenant's cluster-scoped
objects, give each tenant its own `ClusterProvider` and credential, so that credential's RBAC is the
boundary.

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
apiVersion: configbutler.ai/v1alpha3
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

Use `WatchRule` for every **namespaced** resource, whether or not it lives in the `GitTarget`'s own
namespace.

## `ClusterWatchRule`

`ClusterWatchRule` is the **cluster-scoped** variant. Use it for cluster-scoped resources such as
`nodes`, `clusterroles`, or CRDs. It has no scope choice and no source-namespace selection: to mirror
namespaced resources across namespaces, use a `WatchRule` with
[`rules[].sourceNamespace`](#watching-a-different-source-namespace).

Because it is cluster-scoped, its `targetRef` must include the namespace of the referenced
`GitTarget`, and that namespace must be admitted by the target's `ClusterProvider`.

Example:

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: ClusterWatchRule
metadata:
  name: cluster-rbac
spec:
  targetRef:
    name: example-target
    namespace: default
  rules:
    - operations: [CREATE, UPDATE, DELETE]
      apiGroups: ["rbac.authorization.k8s.io"]
      apiVersions: ["v1"]
      resources: ["clusterroles", "clusterrolebindings"]
```

Cluster-scoped objects have no namespace, so `GitTarget.spec.allowedSourceNamespaces` does not bound
a `ClusterWatchRule` at all: it is intentionally cluster-global, limited only by its source
credential's Kubernetes RBAC. Use this sparingly. It grants the widest reach of any rule kind and usually belongs
to cluster-admin-managed setups.

> `spec.rules[].scope` is deprecated and accepts only `Cluster` (its default). Re-applying a
> pre-release manifest that still says `scope: Namespaced` is **rejected**. See
> [UPGRADING.md](UPGRADING.md) for the conversion.

## `CommitRequest`

`CommitRequest` is a one-shot "save now" signal for a same-namespace `GitTarget`. It does not create
or change watch rules. Instead, it asks the branch worker to finalize a matching open commit window
for the request's author instead of waiting for `GitProvider.spec.push.commitWindow`.

The important fields are:

- `spec.targetRef.name`: target whose open window should be finalized
- `spec.message`: optional verbatim commit message
- `spec.closeDelaySeconds`: optional 0-300 second delay before the open window is closed, after the
  request author is known, an extra collect window

Example:

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: CommitRequest
metadata:
  name: save-now
  namespace: default
spec:
  targetRef:
    name: example-target
  message: "save default/example-target"
  closeDelaySeconds: 2
```

The entire spec is immutable. Create a new `CommitRequest` for each save attempt.

Progress and outcome are reported through kstatus-compatible **conditions** (no `phase` string).
`kubectl get commitrequest` surfaces `Ready`, `AuthorAttributed`, and `Pushed`; `kubectl wait
--for=condition=Ready` blocks until the request settles:

- **Ready** (summary): `True` once the request reached a non-error terminal outcome. The `Ready`
  condition's `reason` says which: `Committed` (a commit was pushed; `status.branch`/`status.sha` set),
  or a benign no-commit: `NoWindowInGrace`, `WindowMismatch`, or `AlreadyPresent`. A failed finalize is
  `Ready=False` with reason `FinalizeFailed`.
- **Reconciling** / **Stalled**: the kstatus progress/blocked pair. `Reconciling=True` while the
  request is finalizing or waiting through `closeDelaySeconds`; `Stalled=True` when the finalize failed
  and needs attention (kstatus reports the object Failed).
- **AuthorAttributed**: `True` with reason `AttributedFromAdmission` when the internal commands
  admission webhook captured the request submitter. `False` with reason `CommitterFallback` means capture
  ran but no admission record exists; `False` with reason `AuthorCaptureDisabled` means capture is not
  configured. Neither is a failure. The request then claims no actor and can attach only to an unnamed
  watch window, whose Git author remains either the configured committer or the explicit unresolved author
  according to the watch attribution outcome.
- **Pushed**: `True` once the commit is in the remote repository.

## Audit ingestion settings

Object state comes from Kubernetes **watch**, not from audit. Audit is an optional attribution lookup:
kube-apiserver posts audit events to a named path, `/audit-webhook/<cluster-provider-name>`, and the
operator extracts a minimal attribution fact from each (auditID, user, verb, resourceVersion, GVR, namespace, name, UID,
status, timestamps) into a Redis attribution index keyed for the join. A resolver attaches the commit
author to each watch event by matching a fact (by resourceVersion/UID) within a bounded grace window.
The same Redis connection also stores per-watch resume cursors, so short reconnects can resume a normal
watch from the last processed resourceVersion when the apiserver can still serve that history.

Named ingress is currently authenticated to the shared audit CA and gated on the provider name existing;
it does **not** yet bind a particular client certificate to that provider. Do not use one shared audit
client credential to attribute several independently administered source clusters. A deployment that
needs that boundary should keep sources isolated until provider-bound ingress authentication is shipped.

### Route a shared audit stream by event annotation

Most audit streams represent one source cluster and must use a named route, including
`/audit-webhook/default`. Some control planes emit one shared stream for several logical clusters.
For that shape, the bare `/audit-webhook` endpoint is available only when the configuration model's
annotation key is set:

```yaml
attribution:
  auditRouteAnnotationKey: example.io/source-cluster
```

When this option is set, the receiver reads `example.io/source-cluster` from each event. Its value is
the **audit route** the event belongs to, so events in the same batch may route to different
partitions. A `ClusterProvider` joins a route by setting `spec.attribution.auditRoute` to the same
value; it defaults to the provider's own name.

**The bare endpoint never guesses a route.** Rejection happens at two levels:

| Situation | Result |
|---|---|
| A request reaches `/audit-webhook` while `auditRouteAnnotationKey` is unset | The whole request is rejected with **400**. The bare endpoint is not enabled, so a producer posting to it is misconfigured. |
| An event carries no annotation | That **event** is rejected: it produces no attribution fact and is never credited to a fallback route. The request still returns 200, so correctly-annotated events in the same batch are kept. |

An annotation naming a route no `ClusterProvider` has declared is **not** rejected. The route is a
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

Valkey/Redis is **optional in configured-author mode**: when `--redis-addr` is set, watch resume cursors are
stored so restarts pick up where they left off; when left empty, watches cold-replay from scratch on
restart instead. When author attribution is enabled (`--author-attribution=true`), a non-empty
`--redis-addr` is required: attribution facts and resume cursors both use the same connection. The Helm
chart defaults to **configured-author** (`attribution.enabled: false`): the audit webhook is unused and every
mirrored-resource commit is authored by the configured committer.

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
- `--author-attribution-grace` (default `3s`): bounded per-event wait for a matching audit fact before a
  watch event ships authored by the `attribution-unresolved` sentinel. Note the delivery floor: the
  apiserver's own `--audit-webhook-batch-max-wait` delays every fact by up to that much, so a grace at or
  below it will lose actors systematically.

A matched actor is always named by its own username, humans and service accounts alike (e.g.
`system:serviceaccount:flux-system:kustomize-controller`); there is no option to collapse service
accounts to the committer.

```yaml
attribution:
  ttl: "10m"
  grace: "3s"
```

## Quickstart vs hand-managed resources

Keep using the [root README quickstart](../README.md#quick-start) when you want the fastest first commit.
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
- [sops-age-guide.md](sops-age-guide.md) for `GitTarget.spec.encryption`
