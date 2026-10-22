# Label selection: follow-ups

> **design**: open. Nothing here blocks label selection from shipping. Item 1 is resolved.
> Index: [`../INDEX.md`](../INDEX.md). Parent: [`watches-labels-simplification.md`](watches-labels-simplification.md).
> Checked: 2026-09-29, against `feat/watch-object-selector` (PR #399).

The label-selection branch went through several review rounds before merge. Most findings were
fixed on the branch. This page keeps the ones that were not, and says why. For each open item it
records what is true today, what it costs, and what a fix would take, so that nobody has to
re-derive it.

## Fixed on the branch

For context only. None of these is open.

| Finding | Fix |
|---|---|
| The recovery hint for `unowned-delete-patch` said "rename or remove". For an object a patch hides, the refusal's path is the hidden base document, so the hint read as "delete your manifest". | The hint names the patch and its kustomization reference. |
| A batch holding two events for one inherited object read the pre-batch render for the second one. A re-entry followed by an upsert refused the batch as hidden by an unowned patch. A re-entry followed by a delete left the object rendered. | The batch records what it did to each inherited object's presence and consults that record before the pre-batch store. See `rendersInBatch` in [`plan_flush.go`](../../internal/git/plan_flush.go). |
| Refusal memory and queued `onRefusal` requests were keyed by the selector-bearing collection. After a selector change, the old key survived recovery. A first attempt fixed only the resync side and broke live refusals: those still used the selector-bearing key, so an accepted snapshot never cleared them, even with an unchanged selector. | Every refusal key is built by `newRefusalKey`, which drops the selector. The full collection is still used for watch identity and status. See [`refusal_reapply.go`](../../internal/git/refusal_reapply.go). |
| `targetWatchSet.stop` retired the producer gate before cancelling the stream. An in-flight snapshot enqueue blocked in `Client.Get` on the stream context could hold the owner loop. | Cancel first, then retire. The gate still admits nothing afterwards. |

About the second row: a commit window keeps only the last event per path, and every pending write
is flushed on its own. So the ordinary live stream never hands one batch two events for one
object. The fix repairs a batch invariant, and the tests call the batch directly. No e2e scenario
reaches it today.

## Open

### 1. Real deletions in a selected collection wait the full attribution grace

**Resolved 2026-10-22.** A `DELETED` frame from a selected collection used to resolve only on the
fact at the object's exact `(uid, resourceVersion)`, which a real deletion never fills. Every such
deletion waited out the grace and committed unresolved, and a prune of N objects stalled its stream
for N x grace (it made the Flux E12 save spec fail `NoWindow`).

Attribution no longer reads the selector. A removal of an object that was not terminating asks for
a write at its exact version first (the exit), then for deletion evidence, so a real deletion
resolves on its delete fact in about one audit batch. The one case this names wrongly, a
relabel-out followed by someone else's delete whose fact is in the index first, is accepted and
listed in
[what attribution gives up, on purpose](../spec/attribution.md#what-attribution-gives-up-on-purpose).

Head-of-line blocking itself is still open: an event that legitimately waits out the grace still
delays every later event on its stream. That is ranked in
[`open-asks-priority.md`](open-asks-priority.md).

### 2. The resync fence stops overlapping collections from coalescing

**Today.** `markResyncTailForResyncLocked` fences every other pending resync of the same GitTarget
whose sweep boundary overlaps the new one's. This is what keeps a selector changed A → B → A in
the order A1, B, A2.

**Cost.** None left to pay. A GitTarget can no longer run two overlapping collections side by side
(see [overlapping collections](../configuration.md#overlapping-collections)), so the fence only
orders successive collections of one scope, which must not coalesce anyway.

### 3. Overlap ties within one second go to the lower name

**Today.** When two rules select overlapping collections, the older rule keeps them.
`selectingRule.olderThan` compares `CreationTimestamp`, which has one-second resolution, and breaks
ties by name. See [`collection_overlap.go`](../../internal/watch/collection_overlap.go).

**Cost.** Say rule `b` is created and running, and rule `a` is applied within the same second.
`a` wins the tie and takes the collections. `b` is refused loudly (`CollectionOverlap`), and
under `prune.mode: Always` the next snapshot sweeps with `a`'s selector.

**Why it was not fixed.** The order is deterministic and documented, and the loser is told. A
real fix needs incumbency: the compiler would have to know which rule's streams are already
running and prefer that rule on a tie. That couples rule compilation to watch state it
deliberately does not read.

### 4. Every object coming back rebuilds the whole store

**Today.** An inherited object coming back into the render is planned against the staged render.
`stagedRender` runs `BuildStoreFromScan` over the batch's staged files, which re-renders every
kustomize root, once per object that comes back.

**Cost.** Widening a selector over N inherited objects costs N full builds of every root in one
batch. No correctness defect has been shown.

**Fix.** Rebuild once, after all retirements in the batch, or re-render only the affected root.
Either way, keep the rule that placement for *new* documents reasons about the pre-batch tree.

## Not part of this branch

The audit-route override e2e spec failed intermittently during this work. It passed on the final
CI runs, and its cause is still unexplained. Do not treat it as fixed.
