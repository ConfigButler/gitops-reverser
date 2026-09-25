# Vocabulary cleanup: one name per concept, as a breaking change

> **design**: proposed, nothing implemented yet. The rules are decided and written down in
> [`../definitions.md`](../definitions.md); what is open is how far to take three of the renames.
> Index: [`../INDEX.md`](../INDEX.md)
> Governed by: [`../facts/crd-upgrade-strategies.md`](../facts/crd-upgrade-strategies.md)
> Completes: [`../config-flag-conventions.md`](../config-flag-conventions.md)

## The one-line version

An audit of all six CRDs found that the API is named well where the naming rules were written down
first, and named inconsistently everywhere the rules arrived late. This change writes the remaining
rules down ([`../definitions.md`](../definitions.md)) and then makes the surface obey them, in one
breaking release, because every remaining problem is a user-visible string.

Nothing here is a behavior change. Every item is a name.

## Why one breaking release rather than a deprecation cycle

Three of the items are condition reasons, which no schema can deprecate: a reason is a string a
controller writes into `status`, so there is no field to retain, nothing to reject, and no admission
point to warn from. The only way to change one is to change it, and the only way to give a user
warning is a document. Spreading the change over two releases would therefore buy nothing for the
majority of it while leaving two spellings of several concepts live in between, which is the state
[`../definitions.md`](../definitions.md) rule 1 exists to end.

The schema changes are small enough to ride along. Per
[`../facts/crd-upgrade-strategies.md`](../facts/crd-upgrade-strategies.md), each is checked against
the question that matters: **when this value is pruned, does the object do more or less?** Only one
item in this whole change prunes fail-open, and it is the one held back for a decision.

## What the audit found, and what it got wrong

The audit is the reason this page exists. Three of its findings do not survive contact with the
rulebooks that already existed, and they are recorded here so they are not re-raised:

**The `insecure` flag split is deliberate.**
[`../config-flag-conventions.md`](../config-flag-conventions.md) rule 6 already governs it: the
`-insecure` suffix is a per-component transport toggle (`--metrics-insecure`, `--audit-insecure`,
`--redis-insecure`) and the `insecure-` prefix is a loud dev-only escape hatch
(`--insecure-kubeconfig-exec`, `--insecure-allow-missing-known-hosts`). Only one flag sits on the
wrong side of that rule, and it is a word-order slip rather than a missing convention.

**Bare-noun booleans are sanctioned.** `--author-attribution` is the shape of Flux's `--token-auth`,
and rule 2 names `--enable-http2` as the one tolerated Kubebuilder exception. There is no
inconsistency to fix.

**`--redis-addr` with a `valkey:6379` default is rule 1 working as intended.** The flag is named for
what it controls, not for the implementation behind it, which is the rule's own worked example. It
stays.

And one finding gets narrower rather than disappearing: **`spec.encryption.provider: sops` is
correct.** Flux spells its `spec.decryption.provider` value `sops`, lowercase
(`external-sources/flux/flux2/internal/flags/decryption_provider.go`), and matching the ecosystem
beats internal symmetry, the same argument that justifies aliasing Flux's condition reasons. What
was missing was not the PascalCase rule but its exception, which
[`../definitions.md`](../definitions.md) rule 7 now states.

### The enum casing question, asked and closed

That one value raised a bigger question, since a breaking release is the moment to ask it: should the
PascalCase enums be lowercase, the way much of Flux's surface is? **No, and nothing changes.** The
evidence is in rule 7 and was measured rather than recalled.

Kubernetes `core/v1` is PascalCase for every mode it invents, uppercase for acronyms, and lowercase
only where a value names something outside the API (`linux`, `noexec`, `cpu`). Flux approximates that
convention without holding it: it lowercases modes it invented itself (`extract;copy`,
`none;client;server`, `enabled;warn;disabled`, `poller;legacy`), one of its enums mixes casing
internally (`head;HEAD;Tag;TagAndHEAD`), and `HelmRelease.spec.uninstall.deletionPropagation` is
`background;foreground;orphan` where the `metav1.DeletionPropagation` values it names are
`Background`, `Foreground` and `Orphan`.

