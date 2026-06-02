# Idea: End-user supplied commit messages

> Status: exploratory - captured for future design.
> Date: 2026-05-05

## The proposal in one sentence

Give end users a Kubernetes-native way to attach intent text to audit-backed changes, so frontends
can collect "why was this changed?" and have that reason appear in git history.

## Context

Today, the end user can influence Kubernetes state, but not the commit message gitops-reverser writes
after observing that state change. That is a natural consequence of the architecture:

- the user talks to the Kubernetes API
- audit events describe what happened
- gitops-reverser reconstructs files and commits them later

That decoupling is useful, but it makes commit messages difficult. There is no ordinary field on a
`Deployment`, `ConfigMap`, `Application`, or other watched object that means "use this as the commit
message for the eventual reverse GitOps commit." Frontends may have good user intent available at the
moment of editing, but gitops-reverser currently has no clean channel to receive it.

## Load-bearing principle: audit stream as source of truth

The defensible version of this feature is not "a frontend tells gitops-reverser to use this
message." It is:

"The Kubernetes audit stream contains both the change and the user's reason, and gitops-reverser
derives commits from that audited record."

That principle should hold regardless of the transport:

1. the user or frontend sends a Kubernetes API request that carries the reason somehow
2. Kubernetes authenticates and authorizes the request normally
3. Kubernetes emits an audit event containing the authenticated `userInfo` and the reason
4. the audit consumer reads the reason from that audit event
5. the audit consumer uses the audit event's identity and observed order to update the matching
   commit window

The implementation should not depend on handler-local state. The author should not come from a
user-provided field. Timing should follow the same audit stream ordering that the rest of the
audit-backed authoring model already trusts.

If this principle is too weak for commit messages, it is probably too weak for the audit-backed
authoring model in general.

## Option: audit `user.extra` enrichment

The cheapest possible version may be to put the reason into audit `user.extra`.

Kubernetes audit events already carry `userInfo.extra`. A frontend or backend that can influence the
authenticated request identity could add something like:

```yaml
userInfo:
  username: alice@example.com
  extra:
    gitops-reverser.io/reason:
      - "Increase checkout API memory after load-test failures"
```

gitops-reverser would read that value from the same audit event as the resource mutation. No new CRD,
no aggregated API, no mutating webhook, and no request body capture are required if the audit event
already includes `userInfo.extra`.

This option lines up well with the audit-stream principle:

- the reason is part of the audited request identity/context
- the author still comes from audit `userInfo`
- each resource-changing request can carry its own reason
- commit-window grouping can combine reasons from multiple events instead of relying on one
  per-author slot

The hard part is how real frontends get the value into `user.extra`. Ordinary clients cannot usually
invent arbitrary `user.extra` fields. This likely requires one of:

- an authentication layer that populates extra fields
- Kubernetes impersonation with permission to set `Impersonate-Extra-*` headers
- a backend service that is explicitly trusted to impersonate both user and extra context

That may still be cheaper than introducing a new API surface. It is especially worth prototyping
because many real frontends already reach Kubernetes through a backend service account. If that
backend cannot use impersonation or equivalent identity enrichment, the whole "end-user supplied"
feature becomes much less useful regardless of transport.

## Option: transient metadata stripped by admission

Another serious option is to attach commit context directly to the object being changed, using
transient metadata annotations:

```yaml
metadata:
  annotations:
    gitops-reverser.io/commit-message: "Increase checkout API memory after load-test failures"
```

A mutating admission webhook could read those annotations and remove them before the object is
persisted. The fields would not appear in stored Kubernetes state and would not be rendered into git.

The least surprising version is "strip only, act from audit":

1. the mutating webhook removes the transient annotations before persistence
2. Kubernetes emits an audit event for the original update request
3. the audit consumer reads the stripped annotation from the audit event request payload
4. gitops-reverser applies the reason to the same resource event, using audit `userInfo` for author

This is attractive because the reason rides along with the exact resource update it describes. It is
atomic with the change from the user's point of view, naturally scoped to the same audit identity, and
does not require registering an APIService.

It also handles a real commit-window case better than a single per-author message slot. If the same
user edits A with reason "fix typo" and then B with reason "scale up" inside one commit window, each
event can carry its own reason. The grouped commit can render both reasons or choose a deterministic
summary. A one-message-per-window API would lose or overwrite one of them.

Costs and risks:

