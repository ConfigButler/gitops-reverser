# Design: `CommitRequest` CRD

> Status: **implemented**. Namespaced CRD in API group `configbutler.ai/v1alpha1`.
> Deferred work is tracked in [design-commit-request-phase-2.md](design-commit-request-phase-2.md).
> Date: 2026-05-18

## What it is

`CommitRequest` is a small namespaced CRD that acts as a **"save now" signal**. A frontend
creates one after a user's resource edits; gitops-reverser then finalizes the open commit window
for the named `GitTarget` immediately — instead of waiting for the rolling silence timer — and
records the resulting commit SHA in the object's status. An optional `spec.message` becomes the
commit message verbatim.

It does exactly one thing:

> When the `CommitRequest`'s own audit event is consumed, finalize the open commit window for the
> authenticated author on the referenced `GitTarget`, using `spec.message` if set, and record the
> outcome in `status`.

The commit fires **on the audit event for the create, never on the API create directly** — that
is what makes the timing correct (see [The flow](#the-flow)).

## Resource shape

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: CommitRequest
metadata:
  namespace: team-a
  generateName: save-          # one fresh object per save
spec:
  gitTargetRef:
    name: team-a-config        # required; GitTarget in the same namespace
  message: "Increase checkout API memory after load-test failures"   # optional; omit for a bare "commit now"
status:
  phase: Committed             # WaitingForAuditEvent | Committed | NoOpenWindow | Failed
  branch: main                 # set when Committed
  sha: "a1b2c3d4e5f6789..."    # set when Committed
  message: ""                  # failure detail; set when Failed
  observedTime: "2026-05-18T12:34:56Z"
```

### Spec

- **`gitTargetRef.name`** (required) — the `GitTarget` whose open window to finalize. Must live
  in the `CommitRequest`'s own namespace. One target per object keeps `status` flat: a single
  SHA, no list.
- **`message`** (optional) — the commit message, used verbatim. "Optional" here means the field
  may be **omitted**, not that it may be **blank**: omitting `message` entirely is a valid
  request — it just finalizes the window with the generated grouped-commit message, which is the
  intended "trigger a commit now, no custom message" form. An explicit empty string
  (`message: ""`) is *rejected* by the apiserver, because when the field is present the CRD
  requires 1–1024 Unicode characters. Newlines are allowed (so a subject plus body works); all
  other ASCII control characters are rejected (pattern `^[^\x00-\x09\x0B-\x1F\x7F]*$`).

`spec` is immutable after creation — a CEL validation rule on the CRD (`self == oldSelf`) rejects
any update that changes it, so a delayed audit event always acts on the spec the object was
created with.

### Status

| `phase` | Meaning |
|---|---|
| `WaitingForAuditEvent` | Initial. The object was created; its audit event has not been processed yet. |
| `Committed` | Terminal. The window was finalized; `branch` and `sha` are set. |
| `NoOpenWindow` | Terminal. The audit event arrived but the author had no open window on that `GitTarget` (they saved with nothing pending). Not an error. |
| `Failed` | Terminal. The finalize could not complete — a failed commit, a saturated worker queue, a missing `GitTarget`, etc. `status.message` carries the reason. |

## The flow

The whole design is one ordered sequence. Timing is correct because the commit is driven by the
audit stream, not by the API create.

1. The frontend issues its resource mutations (`Deployment` patch, `ConfigMap` update, …) and
   awaits them.
2. The frontend creates a `CommitRequest` in its namespace, naming a `GitTarget` and optionally a
   message.
3. A minimal controller stamps `status.phase: WaitingForAuditEvent` on the new object. It does no
   other work.
4. kube-apiserver persists the object and emits an audit event for the `create`. Because the CRD
   is served by kube-apiserver itself, the event is **complete**: it carries `auditID`, `user`,
   and `objectRef` (namespace, name, uid).
5. The audit event flows through the existing audit pipeline. By audit-stream ordering, every
   mutation from step 1 produced an *earlier* event — so by the time the consumer reaches the
   `CommitRequest` event, those writes have already been applied to the open window. This is what
   makes "commit now" safe under parallel writes.
6. The audit consumer recognizes the `commitrequests` `create` event. It takes the **author** from
   the audit event's effective user, reads the `CommitRequest` object to get `spec.gitTargetRef`
   and `spec.message`, and verifies the object's `uid` matches `objectRef.uid`.
7. It finalizes the open window **for that author on that `GitTarget`**, producing the commit.
8. It writes the terminal status: `Committed` (+ `branch`/`sha`), `NoOpenWindow`, or `Failed`
   (+ `message`).
9. The frontend, watching the object, sees the terminal phase.

The audit stream's only job here is **timing** — it tells gitops-reverser "every edit before this
save has landed; commit now." The message lives in the object; the author lives in the audit
event.

## Author and target binding

A `BranchWorker` is keyed by `(provider, branch)` only, and holds at most one open window. So "the
open window" must be pinned to the request's intent:

- The **author** is the audit event's effective user (the impersonated user under impersonation).
  Anonymous and unauthenticated callers never get an object created — kube-apiserver rejects them
  first.
- The finalize signal carries `Author`, `GitTargetName`, and `GitTargetNamespace`. The worker
  finalizes the window **only if all three match** the open window. A mismatch — someone else's
  window happens to be the one open — yields `NoOpenWindow` and leaves that window untouched for
  its real author.

## Why no webhook

A stored CRD object does not record its creator, so an admission webhook was considered to capture
the creating user. It is not needed: the audit event for the `create` already carries the
authenticated `user`, and gitops-reverser already reacts to that audit event. The author comes
from the audit event — the same authenticated identity a webhook would have seen — and the message
comes from reading the persisted object. No webhook, no request stash, no extra TLS or admission
plumbing.

## How it works internally

Three pieces are net-new on top of existing machinery:

1. **Finalize signal.** `git.FinalizeSignal` is a `WorkItem` variant meaning "finalize the open
   window now." It is enqueued on the **same per-worker queue** as resource events — which is what
   makes the audit-ordering argument hold: the signal is processed after every earlier write.
   `EventRouter.FinalizeGitTargetWindow` resolves the `GitTarget` to its `(provider, branch)`
   worker, enqueues the signal, and blocks (up to 30s) for the result.
2. **Outcome reporting.** The signal carries a result channel; the worker replies with the commit
   SHA, `NoOpenWindow`, or an error. A finalize error is propagated as a Go error — never silently
   mapped to a benign phase.
3. **Status mapping.** No open window → `NoOpenWindow`. A finalize error → `Failed` with the
   reason in `status.message`. The audit consumer's `handleCommitRequest` writes the terminal
   status, retrying on optimistic-concurrency conflicts and re-checking the object `uid` each
   time.

When no worker exists for the `GitTarget` (nothing has been written to that branch yet) there is,
by definition, no open window → `NoOpenWindow`. In per-event commit mode (`commitWindow == 0`)
every event already commits immediately, so the signal simply finds no open window.

## RBAC and audit policy

- End users (or a backend acting for them) need `create` on `commitrequests` in their namespace.
  That is all.
- The gitops-reverser identity needs `get`/`list`/`watch` on `commitrequests` and `update` on
  `commitrequests/status`.
- Audit policy: `Metadata` level on `commitrequests` `create` is sufficient — the consumer needs
  `auditID`, `user`, `verb`, and `objectRef`, then reads the object body from the apiserver. The
  repo's e2e audit policy captures this via a catch-all `create` rule; shipping a dedicated
  fragment in the Helm chart is a phase-2 item.

## Edge cases

- **No open window** → terminal `NoOpenWindow`. Not an error.
- **Object deleted before its audit event is processed** → the consumer cannot read the spec; it
  logs and skips. The object is gone, so there is no status to write.
- **Object recreated under the same name** (delete + recreate before a delayed audit event) → the
  consumer compares `objectRef.uid` to the live object's `uid` and skips the stale event.
- **Audit event never arrives** (audit pipeline degraded) → the object stays
  `WaitingForAuditEvent`. Acceptable for now; the lifecycle gap this creates is the headline item
  of the phase-2 doc.
- **Transient finalize failure** (e.g. a saturated worker queue) → terminal `Failed`. The audit
  message is ACKed once with no redelivery, so a terminal phase is more honest than a silently
  stuck object. The rolling silence timer still commits the pending edits later with the generated
  message — only the caller's custom message is lost.

## Alternatives considered

The parent exploration ([idea-end-user-commit-messages.md](idea-end-user-commit-messages.md)) and
the superseded aggregated-API design
([design-commit-context-api.md](design-commit-context-api.md)) weighed several transports. They
are summarized here so the choice is not relitigated.

- **Aggregated API `CommitContext`.** A request-only aggregated API kind, nothing persisted. The
  blocker: kube-apiserver's native audit events for aggregated APIs are "hollow" — no
  `requestObject`, no `objectRef.name` — so the message text would have to be recovered from a
  Valkey "request stash" keyed by `Audit-ID`. A plain CRD is served by kube-apiserver directly, so
  **its audit events are complete**. That single fact removes the stash, the `APIService`
  registration, and the requestheader-CA / TLS plumbing. The full deep-dive on the aggregated-API
  audit gap is kept in [design-commit-context-api.md](design-commit-context-api.md).
- **Audit `user.extra` enrichment.** Put the reason in `userInfo.extra` and read it from the same
  audit event as the mutation. Cheap for gitops-reverser and naturally per-event — but ordinary
  clients cannot set `extra`. It needs an authentication webhook on the hot path, OIDC claim
  mappings, or impersonation with `Impersonate-Extra-*`. Granting a backend "impersonate any user
  and set arbitrary extras" is a heavy, alarming RBAC posture for what is essentially a commit
  message.
- **Transient annotations stripped by a mutating webhook.** Attach a
  `gitops-reverser.io/commit-message` annotation to the edited object; a mutating webhook strips
  it before persistence; the consumer recovers it from the audit payload. Per-event and atomic
  with the change — but it requires an always-on mutating webhook on the request path of every
  watched resource type, a steep operational cost for a cosmetic feature, and the intent is hidden
  inside metadata where other tools may disturb it.
- **Short-lived `CommitIntent` CRD.** This design's ancestor. The early sketch treated it as
  ephemeral "use this message for the next window, then delete it" state, which felt unnatural
  because the cleanup clock should be the commit window, not a TTL. Reframing it as an explicit,
  audited **"save now" request that reports its own status** — rather than a hidden message slot —
  is what made a CRD the right shape. That reframed CRD is `CommitRequest`.
- **Do nothing.** Keep template-generated commit messages only. The status quo; rejected because
  frontends do have real user intent at edit time and currently no clean channel for it.

`CommitRequest` wins because a CRD's audit events are complete (no stash, no webhook), the create
is an explicit auditable operation, and reporting status back on the object gives the frontend a
clean success/SHA signal.

## Deferred

Garbage collection, multi-target "save everything I edited," retry for transient finalize
failures, and outcome metrics are intentionally out of this first cut. They are tracked — with the
object-lifecycle question front and center — in
[design-commit-request-phase-2.md](design-commit-request-phase-2.md).

## References

- Deferred / phase-2 work: [design-commit-request-phase-2.md](design-commit-request-phase-2.md)
- Superseded aggregated-API design, kept for the audit-gap deep-dive:
  [design-commit-context-api.md](design-commit-context-api.md)
- Parent exploration — transport options and the audit-stream-as-source-of-truth principle:
  [idea-end-user-commit-messages.md](idea-end-user-commit-messages.md)
- Audit ingestion pipeline this design rides on:
  [design-audit-ingestion-hardening.md](design-audit-ingestion-hardening.md)
- Implementation: `api/v1alpha1/commitrequest_types.go`,
  `internal/controller/commitrequest_controller.go`, `internal/queue/commit_request.go`,
  `internal/git/finalize_signal.go` plus the `FinalizeSignal` path in
  `internal/git/branch_worker.go`, and `EventRouter.FinalizeGitTargetWindow`.
