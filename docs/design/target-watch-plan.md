# Target watch plan: reconcile the changed cells

> **partly built**: the diff is built and applied. Removal semantics are not,
> and the deletion the diff unlocks has not been made.
> Index: [`../INDEX.md`](../INDEX.md)
> Date: 2026-08-26.
> Related: [`watch-and-catalog-architecture.md`](watch-and-catalog-architecture.md),
> [`data-plane-triggering.md`](data-plane-triggering.md),
> [`../spec/type-lifecycle-events-and-wobble-settling.md`](../spec/type-lifecycle-events-and-wobble-settling.md)

**The short version.** A GitTarget's watch set is replaced wholesale today, so
changing one rule replays all of them. This plan diffs the set instead and acts
only on the cells that changed. It applies that decision by starting and
canceling streams, and deliberately leaves the branch worker's queue alone.

New to this area? Read
[Watch and catalog architecture](watch-and-catalog-architecture.md) for the
model and [Data-plane triggering](data-plane-triggering.md) for the failure that
prompted this, then come back here.

## The problem

A **cell** is one `(group, resource, namespace)` slice of one `GitTarget`. The
served API version is data carried by the stream rather than part of the cell's
identity, because Git paths are versionless and a storage-version bump must not
move a file.

The desired watch set was already a map from a cell to an operation filter, but
any difference in that map canceled every stream for the target and replayed
every cell, so adding one rule replayed unrelated resources and could fill the
branch worker's shared queue.

The refactor changes the unit of work from the whole target to the changed
cells. That part is built; what remains is what a cell LEAVING means, and the
mechanism the old behavior forced into existence and no longer justifies.

## The queue is shared, so it stays dumb

One fact shapes everything below. A branch worker is keyed by
`BranchKey{RepoNamespace, RepoName, Branch}`, which names one **GitProvider and
branch**. Every GitTarget writing to that repository and branch therefore shares
a single `eventQueue`, of depth 100.

A shared pipe must not hold per-tenant configuration. If the worker had to decide
whether a dequeued item was still wanted, it would have to reach back into the
watch plan of whichever GitTarget that item named, on every item, for every
tenant on the branch. That is the wrong direction of dependency, and it buys very
little.

## Cut at the producer

**The plan is applied by starting and canceling streams. Nothing filters the
queue.**

Once an item is on the FIFO it will be applied. A canceled stream's goroutine may
still be in flight, so a configuration change can be followed by a short tail of
writes from a cell that is no longer selected. That is accepted behavior, for
three reasons:

- the tail is bounded by the queue, at most 100 items ahead plus one commit
  window flush, and it drains on its own;
- the content it writes is real observed state, gathered while the cell was still
  selected;
- what happens to a deselected cell's **files** is a separate and deliberate
  decision, made in "What a cell leaving means" below. Today the answer is
  retention, so a late write updates a file that is being kept anyway.

The only requirement this places on the design is on the producer side:
cancellation has to be prompt, and a canceled stream must stop enqueuing as soon
as it observes cancellation.

### What this removes

An earlier draft fenced the consumer instead, and grew three mechanisms to do it.
All three are dropped, and this table exists so none of them grows back:

| Dropped | Why it existed | Why it goes |
| --- | --- | --- |
| Per-cell **lease** on every queued item | Tell a canceled stream's in-flight item from its replacement's | A late item is allowed, so there is nothing to reject. **Removed** |
| **Tombstones** for stopped cells | Reject queued work after a `stop` | Retiring one meant knowing no work carrying it could remain in flight, which the queue cannot answer |
| Target-wide **plan generation** and `BeginDelta` | Order plan transitions and reset readiness per epoch | Readiness is per cell and keeps its own revision internally |

The **cell** stays on queued work, so a write or a drop record can name the slice
of the mirror it speaks for. That is what a saturated queue has to be diagnosed
from. Nothing is rejected on it, and nothing should be: the moment the worker
judges an item, the shared queue has learned about tenant configuration again.

The **lease** went with the fence it was built for. It was the stream incarnation
stamped beside the cell, and its only purpose was to let a consumer tell a
canceled stream's item from its replacement's. Keeping a field against a fence
that will never be built is how a retired design grows back, so it was deleted
rather than deprecated. That left `git.Provenance` holding one field, so the type
collapsed with it: queued work now carries a `types.CellKey` named `SourceCell`,
and `sourceCellForLog` renders the zero cell as "unclaimed".

