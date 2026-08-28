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

One exception, and it is about binding rather than about lifecycle. A `design/`
page that has **shipped** stays in `design/` when Go source cites it by path as the
rationale for what the code does. `finished/` means "binds? **no**", and such a page
still binds — moving it would leave a dozen file comments pointing at a folder that
declares itself non-binding. It is labelled **built.** in the table below instead, so
the lifecycle is still readable. `spec/` is for a contract stated as a contract;
this is a plan whose reasoning the code kept.

## If you are new: read these five

1. [`../README.md`](../README.md) — what the operator does.
2. [`architecture.md`](architecture.md) — how the operator is put together.
3. [`spec/manifest-system.md`](spec/manifest-system.md) — **how a live object
   becomes a line in a Git file.** The single best explanation of the core.
4. [`spec/current-manifest-support-review.md`](spec/current-manifest-support-review.md)
   — the manifest store's contract, and the rules that must never break.
5. [`design/support-boundary/support-contract.md`](design/support-boundary/support-contract.md)
   — **what the operator edits, what it refuses, and why.**

Beside `architecture.md`: [`components.md`](components.md) is the component map. It names every
package by plane, and answers which components observe a Kubernetes API server (five do, and only
two of them observe types).

Provider-specific setup that needed writing down:
[`azure-devops-getting-started.md`](azure-devops-getting-started.md) — Azure DevOps end to end, why its
credential Secret is shaped differently from every other provider's, and the three test layers that
cover a provider CI cannot reach.

## The contracts — [`spec/`](spec/)

The code cites these. Breaking one without updating it is how the next person gets
misled. Full list in [`spec/README.md`](spec/README.md); the ones that carry a
*rule* rather than a description:

| Spec | The rule |
|---|---|
| [`manifest-system.md`](spec/manifest-system.md) | the whole live → Git pipeline, and every invariant below in summary |
| [`attribution.md`](spec/attribution.md) | **how a commit gets its author.** Deletion is attributed at intent time; the publish half files a fact under the strongest key it has, the join half walks the tiers strongest-first, and neither branches on the type. The single reference, folded from six design records |
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

[`gittarget-configuration-freshness.md`](design/gittarget-configuration-freshness.md) is
**deferred, kept as a decision record** rather than an active proposal. It works out what a
target-level desired/applied configuration identity would have to be, and why it is not worth
building yet: as sequenced, both markers move together inside one pass, so the only mismatch they
can show is a failing pass that existing status already reports, and everything the page is for
lives in the window where nothing has been published. The one part that stood on its own has
shipped — the e2e rule barrier now requires `observedGeneration == generation` before it accepts
`StreamsRunning=True`. The page carries the trigger conditions for picking the rest up.

**The live workstream** is [`design/support-boundary/`](design/support-boundary/) — editing
existing GitOps folders through the Kubernetes API. Start at
[`support-contract.md`](design/support-boundary/support-contract.md) — **the single page that
says what we support and refuse** — and then its
[README](design/support-boundary/README.md), which maps the rest of the folder: the
kustomize field taxonomy, the write boundary, the orchestrator/expansion line, and
how secrets are handled.

Eighteen other open items:

