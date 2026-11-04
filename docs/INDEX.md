# Index: what to read, and what is history

This page names the documents that bind the current implementation. If a document is not
listed here, it is either a user guide (see [`README.md`](README.md)) or historical context
you can safely skip.

## The folders, and what each one means

| Folder | Means | Binds? |
|---|---|---|
| [`spec/`](spec/) | **This is true now, and the code depends on it.** Most are cited by path from Go source. Change the behaviour, change the doc. | **yes** |
| [`design/`](design/) | **We are still deciding.** Open questions, proposals, unbuilt work. | yes — as intent, not as shipped behaviour |
| [`facts/`](facts/) | Durable reference: how Kubernetes behaves, and what we discovered about it. | yes, as reference |
| [`finished/`](finished/) | **This happened.** Shipped plans, closed investigations. Kept for context. | **no** |
| [`layout/`](layout/README.md) | **One topic, all of it.** Where a document goes in Git: two current-behaviour contracts, the proposal, its plan, its review, and the worked examples. | per document, and each is labelled |

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

A second exception, and this one is about topic rather than lifecycle. [`layout/`](layout/README.md)
collects the whole layout question, which had grown to eight documents across three folders, so
following the argument meant knowing which folder each step lived in. It mixes lifecycle classes
on purpose and labels every entry with the class it would have had, which its
[README](layout/README.md) does. It is the only topic folder, and adding a second one should take
the same amount of argument this one did.

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

**Before you re-raise a claim:** [`misconceptions.md`](misconceptions.md) collects the beliefs the
project held between July and October 2026 that turned out to be false, most of them about applying
Git back to the cluster, each with what is true and where it was corrected.

**Before you name anything:** [`definitions.md`](definitions.md) — one name per concept (resource collection,
watch, branch worker, author attribution, refusal, folder), the two clusters and the two namespace directions, and
the nine rules that decide a CRD field, condition type, reason, enum value or printer column. It is
the fourth of four rulebooks, beside [`style-guide.md`](style-guide.md) for prose,
[`config-flag-conventions.md`](config-flag-conventions.md) for flags, and
[`spec/status-conditions-guide.md`](spec/status-conditions-guide.md) for conditions and reasons. A
name that contradicts it is a defect rather than a variation.

**Before changing source watches:**
[`facts/kubernetes-watch-options.md`](facts/kubernetes-watch-options.md) covers server-side
label and field selection, membership events, initial state, bookmarks, and recovery, with
Kubernetes documentation and upstream source references.

**Before you change any CRD field:**
[`facts/crd-upgrade-strategies.md`](facts/crd-upgrade-strategies.md) — the two honest strategies for
removing or renaming one, a decision matrix keyed on whether pruning fails open or closed, and the
API-server behaviour that decides it. Measured rather than assumed: a status update does not
re-validate spec; a removed field stops being served immediately, not after the next write (so a
migration inventory must be taken BEFORE the upgrade); and a previously-defaulted field cannot be
removed by `kubectl apply`, which makes refusing one an upgrade nobody can complete.

**Before you write a CRD description:**
[`facts/crd-descriptions-in-kubectl-explain.md`](facts/crd-descriptions-in-kubectl-explain.md)
covers how `kubectl explain` renders a doc comment (line breaks kept, each line rewrapped at 76
characters of text, so longer lines leave stray words), behavior a description must not misstate,
and Flux's writing style guide.

## The contracts — [`spec/`](spec/)

The code cites these. Breaking one without updating it is how the next person gets
misled. Full list in [`spec/README.md`](spec/README.md); the ones that carry a
*rule* rather than a description:

