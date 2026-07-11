# `docs/spec/` — this is true now, and the code depends on it

Three folders, three meanings. This is the one that binds.

| Folder | Means |
|---|---|
| [`../design/`](../design/) | **we are still deciding.** Open questions, proposals, unbuilt work. |
| **`spec/`** (here) | **this is true now.** The code implements it and cites it. Change the code, change the doc. |
| [`../finished/`](../finished/) | **this happened.** Shipped plans and closed investigations, kept for `git log`-grade context. Nothing here binds. |

Everything in this folder is cited from Go source. If you change one of these
contracts, the citation is how the next person finds out.

## Start here

- [`manifest-system.md`](manifest-system.md) — **how the whole live → Git pipeline
  works.** Read this first; it summarises everything else in this folder.

## The contracts

| Spec | What it pins |
|---|---|
| [`current-manifest-support-review.md`](current-manifest-support-review.md) | the manifest store, plan/apply/flush, and the all-or-nothing folder claim |
| [`contextual-namespace-and-kustomize-folder-editing.md`](contextual-namespace-and-kustomize-folder-editing.md) | kustomize graph-aware namespace inference; the supported subset |
| [`reconcile-via-watchlist-mark-and-sweep.md`](reconcile-via-watchlist-mark-and-sweep.md) | initial reconcile; **no bookmark, no sweep** |
| [`gittarget-new-file-placement-rules.md`](gittarget-new-file-placement-rules.md) | where a brand-new resource's file goes (F4) |
| [`manifestedit-field-ownership-spike.md`](manifestedit-field-ownership-spike.md) | "the API wins" — full-object ownership, and the do-not-build list |
| [`type-followability.md`](type-followability.md) | is a type followable, and if not, the single reason |
| [`type-lifecycle-events-and-wobble-settling.md`](type-lifecycle-events-and-wobble-settling.md) | removal grace and flap coalescing |
| [`gvk-gvr-mapping-layer.md`](gvk-gvr-mapping-layer.md) | the GVK↔GVR bijection contract |
| [`sops-single-file-no-multidoc.md`](sops-single-file-no-multidoc.md) | one encrypted file is one document |
| [`scale-subresource-audit-rehydration.md`](scale-subresource-audit-rehydration.md) | `/scale` → bounded field patch; every other subresource ignored |
| [`commit-window-refactor.md`](commit-window-refactor.md) | one grouped commit = one (author, GitTarget) |
| [`commitrequest-multi-finalize-design.md`](commitrequest-multi-finalize-design.md) | why `MaxConcurrentReconciles=1` on the CommitRequest controller |
| [`gittarget-isolation-on-rule-change.md`](gittarget-isolation-on-rule-change.md) | a rule change on target A never touches target B |
| [`audit-readiness-probe-plan.md`](audit-readiness-probe-plan.md) | why liveness must never depend on Redis |