### Where the bound is thin

Two cases deserve to be written down rather than discovered.

**Revocation.** A namespace can leave a watch set because a tenant boundary was
withdrawn, which is a stronger reason than a rule narrowed for convenience. The
tail then writes new content from a namespace the policy no longer admits. Git
already retains documents from a deselected namespace by deliberate decision, so
this is a difference of degree, and the queue bound applies. If that window ever
needs to be tighter, the fix belongs on the producer: refuse to enqueue once the
stream's context is canceled, inside the same critical section as the send.

**Ordering between overlapping cells.** A cluster-wide cell and a namespaced cell
can both deliver one object, and nothing orders them against each other. That is
unchanged by this plan. Closing it needs a single ordering domain per type, which
is a larger change.

## Diff the plan

**Built**, in `internal/watch/target_watch_plan.go`. The manager computes a
desired plan from the authoritative watched-type table: a map from cell to
specification, where the specification is the canonical operation filter and the
served version the stream opens at.

Comparing the previous and desired plans gives four outcomes:

| Outcome | Condition | Action |
| --- | --- | --- |
| `keep` | key and specification unchanged | Leave the stream and its readiness result alone |
| `start` | key only in the desired plan | Open the stream, replay that cell, then follow live events |
| `restart` | key in both, specification changed | Cancel, replace, and replay that cell |
| `stop` | key only in the previous plan | Cancel and drop the key |

An operation-filter change is a `restart` rather than a `keep`. The whole-set
replacement handled that correctly by accident, and a diff comparing keys alone
would have regressed it. A served-version change is also a `restart`, because a
watch is opened at a concrete version. A forced recovery classifies every cell as
`restart` and needs no state machine of its own.

One subtlety is invisible in that table and worth stating, because getting it
wrong looks like working code. A running stream is keyed by a watch key that
embeds the full GVR, so diffing THOSE keys would read a storage-version bump as a
`stop` of one key plus a `start` of another: the cell would be canceled,
replayed, and stripped of its readiness result, for a change that moves no file.
The plan is keyed by the cell and carries the version as specification data,
which makes the same event a single `restart`.

Because nothing filters the queue, `stop` is cheap: cancel the stream, drop the
key, and let whatever is already queued drain. `stop` never touches files. The
mirror is converged afterwards by a sweep, described in "Removal is a Git-side
sweep".

## What a cell leaving means

A cell can leave the plan for two very different kinds of reason, and only one of
them is a statement about the world:

| Kind | Examples | Effect |
| --- | --- | --- |
| **Authoritative** | A rule deleted or narrowed, a namespace deselected, a label revoked, a settled `TypeRemoved` past `RemovalGrace` | The cell stops contributing to the desired set. The sweep may then remove its documents, gated by `prune.mode` |
| **Uncertain** | Discovery wobble, list failure, RBAC denial, unreachable source cluster | **Abort the sweep.** An ungatherable cell must never present as an empty one |

That second row is the one that must never be got wrong, and it is why "walk the
folder and delete anything uncovered" cannot stand on its own: an incomplete plan
is indistinguishable from a narrowed one at the moment of the walk. Coverage is
an invariant worth asserting, and an unsafe action to take.

Everything else follows from putting a cell in the right row. There is no third
action, no per-cause policy, and no held-out set.

### Why a withdrawn type is removed rather than kept

An earlier draft recommended retaining a withdrawn type's documents on the
grounds that a missing CRD is often an operator upgrade in progress. That was
wrong twice over.

**A retained orphan is not inert.** This repository is consumed by Flux or Argo
CD. A manifest whose CRD no longer exists fails to apply and takes its
Kustomization or Application out of `Ready` with it, and depending on how the set
is applied the rest of the folder may stop progressing too. Retention has a blast
radius well beyond the orphan file. The same thing shows up locally as a
`kubectl apply -f` that no longer works on the folder.

**And deletion is recoverable here.** This is Git. A swept document stays in
history, and recovery is `git show <sha>:path` rather than a restore from
nothing.

So the criterion is not whether the data is valuable. It is whether the folder
still describes an appliable desired state. A manifest for a type that cannot
exist is a false statement about the cluster, which is the one thing a ledger may
not contain.

The upgrade-in-progress case is real, and it is handled where it belongs:
`RemovalGrace` and the settled `TypeRemoved` are exactly the distinction between
a wobble and a withdrawal. If that grace is trustworthy, an upgrade never reaches
this path. If it is not, the problem is larger than this policy.

