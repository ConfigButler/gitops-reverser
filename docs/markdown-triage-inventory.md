# Markdown Triage Inventory

Snapshot taken from branch `poc/redis-copy` on 2026-06-18.

This inventory was generated from Git metadata only. The files listed here were not opened or read
for this pass; the provisional grouping is based only on current path and filename.

Purpose: before merging the rewrite branch, decide whether each newly added Markdown file should
stay where it is, move to `docs/finished/`, move to `docs/future/`, be summarized into a smaller
canonical doc, or be removed.

## Commands used

```bash
git log --diff-filter=A --name-only --format='' origin/main..HEAD -- '*.md' \
  | sed '/^$/d' | sort -u

git diff --name-status --diff-filter=A origin/main...HEAD -- '*.md' | sort
```

At this snapshot, both views found 68 newly added Markdown paths. Some commit-history paths differ
from the current paths because documents were moved after being created.

## Triage legend

- `Keep`: still relevant where it is.
- `Finished`: move to or keep in `docs/finished/`.
- `Future`: move to or keep in `docs/future/`.
- `Summarize`: fold into a canonical doc and keep only a shorter note or remove the original.
- `Remove`: delete before merge.

## Checklist

### Stable or user-facing docs

| Done | File | Decision | Notes |
| --- | --- | --- | --- |
| [ ] | `docs/UPGRADING.md` |  |  |
| [ ] | `docs/security-model.md` |  |  |

### Active design docs

| Done | File | Decision | Notes |
| --- | --- | --- | --- |
| [ ] | `docs/design/audit-readiness-probe-plan.md` |  |  |
| [ ] | `docs/design/e2e-metrics-over-logs-plan.md` |  |  |
| [ ] | `docs/design/git-credentials-interop.md` |  |  |
| [ ] | `docs/design/manifest/contextual-namespace-and-kustomize-folder-editing.md` |  |  |
| [ ] | `docs/design/manifest/e2e-full-suite-flakiness-findings-2026-06.md` |  |  |
| [ ] | `docs/design/manifest/file-agnostic-placement.md` |  |  |
| [ ] | `docs/design/manifest/manifest-inventory-file-agnostic-placement.md` |  |  |
| [ ] | `docs/design/manifest/manifestedit-abstraction-plan.md` |  |  |
| [ ] | `docs/design/manifest/manifestedit-integration-readonly-reconcile.md` |  |  |
| [ ] | `docs/design/manifest/manifestedit-writer-followups.md` |  |  |
| [ ] | `docs/design/manifest/reconcile-via-watchlist-mark-and-sweep.md` |  |  |
| [ ] | `docs/design/manifest/version2/api-catalog-watched-type-architecture.md` |  |  |
| [ ] | `docs/design/manifest/version2/catalog-mapper-vs-watched-type-table.md` |  |  |
| [ ] | `docs/design/manifest/version2/discovery-catalog-typeset-boundary.md` |  |  |
| [ ] | `docs/design/manifest/version2/double-repo-detection.md` |  |  |
| [ ] | `docs/design/manifest/version2/dream.md` |  |  |
| [ ] | `docs/design/manifest/version2/gittarget-new-file-placement-rules.md` |  |  |
| [ ] | `docs/design/manifest/version2/gittarget-repository-validity-and-placement.md` |  |  |
| [ ] | `docs/design/manifest/version2/per-type-reconcile-and-streaming-tail.md` |  |  |
| [ ] | `docs/design/manifest/version2/subresource-scope-reduction.md` |  |  |
| [ ] | `docs/design/manifest/version2/type-followability-implementation.md` |  |  |
| [ ] | `docs/design/manifest/version2/type-followability-naming-proposal.md` |  |  |
| [ ] | `docs/design/manifest/version2/type-followability.md` |  |  |
| [ ] | `docs/design/startup-robustness-cert-and-crd-wobble.md` |  |  |
| [ ] | `docs/design/stream/architecture-and-bootstrap.md` |  |  |
| [ ] | `docs/design/stream/audit-diagnostic-streams-plan.md` |  |  |
| [ ] | `docs/design/stream/audit-log-ingestion-and-ordering.md` |  |  |
| [ ] | `docs/design/stream/commitrequest-design.md` |  |  |
| [ ] | `docs/design/stream/deletecollection-resync-nudge-plan.md` |  |  |
| [ ] | `docs/design/stream/github-e2e-per-type-tail-failure-investigation.md` |  |  |
| [ ] | `docs/design/stream/ha-improvements.md` |  |  |
| [ ] | `docs/design/stream/implementation-prompt-materialization-and-status.md` |  |  |
| [ ] | `docs/design/stream/late-lane-e2e-2026-06-16-investigation.md` |  |  |
| [ ] | `docs/design/stream/materialization-tail-and-live-readiness-review.md` |  |  |
| [ ] | `docs/design/stream/signing-overlap-band-coverage-drop-investigation.md` |  |  |
| [ ] | `docs/design/stream/signing-snapshot-tail-replay-failure-investigation.md` |  |  |
| [ ] | `docs/design/stream/watch-list-checkpoint-plan.md` |  |  |
| [ ] | `docs/design/typeset-owns-discovery-grace.md` |  |  |

