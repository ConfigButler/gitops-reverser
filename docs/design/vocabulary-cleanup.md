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

## The changes

Grouped by surface, because the surface decides the migration cost. Every row cites the
[`../definitions.md`](../definitions.md) rule it satisfies.

### Condition reasons

Reasons are the largest group and the cheapest to change in code: they are constants in
`internal/controller`. They are also the only group with no schema-level migration path at all, so
every reason below lands in one commit.

| Now | Becomes | Rule | Note |
|---|---|---|---|
| `GitRepoConfigNotFound` | `GitProviderNotFound` | 1 | on `WatchRule` and `ClusterWatchRule`. Names a kind that no longer exists; the Go identifier was renamed years ago and the string was not |
| `GitRepoConfigNotReady` | deleted | 1 | declared on both rule kinds, emitted by neither. Dead constants keeping a dead name alive |
| `GitPathAccepted` | `Succeeded` | 6 | on `GitPathAccepted=True` |
| `RenderMatchesLive` | `Succeeded` | 6 | on `RenderMatchesLive=True` |
| `GitProviderReady` | `Succeeded` | 6 | on `GitProviderReady=True` |
| `ClusterProviderReady` | `Succeeded` | 6 | on `ClusterProviderReady=True` |
| `Pushed` | `Succeeded` | 6 | on `CommitRequest`'s `Pushed=True` |
| `Validated` | `Succeeded` | 6 | on `Validated=True`. `InCluster` stays: it is a second answer with something to say, and it shows the shape rule 6 is asking for |
| `Suspended` | alias `fluxmeta.SuspendedReason` | 6 | same string, now shared rather than re-spelled. No user-visible change |
| `Reconciling` | deleted | 6 | unused in production code; `Progressing` (already a Flux alias) is what the controllers set |
| `Checking` | deleted | 1 | unused in production code, asserted only by tests |
| `Stalled` | `Failed` | 6 | the one generic reason with no upstream equivalent. `fluxmeta` has no `Stalled`, so this aliases `FailedReason`. Used once, in `gittarget_dependency_status.go` |
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
| `WatchRule`, `ClusterWatchRule` | `Target` | `GitTarget` | 1 |
| `ClusterProvider` | `Facts` | `FactsReceived` | 8 |
| `GitTarget` | `ProviderReady` | `GitProviderReady` | 8 |
| `GitProvider`, `ClusterProvider`, `GitTarget` | `Status` | `Message` | 8 |
| `WatchRule`, `ClusterWatchRule`, `CommitRequest` | (absent) | `Message` added | 8 |
| `WatchRule`, `ClusterWatchRule` | (absent) | `ResourcesResolved` added | 8 |
| `GitTarget` | (absent) | `Validated`, `EncryptionConfigured` added | 8 |

`SourceReachable` stays as it is: it is a qualifier drop with no sibling collision, which rule 8
permits. `GitTarget`'s wide output goes from twenty columns to twenty-three, and that is the point at
which rule 8's "every condition gets a column" starts costing something. It is still the right
default, because the alternative is an operator who cannot see a condition exists without describing
the object.

### Status fields

| Kind | Now | Becomes | Prune fails | Strategy |
|---|---|---|---|---|
| `GitTarget` | `status.retention.lastChangedTime` | `lastChangedAt` | n/a, status | delete and rename |
| `GitProvider` | `status.branches[].gitTargets` | `gitTargetCount` | n/a, status | delete and rename |
| `GitTarget` | `status.retention.mode` with no enum | gains the `PruneMode` enum | n/a | narrow, the cheapest row in the matrix |
| both rule kinds and `GitTarget` | `WatchRuleStreamsStatus` + `GitTargetStreamsStatus` | one `StreamsStatus` | n/a | Go-level only; `GitTarget` gains an optional `pendingSample` |

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
