# Bi-Directional Sync with Flux: Risks, Tradeoffs, and Solution Options

## Purpose

This document describes what can go wrong when a normal GitOps loop and a reverse GitOps loop both manage
the same Kubernetes resources.

For the implementation-oriented follow-up plan, see
`docs/design/bi-directional-flux-manual-handshake-plan.md`.

In this repository, the concrete example is:

- FluxCD applies desired state from Git into the cluster
- GitOps Reverser watches live cluster objects and writes changes back to Git
- both systems operate on the same resource set

The central question is not whether this can work at all. It can.

The harder question is:

> how do we distinguish a meaningful new live-cluster intent from a controller replay of an older Git intent?

That distinction is where most of the complexity lives.

## Why People Want This

Bi-directional sync is attractive because it promises:

- emergency fixes directly in the cluster without losing Git history
- easier migration from cluster-first operations to Git-first operations
- support for teams that use both `kubectl` and Git PR workflows
- automatic backfill of live drift into version control

These are real advantages.

## Why It Is Dangerous

The downside is that there is no longer a single writer.

Once both Git and the cluster are allowed to originate changes for the same fields on the same object, the
system becomes a distributed concurrency problem rather than a simple reconciliation problem.

The main risks are:

- race conditions between controllers
- ambiguous ownership of changes
- accidental replay of stale desired state
- commit churn or endless loops
- lost updates when two changes happen close together
- user confusion about which system is "correct"

## The Concrete Flux Race We Observed

The current e2e scenario reproduced an important race:

1. Flux had already applied a Git revision where an `IceCreamOrder` said `Cone / Vanilla / Sprinkles`.
2. A user patched the live object in the cluster to `WaffleBowl / MintChip / Caramel`.
3. GitOps Reverser detected that live change and created the expected reverse-sync commit in Git.
4. Before Flux had refreshed its `GitRepository` artifact to the new commit, `kustomize-controller`
   reconciled again.
5. Because Flux still believed the older revision was desired, it re-applied `Cone / Vanilla / Sprinkles`
   into the cluster.
6. GitOps Reverser then saw that controller-authored change and created another commit.

That is a real race, not a formatting bug.

It happens because Flux has at least two relevant loops:

- `source-controller` refreshes the `GitRepository`
- `kustomize-controller` reconciles the `Kustomization`

Those loops are related, but they are not the same loop and they do not complete atomically.

## Why the Current Test Change Is Not a Full Fix

The current e2e mitigation makes the source refresh faster than the periodic apply loop. That improves the
probability that Flux sees the reverse-sync commit before it re-applies stale desired state.

This is useful for testing, but it is not a full correctness fix.

It still leaves a race window:

- a cluster change can happen just before the next Flux apply interval
- the reverse-sync commit can still land after `kustomize-controller` starts reconciling the old revision
- the stale desired state can still briefly win

In other words, changing intervals reduces the race window. It does not remove the underlying causal
ambiguity.

## General Problem Categories

### 1. Metadata and formatting drift

This is the easiest class of problem.

Examples:

- controller-added labels or annotations
- defaulted fields
- YAML formatting or key-order differences

These should be handled with sanitization and semantic comparison. They are important, but they are not the
hard part.

### 2. Stale desired-state replay

This is the most important problem in shared ownership.

A live change can be real and meaningful, but another controller may still replay an older desired revision
before the new intent has propagated through the system.

This produces:

- extra commits
- temporary reverts
- confusing history
- possible loops

### 3. Concurrent writers with different intent

A user can change Git while another user changes the cluster, or two users can change the cluster in quick
succession.

Questions that immediately appear:

- which intent wins?
- should the system merge, reject, or serialize changes?
- what if the changes touch the same field?

Without a clear policy, the outcome is non-deterministic or at least surprising.

### 4. Controller-authored writes look like user-authored writes

A reverse sync engine only sees "the object changed."

It then has to decide:

- was this a meaningful new human change?
- was it a controller replay?
- was it just defaulting or status movement?

If that attribution is wrong, loops appear quickly.

### 5. Deletion and prune races

Deletes are even harder than updates.

Examples:

- Git deletes an object while a cluster actor recreates it
- a live delete is reversed into Git while Flux still wants the object to exist
- prune removes resources that reverse sync tries to re-add

Deletes need especially strong ownership rules.

### 6. Multi-object consistency

A user may intend one logical change across multiple resources, but the cluster emits several separate
events.

Reverse sync may commit a partial state:

- object A updated
- object B not yet updated
- Git now records an intermediate state that no one really intended

## Pros of Bi-Directional Sync

