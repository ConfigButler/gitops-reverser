# E2E Runtime State vs Stamp State

## Summary

This note explains a local-only e2e failure mode that appeared while validating the
`reverse-gitops` changes, why the existing stamp mechanism did not fully protect us, what
I think the right solution shape is, and what the tradeoffs are.

The short version is:

- the failure was real
- it was not the audit webhook listener IP
- it was caused by a mismatch between **stamp state** and **live cluster runtime state**
- the mismatch is most relevant on **reused local k3d clusters**
- the most defensible fix is to keep stamps for deterministic setup work, but add a small
  amount of **explicit runtime validation** where stamps cannot be authoritative

I do **not** think this means the stamp mechanism is wrong. I do think it was asked to
represent a class of state that it cannot reliably represent on its own.

## What Was Happening

The local failure looked like this:

- `make test-e2e` entered `BeforeSuite`
- `prepare-e2e` ran
- install artifacts were applied successfully
- controller rollout timed out
- the pod ended up in `ImagePullBackOff`

On inspection, the key facts were:

- the local image `gitops-reverser:e2e-local` existed in Docker
- `.stamps/image/controller.id` matched that local image
- `.stamps/cluster/<ctx>/image.loaded` also matched that local image
- but the current k3d nodes did **not** actually have that image in containerd
- kubelet then tried to pull `docker.io/library/gitops-reverser:e2e-local`, which of course failed

So the precise mismatch was:

- Make believed the image had already been loaded into the cluster
- the actual cluster runtime no longer had that image available on the node

That is why rollout failed even though the stamp graph looked correct.

## Why This Can Happen

The stamp mechanism is good at tracking outputs that are:

- deterministic
- created by our own recipes
- still valid as long as their inputs did not change

Examples:

- generated manifests
- rendered install YAML
- built local image ID
- an applied secret manifest

But node-local image availability is different. It is affected by runtime behavior outside
the Make graph, for example:

- node-local container image garbage collection
- manual cluster deletion/recreation
- node replacement
- partial cluster corruption
- external cleanup in Docker or k3d internals

In other words, `image.loaded` is not purely a build artifact. It is a claim about the
current state of another system.

That claim can become false without any Make input changing.

## Why The Stamp Was Not Enough

The existing stamp said:

> At some point, this image was loaded into this cluster context.

What the controller rollout actually needs is stronger:

> Right now, the node that may schedule this pod can start it from the expected image.

Those are not the same statement.

This is why the failure looked surprising:

- from the Makefile perspective, nothing was stale
- from Kubernetes' perspective, the deployment was not runnable

That gap is the central issue.

## Why This Is Mostly a Local Problem

This is mainly a reused-local-cluster problem.

CI usually starts from a fresh environment:

- new container runtime state
- new cluster
- no long-lived node image cache history
- no previously accumulated local drift

Local development is different:

- the cluster lives longer
- nodes may hit disk pressure
- images may be GC'd
- the same cluster name and stamp directory may be reused for a long time

That is why this showed up locally but not on the build server.

## Why The Webhook Listener IP Was Not The Root Cause

The original suspicion was that the audit webhook listener might be bound to the wrong IP.

That did not match the evidence:

- the controller args still showed `--audit-listen-address=0.0.0.0`
- once the controller pod actually started, audit traffic worked
- the blocking failure happened before that, at image start time

So the webhook IP was a plausible hypothesis, but it was not the root cause of the local
rollout failure we observed.

## What Is Actually Needed

I think we need to distinguish two classes of truth:

### 1. Stamp truth

This should continue to cover deterministic setup work:

- cluster bootstrap prerequisites
- rendered manifests
- install outputs
- generated secrets
- local image build identity

### 2. Runtime truth

This has to be checked live because it can drift:

- is the cluster healthy enough to trust?
- is the expected image available to the current nodes?
- did the deployment actually roll out?
- is the running controller the expected build?

The important design point is:

**Runtime truth should not replace stamps. It should sit next to them only where stamps are
not authoritative.**

## Recommended Solution

My preferred solution is this:

### A. Fail hard on unhealthy reused clusters

If `start-cluster.sh` finds an existing cluster that is unhealthy, it should stop and tell
the user to clean it up explicitly.

Reason:

- once the cluster is already unhealthy, we cannot confidently infer which cluster-scoped
  stamps are still valid
- auto-recreating the cluster is convenient, but it is also guessy
- explicit cleanup is safer and more honest

