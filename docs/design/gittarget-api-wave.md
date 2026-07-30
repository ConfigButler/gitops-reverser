# One breaking wave: the folder is described on the GitTarget

> **design**: a sequencing proposal, not a plan of record. Nothing here binds until scheduled.
> Index: [`../INDEX.md`](../INDEX.md)
> Date: 2026-07-30.
>
> Combines three pieces of work that are all `feat(api)!` on `GitTarget` and are cheaper together
> than apart:
>
> - the layout model, [`gittarget-layout-model.md`](gittarget-layout-model.md);
> - the still-open API-surface block of the maintainer review,
>   [`flux-maintainer-review-status-and-config-model.md`](../future/flux-maintainer-review-status-and-config-model.md)
>   §4 "Then": **F6**, **F9**, **F10**, plus F12's reference-type nit and §3's pushbacks;
> - the Tier 2 breaking items in [`open-asks-priority.md`](open-asks-priority.md): **B4**, **B1**,
>   **#5**, **#6**.
>
> It does not touch Tier 1 (the removal-wait decision, #15's condition). Those are not breaking and
> should not wait for this.

## Why one wave

The consumer pins us three ways (image, Go module, `require` line), so each breaking release costs a
coordinated bump. That argues for batching. It is the weaker half of the argument.

The stronger half: **four of these items are the same design decision seen from different angles**,
and building them separately means deciding it four times, inconsistently.

> **The folder is described on the GitTarget. The connection describes only the connection.**

- `spec.layout` says what the folder **is** (layout model).
- `spec.mode: Observe|Write` says whether we write to it at all (B1).
- `spec.suspend` says whether we write to it *now* (F6).
- `commitWindow` and `commit.message` say how writes to it are batched and phrased, and today they
  live on `GitProvider`, which is the connection (B4, and §3's "GitProvider is doing three jobs").

Ship `spec.layout` alone and the principle is asserted by one field while `commitWindow` still
contradicts it. Ship them together and the object reads as one idea: a GitTarget is a folder plus a
policy for writing it, and a GitProvider is how you reach the repository.

## The interactions that change the design

These are the reasons to combine, as opposed to merely batch. Each one changes what gets built.

### 1. `spec.mode: Observe` is how a layout is adopted safely

The layout model's weakest point is adoption: a user pointing `kind: Kustomize` at a real repository
has to trust it before any file moves, and placement only ever affects *new* documents, so there is
nothing to preview by inspection.

`Observe` mode plus `status.layout` is that preview. In `Observe` the operator scans, resolves the
layout, publishes `renderRoot`, `kind`, and what it *would* do, and writes nothing. Flip to `Write`
when the status says what you expected. That turns "declare a layout and hope" into a dry run, and
it costs nothing extra because both halves are already in the wave.

This also gives `Observe` a purpose beyond "a safety switch nobody uses". It is the mode you adopt a
repository in.

### 2. `spec.interval` is what keeps the layout observation fresh

The layout status has two halves ([the other document](placement-visibility-and-declared-defaults.md)
records why): a **current** half derived from the last repository scan, and a **historical** half
accumulated since. The current half is the useful one, and it has a hole: a repository scan happens
on a write or a resync, so a stable target that writes nothing may not scan for a long time, and the
field a user consults would be stamped with a revision from last week.

`spec.interval` (F6) plus `Observe`'s scan-without-writing is the mechanism that closes it: a
periodic observation pass refreshes `renderRoot` and `observedRevision` whether or not anything was
written. Neither piece was proposed for this reason, and together they answer a question neither
answers alone.

### 3. `spec.suspend` is a precondition for a layout that creates files

`kind: Kustomize` with `create: true` writes a `kustomization.yaml` the user did not author. That is
the right behavior and it raises the stakes on the review's central complaint (F6): *this controller
writes to a Git repository and there is no way to make it stop that is not deleting the object.*

So `suspend` is not a rider here, it is a precondition. A layout that creates structure must ship
with the button that stops it. And `suspend` must stop bootstrap creation specifically, not only
resource writes, which is a detail worth stating before either is built.

### 4. The Events question is already answered, and the layout is what to say

[`open-asks-priority.md`](open-asks-priority.md) left one thing open about the inference deletion:
whether a fall-back to canonical should raise an Event on the GitTarget, and it reasoned that this
was expensive because placement runs on the branch worker with no recorder.

That is no longer true. **F7 shipped an `EventRecorder` on every reconciler**
(review §6), and the roll-up seam projects data-plane facts into status with an enqueue on change. So
the Event is now: emit when `status.layout` changes in a way a human should know about, which is
`renderRootReason` becoming `Ambiguous`, or a type falling back for the first time. One Event per
persisted change, the pattern F7 already established for `Ready`.

### 5. Layout is mutable, and that has to be stated beside the immutable fields

`providerRef`, `branch`, `path` and `clusterProviderRef` are immutable because a folder's meaning is
constituted by them (review §3 defends this well). `spec.prune` is deliberately mutable because
freezing it would destroy the one thing that cannot be rebuilt.

`spec.layout` belongs with `prune`: mutable, because it only affects documents that do not exist yet.
Existing files never move, so changing `kind` cannot break the mirror; it changes where the *next*
new document goes. That needs to be in the field doc, and status needs to make the consequence
visible, since a folder can legitimately hold files placed under two layouts. `observedRevision`
already carries "when", and it is worth recording which layout generation produced the historical
half.

This also connects to **#6** (a movable destination via `status.observedDestination`): if `path` ever
becomes movable, the layout has to move with it, because a new folder may have a different structure.
Deciding layout mutability now keeps #6 from having to reopen it.

## The GitTarget after the wave

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: prod
  annotations:
    reconcile.configbutler.ai/requestedAt: "2026-07-30T09:14:22Z"   # F6
spec:
  # --- the connection: unchanged, and now only the connection ---
  providerRef:
    name: platform
  branch: main
  path: clusters/prod

  # --- what the folder is ---
  layout:
    kind: Kustomize                 # Auto | Kustomize | Tree | Flat | Template
    kustomize:
      create: true
    byType:
      v1/secrets: "secrets/{name}{sensitiveSuffix}"

  # --- whether and when we write it ---
  mode: Write                       # B1: Observe | Write
  suspend: false                    # F6
  interval: 5m                      # F6, and what keeps status.layout fresh
  prune:
    mode: OnEvent

  # --- how writes are batched and phrased: moved off the connection ---
  commitWindow: 5s                  # B4, was GitProvider.spec.push.commitWindow
  commit:
    message:
      template: "chore(mirror): {{ .Summary }}"   # B4, was GitProvider.spec.commit.message
status:
  layout:
    declaredKind: Kustomize
    kind: Kustomize
    renderRoot: .
    renderRootReason: SingleKustomization
    observedRevision: 9f3c1ab
    observedTime: "2026-07-30T09:14:22Z"
    placedResources: 14
    refusedResources: 0
  lastHandledReconcileAt: "2026-07-30T09:14:22Z"                    # F6
  observedDestination:                                              # 6
    branch: main
    path: clusters/prod
```

Read top to bottom it is one story: reach this repository, this is what the folder is, this is whether
and when we write it, this is how the writes look.

## What rides along without a claim of synergy

Honesty matters more than a tidy narrative. These are in the wave because they are breaking and the
consumer should pay once, not because they interact with the layout:

- **#5, `CommitRequest.spec.author`, SAR-guarded.** Independent, and it stands on the argument that
  attribution needs an audit webhook a hosted control plane will not give you.
- **F10, CommitRequest lifecycle** (`ttlSecondsAfterFinished` or an `ownerReference`, plus the
  `delete` verb). Unrelated to placement; it is the other object in the API with a lifecycle hole.
- **F12's reference types**: embedding `meta.LocalObjectReference` for the name half of our six
  near-identical reference shapes. If GitTarget is breaking anyway, this is the moment.
- **§3's `TooManyStreams` cap** for `sourceNamespace: "*"` fan-out. A `Stalled` reason plus a bound,
  rather than discovering the cliff as apiserver watch pressure.
- **§3's `ClusterProvider` "default" message.** One error string, and the most likely first-run
  support ticket.

**F9** is not in the wave at all: it is one envtest against the minimum supported Kubernetes version,
and its outcome (widen the enum, or keep it) should be known *before* anyone plans an API change
around it.

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

1. **F9's envtest.** Not an API change; its answer constrains the enum work. Do it before planning.
2. **`spec.suspend`** (F6). Precondition for anything that creates files, and independently the
   review's highest-value gap.
3. **`spec.layout`** with `byType`, plus rule 1 (every written file is registered with the
   kustomization that governs it) and rule 2 (a structural kind excludes a blanket `default`).
   `spec.placement` becomes a loud rejection.
4. **`status.layout`**, current half from the scan, historical half from the roll-up. Needed before
   `Observe` is useful, because `Observe` with nothing to read is a mode that does nothing.
5. **`spec.mode: Observe|Write`** (B1). Now an adoption path rather than a switch.
6. **`spec.interval` + `requestedAt` + `lastHandledReconcileAt`** (F6, rest). Closes the freshness
   hole in step 4 and gives the object the reflexes a Flux user already has.
7. **Events on layout change**, over F7's existing recorder.
8. **B4**: `commitWindow` and `commit.message` move from `GitProvider` to `GitTarget`. Last of the
   principle items, and the one that makes the object coherent.
9. **The riders**: #5, F10, F12 references, `TooManyStreams`, the `default` ClusterProvider message.

Steps 2 to 8 are one release. Step 1 gates the planning. Step 9 can be trimmed if the wave gets too
big to review, since nothing else depends on it.

## What this costs, stated plainly

- **One coordinated consumer bump**, with a mechanical migration for every field: `placement.byType`
  moves verbatim, `placement.default` becomes `kind: Template`, `commitWindow` and `commit.message`
  move object, everything else is additive.
- **One `docs/UPGRADING.md` entry** covering the field moves, the layout mapping, and the one real
  behavior change: a declared template stops silently disabling the render root and starts
  registering its files.
- **A larger review surface than any change this project has taken.** That is the argument for
  trimming step 9 first and for keeping step 1 outside the wave.

## Open questions

- **Does `mode: Observe` write status only, or also refuse admission of new WatchRules?** Observe
  should be silent about everything except what it observed, but a user in Observe mode with rules
  piling up may expect to be told nothing will happen.
- **Should `suspend` and `mode: Observe` be one field?** They are close: both stop writes. They differ
  in intent (temporary versus declared) and in what they do to status, and Flux keeps `suspend`
  separate from everything else. Two fields, but the field docs must each say what the other is for.
- **Where does `interval` live?** `GitProvider` needs it for `ls-remote` cadence (review F6);
  `GitTarget` needs it for the observation pass. Two fields with one name on two objects is a smell,
  and one field on the provider cannot express a per-folder observation cadence.
- **Is `Auto` still the right layout default once `Observe` exists?** With a dry-run mode available,
  requiring an explicit `kind` costs the user much less than it would have, and it would make every
  target's layout self-evident.
