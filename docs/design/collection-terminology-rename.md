# Replace cells with collections and remove event filters

> **Implemented; step 1 of 2.** This is now a historical record of the migration; the code and
> the current documents are the reference. [Label selection and pruning](watches-labels-simplification.md)
> is step 2.
> Index: [`../INDEX.md`](../INDEX.md). Audited: 2026-09-28, source at `69836e70`.
> Rename counts describe tracked working-tree files before adding this plan and its cross-references;
> the existing documentation edits are included. The operation-filter audit is separate, against
> unchanged source at `02e4cd93`. Recount both before implementation.

**Remove the cell concept from active code and documentation.** Call the selected object set a
resource collection and the process observing it a watch. This is the vocabulary already chosen
in [Definitions](../definitions.md#read-side). The object set remains necessary; its project-specific
name and duplicate vocabulary disappear.

**Remove `spec.rules[].operations` from both WatchRule and ClusterWatchRule.** Rules select resource
collections, whose complete lifecycle the operator observes. `GitTarget.spec.prune.mode` decides
whether an observed removal deletes a Git document. There is no second switch to suppress creates,
updates, or deletions within a selected collection.

Kubernetes remains the source of truth and Git follows it. The collection rename preserves behavior;
removing operation filters is an explicit breaking API and behavior change in this same first step.
Step 2 adds server-side selectors, identity-based `Always` pruning for supported Kustomize layouts,
and the required attribution and producer-ordering changes.

## Result and scope

The resulting code reads:

```go
type CollectionKey struct {
    Group     string
    Resource  string
    Namespace string
}

// Existing structures, with their fields renamed:
// targetWatchPlan.Collections map[types.CollectionKey]watchSpec
// ResyncScope.Collection      types.CollectionKey
// Event.SourceCollection     types.CollectionKey
// ResyncRequest.SourceCollection types.CollectionKey
```

A collection still belongs to one GitTarget/source-cluster context. Its key remains versionless;
served version stays in the watch specification, which loses its operation filter. An empty
namespace retains its existing cluster-wide meaning. An all-namespace collection remains distinct
from a named namespace collection, including when they overlap. Zero-valued provenance remains unclaimed.

Make a direct rename throughout the dependency graph. Do not leave `type CellKey = CollectionKey`,
deprecated constructors, duplicate fields, or a wrapper carrying the old vocabulary. Keep
`ResyncScope`: it describes the Git comparison boundary, which step 2 must distinguish from a
label-filtered collection. The rename adds neither selectors nor new state.

## Remove the rule operation filter

### Contract and reason

The current filter lives on
[`ResourceRule.Operations`](../../api/v1alpha3/watchrule_types.go) and
[`ClusterResourceRule.Operations`](../../api/v1alpha3/clusterwatchrule_types.go). It is applied by
[`routeLiveTargetWatchEvent`](../../internal/watch/target_watch.go) after the API server delivers
the event. Initial snapshots already include the full collection. Consequently, a create-only rule
can still write existing objects during initialization, and excluding `DELETE` does not stop
`Always` from pruning an absent identity during a snapshot. Removing the field makes the rule
describe the object set consistently across initialization and live observation.

After this step, every selected collection processes `ADDED`, `MODIFIED`, and `DELETED` events.
Keep bookmark/error handling, content deduplication, sanitization, attribution, and write gates.
Observing every object event does not require a Git commit for an unchanged object. Keep the
existing mapping from a terminating object's `MODIFIED` event to deletion intent.

| GitTarget prune mode | Observed removal | Absence from a complete snapshot |
|---|---|---|
| `Never` | Retain Git document | Retain Git document |
| `OnEvent` (default) | Remove Git document | Retain Git document |
| `Always` | Remove Git document | Remove Git document |

All modes continue accepting creates and updates. There is no create-only or update-only mirror,
replacement event filter, or implicit `operations: ["*"]` field. This also gives step 2 one rule
for label exits: observe the exit, then apply prune policy.

### Additional source and configuration inventory

This focused scan counts Go **identifier tokens**, using the scanner described below and exact
spellings in this table. It finds **316 occurrences in 23 Go files**: 94 in 10 production files,
214 in 12 test files, and eight in the generated deepcopy file. These are review/migration counts;
some identifiers move or survive in a different role. They overlap the cell census and must not
be added to it. Lowercase locals such as `ops`, comments, and string renderings need the source
walkthrough too. Two unrelated `Operations` tokens in Git commit counters are excluded.

| Identifier(s) | Occurrences | Action |
|---|---:|---|
| `Operations` | 57 | Remove rule, compiled-selector, and watch-spec fields and references |
| `OperationSet`, `NamespaceOps` | 47 / 29 | Remove operation sets; retain namespace membership |
| `matchesOperations`, `operationsString`, `operationSpec`, `renderTargetWatchSpecs` | 3 / 3 / 2 / 3 | Delete filter matching and rendering |
| `watchOutcomeOperationFiltered` | 4 | Delete obsolete ingest outcome and its tests |
| `OperationType` | 59 | Remove API/filter uses; move the remaining event classification into `internal/types` |
| `OperationCreate`, `OperationUpdate`, `OperationDelete`, `OperationAll` | 55 / 23 / 19 / 12 | Retain the three event tags internally; delete wildcard selection |

The configuration inventory adds **two CRD properties**, **two generated deepcopy blocks**, and
**one Helm values-schema property**. There are **17 `operations:` entries in 10 active example,
values, or documentation files**, excluding the two CRD properties:

- [`config/samples/`](../../config/samples/): two examples, one entry each.
- [`charts/gitops-reverser/values.yaml`](../../charts/gitops-reverser/values.yaml): one starter-rule entry;
  remove the matching property from [`values.schema.json`](../../charts/gitops-reverser/values.schema.json).
- [`docs/configuration.md`](../configuration.md): two examples, plus its two generated settings rows.
- [`test/e2e/setup/demo-only/`](../../test/e2e/setup/demo-only/): two example entries.
- [`test/e2e/templates/`](../../test/e2e/templates/): ten entries across four rule templates
  (`demo/clusterwatchrule-demo`, `demo/watchrule-all`, `manager/clusterwatchrule-crd`,
  and `restart/watchrule-wildcard`, all `.tmpl`).

Kubernetes admission webhook `rules[].operations` fields in `config/webhook/` and chart webhook
templates remain required for their own API. Do not remove them with a global YAML replacement.
Keep audit verbs, `git.Event.Operation`, commit-operation counts, and mutation classification.
The synthetic CRD-field parser fixture in `hack/crdfields/main_test.go` is also unrelated.

### Implementation changes

1. **API and packaging.** Remove both rule fields and the API's operation enum. Keep the event
   classification type and three concrete tags in `internal/types`, with their existing string
   values, and update audit/watch consumers. Regenerate deepcopy, CRDs, chart CRD copies through
   `task helm-sync`, and the configuration settings index through `task settings-index`.
2. **Rule compilation.** Remove the slice, cloning, and matching check from
   [`compiledSelector`](../../internal/rulestore/store.go), including the operation parameters on
   `GetMatchingRules`, `GetMatchingClusterRules`, and their helpers. Those public matcher methods
   currently have test callers only; preserve their namespace/type contract while simplifying them.
3. **Selection and planning.** In [`watched_type_table.go`](../../internal/watch/watched_type_table.go),
   replace `NamespaceOps` with `NamespaceScopes map[string]struct{}` and remove `watchSelection.ops`.
   Deduplicate namespace selections and served versions without any operation union. Keep named
   and all-namespace collections distinct; this change does not resolve their ordering limitation.
4. **Running watches.** In [`target_watch.go`](../../internal/watch/target_watch.go), use a declared
   watch-key set instead of `map[targetWatchKey]OperationSet`. Remove `targetWatchStream.ops`,
   `runningTargetWatch.spec`, the operation-rendering maps/helpers, and the live `Match` gate.
   Keep `watchSpec{Version: ...}` for plan comparison; version changes still restart a collection.
   Update startup arguments, plan descriptions, sorted-key helpers, and their callers together.
5. **Fingerprints and diagnostics.** Remove operation components from the two rule fingerprints in
   [`watched_type_resolver.go`](../../internal/watch/watched_type_resolver.go) and from the effective
   plan fingerprint in [`owner_observability.go`](../../internal/watch/owner_observability.go).
   Update the in-memory hashes while preserving durable cursor keys. Drop operation suffixes
   from plan logs. Retire `watch_events_total{outcome="operation_filtered"}`; retain the metric name, other
   outcomes, and the exactly-once event census. Update its telemetry comments and documentation.

The internal tag move must preserve [`VerbToOperation`](../../internal/auditutil/objectref.go),
[`IdentityFromAuditEvent`](../../internal/auditutil/identity.go), deletion body preference,
deletion-intent mapping, and author lookup. Do not remove audit mutation filtering or change the
grace policy as a side effect of deleting a user-configurable rule filter.

### Upgrade contract

Keep `v1alpha3` and remove the field directly, consistent with the project's pre-1.0 migration
approach. Update live rules, checked-in manifests, and Helm values; no alias or compatibility toggle
survives. A former subset such as `[CREATE]` now observes updates and removals too. If Git documents
must survive removals, set the target's prune mode to `Never` before removing the old field or
upgrading. This policy applies to the whole GitTarget; mixed per-rule retention has no automatic
equivalent. Create-only/update-only behavior has no replacement.

Do not promise every stale manifest is rejected. Kubernetes
[field validation](https://kubernetes.io/docs/reference/using-api/api-concepts/#field-validation)
rejects unknown fields in `Strict` mode; `Warn` and `Ignore` can accept the request while
[pruning the unknown field](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions/#field-pruning).
The Helm values schema rejects the removed starter-rule key. Document and test these paths so a
leftover field cannot be mistaken for active deletion protection.

Preserve the first-session snapshot in [`runTargetWatch`](../../internal/watch/target_watch.go),
even with a stored cursor, so current objects converge after upgrade. A snapshot cannot recover
the historical deletion events a previous filter discarded: `OnEvent` retains those old orphans;
`Always` can remove them through its existing sweep. This change does not retroactively fabricate
deletions or their authors. Document the API removal and retired metric outcome in `UPGRADING.md`.

## Rename inventory and counting method

The audit scans `git ls-files`, reading working-tree contents. Go files are tokenized with
`go/scanner`; identifier occurrences are separated from comment and string occurrences. Other
tracked text is scanned for the standalone word or an identifier component `cell`/`cells`,
including camel case and underscores. Counts are occurrences, not matching lines or distinct
symbols. Thus `Cell types.CellKey` contributes two identifiers. `cancelled` contributes none.

The broad census has **1,452 occurrences in 93 files**. Manual classification finds:

| Population | Files | Identifier occurrences | Comment occurrences | String/text occurrences | Total |
|---|---:|---:|---:|---:|---:|
| Collection-related production Go | 27 | 396 | 204 | 25 | 625 |
| Collection-related Go tests | 27 | 364 | 74 | 52 | 490 |
| Active documentation/reference cleanup | 18 | n/a | n/a | 265 | 265 |
| **Rename work** | **72** | **760** | **278** | **342** | **1,380** |

The remaining 72 occurrences are 33 unrelated Go occurrences, 22 unrelated non-Go occurrences,
and 17 historical references. File categories can overlap: an index or investigation can contain
both current terminology and historical quotes. The 760 Go identifiers use **49 distinct
spellings**, including **24 test function names**. The core declarations are two types, 12
production functions/methods, and eight struct fields; three test helpers also need renaming.

There are **four file moves**, **24 production log-field occurrences across six key spellings**,
and **one production error-message occurrence**. No collection-related `cell` occurs in a
serialized CRD field, generated CRD/chart schema, metric name/label, or Redis cursor key segment.
The API source does contain five comment occurrences, listed below.

### Identifier map

Counts include declarations and references, including tests, but exclude comments and strings.
Identical local spellings in different scopes share a row; these are not symbol-resolution counts.

| Existing spelling | Replacement | Occurrences |
|---|---|---:|
| `CellKey` | `CollectionKey` | 213 |
| `CellKeyFor` | `CollectionKeyFor` | 50 |
| `Cell` | `Collection` | 70 |
| `Cells` | `Collections` | 17 |
| `cellSpec` | `watchSpec` | 38 |
| `SourceCell` | `SourceCollection` | 16 |
| `sourceCell` | `sourceCollection` | 13 |
| `sourceCellForLog` | `sourceCollectionForLog` | 11 |
| `sourceCellForEvents` | `sourceCollectionForEvents` | 4 |
| `refusalCell` | `refusalCollection` | 3 |
| `RefusedCell` | `RefusedCollection` | 7 |
| `RefusedCellSet` | `RefusedCollectionSet` | 6 |
| `CellSet` | `CollectionSet` | 4 |
| `cellsForWatchKeys` | `collectionsForWatchKeys` | 5 |
| `deduplicateCells` | `deduplicateCollections` | 4 |
| `keysByCell` | `keysByCollection` | 3 |
| `sortCells` | `sortCollections` | 6 |
| `describeCells` | `describeCollections` | 4 |
| `cell` | `collection` | 180 |
| `cells` | `collections` | 48 |
| `byCell` | `byCollection` | 2 |
| `gotCell` | `gotCollection` | 2 |
| `planCell` | `planCollection` | 18 |
| `configMapCell` | `configMapCollection` | 8 |
| `cellForTest` | `collectionForTest` | 4 |
| 24 test function names containing `Cell`/`Cells` | Replace the collection component in each name | 24 |

`Cell` covers both `targetWatchKey.Cell()` and the fields on `ResyncScope` and `gitPathRefusal`.
`sourceCell` covers both `targetWatchStream.sourceCell()` and `WriteRequest.sourceCell()`.
Rename their actual declarations and callers together; avoid substring replacement across all Go.

### File moves

| Existing file | New path |
|---|---|
| `internal/types/cell.go` | [`internal/types/collection.go`](../../internal/types/collection.go) |
| `internal/types/cell_test.go` | [`internal/types/collection_test.go`](../../internal/types/collection_test.go) |
| `internal/git/source_cell.go` | [`internal/git/source_collection.go`](../../internal/git/source_collection.go) |
| `internal/git/source_cell_test.go` | [`internal/git/source_collection_test.go`](../../internal/git/source_collection_test.go) |

Keep the source-provenance rationale with its renamed helper. Update Markdown links and plain
path references in Go comments, including `watchrule_types.go`, `watched_type_resolver.go`, and
`internal/git/types.go`. New paths above are planned paths; their links become valid after the move.

## Walk through the affected behavior

### Identity, planning, and watch lifecycle

Start with the key and constructor in `internal/types`. Preserve equality, comparability,
`String()` output, namespace matching, and version omission byte for byte. Then update
[`target_watch_plan.go`](../../internal/watch/target_watch_plan.go): `Collections` maps keys to
`watchSpec`; `Keep`, `Start`, `Restart`, and `Stop` retain their meaning and ordering. Operation-only
differences disappear from that specification; served-version changes and forced restarts remain.

In [`target_watch.go`](../../internal/watch/target_watch.go), rename key conversion and provenance
helpers and update every keyed map, loop variable, and cancellation comment. Adding an unrelated
rule still preserves running watches; changing served version remains a restart of the same
collection. This phase does not change cancellation timing or fix the known replacement race.

[`stream_readiness.go`](../../internal/watch/stream_readiness.go),
[`watch_plane_state.go`](../../internal/watch/watch_plane_state.go), and namespace planning must
use the same key. Preserve map-copy isolation and the weakest-state roll-up per resource type.
A type observed in three namespaces still counts as one type in public status.

### Git writes, resyncs, and refusals

Rename provenance in `Event`, `ResyncRequest`, write aggregation, queue logging, and
[`refusal_touch.go`](../../internal/git/refusal_touch.go). Preserve these distinct cases:

- A single producer supplies its collection; mixed-producer or whole-target work remains unclaimed.
- `ResyncRequest.refusalCollection()` continues deriving its key from the resync scope. It must
  not be replaced by diagnostic `SourceCollection`, which answers a different question.
- A successful collection clears only that collection's write refusal and queued refusal commit.
  Whole-target scan refusals retain their separate meaning and precedence.
- A nil `ResyncScope` continues covering the whole target. `CollectionKey{}` as provenance means
  unclaimed; it must not become an all-resources wildcard in `Matches`.

[`markResyncTailForWriteLocked`](../../internal/git/branch_worker.go) still matches target and
structural resource identity, including payload-free deletes. Provenance remains diagnostic;
no consumer-side watch-plan filter or generation fence is introduced.

### Readiness, render fidelity, and retention

Update both halves of the render-fidelity gate and all acceptance/retention maps. Scope revisions
must remain attached to the watch incarnation that produced each report. Kept watches retain
their result; stale canceled reports remain rejected. Renaming `CellSet`/`RefusedCellSet` must
preserve the distinction between a scoped refusal and a target-wide refusal.

Retention continues reporting documents the prune policy kept outside the selected snapshot.
Counts, revision checks, and the existing overlap-counting limitation remain as implemented.
Neither retention nor readiness changes units in this phase.

## Compatibility surfaces

| Surface | Step 1 contract |
|---|---|
| Internal Go types, fields, helpers, and tests | Direct rename; no compatibility aliases |
| `cell` log key, 13 occurrences | `collection` |
| `sourceCell` log key, 7 occurrences | `sourceCollection` |
| `keepCells`, `startCells`, `restartCells`, `stopCells`, one each | Corresponding `*Collections` keys |
| Duplicate-collection error in `targetWatchPlanFor`, one occurrence | Replace the word `cell`; retain detection and values |
| `CollectionKey.String()` and `ResyncScope.String()` | Preserve rendered values; coalescing/refusal keys use them |
| CRD fields, condition types/reasons, printer columns | Remove only the two `rules[].operations` properties; preserve the remaining contract |
| Metrics and their labels/values | Retire `watch_events_total`'s `operation_filtered` outcome; preserve remaining definitions |
| Redis cursor and author keys, TTLs | Preserve bytes and versions |
| Plan fingerprints and log descriptions | Remove operation components; preserve collection/version identity |
| Git paths, placement, encryption, prune policy | Preserve behavior; every live object event now reaches the normal processing path |

The six log keys change in place. Document the mapping in `docs/UPGRADING.md` when implementing,
using present tense as required by the repository. Mention the change for log-query consumers;
do not invent duplicate old/new fields as a compatibility layer.

[`watchCursorKey`](../../internal/queue/redis_store.go) already encodes target UID, group/resource,
and namespace under `:watch:v1:`. It contains no cell segment. Preserve its golden strings in
[`key_prefix_test.go`](../../internal/queue/key_prefix_test.go), including the arbitrary
`cell-a:tenant-7` user prefix. That sample is not collection identity. Step 2 owns selector-aware
cursor identity and any resulting storage-version decision.

The five rename occurrences in [`watchrule_types.go`](../../api/v1alpha3/watchrule_types.go) are
rationale or helper comments. Keep them outside field doc comments and Kubebuilder marker blocks.
Use the repository's description-stripped schema comparison for comment-only edits. For the full
step, allow exactly the two `rules[].operations` property removals, including their item enums;
all remaining validation, resource scope, defaults, required fields, and serialization must match.

### Adjacent vocabulary reviewed, outside this rename

These counts are Go identifier occurrences in tracked files, including tests. They are separate
from the cell census. They identify the cost of a broader migration without folding it into this
prerequisite.

| Existing identifier(s) | Occurrences | Disposition |
|---|---:|---|
| `targetWatchStream`, `targetWatchStreams`, `startTargetWatchStreams` | 18 / 6 / 3 | Keep in step 1; managed-watch naming can be changed independently |
| `targetWatchReplayAndStream`, `foldTargetReplayEvent`, `enqueueReplayResync` | 6 / 5 / 3 | Keep identifiers; prose uses initialization/initial events |
| `StreamState`, `StreamStateReplaying` | 10 / 15 | Keep; changing public state/reason vocabulary needs its own complete map |
| `targetStreamStatus`, `StreamSummary`, `StreamsStatus` | 30 / 34 / 14 | Keep status vocabulary and type-count semantics |
| `watchCursorKey` | 9 | Keep; resume-resourceVersion storage remains unchanged |
| `resyncPlan` | 2 | Keep the existing Git snapshot-reconciliation helper |

A watch stream, Redis fact stream, and table cell are valid terms. The acceptance criterion is
removal of the project-specific collection synonym, not removal of those words from every file.
In particular, renaming `status.streams` to `collections` would misdescribe its current type count.

## Documentation work

Update all 18 current documents in the inventory below, including diagrams, section titles,
code examples, and references to types or test names. Keep release/commit/branch quotes literal.
The plan itself necessarily records the old spelling for migration; it is not an active alias.

- In Definitions, remove the transitional `CellKey`/`SourceCell` explanation when the code lands.
  Define the collection directly and point broader status vocabulary to its unchanged contract.
- Update architecture, components, commit-message scope descriptions, and metrics explanations.
  A metric still describes the same observation after its explanatory prose says collection.
- Rename the heading “What a cell leaving means” in `target-watch-plan.md` and fix its inbound
  anchor in `typeset-owns-discovery-grace.md`, plus prose citations in `internal/typeset/lifecycle.go`,
  `docs/TODO.md`, `data-plane-triggering.md`, and `watch-manager-ownership.md`.
- Review `watch-and-catalog-architecture.md` and `target-watch-plan.md` as older design records.
  Updating vocabulary does not adopt their old removal proposals or add missing implementations.
- Rebase step 2's source references and examples onto `CollectionKey`, `SourceCollection`, and
  `watchSpec`. Keep its decisions about cluster authority and full `Always` support intact.
- Remove operation-filter instructions from `configuration.md`, `architecture.md`, `components.md`,
  and the older watch-plan/source-scope designs. Their namespace isolation argument still applies.
  Update `spec/gittarget-isolation-on-rule-change.md` to describe duplicate collection selection
  without operation unions, and `spec/watch-event-ordering-and-attribution-grace.md` to remove the
  filter before attribution. Update `interpreting-metrics.md` and `design/metrics-observability-plan.md`
  for the retired ingest outcome. Cover the changed rule contract in `README.md` and `UPGRADING.md`.

Historical exceptions: the one CHANGELOG entry, 13 occurrences in the two `docs/finished/` records,
and three literal branch/deleted-symbol references in `watch-plane-status-convergence-failures.md`.
Preserve their historical spelling. If a historical link points at a moved source file, update
its destination or pin the historical source; do not leave a broken relative link.

Unrelated exclusions are deliberate: table/layout cells in `hack/crdfields`,
`hack/gitops-layouts-baseline.sh`, Markdown lint/style examples, the Git request ledger, manifest
comparison tables, census metric buckets, the push-conflict matrix, and sparse expansion matrices.
The sample Redis prefix is also unrelated. These account for the 55 unrelated occurrences.

## Implementation sequence

1. **Capture the baseline.** Recount the working tree, record the commit and any existing changes,
   and classify new hits. Confirm the desired `CollectionKey`/`watchSpec` names do not collide.
2. **Rename the Go graph and four files together.** Start with types and fields, then planner,
   watch/queue producers, Git consumers, gates, and tests. Keep the declarations and every caller
   in one reviewable change. Use symbol-aware edits; no temporary alias layer is needed.
3. **Remove operation selection across the same graph.** Apply the API, planner, matcher, live
   routing, internal-tag, metric, and packaging changes above. Remove filter-specific tests while
   preserving their independent namespace, version, ownership, and copy-isolation assertions.
4. **Update diagnostics and documentation.** Apply the six log-key mappings, the error message,
   prose, paths, headings, and affected test names. Cover the deliberate behavior/API change and
   diagnostic changes in the upgrade entry. The original rename counts are a baseline; some of
   those sites disappear with the filter removal instead of being renamed.
5. **Inspect generated output and run the checks below.** Verify the exact schema delta and new
   behavior, as well as the preserved invariants. No selector implementation belongs here.
6. **Land step 1 independently.** Its scope is terminology plus removal of event selection.
   Rebase the label-selection plan onto it before beginning step 2.

No Go renames are performed by creating this document. Implementation and its full validation
suite are the next task.

### Validation for the implementation

Run `task fmt`, `task generate`, `task manifests`, `task helm-sync`, `task settings-index`, and
`task vet`, then the required `task lint` and `task test`. Validate Helm rendering/schema and CRD
installation with the changed rule shapes. Preserve any coverage-baseline increase. Verify Docker
with `docker info` before running `task test-e2e`; run e2e sequentially as the repository requires.
Because the change spans watch plumbing, follow the repository's local-e2e-before-push rule if publishing.

Reuse existing tests for the rename; add focused regressions for operation-filter removal.
Inspect coverage of these preserved behaviors:

| Invariant | Existing evidence to preserve |
|---|---|
| Versionless identity, namespace matching, stable display | `cell_test.go` (renamed), `target_watch_plan_test.go` |
| One watch per collection across served versions; separate namespace scopes | `target_watch_test.go`, `source_namespace_stream_summary_test.go` |
| Unrelated edits keep watches/readiness; forced changes restart the expected set | `target_watch_plan_test.go`, `target_watch_test.go` |
| Nil/zero/mixed provenance and diagnostic rendering | `source_cell_test.go` (renamed), `git_path_refusal_test.go` |
| Scoped snapshots protect sibling namespaces and preserve prune modes | `resync_scope_test.go` in watch and Git packages |
| FIFO and payload-free delete fences prevent unsafe resync coalescing | `internal/git/resync_push_test.go` |
| A recovered collection cannot clear another's refusal | `refusal_observation_test.go`, `git_path_acceptance_scan_test.go` |
| Stale revisions cannot reopen fidelity or restore retired retention counts | `render_fidelity_gate_test.go`, `retention_rollup_test.go` |
| Cursor bytes and prefixes are stable | `internal/queue/key_prefix_test.go`, `redis_store_test.go` |
| Type-count status unchanged; exactly two API properties removed | Stream-summary/controller tests and generated-schema comparison |

Required operation-removal cases:

| Case | Expected result / test location |
|---|---|
| Both rule kinds omit `operations` | Valid CRs; selected collections observe creates, updates, and deletes |
| Old manifests still send the field | Raw requests with `fieldValidation=Strict` fail; `Warn`/`Ignore` drop it, with all-event behavior; cover both CRDs in envtest |
| Helm starter rule still supplies the key | Values-schema validation rejects it; default and corrected examples render and install |
| Live create, changed update, physical delete, terminating `MODIFIED` | Normal mapping and routing in `target_watch_test.go`; no operation-selection gate |
| Delete with each prune mode | `OnEvent`/`Always` remove; `Never` retains; snapshot-only absence still removes only under `Always` |
| Create, unchanged update, delete, recreate under the same name | No-op update suppression survives; delete clears cached content so recreation is written |
| Duplicate rules and several served versions | One collection per namespace scope; deterministic version choice; no operation-union state |
| Named and all-namespace selection coexist | Both scopes remain; removing one rule preserves the other and its readiness |
| Rule-store snapshot isolation | Remaining group/version/resource/namespace slices still cannot mutate stored state |
| A version edit or forced recheck | Restarts the expected collections; replace the old operation-edit restart test with a version edit |
| New watch incarnation with a saved cursor | Starts with a complete snapshot; `OnEvent` does not invent deletions missed by old filters |
| Audit create, PATCH/UPDATE, delete/deletecollection | Same tag strings, body priority, and attribution behavior after the type moves to `internal/types` |
| Delivered watch events and ingest metrics | Exactly one existing applicable outcome each; none emits `operation_filtered` |

### Completion criteria

- No collection-related old identifier, comment, live log key, or active explanatory term remains
  in the audited source graph. No `CellKey` alias or duplicate old/new representation survives.
- Four files are moved, source/doc links resolve, and surviving affected test names use collection.
- The only old spellings remaining are the explicit unrelated/historical exceptions and migration
  documentation. Raw `rg cell` is not a valid zero-occurrence gate because it also matches
  `cancelled` and unrelated table cells.
- Both rule operation fields and their schema, starter-config, matcher, planner, and live-filter
  machinery are gone. Internal operation tags remain; no replacement selection switch exists.
- Outside the two removed API properties and retired metric outcome, serialized contracts and
  metric definitions are unchanged. Redis keys, prune policy, collection equality/matching,
  ordering, revision behavior, and status units remain intact. All object events are processed;
  formerly filtered live events can now change Git. Diagnostics match the documented table.
- Required checks pass and step 2 references the renamed interfaces. Label support is still unbuilt.
- Remove this plan's line from [`.docs-lint-scope`](../../.docs-lint-scope). It is gated only while
  the plan is active; once this step ships, the plan is a historical record and leaves the prose gate.

## Handoff to step 2

| Concern | After step 1 | Label-selection implementation |
|---|---|---|
| Collection identity | Group/resource/namespace | Add canonical selector consistently |
| Watch specification | Served version only; all object events observed | Preserve the same separation from identity; no operation filter |
| Rule contract | Resources, groups, versions, namespace scope | Add `objectSelector`; prune mode still controls removals |
| Provenance | `SourceCollection`, diagnostic | Carry selector-aware identity; retain raw attribution evidence |
| Git sweep predicate | Structural `ResyncScope.Matches` | Keep structural ownership; compare with the selected snapshot |
| Queue coalescing | Existing identifier-based ordering fence | Include selector identity; preserve the structural write fence |
| Watch replacement | Existing cancellation behavior | Add the producer handoff specified by step 2 |
| Redis resume position | Existing `:watch:v1:` bytes | Prevent resume under a different selector; decide format migration there |
| Pruning/Kustomize/attribution | Existing behavior | Full `Always`, shared semantic removal, and filtered-removal attribution |

Do not make `CollectionKey.Matches` evaluate labels when step 2 adds a selector. The object set
observed by a watch and the structural scope of Git objects owned by its mirror answer different
questions. Step 1 gives them accurate names; step 2 establishes the new membership behavior.

## Per-file audit

This is the baseline inventory, not a claim that implementation has happened. `IDs` counts Go
identifier occurrences; `comments` and `strings` count matched words/components inside those tokens.

### Go files

| File | IDs | Comments | Strings | Total |
|---|---:|---:|---:|---:|
| [`api/v1alpha3/watchrule_types.go`](../../api/v1alpha3/watchrule_types.go) | 0 | 5 | 0 | 5 |
| [`internal/authz/source_namespace.go`](../../internal/authz/source_namespace.go) | 0 | 3 | 0 | 3 |
| [`internal/authz/source_namespace_test.go`](../../internal/authz/source_namespace_test.go) | 0 | 1 | 3 | 4 |
| [`internal/git/branch_worker.go`](../../internal/git/branch_worker.go) | 14 | 7 | 5 | 26 |
| [`internal/git/commit.go`](../../internal/git/commit.go) | 4 | 0 | 0 | 4 |
| [`internal/git/git_path_refusal.go`](../../internal/git/git_path_refusal.go) | 7 | 2 | 1 | 10 |
| [`internal/git/git_path_refusal_test.go`](../../internal/git/git_path_refusal_test.go) | 16 | 0 | 0 | 16 |
| [`internal/git/refusal_observation_test.go`](../../internal/git/refusal_observation_test.go) | 9 | 9 | 3 | 21 |
| [`internal/git/refusal_touch.go`](../../internal/git/refusal_touch.go) | 23 | 14 | 1 | 38 |
| [`internal/git/refusal_touch_test.go`](../../internal/git/refusal_touch_test.go) | 2 | 2 | 0 | 4 |
| [`internal/git/render_fidelity_gate.go`](../../internal/git/render_fidelity_gate.go) | 13 | 6 | 0 | 19 |
| [`internal/git/render_fidelity_gate_test.go`](../../internal/git/render_fidelity_gate_test.go) | 16 | 7 | 8 | 31 |
| [`internal/git/resync_flush.go`](../../internal/git/resync_flush.go) | 2 | 3 | 0 | 5 |
| [`internal/git/resync_push_test.go`](../../internal/git/resync_push_test.go) | 8 | 0 | 1 | 9 |
| [`internal/git/resync_scope_test.go`](../../internal/git/resync_scope_test.go) | 2 | 1 | 1 | 4 |
| `internal/git/source_cell.go` | 13 | 17 | 0 | 30 |
| `internal/git/source_cell_test.go` | 19 | 0 | 4 | 23 |
| [`internal/git/types.go`](../../internal/git/types.go) | 17 | 19 | 0 | 36 |
| [`internal/git/write_boundary_precondition_test.go`](../../internal/git/write_boundary_precondition_test.go) | 1 | 0 | 0 | 1 |
| [`internal/rulestore/store.go`](../../internal/rulestore/store.go) | 0 | 1 | 0 | 1 |
| [`internal/telemetry/exporter.go`](../../internal/telemetry/exporter.go) | 0 | 1 | 0 | 1 |
| `internal/types/cell.go` | 6 | 11 | 0 | 17 |
| `internal/types/cell_test.go` | 10 | 3 | 2 | 15 |
| [`internal/typeset/lifecycle.go`](../../internal/typeset/lifecycle.go) | 0 | 2 | 0 | 2 |
| [`internal/watch/config_plane_split_review_fixes_test.go`](../../internal/watch/config_plane_split_review_fixes_test.go) | 1 | 0 | 0 | 1 |
| [`internal/watch/event_router.go`](../../internal/watch/event_router.go) | 22 | 0 | 6 | 28 |
| [`internal/watch/event_router_test.go`](../../internal/watch/event_router_test.go) | 8 | 1 | 0 | 9 |
| [`internal/watch/git_path_acceptance.go`](../../internal/watch/git_path_acceptance.go) | 29 | 9 | 0 | 38 |
| [`internal/watch/git_path_acceptance_scan_test.go`](../../internal/watch/git_path_acceptance_scan_test.go) | 14 | 2 | 2 | 18 |
| [`internal/watch/git_path_acceptance_test.go`](../../internal/watch/git_path_acceptance_test.go) | 53 | 4 | 1 | 58 |
| [`internal/watch/gitpath_events.go`](../../internal/watch/gitpath_events.go) | 0 | 2 | 0 | 2 |
| [`internal/watch/manager.go`](../../internal/watch/manager.go) | 3 | 1 | 0 | 4 |
| [`internal/watch/owner_observability.go`](../../internal/watch/owner_observability.go) | 0 | 1 | 0 | 1 |
| [`internal/watch/owner_test.go`](../../internal/watch/owner_test.go) | 4 | 3 | 0 | 7 |
| [`internal/watch/render_fidelity_gate.go`](../../internal/watch/render_fidelity_gate.go) | 19 | 4 | 2 | 25 |
| [`internal/watch/resync_scope_test.go`](../../internal/watch/resync_scope_test.go) | 1 | 0 | 0 | 1 |
| [`internal/watch/retention_rollup.go`](../../internal/watch/retention_rollup.go) | 12 | 12 | 5 | 29 |
| [`internal/watch/retention_rollup_test.go`](../../internal/watch/retention_rollup_test.go) | 40 | 8 | 4 | 52 |
| [`internal/watch/source_namespace_planning_test.go`](../../internal/watch/source_namespace_planning_test.go) | 0 | 3 | 0 | 3 |
| [`internal/watch/source_namespace_stream_summary_test.go`](../../internal/watch/source_namespace_stream_summary_test.go) | 1 | 3 | 1 | 5 |
| [`internal/watch/source_namespace_test.go`](../../internal/watch/source_namespace_test.go) | 1 | 1 | 0 | 2 |
| [`internal/watch/stream_readiness.go`](../../internal/watch/stream_readiness.go) | 49 | 7 | 0 | 56 |
| [`internal/watch/stream_readiness_test.go`](../../internal/watch/stream_readiness_test.go) | 24 | 1 | 1 | 26 |
| [`internal/watch/target_watch.go`](../../internal/watch/target_watch.go) | 93 | 42 | 0 | 135 |
| [`internal/watch/target_watch_plan.go`](../../internal/watch/target_watch_plan.go) | 64 | 23 | 5 | 92 |
| [`internal/watch/target_watch_plan_test.go`](../../internal/watch/target_watch_plan_test.go) | 120 | 6 | 14 | 140 |
| [`internal/watch/target_watch_session_end_test.go`](../../internal/watch/target_watch_session_end_test.go) | 1 | 2 | 1 | 4 |
| [`internal/watch/target_watch_test.go`](../../internal/watch/target_watch_test.go) | 13 | 13 | 6 | 32 |
| [`internal/watch/watch_event_metrics.go`](../../internal/watch/watch_event_metrics.go) | 0 | 2 | 0 | 2 |
| [`internal/watch/watch_event_metrics_test.go`](../../internal/watch/watch_event_metrics_test.go) | 0 | 1 | 0 | 1 |
| [`internal/watch/watch_plane_state.go`](../../internal/watch/watch_plane_state.go) | 5 | 5 | 0 | 10 |
| [`internal/watch/watched_type_resolver.go`](../../internal/watch/watched_type_resolver.go) | 1 | 5 | 0 | 6 |
| [`test/e2e/audit_route_attribution_e2e_test.go`](../../test/e2e/audit_route_attribution_e2e_test.go) | 0 | 2 | 0 | 2 |
| [`test/e2e/refusal_empty_commit_e2e_test.go`](../../test/e2e/refusal_empty_commit_e2e_test.go) | 0 | 1 | 0 | 1 |

### Current documents

The counts omit the unrelated matrix cell in `docs/INDEX.md` and the three literal historical
references in the status investigation. Other historical and unrelated documents are excluded.

| Document | Occurrences to review |
|---|---:|
| [`docs/INDEX.md`](../INDEX.md) | 5 |
| [`docs/TODO.md`](../TODO.md) | 4 |
| [`docs/UPGRADING.md`](../UPGRADING.md) | 2 |
| [`docs/architecture.md`](../architecture.md) | 6 |
| [`docs/commit-messages.md`](../commit-messages.md) | 1 |
| [`docs/components.md`](../components.md) | 9 |
| [`docs/definitions.md`](../definitions.md) | 6 |
| [`docs/design/data-plane-triggering.md`](../design/data-plane-triggering.md) | 14 |
| [`docs/design/gittarget-configuration-freshness.md`](../design/gittarget-configuration-freshness.md) | 1 |
| [`docs/design/source-scope-simplification.md`](../design/source-scope-simplification.md) | 15 |
| [`docs/design/target-watch-plan.md`](../design/target-watch-plan.md) | 96 |
| [`docs/design/watch-and-catalog-architecture.md`](../design/watch-and-catalog-architecture.md) | 76 |
| [`docs/design/watch-manager-ownership.md`](../design/watch-manager-ownership.md) | 11 |
| [`docs/design/watch-plane-status-convergence-failures.md`](../design/watch-plane-status-convergence-failures.md) | 12 |
| [`docs/design/watches-labels-simplification.md`](../design/watches-labels-simplification.md) | 3 |
| [`docs/interpreting-metrics.md`](../interpreting-metrics.md) | 1 |
| [`docs/spec/typeset-owns-discovery-grace.md`](../spec/typeset-owns-discovery-grace.md) | 1 |
| [`docs/spec/watch-event-ordering-and-attribution-grace.md`](../spec/watch-event-ordering-and-attribution-grace.md) | 2 |

### Reproduce the census

Start from tracked files so ignored `docs/local-only/`, external checkouts, module caches, and
build artifacts do not inflate the plan. This component expression avoids `cancelled`:

```bash
git grep -n -E 'Cell(s)?([^a-z]|$)|(^|[^A-Za-z0-9])(cell|cells|CELL|CELLS)([A-Z0-9_]|[^A-Za-z0-9_]|$)' -- '*.go' '*.md'
git ls-files '*cell*' '*Cell*'
```

That command is for finding candidates; it prints matching lines, so its line count is not the
occurrence count above. To reproduce the precise totals, enumerate `git ls-files -z`, scan Go
with `go/scanner` using `scanner.ScanComments`, and count matching components separately in
`token.IDENT`, `token.COMMENT`, and `token.STRING`. Scan other text using the same component rule,
then apply the unrelated/historical exclusions documented above. Recount on the implementation
base; this plan and new cross-references intentionally add migration mentions after the baseline.
