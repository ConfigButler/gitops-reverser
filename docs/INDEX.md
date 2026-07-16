# Index: what to read, and what is history

> Rebuilt 2026-07-11, when the tree went from 180 documents to 117.

There are 117 markdown files here. **About 35 of them bind.** This page names
those. If a document is not on this page, it is either a user guide (see
[`README.md`](README.md)) or history you can safely not read.

## The four folders, and what each one means

| Folder | Means | Binds? |
|---|---|---|
| [`spec/`](spec/) | **This is true now, and the code depends on it.** Most are cited by path from Go source. Change the behaviour, change the doc. | **yes** |
| [`design/`](design/) | **We are still deciding.** Open questions, proposals, unbuilt work. | yes — as intent, not as shipped behaviour |
| [`facts/`](facts/) | Durable reference: how Kubernetes behaves, and what we discovered about it. | yes, as reference |
| [`finished/`](finished/) | **This happened.** Shipped plans, closed investigations. Kept for context. | **no** |

The rule that was missing before: `design/` used to hold shipped work and
`finished/` used to hold live contracts. If you are adding a document, pick the
folder by **lifecycle**, not by topic.

## If you are new: read these five

1. [`../README.md`](../README.md) — what the operator does.
2. [`architecture.md`](architecture.md) — how the operator is put together.
3. [`spec/manifest-system.md`](spec/manifest-system.md) — **how a live object
   becomes a line in a Git file.** The single best explanation of the core.
4. [`spec/current-manifest-support-review.md`](spec/current-manifest-support-review.md)
   — the manifest store's contract, and the rules that must never break.
5. [`design/support-boundary/support-contract.md`](design/support-boundary/support-contract.md)
   — **what the operator edits, what it refuses, and why.**

## The contracts — [`spec/`](spec/)

The code cites these. Breaking one without updating it is how the next person gets
misled. Full list in [`spec/README.md`](spec/README.md); the ones that carry a
*rule* rather than a description:

| Spec | The rule |
|---|---|
| [`manifest-system.md`](spec/manifest-system.md) | the whole live → Git pipeline, and every invariant below in summary |
| [`current-manifest-support-review.md`](spec/current-manifest-support-review.md) | all-or-nothing folder claim; never half-write a multi-doc file; **refuse rather than prune** |
| [`manifestedit-field-ownership-spike.md`](spec/manifestedit-field-ownership-spike.md) | the API wins — full-object ownership, never field-subset |
| [`reconcile-via-watchlist-mark-and-sweep.md`](spec/reconcile-via-watchlist-mark-and-sweep.md) | **no bookmark, no sweep** |
| [`contextual-namespace-and-kustomize-folder-editing.md`](spec/contextual-namespace-and-kustomize-folder-editing.md) | kustomize namespace inference; the supported subset |
| [`gittarget-new-file-placement-rules.md`](spec/gittarget-new-file-placement-rules.md) | where a new resource's file goes |
| [`sops-single-file-no-multidoc.md`](spec/sops-single-file-no-multidoc.md) | one encrypted file is one document |
| [`scale-subresource-audit-rehydration.md`](spec/scale-subresource-audit-rehydration.md) | `/scale` only; every other subresource ignored |
| [`commit-window-refactor.md`](spec/commit-window-refactor.md) | one grouped commit = one (author, GitTarget) |
| [`gittarget-isolation-on-rule-change.md`](spec/gittarget-isolation-on-rule-change.md) | a rule change on target A never touches target B |
| [`audit-readiness-probe-plan.md`](spec/audit-readiness-probe-plan.md) | liveness must never depend on Redis |
| [`type-followability.md`](spec/type-followability.md) | is a type followable, and if not, the one reason |
| [`gitpath-foreign-content-stringency.md`](spec/gitpath-foreign-content-stringency.md) | refusing a path that shadows foreign content |
| [`unsupported-folder-refusal-plan.md`](spec/unsupported-folder-refusal-plan.md) | `GitPathAccepted`, and refusing what we cannot own |
| [`commitrequest-design.md`](spec/commitrequest-design.md) | the CommitRequest window and its conditions |
| [`commitrequest-admission-authorship.md`](spec/commitrequest-admission-authorship.md) | how a real Kubernetes user becomes a commit author |
| [`e2e-serial-registry.md`](spec/e2e-serial-registry.md) | which e2e specs must run Serial, and why |

## What is being decided now — [`design/`](design/)

**The live workstream** is [`design/support-boundary/`](design/support-boundary/) — editing
existing GitOps folders through the Kubernetes API. Start at
[`support-contract.md`](design/support-boundary/support-contract.md) — **the single page that
says what we support and refuse** — and then its
[README](design/support-boundary/README.md), which maps the rest of the folder: the
kustomize field taxonomy, the write boundary, the orchestrator/expansion line, and
how secrets are handled.

Ten other open items:

| Doc | Open question |
|---|---|
| [`config-plane-split.md`](design/config-plane-split.md) | remote-cluster mirroring via an inline `GitTarget.spec.sourceCluster` — **redesign of #220's #1, awaiting build** |
| [`watch-and-catalog-architecture.md`](design/watch-and-catalog-architecture.md) | the target three-layer watch model — **needs a human call before building** |
| [`metrics-observability-plan.md`](design/metrics-observability-plan.md) | the watch-stage metrics do not exist yet |
| [`reconcile-triggering.md`](design/reconcile-triggering.md) | which controllers still fail to wake up |
| [`multi-cluster-audit-ingestion-implications.md`](design/multi-cluster-audit-ingestion-implications.md) | §5's `SourceCluster` CRD is **superseded** by [`config-plane-split.md`](design/config-plane-split.md); the rest (per-cluster audit ingestion) is still open |
| [`release-image-reuse-plan.md`](design/release-image-reuse-plan.md) | PRs 2–5 unstarted |
| [`e2e-coverage-gaps-and-improvements-plan.md`](design/e2e-coverage-gaps-and-improvements-plan.md) | tests A/B/C still proposals |
| [`e2e-finish-plan.md`](design/e2e-finish-plan.md) | remaining e2e harness work |
| [`residual-e2e-flakes-2026-06-19.md`](design/residual-e2e-flakes-2026-06-19.md) | Flake B still open |
| [`sensitive-resource-diagnostics-follow-up.md`](design/sensitive-resource-diagnostics-follow-up.md) | deferred diagnostics |

## Deferred, but still wanted — [`future/`](future/)

[`idea-application-editing.md`](future/idea-application-editing.md) is where the
whole edit-through-the-API workstream started, and still holds the branch/session
grouping strategies nothing else covers.
[`ha-gittarget-distribution-plan.md`](future/ha-gittarget-distribution-plan.md) is
the HA plan `architecture.md` cites three times (and the reason Redis is required).
[`least-privilege-remaining-work.md`](future/least-privilege-remaining-work.md) has
three open RBAC items. Five more ideas sit beside them.

## History — [`finished/`](finished/)

Eighteen shipped plans and closed investigations. **Nothing here binds.** Read one
only when you want to know *why* something is the way it is; the answer to *what it
is* always lives in `spec/`.

Note that most of the pre-2026-07 audit-pipeline archaeology has been deleted
outright: the watch-first rewrite removed `internal/gate`, the audit joiner, and
the audit-as-state pipeline, so ~30 documents describing them were prose about
code that no longer exists. `git log` has them.
