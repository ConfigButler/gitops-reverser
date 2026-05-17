# Design: `ExplicitCommit` CRD

> Status: design — active. Supersedes
> [design-commit-context-api.md](design-commit-context-api.md) (kept as a reference for the
> aggregated-API transport and its audit gap). Parent exploration:
> [idea-end-user-commit-messages.md](idea-end-user-commit-messages.md).
> Date: 2026-05-17

## Summary

`ExplicitCommit` is a small namespaced CRD that acts as a **"save" signal**: a frontend creates
one after its resource edits, and gitops-reverser finalizes the matching open commit window now
instead of waiting for the silence timer. The resulting commit SHA is reported back in the
object's status.

This design deliberately starts simple and picks **one** transport — a CRD. The aggregated-API
alternative is documented in [design-commit-context-api.md](design-commit-context-api.md) and is
not pursued here.

### What changed from the predecessor design

[design-commit-context-api.md](design-commit-context-api.md) used an aggregated API whose audit
events are "hollow" (no body, no object reference), which forced a Valkey request stash to carry
the message. A CRD is served by kube-apiserver directly, so **its audit events are complete** —
they carry the author and the object reference. That removes the stash entirely, and (see
[Why no webhook](#why-no-webhook)) it also removes the need for an admission webhook. What is
left is a plain CRD plus one new branch in the audit consumer.

## Behavior

`ExplicitCommit` does exactly one thing:

> When the `ExplicitCommit`'s own audit event is consumed, finalize the matching open commit
> window(s) for the authenticated author, using the optional `spec.message` as the commit
> message, and report the resulting SHA in `status`.

The commit fires **on the audit event, never on the API create directly**. This is the only way
to get the timing right — see [The flow](#the-flow).

## Resource shape

```yaml
apiVersion: gitops-reverser.io/v1alpha1
kind: ExplicitCommit
metadata:
  namespace: team-a
  generateName: save-
spec:
  message: "Increase checkout API memory after load-test failures"   # optional, 1–1024 bytes
  gitTargetRef:
    name: team-a-config        # optional
status:
  phase: Committed             # WaitingForAuditEvent | Committed | NoOpenWindow
  commits:
    - gitTargetRef:
        name: team-a-config
      branch: main
      sha: "a1b2c3d4e5f6789..."
  observedTime: "2026-05-17T12:34:56Z"
```

- `spec.message` (optional) — commit message. Validated for length, UTF-8, and no control
  characters except `\n`. If omitted, the existing generated message is used.
- `spec.gitTargetRef.name` (optional) — restrict the commit to one `GitTarget` in the request's
  namespace. If omitted, every open window for the author within that namespace is finalized.
- `status.phase`:
  - **`WaitingForAuditEvent`** — the object was created; gitops-reverser has not yet seen its
    audit event. This is the initial state.
  - **`Committed`** — a terminal state. One or more windows were finalized; `status.commits`
    lists each `GitTarget`, branch, and SHA.
  - **`NoOpenWindow`** — a terminal state. The audit event arrived but the author had no open
    window to finalize (they pressed save with nothing pending). Not an error.

## The flow

The whole design is one ordered sequence. Timing is correct because the commit is driven by the
audit stream, not by the API create.

1. The frontend issues its resource mutations (`Deployment` patch, `ConfigMap` update, …) and
   awaits them.
2. The frontend creates an `ExplicitCommit` in its namespace, optionally with a message and a
   `gitTargetRef`.
3. gitops-reverser sets `status.phase: WaitingForAuditEvent` on the new object. (A minimal
   controller stamps this; it does no other work.)
4. kube-apiserver persists the object and emits an audit event for the `create`. Because the CRD
   is served by kube-apiserver itself, the event is complete: it carries `auditID`,
   `user.username`, and `objectRef` (namespace, name, uid). There is **no aggregated-API gap**.
5. The audit event flows through the existing audit pipeline. By audit-stream ordering, every
   mutation from step 1 produced an *earlier* audit event — so by the time the consumer reaches
   the `ExplicitCommit` event, those writes have already been applied to the open window. This
   is what makes "commit now" safe under parallel writes.
6. The audit consumer recognizes the `explicitcommits` `create` event. It takes the **author**
   from `user.username` and reads the `ExplicitCommit` object by `objectRef.namespace`/`name` to
   get `spec.message` and `spec.gitTargetRef`.
7. It finalizes the matching open window(s) for that author immediately, producing the commit(s).
8. It writes the terminal status: `Committed` with `status.commits`, or `NoOpenWindow`.
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

## Lifecycle

`ExplicitCommit` objects are short-lived. Once an object reaches a terminal phase
(`Committed` / `NoOpenWindow`), a controller garbage-collects it after a few minutes — long
enough for a frontend to read the result, short enough not to accumulate. Nothing else persists;
the commit itself is the durable artifact.

## RBAC and audit policy

- End users (or a backend acting for them) need `create` on `explicitcommits` in their
  namespace. That is all.
- The gitops-reverser identity needs `get` on `explicitcommits` and `update` on
  `explicitcommits/status`.
- Audit policy: `Metadata` level on `explicitcommits` `create` is enough — the consumer needs
  `auditID`, `user`, `verb`, and `objectRef`, then reads the object for the body. The Helm chart
  should ship this fragment.

## Edge cases

- **No open window.** Terminal `NoOpenWindow`. Not an error.
- **Object deleted before its audit event is processed.** The consumer cannot read the spec; it
  logs and skips. The object is already gone, so there is no status to write.
- **Audit event never arrives** (audit pipeline degraded). The object stays
  `WaitingForAuditEvent`; the existing audit-pipeline health metrics cover the underlying
  problem.

## Open questions

- **Commit failure.** The first version assumes the finalize/push succeeds. A `Failed` terminal
  phase (push cooldown exhausted, conflict, provider error) is a deliberate follow-up, not part
  of this simple first cut.
- **Stale `WaitingForAuditEvent`.** Should an object that never receives its audit event flip to
  a terminal `Failed`/`Expired` phase after a timeout, or is leaving it `WaitingForAuditEvent`
  acceptable? Deferred with the failure-handling question above.
- **Spec immutability.** `spec` is expected to be write-once; editing `message` after create is
  out of contract. Enforcing it would need a validating webhook — left as convention for now.
- **GC window.** The exact retention for terminal objects (a few minutes vs. configurable).
- **Multiple windows.** With no `gitTargetRef`, several windows can be finalized at once; the
  shape (`status.commits[]`) already covers it — confirm the renderer handles per-window
  messages sensibly.

## References

- Predecessor design — aggregated-API transport, native audit gap, request stash:
  [design-commit-context-api.md](design-commit-context-api.md) (superseded by this doc).
- Parent exploration — transport options, audit-stream-as-source-of-truth principle,
  "commit now" risks: [idea-end-user-commit-messages.md](idea-end-user-commit-messages.md).
- Audit ingestion pipeline this design rides on:
  [design-audit-ingestion-hardening.md](design-audit-ingestion-hardening.md).