This is especially true because cluster state and `.stamps/cluster/<ctx>` are coupled by
convention, not by a strong transactional boundary.

### B. Keep `controller.deployed` self-sufficient

The deployment recipe should still ensure the image is really available before rollout.

Reason:

- if `controller.deployed` runs, it must be able to succeed on its own
- relying only on `image.loaded` is not enough when node runtime state may have drifted
- the deployment recipe is exactly the place that knows what the controller pod needs next

### C. Keep runtime revalidation explicit

If we want an always-run check for reused clusters, it should be a clearly named step such as
`ensure-runtime-ready`, not an accidental side effect hidden behind a stale-looking stamp.

That keeps the model understandable:

- stamps represent reproducible outputs
- ensure steps validate live state

### D. Add a build-info endpoint

This is a very good complement.

It would let the e2e suite verify:

- the controller is running
- the running controller is the expected build

For that endpoint I would expose:

- `version`
- `gitCommit`
- `buildDate`
- optionally `dirty`
- optionally image digest if available

I would validate on `gitCommit` or digest, not `buildDate` alone.

## Why A Build-Info Endpoint Helps

A build-info endpoint solves a different but important problem:

- stamps can say what we built
- rollout can say something is running
- build-info can say the thing that is running is the one we expected

That is stronger than today's implicit assumption.

It is especially useful for:

- local reuse of an existing deployment
- accidental drift between install modes
- debugging "did I actually run the new binary?" questions

## Implementation Revision: Trusting the Stamp via Containerd Pinning

After implementing the hybrid model above, we reconsidered the `cluster_has_project_image`
node probe in `load-image.sh` and the `ensure-runtime-ready` phony target.

The node-probe approach ran `crictl images` inside every k3d node container on each
`prepare-e2e` call to verify the image was still present in containerd. The justification was
that node-local GC could silently invalidate the stamp. We initially dismissed this as
overcomplicated — the cluster is developer-owned, k3d nodes do not arbitrarily lose images,
and if the cluster is broken `start-cluster.sh` would catch it.

**That bet was wrong.** We observed the exact failure on the next run:

- the stamp was valid — the image had been loaded into the nodes
- a code change we made invalidated the `config-dir/install.yaml` stamp, triggering an
  automatic cleanup that deleted the deployment and namespace
- with no running containers referencing the image, containerd's kubelet-driven image GC
  evicted it from all nodes within minutes
- on the next `prepare-e2e`, Make skipped the import (stamp still matched), the deploy ran,
  and rollout failed with `ImagePullBackOff`

The root cause is: **containerd's CRI plugin GCs unreferenced images on behalf of the kubelet**.
This is not a node restart or manual intervention — it is normal Kubernetes image lifecycle
behaviour. The stamp cannot know about it.

### Why we did not restore the runtime node probe

The per-node `crictl images` probe on every `prepare-e2e` call is the safe but expensive fix.
We looked for a cheaper alternative that makes the stamp itself reliable.

### The fix: pin the image after import

Containerd's CRI plugin checks for the label `io.cri-containerd.pinned=pinned` before evicting
any image. If the label is set, the image is skipped unconditionally by the GC.

After `k3d image import`, `load-image.sh` now runs:

```
ctr -n k8s.io images label <ref> io.cri-containerd.pinned=pinned
```

on every node. This is done once, at import time. The stamp remains the sole fast-path check
on subsequent runs, and is now reliable: if the stamp says the image is loaded, containerd
still has it because it is pinned.

We verified this in the cluster by inspecting the image labels after import:

```
docker.io/library/gitops-reverser:e2e-local   ...   io.cri-containerd.image=managed,io.cri-containerd.pinned=pinned
```

The simplification that was also applied at this point and is still in effect:

- removed `ensure-runtime-ready` from the `prepare-e2e` flow entirely
- kept `deploy-controller.sh` idempotent (checks spec image + available replicas before
  issuing `kubectl set image`) to avoid the unconditional 180s rollout wait on no-op runs

## What A Build-Info Endpoint Does Not Solve

It does **not** replace rollout or image-availability checks.

If the image has been GC'd and the pod cannot start:

- there is no running process
- there is no endpoint to query

So it cannot be the only guard.

It is a valuable verification layer, not a startup-recovery mechanism.

## Alternative Approaches

## Option 1: Trust stamps only

Description:

- keep current stamp graph
- do not perform live runtime validation

Pros:

- fastest in the no-op case
- simplest Makefile mental model
- no extra runtime checks or imports

Cons:

