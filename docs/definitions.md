# Definitions

One name per concept, and the rules that decide the name. If you need a word for something and it
is not here, add it here first.

This page completes a set of three rulebooks, and each owns one surface:

| Rulebook | Governs |
|---|---|
| [`style-guide.md`](style-guide.md) | prose: voice, punctuation, spelling, structure |
| [`config-flag-conventions.md`](config-flag-conventions.md) | command-line flags, and the chart values that map onto them |
| this page | concepts, and the API names that carry them: kinds, fields, condition types, reasons, enum values, printer columns |

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
| a field we mirror verbatim | Flux | `spec.accessFrom`, `meta.KubeConfigReference`, `suspend`, `sops` |
| enum value shape | Kubernetes `core/v1` | Flux is inconsistent here, and `core/v1` is not. See rule 7 |
| field, block and timestamp shape | Kubernetes API conventions, then Flux | rules 2 through 4 |

Borrowing is not the same as guessing. A claim that "Flux does it this way" is checked against
`external-sources/flux/` or the module cache before it decides anything, because twice now it has
turned out to be the opposite of what was assumed.

## The two clusters, and the two directions

Almost every naming mistake in this project's history came from one ambiguity: the word "cluster"
without a side, or "namespace" without a direction. Fix the side first and the rest of the
vocabulary follows.

**Control cluster.** The cluster the operator runs in, and where its own six kinds live. When a
document says "a namespace" with no qualifier, it means a control-cluster namespace.

**Source cluster.** The cluster whose objects are mirrored into Git. A `ClusterProvider` names one.
It is the same cluster as the control cluster on a single-cluster install, which is why the two
words have to stay distinct even when they point at one machine: `ClusterProvider.spec.kubeConfig`
omitted means "the control cluster, acting as a source".

**Mirror.** The projection of source-cluster objects into a Git folder. It is a noun for the result
and a verb for the act. The operator mirrors; it does not "sync", "export" or "back up", and those
three words are not used.

**Folder.** The Git directory one `GitTarget` owns, at `spec.path` on `spec.branch`. A folder has
exactly one `GitTarget`, and one `GitTarget` has exactly one folder.

The two directions, which share the word "source" and must not share anything else:

