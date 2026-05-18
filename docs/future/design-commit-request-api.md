# Design: `CommitRequest` CRD

> Status: **implemented**. Supersedes
> [design-commit-context-api.md](design-commit-context-api.md) (kept as a reference for the
> aggregated-API transport and its audit gap). Parent exploration:
> [idea-end-user-commit-messages.md](idea-end-user-commit-messages.md).
> Date: 2026-05-17
>
> **Implementation note:** the CRD ships under the project's existing API group,
> so the real `apiVersion` is `configbutler.ai/v1alpha1` (not the illustrative
> `gitops-reverser.io` used in the examples below). Pieces: `api/v1alpha1/commitrequest_types.go`,
> `internal/controller/commitrequest_controller.go` (stamps the initial phase),
> `internal/git/finalize_signal.go` + the `FinalizeSignal` branch in
> `internal/git/branch_worker.go`, `EventRouter.FinalizeGitTargetWindow`, and the
> `commitrequests` branch in `internal/queue/commit_request.go`.

## Summary

`CommitRequest` is a small namespaced CRD that acts as a **"save" signal**: a frontend creates
one after its resource edits, and gitops-reverser finalizes the open commit window for the named
`GitTarget` now instead of waiting for the silence timer. The resulting commit SHA is reported
back in the object's status.

This design deliberately starts simple: one transport (a CRD), one target per object. The
aggregated-API alternative is documented in
[design-commit-context-api.md](design-commit-context-api.md) and is not pursued here.

### What changed from the predecessor design

