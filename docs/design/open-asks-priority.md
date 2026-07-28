# What to build next: the open asks, ordered

> **design**: a priority call, not a plan of record. Nothing here binds until scheduled.
> Index: [`../INDEX.md`](../INDEX.md)
> Date: 2026-07-28. Written against `v0.40.1` / `main`.
>
> Three backlogs are open at once and they overlap: the gitops-api consumer asks (revision 11,
> 2026-07-28), the maintainer review's unbuilt block in
> [`flux-maintainer-review-status-and-config-model.md`](../future/flux-maintainer-review-status-and-config-model.md)
> (F6, F9, F10), and the config-surface proposal in
> [`config-surface-for-a-structured-repository.md`](../future/config-surface-for-a-structured-repository.md)
> (B1–B6). This page merges them into one queue and says where we deliberately do **not** do
> what was asked.
>
> One of the queue's entries is already specified rather than merely wanted:
> [`attribution-fact-stream.md`](../finished/attribution-fact-stream.md) picks and details the replacement for
> the attribution keyspace. It is ranked here like everything else, and it changes how the
> highest-priority consumer ask should be built. See
> [attribution facts as a stream](#attribution-facts-as-a-stream-tier-2-and-it-answers-15s-hard-part).

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
on sibling inference. It has a second instance, arrived at independently:
[`attribution-fact-stream.md`](../finished/attribution-fact-stream.md) deletes the fact keyspace and the
`deletecollection` expander rather than optimizing either, and ends up with less code doing more.
Two of these in one quarter is a pattern worth naming: the parts of this system that hurt are the
parts that reconstruct something from state they do not own.

---

## The one real design call: delete sibling inference, do not switch it off

The config-surface proposal's **B3** offers `spec.placement.mode: Infer|Declared|Strict`: an
enum that lets a user turn inference off. This document argues the opposite: **remove Option C's
cohort ladder entirely**, keep the kustomize-root fallback, and ship no enum at all.

### What inference is, precisely

[`gittarget-new-file-placement-rules.md`](../spec/gittarget-new-file-placement-rules.md) Option C.
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

**Open, and worth deciding before writing code:** whether the deletion lands with a
`PlacementFellBackToCanonical`-style Event on the first new type per target, so the user learns
they need a `byType` line at the moment it matters rather than by noticing a file.

---

## The queue

| # | Ask | Source | Tier |
|---|---|---|---|
| 22 | `ReasonRefusedStructural` doc says "permanent"; refusal-detail stem; `Actor` reachability | gitops-api | **0** |
| 15 | A declared `auditRoute` with zero facts must say so | gitops-api | **1** |
| n/a | Delete sibling inference (answers #10) | this doc | **1** |
| n/a | Attribution facts become a stream; the keyspace and the expander are deleted | [`attribution-fact-stream.md`](../finished/attribution-fact-stream.md) | **2** |
| F6 | `spec.suspend`, `spec.interval`, `requestedAt` | maintainer review | **2** |
| 5 | `CommitRequest.spec.author`, SAR-guarded | gitops-api (#220) | **2** |
| B4 | `commitWindow` / `commit.message` move to GitTarget | config surface | **2** |
| B1 | `GitTarget.spec.mode: Observe\|Write` | config surface | **2** |
| 6 | Movable destination via `status.observedDestination` | gitops-api (#220) | **2** |
| F10 | CommitRequest TTL / ownerRef + the `delete` verb | maintainer review | **2** |
| 11 | One encoder for `[CREATE]` and `[UPDATE]` bodies | gitops-api | **3** |
| B2 | `GitTarget.status.layout` | config surface | **3** |
| F9 | The `scope: Namespaced` status-write envtest | maintainer review | **3** |
| B6 | The `default` ClusterProvider not-found message | config surface | **3** |
| 10 | Namespace-aware sibling inference *as asked* | gitops-api | **declined** |
| B3 | `spec.placement.mode` enum | config surface | **declined** |

### Tier 0: correct what is false, this week

**#22 is three separate things and all three claims check out against `main` today.** They are
worth doing immediately because each one is a sentence and each one is currently misleading a
consumer that reads our source as the contract.

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
- **The `Actor` question, answered: yes, it is structural.** `AcceptancePolicy.InScope` is
  assigned in exactly two places in the tree, both tests. `folderScanPolicy` leaves it nil and
  `ScanRepo` never sets it, and `mappingRefusal`'s `IssueOutOfScope` is gated on it being
  non-nil, so neither `ScanFolder` nor `ScanRepo` can emit `ActorPlatformOperator`. A consumer's
  platform-operator branch is dead code today. The right fix is not only the doc line they asked
  for: `Actor`'s doc should state which scan kinds can produce which values, so the guarantee is
  written where it is read rather than reconstructed from a nil check three files away.

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

**Do not wait for the stream work to build it, and do not build it twice.** The obvious trap is to
defer #15 until the transport changes, because the stream design makes the signal so much easier
to produce. The right move is the opposite: ship the condition now, and define it in terms that
both transports can answer, which is "how many facts has this route contributed, and when was the
last one". Today that is a counter incremented where
`RecordFact` writes; after the change it is the same
counter incremented where the receiver appends. The condition never learns which transport it has,
which is the same seam rule the stream design argues for one level down.

Where `auditRoute` came from is [`attribution-fact-identity.md`](attribution-fact-identity.md).
The related open question about *how the watch waits* is no longer open in the way it was: the six
options in [`attribution-wait-poll-vs-push.md`](attribution-wait-poll-vs-push.md) are superseded by
[`attribution-fact-stream.md`](../finished/attribution-fact-stream.md), which picks one and specifies it.

**The inference deletion** sits in this tier for the reason argued above: repo state changing
operator behavior invisibly is the same class of defect as an audit route that silently resolves
nothing.

### Tier 2: the breaking wave, all at once, while `v1alpha3`

These all add or change a spec field. Doing them as one `feat(api)!` sequence costs the consumer
one coordinated bump; doing them one at a time costs six.

#### Attribution facts as a stream: Tier 2, and it answers #15's hard part

[`attribution-fact-stream.md`](../finished/attribution-fact-stream.md) is the only item in this queue that
arrives already specified, and it is the largest. It is in Tier 2 rather than Tier 1 for an honest
reason: today's keyspace is *slow*, not *wrong*. The poll loop runs to completion on essentially
every attributable event, which is waste, and waste does not outrank a silent misconfiguration.

What lifts it above the rest of Tier 2 is that it retires three things at once instead of adding a
fourth. The per-key `SET`/`GET` and the poll loop go; the `deletecollection` expander, which
rebuilds N per-object facts by parsing a response body that truncation removes exactly when the
collection is large, goes with them; and a collection delete becomes one fact that removals join by
scope, which resolves the aggregated-API and truncated cases that degrade to committer-authored
today. That is test 3 again, and it is why this ranks above the spec-field work rather than beside
it.

Three things it settles that this queue had left open:

- **#15's signal gets cheaper and more honest.** A per-`(route, group/resource)` stream makes "this
  route has contributed nothing" a directly observable fact rather than a statistic we choose to
  keep. The design already proposes a trim-gap counter and asks, in its own open questions, whether
  that counter should feed a condition. It should, and it should feed *the same* condition #15
  creates: a route that is losing facts and a route that never had any are the same user-visible
  failure (commits authored `unknown`), and they should not be two unrelated surfaces.
- **The circuit breaker keeps its separate justification.** The stream design is explicit that a
  status subresource update and a graceful pod delete produce no audit event at all, so no
  transport can name their author. That population is why "a route that has never resolved
  anything" is a distinct signal from "this event did not resolve", and it is what #15 is really
  asking us to expose.
- **It is a prerequisite for HA, not a detour from it.** Under multiple replicas the audit POST and
  the watch shard land on different replicas by construction. A per-type stream with independent
  cursors is the primitive for that; an in-process channel would work today and have to be thrown
  away on the second replica. The ownership problem in
  [`ha-gittarget-distribution-plan.md`](../future/ha-gittarget-distribution-plan.md) is untouched
  by it, and remains the real blocker.

The sequencing that follows: **#15 first** (small, and specified above so it survives the
transport change), **then the stream work**, and the trim-gap counter joins #15's condition when it
lands rather than arriving as a second one.

The one thing to decide before code, beyond that document's own open questions: whether the
in-memory transport ships in the first cut at all. It is argued well, and the conformance-suite
condition is right, but it is a second implementation of the piece that carries attribution
correctness, in service of an install shape (single pod, attribution on, no Redis) we have not been
asked for. Shipping the seam and one implementation is the smaller first commit.

**F6: `spec.suspend` first.** The maintainer review's bottom line stands: this controller writes
to a Git repository and there is no way to make it stop that is not deleting the object.
`spec.interval` on GitProvider (a real `ls-remote` per pass, hardcoded at 5 min, no jitter) and
the `requestedAt` annotation ride along.

**#5: `CommitRequest.spec.author`, SAR-guarded.** Two arguments, and the second is the one that
cannot be worked around: attribution needs an audit webhook, which a hosted control plane will
not give you; and *audit cannot attribute a finalized delete at all*: the human's delete is
recorded as an update setting `deletionTimestamp`, and the real delete names whichever controller
cleared the last finalizer. Most operator-managed CRs have finalizers. No audit stream carries
that answer, so no amount of work on the audit path fixes it. The `#220` shape (honored only
against an admission record carrying an authorized verdict, fail-closed independent of the
webhook's `failurePolicy`) is the right one.

**B4, B1, #6, F10** as written in their source documents. #6 is explicitly a lower priority than
when it was filed: the consumer downgraded it themselves, because branch and folder are now
chosen once per repository on an object that exists because the user picked that repository.
It rides the wave because it is in the wave, not because it is urgent.

### Tier 3: legibility

**#11: one encoder, and we should not accept the "cosmetic" label.** The consumer filed this as
cosmetic. For a product whose entire output is a Git diff, a create that renders list items at
2-space indent and an update that renders them at 4 means every install rewrites all 19
`repositories` lines to carry one changed field. That does not make the diff ugly; it makes it
*unreadable*, which defeats the mirror. The two paths reach different encoders: `contract.go`
and `render.go` go through JSON→YAML, `manifestedit/patch.go` uses `yaml.v3` with its own indent,
which is the shape that produces it. Unverified since v0.35.0, so step one is a test that pins
create and update output byte-for-byte against each other; the fix follows from wherever that
fails.

**B2: `status.layout`.** Highest value per line of code in the config-surface doc, and more so
after the inference deletion. `ambiguousDocuments` in particular is a correctness-relevant
fallback that is currently a debug-level store diagnostic.

**F9, B6** as filed. F9 is one envtest; B6 is one error message that will otherwise be the most
likely first-run support ticket.

### Declined, with the reason

- **#10 as asked**: superseded by the deletion. See above; we agree the behavior is wrong and
  disagree about the fix.
- **B3 (`spec.placement.mode`)**: superseded by the deletion. An off-switch for a feature we are
  removing is a permanent API field bought to solve a temporary problem.
- **`spec.expect.layout`**: unchanged from the config-surface doc: publish the observation (B2)
  before inventing the assertion.

---

## What this commits us to

1. A `feat(api)!` sequence for Tier 2, landed together, with one `docs/UPGRADING.md` entry.
2. A behavior change (inference removal) that needs its own UPGRADING entry and a decision on the
   fall-back-to-canonical Event.
3. Rewriting Option C's sections in
   [`gittarget-new-file-placement-rules.md`](../spec/gittarget-new-file-placement-rules.md). That
   document binds the code, so the ladder cannot be deleted from one and left in the other. The
   kustomize-root fallback keeps its section; P1–P10 become history rather than live risks.
4. Telling the gitops-api team which two of their asks we are answering differently, before they
   build against the shapes they proposed.
5. Specifying #15's condition in transport-neutral terms *before* the stream work starts, so it is
   not built twice, and folding the stream design's trim-gap counter into that same condition
   rather than adding a second surface.
6. Retiring §5 and §8 of
   [`deletecollection-attribution-expander.md`](../spec/deletecollection-attribution-expander.md)
   when the expander goes, and keeping §2's deletion-as-intent rule, which the collection join
   depends on. That spec binds the code, like the placement one.
