# Definitions

One name per concept, and the rules that decide the name. If you need a word for something and it
is not here, add it here first.

This page completes a set of four rulebooks, and each owns one surface:

| Rulebook | Governs |
|---|---|
| [`style-guide.md`](style-guide.md) | prose: voice, punctuation, spelling, structure |
| [`config-flag-conventions.md`](config-flag-conventions.md) | command-line flags, and the chart values that map onto them |
| [`spec/status-conditions-guide.md`](spec/status-conditions-guide.md) | conditions and reasons: the kstatus trio, polarity, the generic reason vocabulary |
| this page | concepts, and the API names that carry them: kinds, fields, blocks, timestamps, enum values, printer columns |

It binds. A name in the code that contradicts a rule here is a defect, not a variation, and the
cleanup that produced this page is tracked in
[`design/vocabulary-cleanup.md`](design/vocabulary-cleanup.md).

## Who we borrow from, and for what

Two projects set precedent here, and they do not agree about everything, so the split is decided once
rather than per field:

| Surface | Follow | Because |
|---|---|---|
| condition reasons | Flux (`fluxcd/pkg/apis/meta`) | one alerting rule should span our kinds and the Flux kinds in the same cluster |
| readiness shape (`Ready`, `Reconciling`, `Stalled`, `observedGeneration`) | Flux and kstatus | the tools that read a cluster's health read this shape |
| flags and their chart values | Flux | [`config-flag-conventions.md`](config-flag-conventions.md) is built on it |
| a field we mirror verbatim | Flux | `meta.KubeConfigReference`, `suspend`, `sops` |
| who a grant admits (`consumerNamespaces`, with `from: Same`, `Names`, `Selector`, `All`) | Gateway API | `allowedRoutes.namespaces` and its `from`; `Names` is ours, a plain list where Gateway API matches a label, and nothing defaults where Gateway API defaults to `Same` |
| enum value shape | Kubernetes `core/v1` | Flux is inconsistent here, and `core/v1` is not. See rule 7 |
| field, block and timestamp shape | Kubernetes API conventions, then Flux | rules 2 through 4 |

Borrowing is not the same as guessing. A claim that "Flux does it this way" is checked against
`external-sources/flux/` or the module cache before it decides anything, because twice now it has
turned out to be the opposite of what was assumed.

## The two clusters, and the two directions

Almost every naming mistake in this project's history came from one ambiguity: the word "cluster"
without a side, or "namespace" without a direction. Fix the side first and the rest of the
vocabulary follows.

**Control cluster.** The cluster the operator runs in, and where its own ten kinds live. When a
document says "a namespace" with no qualifier, it means a control-cluster namespace.

**Source cluster.** The cluster whose objects are mirrored into Git. A `GitTarget` names it through
its `KubeProvider`. It is the same cluster as the control cluster on a single-cluster install,
which is why the two words have to stay distinct even when they point at one machine:
`KubeProvider.spec.inCluster` means "the control cluster, reached with the operator's own
identity".

**KubeProvider.** A connection to one cluster and the identity used there; a connection, not a role.
The same provider can be a `GitTarget`'s source cluster and an applier claim's management cluster.

**Mirror.** The projection of source-cluster objects into a Git folder. It is a noun for the result
and a verb for the act. The operator mirrors; it does not "sync", "export" or "back up", and those
three words are not used.

**Folder.** The Git directory one `GitTarget` owns, at `spec.path` on its `GitBranch`'s branch. A
folder has exactly one `GitTarget`, and one `GitTarget` has exactly one folder.

The two directions, which share the word "source" and must not share anything else:

- **Access** is the control-cluster direction: which control-cluster namespaces may reference an
  object that lends something (a `GitProvider`, a `GitBranch`, a `KubeProvider`). It is granted on
  the referenced object, in a **grant** list. An omitted list on a `GitBranch` or a `KubeProvider`
  is one grant for the object's own namespace; a written list is the whole policy, and `[]` admits
  nobody. A `GitProvider`'s list has no default.
- **Source namespace** is the source-cluster direction: which source-cluster namespace a rule reads
  objects from. `WatchRule.spec.rules[].sourceNamespace`, bounded by the union of the
  `sourceNamespaces` of the `KubeProvider` grants that admit the `GitTarget`.

