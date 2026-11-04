# Review: one CommitRequest, no admission webhook

Review of `feat/gitops-api-asks-converge-root-ca` (PR #456, at `70d07d91`), on one question: how
much of the `SaveRequest` / `RemoteSaveRequest` machinery can go, and in particular whether the
admission webhook can go entirely.

Status: design decided on 2026-10-27; phase 1 is built in PR #456, phase 2 of the CRD signal is not.
The first version of this review proposed one kind on the source cluster only, named `SaveRequest`.
The decisions below replace that proposal. Line references are to `70d07d91`, before the change.

## Decisions

1. **One kind, named `CommitRequest`, in `configbutler.ai/v1alpha3`.** That is its name on `main`,
   so the rename to `SaveRequest` (`b86dffbe`) is undone, which removes one breaking change.
2. **It is served in two ways, through one shared core.**
   - **On the control cluster** it is an operator kind like any other. A controller in the
     operator serves it with the operator's own identity, and a GitTarget admits it by
     `consumerNamespaces`, default `from: Same`.
   - **On any other source cluster** it is reached through the GitTarget's KubeProvider. That
     KubeProvider sets `commitRequests` to `Reconcile` or `ReconcileAndDeleteFinished`, and the
     GitTarget admits the request by `sourceNamespaces`.
3. **`spec.gitTargetRef` follows the API's reference convention.** An omitted namespace means the
   request's own namespace, as in every other cross-namespace reference. On a source cluster, the
   namespace is required, for the reason a ClusterWatchRule requires it: there is no
   control-cluster namespace to default to. See [Naming the GitTarget](#naming-the-gittarget).
4. **The admission webhook goes completely.** That covers both handlers, the `/validate-all` e2e
   endpoint, the command-author Redis store, the chart values, the manifests and the docs.
   Takeover (`SavedBy`/`TakeOver`) goes with it, because only admission could name a taker. On
   both paths, a request's author comes from audit facts, the same way an edit's author does.
5. **cert-manager stays**, but only as a prerequisite of attribution. See
   [cert-manager](#cert-manager).
6. **GitOps Reverser never writes the CommitRequest CRD on a source cluster.** It reports the CRD's
   state on the KubeProvider. Creating the CRD when it is absent is
   [future work](#future-work-creating-the-crd), and updating it is ruled out for good.
7. **Several identities on one cluster are a goal**, so the path for other clusters works with
   credentials scoped to namespaces. See
   [Several identities on one cluster](#several-identities-on-one-cluster).

## The design

### The CommitRequest

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: CommitRequest
metadata:
  name: raise-replicas
  namespace: shop              # on the control cluster, or on the GitTarget's source cluster
spec:
  gitTargetRef:
    namespace: team-shop       # a control-cluster namespace; omitted, this request's own
                               # namespace, which only the control-cluster path accepts
    name: shop
  message: "Raise the replica count"
  window:                      # defaults shown
    attachTimeout: 5s
    maxDuration: 2s
  catchUp: AsDefinedOnClaim    # or AfterPush
status:
  author: jane@example.com     # from audit; empty when no audit event names the creator
  branch: main
  commit: 3f2a…
  conditions: [Ready, Stalled, Applied]
```

- **The spec and status** are today's `RemoteSaveRequest` ones, under the name and group of
  `main`. `gitTargetRef` is a `meta.NamespacedObjectReference`, like every other cross-namespace
  reference.
- **The phase reasons and the `save_requests_total` metric** come from today's control-cluster
  reconciler.
- **Gone from the `main` version:** `spec.window.author`, because takeover is removed.
- **Kept from the `main` version:** `spec.window.attach` (`CurrentOrNext`/`Next`) and
  `spec.whenNothingToCommit` (`Resolve`/`CommitEmpty`). Today only the control-cluster reconciler
  has them. They move into the shared core, so a request on a source cluster gets them too.
- **Retention keeps `main`'s contract on both paths.** When a request finishes, it is stamped with
  `configbutler.ai/delete-after`, and it is deleted once that time has passed. Removing the
  annotation keeps the request. The source-cluster path's own TTL, which is computed from the
  conditions, goes away. On a source cluster, deleting is opted into with
  `commitRequests: ReconcileAndDeleteFinished` (see [The KubeProvider](#the-kubeprovider-speccommitrequests)).

#### Naming the GitTarget

Every reference to another namespace follows one rule: the namespace may be omitted, and then it is
the referring object's own namespace. The one exception is a ClusterWatchRule, which must name the
namespace because it has none of its own.

| Reference | Type | An omitted namespace means |
| --- | --- | --- |
| GitTarget `gitBranchRef`, `kubeProviderRef` | `NamespacedObjectReference` | the GitTarget's namespace |
| GitBranch `gitProviderRef` | `NamespacedObjectReference` | the GitBranch's namespace |
| FluxClaim `kubeProviderRef`, `kustomizationRef`; ArgoCDClaim `kubeProviderRef`, `applicationRef` | `NamespacedObjectReference` | the claim's namespace |
| ClusterWatchRule `gitTargetRef` | `NamespacedObjectReference` | nothing: required, "a ClusterWatchRule has no namespace to default to" |
| WatchRule `gitTargetRef` | `LocalObjectReference` | always the WatchRule's namespace |
| `CommitRequest` `gitTargetRef` on `main` | `LocalObjectReference` | always the request's namespace |

So `CommitRequest` follows the rule:

- **On the control cluster, the namespace is optional**, and defaults to the request's namespace.
  This is the behavior of `main` and of every other reference, and a single-cluster user writes
  what they write today.
- **On a source cluster, the namespace is required**, for the ClusterWatchRule's reason.
  - The request's namespace belongs to another cluster, so it is not a control-cluster namespace
    to default to.
  - Defaulting to "the control-cluster namespace with the same name" would turn a naming
    coincidence into a reference. [definitions.md](../definitions.md) exists to prevent exactly
    that.
  - The KubeProvider's `{consumer.namespace}` does bridge the two sides by name, but only as a
    variable someone writes on purpose, never as a silent default.

**Where the rule is enforced.** Both clusters use the same CRD, and a CEL rule cannot know which
cluster it runs on, so the schema allows an omitted namespace.

The source-cluster path does not pick up a request without a namespace, and it writes nothing to
it. It cannot refuse such a request, because it cannot know the refusal is true. A KubeProvider
whose kubeconfig points back at the control cluster sees the control cluster's requests, and there
an omitted namespace is valid.

This is not a far-fetched case. `inCluster` is honored only for providers the installer lists in
`--in-cluster-kube-providers`, so a tenant who wants its own identity on the operator's cluster
needs exactly such a kubeconfig. That is the "several identities on one cluster" goal. A source
cluster user who forgets the namespace therefore sees no status. The field's doc says so, and
gitops-api always sets the namespace.

**Rejected: a separate CRD variant for source clusters with the namespace required in CEL.** The
check would move into the schema. But one kind would then have two schemas, and the chart's
variant applied to a workspace would silently undo the check.

### Which path serves a request

The named GitTarget's KubeProvider alone decides which path serves a request: that path is the
request's **owner**. Two rules keep several observers of one request safe:

1. **A path writes only what it knows to be true.** Only the owner serves a request and writes
   its progress. An observer that is not the owner writes only `GitTargetNotFound`, after
   `attachTimeout`, and only into an empty status. It writes nothing else, because nothing else is
   true for every observer.
2. **Every status write is conditional on the resourceVersion its decision was made from.** On a
   conflict, the writer reads the request again and decides again. Today's writer reads the object
   again and overwrites it without deciding again
   (`internal/watch/remote_save_requests.go:689-701`). An observer that sees a status it did not
   write stops. So a `GitTargetNotFound` that was true when it was decided can land, but it never
   overwrites an owner that has started.
3. **The owner claims a request by persisting its first status, and attaches only after that.**
   The first write records the author and `Ready=Unknown`, reason `Saving`, and it is conditional
   on the resourceVersion the owner read. Only when it succeeds does the owner attach to the
   worker. If it conflicts and the fresh object already holds an outcome, such as a non-owner's
   `GitTargetNotFound`, the owner stops without saving.
   - Today's `serve` logs a failed status write and continues into `finalize` (l.348-352 and
     l.391-393). Fixing the writer alone would therefore not be enough.
   - **After the claim, a conflict re-applies the owner's outcome. It never restarts the save.**
     Once the claim has landed, no other observer writes, by rule 1. So a later conflict comes only
     from the owner's own writes, and the worker's outcome, once produced, is written onto the fresh
     object as it is.
   - **Ownership is structural, not "this process wrote the status".** After a restart, the owner
     is still the path the GitTarget's KubeProvider picks. It finds its own `Saving` status and
     resumes, as the server does today.

| The request is on | The GitTarget's KubeProvider | What happens |
| --- | --- | --- |
| the control cluster | `inCluster` | **the control-cluster path serves it** |
| the control cluster | a kubeconfig, with either `Reconcile` value | left alone, because that kubeconfig may point back at the control cluster, and then its watcher serves the request |
| the control cluster | a kubeconfig, with `commitRequests: Ignore` | `NotAllowed`: "GitTarget … mirrors another cluster; create the CommitRequest there, and set `commitRequests: Reconcile` on its KubeProvider" |
| a source cluster that provider P watches | P, with a kubeconfig | **P's watcher serves it** |
| a source cluster that provider P watches | any other provider | left alone |
| a source cluster | `gitTargetRef` has no namespace | not picked up, and no status ([Naming the GitTarget](#naming-the-gittarget)) |
| either | the GitTarget does not exist | `GitTargetNotFound` after `attachTimeout`, by rule 1 and rule 2 |

- **A control-cluster request for a GitTarget that mirrors another cluster is refused**, in every
  install mode. Its author would come from the control cluster's audit, and the edits' authors
  from the other cluster's. Those are two identity domains (F3), and the request would only work
  while attribution is off.
- **"Local" means the provider's `inCluster` arm.** A kubeconfig that points back at the control
  cluster counts as another cluster, because the operator cannot tell the difference.
- **Known gap.** A control-cluster request whose GitTarget's provider has `Reconcile` and points
  somewhere else gets no status from anyone. The doc for the field says so. A cluster ID would
  close the gap ([Future work](#future-work)).

### What both paths share

Once a path owns a request, the same core runs. These are its responsibilities, in order:

1. **Admission** by `allowedCommitRequests`. A request no grant admits fails with `NotAllowed`.
2. **The author.** It comes from audit facts on the GitTarget's KubeProvider's route, from the
   same index and with the same grace as the edits.
   - **The core acquires the fact stream** for that route and `commitrequests`, as
     `RemoteSaveRequestServer.run` does today (l.200). Today's control-cluster reconciler never
     acquires one, because admission gave it the author. Without the stream, the fact follower
     never reads the request's audit facts, so every save on that path would wait out the grace and
     end without an author.
   - **The wait runs whenever attribution is on**, and is skipped only when it is off (no
     `AuthorResolver`). Whether a provider has received audit facts is checked after the wait, as
     `sentAuditFacts` does today, and decides only `AuthorUnresolved`. Skipping the wait on a
     provider that has not received a fact yet would lose the author while the provider's first
     audit event is still in transit, or while its status condition lags.
   - **The author is written to status in the first status write.** The lookup matches the
     request's resourceVersion, and every status write changes it, so a later lookup could never
     match. Today's server already does this (`status.Author` before the `Saving` write).
3. **Attach and outcomes:** one save in progress per author per GitTarget (`SaveInProgress`), the
   UID binding, recovery after a restart, the phase reasons, the metric, and the worker's
   `git.AttachSaveRequest`.
4. **Following the apply:** `catchUp: AfterPush` and the `Applied` condition.
5. **Retention:** stamping `configbutler.ai/delete-after`, and deleting once it has passed.

Every status write follows rule 2 of [Which path serves a request](#which-path-serves-a-request).

Each path feeds this core:

- **The control-cluster path** is a controller-runtime reconciler. It uses the operator's cache,
  RBAC and status client, and is cut down from today's `saverequest_controller.go`.
- **The source-cluster path** is today's `RemoteSaveRequestServer`. It runs a dynamic watch per
  KubeProvider and writes status through that provider's credential.

The core is written once, so F4 cannot come back. If the two paths ever need different logic
beyond picking up a request and writing its status, treat that as a design smell.

**The default watch policy excludes `configbutler.ai/commitrequests`**, in place of
`source.configbutler.ai/remotesaverequests`. A request can sit in a namespace a WatchRule mirrors,
and it must never land in Git.

What a save attaches to, in each install mode:

| Install | Edit author | Request author | The request saves |
| --- | --- | --- | --- |
| Default (no attribution) | committer | nobody, no wait | the GitTarget's one unnamed window |
| Attribution on, a source cluster whose first audit event has not arrived | from audit, once it arrives | from audit, within the same grace | its creator's own window |
| Attribution on, single cluster | from audit | from audit | its creator's own window |
| kcp with audit (the managed product) | from audit | from audit | its creator's own window |
| A source cluster that sends no audit | nobody | nobody | the unnamed window: the message is kept, and the commit has no named author |

Who may save is decided by `allowedCommitRequests`. Who authored the commit is decided by audit, or
by nobody. Neither is decided by admission.

### The GitTarget: `spec.allowedCommitRequests`

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
spec:
  allowedCommitRequests:
    # The default when omitted: CommitRequests in this GitTarget's own namespace.
    - consumerNamespaces: {from: Same}
    # Requests on the control cluster, from other namespaces, by label:
    - consumerNamespaces:
        from: Selector
        selector: {matchLabels: {team: shop}}
    # Requests on the source cluster that spec.kubeProviderRef names:
    - sourceNamespaces: ["*"]          # every namespace this GitTarget mirrors
```

- **One list, and each grant sets exactly one of two fields.** A CEL rule enforces this. The two
  fields are the ones the API already uses for each side:
  - **`consumerNamespaces`** has the same type and meaning as in every other `allowed*` list:
    control-cluster namespaces, with `from: Same | Names | Selector | All`. It admits requests on
    the control-cluster path.
  - **`sourceNamespaces`** has the same meaning as the KubeProvider's
    `allowedGitTargets[].sourceNamespaces` and a WatchRule's `rules[].sourceNamespace`: namespaces
    of the GitTarget's source cluster, as exact names or `"*"`. It admits requests on the
    source-cluster path.
    - Here `"*"` means every namespace this GitTarget mirrors.
    - Variables are refused, because `{consumer.namespace}` would be the request's own namespace,
      which bounds nothing.
    - It can admit only namespaces the KubeProvider lends this GitTarget in `allowedGitTargets`,
      the same bound its WatchRules have.
- **A grant on the side the GitTarget does not use admits nothing.** An `inCluster` GitTarget
  never serves through `sourceNamespaces`, and any other GitTarget never serves through
  `consumerNamespaces`. A `NotAllowed` message says which field applies.
- **Omitted, the list is `[{consumerNamespaces: {from: Same}}]`, and `[]` admits nobody.** This is
  the pattern of the other lists: an API-server default, no `omitempty`, a Go mirror, and
  `TestGrantDefaultsMatchCRDs`. A single-cluster user writes nothing, as on `main`. A user of
  another cluster writes a `sourceNamespaces` grant, and that list replaces the default.
- **`requestSelector` is dropped.** It picked requests by labels their creator chooses, so it
  proved nothing. It can come back later as an additive field.
- **`remote` is not used in any field name.** In this API, "remote" means the Git remote
  (`status.remote`), and [definitions.md](../definitions.md) fixes "control cluster" and "source
  cluster" as the two sides.

### The KubeProvider: `spec.commitRequests`

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: KubeProvider
spec:
  secretRef: {name: shop-workspace}     # a kubeconfig Secret
  commitRequests: ReconcileAndDeleteFinished   # Ignore (default) | Reconcile | ReconcileAndDeleteFinished
```

Each value makes this cluster a bigger source of CommitRequests, and needs more of the credential
on `commitrequests` in the `configbutler.ai` group:

| Value | What GitOps Reverser does | The credential needs |
| --- | --- | --- |
| `Ignore` (default) | nothing | nothing |
| `Reconcile` | serves the requests in the namespaces this KubeProvider lends its GitTargets, and writes their status. A finished request stays, and no `delete-after` is stamped on it, because nothing would act on it | `get`, `list`, `watch`; `update` on `commitrequests/status` |
| `ReconcileAndDeleteFinished` | as `Reconcile`, and also stamps `configbutler.ai/delete-after` when a request finishes, and deletes the request once that time has passed | as `Reconcile`, plus `patch` and `delete` |

- **Why deletion is a separate value.** Deleting an object on someone else's cluster is a bigger
  write than updating its status. The KubeProvider's owner lends the cluster, so the provider is
  where that consent belongs, and the credential's RBAC follows it. gitops-api uses
  `ReconcileAndDeleteFinished` for kcp workspaces.
- **Not `ReconcileAndPrune`.** [definitions.md](../definitions.md) reserves "prune" for
  `GitTarget.spec.prune.mode`, which removes documents from a Git folder.
- **How long a finished request is kept stays installation-wide.** That is
  `--save-request-ttl`, renamed back to `--commit-request-ttl`, its name on `main`:
  default `48h`, and `0` stamps nothing in any mode. The annotation on a single request still
  overrides it: remove it to keep the request, or edit it to move the deletion.
- **The annotation counts only while deletion is on.** If a provider goes from
  `ReconcileAndDeleteFinished` back to `Reconcile`, annotations that were already stamped stay, but
  nothing deletes those requests any more.
- **A delete the credential may not make** leaves the request in place. The
  `CommitRequestsServed` message says so, and the condition stays `True`, because serving works.
- **The control cluster has no switch.** A CEL rule refuses every value but `Ignore` together with
  `inCluster`. CommitRequests on the control cluster are always served, and finished ones are
  deleted by the same flag and annotation, as on `main`. The operator owns that whole lifecycle, as
  it does for every other kind.
- **I chose an enum over a boolean `reconcileCommitRequests: true`.** The API conventions avoid
  booleans, and this third value is the reason: it was added without a second field.
- **It replaces `remoteSaveRequests: Enabled|Disabled`.** Today `Enabled` serves the requests and
  also deletes them, as [configuration.md](../configuration.md#serving-the-remotesaverequests-of-a-cluster)
  now lays out in a table of the same shape. The PR that builds this design replaces that table
  with the one above.

Both `Reconcile` values add one condition and one status field. A second condition comes in phase
2:

- **`CommitRequestsServed`**: whether requests are being served, including whether the CRD is
  there.
- **`status.commitRequests.namespaces`**: what this KubeProvider watches.
- **Phase 2: `CommitRequestCRDUpToDate`**, whether the CRD's schema matches the operator's.

None of them exists on an `inCluster` provider. On the control cluster, the chart owns the CRD, as
it owns every other CRD, and the operator watches with its own permissions.

#### Phase 1: the `CommitRequestsServed` condition

This ships in the main PR. It needs only discovery and the watch itself.

| Status | Reason | When |
| --- | --- | --- |
| True | `Watching` | the message says how many namespaces, or "every namespace" |
| True | `NothingToWatch` | no GitTarget mirroring through this KubeProvider is lent a namespace |
| False | `CRDNotInstalled` | discovery shows no `commitrequests.configbutler.ai` |
| False | `VersionNotServed` | the CRD is there, but does not serve `v1alpha3` |
| False | `Forbidden` | `list` or `watch` on CommitRequests was denied; the message names the namespaces |
| Unknown | `Unreachable` | discovery could not be read |

- **Each message ends with its fix.**
  - `CRDNotInstalled` and `VersionNotServed`: `kubectl apply --server-side -f` with the release's
    `commitrequest-crd.yaml`. That is a new release asset. The existing `crds.yaml` holds every
    CRD, and a source cluster needs only this one.
  - `Forbidden`: the missing verbs and namespaces.
  - `Unreachable`: the discovery error.
  - `Watching` and `NothingToWatch` need no fix.
- **When it runs:** on every KubeProvider resync, and whenever the spec changes. A CRD applied by
  hand therefore clears the condition without a restart.
- **Not part of `Ready`.** Mirroring works without it, as it does without `AuditFactsReceived`.
- **What it replaces.** Today a missing CRD shows up only as a log line, "The watch of
  RemoteSaveRequests ended; reconnecting".
- **A printer column, `Saves`,** shows the reason, at `priority=1`.

#### Phase 2: knowing whether the CRD is out of date

This is its own PR. It is the "outdated" signal you asked for. It is deferred because it adds a
compatibility protocol, and because one part of it depends on kcp in a way that is not yet verified.

**The revision line itself ships in phase 1**, in the type's doc comment, with its hash test. It is
a comment and a test, so it costs almost nothing. Without it, every CRD installed from phase 1's
release asset would carry no revision, and phase 2 could not judge any of them.

The CRD carries a schema revision, and the operator compares it with its own.

- **Where the revision is.** The last line of the kind's description reads
  `Schema revision 7, compatible from revision 5.` kubebuilder renders the type's doc comment into
  the CRD, so `kubectl explain commitrequest` prints the line. It is also in the cluster's published
  OpenAPI v3 document.
- **How the operator reads it.** It reads `/openapi/v3/apis/configbutler.ai/v1alpha3`. The default
  `system:discovery` role lets every authenticated identity read that document, so the check needs
  no permission on CRDs and works with a credential scoped to a namespace. There is no fallback to
  reading the CRD itself: an API that a kcp `APIBinding` provides has no CRD in the consuming
  workspace ([kcp docs](https://docs.kcp.io/kcp/v0.28/concepts/apis/exporting-apis/)).
- **The gate.** The PR starts by verifying that kcp serves per-workspace OpenAPI v3, including for
  APIs provided through an `APIBinding`. If it does not, the design changes before any code is
  written.
- **The revision** goes up with every change to the generated CRD. A unit test hashes the CRD and
  fails when the hash changes but the revision does not, the same way `hack/crdfields` guards the
  field list.
- **The floor ("compatible from")** is raised by hand when a change breaks an older reader or
  writer: a new required field, a removed field, or a changed meaning. This project makes hard
  cuts, so the floor will often move with the revision, which is fine.
- **The comparison.** Take the operator's revision O and floor Fo, and the CRD's revision C:
  - C equal to O: current.
  - C below O: served when C is at least Fo.
  - C above O: **never served**.
- **Why a newer CRD is never served.** The status writer replaces the whole `status` map, so an
  older operator would erase every status field that a newer schema added. Serving a newer CRD
  safely would need rules for preserving unknown fields, and tests for them. Refusing it costs one
  operator upgrade, and that is the fix anyway. The CRD's own floor is therefore never read.
- **Rejected: comparing the served schema field by field.** It needs no bookkeeping, but it cannot
  see changed validation, defaults or meaning, and a condition message cannot explain its result
  well.

| Status | Reason | When | Requests served? |
| --- | --- | --- | --- |
| True | `Current` | the CRD's revision is the operator's | yes |
| False | `Outdated` | older than the operator, but at or above the operator's floor | yes |
| False | `Incompatible` | older than the operator's floor | no |
| False | `NewerThanOperator` | newer than the operator; upgrade the operator | no |
| Unknown | `RevisionUnknown` | there is no revision line, so compatibility cannot be judged. This is not taken as evidence that the CRD came from elsewhere | yes |
| Unknown | `Unreadable` | the OpenAPI document could not be read | unchanged |

- **`Outdated` keeps serving.** A user should not lose saves because a revision moved without
  breaking anything. Only a crossed floor stops serving.
- **A reason that stops serving** also sets `CommitRequestsServed=False`, with reason
  `CRDNotUsable`, pointing here.
- **The operator never updates the CRD**, whatever this condition says, for the reasons in
  [Future work](#future-work-creating-the-crd). The condition is the signal, and a person acts on
  it.

#### When `commitRequests` is `Ignore`

- **The conditions and `status.commitRequests` are removed.**
  - A condition that would always say "not applicable" is noise.
  - A `True` left over from before the switch would be wrong.
  - `AuditFactsReceived` already works this way: it is absent while attribution is off.
- **The watches stop. The CRD and the requests on the cluster stay as they are.** A request created
  there stays without status, unless another KubeProvider serves it.
- **Rejected: keep reporting the CRD read-only while the switch is off.** The check is cheap, but
  it reports on something the KubeProvider's owner said it has nothing to do with. With several
  providers on one cluster, every one of them would report the same state.

### Several identities on one cluster

On the control-cluster path, there is one identity: the operator's. This section is about the
source-cluster path. A KubeProvider is a cluster and an identity in one object, and several
KubeProviders can point at one cluster with different credentials, for example two kubeconfigs for
two teams' ServiceAccounts.

| Concern | Today | With this design |
| --- | --- | --- |
| Which provider serves a request | every enabled provider tries, and the first one decides (F11) | the GitTarget's own provider only ([Which path serves a request](#which-path-serves-a-request)) |
| Watch scope | one cluster-wide list and watch, so a credential scoped to a namespace gets `Forbidden` | the namespaces the provider lends its GitTargets; a `Role` in each is enough |
| The CRD | cluster-scoped, so a credential scoped to a namespace cannot `get` it | read through OpenAPI, so every provider reports its state |
| Audit facts | providers in one namespace can share a route. A provider in another namespace that uses the same route is `Stalled`, reason `AuditRouteConflict` | unchanged |
| Duplicate work | each provider runs its own watch | each provider watches only its own slice; slices that overlap are watched twice |

#### Chosen: the provider's lent namespaces set the scope

A KubeProvider with `Reconcile` watches CommitRequests only in the namespaces it lends to the
GitTargets that mirror through it. These are the `sourceNamespaces` of the `spec.allowedGitTargets`
grants that admit each of those GitTargets, with `{consumer.namespace}` resolved per GitTarget.

- **If any grant lends `"*"`**, the provider runs one watch across the cluster. Otherwise it runs
  one watch per namespace.
- **`status.commitRequests.namespaces`** shows the result, so the derived scope is never hidden.
- **The credential already reads these namespaces for mirroring.** Its `Role` in each one gains
  `get`, `list` and `watch` on `commitrequests`, and `update` on `commitrequests/status`. With
  `ReconcileAndDeleteFinished` it also gains `patch` and `delete` on `commitrequests`.
- **A request in a namespace that is lent but not admitted still gets `NotAllowed`** from the
  GitTarget's provider, so a wrong grant is visible.
- **Overlapping slices are harmless**, because only the GitTarget's provider acts.

It needs no new field: the provider's owner chooses which namespaces to lend in
`allowedGitTargets`, and the GitTarget's owner chooses which to admit in `allowedCommitRequests`.

Costs:

- **A request outside every lent namespace is seen by nobody and gets no status.**
- **The watches change when a provider grant changes.** Grants change rarely, and the mirror
  already manages watches per namespace.
- **Watching only the namespaces the GitTarget mirrors would be narrower, and was not chosen.** It
  would change with every WatchRule edit, and it would lose `NotAllowed` for namespaces that are
  lent but not mirrored.

#### Not chosen

- **An explicit namespace list on the provider**, `commitRequests: {mode: Reconcile,
  sourceNamespaces: [...]}`. It would be a third list of namespaces, next to `allowedGitTargets`
  and `allowedCommitRequests`, and the three would drift apart.
- **One connection with impersonated identities**, the way an Argo CD AppProject uses
  `destinationServiceAccounts`. `impersonate` is close to cluster-admin, and it concentrates power
  in one credential, which is the opposite of what several identities are for.
- **Splitting the cluster from the identity, and riding the GitTarget's watch streams.** These are
  in [Future work](#future-work).

### cert-manager

You were right: it does not go away. The audit ingress mints three things through cert-manager:

- a root CA;
- its serving certificate;
- the kube-apiserver client certificate for mTLS (`audit-certificates.yaml`).

These render only with `attribution.enabled`. Metrics TLS is off by default. So once admission is
removed:

- **Default install** (no attribution): no certificate is rendered, and cert-manager is not
  needed.
- **Attribution on**, which includes the managed product: cert-manager is needed, or Secrets you
  bring yourself with `servers.audit.tls.certManager: false`.

Two chart changes make this true:

1. Gate the self-signed `Issuer` in `certificates.yaml` on some certificate being rendered, meaning
   attribution with audit TLS, or metrics TLS. Today it renders whenever `certManager.enabled` is
   true, which is the default, so an install without cert-manager's CRDs would fail on it.
2. Move [quickstart.md](../quickstart.md) step 1 into the "Want named users on your commits?"
   section.

## Future work

### Future work: creating the CRD

Not in this design. If it comes, these are the rules:

- **Create it only when it is absent.** While either `Reconcile` value is set and discovery shows
  that the cluster serves no `commitrequests.configbutler.ai`, the CRD is created from a copy built
  into the operator binary. `AlreadyExists` counts as present. `NotInstalled` would then be
  replaced by `Created` (True) and `CreationFailed` (False, with the API error and the missing
  permission).
- **Never update it, and never delete it.** Not after an operator upgrade, and not on `Ignore` or
  when the KubeProvider goes. Deleting a CRD deletes every CommitRequest on the cluster.

Updating is ruled out for good, because it is a minefield:

- **An update changes the rules for objects that are already stored.** A version that stops being
  served, or validation that gets stricter, makes the next status write fail on an object the
  update broke.
- **The CRD may have another owner**: Helm's `crds/`, Flux or Argo CD applying it from Git,
  gitops-api's `workspaceCRDs`, or a kcp `APIExport`. Two writers fight, and a GitOps owner reverts
  the operator's change.
- **An operator upgrade would change the API on every connected cluster at once**, with nobody
  reviewing it.
- **The permission is broad.** `update` on CRDs is power over the cluster's API surface.

Creating is different, because it replaces nothing. Its cost is the permission: `create` on
`customresourcedefinitions` is cluster-wide, on a credential that is otherwise read-mostly. Its
gain is one step fewer for every new source cluster or kcp workspace. In kcp, an `APIBinding` that
already provides `configbutler.ai` shows up in discovery, so nothing would be created. That is the
right outcome, but it would need a test.

### Future work: other items

- **A cluster ID** per KubeProvider, such as the `kube-system` namespace UID, or the logical cluster
  name in kcp. It would close the known gap in
  [Which path serves a request](#which-path-serves-a-request): the operator could tell that a
  kubeconfig points back at the control cluster.
- **Split the cluster from the identity.** A new kind, for example `KubeCluster`, would hold the
  endpoint, the audit route, the CRD state and the cluster ID. A KubeProvider would become one
  identity on a cluster.
  - It fixes `AuditRouteConflict` across namespaces, because every identity on a cluster should
    read that cluster's one audit stream.
  - It also stops N providers from reporting the same CRD state.
  - It costs a new kind, a breaking split of KubeProvider, and a platform-team owner for the
    cluster object.
  - kcp gives every workspace its own logical cluster with one identity, so the managed product
    does not need it. Build it when the first `AuditRouteConflict` between two identities shows up.
- **CommitRequests ride the GitTarget's own watch streams.** The request's author would come from
  the same inline `attachAuthor` call that attaches an edit's author, and the source-cluster path's
  own server would go away. The watch plane is the most delicate code in the project, so this is
  too large for now.

## What exists today

| | `SaveRequest` (`configbutler.ai/v1alpha3`) | `RemoteSaveRequest` (`source.configbutler.ai/v1alpha1`) |
| --- | --- | --- |
| Created on | the control cluster | the source cluster (kcp workspace, or any KubeProvider) |
| Author from | the `/validate-operator-types` admission webhook, written to Redis `author:v1:command:<uid>` | the source cluster's audit facts, same index as the edits |
| Author when that source is absent | none (`AuthorAttributed=False`). The default install is in this state: admission on, Redis empty | none, and refused `AuthorUnresolved` if the provider ever sent audit |
| Admitted by | `GitTarget.spec.allowedSaveRequests` (default: `from: Same`) | `GitTarget.spec.allowedRemoteSaveRequests` (default: none) and `KubeProvider.spec.remoteSaveRequests: Enabled` |
| Window spec | `attach` (`CurrentOrNext`/`Next`), `author` (`Own`/`SavedBy`/`TakeOver`), `attachTimeout` 2s, `idleTimeout`, `maxDuration` 2s | `attachTimeout` 5s, `idleTimeout`, `maxDuration` 2s |
| Extra spec | `whenNothingToCommit` (`Resolve`/`CommitEmpty`), `catchUp` | `catchUp` |
| Served by | a controller-runtime reconciler: `saverequest_controller.go`, `_finalize.go`, `_apply.go`, 1,468 lines | `internal/watch/remote_save_requests.go`, 702 lines |
| Same worker API | `git.AttachSaveRequest` | `git.AttachSaveRequest` |

The decided design keeps both ways of picking a request up, under the left-hand column's name and
API group. It has one core with the right-hand column's identity rule, and one grant list. Both
end up in the same branch-worker code (`internal/git/save_request_attach*.go`, 1,212 lines), which
stays.

## How it got here

The design history, in the steps that matter here:

1. **Audit with a 60s wait** (`c6220afc`, May). The `CommitRequest` author was read from its own
   create event in the ordered audit stream. It flaked on `generateName` when an event was diverted
   below the high-water mark (`91cc3172`).
2. **Admission** (`58dd37a8`, `96ec3906`, June). It was introduced for two reasons. Option C "kept
   the wrong source … and therefore kept a wait", and "on a managed cluster where you cannot enable
   an apiserver audit webhook, your commands are still attributed".
3. **The keyed fact index** (#211, July) and **watch-first ingestion** removed both reasons.
   - A fact is now awaited from the index, bounded by `--author-attribution-grace` (3s by default,
     `internal/watch/author_resolver.go:26`).
   - That is the same pipeline, and the same wait, as the edits the save publishes.
   - The 60s wait and the ordered-stream divert are gone.
4. **The race admission created** (#367, September). The request took the fast path (admission) and
   its edit the slow one (audit join). Saves were "finalized before the write it exists to publish
   reached the worker", so `attachTimeout` got a 2s default to cover the gap
   (wait-options, l.16-43, 97-114).
5. **"Works without audit" was never true on its own.** A named request on an audit-less cluster
   meets windows that name nobody: `WindowMismatch`, "every time, permanently"
   (wait-options l.277-281). It became true only on 2026-10-26, with `SavedBy`/`TakeOver`
   (`cf3b97be`, `ee8946c5`), and only with an opt-in grant and an un-latched provider.
6. **`RemoteSaveRequest`** (`beddcdef` to `70d07d91`). It is authored from source audit. It was
   built so that "a customer's only API surface is their kcp workspace" holds with no exceptions:
   no tenant grant into the control cluster, and no `/editor/commit` on the save path
   (source-side-save-plan, l.46-59).

In short: admission was the right answer to a pipeline that no longer exists. Every argument for it
since then has been about either takeover or gitops-api's current save path, and the source-side
plan already replaces that path.

## Review findings

F1 to F10 are complexity findings, not correctness bugs; the correctness review of these commits
happened in the three rounds recorded on the PR. F11 is a correctness bug, found while working out
the design.

**F1. The default install pays for admission and gets nothing for saves.** The chart has
`servers.admission.enabled: true` while `queue.redis.addr` is `""`. The command-author store is
nil, so every `SaveRequest` reports `AuthorCaptureDisabled`. The webhook's only effect in the
default install is the WatchRule feedback (F6).

**F2. cert-manager is a prerequisite of the default install only because of admission.**

- The admission certificate is the only certificate a default install renders, and
  [quickstart.md](../quickstart.md) step 1 says so: "The controller mounts an admission certificate
  at startup, so cert-manager must be healthy before…".
- Attribution still needs cert-manager (see [cert-manager](#cert-manager)).
- [where-validation-lives](../spec/where-validation-lives.md) prices a webhook as "cert wiring in
  the chart, a failure mode that can block tenant writes, and an extra moving part in every install
  path".

**F3. Two identity domains, joined only by name equality.** A control-cluster save attaches to a
window only when the admission `userInfo.username` equals the audit user on the source cluster.
That holds for gitops-api only because its server impersonates the Dex email
(source-side-save-plan l.42-48). For anyone else with two clusters, it is a coincidence. The design
removes it by refusing a control-cluster request whose GitTarget mirrors another cluster.

**F4. Two servers for one job, already drifting.**

- The reconciler has these, and the remote server lacks them:
  - `save_requests_total`;
  - the phase reasons (`WaitingForWorker` / `WaitingForWindow` / `CollectingWindow` /
    `WaitingForPush`);
  - `attach: Next`;
  - `CommitEmpty`;
  - the `AuthorAttributed` condition.
- The remote server has these, and the reconciler lacks them:
  - `status.author`;
  - `AuthorUnresolved`;
  - a 5s default instead of 2s, because its timing model differs.
- `gitTargetUID` handling exists in both, written twice: `refuseOnceUnheld` / `readmit`.
- Every later fix has to be made twice, or the two drift further. The design keeps two ways of
  picking a request up, but with one core behind them.

**F5. Takeover is the largest piece of policy on the branch, and it serves one case.**

- It takes three conditions, two enums of three values, the provider latch, a per-grant bound,
  "max over admitting grants", and two commit trailers (`ee8946c5` + `cf3b97be`: +767/−171 lines).
- It exists only for a control-cluster save on a source cluster with no audit.
- Its name can come only from admission (source-side-save-plan l.260-262), so it is the one feature
  that makes admission load-bearing.

**F6. The WatchRule check on the same endpoint is feedback, not a gate.**

- The writer refuses the configuration whether or not the webhook ran.
- The GitTarget already reports it as `MultipleSourceNamespaces`
  (`internal/controller/gittarget_controller.go:91`).
- `failurePolicy: Ignore` means the webhook was never the guarantee.

**F7. `/validate-all` is dead weight in e2e.**

- It always allows, and matches every write on every resource with `timeoutSeconds: 1`
  (`config/webhook/validating-webhook.yaml`).
- The chart does not ship it.
- Every API write in the e2e system under test makes an extra webhook round trip for nothing.

**F8. No e2e proves a save's author round trip on the audit path.**

- The kcp corner runs without audit (`test/e2e/setup/kcp/instance.yaml:11-14`), so
  `RemoteSaveRequest` is only exercised on the unnamed-attach path
  (`test/e2e/source_cluster_e2e_test.go:872`).
- The main suite runs with audit on, against the in-cluster provider. Once the control-cluster path
  takes its author from audit instead of admission, `save_request_e2e_test.go` proves the round
  trip with no change to where its requests are created.

**F9. Two grant lists with opposite defaults on one GitTarget.**

- `allowedSaveRequests` defaults to `from: Same`, with no `omitempty`, a Go mirror and an envtest.
- `allowedRemoteSaveRequests` defaults to none.
- A reader has to learn both, and why they differ. The design has one list and one default.

**F10. Docs gap.**

- [security-model.md](../security-model.md) does not list the admission endpoint or the
  command-author Redis records as a trust boundary, and its Redis row says "Buffers decoded audit
  events".
- The gap goes away with the endpoint.

**F11. Two KubeProviders on one cluster fight over a request.**

- `RemoteSaveRequestServer.admit` returns `NotAllowed`, "GitTarget … mirrors another cluster than
  this one", when the request's GitTarget names a different KubeProvider
  (`internal/watch/remote_save_requests.go:480`).
- `serve` writes that as a final `Ready=False`, and a finished `Ready` is never served again
  (l.353).
- Two providers enabled on one cluster therefore race: for example the chart's `local` provider
  next to a team's own `inCluster` one, or two kubeconfigs with different credentials. If the
  provider that does not serve the GitTarget sees the request first, the request fails.
- It also waits for the author before it checks the GitTarget, so the wrong provider spends the
  attribution grace first.
- The design fixes it, because the GitTarget's provider alone decides who serves
  ([Which path serves a request](#which-path-serves-a-request)).

## Options considered

- **A: remove admission and takeover, keep both kinds.** Not chosen. Two kinds would keep F4 and
  F9.
- **B: one kind on the source cluster only**, in the first version of this review named
  `SaveRequest` in `source.configbutler.ai`. It was chosen at first and later revised. It made
  CommitRequest the only operator kind that a single-cluster user reaches through a KubeProvider,
  with a provider switch, a grant in source-cluster terms, a derived watch scope and CRD conditions
  on the `local` provider.
- **B': one kind, two ways of picking it up, one core.** **Chosen.** A single-cluster user gets the
  behavior of `main`, and other clusters get the special path.
- **C: no save kind at all.** Not chosen. It removes the user's message on their commit and the
  `catchUp: AfterPush` signal built for gitops-api.

## What goes, by numbers

Line counts are `wc -l` at `70d07d91`. Rows marked "≈" are portions of shared files, estimated from
the functions and blocks involved.

### Admission and takeover

| Item | Non-test | Test |
| --- | ---: | ---: |
| `validate_operator_types_handler.go`, `admission_allow_handler.go`, `watchrule_source_namespace_admission.go` | 339 | 390 |
| `internal/queue/command_author_store.go` | 84 | 83 |
| `saverequest_attribution_agreement_test.go` | | 152 |
| `cmd/main.go`: flags, validation, server setup, store wiring (48 lines mention admission) | ≈110 | ≈40 |
| Takeover: API enums, grant field, `claimOn`, `claims`, trailers, `openWindow.claim` | ≈250 | ≈280 |
| Chart: `admission-server-certificate.yaml`, `validate-operator-types-webhook.yaml`, values, deployment, services, helpers | ≈150 | |
| `config/`: `webhook/` (124), admission `Certificate`, deployment, service and kustomization lines | ≈170 | |
| Docs: `spec/saverequest-admission-authorship.md` (68), sections in where-validation-lives, quickstart step 1, chart README rows | ≈150 | |
| **Subtotal** | **≈1,450** | **≈950** |

### One kind, one core

| Item | Non-test | Test |
| --- | ---: | ---: |
| The `source.configbutler.ai` group: `api/source/v1alpha1` (411) and its CRD (299), plus scheme, PROJECT and kustomize lines | ≈740 | |
| The control-cluster reconciler cut down to a front end over the shared core: 1,468 lines become about 300 | ≈1,170 | ≈1,200 |
| `requestSelector`, and the second grant list merged into one | ≈80 | ≈100 |
| RBAC rules, deepcopy, `hack/crdfields` rows | ≈80 | |
| Added back: F11's dispatch rule, the skipped await, the `NotAllowed` message for a GitTarget that mirrors another cluster | ≈−150 | ≈−250 |
| Added back: the revision check, both KubeProvider conditions, the derived watch scope | ≈−250 | ≈−350 |
| **Subtotal** | **≈1,670** | **≈700** |

**Total: about 3,100 non-test lines (about 700 of them generated CRD and YAML) and about 1,650
test lines, roughly 4,800 net.**

- This is less than option B's 6,600, because the control-cluster path stays. That buys the
  behavior of `main` for single-cluster users.
- The `v1alpha3` `CommitRequest` type and CRD stay, with the remote kind's spec and status.
- The rename of `b86dffbe` is undone for the kind. That is a large diff but adds no lines.
- `save_request_e2e_test.go` (620 lines) stays where it is. Only its kind name and its assertions
  on the author change.

### Kept: `attach: Next` and `whenNothingToCommit: CommitEmpty`

Both stay in the design (decided 2026-10-27). They move into the shared core, so the source-cluster
path gains them, and the counts above are unchanged. The source-cluster server never had them, so
the move needs tests on that path: `Next` skipping an open window, and `CommitEmpty` making a
commit when there is nothing to save.

### Settings

| Kind | Removed | Added |
| --- | --- | --- |
| Operator flags | `--admission-webhook`, `--admission-webhook-bind-address`, `--admission-webhook-cert-path`, `--admission-webhook-cert-name`, `--admission-webhook-cert-key` | |
| Chart values | `servers.admission.{enabled, bindAddress, port, timeoutSeconds, tls.certManager, tls.certPath, tls.certName, tls.certKey, tls.secretNameOverride}` | |
| Install prerequisites | cert-manager for the default install (still needed with attribution) | |
| Kubernetes objects | `ValidatingWebhookConfiguration`, the admission `Certificate` and its Secret, Service port 9443, the `source.configbutler.ai` API group and its CRD | |
| Redis | `author:v1:command:*` | |
| KubeProvider | `spec.remoteSaveRequests` | `spec.commitRequests`, not with `inCluster`; condition `CommitRequestsServed`, printer column `Saves`, `status.commitRequests.namespaces`; in phase 2, condition `CommitRequestCRDUpToDate` |
| GitTarget | `spec.allowedSaveRequests`, `spec.allowedRemoteSaveRequests`, `unattributedWindows`, `requestSelector` | `spec.allowedCommitRequests`, whose grants set `consumerNamespaces` or `sourceNamespaces` |
| CommitRequest | `spec.window.author` | `gitTargetRef.namespace` (optional; required on a source cluster, where a request without it is not picked up), `catchUp` and `status.author` |
| CRD | | the schema revision line in the kind's description, and its hash test |
| Release assets | | `commitrequest-crd.yaml` |
| Conditions and reasons | `AuthorAttributed` (`AttributedFromAdmission`, `CommitterFallback`, `AuthorCaptureDisabled`), `TakeOverNotAllowed` | |
| Commit trailers | `Saved-By:`, `Author-Source:` | |

Compared with `main`, which has `CommitRequest` and the admission webhook, a single-cluster user
sees only these changes:

- the request may name a GitTarget in another namespace, which then admits it in
  `allowedCommitRequests`;
- the author comes from audit, not admission;
- the webhook is gone, and with it the need for cert-manager when attribution is off.

Saving on another cluster is new.

## If takeover is needed again later

Do not bring back a webhook server for it. Two lighter routes exist, and neither is proposed now:

- **A `ValidatingAdmissionPolicy` on the source cluster** that requires
  `object.spec.savedBy == request.userInfo.username`.
  - It is in-tree CEL, GA since Kubernetes 1.30, with no server, certificate or Redis.
  - kcp appears to support it per workspace (kcp PR #3743). That is unverified for the release you
    run.
  - It collides with the declined #5 principle, "authorship is established, never asserted"
    ([attribution.md](../spec/attribution.md) l.39-67): the operator cannot prove the policy ran
    when the request was created. That objection would have to be overruled knowingly.
- **A `MutatingAdmissionPolicy`** that stamps the creator onto the object. It is beta in 1.34 and
  tracked for GA in 1.36, and the same objection applies.

## What this frees time for: the kcp guide

The pitch fits in one sentence: edit a data-only cluster, and every edit and every save lands in
Git under the name of the person who made it.

**There today:**

- a kcp install as kcp-operator CRs (`test/e2e/setup/kcp/`);
- a kubeconfig per workspace through the front-proxy (`test/e2e/kcp_workspace_test.go`);
- `kcp.io/cluster` stripped from Git;
- `attribution.auditRouteAnnotationKey`, for one shared audit stream.

**Missing:**

- the shard audit flags, and the annotation that routes each event to a workspace's KubeProvider;
- per-workspace OIDC (`WorkspaceAuthentication`), so audit names a person rather than an admin;
- installing `commitrequest-crd.yaml` into each workspace, which gitops-api's `workspaceCRDs`
  already does in the managed product;
- the JSON-only caveat;
- an e2e with kcp audit on, which would also close F8 for the source-cluster path.

The e2e instance leaves each of these out on purpose (`instance.yaml:11-14`), and the reference
deployment has them.

**Order:** an e2e kcp corner with audit on comes first. A test then keeps the guide true, and it
is the first place a save's audit round trip is proven across clusters.

## Proposed sequence

Hard cuts, no migration.

1. **gitops-api switches to the source-side save**: the browser creates a `CommitRequest` in the
   workspace. This is the only ordering constraint. Until the cut lands, the kind it creates is
   `RemoteSaveRequest`, so gitops-api can also wait and switch once, straight to the final name.
2. **One PR implements the design:**
   - Delete the admission server, the command-author store, the takeover and the
     `source.configbutler.ai` group.
   - Rename the kind back to `CommitRequest`.
   - Extract the shared core. The control-cluster reconciler and the source-cluster server become
     front ends over it, with the dispatch rule that fixes F11.
   - Add `allowedCommitRequests` with its two fields, and `commitRequests` with its CEL rule.
   - In the core: acquire the fact stream for `commitrequests`, make status writes conditional on
     the resourceVersion with a fresh decision on conflict, claim the request before attaching
     (rule 3), and stamp `delete-after` for retention.
   - Move `attach: Next` and `whenNothingToCommit: CommitEmpty` into the core, so both paths
     have them.
   - Scope each provider's watch to the namespaces it lends, with `status.commitRequests.namespaces`
     and the `CommitRequestsServed` condition.
   - Gate the chart `Issuer`, and move the quickstart's cert-manager step under attribution.
   - Rewrite [commitrequest-design.md](../spec/commitrequest-design.md) to describe the one kind, and
     add a sentence to [definitions.md](../definitions.md): CommitRequest is the one operator kind
     that can also live on a source cluster.
   - Publish the `commitrequest-crd.yaml` release asset, with the schema revision line and its hash
     test.
   - Tests, described in [Acceptance](#acceptance).
3. **Phase 2 of the CRD signal**, as its own PR: first verify that kcp serves per-workspace OpenAPI
   v3, then add the `CommitRequestCRDUpToDate` condition, with a unit test per reason.
4. **The kcp audit e2e corner, then the guide.**

## Acceptance

The main PR is done when these pass, besides `task lint`, `task test` and `task test-e2e`:

- **Overlapping observers**, as unit tests with fake clients:
  - a control-cluster request seen by the local controller and by a kubeconfig watcher pointing at
    the same cluster, with the namespace omitted and with it set;
  - two kubeconfig providers on one source cluster;
  - a GitTarget created during the attach wait, in both orders: when the owner's claim wins, the
    non-owner cannot overwrite it; when the non-owner's `GitTargetNotFound` wins, the owner neither
    attaches nor commits;
  - a status conflict after the worker has produced an outcome: the outcome is re-applied, and the
    save is not restarted;
  - a restart while a request is `Saving`: the owner resumes it.
- **The existing source-cluster tests keep passing** through the extraction of the core, including
  the kcp corner in `test/e2e/source_cluster_e2e_test.go`.
- **`attach: Next` and `CommitEmpty` on the source-cluster path**, which has never had them: `Next`
  skips an open window and takes the next one, and `CommitEmpty` makes a commit when there is
  nothing to save.
- **Delayed audit delivery**, as a unit test: the request's fact arrives late, but within the
  grace, on a provider whose `AuditFactsReceived` is not yet True. The request is attributed.
- **Retention**: `delete-after` is stamped and honored on both paths. Removing the annotation keeps
  the request.
- **The main e2e suite** asserts that a save's author comes from audit on the control-cluster
  path. That is the round trip admission used to stand in for.

**The kcp audit e2e is not a gate for removing admission.** The source-cluster path never used
admission, and removing admission changes only the control-cluster path, which the main suite
covers with audit on. The kcp audit corner proves the source-cluster round trip, and stays step 5.

## Review round 2 (2026-10-27)

An outside review of the previous revision raised five points:

| Point | Verdict | Change |
| --- | --- | --- |
| A source watcher can refuse a valid control-cluster request | **Accepted.** The scenario needs a kubeconfig that points back at the control cluster. That is not an exotic case: it is how a tenant who is not listed in `--in-cluster-kube-providers` gets its own identity there | `GitTargetNamespaceRequired` is gone, and rule 1 applies: a path writes only what it knows to be true |
| Skipping the author wait before the first audit fact loses authors | **Accepted.** The shortcut was this design's own addition, and the code checks `sentAuditFacts` after the wait. The fix is to not add the shortcut | the wait runs whenever attribution is on |
| Several writers of `GitTargetNotFound` are unsafe | **Accepted, narrower than stated.** It needs two observers of one cluster and a GitTarget created during the attach wait. The cause exists, though: the writer overwrites without deciding again | rule 2: writes are conditional on the resourceVersion, and a conflict means deciding again |
| Adopting the remote TTL breaks `delete-after` retention | **Accepted** | the core keeps the annotation contract on both paths |
| The CRD compatibility protocol is large and unresolved | **Partly accepted.** The status writer does erase fields a newer schema added, and the fallback to reading the CRD cannot see an API that an `APIBinding` provides. Deferring the signal entirely was **not** accepted, because an outdated CRD is a signal you asked for | split into phase 1, discovery only, in the main PR, and phase 2, the revision, as its own PR behind a kcp check. A newer CRD is never served, so no preservation rules are needed |

Also accepted: the core's responsibilities are now listed, including acquiring the fact stream,
which the control-cluster reconciler never did.

**Rejected: the kcp audit e2e as a gate for removing admission.** See [Acceptance](#acceptance).

## Review round 3 (2026-10-27)

The same reviewer approved phase 1, and raised one invariant and four smaller points:

| Point | Verdict | Change |
| --- | --- | --- |
| The owner must persist its claim before attaching, because today's `serve` continues after a failed write | **Accepted.** Checked: `write` only logs | rule 3, and both race orders in [Acceptance](#acceptance) |
| Deciding again after a later conflict must not restart a save, and ownership must survive a restart | **Accepted** | under rule 3 |
| The RBAC list omitted `patch` | **Accepted** | listed per mode |
| Not every condition message should say to apply the CRD | **Accepted** | a fix per reason |
| A missing revision means "compatibility unknown", not "from elsewhere" | **Accepted, and taken further.** Phase 1's release asset would otherwise ship without a revision, so every cluster set up in phase 1 would be unjudgeable in phase 2 | the revision line ships in phase 1; phase 2 reports `RevisionUnknown` and keeps serving |
| Keep `Next` and `CommitEmpty` unless their removal is settled | **Accepted**, and since settled: both stay | ported into the core |