So Flux is the wrong model for this specific rule, and our enums already match the right one: `Never`
and `Always` and `Ignore` are literal `core/v1` values, and `ConfigMap;Secret` matches Flux's own
`Secret;ConfigMap`. Flipping them to lowercase would move away from both projects at once. What the
question did produce is the "who we borrow from, and for what" table at the top of
[`../definitions.md`](../definitions.md), which exists so this is settled once instead of per field.

## The changes

Grouped by surface, because the surface decides the migration cost. Every row cites the
[`../definitions.md`](../definitions.md) rule it satisfies.

### Condition reasons

Reasons are the largest group and the cheapest to change in code: they are constants in
`internal/controller`. They are also the only group with no schema-level migration path at all, so
every reason below lands in one commit.

The rule most of these breach is not new and is not this change's to make. **"A reason that restates
the condition type answers nothing and is not used" is already in
[`../spec/status-conditions-guide.md`](../spec/status-conditions-guide.md), under Reason
vocabulary**, and `spec/` binds. So the eight `Succeeded` rows below are not a proposal: they are a
spec the code does not currently obey. The `guide` rows cite it; the numbered rows cite
[`../definitions.md`](../definitions.md).

| Now | Becomes | Rule | Note |
|---|---|---|---|
| `GitRepoConfigNotFound` | `GitProviderNotFound` | 1 | on `WatchRule` and `ClusterWatchRule`. Names a kind that no longer exists; the Go identifier was renamed years ago and the string was not |
| `GitRepoConfigNotReady` | deleted | 1 | declared on both rule kinds, emitted by neither. Dead constants keeping a dead name alive |
| `GitPathAccepted` | `Succeeded` | guide | on `GitPathAccepted=True` |
| `RenderMatchesLive` | `Succeeded` | guide | on `RenderMatchesLive=True` |
| `GitProviderReady` | `Succeeded` | guide | on `GitProviderReady=True` |
| `ClusterProviderReady` | `Succeeded` | guide | on `ClusterProviderReady=True` |
| `Pushed` | `Succeeded` | guide | on `CommitRequest`'s `Pushed=True` |
| `Validated` | `Succeeded` | guide | on `Validated=True`. `InCluster` stays: it is a second answer with something to say, and it shows the shape rule 6 is asking for |
| `Suspended` | alias `fluxmeta.SuspendedReason` | guide | same string, now shared rather than re-spelled. No user-visible change |
| `Reconciling` | deleted | guide | unused in production code; `Progressing` (already a Flux alias) is what the controllers set |
| `Checking` | deleted | 1 | unused in production code, asserted only by tests |
| `Stalled` | `Failed` | guide | the one generic reason with no upstream equivalent. `fluxmeta` has no `Stalled`, so this aliases `FailedReason`. Used once, in `gittarget_dependency_status.go` |
| `Resolved` / `UnresolvedResources` | `Succeeded` / `ResourcesNotServed` | 6 | on `ResourcesResolved`. The False reason currently inverts the noun order of its own type instead of naming a cause |

The `Succeeded` collapse is the item most worth arguing about, so the argument is here rather than
in a commit message. Six conditions currently answer "why are you True?" with their own name. The
alternative to collapsing them is inventing six domain reasons for the uninteresting end of six
binary gates, and `Ready=True` already answers `Succeeded`, so the vocabulary exists and is in use.
Where a True state does have something to say, a domain reason stays: `InCluster` on `Validated`,
`Received` on `AuditFactsReceived`, `AttributedFromAdmission` on `AuthorAttributed`.

