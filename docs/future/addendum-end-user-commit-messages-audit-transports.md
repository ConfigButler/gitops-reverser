# Idea: `user.extra` enrichment for end-user commit context

> Status: exploratory - companion to [idea-end-user-commit-messages.md](idea-end-user-commit-messages.md).
> Date: 2026-05-07

## Why this doc exists

The end-user commit messages design lists `user.extra` enrichment as the cheapest credible
transport, but glosses over how a real cluster populates that field. This doc collects what
gitops-reverser would actually depend on if it consumed a key like `gitops-reverser.io/reason` from
audit `userInfo.extra`.

It is not a Kubernetes tutorial. It is the subset of Kubernetes auth and audit behavior that
matters for the audit-stream-as-source-of-truth principle to hold for commit messages.

## What `user.extra` is

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

## How it surfaces in audit

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

## Sources of `user.extra` values

There are four practical ways a value lands in `user.extra`. Each has different operational and
trust properties.

### 1. OIDC structured authentication

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
    claimValidationRules: []
    userValidationRules: []
    extra:
      - key: "gitops-reverser.io/reason"
        valueExpression: "claims.gitops_reverser_reason"
```

This shape is appealing because the reason is bound to the user's authenticated session and the
mapping is centrally controlled by the cluster operator. It only works if the IdP can be made to
issue a per-request claim, which is unusual: OIDC tokens are typically reused for many requests in
a session, so the reason would either be missing or stale for most calls. Useful as a baseline
("this token was minted for a reason-aware UI") rather than a per-mutation channel.

### 2. Webhook token authenticator

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
it slows down or fails, the API server does. Building a custom webhook just to forward a reason
field is heavy.

### 3. Impersonation headers

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

This is the most plausible path for a real frontend. A backend service authenticates with its own
service account, validates the user's session itself, and then issues impersonated requests
carrying the reason in an extra header.

### 4. Service account tokens

Service account tokens, including projected tokens with bound audiences, do not carry custom
extras. They populate fixed fields like `authentication.kubernetes.io/pod-name`. There is no
mechanism for a SA-authenticated request to set an arbitrary `gitops-reverser.io/reason` value
without going through impersonation or a webhook authenticator.

The practical consequence is the one already noted in the parent doc: if a UI's only path to
Kubernetes is "POST to backend, backend talks to apiserver as its own SA," and that backend does
not impersonate, gitops-reverser will only ever see the backend's identity and no reason.

## RBAC summary

To make the impersonation path usable for a backend frontend:

- The backend's identity (typically a service account) needs `impersonate` on the user, on
  optional `groups`, on `uids`, and on each `userextras/<key>` it wants to set.
- The cluster operator should restrict `resourceNames` so the backend cannot impersonate
  arbitrary users or set arbitrary extra keys.
- Auditing the backend's own service account activity becomes important, because every
  user-attributed change in audit will have been issued by that backend. The audit `impersonatedUser`
  field, combined with the backend's authenticated identity, is what proves who actually made the
  call.

## Audit policy

The minimum audit policy needs to:

- record `userInfo` (always present on events)
- include the verbs and resources that gitops-reverser already watches at a level that produces
  an event at all (i.e. not `None`)

A `Metadata` level event already contains `userInfo.extra`, so no payload-level audit is required
for this transport. Compare with the transient-annotation option, which requires `Request` or
higher to recover the stripped annotation from the request body.

If the cluster currently filters `user.extra` from audit logs for privacy reasons (some operators
do this in a downstream log pipeline rather than in audit policy itself), that filter has to be
loosened for the chosen reason key.

## Worked example: the `gitops-reverser.io/reason` key

Putting the pieces together for the most likely deployment shape:

1. A frontend collects the reason text from the user along with the resource change.
2. The frontend's backend service authenticates the user (its own session model) and issues the
   resource update against Kubernetes via impersonation.
3. The impersonated request carries headers:

   ```http
   Impersonate-User: alice@example.com
   Impersonate-Extra-gitops-reverser.io%2Freason: Increase checkout API memory after load-test failures
   ```

4. The Kubernetes API server checks RBAC, allows the impersonation, and processes the update.
5. The audit pipeline emits an event with:

   ```yaml
   user:
     username: alice@example.com
     extra:
       gitops-reverser.io/reason:
         - "Increase checkout API memory after load-test failures"
   ```

6. gitops-reverser's audit consumer reads `user.extra["gitops-reverser.io/reason"]` and attaches it
   to the pending write that corresponds to this audit event.
7. When the commit window finalizes, the commit message renders the reason. Author is taken from
   `user.username` per existing audit-backed authoring rules.

## Limitations and risks

- **Impersonation is the bottleneck.** Without it, SA-only backends silently degrade to "no
  reason, backend identity." The feature is only as good as the auth layer below it.
- **Header encoding.** `/`, `.`, and other characters in extra keys need percent-encoding in
  `Impersonate-Extra-*` header names. Tooling that constructs these headers must do that
  consistently or the API server will return 400 or silently drop the value.
- **Multi-request edits.** One reason per request fits the per-event model well, but if a single
  user intent spans several requests, gitops-reverser still has to decide how to combine multiple
  reasons in one commit window. That decision is shared with the transient-annotation option.
- **Audit log volume.** The reason text becomes part of every relevant audit event. Long or
  noisy reasons inflate audit storage. A length cap and a `Metadata`-level audit suffice in most
  clusters, but operators with strict retention budgets should know.
- **PII and sensitive content.** Reasons are user-supplied free text and end up in audit logs and
  git history. Both surfaces should be treated as part of the data classification when this
  feature is offered to end users.
- **No native SA path.** There is no "annotate this token with extras" mechanism for projected
  SA tokens. Anyone hoping to skip impersonation by minting a custom SA token will not find a
  supported way to attach `gitops-reverser.io/reason`.

## Open questions

- Should gitops-reverser define and document the canonical extra key (`gitops-reverser.io/reason`),
  or accept a configurable key per `GitTarget` or per cluster? A configurable key complicates
  RBAC examples but matches multi-tenant clusters.
- Does the consumer accept multi-value `extra` lists (joining them) or only the first value? The
  `[]string` shape allows multiple values; the simplest rule is "join with newlines, cap length."
- Should the consumer surface a structured warning event when a reason was supplied but the
  audit-driven write was rejected for an unrelated reason, so the frontend learns that the reason
  was discarded along with the change?
- How should this interact with the future aggregated `CommitContext` API (parent doc): are
  per-event reasons composed with a "close-off" message, or does the close-off message replace
  them?
