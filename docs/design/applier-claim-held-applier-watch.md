# Watching the held applier, and Argo CD `selfHeal` under a held switch

Status: the `selfHeal` answer is decided (leave it alone) and shipped with #437, where an
`ArgoCDClaim` holds `spec.syncPolicy.automated.enabled: false`. The follow-up at the end, watching
the held applier, is **wanted and not built**. Argo CD facts are read from Argo CD's source at 3.5.0
(`external-sources/argo-cd`, `controller/appcontroller.go`).

## The question

Should the claim also switch `selfHeal` off, so that nothing can revert an incoming edit, for
example during a sync?

## Answer: no

### `selfHeal` is part of automated sync

Self-heal is not a watcher of its own. It is a step inside `autoSync`, and `autoSync` returns before
that step when automated sync is off:

```go
// controller/appcontroller.go:2587
if app.Spec.SyncPolicy == nil || !app.Spec.SyncPolicy.IsAutomatedSyncEnabled() {
    return nil, 0
}
// ... the selfHeal check comes later, at :2668
```

So while the claim holds `enabled: false`, `selfHeal: true` has no effect, whether or not an
operation is running.

### During a sync it changes nothing

During a sync GitOps Reverser started, an incoming edit can be undone by two things:

- the sync itself applying the pinned commit over it;
- Argo CD's retries of that same pinned commit.

Neither is self-heal. The apply window and the `Repair` reason already cover them.

### It would only matter in the switch-on window, at a price

If somebody switches automated sync on, the claim notices at its next periodic read (at most
`heldClaimSteadyInterval`, one minute) and switches it off again. During that window:

| | `selfHeal` left as is | `selfHeal` also held off |
| --- | --- | --- |
| A live edit is reverted within seconds | yes, if `selfHeal: true` | no |
| A new commit, GitOps Reverser's own pushes included, is synced over pending edits | yes | yes |

So holding `selfHeal` off would reduce the harm in the window, but not remove it.

The price is the hand-back. Measured in envtest (`argocdclaim_apiserver_test.go`):

- a forced apply of a **different** value takes the field from every other owner;
- giving the field up then removes it.

A user's `selfHeal: true` would therefore come back unset, which is `false`. Restoring it would mean
storing the original value somewhere: the extra state the `enabled` switch avoids. The switch has
no such problem, because a user who had automated sync on never owned `enabled`; Argo CD reads a
missing `enabled` as on.

## Follow-up (wanted): watch the held applier, and respond at once

The window, not `selfHeal`, is the exposure. Responding at once from our side closes it for
automated sync and `selfHeal` alike, and for a `FluxClaim`'s `flux resume` too. It takes "the
claim takes over the applier" from a one-minute poll to a real control loop.

**Use cases behind it.**

- **Testing.** Today a test that switches automated sync on (or runs `flux resume`) has to request
  a reconcile, or wait up to a minute, before anything happens. With the watch the response is part
  of the behavior under test: an e2e spec can switch it on and assert it is off again within
  seconds, and the unit tests can drive the same path through the fake's watch.
- **Understanding user edits that come in during a sync.** What happens to an API edit that arrives
  while an apply runs is the hardest part of bi-directional editing: the apply window, the applier's
  echoes, `Repair`. We now learn when an applier's state changed only when we next read it. A watch
  gives exact moments: when a sync started, ended, or was started by somebody else, and when the
  switch flipped. Lined up against the times of incoming edits, that shows which edits landed during
  a sync and what became of them, in logs and in the test assertions.
- **Alerting.** The gitops-api team asked for a metric of applies the claim did not start (their
  ask #31, in [the gitops-api asks](../future/gitops-api-open-asks.md)); they saw the detection time
  vary from 2s to about 75s, which is the poll. A watch makes that metric exact.

**Now.** The applier claim reconciler reads the applier on a timer (`heldClaimSteadyInterval`, one
minute) and watches nothing in the applier's cluster. That is deliberate
(`applierclaim_controller.go`): Flux or Argo CD may not be installed there, and an informer on a
kind that is not served would be a failure to report, not a check that answers "not found".

**Proposal.** Watch an applier only while a claim holds it, so the reason for not watching does
not apply:

- **When:** start the watch when the claim puts its finalizer on (it holds the applier, so the
  kind is served and readable), and stop it when the finalizer comes off (hand-back done).
- **What:** one watch per held object, with a field selector on `metadata.name` in its namespace,
  through the `KubeProvider`'s dynamic client. A watch per held object, not a cluster-wide informer,
  keeps the RBAC at `get`/`list`/`watch` on what the claim already reads, and the cost
  proportional to what is held.
- **On an event:** enqueue the claim (a `source.Channel`, or `GenericEvent`s into the applier claim
  controller). The reconcile does what it does now: it reads, and switches off or suspends again
  with the same Event. The watch only makes the read immediate. Nothing is decided from the event
  itself.
- **Filter:** only enqueue when the held field changed (`spec.suspend`, or
  `spec.syncPolicy.automated.enabled`). An Argo CD `Application` writes its status on every
  refresh, and has no status subresource, so an unfiltered watch would reconcile the claim every
  few seconds.
- **Record, don't reconcile, the sync transitions:** a change of the applier's operation state (an
  `Application`'s `status.operationState.phase` or a new `operation`; a `Kustomization`'s handled
  `requestedAt` and `Ready`) is logged with its time, and is available for the dispatcher to follow,
  so the moments a sync started and ended sit next to the incoming edits (the second use case).
  These do not enqueue the claim.
- **Keep the timer:** the one-minute read stays as the fallback for a watch that dropped, expired
  or could not start (a `KubeProvider` whose identity lacks `watch`). A watch that cannot start is
  logged and leaves the claim on the timer; it never makes the claim not Ready.
- **Both kinds:** a Flux `Kustomization`'s `spec.suspend` gets the same treatment, so `flux resume`
  is suspended again at once, before kustomize-controller has finished its apply in most cases.

**Cost.** A watch per held applier, its lifecycle tied to the finalizer, `watch` added to the
applier grants (chart and `config/rbac`), and a test that a switch-on is undone well within the
timer. The e2e spec E (`argocd_bi_directional_e2e_test.go`) can then drop its explicit
`requestClaimReconcile`, and prove the response time instead.

Recommendation: build the watch as its own PR, for both kinds.