A sentence about namespace policy names one of those two. Never "namespace access" unqualified.

## Read side

Use Kubernetes' collection, watch, and initial-events vocabulary in explanatory prose; see
[Kubernetes watch options](facts/kubernetes-watch-options.md). Status identifiers such as
`Replaying` and `status.streams` keep their spelling; their public contract is unchanged.

**Resource collection.** Objects of one group/resource within a namespace selection, as the
API server selects them with an optional label selector. One collection is one list/watch request.
[`CollectionKey`](../internal/types/collection.go) contains `(group, resource, namespace, label
selector)`; its containing `GitTarget` supplies target and source-cluster context. The served API
version is separate data. The Git documents a collection's snapshot owns are structural: every
managed document of the type in the namespace scope, whatever the selector. An all-namespace
collection can overlap a named-namespace collection, and overlapping collections share one
selector. A collection
is distinct from both its resource type and that type's namespaced or cluster-scoped
classification. A rule selects whole collections: every selected collection is observed through
creates, updates, and deletions, and `GitTarget.spec.prune.mode` decides what a removal does to
Git.

**Watch.** The managed observation of a resource collection through Kubernetes watch requests.
It can initialize, stream events, lose a connection, and reconnect. The implementation currently
tracks `Replaying`, `Streaming`, and `Blocked` states. Write `WatchRule` or `ClusterWatchRule`
when naming configuration kinds; a rule selects objects, while a watch observes them.

**Watch stream.** Events delivered by an open watch connection. `Stream` is also used in legacy
implementation names for the managed watch. `status.streams` counts **resource types**: watching
one type in three namespaces produces three collection watches but one status entry, using the
weakest collection state. Changing the vocabulary does not change that public field's unit.

**Initial events.** Synthetic events representing the current selected objects, requested with
`sendInitialEvents=true` and completed by the `k8s.io/initial-events-end` bookmark. Kubernetes
calls this streaming lists. The implementation's `replay` names refer to this initialization;
they do not imply a history of every mutation.

**Resume resourceVersion.** An observed position used to reconnect a running watch. The code
and storage call it a cursor. The first attempt after startup or watch replacement initializes
current state even if a stored cursor exists; later attempts in that watch's loop may resume.
A cursor records observation progress, not successful Git publication.