Also here, and not a rename: the five test-only compatibility aliases
(`ConditionTypeStreamsReady`, `GitTargetConditionStreamsReady`, `GitTargetStreamsReadyReasonNotReady`,
`GitTargetReasonReady`, `GitTargetReasonConflict`) are deleted and their call sites updated. They
keep the pre-rename `StreamsReady` spelling readable in the tree, which is the single biggest reason
"is this named consistently?" cannot be answered by grep.

### Printer columns

Cheap, visible, and breaking only for someone parsing `kubectl get` output, which nothing should do.

| Kind | Now | Becomes | Rule |
|---|---|---|---|
| `GitTarget` | `ProviderReady`, `ClusterProviderReady` | **both removed** | 8 |
| `WatchRule`, `ClusterWatchRule` | `Target` | `GitTarget` | 1 |
| `ClusterProvider` | `Facts` | `FactsReceived` | 8 |
| `GitProvider`, `ClusterProvider`, `GitTarget` | `Status` | `Message` | 8 |
| `WatchRule`, `ClusterWatchRule`, `CommitRequest` | (absent) | `Message` added | 8 |
| `WatchRule`, `ClusterWatchRule` | (absent) | `ResourcesResolved` added | 8 |

The first row replaces what an earlier draft of this page proposed, and the reasoning is worth keeping
because it changed the rule rather than the row. The draft renamed `ProviderReady` to
`GitProviderReady` so it would stop reading as the general case of `ClusterProviderReady` beside it.
Shortening the other way (`GitReady` and `ClusterReady`, symmetric and narrower) does not work:
`GitReady` sits beside `GitPathAccepted`, where `Git` is the repository, and `ClusterReady` sits beside
`SourceReachable`, which is about the cluster itself. `Provider` is the token that distinguishes the
config object from the thing it names, so neither name can lose it.

Which left the question worth asking: why are those two columns there at all? Both only project another
object's `Ready`, and `Ready`'s own reason on the `GitTarget` already carries `GitProviderNotReady` or
`ClusterProviderNotReady` when either is why the target is not ready. They are the first two
progressing contributors in `internal/controller/gittarget_controller.go`, so in the ordinary
single-cause case the default `REASON` column has already said it. The column repeats it and then
sends the reader to the other object, which is where the detail lives anyway.

So rule 8 now excludes a dependency-readiness projection from getting a column, and the asymmetry
disappears without a rename. `GitTarget`'s output goes from twenty columns to eighteen. The two
conditions are still set, still in `kubectl describe`, and still aggregated into `Ready`.

`SourceReachable` stays: dropping `Cluster` collides with nothing, which rule 8 permits. `Validated`
and `EncryptionConfigured` are **not** given columns after all; they were in the draft under the
"every condition gets a column" clause, and dropping them is the same judgment applied consistently to
`GitTarget`'s own-spec gates.

**One consequence to accept or reject.** The same rule condemns the existing `GitTargetReady` column
on `WatchRule` and `ClusterWatchRule`: it too projects another object's `Ready`, and it literally
copies that object's reason (`gitTargetReadyReasonIsStalled` in `internal/controller/stream_status.go`
switches on `GitTarget` reasons). Dropping it would be consistent. It is listed here rather than in the
table because it was not part of the audit, and a rule discovering extra work is exactly the moment to
say so out loud instead of quietly widening the change.

`ResourcesResolved` is not a projection and keeps its new column. It reports this rule's own work,
answering whether `spec.rules[]` matched anything the cluster serves, and no other object holds that
answer.

### Status fields

