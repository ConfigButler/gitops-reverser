# manifestedit writer: follow-ups after the file-agnostic placement fixes

> Status: open follow-ups — captured 2026-06-03 on branch `poc/manifestedit`
> Related: [manifestedit-new-file-placement-spike.md](manifestedit-new-file-placement-spike.md),
> [manifest-inventory-file-agnostic-placement.md](manifest-inventory-file-agnostic-placement.md),
> [file-agnostic-placement.md](file-agnostic-placement.md),
> [manifestedit-integration-readonly-reconcile.md](manifestedit-integration-readonly-reconcile.md),
> [manifestedit-field-ownership-spike.md](manifestedit-field-ownership-spike.md)

## What this branch already fixed

A review of the live writer on this branch turned up four defects. Three were
fixed; each has a regression test that started red and now pins the corrected
behavior. This document records what is fixed and what is deliberately left for a
follow-up, so the remaining edges are not lost.

| # | Defect | Fix | Guard test |
|---|---|---|---|
| High | Writer was not file-agnostic: update wrote a second copy at the canonical path and delete missed a moved manifest. | `resolveManifestLocation` indexes the GitTarget tree and edits/deletes the resource where it already lives (match-first); only a genuinely new resource uses the deterministic path. Delete is now per-document via `manifestedit.DeleteDocument`. | `TestApplyEvent_UpdateMustFollowExistingPlacement`, `TestApplyEvent_DeleteMustFollowExistingPlacement` ([internal/git/known_placement_bugs_test.go](../../internal/git/known_placement_bugs_test.go)) |
| Medium | An in-place no-op (multi-doc edge) was staged as a change and could drive an empty commit. | `handleCreateOrUpdateOperation` returns `false` when the preserved edit equals the bytes on disk. | `TestHandleCreateOrUpdate_NoOpInMultiDocReportsNoChange` (same file) |
| Medium | `BuildReport` classified a desired resource whose only Git doc is non-editable as both `Create` and `Skip`. | When there is no editable location but a non-editable record exists, the desired side no longer emits `Create`; the single `Skip` from `gitOnlyEntries` stands. | `TestBuildReport_NonEditableDesiredIsNotDoubleClassified` ([internal/manifestreport/noneditable_desired_bug_test.go](../../internal/manifestreport/noneditable_desired_bug_test.go)) |
| (perf, load-bearing) | Match-first scanned the tree **per event**. A snapshot of many large manifests (cluster-wide CRD watch ≈ O(events × tree) on big YAML) blew the per-commit deadline — a real e2e failure, not just slowness. | `manifestLocator` scans each base path **once per write batch** (the checked-out commit) and caches it. | Covered by the CRD-install e2e (`crd_lifecycle_e2e_test.go`); see "Follow-up 2 (done)". |

Code: [internal/git/git.go](../../internal/git/git.go) (`manifestLocator`,
`handleDeleteOperation`, `handleCreateOrUpdateOperation`),
[internal/git/commit_executor.go](../../internal/git/commit_executor.go) (one
locator per batch),
[internal/manifestreport/report.go](../../internal/manifestreport/report.go).

## Follow-up 1 — DELETE match-first needs the resource identity

Match-first resolves a resource's location from its **content identity** (GVK +
namespace + name), read from the event's object. Production DELETE events carry
only the API identifier (GVR + namespace + name) and **no object**
([internal/reconcile/folder_reconciler.go](../../internal/reconcile/folder_reconciler.go)
builds `toDelete` events with `Identifier` only). The inventory keys by GVK, and
mapping GVR→GVK needs a live RESTMapper, which the manifestedit POC deliberately
does not own.

Consequence today: a delete of a resource a user **moved** off the canonical path
falls back to the deterministic path and therefore misses the moved file (same as
before this branch — safe, not a regression). Update/create placement is fully
fixed; deletes are only fixed when the event happens to carry an object.

Options for the follow-up:

- Attach a minimal identity (apiVersion/kind/name/namespace) to DELETE events in
  the reconcile layer so the writer can content-match without a RESTMapper.
- Or give the writer a GVR→GVK resolver (RESTMapper) and match deletes by GVR.

The first is smaller and keeps the GVR→GVK mapping out of the writer.