Users who want an archive already have a way to say so. `prune.mode:
Never` is documented as "an archive or tombstone mirror that only ever gains
documents". Withdrawal therefore routes through the existing gate rather than
through a policy of its own.

### The withdrawal signal, and the mechanism to avoid

[Type lifecycle events and wobble settling](../spec/type-lifecycle-events-and-wobble-settling.md)
produces this distinction already. `internal/typeset` emits the per-type
lifecycle, and `RemovalGrace` separates a wobble from a removal, so the waiting is
part of the abstraction rather than something this plan adds.
`Registry.Subscribe` has no production consumer yet; the plan's `stop`
classification is the natural one.

The signal to consume is a **settled `TypeRemoved`**. Do not substitute direct
observation of a CRD delete. Local CRD and APIService informers run in the
operator's own control plane, while a remote source cluster's API surface is
learned through discovery refresh, where a failed group keeps serving last known
facts instead of presenting as an empty surface. Watching CRDs is right for the
local cluster and wrong for every mirrored one.

### The one decision still open

Withdrawal is settled by the argument above. **Intent is not.**

[`configuration.md`](../configuration.md) currently promises that when a
namespace leaves a target's watch set, its documents stay in Git "whatever
`spec.prune.mode` says", because deleting a tenant's manifests as a side effect
of a policy edit is destructive and a typo in a selector would be enough to
trigger it. Sweeping on intent reverses a documented, user-facing commitment.

`spec.prune.mode` also has no value that means "a rule was deleted": `Never`
suppresses all deletes, `OnEvent` mirrors observed DELETE events, and `Always`
permits inferred resync sweeps.

Three defensible resolutions, none yet picked:

1. **Honor `prune.mode`.** Rule removal deletes only under `Always`. Now the most
   attractive of the three, because it is the same answer withdrawal gets, and it
   leaves one gate in the system rather than two.
2. **A third category.** Deselection is neither an observed DELETE nor an
   inferred sweep, so it gets its own field. Costs an API field.
3. **Redefine `OnEvent`.** Treat a deselection as an event. Cheapest to build, and
   most likely to surprise a user who read the documentation.

Whichever is chosen, it has to be written where users read it, not only here.

## Removal is a Git-side sweep

The question "which files does this cell own?" is the wrong one to answer.
Answering it means a **managed projection**, a per-cell file index maintained in
the watch layer, and that layer has no business knowing about files.

Turn it around. Walk the managed documents in the GitTarget subtree and ask of
each one: **is this still wanted?** A document is wanted when it appears in the
gathered state of some currently selected cell. Anything else is a candidate for
removal. No projection is needed, because the question is asked of files rather
than of cells.

This is the right layer for it. Deciding what may be removed from Git already
lives on the Git side, along with the acceptance gate, `.gittargetignore`, and
the retention policy. The watch layer's whole contribution is a complete desired
set plus an assertion that the set is authoritative.

### Most of it is already built

A resync whose `Scope` is nil is exactly this sweep: `BuildPlan` drops every
managed document absent from the desired set, and `prune.mode` already gates it.
Under `never` and `onEvent` the planner emits no managed drop at all, and
`always` is the opt-in that restores full convergence. The mark-and-sweep, the
atomic flush, and the retention logging are all shipped.

What is missing is a producer. The only place a `ResyncRequest` is constructed
today always sets a scope, so the whole-target branch is reachable code with no
caller.

**The walk itself is already paid for.** The branch worker holds no resident
index of the repository; it keeps a checked-out worktree, and every write batch
already walks the whole GitTarget subtree with `scanWorktreeSubtree` and rebuilds
the manifest store from it. A sweep therefore costs about what an ordinary write
batch costs on the Git side. The new expense is the union gather from the
cluster, not the file walk.

### The guard everything rests on

**The union gather is all-or-nothing.** A scoped gather already aborts and
produces nothing on a partial stream. Across a union of cells the rule has to be
stronger: if any selected cell fails to gather, for any reason, abandon the whole
sweep and remove nothing. Otherwise an outage presents as "these objects are
gone" and the sweep deletes a tenant's manifests.

`prune.mode` does not cover this on its own, because `Always` is a standing
setting rather than consent for one particular sweep.

A settled withdrawal is not a gather failure. The type is gone, so it is no
longer a selected cell, and it contributes nothing to the desired set without
holding anything up. The distinction between "a cell I should be able to gather
and cannot" and "a cell that is legitimately gone" is the wobble-versus-withdrawal
call, which `typeset` already owns.

