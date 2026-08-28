# The wave after placement left it

> **design**: a sequencing proposal, not a plan of record. Nothing here binds until scheduled.
> Index: [`../INDEX.md`](../INDEX.md)
> Date: 2026-08-28 (originally 2026-07-30).
>
> **What changed.** This document was written to sequence one breaking wave whose centrepiece was
> `spec.layout`. [`model.md`](model.md) has since reversed: the path template stays and gains two
> additive fields, so the placement work is not breaking at all. The wave loses its largest member
> and most of its urgency, and what is left is smaller and more honest about being a batch.
>
> What remains:
>
> - the API-surface work left over from the status and configuration-model review: `spec.suspend`,
>   `GitProvider.spec.interval` and the reconcile-request annotation; the `CommitRequest` lifecycle
>   hole; the
>   `meta.LocalObjectReference` reference-type nit; and the `TooManyStreams` cap and `default`
>   `ClusterProvider` message. That review has been retired into the documents that own its
>   findings, and this is where its unbuilt block landed;
> - the Tier 2 breaking items in [`open-asks-priority.md`](../design/open-asks-priority.md): **B4**,
>   **#5**, **#6**. **B1 (`spec.mode: Observe|Write`) has been dropped**, see below;
> - the breaking half of
>   [`source-scope-simplification.md`](../design/source-scope-simplification.md), which arrived
>   after this document was written and is the only member that makes the API **smaller**.
>
> It does not touch Tier 1 (the removal-wait decision, #15's condition). Those are not breaking and
> should not wait for this.

## Why still one wave

The consumer pins us three ways (image, Go module, `require` line), so each breaking release costs a
coordinated bump. That argues for batching. It was always the weaker half of the argument, and with
placement gone it is now most of what is left.

The stronger half survives in reduced form: **several of these items are the same design decision
seen from different angles**, and building them separately means deciding it several times,
inconsistently.

> **The folder is described on the GitTarget. The connection describes only the connection.**

- `spec.suspend` says whether we write to the folder.
- `commitWindow` and `commit.message` say how writes to it are batched and phrased, and today they
  live on `GitProvider`, which is the connection (B4, and the config-model review's "GitProvider is
  doing three jobs").
- `serializeNamespace` and `kustomizeRoot` say what the documents in it look like — **and these are
  additive**, so they can ship before, during or after the wave without anyone paying a bump.

The principle is unchanged; only its most expensive expression left. Shipping `commitWindow`'s move
alone would still assert half of it, which is the reason to keep the rest together.

## The interactions that change the design

These are the reasons to combine, as opposed to merely batch. Each one changes what gets built.

### 1. Adoption is a dry run, and `spec.suspend` is enough to give it

Adoption is the weakest point of any placement scheme: placement only ever affects *new* documents,
so there is nothing to preview by inspection. You find out where files go by letting one be written.

A suspended target plus `status.placement` is that preview. The operator scans, resolves the render
root, publishes it and what it *would* do, and writes nothing; clear `suspend` when the status says
what you expected. That turns "declare and hope" into a dry run, and it needs no second field.

**`spec.mode: Observe|Write` (B1) was here and has been dropped.** It bought one thing over a
suspended target: the difference between a temporary pause and a declared, permanent read-only
posture. That is a distinction in intent, not in behavior, and paying an enum plus its own status
semantics for it made the wave harder to hold in your head than the capability was worth. Two of
this document's open questions were about nothing else, and both close with the deletion.

The cost is stated rather than hidden: **`suspend` must keep observing.** Flux's `suspend` stops
reconciliation altogether; ours stops *writes* and keeps scanning, so the status a user is waiting
on stays fresh while they wait. That deviation belongs in the field's documentation, in one
sentence, because it is the only place we differ from a convention a Flux user brings with them.

**Re-open trigger**: someone who needs a target that can never write, as a property of the object
rather than a switch a colleague can flip. `mode` is the answer if that arrives; a permission is
not, and neither is a comment.

### 2. Dissolved: `GitTarget.spec.interval`

**Dropped. We watch, and a watch is already the freshness mechanism.**

The hole it was proposed for is real: the current half of `status.placement` is derived from the
last repository scan, a scan happens on a write or a resync, and a target that writes nothing could
carry a `renderRoot` stamped with last week's revision. A timer closes that. So does noticing that
nothing in this system is waiting on a timer in the first place.

Every input that can change what a scan would conclude arrives as an event, on a watch, in
milliseconds: a live object appears, a rule changes, a spec changes. Flux polls because **a Git
remote cannot be watched**, which is exactly why `GitProvider.spec.interval` stays and is the
honest one of the pair. `GitTarget` sits on the other side of that asymmetry: its inputs are API
objects, and we are already streaming them.

What remains uncovered is narrow enough to name in one sentence: a repository whose folder was
changed **by someone else** while our target wrote nothing. That is a poll of the output, we would
be paying it on every target forever to catch it, and the reconcile-request annotation refreshes it
on demand for the one person who wants it now. A stale `observedRevision` on an idle target is a
legible cost; a periodic scan on every target is not.

The inverted reading, a periodic **re-list of the API** so the mirror re-derives desired state from
the cluster rather than from the event stream, is the one that would actually correspond to Flux's
interval, because desired state lives in the cluster here. It is a real design with a real cost, and
nobody has asked for it. Not now.

**It is not a re-apply, and the name would have made people think it was.** In Flux, `interval` is
the drift-correction cadence: re-fetch the source, re-apply the desired state stored in Git. Had we
put the field on `GitTarget`, a Flux user would have read it as that, and it would have driven
neither of the two passes they were picturing:

| | reads | writes | decides deletions |
|---|---|---|---|
| the observation pass | this target's folder, to resolve the render root and stamp `observedRevision` | never | never |
| the resync mark-and-sweep | the API, via the streaming list's initial-events snapshot | yes | yes, where `spec.prune.mode` allows |

The sweep is enqueued from the watch plane with a cluster-gathered `desired` snapshot
([`event_router.go`](../../internal/watch/event_router.go),
[`reconcile-via-watchlist-mark-and-sweep.md`](../spec/reconcile-via-watchlist-mark-and-sweep.md)),
scoped per cell. Nothing about a Git-side scan is qualified to infer a deletion, because a document
whose object is gone and a document nobody has written yet look identical from that side. Dropping
the field removes the only place on `GitTarget` where that confusion had somewhere to attach.

**Re-open trigger**: a user who needs an idle target's `status.placement` to track a repository
other people edit, and for whom the reconcile-request annotation is not enough. Then it is a scan
cadence, named for scanning.

### 3. `spec.suspend` is a precondition for `kustomizeRoot: Create`

`kustomizeRoot: Create` writes a `kustomization.yaml` the user did not author. That is
the right behavior and it raises the stakes on the review's central complaint: *this controller
writes to a Git repository and there is no way to make it stop that is not deleting the object.*

So `suspend` is not a rider here, it is a precondition. A value that creates structure must ship
with the button that stops it. And `suspend` must stop bootstrap creation specifically, not only
resource writes, which is a detail worth stating before either is built.

### 4. The Events question is already answered, and the resolution is what to say

[`open-asks-priority.md`](../design/open-asks-priority.md) left one thing open about the inference deletion:
whether a fall-back to canonical should raise an Event on the GitTarget, and it reasoned that this
was expensive because placement runs on the branch worker with no recorder.

That is no longer true. **An `EventRecorder` shipped on every reconciler**
(review §6), and the roll-up seam projects data-plane facts into status with an enqueue on change. So
the Event is now: emit when `status.placement` changes in a way a human should know about, which
is the `LayoutResolved` reason becoming `Ambiguous`, or a type falling back for the first time. One Event per
persisted change, the pattern already established for `Ready`.

### 4b. The source-scope deletion is the only member that shrinks the API

[`source-scope-simplification.md`](../design/source-scope-simplification.md) decided four things,
and three of them are breaking:

| Change | Object | Kind |
|---|---|---|
| `spec.allowedSourceNamespaces` deleted, selector machinery with it | `GitTarget` | removal |
| `allowSourceNamespaceOverride` renamed `allowAnySourceNamespace` | `ClusterProvider` | pure rename |
| `allowedNamespaces` renamed `accessFrom` | `ClusterProvider` | pure rename |
| `sourceNamespace: "*"` becomes one cluster-wide list and watch | `WatchRule` semantics | redefinition |

It belongs in this wave for the ordinary reason first: **it removes a field from `GitTarget`**, and
B4 already breaks `GitTarget` in the same release. Shipping them apart costs the consumer two bumps
for one object.

The better reason is what it does to the review surface. Every other member of this wave adds a
field. This one deletes 4,569 lines and a whole three-valued verdict, and it deletes the only
cross-cluster read in the authorization path. A wave that is otherwise all addition is easier to
justify when the object it lands on comes out simpler than it went in, and easier to describe in
one `UPGRADING.md` entry.

Two interactions worth naming, because they change what gets built rather than merely when:

- **The `SelfSubjectAccessReview` pass is the same shape as the dry run.** Both answer "tell me what
  you would do before you do it": a suspended target plus `status.placement` for the Git side, the
  SAR pass for the source side. They are separate conditions on separate objects and neither depends on the
  other, so the SAR work stays additive and can ship after the wave. Whoever writes the second
  should read the first, because a user adopting a repository and a user asking which source cells
  are reachable are the same user on the same afternoon.
- **`TooManyStreams` is sized by the `*` decision**, below.

### 5. Dissolved: layout immutability

An earlier draft of this document put `spec.layout` with `prune` as a mutable field, then reversed
to immutable-with-a-widening-exception, on the argument that a mutable structural kind leaves a
folder permanently half one shape and half another.

There is no `spec.layout`, and `spec.placement` is mutable today. The condition the argument
described is real and stays: existing files never move, so a template change affects only what is
written afterwards, and a folder can hold documents placed under two different templates. That is
worth saying plainly rather than fixing, because match-first identity keeps finding those documents
and editing them in place. The CEL machinery — immutability plus a widening exception — was invented
to protect a discriminator that no longer exists.

The one fact worth keeping from that argument, because it will be reached for again: **`GitTarget`
has no finalizer**, so deleting one leaves the folder in Git untouched and re-creating it at the same
path re-adopts every document by identity. Recreating a target costs status and a moment of
mirroring, not data. That is what made immutability affordable for `path` and unaffordable for
`prune`, and it is the right test for any future field on this object.

### 6. Dissolved: the namespace agreement rule

The layout's `scope: SingleNamespace` and `namespace` were structural claims that had to be kept in
agreement with `spec.allowedSourceNamespaces` by an admission rule. Neither field exists now: a
folder is single-namespace because no `{namespace}` appears in its paths.

What survives is the observation underneath, which is why this section is not simply deleted. The
namespace-in-file question is inference today, and it is the one piece of inference an empty folder
cannot perform — there is no kustomization to inherit a convention from. That is exactly what
`serializeNamespace: Never` with `kustomizeRoot: Create` answers, and it needs no authorization
field to do it.

## The GitTarget after the wave

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: prod
  annotations:
    reconcile.configbutler.ai/requestedAt: "2026-07-30T09:14:22Z"
spec:
  # --- the connection: unchanged, and now only the connection ---
  providerRef:
    name: platform
  branch: main
  path: clusters/prod
  # allowedSourceNamespaces is gone: the provider's credential bounds what can be read,
  # and ClusterProvider.accessFrom bounds who may wield it.

  # --- what the documents in it look like: ADDITIVE, not part of the wave ---
  placement:                        # unchanged from today
    byType:
      v1/secrets: "secrets/{name}{sensitiveSuffix}"
  serializeNamespace: Never         # the created kustomization carries namespace:
  kustomizeRoot: Create

  # --- whether and when we write it ---
  suspend: false                    # the only stop-writes switch; a suspended target still scans
  prune:
    mode: OnEvent

  # --- how writes are batched and phrased: moved off the connection ---
  commitWindow: 5s                  # B4, was GitProvider.spec.push.commitWindow
  commit:
    message:
      template: "chore(mirror): {{ .Summary }}"   # B4, was GitProvider.spec.commit.message
status:
  observedGeneration: 4
  conditions:
    - type: LayoutResolved
      status: "True"
      reason: SingleKustomization
      message: "render root '.' governs new files"
      observedGeneration: 4
  placement:
    renderRoot: .
    serializeNamespace: Never
    observedRevision: 9f3c1ab
  lastHandledReconcileAt: "2026-07-30T09:14:22Z"
```

The two placement fields are shown in place so the object reads as a whole, but they are marked
additive for a reason: they carry defaults equal to today's behavior, so they can ship in any
release without a bump. Only the `commitWindow` and `commit.message` move, and the riders below,
make this release breaking.

## What rides along without a claim of synergy

Honesty matters more than a tidy narrative. These are in the wave because they are breaking and the
consumer should pay once, not because they interact with anything else here:

- **#5, `CommitRequest.spec.author`, SAR-guarded.** Independent, and it stands on the argument that
  attribution needs an audit webhook a hosted control plane will not give you.
- **The `CommitRequest` lifecycle hole** (`ttlSecondsAfterFinished` or an `ownerReference`, plus the
  `delete` verb). Unrelated to placement; it is the other object in the API with a lifecycle hole.
- **The reference types**: embedding `meta.LocalObjectReference` for the name half of our six
  near-identical reference shapes. If GitTarget is breaking anyway, this is the moment.
- **The `TooManyStreams` cap**, now smaller than it was.
  [`source-scope-simplification.md`](../design/source-scope-simplification.md) has decided that
  `sourceNamespace: "*"` compiles to **one cluster-wide list and watch** rather than one stream per
  admitted namespace, which removes the fan-out this cap was queued for. What is left to bound is
  explicit enumeration (a rule naming many namespaces, or many rules on one target), so the cap is
  still worth a `Stalled` reason plus a bound rather than discovering the cliff as apiserver watch
  pressure, but it is no longer guarding the case that produced it. Size it against enumerated
  rules, and do not let it be planned before the `*` change lands, or it will be sized against a
  fan-out that no longer exists.
- **The `ClusterProvider` "default" message.** One error string, and the most likely first-run
  support ticket.

### The envtest that has to run before any of this is planned

One question is not in the wave at all, and its answer constrains the enum work, so it should be
settled before anyone plans an API change around it.

`ClusterWatchRule`'s `rules[].scope` enum is narrowed to `Cluster` only, deliberately keeping the
field so that re-applying a stored `Namespaced` value **fails**
([`clusterwatchrule_types.go`](../../api/v1alpha3/clusterwatchrule_types.go)). The concern is what
that does to the object's own ability to explain itself. For CRDs the apiserver validates the
**whole object** against the OpenAPI schema on **status-subresource** updates too, not only on spec
updates. If that holds here, the controller cannot write `Stalled=True` onto an object whose stored
`spec.rules[].scope` is `Namespaced` — the status update is rejected 422, and the one object that
most needs to explain itself is the one that cannot.

The mitigating factor is CRD Validation Ratcheting, which skips re-validation of *unchanged* fields
and is beta and default-on from 1.30, GA in 1.33. So the exposure is older clusters, or a cluster
with the feature gate off — which is exactly why the test has to name a version.

**The test.** Create a `ClusterWatchRule` with `scope: Namespaced` through a client that bypasses
the enum (or against an older CRD), then attempt a status update, on the minimum supported
Kubernetes version.

**The fallback if it fails.** Widen the enum back and rely on the compile-path refusal plus a loud
`Stalled` condition. Refusing at admission is nice, but not at the cost of being unable to report
the refusal.

## Version strategy: stay `v1alpha3`

A wave this size invites `v1alpha4`, and I would not take it.

- A new version means a **conversion path**, and the honest options are a conversion webhook (a
  serving dependency for the CRD, plus a cert lifecycle) or `None` conversion with a stored-version
  migration. Both cost more than the problem.
- We are `v1alpha3` and pre-1.0 with **one consumer**. The convention the repo already uses is a
  **loud rejection**: keep the removed field in the schema, refuse it with a message naming the
  replacement, for one release. `ClusterWatchRule.spec.rules[].scope` set that precedent, and the
  reasoning holds better here: refusing a stored field the user can see beats translating it behind
  their back.
- The pattern's members are now the source-scope changes rather than `spec.placement`, which no
  longer moves at all. `allowedSourceNamespaces` becomes a rejection naming what replaced it (the
  provider's own credential, plus `allowAnySourceNamespace` for the cluster-wide case), and the two
  `ClusterProvider` renames are mechanical enough that the message can name the new field and
  nothing else. A rename is exactly the case loud rejection is kindest on: the user's next `apply`
  tells them the new spelling.

If a second consumer appears before this ships, revisit: the calculus that makes loud rejection cheap
is one coordinated bump.

## Order inside the wave

Dependencies first, then the things that only need the object to be breaking.

1. **The `scope: Namespaced` envtest**, above. Not an API change; its answer constrains the enum
   work. Do it before planning.
2. **`spec.suspend`**. Precondition for anything that creates files, and independently the
   review's highest-value gap.
3. **`status.placement`** plus the post-scan validation pass. Needed before a suspended target is
   useful to look at, because a dry run with nothing to read previews nothing.
4. **`requestedAt` + `lastHandledReconcileAt`**. On-demand refresh of the status in step 3, and the
   one reflex a Flux user brings that we do keep. `GitTarget.spec.interval` was step 4 and is
   dropped, per interaction 2.
5. **Events on a changed resolution**, over the existing recorder.
6. **B4**: `commitWindow` and `commit.message` move from `GitProvider` to `GitTarget`. Last of the
   principle items, and the one that makes the object coherent.
7. **The source-scope deletion**: `allowedSourceNamespaces` removed, the two `ClusterProvider`
   renames, and `*` redefined as cluster-wide. Independent of every step above, so it can be written
   in parallel; it is placed here because it is the one member whose review is a deletion, and a
   deletion reviews better once the additions it is not entangled with are settled.
8. **The riders**: #5, the `CommitRequest` lifecycle, the reference types, `TooManyStreams`, the
   `default` ClusterProvider message. `TooManyStreams` must come after step 7, which removes the
   fan-out it was written to bound.

**`serializeNamespace` and `kustomizeRoot` are deliberately absent from this list.** They are
additive, so they do not belong in a breaking wave's ordering at all; their order is
[`implementation-plan.md`](implementation-plan.md)'s, and it puts them after `status.placement`
because the post-scan pass is what makes them honest.

Steps 3 to 7 are one release. Step 1 gates the planning. Step 8 can be trimmed if the wave gets too
big to review, since nothing else depends on it.

## What this costs, stated plainly

- **One coordinated consumer bump**, for a much smaller set than this document originally proposed:
  `commitWindow` and `commit.message` change object, the riders change shape, and everything else
  here is additive.
- **One `docs/UPGRADING.md` entry** covering the two field moves, the source-scope removal and
  renames, the `*` redefinition, and the riders. The placement work needs no migration entry at all,
  which is the largest single change to this document. The `*` paragraph is the one that has to be
  written carefully: it is the only change here that keeps its spelling and changes its meaning.
- **A capability genuinely lost**: source-side label selectors, priced in the source-scope document.
  It is the only thing in this wave a user can do today and cannot do afterwards.
- **A review surface that is now ordinary.** The argument for trimming the riders first still holds,
  but the wave is no longer the largest change this project has taken.

## Open questions

- ~~**Does `mode: Observe` write status only, or also refuse admission of new WatchRules?**~~
  ~~**Should `suspend` and `mode: Observe` be one field?**~~ **Both closed by dropping `mode`**: one
  field, and a suspended target keeps observing. See interaction 1.
- ~~**Where does `interval` live?**~~ **Decided: only on `GitProvider`.** Two objects with a
  reconcile cadence would have been the Flux convention rather than a collision, and the name was
  never the problem. The problem was that `GitTarget` did not need the field: a Git remote cannot be
  watched and an API object can, so the poll belongs on the side that polls. Interaction 2.
