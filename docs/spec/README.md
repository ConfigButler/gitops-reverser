# `docs/spec/` — this is true now, and the code depends on it

Three folders, three meanings. This is the one that binds.

| Folder | Means |
|---|---|
| [`../design/`](../design/) | **we are still deciding.** Open questions, proposals, unbuilt work. |
| **`spec/`** (here) | **this is true now.** The code implements it and cites it. Change the code, change the doc. |
| [`../finished/`](../finished/) | **this happened.** Shipped plans and closed investigations, kept for `git log`-grade context. Nothing here binds. |

Everything in this folder states **current behaviour that the code relies on**, and
most of it is cited by path from Go source — that citation is how the next person
finds out you changed a contract. The handful that are not Go-cited
(`gvk-gvr-mapping-layer`, `sops-single-file-no-multidoc`, `e2e-test-design`) are
here because they are the only written record of a rule the code still obeys.

If you change one of these behaviours, change the document in the same commit.

## Start here

- [`manifest-system.md`](manifest-system.md) — **how the whole live → Git pipeline
  works.** Read this first; it summarises everything else in this folder.

## The contracts

| Spec | What it pins |
|---|---|
| [`current-manifest-support-review.md`](current-manifest-support-review.md) | the manifest store, plan/apply/flush, and the all-or-nothing folder claim |
| [`contextual-namespace-and-kustomize-folder-editing.md`](contextual-namespace-and-kustomize-folder-editing.md) | kustomize graph-aware namespace inference; the supported subset |
| [`reconcile-via-watchlist-mark-and-sweep.md`](reconcile-via-watchlist-mark-and-sweep.md) | initial reconcile; **no bookmark, no sweep** |
| [`gittarget-new-file-placement-rules.md`](gittarget-new-file-placement-rules.md) | where a brand-new resource's file goes |
| [`manifestedit-field-ownership-spike.md`](manifestedit-field-ownership-spike.md) | "the API wins" — full-object ownership, and the do-not-build list |
| [`type-followability.md`](type-followability.md) | is a type followable, and if not, the single reason |
| [`type-lifecycle-events-and-wobble-settling.md`](type-lifecycle-events-and-wobble-settling.md) | removal grace and flap coalescing |
| [`gvk-gvr-mapping-layer.md`](gvk-gvr-mapping-layer.md) | the GVK↔GVR bijection contract |
| [`where-validation-lives.md`](where-validation-lives.md) | schema → CEL → **the reconciler**; a webhook only for what exists solely at admission |
| [`sops-single-file-no-multidoc.md`](sops-single-file-no-multidoc.md) | one encrypted file is one document |
| [`scale-subresource-audit-rehydration.md`](scale-subresource-audit-rehydration.md) | `/scale` → bounded field patch; every other subresource ignored |
| [`commit-window-refactor.md`](commit-window-refactor.md) | one grouped commit = one (author, GitTarget) |
| [`commitrequest-design.md`](commitrequest-design.md) | how a request binds to a same-actor commit window and reports its outcome |
| [`gittarget-isolation-on-rule-change.md`](gittarget-isolation-on-rule-change.md) | a rule change on target A never touches target B |
| [`audit-readiness-probe-plan.md`](audit-readiness-probe-plan.md) | why liveness must never depend on Redis |