| Kind | Now | Becomes | Prune fails | Strategy |
|---|---|---|---|---|
| `CommitRequest` | `status.sha` | `status.commit` | n/a, status | delete and rename |
| `GitTarget` | `status.remote.revision` | `status.remote.commit` | n/a, status | delete and rename |
| `GitTarget` | `status.placement.resolvedAtRevision` | `resolvedAtCommit` | n/a, status | delete and rename |
| `CommitRequest` | column `SHA` | column `Commit` | n/a | follows the field |
| `GitTarget` | `status.retention.lastChangedTime` | `lastChangedAt` | n/a, status | delete and rename |
| `GitProvider` | `status.branches[].gitTargets` | `gitTargetCount` | n/a, status | delete and rename |
| `GitTarget` | `status.retention.mode` with no enum | gains the `PruneMode` enum | n/a | narrow, the cheapest row in the matrix |
| both rule kinds and `GitTarget` | `WatchRuleStreamsStatus` + `GitTargetStreamsStatus` | one `StreamsStatus` | n/a | Go-level only; `GitTarget` gains an optional `pendingSample` |

#### One commit hash, three field names

`CommitRequest.status.sha`, `GitTarget.status.remote.revision` and
`GitTarget.status.placement.resolvedAtRevision` all hold the same kind of value: a bare 40-character
commit hash. Verified rather than assumed, at the three places each is written:
`pw.CommitSHA.String()` in `internal/git/branch_worker.go`, `outcome.Head.String()` and
`report.HEAD.Sha` for the remote observation, and `head.Hash().String()` in `worktreeRevision`. One
concept, two words, which is rule 1.

Neither of the two words is the right one, and Flux says why in its own doc comments. Its
`Artifact.Revision` is "a human readable identifier traceable in the origin source system. It can be
a Git commit SHA, Git tag, a Helm index timestamp, a Helm chart version, etc.", so `revision` is
deliberately polymorphic across source kinds, and its Git form in `v1` is composite
(`main@sha1:<hash>`) rather than a bare hash. Its `GitRepository.spec.ref.commit` is documented as
"Commit SHA to check out", which is the bare hash. And Flux has no `sha` field anywhere in any of its
APIs.

So the word for a bare Git commit hash is **`commit`**. It is what Flux calls that value, it is one
word for all three fields, and it drops an algorithm name that Git's SHA-256 transition makes a poor
choice for a field. `revision` is left available for a composite identifier if this project ever needs
one, which is the distinction Flux is making and which naming both the same thing would destroy.

`status.placement.resolvedAtRevision` becomes `resolvedAtCommit`, which also lands it on rule 4: the
`At` names a point, and what follows now says what kind of point.

Status fields are the easy case and the matrix says why: the operator writes them, no user manifest
carries them, and nothing is pruned from a spec. A renamed status field is absent for one
reconcile and then present under its new name. The enum narrowing on `status.retention.mode` is safe
for the same reason: only the controller writes it, and it only ever writes the three values the
enum lists.

The two flat-versus-nested findings are **resolved by writing the rule down, not by moving
anything**. Rule 2 says a block groups two or more facts about one subject and a single fact stays
flat, which retro-fits `GitTarget.status.remote` (three facts, a block) and
`GitProvider.status.lastVerifiedAt` (one fact, flat) with no change. The same is true of the
first-column ordering finding, which rule 8's "defining fact, else Ready" covers as written.

### Spec fields and settings

| Surface | Now | Becomes | Prune fails | Strategy |
|---|---|---|---|---|
| `ClusterProvider` | `spec.qps`, `spec.burst` | `spec.client.qps`, `spec.client.burst` | **open** | see below |
| flag | `--allow-insecure-git-http` | `--insecure-allow-git-http` | n/a | rename, chart value follows |
| chart | `controllerManager.gitRefreshInterval` | `git.refreshInterval` | n/a | rename, documented in `UPGRADING.md` |

`spec.qps`/`spec.burst` is the only fail-open row in the change: pruning a per-cluster override
falls back to the operator-wide `--source-cluster-qps`, which on a deliberately throttled provider
means the operator starts talking to that source cluster faster than the user asked. That is a
widening, and the matrix's rule for a widening is retain-and-refuse or a loud pre-upgrade inventory
rather than a quiet delete.