[design-commit-context-api.md](design-commit-context-api.md) used an aggregated API whose audit
events are "hollow" (no body, no object reference), which forced a Valkey request stash to carry
the message. A CRD is served by kube-apiserver directly, so **its audit events are complete** —
they carry the author and the object reference. That removes the stash entirely, and (see
[Why no webhook](#why-no-webhook)) it also removes the need for an admission webhook. What is
left is a plain CRD plus one new branch in the audit consumer.

## Behavior

`CommitRequest` does exactly one thing:

> When the `CommitRequest`'s own audit event is consumed, finalize the open commit window for
> the authenticated author on the referenced `GitTarget`, using the optional `spec.message` as
> the commit message, and report the resulting SHA in `status`.

The commit fires **on the audit event, never on the API create directly**. This is the only way
to get the timing right — see [The flow](#the-flow).

## Resource shape

```yaml
apiVersion: gitops-reverser.io/v1alpha1
kind: CommitRequest
metadata:
  namespace: team-a
  generateName: save-
spec:
  gitTargetRef:
    name: team-a-config        # required
  message: "Increase checkout API memory after load-test failures"   # optional, 1–1024 bytes
status:
  phase: Committed             # WaitingForAuditEvent | Committed | NoOpenWindow
  branch: main
  sha: "a1b2c3d4e5f6789..."
  observedTime: "2026-05-17T12:34:56Z"
```

- `spec.gitTargetRef.name` (**required**) — the `GitTarget` whose open window to finalize. Must
  be in the request's namespace. One target per object keeps the status flat — a single SHA, no
  list.
- `spec.message` (optional) — commit message. Validated for length, UTF-8, and no control
  characters except `\n`. If omitted, the existing generated message is used.
- `status.phase`:
  - **`WaitingForAuditEvent`** — the object was created; gitops-reverser has not yet seen its
    audit event. This is the initial state.
  - **`Committed`** — terminal. The window was finalized; `status.branch` and `status.sha` are
    set.
  - **`NoOpenWindow`** — terminal. The audit event arrived but the author had no open window on
    that `GitTarget` (they pressed save with nothing pending). Not an error.

## The flow

The whole design is one ordered sequence. Timing is correct because the commit is driven by the
audit stream, not by the API create.

1. The frontend issues its resource mutations (`Deployment` patch, `ConfigMap` update, …) and
   awaits them.
2. The frontend creates a `CommitRequest` in its namespace, naming a `GitTarget` and
   optionally a message.
3. gitops-reverser sets `status.phase: WaitingForAuditEvent` on the new object. (A minimal
   controller stamps this; it does no other work.)
4. kube-apiserver persists the object and emits an audit event for the `create`. Because the CRD
   is served by kube-apiserver itself, the event is complete: it carries `auditID`,
   `user.username`, and `objectRef` (namespace, name, uid). There is **no aggregated-API gap**.
5. The audit event flows through the existing audit pipeline. By audit-stream ordering, every
   mutation from step 1 produced an *earlier* audit event — so by the time the consumer reaches
   the `CommitRequest` event, those writes have already been applied to the open window. This
   is what makes "commit now" safe under parallel writes.
6. The audit consumer recognizes the `commitrequests` `create` event. It takes the **author**
   from `user.username` and reads the `CommitRequest` object by `objectRef.namespace`/`name` to
   get `spec.gitTargetRef` and `spec.message`.
7. It finalizes the open window for that author on that `GitTarget` immediately, producing the
   commit.
8. It writes the terminal status: `Committed` with `branch`/`sha`, or `NoOpenWindow`.
9. The frontend, watching the object, sees the terminal phase and the SHA.

The audit stream's only job here is **timing** — it tells gitops-reverser "every edit before
this save has landed; commit now." It is not a body transport (the body is in the object) and
not the identity source of last resort (the audit event itself carries the author).

## Why no webhook

The predecessor design considered an admission webhook to capture the creating user, because a
stored CRD object does not record its own creator. It is not needed here: the **audit event for
the `create` already carries `user.username`**, and gitops-reverser is already going to react to
that audit event (step 6). So the author comes from the audit event — the same authenticated
identity an admission webhook would have seen — and the message comes from reading the persisted
object. No webhook, no stash, no extra TLS or admission plumbing.

Author binding follows the existing rule: the author is the effective user (the impersonated
user when impersonation is used). Anonymous and unauthenticated callers are rejected by
kube-apiserver before an object is ever created.

## Implementation seam

Most of this design reuses existing machinery, but three pieces are net-new. They are called
out here so a fresh implementation context does not have to rediscover them.

A `BranchWorker` is keyed by **(provider, branch)** and holds exactly **one** open window at a
time ([branch_worker.go](../../internal/git/branch_worker.go)). The window finalizes on its own
when an event with a different author/target arrives, when the byte cap trips, or when a timer
fires — `finalizeOpenWindow()` is internal to the event loop and has no public caller.

1. **A force-finalize signal.** Add a `WorkItem` / `WriteRequest` variant meaning "finalize the
   open window now" and route it `EventRouter`
   ([event_router.go](../../internal/watch/event_router.go)) → `BranchWorker.eventQueue` → a new
   branch in `handleQueueItem` that calls `finalizeOpenWindow()` (then `maybeSchedulePush()`).
   Enqueuing it on the **same per-worker queue** as resource events is what makes the timing
   argument in [The flow](#the-flow) hold — the signal is processed in audit order, after every
   earlier write.
2. **SHA reporting.** The worker tracks `lastCommitSHA` but has no way to return the SHA
   produced by a *specific* signal. Add a result callback (or channel) on the signal so the
   commit SHA flows back to whoever writes `CommitRequest/status`.
3. **`NoOpenWindow` detection.** Falls out for free: if `openWindow == nil` when the signal is
   dequeued, there was nothing to commit → terminal `NoOpenWindow`.

Because the signal rides the per-worker queue, "the open window for that author" is **not a
lookup** — it is simply whichever window is open when the signal is dequeued, which by audit
ordering is the author's own. The per-event-commit mode (`commitWindow == 0`) is a no-op case:
every event already commits, so the signal just finds `openWindow == nil`.

## RBAC and audit policy

- End users (or a backend acting for them) need `create` on `commitrequests` in their
  namespace. That is all.
- The gitops-reverser identity needs `get` on `commitrequests` and `update` on
  `commitrequests/status`.
- Audit policy: `Metadata` level on `commitrequests` `create` is enough — the consumer needs
  `auditID`, `user`, `verb`, and `objectRef`, then reads the object for the body. The Helm chart
  should ship this fragment.

## Edge cases

- **No open window.** Terminal `NoOpenWindow`. Not an error.
- **Object deleted before its audit event is processed.** The consumer cannot read the spec; it
  logs and skips. The object is already gone, so there is no status to write.
- **Audit event never arrives** (audit pipeline degraded). The object stays
  `WaitingForAuditEvent`. That is acceptable for now — the existing audit-pipeline health
  metrics cover the underlying problem.

## Deliberately not in this first cut

Kept here with their reasoning so a later pass does not relitigate them:

- **No garbage collection.** Terminal `CommitRequest` objects are left in place for now.
  Cleanup (a TTL controller) is a follow-up; it is not needed to prove the design out.
- **No admission webhook.** Not needed at all — see [Why no webhook](#why-no-webhook). The audit
  event supplies the author, so a webhook would only add TLS and admission plumbing for nothing.
- **One `GitTarget` per object.** `spec.gitTargetRef` is required precisely so the status stays
  flat (one SHA, no list) and the matching logic stays trivial. A bare "save everything I
  edited" form is a future extension, not a first-cut requirement.
- **No `Failed` phase.** The first version assumes the finalize/push succeeds. A `Failed`
  terminal phase (push cooldown exhausted, conflict, provider error) is a deliberate follow-up.
- **No spec immutability enforcement.** `spec` is write-once by convention; editing `message`
  after create is out of contract. Enforcing it would need a validating webhook — left as
  convention for now.

## References

- Predecessor design — aggregated-API transport, native audit gap, request stash:
  [design-commit-context-api.md](design-commit-context-api.md) (superseded by this doc).
- Parent exploration — transport options, audit-stream-as-source-of-truth principle,
  "commit now" risks: [idea-end-user-commit-messages.md](idea-end-user-commit-messages.md).
- Audit ingestion pipeline this design rides on:
  [design-audit-ingestion-hardening.md](design-audit-ingestion-hardening.md).
