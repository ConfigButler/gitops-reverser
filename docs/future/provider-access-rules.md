# Access between objects: who may reference what

> **future work**, 2026-10-23. Not built. **The user-facing part should be settled before the next
> release**, because until then renaming costs nothing (see [why now](#why-now)). This page
> replaces three drafts: `docs/reviews/20261023-gitbranch-claims-from-other-namespaces.md` (the
> proposal), `docs/reviews/20261023-gitbranch-claims-review.md` (its review), and
> `docs/future/gitprovider-branch-rules.md`. It follows up items 1 and 2 of "Still missing" in the
> [one-writer-per-branch status](../design/one-writer-per-branch-plan.md). It includes two reviews
> of 2026-10-23. The second made every grant explicit, made claiming a branch include sharing it,
> and dropped the phases.

## The goal

A platform team keeps one repository credential and one cluster connection in its own namespace.
It decides, per activity, which other namespaces may use them, and for what:

- **On the repository:** the platform holds `main` and decides who writes it. Each team claims
  `teams/<team>/*` branches and decides who collaborates on them. Any tenant may claim a
  `feature/*` branch.
- **On the cluster:** tenants mirror their own namespace and control the appliers that live in it.
  One audit namespace mirrors every namespace, and cluster-scoped resources. The platform namespace
  controls the `Kustomization`s in `flux-system`.

A team creates and deletes its own `GitBranch` in its own namespace when a grant allows it. No
credential leaves the platform namespace.

## Why now

| Field | In 0.53.0 | In the next release as it stands |
|---|---|---|
| `GitProvider.spec.allowedBranches` | Yes | Yes |
| `GitProvider.spec.accessFrom` | No | New (#428) |
| `KubeProvider.spec.accessFrom`, `.allowCrossNamespace` | On `ClusterProvider`, which the next release removes | New, on a new kind (#430) |

`allowedBranches` is the only released name among them, and `GitProvider` breaks in that release
anyway (`spec.receiver` is gone, and `GitBranch` takes over the branch). If `accessFrom` ships, a
later rename breaks released manifests. If the rename lands first, those names are never released.

## Access between objects: the whole picture

Every reference in `api/v1alpha3`, and whether it needs a grant. **An object that lends something
lists every grant explicitly: a credential, a branch, or reach into other namespaces.** A
reference that stays in its own namespace and lends nothing needs no grant. Kubernetes draws the
same line. A Pod mounts a Secret in its own namespace without consent, and Gateway API asks for a
`ReferenceGrant` only when a reference crosses namespaces.

| Reference | What it lends | Grant |
|---|---|---|
| `GitBranch.gitProviderRef` → `GitProvider` | A credential, and a branch of its repository | **`GitProvider.gitBranchesFrom`**, scoped by `branches` |
| `GitTarget.gitBranchRef` → `GitBranch` | Writing a folder into the branch | **`GitBranch.gitTargetsFrom`** |
| `GitTarget.kubeProviderRef` → `KubeProvider` | Reading a cluster with its credential | **`KubeProvider.gitTargetsFrom`**, scoped by `sourceNamespaces` and `clusterScoped` |
| `FluxClaim` / `ArgoCDClaim` `.kubeProviderRef` → `KubeProvider` | Control over an applier, with its credential | **`KubeProvider.applierClaimsFrom`**, scoped by `applierNamespaces` |
| `FluxClaim.kustomizationRef`, `ArgoCDClaim.applicationRef` → the applier in the source cluster | (points into a cluster whose objects we do not own) | Bounded by `applierNamespaces` |
| `WatchRule.rules[].sourceNamespace` → a source-cluster namespace | (same) | Bounded by `sourceNamespaces` |
| `ClusterWatchRule.gitTargetRef` → a `GitTarget` in any namespace | Cluster-scoped reads through that target's `KubeProvider` | Bounded by `clusterScoped` on the grant that admits the target |
| `WatchRule.gitTargetRef`, `CommitRequest.gitTargetRef` (own namespace only) | Nothing: what flows in is bounded by the `GitTarget`'s own grants | None |
| `GitTarget.applierClaimRef` (own namespace only) | Nothing new: the applier claim is already bound to one applier by its own grant | None |
| `secretRef`, `knownHostsRef` (own namespace only) | The Kubernetes convention for Secrets | None |
| `GitReceiver` | Nothing references it; a delivery only wakes a worker, which reads the remote with its own credential | None |
| `KubeProvider.inCluster` | The operator's own identity | Consent at install time (`--in-cluster-kube-providers`), unchanged |

The list is closed. A new kind that references a provider, or lends reach, gets a grant list on the
object it references, following the rules below.

## The rules

Every grant answers one question: **who may use this object, and for what?**

| Question | Answer |
|---|---|
| Where does permission live? | On the referenced object |
| Who gets access automatically? | Nobody, including the object's own namespace |
| What does an omitted or empty grant list mean? | No access |
| How do several grants combine? | Union; order never matters |
| What does an omitted scope mean? | The grant is refused where a scope is required |
| Who controls the grant? | Whoever may edit the referenced object |

The field is named after the referring kind, or after a family of kinds the API already defines.
"Applier claim" is such a family: there is no `ApplierClaim` kind, but a `GitTarget` points at one
with `applierClaimRef`, and `definitions.md` defines the term. The same `gitTargetsFrom` on two
objects always means "which `GitTarget`s may reference me"; the referenced object decides the
activity. The names follow [definitions.md](../definitions.md) rule 2, and avoid a bare "claim",
which `definitions.md` keeps apart as "branch claim" and "applier claim".

This means a little more YAML and fewer hidden rules. It replaces the phase 1 decision that a
`GitBranch` has no access list and its provider's `accessFrom` "governs both hops". The provider
decides who may claim, and the branch decides who may write.

### Semantics, field by field

| Field | Omitted | `[]` | A value | `"*"` |
|---|---|---|---|---|
| A grant list | No access | No access | Grants, combined by union | Not a value of a list |
| `names` | Not matched by name | Refused (CEL) | Exact control-cluster namespace names | Refused: use `selector: {}` |
| `selector` | Not matched by label | (not a list) | Label selector on control-cluster Namespaces | `selector: {}` matches every namespace |
| `names` and `selector` | A grant matches a namespace in `names` **or** matching `selector`; both omitted is refused (CEL) | | | |
| `GitProvider.gitBranchesFrom[].branches` | **Refused**: required | Refused | `filepath.Match` patterns | One path segment: `*` matches `main`; `teams/a` needs `teams/*` |
| `KubeProvider.gitTargetsFrom[].sourceNamespaces` | No namespaces | Refused | Exactly these source-cluster namespaces | Every namespace; also allows `rules[].sourceNamespace: "*"` |
| `KubeProvider.gitTargetsFrom[].clusterScoped` | `false` | (not a list) | `true`: may receive cluster-scoped resources from a `ClusterWatchRule` | (not a list) |
| `KubeProvider.applierClaimsFrom[].applierNamespaces` | **Refused**: required | Refused | Exactly these namespaces | Every namespace |
| `GitBranch.gitTargetsFrom[]` | no scope field | | | |

A `KubeProvider` `gitTargetsFrom` grant must set `sourceNamespaces`, `clusterScoped: true`, or
both. CEL refuses one that grants nothing. **An explicit list is the whole scope, never "these
plus your own".** `applierNamespaces: [flux-system]` means `flux-system` only. `["*"]` is a
deliberate, broad grant.

**`{namespace}` always means the referring object's namespace in the control cluster.** It is the
`GitBranch`'s in `branches`, the `GitTarget`'s in `sourceNamespaces`, and the applier claim's in
`applierNamespaces`. In a source-namespace or applier scope it expands to a literal namespace name
in the source cluster. In a remote cluster that is the namespace of the same name, as "own
namespace" means today. A namespace name cannot contain glob characters. CEL refuses any other
`{...}`, and any pattern `filepath.Match` rejects. `sourceNamespaces` and `applierNamespaces` take
exact names, `{namespace}`, or `"*"`, and no globs: a `WatchRule` source namespace is already a name
or `"*"`.

### Refusal messages name the grant

A refusal says what the grants allow and what was asked. A tenant should never need a broad grant
to find out why something failed:

- `GitBranch`: "namespace `team-a` may claim `teams/team-a/*`, `feature/*` through GitProvider
  `platform/shared-repo`; requested branch `main` is not allowed".
- `GitTarget` on a branch: "GitBranch `team-a/live` admits `team-a`, `team-b`; namespace `team-c` is
  not admitted (`spec.gitTargetsFrom`)".
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
  gitBranchesFrom:
    - names: [platform]
      branches: [main, "release/*"]
    - selector: {matchLabels: {platform.example.com/tenant: "true"}}
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
- **Naming the own namespace.** `names: [platform]` is a literal name, which a kustomize overlay
  that changes the namespace does not rewrite. A label `selector` avoids that.

## `GitBranch`: who may write through it

```yaml
kind: GitBranch
metadata: {name: live, namespace: team-a}
spec:
  gitProviderRef: {name: shared-repo, namespace: platform}
  branch: teams/team-a/live
  gitTargetsFrom:
    - names: [team-a]
---
kind: GitBranch
metadata: {name: main, namespace: platform}
spec:
  gitProviderRef: {name: shared-repo}
  branch: main
  gitTargetsFrom:
    - names: [release-team]
```

- **Claiming a branch delegates it, including the right to share it.** The platform decides which
  branches it delegates, and the team decides who collaborates there. Sharing `teams/team-a/live`
  with `team-b` means adding `team-b` to this list; the provider and its credential do not change.
- **To keep control of the writers, the platform owns the `GitBranch`,** as for `main` above. There
  is no provider-level writer policy beside this one. That would turn delegation into two
  policies that intersect.
- **The `GitBranch`'s own namespace needs a grant too.** `names: [team-a]` above is not implied.
- **`gitTargetsFrom` is mutable.** `gitProviderRef`, `branch` and `parentBranch` stay immutable,
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
  gitTargetsFrom:
    - selector: {matchLabels: {platform.example.com/tenant: "true"}}
      sourceNamespaces: ["{namespace}"]
    - names: [platform-audit]
      sourceNamespaces: ["*"]
      clusterScoped: true
  applierClaimsFrom:
    - selector: {matchLabels: {platform.example.com/tenant: "true"}}
      applierNamespaces: ["{namespace}"]
    - names: [platform]
      applierNamespaces: [flux-system]
```

- **`gitTargetsFrom`** admits a `GitTarget` to mirror through this provider. Its
  `sourceNamespaces` bound what the `WatchRule`s feeding that target may watch. A `WatchRule` feeds
  a `GitTarget` in its own namespace.
- **`clusterScoped: true`** lets a `ClusterWatchRule` mirror cluster-scoped resources through that
  `GitTarget`. Today a `ClusterWatchRule` needs only the target's admission. Only a cluster admin can
  create one, but without this field `sourceNamespaces: ["{namespace}"]` would look like the whole
  reach and would not be.
- **`applierClaimsFrom`** admits an applier claim, and its `applierNamespaces` bound where the
  `Kustomization` or `Application` it holds may live.
- **`allowCrossNamespace` and `accessFrom` are removed.** Mirroring and controlling appliers are
  granted separately: a namespace admitted to mirror is no longer admitted to apply.

The chart's `local` provider keeps today's default behavior with explicit grants:

```yaml
gitTargetsFrom:
  - selector: {}
    sourceNamespaces: ["{namespace}"]
applierClaimsFrom:
  - selector: {}
    applierNamespaces: ["{namespace}"]
```

The `inCluster` arm's consent (`--in-cluster-kube-providers`) is unchanged. A scope granted on an
`inCluster` provider still bypasses live namespace RBAC through the operator's identity, so the
configuration docs keep that warning for `sourceNamespaces`, `clusterScoped` and
`applierNamespaces`.

## Boundaries the grants do not replace

State these in the configuration docs, beside the grants:

- **A namespace grant trusts that namespace's administrators.** Anyone allowed to create the
  referring kind there can use the grant. Labels used in a `selector` must be controlled by trusted
  administrators. Kubernetes treats namespace isolation the same way: it comes from
  [RBAC and other controls](https://kubernetes.io/docs/concepts/security/multi-tenancy/), not from
  namespace names alone. A `names` grant also admits a namespace that is deleted and recreated with
  the same name.
- **Controlling an applier is not limiting its workloads.** `applierNamespaces` bounds where the
  `Kustomization` or `Application` lives. What it deploys, and where, is its own configuration and
  credentials: a Flux `Kustomization`'s `serviceAccountName` and `targetNamespace`, or an Argo CD
  `AppProject`'s destinations.
- **The repository's authority must be trusted.** The model assumes platform administrators manage
  `GitProvider`s, while tenants manage `GitBranch`es and `GitTarget`s. If tenants may also create
  providers, see [the limit](#the-limit-another-provider-on-the-same-repository): grants on one
  provider cannot stop a competing claim through another.

## Revocation

A grant can shrink in three ways: an edited list, a removed Namespace label, or a deleted object.
Shrinking can end admission altogether, or narrow a scope while the object stays admitted. For
example, `platform-audit` keeps a narrower grant after losing `"*"`. Both take effect at once. The
product already has two revocation paths, and both drain:

- **Deleting a `GitBranch`.**
  [`finalize`](../../internal/controller/gitbranch_controller.go) releases the claim first. It then
  retires the worker, which closes intake, pushes or refuses each pending write, and settles every
  `CommitRequest`. Last, it wakes the next holder.
- **A `GitTarget`, `WatchRule`, `ClusterWatchRule` or applier claim that loses admission or scope.**
  It is refused on its next reconcile. A write already decided is still committed and pushed
  ([`write_gate.go`](../../internal/git/write_gate.go) explains why: a write held back would come
  out of order later).

A `GitBranch` that loses `gitBranchesFrom` takes the `finalize` path but keeps its finalizer, and
reports `Stalled=True`, reason `NamespaceNotAuthorized`. Writes can therefore land after the API
server accepts a policy edit. Git cannot take back a push that is already sent, and the docs should
promise no more than that.

Two alternatives were rejected:

- **Refusing unpublished work at revocation.** It needs a worker shutdown with no flush, which
  nothing else uses.
- **Keeping a revoked claim reserved until released.** It needs a persisted claim, which belongs
  to the HA work.

### Authorization and priority are separate

- **Authorization: a `GitBranch` is a candidate only while a grant admits it.** An unadmitted one
  never opens a push session with the shared credential. Once a grant admits it, it proves itself
  and becomes a candidate like any other.
- **Priority: the oldest candidate by `creationTimestamp` holds the branch**, as it does today for a
  `GitBranch` whose proof succeeds late. Priority by admission time would need the time of
  admission to survive a restart, and the claim is not persisted. Priority by creation time is the
  only order a restart recomputes the same way.
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
platform's `GitBranch`, and the `gitTargetsFrom` on it, then report `BranchConflict` and govern
nothing.

State the limit wherever the grants are documented:

> Grants decide who may claim a branch through this provider, and who may write through this
> `GitBranch`. They cannot stop a claim on the same repository branch through another provider
> whose credential opens a push session.

Under the trust boundary above, platform administrators create providers, so this needs a copied
credential. If tenants must also create arbitrary providers, one governing authority per repository
becomes part of the design. Either of these would do:

- **A cluster-scoped object per repository URL.** This is option C of the original one-writer
  plan: `ClusterGitProvider`, or `GitCredential`.
- **A cluster-level rule that names one governing provider.** For a repository it names, branches
  may only be claimed through that `GitProvider`.

Neither exists.

## The change, surface by surface

One PR. Measured with `git grep` on 2026-10-23, outside `docs/reviews`: `accessFrom` is in about 75
files, `allowCrossNamespace` in about 50, and `allowedBranches` in about 40. Most of these are
docs, samples, the chart and e2e fixtures.

| Surface | Change |
|---|---|
| `api/v1alpha3` | `GitProvider`: drop `AccessFrom` and `AllowedBranches`; add `GitBranchesFrom []GitBranchGrant`. `GitBranch`: `gitProviderRef` becomes namespaced; add `GitTargetsFrom []NamespaceGrant`. `KubeProvider`: drop `AccessFrom` and `AllowCrossNamespace`; add `GitTargetsFrom []SourceGrant` and `ApplierClaimsFrom []ApplierGrant`. Each grant embeds `NamespaceMatcher` inline, with the CEL rules from the semantics table. Doc comments on `GitTarget`, `GitBranch`, `FluxClaim` and `ArgoCDClaim` that say `accessFrom` |
| Accessors | `GitProvider.AdmitsGitBranch(ns, labels, branch)`, `GitBranch.AdmitsGitTarget(ns, labels)`, `KubeProvider.AdmitsGitTarget` and `AdmitsApplierClaim`, each returning the scope that applies. They replace `AdmitsNamespace`, `AllowsCrossNamespaceSources` and `AllowsCrossNamespaceAppliers`. `{namespace}` expands in one helper |
| `internal/authz` | Every own-namespace shortcut goes: `gitBranchAdmitted`'s and `namespaceAdmitted`'s, and **`ApplierAdmitted`'s**, which returns early when the claim and the target share a provider and a namespace. It must now check `applierClaimsFrom` on its own. `source_namespace.go` checks `sourceNamespaces`, and `watchrule_compile.go` checks `clusterScoped`. Messages follow [refusal messages](#refusal-messages-name-the-grant) |
| Claim and worker | `GitBranch` admission is checked before proof and candidacy; revocation goes through `finalize` without removing the finalizer; `WorkerRequest` carries the resolved provider namespace (see implementation notes) |
| Controllers and watches | `WatchRule` and `GitBranch` get Namespace watches; `GitBranch`es are indexed by resolved provider key (see implementation notes) |
| CRDs and chart | Regenerate; the chart's `local` provider renders the explicit default above, and its values change; `config/kubeprovider-local.yaml`; samples |
| `hack/crdfields` | The field inventory |
| Docs | `configuration.md` (sharing section, the semantics table, `GitProvider`, `GitBranch`, `KubeProvider`, the boundaries), `security-model.md`, `rbac.md`, `quickstart.md`, `components.md`, `architecture.md`, the bi-directional and attribution guides, `docs/spec` |
| `definitions.md` | Remove `spec.accessFrom` from the fields mirrored from Flux word for word, and rewrite the line saying a provider's "`spec.accessFrom` admits the others". Add "grant". The `NamespaceMatcher` comment ("One field uses it") is already stale |
| `gitbranch-and-kubeprovider-plan.md` | Its "no `accessFrom` of its own" and "one policy governs both hops" for `GitBranch` are superseded; point there from here |
| `UPGRADING.md` | One entry: `allowedBranches` is replaced by `gitBranchesFrom`, and every `GitBranch`, `GitTarget` and applier claim now needs a grant, also in the provider's own namespace |
| e2e and fixtures | Every `accessFrom`, `allowCrossNamespace` and `allowedBranches` in `test/e2e` and `test/fixtures`; same-namespace fixtures gain explicit grants |

The worker and the Git write path change, so this is the high-risk case in `AGENTS.md`: run local
e2e before pushing.

## What works today, before any of this

Nothing refuses two `GitProvider`s for one URL in one namespace, so a platform can already isolate
teams on one repository with one Secret. It creates one provider per team, and creates the
`GitBranch`es itself:

```yaml
kind: GitProvider
metadata: {name: repo-team-a, namespace: platform}
spec:
  url: https://git.example.com/platform/config.git
  secretRef: {name: repository-auth}   # the one Secret every team's provider shares
  allowedBranches: ["teams/team-a/*"]
  accessFrom: {names: [team-a]}
```

The example in [configuration.md](../configuration.md#sharing-connections-across-namespaces) has
one provider with `accessFrom: {names: [team-a, team-b]}`, which lets each team write the other's
branch. Until this lands, the page should say so, or show the per-team form.

## Alternatives considered

| Alternative | Why not chosen |
|---|---|
| Keep `accessFrom` (Flux's spelling) | It does not say which act it grants, and it cannot grant mirroring without applying, or one branch without all |
| Admit the own namespace automatically, with its own namespace as the scope, and keep `allowedBranches` as a limit on every claim (the previous version of this page) | Hidden rules: the default differed per field, `[flux-system]` meant "flux-system plus your own", and a delegated branch needed two fields read together. Explicit grants show the whole policy in one place |
| Admit the own namespace with unlimited scope | Widens today's `KubeProvider` behavior, and loses the RBAC split where a team may create `GitBranch`es beside a provider it may not edit |
| A provider-level writer policy beside `GitBranch.gitTargetsFrom` | Two policies that intersect. The platform keeps control of a branch by owning its `GitBranch` |
| `gitTargetsFrom` on `GitProvider`, with `branches` per grant | A `GitTarget` does not reference the provider, so the decision reached through the branch |
| Rules keyed by branch on `GitProvider`, first match wins | It needs list order to keep `main` narrow beside a `*` rule. That order is invisible in a merged manifest, and the shape does not carry over to `KubeProvider` |
| Flat `branchClaimsFrom` beside `allowedBranches` and `accessFrom` | Three lists that interact. The effective rule for one branch is their intersection |
| A separate grant kind in the provider's namespace (`GitBranchGrant`, after Gateway API's `ReferenceGrant`) | Worth it only if someone needs separate RBAC for grants and connections; it adds a kind, a watch and grant-deletion handling |
| Two phases: the renames first, cross-namespace claims later | Ships an API whose own-namespace rules change again in the second phase. One PR settles the model once |
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
    holds for `ClusterWatchRule` and `clusterScoped`, mapped through the targets in that namespace.
  - **`GitBranch` needs a Namespace watch,** mapped to the `GitBranch`es in that namespace. From
    there, revocation reaches their `GitTarget`s through `gitTargetsOn`.
  - **Index `GitBranch`es by resolved provider key.** `gitTargetsOn` and `gitProviderToGitBranches`
    find a provider's `GitBranch`es in the provider's namespace only.
- **Credential resolution.**
  [`WorkerRequest.provider()`](../../internal/git/worker_manager.go) builds the provider key from
  `GitBranch.Namespace`. The resolved provider namespace has to be carried through
  `gitDestination`, `WorkerRequest` and worker creation. Test two cases: no provider in the tenant
  namespace, and a same-name provider there holding a different credential. Both must use the
  platform provider and its Secret.
- **One admission function per act, in `internal/authz`.** Each returns the decision, the scope
  that applies, and the grants for the refusal message. Call the `GitBranch` one before proof and
  candidacy. Keep distinct reasons for each case: reference missing, namespace not admitted, branch
  not allowed, proof failed, branch conflict.
- **Status.** `kubectl get gitbranch` should show the provider's namespace.

## Evidence before it is ready

- **Admission tables, per act, row by row from the semantics table.**
  - Own, matched and third namespaces for each of the four lists, with the own namespace refused
    when no grant matches it.
  - Omitted and `[]` for every field, with CEL refusing what it should.
  - `names` only, `selector` only, both (OR), neither (refused).
  - `{namespace}` in each of the three scopes, and pattern depth (`teams/*` against `teams/a/b`).
  - `sourceNamespaces: ["*"]` against `sourceNamespace: "*"`; exact names against `"*"`.
  - `applierNamespaces: [flux-system]` refusing the claim's own namespace.
  - `clusterScoped` false and true for a `ClusterWatchRule`.
- **Mirroring allowed, applying denied, on one provider.** A `GitTarget` and its applier claim in
  one namespace, through one `KubeProvider`, where `gitTargetsFrom` matches and
  `applierClaimsFrom` does not. The apply must be refused; today's `ApplierAdmitted` shortcut would
  allow it.
- **Scope narrowed while admission stays.** A namespace matched by two grants loses the label of
  the one granting `"*"`. Its `WatchRule`s stop watching the other namespaces through the watch,
  with the `GitTarget` still admitted. The same for `applierNamespaces` and `clusterScoped`.
- **Sharing.** Adding and removing a namespace from `GitBranch.gitTargetsFrom` wakes and refuses
  that namespace's `GitTarget`s through the watch.
- **Authorization before candidacy.** An unadmitted `GitBranch` never becomes a candidate and never
  opens a push session while it is unadmitted. Once admitted, it becomes a candidate. An older
  admitted one takes the branch by `creationTimestamp`, also after a restart.
- **Credential resolution.** The two cases in the implementation notes. Receiver routing still
  reaches the right worker.
- **Refusal messages.** Each act's message names the grants that apply and the requested value.
- **Revocation, driven by watch events.** A direct `Reconcile` call proves no event wiring. Cover:
  - grant edit, grant removal and label removal, with several candidates revoked by one edit;
  - queued events, open windows, decided writes, push retry, a push in flight, and waiting or
    attached `CommitRequest`s;
  - the successor waiting for retirement, and receiving none of the old work.
- **End to end.** A team claims and writes `teams/<team>/live`, and shares it with a second team.
  Claims on `main` and on another team's branch fail. A tenant mirrors its own namespace and is
  refused another. The audit namespace mirrors `*` and cluster-scoped resources. The chart's default
  `local` provider behaves as today.

## Open questions

1. **Branch-count limits.** A `teams/{namespace}/*` grant lets a team create any number of
   `GitBranch`es, each a worker with a clone. Exact names cap the count. Is a per-grant limit needed?
2. **`parentBranch` reads outside the grants.** A team's branch can be created from `main` although
   the team may not claim `main`. That looks intended, because it is how a feature branch starts.
   Confirm it, and document it.
3. **Path isolation.** Grants are per branch. Per-folder grants inside one branch stay out of scope;
   the overlap check only stops two targets claiming the same paths.
4. **Recreated namespaces.** A `names` grant admits a recreated namespace of the same name. That is
   stated as a boundary above. Bind to the UID only if someone needs it.
