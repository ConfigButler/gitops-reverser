# GitBranch, KubeProvider, and a phased API break

Status: **plan**, 2026-10-21. Phase 1 shipped in #429, phase 2 in #430, and phase 3 after them.
The current contract is in [architecture.md](../architecture.md) and
[configuration.md](../configuration.md). This page records what we decided and in what order we
build it.

> **Superseded in part.** That a `GitBranch` has no access list of its own, and that the
> `GitProvider`'s `accessFrom` governs both hops, no longer hold: grants on each referenced object
> replace `accessFrom`, `allowedBranches` and `allowCrossNamespace`
> ([who may reference what](../finished/provider-access-rules.md)).

The plan breaks the API on purpose, more than once. There are no production users to carry across,
so each phase replaces a shape outright instead of keeping a deprecated twin beside it. A removed
field stays in the schema only so that setting it is refused by name, the pattern the API already
uses for `qps`, `burst`, and `allowAnySourceNamespace`.

## Decisions

| Decision | Choice |
|---|---|
| The repository branch gets its own kind | `GitBranch`, namespaced, referenced as `gitBranchRef` |
| A `GitBranch` names its credential | `gitProviderRef` in its own namespace only |
| A `GitTarget` names its branch | `gitBranchRef` is required; `gitProviderRef`, `branch`, `parentBranch` leave the target |
| Who may write a branch | One `GitBranch` per repository branch, and it must prove push access to claim it |
| Changing a branch's identity | `gitProviderRef`, `branch`, and `parentBranch` are immutable; replace the object instead |
| Deleting a `GitBranch` | Never deletes the branch on the remote |
| The Kubernetes connection | `KubeProvider`, namespaced, replacing `ClusterProvider` |
| Its credential | A flat `secretRef` to a kubeconfig Secret in the provider's namespace |
| The operator's own identity | An explicit `inCluster: {}` arm, honored only where the installation authorizes it |
| References between kinds | Explicit everywhere; no `default` name, no fallback |

## The API after phase 2

```text
                       GitProvider (credential, allowedBranches, accessFrom)
                            ^
                            | gitProviderRef (same namespace)
                            |
GitTarget --gitBranchRef--> GitBranch (repository branch: one per repo+branch, one branch worker)
    |
    +--kubeProviderRef--> KubeProvider (source cluster: kubeconfig or inCluster, accessFrom)
    |
    +--applyTriggerRef--> FluxTrigger / ArgoCDTrigger --kubeProviderRef--> KubeProvider
```

A `GitTarget` is a folder: what it mirrors, where in the branch it writes, how it places, prunes,
encrypts, and commits, and which applier it holds. Everything that is one fact per repository
branch lives on the `GitBranch`. Everything that is one fact per credential lives on the provider.

| Field or status | Moves to | Why |
|---|---|---|
| `GitTarget.spec.gitProviderRef`, `.branch`, `.parentBranch` | `GitBranch.spec` | Two targets can no longer disagree about one branch |
| `GitTarget.status.remote` | `GitBranch.status.remote` | It describes the branch, so N targets carried N copies |
| `GitProvider.status.branches` | Removed | Listing `GitBranch` objects answers the same question |
| `GitProvider.spec.commit` (committer, signing) | Stays | The signing key belongs to the account the credential is for |
| `GitProvider.spec.allowedBranches` | Stays | It is the ceiling every `GitBranch` must fit under |
| `GitTarget.spec.commit.window`, `.commit.message`, `.suspend` | Stays | They describe the folder |

## Phase 0: merge the shared `GitProvider`

PR #428 keys the branch worker by repository and branch, adds `GitProvider.spec.accessFrom`, and
lets a `GitTarget` name a `GitProvider` in another namespace. Phase 1 builds on all three. Its one
known gap is that the oldest `GitTarget` decides which credential owns a branch, so any namespace
can squat a repository branch without a credential. Phase 1 closes that, so do not cut a release
from main between the two unless that gap is acceptable for it.

## Phase 1: `GitBranch`

### Shape

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitBranch
metadata:
  name: customer-main
  namespace: platform-connections
spec:
  gitProviderRef:
    name: customer-config   # a GitProvider in this namespace
  branch: main              # must match the provider's allowedBranches
  parentBranch: develop     # optional; only used to create a missing branch
status:
  observedGeneration: 3
  conditions: []            # Ready, Reconciling, Stalled
  repository: git.example.com/acme/config   # normalized; half of the claim key
  remote:
    commit: 4f1c...
    lastVerifiedAt: "2026-10-21T09:12:00Z"
    verifiedBy: Push
    parent: {}              # unchanged shape, moved from GitTarget
  gitTargetCount: 2