- vulnerable to exactly the class of local drift we observed
- difficult to debug when runtime state diverges from cached assumptions
- rollout failures become surprising instead of explainable

Assessment:

- acceptable only if we believe node-local image drift is effectively impossible
- the observed failure shows that belief is too strong

## Option 2: Always self-heal runtime state

Description:

- always re-check runtime state
- re-import image if missing
- possibly auto-recreate unhealthy clusters

Pros:

- robust local developer experience
- fewer manual cleanup steps
- reduced chance of confusing local failures

Cons:

- can undermine the spirit of stamps if done too broadly
- adds time to no-op or mostly-no-op flows
- may hide underlying local environment problems
- auto-repairing unhealthy clusters can guess incorrectly about which stamps remain valid

Assessment:

- useful in moderation
- too aggressive if it starts replacing stamp invalidation logic with runtime probing everywhere

## Option 3: Fail fast on any runtime mismatch

Description:

- if node image is missing, fail and instruct user to clean/reset
- if cluster is unhealthy, fail immediately

Pros:

- very clean model
- preserves stamp meaning
- surfaces environmental problems directly

Cons:

- more manual recovery for developers
- repeated local friction for recoverable situations
- harder to use in long-lived dev clusters

Assessment:

- strong from a correctness perspective
- maybe too punishing for day-to-day local iteration

## Option 4: Hybrid model

Description:

- fail hard on unhealthy cluster reuse
- keep `controller.deployed` self-sufficient for image availability
- optionally add one explicit runtime ensure step for reused-cluster no-op paths
- add build-info verification

Pros:

- clean separation between stamp truth and runtime truth
- targeted protection exactly where stamps are weak
- avoids pretending the stamp graph can represent external runtime perfectly
- easier to reason about than broad self-healing

Cons:

- still adds some runtime overhead
- slightly more complex than pure-stamp logic
- can duplicate checks if wired poorly

Assessment:

- this is the best balance in my view

## My Recommended Shape

If I were shaping this for the long term, I would recommend:

1. `start-cluster.sh` fails hard on unhealthy existing clusters
2. `controller.deployed` ensures image availability before rollout
3. `prepare-e2e` does not broadly bypass the stamp graph
4. if we keep an always-run runtime check, make it explicit and narrow
5. add a build-info endpoint and assert the running binary identity in e2e

That keeps the system honest:

- stamps are still the primary mechanism
- runtime checks cover only what stamps cannot know

## Build-Time / Runtime Cost

This definitely has a cost.

The main cost comes from image import when the runtime check decides the node image is missing.

From the observed local runs, a re-import was roughly:

- about 7 to 12 seconds for the import itself
- plus rollout time after that

If the check is wired in the wrong place, that cost can be paid more than once in one e2e
preparation path. That is real overhead and should be minimized.

So I would separate the cost discussion like this:

### Cheap path

When the image is already present and the check exits fast:

- low overhead
- mostly process startup plus a few `crictl images` calls

### Expensive path

When the image must be re-imported:

- noticeable local cost
- but that cost is still cheaper than a confusing `ImagePullBackOff` failure followed by manual debugging

## How Sure Am I That This Adds Value?

### On the existence of the problem

High confidence.

We directly observed:

- matching local image + stamp state
- missing node-local image
- `ImagePullBackOff`
- rollout recovery immediately after forced re-import

That part is not speculative.

### On the need for some runtime validation

High confidence.

The failure class is exactly one that stamps cannot fully represent.

### On the exact final shape

Medium confidence.

I am confident in the principle:

- fail hard on unhealthy clusters
- keep deployment recipes self-sufficient
- verify running build identity explicitly

I am less confident that the current arrangement of checks is the final ergonomic optimum.
There is still room to reduce duplicate imports/checks on cold rebuilds.

### On overall value

Moderate to high value for local developer correctness.

The value is highest when:

- the same local k3d cluster is reused often
- developers rely on e2e repeatedly during iteration
- debugging time matters more than a few extra seconds

The value is lower if:

- clusters are always torn down fresh
- local e2e is rare
- CI is the only trusted validation path

## Final Position

I do not think we should abandon the stamp mechanism.

I do think we should stop expecting stamps to prove live runtime facts that belong to the
cluster and node runtime.

So my final position is:

- stamps remain the primary mechanism
- unhealthy existing clusters should fail hard
- deployment/runtime checks should cover only the live state that stamps cannot guarantee
- a build-info endpoint would be an excellent next improvement to verify that the running
  controller is actually the intended build