- supports emergency hotfixes directly in the cluster
- can preserve Git as an audit trail even for cluster-originated changes
- helps bootstrap brownfield clusters into GitOps
- reduces friction for teams that are not ready for strict Git-only workflows

## Cons of Bi-Directional Sync

- significantly more complex mental model
- harder failure analysis
- ambiguous authority over the same fields
- more controller coupling
- more test flakiness unless causality is modeled explicitly
- can hide process problems by making every write path "sort of valid"

## Solution Strategies

Below are the main approaches, ordered roughly from easiest to strongest.

### Option 1. Reduce the race window

Examples:

- shorter Flux source refresh interval
- longer Flux apply interval
- webhook-triggered `GitRepository` refresh on push
- explicit source refresh after a reverse-sync commit
- event-driven source update if Flux supports it in the deployment model

Pros:

- simple
- low implementation cost
- helps tests a lot

Cons:

- not a correctness guarantee
- still race-prone under unlucky timing
- depends on controller behavior and cluster load

This is useful as a mitigation, not as the final architecture.

### Flux webhook-driven updates as a partial improvement

Flux does support a better variant of interval tuning:

- keep a normal non-zero `GitRepository` interval
- add a webhook `Receiver`
- trigger source refresh immediately when Git receives a push

This is better than relying only on polling because the new Git revision can become visible to Flux much
faster after a reverse-sync commit.

That helps in exactly the race we observed:

- reverse sync writes a commit
- Git sends a webhook
- Flux refreshes the source earlier
- the chance that `kustomize-controller` replays stale desired state becomes smaller

But this is still not a full fix.

Reasons:

- the `Kustomization` still has its own reconciliation behavior
- webhook delivery is not the same as atomic end-to-end acknowledgment
- stale desired-state replay can still happen if timing is unlucky
- webhook failures or delays put the system back into polling behavior

So webhook-triggered updates are a useful mitigation and probably worth documenting, but they still do not
replace explicit causal tracking or a pending-acknowledgment model.

### Flux Kustomization is not really "event-driven only"

For Flux `Kustomization`, `.spec.interval` is the reconciliation interval and is a required duration field.
In practice, this means a `Kustomization` is designed to reconcile periodically, not only on external
triggers.

There are still a few useful control patterns:

- make the interval large if you want fewer autonomous reconciles
- suspend the `Kustomization` with `.spec.suspend: true`
- reconcile on demand with `flux reconcile kustomization <name>`
- refresh source and then reconcile with `flux reconcile kustomization <name> --with-source`

Examples:

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: my-app
spec:
  interval: 24h
  suspend: false
```

or a more manual operating mode:

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: my-app
spec:
  interval: 24h
  suspend: true
```

Then a human or controller can drive it explicitly:

```bash
flux resume kustomization my-app
flux reconcile kustomization my-app --with-source
flux suspend kustomization my-app
```

This is useful operationally, but it still does not create a real "source updates only, never periodic"
mode.

Important caveats:

- while suspended, Flux will not apply new source revisions
- while suspended, drift detection and correction are paused
- once resumed, normal periodic reconciliation returns

So for a reverse-sync architecture, the least awkward control model is often:

- keep Flux intervals relatively long
- suspend a `Kustomization` during a deliberate freeze window if needed
- explicitly call `flux reconcile kustomization ... --with-source` at controlled moments

That gives more operational control, but it still does not solve the deeper shared-writer problem by
itself.

### Option 2. Blanket-ignore controller-authored writes

Example:

- ignore writes from `kustomize-controller`

Pros:

- easy to implement
- reduces churn fast

Cons:

- throws away potentially meaningful information
- brittle across environments and controllers
- can hide legitimate controller-mediated state changes
- does not actually model intent

This is usually too blunt for a general solution.

### Option 3. Suppress only known replay events

This is much stronger than blanket-ignore.

Idea:

- when GitOps Reverser creates a reverse-sync commit for resource `R`, record:
  - resource identity
  - a desired-state fingerprint
  - the commit SHA
  - a timeout window
- if a cluster change arrives shortly afterward from Flux and it matches an older desired fingerprint rather
  than a new user intent, suppress it as a replay

Pros:

- more precise than ignoring controller users
- directly targets the race we observed

Cons:

- still requires strong causal evidence
- needs durable state or in-memory coordination
- can become subtle around retries and multiple fast changes

This is a good medium-term direction.

### Option 4. Add an explicit "pending reverse-sync" handshake

Idea:

1. live cluster change arrives
2. reverse sync commits it to Git
3. system marks the resource as "pending acknowledgment"
4. while pending, replay-like cluster changes are not treated as fresh intent
5. the pending state clears only after Flux has observed and applied the new Git revision, or after timeout