- every resource type that wants this behavior must pass through the webhook
- the intent is hidden inside object metadata rather than modeled as an explicit operation
- tools that preserve, diff, or rewrite annotations may accidentally retain or disturb it
- audit policy must capture enough request payload to recover the stripped metadata
- patch requests may need careful handling because the reason may live in the patch body, not a full
  object

This option is somewhat hacky, but the objection is not enough to discard it. Its benefits match the
project's grain: Kubernetes request in, audit event out, no durable side channel.

## Option: aggregated API commit context

An aggregated API endpoint is still plausible, but it should not be treated as the obvious first
version. APIServices need registration, TLS and requestheader CA wiring, availability handling, and
meaningful e2e coverage. gitops-reverser has already paid some of that cost for audit proxy work, but
that does not make the next APIService free.

A frontend would call a lightweight aggregated endpoint:

```yaml
apiVersion: gitops-reverser.io/v1alpha1
kind: CommitContext
action: setMessage
message: "Increase checkout API memory after load-test failures"
scope:
  gitTargetRef:
    name: team-a-config
```

The handler would authenticate normally, validate the payload, and return. gitops-reverser would not
act on the handler call directly. It would wait for the `CommitContext` request to appear in the
Kubernetes audit stream, then read the message and author from that audit event.

This shape is useful when:

- the frontend cannot alter the resource update payload
- `user.extra` enrichment is not available
- the reason should be an explicit operation rather than metadata on the changed object
- a `gitTargetRef` or other gitops-reverser-specific scope is needed

The downsides are the operational cost and the message-cardinality problem. A standalone
`setMessage` operation naturally creates a per-author or per-author/target slot. That fits a single
edit session, but it collapses when multiple distinct reasons land inside the same commit window.

## Option: short-lived CommitIntent CRD

Another possible design is a small CRD, tentatively called something like `CommitIntent`,
`CommitMessageIntent`, or `NextCommitMessage`.

A frontend would create one before or near the mutating Kubernetes request:

```yaml
apiVersion: gitops-reverser.io/v1alpha1
kind: CommitIntent
metadata:
  generateName: alice-
spec:
  message: "Increase checkout API memory after load-test failures"
```

gitops-reverser would interpret the audited creation of that object as:

"For the current authenticated author, use this message for the next matching commit window, then
delete the intent object."

The object is not meant to be durable project state. It is closer to a request-side note that crosses
the Kubernetes API boundary so the asynchronous git writer can pick it up later.

This feels less natural than the audit-carried options because the cleanup clock should be the commit
window, not a broad TTL. A CRD is only worth revisiting if users need inspectable pending intent
state, ordinary watch/list behavior, or RBAC boundaries that are hard to express otherwise.

## Commit now

`commitNow` is a possible future action, but it should be treated as v2 at the earliest.

The tempting shape is:

```yaml
apiVersion: gitops-reverser.io/v1alpha1
kind: CommitContext
action: commitNow
message: "Scale worker pool for import backlog"
```

`commitNow` would set or replace a message and ask the branch worker to finalize the matching open
commit window immediately, instead of waiting for the rolling silence timer.

That is useful when `commitWindow` is deliberately lengthened and a UI knows the user pressed
save/apply. But it also exposes commit timing as a frontend-visible contract. Today the operator owns
when gitops-reverser commits. Once frontends can force a commit, users will depend on that timing.

Risks to resolve before implementing it:

- interaction with byte-cap finalization
- interaction with push cooldown
- interaction with conflict handling and replay
- whether `commitNow` waits for the commit or only acknowledges a requested flush
- whether it applies to all open windows for the author or requires a `gitTargetRef`
- whether committing an empty window is a no-op or an error

Recommendation: defer `commitNow` unless the product deliberately chooses longer commit windows and
needs an explicit "save session is complete" signal.

## Why this is compelling

- **Better audit trail.** Git history can explain the reason for a change, not only the object that
  changed.
- **Frontend-friendly.** Any UI with a "reason" or "description" field can forward that text without
  needing git credentials.
- **Kubernetes-native authorization.** RBAC, authentication, impersonation, and audit policy remain
  the control points.
- **Decoupling preserved.** The Kubernetes API remains the integration point; frontends do not need
  direct access to the gitops-reverser queue.

## Core design questions

### How is "current author" determined?

The author must come from the audit event's `userInfo`, not from a user-provided body field,
annotation value, or CRD spec field. That keeps one user from setting a message for another user's
commit unless an explicit, auditable impersonation or delegation model exists.

Service-account frontends are the hard case. If most UIs talk to Kubernetes through a backend service
account and that backend does not use impersonation or identity enrichment, gitops-reverser will only
see the service account. In that world, the feature is not truly end-user supplied. Solving that may
be more important than the transport shape.