That leaves a clean division of labor:

```text
typeset  decides   present / wobbling / settled-gone
watch    gathers   every selected cell, all-or-nothing
git      sweeps    anything absent from desired, gated by prune.mode
```

### Worked examples

Take a GitTarget mirroring `widgets.example.com` from `team-a`, whose operator is
being upgraded, and assume `prune.mode: Always` unless stated otherwise.

| Situation | What `typeset` says | What happens to Git |
| --- | --- | --- |
| CRD disappears for 20s during a rolling operator upgrade | `TypeWobbling`; `RemovalGrace` (60s) has not elapsed | Nothing. The cell holds, and a sweep in flight aborts. Documents untouched |
| Operator uninstalled for good; Kubernetes cascade-deletes the widgets | Settled `TypeRemoved` after the grace | The cell leaves the plan, so its documents are absent from desired and swept. The folder applies cleanly again |
| Same, but the target is `prune.mode: Never` | Settled `TypeRemoved` | Nothing is removed. This is the archive mirror the mode exists for |
| Source cluster unreachable while a sweep is triggered | The cell is selected but cannot be gathered | The union gather aborts. Nothing is removed anywhere in the target, even under `Always` |
| RBAC for `widgets` revoked | List returns `Forbidden`, so the cell cannot be gathered | Same as above. A permission loss is not a deselection |
| The `team-a` label is revoked, so the namespace leaves the watch set | Nothing. The type is healthy; the cell was deselected by **intent** | Governed by the open decision above. Today, nothing is removed |

The fourth and fifth rows are the ones worth internalizing. Both look like "the
objects are gone" from inside a naive walk, and in both cases removing anything
would be destroying a tenant's manifests during an outage.

### What this deletes from the plan

- The managed projection, and the dependency on
  [Watch and catalog architecture](watch-and-catalog-architecture.md) building it
  first.
- The held-out set for withdrawn types, and with it the need to resolve a Git
  file back to a type whose mapping discovery may no longer offer.
- A per-cause action table. There is one gate, `prune.mode`, and one guard.
- `stop` touching files at all. It cancels the stream and drops the key; the
  sweep converges the mirror afterwards.
- The need to prevent the accepted tail. A late write from a deselected cell is
  removed by the next sweep rather than fenced at the queue.

Deletion becomes level-triggered convergence instead of an edge-triggered side
effect of a configuration change, which is the same split this system already
draws between a write and a resync.

## Queue ordering and coalescing

This section is about **ordering**, which the producer cut does not address. A
late item is acceptable; a stale one that overwrites newer state is not.

Resyncs are level-triggered, so a newer snapshot for the same target and scope
can replace an older queued one. That replacement is safe only while no work for
that scope has been queued behind the snapshot's FIFO marker. When a write or a
commit attachment is queued behind a pending resync, mark that resync as having
passed its safe point, and let the next resync take a fresh position at the tail:

```text
snapshot @ rv100     marker P
write @ rv101        position P+1
snapshot @ rv103     new marker at the tail
```

Two implementation traps, both already paid for once:

- The marker and the pending-map update must be under the same mutex as the
  non-blocking send. Marking before an unlocked send leaves a window where a
  resync sees the mark, declines to coalesce, and takes its tail position before
  the write it is fencing against has entered the queue.
- Identify a pending entry by its marker, not only by its map key, because a
  later request can reuse that key after the older marker is already queued.

The match deciding whether a write falls inside a resync scope is by object
identity. Two overlapping cells can both deliver one object, so the producing
cell cannot be recovered from the object. Over-matching may forgo a coalesce, and
it can never move a snapshot ahead of a write it might overwrite.

This fence is built and shipped. It is recorded here because it holds only while
the reason for it stays legible.

### Whether coalescing should survive the diff

Coalescing is the one place in this system where a queued item is **mutated**:
the marker keeps its FIFO position while its payload is swapped for a newer
snapshot. Everything above exists to make that mutation safe. The alternative is
to stop mutating and let every trigger take a fresh position at the tail, which
is what the fence already degrades to when it trips.

That alternative is correct. Applying `snapshot(rv100)`, `write(rv101)`,
`snapshot(rv103)` in queue order converges, because each snapshot then sits at a
position consistent with the state it carries.