### Facts

| Done | File | Decision | Notes |
| --- | --- | --- | --- |
| [ ] | `docs/facts/generated-name-support.md` |  |  |
| [ ] | `docs/facts/resource-types.md` |  |  |
| [ ] | `docs/facts/resource-versions.md` |  |  |
| [ ] | `docs/facts/subresources.md` |  |  |

### Finished docs

These are already under `docs/finished/` by current path. The triage question is whether each should
remain as historical context, be summarized into a canonical doc, or be removed.

| Done | File | Decision | Notes |
| --- | --- | --- | --- |
| [ ] | `docs/finished/api-source-of-truth-reconcile.md` |  |  |
| [ ] | `docs/finished/canonical-stream-retirement.md` |  |  |
| [ ] | `docs/finished/commitrequest-barrier-timeout-decision.md` |  |  |
| [ ] | `docs/finished/commitrequest-multi-finalize-design.md` |  |  |
| [ ] | `docs/finished/current-flows-and-cutover.md` |  |  |
| [ ] | `docs/finished/current-manifest-support-review-feedback.md` |  |  |
| [ ] | `docs/finished/current-manifest-support-review.md` |  |  |
| [ ] | `docs/finished/demand-driven-type-materialization-lifecycle.md` |  |  |
| [ ] | `docs/finished/demand-gated-audit-ingestion.md` |  |  |
| [ ] | `docs/finished/gvk-gvr-mapping-layer.md` |  |  |
| [ ] | `docs/finished/implementation-plan.md` |  |  |
| [ ] | `docs/finished/m12-bootstrap-decoupling-plan.md` |  |  |
| [ ] | `docs/finished/manifest-parser-poc.md` |  |  |
| [ ] | `docs/finished/manifestedit-field-ownership-spike.md` |  |  |
| [ ] | `docs/finished/manifestedit-new-file-placement-spike.md` |  |  |
| [ ] | `docs/finished/per-resource-type-rv-keyed-streams-experiment.md` |  |  |
| [ ] | `docs/finished/pr164-review-completion.md` |  |  |
| [ ] | `docs/finished/scale-subresource-audit-rehydration.md` |  |  |
| [ ] | `docs/finished/sops-single-file-no-multidoc.md` |  |  |
| [ ] | `docs/finished/type-lifecycle-events-and-wobble-settling.md` |  |  |
| [ ] | `docs/finished/wildcard-ci-failure-findings.md` |  |  |

### Future docs

| Done | File | Decision | Notes |
| --- | --- | --- | --- |
| [ ] | `docs/future/ha-gittarget-distribution-plan.md` |  |  |

### Internal or fixture docs

| Done | File | Decision | Notes |
| --- | --- | --- | --- |
| [ ] | `internal/git/manifestedit/DECISION.md` |  |  |
| [ ] | `internal/manifestanalyzer/testdata/contextual-namespace/README.md` |  |  |

## Path moves inferred from Git metadata

