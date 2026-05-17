# Design: `ExplicitCommit` API

> Status: design — active. Supersedes
> [design-commit-context-api.md](design-commit-context-api.md) (kept as a reference for the
> aggregated-API audit gap). Parent exploration:
> [idea-end-user-commit-messages.md](idea-end-user-commit-messages.md).
> Date: 2026-05-17

## What changed and why

[design-commit-context-api.md](design-commit-context-api.md) designed a kind called
`CommitContext` whose first action, `setMessage`, attached free text to an open commit window
*asynchronously*: the handler stashed the request body in Valkey, the audit event flowed through
the pipeline, and the audit consumer recovered the body from the stash and attached it. The
audit stream was the body transport, which is why that design needed the `Audit-ID` stash and a
whole [section](design-commit-context-api.md#native-audit-gap-and-the-local-request-stash) on the
native audit gap.

This design makes two changes that collapse most of that machinery:

1. **The primary behavior is "commit the open window now."** The resource is a *save signal*,
   not a message-attach. It finalizes the matching open commit window immediately instead of
   waiting for the silence timer. An optional message rides along as the commit message.
2. **The resource reports the resulting commit SHA in its status.** Because the request *causes*
   a commit, it has something concrete to report back.

Once the request directly causes a commit, the handler/controller must act on the request itself
— it cannot just emit an audit event and forget. That means the body no longer has to travel
through the audit pipeline, so **the Valkey request stash is gone**. The body is read straight
off the request (aggregated API) or off the stored object (CRD). The audit stream keeps one
narrow job — see [What "immediate" means](#what-immediate-means).

The kind is renamed `ExplicitCommit` to match what it does. `CommitContext` described
context-attachment; this resource is an explicit, audited commit trigger.

## Behavior

`ExplicitCommit` does exactly one thing in v1alpha1:

> For the authenticated author, finalize the matching open commit window(s) now, using the
> optional `spec.message` as the commit message, and report the resulting commit SHA(s).

- It is a "save" / "I'm done editing" signal a frontend sends after its resource mutations.
- It does **not** create durable project state. The object (or request) is a short-lived RPC.
- It does **not** replace template-driven commit messages — if `spec.message` is omitted, the
  existing generated message is used.

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
  phase: Committed             # Pending | Committed | NoChanges | Failed
  commits:
    - gitTargetRef:
        name: team-a-config
      branch: main
      sha: "a1b2c3d4e5f6789..."
  observedTime: "2026-05-17T12:34:56Z"
```

- `spec.message` (optional, 1–1024 bytes) — commit message. Validated for length, UTF-8, and no
  control characters except `\n`. If omitted, the generated message is used.
- `spec.gitTargetRef.name` (optional) — restricts the commit to one `GitTarget` in the request's
  namespace. If omitted, every open window for the author whose `GitTarget` is in that namespace
  is finalized. Cross-namespace references are rejected.
- `status.phase` — `Pending` (accepted, commit not yet done), `Committed`, `NoChanges` (no open
  window / empty window — not an error), `Failed` (commit or push failed).
- `status.commits[]` — one entry per finalized window, each with its `GitTarget`, branch, and
  resulting SHA. A bare `ExplicitCommit` with no `gitTargetRef` can finalize several windows and
  therefore report several SHAs.

No `action` discriminator in v1alpha1: the kind *is* the action. Future audited actions that are
not "commit now" (e.g. propose-as-PR) should be their own kind rather than overloading this one —
see [Out of scope](#out-of-scope).

## Two transport options

The same resource shape works over two transports. They differ in where the object lives, how
the author is proven, and how the SHA gets back to the caller.

### Option A — dedicated CRD

A real namespaced CRD, `explicitcommits.gitops-reverser.io`, stored in etcd, with a `status`
subresource. gitops-reverser runs a controller that reconciles it.

- **Author** — a CRD object does not natively record its creator. A **mutating admission
  webhook scoped to `explicitcommits` / `create` only** stamps the authenticated
  `AdmissionRequest.userInfo` into a controller-owned field the user cannot set. This is the
  same authenticated identity kube-apiserver would put on the audit event, so attribution is
  just as trustworthy. The webhook is one Kind, create-only — far lighter than the
  "webhook on every watched type" that [idea-end-user-commit-messages.md](idea-end-user-commit-messages.md)
  rejected.
- **Trigger** — the controller reconciles the object on create. It sets `phase: Pending`,
  finalizes the matching window(s) (see [What "immediate" means](#what-immediate-means)), then
  writes `status.commits` and the terminal `phase`.
- **SHA reporting** — native: the `status` subresource. The frontend `watch`es the object until
  `phase` is terminal. Slow commits (push cooldown, conflict replay) are handled gracefully —
  `phase` simply stays `Pending` longer.
- **Lifecycle** — the object is persisted, so it needs cleanup. The controller GCs objects a few
  minutes after they reach a terminal phase (TTL-after-finished). Spec is treated as immutable
  after create; the validating side of the webhook rejects spec edits.

### Option B — aggregated API

A request-only aggregated API, `ExplicitCommit` served by gitops-reverser, registered with an
`APIService` — the shape from [design-commit-context-api.md](design-commit-context-api.md), minus
the stash. Nothing is persisted.

- **Author** — read directly from the front-proxy authentication headers
  (`X-Remote-User` etc.) that kube-apiserver injects into proxied aggregated requests, signed by
  the requestheader CA. No audit correlation, no stash.
- **Trigger** — the handler runs in the controller pod and drives the commit directly.
- **SHA reporting** — the response body only. To return a SHA the handler must **block** until
  the commit completes, then return `201` with `status.phase: Committed`. If the commit is slow,
  it either keeps blocking (long request) or returns `202` with `phase: Pending` and **no SHA**
  — and because nothing is persisted there is nothing to poll afterwards.
- **Lifecycle** — none. No cleanup, no GC.

## What "immediate" means

"Commit the open window now" has one race that must be designed for: when the frontend sends
`ExplicitCommit`, its earlier resource mutations may **not yet have landed in the window**. A
`200` from kube-apiserver on a `Deployment` patch only means the object was persisted — the
audit event for that patch still has to travel through the audit pipeline before the write
appears in a `BranchWorker` window. Finalizing the window the instant the request arrives would
commit a window that is missing the last edits.

The fix reuses ordering the system already trusts: **audit stream order**.

`ExplicitCommit` (CRD create or aggregated request) is itself an audited API call. Every resource
mutation the frontend issued *before* it produced an audit event *earlier* in the stream. So when
the audit consumer reaches the `ExplicitCommit`'s own audit event, every earlier resource event
for that author has already been consumed and applied to the window. The trigger therefore fires
**when the `ExplicitCommit`'s own audit event drains**, not when the HTTP request returns.

This is the audit stream's *only* remaining job here — a timing barrier, not a body transport.
`Metadata`-level audit is enough; the consumer needs just `auditID`, `user`, `verb`,
`objectRef`, and `responseStatus`. No request body, no stash.

- **CRD** — the controller sets `phase: Pending` on create and finalizes the window when the
  matching audit event drains. The latency (audit lag + commit) is absorbed by the async status.
- **Aggregated API** — the handler must wait for its own `auditID` to drain before it can commit
  and return a SHA. That couples request latency to audit-pipeline lag. A timeout fallback to
  `202` is the escape hatch, at the cost of the SHA.

A simpler, less precise alternative exists: skip the audit-event anchor and just wait a fixed
drain delay (≈ one `commitWindow`) for the author's events to settle. It avoids teaching the
consumer about `ExplicitCommit` at all, but it is a guess rather than a guarantee. Prefer the
audit-anchored trigger; fall back to the fixed delay only if the consumer hook is too invasive.

## Shared semantics

- **Matching windows.** Author equals the authenticated user; if `spec.gitTargetRef` is set, the
  `GitTarget` must match; the window must be open. Multiple matches → multiple commits, one
  `status.commits` entry each. This keeps "one save applies to everything I just edited."
- **No open window / empty window.** `phase: NoChanges`, no `commits`. Not an error — the user
  pressed save with nothing pending.
- **Author binding.** The author is the authenticated identity (effective user under
  impersonation), never a spec field. Anonymous or unauthenticated callers are rejected by
  kube-apiserver before the handler/controller ever sees them.
- **Commit failure.** Push cooldown, conflict, or provider error → `phase: Failed` with a
  `status` message; the CRD can be retried by creating a new object. This is unchanged commit
  behavior, just surfaced.
- **Audit policy.** `Metadata` level on `explicitcommits` `create` is sufficient (timing
  barrier only). The Helm chart should ship the fragment.
- **RBAC.** End users (or a backend on their behalf) need `create` on `explicitcommits` in their
  namespace. Nothing else for v1alpha1.

## Comparison

| Aspect | Dedicated CRD | Aggregated API |
|---|---|---|
| Infra to register | CRD + scoped create-only mutating webhook | `APIService` + TLS / requestheader CA wiring |
| Persistence | Object in etcd; needs TTL GC | None — request-only |
| Author attribution | `AdmissionRequest.userInfo` via webhook | Front-proxy headers on the proxied request |
| SHA reporting | `status` subresource, async; frontend `watch`es | Response body only; handler must block to fill it |
| Slow commit (push cooldown) | Graceful — `phase` stays `Pending` | Handler blocks (long request) or drops the SHA |
| History / observability | `kubectl get explicitcommits`, watchable | Ephemeral; nothing to list |
| Valkey stash | Not needed | Not needed |
| Audit stream role | Timing barrier only | Timing barrier only |

## Leaning

**Lean toward the dedicated CRD.** The feature now genuinely wants observable status — the
commit SHA — and a CRD `status` subresource is the native home for it: the frontend creates the
object and `watch`es until `phase` is terminal. The CRD also absorbs audit-pipeline lag and
push-cooldown latency without a multi-second blocking HTTP call, needs no APIService/TLS
plumbing, and its only extra cost (a create-only webhook on a single Kind) is small.

Choose the aggregated API instead if a **truly synchronous** save→SHA round-trip inside one API
call is a hard product requirement, or if persisting short-lived objects in etcd is unacceptable
in the target environment. Note that even the aggregated API cannot fully escape audit-pipeline
lag — see [What "immediate" means](#what-immediate-means).

## Out of scope

- **Other audited actions** (propose-as-PR, label/link attachment). With `ExplicitCommit` named
  for what it does, these no longer fold into a discriminator. They should be their own kinds if
  pursued. The PR-proposal sketch in [design-commit-context-api.md](design-commit-context-api.md#proposeaspullrequest)
  still reads as useful background.
- **Per-event commit messages.** Audit-carried transports (`user.extra`, stripped annotations)
  remain the better tool for a one-message-per-resource-event mode; see
  [idea-end-user-commit-messages.md](idea-end-user-commit-messages.md).
- **Persisted message history.** `ExplicitCommit` is a request, not a record. The commit itself
  is the durable artifact.

## Open questions

- **Aggregated-API blocking budget.** If Option B is chosen, what is the max time the handler
  blocks before falling back to `202`? It must cover worst-case audit lag plus a commit.
- **CRD GC window.** How long to keep a terminal `ExplicitCommit` before GC — long enough for a
  frontend that polls instead of watching, short enough to not accumulate.
- **Empty save UX.** Is `phase: NoChanges` enough, or should the frontend distinguish "no window
  open" from "window open but empty"? A `status` reason string can cover both.
- **Webhook availability (CRD).** A failed mutating webhook blocks `ExplicitCommit` creation.
  `failurePolicy` must be chosen deliberately — `Fail` is correct here (no author stamp ⇒ no
  trustworthy commit) but it makes the webhook a hard dependency for the save path.
- **Concurrent saves.** Two `ExplicitCommit`s from one author racing on the same window — the
  second should see `NoChanges` once the first finalized it. Confirm the window state machine
  makes that deterministic.
- **Metrics.** `explicit_commit_total{phase}`, `explicit_commit_latency_seconds`,
  `explicit_commit_no_window_total`.

## References

- Active predecessor design (aggregated-API transport, native audit gap deep-dive):
  [design-commit-context-api.md](design-commit-context-api.md) — superseded by this doc.
- Parent design exploration (transport options, audit-stream principle, "commit now" risks):
  [idea-end-user-commit-messages.md](idea-end-user-commit-messages.md).
- Audit-carried transport alternatives:
  [addendum-end-user-commit-messages-audit-transports.md](addendum-end-user-commit-messages-audit-transports.md).
- Audit ingestion pipeline (the recently simplified path this design rides on):
  [design-audit-ingestion-hardening.md](design-audit-ingestion-hardening.md).
