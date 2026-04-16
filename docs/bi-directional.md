# Bi-Directional Usage Guide

## Summary

GitOps Reverser can participate in a bi-directional workflow, but only if the write authority is explicit.

This area is still experimental. The first results are promising, but it needs careful design and
operational discipline before it can be treated as a normal default workflow.

The safe default is:

- cluster changes happen through the Kubernetes API
- GitOps Reverser writes those changes back to Git
- Flux or another GitOps controller applies that exact revision in a controlled acknowledgment step

What is not safe is letting GitOps Reverser and a normal always-on GitOps loop continuously reconcile the
same resources at the same time.

It also requires some form of alignment with the normal GitOps operator in the loop, whether that is
Flux, Argo CD, or another controller. Without that coordination model, the system is very easy to
make noisy or unstable.

## Recommended Modes

Use one of these operating models:

### 1. Audit only

- GitOps Reverser captures live changes into Git
- no other controller writes the same path back into the cluster

Best for:

- audit trails
- brownfield discovery
- teams that are not ready for strict GitOps yet

### 2. Human in the loop

- users make a live change in the cluster
- GitOps Reverser commits it
- a human reviews, merges, or promotes the change later

Best for:

- hotfix capture
- migration from cluster-first operations to Git workflows

### 3. Split ownership

- shared application resources stay API-first
- infra or platform resources stay Git-first
- the same fields are not owned by both loops

Best for:

- teams that want both GitOps and interactive operations without shared write ownership

### 4. Controlled bi-directional mode

- GitOps Reverser writes a commit
- the GitOps controller is triggered deliberately
- GitOps Reverser waits until that exact revision is acknowledged

Best for:

- advanced teams that need shared-path workflows and are willing to accept operational complexity
- experiments and tightly controlled rollouts, not broad default adoption yet

## What To Avoid

Do not treat this as:

- "turn on Flux and GitOps Reverser for the same path and walk away"
- "Git is desired state and the cluster is also desired state"
- "two controllers can compete and sanitization will solve it"

Sanitization helps with metadata noise. It does not solve stale desired-state replay or concurrent writers.

## Why Shared Automatic Ownership Breaks Down

The core problem is causality, not YAML formatting.

Example failure mode:

1. Flux has already applied revision `A`.
2. A user changes the live object in the cluster.
3. GitOps Reverser writes revision `B` with that new intent.
4. Before Flux has refreshed its source to `B`, it reconciles again from revision `A`.
5. Flux replays stale desired state into the cluster.
6. GitOps Reverser sees that replay as another live change and may commit again.

That produces:

- confusing history
- stale reverts
- extra commits
- possible loops

This is why the system needs one write path and one acknowledgment path, not two fully autonomous loops.

## Flux Recommendation

For shared paths, treat Flux as a manually triggered applier.

Recommended shape:

- keep the Flux `GitRepository`
- suspend the Flux `Kustomization` for the shared path
- refresh the source on demand
- reconcile the `Kustomization` on demand
- wait until Flux reports that it applied the expected commit SHA
- suspend the `Kustomization` again if that is the steady-state policy

This turns the workflow into:

- API is the interactive write path
- GitOps Reverser is the Git publisher
- Flux is the explicit acknowledgment step

That is a much safer model than two always-on reconcilers.

## Practical Platform Guidance

If you are evaluating this as a platform engineer, start with these rules:

- prefer audit-only or split-ownership mode first
- do not let shared resources have two autonomous reconciliation loops
- make the authoritative write path obvious to operators
- document who is allowed to make live changes and when
- keep rollback expectations clear: Git history is useful, but replay timing still matters
- test remote-moved, delete, and controller-restart scenarios before calling the workflow production-ready

Questions to answer before rollout:

- which resources are API-first and which are Git-first?
- who approves live-cluster hotfixes?
- what should happen when the remote branch advances outside GitOps Reverser?
- how will operators detect a stuck or timed-out acknowledgment?
- what metrics and alerts prove the workflow is healthy?

## Practical Engineering Guidance

If you are evaluating this as a Go or controller engineer, focus on:

- idempotency across repeated watch events
- semantic comparison rather than YAML text comparison
- sanitization of controller-added operational metadata
- explicit tracking of pending acknowledgments
- deterministic handling of "remote moved" conditions
- timeout, status, and degraded-mode behavior when Flux does not acknowledge a revision

Good implementation boundaries:

- keep Flux-specific coordination isolated from generic Git write logic
- expose handshake progress in status instead of only in logs
- make replay suppression depend on observed revisions, not on timing guesses alone

## Current Status In This Repository

Today the repository already supports the foundation for this direction:

- reverse writes are optimized for frequent live changes
- the Git path notices when the tracked branch moved externally
- e2e coverage exists for a controlled shared-resource scenario
- known Flux operational metadata is sanitized so tests focus on meaningful diffs

What is not complete yet:

- a stable, well-shaped product story for bi-directional usage
- a finished first-class product surface for manual Flux acknowledgment
- equivalent alignment patterns for Argo CD or other GitOps operators
- HA support for the controller
- full production hardening for all shared-ownership edge cases

## Suggested Rollout Path

Adopt the feature in this order:

1. Run GitOps Reverser in audit-only mode.
2. Move to human-in-the-loop hotfix capture.
3. Use split ownership for real environments.
4. Add controlled bi-directional mode only for paths that truly need it.

This keeps the simplest and most understandable workflows as the default.

## References

For the concrete exercised behavior, see:

- [`../test/e2e/bi_directional_e2e_test.go`](../test/e2e/bi_directional_e2e_test.go)

For broader controller and repository lifecycle background, see:

- [`design/gittarget-lifecycle-and-repo-architecture.md`](design/gittarget-lifecycle-and-repo-architecture.md)