`accessFrom` and `allowAnySourceNamespace` are **not renamed.** The audit read them as two
namespace-policy settings with names that do not say which direction each governs, which is true,
but `accessFrom` is Flux's spelling and `allowAnySourceNamespace` already names its axis. What was
missing is a page that says "access" is the control-cluster direction and "source namespace" is the
source-cluster one, which [`../definitions.md`](../definitions.md) now does. This is the clearest
case on the page of a definition being the right fix for an ambiguity, and it is worth noticing that
the audit reached for a rename first.

## Open: three calls to make

Everything above is decided except these.

**How to land `spec.client.{qps,burst}`.** Retain-and-refuse costs a release of schema residue
plus the code to reject the old spelling, and buys a loud signal for a field that is almost certainly
unset on every object in existence. A one-shot delete costs nothing and silently unthrottles anyone
who did set it. Recommendation: **one-shot delete with a `UPGRADING.md` inventory command**, on the
grounds that the population is small and knowable, but this is a judgment about that population
rather than about the API, so it is the user's call.

**Whether `Message` goes on all six kinds, or on none.** Adding it is three new columns; dropping it is three
removed ones. Recommendation: **add it**, because the three kinds that have it were written later and
reached for it, which is evidence it earns its place. A Ready message is long, so it is `priority=1`
and second-to-last.

**Whether the True-state reasons collapse to `Succeeded`.** Six True-state reasons become one string, which means an alerting
rule that currently matches `reason=GitPathAccepted` has to match the condition type instead, where
it belonged. Recommendation: **do it.** The counter-case is worth stating: if anything downstream
keys on the True reason of a specific gate, it breaks silently rather than loudly, because the value
is still a valid non-empty reason.

## Commit split

Seven commits, each independently green, ordered so the rules exist before anything cites them:

1. `docs: define the project vocabulary and the naming rules` (`definitions.md`, this page, the
   `INDEX.md` entries)
2. `fix(api): stop naming a kind that no longer exists in two rule reasons` (A, the
   `GitRepoConfig` rows, plus the dead constants)
3. `refactor(api): stop restating condition types in reasons` (A, the rest)
4. `refactor(api): delete the pre-rename condition aliases` (A, the five test-only aliases)
5. `feat(api): name every printer column for the condition it reads` (B)
6. `feat(api): rename the two status fields the naming rules rule out` (C, plus the enum and the
   shared `StreamsStatus`)
7. `feat(api)!: group the per-cluster client throttles, and fix one flag's word order` (D, the
   breaking half, gated on the throttle-override call above)

Then `docs: record the vocabulary cleanup in the upgrade guide` (`UPGRADING.md`), written in the
present tense the way that file requires: "`GitRepoConfigNotFound` is `GitProviderNotFound`", never
"will be renamed".

## Out of scope

- `CommitRequest` not implementing the shared status accessors. Deliberate and documented; it does
  not use the status session helper.
- The `controllerManager.*` chart block as a whole. `gitRefreshInterval` moves because it has an
  obvious home; the remaining four leftovers do not, and inventing a block for them is a bigger
  argument than this change should carry.
- Metric names and labels. They have their own consistency question and
  [`../interpreting-metrics.md`](../interpreting-metrics.md) is the surface that documents them.
- Anything behavioral. If a rename is tempting because the behavior is confusing, the behavior is
  the change and it does not belong here.

## Validation

The usual gate (`task lint`, `task test`, `task test-e2e`, run sequentially), plus two things this
change specifically needs:

Every renamed reason and column string appears in e2e assertions and in docs. `task lint-docs` runs
`hack/doccheck` over every tracked file, so a document still citing an old spelling by path fails the
lint run, but a document citing one in prose does not. A grep for each retired string across
`docs/`, `test/` and `charts/` is part of the commit that retires it.

After the API comment edits, confirm only descriptions moved, per the `AGENTS.md` procedure:
regenerate into a scratch directory with `controller-gen` and compare against `config/crd/bases`
with every `description` key stripped from both. This change edits comments on types that carry
`+kubebuilder:` marker blocks, which is exactly the case where a misplaced comment silently drops a
marker.