## Follow-up 2 (done) — cache the inventory per write batch

Originally deferred as "just performance," this turned out to be **load-bearing**.
The first implementation rebuilt the inventory (`manifestedit.IndexDir`) per event,
so a batch of N writes was O(N × tree). The CRD-install e2e — whose `ClusterWatchRule`
watches **all** CustomResourceDefinitions cluster-wide — snapshots ~40 large CRDs
into one commit; the O(N²) re-scan of those big schemas pushed the single commit
past the test's 60 s deadline, so the spec failed (zero commits). The bug-for-bug
controlled test confirmed it: neutralizing match-first made the spec pass; restoring
it with the per-batch cache also passed.

`manifestLocator` ([internal/git/git.go](../../internal/git/git.go)) now scans each
base path **once per write batch** (the checked-out commit) and caches it; the batch
gets one locator in [commit_executor.go](../../internal/git/commit_executor.go).
Building the inventory once from the pre-batch state is also the semantically correct
unit per decision 1 of
[manifestedit-new-file-placement-spike.md](manifestedit-new-file-placement-spike.md)
(location is valid for the checked-out commit).

On top of that, `locate` takes a **stat fast-path**: the operator writes each
resource to its canonical path, so if a file already exists there, the resource
lives there and no scan happens at all. The inventory scan only fires for a resource
whose canonical file is absent — a genuinely new resource, or one a user moved off
the canonical path (the only case match-first actually needs). In steady state every
resource is at its canonical path, so the amortized cost is ~one `stat` per event,
i.e. back to the pre-match-first baseline. This was needed because the per-batch
cache alone still re-scanned the (growing) base path on every reconcile, which under
full-suite load was enough for the cluster-wide CRD watch to miss the commit
deadline even after the O(N²)→O(N) fix.

Remaining (smaller) optimization: a cache that lives **longer than one batch**,
governed by the rebuild-on-change gate in
[manifest-inventory-file-agnostic-placement.md](manifest-inventory-file-agnostic-placement.md),
so back-to-back batches on a warm worktree don't each re-scan. Not required for
correctness.

Note: the scan is rooted at the GitTarget `spec.path`, so a non-empty path keeps
`.git` out of the walk. A target with an empty path walks the worktree root
(including `.git`, which holds no manifests) — another reason the longer-lived cache
is worthwhile.

## Follow-up 3 — cleanup of duplicate ("double") entries

**Yes — this is part of the same theme.** Match-first reduces how often we *create*
duplicates, but it does not yet *remove* duplicates that already exist, and the
incomplete delete in Follow-up 1 can still leave a stale copy behind. Those are
"double entries": the same resource identity present in Git at more than one path.

What exists:

- The inventory already detects duplicates with first-occurrence-wins
  (lexicographically first path is authoritative; later copies are losers) —
  [internal/git/manifestedit/index.go](../../internal/git/manifestedit/index.go).
- `BuildReport` already surfaces every duplicate loser as an `ActionDelete`
  "prune candidate", and every Git-only resource the cluster lacks as a prune
  candidate too — [internal/manifestreport/report.go](../../internal/manifestreport/report.go).

What is missing: this is **report-only**. Nothing in the writer acts on those
prune candidates, by design — the prune hazard (a partial cluster view turning
into spurious deletions) is deliberately deferred across the placement docs. So a
duplicate, once present, is reported but never cleaned up.

The follow-up is to wire duplicate/orphan pruning from the read-only report into
the writer, behind whatever safety gate the prune-hazard decision lands on
(e.g. only prune duplicate *losers*, which is safe because the authoritative copy
is kept, before touching cluster-absent orphans). Doing duplicate-loser cleanup
first is attractive: it is the half with no prune hazard (the resource still
exists in exactly one place afterwards) and it directly cancels any double entry
that Follow-up 1's fallback delete might leave.

## Suggested order

1. Follow-up 1 (identity on deletes) — closes the last placement correctness gap.
2. Follow-up 3, duplicate-loser pruning only — safe, cancels leftover double
   entries, no prune-hazard exposure.
3. Orphan pruning (cluster-absent resources) — only after the prune-hazard gate is
   designed.

(Follow-up 2, the per-batch inventory cache, is already done — see above. A
longer-lived cache across batches remains an optional optimization.)
