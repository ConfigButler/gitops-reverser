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

**Before you change any CRD field:**
[`facts/crd-upgrade-strategies.md`](facts/crd-upgrade-strategies.md) — the two honest strategies for
removing or renaming one, a decision matrix keyed on whether pruning fails open or closed, and the
API-server behaviour that decides it. Measured rather than assumed: a status update does not
re-validate spec; a removed field stops being served immediately, not after the next write (so a
migration inventory must be taken BEFORE the upgrade); and a previously-defaulted field cannot be
removed by `kubectl apply`, which makes refusing one an upgrade nobody can complete.

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

Each page opens with a **label** saying where it stands: **design** (still being
decided), **design, decided** (decision made, not built), **partly built**, **built**, or
**deferred** (parked, kept as a decision record). The label is the first thing in the page,
so you never have to read a proposal to find out it already shipped.

### Open — 16 pages

| Doc | Open question |
|---|---|
| [`open-asks-priority.md`](design/open-asks-priority.md) | **the work queue.** Merges three overlapping backlogs — the gitops-api consumer asks, the API-surface block left unbuilt by the status and configuration-model review, and the config-surface proposal (B1–B6) — into one ordered queue under four stated tests, and says where we deliberately do **not** do what was asked. The standing caveat narrowed once the layout model reversed: a Tier 2 entry belongs to postponed [#294](https://github.com/ConfigButler/gitops-reverser/issues/294) only if it breaks a `GitTarget` field, and everything else is independently schedulable. Makes one design call against what was asked: **delete Option C sibling inference** rather than ship an off-switch for it, because it let a human's edit to the repository change operator behavior with nothing in status recording the move. That deletion has shipped, and "what the deletion taught" records what building it found. **F9 is Tier 1**: the only item whose answer is unknown rather than whose work is unscheduled, and it gates planning the enum work |
| [`placement-visibility-and-declared-defaults.md`](design/placement-visibility-and-declared-defaults.md) | **design.** The three questions the inference deletion left, **decided and then not built**: PR #291 shipped the deletion and none of the eight items queued behind it. The residue was filed as [#295](https://github.com/ConfigButler/gitops-reverser/issues/295) — **which shipped in 0.42.1 via [#319](https://github.com/ConfigButler/gitops-reverser/pull/319) and is what reversed the layout model** — and [#296](https://github.com/ConfigButler/gitops-reverser/issues/296). Its Question 2 is superseded outright by [`layout/model.md`](layout/model.md). What still stands: keep `canonical` as the name for the built-in path and split `declared` into `byType`/`default`; **no CRD default for `placement.default`**, on the structural argument that a defaulted default is never empty and so shadows the kustomize-root rung; `status.layout` instead, over the `MarkTargetRetention` seam that already enqueues on change; and `{kindLower}`, not a `toLower` function |
| [`created-root-namespace.md`](design/created-root-namespace.md) | **design, decided.** One question with five answers: what namespace a `kustomization.yaml` the operator CREATES should carry. Decided **B, never write one** — `spec.serializeNamespace: false` means the artifact does not encode its deployment namespace, and adding a root must not quietly change that contract; the namespace comes from the documents when the field is unset or `true`, and from the installer (Flux `targetNamespace`, Argo `destination.namespace`) when it is `false`. Records the three facts an earlier draft got wrong (a namespace-less root is ordinary, both installers supply one, and what refused it was our own fidelity gate rather than kustomize), and carries the scoped fidelity rule that follows: the namespace is ignored in the render comparison ONLY when the governing root sets none, so a root that declares `namespace: shop` still rejects a live `billing` object. Also the sibling call: under `useKustomize` a placement no `resources:` list would name is refused rather than committed unrendered |
| [`build-order.md`](design/build-order.md) | **design**, and the one page that is only about sequencing. Five in-flight changes resolve to **three tracks that do not block each other** — additive placement, the breaking source-scope wave, and patch authoring — with the two real couplings named (`*` is defined in terms of the field the wave deletes; `useKustomize`'s created root depends on the one-source-namespace rule) and three couplings people keep assuming that do not exist. Holds no design: every item is specified elsewhere and the specification wins |
| [`gittarget-api-wave.md`](design/gittarget-api-wave.md) | **design**, filed as [#294](https://github.com/ConfigButler/gitops-reverser/issues/294). What is left of one breaking wave on `GitTarget` after the layout model reversed and left it: B4's `commitWindow`/`commit.message` move off the connection, the source-scope deletion (the only member that makes the API smaller), and the riders. Organizing principle: **the folder is described on the GitTarget, the connection describes only the connection** — and this is where that becomes a struct boundary rather than a sentence, since grouping a field is free only in a release that is already breaking. `spec.mode` and `GitTarget.spec.interval` are both **dropped**, with re-open triggers. Records that F9's envtest stays OUTSIDE the wave and gates it, and that staying `v1alpha3` on loud rejections is a **one-consumer countdown**, not a constant |
| [`target-watch-plan.md`](design/target-watch-plan.md) | **partly built.** The diff is built and applied; removal semantics are not. The companion to [`watch-manager-ownership.md`](design/watch-manager-ownership.md): the ownership page says WHO applies a plan, this one says WHAT a plan is and what changing it may touch. A cell — group, resource, namespace, deliberately no served version — is the one identity the watch stream, the render-fidelity scope and the mark-and-sweep boundary all agree on, because a key that does not round-trip to the scope it sweeps under is the class of error that deletes user data. The plan is diffed into `keep`/`start`/`restart`/`stop` and applied per cell, so adding one WatchRule stops replaying every unrelated cell into a queue shared with other tenants; a `restart` is a served-version change, which is why the version is spec DATA rather than identity. Readiness and the fidelity revision are per scope, so a KEPT cell holds the result its own replay produced rather than being asked to prove itself again over an unrelated edit. `stop` never touches files — removal is a Git-side sweep under the target's existing `spec.prune.mode`, not a watch-layer delete. "Cut at the producer" is the accepted consequence: nothing fences the queue, so a deselected cell may leave a short tail of writes, bounded by the queue and converged afterwards. Still open: the `stop` classification wants a settled `TypeRemoved` from `typeset` (see TODO), and removal on INTENT is undecided. |
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

### Built, and kept here anyway — 4 pages

These have shipped. They stay in `design/` under the exception above, because Go source
cites them by path as the rationale for what the code does, and `finished/` declares
itself non-binding. Read them as history that the code still points at.

| Doc | Open question |
|---|---|
| [`watch-manager-ownership.md`](design/watch-manager-ownership.md) | A rule edit used to be applied inline by the controller worker that observed it, and it re-planned EVERY GitTarget rather than the one the rule names: 1256 plan reconciles across 28 targets in one e2e run, peaking at 78 in a second, behind two network calls, on a shared worker pool. The watch manager had no owner, so eleven mutexes stood in for one. Now controllers post a trigger naming a GitTarget and return, one loop owns the plan and paces itself, and repeated triggers for one target collapse into a single pass. The debounce is framed around how the config is actually edited: a GitTarget and its rules are one piece of configuration applied together, so a per-target ROLLING SILENCE window of 2s (max wait ~10s) turns a five-object `kubectl apply` into one pass, the same mechanism `DefaultCommitWindow` already uses one layer down on the write path. That reverses an earlier revision's "never debounce the first declaration": declaring a GitTarget the instant it lands means declaring it with no rules yet, which manufactures a transient EMPTY plan on every cold start, and an empty plan is what vacuously cleared a write divergence in the fidelity gate. States the contract as "one settled configuration adjustment, not one function invocation": the window is a heuristic and never a correctness boundary (Kubernetes has no apply-complete event), a per-target DIRTY SEQUENCE means a change arriving mid-pass is never lost, and the pass reads a coherent rule-store snapshot rather than the rule that triggered it. Carries the deletion inventory, because the point is that the system got smaller: four trigger mechanisms collapsed to one (`signalCatalogRefresh` and `catalogRefreshCh` are gone), `refreshRunningTargetWatches` and its running-set filter are gone (that filter is why a target whose first declare never completed was never picked up again), and six mutexes went for stated reasons, with `RenderFidelityGate.mu` and the two event-channel locks kept and justified. The four steps shipped: the local-cluster discovery call is bounded (a real defect — the legacy non-context `ServerGroupsAndResources()` ran with no deadline at all), the owner loop carries the debounce, dirty sequence, per-target deadline and 2s/5s/10s/30s/1m backoff, the plan now carries NO lock while the projection is a published snapshot, and catalog invalidation is scoped by diffing each target's rendered plan across the re-projection. "What shipped" records where the implementation departed from the page, including the one bug only e2e caught: streams were parented to the PASS context, which a deadline cancels the moment the pass returns, so every stream died the instant its plan was applied — and it reads like health, because the plan logs `start:1`, every later pass reports `keep:1` and never restarts it, and nothing logs an error while readiness sits at `Replaying` and every WatchRule sits `Ready=False`. A stream's parent is the manager's lifetime; the pass deadline bounds the pass. A second e2e catch is a BEHAVIORAL consequence worth knowing: toggling a rule off and on inside the settle window is no longer a replay — it used to tear the stream down and re-establish it because each apply replanned synchronously, and it is now one pass over a plan that never changed. Correct (a net-zero change is no change; widening `prune.mode` remains the supported force) but a real difference in what an operator gesture does. Both specs that broke were also gating on the wrong thing: asking a GitTarget "are all your streams running" about a change to ONE rule, which a target that is already mirroring answers True to before that rule has been planned, and which a different controller publishes than the one that compiled the rule — so the rule's OWN StreamsRunning is the gate, and `waitForWatchRuleStreamsRunning` existed unused for exactly this. Departures: reports became a published snapshot rather than a second channel; ISOLATION came from taking the I/O off the loop rather than from the deadline (a pass never dials, the shared refresh runs on its own goroutine, and the deadline is the backstop it should have been — a first cut that kept two network calls on the loop had one unreachable cluster holding every healthy target, which is the same availability failure relocated); DELETION names an incarnation resolved when it is queued, because both production callers react to a NotFound and carry no UID, so a UID-less delete matched everything and could tear down the successor of a same-name recreate; and persistent failure surfaces as `WatchPlanFailing`, where pending means "no pass has ever landed", not "dirty right now". Step 3's type-to-target index did not earn its staleness. Still open: whether the settle window ever needs to be configurable |
| [`docs-linting.md`](design/docs-linting.md) | how to mechanize [`style-guide.md`](style-guide.md) with markdownlint-cli2 and Vale. Both are wired into `task lint`, gated on the files [`.docs-lint-scope`](../.docs-lint-scope) lists rather than the whole tree: 102 of 174 files fail markdownlint and 148 of 174 fail Vale, so the two backlogs need different gates. Open: how the scope list grows to cover the tree, the `MD013` limit, and whether `AGENTS.md` and the chart READMEs are in scope |
| [`azure-devops-multi-ack.md`](design/azure-devops-multi-ack.md) | go-git v6 was the answer: why Azure DevOps rejects our fetches, and what to do instead of PR [#292](https://github.com/ConfigButler/gitops-reverser/pull/292)'s bundled `git` binary. The capability filter fails in two independent halves: advertising `multi_ack` is a four-line change, but v5 then cannot parse the multi-ACK **response**, which only a fetch with `have` lines provokes. That is why **Flux ships ADO support on v5 with no git binary — it never fetches**, only `CloneContext`, so it never enters the path v5 cannot serve; our persistent-clone-plus-incremental-fetch design is the opposite, which makes the trim alone insufficient for us. **go-git v6 already implements `multi_ack`** (PR #1204, in every v6 tag; upstream then deleted their ADO example saying it "works out of the box"), and its churn in the packages we import runs 96 → 39 → **1** → **9** removals per alpha, so it is one settled breaking wave rather than a moving target; the migration is four known API removals over two rewritten files, `transport.AuthMethod` being the invasive one. Prices PR #292 as measured rather than argued: the image goes **217 MB → 940 MB**, of which 723 MB is a `cp -rL` that dereferences 165 hardlinks to one binary (a one-character fix), arm64 is unaffected and native, but **Trivy reports zero findings on both images** while the new one carries git 2.54.0, OpenSSH 10.3p1 and OpenSSL 3.5.7 as loose files no package database describes — so the CRITICAL gate is blind to a third of the runtime. Also catches an unflagged non-ADO regression (`Depth: 1` dropped, so every provider full-fetches) and 10% patch coverage on an untestable path. The unlock is that **canonical `git upload-pack` advertises `multi_ack`** (verified), so the Gitea already in the e2e lab plus a 400-injecting proxy is a faithful ADO simulator — no tenant needed, and the only way any option becomes CI-testable. Four options priced, and Option A (v6) is the one shipped. Carries a measured **capability matrix** over our three network calls with two diagrams, which narrows the blast radius to **one call, `repo.Fetch`**: `receive-pack` never advertises `multi_ack` (measured), so **the atomic push is out of scope for every option** — its safety rests on the same-session advertisement plus the server-side `Old`/`New` compare-and-swap in `packp.Command`, neither of which touches `upload-pack`, and we already push from a shallow store today. v6 keeps that pattern 1:1 (`Handshake` → `GetRemoteRefs`/`Push`, same `[]*packp.Command`), which is an argument *for* migrating. Records what the migration actually cost, including the four v6 behaviour changes it surfaced — two of them settings v6 reads from the environment and fails closed on, invisible to unit tests |
| [`source-scope-simplification.md`](design/source-scope-simplification.md) | except the additive `SelfSubjectAccessReview` pass it explicitly leaves for later. Declines Flux-style impersonation, deletes `GitTarget.spec.allowedSourceNamespaces` and its selector machinery (**4,569 lines**, and the only cross-cluster read in the authorization path), renames two `ClusterProvider` fields, and redefines `sourceNamespace: "*"` as one cluster-wide list and watch. The argument is an API reading, not a security one: the chain from a Git folder back to the object that fills it never leaves one namespace, so ordinary RBAC on `watchrules` already answers it. **Keeps `allowedNamespaces`** (renamed `accessFrom`), reversing an earlier draft — source RBAC bounds what a credential may READ, never which tenant may WIELD it. Prices what is lost: source-side label selectors, which have no replacement. Archaeology in [`facts/kubernetes-impersonation-and-flux-identity.md`](facts/kubernetes-impersonation-and-flux-identity.md) |

## The layout topic — [`layout/`](layout/README.md)

Where a live object's document goes in Git, and what else has to change so that file is reachable.
Collected by topic rather than by lifecycle, so the folder mixes binding contracts with an unbuilt
proposal; its [README](layout/README.md) labels each one. Two of these are `spec/`-class and cited
by path from Go source.

| Document | Class | What it holds |
|---|---|---|
| [`contextual-namespace.md`](layout/contextual-namespace.md) | **spec** | kustomize namespace inference; the supported subset |
| [`new-file-placement-rules.md`](layout/new-file-placement-rules.md) | **spec** | where a new resource's file goes: declared, the folder's one kustomize root, canonical. Sibling inference is removed, and kept as history |
| [`model.md`](layout/model.md) | **design** | **reversed, and much smaller than it was.** The earlier thesis wanted `spec.placement` replaced by a `spec.layout` discriminated union; three of its five arguments were retired by [#319](https://github.com/ConfigButler/gitops-reverser/pull/319), which made registration an invariant. So the template **stays** and gains two optional booleans: **`spec.placement.useKustomize`** (create and maintain the folder's root; registering into a root that already exists is an invariant, not a setting) and **`spec.serializeNamespace`** (a `*bool`, because unset must keep meaning "infer" — no plain default preserves today's behavior), which sits one level up because it governs the bytes of every write and the identity a managed document is found by, not just new files. Carries four kustomize facts **measured** against v5.8.1, three of which contradict the earlier model; the `status.placement` stanza and the post-scan pass; and the build order. The headline is what it deletes — `spec.layout`, `kind`, `scope`, `kustomize.create`, the `LayoutProfile` question, the migration, **the post-scan supplier guard** (the supplier of a namespace-free folder lives in another cluster and may not even be single, so the check fires on the correct configuration), and **the dry-run framing of `spec.suspend`** (a scratch branch is a better preview and needs nothing built, so `suspend` is a panic knob and `status.placement` explains writes rather than previewing them) — so the largest breaking change in the queue stops being breaking at all |

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
Four more ideas sit beside them.

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
