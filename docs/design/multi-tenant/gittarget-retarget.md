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
    sourceCluster: acme-workspace-kubeconfig   # empty for the local cluster
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

1. sets `Retargeting=True` (reason `DestinationChanged`), `Ready=False`,
   `Reconciling=True`, and clears `status.lastPushTime`;
2. re-runs the ordinary `Validated` gate against the **new** destination — the
   branch must match the provider's `allowedBranches`, and the new path must not
   overlap another `GitTarget` on the same provider+branch. A retarget onto a
   conflicting path is refused exactly as a create onto one would be, and the
   target stalls with `TargetConflict` while continuing to serve the *old*
   destination;
3. tears down the old materialization: unregisters the `GitTargetEventStream`
   (releasing the old branch worker), forgets the declaration, and drops the
   per-type watch cursors so the new folder is built from a full replay rather
   than resumed mid-stream;
4. re-declares against the new destination, which drives a fresh full snapshot
   into the new folder;
5. once the new destination reports `GitPathAccepted=True` and streams are
   running, writes `status.observedDestination` and sets
   `Retargeting=False` (reason `DestinationSettled`).

Steps 3–5 are exactly what a delete-and-recreate did, minus the window in which
the object does not exist.

## The old folder is left alone

**A retarget never deletes the old folder's files.** Two reasons, either
sufficient:

- Deleting from Git is the one irreversible thing the reverser can do, and a
  destination change is the moment when the operator is least sure of what they
  meant.
- The path the target is leaving may already have a new owner — the overlap check
  in step 2 only guards the destination it is *moving to*.

The old folder becomes ordinary, unmanaged Git content. The controller emits a
`Retargeted` event naming the abandoned `branch:path` so the operator can
`git rm` it deliberately, and repeats it in the `Retargeting=False` condition
message.

If the new path is a **subfolder or parent** of the old one, the overlap check in
step 2 fails against the target's own previous location only if another
`GitTarget` claimed it in the meantime; a target never conflicts with itself.
The files under the old path that are not under the new one simply stay.

## What still requires delete-and-recreate

Changing `spec.providerRef`. The materialization lives in a different repository;
there is nothing to move and nothing to observe. The CEL message says so:

> `spec.providerRef is immutable; delete and recreate the GitTarget to change its repository (branch, path and sourceCluster are mutable — see docs/design/multi-tenant/gittarget-retarget.md)`