| Spec | The rule |
|---|---|
| [`manifest-system.md`](spec/manifest-system.md) | the whole live → Git pipeline, and every invariant below in summary |
| [`attribution.md`](spec/attribution.md) | **how a commit gets its author.** Deletion is attributed at intent time; the publish half files a fact under the strongest key it has, the join half walks the tiers strongest-first, and neither branches on the type. The single reference, folded from six design records |
| [`watch-event-ordering-and-attribution-grace.md`](spec/watch-event-ordering-and-attribution-grace.md) | how attribution waits and branch queues preserve **per-watch** order, and why that order does not extend across overlapping watches |
| [`current-manifest-support-review.md`](spec/current-manifest-support-review.md) | all-or-nothing folder claim; never half-write a multi-doc file; **refuse rather than prune** |
| [`manifestedit-field-ownership-spike.md`](spec/manifestedit-field-ownership-spike.md) | the API wins — full-object ownership, never field-subset |
| [`reconcile-via-watchlist-mark-and-sweep.md`](spec/reconcile-via-watchlist-mark-and-sweep.md) | **no bookmark, no sweep** |
| [`sops-single-file-no-multidoc.md`](spec/sops-single-file-no-multidoc.md) | one encrypted file is one document |
| [`scale-subresource-audit-rehydration.md`](spec/scale-subresource-audit-rehydration.md) | `/scale` only; every other subresource ignored |
| [`commit-window-refactor.md`](spec/commit-window-refactor.md) | one grouped commit = one (author, GitTarget) |
| [`gittarget-isolation-on-rule-change.md`](spec/gittarget-isolation-on-rule-change.md) | a rule change on target A never touches target B |
| [`audit-readiness-probe-plan.md`](spec/audit-readiness-probe-plan.md) | liveness must never depend on Redis |
| [`type-followability.md`](spec/type-followability.md) | is a type followable, and if not, the one reason |
| [`gitpath-foreign-content-stringency.md`](spec/gitpath-foreign-content-stringency.md) | refusing a path that shadows foreign content |
| [`unsupported-folder-refusal-plan.md`](spec/unsupported-folder-refusal-plan.md) | `GitPathAccepted`, and refusing what we cannot own |
| [`commitrequest-design.md`](spec/commitrequest-design.md) | the CommitRequest window and its conditions |
| [`where-validation-lives.md`](spec/where-validation-lives.md) | schema → CEL → **the reconciler**; no admission webhook ships |
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

Each page opens with a **label** saying where it stands: **design** (still being
decided), **design, decided** (decision made, not built), **partly built**, **built**, or
**deferred** (parked, kept as a decision record). The label is the first thing in the page,
so you never have to read a proposal to find out it already shipped.

### Open (27 pages)

