# Access between objects: who may reference what

> **future work**, 2026-10-23. Not built. The proposed contract for who may use a provider or a
> branch, to land before the next release while the names are still free (see [why now](#why-now)).
> It replaces three drafts: `docs/reviews/20261023-gitbranch-claims-from-other-namespaces.md`,
> `docs/reviews/20261023-gitbranch-claims-review.md` and `docs/future/gitprovider-branch-rules.md`,
> and follows up items 1 and 2 of "Still missing" in the
> [one-writer-per-branch status](../design/one-writer-per-branch-plan.md).
> [Tests to add](#tests-to-add) is the checklist for the PR.

## How it works: one tenant, end to end

A platform team keeps one repository credential and one cluster connection in its namespace,
`platform`. Namespace `team-a` carries the label `platform.example.com/tenant: "true"`.

1. The platform allows tenants to claim `teams/<own namespace>/*` and to mirror their own
   namespace.
2. Team A creates a `GitBranch` for `teams/team-a/live`, and allows `GitTarget`s from its own
   namespace to write it.
3. Team A creates a `GitTarget` that references that branch and the platform's `KubeProvider`.
4. The `GitTarget` runs only while **both** grants allow it: the branch's writer grant and the
   provider's source grant.

```yaml
# platform: the credential, and who may claim which branches
kind: GitProvider
metadata: {name: shared-repo, namespace: platform}
spec:
  url: https://git.example.com/platform/config.git
  secretRef: {name: repository-auth}
  allowedGitBranches:
    - from: Selector
      selector: {matchLabels: {platform.example.com/tenant: "true"}}
      branches: ["teams/{namespace}/*"]
---
# platform: the cluster connection, and who may mirror which namespaces
kind: KubeProvider
metadata: {name: production, namespace: platform}
spec:
  secretRef: {name: production-auth}
  allowedGitTargets:
    - from: Selector
      selector: {matchLabels: {platform.example.com/tenant: "true"}}
      sourceNamespaces: ["{namespace}"]
---
# team-a: the branch it claims, and who may write it
kind: GitBranch
metadata: {name: live, namespace: team-a}
spec:
  gitProviderRef: {name: shared-repo, namespace: platform}
  branch: teams/team-a/live
  allowedGitTargets:
    - from: Same
---
# team-a: what to mirror, and where; a WatchRule in team-a selects what
kind: GitTarget
metadata: {name: apps, namespace: team-a}
spec:
  gitBranchRef: {name: live}
  kubeProviderRef: {name: production, namespace: platform}
  path: apps
```

| Request | Result | Because |
|---|---|---|
| `team-a`'s `GitBranch` claims `teams/team-a/live` | Allowed | The provider's grant selects `team-a`, and `teams/{namespace}/*` expands to `teams/team-a/*` |
| `team-a`'s `GitBranch` claims `main` or `teams/team-b/live` | Refused, `BranchNotAllowed` | No grant gives `team-a` either branch |
| `team-a`'s `GitTarget` writes through `team-a/live` | Allowed | `from: Same` on the `GitBranch` |
| `team-b`'s `GitTarget` writes through `team-a/live` | Refused, until team A adds `from: Names, names: [team-b]` | The branch's owner decides who shares it |
| A `WatchRule` in `team-a` watches `team-a` | Allowed | `sourceNamespaces: ["{namespace}"]` |
| A `WatchRule` in `team-a` watches `kube-system` | Refused, `SourceNamespaceNotAllowed` | The grant's scope is `team-a` only |
| A `GitTarget` in `platform` mirrors through `production` | Refused, `NamespaceNotAuthorized` | No grant admits `platform`; its own namespace is not automatic |

Two expressions name a namespace, and they name different ones:

| Expression | Whose namespace |
|---|---|
| `from: Same` | The object **containing** the grant: for this `GitBranch`, `team-a`; for the providers, `platform` |
| `{namespace}` in a scope | The object **asking** for access: for `team-a`'s `GitBranch`, `team-a` |

**Platform administrators manage provider grants and authorization labels. Branch owners manage
writer grants on their branches.** That split is why team A can share its branch without asking
the platform.

## The rules

Every grant answers one question: **who may use this object, and for what?** Grants exist only
where an object lends something: a credential, a branch, or reach into other namespaces (the
[whole picture](#access-between-objects-the-whole-picture) lists every reference).

This is stricter than Flux on purpose. Flux keeps consent on the referenced object too (`accessFrom`),
but its ACL helper admits every same-namespace reference. GitOps Reverser requires explicit consent
for provider and branch use, also within one namespace. Administering a provider and creating its
consumers can then carry different RBAC permissions.

| Question | Answer |
|---|---|
| Where does permission live? | On the referenced object |
| Who gets access automatically? | Nobody, including the object's own namespace |
| What does an omitted or empty grant list mean? | No access |
| How do several grants combine? | Union; order never matters |
| What does an omitted scope mean? | The grant is refused where a scope is required |
| Who controls the grant? | Whoever may edit the referenced object |
| Who owns a grant list? | One policy owner per object: every grant list is `+listType=atomic`, and `names`, `branches`, `sourceNamespaces`, `clusterResources` and `applierNamespaces` are `+listType=set` |
| Are there booleans? | No. A grant's source is an enum (`from`), and every scope is a list, so either can grow a value later without a new field ([API conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md): think twice about bool fields) |

The field is `allowed` plus the referring kind, or a family of kinds the API already defines,
followed by a `from` in each grant. That is Gateway API's `allowedRoutes.namespaces.from`, the
shape `from` is borrowed from. "Applier claim" is such a family: there is no `ApplierClaim` kind,
but a `GitTarget` points at one with `applierClaimRef`, and `definitions.md` defines the term.
The same `allowedGitTargets` on two objects always means "which `GitTarget`s may reference me";
the referenced object decides the activity. The names follow [definitions.md](../definitions.md)
rule 2, and avoid a bare "claim", which `definitions.md` keeps apart as "branch claim" and "applier
claim".

This means a little more YAML and fewer hidden rules. It replaces the phase 1 decision that a
`GitBranch` has no access list and its provider's `accessFrom` "governs both hops". The provider
decides who may claim, and the branch decides who may write.

### Where a grant admits from

Every grant has a required `from`. The values `Same`, `Selector` and `All` are Gateway API's
(`allowedRoutes.namespaces.from`); Gateway API makes `from` optional with `Same` as the default, and
here it is required, so no grant admits anyone by omission:

```yaml
allowedGitTargets:
  - from: Same                  # this object's own namespace, whatever it is named
  - from: Names
    names: [team-b]
  - from: Selector
    selector: {matchLabels: {platform.example.com/tenant: "true"}}
```

| `from` | Admits | Requires |
|---|---|---|
| `Same` | The referenced object's own namespace | nothing else |
| `Names` | Exactly these control-cluster namespaces | `names` |
| `Selector` | Control-cluster namespaces whose labels match | `selector` |
| `All` | Every namespace | nothing else |

CEL refuses `names` without `Names` and `selector` without `Selector`. "Team-b or every tenant" is
two grants, combined by union; one grant never combines two sources. `Same` is how an object admits
its own namespace without writing its name. A kustomize overlay that sets `namespace:` does not
rewrite a name inside `names`, so a literal own-namespace name breaks silently; `Same` does not.
`Names` has no Gateway API counterpart, which matches by the `kubernetes.io/metadata.name` label;
a plain list reads better. Admitting everyone takes `from: All`, a word a reviewer sees:
`from: Selector` with an empty `selector: {}` is refused.

### Semantics, field by field

| Field | Omitted | `[]` | A value | `"*"` |
|---|---|---|---|---|
| A grant list | No access | No access | Grants, combined by union | Not a value of a list |
| `from` | **Refused**: required | (not a list) | `Same`, `Names`, `Selector` or `All` | Refused: use `All` |
| `selector` (with `Selector`) | Refused | (not a list) | A label selector on control-cluster Namespaces | `selector: {}` is refused: use `from: All` |
| `names` (with `Names`) | Refused | Refused | Exact control-cluster namespace names; `{namespace}` is refused, because it would admit every referrer (use `Same` for the own namespace) | Refused: use `All` |
| `GitProvider.allowedGitBranches[].branches` | **Refused**: required | Refused | Branch patterns (see [patterns](#patterns)) | Every branch |
| `KubeProvider.allowedGitTargets[].sourceNamespaces` | No namespaces | Refused | Exactly these source-cluster namespaces | Every namespace; also allows `rules[].sourceNamespace: "*"` |
| `KubeProvider.allowedGitTargets[].clusterResources` | No cluster-scoped resources | Refused | Exactly these cluster-scoped resources, by identity (see [cluster-scoped resources](#cluster-scoped-resources)) | Every cluster-scoped resource, including CRDs installed later |
| `KubeProvider.allowedApplierClaims[].applierNamespaces` | **Refused**: required | Refused | Exactly these namespaces | Every namespace |
| `GitBranch.allowedGitTargets[]` | no scope field | | | |

A `KubeProvider` `allowedGitTargets` grant must set `sourceNamespaces`, `clusterResources`, or
both. CEL refuses one that grants nothing. **An explicit list is the whole scope, never "these
plus your own".** `applierNamespaces: [flux-system]` means `flux-system` only. `["*"]` is a
deliberate, broad grant.

### Patterns

A small language of our own, specified here and independent of the platform. Go's
`filepath.Match` was dropped: its `*` stops at `/`, and it brings character classes, `?` and
escaping, none of which a branch grant needs.

- **A branch pattern is matched against the whole branch name.** It is made of literal text, `*`
  and `{namespace}`.
- **`*` matches any sequence of characters, `/` included, and the empty one.** `feature/*`
  matches `feature/checkout/v2`, `teams/{namespace}/*` covers a team's whole prefix, and `*` alone
  is every branch.
- **`{namespace}` must be a whole path segment:** at the start or after a `/`, and at the end or
  before a `/`. `teams/{namespace}/*` is valid. `teams/{namespace}*` is refused, because for
  `team-a` it would also match `teams/team-ab/live`. This is placement's rule, "every value is a
  single path segment", applied to a grant.
- **No escaping, `?` or character classes.** Git refuses `*`, `?` and `[` in a branch name
  (`git check-ref-format`), so a literal never needs escaping.
- **`sourceNamespaces` and `applierNamespaces`** take exact names, `{namespace}` alone, or `"*"`
  alone. A `WatchRule` source namespace is already a name or `"*"`, so these scopes need no
  patterns.
- **CEL checks the grammar at admission:** allowed characters, `*`, and `{namespace}` as a whole
  segment. Any other `{...}` is refused with a message in placement's style, naming the unknown
  variable and the one that exists: "branch pattern `teams/{ns}/*` references unknown variable
  `{ns}`; the only variable is `{namespace}`".

**`{namespace}` always means the referring object's namespace in the control cluster.** It is the
`GitBranch`'s in `branches`, the `GitTarget`'s in `sourceNamespaces`, and the applier claim's in
`applierNamespaces`. In a source-namespace or applier scope it expands to a literal namespace name
in the source cluster. In a remote cluster that is the namespace of the same name, as "own
namespace" means today. A namespace name is a DNS label, so it is always valid inside a branch name.

The syntax is placement's: single braces, as in `{namespace}/{resource}/{name}.yaml`, so the API
has one variable syntax. The subject differs, and the configuration docs say so side by side.
In a placement template `{namespace}` is the mirrored resource's namespace, and can render
`_cluster`. In a grant it is the referring object's, which always exists. Placement's `|fallback`,
its sanitizing and its empty-segment dropping do not carry over: they exist to render a path, and a
grant renders nothing. The matcher is a few lines in `api/v1alpha3`. It
does not reuse the placement parser, which carries fourteen variables, fallbacks and label keys.

### Cluster-scoped resources

`clusterResources` is the one scope that names resource types, so it defines their identity:

- **A non-core resource is `resource.group`:** `clusterroles.rbac.authorization.k8s.io`,
  `customresourcedefinitions.apiextensions.k8s.io`. **A bare name is the core group only:**
  `nodes`, `namespaces`. A bare `clusterroles` therefore matches nothing.
- **Exact identities or `"*"`,** with no aliases and no partial wildcards. An identity covers every
  served version of its group and resource.
- **It is decided on the rule's text, as `sourceNamespace: "*"` is.** A `ClusterWatchRule` item
  whose `apiGroups` or `resources` holds `"*"` needs `clusterResources: ["*"]`. An item that names
  its groups and resources is checked pair by pair. An item that omits `apiGroups` resolves to one
  group by discovery, as today, and that pair is checked.
- **One forbidden type refuses the whole `ClusterWatchRule`,** with a message that names the
  forbidden types, as an invalid `objectSelector` refuses it today. Nothing is filtered silently.
- **Discovery never widens a grant.** Under a narrow grant no wildcard item compiles, so a CRD
  installed later cannot slip in. Under `["*"]`, a CRD installed later is included: that is what
  `"*"` grants.

### Refusal messages name the grant

A refusal says what the grants allow and what was asked. A tenant should never need a broad grant
to find out why something failed:

- `GitBranch`: "namespace `team-a` may claim `teams/team-a/*`, `feature/*` through GitProvider
  `platform/shared-repo`; requested branch `main` is not allowed".
- `GitTarget` on a branch: "GitBranch `team-a/live` admits `team-a`, `team-b`; namespace `team-c` is
  not admitted (`spec.allowedGitTargets`)".
- `WatchRule`: "KubeProvider `platform/production` lets namespace `team-a` watch `team-a`;
  `sourceNamespace: kube-system` is not allowed".

The reasons `NamespaceNotAuthorized`, `BranchNotAllowed`, `SourceNamespaceNotAllowed` and
`ApplierNamespaceNotAllowed` stay. Only their messages change.

## `GitProvider`: who may claim which branches

```yaml
kind: GitProvider
metadata: {name: shared-repo, namespace: platform}
spec:
  url: https://git.example.com/platform/config.git
  secretRef: {name: repository-auth}
  allowedGitBranches:
    - from: Same
      branches: [main, "release/*"]
    - from: Selector
      selector: {matchLabels: {platform.example.com/tenant: "true"}}
      branches: ["teams/{namespace}/*", "feature/*"]
```

- **The complete claim policy is in one place.** A `GitBranch` claims through this provider only if
  a grant matches its namespace and its branch. That includes the provider's own namespace, so a
  team that owns its provider writes one grant for itself.
- **`allowedBranches` and `accessFrom` are removed.** There is no separate limit and no implicit
  bypass for the platform. Who may write moves to the `GitBranch`.
- **`GitBranch.spec.gitProviderRef` becomes a `NamespacedObjectReference`.** An omitted namespace
  means the `GitBranch`'s own. CEL's `self == oldSelf` treats an omitted namespace and an explicit
  own namespace as different values. Default the field at admission, or compare the resolved
  namespace in the immutability rule.

## `GitBranch`: who may write through it

```yaml
kind: GitBranch
metadata: {name: live, namespace: team-a}
spec:
  gitProviderRef: {name: shared-repo, namespace: platform}
  branch: teams/team-a/live
  allowedGitTargets:
    - from: Same
---
kind: GitBranch
metadata: {name: main, namespace: platform}
spec:
  gitProviderRef: {name: shared-repo}
  branch: main
  allowedGitTargets:
    - from: Names
      names: [release-team]
```

- **Claiming a branch delegates it, including the right to share it.** The platform decides which
  branches it delegates, and the team decides who collaborates there. Sharing `teams/team-a/live`
  with `team-b` means adding a `from: Names` grant for it; the provider and its credential do not change.
- **To keep control of the writers, the platform owns the `GitBranch`,** as for `main` above. There
  is no provider-level writer policy beside this one. That would turn delegation into two
  policies that intersect.
- **The `GitBranch`'s own namespace needs a grant too.** `from: Same` above is not implied.
- **`allowedGitTargets` is mutable.** `gitProviderRef`, `branch` and `parentBranch` stay immutable,
  because together they are the branch's identity. The type comment "every field is immutable"
  changes with it.
- [`gitBranchAdmitted`](../../internal/authz/kubeprovider_admission.go) reads this list on the
  `GitBranch` it already fetches. It no longer reads the provider, and it no longer admits a target
  in the `GitBranch`'s namespace automatically.

## `KubeProvider`: who may mirror, and who may control appliers

```yaml
kind: KubeProvider
metadata: {name: production, namespace: platform}
spec:
  secretRef: {name: production-auth}
  allowedGitTargets:
    - from: Selector
      selector: {matchLabels: {platform.example.com/tenant: "true"}}
      sourceNamespaces: ["{namespace}"]
    - from: Names
      names: [platform-audit]
      sourceNamespaces: ["*"]
      clusterResources: ["*"]
  allowedApplierClaims:
    - from: Selector
      selector: {matchLabels: {platform.example.com/tenant: "true"}}
      applierNamespaces: ["{namespace}"]
    - from: Same
      applierNamespaces: [flux-system]
```

- **`allowedGitTargets`** admits a `GitTarget` to mirror through this provider. Its
  `sourceNamespaces` bound what the `WatchRule`s feeding that target may watch. A `WatchRule` feeds
  a `GitTarget` in its own namespace.
- **`clusterResources`** lets a `ClusterWatchRule` mirror those cluster-scoped resources through
  that `GitTarget` (see [cluster-scoped resources](#cluster-scoped-resources)). Today a
  `ClusterWatchRule` needs only the target's admission. Only a cluster admin can create one, but
  without this field `sourceNamespaces: ["{namespace}"]` would look like the whole reach and would
  not be.
- **`allowedApplierClaims`** admits an applier claim, and its `applierNamespaces` bound where the
  `Kustomization` or `Application` it holds may live.
- **`allowCrossNamespace` and `accessFrom` are removed.** Mirroring and controlling appliers are
  granted separately: a namespace admitted to mirror is no longer admitted to apply.

### The chart's `local` provider

The installer chooses which namespaces may use the operator's identity; the chart never chooses for
them.

- **Default install: empty grant lists.** Nothing mirrors or holds an applier through `local` until
  values grant it. The controller's refusal names the API field, because the
  same provider may be managed with Kustomize: "KubeProvider `gitops-reverser/local` admits no
  namespace in `spec.allowedGitTargets`". The chart docs map that field to
  `kubeProvider.allowedGitTargets`.
- **`quickstart.enabled=true`** also renders a grant for exactly the quickstart namespace, so the
  [quickstart](../quickstart.md) stays one `helm install`:

  ```yaml
  allowedGitTargets:
    - from: Names
      names: [gitops-reverser-quickstart-demo]
      sourceNamespaces: ["{namespace}"]
  allowedApplierClaims:
    - from: Names
      names: [gitops-reverser-quickstart-demo]
      applierNamespaces: ["{namespace}"]
  ```

- **`values-playground.yaml`,** shipped with the chart, grants every namespace its own namespace.
  That is today's default, chosen visibly with `-f values-playground.yaml`:

  ```yaml
  kubeProvider:
    allowedGitTargets:
      - from: All
        sourceNamespaces: ["{namespace}"]
    allowedApplierClaims:
      - from: All
        applierNamespaces: ["{namespace}"]
  ```

- **No default grants `clusterResources`,** the playground included. A `ClusterWatchRule` through
  `local` stops mirroring until a grant sets it. That is an intentional restriction, and
  `UPGRADING.md` says so.

The `inCluster` arm's consent (`--in-cluster-kube-providers`) is unchanged. A scope granted on an
`inCluster` provider still bypasses live namespace RBAC through the operator's identity, so the
configuration docs keep that warning for `sourceNamespaces`, `clusterResources` and
`applierNamespaces`.

## Boundaries the grants do not replace

State these in the configuration docs, beside the grants:

- **Every operation runs as the provider's identity, never as the referrer's.** Grants decide who
  may use that identity, and for what. They are not Flux-style impersonation, and this PR does not
  add impersonation.
  - A source-namespace grant permits mirroring the resource kinds the provider identity can read in
    that namespace. It does not reproduce the referring user's Kubernetes permissions.
  - An applier-namespace grant permits controlling matching appliers in that namespace. The
    applier's own execution identity and RBAC bound what it deploys: a Flux `Kustomization`'s
    `serviceAccountName`, or an Argo CD `AppProject`'s destinations. `targetNamespace` only places
    the workloads; the service account's permissions authorize them.
- **A namespace grant trusts that namespace's administrators.** Anyone, or any reconciler, allowed
  to create the referring kind there can use the grant. Labels used by a `Selector` grant must
  be controlled by trusted administrators. A `Names` grant also admits a namespace that is deleted and
  recreated with the same name.
- **Branch grants restrict claiming and writing. They do not make branches confidential from each
  other.** A `parentBranch` may name any branch the provider's credential can read, so a permitted
  parent's contents can be copied into the delegated branch. Inside one branch, the folder overlap
  check coordinates writers; it is not a boundary between tenants.

## A platform and its tenants

The model assumes one split, and RBAC enforces it. Platform administrators manage providers, their
grants and the authorization labels. Branch owners manage writer grants on their branches, and
tenants manage the objects that use both. With Flux, objects are applied by a
reconciler, so what counts is what each reconciler's identity may create. The platform's
reconciler applies:

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

The platform's reconciler also applies the `GitProvider` and `KubeProvider` above, with their
grants, and the `GitBranch` for `main`. The Role also needs whatever team-a's own workloads require.

| Identity | Manages | Cannot |
|---|---|---|
| The platform's reconciler | `GitProvider`, `KubeProvider` and their grants; Namespaces and their labels; tenant RBAC; platform `GitBranch`es such as `main` | |
| `team-a/tenant-reconciler` | `GitBranch`, `GitTarget`, `WatchRule`, applier claims and `CommitRequest`s in `team-a` | Create or edit a provider; label a Namespace; change RBAC |

The last column is what makes the grants hold. A tenant cannot grant itself a label, and cannot
create the second provider that [the limit](#the-limit-another-provider-on-the-same-repository)
describes. Flux's multi-tenancy lockdown (`--no-cross-namespace-refs` and
`--default-service-account` on kustomize-controller) makes the tenant identity mandatory, so no
tenant `Kustomization` falls back to the controller's own.

## Revocation

A grant can shrink in three ways: an edited list, a removed Namespace label, or a deleted object.
Shrinking can end admission altogether, or narrow a scope while the object stays admitted. For
example, `platform-audit` keeps a narrower grant after losing `"*"`. The promise, for both:

> Policy changes trigger reconciliation. Once revocation is observed, the controller stops
> admitting new work under the revoked permission. Work accepted before that may complete,
> following the drain below.

**Accepted** means what the branch worker already holds when the revocation is observed: queued
events, open commit windows, and decided writes. That matches what deleting a `GitBranch` drains
today. An event that arrives after the observation is refused. The tests rely on this line to tell
a permitted drain from a revocation bug. The product already has two revocation paths, and both
drain:

- **Deleting a `GitBranch`.**
  [`finalize`](../../internal/controller/gitbranch_controller.go) releases the claim first. It then
  retires the worker, which closes intake, pushes or refuses each pending write, and settles every
  `CommitRequest`. Last, it wakes the next holder.
- **A `GitTarget`, `WatchRule`, `ClusterWatchRule` or applier claim that loses admission or scope.**
  It is refused on its next reconcile. A write already decided is still committed and pushed
  ([`write_gate.go`](../../internal/git/write_gate.go) explains why: a write held back would come
  out of order later).

A `GitBranch` that loses `allowedGitBranches` takes the `finalize` path but keeps its finalizer, and
reports `Stalled=True`, reason `NamespaceNotAuthorized`. Writes can therefore land after the API
server accepts a policy edit. Git cannot take back a push that is already sent, and the docs should
promise no more than that.

**An applier claim that loses `allowedApplierClaims`:**

> Revocation stops new apply requests and starts handing back the existing hold. The hand-back
> restores the recorded prior state and leaves other owners' declarations alone. It may need retries
> and may fail; its outcome is visible in status and Events.

Today a claim that stops being `Configured` keeps its `Kustomization` suspended, and hands back only
once no eligible `GitTarget` references it or it is deleted. A denial now takes the same
[`retire`](../../internal/controller/fluxclaim_hold.go) path as "no eligible `GitTarget`". That path:

- keeps the claim the holder (`Held=False`, `Releasing`) while it hands back;
- keeps a `Kustomization` that was suspended before adoption suspended
  (`status.previousSuspendState`);
- gives up after `releaseDeadline` (10 minutes) with a Warning Event, as it already does for a live
  claim. It may be unable to finish: the Secret is gone, the identity lost its RBAC, or the cluster
  is unreachable.

A regrant during the hand-back lets it finish, then adopts the applier anew. A read error never
starts a hand-back; the controller already tells it apart from a denial (`kept` against
`falseCheck`). Once the applier is resumed, Flux or Argo CD may apply Git over live edits not
mirrored yet. That is what losing control of an applier means.

Two alternatives were rejected:

- **Refusing unpublished work at revocation.** It needs a worker shutdown with no flush, which
  nothing else uses.
- **Keeping a revoked claim reserved until released.** It needs a persisted claim, which belongs
  to the HA work.

### Authorization and priority are separate

- **Authorization: a `GitBranch` is a candidate only while a grant admits it.** An unadmitted one
  never opens a push session with the shared credential. Once a grant admits it, it proves itself
  and becomes a candidate like any other.
- **Priority: the oldest authorized candidate holds the branch,** by `creationTimestamp` and then
  UID, as it does today for a `GitBranch` whose proof succeeds late. That is a choice: it is
  deterministic, and a restart recomputes it from the objects alone.
- **The consequence:** granting a namespace that holds an old `GitBranch` for a branch someone else
  holds hands the branch to the old one once it proves itself. The live holder then reports
  `BranchConflict`. The configuration docs say so: remove stale `GitBranch`es before granting.

The implementation must get these right:

- **Revocation calls `Claims().Release(uid)`, not `Observe(..., false)`.** `Observe` keeps a
  holder's candidacy through a failed proof. A revoked candidate that does not hold the branch must
  stop being a candidate too.
- **A transient read error is not a revocation.** Keep the old answer, retry, and hand nothing off
  on an error alone.
- **A successor starts only after the old worker retires.** `EnsureWorker` already refuses while
  another `GitBranch`'s worker is live. Check the successor's admission again: one edit can revoke
  several candidates.
- **Pending work never moves to a new holder or credential.**

## The limit: another provider on the same repository

The claim key is the repository and the branch; the provider is not part of it. Grants on
`shared-repo` decide who may claim **through that provider**. They do not stop a `GitBranch` that
claims `main` through a different `GitProvider` for the same URL. That provider can be in a tenant's
namespace with a copied token. On Gitea or Forgejo, a token that can only read is enough, because it
still opens a receive-pack session. If that `GitBranch` is older, or first, it holds the branch. The
platform's `GitBranch`, and the `allowedGitTargets` on it, then report `BranchConflict` and govern
nothing.

State the limit wherever the grants are documented:

> Grants decide who may claim a branch through this provider, and who may write through this
> `GitBranch`. They cannot stop a claim on the same repository branch through another provider
> whose credential opens a push session.

In the [platform example](#a-platform-and-its-tenants), tenants cannot create providers, so a
competing claim needs someone with provider rights. If tenants must create their own providers, one
governing authority per repository becomes part of the design: a cluster-scoped object per
repository URL (option C of the original one-writer plan), or a cluster-level rule naming the one
provider through which a repository's branches may be claimed. Neither exists.

## Access between objects: the whole picture

Every reference in `api/v1alpha3`, and whether it needs a grant. **An object that lends something
lists every grant explicitly: a credential, a branch, or reach into other namespaces.** A
reference that stays in its own namespace and lends nothing needs no grant.

| Reference | What it lends | Grant |
|---|---|---|
| `GitBranch.gitProviderRef` → `GitProvider` | A credential, and a branch of its repository | **`GitProvider.allowedGitBranches`**, scoped by `branches` |
| `GitTarget.gitBranchRef` → `GitBranch` | Writing a folder into the branch | **`GitBranch.allowedGitTargets`** |
| `GitTarget.kubeProviderRef` → `KubeProvider` | Reading a cluster with its credential | **`KubeProvider.allowedGitTargets`**, scoped by `sourceNamespaces` and `clusterResources` |
| `FluxClaim` / `ArgoCDClaim` `.kubeProviderRef` → `KubeProvider` | Control over an applier, with its credential | **`KubeProvider.allowedApplierClaims`**, scoped by `applierNamespaces` |
| `FluxClaim.kustomizationRef`, `ArgoCDClaim.applicationRef` → the applier in the source cluster | (points into a cluster whose objects we do not own) | Bounded by `applierNamespaces` |
| `WatchRule.rules[].sourceNamespace` → a source-cluster namespace | (same) | Bounded by `sourceNamespaces` |
| `ClusterWatchRule.gitTargetRef` → a `GitTarget` in any namespace | Cluster-scoped reads through that target's `KubeProvider` | Bounded by `clusterResources` on the grant that admits the target |
| `WatchRule.gitTargetRef`, `CommitRequest.gitTargetRef` (own namespace only) | Nothing: what flows in is bounded by the `GitTarget`'s own grants | None |
| `GitTarget.applierClaimRef` (own namespace only) | Nothing new: the applier claim is already bound to one applier by its own grant | None |
| `secretRef`, `knownHostsRef` (own namespace only) | The Kubernetes convention for Secrets | None |
| `GitReceiver` | Nothing references it; a delivery only wakes a worker, which reads the remote with its own credential | None |
| `KubeProvider.inCluster` | The operator's own identity | Consent at install time (`--in-cluster-kube-providers`), unchanged |

The list is closed. A new kind that references a provider, or lends reach, gets a grant list on the
object it references, following [the rules](#the-rules).

## Why now

| Field | In 0.53.0 | In the next release as it stands |
|---|---|---|
| `GitProvider.spec.allowedBranches` | Yes | Yes |
| `GitProvider.spec.accessFrom` | No | New (#428) |
| `KubeProvider.spec.accessFrom`, `.allowCrossNamespace` | On `ClusterProvider`, which the next release removes | New, on a new kind (#430) |

`allowedBranches` is the only released name among them, and `GitProvider` breaks in that release
anyway (`spec.receiver` is gone, and `GitBranch` takes over the branch). If `accessFrom` ships, a
later rename breaks released manifests. If the rename lands first, those names are never released.

## The change, surface by surface

One PR. Measured with `git grep` on 2026-10-23, outside `docs/reviews`: `accessFrom` is in about 75
files, `allowCrossNamespace` in about 50, and `allowedBranches` in about 40. Most of these are
docs, samples, the chart and e2e fixtures.

| Surface | Change |
|---|---|
| `api/v1alpha3` | `GitProvider`: drop `AccessFrom` and `AllowedBranches`; add `AllowedGitBranches []GitBranchGrant`. `GitBranch`: `gitProviderRef` becomes namespaced; add `AllowedGitTargets []NamespaceGrant`. `KubeProvider`: drop `AccessFrom` and `AllowCrossNamespace`; add `AllowedGitTargets []SourceGrant` and `AllowedApplierClaims []ApplierGrant`. Each grant has `from` with `names` or `selector`, and the CEL rules from the semantics table. `NamespaceMatcher` goes: nothing else uses it. Doc comments on `GitTarget`, `GitBranch`, `FluxClaim` and `ArgoCDClaim` that say `accessFrom` |
| Accessors | `GitProvider.AdmitsGitBranch(ns, labels, branch)`, `GitBranch.AdmitsGitTarget(ns, labels)`, `KubeProvider.AdmitsGitTarget` and `AdmitsApplierClaim`, each returning the scope that applies. They replace `AdmitsNamespace`, `AllowsCrossNamespaceSources` and `AllowsCrossNamespaceAppliers`. One `from` matcher, and one pattern matcher with `{namespace}` expansion |
| `internal/authz` | Every own-namespace shortcut goes: `gitBranchAdmitted`'s and `namespaceAdmitted`'s, and **`ApplierAdmitted`'s**, which returns early when the claim and the target share a provider and a namespace. It must now check `allowedApplierClaims` on its own. `source_namespace.go` checks `sourceNamespaces`, and `watchrule_compile.go` checks `clusterResources`. Messages follow [refusal messages](#refusal-messages-name-the-grant) |
| Claim and worker | `GitBranch` admission is checked before proof and candidacy; revocation goes through `finalize` without removing the finalizer; `WorkerRequest` carries the resolved provider namespace (see implementation notes) |
| Applier claims | An explicit admission denial takes the `retire` path; a read error keeps the hold |
| Controllers and watches | `WatchRule` and `GitBranch` get Namespace watches; `GitBranch`es are indexed by resolved provider key (see implementation notes) |
| CRDs and chart | Regenerate. The chart's `local` provider gets empty grant lists by default, a grant for the quickstart namespace under `quickstart.enabled`, and `values-playground.yaml` (see [the chart's `local` provider](#the-charts-local-provider)). `config/kubeprovider-local.yaml` and the e2e install grant what the suite needs; samples |
| `hack/crdfields` | The field inventory |
| Docs | `configuration.md` (sharing section, the semantics table, `GitProvider`, `GitBranch`, `KubeProvider`, the boundaries, the platform example), `security-model.md`, `rbac.md`, `quickstart.md`, `components.md`, `architecture.md`, the bi-directional and attribution guides, `docs/spec` |
| `definitions.md` | Remove `spec.accessFrom` from the fields mirrored from Flux word for word, and rewrite the line saying a provider's "`spec.accessFrom` admits the others". Add "grant", and `from` among the names borrowed from Gateway API |
| `gitbranch-and-kubeprovider-plan.md` | Its "no `accessFrom` of its own" and "one policy governs both hops" for `GitBranch` are superseded; point there from here |
| `UPGRADING.md` | One entry: `allowedBranches` became `allowedGitBranches`, which also says who may claim; every `GitBranch`, `GitTarget` and applier claim now needs a grant, also in the provider's own namespace; the chart's `local` provider grants nothing unless values or the quickstart do; and a `ClusterWatchRule` needs `clusterResources` |
| e2e and fixtures | Every `accessFrom`, `allowCrossNamespace` and `allowedBranches` in `test/e2e` and `test/fixtures`; same-namespace fixtures gain explicit grants |

The worker and the Git write path change, so this is the high-risk case in `AGENTS.md`: run local
e2e before pushing.

## Until this lands

Today a platform isolates teams on one repository by creating one `GitProvider` per team, all
sharing one Secret, each with its own `allowedBranches` and `accessFrom`, and creating the
`GitBranch`es itself. The example in
[configuration.md](../configuration.md#sharing-connections-across-namespaces) uses one provider for
two teams, which lets each write the other's branch; the page should say so.

## Alternatives considered

| Alternative | Why not chosen |
|---|---|
| Keep `accessFrom` (Flux's spelling) | It does not say which act it grants, and it cannot grant mirroring without applying, or one branch without all |
| Admit the own namespace automatically, with its own namespace as the scope, and keep `allowedBranches` as a limit on every claim (the previous version of this page) | Hidden rules: the default differed per field, `[flux-system]` meant "flux-system plus your own", and a delegated branch needed two fields read together. Explicit grants show the whole policy in one place |
| Admit the own namespace with unlimited scope | Widens today's `KubeProvider` behavior, and loses the RBAC split where a team may create `GitBranch`es beside a provider it may not edit |
| A provider-level writer policy beside `GitBranch.allowedGitTargets` | Two policies that intersect. The platform keeps control of a branch by owning its `GitBranch` |
| `allowedGitTargets` on `GitProvider`, with `branches` per grant | A `GitTarget` does not reference the provider, so the decision reached through the branch |
| Rules keyed by branch on `GitProvider`, first match wins | It needs list order to keep `main` narrow beside a `*` rule. That order is invisible in a merged manifest, and the shape does not carry over to `KubeProvider` |
| Field names ending in `From` (`gitBranchesFrom`, an earlier version of this page) | With `from` in every grant, `gitBranchesFrom: [{from: Same}]` says "from" twice. `allowed` plus the kind reads as one sentence, and is Gateway API's own pattern |
| Flat `branchClaimsFrom` beside `allowedBranches` and `accessFrom` | Three lists that interact. The effective rule for one branch is their intersection |
| A separate grant kind in the provider's namespace (`GitBranchGrant`, after Gateway API's `ReferenceGrant`) | Worth it only if someone needs separate RBAC for grants and connections; it adds a kind, a watch and grant-deletion handling |
| Two phases: the renames first, cross-namespace claims later | Ships an API whose own-namespace rules change again in the second phase. One PR settles the model once |
| `filepath.Match` for branch patterns | Its `*` stops at `/`, so `feature/*` missed `feature/a/b` and nothing meant "every branch"; and it brings character classes, `?` and escaping that a grant never needs |
| A chart default that grants every namespace, or every namespace carrying a label | Every namespace silently gains use of the operator's identity. A label default quietly becomes a grant to everyone on platforms where tenants may label their own namespaces |
| `names` and `selector` side by side in one grant, and `clusterScoped: true` (the previous version of this page) | Two booleans in disguise: an unset field meant something, `selector: {}` meant everyone, and a bool cannot narrow later. `from` and list scopes say it outright |
| A per-grant limit on the number of `GitBranch`es | Quota and authorization have different owners. Overlapping grants make per-grant counting complicated, and a namespace `ResourceQuota` already counts custom resources |
| Binding `Names` grants to namespace UIDs | Breaks declarative provisioning, because the UID exists only after the namespace is created. Recreating a namespace is a trusted administrative act |
| Tenant-owned providers with their own credentials | Right when repositories separate tenants. On one shared repository, a copied repository-wide token can claim any branch (see the limit) |

## Implementation notes

- **Watches.**
  - **Already wired:** a `GitBranch` event reaches the `GitTarget`s in every namespace that
    reference it ([`gitTargetsOn`](../../internal/controller/gittarget_controller.go)). A Namespace
    label change reaches that namespace's `GitTarget`s and applier claims (`namespaceToClaims`). A
    `KubeProvider` spec change reaches the `WatchRule`s (`kubeProviderToWatchRules`).
  - **`WatchRule` needs a Namespace watch.** Its `GitTarget` watch fires only on a spec change or a
    readiness move, so a label change that narrows `sourceNamespaces` while the `GitTarget` stays
    admitted would re-evaluate no `WatchRule`. Map a Namespace to the `WatchRule`s in it. The same
    holds for `ClusterWatchRule` and `clusterResources`, mapped through the targets in that namespace.
  - **`GitBranch` needs a Namespace watch,** mapped to the `GitBranch`es in that namespace. From
    there, revocation reaches their `GitTarget`s through `gitTargetsOn`.
  - **Index `GitBranch`es by resolved provider key.** `gitTargetsOn` and `gitProviderToGitBranches`
    find a provider's `GitBranch`es in the provider's namespace only.
- **Credential resolution.**
  [`WorkerRequest.provider()`](../../internal/git/worker_manager.go) builds the provider key from
  `GitBranch.Namespace`. The resolved provider namespace has to be carried through
  `gitDestination`, `WorkerRequest` and worker creation.
- **One admission function per act, in `internal/authz`.** Each returns the decision, the scope
  that applies, and the grants for the refusal message. Call the `GitBranch` one before proof and
  candidacy. Keep distinct reasons for each case: reference missing, namespace not admitted, branch
  not allowed, proof failed, branch conflict.
- **Status.** `kubectl get gitbranch` should show the provider's namespace.

## Decided

The questions earlier versions left open, closed without a new grant field:

| Question | Decision | Why |
|---|---|---|
| Branch-count limits | No per-grant limit; a namespace `ResourceQuota` (`count/gitbranches.configbutler.ai: "20"`) | Quota and authorization have different owners. It bounds `GitBranch` objects in a namespace, across providers; repository size and churn stay unbounded. Exact branch names in a grant cap the distinct branches, while competing `GitBranch` objects for one branch stay uncounted |
| `parentBranch` outside the claim grant | Allowed, as far as the provider's credential can read | Creating `feature/x` from `main` should not need the right to claim `main`. The confidentiality boundary above says what follows |
| Path isolation | Out of scope | The delegated unit is the branch; the overlap check coordinates writers |
| Recreated namespaces | `Names` grants stay name-based, with no UID binding | It fits declarative provisioning; reusing a namespace is a trusted administrative act |

## Tests to add

The checklist for the PR. Each line is a behavior a later change could break without noticing.
Unit tests cover the matcher and the admission functions. Watch-driven tests run in envtest, driven
by watch events: a direct `Reconcile` call proves no event wiring. e2e covers what needs a Git
server and an applier.

**Patterns and CEL** (unit, plus CEL tests on the CRD):

- [ ] `teams/{namespace}/*` for `team-a` matches `teams/team-a/live` and **does not** match
  `teams/team-ab/live`.
- [ ] `teams/{namespace}*` is refused at admission (`{namespace}` is not a whole segment).
- [ ] `feature/*` matches `feature/checkout/v2`; `*` matches every branch, `main` included.
- [ ] `teams/*` matches `teams/a/b` (`*` crosses `/`); `main` does not match `main-old` (whole name).
- [ ] `teams/{ns}/*` is refused, and the message names `{ns}` and `{namespace}`.
- [ ] `[]` is refused for `names`, `branches`, `sourceNamespaces`, `clusterResources` and
  `applierNamespaces`; a grant without `from` is refused, and so are `names` without `Names` and
  `selector` without `Selector`; a `KubeProvider` `allowedGitTargets` grant with neither
  `sourceNamespaces` nor `clusterResources` is refused.
- [ ] `names: ["{namespace}"]` is refused, with a message pointing to `from: Same`.
- [ ] `from: Selector` with no `selector`, or with `selector: {}`, is refused (use `from: All`);
  a non-empty selector is accepted.
- [ ] `from: Same` admits the object's own namespace after a kustomize overlay moves the object to
  another namespace, and admits nothing else; `from: All` admits every namespace.

**Admission** (unit, `internal/authz`):

- [ ] The own namespace is refused when no grant names it, on all four lists. For a `GitBranch`,
  that includes a `GitTarget` in the `GitBranch`'s own namespace.
- [ ] `applierNamespaces: [flux-system]` refuses a claim on an applier in the claim's own
  namespace. An explicit list is the whole scope.
- [ ] `sourceNamespaces: ["{namespace}"]` refuses `sourceNamespace: "*"`, and `["*"]` allows it.
- [ ] Mirroring allowed, applying denied, on one `KubeProvider`, with the `GitTarget` and its applier
  claim in one namespace. Today's `ApplierAdmitted` shortcut would allow the apply.
- [ ] A `ClusterWatchRule` for RBAC `clusterroles` is refused through a target whose grant has no
  `clusterResources`, or only `[customresourcedefinitions.apiextensions.k8s.io]`, or a bare
  `[clusterroles]`. It compiles with `[clusterroles.rbac.authorization.k8s.io]` or `["*"]`.
- [ ] A `ClusterWatchRule` item with `resources: ["*"]` or `apiGroups: ["*"]` is refused under a
  narrow grant, and a CRD installed later is not mirrored. Under `["*"]` it is.
- [ ] A `ClusterWatchRule` naming one permitted and one forbidden type is refused whole, and the
  message names the forbidden type.
- [ ] Removing one identity from `clusterResources` refuses the rules naming it, while the
  `GitTarget` stays admitted for its other rules.
- [ ] Refusal messages name the grants that apply and the requested value, for each act.

**Claim and credential** (unit and envtest):

- [ ] An unadmitted `GitBranch` never opens a push session with the shared credential.
- [ ] A `GitBranch` admitted later becomes a candidate. If it is older than the holder, it takes the
  branch (`creationTimestamp`, then UID), also after a restart.
- [ ] A cross-namespace `GitBranch` pushes with the platform provider's Secret, both when its own
  namespace has no provider and when it has a same-name provider with a different credential.
- [ ] Receiver routing still wakes the worker of a cross-namespace `GitBranch`.

**Revocation** (envtest, driven by watch events):

- [ ] Removing a namespace from `GitBranch.allowedGitTargets` refuses that namespace's `GitTarget`s;
  adding it back admits them.
- [ ] Scope narrowed while admission stays: a namespace matched by two grants loses the label of the
  one granting `"*"`. Its `WatchRule`s stop watching the other namespaces, and the `GitTarget`
  stays admitted. This is the new `WatchRule` Namespace watch.
- [ ] A label removed from a claimant's namespace revokes its `GitBranch` through the new
  `GitBranch` Namespace watch, and its `GitTarget`s in other namespaces follow.
- [ ] A revoked `GitBranch`'s worker drains what it held when the revocation was observed. An event
  arriving afterwards is refused. The successor starts only after the old worker retires, and
  receives none of its work.
- [ ] One grant edit revokes two candidates; neither holds the branch afterwards.
- [ ] An applier claim denied by `allowedApplierClaims` hands its applier back. A read error on the
  `KubeProvider` keeps the hold.
- [ ] A `Kustomization` suspended before adoption stays suspended after a denial's hand-back.
- [ ] A hand-back that cannot finish (the identity lost its RBAC) shows in status and a Warning
  Event, and gives up at `releaseDeadline`.
- [ ] A regrant during the hand-back lets it finish, then adopts the applier anew.

**Recovery** (envtest; fixing the grant must be enough, with no consumer edited):

- [ ] Restoring a grant re-admits, each through its watch: a `GitBranch` (it proves itself and
  claims), a namespace's source scope, `clusterResources` for a `ClusterWatchRule`, and an applier
  claim (it adopts its applier).
- [ ] The same after a controller restart: restored grants are read fresh, and nothing revoked
  earlier stays refused.

**Chart and end to end:**

- [ ] A default install grants nothing: a `GitTarget` through `local` is refused with the message
  that names `spec.allowedGitTargets`.
- [ ] `quickstart.enabled=true` mirrors the quickstart namespace and nothing else.
- [ ] `values-playground.yaml` lets any namespace mirror its own namespace, and no other.
- [ ] e2e: a team claims `teams/<team>/live` through a platform provider and writes it, then shares it
  with a second team. Claims on `main` and on another team's branch are refused with their
  messages.
