# Bi-Directional Flux Manual Handshake Plan

## Goal

Make bi-directional sync deterministic again by removing Flux's autonomous apply loop from the critical path.

The key change is:

- Flux appliers do not run continuously for these targets
- GitOps Reverser triggers Flux explicitly when a specific Git revision must be acknowledged
- GitOps Reverser waits for concrete Flux status signals before it continues normal processing

This turns the problem from "two live loops racing" into "one controller asks Flux to consume revision X and
waits until Flux proves it did so".

## Decision

For the bi-directional mode, treat Flux as a manually triggered applier, not as an always-live reconciler.

Recommended first scope:

- keep `GitRepository` present as the Flux source object
- suspend the Flux `Kustomization` objects that apply the same path GitOps Reverser writes to
- trigger source refresh plus apply on demand
- wait for `GitRepository` and `Kustomization` acknowledgment of the exact revision

Why this shape:

- suspending the `Kustomization` removes the stale desired-state replay race
- keeping the `GitRepository` object gives us a clean status surface to observe
- explicit trigger plus explicit wait gives us a deterministic checkpoint

## What We Wait For

For plain manifest GitOps, the deterministic handshake should watch these Flux resources in order.

### 1. Source acknowledgment

Resource:

- `GitRepository` in `source.toolkit.fluxcd.io`

Required signals:

- `Ready=True`
- `.status.artifact.revision` contains the commit SHA we want Flux to consume

Useful extra signal when we trigger manually:

- `.status.lastHandledReconcileAt` matches the trigger token we wrote via
  `reconcile.fluxcd.io/requestedAt`

Meaning:

- Flux source-controller has fetched the Git revision we care about
- the artifact used by downstream appliers now points at the right commit

### 2. Apply acknowledgment

Resource:

- `Kustomization` in `kustomize.toolkit.fluxcd.io`

Required signals:

- `Ready=True`
- `.status.lastAppliedRevision` contains the same commit SHA

Useful extra signals:

- `.status.observedGeneration` has caught up to `.metadata.generation`
- `.status.lastHandledReconcileAt` matches the trigger token for the manual reconcile request

Meaning:

- kustomize-controller has finished reconciling the exact revision we asked for
- GitOps Reverser can now treat that Git revision as acknowledged by Flux

## Operating Model

### Steady state

For managed bi-directional targets:

- `Kustomization.spec.suspend=true`
- GitOps Reverser keeps watching cluster changes
- Flux does not continuously re-apply the path

### When GitOps Reverser creates a reverse-sync commit

1. GitOps Reverser writes and pushes commit `R`.
2. GitOps Reverser records a pending handshake for commit `R`.
3. GitOps Reverser triggers Flux for that target.
4. GitOps Reverser waits for:
   - `GitRepository.status.artifact.revision` to include `R`
   - `Kustomization.status.lastAppliedRevision` to include `R`
   - both resources to be `Ready=True`
5. GitOps Reverser re-suspends the `Kustomization` if it had to resume it.
6. GitOps Reverser clears the pending handshake.

Even though the cluster already has the change, this step still matters. It proves Flux has consumed the new
Git desired state and will not later "discover" an older revision and replay it.

### When GitOps Reverser detects a non-ours remote commit

This includes the existing "push failed because remote moved" path.

1. GitOps Reverser fetches the new remote head `E`.
2. GitOps Reverser pauses reverse processing for that target.
3. GitOps Reverser triggers Flux for revision `E`.
4. GitOps Reverser waits for the same source and apply acknowledgment gates.
5. GitOps Reverser refreshes its repo state view after Flux acknowledgment.
6. GitOps Reverser resumes normal processing.

This makes the race explicit and observable instead of silently letting Flux and reverse sync compete.

## Trigger Sequence

The plan should prefer Kubernetes API actions over shelling out to the `flux` CLI.

### Recommended trigger flow

1. Generate a unique trigger token, for example a Unix timestamp or UUID.
2. Annotate the `GitRepository` with:
   - `reconcile.fluxcd.io/requestedAt=<token>`
3. Wait for `GitRepository.status.lastHandledReconcileAt=<token>` and the expected
   `.status.artifact.revision`.
4. Patch the target `Kustomization.spec.suspend=false`.
5. Annotate the `Kustomization` with:
   - `reconcile.fluxcd.io/requestedAt=<token>`
6. Wait for:
   - `Kustomization.status.lastHandledReconcileAt=<token>`
   - `Kustomization.status.lastAppliedRevision=<expected revision>`
   - `Ready=True`
7. Patch the `Kustomization` back to `spec.suspend=true`.

This sequence avoids trusting timing alone. Every step has a concrete status-based acknowledgment.

## Why Not Only Trigger the Kustomization

