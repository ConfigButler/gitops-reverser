# Addendum: audit-carried transports for end-user commit context

> Status: exploratory — addendum to [idea-end-user-commit-messages.md](idea-end-user-commit-messages.md).
> Date: 2026-05-07

## Why this addendum exists

The parent doc lists two audit-carried transports for end-user reasons:

1. audit `user.extra` enrichment
2. transient metadata annotations stripped by a mutating admission webhook, with the reason
   recovered from the audit request payload

It treats them as roughly comparable, with `user.extra` framed as "cheapest credible." This
addendum re-examines that framing. After looking closer at how `user.extra` actually gets
populated in real clusters, the answer is more nuanced. It also surfaces a cleaner variant of the
second transport: instead of having the audit consumer reconstruct the stripped annotation from
the audit request payload, a mutating webhook can copy it into an audit annotation on the
`AdmissionResponse`, where the apiserver records it on the audit event at every audit level.

The doc covers each transport in turn, then compares them.

## Transport A: `user.extra` enrichment

### What `user.extra` is

Every authenticated Kubernetes request resolves to a `userInfo` struct on the API server side:

```yaml
userInfo:
  username: alice@example.com
  uid: ...
  groups: [...]
  extra:
    <key>:
      - <value>
      - <value>
```

`extra` is a `map[string][]string`. Keys are arbitrary strings; values are ordered lists. The
field is populated by whichever authenticator handled the request (OIDC, webhook, client cert,
service account, anonymous) and may be augmented by impersonation. Authorization, admission, and
audit all see the same `userInfo`.

For gitops-reverser the relevant property is that this struct is part of the audit event envelope.
It does not require capturing the request body.

### How it surfaces in audit

`userInfo` is recorded on every audit event at every audit level, including `Metadata`. That
matters: most clusters run with `Metadata` for chatty resources to control log volume, and switch
to `Request` or `RequestResponse` only for sensitive resources. A commit-message reason carried in
`user.extra` rides along with the existing `userInfo` capture and does not require widening audit
policy to record full request bodies.

A representative audit event fragment:

```yaml
kind: Event
apiVersion: audit.k8s.io/v1
level: Metadata
verb: update
objectRef:
  resource: deployments
  namespace: checkout
  name: api
user:
  username: alice@example.com
  groups: ["system:authenticated"]
  extra:
    gitops-reverser.io/reason:
      - "Increase checkout API memory after load-test failures"
```

gitops-reverser's audit consumer would read `user.extra["gitops-reverser.io/reason"]` from this
event and attach it to the matching pending write before the commit window closes.

### Sources of `user.extra` values

There are four practical ways a value lands in `user.extra`. Each has different operational and
trust properties.

#### 1. OIDC structured authentication

Modern Kubernetes (`AuthenticationConfiguration`, beta from 1.30) lets the cluster operator map
OIDC token claims into `extra` keys via CEL expressions:

```yaml
apiVersion: apiserver.config.k8s.io/v1beta1
kind: AuthenticationConfiguration
jwt:
  - issuer:
      url: https://idp.example.com
      audiences: [kubernetes]
    claimMappings:
      username:
        claim: email
      groups:
        claim: groups
    extra:
      - key: "gitops-reverser.io/reason"
        valueExpression: "claims.gitops_reverser_reason"
```

The mapping is centrally controlled by the cluster operator. It only really works if the IdP can
be made to issue a per-request claim, which is unusual: OIDC tokens are typically reused for many
requests in a session, so the reason would either be missing or stale for most calls.

#### 2. Webhook token authenticator

A cluster can delegate token validation to a webhook. The webhook's `TokenReview` response can
populate `status.user.extra`:

```json
{
  "apiVersion": "authentication.k8s.io/v1",
  "kind": "TokenReview",
  "status": {
    "authenticated": true,
    "user": {
      "username": "alice@example.com",
      "extra": {
        "gitops-reverser.io/reason": ["Increase checkout API memory after load-test failures"]
      }
    }
  }
}
```

This gives full control over `extra` content at request time, but it pushes a non-trivial
component into the auth path: the webhook is on the hot path of every authenticated API call. If
it slows down or fails, the API server does.

#### 3. Impersonation headers

