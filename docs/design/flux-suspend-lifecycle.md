# Flux suspend lifecycle: measured

> **design, measured, not built.** A follow-up to [`apply-trigger.md`](apply-trigger.md). What it
> would cost and buy to keep a Flux `Kustomization` suspended while it is idle and resume it only to
> apply, instead of requesting reconciles of a `Kustomization` running on a 24h interval. Measured
> 2026-10-17 on the e2e cluster. Index: [`../INDEX.md`](../INDEX.md).

The third review of #419 recommended explicit ownership: controlled Kustomizations stay suspended
while they are idle, and GitOps Reverser resumes them for each apply and suspends them again. This
page measures what that costs compared with today's request, which stamps
`reconcile.fluxcd.io/requestedAt`.

**Short answer: about 50 ms per apply, at any payload size.** Speed is not a reason to avoid suspend.
The spike did turn up behaviours that any suspend lifecycle has to handle (see
[What the timings don't show](#what-the-timings-dont-show)). One of them is a bug in the code we have
today. Suspend also gives Flux the fetch-only mode that [`apply-trigger.md`](apply-trigger.md) says it
lacks, which lets push webhooks reach the source again (see
[Prefetching the source](#prefetching-the-source)).

## Setup

- The e2e cluster (`k3d-gitops-reverser-test-e2e`) running Flux v2.9.6, with kustomize-controller and
  source-controller at v1.9.6 and their default flags (`--concurrent=10`).
- A throwaway public Gitea repository. It feeds one `GitRepository` and one `Kustomization` (both
  `interval: 24h`, `prune: true`, no `wait` or health checks) in a dedicated namespace.
- The payload is either 1 ConfigMap or 200 ConfigMaps. Every run pushes a new commit that changes
  every value.
- Timings are taken by polling through `kubectl proxy` with curl. One poll takes about 27 ms, so every
  number below is ±30 ms. The `git push` itself is not counted: the clock starts at the first Flux API
  write.
- Source line numbers refer to kustomize-controller v1.9.6,
  `internal/controller/kustomization_controller.go`.

### The two flows measured

**Request (today, [`internal/applier/flux.go`](../../internal/applier/flux.go)):**

1. Push the commit.
2. Stamp the GitRepository.
3. The source fetches a new artifact.
4. The Kustomization wakes up because the artifact changed, then builds and applies.

**Suspend lifecycle:** the Kustomization is `suspend: true` while idle.

1. Push the commit.
2. Stamp the GitRepository and wait for the artifact.
3. Patch `suspend: false`.
4. Wait for the apply to finish.
5. Patch `suspend: true` again.

## Results

Medians, in milliseconds from the first Flux write.

| Step                                                 | 1 object, request | 1 object, suspend | 200 objects, request | 200 objects, suspend |
| ---------------------------------------------------- | ----------------: | ----------------: | -------------------: | -------------------: |
| Source artifact holds the commit                     |                60 |                58 |                  122 |                  111 |
| Kustomization `lastAttemptedRevision` = commit       |                91 |  58 + 57 (resume) |                  152 |    111 + 65 (resume) |
| Kustomization applied (`lastAppliedRevision`, Ready) |           **149** |      58 + **115** |              **928** |        111 + **852** |
| Re-suspend write                                     |                 – |                10 |                    – |                   12 |
| **Total**                                            |           **149** |           **193** |              **928** |              **980** |
| Runs                                                 |                10 |                10 |                    6 |                    6 |

There was one more variant: a stamp on the Kustomization while the artifact already holds the commit
(today's refusal path, `OutcomeStampedKustomization`). Its median was **97 ms** until
`lastHandledReconcileAt` matched, over 10 runs with 1 object.

What the numbers say:

- **The suspend overhead is a fixed ~45–50 ms.** It is one extra serialized round trip: the apply can
  only start once the artifact exists, instead of being triggered by it. Re-suspending costs one write
  of about 10 ms. The overhead does not grow with the payload.
- **Build and apply dominate.** They take about 0.85 s for 200 ConfigMaps on both paths, and they are
  the same code path in Flux. On a real cluster, `wait: true`, health checks or a remote Git host add
  the same amount to both columns.
- **Our apply window closes too early, now measured.** With 200 objects, `lastAttemptedRevision`
  changes at about 150 ms, but the apply finishes at about 930 ms. Kustomize-controller writes that
  field before it builds anything (line 390). `Flux.Handled()` uses it as "finished", so today the
  protection window ends about 0.8 s too early per 200 objects, and the gap grows with payload and
  health checks. This is the review's finding #1, now with a number on it.

## What the timings don't show

1. **A suspended Kustomization ignores new artifacts.** In 16 runs with a new artifact present for 2 s,
   it never attempted the revision. This is the property that stops shared-source fan-out (the
   review's finding #3) from applying to a consumer we did not choose.
2. **Suspend does not stop an apply that is already running.** I set `suspend: true` 125 ms after
   resuming (200 objects). The apply still finished on the new revision, down to the last ConfigMap.
   Flux checks `spec.suspend` only when a reconcile starts (line 190). So the lifecycle must wait for
   completion before it re-suspends and before it trusts that nothing is applying.
3. **`requestedAt` on a suspended Kustomization is marked handled without any apply.** The token
   showed up in `status.lastHandledReconcileAt` and `lastAppliedRevision` did not move. The cause is
   the deferred `finalizeStatus` (lines 144–146 and 1335–1340): it copies the annotation on every
   return, including the "suspended" early return. **This already matters today.** If anyone suspends a
   Kustomization we manage, a Refusal stamp reports `Handled` at once and the window closes with
   nothing applied. `Handled()` should not count a token as handled while `spec.suspend` is true.
4. **Resuming is enough to trigger an apply.** It changes the generation, so it needs no `requestedAt`.
   With no new commit, a resume reverted a drifted ConfigMap in 318 ms (200 objects). That is the
   refusal re-apply for free.
5. **Resume gives an exact completion signal.** The resume patch returns generation N. Then
   `status.observedGeneration == N` matched `lastAppliedRevision == commit` to the poll in all 5 runs
   (about 800–880 ms, 200 objects). The suspended reconcile had already observed N-1, so N cannot be
   satisfied early. This signal belongs to one Kustomization: unlike a GitRepository stamp it cannot be
   satisfied by a sibling consumer. Caveat from source: `observedGeneration` is set only when Ready is
   True (line 1347) or on a terminal `Stalled` error (lines 201 and 258). A failed apply that will be
   retried leaves the generation behind, so it has to be read from `Ready=False` and `Reconciling`,
   not by waiting for the generation.
6. **Not measured, but follows from the flow:** a resume applies whatever the artifact holds at that
   moment, not necessarily the commit we checked. Anyone can stamp the GitRepository between our check
   and our resume. The lifecycle has to recheck the artifact revision immediately before resuming, and
   accept that a small race remains. Separately, whoever applies the Kustomization object itself (a
   parent Kustomization, Helm) can overwrite `spec.suspend` if its manifest sets that field.

## Prefetching the source

[`apply-trigger.md`](apply-trigger.md) removed push webhooks into Flux because "Flux has no fetch-only
mode": a new artifact makes every Kustomization reading it apply at once. Suspend changes that. A
suspended consumer ignores new artifacts (point 1). So with the Kustomizations suspended, the
GitRepository can fetch on every push and the artifact is already there when we decide to apply.

**The speed gain is small.** Prefetching takes the source fetch off the apply's critical path. That
fetch took 60–120 ms in the spike. Against a hosted Git server I'd expect somewhere between half a
second and a couple of seconds, but I didn't measure it. With a `quietPeriod` of 30 s, users won't
notice the difference.

**What it buys besides speed:**

- The lifecycle's "prepare and verify the source revision" step is usually already done when we
  decide to apply. We check the artifact and resume.
- A stale artifact stops being dangerous. Today `CatchUp` keeps the artifact current because Flux
  re-applies from whatever artifact it holds when kustomize-controller restarts. A suspended
  Kustomization doesn't apply on restart either, so that reason goes away.

**Conditions it needs:**

1. **Every consumer of that GitRepository has to be suspended by us.** That includes Kustomizations no
   `GitTarget` references and HelmReleases that take a chart from the GitRepository. A single
   unsuspended consumer brings back "every push applies at once". This needs a checked
   precondition, not a sentence in the docs. If it doesn't hold, we don't prefetch.
2. **The race from point 6 gets more frequent.** The artifact moves on every push, so the "artifact
   is still the commit we decided on" check right before resume fails more often. That check prevents
   the dangerous case: a foreign commit F lands in the artifact before our replay F+W is pushed, and a
   resume would apply F and revert W. It stays correct, because a failed check means Stale and a
   retry, but expect more retries on busy branches.
3. **Prefetch from our own receiver, not a Flux `Receiver`.** The push receiver already gets the
   push. When the worker observes one, it can also stamp the GitRepository, but only while condition
   1 holds. That keeps one webhook per repository and a single place to decide.

**Recommendation:** build prefetching as part of the suspend lifecycle, gated on condition 1. Don't
build it on today's 24h-interval setup, because there it would make Flux apply every push again.

## Is it worth it?

On speed, yes: about 50 ms per apply is noise next to the apply itself, and nobody will notice it at a
`quietPeriod` of 30 s.

Correctness is the real argument for suspend:

- A suspended consumer cannot be woken up by someone else's artifact (point 1).
- Resume plus `observedGeneration` is a completion signal that fires only when the apply has really
  finished (point 5). That fixes the review's finding #1 without guessing.

The cost is a lifecycle we have to own, and a single toggle does not cover it:

- Wait out a running reconcile before re-suspending (point 2).
- Never treat a handled token as an apply while the Kustomization is suspended (point 3).
- Recheck the artifact revision just before resuming (point 6).
- Recover after a crash when we left a Kustomization resumed.

Point 3 is a bug in the current code either way, and can be fixed before any of this is built.