The following paths appeared in the commit-history "added Markdown" list but are not current added
paths versus `origin/main`. They likely represent documents that were moved or replaced later in the
branch. This is path-only information; contents were not inspected.

```text
docs/design/e2e-full-suite-flakiness-findings-2026-06.md
docs/design/manifest/current-manifest-support-review-feedback.md
docs/design/manifest/current-manifest-support-review.md
docs/design/manifest/dream.md
docs/design/manifest/gittarget-repository-validity-and-placement.md
docs/design/manifest/gvk-gvr-mapping-layer.md
docs/design/manifest/implementation-plan.md
docs/design/manifest/per-type-reconcile-and-streaming-tail.md
docs/design/manifest/pr164-review-completion.md
docs/design/manifest/sops-single-file-no-multidoc.md
docs/design/manifest/version2/m12-bootstrap-decoupling-plan.md
docs/design/manifest/version2/scale-subresource-audit-rehydration.md
docs/design/manifest/version2/type-lifecycle-events-and-wobble-settling.md
docs/design/stream/api-source-of-truth-reconcile.md
docs/design/stream/canonical-stream-retirement.md
docs/design/stream/commitrequest-barrier-timeout-decision.md
docs/design/stream/commitrequest-multi-finalize-design.md
docs/design/stream/current-flows-and-cutover.md
docs/design/stream/demand-driven-type-materialization-lifecycle.md
docs/design/stream/demand-gated-audit-ingestion.md
docs/design/stream/per-resource-type-rv-keyed-streams-experiment.md
docs/future/file-agnostic-placement.md
docs/future/manifest-inventory-file-agnostic-placement.md
docs/future/manifest-parser-poc.md
docs/future/manifestedit-abstraction-plan.md
docs/future/manifestedit-field-ownership-spike.md
docs/future/manifestedit-integration-readonly-reconcile.md
docs/future/manifestedit-new-file-placement-spike.md
docs/future/manifestedit-writer-followups.md
docs/wildcard-ci-failure-findings.md
```

The following current added paths did not appear under the same path in the commit-history additions
list, which is the other side of the same move/replacement story.

```text
docs/design/manifest/e2e-full-suite-flakiness-findings-2026-06.md
docs/design/manifest/file-agnostic-placement.md
docs/design/manifest/manifest-inventory-file-agnostic-placement.md
docs/design/manifest/manifestedit-abstraction-plan.md
docs/design/manifest/manifestedit-integration-readonly-reconcile.md
docs/design/manifest/manifestedit-writer-followups.md
docs/design/manifest/version2/dream.md
docs/design/manifest/version2/gittarget-repository-validity-and-placement.md
docs/design/manifest/version2/per-type-reconcile-and-streaming-tail.md
docs/finished/api-source-of-truth-reconcile.md
docs/finished/canonical-stream-retirement.md
docs/finished/commitrequest-barrier-timeout-decision.md
docs/finished/commitrequest-multi-finalize-design.md
docs/finished/current-flows-and-cutover.md
docs/finished/current-manifest-support-review-feedback.md
docs/finished/current-manifest-support-review.md
docs/finished/demand-driven-type-materialization-lifecycle.md
docs/finished/demand-gated-audit-ingestion.md
docs/finished/gvk-gvr-mapping-layer.md
docs/finished/implementation-plan.md
docs/finished/m12-bootstrap-decoupling-plan.md
docs/finished/manifest-parser-poc.md
docs/finished/manifestedit-field-ownership-spike.md
docs/finished/manifestedit-new-file-placement-spike.md
docs/finished/per-resource-type-rv-keyed-streams-experiment.md
docs/finished/pr164-review-completion.md
docs/finished/scale-subresource-audit-rehydration.md
docs/finished/sops-single-file-no-multidoc.md
docs/finished/type-lifecycle-events-and-wobble-settling.md
docs/finished/wildcard-ci-failure-findings.md
```

## Removed Markdown paths

These Markdown files are deleted relative to `origin/main`. They are not newly created docs, but are
part of the documentation cleanup surface.

```text
docs/serious-bug/cozystack-bugreport.md
docs/tasks/generated-name-support.md
```