Because the observed race happens between source refresh and apply.

If we only trigger the `Kustomization`, we still need proof that the source-controller has already published
the right artifact revision. Waiting on `GitRepository.status.artifact.revision` gives that proof.

## Resource Scope Rules

For the first implementation, only support one explicit Flux apply chain per `GitTarget`.

Recommended first cut:

- one `GitRepository`
- one or more `Kustomization` objects explicitly listed on the `GitTarget`
- wait for all listed `Kustomization` objects to acknowledge the same revision

If multiple `Kustomization` objects apply the same repo path, the handshake is complete only when every one
of them reports the expected revision and `Ready=True`.

## Proposed Product Shape

Add an opt-in Flux coordination block to `GitTarget`.

Illustrative shape:

```yaml
spec:
  flux:
    mode: ManualHandshake
    sourceRef:
      apiVersion: source.toolkit.fluxcd.io/v1
      kind: GitRepository
      name: app-source
      namespace: flux-system
    kustomizations:
      - name: app-crds
        namespace: flux-system
      - name: app-live
        namespace: flux-system
    timeout: 2m
```

This gives GitOps Reverser the exact resources it must trigger and wait on. We should not try to discover
them indirectly in the first version.

## Internal Implementation Plan

### Phase 1. Flux handshake coordinator

Add a small Flux-specific coordinator package, for example:

- `internal/flux/`

Responsibilities:

- patch `suspend` on target `Kustomization` resources
- write `reconcile.fluxcd.io/requestedAt`
- poll or watch Flux objects until the expected revision is acknowledged
- return structured progress and timeout errors

This should stay separate from the generic reconcile diff logic in `internal/reconcile/`.

### Phase 2. Pending handshake state

Track per-target pending state in memory first.

Needed fields:

- expected revision SHA
- trigger token
- target Flux source ref
- target Flux `Kustomization` refs
- started at
- timeout
- reason:
  - reverse-sync commit
  - external remote commit detected

While pending:

- do not treat Flux-caused writes as fresh new intent
- do not start another Flux handshake for the same target unless we collapse onto a newer expected revision

### Phase 3. Integrate with Git event paths

Trigger the handshake from two places:

- after a successful reverse-sync push
- after branch worker detects that remote HEAD advanced with a commit that is not ours

The existing git-side trigger point is the right place to start the second integration.

### Phase 4. Block resume on full acknowledgment

Only resume normal reverse processing after:

- source acknowledgment succeeded
- every configured `Kustomization` acknowledged the expected revision
- the apply chain is back in its intended suspended state

If timeout happens:

- keep the target clearly marked as blocked/degraded
- surface the reason in status and logs
- do not silently fall back to race-prone live behavior

## Status and Observability

Expose the handshake explicitly in `GitTarget` status.

Useful conditions:

- `FluxHandshakePending`
- `FluxSourceReady`
- `FluxApplyReady`
- `FluxHandshakeFailed`

Useful status fields:

- expected revision
- last acknowledged revision
- last trigger token
- last handshake duration
- blocked reason

Useful metrics:

- handshake duration histogram
- handshake timeout count
- external-commit-trigger count
- reverse-sync-trigger count

## Failure Handling

### Source never reaches expected revision

Treat as a source-side failure:

- `GitRepository` not ready
- webhook/poll did not fetch the new commit
- wrong branch or auth problem

Result:

- leave target blocked
- do not continue reverse processing as if Flux caught up

### Kustomization never reaches expected revision

Treat as an apply-side failure:

- apply error
- dependency not ready
- health checks failing

Result:

- leave target blocked
- keep the pending revision visible in status

### Newer revision arrives while waiting

Collapse forward, do not finish the old handshake.

Result:

- update expected revision to the newest remote head
- re-trigger the handshake
- report that the previous revision was superseded

## E2E Plan Changes

Extend the existing bi-directional e2e scenario to cover the manual handshake mode.

Minimum assertions:

1. Flux `Kustomization` starts suspended.
2. Reverse-sync push triggers source and apply.
3. Test waits for:
   - `GitRepository.status.artifact.revision == expected`
   - `Kustomization.status.lastAppliedRevision == expected`
   - both `Ready=True`
4. `Kustomization` returns to suspended state.
5. No extra reverse-sync commit appears after the handshake settles.
6. External non-ours commit triggers the same flow and remains deterministic.

The existing helper methods in `test/e2e/bi_directional_e2e_test.go` already cover most of the needed
revision checks.

## Recommendation

Implement this in two bounded steps:

1. Support `GitRepository` + `Kustomization` manual handshake only.
2. Add `HelmRelease` or other Flux apply kinds later if we still need them.

That keeps the first version focused on the exact race we already reproduced and measured.