### What does "current commit window" mean?

By default, `spec.push.commitWindow` is `5s`, so a user has a short rolling window where their latest
actions are grouped into one commit. Commit-message context should have the same lifetime. Keeping an
unused message around for minutes would be surprising because it no longer belongs to the edit
session that produced it.

Message cardinality depends on the transport:

- `user.extra` and stripped annotations can attach a reason to each audited resource mutation
- aggregated API and CRD shapes tend to create one message slot per author or author/target window

Per-event reasons are more expressive. If multiple reasons land inside one commit window, the grouped
commit template needs a rule: join unique reasons, choose the latest reason, render a subject plus
body, or fall back to the existing generated message.

### What does cleanup mean?

For audit-carried options, cleanup is mostly "do not persist it":

- `user.extra` is already request context
- transient metadata is stripped before the object is stored
- extracted reasons live only until the corresponding commit window is finalized

For aggregated API or CRD slots:

- consumed successfully: remove the slot when the window is finalized
- not consumed: remove it after the same `commitWindow` duration
- invalid or too long: reject the request

The commit itself should not depend on successful cleanup.

### What must audit policy capture?

The answer depends on the option:

- `user.extra`: audit `userInfo.extra` must be present
- transient metadata: audit must capture the request object or patch payload before the webhook strips
  the annotation
- aggregated API: audit must capture the `CommitContext` request body
- CRD: audit of the create request must capture enough object content to recover the message

If the audit event only contains metadata, gitops-reverser may prove who made the request but still
cannot recover the requested message.

## Possible shapes

### `user.extra`

```yaml
userInfo:
  username: alice@example.com
  extra:
    gitops-reverser.io/reason:
      - "Increase checkout API memory after load-test failures"
```

### Transient metadata

```yaml
metadata:
  annotations:
    gitops-reverser.io/commit-message: "Increase checkout API memory after load-test failures"
```

### Aggregated API

```yaml
apiVersion: gitops-reverser.io/v1alpha1
kind: CommitContext
action: setMessage
message: "Increase checkout API memory after load-test failures"
scope:
  gitTargetRef:
    name: team-a-config
```

## Comparison

Modern frontends often issue resource updates as parallel requests rather than one synchronous
sequence. That changes how these options weigh up. Audit-carried reasons survive parallel writes
naturally because each request carries its own context. A separate "send the message last" signal,
such as an aggregated API call after the mutations have landed, becomes more attractive when a
frontend wants a single consolidated reason for a batch of updates rather than one reason per
request.

| Approach | Running complexity | Flexibility | Correctness | Parallel-write fit | Notes |
|----------|--------------------|-------------|-------------|--------------------|-------|
| audit `user.extra` | Low for gitops-reverser, but depends on an auth layer or impersonation upstream | Per-event reason | Author and reason both bound to the audited request identity | Excellent — each parallel request carries its own reason | Most frontends cannot set `user.extra` without a trusted backend that impersonates |
| transient metadata stripped by webhook | Medium — mutating webhook on every watched type, audit policy must capture request payload | Per-event reason, scoped to the exact resource update | High if audit captures the request object before strip; patch requests need care | Excellent — each parallel request carries its own reason | Intent hidden inside metadata; other tools may retain or disturb the annotation |
| aggregated API `CommitContext` | High — APIService registration, TLS, requestheader CA, availability handling | One message per author or author/target slot; can act as an explicit close-off after parallel writes | Author from audit `userInfo`; ordering follows the audit stream | Strong when used as a "send last" terminator after a batch of mutations | Best fit when a frontend deliberately wants one consolidated reason for many updates |
| `CommitIntent` CRD | Medium — CRD plus controller cleanup logic | One message per intent object | Author from audit of the create; cleanup tied to commit window, not TTL | Adequate, but the CRD adds visible state for something meant to be ephemeral | Worth revisiting only if inspectability or RBAC needs it |
| do nothing | None | None — template-driven messages only | N/A | N/A | The current behavior |

The parallel-write angle does not eliminate the audit-carried options, but it does strengthen the
case for the aggregated API as a *complement* rather than a competitor. Per-event reasons via
`user.extra` or stripped annotations give fine-grained context; an optional `CommitContext`
"close off" call lets a frontend consolidate a multi-request edit session into a single commit
message when that is what the UI actually wants to express.

## Reassessment after deeper review

