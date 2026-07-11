# Phase 2: `CommitRequest` — deferred work

> Status: proposed. Follow-up to [design-commit-request-api.md](../spec/commitrequest-design.md).
> Date: 2026-05-18

The first cut of `CommitRequest` is intentionally minimal. This doc tracks what was deliberately
left out, with enough analysis that phase 2 does not start cold. The object-lifecycle question is
the largest gap and is treated first.

## 1. Object lifecycle — when are `CommitRequest`s deleted?

This is the biggest open question. Today **nothing deletes `CommitRequest` objects.** Every save
leaves a permanent object in the namespace. With `generateName` and a frontend that saves often,
a namespace accumulates them without bound.

There are two distinct populations, and they need different answers.

### a. Terminal objects (`Committed` / `NoOpenWindow` / `Failed`)

These have done their job. They are useful only as a short-lived receipt the frontend reads once.
Options:

- **TTL-after-finished controller**, modeled on Job's `ttlSecondsAfterFinished`. A controller
  deletes terminal objects a configurable interval after `status.observedTime`. Owns the clock
  server-side and survives a frontend that never comes back. Recommended.
- **Frontend deletes after reading status.** Pushes the clock to the client. Clean when the
  client is well-behaved; leaks on every crashed or forgetful client. Not sufficient on its own.
- **Owner references.** GC would cascade if a save were owned by some parent object — but there
  is no natural owner here, so this does not really apply.

### b. Objects stuck in `WaitingForAuditEvent`

This is the subtle one. If the audit pipeline is degraded or the audit event is dropped, the
object **never reaches a terminal phase** — so a TTL keyed on "terminal + `observedTime`" will
never collect it. These objects are both a leak *and* a silent failure: the user's save did
nothing and nothing says so.

A TTL-after-finished controller must therefore be paired with a **max-age sweep**: any
`CommitRequest` older than some bound that is still `WaitingForAuditEvent` should be transitioned
to `Failed` ("audit event never observed") and then become eligible for normal terminal cleanup.
The bound must be safely longer than worst-case audit-pipeline lag.

### Decisions phase 2 must make

- Who owns the delete (a TTL controller — recommended) and the default TTL.
- The separate max-age for stuck `WaitingForAuditEvent`, and whether it flips the object to
  `Failed` or just deletes it. Flipping to `Failed` is preferable — it stays observable.
- Whether the TTL is global (a Helm value) or per-object (a `spec.ttlSecondsAfterFinished`-style
  field), or both.
- Whether the controller emits a metric/event when it reaps a stuck `WaitingForAuditEvent` object,
  so the underlying audit-pipeline problem is visible (see §4).

## 2. Retry for transient finalize failures

Today any finalize error — including the transient `ErrFinalizeQueueFull` — becomes a terminal
`Failed`, and the caller's custom message is lost (the silence timer still commits the edits later
with a generated message). For a saturated-queue case specifically, a bounded retry before giving
up would preserve the custom message.

This needs care: the audit message is ACKed exactly once with no redelivery, so a retry must
happen within the consumer's processing of that one event, or via an explicit requeue mechanism.
Worth doing only if queue-full turns out to be common in practice — gate it on the metrics from
§4.

## 3. Multi-target "save everything I edited"

`spec.gitTargetRef` is required precisely so `status` stays flat (one SHA, no list). A bare
"finalize every open window I have in this namespace" form is a plausible UX, but it makes
`status` a list and the matching logic non-trivial. Deferred until there is a concrete need.

## 4. Observability

There are no metrics specific to `CommitRequest` yet. Phase 2 should add at least:

- a counter of outcomes by phase (`committed` / `no_open_window` / `failed`);
- a counter (or gauge) of objects reaped while still `WaitingForAuditEvent` — the
  audit-pipeline-degraded signal from §1b;
- finalize latency (object creation → terminal phase).

## 5. Helm: ship an audit-policy fragment

The consumer only needs `Metadata`-level audit on `commitrequests` `create`. The repo's e2e
policy captures this via a catch-all `create` rule, but operators applying their own restrictive
audit policy have to derive the rule themselves. The chart should ship a recommended fragment, or
document it prominently, so `CommitRequest` works on a cluster with a narrow audit policy.

## 6. Smaller items

- **Cluster-scoped use.** `CommitRequest` is namespaced only. No current need for a cluster-scoped
  variant; noted for completeness.

## Suggested order

1. **Lifecycle** — the TTL-after-finished controller **and** the stuck-`WaitingForAuditEvent`
   max-age sweep (§1). This is the only item that is a correctness / operational problem rather
   than a nicety.
2. **Outcome metrics** (§4) — needed to even know whether the retry work in §2 is worth doing.
3. **Audit-policy fragment** in the Helm chart (§5).
4. Revisit **retry** (§2) and **multi-target** (§3) only if real usage asks for them.
