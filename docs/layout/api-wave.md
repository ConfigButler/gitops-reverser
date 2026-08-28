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
>   `spec.interval` and the reconcile-request annotation; the `CommitRequest` lifecycle hole; the
>   `meta.LocalObjectReference` reference-type nit; and the `TooManyStreams` cap and `default`
>   `ClusterProvider` message. That review has been retired into the documents that own its
>   findings, and this is where its unbuilt block landed;
> - the Tier 2 breaking items in [`open-asks-priority.md`](../design/open-asks-priority.md): **B4**,
>   **B1**, **#5**, **#6**.
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

- `spec.mode: Observe|Write` says whether we write to the folder at all (B1).
- `spec.suspend` says whether we write to it *now*.
- `commitWindow` and `commit.message` say how writes to it are batched and phrased, and today they
  live on `GitProvider`, which is the connection (B4, and the config-model review's "GitProvider is
  doing three jobs").
- `serializeNamespace` and `kustomizeRoot` say what the documents in it look like — **and these are
  additive**, so they can ship before, during or after the wave without anyone paying a bump.

The principle is unchanged; only its most expensive expression left. Shipping `commitWindow`'s move
alone would still assert half of it, which is the reason to keep the rest together.

## The interactions that change the design

These are the reasons to combine, as opposed to merely batch. Each one changes what gets built.

### 1. `spec.mode: Observe` is how a folder is adopted safely

Adoption is the weakest point of any placement scheme: placement only ever affects *new* documents,
so there is nothing to preview by inspection — you find out where files go by letting one be
written.

`Observe` mode plus `status.placement` is that preview. In `Observe` the operator scans, resolves the
render root, publishes it and what it *would* do, and writes nothing. Flip to `Write`
when the status says what you expected. That turns "declare and hope" into a dry run, and
it costs nothing extra because both halves are already in the wave.

This also gives `Observe` a purpose beyond "a safety switch nobody uses". It is the mode you adopt a
repository in.

### 2. `spec.interval` is what keeps the observation fresh

The placement status has two halves ([the other document](placement-visibility-and-declared-defaults.md)
records why): a **current** half derived from the last repository scan, and a **historical** half
accumulated since. The current half is the useful one, and it has a hole: a repository scan happens
on a write or a resync, so a stable target that writes nothing may not scan for a long time, and the
field a user consults would be stamped with a revision from last week.

`spec.interval` plus `Observe`'s scan-without-writing is the mechanism that closes it: a
periodic observation pass refreshes `renderRoot` and `observedRevision` whether or not anything was
written. Neither piece was proposed for this reason, and together they answer a question neither
answers alone.

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

  # --- what the documents in it look like: ADDITIVE, not part of the wave ---
  placement:                        # unchanged from today
    byType:
      v1/secrets: "secrets/{name}{sensitiveSuffix}"
  serializeNamespace: Never         # the created kustomization carries namespace:
  kustomizeRoot: Create

  # --- whether and when we write it ---
  mode: Write                       # B1: Observe | Write
  suspend: false
  interval: 5m                      # what keeps status.placement fresh
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
- **The `TooManyStreams` cap** for `sourceNamespace: "*"` fan-out. A `Stalled` reason plus a bound,
  rather than discovering the cliff as apiserver watch pressure.
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
- `spec.placement` therefore becomes a rejection that says "use `spec.layout`; `byType` moves
  verbatim, `default` becomes `layout.kind: Template`". The mapping is mechanical, which is what makes
  the rejection kind rather than merely strict.

If a second consumer appears before this ships, revisit: the calculus that makes loud rejection cheap
is one coordinated bump.

## Order inside the wave

Dependencies first, then the things that only need the object to be breaking.

1. **The `scope: Namespaced` envtest**, above. Not an API change; its answer constrains the enum
   work. Do it before planning.
2. **`spec.suspend`**. Precondition for anything that creates files, and independently the
   review's highest-value gap.
3. **`status.placement`** plus the post-scan validation pass. Needed before `Observe` is useful,
   because `Observe` with nothing to read is a mode that does nothing.
4. **`spec.mode: Observe|Write`** (B1). Now an adoption path rather than a switch.
5. **`spec.interval` + `requestedAt` + `lastHandledReconcileAt`**. Closes the freshness
   hole in step 3 and gives the object the reflexes a Flux user already has.
6. **Events on a changed resolution**, over the existing recorder.
7. **B4**: `commitWindow` and `commit.message` move from `GitProvider` to `GitTarget`. Last of the
   principle items, and the one that makes the object coherent.
8. **The riders**: #5, the `CommitRequest` lifecycle, the reference types, `TooManyStreams`, the
   `default` ClusterProvider message.

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
- **One `docs/UPGRADING.md` entry** covering the two field moves and the riders. The placement work
  needs no migration entry at all, which is the largest single change to this document.
- **A review surface that is now ordinary.** The argument for trimming the riders first still holds,
  but the wave is no longer the largest change this project has taken.

## Open questions

- **Does `mode: Observe` write status only, or also refuse admission of new WatchRules?** Observe
  should be silent about everything except what it observed, but a user in Observe mode with rules
  piling up may expect to be told nothing will happen.
- **Should `suspend` and `mode: Observe` be one field?** They are close: both stop writes. They differ
  in intent (temporary versus declared) and in what they do to status, and Flux keeps `suspend`
  separate from everything else. Two fields, but the field docs must each say what the other is for.
- ~~**Where does `interval` live?**~~ **Decided: both objects, one name.** `spec.interval` appears on
  `GitRepository`, `OCIRepository`, `HelmRepository`, `Bucket`, `Kustomization`, `HelmRelease`,
  `ImageRepository` and `Receiver`, meaning the same thing on each — how often this object
  reconciles. Two objects having a reconcile cadence is the convention, not a collision, and
  `GitProvider.spec.interval` is the one that matches Flux most exactly since it really is an
  `ls-remote` cadence. Each field's documentation says what it drives; `GitTarget`'s is where the
  meaning is novel, and that is a doc string rather than a name.
