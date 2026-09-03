# What to build next: the open asks, ordered

> **design**: a priority call, not a plan of record. Nothing here binds until scheduled.
> Index: [`../INDEX.md`](../INDEX.md)
> Date: 2026-07-29, swept 2026-07-30 against the 0.41.0 release and again 2026-09-03 against 0.43.0.
>
> **What the second sweep found.** The postponed wave is no longer postponed: `spec.suspend`,
> `status.placement` and the reconcile-request annotation shipped in
> [#326](https://github.com/ConfigButler/gitops-reverser/pull/326); `useKustomize` and
> `serializeNamespace` in [#328](https://github.com/ConfigButler/gitops-reverser/pull/328); B4 and
> the source-scope deletion in [#330](https://github.com/ConfigButler/gitops-reverser/pull/330),
> which is what makes 0.43.0 a breaking release. Of the consumer asks this page ranks, **#22, #11
> and #10 are gone**: the `permanent` doc comment was rewritten, the two encoders became one
> (`internal/yamlstyle`, which is why a create and an update now emit identical bytes), and sibling
> inference — the thing #10 was about — was deleted outright. What is left of the consumer list is
> **#15** (a declared `auditRoute` that has received zero facts should say so), **#23** (which actor
> deletion-as-intent picks when a controller clears a finalizer), and **#6** (a movable
> `GitTarget` destination), and of the wave, the riders. The tiers below have not been re-ordered
> against that; read them as the argument, and [`../TODO.md`](../TODO.md) as the queue.
>
> **Where this stands as of the sweep.** `0.41.0` is the attribution release: the fact stream, the
> sticky removal pointer, the metric relabel, the name tier, and the analyzer/encoder corrections,
> plus PR #291's sibling-inference deletion and placement counters. That is a large breaking release
> on its own, and **the GitTarget work is deliberately not in it**. The standing caveat has since
> narrowed, because [`../layout/model.md`](../layout/model.md) reversed and the placement work is no
> longer breaking: **a Tier 2 entry belongs to the postponed wave only if it changes a `GitTarget`
> field in a breaking way**, and [`gittarget-api-wave.md`](gittarget-api-wave.md) is the one place
> that lists which those are. Everything else here — the placement fields, `suspend`,
> `status.placement`, and every Tier 1 entry — is additive and independently schedulable, and should
> not wait for a bump.
>
> **The queue was built bottom-up rather than top-down.** Tier 0 and Tier 1 are still unbuilt, and
> the Tier 2 item nobody scheduled — the attribution fact stream — shipped anyway, together with a
> consumer ask (#23) that arrived after this page was first written and was fixed before it was ever
> ranked. That is recorded in [already shipped](#already-shipped-and-struck-from-the-queue) rather
> than smoothed over: the ordering rule below is still the argument, and the deviation from it is
> a fact about the last week, not a revision of the rule.
>
> Three backlogs are open at once and they overlap: the gitops-api consumer asks (revision 11,
> 2026-07-28, which is the revision that filed #23), the API-surface block left unbuilt by the
> status and configuration-model review — now sequenced in
> [`gittarget-api-wave.md`](gittarget-api-wave.md) — and the config-surface proposal in
> [`config-surface-for-a-structured-repository.md`](../future/config-surface-for-a-structured-repository.md)
> (B1–B6). This page merges them into one queue and says where we deliberately do **not** do
> what was asked.
>
> The entry that used to be "already specified rather than merely wanted" is now built:
> [`attribution-fact-stream.md`](../finished/attribution-fact-stream.md) has moved to `finished/`.
> What it leaves behind is smaller and differently shaped, and it still changes how the
> highest-priority consumer ask should be built. See
> [what the stream work left open](#what-the-stream-work-left-open-tier-2).

## The ordering rule

Four tests, applied in order. They are what produced the queue in
[the queue](#the-queue), and they are the part worth arguing with:

1. **Does it stop the product being silently wrong?** Attribution is the product. A
   misconfiguration that mirrors perfectly and authors every commit `unknown` outranks
   everything, because nothing surfaces it until the commits already exist.
2. **Does it get cheaper by being done now?** Every spec-field change is free while we are
   `v1alpha3` and the consumer count is one. gitops-api pins us three ways (image, Go module,
   `require` line), so the breaking wave costs one coordinated bump today and N tomorrow.
3. **Is a deletion available instead of a switch?** A feature that needs an off-switch is a
   feature with a design problem. Removing it is smaller than the enum that guards it, and it
   removes the enum too.
4. **Does it make the output legible?** This product's entire output is a Git diff. Things that
   make the diff unreadable are not cosmetic here, whatever they would be elsewhere.

Test 3 is why this document exists rather than a straight re-ranking of the asks, and it lands
on sibling inference. It has a second instance, arrived at independently and since **measured**:
[`attribution-fact-stream.md`](../finished/attribution-fact-stream.md) deleted the fact keyspace, the
150ms poll loop and the `deletecollection` expander rather than optimizing any of them, and ended up
with less code answering *more* cases than before — a collection delete the API server sent no
response body for used to lose its author entirely. Two of these in one quarter is a pattern worth
naming: the parts of this system that hurt are the parts that reconstruct something from state they
do not own.

---

## The one real design call: delete sibling inference, do not switch it off — SHIPPED

> **Built.** `resolveInferred` through `allSameDir` are gone, the kustomize-root fallback stayed, and no
> enum was added. What building it added to the argument below is recorded in
> [what the deletion taught](#what-the-deletion-taught). The spec's Option C sections are retained as
> history in [`../layout/new-file-placement-rules.md`](../layout/new-file-placement-rules.md),
> and the behaviour change has a [`docs/UPGRADING.md`](../UPGRADING.md) entry.

The config-surface proposal's **B3** offers `spec.placement.mode: Infer|Declared|Strict`: an
enum that lets a user turn inference off. This document argues the opposite: **remove Option C's
cohort ladder entirely**, keep the kustomize-root fallback, and ship no enum at all.

### What inference is, precisely

[`../layout/new-file-placement-rules.md`](../layout/new-file-placement-rules.md) Option C.
It fires **only** for a resource that has no document in Git yet; everything already written is
match-first and never moves. For that narrow case it finds the largest cohort of similar existing
documents (step 1: same type + namespace; step 2: same type, any namespace) and puts the new
document in that cohort's directory, appending to a bundle if the cohort is bundled. Roughly
`resolveInferred` through `allSameDir` in
[`internal/manifestanalyzer/placement.go`](../../internal/manifestanalyzer/placement.go): about a
third of that file, plus its tests.

### Why it should go

**It makes a human's edit to the repository change the operator's behavior, with no Kubernetes
object changing and nothing in status recording the move.** That is the finding already written
down as C3 in the config-surface doc, and it is the whole objection in one line. Delete enough of
one namespace's ConfigMaps from a bundle and the bundle stops being namespace-agnostic, so the
next new ConfigMap takes a different path. The GitTarget did not change. Nobody approved it. No
condition mentions it.

**Its central guard has already failed once, and it failed by cascading.** The
namespace-agnosticism check was, for a period, vacuous on the singleton branch: one directory
holding one namespace satisfied "all the same directory" trivially, so a new namespace's object
was appended into the first namespace's file, which then genuinely spanned two namespaces, which
legitimized the bundle for every later object, which collapsed a whole type into one file. Fixed
on `feat/watchrule-source-namespace-pr4`, and the fix is right. The lesson is not "that bug is
gone"; it is that a rule inferred from mutable state has failure modes that *feed themselves*,
and no amount of care makes the class safe.

**The explainability it requires was declared mandatory and never built.** The spec's own P8 says
the scan/dry-run output **must** state, per new resource, the chosen path plus the cohort and
ladder step that produced it: "without that, 'why did it land there?' is unanswerable". There is
no such trace, no status surface, and the placement skips that do exist are a log line and a
counter. We shipped the inference and not the thing that made it defensible.

**What it actually buys is smaller than the demo suggests.** "Point me at an existing repo and it
just works" is carried overwhelmingly by *match-first*, not by inference: every resource that
already has a document is edited exactly where it lives, forever. Inference only fires for a type
or namespace the target has never written. That is a rare event, and it is precisely the event
where a wrong guess is invisible, because there is no prior file to compare against.

**And the case it cannot handle is the case people have.** P4 in the spec: a custom
per-namespace layout cannot be extended to an unseen namespace, because inference refuses to
reverse-engineer the path segment. So the layout most likely to be hand-authored is exactly the
one inference drops to canonical. The user has to declare it anyway.

### What we keep

**The kustomize-root fallback stays.** It is not inference: it is a structural fact. If the
scanned subtree is governed by exactly one supported kustomization, a new file that is not
reachable from that root is not merely oddly placed, it is *unreachable*; it would never be
rendered. Placing it beside that root and adding the `resources:` entry follows from there being
one root, not from picking the largest matching cohort. Deleting it would reintroduce the bug it
was added to fix.

**`placement.byType` + `placement.default` stay.** The B2 map is already shipped and is the
declared answer for every layout inference cannot reach.

**Match-first stays**, untouched. This changes nothing about existing documents.

### What follows from it

- **Ask #10 dissolves.** The reported symptom (two tenants' `infra` objects, one landing in the
  other's directory, so `git log -- tenants/tenant-acmelive/` shows another tenant's history) is
  not a missing namespace term in the cohort key. Adding one would be `mode: Declared` reached by
  a slower route: every namespace-segmented layout would fall to canonical anyway, which is what
  deleting the ladder does directly. We answer #10 with the deletion, not with the patch.
- **B3's enum is never built.** There is nothing left to switch off, so the API surface shrinks
  by an enum instead of growing by one. This is the trade worth naming: B3 costs a permanent
  three-valued field in a CRD; deletion costs a one-time behavior change.
- **`status.layout` (B2) becomes more valuable, not less.** With inference gone, what remains
  invisible is what the operator *understood* about the folder. That is a projection of facts the
  analyzer already computes, and it is honest in a way an inference never was.

### The cost, stated plainly

A brownfield repo with a bundle layout that today gets a new type appended to the bundle will,
after this, get a canonical path unless the user writes one `byType` line. That is a real
regression for the demo and a real gain for predictability. Cold-start repos are unchanged:
inference already fell to canonical there. It is a behavior change for existing targets, so it
needs a `docs/UPGRADING.md` entry, and it is cheapest now, while the user count makes "one
`byType` line" a sentence in a release note rather than a migration.

**Open, and worth deciding before writing code:** how the user learns they need a `byType` line at
the moment it matters, rather than by noticing a file. The earlier wording here — "a
`PlacementFellBackToCanonical`-style Event on the first new type per target" — hid three decisions,
and the word *Event* meant a real `corev1.Event` through
`mgr.GetEventRecorderFor(...)`, the same recorder
[`status.go`](../../internal/controller/status.go) uses to announce a persisted `Ready` transition.

- **It is not nearly free here, because placement is not a controller.** `LocateNew` is called on the
  branch worker's write path in [`plan_flush.go`](../../internal/git/plan_flush.go) — no reconcile
  context and no recorder in `internal/git` at all. The two surfaces that exist today are a log line
  at the skip site and `ResyncStats.PlacementSkipped`, which is a field in a resync summary rather
  than a Prometheus counter. Neither is user-visible.
- **Which object it attaches to is the actual question.** Not the watched resource: since the
  config-plane split that object may live in a remote source cluster, so an Event on it lands in a
  cluster the user does not read. The right `involvedObject` is the **GitTarget** — local, and the
  object whose `byType` line is the fix — which means the write path has to hand the fact back rather
  than emit it. That seam already exists as the `pathRefusal` →
  `GitPathAccepted=False` projection in
  [`git_path_refusal.go`](../../internal/git/git_path_refusal.go).
- **That seam has a timing flaw which undercuts "at the moment it matters".** A refusal recorded on
  the data plane does not enqueue the GitTarget; it surfaces on the next requeue, up to ten minutes
  later. Good enough for a durable condition, weak for a notification. Events are also
  deduplicated and expire (`--event-ttl` defaults to 1h), so an Event is never the record.

**The shape that follows is a split, not one of the three.** An **Event on the GitTarget** for
timeliness, over the existing refusal seam plus an enqueue; and **`status.layout` (B2)** for
durability, because "what the operator understood about this folder" is where someone looks a day
later. The log line stays.

**What shipped, and where this paragraph was wrong.** A Prometheus counter was the one this document
argued *against* leading with, on the grounds that `placement_fell_back_total` says it happened
somewhere and not which type in which target. That objection was to the **labels**, and it does not
survive them being fixed: `placements_total{source, disposition, gittarget_namespace, gittarget_name,
group, version, resource}` names the target and the exact `byType` key, so one series **is** the line
that is missing. It shipped with two companions the argument had not asked for and should have —
`placement_refusals_total{reason}`, because a resource the writer *declined* to place had no
countable trace at all, and `placement_kustomization_entries_total{outcome}`, whose `failed` value is
a file committed outside every render. The Event and `status.layout` are still the right split for
timeliness and durability, and neither is built; what is no longer true is that there was nothing
actionable in a metric.

### What the deletion taught

Three things came out of building it that the argument above did not contain.

- **Deleting the inference exposed a second implementation of a rule, not just the rule.** "Omit
  `metadata.namespace`, the build context supplies it" was read off a *sibling's bytes*, so it only
  ever fired for an inferred placement. A **declared** path into the same kustomize directory wrote a
  `namespace:` line every other document in that folder omits. The obligation belongs to the
  kustomization that governs the destination, not to whatever document happened to be next door, and
  moving it there fixed the declared path for free.
- **And that rule was missing its safety half.** The old kustomize-root fallback asked only whether a
  `namespace:` transformer was *set*, never whether it named the resource's own namespace. Omitting
  the namespace hands it to kustomize, so a transformer naming a different namespace rendered the
  document as a different object: the mirror claimed to hold a resource it did not. It now writes the
  namespace explicitly in that case and lets the render oracle report a folder that cannot express
  the object. This was reachable before the deletion and is one of the things the deletion's own
  test table found.
- **The write path had no identity to label with, and that is why it had no metrics.** `LocateNew`
  runs on the branch worker with no reconcile context, which this document already noted about the
  Event. The same fact is why the placement counters did not exist: there was no GitTarget on the
  batch to name. It is one field, taken from the events on the live path and from the resolved
  metadata on the resync path — deliberately both, because which of the two created a file is not
  something the operator chose, and a fall-back visible for one and invisible for the other would be
  worse than neither.

---

## The queue

**Filed** means there is a GitHub issue, so the item is legible without this page. **Wave** means it is
part of the postponed breaking GitTarget sequence ([#294](https://github.com/ConfigButler/gitops-reverser/issues/294))
and is not independently schedulable.

| # | Ask | Source | Tier | Tracked |
|---|---|---|---|---|
| 15 | A declared `auditRoute` with zero facts must say so, and a route losing them with it | gitops-api | **1** | — |
| n/a | Stop paying a full grace for a delete fact that will never arrive (F, then C) | [`attribution-removal-wait-options.md`](attribution-removal-wait-options.md) | **1** | — |
| F9 | The `scope: Namespaced` status-write envtest | maintainer review | **1** | outside the wave, and **gates its planning**: the answer decides whether the narrowed enum can be kept ([`gittarget-api-wave.md`](gittarget-api-wave.md)) |
| ~~n/a~~ | ~~A declared path in a kustomize subdirectory is never rendered; the identity gate rejects the versionless canonical path~~ **SHIPPED** in 0.42.1 | [`placement-visibility-and-declared-defaults.md`](placement-visibility-and-declared-defaults.md) | — | [#295](https://github.com/ConfigButler/gitops-reverser/issues/295), [#319](https://github.com/ConfigButler/gitops-reverser/pull/319) |
| n/a | `useKustomize` and `serializeNamespace`: the two things a path template cannot say (`spec.layout` was reversed) | [`../layout/model.md`](../layout/model.md) | **2** | [#322](https://github.com/ConfigButler/gitops-reverser/issues/322), **not** breaking, so not the wave |
| F6 | `spec.suspend`, `GitProvider.spec.interval`, `requestedAt` (no `interval` on `GitTarget`, see [`gittarget-api-wave.md`](gittarget-api-wave.md)) | maintainer review | **2** | wave |
| 5 | `CommitRequest.spec.author`, SAR-guarded | gitops-api (#220) | **2** | wave |
| B4 | `commitWindow` / `commit.message` move to GitTarget | config surface | **2** | wave |
| ~~B1~~ | ~~`GitTarget.spec.mode: Observe\|Write`~~ **dropped**: `suspend` already stops the writes, and `mode` buys only a declared posture over a pause | config surface | — | [`gittarget-api-wave.md`](gittarget-api-wave.md) |
| 6 | Movable destination via `status.observedDestination` | gitops-api (#220) | **2** | wave |
| F10 | CommitRequest TTL / ownerRef + the `delete` verb | maintainer review | **2** | wave |
| n/a | The blocking resolve is head-of-line on the shard goroutine | [`../spec/attribution.md`](../spec/attribution.md#the-wait) | **2** | — |
| B2 | `GitTarget.status.placement` (was `status.layout`) | config surface | **3** | [#296](https://github.com/ConfigButler/gitops-reverser/issues/296) |
| n/a | The ambiguous render root, the `declared` metric split, `{kindLower}`, canonical-as-template | [`placement-visibility-and-declared-defaults.md`](placement-visibility-and-declared-defaults.md) | **3** | [#296](https://github.com/ConfigButler/gitops-reverser/issues/296) |
| B6 | The `default` ClusterProvider not-found message | config surface | **3** | — |
| n/a | An aggregated create carries no name and no body: accept it, or stop waiting for it | [`../spec/attribution.md`](../spec/attribution.md#what-the-shape-driven-rules-reach-and-what-they-do-not) | **3** | — |
| n/a | Entry-size ceiling and per-type stream count under a few hundred watched types | [`attribution-fact-stream.md`](../finished/attribution-fact-stream.md) | **3** | — |
| 10 | Namespace-aware sibling inference *as asked* | gitops-api | **declined — answered by the deletion, SHIPPED** | — |
| B3 | `spec.placement.mode` enum | config surface | **declined** | — |

**Two entries moved up in this sweep.** F9 is Tier 1 because it is not merely one envtest: until it
is run, nobody can plan the enum work, and the answer can force a design change in an object that is
already shipped. An unmeasured fact that gates other people's planning outranks a legibility item.
The declared-path-in-a-subdirectory bug is Tier 1, not Tier 3,
under this page's own first test: one line of ordinary user configuration silently produces a file that
is in Git and rendered by nothing, and nothing in status or in the counters says so. That is the
product being silently wrong, which is what Tier 1 is for. It was written down as a finding rather than
ranked, because it was found while arguing about metric names.

### Already shipped, and struck from the queue

Everything in this section is in **0.41.0**. Four items left this page between 2026-07-28 and
2026-07-29 on `feat/attribution-sticky-removal-pointer`, two more arrived with #290, and the
placement break arrived with #291. They are listed rather than deleted because several of them change
what the *remaining* entries should be.

The release is worth naming as one thing, because it is what makes postponing the GitTarget wave the
right call rather than a delay: **0.41.0 replaces the whole attribution model** (a fact keyspace
becomes a per-type stream, the resolver stops polling Redis, the metric surface is relabelled, and
three populations that used to ship committer-authored now resolve) **and breaks placement**
(sibling inference is gone). Two breaking dimensions in one release is already a lot to ask a consumer
to absorb. A third, on the shape of `GitTarget` itself, is a separate conversation.

- **The attribution fact stream** (was the largest Tier 2 entry). Built as #283, #284, #286 and #287:
  the audit receiver appends one batched entry per type to a per-`(route, group/resource)` stream,
  every process follows only the types it watches, and the facts live in one bounded, TTL'd in-memory
  index. The fact keys, the poll loop and the `deletecollection` expander are gone;
  `exact_deletecollection_item` is replaced by `deletecollection_body_uid` and
  `deletecollection_scope`, and `--author-attribution-transport=memory` runs attribution with no
  Redis on one replica. Record: [`attribution-fact-stream.md`](../finished/attribution-fact-stream.md).
  the expander spec has been folded into [`../spec/attribution.md`](../spec/attribution.md), which keeps
  its deletion-as-intent rule as §1 and drops the sections about the deleted machinery. That discharges
  commitment 6 below.
- **#23 — deletion-as-intent picked the cleanup controller, not the deleter.** Filed in revision 11
  and fixed before it was ranked, because the reproduction fell out of the switchover's own corpus:
  the human's `delete` and the controller's finalizer `patch` both return a body carrying the
  resourceVersion the deletion stamped, so both facts were filed under the same `(uid, resourceVersion)`
  key and the index was last-writer-wins. The deleter's fact was not outranked, it was *replaced*.
  Built as a **sticky removal pointer**: a fact about a deletion may not be overwritten by a fact
  about a write, keyed strictly by uid, consulted ahead of the exact tier for a removal, bounded by
  the index's caps rather than the join TTL. Ships `delete_sticky` on
  `attribution_resolutions_total{tier}`. Record:
  [`../spec/attribution.md`](../spec/attribution.md#three-rules-that-are-easy-to-miss).
- **A name tier**, which was not asked for by anyone. An aggregated-API write or single delete is
  audited with a name but no uid and no resourceVersion, so every stronger tier misses it and it used
  to ship committer-authored. Facts carrying neither identifier are now filed under
  `(namespace, name)` and consulted last. Record:
  [`../spec/attribution.md`](../spec/attribution.md#the-tiers-strongest-first).
- **Phase 1 of the attribution metric surface.** `result` split into `tier` and `actor_kind`, the
  `no_attribution_fact` audit outcome, and the loss-path counters — including the stream decode
  error, which had no symptom at all. Record:
  [`../spec/attribution.md`](../spec/attribution.md#what-is-observable), migration in
  [`UPGRADING.md`](../UPGRADING.md).

- **#22 — the analyzer contract's three false sentences**, all three fixed:
  `ReasonRefusedStructural` no longer calls itself permanent and points at `Solvable`; the refusal
  detail derives its stem from the same classification that sets `Solvable`, so a solvable refusal
  is described as a fault that can be fixed rather than as an unsupported feature (the corpus
  baseline moved on exactly the one refusal that was lying, and on nothing else); and `Actor`
  states which scans can report which values, pinned by a corpus test. The nested-kustomization
  message shares the new stem, which also replaced a catch-all listing every construct it might
  have been with the constructs it actually found.
- **#11 — one encoder.** `internal/yamlstyle` now owns the single style everything committed to
  Git is written in, and a source-scanning test refuses a second encoder in the write path. The
  create path had been rendering sequences at the parent key's column (JSON→YAML) while an
  in-place edit rendered them two columns deeper (yaml.v3), so the first update after a create
  rewrote every list line in the file.

**What that changes for the entries above.** #15 gets cheaper and gains a second half (a route that
is *losing* facts is the same user-visible failure as one that never had any, so it is the same
condition, not a second surface). And two new Tier 1/2 entries exist that did not before, because the
stream made the cost of waiting measurable for the first time: a removal that no delete fact will
ever arrive for spends the whole grace to return the answer it already had at t=0, measured at ~3.1s
against ~70ms when evidence is present, and it does that on the shard's own goroutine.

### Tier 0: correct what is false — SHIPPED

**#22 was three separate things and all three claims checked out against `main`.** They were worth
doing immediately because each one was a sentence and each one was misleading a consumer that reads
our source as the contract. What each one turned into is below; the queue table no longer carries
them.

- **The doc comment.** `ReasonRefusedStructural` is documented as "the permanent support
  boundary" in [`pkg/manifestanalyzer/repo.go`](../../pkg/manifestanalyzer/repo.go), while
  `refusedStructuralReason` in
  [`internal/manifestanalyzer/scan_repo.go`](../../internal/manifestanalyzer/scan_repo.go) sets
  `Solvable` from `classifyKustomizeFeatures` and can return `true`. The code is right; the
  comment is a year-old summary of a boundary that has since acquired a `Solvable` field. Delete
  "permanent" and point the reader at `Solvable`. The consumer wrote `Permanent = true` off that
  sentence and shipped "can never be synced" for a folder whose author had a typo to fix.
  Telling a user "never" when the answer is "not yet" is the worse of the two lies.
- **The refusal-detail stem, and we should go further than asked.** `refusedStructuralDetail`
  builds one stem ("kustomization uses unsupported feature(s): …") for both branches, so a
  solvable refusal is described as an unsupported feature. The parenthetical from
  `kustomizationDecodeError` is the useful half. The ask is a second stem for the solvable
  branch; the better shape is to derive the stem from the same classification that sets
  `Solvable`, so the two cannot drift the way the doc comment did. One function, two stems, one
  input.
- **The `Actor` question, answered: yes, it is structural — and their trace found one of the two
  gates.** The conclusion holds, the reasoning was half of it. `IssueOutOfScope` is gated on
  `AcceptancePolicy.InScope`, which is assigned in exactly two places in the tree, both tests. But
  `ActorPlatformOperator` has a second acceptance raise site they did not reach:
  `IssueUnresolvedKRM` carries it when the type registry has never heard of the GVK
  (`resolveMapping` in [`store.go`](../../internal/manifestanalyzer/store.go)), and that one is
  gated on something else entirely — a not-ready registry resolves every document to
  `MappingNoSource`, so `MappingNotFollowable` never happens on a structure-only path.
  **Cluster-awareness is the real gate, not `InScope`**, which is a better answer than the one
  asked for: it says *why* the branch is unreachable, and it is what makes the guarantee safe to
  state. The doc line lands on `Actor` in both the internal and the exported copy, and a test over
  the layout corpus pins that no structure-only scan names the platform operator.

### Tier 1: silent wrongness

**#15: a declared audit route that has received zero facts.** This is the highest-value item on
the page, because it is the only one where the failure is invisible *and* the thing it breaks is
the product's reason to exist. If the write route and the read route diverge, mirroring stays
perfect and every commit is authored `unknown (attribution unresolved)`. The consumer's own
mitigation (an alert plus a test) fires *after* a wrong commit exists.

Design notes, going slightly beyond the ask:

- It must **not** be a `Stalled`, and must not make `Ready` false. Mirroring genuinely works; a
  kstatus consumer that reads `Failed` here would be wrong. This is a separate condition on
  ClusterProvider (`AuditRouteReceiving`, or similar) plus an Event.
- It must start `Unknown`, not `False`. A route that has existed for four seconds has legitimately
  received nothing. `False` after a grace window, with the window named in the message.
- It should carry the two facts we already hold and the user cannot see: when a fact last arrived
  on this route, and how many have. Zero-with-a-timestamp is the whole signal.
- The Event recorder landed with F7, so the Event is nearly free.

**The transport question is settled, and it settled in #15's favour.** This section used to warn
against deferring #15 until the transport changed. The transport changed first anyway — and the
signal the condition needs is now a directly observable property rather than a statistic we choose
to keep: a per-`(route, group/resource)` stream either has entries or it does not, and the
`attribution_fact_stream_gaps_total` and `attribution_fact_index_evictions_total` counters that
shipped with it already say when a follower is losing facts. Two consequences:

- **Build it against the stream, not against a transport-neutral counter.** The seam still exists
  (`memory` and `redis` both answer "how many facts, and when was the last one"), so define the
  condition on the seam — but there is no longer a keyspace whose counter has to be kept in step.
- **The trim-gap counter folds into this condition rather than growing a second one.** A route
  losing facts and a route that never had any are the same user-visible failure: commits authored
  `unknown (attribution unresolved)`. One condition, two messages.

Where `auditRoute` came from is [`../spec/attribution.md`](../spec/attribution.md#the-scope-is-an-audit-route-and-a-type).
The related question about *how the watch waits* is answered for the transport and reopened one
level down: the six options in
an earlier option analysis are superseded by
[`attribution-fact-stream.md`](../finished/attribution-fact-stream.md), and what remains is *when a
removal should stop waiting*, immediately below.

**Stop paying a full grace for a delete fact that will never arrive.**
[`attribution-removal-wait-options.md`](attribution-removal-wait-options.md) enumerates the eight
situations a resolution can be in and shows the cost is concentrated in exactly one: a removal for
which no delete fact will *ever* arrive — a graceful pod delete, a status-only removal, a type the
audit policy excludes — spends the whole grace to return the answer it held at t=0. It recommends
**F then C**, and they are complementary rather than alternatives:

- **F, a per-`(route, type)` circuit breaker**: stop waiting for a fact that has never once arrived.
  It is in this tier and not a lower one because it is not only a latency fix — a watched type the
  audit policy excludes is a *misconfiguration nobody currently learns about*, which is #15's failure
  mode one level down. **Build it with #15, from the same counters, or it becomes a third surface
  saying the same thing.**
- **C, a per-route watermark**: stop waiting once the fact stream has demonstrably moved past this
  event. It removes the transient case using data already in the index, and it is the only option
  that answers "is a fact still coming?" rather than guessing with a timeout.

What is explicitly *not* measured yet, and should be before either lands: how common the
never-resolved population is outside the e2e suite, and whether a quiet route's watermark advances
often enough to be worth having. The one number we do have came from a single run.

**The inference deletion — SHIPPED.** It sat in this tier for the reason argued above: repo state
changing operator behavior invisibly is the same class of defect as an audit route that silently
resolves nothing. What replaced it is a declaration plus
`placements_total{source="canonical"}` per (GitTarget, type), so the same class of defect now has a
query. `#10` and `B3` are answered by it and stay declined.

**F9: the `scope: Namespaced` status-write envtest.** In this tier because it is the only item on
the page whose *answer is unknown* rather than whose work is unscheduled, and because everything
downstream assumes an answer. If the apiserver validates the whole object on a status-subresource
write, the narrowed `ClusterWatchRule` enum leaves the one object that most needs to explain itself
unable to write its own `Stalled` condition. The test, the version it has to name and the fallback
are in [`gittarget-api-wave.md`](gittarget-api-wave.md). Run it before planning anything that
depends on it.

### Tier 2: the breaking wave, all at once, while `v1alpha3`

These all add or change a spec field. Doing them as one `feat(api)!` sequence costs the consumer
one coordinated bump; doing them one at a time costs six.

#### What the stream work left open (Tier 2)

The stream itself is built, and the only two decisions this section used to hold — whether to do it,
and whether the in-memory transport ships in the first cut — were both taken: yes, and yes (guarded
by a conformance suite both transports pass, and refused by the chart for `replicaCount > 1`). What
is left is one structural item and two capacity questions.

**The blocking resolve is head-of-line on the shard goroutine.** This was measured, not reasoned:
three removals ahead of a write each sat out most of a ten-second grace — 20.2s between them — for
evidence the index already held, and the write behind them missed the commit window it belonged to.
The lookup-ordering half is fixed (that is what the sticky pointer and the tier reordering did), and
the structural half is untouched: *any* removal that must wait out its grace still stalls every later
event on its shard. Two directions, from
[`../spec/attribution.md`](../spec/attribution.md#the-wait):

1. **Bound the removal's extra wait separately from the grace.** Once a fallback is in hand the fact
   stream for that scope is demonstrably live, so what is outstanding is an audit-batch interval
   rather than a full grace. Small; needs a number chosen with evidence. This is the same measurement
   the F-then-C work in Tier 1 needs, so the two share their evidence.
2. **Stop blocking the shard.** Resolve attribution off the event loop and reassemble in order. The
   real answer, and the larger one; it fixes every other cause of a slow resolve too. It is in Tier 2
   rather than Tier 1 because nothing is *wrong* — commits are correct and correctly attributed, they
   are late.

One test-environment note for whoever picks this up: the e2e default of
`--author-attribution-grace=10s` makes the blocking three times worse than the product default of
3s. That amplifies a product behaviour; it is not a setting anyone runs.

**Two capacity questions, both Tier 3 and both unanswered by design rather than by oversight**
(the stream doc's own remaining open questions): nothing bounds a single stream entry's size today —
`DefaultFactStreamMaxLen` bounds a stream in entries — and one `XREAD` across a few dozen streams is
ordinary but a cluster watching several hundred types wants checking before it is assumed.

**HA is closer, and the ownership problem is still the blocker.** A per-type stream with independent
cursors is the primitive multiple replicas need, and it exists now. What remains, beyond
[`ha-gittarget-distribution-plan.md`](../future/ha-gittarget-distribution-plan.md)'s ownership work,
is one small ordering decision with a visible effect: whether a replica warms its index before
starting a watch it has taken over. Replaying the type's window is cheap; starting the watch first
loses attribution for the handover window. That belongs with the ownership work, not here.

**F6: `spec.suspend` first.** The maintainer review's bottom line stands: this controller writes
to a Git repository and there is no way to make it stop that is not deleting the object.
`spec.interval` on GitProvider (a real `ls-remote` per pass, hardcoded at 5 min, no jitter) and
the `requestedAt` annotation ride along.

**#5: `CommitRequest.spec.author`, SAR-guarded — and #23 has retired one of its two
arguments.** This section used to claim that *audit cannot attribute a finalized delete at all*, because
the human's delete is recorded as an update setting `deletionTimestamp` while the real delete names
whichever controller cleared the last finalizer. **That claim is now false, and it is worth saying so
here rather than quietly dropping it**: the sticky removal pointer attributes exactly that case from
the audit path, from the fact the human's own delete request published, and it is pinned by a
two-actor corpus scenario. The argument was right about the *shape* of the problem and wrong about
its being unfixable on the audit path.

What still stands is the first argument, which is the one that never depended on the audit
semantics: **attribution needs an audit webhook, and a hosted control plane will not give you one.**
Two smaller populations survive as well, and neither is a finalizer race: an event the API server
never logs (a status-subresource update, a graceful pod delete) has no fact for any transport to
carry, and an aggregated-API create is logged with no name and no response body (Tier 3, below). The
`#220` shape — honored only against an admission record carrying an authorized verdict, fail-closed
independent of the webhook's `failurePolicy` — remains the right one, on the first argument alone.

**B4, #6, F10** as written in their source documents. **B1 has left the wave**: `suspend` already
stops a target writing without deleting it, so `mode` buys only the difference between a pause and a
declared permanent posture — a distinction in intent, not in behavior. The re-open trigger is in the wave
document. #6 is explicitly a lower priority than
when it was filed: the consumer downgraded it themselves, because branch and folder are now
chosen once per repository on an object that exists because the user picked that repository.
It rides the wave because it is in the wave, not because it is urgent.

### Tier 3: legibility

**#11: one encoder — SHIPPED, and the "cosmetic" label was wrong.** The consumer filed this as
cosmetic. For a product whose entire output is a Git diff, a create rendering list items at one
indentation and an update at another means every install rewrites all 19 `repositories` lines to
carry one changed field. That does not make the diff ugly; it makes it *unreadable*, which defeats
the mirror.

It was built the way this entry said to: a test pinning create and update byte-for-byte first, then
the fix wherever it failed. Three things that came out of doing it —

- **The direction was forced.** yaml.v3 always indents a sequence under its mapping key, so the
  create style (dashes at the key's own column, which is what a JSON→YAML round-trip emits) cannot
  be produced by the encoder that edits documents in place. Create moved to yaml.v3, not the
  reverse. It is also the style every already-updated file in a repository is in.
- **The fix is a package, not a constant.** Sharing an indent width would have left two encoders
  one refactor from diverging again, silently, because both produce valid YAML and nothing fails.
  `internal/yamlstyle` is the only place the write path constructs an encoder, and a test walking
  the source refuses another.
- **One behaviour difference had to be absorbed.** yaml.v3 panics with a bare string on a value it
  cannot marshal, and that panic escapes the library's own recover, where the JSON path returned an
  error. The shared encoder contains it, because a write path that returns an error retries and a
  write path that panics takes the process down.

The other `sigs.k8s.io/yaml` uses stay: they parse, or they serialize Go structs through their JSON
tags (the analyzer report, the in-memory kustomization copies handed to kustomize), and none of those
bytes reach a mirror. One style authority for Git output, not one library for the tree.

**B2: `status.layout`.** Highest value per line of code in the config-surface doc, and more so
after the inference deletion. `ambiguousDocuments` in particular is a correctness-relevant
fallback that is currently a debug-level store diagnostic.

It also carries the durable half of the inference deletion's notification question, argued above:
**the Event says a fall-back to canonical happened, `status.layout` says what the operator
understood.** An Event is deduplicated and expires; a status field is what someone reads a day later,
and it is the only one of the two a `kubectl get -o yaml` in a bug report will contain. So the two are
not alternatives, and B2 should carry the per-target record of which types resolved by declaration and
which fell back — which is why this is worth doing in the same change as the deletion rather than
after it.

**B6** as filed: one error message that will otherwise be the most likely first-run support ticket.
F9 has moved to Tier 1.

**The aggregated create: decide, and the decision is small either way.** The name tier reaches an
aggregated update, patch and single delete. It cannot reach a create: the `objectRef` carries no name
and there is no response body to recover one from, so nothing is published for any tier to join. Two
non-exclusive options, from [`../spec/attribution.md`](../spec/attribution.md) —
**accept it** (document that per-object attribution does not apply, and let it ship
committer-authored, which makes the guarantee type-dependent in a way a user cannot predict from the
API surface), or **stop paying for it** (recognize the shape at publish time and skip the grace, which
attributes nothing but stops waiting for evidence that cannot arrive). The second is the same
mechanism as Tier 1's F, so if F lands this is a case it should already cover.

**The stream's two capacity questions** as listed under Tier 2: an entry-size ceiling, and the
per-type stream count at a few hundred watched types. Both are "check before assuming", not known
defects.

### Declined, with the reason

- **#10 as asked**: superseded by the deletion. See above; we agree the behavior is wrong and
  disagree about the fix.
- **B3 (`spec.placement.mode`)**: superseded by the deletion. An off-switch for a feature we are
  removing is a permanent API field bought to solve a temporary problem.
- **`spec.expect.layout`**: unchanged from the config-surface doc: publish the observation (B2)
  before inventing the assertion.

---

## What this commits us to

1. A `feat(api)!` sequence for Tier 2, landed together, with one `docs/UPGRADING.md` entry — **and
   not in this release.** It is tracked as [#294](https://github.com/ConfigButler/gitops-reverser/issues/294)
   with the layout model as [#293](https://github.com/ConfigButler/gitops-reverser/issues/293). The
   commitment this sweep adds is the negative one: 0.41.0 ships the attribution model and the
   placement break, the wave waits, and no half of the wave is allowed to land on its own — because
   the reason to batch it was never only the consumer's bump, it was that four of the items are one
   decision and deciding it four times is how the object stops reading as one idea.
2. ~~A behavior change (inference removal) that needs its own UPGRADING entry and a decision on the
   fall-back-to-canonical Event.~~ **Done for the removal and the entry**; the Event is still
   undecided, and the metric now carries the actionable part in the meantime.
3. ~~Rewriting Option C's sections in
   [`../layout/new-file-placement-rules.md`](../layout/new-file-placement-rules.md).~~
   **Done**: the ladder is documented as three steps, the kustomize-root fallback keeps its section
   and gained the namespace-match rule, and P1–P10 are annotated one by one with which are retired by
   the deletion and which (P7, P9, P10) are facts about the code that remains.
4. Telling the gitops-api team which two of their asks we are answering differently, before they
   build against the shapes they proposed — and that **#23 is fixed**, that the fix has a name
   (`delete_sticky` on `attribution_resolutions_total{tier}`) they can assert on, and that the
   reproduction they offered was not needed because their own report matched a corpus scenario we
   could build.
5. Building #15's condition **once, on the stream**, with the trim-gap counter and the F circuit
   breaker feeding the same surface rather than three that say "attribution is not resolving".
   ~~Specifying it in transport-neutral terms before the stream work starts~~ — the stream landed
   first, which makes this cheaper rather than harder.
6. ~~Retiring §5 and §8 of the expander spec when the expander goes~~ — **done**: §5, §6 and §8 of
   the expander spec are gone with the spec itself, folded into
   [`../spec/attribution.md`](../spec/attribution.md). Its deletion-as-intent rule is kept as §1, which
   the collection join and the sticky pointer both depend on.
7. Deciding the removal-wait question (F, then C) with a measurement rather than by argument, since
   the one number on the table came from a single e2e run whose population is not a workload.
8. Keeping attribution documented in **one** place. Six design records described one part each while
   it was being built; they are folded into [`../spec/attribution.md`](../spec/attribution.md), which
   binds, and the only reasoning trail kept in full is
   [`attribution-fact-stream.md`](../finished/attribution-fact-stream.md). The commitment is that a
   change to attribution behaviour changes that spec, rather than adding a seventh record.
9. Not letting a decided-but-unbuilt list read as imminent. The placement work is filed as
   [#295](https://github.com/ConfigButler/gitops-reverser/issues/295) (correctness) and
   [#296](https://github.com/ConfigButler/gitops-reverser/issues/296) (visibility);
   [`placement-visibility-and-declared-defaults.md`](placement-visibility-and-declared-defaults.md)
   now says which of its eight items shipped, which is **none of them**.