What coalescing buys is a bounded queue. A `ResyncRequest` is a payload and a
reply channel, so one trigger is one slot out of 100, and the storm that prompted
this work was 595 resyncs in 16 seconds. Without coalescing that is roughly 500
dropped requests, and a drop is not free: `EnqueueResync` reports it precisely so
a caller cannot mark a target reconciled through a watermark no reconcile
reached.

**The cause of that storm is what this plan deletes.** The 595 came from
whole-target replacement. Once the diff lands, one rule edit produces one restart
and one resync. So: keep coalescing while the diff is built, measure the real
resync rate afterwards, and if it sits comfortably inside the queue, delete
coalescing, the tail fence, `pendingResyncs`, `tailPassed` and
`ErrResyncSuperseded` together. That is the largest deletion available here, and
it becomes safe because the diff landed.

One variant is worth naming so it is not tried: making a resync a bare **signal**
that is deduplicated by key and gathers state when the worker dequeues it. That
is the usual work-queue pattern and it is wrong here, because gathering at
dequeue makes the payload newer than its FIFO position. That is the stale-write
hazard above, applied to every resync rather than to a rare coalesce. The reply
channel each request carries is a second obstacle.

## Readiness

Readiness is per cell. A new or restarted cell is pending until its replay
finishes, an unchanged cell keeps its prior clean or divergent result, and a
removed cell leaves the target's readiness reduction.

The readiness store may use an internal revision to ignore a late report, but
that revision stays an implementation detail of the store. It is not a second
watch protocol and it does not travel through queued work. That is what
`RenderFidelityGate` keeps: one revision per scope, issued when the cell's stream
starts and carried by that stream alone, so a retired stream's tail is stale by
construction.

An unrelated plan change must not clear a divergence. A target held open by a
render-fidelity divergence stays held, because adding a WatchRule is no evidence
that the divergence was resolved. Only a successful replay of the divergent cell
clears it.

The gate used to key this on a single per-target epoch that every declaration
bumped, which was coherent only while every declaration restarted every stream.
Applying the plan per cell breaks it in both directions at once: a kept cell
would sit pending under an epoch it never replayed for, holding the target
Unknown and closing its writes forever, and an unrelated edit would clear a
divergence nothing had re-measured. The revision had to become per scope before
the diff could be applied at all.

Retention rides the same revisions, because it used that epoch for scope
eviction. The roll-up is keyed by cell, so the plan installs its selected cells
and their revisions, and a count is accepted only for a cell the plan holds, at
the revision it holds. A cell that leaves takes its count with it; a cell that is
kept keeps the count nothing re-measured.

### The one divergence that belongs to no cell

A divergence a live WRITE discovers is target-level, and it cannot be filed under
a cell. Two facts settle that, and both were checked rather than assumed:

- a commit window batches events from several cells of one GitTarget, so the
  refused flush is not one cell's work;
