# Index: what to read, and what is history

This page names the documents that bind the current implementation. If a document is not
listed here, it is either a user guide (see [`README.md`](README.md)) or historical context
you can safely skip.

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
| [`gittarget-new-file-placement-rules.md`](spec/gittarget-new-file-placement-rules.md) | where a new resource's file goes: declared, the folder's one kustomize root, canonical. Sibling inference is removed, and kept as history |
| [`sops-single-file-no-multidoc.md`](spec/sops-single-file-no-multidoc.md) | one encrypted file is one document |
| [`scale-subresource-audit-rehydration.md`](spec/scale-subresource-audit-rehydration.md) | `/scale` only; every other subresource ignored |
| [`commit-window-refactor.md`](spec/commit-window-refactor.md) | one grouped commit = one (author, GitTarget) |
| [`gittarget-isolation-on-rule-change.md`](spec/gittarget-isolation-on-rule-change.md) | a rule change on target A never touches target B |
| [`audit-readiness-probe-plan.md`](spec/audit-readiness-probe-plan.md) | liveness must never depend on Redis |
| [`type-followability.md`](spec/type-followability.md) | is a type followable, and if not, the one reason |
| [`gitpath-foreign-content-stringency.md`](spec/gitpath-foreign-content-stringency.md) | refusing a path that shadows foreign content |
| [`unsupported-folder-refusal-plan.md`](spec/unsupported-folder-refusal-plan.md) | `GitPathAccepted`, and refusing what we cannot own |
| [`commitrequest-design.md`](spec/commitrequest-design.md) | the CommitRequest window and its conditions |
| [`commitrequest-admission-authorship.md`](spec/commitrequest-admission-authorship.md) | how command submitters are captured and matched to commit windows |
| [`where-validation-lives.md`](spec/where-validation-lives.md) | schema → CEL → **the reconciler**; a webhook only for what exists solely at admission |
| [`e2e-serial-registry.md`](spec/e2e-serial-registry.md) | which e2e specs must run Serial, and why |

## What is being decided now — [`design/`](design/)

**The live workstream** is [`design/support-boundary/`](design/support-boundary/) — editing
existing GitOps folders through the Kubernetes API. Start at
[`support-contract.md`](design/support-boundary/support-contract.md) — **the single page that
says what we support and refuse** — and then its
[README](design/support-boundary/README.md), which maps the rest of the folder: the
kustomize field taxonomy, the write boundary, the orchestrator/expansion line, and
how secrets are handled.

Fifteen other open items:

| Doc | Open question |
|---|---|
| [`open-asks-priority.md`](design/open-asks-priority.md) | **the work queue.** Reconciled 2026-07-29 to what the attribution branch shipped: the fact stream, consumer ask #23, the name tier and metrics Phase 1 are struck from it and recorded in "already shipped", the residue they leave (the removal-wait decision, the head-of-line block on the shard, the aggregated create) is ranked, and #5 loses one of its two arguments because #23's fix retired it. Three backlogs are open at once — the gitops-api consumer asks, the maintainer review's unbuilt block (F6, F9, F10), and the config-surface proposal (B1–B6) — and they overlap. Merges them into one ordered queue under four stated tests, and makes one design call against what was asked: **delete Option C sibling inference** rather than ship an off-switch for it, because it lets a human's edit to the repository change the operator's behaviour with nothing in status recording the move, its central guard has already failed once by cascading, and the explainability its own spec made mandatory was never built. That answers the namespace-leak ask by removal, and means `spec.placement.mode` is never built. **The deletion has shipped**, together with the placement metrics the argument had said to lead *away* from — an objection to their labels, which naming the GitTarget and the type retires — and "what the deletion taught" records the two things building it found: namespace inheritance was a second implementation of a rule that belonged to the governing kustomization, and it was missing the check that the transformer names the resource's own namespace. Open: whether a fall-back to canonical also raises an Event on the GitTarget, and `status.layout` |
| [`placement-visibility-and-declared-defaults.md`](design/placement-visibility-and-declared-defaults.md) | the three questions the inference deletion left, decided. **Keep `canonical`** as the name for the built-in path and split `declared` into `byType`/`default`, because reusing "default" for both a declaration and the absence of one makes the metric unreadable. **No CRD default for `placement.default`**, and the reason is structural rather than the two that look obvious: a defaulted default is never empty, so it shadows the kustomize-root step and every new file in an overlay would take the canonical path, in Git and rendered by nothing. The validation failure is real but the rule doing the rejecting is itself wrong, and the persistence objection is a trade we could take; defaulting the Secret route to work around the first is a floor that vanishes when a user writes any `byType` entry, because map defaults never merge. **`status.layout` instead**, with five worked examples (greenfield, kustomize overlay, brownfield missing one rule, two ambiguous roots, a refusal from an operator-configured sensitive type) over the `MarkTargetRetention` seam, which already enqueues on change and so retires the "the data plane cannot notify the GitTarget" objection. **`{kindLower}`, not a `toLower` function.** Carries three findings that changed a decision: `IdentityCompletePlacementTemplate` demanding `{version}` contradicts the versionless-path decision; two supported kustomizations still produce a file nothing renders and nothing counts; and **a declared path into a subdirectory of a kustomize folder is registered only when render-root scoping happens to be in force**, so one `byType` line reproduces the unrendered-file bug today. Fixing that last one (walk up to the nearest kustomization) also weakens the case against the CRD default from a correctness wall to a legibility trade, which the page says rather than leaving the stronger argument standing |
| [`gittarget-layout-model.md`](design/gittarget-layout-model.md) | **the proposal the placement questions were circling around**: a path template is the wrong primitive, so declare what the folder IS. `spec.layout.kind` with the values `Auto`, `Kustomize`, `Tree`, `Flat` and `Template`, plus `byType` overrides valid under every kind, with two rules that carry the value: whatever chose the path, the file is registered with the kustomization that governs it (so F10 becomes unstatable rather than fixed), and a structural kind excludes a blanket `default` (so a declared template can no longer silently disable the render root). `kind: Auto` is a safe CRD default because it NAMES the structural rule instead of standing in front of it, which is why defaulting a mode works where defaulting a path did not, and it is declared inference rather than the undeclared kind that was deleted. `kind: Kustomize` with `create: true` bootstraps an empty repository into a folder `kubectl apply -k` can build. Seven worked examples, a status shape with `declaredKind` beside the resolved `kind`, a mechanical migration for every current configuration, and an argument that the layout should NOT be its own CRD: a shared object changing where N folders write, with nothing on the GitTarget recording it, is the same defect as sibling inference with a different actor, the shared thing is four lines, and generators already solve reuse. Open: whether `Auto` should be the default at all, and whether `Flat` refuses a multi-namespace target or warns |
| [`gittarget-api-wave.md`](design/gittarget-api-wave.md) | **one breaking wave on GitTarget**, sequencing the layout model with the maintainer review's still-open API block (F6, F10, F12's reference nit, §3's pushbacks) and the queue's Tier 2 items (B4, B1, #5, #6). The batching argument is the weaker half; the stronger one is that four of them are the same decision seen from different angles: **the folder is described on the GitTarget and the connection describes only the connection**, which is why `commitWindow` and `commit.message` move off `GitProvider`. Two findings change the layout design rather than accompanying it: `spec.mode: Observe` becomes how a layout is adopted safely (a dry run over `status.layout` instead of declare-and-hope), and `spec.interval` plus an observation pass is what keeps the scan-derived half of that status fresh for a target that writes nothing. `spec.suspend` is a precondition rather than a rider, because a layout that creates a `kustomization.yaml` needs a stop button. Records that F7 already shipped the EventRecorder the placement Event was said to be too expensive for, that layout is mutable like `prune`, that F9 stays OUTSIDE the wave because its answer constrains the enum work, and that the version stays `v1alpha3` with loud rejections rather than paying for a conversion path |
| [`docs-linting.md`](design/docs-linting.md) | how to mechanize [`style-guide.md`](style-guide.md) with markdownlint-cli2 and Vale. Both are wired into `task lint`, gated on the files [`.docs-lint-scope`](../.docs-lint-scope) lists rather than the whole tree: 102 of 174 files fail markdownlint and 148 of 174 fail Vale, so the two backlogs need different gates. Open: how the scope list grows to cover the tree, the `MD013` limit, and whether `AGENTS.md` and the chart READMEs are in scope |
| [`attribution-deletion-intent-actor.md`](design/attribution-deletion-intent-actor.md) | a finalized deletion used to be attributed to the controller that cleared the finalizer rather than to the human who asked for it. Reproduced from a two-actor mutation-lab capture (`configmap/deletion-intent-actor`, with a tunable hold between the phases) and pinned by a corpus-driven unit test: the human's `delete` and the controller's finalizer `patch` both return a body carrying the resourceVersion the DELETION stamped, so both facts are filed under the same `(uid, resourceVersion)` key and the index is last-writer-wins — the deleter's fact is not outranked, it is replaced. **Built**: a STICKY removal pointer — a fact about a deletion may not be overwritten by a fact about a write — keyed strictly by uid, consulted ahead of the exact tier for a removal, and bounded by the index's caps rather than the join TTL, because a uid is unique across space and time — in memory, so a restart still re-warms from one TTL of stream retention. It ships the `delete_sticky` value on `attribution_resolutions_total{tier}` and collapses the full-grace wait a `Terminating` object seen on replay used to pay. Records why the cheaper alternatives are second: filing delete facts under the name tier answers at a tier that misstates the evidence, and making the exact entry write-once makes correctness depend on fact arrival order and does nothing for replay. Open: nothing |
| [`attribution-removal-wait-options.md`](design/attribution-removal-wait-options.md) | a removal now waits for evidence about the DELETION rather than accepting the object's last write, which stopped it naming whoever last edited the object as the author of a deletion they did not perform. Enumerates the eight situations a resolution can be in and shows the cost is concentrated in exactly one: a removal for which no delete fact will ever arrive (a graceful pod delete, a status-only removal, a type the audit policy skips) spends the whole grace to return the answer it had at t=0, measured at ~3.1s against ~70ms when evidence is present. Prices five options against that, and recommends a per-route watermark — stop waiting once the fact stream has demonstrably moved past this event — over a second timeout flag whose right value lives in the API server's config rather than ours. Open: the decision, and how common the case is outside the e2e suite |
| [`attribution-metrics-proposal.md`](design/attribution-metrics-proposal.md) | a phased attribution metric surface, revised after review cut an earlier draft of thirteen new families down to a first release that covers health and the unseen loss paths. Splits `result` into `tier` and `actor_kind` (how `commits_total` already models it) and `weak` into `latest` and `resource_version`, taking the break in the release that has broken `result` anyway. Adds watch-queue delay, follower error and last-success health, a `no_attribution_fact` outcome on the existing bounded audit vocabulary, and a decode-error counter for the one loss path with no symptom at all: both transports discard an undecodable stream entry and advance past it with no log and no metric. Records what the first draft got wrong and why, including a proposed series that would have been permanently zero and a gauge that would have counted registrations rather than blocked resolvers. Its Phase 1 is now Phase 1 of [`metrics-observability-plan.md`](design/metrics-observability-plan.md), which absorbed the surface and records the drift it had accumulated; this stays as the reasoning trail. **Phase 1 has shipped** — the migration is in [`UPGRADING.md`](UPGRADING.md). Open: nothing structural |
| [`attribution-publish-and-join.md`](design/attribution-publish-and-join.md) | the reference for what attribution's two halves each do, exactly: the publish side that turns one audit event into zero or one fact and files it under the keys it happens to have, and the join side that walks the tiers strongest-first to name an author for a watch event. A flowchart per half, the tier table, and the two rules that are easy to miss (a removal never answers with a write fact without looking further; an exact-capable event may never fall through to the removal tiers). Also answers whether anything special-cases a type: nothing does, every branch is on the verb or on which fields are present, and the two ConfigMap deletes in the corpus — one answered with the object, one with a `Status` — are the standing argument that a type-based rule would be unsound |
| [`attribution-branch-findings.md`](design/attribution-branch-findings.md) | what the attribution switchover's loose ends turned out to be, measured rather than reasoned. The mutation lab was serving `/audit-webhook` as an exact path while the cluster posts to the named `/audit-webhook/default`, so every audit event 404'd and every audit-carrying scenario timed out — a routing mismatch that reads exactly like a broken cluster. With it fixed, the corpus answers the aggregated-API removal question: a proxied delete is audited with a name but **no uid and no resourceVersion**, so the exact and latest tiers can never match it, and a proxied `deletecollection` returns **no response body**, so its fact carries no uid set and the join must fall back to scope. Separates the missing name from the missing body — a `generateName` create recovers both from the response object, an aggregated write has nothing to recover from — and prices a name tier against accepting that aggregated types are collection-only. Open: the tier decision, and whether a CommitRequest missing the window of the write it follows by two seconds is new on this branch |
| [`attribution-fact-identity.md`](design/attribution-fact-identity.md) | several `ClusterProvider`s may name one physical cluster, but a kube-apiserver posts audit to one route, so only one of those names is ever fed and every other one authors `unknown (attribution unresolved)`. Proposes a declared `spec.attribution.auditRoute` that partitions the facts instead of `metadata.name`, so several providers can share one cluster's facts while cloned clusters stay separate, ingestion loses its last Kubernetes read, and a misrouted provider becomes loud. Renames the key infix and the annotation-key flag to the same word |
| [`attribution-wait-poll-vs-push.md`](design/attribution-wait-poll-vs-push.md) | **superseded by the above, kept as the reasoning trail.** a watch event needs its author before it can be routed, and the audit fact naming that author may not have arrived yet, so `ResolveAuthor` polls Redis every 150ms for up to three seconds on the watch shard's own goroutine. Separates the wait (forced: two unordered deliveries out of one kube-apiserver, and the commit window groups by author) from the poll (a choice). Answers which of the two fires first: the watch, nearly always, because audit delivery is batched by the apiserver while the watch is streamed, so the first lookup is a near-guaranteed miss and the loop runs to completion on every attributable event. Six options priced against that, from shifting the first check to the delivery floor and a circuit breaker for an audit route that has never resolved anything, through a Redis publish and subscribe, to the reassembly-buffer design that stops blocking the watch shard. Open: whether the wait population is dominated by resolved-late (favors publish and subscribe) or never-resolved (favors the buffer), plus a proposed per-scenario timing report from the mutation-capture lab |
| [`watch-and-catalog-architecture.md`](design/watch-and-catalog-architecture.md) | the target three-layer watch model — **needs a human call before building** |
| [`metrics-observability-plan.md`](design/metrics-observability-plan.md) | the canonical metrics plan, reconciled to the code after the fact-stream switchover and now carrying the attribution surface from [`attribution-metrics-proposal.md`](design/attribution-metrics-proposal.md). Reads the product as one pipeline — watch events arrive, and are processed into commits — and maps a metric to each stage. The attribution join is built and correctly labelled; **watch ingestion, shard queue delay, and the relevance filter are still dark**. **Phase 1 — the attribution relabel plus the loss-path counters — has shipped**; Phase 2 is the watch stage, Phase 3 the filter and push health, Phase 4 the dashboard and alerts. Open: Phases 2-4, and the dashboard JSON is deliberately not written until the watch families exist |
| [`reconcile-triggering.md`](design/reconcile-triggering.md) | which controllers still fail to wake up |
| [`multi-source-audit-ingress-hardening.md`](design/multi-source-audit-ingress-hardening.md) | how independent sources authenticate to a named audit route, when annotation routing is trustworthy, and how multi-provider ingestion remains fair |
| [`release-image-reuse-plan.md`](design/release-image-reuse-plan.md) | PRs 2–5 unstarted |
| [`e2e-coverage-gaps-and-improvements-plan.md`](design/e2e-coverage-gaps-and-improvements-plan.md) | tests A/B/C still proposals |
| [`e2e-finish-plan.md`](design/e2e-finish-plan.md) | remaining e2e harness work |
| [`sensitive-resource-diagnostics-follow-up.md`](design/sensitive-resource-diagnostics-follow-up.md) | deferred diagnostics |
| [`e2e-git-server-choice.md`](design/e2e-git-server-choice.md) | stay on Gitea or move to Forgejo — the `_csrf` pin is fixable in place on both, so the migration is now a preference call, not a fix; also why we adopt no SDK either way |
| [`watchrule-source-namespace/`](design/watchrule-source-namespace/README.md) | letting a WatchRule address differently-named namespaces on its source cluster — a deny-by-default `allowedSourceNamespaces` on the **GitTarget** (so scope is per-tenant, not a provider-wide union), unlocked by a false-by-default delegation flag on the ClusterProvider. Five PRs: three landed prerequisite scope fixes (the namespace-blind resync sweep that would delete other namespaces' manifests, the cluster-wide/named stream collapse, and ClusterWatchRule's unchecked GitTarget attachment), then the breaking **scope-by-kind** change — `WatchRule.spec.rules[].sourceNamespace` (a name or `"*"` for the target's admitted set) and a cluster-scope-only ClusterWatchRule — and a GitTarget `prune.mode` that makes the resync sweep opt-in, released together with it |

## Deferred, but still wanted — [`future/`](future/)

[`idea-application-editing.md`](future/idea-application-editing.md) is where the
whole edit-through-the-API workstream started, and still holds the branch/session
grouping strategies nothing else covers.
[`ha-gittarget-distribution-plan.md`](future/ha-gittarget-distribution-plan.md) is
the HA plan `architecture.md` cites three times (and the reason Redis is required).
[`least-privilege-remaining-work.md`](future/least-privilege-remaining-work.md) has
three open RBAC items.
[`config-surface-for-a-structured-repository.md`](future/config-surface-for-a-structured-repository.md)
reviews the configuration docs and argues the API never caught up with what the
folder analysis learned — a look-before-you-write mode, a `status.layout`
projection, an inference switch, and moving `commitWindow` onto the GitTarget.
Five more ideas sit beside them.

## History — [`finished/`](finished/)

Twenty-two shipped plans and closed investigations. **Nothing here binds.** Read one
only when you want to know *why* something is the way it is; the answer to *what it
is* always lives in `spec/`.

The newest is
[`attribution-fact-stream.md`](finished/attribution-fact-stream.md): why attribution facts stopped
being a keyspace the watch side polls and became a per-type log it follows into a bounded in-memory
index. It deleted the `SET`/`GET` fact keys, the 150ms poll loop, and the `deletecollection`
expander, replacing the expander with one collection fact that every removal in its scope joins by
uid membership or by scope. That last part is a capability gain rather than a like-for-like swap: a
collection delete the API server sent no response body for used to lose its author entirely.
`exact_deletecollection_item` is replaced by `deletecollection_body_uid` and `deletecollection_scope`, and
`--author-attribution-transport=memory` runs attribution with no Redis on a single replica.
Shipped as #283, #284, #286 and #287.

Before it,
[`analyzer-consumer-contract-asks.md`](finished/analyzer-consumer-contract-asks.md): why a
refusal carries whether it can be solved and by whom, why the analyzer's report is a KRM
document that names the build that produced it, and why `ResourceIdentifier.Key()` is a
cross-product contract with a golden test. Shipped as #273 and #275.

Note that most of the pre-2026-07 audit-pipeline archaeology has been deleted
outright: the watch-first rewrite removed `internal/gate`, the audit joiner, and
the audit-as-state pipeline, so ~30 documents describing them were prose about
code that no longer exists. `git log` has them.