| Doc | Open question |
|---|---|
| [`branch-worker-event-model.md`](design/branch-worker-event-model.md) | **proposal; its in-process fixes are built in #412 and #413.** Publication retry, Git operation deadlines, intake pause and resume, and publication status are done. Worker ownership, FIFO inputs, save and deadline semantics, and the transition boundary for later durability, which is next. Journal storage and rollout belong to the HA plan. |
| [`gittarget-branch-worker-pending-writes.md`](design/gittarget-branch-worker-pending-writes.md) | **built in #413.** Ordered pending writes, one materializer and one retry schedule; a safe admission boundary for the watch; Git call deadlines; intake pause and resume; publication status. Holds the recovery contract for an outage, replay limits, and the acceptance scenarios. |
| [`event-pipeline-overview.md`](design/event-pipeline-overview.md) | **snapshot, 2026-10-04.** Diagrams of the pipeline as built: watch streams and their replay/resume rules, the three meanings of "replay", the branch worker's inputs, the pending writes' lifecycle, retry and intake, a save end to end, what is an event today, and an honest list of what is still missing. |
| [`gittarget-parent-branch.md`](design/gittarget-parent-branch.md) | **merged in #407.** `spec.parentBranch`: a target stays on standby until a write needs a branch, then creates its write branch from the parent's tip as checked at publication. The contract as built, the defect it started from, prior art and naming. Open: parent per `GitProvider.status.branches` entry. |
| [`gittarget-parent-hardening.md`](design/gittarget-parent-hardening.md) | **done in #407.** Parent changes on a live worker, an empty repository gaining a ref, an unresolved default branch, recovery that cannot strand work, and the refused upload, without adding a pull to the fast path; plus the two recovery fixes from the review at `93120caf`. Records the merge boundary and the next separate PRs. |
| [`gittarget-parent-empty-repository.md`](design/gittarget-parent-empty-repository.md) | **deferred.** An empty repository's default branch: the server reports it over protocol v2 but go-git discards it (upstream fix first); then a separate policy decision on which branch the first commit may create. Assuming `main` was considered and rejected. |
| [`gittarget-parent-observation.md`](design/gittarget-parent-observation.md) | **deferred.** Keep the parent visible after the write branch exists: availability first, ancestry only after measuring how often a shallow checkout can answer it. Informational; authorizes no branch mutation. |
| [`gittarget-red-status-plan.md`](design/gittarget-red-status-plan.md) | **design, partly built.** The smaller alternative to Git write preflight: make a `GitTarget` turn red and explain why no commit was made, without moving Git checks into the admission webhook and without adding a second state source. Keeps the existing kstatus trio and domain conditions, ranks the most likely operator-facing failures, and has now shipped the first status-transition slice: refusal messages republish when details change, scoped refusals cannot be cleared by unrelated scope success, `BlockMessage()` carries stable issue kind/path/actor detail, and the unsupported-folder e2e covers Git-side break and recovery. |
| [`open-asks-priority.md`](design/open-asks-priority.md) | **the work queue.** Merges three overlapping backlogs — the downstream consumer asks, the API-surface block left unbuilt by the status and configuration-model review, and the config-surface proposal (B1–B6) — into one ordered queue under four stated tests, and says where we deliberately do **not** do what was asked. The standing caveat narrowed once the layout model reversed: a Tier 2 entry belongs to postponed [#294](https://github.com/ConfigButler/gitops-reverser/issues/294) only if it breaks a `GitTarget` field, and everything else is independently schedulable. Makes one design call against what was asked: **delete Option C sibling inference** rather than ship an off-switch for it, because it let a human's edit to the repository change operator behavior with nothing in status recording the move. That deletion has shipped, and "what the deletion taught" records what building it found. **F9 is Tier 1**: the only item whose answer is unknown rather than whose work is unscheduled, and it gates planning the enum work |
| [`proving-the-metric-surface.md`](design/proving-the-metric-surface.md) | **design, partly built.** What identity an operator needs to locate a failure, and what an e2e check has to do to be worth trusting. Written after a metric value that was declared, switched on, surfaced as a condition reason and counted turned out to have no producer at all, and the check added to catch that class then skipped whenever the counter read zero — so deleting every recording call left the suite green. Two rules follow: an assertion must not take its trigger from the metric under test, and no enum value may be reachable only through an injected fake. Carries a cardinality policy that separates bounded configuration identity from per-request identity, a table of which instrument families should own `source_cluster` directly rather than by join, and the PromQL reasoning for `increase()` versus `max_over_time` (neither applies mechanically: one cannot see an event before the first sample, the other cannot reconstruct totals across a reset). Steps 1–4 are built; step 5, diagnostics export, the `e2e_run` scrape label and a pinned-seed regression are not |
| [`target-watch-plan.md`](design/target-watch-plan.md) | **partly built.** The diff is built and applied; removal semantics are not. The companion to [`watch-manager-ownership.md`](design/watch-manager-ownership.md): the ownership page says WHO applies a plan, this one says WHAT a plan is and what changing it may touch. A resource collection — group, resource, namespace, deliberately no served version — is the one identity the watch stream, the render-fidelity scope and the mark-and-sweep boundary all agree on, because a key that does not round-trip to the scope it sweeps under is the class of error that deletes user data. The plan is diffed into `keep`/`start`/`restart`/`stop` and applied per collection, so adding one WatchRule stops re-initializing every unrelated collection into a queue shared with other tenants; a `restart` is a served-version change, which is why the version is spec DATA rather than identity. Readiness and the fidelity revision are per scope, so a KEPT collection holds the result its own initialization produced rather than being asked to prove itself again over an unrelated edit. `stop` never touches files — removal is a Git-side sweep under the target's existing `spec.prune.mode`, not a watch-layer delete. "Cut at the producer" is the accepted consequence: nothing fences the queue, so a deselected collection may leave a short tail of writes, bounded by the queue and converged afterwards. Still open: the `stop` classification wants a settled `TypeRemoved` from `typeset` (see TODO), and removal on INTENT is undecided. |
| [`watches-labels-simplification.md`](design/watches-labels-simplification.md) | **step 2, implemented.** After the collection rename and operation-filter removal, native per-rule `objectSelector` (#146). Kubernetes is authoritative: `Always` compares selected identities with all managed Git identities in the type/namespace scope, without comparing Git labels. Full support includes supported Kustomize layouts from the first release, plus label-exit attribution (now selector-agnostic, see the attribution spec) and ordered producer replacement. |
| [`label-selection-follow-ups.md`](design/label-selection-follow-ups.md) | **design, open.** What the label-selection review rounds left open, with the evidence for each: real deletions in a selected collection wait the full 3s attribution grace (the watch cannot tell them from a label exit, measured), the resync fence that stops overlapping collections from coalescing (kept on purpose), same-second overlap ties, and one store rebuild per object coming back. Also lists what was fixed on the branch |
| [`attribution-removal-wait-options.md`](design/attribution-removal-wait-options.md) | a removal now waits for evidence about the DELETION rather than accepting the object's last write, which stopped it naming whoever last edited the object as the author of a deletion they did not perform. Enumerates the eight situations a resolution can be in and shows the cost is concentrated in exactly one: a removal for which no delete fact will ever arrive (a graceful pod delete, a status-only removal, a type the audit policy skips) spends the whole grace to return the answer it had at t=0, measured at ~3.1s against ~70ms when evidence is present. Prices five options against that, and recommends a per-route watermark — stop waiting once the fact stream has demonstrably moved past this event — over a second timeout flag whose right value lives in the API server's config rather than ours. Open: the decision, and how common the case is outside the e2e suite |
| [`concurrent-author-windows.md`](design/concurrent-author-windows.md) | **proposal, not built.** A branch worker holds one commit window at a time, so a second person's edit closes the first person's window and splits their change into two commits. Proposes a window per GitTarget and author, open side by side only when the writes' conflict footprints (the Git files they may change, kept separate from the per-object coalescing key) are proven disjoint; anything unproven stays serial. Every close trigger keeps today's exceptions (a push hint and an apply flush leave a save's window alone), windows close in opening order, and commit order becomes close order. Tested against an independent reference model on every commit, and footprint proofs tied to a tree revision and recomputed when it moves, and shipped in five stages behind a startup flag. Follow-ups in `window-field-ownership-and-apply-hold.md` |
| [`window-field-ownership-and-apply-hold.md`](design/window-field-ownership-and-apply-hold.md) | **proposal, not built.** Two separate follow-ups to concurrent author windows. Part 1: a window owns the fields it changed, the way an SSA field manager does, so two authors on one object split by field. It reopens the decided `manifestedit-field-ownership-spike.md` and argues the use case it asks for; every field-owned commit writes only owned fields, and convergence is a separate, explicitly attributed repair that compares Git with a desired view (withheld applies undone), never the live object; experimental until that is accepted. Part 2: an incoming Git commit that is provably disjoint from the open windows (rendered, against the last applied revision) holds the apply instead of closing them, with an absolute deadline of its own; it needs a fetch on a busy branch and brings back a bounded form of the wait S1 removed. The next candidate if measurements justify it. Lists each part's potential problems |
| [`commit-timing-surface.md`](design/commit-timing-surface.md) | **built.** One configuration surface for commit windows and saves, an in-place `v1alpha3` break with one tested upgrade procedure: only writes open a window; a `window` block (`idleTimeout`, `maxDuration`, target default `1m`) on both kinds, which a `CommitRequest` replaces whole for the window it attaches to; `attach: CurrentOrNext \| Next` with `attachTimeout`; opt-in `whenNothingToCommit: CommitEmpty` that keeps the cause in the reason. Precise clock and zero rules. Supersedes the save-wait timer model. Open: the target's `maxDuration` value |
| [`metrics-observability-plan.md`](design/metrics-observability-plan.md) | **built.** the canonical metrics document, rewritten 2026-09 and shipped in the same wave. Reads the product as two ingestion paths — object state through watch, authorship through audit — joining at the resolver, and gives each stage one counter with a bounded `outcome` so the flow and its loss paths are drawable. Records what three review rounds corrected, including two reversals worth not re-litigating: the fact-loss counters must NOT be merged (an eviction counts a fact, a trim gap an occurrence, a decode error a whole batch), and a cumulative counter cannot replace a current count, which is why `status.streams` and `status.retainedDocuments` stayed. The instrument inventory lives in [`interpreting-metrics.md`](interpreting-metrics.md), not here. Open: the Grafana JSON and alert rules |
| [`reconcile-triggering.md`](design/reconcile-triggering.md) | which controllers still fail to wake up |
| [`applier-echoes.md`](design/applier-echoes.md) | **built 2026-10-20, released as one change.** The applier's own write, back through the watch, is mirrored and re-planned onto a newer tip on replay, reverting a Git-side push. Two ideas close every way a stale picture can reach a commit: a write that changes nothing does not exist, and a pending write carries the actions decided on its tree, not a picture. The applier's write in the apply window is not mirrored (F1); a write is upserts and deletes decided once, so a replay never sweeps again and a save keeps its objects (F2); a resync never decides from a snapshot collected while Git was ahead or across an apply, by an apply epoch (F3); another applier's object is not mirrored into a coordinated folder (F4); flush, then apply (S1); `CatchUp` only to adopt API creates and to converge, for Flux and Argo CD alike, and no apply before every initial snapshot is published (S2). Carries the collisions left open, the promises, the code plan, thirteen e2e scenarios, and the twelve decisions taken while building (among them: siblings do not count for F3, a 5-minute hold, initial snapshots exempt from F3, the prefetch removed, no heal resync, and the flush covering the sibling workers) |
| [`apply-trigger.md`](design/apply-trigger.md) | **built for Flux and Argo CD; a webhook applier is the aim.** Why GitOps Reverser decides when Git is applied, and why the applier never applies on its own: a Flux `Kustomization` is held suspended by its `FluxClaim` and resumed for each apply, an Argo CD `Application`'s automated sync is held off by its `ArgoCDClaim`. Carries what building it taught (timers are triggers, a long interval is not exclusive control, a decision must hold at the write, a retry is a decision so only the worker makes one, an apply is an operation whose end is observed), the kustomize-controller v1.9.6 facts the suspended lifecycle rests on, its operation (resume token, outcome, the suspend after the apply), who owns `spec.suspend` (an applier claim acts only while an eligible `GitTarget` references it, one applier claim per applier), restart recovery, shared sources, the measured ~50 ms cost, flush then apply, `CatchUp` as adopt and converge for Flux and Argo CD alike, the apply window with the applier's own writes kept out of Git, resyncs held off while Git is ahead, another applier's objects kept out of a coordinated folder, and the decisions of 2026-10-17, 2026-10-19, 2026-10-20 and 2026-10-23 (the rule, the `allowCrossNamespace.appliers` grant, `InControl`, the reversals of 2026-10-20, Argo CD's automated sync held off like a suspend) |
| [`push-cooldown.md`](design/push-cooldown.md) | **partly built.** Whether the fixed `5s` push cooldown still earns its place now that the commit window exists. Ordinary timer closures already space one author writing one target, but identity changes, saves with shorter timers and buffer-limit closures commit sooner, and serialization does not batch queued commits, so the cooldown is what batches them. Option C, the failure backoff, is built in #412 and shares the push timer; removing the success wait would now remove only `lastPushAt` and its branch, so that decision waits on the event-loop measurements in §9 |
| [`multi-source-audit-ingress-hardening.md`](design/multi-source-audit-ingress-hardening.md) | how independent sources authenticate to a named audit route, when annotation routing is trustworthy, and how multi-provider ingestion remains fair |
| [`release-image-reuse-plan.md`](design/release-image-reuse-plan.md) | PRs 2–5 unstarted |
| [`e2e-coverage-gaps-and-improvements-plan.md`](design/e2e-coverage-gaps-and-improvements-plan.md) | tests A/B/C still proposals |
| [`sensitive-resource-diagnostics-follow-up.md`](design/sensitive-resource-diagnostics-follow-up.md) | deferred diagnostics |
| [`e2e-git-server-choice.md`](design/e2e-git-server-choice.md) | stay on Gitea or move to Forgejo — the `_csrf` pin is fixable in place on both, so the migration is now a preference call, not a fix; also why we adopt no SDK either way |

### Built, and kept here anyway (7 pages)

These have shipped. They stay in `design/` under the exception above, because Go source
cites them by path as the rationale for what the code does, and `finished/` declares
itself non-binding. Read them as history that the code still points at.

| Doc | Open question |
|---|---|
| [`push-notification-and-reconcile-trigger.md`](design/push-notification-and-reconcile-trigger.md) | **built, and superseded** by [`apply-trigger.md`](design/apply-trigger.md): the record of how the applier claim was decided. Kept here because its Part 1, how the branch worker reads the remote (the pre-push fetch, the push advertisement, base trust), is cited from the Git code by section number |
| [`vocabulary-cleanup.md`](design/vocabulary-cleanup.md) | **built.** The breaking release that made the API obey [`definitions.md`](definitions.md): condition reasons that restated their type are `Succeeded`, `GitRepoConfigNotFound` is `GitProviderNotFound`, printer columns are named for what they read and the dependency projections are gone, a bare commit hash is `commit` in every status field, and the source-cluster provider's throttles are `spec.client`. Kept here because `definitions.md` cites it as the record of which audit findings the existing rulebooks already settled: the `insecure` flag split, bare-noun booleans, `--redis-addr`, and the lowercase `sops` enum |
| [`created-root-namespace.md`](design/created-root-namespace.md) | **built.** One question with five answers: what namespace a `kustomization.yaml` the operator CREATES should carry. Decided **B, never write one** — `spec.serializeNamespace: false` means the artifact does not encode its deployment namespace, and adding a root must not quietly change that contract; the namespace comes from the documents when the field is unset or `true`, and from the installer (Flux `targetNamespace`, Argo `destination.namespace`) when it is `false`. Records the three facts an earlier draft got wrong (a namespace-less root is ordinary, both installers supply one, and what refused it was our own fidelity gate rather than kustomize), and carries the scoped fidelity rule that follows: the namespace is ignored in the render comparison ONLY when the governing root sets none, so a root that declares `namespace: shop` still rejects a live `billing` object. Shipped in #328, which is also why [`kustomization_bootstrap.go`](../internal/git/kustomization_bootstrap.go) cites the page by path. Also the sibling call: under `useKustomize` a placement no `resources:` list would name is refused rather than committed unrendered |
| [`watch-manager-ownership.md`](design/watch-manager-ownership.md) | A rule edit used to be applied inline by the controller worker that observed it, and it re-planned EVERY GitTarget rather than the one the rule names: 1256 plan reconciles across 28 targets in one e2e run, peaking at 78 in a second, behind two network calls, on a shared worker pool. The watch manager had no owner, so eleven mutexes stood in for one. Now controllers post a trigger naming a GitTarget and return, one loop owns the plan and paces itself, and repeated triggers for one target collapse into a single pass. The debounce is framed around how the config is actually edited: a GitTarget and its rules are one piece of configuration applied together, so a per-target ROLLING SILENCE window of 2s (max wait ~10s) turns a five-object `kubectl apply` into one pass, the same mechanism `DefaultCommitWindow` already uses one layer down on the write path. That reverses an earlier revision's "never debounce the first declaration": declaring a GitTarget the instant it lands means declaring it with no rules yet, which manufactures a transient EMPTY plan on every cold start, and an empty plan is what vacuously cleared a write divergence in the fidelity gate. States the contract as "one settled configuration adjustment, not one function invocation": the window is a heuristic and never a correctness boundary (Kubernetes has no apply-complete event), a per-target DIRTY SEQUENCE means a change arriving mid-pass is never lost, and the pass reads a coherent rule-store snapshot rather than the rule that triggered it. Carries the deletion inventory, because the point is that the system got smaller: four trigger mechanisms collapsed to one (`signalCatalogRefresh` and `catalogRefreshCh` are gone), `refreshRunningTargetWatches` and its running-set filter are gone (that filter is why a target whose first declare never completed was never picked up again), and six mutexes went for stated reasons, with `RenderFidelityGate.mu` and the two event-channel locks kept and justified. The four steps shipped: the local-cluster discovery call is bounded (a real defect — the legacy non-context `ServerGroupsAndResources()` ran with no deadline at all), the owner loop carries the debounce, dirty sequence, per-target deadline and 2s/5s/10s/30s/1m backoff, the plan now carries NO lock while the projection is a published snapshot, and catalog invalidation is scoped by diffing each target's rendered plan across the re-projection. "What shipped" records where the implementation departed from the page, including the one bug only e2e caught: streams were parented to the PASS context, which a deadline cancels the moment the pass returns, so every stream died the instant its plan was applied — and it reads like health, because the plan logs `start:1`, every later pass reports `keep:1` and never restarts it, and nothing logs an error while readiness sits at `Replaying` and every WatchRule sits `Ready=False`. A stream's parent is the manager's lifetime; the pass deadline bounds the pass. A second e2e catch is a BEHAVIORAL consequence worth knowing: toggling a rule off and on inside the settle window is no longer a replay — it used to tear the stream down and re-establish it because each apply replanned synchronously, and it is now one pass over a plan that never changed. Correct (a net-zero change is no change; widening `prune.mode` remains the supported force) but a real difference in what an operator gesture does. Both specs that broke were also gating on the wrong thing: asking a GitTarget "are all your streams running" about a change to ONE rule, which a target that is already mirroring answers True to before that rule has been planned, and which a different controller publishes than the one that compiled the rule — so the rule's OWN StreamsRunning is the gate, and `waitForWatchRuleStreamsRunning` existed unused for exactly this. Departures: reports became a published snapshot rather than a second channel; ISOLATION came from taking the I/O off the loop rather than from the deadline (a pass never dials, the shared refresh runs on its own goroutine, and the deadline is the backstop it should have been — a first cut that kept two network calls on the loop had one unreachable cluster holding every healthy target, which is the same availability failure relocated); DELETION names an incarnation resolved when it is queued, because both production callers react to a NotFound and carry no UID, so a UID-less delete matched everything and could tear down the successor of a same-name recreate; and persistent failure surfaces as `WatchPlanFailing`, where pending means "no pass has ever landed", not "dirty right now". Step 3's type-to-target index did not earn its staleness. Still open: whether the settle window ever needs to be configurable |
| [`docs-linting.md`](design/docs-linting.md) | how to mechanize [`style-guide.md`](style-guide.md) with markdownlint-cli2 and Vale. Both are wired into `task lint`, gated on the files [`.docs-lint-scope`](../.docs-lint-scope) lists rather than the whole tree: 102 of 174 files fail markdownlint and 148 of 174 fail Vale, so the two backlogs need different gates. Open: how the scope list grows to cover the tree, the `MD013` limit, and whether `AGENTS.md` and the chart READMEs are in scope |
| [`azure-devops-multi-ack.md`](design/azure-devops-multi-ack.md) | go-git v6 was the answer: why Azure DevOps rejects our fetches, and what to do instead of PR [#292](https://github.com/ConfigButler/gitops-reverser/pull/292)'s bundled `git` binary. The capability filter fails in two independent halves: advertising `multi_ack` is a four-line change, but v5 then cannot parse the multi-ACK **response**, which only a fetch with `have` lines provokes. That is why **Flux ships ADO support on v5 with no git binary — it never fetches**, only `CloneContext`, so it never enters the path v5 cannot serve; our persistent-clone-plus-incremental-fetch design is the opposite, which makes the trim alone insufficient for us. **go-git v6 already implements `multi_ack`** (PR #1204, in every v6 tag; upstream then deleted their ADO example saying it "works out of the box"), and its churn in the packages we import runs 96 → 39 → **1** → **9** removals per alpha, so it is one settled breaking wave rather than a moving target; the migration is four known API removals over two rewritten files, `transport.AuthMethod` being the invasive one. Prices PR #292 as measured rather than argued: the image goes **217 MB → 940 MB**, of which 723 MB is a `cp -rL` that dereferences 165 hardlinks to one binary (a one-character fix), arm64 is unaffected and native, but **Trivy reports zero findings on both images** while the new one carries git 2.54.0, OpenSSH 10.3p1 and OpenSSL 3.5.7 as loose files no package database describes — so the CRITICAL gate is blind to a third of the runtime. Also catches an unflagged non-ADO regression (`Depth: 1` dropped, so every provider full-fetches) and 10% patch coverage on an untestable path. The unlock is that **canonical `git upload-pack` advertises `multi_ack`** (verified), so the Gitea already in the e2e lab plus a 400-injecting proxy is a faithful ADO simulator — no tenant needed, and the only way any option becomes CI-testable. Four options priced, and Option A (v6) is the one shipped. Carries a measured **capability matrix** over our three network calls with two diagrams, which narrows the blast radius to **one call, `repo.Fetch`**: `receive-pack` never advertises `multi_ack` (measured), so **the atomic push is out of scope for every option** — its safety rests on the same-session advertisement plus the server-side `Old`/`New` compare-and-swap in `packp.Command`, neither of which touches `upload-pack`, and we already push from a shallow store today. v6 keeps that pattern 1:1 (`Handshake` → `GetRemoteRefs`/`Push`, same `[]*packp.Command`), which is an argument *for* migrating. Records what the migration actually cost, including the four v6 behaviour changes it surfaced — two of them settings v6 reads from the environment and fails closed on, invisible to unit tests |
| [`source-scope-simplification.md`](design/source-scope-simplification.md) | except the additive `SelfSubjectAccessReview` pass it explicitly leaves for later. Declines Flux-style impersonation, deletes `GitTarget.spec.allowedSourceNamespaces` and its selector machinery (**4,569 lines**, and the only cross-cluster read in the authorization path), renames two source-cluster provider fields, and redefines `sourceNamespace: "*"` as one cluster-wide list and watch. The argument is an API reading, not a security one: the chain from a Git folder back to the object that fills it never leaves one namespace, so ordinary RBAC on `watchrules` already answers it. **Keeps `allowedNamespaces`** (renamed `accessFrom`), reversing an earlier draft — source RBAC bounds what a credential may READ, never which tenant may WIELD it. Prices what is lost: source-side label selectors, which have no replacement. Archaeology in [`facts/kubernetes-impersonation-and-flux-identity.md`](facts/kubernetes-impersonation-and-flux-identity.md) |

## The layout topic — [`layout/`](layout/README.md)

Where a live object's document goes in Git, and what else has to change so that file is reachable.
Collected by topic rather than by lifecycle, so the folder mixes binding contracts with an unbuilt
proposal; its [README](layout/README.md) labels each one. Two of these are `spec/`-class and cited
by path from Go source.

| Document | Class | What it holds |
|---|---|---|
| [`contextual-namespace.md`](layout/contextual-namespace.md) | **spec** | kustomize namespace inference; the supported subset |
| [`new-file-placement-rules.md`](layout/new-file-placement-rules.md) | **spec** | where a new resource's file goes: declared, the folder's one kustomize root, canonical. Sibling inference is removed, and kept as history |
| [`model.md`](layout/model.md) | **built** | **reversed, much smaller than it was, and now shipped** in #326 and #328. The earlier thesis wanted `spec.placement` replaced by a `spec.layout` discriminated union; three of its five arguments were retired by [#319](https://github.com/ConfigButler/gitops-reverser/pull/319), which made registration an invariant. So the template **stays** and gains two optional booleans: **`spec.placement.useKustomize`** (create and maintain the folder's root; registering into a root that already exists is an invariant, not a setting) and **`spec.serializeNamespace`** (a `*bool`, because unset must keep meaning "infer" — no plain default preserves today's behavior), which sits one level up because it governs the bytes of every write and the identity a managed document is found by, not just new files. Carries four kustomize facts **measured** against v5.8.1, three of which contradict the earlier model; the `status.placement` stanza and the post-scan pass; and the build order. The headline is what it deletes — `spec.layout`, `kind`, `scope`, `kustomize.create`, the `LayoutProfile` question, the migration, **the post-scan supplier guard** (the supplier of a namespace-free folder lives in another cluster and may not even be single, so the check fires on the correct configuration), and **the dry-run framing of `spec.suspend`** (a scratch branch is a better preview and needs nothing built, so `suspend` is a panic knob and `status.placement` explains writes rather than previewing them) — so the largest breaking change in the queue stops being breaking at all |

The worked examples that used to sit in this folder are now the **layout corpus** at
[`test/fixtures/layout-corpus/`](../test/fixtures/layout-corpus/README.md), because a test executes
every one of them: `shapes/` is the specification by example (the cross-product of folder shapes,
with one live object written into all of them, so the only difference between two folders is the
configuration that produced it), and `specific-examples/` is the remainder (an Argo CD app-of-apps,
a Flux two-layer repository, and the shared prerequisites). They are still design material rather
than install manifests. What changed is that they are now checked.

## Deferred, but still wanted — [`future/`](future/)

[`idea-application-editing.md`](future/idea-application-editing.md) is where the
whole edit-through-the-API workstream started, and still holds the branch/session
grouping strategies nothing else covers.
[`ha-gittarget-distribution-plan.md`](future/ha-gittarget-distribution-plan.md) owns the durable
journal, publication recovery, outage retention, and persistence/HA rollout. The branch worker
event model owns execution semantics and the Git operation-deadline fix; its publication retry
shipped in #412.
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
[`git-api-three-way-comparison.md`](future/git-api-three-way-comparison.md) is the
deferred investigation into two independent edits to different fields of one
object: what a common baseline would have to be, five evidence sources weighed for supplying or standing in for one, and
the counterexample that limits each. It opens by naming what API-first already
settles, and records why a partial merge would be worse than the rule we have.
Four more ideas sit beside them.

## History — [`finished/`](finished/)

Fourteen shipped plans and closed investigations. **Nothing here binds.** Read one
only when you want to know *why* something is the way it is; the answer to *what it
is* always lives in `spec/`.

The newest is
[`provider-access-rules.md`](finished/provider-access-rules.md): why every reference that lends a
credential, a branch or reach needs an explicit grant on the object it reaches (`allowedGitBranches`,
`allowedGitTargets`, `allowedApplierClaims`, each with a required `from`), with nothing automatic,
not even the own namespace. It replaced `accessFrom`, `allowedBranches` and `allowCrossNamespace`
before any of them shipped, and records what revocation promises, where the code departs from the
plan, and the two recovery tests still missing. Shipped as #441.

Before that,
[`attribution-fact-stream.md`](finished/attribution-fact-stream.md): why attribution facts stopped
being a keyspace the watch side polls and became a per-type log it follows into a bounded in-memory
index. It deleted the `SET`/`GET` fact keys, the 150ms poll loop, and the `deletecollection`
expander, replacing the expander with one collection fact that every removal in its scope joins by
uid membership or by scope. That last part is a capability gain rather than a like-for-like swap: a
collection delete the API server sent no response body for used to lose its author entirely.
`exact_deletecollection_item` is replaced by `deletecollection_body_uid` and `deletecollection_scope`, and
`--author-attribution-transport=memory` runs attribution with no Redis on a single replica (the
watch resume cursors still live in Redis, so this frees attribution from it rather than the install).
Shipped as #283, #284, #286 and #287.

Earlier,
[`analyzer-consumer-contract-asks.md`](finished/analyzer-consumer-contract-asks.md): why a
refusal carries whether it can be solved and by whom, why the analyzer's report is a KRM
document that names the build that produced it, and why `ResourceIdentifier.Key()` is a
cross-product contract with a golden test. Shipped as #273 and #275.

Note that most of the pre-2026-07 audit-pipeline archaeology has been deleted
outright: the watch-first rewrite removed `internal/gate`, the audit joiner, and
the audit-as-state pipeline, so ~30 documents describing them were prose about
code that no longer exists. `git log` has them.