---
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: settings
  namespace: tenant-a
spec:
  gitBranchRef:
    name: customer-main
    namespace: platform-connections   # omitted means the target's own namespace
  path: tenants/a
```

The schema requires `gitProviderRef.name` and `branch`. CEL keeps all three spec fields immutable
(`self == oldSelf`), the rule `GitTarget` applies to these fields today. On the target,
`gitBranchRef` becomes immutable the same way, and `path` stays immutable.

`gitProviderRef` is a `LocalObjectReference`. Creating a `GitBranch` therefore requires the right to
create objects in the namespace that holds the credential, and that is what makes a branch claim
an act of the credential's owner. A `GitBranch` has no `accessFrom` of its own: a `GitTarget` in
another namespace may reference it when the branch's `GitProvider` admits the target's namespace.
One policy governs both hops, so it cannot be split across two objects that disagree.

### Who owns a repository branch

The claim key is the provider's repository URL, as `internal/repourl` normalizes it, plus the
branch. The `GitBranch` UID holds the claim, so a recreated object with the same name starts
without it.

1. A `GitBranch` becomes a candidate once its `GitProvider` is ready and its credential opens a
   receive-pack session on the remote. On GitHub and GitLab that requires push permission, so a
   provider with a missing or wrong Secret, or none on a public repository, never becomes a
   candidate. On a host that advertises receive-pack anonymously this proof degrades to
   reachability; the upgrade notes say so.
2. Among the candidates for one key, the oldest by `creationTimestamp` (then UID) holds the claim
   and gets the branch worker. Every other `GitBranch` for that key reports `Stalled=True`, reason
   `BranchConflict`, with a message naming the holder.
3. The holder keeps the claim through a later credential or network failure. It reports
   `Ready=False` with the failure and does not hand the branch to another `GitBranch`.
4. Admission refuses a new `GitBranch` whose key a holder already claims. It reads `GitBranch`
   status only and makes no Git call. An unproven `GitBranch` claims nothing, so creating one first
   cannot block anyone at admission.
5. The claim lives in the manager's memory. After a restart it is recomputed by rules 1 and 2. A
   persisted claim, and with it a holder that survives a restart while its credential is broken,
   comes with the HA work, not here.

This replaces `internal/git/branch_owner.go`. Its three callers (the reconciler's conflict check,
the worker sweep, and `EnsureWorker`) ask the claim instead of electing the oldest `GitTarget`.
`git.BranchKey` stays as it is. The worker's credential, signing key, and metric labels come from
the claiming `GitBranch`'s provider, so `provider_namespace` and `provider_name` keep their
meaning for existing dashboards.

### References are checked in status, not at admission

Admission refuses what is wrong with the object itself: a missing required field, a removed field,
an immutable field changed, and a duplicate claim from rule 4. It does not refuse a `GitTarget`
whose `GitBranch` does not exist yet. Flux and Argo CD apply a folder of manifests in one pass, in
no guaranteed order, and an admission rule that demands dependency order breaks the most common
way this API is installed. A missing or unauthorized reference is a status condition instead:

| Object | Reason | When |
|---|---|---|
| `GitTarget` | `GitBranchNotFound` | The referenced `GitBranch` does not exist |
| `GitTarget` | `GitBranchNotReady` | It exists but holds no claim or is failing |
| `GitTarget` | `GitTargetNamespaceNotAuthorized` | The branch's `GitProvider` does not admit the target's namespace |
| `GitBranch` | `GitProviderNotFound`, `GitProviderNotReady` | Moved from `GitTarget` |
| `GitBranch` | `BranchNotAllowed` | `branch` is outside the provider's `allowedBranches`; moved from `GitTarget` |
| `GitBranch` | `ParentBranchNotFound`, `RecoveringParentBranch` | Moved from `GitTarget`, with their rules unchanged |
| `GitBranch` | `BranchConflict` | Another `GitBranch` holds the key (new) |

Strict admission, which refuses a reference to an object that does not exist yet, was considered
and rejected for that reason. `BranchConflict` is a new reason; the others exist today and change
only which kind reports them.

### Lifecycle

A `GitBranch` carries a finalizer. Deletion stops new intake for every target on the branch. The
worker publishes or refuses each pending write, and each attached `CommitRequest` gets its outcome.
Then the finalizer is removed and the claim released. Nothing deletes the branch on the remote.

Deleting a `GitBranch` that targets still reference is allowed; they report `GitBranchNotFound`
afterwards. A new `GitBranch` with the same name is a new UID and cannot pick up the old one's
pending work.

### Removed from `GitTarget`

`spec.gitProviderRef`, `spec.branch`, `spec.parentBranch`, and `status.remote` leave `GitTarget`.
The three spec fields stay in the schema only to be refused by name, with a message that names
`gitBranchRef`. `GitProvider.status.branches` is removed outright: it is status, so there is no
stored value for a user to be refused on.

### Done when

- Two `GitTarget`s in two namespaces reference one `GitBranch` and publish through one worker with
  one credential, without copying the Secret.
- A `GitBranch` with a bad Secret created first does not stop a valid one created later from
  claiming the branch.
- A second `GitBranch` for a claimed repository branch is refused at admission, also when it names
  another `GitProvider` for the same repository.
- Two `GitBranch` objects for one key that both pass admission (a create race) never run two
  workers; the younger reports `BranchConflict`.
- Setting a removed `GitTarget` field is refused with a message naming `gitBranchRef`.
- Deleting a `GitBranch` with pending writes gives each one an outcome before the finalizer goes.
- The parent-branch e2e specs pass with `parentBranch` on the `GitBranch`.
- The chart's quick start renders a `GitBranch`.

## Phase 2: `KubeProvider`

### Shape

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: KubeProvider
metadata:
  name: workspace-a
  namespace: platform-connections
spec:
  secretRef:
    name: workspace-a-kubeconfig   # a kubeconfig Secret in this namespace
    key: value                     # optional; default "value", then "value.yaml"
  accessFrom:
    selector:
      matchLabels:
        platform.example.com/tenant: "true"
  allowCrossNamespace:
    sources: false
    appliers: false
  client:
    qps: 20
    burst: 30
  attribution:
    auditRoute: workspace-a
---
apiVersion: configbutler.ai/v1alpha3
kind: KubeProvider
metadata:
  name: local
  namespace: gitops-reverser
spec:
  inCluster: {}
```

