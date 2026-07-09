# Moving a GitTarget's destination

> Status: implemented
> Related: [README.md](README.md), [config-plane-split.md](config-plane-split.md)

## Problem

`spec.branch` and `spec.path` were immutable. The reasoning was sound: a
`GitTarget` materializes its watched resources at exactly one (provider, branch,
folder), and letting that move would orphan the old materialization and silently
invalidate the initial-snapshot gate — "a successful snapshot can never be
invalidated by a destination change".

The cost fell on anyone who ever repoints a target. An operator that stamps a
`GitTarget` with a default branch or folder before the user has chosen one has
frozen the object against that default. The only recovery was to catch the
admission rejection and delete-and-recreate, which every such operator ends up
writing for itself.

## Shape

`spec.branch`, `spec.path` and `spec.sourceCluster` are **mutable**.
`spec.providerRef` stays immutable — pointing at a different repository is not a
move, it is a different object.

The snapshot-gate invariant is preserved by making the destination a thing the
status *observes* rather than a thing the spec cannot change:

```yaml
status:
  observedDestination:
    branch: main
    path: clusters/acme
    sourceCluster: team-a/acme-kubeconfig/value.yaml   # empty for the local cluster
  conditions:
    - type: Retargeting
      status: "False"
      reason: DestinationSettled
```

The invariant becomes: *a successful snapshot is valid for the destination
recorded in `status.observedDestination`.* When `spec` and
`status.observedDestination` disagree, the snapshot is by definition stale, and
the reverser says so.

## Lifecycle

On observing a destination change the controller:

1. tears the old materialization down **before anything is validated**, and sets
   `Retargeting=True` (reason `DestinationChanged`), clearing
   `status.lastPushTime`. Teardown must come first: the writer reads `spec.path`
   fresh on every write while the branch worker is bound to the branch its event
   stream was registered against, so a live event arriving mid-move would
   otherwise be written to the *new* path on the *old* branch;
2. re-runs the ordinary `Validated` gate against the **new** destination — the
   branch must match the provider's `allowedBranches`, and the new path must not
   overlap another `GitTarget` on the same provider+branch. A retarget onto a
   conflicting path is refused exactly as a create onto one would be, and the
   target stalls with `TargetConflict`. Note that because the teardown in step 1
   already happened, the target then mirrors **nothing** until a free destination
   is chosen — it does not fall back to the old one. That is the price of the
   ordering, and it is the right price: the alternative is a live event landing at
   the new path on the old branch;
3. re-declares against the new destination, which drives a fresh full snapshot
   into the new folder;
4. once the new destination reports `GitPathAccepted=True` and streams are
   running, writes `status.observedDestination`, sets `Retargeting=False` (reason
   `DestinationSettled`), and emits a `Retargeted` event.

The teardown in step 1 unregisters the `GitTargetEventStream` (releasing the old
branch worker), forgets the declaration, and — the load-bearing part — drops the
per-type watch **resume cursors**. Those are keyed by `GitTarget` UID, and a
retarget keeps the same object; without dropping them the new folder would only
ever receive the changes that happen *after* the move, never the state that
already existed.

The whole sequence is exactly what a delete-and-recreate did, minus the window in
which the object does not exist.

It is idempotent per generation: a reconcile that finds the teardown already done
for the current generation skips it and lets the rebuild proceed. A *second*
destination change arriving mid-move bumps the generation, which makes the
recorded teardown stale, so it tears down again rather than quietly continuing to
build the first one.

## The old folder is left alone

**A retarget never deletes the old folder's files.** Two reasons, either
sufficient:

- Deleting from Git is the one irreversible thing the reverser can do, and a
  destination change is the moment when the operator is least sure of what they
  meant.
- The path the target is leaving may already have a new owner — the overlap check
  in step 2 only guards the destination it is *moving to*.

The old folder becomes ordinary, unmanaged Git content. The controller emits a
`Retargeted` event naming the abandoned `branch:path` so the operator can `git rm`
it deliberately, and repeats it in the `Retargeting=False` condition message —
which is then left alone by later reconciles, because that message is the only
place in status where the abandoned folder is named.

If the new path is a **subfolder or parent** of the old one, the overlap check in
step 2 fails against the target's own previous location only if another
`GitTarget` claimed it in the meantime; a target never conflicts with itself.
The files under the old path that are not under the new one simply stay.

## What still requires delete-and-recreate

Changing `spec.providerRef`. The materialization lives in a different repository;
there is nothing to move and nothing to observe. The CEL message says so:

> `spec.providerRef is immutable; delete and recreate the GitTarget to change its repository (spec.branch and spec.path are mutable — the controller retargets, see docs/design/multi-tenant/gittarget-retarget.md)`
