# The wave after placement left it

> **design**: a sequencing proposal, not a plan of record. Nothing here binds until scheduled.
> Index: [`../INDEX.md`](../INDEX.md)
> Date: 2026-08-28 (originally 2026-07-30).
>
> This document was written to sequence one breaking wave whose centrepiece was `spec.layout`.
> [`model.md`](../layout/model.md) has since reversed: the path template stays and gains two additive fields,
> so the placement work is not breaking at all. The wave lost its largest member. What is left is a
> batch, and this page is honest about being one.

## What is in it

| Member | Object | Why breaking |
|---|---|---|
| **B4**: `commitWindow` and `commit.message` move off the connection | `GitProvider` → `GitTarget` | fields change object |
| The source-scope deletion ([`source-scope-simplification.md`](source-scope-simplification.md)) | `GitTarget`, `ClusterProvider`, `WatchRule` | one removal, two renames, one redefinition |
| The riders: **#5** asserted `CommitRequest.spec.author`, the `CommitRequest` lifecycle hole, `meta.LocalObjectReference` for our six reference shapes, the `TooManyStreams` cap, the `default` `ClusterProvider` message | various | shape changes |

Additive, and therefore **not** wave members even though they are discussed here: `spec.suspend`,
`status.placement` and the post-scan pass, the reconcile-request annotation, and the two placement
fields. They ship whenever they are ready. Tier 1 in
[`open-asks-priority.md`](open-asks-priority.md) — the removal-wait decision, #15's
condition — is untouched and should not wait for this.

## Why still one wave

The consumer pins us three ways (image, Go module, `require` line), so each breaking release costs a
coordinated bump. That argues for batching, and with placement gone it is most of what is left.

The stronger half survives in reduced form: several of these items are the same design decision seen
from different angles, and building them separately means deciding it several times, inconsistently.

> **The folder is described on the GitTarget. The connection describes only the connection.**