The schema requires exactly one of `secretRef` and `inCluster`. `client` is refused beside
`inCluster`; today it is silently ignored there. The kubeconfig safety checks (exec auth, insecure
TLS, file references) and their reasons stay as they are, because they describe the Secret's
contents.

`KubeProvider` is a new kind, not `ClusterProvider` changing scope. Kubernetes cannot change an
existing CRD's scope in place, so a rename is also the cheapest way to make the break: the
`ClusterProvider` CRD is removed and `KubeProvider` is added beside it. Rule 1 of
[definitions.md](../definitions.md) then renames every `ClusterProvider*` reason and identifier,
with no alias left behind.

### Behavior that changes

- **Secrets resolve in the provider's namespace.** Today they resolve in the operator's namespace.
  A tenant's connection keeps its credential with the tenant.
- **`inCluster` needs the installation's consent.** An installation flag lists the exact
  `namespace/name` of each `KubeProvider` allowed to use the operator's own identity. The chart sets
  it for the provider it creates. A `KubeProvider` with `inCluster` that is not on the list reports
  `Stalled=True`, reason `InClusterNotAuthorized`, and is never used. There is no path from an
  omitted, malformed, or forbidden credential to the operator's identity.
- **The own namespace is admitted.** Like `GitProvider`, a `KubeProvider` admits consumers in its
  own namespace, and `accessFrom` admits the others. Today an empty `ClusterProvider.accessFrom`
  admits nobody, because a cluster-scoped object has no own namespace.
- **References are required.** `kubeProviderRef` is a required `NamespacedObjectReference` on
  `GitTarget`, `FluxTrigger`, and `ArgoCDTrigger`; an omitted namespace means the referrer's own.
  The provider named `default` that an omitted `clusterProviderRef` selects today goes away.
- **Audit routes cannot be borrowed across namespaces.** An omitted `auditRoute` defaults to
  `<namespace>.<name>`, which cannot collide because a namespace name has no dots. An explicit
  route may be shared by providers in one namespace (several connections to one API server). It is
  held by one namespace: the one its first segment names, when a provider there carries it, and
  otherwise the namespace of the oldest provider carrying it. A provider in another namespace that
  carries it reports `Stalled=True` (`AuditRouteConflict`) and is not used. This is a status
  condition rather than an admission refusal, for the reasons `where-validation-lives.md` gives for
  every cross-object check: the webhook is optional and fail-open, and it cannot see a provider that
  changes its route later.