A trusted client can call the API server as another user by setting impersonation headers, and
that includes setting extra fields:

```http
Authorization: Bearer <backend-service-account-token>
Impersonate-User: alice@example.com
Impersonate-Group: dev
Impersonate-Extra-gitops-reverser.io%2Freason: Increase checkout API memory after load-test failures
```

The `Impersonate-Extra-<key>` header maps directly to `userInfo.extra[<key>]`. Keys containing
characters disallowed in HTTP header names (notably `/`) must be percent-encoded in the header
name. Multiple values for the same key are sent as repeated headers.

Impersonation requires explicit RBAC. To set the example key above, the impersonating identity
needs at minimum:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: gitops-reverser-impersonator
rules:
  - apiGroups: [""]
    resources: ["users", "groups"]
    verbs: ["impersonate"]
  - apiGroups: ["authentication.k8s.io"]
    resources: ["userextras/gitops-reverser.io/reason"]
    verbs: ["impersonate"]
  - apiGroups: ["authentication.k8s.io"]
    resources: ["uids"]
    verbs: ["impersonate"]
```

`resourceNames` can further narrow which users or groups may be impersonated. The
`userextras/<key>` resource pattern is how RBAC scopes which extra keys an impersonator is
allowed to set; without that rule the API server rejects the `Impersonate-Extra-*` header.

#### 4. Service account tokens

Service account tokens, including projected tokens with bound audiences, do not carry custom
extras. They populate fixed fields like `authentication.kubernetes.io/pod-name`. There is no
mechanism for a SA-authenticated request to set an arbitrary `gitops-reverser.io/reason` value
without going through impersonation or a webhook authenticator.

The practical consequence is the one noted in the parent doc: if a UI's only path to Kubernetes
is "POST to backend, backend talks to apiserver as its own SA," and that backend does not
impersonate, gitops-reverser will only ever see the backend's identity and no reason.

### Pragmatic reading of the four sources

Looking at the four together, the picture is less rosy than "cheapest credible":

- **OIDC structured auth.** The argument that "issue a fresh token per save action is itself a
  security measure" is weak. OIDC tokens are session identity, not per-action authorization.
  Forcing a fresh token per save would conflate identity assurance with intent capture, hurt UX,
  and only works if the IdP can be made to mint per-request claims. Step-up auth exists for
  high-value operations, but that is about identity confidence, not about ferrying free-text
  intent. Useful as a baseline ("this token belongs to a reason-aware UI") rather than a
  per-mutation channel.
- **Webhook token authenticator.** Disqualified for this use case. Auth webhooks sit on the hot
  path of every authenticated API call. Adding one to forward a free-text reason is wildly
  disproportionate to the value, and the failure-mode blast radius is the whole cluster.
- **Impersonation headers.** Workable but semantically off-label. `Impersonate-Extra-*` was
  designed for "this trusted client is acting as user X with these identity claims." Repurposing
  it as a generic payload-shipping mechanism for free-text reasons is, well, a repurposing. The
  RBAC story (`userextras/<key>` rules) was built to gate identity claims, not message text.
  Cluster operators reading the resulting RBAC will be surprised at why a generic backend has
  impersonation rights at all.
- **Service account tokens.** Dead end. SA-only frontends silently degrade to "no reason,
  backend identity."

The conclusion: `user.extra` is the option with the smallest gitops-reverser-side change but the
largest off-label Kubernetes-side usage. Its "cheapest credible" framing in the parent doc
deserves to be softened.

### RBAC summary

To make the impersonation path usable for a backend frontend:

- The backend's identity (typically a service account) needs `impersonate` on the user, on
  optional `groups`, on `uids`, and on each `userextras/<key>` it wants to set.
- The cluster operator should restrict `resourceNames` so the backend cannot impersonate
  arbitrary users or set arbitrary extra keys.
- Auditing the backend's own service account activity becomes important, because every
  user-attributed change in audit will have been issued by that backend. The audit
  `impersonatedUser` field, combined with the backend's authenticated identity, is what proves
  who actually made the call.

### Audit policy

The minimum audit policy needs to:

- record `userInfo` (always present on events)
- include the verbs and resources that gitops-reverser already watches at a level that produces
  an event at all (i.e. not `None`)

A `Metadata` level event already contains `userInfo.extra`, so no payload-level audit is required
for this transport.

If the cluster currently filters `user.extra` from audit logs for privacy reasons (some operators
do this in a downstream log pipeline rather than in audit policy itself), that filter has to be
loosened for the chosen reason key.

## Transport B: audit annotations from a mutating admission webhook

A different audit-carried path avoids both the off-label feel of impersonation and the audit
payload reconstruction the parent doc's stripped-annotation option requires. A mutating admission
webhook can attach key/value pairs directly to the audit event via the `auditAnnotations` field
on its `AdmissionResponse`, without modifying the object.

### What audit annotations are

Audit annotations are not object annotations. They are key/value pairs the apiserver records on
an audit event under the top-level `annotations:` field, alongside `authorization.k8s.io/decision`
and similar built-in entries. They exist precisely so admission controllers can attach decision
context to the audit log.

A mutating or validating webhook returns them in its `AdmissionResponse`:

```json
{
  "apiVersion": "admission.k8s.io/v1",
  "kind": "AdmissionReview",
  "response": {
    "uid": "...",
    "allowed": true,
    "patchType": "JSONPatch",
    "patch": "<base64 patch removing transient annotations>",
    "auditAnnotations": {
      "reason": "Increase checkout API memory after load-test failures"
    }
  }
}
```

The apiserver prefixes the keys with the webhook configuration name. If the webhook is named
`gitops-reverser-commit-context.example.com`, the audit event records:

```yaml
annotations:
  gitops-reverser-commit-context.example.com/reason: "Increase checkout API memory after load-test failures"