The addendum [addendum-end-user-commit-messages-audit-transports.md](addendum-end-user-commit-messages-audit-transports.md)
examines the audit-carried transports more closely. It surfaces three points that shift the
weighting in the comparison table.

**The audit-carried options are heavier than the table suggests.**

- `user.extra` enrichment requires one of: an authentication webhook on the hot path of every
  authenticated API call (disqualified — putting cosmetic-ish features in the auth path is
  wildly disproportionate), OIDC claim mappings minted per save action (uncommon in real IdPs;
  the "fresh token per save = security" framing is also weak — OIDC tokens are session
  identity, not per-action authorization), or impersonation. Impersonation works, but
  `Impersonate-Extra-*` was designed to carry identity claims, not free-text reasons. The
  resulting RBAC story ("backend X may impersonate any user and set arbitrary extras") is
  alarming for a feature that "just" attaches a commit message. Calling the impersonation path
  off-label is fair.
- The transient-annotation transport — including the cleaner audit-annotation variant the
  addendum considered, which uses `AdmissionResponse.auditAnnotations` instead of reading from
  the audit request payload — requires a mutating admission webhook on the path of every
  relevant change. That webhook becomes a new always-on dependency in the request path, called
  for every incoming mutation on every watched type. For a feature whose value is "the commit
  message has more context," that is a steep operational cost.

**The aggregated API has an ordering nuance that matters.**

- In grouping mode (current default `commitWindow: 5s`), the natural pattern is "send the
  message last" as a close-off after the related resource changes have landed. The audit event
  for the `CommitContext` request falls inside the still-open window, gitops-reverser attaches
  the message to the in-progress group, and the window finalizes shortly after. Clean and
  well-defined, and a particularly good fit for parallel-write frontends because the message
  acts as a synchronization barrier after the parallel batch completes.
- In a hypothetical per-event mode (very short or per-event windows), the message would have to
  be sent *before* the resource change to be picked up — and that is racy: other parallel
  actions could land between the message and the action, and the message could be wrongly
  associated. This is a real point in favour of audit-carried per-event reasons for that mode,
  but per-event mode is not the default and not a near-term goal.
- For the current default and the typical UI shape, the grouping-mode close-off pattern is
  enough.

**Aggregated API is also future-proof for other transient actions.**

A `CommitContext`-shaped resource is a natural place to put other temporary, audited actions
that are not durable Kubernetes state: a `commitNow` flush signal (still deferred — see
"Commit now" above), a `proposeAsPullRequest` action that asks the branch worker to push a
feature branch and open a PR instead of pushing to the target branch directly, and so on.
Audit-carried transports are tied to "the reason for this single mutation"; they do not
naturally extend to actions that are not themselves a Kubernetes resource mutation. Once the
APIService cost is paid, additional actions are cheap to add and they all share the same
audit-stream-as-source-of-truth treatment.

**Updated ordering.**

For the current defaults and the most likely deployment shapes:

1. **Aggregated API `CommitContext`** — preferred. The operational cost is a one-time
   APIService registration; once that exists, additional actions are cheap. Grouping-mode
   close-off handles the parallel-write case the comparison table flagged.
2. **`user.extra` via impersonation** — viable as a fallback when the cluster already has
   impersonation infrastructure in regular use for other reasons. Skip for clusters that do
   not.
3. **Transient annotations (any variant) via mutating webhook** — defer. The always-on webhook
   in the request path is a heavier ask than the value of the feature, especially when the
   aggregated API can cover the same use case and more.
4. **`CommitIntent` CRD** — defer. The aggregated API covers the same shape with less
   persisted state.
5. **Do nothing** — fall back if none of the above is available.

## Recommendation

Build the aggregated API `CommitContext` first. It is operationally heavier than the
audit-carried options on paper, but the audit-carried options have hidden costs (impersonation
RBAC for arbitrary user identities, always-on admission webhook on every relevant change) that
this comparison underweighted on the first pass. The aggregated API is also future-proof:
future temporary actions (`commitNow`, "propose as PR") fit the same shape with no new
transports, and the close-off pattern is a clean fit for parallel-write frontends.

The detailed design lives in [design-commit-context-api.md](../finished/design-commit-context-api.md).

The audit-carried transports remain valuable as a fallback for clusters that already have the
infrastructure in place (impersonation in regular use, or OIDC `AuthenticationConfiguration`
mappings), and as documentation of what the design considered.

The most important invariant is unchanged: users can only set commit messages for their own
attributed changes, and that is proven through the audit event itself unless an explicit,
auditable impersonation or delegation model exists.