- **Access** is the control-cluster direction: which control-cluster namespaces may reference a
  `ClusterProvider` from a `GitTarget`. `spec.accessFrom` (the spelling is Flux's, on purpose).
- **Source namespace** is the source-cluster direction: which source-cluster namespace a rule reads
  objects from. `WatchRule.spec.rules[].sourceNamespace`, gated by
  `ClusterProvider.spec.allowAnySourceNamespace`.

A sentence about namespace policy names one of those two. Never "namespace access" unqualified.

## Read side

**Cell.** The unit of watching: a `(GitTarget, group, resource, namespace)` tuple. The served
version is carried as data, not as part of the identity, so a type served at two versions is one
cell and not two. One cell is one raw watch. "Cell" is the only word for this; it is not a "watch
target", a "type" or a "scope".

**Stream.** The open watch behind one cell, and its readiness state: `Replaying`, `Streaming` or
`Blocked`. A stream is a thing that runs; a cell is a thing that is selected. `status.streams` on a
`GitTarget` or a rule is a count of streams, keyed by cell.

**Replay.** The initial burst of events a watch opened with `sendInitialEvents=true` delivers before
`initial-events-end`. It is the API server's own term for it.

**Cursor.** A stored `resourceVersion` a stream resumes from after a restart, so the restart does
not cold-replay.

**Catalog.** One source cluster's normalized discovery result. It holds no judgment about whether a
type should be watched.

**Followable.** A verdict about a type, from the relevance funnel: can this type be watched at all.
Separate from **claimed**, which is a verdict about a rule: does any rule select this type. A cell
exists where claimed and followable meet.

**Sweep.** The mark-and-sweep at the end of a replay that deletes managed documents the source no
longer has. A sweep acts on intent, never on an inability to observe.

## Write side

**Branch worker.** The single event loop that owns one `(GitProvider namespace, GitProvider name,
branch)` tuple. Every write to that branch goes through it. Two words, lowercase in prose,
`BranchWorker` as the Go type.

**Commit window.** The interval a branch worker batches writes over before it commits. Windows do
not merge across authors, which is why attribution can be per-commit.

**Publication.** One commit-and-push cycle by a branch worker. The thing a `GitTarget` reports in
`status.remote`.

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
is the `<name>` segment in `/audit-webhook/<name>`, and defaults to the `ClusterProvider`'s own name.

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
`GitProvider.status.branches`. Observations are bounded (counts and capped samples, never a list
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

A **block** groups two or more facts about one subject (`status.remote` holds a revision, a
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

### 5. Condition types are positive state assertions

PascalCase, and phrased so that True is the healthy or settled state: `GitPathAccepted`,
`RenderMatchesLive`, `StreamsRunning`, `SourceNamespaceAuthorized`, `AuditFactsReceived`. Never
`...Failed`, `...Error`, `...Invalid` or `...Missing` as a condition type: those belong in the
reason.

A condition type names one axis and is never reused for a second one. `GitTargetReady` is the health
of a referenced `GitTarget` and is not available for source authorization, which is why
`SourceNamespaceAuthorized` exists separately.

### 6. A reason answers why, and never restates its condition type

`GitPathAccepted=True` with `reason: GitPathAccepted` tells the reader nothing they did not get from
the type. Use the shared vocabulary for the uninteresting end of a binary gate, and a domain reason
wherever there is something to say.

Generic reasons are aliases of [`fluxcd/pkg/apis/meta`](https://github.com/fluxcd/pkg/tree/main/apis/meta),
so one alerting rule spans our kinds and Flux's: `Succeeded`, `Progressing`, `Suspended`, `Failed`.
Domain reasons are ours, and earn their place by carrying information the generic one cannot:
`WriteBoundaryRefused`, `UnsupportedContent`, `RouteUnused`, `KubeConfigInvalid`.

A reason names a cause, not a negated type. `UnresolvedResources` inverts the noun order of its own
condition; `ResourcesNotServed` would name what happened.

A reason never repeats a word the condition type already carries, and never says "yet": the
`Unknown` status already says it.

### 7. An enum value we invent is PascalCase; a value that names something else keeps its spelling

`Never`, `OnEvent`, `Always`, `Plain`, `KustomizeRoot`, `KustomizeOverlay`, `Push`, `Fetch`,
`Ignore`, `PushEmptyCommit`, `ConfigMap`, `Secret`. An acronym stays uppercase (`HTTP`, not `Http`).

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

### 8. Printer columns shorten a name, never rename a concept

A condition's column header is the condition type, or the condition type with a leading qualifier
dropped. `SourceClusterReachable` may print as `SourceReachable`. `AuditFactsReceived` may print as
`FactsReceived`. It may not print as `Facts`, which is a different word and does not lead the reader
back to the condition they now have to describe.

Drop a qualifier only when no sibling column collides after the drop. `GitProviderReady` stays
spelled out because `ClusterProviderReady` sits next to it, and `ProviderReady` beside
`ClusterProviderReady` reads as a general case and a special case of one thing rather than two
peers.

Every condition a controller sets gets a column, at `priority=1` unless an operator needs it in the
default four. Every kind's columns are `<identity>`, `Ready`, `Reason`, ... , `Message`, `Age`, in
that order: the first column is the kind's defining fact when it has one (`URL`, `GitTarget`) and
`Ready` when it does not. A column holding `Ready`'s message is named `Message`, not `Status`.

A column over a field that exists only to feed it is display-only, and its doc comment says so:
`status.streams.summary` is the `3/4` string a column can read in one JSONPath, and nothing computes
from it.

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
| watch target, type, scope (for a watched unit) | cell | "type" and "scope" are both already taken |
| initial sync, backfill | replay | it is the API server's own term |
| git worker, commit worker | branch worker | the tuple it owns is keyed by branch |
| attribution (bare) | author attribution, render attribution | two unrelated concepts share the word |
| `GitRepoConfig` | `GitProvider` | the kind was renamed; the string survived in two reasons |
| failure, error (for a declined write) | refusal | the operator worked correctly |
| namespace access (bare) | access, or source namespace | two directions, two names |
