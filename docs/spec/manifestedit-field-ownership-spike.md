# Field ownership: the API wins

> **spec** — current behaviour. The code depends on this document; change one, change the other. Index: [`../INDEX.md`](../INDEX.md)
> 
> Status: decided
> Related: [manifestedit-abstraction-plan.md](manifest-system.md)
> (step 5), [manifest-inventory-file-agnostic-placement.md](manifest-system.md),
> [bi-directional.md](../bi-directional.md),
> POC decision record: `internal/git/manifestedit/DECISION.md`

The abstraction plan flagged field ownership — "does the reverser own the whole
object or a declared subset?" — as a product decision. Here is the decision, and
it is blunt:

**This is an API-first system. The API wins. A field that is not in the API
projection is deleted from Git. Full stop.**

GitOps Reverser does not own a "declared subset" of fields. When you point a
GitTarget at a branch and folder path, GitTarget owns that path and makes it
**exactly** match the managed cluster state. The desired object is the *entire*
truth for every document it writes. There is no per-field ownership predicate to
configure, no list of "fields we promise to leave alone," no managed-fields
bookkeeping. Git is a faithful mirror of the API, and the reverser keeps it that
way.

## "But that deletes my hand-edited field"

It deletes a field that exists in Git but not in the cluster. That sounds
hostile, and in a one-directional world it would be. We are not building a
one-directional world.

The mitigation is the rest of the product: **bi-directional GitOps.** A change
made directly in Git is picked up and applied to the live cluster
([bi-directional.md](../bi-directional.md)). Once it is in the cluster it is in
the API, so it is in the next projection, so the reverser preserves it — because
now it genuinely *is* part of the API object.

So the loop closes:

- Edit a field in the cluster → the reverser writes it to Git.
- Edit a field in Git → the bi-directional reconciler applies it to the cluster →
  the reverser keeps it in Git.
- A field that is in Git but in neither the cluster nor a pending Git-to-cluster
  apply is, by definition, orphaned state. Deleting it is correct.

There is no "foreign field" category once both directions are wired. A field is
either in the shared truth (cluster API) or it is garbage. The fear only exists
if you imagine Git as an independent write surface the cluster never learns
about — which is exactly the gap bi-directional GitOps removes.

This also resolves the apparent tension with the file-agnostic-placement vision.
Attaching to an existing repository does not mean tiptoeing around its contents;
it means GitTarget *adopts* that path and brings it into lockstep with the
cluster. Getting the bi-directional game straight is what makes that safe — not a
field-ownership escape hatch.

## What GitTarget promises

> If you ask a GitTarget to take care of a git branch and folder path, it does
> exactly that: that path is kept exactly in line with the managed cluster
> state. GitTarget owns the path; the API is the source of truth.

Everything under a GitTarget's path is the reverser's to write. Documents are
whole-object truth. New files are placed by the inventory/placement layer; existing
documents are edited in place (preserving formatting of the bytes that did not
change — that is the `manifestedit` mechanism, and it is *fidelity*, not
ownership). Removal of a managed resource removes its document.

## Explicitly rejected (do not build)

The following were considered and rejected. They are a maintenance minefield and
they fight the API-first model:

- **Declared-subset / managed-paths ownership.** A configured set of "fields we
  own" is a heuristic that drifts from reality, needs a config surface, needs
  per-key rules for labels/annotations, and turns every "why didn't my change
  propagate?" into a support puzzle. No.
- **Server-Side-Apply `managedFields`-driven ownership.** Principled in theory,
  but `fieldsV1` set-encoding, shared ownership across managers, and apply-vs-update
  semantics make a faithful predicate expensive to build and brittle to maintain —
  for a guarantee bi-directional GitOps already provides more simply. No.

If a real, concrete need for partial ownership ever appears, it must come back
through its own proposal with a worked use case that bi-directional GitOps cannot
serve. Until then, the answer is whole-object truth.

## Mechanism note: the `Owns` seam

The merge carries an `Owns(FieldPath) bool` seam (`EditOptions.Owns`,
default `nil` = own everything). That seam stays, because it keeps the merge
honest about *where* a deletion decision is made and it is free. But it is a
mechanism detail, **not** a product feature: policy is pinned to own-all, and we
do not ship or support any non-trivial predicate. Do not grow configuration on top
of it.

## Convergence

Own-all is the clean, already-proven case. Because the desired object is the whole
truth, `Decide`'s object-equality fast path fires exactly when Git already equals
the projection, and the convergence property (`assertConverges`: a second `Decide`
after `Apply` is `NoChange`, byte-stable) holds with no caveats. Whole-object truth
is the model the convergence tests already assume — choosing it changes nothing
and adds no new failure modes. That is the point.