| Doc | Open question |
|---|---|
| [`open-asks-priority.md`](design/open-asks-priority.md) | **the work queue.** Swept 2026-07-30 against the 0.41.0 release, which carries the attribution model and the placement break and deliberately **not** the GitTarget wave: every Tier 2 entry that changes a `GitTarget` field is part of postponed [#294](https://github.com/ConfigButler/gitops-reverser/issues/294) and is not independently schedulable, while the Tier 1 entries are not and must not wait for it. One entry moved up in the sweep, to Tier 1: a declared path into a kustomize subdirectory produces a file that is in Git and rendered by nothing, which is the product being silently wrong. Reconciled 2026-07-29 to what the attribution branch shipped: the fact stream, consumer ask #23, the name tier and metrics Phase 1 are struck from it and recorded in "already shipped", the residue they leave (the removal-wait decision, the head-of-line block on the shard, the aggregated create) is ranked, and #5 loses one of its two arguments because #23's fix retired it. Three backlogs are open at once — the gitops-api consumer asks, the maintainer review's unbuilt block (F6, F9, F10), and the config-surface proposal (B1–B6) — and they overlap. Merges them into one ordered queue under four stated tests, and makes one design call against what was asked: **delete Option C sibling inference** rather than ship an off-switch for it, because it lets a human's edit to the repository change the operator's behaviour with nothing in status recording the move, its central guard has already failed once by cascading, and the explainability its own spec made mandatory was never built. That answers the namespace-leak ask by removal, and means `spec.placement.mode` is never built. **The deletion has shipped**, together with the placement metrics the argument had said to lead *away* from — an objection to their labels, which naming the GitTarget and the type retires — and "what the deletion taught" records the two things building it found: namespace inheritance was a second implementation of a rule that belonged to the governing kustomization, and it was missing the check that the transformer names the resource's own namespace. Open: whether a fall-back to canonical also raises an Event on the GitTarget, and `status.layout` |
| [`placement-visibility-and-declared-defaults.md`](design/placement-visibility-and-declared-defaults.md) | the three questions the inference deletion left, **decided and then not built** — PR #291 shipped the deletion, the counters and the namespace-transformer fix, and none of the eight items this page had queued behind them, which the page now says. The residue is filed as [#295](https://github.com/ConfigButler/gitops-reverser/issues/295) (correctness: a declared path into a kustomize subdirectory is never rendered, and the identity gate rejects the versionless canonical path) and [#296](https://github.com/ConfigButler/gitops-reverser/issues/296) (visibility: `status.layout`, the ambiguous render root, the `declared` metric split, `{kindLower}`). Its Question 2 is superseded outright by the layout model. What still stands: **Keep `canonical`** as the name for the built-in path and split `declared` into `byType`/`default`, because reusing "default" for both a declaration and the absence of one makes the metric unreadable. **No CRD default for `placement.default`**, and the reason is structural rather than the two that look obvious: a defaulted default is never empty, so it shadows the kustomize-root step and every new file in an overlay would take the canonical path, in Git and rendered by nothing. The validation failure is real but the rule doing the rejecting is itself wrong, and the persistence objection is a trade we could take; defaulting the Secret route to work around the first is a floor that vanishes when a user writes any `byType` entry, because map defaults never merge. **`status.layout` instead**, with five worked examples (greenfield, kustomize overlay, brownfield missing one rule, two ambiguous roots, a refusal from an operator-configured sensitive type) over the `MarkTargetRetention` seam, which already enqueues on change and so retires the "the data plane cannot notify the GitTarget" objection. **`{kindLower}`, not a `toLower` function.** Carries three findings that changed a decision: `IdentityCompletePlacementTemplate` demanding `{version}` contradicts the versionless-path decision; two supported kustomizations still produce a file nothing renders and nothing counts; and **a declared path into a subdirectory of a kustomize folder is registered only when render-root scoping happens to be in force**, so one `byType` line reproduces the unrendered-file bug today. Fixing that last one (walk up to the nearest kustomization) also weakens the case against the CRD default from a correctness wall to a legibility trade, which the page says rather than leaving the stronger argument standing |
| [`gittarget-layout-model.md`](design/gittarget-layout-model.md) | **postponed to a later deployment, filed as [#293](https://github.com/ConfigButler/gitops-reverser/issues/293)** — the proposal the placement questions were circling around: a path template is the wrong primitive, so declare what the folder IS. `spec.layout.kind` with the values `Auto`, `Kustomize`, `Tree`, `Flat` and `Template`, plus `byType` overrides valid under every kind, with two rules that carry the value: whatever chose the path, the file is registered with the kustomization that governs it (so F10 becomes unstatable rather than fixed), and a structural kind excludes a blanket `default` (so a declared template can no longer silently disable the render root). `kind: Auto` is a safe CRD default because it NAMES the structural rule instead of standing in front of it, which is why defaulting a mode works where defaulting a path did not, and it is declared inference rather than the undeclared kind that was deleted. `kind: Kustomize` with `create: true` bootstraps an empty repository into a folder `kubectl apply -k` can build. Seven worked examples, a status shape with `declaredKind` beside the resolved `kind`, a mechanical migration for every current configuration, and an argument that the layout should NOT be its own CRD: a shared object changing where N folders write, with nothing on the GitTarget recording it, is the same defect as sibling inference with a different actor, the shared thing is four lines, and generators already solve reuse. Also carries the namespace half: `scope: SingleNamespace` is a STRUCTURAL claim that must agree with the authorization bound `allowedSourceNamespaces`, and it cannot be derived because that matcher may be absent and because the namespaces that arrive come from WatchRule objects that do not own the folder; `writeNamespace` with the values `FromContext`, `Always` and `Never` replaces the inference that decides whether `metadata.namespace` is written, which is the one inference an empty folder cannot perform, and `create: true` lets the operator ESTABLISH the convention by writing `namespace:` into the kustomization it creates. The layout is **immutable** except a widening transition, because GitTarget has no finalizer so recreating one re-adopts every document by identity, and `Auto` resolves once and pins the result so a deleted `kustomization.yaml` cannot silently re-lay-out the folder. Open: whether `scope` should be derived and materialized at creation instead of declared |
| [`gittarget-api-wave.md`](design/gittarget-api-wave.md) | **postponed, filed as [#294](https://github.com/ConfigButler/gitops-reverser/issues/294). Not in 0.41.0**, which already carries the attribution model and the placement break. One breaking wave on GitTarget, sequencing the layout model with the maintainer review's still-open API block (F6, F10, F12's reference nit, §3's pushbacks) and the queue's Tier 2 items (B4, B1, #5, #6). The batching argument is the weaker half; the stronger one is that four of them are the same decision seen from different angles: **the folder is described on the GitTarget and the connection describes only the connection**, which is why `commitWindow` and `commit.message` move off `GitProvider`. Two findings change the layout design rather than accompanying it: `spec.mode: Observe` becomes how a layout is adopted safely (a dry run over `status.layout` instead of declare-and-hope), and `spec.interval` plus an observation pass is what keeps the scan-derived half of that status fresh for a target that writes nothing. `spec.suspend` is a precondition rather than a rider, because a layout that creates a `kustomization.yaml` needs a stop button. Records that F7 already shipped the EventRecorder the placement Event was said to be too expensive for, that layout is mutable like `prune`, that F9 stays OUTSIDE the wave because its answer constrains the enum work, and that the version stays `v1alpha3` with loud rejections rather than paying for a conversion path |
| [`target-watch-plan.md`](design/target-watch-plan.md) | **built.** The companion to [`watch-manager-ownership.md`](design/watch-manager-ownership.md): the ownership page says WHO applies a plan, this one says WHAT a plan is and what changing it may touch. A cell — group, resource, namespace, deliberately no served version — is the one identity the watch stream, the render-fidelity scope and the mark-and-sweep boundary all agree on, because a key that does not round-trip to the scope it sweeps under is the class of error that deletes user data. The plan is diffed into `keep`/`start`/`restart`/`stop` and applied per cell, so adding one WatchRule stops replaying every unrelated cell into a queue shared with other tenants; a `restart` is a served-version change, which is why the version is spec DATA rather than identity. Readiness and the fidelity revision are per scope, so a KEPT cell holds the result its own replay produced rather than being asked to prove itself again over an unrelated edit. `stop` never touches files — removal is a Git-side sweep under the target's existing `spec.prune.mode`, not a watch-layer delete. "Cut at the producer" is the accepted consequence: nothing fences the queue, so a deselected cell may leave a short tail of writes, bounded by the queue and converged afterwards. Still open: the `stop` classification wants a settled `TypeRemoved` from `typeset` (see TODO), and removal on INTENT is undecided. |
| [`watch-manager-ownership.md`](design/watch-manager-ownership.md) | **built.** A rule edit used to be applied inline by the controller worker that observed it, and it re-planned EVERY GitTarget rather than the one the rule names: 1256 plan reconciles across 28 targets in one e2e run, peaking at 78 in a second, behind two network calls, on a shared worker pool. The watch manager had no owner, so eleven mutexes stood in for one. Now controllers post a trigger naming a GitTarget and return, one loop owns the plan and paces itself, and repeated triggers for one target collapse into a single pass. The debounce is framed around how the config is actually edited: a GitTarget and its rules are one piece of configuration applied together, so a per-target ROLLING SILENCE window of 2s (max wait ~10s) turns a five-object `kubectl apply` into one pass, the same mechanism `DefaultCommitWindow` already uses one layer down on the write path. That reverses an earlier revision's "never debounce the first declaration": declaring a GitTarget the instant it lands means declaring it with no rules yet, which manufactures a transient EMPTY plan on every cold start, and an empty plan is what vacuously cleared a write divergence in the fidelity gate. States the contract as "one settled configuration adjustment, not one function invocation": the window is a heuristic and never a correctness boundary (Kubernetes has no apply-complete event), a per-target DIRTY SEQUENCE means a change arriving mid-pass is never lost, and the pass reads a coherent rule-store snapshot rather than the rule that triggered it. Carries the deletion inventory, because the point is that the system got smaller: four trigger mechanisms collapsed to one (`signalCatalogRefresh` and `catalogRefreshCh` are gone), `refreshRunningTargetWatches` and its running-set filter are gone (that filter is why a target whose first declare never completed was never picked up again), and six mutexes went for stated reasons, with `RenderFidelityGate.mu` and the two event-channel locks kept and justified. The four steps shipped: the local-cluster discovery call is bounded (a real defect — the legacy non-context `ServerGroupsAndResources()` ran with no deadline at all), the owner loop carries the debounce, dirty sequence, per-target deadline and 2s/5s/10s/30s/1m backoff, the plan now carries NO lock while the projection is a published snapshot, and catalog invalidation is scoped by diffing each target's rendered plan across the re-projection. "What shipped" records where the implementation departed from the page, including the one bug only e2e caught: streams were parented to the PASS context, which a deadline cancels the moment the pass returns, so every stream died the instant its plan was applied — and it reads like health, because the plan logs `start:1`, every later pass reports `keep:1` and never restarts it, and nothing logs an error while readiness sits at `Replaying` and every WatchRule sits `Ready=False`. A stream's parent is the manager's lifetime; the pass deadline bounds the pass. A second e2e catch is a BEHAVIORAL consequence worth knowing: toggling a rule off and on inside the settle window is no longer a replay — it used to tear the stream down and re-establish it because each apply replanned synchronously, and it is now one pass over a plan that never changed. Correct (a net-zero change is no change; widening `prune.mode` remains the supported force) but a real difference in what an operator gesture does. Both specs that broke were also gating on the wrong thing: asking a GitTarget "are all your streams running" about a change to ONE rule, which a target that is already mirroring answers True to before that rule has been planned, and which a different controller publishes than the one that compiled the rule — so the rule's OWN StreamsRunning is the gate, and `waitForWatchRuleStreamsRunning` existed unused for exactly this. Departures: reports became a published snapshot rather than a second channel; ISOLATION came from taking the I/O off the loop rather than from the deadline (a pass never dials, the shared refresh runs on its own goroutine, and the deadline is the backstop it should have been — a first cut that kept two network calls on the loop had one unreachable cluster holding every healthy target, which is the same availability failure relocated); DELETION names an incarnation resolved when it is queued, because both production callers react to a NotFound and carry no UID, so a UID-less delete matched everything and could tear down the successor of a same-name recreate; and persistent failure surfaces as `WatchPlanFailing`, where pending means "no pass has ever landed", not "dirty right now". Step 3's type-to-target index did not earn its staleness. Still open: whether the settle window ever needs to be configurable |
| [`docs-linting.md`](design/docs-linting.md) | how to mechanize [`style-guide.md`](style-guide.md) with markdownlint-cli2 and Vale. Both are wired into `task lint`, gated on the files [`.docs-lint-scope`](../.docs-lint-scope) lists rather than the whole tree: 102 of 174 files fail markdownlint and 148 of 174 fail Vale, so the two backlogs need different gates. Open: how the scope list grows to cover the tree, the `MD013` limit, and whether `AGENTS.md` and the chart READMEs are in scope |
| [`attribution-removal-wait-options.md`](design/attribution-removal-wait-options.md) | a removal now waits for evidence about the DELETION rather than accepting the object's last write, which stopped it naming whoever last edited the object as the author of a deletion they did not perform. Enumerates the eight situations a resolution can be in and shows the cost is concentrated in exactly one: a removal for which no delete fact will ever arrive (a graceful pod delete, a status-only removal, a type the audit policy skips) spends the whole grace to return the answer it had at t=0, measured at ~3.1s against ~70ms when evidence is present. Prices five options against that, and recommends a per-route watermark — stop waiting once the fact stream has demonstrably moved past this event — over a second timeout flag whose right value lives in the API server's config rather than ours. Open: the decision, and how common the case is outside the e2e suite |
| [`watch-and-catalog-architecture.md`](design/watch-and-catalog-architecture.md) | the target three-layer watch model — **needs a human call before building** |
| [`metrics-observability-plan.md`](design/metrics-observability-plan.md) | the canonical metrics plan, reconciled to the code after the fact-stream switchover and now carrying the attribution surface that shipped (documented in [`spec/attribution.md`](spec/attribution.md)). Reads the product as one pipeline — watch events arrive, and are processed into commits — and maps a metric to each stage. The attribution join is built and correctly labelled; **watch ingestion, shard queue delay, and the relevance filter are still dark**. **Phase 1 — the attribution relabel plus the loss-path counters — has shipped**; Phase 2 is the watch stage, Phase 3 the filter and push health, Phase 4 the dashboard and alerts. Open: Phases 2-4, and the dashboard JSON is deliberately not written until the watch families exist |
| [`reconcile-triggering.md`](design/reconcile-triggering.md) | which controllers still fail to wake up |
| [`multi-source-audit-ingress-hardening.md`](design/multi-source-audit-ingress-hardening.md) | how independent sources authenticate to a named audit route, when annotation routing is trustworthy, and how multi-provider ingestion remains fair |
| [`release-image-reuse-plan.md`](design/release-image-reuse-plan.md) | PRs 2–5 unstarted |
| [`e2e-coverage-gaps-and-improvements-plan.md`](design/e2e-coverage-gaps-and-improvements-plan.md) | tests A/B/C still proposals |
| [`e2e-finish-plan.md`](design/e2e-finish-plan.md) | remaining e2e harness work |
| [`sensitive-resource-diagnostics-follow-up.md`](design/sensitive-resource-diagnostics-follow-up.md) | deferred diagnostics |
| [`e2e-git-server-choice.md`](design/e2e-git-server-choice.md) | stay on Gitea or move to Forgejo — the `_csrf` pin is fixable in place on both, so the migration is now a preference call, not a fix; also why we adopt no SDK either way |
| [`azure-devops-multi-ack.md`](design/azure-devops-multi-ack.md) | **decided and built: go-git v6** — why Azure DevOps rejects our fetches, and what to do instead of PR [#292](https://github.com/ConfigButler/gitops-reverser/pull/292)'s bundled `git` binary. The capability filter fails in two independent halves: advertising `multi_ack` is a four-line change, but v5 then cannot parse the multi-ACK **response**, which only a fetch with `have` lines provokes. That is why **Flux ships ADO support on v5 with no git binary — it never fetches**, only `CloneContext`, so it never enters the path v5 cannot serve; our persistent-clone-plus-incremental-fetch design is the opposite, which makes the trim alone insufficient for us. **go-git v6 already implements `multi_ack`** (PR #1204, in every v6 tag; upstream then deleted their ADO example saying it "works out of the box"), and its churn in the packages we import runs 96 → 39 → **1** → **9** removals per alpha, so it is one settled breaking wave rather than a moving target; the migration is four known API removals over two rewritten files, `transport.AuthMethod` being the invasive one. Prices PR #292 as measured rather than argued: the image goes **217 MB → 940 MB**, of which 723 MB is a `cp -rL` that dereferences 165 hardlinks to one binary (a one-character fix), arm64 is unaffected and native, but **Trivy reports zero findings on both images** while the new one carries git 2.54.0, OpenSSH 10.3p1 and OpenSSL 3.5.7 as loose files no package database describes — so the CRITICAL gate is blind to a third of the runtime. Also catches an unflagged non-ADO regression (`Depth: 1` dropped, so every provider full-fetches) and 10% patch coverage on an untestable path. The unlock is that **canonical `git upload-pack` advertises `multi_ack`** (verified), so the Gitea already in the e2e lab plus a 400-injecting proxy is a faithful ADO simulator — no tenant needed, and the only way any option becomes CI-testable. Four options priced, and Option A (v6) is the one shipped. Carries a measured **capability matrix** over our three network calls with two diagrams, which narrows the blast radius to **one call, `repo.Fetch`**: `receive-pack` never advertises `multi_ack` (measured), so **the atomic push is out of scope for every option** — its safety rests on the same-session advertisement plus the server-side `Old`/`New` compare-and-swap in `packp.Command`, neither of which touches `upload-pack`, and we already push from a shallow store today. v6 keeps that pattern 1:1 (`Handshake` → `GetRemoteRefs`/`Push`, same `[]*packp.Command`), which is an argument *for* migrating. Records what the migration actually cost, including the four v6 behaviour changes it surfaced — two of them settings v6 reads from the environment and fails closed on, invisible to unit tests |

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
[`direction-and-configuration-surface.md`](future/direction-and-configuration-surface.md)
is the strategy review on top of it: the config-as-data direction as the headline with
brownfield mirroring as the on-ramp, a decided Helm standpoint (declaration editing plus
the values projection; helm-light inversion parked behind entry criteria), and worked
examples of where the configuration surface should go.
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