```

### Why this is cleaner than reading the request payload

Compared to the parent doc's "strip and read from request payload" version of the
transient-annotation transport:

- **Audit level.** Audit annotations are present on the event regardless of audit level. The
  request body is not. The parent doc's variant requires `Request` or higher audit on every
  watched resource type to recover the stripped annotation. The audit-annotation variant works
  at `Metadata` level, the same as `user.extra`.
- **Structured place.** The apiserver records audit annotations in a place explicitly designed
  for workflow context attached to a request. The consumer reads a known field, not a
  reconstructed object payload.
- **No payload reconstruction.** Patches, server-side apply, and create-vs-update variants all
  just work. The webhook sees the request, parses the transient annotation wherever it is, and
  emits one audit annotation. The consumer does not have to handle multiple request shapes.

### Worked example

1. The frontend sets a transient annotation on the resource update:

   ```yaml
   metadata:
     annotations:
       gitops-reverser.io/commit-message: "Increase checkout API memory after load-test failures"
   ```

2. The mutating admission webhook receives the request, reads the annotation, and returns:
   - a JSON patch that removes the transient annotation from the object
   - `auditAnnotations: { "reason": "Increase checkout API memory after load-test failures" }`

3. The apiserver applies the patch, persists the cleaned object, and emits an audit event:

   ```yaml
   user:
     username: alice@example.com
   annotations:
     gitops-reverser-commit-context.example.com/reason: "Increase checkout API memory after load-test failures"
   ```

4. gitops-reverser's audit consumer reads the annotation under the configured webhook prefix,
   attaches it to the matching pending write, and the commit window finalizes with that reason
   in the commit message. Author still comes from `user.username` per existing audit-backed
   authoring rules.

### Trade-offs

- **The webhook is still in the path,** but it is admission-time on the watched resource types
  only, not auth-time on every API call. That is a qualitatively different risk profile than the
  webhook-token-authenticator option in Transport A. If the webhook fails open (or fails closed
  for only a small set of resources), the blast radius is narrow.
- **Webhook-name prefix matters.** The apiserver prefixes audit-annotation keys with the webhook
  configuration name, so the audit consumer needs to know that prefix. It is a deployment-time
  configuration item.
- **Patch-versus-strip behaviour.** The webhook is responsible for stripping the transient
  annotation from the object. If it is misconfigured to a smaller resource scope than the audit
  consumer watches, transient annotations may leak into stored state.
- **Multi-request edits.** Per-event reasons. The same composition question as Transport A and
  the parent doc's stripped-annotation option.
- **Confidentiality.** Audit annotations land in audit logs the same way `user.extra` does.
  Length cap and PII review apply equally.

## Comparison: A vs B

| Property | A: `user.extra` | B: audit annotation from mutating webhook |
|----------|------------------|--------------------------------------------|
| Frontend mechanism | Impersonation headers (or auth-layer enrichment) | Transient object annotation, stripped by webhook |
| Required Kubernetes feature | Impersonation RBAC for `userextras/<key>` | `MutatingWebhookConfiguration` on watched types |
| Auth-path component | Yes if using webhook authenticator; no if using impersonation | No |
| Admission-path component | No | Yes, admission-time only |
| Required audit level | `Metadata` | `Metadata` |
| Author binding | `user.username` from impersonated identity | `user.username` from authenticated request |
| Off-label feel | High — repurposing identity-claim plumbing for payload | Low — audit annotations are designed for this |
| Operator surprise | RBAC grants impersonation rights to a backend | Standard mutating webhook with documented purpose |

Neither option is free. Transport A asks the cluster to grant impersonation rights to a frontend
backend and to accept that those rights will be used to ferry free-text reasons, not just
identity claims. Transport B asks the cluster to install one more mutating admission webhook, but
that webhook is using the audit-annotation field exactly as Kubernetes intends.

The recommendation that follows from this addendum is:

- Transport B (audit annotation via mutating webhook) is the cleanest audit-carried path and
  should be prototyped before Transport A.
- Transport A is still worth understanding because some clusters will already have OIDC or
  webhook-authenticator infrastructure that can populate `user.extra` essentially for free, in
  which case its off-label feel is less load-bearing.
- Both transports are per-event by nature. The aggregated `CommitContext` API in the parent doc
  remains useful as a complementary "send last" close-off for parallel-write frontends, not as a
  competitor.

## Open questions

- Should gitops-reverser define and document the canonical reason key, or accept a configurable
  key per `GitTarget` or per cluster?
- Does the consumer accept multi-value lists / multiple reasons in one event (joining them) or
  only the first value?
- Should the consumer surface a structured warning event when a reason was supplied but the
  audit-driven write was rejected for an unrelated reason, so the frontend learns that the
  reason was discarded along with the change?
- For Transport B, should gitops-reverser ship the mutating webhook itself, or define the
  contract and let operators install whichever webhook they prefer?
- How should audit-carried per-event reasons compose with a future aggregated `CommitContext`
  close-off message: are they joined, or does the close-off message replace them?

## References

### Kubernetes documentation
- [Dynamic Admission Control](https://kubernetes.io/docs/reference/access-authn-authz/extensible-admission-controllers/) — `auditAnnotations` field on the webhook response.
- [Audit Annotations](https://kubernetes.io/docs/reference/labels-annotations-taints/audit-annotations/) — catalog of built-in audit annotations and how they appear on events.
- [kube-apiserver Admission (v1) API reference](https://kubernetes.io/docs/reference/config-api/apiserver-admission.v1/) — formal `AdmissionResponse` schema.
- [Validating Admission Policy](https://kubernetes.io/docs/reference/access-authn-authz/validating-admission-policy/) — same audit-annotation pattern in CEL-based policies.

### Source-of-truth Go types
- [`admission/v1/types.go` in kubernetes/api](https://github.com/kubernetes/api/blob/master/admission/v1/types.go) — the `AdmissionResponse` struct and the `AuditAnnotations` field comment.
- [`admissionreview.go` in kubernetes/apiserver](https://github.com/kubernetes/apiserver/blob/master/pkg/admission/plugin/webhook/request/admissionreview.go) — apiserver-side handling that copies webhook audit annotations onto the audit event with the `<webhook-name>/<key>` prefix.

### Background and history
- [PR #58679 — support annotations for admission webhook](https://github.com/kubernetes/kubernetes/pull/58679/files) — original change introducing `auditAnnotations` on `AdmissionResponse`.
- [PR #115973 — KEP-3488: enforcement actions and audit annotations](https://github.com/kubernetes/kubernetes/pull/115973) — extension of the same pattern to `ValidatingAdmissionPolicy`.
- [Issue #125522 — `auditAnnotations` always included on the audit event](https://github.com/kubernetes/kubernetes/issues/125522) — discussion of when audit annotations actually surface on audit events.
