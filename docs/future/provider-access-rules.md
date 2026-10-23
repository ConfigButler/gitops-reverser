# Who may reference what: `gitBranchesFrom`, `gitTargetsFrom`, `applierClaimsFrom`

> **future work**, 2026-10-23. Not built. **The user-facing part should be settled before the next
> release**, because until then renaming costs nothing (see [why now](#why-now)). This page
> replaces three drafts: `docs/reviews/20261023-gitbranch-claims-from-other-namespaces.md` (the
> proposal), `docs/reviews/20261023-gitbranch-claims-review.md` (its review), and
> `docs/future/gitprovider-branch-rules.md`. It follows up items 1 and 2 of "Still missing" in the
> [one-writer-per-branch status](../design/one-writer-per-branch-plan.md). A review of 2026-10-23
> corrected the defaults, the own-namespace rule, phase 1's watches and the regrant rule; this
> version includes those corrections.

## The goal

A platform team keeps one repository credential and one cluster connection in its own namespace.
It decides, per activity, which other namespaces may use them, and how far:

- **On the repository:** `main` is written by a few named namespaces. Each team claims and writes
  its own `teams/<team>/*` branches. Any tenant may claim a `feature/*` branch and share it with
  other tenants.
- **On the cluster:** tenants mirror their own namespace, and control the appliers that live in
  it. One audit namespace mirrors every namespace. The platform namespace may hold the
  `Kustomization`s in `flux-system`.

A team creates and deletes its own `GitBranch` in its own namespace when a grant allows it. No
credential leaves the platform namespace.

**Where an applier lives is not where it deploys.** `applierClaimsFrom` decides which namespace's
`Kustomization` or `Application` a claim may hold. What that applier then deploys, and where, is
the applier's own configuration and credentials: a Flux `Kustomization`'s `serviceAccountName`
and `targetNamespace`, or an Argo CD `AppProject`'s destinations. That is the workload boundary,
and these grants do not replace it.

## Why now

| Field | In 0.53.0 | In the next release as it stands |
|---|---|---|
| `GitProvider.spec.allowedBranches` | Yes | Yes |
| `GitProvider.spec.accessFrom` | No | New (#428) |
| `KubeProvider.spec.accessFrom`, `.allowCrossNamespace` | On `ClusterProvider`, which the next release removes | New, on a new kind (#430) |

`allowedBranches` is the only released name among them, and `GitProvider` breaks in that release
anyway (`spec.receiver` is gone, and `GitBranch` takes over the branch). If `accessFrom` ships, a
later rename breaks released manifests. If the rename lands first, those names are never released.

## The rule: consent lives on the object referenced

**Every object lists who may reference it. The field is named after the referring kind, or after a
family of kinds the API already defines.** That is where Flux keeps `accessFrom`, where a Gateway
listener keeps `allowedRoutes`, and where `KubeProvider` keeps it today. Each referrer does one
thing with what it references, so the field name also says which act is granted:

| Object | Referenced by | Field | Act it allows |
|---|---|---|---|
| `GitProvider` | `GitBranch` (`gitProviderRef`) | `gitBranchesFrom` | claim a branch with this credential |
| `GitBranch` | `GitTarget` (`gitBranchRef`) | `gitTargetsFrom` | write a folder into this branch |
| `KubeProvider` | `GitTarget` (`kubeProviderRef`) | `gitTargetsFrom` | mirror from this cluster |
| `KubeProvider` | applier claims: `FluxClaim`, `ArgoCDClaim` (`kubeProviderRef`) | `applierClaimsFrom` | hold an applier in this cluster |

There is no `ApplierClaim` kind. "Applier claim" is the family the API already names: a
`GitTarget` points at one with `applierClaimRef`, and `definitions.md` defines the term.

A `GitTarget` never names a `GitProvider`, so a `GitProvider` has no `gitTargetsFrom`: deciding there
would mean reaching through the branch. `GitReceiver` is referenced by nothing and has no list.

The names follow [definitions.md](../definitions.md) rule 2: a reference names the kind it points
at. They also avoid a bare "claim", which `definitions.md` keeps apart as "branch claim" and
"applier claim".

One decision covers each act, so two policies never cover the same act. This replaces the phase 1
rule that a `GitBranch` has no access list and its provider's `accessFrom` "governs both hops". Only
the shape changed: the provider decides who may claim, and the branch decides who may write.

## The grant contract

Each field is a list of grants. A grant names some namespaces, and may say how far they reach.

1. **The object's own namespace is admitted without a grant, with the default reach.** Today's
   rules for the own namespace stay as they are: a local `GitTarget` or applier claim reaches only
   its own namespace, and a local `GitBranch` stays within `allowedBranches`.
2. **A grant only ever adds.** A namespace's reach is its default reach plus the reach of every
   grant that matches it, and this also holds for the own namespace. The order of the list never
   matters.
3. **`names` OR `selector`.** A grant matches a namespace whose name is in `names`, or whose labels
   match `selector`. The two are alternatives, and a namespace needs only one of them. This is
   `NamespaceMatcher` as it is today.
4. **One owner writes each list whole** (`+listType=atomic`), so it needs no merge keys.

### Semantics, field by field

| Field | Omitted | `[]` | A value | `"*"` |
|---|---|---|---|---|
| A grant list (`gitTargetsFrom`, ...) | Own namespace only | Same as omitted | Grants as below | Not a value of a list |
| `names` | Not matched by name | Refused (CEL) | Exact namespace names | Refused: use `selector: {}` |
| `selector` | Not matched by label | (not a list) | Label selector on control-cluster Namespaces | `selector: {}` matches every namespace |
| `names` and `selector` both omitted | Refused (CEL): a grant must match something | | | |
| `gitBranchesFrom[].branches` | Every branch within `allowedBranches` | Refused (CEL) | `filepath.Match` patterns; `{namespace}` is the `GitBranch`'s namespace | One path segment: `*` matches `main`, never `teams/a` |
| `KubeProvider` `gitTargetsFrom[].sourceNamespaces` | Own namespace only | Refused (CEL) | Exact source-cluster names, added to the own namespace | Every namespace; also allows `rules[].sourceNamespace: "*"` |
| `KubeProvider` `applierClaimsFrom[].applierNamespaces` | Own namespace only | Refused (CEL) | Exact names, added to the own namespace | Every namespace |
| `GitBranch.gitTargetsFrom[]` | no reach field | | | |

Two defaults differ on purpose. A grant that omits `branches` gets every branch within
`allowedBranches`, because `allowedBranches` is already the limit. A grant that omits
`sourceNamespaces` or `applierNamespaces` gets the own namespace only, because nothing else limits
those. The configuration reference must show both side by side.

`sourceNamespaces` and `applierNamespaces` take exact names or `"*"`, never globs: a source
namespace in `WatchRule` is already a name or `"*"`. "Own namespace" in a remote cluster means the
namespace of the same name. `{namespace}` exists only in `branches`. A namespace name cannot contain
glob characters, and CEL refuses any other `{...}`.

## `GitProvider`: who may claim a branch

```yaml
kind: GitProvider
metadata: {name: platform-repo, namespace: platform}
spec:
  url: https://git.example.com/platform/config.git
  secretRef: {name: platform-repo-auth}
  allowedBranches: ["main", "release/*", "teams/*/*", "feature/*"]   # every claim stays inside
  gitBranchesFrom:                        # phase 2: who else may claim, and which branches
    - selector: {matchLabels: {tenant: "true"}}
      branches: ["teams/{namespace}/*", "feature/*"]
```

- **A `GitBranch` in the provider's namespace** may claim any branch within `allowedBranches`.
- **A `GitBranch` in a namespace a grant matches** may claim a branch within both `allowedBranches`
  and the grant's `branches`.
- **`accessFrom` is removed.** Who may write moves to the `GitBranch`.
- **`GitBranch.spec.gitProviderRef` becomes a `NamespacedObjectReference`** (phase 2). An omitted
  namespace means the `GitBranch`'s own. CEL's `self == oldSelf` treats an omitted namespace and an
  explicit own namespace as different values. Default the field at admission, or compare the
  resolved namespace in the immutability rule.

### Can `allowedBranches` go? What people need to configure

Removing `allowedBranches` was proposed on the assumption that whoever may create a `GitBranch`
beside a provider may also edit the provider. RBAC can separate the two: an admin can put a provider
and its Secret in a team's namespace and let the team create `GitBranch`es there, without letting
the team edit the provider. There, `allowedBranches` is an enforced limit. It is more than a guard
against typos.

Three ways to handle it, checked against what people configure:

| Need | Today | **K**: keep `allowedBranches` as the limit for every claim | **R**: remove it; own namespace needs a grant | **U**: remove it; own namespace unlimited |
|---|---|---|---|---|
| A team's own provider writes only `main` | `allowedBranches: [main]` | Same | `gitBranchesFrom: [{names: [<own namespace>], branches: [main]}]` | Not expressible |
| An admin provides the credential in a team's namespace; the team creates `GitBranch`es but must stay on `teams/a/*` | `allowedBranches` | Same | A grant naming the own namespace | **Lost**: the team may claim any branch, `main` included |
| A credential that must never claim `main`, whoever creates the `GitBranch` | `allowedBranches` without `main` | Same | Grant no branch that matches `main` | Lost for the own namespace |
| Delegate `teams/{namespace}/*` to tenants (phase 2) | Not possible | Grant with `branches`, inside the limit | Grant with `branches` | Grant with `branches` |
| Give one named namespace one fixed branch (phase 2) | Not possible | Grant with `names` and `branches` | Same | Same |
| The platform's own namespace claims `main` and `release/*` | `allowedBranches` | Same | A grant naming the own namespace | Implicit |
| Who may write into one branch | Provider-wide `accessFrom` | `GitBranch.gitTargetsFrom` in every option | | |
| Fields to read for one delegated branch | n/a | Two: the limit, then the grant | One | One |
| Same rule as the other lists (own namespace admitted, grants add) | n/a | Yes; `allowedBranches` is the own namespace's default reach and the limit on every grant | **No**: the one list where the own namespace is not admitted | Yes, with unlimited reach, unlike `KubeProvider` |
| Release note | n/a | None | `allowedBranches` removed | `allowedBranches` removed |

**U loses two needs and is out.** **R** keeps everything configurable, but in exchange:

- every provider, including the single-team case, has to name its own namespace in a manifest,
  which a kustomize overlay that changes the namespace does not rewrite;
- `GitProvider` becomes the one list without an implicit own namespace;
- `gitBranchesFrom` has to ship in phase 1 for the own namespace alone.

**K** keeps everything configurable with no change for anyone using it today. Its cost is reading
two fields for a delegated branch, and the two have separate jobs: what this credential may touch,
and who else may touch which part of it.

**Recommendation: K.** `allowedBranches` stays as it is, required, as the limit on every claim
through the provider. This answers the open question of whether to keep it as an "optional
ceiling", but as a limit on everyone rather than the own namespace alone. Choose R only if a single
access field per provider matters more than naming the own namespace in manifests.

## `GitBranch`: who may write into it

```yaml
kind: GitBranch
metadata: {name: main, namespace: platform}
spec:
  gitProviderRef: {name: platform-repo}
  branch: main
  gitTargetsFrom:
    - names: [release-team]
---
kind: GitBranch
metadata: {name: checkout-v2, namespace: team-a}
spec:
  gitProviderRef: {name: platform-repo, namespace: platform}   # phase 2
  branch: feature/checkout-v2
  gitTargetsFrom:
    - names: [team-b]                     # team-a shares its feature branch with team-b
```

- **A `GitTarget` in a matched namespace may write through this `GitBranch`.** A `GitTarget` in
  the `GitBranch`'s own namespace always may.
  [`gitBranchAdmitted`](../../internal/authz/kubeprovider_admission.go) already admits that case,
  and reads the list on the `GitBranch` it has already fetched instead of reading the provider.
- **`gitTargetsFrom` is mutable.** `gitProviderRef`, `branch` and `parentBranch` stay immutable,
  because together they are the branch's identity. The type comment "every field is immutable"
  changes with it.
- **Whoever owns the `GitBranch` decides who shares it.** In phase 1 a `GitBranch` lives beside its
  provider, so the provider's namespace decides; with an RBAC split, that is whoever may edit
  `GitBranch`es there. In phase 2 a team that claimed a branch decides who else may write into it
  with the platform's credential. That reaches nothing new: the writes stay inside the branch the
  platform delegated, and the team could write anything there itself. The platform can no longer
  keep team branches private, though. If it needs to, a `gitBranchesFrom` grant can carry a ceiling
  on how far the claimant may share. Leave that out until someone asks.

With this, the per-team-provider workaround (see [what works today](#what-works-today-before-any-of-this))
is no longer needed. One provider, with a `GitBranch` per branch, isolates teams.

## `KubeProvider`: who may mirror, and who may hold an applier

```yaml
kind: KubeProvider
metadata: {name: workspace-a, namespace: platform}
spec:
  secretRef: {name: workspace-a-kubeconfig}
  gitTargetsFrom:                         # who may mirror from this cluster
    - selector: {matchLabels: {tenant: "true"}}
    - names: [platform-audit]
      sourceNamespaces: ["*"]
  applierClaimsFrom:                      # who may hold an applier in this cluster
    - selector: {matchLabels: {tenant: "true"}}
    - names: [platform]                   # the own namespace, extended to flux-system
      applierNamespaces: [flux-system]
```

- **Tenants** mirror their own namespace and hold appliers that live in it. That is the default
  reach of every grant.
- **`platform-audit`** mirrors every namespace.
- **`platform`** is the provider's own namespace. It is admitted without a grant, but only to its own
  namespace. The grant naming it adds `flux-system`.
- **`allowCrossNamespace` is removed.** Its two booleans granted cross-namespace reach to every
  admitted namespace at once, own namespace included. The API docs had to warn that "the trust is
  in every admitted namespace". Reach per grant ends that warning. To keep today's
  `sources: true` for everyone, add `sourceNamespaces: ["*"]` to every grant, plus a grant naming
  the own namespace.
- **Splitting `accessFrom` changes one thing.** A namespace admitted to mirror is no longer admitted
  to hold an applier. Today one matcher lets it do both.

The `inCluster` arm's consent (`--in-cluster-kube-providers`) is unchanged. Reach granted on an
`inCluster` provider still bypasses live namespace RBAC through the operator's identity, so the
configuration docs keep that warning for `sourceNamespaces` and `applierNamespaces`.

## The rename, surface by surface

Measured with `git grep` on 2026-10-23, outside `docs/reviews`. `accessFrom` is in about 75 files,
`allowCrossNamespace` in about 50. With K, `allowedBranches` keeps its name and its meaning for the
own namespace. Most hits are docs, samples, chart and e2e fixtures.

| Surface | Change |
|---|---|
| `api/v1alpha3` | `GitProvider`: drop `AccessFrom`; add `GitBranchesFrom []GitBranchGrant` in phase 2. `GitBranch`: add `GitTargetsFrom []NamespaceGrant`. `KubeProvider`: drop `AccessFrom` and `AllowCrossNamespace`; add `GitTargetsFrom []SourceGrant` and `ApplierClaimsFrom []ApplierGrant`. Each grant embeds `NamespaceMatcher` inline, with the CEL rules from the semantics table. Doc comments on `GitTarget`, `GitBranch`, `FluxClaim` and `ArgoCDClaim` that say `accessFrom` |
| Accessors | `GitBranch.AdmitsGitTarget(ns, labels)`; `KubeProvider.AdmitsGitTarget` and `AdmitsApplierClaim`, each returning the reach that applies, replacing `AllowsCrossNamespaceSources` and `AllowsCrossNamespaceAppliers`; `GitProvider.AdmitsGitBranch(ns, labels, branch)` in phase 2. `GitProvider.AdmitsNamespace` goes |
| `internal/authz` | `gitBranchAdmitted` reads the `GitBranch` instead of its provider. **`ApplierAdmitted` loses its shortcut**: it returns early when the claim and the target share a provider and a namespace, because one matcher used to grant both acts. It must check `applierClaimsFrom` and `applierNamespaces` on its own. Also `namespaceAdmitted`, `ApplierClaimAdmitted` and `source_namespace.go`. Messages that print `(spec.accessFrom)` or `allowCrossNamespace` name the new field. The reasons `NamespaceNotAuthorized`, `SourceNamespaceNotAllowed` and `ApplierNamespaceNotAllowed` stay |
| Controllers and watches | **`WatchRule` gets a Namespace watch** (phase 1, see implementation notes). `gitDestinationToGitTargets`' comment about the provider's `accessFrom` |
| CRDs and chart | Regenerate; the chart's `local` provider and its values; `config/kubeprovider-local.yaml`; samples |
| `hack/crdfields` | The field inventory |
| Docs | `configuration.md` (sharing section, `GitProvider`, `GitBranch`, `KubeProvider`), `security-model.md`, `rbac.md`, `quickstart.md`, `components.md`, `architecture.md`, the bi-directional and attribution guides, `docs/spec` |
| `definitions.md` | Remove `spec.accessFrom` from the fields mirrored from Flux word for word, and rewrite the line saying a provider's "`spec.accessFrom` admits the others". The `NamespaceMatcher` comment ("One field uses it") is already stale |
| `gitbranch-and-kubeprovider-plan.md` | Its "no `accessFrom` of its own" and "one policy governs both hops" for `GitBranch` are superseded; point there from here |
| `UPGRADING.md` | Nothing with K: `accessFrom` and `allowCrossNamespace` were never released on these kinds, and `allowedBranches` is unchanged |
| e2e and fixtures | Every `accessFrom` and `allowCrossNamespace` in `test/e2e` and `test/fixtures`; `accessFrom` on a shared `GitProvider` moves to `gitTargetsFrom` on its `GitBranch`es |

## Phasing

1. **Before the next release: the user-facing shape.** All of it changes admission and its
   wiring only.
   - `GitBranch.gitTargetsFrom`, replacing `GitProvider.accessFrom`.
   - `KubeProvider` `gitTargetsFrom` and `applierClaimsFrom`, with per-grant reach. This replaces one
     matcher and two booleans in authz, and removes the `ApplierAdmitted` shortcut.
   - The `WatchRule` Namespace watch, so a reach that shrinks takes effect at once.
   - `GitBranch` stays beside its provider, so `gitBranchesFrom` is not added yet. A field the
     controller ignores would be a promise the API cannot keep.
2. **Then: teams claim their own branches.** Add `gitBranchesFrom`, make `gitProviderRef`
   namespaced, and handle claim revocation. All of it is additive.
3. **Separately: one authority per repository** (see [the limit](#the-limit-another-provider-on-the-same-repository)).

Phase 1 leaves the worker, the claim and the Git write path as they are, so lint and unit tests
plus CI e2e are enough. Phase 2 changes the worker, which is the high-risk case in `AGENTS.md`: run
local e2e before pushing.

## What works today, before any of this

Nothing refuses two `GitProvider`s for one URL in one namespace, so a platform can already isolate
teams on one repository with one Secret. It creates one provider per team, and creates the
`GitBranch`es itself:

```yaml
kind: GitProvider
metadata: {name: repo-team-a, namespace: platform}
spec:
  url: https://git.example.com/platform/config.git
  secretRef: {name: platform-repo-auth}   # the one Secret every team's provider shares
  allowedBranches: ["teams/team-a/*"]
  accessFrom: {names: [team-a]}
```

The example in [configuration.md](../configuration.md#sharing-connections-across-namespaces) has
one provider with `accessFrom: {names: [team-a, team-b]}`, which lets each team write the other's
branch. Until phase 1 lands, the page should say so, or show the per-team form.

## Revocation

A grant can shrink in three ways: an edited list, a removed Namespace label, or a deleted object.
Shrinking can end admission altogether, or only narrow a reach while the object stays admitted:
`platform-audit` keeps mirroring its own namespace after losing `"*"`. Both must take effect at
once. The product already has two revocation paths, and both drain:

- **Deleting a `GitBranch`.**
  [`finalize`](../../internal/controller/gitbranch_controller.go) releases the claim first. It then
  retires the worker, which closes intake, pushes or refuses each pending write, and settles every
  `CommitRequest`. Last, it wakes the next holder.
- **A `GitTarget`, `WatchRule` or applier claim that loses admission or reach.** It is refused on
  its next reconcile. A write already decided is still committed and pushed
  ([`write_gate.go`](../../internal/git/write_gate.go) explains why: a write held back would come
  out of order later).

Phase 1 changes only what is admitted, so the second path covers it, given the watch it needs. In
phase 2, a `GitBranch` that loses `gitBranchesFrom` takes the `finalize` path but keeps its
finalizer, and reports `Stalled=True`, reason `NamespaceNotAuthorized`. Writes can therefore land
after the API server accepts a policy edit. Git cannot take back a push that is already sent, and
the docs should promise no more than that.

Two alternatives were rejected:

- **Refusing unpublished work at revocation.** It needs a worker shutdown with no flush, which
  nothing else uses.
- **Keeping a revoked claim reserved until released.** It needs a persisted claim, which belongs
  to the HA work.

### Authorization and priority are separate

- **Authorization: a `GitBranch` is a candidate only while it is admitted.** An unadmitted one never
  opens a push session with the shared credential. Once a grant admits it, it proves itself and
  becomes a candidate like any other.
- **Priority: the oldest candidate by `creationTimestamp` holds the branch**, as it does today for a
  `GitBranch` whose proof succeeds late. Priority by admission time would need the time of
  admission to survive a restart, and the claim is not persisted. Priority by creation time is the
  only order a restart recomputes the same way.
- **The consequence:** granting a namespace that holds an old `GitBranch` for a branch someone else
  holds hands the branch to the old one once it proves itself. The live holder then reports
  `BranchConflict`. The configuration docs say so: remove stale `GitBranch`es before granting.

For phase 2, the implementation must get these right:

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
`platform-repo` decide who may claim **through that provider**. They do not stop a `GitBranch` that
claims `main` through a different `GitProvider` for the same URL. That provider can be in a tenant's
namespace with a copied token. On Gitea or Forgejo, a token that can only read is enough, because it
still opens a receive-pack session. If that `GitBranch` is older, or first, it holds the branch. The
platform's `GitBranch`, and the `gitTargetsFrom` on it, then report `BranchConflict` and govern
nothing.

Per-branch grants make that gap more visible, because "`main` only for the release team" reads as
absolute. State the limit wherever the grants are documented:

> Grants decide who may claim a branch through this provider, and who may write through this
> `GitBranch`. They cannot stop a claim on the same repository branch through another provider
> whose credential opens a push session.

Today a platform protects a branch by creating its `GitBranch` first. The fix is a single authority
per repository. Either of these would do:

- **A cluster-scoped object per repository URL.** This is option C of the original one-writer
  plan: `ClusterGitProvider`, or `GitCredential`.
- **A cluster-level rule that names one governing provider.** For a repository it names, branches
  may only be claimed through that `GitProvider`.

Neither exists.

## Alternatives considered

| Alternative | Why not chosen |
|---|---|
| Keep `accessFrom` (Flux's spelling) | It does not say which act it grants, and it cannot grant mirroring without applying, or one branch without all |
| Remove `allowedBranches` (options R and U above) | U loses two needs; R keeps them at the cost of naming the own namespace in every provider and an exception to the own-namespace rule |
| Own namespace admitted with unlimited reach (an earlier version of this page) | Widens today's `KubeProvider` behavior, where the own namespace also needs `allowCrossNamespace` to leave itself, and made a grant naming the own namespace meaningless |
| `gitTargetsFrom` on `GitProvider`, with `branches` per grant | A `GitTarget` does not reference the provider, so the decision reached through the branch; it needed branch patterns and `{namespace}` on the writer side too, and per-team providers stayed the only way to isolate before phase 2 |
| Rules keyed by branch on `GitProvider`, first match wins | It needs list order to keep `main` narrow beside a `*` rule. That order is invisible in a merged manifest, and the shape does not carry over to `KubeProvider` |
| Flat `branchClaimsFrom` beside `allowedBranches` and `accessFrom` | Three lists that interact. The effective rule for one branch is their intersection, which nobody can read at a glance |
| Keep `allowCrossNamespace` beside the split lists | Reach stays global across every admitted namespace, which is the warning its own docs carry |
| A separate grant kind in the provider's namespace (`GitBranchGrant`, after Gateway API's `ReferenceGrant`) | Worth it only if someone needs separate RBAC for grants and connections; it adds a kind, a watch and grant-deletion handling |
| Tenant-owned providers with their own credentials | Right when repositories separate tenants. On one shared repository, a copied repository-wide token can claim any branch (see the limit) |

## Implementation notes

- **Watches, phase 1.**
  - **Already wired:** a `GitBranch` event reaches the `GitTarget`s in every namespace that
    reference it ([`gitTargetsOn`](../../internal/controller/gittarget_controller.go)). A Namespace
    label change reaches that namespace's `GitTarget`s and applier claims (`namespaceToClaims`). A
    `KubeProvider` spec change reaches the `WatchRule`s (`kubeProviderToWatchRules`).
  - **Missing:** the `WatchRule` controller does not watch Namespaces. Its `GitTarget` watch fires
    only on a spec change or a readiness move. A label change that narrows `sourceNamespaces`
    while the `GitTarget` stays admitted therefore re-evaluates no `WatchRule`. Add a Namespace
    watch that maps to the `WatchRule`s in that namespace. Today this gap is harmless, because
    reach is provider-wide and a label change can only change admission.
- **Watches, phase 2.** `gitTargetsOn` and `gitProviderToGitBranches` find a provider's
  `GitBranch`es in the provider's namespace only. Index `GitBranch`es by resolved provider key, and
  use the index in both. The `GitBranch` controller does not watch Namespaces either: add a mapper
  to the `GitBranch`es in a namespace whose labels change. From there, revocation reaches their
  `GitTarget`s through `gitTargetsOn`.
- **Credential resolution (phase 2).**
  [`WorkerRequest.provider()`](../../internal/git/worker_manager.go) builds the provider key from
  `GitBranch.Namespace`. The resolved provider namespace has to be carried through
  `gitDestination`, `WorkerRequest` and worker creation. Test two cases: no provider in the tenant
  namespace, and a same-name provider there holding a different credential. Both must use the
  platform provider and its Secret.
- **One admission function per act, in `internal/authz`.** Each returns the decision and the reach
  that applies. Call the `GitBranch` one (phase 2) before proof and candidacy. Keep distinct reasons
  for each case: reference missing, namespace not admitted, branch not allowed, proof failed, branch
  conflict.
- **Status.** `kubectl get gitbranch` should show the provider's namespace once it can differ.

## Evidence before it is ready

- **Admission tables, per act, row by row from the semantics table.**
  - Own, matched and third namespaces, for each of the four lists.
  - Omitted and `[]` for every field, with CEL refusing the `[]` it should refuse.
  - `names` only, `selector` only, both (OR), neither (refused).
  - `{namespace}` expansion, and pattern depth (`teams/*` against `teams/a/b`).
  - `sourceNamespaces: ["*"]` against `sourceNamespace: "*"`; exact names against `"*"`.
  - The own namespace alone (own reach only) and with a grant naming it (extended).
  - A `GitBranch` claim inside `allowedBranches` but outside a grant's `branches`, and the reverse.
- **Mirroring allowed, applying denied, on one provider.** A `GitTarget` and its applier claim in
  one namespace, through one `KubeProvider`, where `gitTargetsFrom` matches and
  `applierClaimsFrom` does not. The apply must be refused; today's `ApplierAdmitted` shortcut would
  allow it.
- **Reach reduced while admission stays.** A namespace matched by two grants loses the label of
  the one granting `"*"`. Its `WatchRule`s stop watching other namespaces through the watch, with
  the `GitTarget` still admitted. The same for `applierNamespaces`.
- **Sharing edits.** Adding and removing a namespace from `GitBranch.gitTargetsFrom` wakes and
  refuses that namespace's `GitTarget`s through the watch.
- **Authorization before candidacy (phase 2).** An unadmitted `GitBranch` never becomes a candidate
  and never opens a push session while it is unadmitted. Once admitted, it becomes a candidate.
  An older admitted one takes the branch by `creationTimestamp`, also after a restart.
- **Credential resolution (phase 2).** The two cases in the implementation notes. Receiver routing
  still reaches the right worker.
- **Revocation, driven by watch events.** A direct `Reconcile` call proves no event wiring. Cover:
  - grant edit, grant removal and label removal, with several candidates revoked by one edit;
  - queued events, open windows, decided writes, push retry, a push in flight, and waiting or
    attached `CommitRequest`s;
  - the successor waiting for retirement, and receiving none of the old work.
- **End to end.** A team claims and writes `teams/<team>/live`, and shares a feature branch with a
  second team. Claims on `main` and on another team's branch fail. A tenant mirrors its own
  namespace and is refused another. The audit namespace mirrors `*`.

## Open questions

1. **`allowedBranches`: K or R.** The table above recommends K.
2. **A sharing ceiling (phase 2).** May a team that claimed a branch share it with anyone, or does
   its `gitBranchesFrom` grant bound whom it may admit?
3. **Recreated namespaces.** A `names` grant also admits a namespace that is deleted and recreated
   with the same name. Acceptable, or bind to the UID?
4. **Branch-count limits.** A `teams/{namespace}/*` grant lets a team create any number of
   `GitBranch`es, each a worker with a clone. Exact names cap the count. Is a per-grant limit needed?
5. **`parentBranch` reads outside the grants.** A team's branch can be created from `main` although
   the team may not write `main`. That looks intended, because it is how a feature branch starts.
   Confirm it, and document it.
6. **Path isolation.** Grants are per branch. Per-folder grants inside one branch stay out of scope;
   the overlap check only stops two targets claiming the same paths.
