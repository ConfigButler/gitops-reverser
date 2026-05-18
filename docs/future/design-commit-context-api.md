# Design: aggregated API `CommitContext`

> **Status: superseded.** This document is kept as a detailed reference for the aggregated-API
> transport and the native-audit-gap analysis. The active design is
> [design-commit-request-api.md](design-commit-request-api.md), which renames the kind to
> `CommitRequest`, makes "commit the open window now" the primary behavior, drops the Valkey
> request stash, and reports the resulting commit SHA in resource status. Read this doc only
> for the deep-dive on the aggregated-API audit gap; everything else here is reframed there.
>
> Original status: design — concrete pass at Option 3 from
> [idea-end-user-commit-messages.md](idea-end-user-commit-messages.md).
> Date: 2026-05-07

## Goals

- Give frontends a Kubernetes-native way to attach intent text (commit messages, and future
  related actions) to audit-backed changes.
- Use the same audit-stream-as-source-of-truth principle that the rest of gitops-reverser
  already follows. The handler does not write project state directly; the audit event for the
  request is what gitops-reverser acts on.
- Be future-proof. The same resource shape should accommodate later actions like "propose as
  pull request" without redesigning the transport.
- Avoid the operational costs of the audit-carried alternatives: no impersonation RBAC for free
  text, no always-on mutating webhook on every relevant resource change.

## Non-goals

- Not a place to store durable user data. `CommitContext` is request-only; nothing is persisted
  in the API server backing store for the kind. This is what `TokenReview` and
  `SubjectAccessReview` do.
- Not the path for resource mutations themselves. Frontends still write to `Deployment`,
  `ConfigMap`, etc. through the usual API.
- Not an attempt to make the message the source of truth. The audit event is. The message
  rides along.
- Not a synchronous commit acknowledgement. Returning 201 means "request accepted and audited";
  the actual commit happens later, on the same schedule it does today.
- Not a replacement for template-driven commit messages. It augments them.

## Architectural fit

gitops-reverser already runs:

- An audit webhook receiver (mTLS, AuditHandler) that pushes audit events into a Valkey stream.
- A consumer (`redis_audit_consumer`) that reads events from the stream and routes them via
  `EventRouter` into a per-target `BranchWorker`.
- `BranchWorker` collects pending writes into commit windows
  (`spec.push.commitWindow`, default `5s`) and produces commits.

`CommitContext` adds:

- An aggregated `APIService` that gitops-reverser serves. Registered against the cluster's
  apiserver, like any other `APIService`.