## Worked before and after, per kind

The tables above name strings without showing where they sit. This section is the same change seen
from `kubectl`. Column rows are the current headers, taken from `config/crd/bases`; status
snippets show only the fields and reasons this change touches.

### GitProvider

```text
# before
NAME   URL                              READY   REASON      AGE
  -o wide adds:  STATUS   VERIFIED
# after
NAME   URL                              READY   REASON      AGE
  -o wide adds:  MESSAGE  VERIFIED
```

```yaml
# before
status:
  branches:
    - name: main
      gitTargets: 3          # a count named like a list
  lastVerifiedAt: "2026-09-25T09:14:02Z"
# after
status:
  branches:
    - name: main
      gitTargetCount: 3      # rule 3
  lastVerifiedAt: "2026-09-25T09:14:02Z"   # unchanged: rule 4 already satisfied
```

### ClusterProvider

The `Facts` header is the one column that renames its concept instead of shortening it, so an
operator who sees it has no way to guess which condition to describe.

```text
# before
NAME      READY   REASON      FACTS     AGE
  -o wide adds:  VALIDATED  STATUS
# after
NAME      READY   REASON      FACTSRECEIVED   AGE
  -o wide adds:  VALIDATED  MESSAGE
```

```yaml
# before
status:
  conditions:
    - type: Validated
      status: "True"
      reason: Validated             # restates its own type
    - type: AuditFactsReceived
      status: "Unknown"
      reason: RouteUnused           # already correct: names the cause
# after
status:
  conditions:
    - type: Validated
      status: "True"
      reason: Succeeded             # spec/status-conditions-guide.md, Reason vocabulary
    - type: AuditFactsReceived
      status: "Unknown"
      reason: RouteUnused
```

`InCluster` stays as the other `Validated=True` reason. It is the shape the rule is asking for: a
second answer that tells you something `Succeeded` cannot.

### GitTarget

The two provider gates sit side by side in wide output, and only one of them is abbreviated.

```text
# before, -o wide (20 columns, the four provider/source ones shown)
... STREAMSRUNNING   SOURCEREACHABLE   PROVIDERREADY   CLUSTERPROVIDERREADY   STATUS ...
# after (23 columns)
... STREAMSRUNNING   SOURCEREACHABLE   GITPROVIDERREADY   CLUSTERPROVIDERREADY   MESSAGE ...
    plus VALIDATED and ENCRYPTIONCONFIGURED, which the controller sets and no column showed
```

`SOURCEREACHABLE` stays: dropping `Cluster` from `SourceClusterReachable` collides with nothing.
Dropping `Git` from `GitProviderReady` collides with `ClusterProviderReady`, which is why that one
goes back to its full name.

```yaml
# before
status:
  conditions:
    - type: GitPathAccepted
      status: "True"
      reason: GitPathAccepted        # restates its own type
    - type: RenderMatchesLive
      status: "True"
      reason: RenderMatchesLive      # restates its own type
    - type: GitProviderReady
      status: "True"
      reason: GitProviderReady       # restates its own type
  remote:
    revision: 4f2c1ab9e3d5...        # a bare commit hash, called something else on CommitRequest
    verifiedBy: Push
  placement:
    resolvedAtRevision: 4f2c1ab9e3d5...
  retention:
    mode: OnEvent                    # no enum on the schema, unlike spec.prune.mode
    retainedDocuments: 0
    lastChangedTime: "2026-09-25T09:14:02Z"
  streams:
    summary: 3/4
    total: 4
    ready: 3
# after
status:
  conditions:
    - type: GitPathAccepted
      status: "True"
      reason: Succeeded
    - type: RenderMatchesLive
      status: "True"
      reason: Succeeded
    - type: GitProviderReady
      status: "True"
      reason: Succeeded
  remote:
    commit: 4f2c1ab9e3d5...          # rule 1: one word for one concept
    verifiedBy: Push
  placement:
    resolvedAtCommit: 4f2c1ab9e3d5...
  retention:
    mode: OnEvent                    # now carries Enum=Never;OnEvent;Always, rule 7
    retainedDocuments: 0
    lastChangedAt: "2026-09-25T09:14:02Z"   # rule 4
  streams:
    summary: 3/4
    total: 4
    ready: 3
    pendingSample: [apps/deployments]       # gained with the shared StreamsStatus type
```