This turns the shared-write problem into a state machine instead of pure event reaction.

Pros:

- directly models the propagation delay
- fits the actual behavior of Flux
- can collapse noisy intermediate events

Cons:

- more implementation complexity
- needs careful timeout and retry behavior
- requires knowing when Flux has truly acknowledged a revision

This is one of the most realistic robust solutions.

### Option 5. Track desired-state revision on the object

Idea:

- stamp managed objects with a desired-state fingerprint or Git revision annotation
- when the cluster object changes, compare the object's revision marker with the newest known Git revision
- if Flux re-applies an older revision, the reverse sync engine can prove that this is stale desired-state
  replay rather than new intent

Pros:

- explicit causality
- easier debugging
- helps both runtime logic and operator understanding

Cons:

- requires agreement on metadata that will stay on the object
- must be stable across sanitization
- may be awkward for some resources or controllers

This is promising if the project is willing to encode sync state in resource metadata.

### Option 6. Make cluster changes proposals, not authoritative writes

Idea:

- live cluster changes are detected
- they are written to Git as a proposal or queued change
- they do not become accepted desired state until Git confirms them

This can be implemented in several ways:

- separate proposal branch
- PR workflow
- staging area before merge to the main desired-state branch

Pros:

- cleanest source-of-truth story
- avoids direct shared authority on the main desired branch
- easier audit and review

Cons:

- less immediate
- more operational overhead
- not as seamless for break-glass workflows

Architecturally, this is the safest model.

### Option 7. Partition field ownership

Idea:

- Git owns some fields
- cluster-side actors own other fields
- reverse sync only writes the fields it truly owns

This is similar in spirit to server-side apply field ownership, but applied at the GitOps design level.

Pros:

- avoids some direct conflicts
- makes ownership explicit

Cons:

- only works if ownership boundaries are real and stable
- many resources do not have clean ownership boundaries
- hard to explain to users

Useful in narrow domains, but usually not enough on its own.

## Recommended Direction

If this project wants bi-directional sync to be more than a demo, the most credible direction is:

1. Keep semantic sanitization and metadata filtering.
2. Keep the test-level interval shaping because it reduces noise.
3. Add explicit causal tracking for reverse-sync commits.
4. Treat post-commit replay events as a special "pending acknowledgment" state, not as fresh intent.
5. Consider stamping managed resources with a desired revision or fingerprint if Flux-compatible metadata can
   be found.

In short:

- interval tuning helps
- replay detection helps more
- explicit acknowledgment/state-machine logic is the real fix class

## A Practical Near-Term Design

A practical implementation path could look like this:

### Phase 1. Improve observability

Record in logs and metrics:

- reverse-sync commit SHA per resource
- time from live event to Git push
- time until Flux `GitRepository` sees the new SHA
- time until Flux `Kustomization` reports that SHA
- count of replay-suppressed events

This makes the race measurable.

### Phase 2. Add pending-revision tracking

Maintain per-resource state:

- `resourceID`
- `pendingCommitSHA`
- `pendingDesiredFingerprint`
- `createdAt`
- `expiresAt`

When a follow-up cluster event appears:

- if it matches the pending desired fingerprint, ignore it as already-accepted convergence
- if it matches the previous fingerprint and comes from the Flux path, treat it as stale replay
- if it is a genuinely new fingerprint, treat it as a fresh live change

### Phase 3. Add explicit Flux acknowledgment checks

Clear pending state only when:

- Flux source has observed the new commit
- the relevant Kustomization reports reconciliation of that revision

Timeouts still need to exist so the system can recover if Flux is unhealthy.

## Design Principles

If bi-directional sync is kept, the system should follow these rules:

- do not treat every cluster write as new human intent
- do not treat every controller write as irrelevant either
- prefer semantic state comparisons over raw YAML comparisons
- model propagation delays explicitly
- keep a clear story for who owns desired state at each moment

## Recommendation for Documentation and Messaging

Bi-directional sync should be described as:

- possible
- useful in some workflows
- advanced
- higher risk than one-way GitOps

It should not be described as "just works" shared ownership.

The safest statement is:

> Git remains the durable desired-state source, and cluster-originated changes need explicit handling to avoid
> replay and loop conditions.

## Summary

The Flux race is not an edge case. It is one of the central problems in bi-directional sync.

The current interval-based mitigation is useful for e2e stability, but it does not prove correctness.

A stronger solution likely needs:

- causal tracking
- replay suppression tied to a known pending revision
- explicit acknowledgment from the forward GitOps loop

Without that, bi-directional sync will remain vulnerable to timing-sensitive stale-replay behavior.