- A small handler that validates the create request, **stashes the request body in Valkey
  keyed by the `Audit-ID` header** (because kube-apiserver's native audit events for aggregated
  APIs do not include `requestObject` — see
  [Native audit gap and the local request stash](#native-audit-gap-and-the-local-request-stash)),
  and returns. The handler does *not* directly poke `EventRouter` or `BranchWorker`. It relies
  on the audit event for the create call to flow through the existing audit pipeline.
- A new branch in `EventRouter` (or earlier in the consumer) that recognises `CommitContext`
  audit events, reads the request body from the stash, and routes them to a context-attach
  path on the matching `BranchWorker` instead of treating them as a pending write.

Reusing the existing audit pipeline for routing keeps the design honest. If for some reason the
audit pipeline is degraded, the message is degraded the same way as resource events. There is no
second clock to keep in sync.

Notably, this design does *not* require running
[`apiservice-audit-proxy`](../../external-sources/apiservice-audit-proxy/README.md) in front of
gitops-reverser. The local stash closes the audit gap for `CommitContext` specifically. Operators
who already run `apiservice-audit-proxy` for other aggregated APIs they audit will get richer
synthetic events as a bonus, but it is not a deployment requirement for this feature.

## API resource shape

```yaml
apiVersion: gitops-reverser.io/v1alpha1
kind: CommitContext
metadata:
  namespace: team-a
  generateName: cc-
spec:
  action: setMessage
  message: "Increase checkout API memory after load-test failures"
  gitTargetRef:
    name: team-a-config
status:
  accepted: true
  observedAction: setMessage
  serverTime: "2026-05-07T12:34:56Z"
```

- `kind: CommitContext`, group `gitops-reverser.io`, version `v1alpha1`. Namespaced.
- Spec holds an `action` discriminator; the rest of `spec` is action-specific.
- Status is an echo of what the server accepted. It is filled in on the response only — there is
  nothing to "watch" since the object is not persisted.
- No subresources for v1alpha1.

### Discriminated by `spec.action`

`spec.action` is a closed enum. v1alpha1 defines exactly one value: `setMessage`. Future actions
extend the enum and add their own optional fields under `spec`.

The handler rejects unknown actions with a 422, including the list of supported actions in the
error.

### Initial action: `setMessage`

```yaml
spec:
  action: setMessage
  message: "Increase checkout API memory after load-test failures"
  gitTargetRef:
    name: team-a-config        # optional
```

- `spec.message` (required, 1–1024 bytes) — free-text reason. Stored on the audit event in the
  request body. The handler validates length and rejects empty strings.
- `spec.gitTargetRef.name` (optional) — restricts the message to one specific `GitTarget` in the
  request's namespace. If omitted, the message attaches to all open commit windows for the
  authenticated author within the namespace.

The handler returns:

```yaml
status:
  accepted: true
  observedAction: setMessage
  serverTime: "2026-05-07T12:34:56Z"
```

`accepted: true` means "the request was authenticated, authorized, validated, and emitted to the
audit log." It does *not* mean the message was attached to a window — that is the audit
consumer's responsibility and happens asynchronously.

### Future actions (sketched, not for v1alpha1)

These are listed to validate that the resource shape generalises. None of them are part of the
initial implementation.

- `commitNow` — finalize the matching open commit window immediately instead of waiting for the
  silence timer. Open questions are tracked in [idea-end-user-commit-messages.md](idea-end-user-commit-messages.md)
  under "Commit now."
- `proposeAsPullRequest` — instruct the branch worker to push the in-progress commit window to
  a feature branch and open a pull request via `GitProvider`, instead of pushing to the target
  branch directly. Spec would carry a feature branch name (or generate one), a target branch, a
  PR title, and a PR body.
- `attachLabel` / `attachLink` — secondary metadata to attach to the commit message or PR body
  (e.g., a Jira link, a Slack thread URL).

The discriminator pattern keeps these additive. A v1alpha1 client that sends `setMessage` keeps
working when v1alpha2 adds `proposeAsPullRequest`.

## End-to-end flow

For the close-off pattern in grouping mode:

1. Frontend issues parallel resource mutations against the apiserver (Deployment patch,
   ConfigMap update, etc.).
2. Each mutation produces an audit event; `EventRouter` routes them into a `BranchWorker`
   commit window scoped by author and `GitTarget`. The window's silence timer ticks.
3. After all mutations have been issued, the frontend issues a `CommitContext` with
   `action: setMessage` and the user's reason text. The aggregated API server in
   gitops-reverser validates the request, stashes the request body in a short-lived Valkey
   entry keyed by the `Audit-ID` header that kube-apiserver propagates, and returns 201.
4. kube-apiserver emits an audit event for the `CommitContext` create. The native event is
   "hollow" for aggregated APIs (no `requestObject`, no `objectRef.name`); the body lives in
   the stash. See
   [Native audit gap and the local request stash](#native-audit-gap-and-the-local-request-stash).
   The audit pipeline delivers the event to the consumer like any other event.
5. The consumer recognises the event as a `CommitContext`, reads the `auditID` and
   `user.username` from the audit event, and retrieves the request body from the stash by
   audit ID. With that body in hand, it has `spec.action`, `spec.message`, and optional
   `spec.gitTargetRef`. It routes the event to the context-attach path, not the
   resource-write path. The stash entry is deleted after a successful match.
6. The matching `BranchWorker` attaches the message to the open window. The window's silence
   timer continues unaffected — `CommitContext` events do not extend the window.
7. The window finalizes when its silence timer expires. The commit message uses the attached
   reason.

## Ordering and timing semantics

The single most important design point. Get this wrong and the feature is unreliable.

### Grouping mode close-off (current default)

`spec.push.commitWindow` defaults to `5s`. After the last resource activity for a given window
key, the window finalizes 5s later. The close-off pattern fits this naturally:

- Frontend awaits its parallel mutations.
- Frontend then sends `CommitContext` with the message.
- Audit ordering: resource events first, `CommitContext` event last, all within the 5s window.
- Audit consumer processes events in audit order. By the time it sees the `CommitContext`, the
  window for that author+target is open and contains the resource events. The message attaches.
- The window's silence timer started ticking on the last resource event. The `CommitContext`
  is not counted as resource activity, so it does not extend the window. The commit happens
  on schedule.

This works for parallel writes because all parallel mutations land before the close-off, and
they all share the same window key (author + GitTarget + branch).

### What "matching open window" means

A `CommitContext setMessage` event attaches to windows that satisfy:

- author equals `audit.user.username` from the `CommitContext` event
- if `spec.gitTargetRef.name` is set, GitTarget name equals that name in the request namespace
- window is in the open state (silence timer has not fired yet)

If multiple windows satisfy the predicate (no gitTargetRef, multiple targets in flight), the
message attaches to all of them. This matches the "single user intent applies to everything they
just edited" assumption. Operators who want stricter scoping should set `spec.gitTargetRef`.

### Handling when message arrives outside any window

Two subcases:

1. **Stream lag.** The audit consumer is behind real time, so the resource events have not
   reached the consumer yet but the `CommitContext` has. This should not happen because audit
   events are delivered in stream order and the resource events were emitted earlier — but if
   it does, the consumer holds the message in a small per-author buffer for one `commitWindow`
   duration, attaches it when a matching window opens, and drops it otherwise.
2. **Genuine orphan.** The user sent a message but no resource changes happened (or all of them
   were filtered). After `commitWindow` of grace with no matching window opening, the message
   is dropped. The consumer logs a debug entry and increments a metric so this is observable.

The consumer must not retain orphan messages indefinitely. A grace bounded by `commitWindow`
matches the rest of the system's lifetime model.

### Multiple messages within a window

A frontend could send several `setMessage` calls in one window. The window may also receive
messages for two different reasons that happen to land in the same grouping window (rare but
possible).

The consumer collects messages in arrival order. The commit message template renders them in
order, deduplicated, with a separator. A reasonable default rendering:

```
<first message>

<second message>

<auto-generated summary of changes>
```

The exact template is a runtime decision, not part of the API. The API guarantees only that
each accepted `CommitContext` contributes its `spec.message` to the eventual commit if a
matching window exists.

### Per-event mode (deferred)

If the project later supports a "one commit per resource event" mode, the close-off pattern no
longer works: there is no window to land in, and sending the message before the resource event
is racy under parallel writes. For that mode, audit-carried per-event reasons (the
`user.extra` / audit-annotation transports) become the right tool — but that mode is not
v1alpha1's concern.

## Author binding

`audit.user.username` is the author. Period.

- The handler does *not* trust `spec` to identify the user.
- If the request was impersonated, audit records `impersonatedUser` separately. The author is
  the effective user (the one being impersonated), the same rule the audit-backed authoring
  model uses today.
- If audit shows the request was unauthenticated or anonymous, the consumer ignores the
  `CommitContext` event. Anonymous reasons are not attributable.

This is the same invariant the parent doc calls out: users can only set messages for their own
attributed changes, and that is proven through the audit event.

## Scope and addressing

`CommitContext` is namespaced. The namespace it is created in defines:

- the namespace context for `spec.gitTargetRef`
- where RBAC verbs against the kind are evaluated

Multi-tenant clusters can grant `create` on `commitcontexts` within a tenant namespace and not
elsewhere. Cluster-scoped global use is not supported in v1alpha1.

If `spec.gitTargetRef` is set, it must point to a `GitTarget` in the same namespace. Cross-namespace
targeting is rejected.

If `spec.gitTargetRef` is omitted, the consumer attaches the message to all open windows for the
author whose GitTarget lives in the request's namespace. Open windows in other namespaces are
not affected.

## Lifecycle: request-only, no persistence

The aggregated API server does not back `CommitContext` with storage. Each request is a
short-lived RPC.

- Create: handler validates, returns 201 with status echo.
- Get / List / Update / Patch / Delete: not supported. The handler returns 405 with a body
  explaining that `CommitContext` is request-only.
- Discovery: the handler advertises the kind so clients can `kubectl explain` and so client-go
  can serialize requests properly. Discovery returns the schema; List against that schema does
  not return objects.

Cleanup is a non-question because nothing is persisted. The audit event is the only artifact,
and it lives in the audit log subject to existing retention.

## Native audit gap and the local request stash

kube-apiserver's native audit pipeline does *not* populate `objectRef.name`, `requestObject`,
or `responseObject` for aggregated API requests. This is the gap documented in
[external-sources/apiservice-audit-proxy/README.md](../../external-sources/apiservice-audit-proxy/README.md).
Without help, the audit event for a `CommitContext` create reaches the consumer with the user
and verb but not the message text.

`apiservice-audit-proxy` exists to close that gap by sitting in front of an aggregated backend
and emitting synthetic audit events with full request/response bodies. **This design
intentionally does not require `apiservice-audit-proxy` to be running** for the `CommitContext`
feature to work. Operators who already run it for other aggregated APIs they audit will benefit
from richer events; operators who do not run it should not be forced to deploy a second
component just to use commit messages.

Instead, gitops-reverser closes the gap locally:

1. The aggregated API handler reads the `Audit-ID` HTTP header that kube-apiserver attaches to
   every proxied request. This is the same value that ends up in the audit event's `auditID`
   field, so it is a reliable correlation key.
2. Before returning 201, the handler writes a stash entry to Valkey:
   - key: `commitcontext:stash:<audit-id>`
   - value: the canonicalised request object (action, message, gitTargetRef, namespace,
     authenticated author)
   - TTL: a few minutes — longer than the worst-case audit-pipeline lag, much shorter than the
     longest plausible commit window
3. When the audit consumer encounters a `commitcontexts` audit event, it reads `auditID` and
   `user.username` from the event, fetches the stash entry, and treats that as the source of
   truth for the request payload. The audit event itself contributes the audit identity,
   timestamps, and acknowledgement that the request was accepted (`responseStatus.code == 201`);
   the body comes from the stash.
4. After a successful match, the consumer deletes the stash entry.

Why Valkey rather than an in-process map? The handler and the audit consumer may be different
replicas of the gitops-reverser deployment. Valkey is already a hard dependency; using it
keeps the design HA-friendly without introducing new infrastructure.

The audit-stream-as-source-of-truth principle is preserved: the audit event still proves that
the request happened, who issued it, and when. The stash holds only the *content* of a
request that the apiserver has already accepted and audited. There is no shadow channel
through which a `CommitContext` could take effect without an audit event also existing.

### Failure modes

- **Valkey unavailable when the handler tries to stash.** The handler returns 503; the
  frontend can retry. Accepting the request without a stash would silently lose the message,
  which is worse than a clean failure.
- **Audit event arrives, stash is missing.** Two distinct cases must be told apart:
  - *Already attached.* If `apiservice-audit-proxy` is also in front of gitops-reverser, the
    consumer sees two audit events with the same `auditID` (one hollow native, one enriched
    synthetic — both POSTed to the same `/audit-webhook/` endpoint today, see
    [idea-audit-enrichment-side-channel.md](idea-audit-enrichment-side-channel.md) for the
    proposed cleaner architecture). The first arrival drains and deletes the stash;
    the second arrival finds the stash empty *but the message has already been attached* —
    this is not an alarm. To distinguish: when a successful attach happens, the consumer also
    writes a marker `commitcontext:attached:<audit-id>` with a TTL of ~30 minutes. On a
    subsequent stash miss for the same `auditID`, the consumer checks the marker and silently
    no-ops if present.
  - *Real miss.* No stash, no marker. Indicates an unusual race or a misconfiguration where
    the handler and consumer are reading different Valkey backends. The consumer drops the
    event with `commit_context_stash_miss_total++` and a warning log. Persistent non-zero
    values of this metric should alert.
- **Stash is written but no audit event arrives.** TTL expires; nothing leaks. Indicates the
  audit pipeline is degraded; the existing audit-pipeline-health metrics cover that.
- **`apiservice-audit-proxy` is in front and emits a synthetic event.** With today's single
  audit endpoint, both events flow through the canonical pipeline with the same `auditID`.
  The idempotency-marker mechanism described above handles this. With the proposed refinement
  the dual-event case disappears entirely.

### Required header behaviour

kube-apiserver allocates the audit ID at the request-received stage (before any handler runs)
and propagates it as the `Audit-ID` request header through the serving hierarchy, including
to aggregated API backends. The same value appears as `auditID` on every audit event for that
request (`RequestReceived`, `ResponseStarted`, `ResponseComplete`). This is the documented
correlation mechanism — see [Audit ID Chain (kubernetes/kubernetes#101597)](https://github.com/kubernetes/kubernetes/issues/101597)
and the [Auditing reference](https://kubernetes.io/docs/tasks/debug/debug-cluster/audit/).

The handler therefore receives the audit ID on the inbound request, before it produces a
response, and can stash by it deterministically. The same value appears on the audit event
that flows through the audit pipeline shortly after.

**Proof from this repo's own audit proxy.**
[external-sources/apiservice-audit-proxy/pkg/audit/builder.go](../../external-sources/apiservice-audit-proxy/pkg/audit/builder.go)
uses exactly this trick to populate synthetic audit events:

```go
func (b *Builder) auditIDFromRequest(req *http.Request) types.UID {
    if value := strings.TrimSpace(req.Header.Get(v1.HeaderAuditID)); value != "" {
        // ...
    }
}
```

`v1.HeaderAuditID` is the canonical Go constant for `Audit-ID`, defined in
[`k8s.io/apiserver/pkg/apis/audit/v1`](https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/apiserver/pkg/apis/audit/types.go).
The proxy's tests seed `Audit-Id` on inbound test requests
([handler_test.go](../../external-sources/apiservice-audit-proxy/pkg/proxy/handler_test.go),
[builder_test.go](../../external-sources/apiservice-audit-proxy/pkg/audit/builder_test.go)) to
assert the same path.

**Timeline for a `CommitContext setMessage` request.**

1. Client → kube-apiserver: request arrives; apiserver mints the audit ID; emits the
   `RequestReceived` audit event with that ID.
2. kube-apiserver → gitops-reverser aggregated handler: proxied request includes the
   `Audit-ID` header.
3. Handler reads the header, validates the body, stashes under
   `commitcontext:stash:<audit-id>`, returns 201.
4. kube-apiserver emits the `ResponseComplete` audit event with the same `auditID`.
5. Audit event reaches the consumer; consumer joins by `auditID` against the stash.

The stash is written before the handler responds, so by the time the audit event flows through
the pipeline (which always lags the HTTP response), the entry is already in Valkey.

The handler treats a missing `Audit-ID` header as a hard error (500), not a soft warning,
because the request would be unrecoverable.

## Related: dedicated audit enrichment side-channel

A separate proposal,
[idea-audit-enrichment-side-channel.md](idea-audit-enrichment-side-channel.md), would route
`apiservice-audit-proxy` events to a dedicated endpoint and side-cache instead of the
canonical audit stream. If that lands, the idempotency-marker mechanism in the failure-modes
list above becomes unnecessary because only one event per `auditID` would reach the
consumer. `CommitContext` does not depend on that refinement and can ship before or after
it; see the linked doc for the full architectural argument, observability story, open
questions, and risks.

## Audit policy requirements

Because the request body is recovered from the local stash, the audit policy can be
`Metadata`-level — the consumer only needs `auditID`, `user`, `verb`, `objectRef.namespace`,
`objectRef.resource`, and `responseStatus` from the audit event itself.

```yaml
apiVersion: audit.k8s.io/v1
kind: Policy
rules:
  - level: Metadata
    resources:
      - group: gitops-reverser.io
        resources: ["commitcontexts"]
    verbs: ["create"]
```

Setting it higher than `Metadata` is harmless but unnecessary. `Request` level would not
even buy anything because kube-apiserver does not record `requestObject` for aggregated APIs
regardless of the level configured.

The Helm chart should ship a recommended audit policy fragment alongside the chart so
operators do not have to derive this themselves.

## RBAC

End users (or the backend acting on their behalf) need:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  namespace: team-a
  name: gitops-reverser-commit-context-writer
rules:
  - apiGroups: ["gitops-reverser.io"]
    resources: ["commitcontexts"]
    verbs: ["create"]
```

That is it for v1alpha1. Granular per-target restrictions can come later via admission, not
RBAC, since the GitTarget reference is a spec field.

The aggregated API server itself runs with the gitops-reverser controller's existing identity
and does not need new RBAC for receiving the request. It does need the standard aggregated-API
plumbing (TLS server cert signed by the front-proxy CA, an `APIService` registration object,
etc.).

## Implementation outline

### APIService registration and TLS

- Reuse the cert-manager flow already used for the audit webhook to issue a server cert for the
  aggregated API endpoint.
- Helm chart adds an `APIService` object pointing the cluster apiserver at the gitops-reverser
  service on a dedicated port (e.g., `:8443/aggregated`).
- Wire `--requestheader-client-ca-file` etc. as standard for aggregated APIs. The project
  notes from past audit-proxy work apply here.

### Handler responsibilities

The handler is small. For `POST /apis/gitops-reverser.io/v1alpha1/namespaces/<ns>/commitcontexts`:

1. Authenticate (handled by the apiserver before the request arrives at us, via front-proxy
   headers — we trust them because they are signed by the requestheader CA).
2. Authorize (the apiserver also performs SubjectAccessReview against the user before
   forwarding; verify with a defensive check).
3. Validate `spec.action` is supported.
4. Validate action-specific fields:
   - `setMessage`: `spec.message` non-empty, length cap, no control characters except newline.
   - `spec.gitTargetRef.name` shape only (existence check is *not* required at handler time —
     the audit consumer is the place that resolves it; rejecting here would couple the handler
     to GitTarget store state).
5. Read the `Audit-ID` header that kube-apiserver attaches to the proxied request. If the
   header is missing, fail with 500 — without it the audit event cannot be correlated. (This
   should not happen in any normal cluster; kube-apiserver always propagates `Audit-ID`.)
6. Stash the canonicalised request body to Valkey under `commitcontext:stash:<audit-id>`
   with a TTL of a few minutes. If Valkey is unavailable, return 503 — accepting the request
   without a stash would silently lose the message.
7. Build the response object, set `status.accepted = true`, `status.observedAction`,
   `status.serverTime`.
8. Return 201.

The handler runs in the existing controller manager pod. It does not talk to the audit
consumer or the BranchWorker directly. The audit pipeline (with the Valkey stash as the body
channel) is the only channel.

### Audit consumer integration

`redis_audit_consumer` (or whichever component currently reads the Valkey stream) needs:

1. A predicate to recognise `CommitContext` events: `objectRef.apiGroup == "gitops-reverser.io"`,
   `objectRef.resource == "commitcontexts"`, `verb == "create"`,
   `responseStatus.code == 201`.
2. A stash lookup: read `auditID` from the event, GET `commitcontext:stash:<audit-id>` from
   Valkey, and parse the canonicalised request body. On a miss, drop the event with
   `commit_context_stash_miss_total++`. On a hit, DEL the stash entry.
3. A new event path that does *not* go through `EventRouter`'s pending-write logic. Instead it
   produces a `CommitContextEvent` with:
   - author from `audit.user.username` (using `impersonatedUser` if present, per existing
     authoring rules)
   - namespace from `objectRef.namespace`
   - action and action-specific fields from the stash body
4. A dispatcher that locates matching `BranchWorker` instances by (author, namespace, optional
   gitTargetRef) and calls a new `AttachContext(*CommitContextEvent)` method.

The audit event is the source of truth for *who* and *when*; the stash is the source of truth
for the message *content*. Trusting the audit event for identity preserves the
audit-stream-as-source-of-truth principle even though the body is recovered locally.

`BranchWorker.AttachContext` mutates the in-progress commit window:

- if a window is open, append the message to the window's collected messages slice
- if no window is open, hold the event in a small per-author grace buffer (single-element ring
  per (author, namespace, optional gitTargetRef) is enough); evict after `commitWindow`
- when a new window opens within grace, drain the buffered context onto it

When the window finalizes, the commit-message renderer joins the collected messages into the
final commit message using the template logic.

### Branch worker / commit window changes

Concrete additions to the existing commit-window struct:

- `messages []string` collected in arrival order
- `messagesAttachedAt time.Time` of the last attach (for metrics / debugging)

Window finalization renders the commit message as:

```
<joined user messages, deduplicated, separated by blank lines>

<existing auto-generated summary of changes>
```

If `messages` is empty, the existing template-driven flow runs unchanged.

## Edge cases and failure modes

- **No matching window, no future window.** Buffered for `commitWindow`, then dropped with a
  `commit_context_orphans_total` metric increment.
- **Window finalized before message arrives.** Message dropped, metric incremented
  (`commit_context_late_total`). This indicates the frontend's close-off was sent too late or
  audit lag is large. Operators can react by lengthening `commitWindow` or improving audit
  delivery.
- **`spec.gitTargetRef` points to a non-existent GitTarget.** The dispatcher finds no matching
  worker; behaves like an orphan. Logged, metric incremented.
- **Audit log retention purges the event before the consumer sees it.** Treated as "event
  never delivered." This is a general audit-pipeline concern, not specific to `CommitContext`.
- **Multiple concurrent `CommitContext` events from the same author.** Audit ordering
  determines arrival order; messages collect in that order; rendering deduplicates if the same
  text appears more than once.
- **`spec.message` contains malicious content (HTML, control chars, very long strings).** The
  handler rejects control chars except `\n`, caps length, and validates UTF-8. The renderer
  treats the string as plain text in commit messages; it is never rendered as HTML, never
  shell-evaluated.
- **`CommitContext` request from anonymous or rejected identity.** The apiserver rejects before
  forwarding to the handler. The audit event in that case has `responseStatus.code != 201`; the
  consumer ignores any `CommitContext` event whose response was not successful.

## Interaction with existing CRDs and concepts

- `GitTarget` is referenced read-only via `spec.gitTargetRef`. No changes required.
- `WatchRule` / `ClusterWatchRule` are not involved. `CommitContext` is a context attachment,
  not a watched resource.
- `GitProvider` is not directly involved in v1alpha1. Future actions like `proposeAsPullRequest`
  will use it for PR creation.
- The audit webhook handler does not need changes. The audit event for `CommitContext` flows
  through the same mTLS-protected webhook as everything else.

## Future-proofing: PR flow and other transient actions

The reason to invest in `CommitContext` rather than the audit-carried alternatives is that
`CommitContext` extends naturally. Sketches, not commitments:

### `proposeAsPullRequest`

```yaml
spec:
  action: proposeAsPullRequest
  pullRequest:
    title: "Increase checkout API memory after load-test failures"
    body: |
      ## Why
      Load test 2026-05-07 saturated memory. Bumping limits to unblock the team.
    sourceBranch: alice/checkout-memory   # optional, server-generated otherwise
    targetBranch: main
  gitTargetRef:
    name: team-a-config
```

The audit-driven flow:

1. The frontend opens a `CommitContext proposeAsPullRequest` *before* its resource changes.
2. The branch worker, on seeing the audit event, marks the next commit window for that author
   as "PR mode": when the window finalizes, push to `sourceBranch` and call `GitProvider` to
   open a PR with the supplied title and body, instead of pushing to `targetBranch` directly.
3. Resource changes flow as normal during the window.
4. Window finalizes; PR is opened.

This shape needs more design (which audit ordering is required? what if the PR is rejected by
the GitProvider?), but the resource shape generalises cleanly and the audit-stream principle is
preserved.

### `commitNow`

Discussed in detail in the parent doc under "Commit now." It is action-shaped already and fits
this resource. The risks listed in the parent doc still apply; not a v1alpha1 concern.

### Other audit-anchored intents

Any future capability that is "audited intent that affects how a commit/push is produced"
(label this commit, attach a release notes block, suppress this push, etc.) maps onto a new
action without requiring a new transport. Adding actions is a non-breaking change as long as
clients gate their use on discovery.

## Open questions

- **Naming.** `CommitContext` is fine but the kind ends up being a mix of "commit context" and
  "non-commit actions like PR proposal" once future actions land. Worth a second look before
  v1beta1.
- **Versioning of actions.** A `setMessage` shape change between versions is awkward because
  the discriminator is in `spec`. Convertible types via the Kubernetes scheme handle this, but
  the project should pick a convention early.
- **Rate limiting.** Should the handler rate-limit per author to prevent runaway usage from a
  buggy frontend? A reasonable default is "client-side problem" but a soft cap in the handler
  protects the audit pipeline.
- **Metrics.** Concrete set: `commit_context_accepted_total{action}`,
  `commit_context_attached_total{action}`, `commit_context_orphans_total`,
  `commit_context_late_total`, `commit_context_handler_errors_total{reason}`.
- **e2e coverage.** A first e2e test should cover the close-off pattern: parallel writes,
  followed by `CommitContext setMessage`, asserting the resulting commit message contains the
  reason. The audit-pipeline e2e harness is already in place from prior work.
- **OpenAPI schema source.** Hand-write or generate from Go types? gitops-reverser has Go-typed
  CRDs already; the same `+kubebuilder` annotations should generate OpenAPI for the aggregated
  kind. Worth checking that `kubebuilder` works for non-CRD types or whether the project needs
  a small helper.
- **Composition with audit-carried fallback.** If the cluster also runs OIDC `extra` mappings or
  impersonation-based `user.extra`, both sources may carry reasons. The renderer should pick a
  rule (prefer `CommitContext`, append `user.extra` reasons, deduplicate) and document it.
- **Stash TTL tuning.** The default of "a few minutes" is fine for normal audit-pipeline lag.
  Worth deciding whether it is configurable per-deployment or fixed.
- **Stash backend coupling.** The handler and the consumer must read/write the same Valkey.
  In the current single-Valkey deployment shape this is automatic, but it should be documented
  as a hard requirement so an operator does not split them across separate caches.
- **Stash-miss alerting threshold.** No equivalent miss/orphan counter exists on the audit
  pipeline today, so the alerting story is net-new. Decide alert thresholds and runbook
  entries before the feature is GA. See
  [idea-audit-enrichment-side-channel.md](idea-audit-enrichment-side-channel.md) for the
  broader observability picture.

## Appendix: example payloads

### Single-user save with one target

Request:

```yaml
apiVersion: gitops-reverser.io/v1alpha1
kind: CommitContext
metadata:
  namespace: team-a
spec:
  action: setMessage
  message: "Increase checkout API memory after load-test failures"
  gitTargetRef:
    name: team-a-config
```

Response:

```yaml
apiVersion: gitops-reverser.io/v1alpha1
kind: CommitContext
metadata:
  namespace: team-a
  name: cc-2026-05-07-12-34-56-abc
spec:
  action: setMessage
  message: "Increase checkout API memory after load-test failures"
  gitTargetRef:
    name: team-a-config
status:
  accepted: true
  observedAction: setMessage
  serverTime: "2026-05-07T12:34:56Z"
```

### Resulting native audit event (hollow, as kube-apiserver actually emits)

```yaml
kind: Event
apiVersion: audit.k8s.io/v1
level: Metadata
auditID: 0a1b2c3d-4e5f-6789-abcd-ef0123456789
verb: create
objectRef:
  apiGroup: gitops-reverser.io
  apiVersion: v1alpha1
  resource: commitcontexts
  namespace: team-a
  # name, requestObject, responseObject deliberately absent —
  # this is the aggregated-API audit gap.
user:
  username: alice@example.com
  groups: ["system:authenticated"]
responseStatus:
  code: 201
```

The consumer joins this event with the stash entry it finds at
`commitcontext:stash:0a1b2c3d-4e5f-6789-abcd-ef0123456789` to recover the full request body.

### Multi-target save with no scoping

Request:

```yaml
apiVersion: gitops-reverser.io/v1alpha1
kind: CommitContext
metadata:
  namespace: team-a
spec:
  action: setMessage
  message: "Refresh team-a config for 2026-05 quarterly review"
```

The message attaches to every open window for `alice@example.com` whose GitTarget is in
namespace `team-a`.

## References

- Parent design exploration:
  [idea-end-user-commit-messages.md](idea-end-user-commit-messages.md)
- Audit-carried transport addendum (alternatives this design supersedes for v1alpha1):
  [addendum-end-user-commit-messages-audit-transports.md](addendum-end-user-commit-messages-audit-transports.md)
- Existing audit pipeline reference: `docs/design/audit-consumer-next-steps.md`
- The aggregated-API audit gap and the related proxy:
  [external-sources/apiservice-audit-proxy/README.md](../../external-sources/apiservice-audit-proxy/README.md)
- In-repo proof that `Audit-ID` arrives as a request header on aggregated backends:
  [pkg/audit/builder.go](../../external-sources/apiservice-audit-proxy/pkg/audit/builder.go)
  (`auditIDFromRequest` reads `req.Header.Get(v1.HeaderAuditID)`),
  with tests at
  [pkg/proxy/handler_test.go](../../external-sources/apiservice-audit-proxy/pkg/proxy/handler_test.go)
  and
  [pkg/audit/builder_test.go](../../external-sources/apiservice-audit-proxy/pkg/audit/builder_test.go).
- Audit ID propagation through the serving hierarchy:
  [Audit ID Chain (kubernetes/kubernetes#101597)](https://github.com/kubernetes/kubernetes/issues/101597)
- `HeaderAuditID` constant definition:
  [k8s.io/apiserver/pkg/apis/audit/v1 types.go](https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/apiserver/pkg/apis/audit/types.go)
- Kubernetes [aggregated API documentation](https://kubernetes.io/docs/concepts/extend-kubernetes/api-extension/apiserver-aggregation/)
- Kubernetes [APIService reference](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.30/#apiservice-v1-apiregistration-k8s-io)
- Audit policy and Audit-ID propagation reference:
  [Auditing | Kubernetes](https://kubernetes.io/docs/tasks/debug/debug-cluster/audit/)
- [kube-apiserver Audit Configuration (v1)](https://kubernetes.io/docs/reference/config-api/apiserver-audit.v1/)