**Desired-state change filter.** Passes creates, deletes, and updates that change Git-visible
desired state, including data and retained labels and annotations. Drops updates whose sanitized
content matches the accepted baseline; a missing baseline passes. The metric outcome is
`unchanged`. Code: `desiredStateChangeFilter`, one per watch stream, seeded from the stream's
accepted replay; see
[step 5a2](design/gittarget-branch-worker-pending-writes.md#step-5a2-desired-state-change-filter-built).

**Catalog.** One source cluster's normalized discovery result. It holds no judgment about whether a
type should be watched.

**Eligible for mirroring.** The product verdict called `followable` in code: discovery and
mirroring policy permit the type. Kubernetes watch support alone is insufficient. **Selected
by a rule** is the demand called `claimed` in code. The current planner opens watches where
selection and eligibility meet.

**Snapshot reconciliation.** Applying an observed collection to the managed Git documents of its
type and namespace scope. Its sweep can remove documents inferred absent only when pruning policy permits
it; `Always` enables this cleanup, while `Never` and `OnEvent` retain snapshot orphans.
Observation failure supplies no evidence of absence. The code calls this write-side operation
`resync`; client-go informer resync instead notifies handlers about cached objects.

## Write side

**Branch worker.** The single event loop that owns one repository branch: the repository's URL, as
`internal/repourl` normalizes it, and the branch. Every write to that branch goes through it, with
the credential of the `GitProvider` named by the `GitBranch` that holds the branch. Two words,
lowercase in prose, `BranchWorker` as the Go type.

**Branch claim.** Which `GitBranch` holds a repository branch: the oldest one whose credential opened
a push session on the remote. Only the holder gets a branch worker; another `GitBranch` for the same
repository branch reports `BranchConflict`. Say "branch claim", or that a `GitBranch` "holds" its
branch; never a bare "claim", which names a rule's demand on the read side. `BranchClaims` in Go.
The **applier claim** is the cluster side of the same idea: one holds an applier, and another
reports `HeldByAnotherClaim`.

**Write branch.** The branch a `GitTarget` writes to: its `GitBranch`'s `spec.branch`. A grant in the
`GitProvider`'s `spec.allowedGitBranches` must allow it for the `GitBranch`'s namespace.

**Grant.** One entry in a list named `allowed` plus the kind that refers to it
(`GitProvider.spec.allowedGitBranches`, `GitBranch.spec.allowedGitTargets`,
`KubeProvider.spec.allowedGitTargets` and `spec.allowedApplierClaims`). It says which
control-cluster namespaces it admits consumers from (`consumerNamespaces`, with its `from`) and,
where the object lends reach, the scope it gives (`branchPatterns`, `sourceNamespaces`,
`clusterResources`, `applierNamespaces`). Grants combine by union; an explicit scope is the whole
scope. Not "permission" or "policy".

**Consumer.** The object a grant admits or refuses: the `GitBranch` for `allowedGitBranches`, the
`GitTarget` for `allowedGitTargets`, the applier claim for `allowedApplierClaims`. A grant's scope
names it with `{consumer.namespace}`, and a branch pattern may add `{consumer.name}` beside it; the
name alone bounds nothing, since the consumer picks it. The grant's own object is its holder,
and `from: Same` is the holder's namespace, never the consumer's. Not "referrer", "requester" or
"caller".

**Parent branch.** The branch a write branch is created from, and compared against while the write
branch does not exist on the remote: the `GitBranch`'s `spec.parentBranch`, or the remote's default
branch when that is omitted. Say "parent branch"; a bare "parent" is fine where the subject is
already a branch, such as the `status.remote.parent` stanza or a sentence that has just named the parent branch. Use the
full term wherever a parent *resource* could be meant. Not "base", which names a kustomize read-only
base and base trust, and not "source", which names the source cluster. Once the write branch exists,
the parent branch is not followed.

**Commit window.** The interval a branch worker batches one author's writes to one target over before
it commits. Only a write opens one; nothing else does. It closes on its `idleTimeout` of silence or
its `maxDuration` after opening, whichever comes first, or earlier for a flush, unless a
`SaveRequest` holds it. Windows do not merge across authors, which is why attribution can be
per-commit.

**Attach.** What a `SaveRequest`, or a `RemoteSaveRequest` made on a source cluster, does to a commit
window: it gives the window its message and
replaces the window's timers with its own. A request attaches to at most one window and never opens
one. One word for the act, in the API (`window.attach`, `attachTimeout`), the code and the docs: not
"claim", which names a rule's demand on the read side.

**Pending write.** A write a branch worker has decided to make and not yet published: a closed
commit window, an atomic batch, a resync, or a save's empty record. The branch worker keeps its
pending writes in order, commits them, and pushes them on one retry schedule. A pending write can
be waiting to become a commit, committed locally and waiting for a push, or replayed onto a remote
that moved. It carries the actions decided once, on its tree: an upsert per object that changed
the tree and a delete per document removed, while a save keeps every object it captured. A replay
applies those actions to the newer tip instead of planning the cluster's picture again.
`PendingWrite` and `pendingWrites` in Go. Not "log", which also names diagnostic logs and suggests
a durable journal, which this is not.

**Publication.** One commit-and-push cycle by a branch worker. The thing a `GitBranch` reports in
`status.remote`.

**Commit.** A bare Git commit hash, wherever one appears in the API: `status.remote.commit`,
`status.remote.parent.commit`, `SaveRequest.status.commit`, `status.placement.resolvedAtCommit`.
Not "sha", which names an algorithm Git is in the middle of changing, and not "revision", which Flux uses for a polymorphic
identifier that may be a tag, a chart version or a composite like `main@sha1:<hash>`. If this project
ever needs that composite, `revision` is the word waiting for it.

**Placement.** Where in the folder a document lands, and what governs it. Resolved by a scan, and
reported in `status.placement` with a **mode**: `Plain` (no kustomization), `KustomizeRoot` (one
kustomization, self-contained) or `KustomizeOverlay` (one kustomization that also renders a base
outside the folder).

**Render root.** The kustomization directory that governs new documents in a folder.
**Read-only base.** A kustomization directory the folder renders but may never write to.

**Refusal.** A write the operator declined to make, named for its cause
(`WriteBoundaryRefused`, `UnrenderedPlacement`, `MultipleSourceNamespaces`,
`IgnoreShadowsManagedPath`). A refusal commits nothing. It is not a "failure" or an "error": those
words are reserved for something that went wrong, and a refusal is the operator working correctly.

**Prune** is the policy (`spec.prune.mode`: `Never`, `OnEvent`, `Always`). **Retention** is what the
policy kept (`status.retention`). The policy is never called retention and the observation is never
called pruning.

**Refresher.** The periodic re-proof of an idle branch's remote state. Freshness is not base trust,
and the two are not interchangeable.

## Applying back

**Applier.** The object that applies a folder to its source cluster: a Flux `Kustomization` or an
Argo CD `Application`. An applier claim names it. Not "reconciler", which also names this
operator's own controllers, and not "orchestrator", which says nothing about what it does to the
cluster.

**Applier claim.** This operator's resource that declares GitOps Reverser should take and retain
control of when one applier applies: a `FluxClaim` (`spec.kustomizationRef`) or an `ArgoCDClaim`
(`spec.applicationRef`), each with the `KubeProvider` that reaches it. That control covers apply
timing and nothing else: the applier still fetches, renders, applies, prunes and reports health.
The applier claim does not say when to apply either; the branch worker decides that. Among applier
claims that reach an applier through the same `KubeProvider`, one holds it and the others report
`HeldByAnotherClaim`; applier claims made through two `KubeProvider`s for one cluster are not
arbitrated (see **In line**). Holding the applier is not the same as being **in control** of it,
and neither is being **Bound**. While it holds a `Kustomization`, a `FluxClaim` keeps it suspended and resumes it only
for an apply GitOps Reverser asks for. While it holds an `Application`, an `ArgoCDClaim` keeps
automated sync off and starts each sync itself. Deleting either hands the applier back as
`status.previousSuspendState` recorded it. A `GitTarget` names one with `spec.applierClaimRef` and
reports it in its `ApplierClaimReady` condition. Write "applier claim" or the kind name, never a
bare "claim" (see **Branch claim**). The applier claim is never called the applier: the applier is
the object it claims.

**Coordinated.** A folder configured for GitOps Reverser to decide when its applier applies.
Naming an applier claim makes a `GitTarget`'s folder coordinated. The word also describes the
`GitTarget` that writes it. It describes the setup; `ApplierClaimReady` reports whether the applier
claim is ready. A shared folder applied without that coordination is **uncoordinated**: the applier
keeps its own schedule and can revert live edits before they reach Git.

**Eligible.** A `GitTarget` is eligible for the applier claim it names when it is in the applier
claim's namespace, not being deleted, admitted by its `KubeProvider`s, and writes the repository
and branch the applier fetches. An applier claim acts only while an eligible `GitTarget` references
it; one nothing references is `NotReferenced`. Eligibility is judged from those inputs alone, never
from the `GitTarget`'s `Ready`. **Bound** names an applier claim with at least one eligible
`GitTarget`; it says nothing about whether the applier claim holds its applier, or is **in
control** of it.

**In line.** The applier claims that may hold one applier: those an eligible `GitTarget`
references, and one that holds the applier or is handing it back. One of them holds it; the others
report `HeldByAnotherClaim`. Applier claims are in line together only when they reach the applier
through the same `KubeProvider`. GitOps Reverser cannot tell that two `KubeProvider`s reach the same
cluster, so name one `KubeProvider` for every applier claim that reaches a given cluster.

**In control.** An applier claim is in control when it decides, right now, when its applier
applies: nothing the applier starts on its own can apply. Its `InControl` condition reports this,
and the reason says how the applier stands. The reasons and their values are listed with each kind,
in [The `FluxClaim`](bi-directional-flux.md#the-fluxclaim) and
[The `ArgoCDClaim`](bi-directional-argocd.md#the-argocdclaim) (and in
`kubectl explain fluxclaim.status.conditions`). In control is not **holding**: an applier claim
holds its applier from the moment it is first in line until it has handed it back, and while it
hands it back (`Releasing`, `Revoking`) it still holds it but is no longer in control. Nor is it
**Bound**. For a `FluxClaim`, a suspend counts only once it is handled: by then no reconcile that
started before it is still running, so a `Kustomization` suspended but not yet handled is not in
control. Not "held", which names holding; not "controlled", which reads as if the applier claim
were the one controlled; not "fenced"; and not "suspended", which says what happens to a
`Kustomization` rather than what the applier claim does. **Hand back** is releasing
`spec.suspend`, or switching automated sync back, so the applier applies on its own again.

**Apply request.** What GitOps Reverser sends an applier: apply this commit, for this reason
(`ForeignCommit`, `Refusal`, `Repair`, `CatchUp`; a `CatchUp` adopts or converges, both defined
below). It asks; the operation it starts is followed until the applier's status shows the apply
completed. A reason is **pending** for an applier from
the moment the branch worker decides it until an attempt **carries** it; an operation that
succeeds **answers** it, and one that does not gives it back to pending. A pending `Repair` shows
on the applier claim as `status.repairPending`.

**Flush.** What a branch worker does before it sends an apply request: it closes the open commit
window, takes the **flush mark** (the last write decided so far), and publishes every write up to
the mark at once, without waiting for the push cooldown. The request is sent for the tip that
publication produced. An edit decided after the mark does not hold the request back: the applier's
[apply window](design/apply-trigger.md#the-apply-window) records it from the mark, so an apply that
overwrites it is withheld and a `Repair` puts it back.

**Echo.** The applier's own write landing: the watch event of an object the applier wrote while
applying a commit GitOps Reverser asked for. The branch worker knows one by its newest
`managedFields` entries, which are the applier's field managers alone, written at or after the
earliest send of the apply windows still open or carried, to an object nobody edited in the window.
An echo is not mirrored, because its content is the commit's, which Git already holds. An applier
write with no window open is mirrored as an edit, and counted as a late echo.

**Adopt.** A `CatchUp` that has the applier take ownership of an object the API created in its
folder. An applier prunes only what it applied, and an apply is what stamps an object as applied:
Flux's owner labels and inventory, Argo CD's tracking annotation. So an object nobody applied stays
on the cluster when Git later deletes its file. An adopt is pending once the branch worker observes
an API create in a folder an applier applies, or a resync adds an object to Git, and it is never
skipped as already applied. An applier claim's `Adopted` Event names something else, the applier claim taking
hold of its `Kustomization`'s `spec.suspend`.

**Converge.** A `CatchUp` that applies the tip because nothing says the applier runs it: once when
the branch worker starts, and once when a `GitTarget` first names the applier claim (per incarnation, by
uid). The worker keeps, per applier, the commit its last succeeded operation applied, recovered on
restart from the applier claim's `status.operation`, and a converge of that commit asks nothing.

**Apply epoch.** A counter on the branch worker, raised at every send of an apply attempt and at
every end of one. When a snapshot's collection starts, the watch side reads it, with whether **Git
is ahead** of the cluster: an applier on the branch has a `ForeignCommit`, `Refusal` or `Repair`
pending, a converge it may be asked for, or an attempt out. A snapshot collected while Git was
ahead, or across an epoch change, holds a picture the apply has made stale, so the worker discards
it and asks for a fresh one once Git is no longer ahead. A `GitTarget`'s initial snapshots, the
first of each collection until one of it settles, are exempt, and its snapshots are held off for at
most 5 minutes.

**Receiver.** A `GitReceiver`: the endpoint a Git host notifies on a push. Nothing references it: a
delivery wakes every `GitProvider` whose `spec.url` names the repository it reports. The word and
its `type` values are Flux's; the kind is `GitReceiver` because Flux's `Receiver` sits in the same
clusters, and in prose "the receiver" is fine. A delivery to it is a hint to look, never a fact
about the branch: it wakes the branch worker of each provider for the repository and branch it
names, and the worker reads the remote itself.

## Attribution

The word alone is ambiguous, so it is never used alone. There are two:

**Author attribution.** Naming the human or service account behind a change as the Git commit
**author**, from kube-apiserver audit events. Optional. It can never change *what* is written, only
who it is credited to. The flags are `--author-attribution*`; nested configuration drops the
`author` prefix, because a block on a source-cluster object has no other kind of attribution.

**Render attribution.** Deciding which source document a rendered value came from. Unrelated to
commits and to audit.

Inside author attribution:

**Fact.** One minimal record derived from an audit event: who changed what, when. Facts travel on a
transport (Redis Streams, or an in-process ring) and are held in a bounded, TTL'd index.

**Audit route.** The partition facts are keyed by, so two clusters never cross-credit an author. It
is the `<route>` segment in `/audit-webhook/<route>`, and defaults to the `KubeProvider`'s
`<namespace>.<name>`. A route is **held** by one namespace: the one its first dot-segment names when
a provider there carries it, otherwise the namespace of the oldest provider carrying it. A provider
elsewhere that carries it is not used (`AuditRouteConflict`), so no namespace reads another's facts.

**Grace window.** The bounded wait for a matching fact before a watch event ships credited to the
committer instead.

**Author** is the person the change is credited to. **Committer** is the operator's own identity,
and it is what a commit falls back to when no fact matched. The two are the Git fields of the same
name and are never used loosely.

## Status vocabulary

**Condition.** A named assertion about the object, positive and state-style. Four are shared by
every kind: `Ready` (the summary), `Reconciling` and `Stalled` (the kstatus pair), and
`observedGeneration` beside them.

**Gate.** A condition that `Ready` aggregates. A condition that `Ready` deliberately does not
aggregate is not a gate, and its doc comment has to say why not (`AuditFactsReceived` is the
example).

**Latch.** A condition that never regresses once True, because later silence is not evidence of a
fault. `AuditFactsReceived` is the only one.

**Observation.** A status field that reports what was measured rather than asserting health:
`status.streams`, `status.retention`, `status.placement`, `status.remote`,
`GitBranch.status.gitTargetCount`. Observations are bounded (counts and capped samples, never a list
that grows with the cluster) and never drive a condition on their own.

**Reason.** The short answer to "why is this condition in this state". It is not a restatement of
the condition type, and not a sentence: the sentence is the message.

## How we name things

These are the rules. Each one names the surface it governs and the retired spellings it rules out.

### 1. One concept, one word, everywhere

A concept gets one name and keeps it in Go identifiers, field names, condition text, metric labels,
documents and log lines. When a concept is renamed, the old spelling is removed rather than left as
an alias, because two live spellings is how a reader concludes they are two concepts.

### 2. Kinds and fields

Kinds are PascalCase and singular. Fields are camelCase. A reference to another object ends in
`Ref` (`gitTargetRef`, `secretRef`, `knownHostsRef`), and a reference names the kind it points at,
not the role it plays.

A **block** groups two or more facts about one subject (`status.remote` holds a commit, a
timestamp and what proved it). A single fact stays flat (`GitProvider.status.signingPublicKey`).
Do not create a block for one field, and do not leave three related fields flat.

A block in nested configuration drops a prefix that only existed to flatten a namespace:
`spec.attribution.auditRoute`, not `spec.authorAttribution.auditRoute`. This is rule 5 of
[`config-flag-conventions.md`](config-flag-conventions.md) seen from the other side.

### 3. Counts are named as quantities

`total`, `ready`, `blocked`, `replaying`, `retainedDocuments`, `gitTargetCount`. Never the bare
plural of the thing counted: `gitTargets: 3` reads as a truncated list, and every reader has to
check the type to find out it is not one.

### 4. Status timestamps end in `At`

`lastVerifiedAt`, `resolvedAt`, `lastChangedAt`. One suffix, chosen to match the Flux vocabulary
this project already aliases (`lastHandledReconcileAt`). Not `...Time`, which would be the
`metav1` house style and is the losing side of a split we are closing rather than a second option.

Say what the timestamp dates. A field that dates a *resolution* is not evidence that anything
looked recently, and its doc comment has to say so.

### 5. Conditions and reasons are specified in `spec/status-conditions-guide.md`, and it binds

That page is the contract: the `Ready`/`Reconciling`/`Stalled` trio and its single writer, condition
polarity, state-not-transition naming, the Flux-aliased generic reason vocabulary, the rule that a
reason restating its condition type answers nothing, when a status field may mirror a condition, and
the one deliberate deviation from the API conventions. This page does not restate any of it. A rule
about conditions or reasons belongs there, and the next rule is only what that page does not cover.

### 6. A condition type names one axis; a reason names a cause

A condition type names one axis and is never reused for a second one. `GitTargetReady` is the health
of a referenced `GitTarget` and is not available for source authorization, which is why
`SourceNamespaceAuthorized` exists separately.

A reason names a cause, not a negated type. `UnresolvedResources` inverts the noun order of its own
condition; `CatalogNotReady` names what happened. A reason also never repeats a word the condition
type already carries, and never says "yet": the `Unknown` status already says it.

### 7. An enum value we invent is PascalCase; a value that names something else keeps its spelling

`Never`, `OnEvent`, `Always`, `Plain`, `KustomizeRoot`, `KustomizeOverlay`, `Push`, `Fetch`,
`Ignore`, `Reapply`, `ForeignCommit`, `CatchUp`, `ConfigMap`, `Secret`. An acronym stays uppercase
(`HTTP`, not `Http`).

The exception is a value that names a thing outside this API: a tool, an operating system, a
protocol, a Kubernetes resource name. It keeps that thing's own spelling.
`spec.encryption.provider: sops` is lowercase because SOPS spells itself that way and Flux's
`spec.decryption.provider` value matches. An exception has to point at the thing it copies; "it is a
product name" on its own is not enough.

This rule is Kubernetes `core/v1`, measured rather than recalled. Every invented mode there is
PascalCase (`Always`, `IfNotPresent`, `Never`, `ClusterFirst`, `Bidirectional`, `HostToContainer`,
`BestEffort`, `DoNotSchedule`, `Retain`), acronyms are uppercase (`HTTP`, `TCP`, `SCTP`), and the
lowercase values are all names of something else: `linux` and `windows` (operating systems),
`noexec` and `nosuid` (Linux mount flags), `cpu` and `pods` (resource names).

**Flux is not the model for this rule, and that is deliberate.** Its condition reasons and its flag
conventions are what this project borrows from Flux, and it is worth being clear that enums are not:
Flux lowercases several modes it invented itself (`extract;copy`, `none;client;server`, `asc;desc`,
`enabled;warn;disabled`, `poller;legacy`, `small;medium;large`), one of its enums mixes casing inside
itself (`head;HEAD;Tag;TagAndHEAD`), and `HelmRelease.spec.uninstall.deletionPropagation` is
`background;foreground;orphan` where the `metav1.DeletionPropagation` values it names are `Background`,
`Foreground` and `Orphan`. Where Flux and `core/v1` disagree about a value's shape, follow `core/v1`.
Where they disagree about a *name* we are mirroring, follow Flux, which is the `sops` case.

An enum on a status field carries the same `+kubebuilder:validation:Enum` as the spec field of the
same type. A status mirror of a spec enum with no enum marker is a defect.

### 8. Printer columns: a dependency projection gets none, and a header may shorten but not rename

**Which conditions get a column.** Every condition a controller sets gets one, at `priority=1` unless
an operator needs it in the default four, with one exclusion: a condition that merely **projects
another object's readiness** gets no column at all. `Ready`'s reason already names the dependency that
is not ready (`GitProviderNotReady`, `KubeProviderNotReady`), and the detail an operator then needs
lives on the object being projected, which is where they have to go anyway. A column for it repeats
the default output one place to the right and then sends the reader somewhere else.

This is deliberately narrower than "drop any column whose state is already legible from `Ready`'s
reason", which sounds like the same rule and is not. Because the trio takes its reason from the
worst-ranked gate, that wider test would condemn nearly every domain column we have: a stalled
`GitPathAccepted` or `RenderMatchesLive` also surfaces as `Ready`'s reason. The difference is that
those report **this** object's own work, so a column showing one of them `True` beside `Ready=False`
tells an operator which gates are fine, which no reason can. A projection has no such content: it is
a pointer, and `Ready` already carries the pointer.

**How a header may be shortened.** A header may drop any tokens, as long as what remains still names
the same subject and cannot be read as naming another column's subject. This is not a rule about
length or about which end of the name is cut.

`SourceClusterReachable` may print as `SourceReachable`: nothing else in that output is about the
source. `AuditFactsReceived` may print as `FactsReceived`, but not as `Facts`, which is a different
word and does not lead back to the condition to describe.

`GitProviderReady` may not print as `GitReady`, and `KubeProviderReady` may not print as
`KubeReady`, which is the worked case for why the test is about subjects rather than length.
`GitReady` would sit beside `GitPathAccepted`, where `Git` means the repository, so it reads as a
claim about Git rather than about the `GitProvider` object. `KubeReady` would sit beside
`SourceReachable`, which is about the cluster itself, so it invites the reader to wonder how a cluster
being ready differs from it being reachable. `Provider` is the token that says "the config object, not
the thing it names", and it is the one token neither name can lose. Both are moot now that a
projection gets no column, and the pair is kept here as the example.

**Order.** `<identity>`, `Ready`, `Reason`, the domain columns, `Message`, `Age`. The first column is
the kind's defining fact when it has one (`URL`, `GitTarget`) and `Ready` when it does not. A column
holding `Ready`'s message is named `Message`, not `Status`.

**What a column reads.** `.status`, so True/False/Unknown lines up down the output. A column reads
`.reason` only where the condition's whole content is which of several named states it is in, and
never for a binary gate.

A column may be fed by a field that exists only to feed it. That is the one sanctioned exception to
"do not duplicate between conditions and status fields", and it is
`spec/status-conditions-guide.md`'s to grant, not this page's: `status.streams.summary` is the `3/4`
string a column can read in one JSONPath.

### 9. Refusals are named for their cause, failures for their effect

A refusal reason names what the operator would not do and why (`WriteBoundaryRefused`). A failure
reason names what broke (`ConnectionFailed`, `SecretMalformed`). Do not name a refusal as a failure:
the difference is the whole support contract, and a user who reads "failed" goes looking for an
outage.

## Words we do not use

| Not this | This | Because |
|---|---|---|
| sync, export, back up | mirror | "sync" implies two-way convergence, which this is not |
| repo path, directory | folder | one word for the thing a `GitTarget` owns |
| cell, watch target, type, scope (for the object set) | resource collection | identifies the object set without overloading resource type or resource scope |
| stream (for the managed lifecycle) | watch | reserve watch stream for delivered events; configuration kinds keep their full names |
| initial sync, backfill, replay (for current state) | initial events or initialization | follows Kubernetes streaming-list terminology |
| git worker, commit worker | branch worker | the tuple it owns is keyed by branch |
| log, journal (for what a branch worker holds) | pending writes | they are in memory and not a record of history; "log" also names diagnostic logs |
| attribution (bare) | author attribution, render attribution | two unrelated concepts share the word |
| `GitRepoConfig` | `GitProvider` | the kind was renamed; the string survived in two reasons |
| failure, error (for a declined write) | refusal | the operator worked correctly |
| sha, revision (for a commit hash) | commit | `revision` is Flux's word for a polymorphic identifier, and is reserved for one |
| namespace access (bare) | access, or source namespace | two directions, two names |
| reconciler, orchestrator (for Flux or Argo CD) | applier | "reconciler" also names our own controllers |
| owed, owes, debt, obligation | pending, decided, sent, waiting for | an accounting word names no state: say what is pending, for whom, and what ends it |
| fence, fenced (for what an applier claim does to its applier) | suspend, held | the `Kustomization` is suspended, and the applier claim holds it; say which, and whether the suspend is handled |
| trigger, apply trigger (for an applier claim) | applier claim | the resource does not declare when to apply, and "trigger" already names watch-plan, API-surface and Argo Events triggers |
| quiet branch | flush | no apply waits for a branch with nothing pending; it goes after a flush |
| consumer (for what applies a `GitRepository`) | what applies it, `alsoAppliedBy` | consumer names the object a grant admits |