`spec.suspend` says whether we write to the folder; `commitWindow` and `commit.message` say how those
writes are batched and phrased, and today they live on `GitProvider`, which is the connection (B4,
and the config-model review's "GitProvider is doing three jobs"). Shipping B4 alone would still
assert half the principle, which is the reason to keep the rest together.

## Where the fields live

The principle above has been a sentence in a document for two drafts. This wave is the moment it
becomes a struct boundary, because grouping a field costs nothing extra in a release that is
breaking anyway, and costs a bump in every release that is not.

`GitTargetSpec` today is flat: `providerRef`, `branch`, `path`, `encryption`, `placement`,
`clusterProviderRef`, `allowedSourceNamespaces`, `prune`. Queued on top of it were `suspend`,
`useKustomize`, `serializeNamespace`, `commitWindow` and `commit.message` — five more members on a
spec that already flattens six orthogonal axes. Left alone, this object accumulates faster than it
sheds, and moving `commit.message` off `GitProvider` for exactly that reason while doing it just
relocates the problem one hop.

Two groupings, and one deliberate exception:

- **`useKustomize` and `serializeNamespace` nest under `spec.placement`**, where an earlier draft of
  [`model.md`](../layout/model.md) had them at the top level beside it. They are placement concerns by
  their own argument — one decides what governs the produced document, the other what is inside it —
  and `spec.placement` is an existing optional struct, so nesting them is **still purely additive**.
  It costs no bump, it is free only before they exist, and it means the placement axis is one member
  of the spec rather than three.
- **`commitWindow` and `commit.message` land as `spec.commit`**, not as two top-level fields:
  `spec.commit.window` and `spec.commit.message.template`. The move is breaking either way, so the
  grouping is free, and `GitProvider.spec.commit` is the shape they already have.
- **`spec.suspend` stays top-level.** It is the object-level switch, it is where every Flux user
  reaches for it, and a `spec.write` wrapper invented to hold it plus `prune` plus `commit` would be
  a category we made up rather than one the API already has. `prune` is its own struct already.

After the wave the spec reads as named axes rather than a list: the immutable destination
(`providerRef`/`branch`/`path`), `encryption`, `clusterProviderRef`, `placement`, `commit`, `prune`,
and the one switch. That is the test for the next field too — a new member either joins an axis or
names a new one, and if it can do neither it is probably not a `GitTarget` field.

## The interactions that change the design

These are the reasons to combine, as opposed to merely batch. Each one changes what gets built.

### 1. Adoption is a dry run, and `spec.suspend` is enough to give it

Placement only ever affects *new* documents, so there is nothing to preview by inspection: you find
out where files go by letting one be written.

A suspended target plus `status.placement` is that preview. The operator scans, resolves the render
root, publishes what it *would* do, and writes nothing; clear `suspend` when the status says what you
expected. That needs no second field, which is why **`spec.mode: Observe|Write` (B1) was dropped**:
it bought only the difference between a temporary pause and a declared permanent read-only posture,
which is a distinction in intent, not in behavior.

The cost is stated rather than hidden: **`suspend` must keep observing.** Flux's `suspend` stops
reconciliation altogether; ours stops *writes* and keeps scanning, so the status a user is waiting on
stays fresh while they wait. That deviation belongs in the field's documentation, in one sentence,
because it is the only place we differ from a convention a Flux user brings with them.

**Re-open trigger for `mode`**: someone who needs a target that can never write, as a property of the
object rather than a switch a colleague can flip.

### 2. `GitTarget.spec.interval` is dropped, because we watch

Flux polls because **a Git remote cannot be watched**, which is why `GitProvider.spec.interval` stays
and is the honest one of the pair. A `GitTarget`'s inputs are API objects and we are already
streaming them, so every input that can change what a scan would conclude arrives as an event.

The name would also have misled: in Flux, `interval` is the drift-correction cadence, and it would
have driven neither of the two passes a Flux user pictures — the observation pass (reads this
target's folder, never writes, never decides deletions) or the resync mark-and-sweep (reads the API
via the streaming list's initial-events snapshot, and is the only thing qualified to infer a
deletion; [`event_router.go`](../../internal/watch/event_router.go),
[`reconcile-via-watchlist-mark-and-sweep.md`](../spec/reconcile-via-watchlist-mark-and-sweep.md)).

What is left uncovered is one case: a repository whose folder was changed **by someone else** while
our target wrote nothing. The reconcile-request annotation refreshes that on demand. A stale
`observedRevision` on an idle target is a legible cost; a periodic scan on every target is not.

**Re-open trigger**: a user who needs an idle target's `status.placement` to track a repository other
people edit, for whom the annotation is not enough. Then it is a scan cadence, named for scanning.

### 3. `spec.suspend` is a precondition for `useKustomize: true`

Creating a root writes a `kustomization.yaml` the user did not author. That raises the stakes on the
review's central complaint: *this controller writes to a Git repository and there is no way to make
it stop that is not deleting the object.* So `suspend` is not a rider here, it is a precondition —
and it must stop bootstrap creation specifically, not only resource writes.

### 4. The Events question is answered by the recorder that shipped

[`open-asks-priority.md`](open-asks-priority.md) left open whether a fall-back to canonical
should raise an Event, and reasoned it was expensive because placement runs on the branch worker with
no recorder. An `EventRecorder` now ships on every reconciler, and the roll-up seam projects
data-plane facts into status with an enqueue on change. So the Event is: emit when `status.placement`
changes in a way a human should know about — `LayoutResolved` becoming `Ambiguous`, or a type falling
back for the first time. One Event per persisted change, the pattern already established for `Ready`.

### 5. The source-scope deletion is the only member that shrinks the API

It belongs here for the ordinary reason first: it removes a field from `GitTarget`, and B4 already
breaks `GitTarget` in the same release, so shipping them apart costs the consumer two bumps for one
object. The better reason is the review surface. Every other member adds a field; this one deletes
4,569 lines, a three-valued verdict, and the only cross-cluster read in the authorization path. A
wave that is otherwise all addition is easier to justify when the object comes out simpler than it
went in.

Two interactions worth naming, because they change what gets built rather than merely when:

- **The `SelfSubjectAccessReview` pass is the same shape as the dry run.** Both answer "tell me what
  you would do before you do it". They are separate conditions on separate objects and neither
  depends on the other, so the SAR work stays additive and can ship after the wave — but whoever
  writes the second should read the first.
- **`TooManyStreams` is sized by the `*` decision.** `sourceNamespace: "*"` compiling to one
  cluster-wide list and watch removes the fan-out the cap was queued for. What is left to bound is
  explicit enumeration, so the cap is still worth a `Stalled` reason and a bound — sized against
  enumerated rules, and not planned before the `*` change lands.

### 6. Two facts kept from arguments that dissolved

`spec.layout` immutability and the namespace-agreement rule were both settled by the reversal:
neither field exists. Two facts from underneath them survive and will be reached for again.

- **`spec.placement` is mutable and stays mutable.** Existing files never move, so a template change
  affects only what is written afterwards and a folder can hold documents placed under two templates.
  Match-first identity keeps finding and editing those in place, so this is worth saying plainly
  rather than fixing.
- **`GitTarget` has no finalizer**, so deleting one leaves the folder untouched and re-creating it at
  the same path re-adopts every document by identity. Recreating a target costs status and a moment
  of mirroring, not data — which is what made immutability affordable for `path` and unaffordable for
  `prune`, and is the right test for any future field on this object.

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

  # --- what the documents look like: ADDITIVE, not part of the wave ---
  placement:
    byType:
      v1/secrets: "secrets/{name}{sensitiveSuffix}"
    useKustomize: true            # the created kustomization carries namespace:
    serializeNamespace: false

  # --- whether we write, and how those writes are batched and phrased ---
  suspend: false                  # the only stop-writes switch; a suspended target still scans
  commit:                         # B4, was GitProvider.spec.push.commitWindow and .commit.message
    window: 5s
    message:
      template: "chore(mirror): {{ .Summary }}"
  prune:
    mode: OnEvent
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
    serializeNamespace: false
    observedRevision: 9f3c1ab
  lastHandledReconcileAt: "2026-07-30T09:14:22Z"
```

The placement fields are shown in place so the object reads as a whole, but they carry defaults equal
to today's behavior and are additive: only `spec.commit` and the riders make this release breaking.
The status stanza is specified where it is built, in
[`model.md`](../layout/model.md).

## The envtest that has to run before any of this is planned

One question is not in the wave, and its answer constrains the enum work, so settle it first.

`ClusterWatchRule`'s `rules[].scope` enum is narrowed to `Cluster` only, deliberately keeping the
field so that re-applying a stored `Namespaced` value **fails**
([`clusterwatchrule_types.go`](../../api/v1alpha3/clusterwatchrule_types.go)). For CRDs the apiserver
validates the **whole object** against the OpenAPI schema on **status-subresource** updates too. If
that holds here, the controller cannot write `Stalled=True` onto an object whose stored
`spec.rules[].scope` is `Namespaced` — the status update is rejected 422, and the one object that
most needs to explain itself is the one that cannot. CRD Validation Ratcheting skips re-validation of
*unchanged* fields and is beta and default-on from 1.30, GA in 1.33, so the exposure is older
clusters or the gate turned off — which is why the test has to name a version.

**The test.** Create a `ClusterWatchRule` with `scope: Namespaced` through a client that bypasses the
enum (or against an older CRD), then attempt a status update, on the minimum supported Kubernetes
version.

**The fallback if it fails.** Widen the enum back and rely on the compile-path refusal plus a loud
`Stalled` condition. Refusing at admission is nice, but not at the cost of being unable to report the
refusal.

## Version strategy: stay `v1alpha3`

A wave this size invites `v1alpha4`, and I would not take it. A new version means a conversion path,
and the honest options — a conversion webhook (a serving dependency plus a cert lifecycle) or `None`
conversion with a stored-version migration — both cost more than the problem. The convention the repo
already uses is a **loud rejection**: keep the removed field in the schema, refuse it with a message
naming the replacement, for one release. `ClusterWatchRule.spec.rules[].scope` set that precedent,
and a rename is the case it is kindest on: the user's next `apply` tells them the new spelling.

**The assumption it rests on is one consumer, and that is a countdown rather than a constant.** Loud
rejection is cheap because a single coordinated bump can absorb it; a second consumer makes the same
wave cost two, on two schedules. Two things follow, and both belong on this repo's roadmap rather
than only on the consumer's:

- **Each wave leaves residue.** A refused field stays in the schema to say "no, not that anymore" —
  `allowedSourceNamespaces` now, more later. That graveyard is a real cost to a newcomer reading the
  CRD, and it is paid per wave, so the number of remaining waves on `v1alpha3` is finite. Sweeping
  the refusals is what `v1alpha4` should be for, and it should be one version bump carrying the
  removals rather than a version bump per change.
- **The coupling is a dependency, not a courtesy.** Staying on `v1alpha3` requires the consumer to
  keep pace with this repo's release cadence. If it cannot, the choice is not "revisit" in the
  abstract — it is a real API version with a real conversion path, and the time to notice is before
  the wave, not during it.

## Order inside the wave

Dependencies first, then the things that only need the object to be breaking.

1. **The `scope: Namespaced` envtest**, above. Not an API change; its answer constrains the enum
   work. Do it before planning.
2. **`spec.suspend`.** Precondition for anything that creates files, and independently the review's
   highest-value gap.
3. **`status.placement`** plus the post-scan validation pass. A dry run with nothing to read previews
   nothing.
4. **`requestedAt` + `lastHandledReconcileAt`.** On-demand refresh of step 3.
5. **Events on a changed resolution**, over the existing recorder.
6. **B4**, as `spec.commit`. Last of the principle items, and the one that makes the object coherent.
7. **The source-scope deletion.** Independent of every step above, so it can be written in parallel;
   placed here because a deletion reviews better once the additions it is not entangled with are
   settled.
8. **The riders.** `TooManyStreams` must come after step 7, which removes the fan-out it was written
   to bound.

Steps 2 to 5 are additive and need no bump. Steps 6 to 8 are one release; step 1 gates the planning;
step 8 can be trimmed if the wave gets too big to review, since nothing else depends on it. The
placement fields are absent from this list on purpose: their order is
[`model.md`](../layout/model.md)'s.

## What this costs, stated plainly

- **One coordinated consumer bump**, for `spec.commit`, the source-scope changes and the riders.
- **One `docs/UPGRADING.md` entry.** The placement work needs no migration entry at all. The `*`
  paragraph is the one to write carefully: it is the only change here that keeps its spelling and
  changes its meaning.
- **A capability genuinely lost**: source-side label selectors, priced in the source-scope document.
  It is the only thing in this wave a user can do today and cannot do afterwards.