Nothing changes on the False side. A refused write still reads `GitPathAccepted=False` with
`reason: UnsupportedContent` or `WriteBoundaryRefused`, which is what rule 9 and the conditions
guide both already ask for.

### WatchRule and ClusterWatchRule

This is where the kind-renamed-but-string-did-not shows up, and it is the row an operator
hits: point a rule at a `GitProvider` that does not exist and the reason names a kind that has not
existed for a long time.

```text
# before
NAME   TARGET       READY   REASON                  STREAMS   AGE
rule   my-target    False   GitRepoConfigNotFound    0/0      4m
# after
NAME   GITTARGET    READY   REASON                  STREAMS   AGE
rule   my-target    False   GitProviderNotFound      0/0      4m
```

```yaml
# before
status:
  conditions:
    - type: ResourcesResolved
      status: "False"
      reason: UnresolvedResources    # inverts its own type instead of naming a cause
    - type: ResourcesResolved        # when True, on another object
      status: "True"
      reason: Resolved               # restates its own type
# after
status:
  conditions:
    - type: ResourcesResolved
      status: "False"
      reason: ResourcesNotServed     # rule 6: name the cause
    - type: ResourcesResolved
      status: "True"
      reason: Succeeded
```

`ResourcesResolved` also gains a wide column on both kinds. It is the condition that answers "did
your `rules[]` match anything the cluster serves", and today you have to describe the object to see
it at all.

`ClusterWatchRule` gets the same two changes and not `SourceAuthorized`, which it has no business
carrying: it selects no namespaces.

### CommitRequest

```text
# before
NAME   GITTARGET   READY   REASON      SHA       AGE
  -o wide adds:  AUTHORATTRIBUTED  PUSHED  BRANCH
# after
NAME   GITTARGET   READY   REASON      COMMIT    AGE
  -o wide adds:  AUTHORATTRIBUTED  PUSHED  BRANCH  MESSAGE
```

`GITTARGET` is already right here, and it is the reason the two rule kinds move to match it rather
than the other way round.

```yaml
# before
status:
  sha: 4f2c1ab9e3d5...              # the only field in the API that says "sha"
  conditions:
    - type: Pushed
      status: "True"
      reason: Pushed                 # restates its own type
    - type: AuthorAttributed
      status: "True"
      reason: AttributedFromAdmission  # already correct
# after
status:
  commit: 4f2c1ab9e3d5...           # same value, the word Flux uses for it
  conditions:
    - type: Pushed
      status: "True"
      reason: Succeeded
    - type: AuthorAttributed
      status: "True"
      reason: AttributedFromAdmission
```

### The spec change, and the one flag

```yaml
# before
apiVersion: configbutler.ai/v1alpha3
kind: ClusterProvider
spec:
  kubeConfig:
    secretRef: {name: prod-kubeconfig}
  qps: 20
  burst: 40
# after
spec:
  kubeConfig:
    secretRef: {name: prod-kubeconfig}
  client:
    qps: 20
    burst: 40
```

This is the only fail-open row in the change. Pruned, the two values fall back to the operator-wide
`--source-cluster-qps` and `--source-cluster-burst`, so a deliberately throttled provider starts
talking to its source cluster faster than asked.

```text
# before
--allow-insecure-git-http
# after
--insecure-allow-git-http     # config-flag-conventions.md rule 6, and Flux's --insecure-allow-http
```