The `allowCrossNamespace` grants keep today's meaning. Narrower per-tenant grants are a later
design.

### Done when

- A tenant namespace mirrors a remote cluster through its own `KubeProvider` and Secret, and a
  second tenant cannot reference it without `accessFrom`.
- A `KubeProvider` with `inCluster` outside the authorized list never reaches the API server with
  the operator's identity.
- Two `KubeProvider`s with one name in two namespaces keep separate watches, caches, and
  attribution facts, also after one is deleted and recreated.
- A remote-source `GitTarget` with a local Flux applier (gitops-api ask #34) still exercises the
  apply window end to end.
- A provider carrying an explicit audit route another namespace holds reports
  `AuditRouteConflict`, and nothing mirrors or applies through it.

## Phase 3: one receiver for many repositories

Gitops-api ask #30: a GitHub App has one webhook endpoint, and today every `GitProvider` carries its
own receiver with its own token. Replace `GitProvider.spec.receiver` with a namespaced
`GitReceiver` and a reference to it:

```yaml
kind: GitReceiver
metadata:
  name: github-app
  namespace: platform-connections
spec:
  type: github
  secretRef:
    name: github-app-webhook-token
  accessFrom:
    selector: {}
---
kind: GitProvider
metadata:
  name: customer-config
  namespace: platform-connections
spec:
  url: https://git.example.com/acme/config
  receiverRef:
    name: github-app
```

A delivery authenticates against the receiver's token, resolves the repository and branch from the
payload, and wakes each claiming `GitBranch` whose provider references that receiver, once. A
delivery only wakes; the worker reads the remote with the provider's credential to find out what
happened. Unknown repositories are counted and ignored. A self-service team gets its own endpoint by
creating a `GitReceiver` in its own namespace, so there is one mechanism, not two.

The name avoids Flux's `Receiver` kind, which sits in the same clusters.

Built with three corrections. **There is no reference and no `accessFrom`:** a delivery wakes every
`GitProvider`, in any namespace, whose `spec.url` names its repository. Once the receiver routes by
the payload's repository, a per-provider reference only narrowed a set the URL already narrows, and
cost a line of wiring per provider plus an admission in every namespace for a GitHub App. Waking
across namespaces grants nothing, because a woken worker reads the remote with its own credential;
it holds at most one pending look, and the `202` carries no count, so it says nothing about other
namespaces. The one cost left is availability: a token holder who keeps delivering can keep a
matching worker reading continuously. That is accepted for now; a per-worker cooldown with a
deferred follow-up would bound it, without bringing the reference back. Whether deliveries reach a
branch is `GitBranch.status.lastPushNoticeAt`, a fact rather than a condition about a reference. A
receiver whose token Secret is missing looks again every 30 seconds, because Secrets are not watched
and no delivery reaches it until its path is published. And a `generic-hmac` body must name its
repository, which it used to be allowed to omit, because a shared receiver cannot route one that
does not.

## Not in these phases

Each of these needs its own design when it is next on the list. None blocks phases 1 to 3.

- **Folder and branch grants per namespace.** Until they exist, every namespace a `GitProvider`
  admits may write any folder on any allowed branch. That suits trusted teams; it is not isolation
  between tenants, and the docs must say so.
- **Two installations on one control cluster** (ask #28): disjoint namespace sets per installation,
  with the cache, webhook, and status-writer consequences.
- **Durable pending writes and HA**: a persisted claim, fencing, and saves that survive a restart.
  See the [HA plan](../future/ha-gittarget-distribution-plan.md).
- **Repository aliases**: URLs the normalizer cannot prove equal, such as vanity domains.
- **A branch-wide suspend**: target `suspend` covers today's need, and adding it later breaks
  nothing.
- **The observability asks #31 to #33**: they are not about sharing and do not need this work.

## Open questions per phase

Phase 1:

- Should `GitBranch` get printer columns for provider, branch, commit, and the holder's readiness?
  [definitions.md](../definitions.md) rule 8 governs what a dependency projection may show.
- Is the receive-pack proof enough on the Git hosts the e2e suite covers, or does one of them need a
  documented exception?

Phase 2 (settled): the `inCluster` list is `--in-cluster-kube-providers` (chart value
`controllerManager.inClusterKubeProviders`), and the chart's provider is named `local`.

Phase 3 (settled): routing is by URL alone, so a `GitReceiver` has no `accessFrom` and a
`GitProvider` no `gitReceiverRef` or `GitReceiverReady` condition (see the corrections above).