- the refusal names Git **paths**, and resolving a path back to a type needs the
  GVK-to-GVR mapping this plan deliberately stopped depending on (see "What this
  deletes from the plan").

So the gate holds it beside the scopes rather than inside one, and only a plan
that restarts every scope clears it: a forced recheck, or a target starting from
nothing. That is stricter than `GitPathAccepted`, which any one successful resync
clears, and the asymmetry is deliberate. Acceptance is a property of the subtree
that any successful flush re-proves. Fidelity is per-cell evidence, and one
cell's clean replay says nothing about a token another cell writes.

## Where this stands, and what to merge

Three changes, each independently shippable. **The first two are built.**

### Built

Cell identity (`types.CellKey`, versionless, one stream per cell); the source
cell stamped on queued items, with no lease beside it; the coalescing tail fence;
the whole-target mark-and-sweep itself, which needs a caller rather than an
implementation; and:

**1. The diff.** `target_watch_plan.go` computes the desired plan and classifies
it against the running streams into `keep` / `start` / `restart` / `stop`. Every
reconcile logs the four outcomes with each cell named, an all-`keep` one
included.

**2. Applying it.** A `targetWatchSet` is a map of running streams keyed by cell,
each with its own cancel, so `stop` and `restart` cancel one cell and `keep`
touches nothing. Readiness follows: a kept cell holds the result its own replay
produced. Cancellation is prompt at the producer: a canceled stream stops
enqueuing before routing a live event and before enqueueing a replay snapshot it
can no longer report on. The render-fidelity revision and the retention roll-up
moved with it, for the reasons under "Readiness".

Two mechanisms went with the wholesale replacement they existed for: the
set-wide cancel, and the whole-map specification comparison that answered "did
anything change". The diff answers that per cell, so an unchanged reconcile is
"nothing to start, nothing to stop" rather than an early return.

### The merge boundary is here

Changes 1 and 2 are a merge point, and the argument is that nothing waits on
change 3.

`stop` canceling a stream without touching files is not an interim state. It is
what [`configuration.md`](../configuration.md) promises today: a deselected
namespace's documents stay in Git. The sweep converges the mirror afterwards, and
the plan owes it nothing in the meantime. A reader of the repository sees the
behavior the documentation already describes, reached more cheaply.

Bundling change 3 would put an unresolved product decision ("The one decision
still open") inside a pull request whose other half is finished, and would hold a
performance fix hostage to it. Merge here.

### Then, in this order

1. **Measure, then delete coalescing.** This is the largest deletion available,
   and change 2 is what makes it safe (see "Whether coalescing should survive the
   diff"). It needs a number from a running workload, which is a second reason to
   merge first: the measurement is the real resync rate once one rule edit
   produces one restart. If it sits comfortably inside the queue, `pendingResyncs`,
   `tailPassed`, `ErrResyncSuperseded` and the tail fence go together.
2. **Change 3, removal semantics.** Subscribe to the registry so a settled
   `TypeRemoved` drops its cell as an authoritative cause, and give the
   whole-target sweep a producer: an all-or-nothing union gather across the
   selected cells, enqueued as a nil-scope resync under the existing `prune.mode`
   gate. Withdrawal converges on that alone. Sweeping on **intent** waits for the
   open decision.

The order is deliberate. Change 3 carries a product question and adds a new
resync producer; the coalescing deletion carries neither and is pure subtraction.
Doing the subtraction first also means change 3's sweep is measured against a
queue whose behavior is settled, rather than against one being changed underneath
it.

### What this left behind

Two small debts, recorded so they are paid rather than discovered:

- `streamRevisions` fills in zero revisions when no shared fidelity gate is
  wired. That path exists only because the gate can be absent in test wiring, and
  it is a fork in the code that a required gate would delete.
- The retention roll-up now drops a count for a cell its plan has not installed
  yet. That is the correct fence, but it is an ordering requirement a caller can
  trip, where eviction by epoch could not be got wrong.

If a concurrency problem later resists prompt cancellation and FIFO ordering,
document that specific failure before adding any fence, then add the smallest one
that prevents it.

## Acceptance scenarios

A scenario list rather than a test plan. Each needs a failing-first test. The
ones changes 1 and 2 cover are marked; the rest belong to the work still ahead.

**Classification**, covered in `internal/watch/target_watch_test.go`. Adding a
rule starts one cell and leaves every existing cell running. Removing one of
several rules stops one cell and leaves the others untouched. An operation-filter
edit restarts that cell alone. A no-op edit, such as a status write, classifies
everything as `keep` and preserves readiness results.

**The accepted tail**, partly covered. Work queued before a cell was deselected
is still applied, and its commit is well-formed. A canceled stream stops
enqueuing promptly, so the tail is bounded by the queue rather than by how long
the goroutine lives. A write and a drop record both name the producing cell, with
no lease. What is asserted today is the producer half: a canceled stream
enqueues nothing. That the tail's own commit is well-formed is an end-to-end
claim no unit test makes.

**Ordering.** A restart while events for that scope are queued does not apply an
older event after a newer snapshot. A resync never coalesces past a queued write
for the same scope. Queue saturation drops or coalesces without any accepted
request failing to run.

**Cause and policy.** A discovery wobble holds every cell, stopping nothing and
deleting nothing. An unreachable source cluster, and an RBAC denial, both hold
and never present as deselection.

**The sweep.** A union gather in which one cell fails removes nothing at all,
even under `prune.mode: Always`. A settled `TypeRemoved` past `RemovalGrace`
sweeps that type's documents under `Always` and keeps them under `Never`. A
wobble inside the grace sweeps nothing. A cell deselected by intent is removed
only under the mode the open decision names. Auxiliary and retained files inside
an accepted folder are never touched. A sweep run twice with no cluster change is
a no-op the second time.

**Render fidelity**, covered in `internal/git/render_fidelity_gate_test.go`. A
diverged cell stays diverged across an unrelated plan change, and writes do not
reopen. The diverged cell reporting clean reopens writes, once. A divergence a
live write found is cleared only by a plan that restarts every scope.
