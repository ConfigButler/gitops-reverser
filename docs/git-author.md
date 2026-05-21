# gitops-reverser improvement plan

## Title

Use OIDC display name and email from audit `user.extra` for the git commit author

## Body

`gitops-reverser` attributes each commit to the user that made the cluster
change. Today the author name and email are both derived from a single string —
the Kubernetes username — which produces poor results for OIDC users.

With an OIDC-authenticated user, the Kubernetes username is the issuer-prefixed
identity, e.g. `https://keycloak.cozy.z65.nl/realms/cozy#simon`. The resulting
commit is:

```text
Author: https://keycloak.cozy.z65.nl/realms/cozy#simon <httpskeycloak.cozy.z65.nlrealmscozysimon@noreply.cluster.local>
```

The email is mangled because [`ConstructSafeEmail`](../internal/git/commit.go)
strips every character outside `[a-z0-9.-]` from the username and appends
`@noreply.cluster.local`. The name is the raw username.

The real display name and email *are* available — they just are not used.
A Kubernetes audit event carries `Event.User.Extra` (`map[string]ExtraValue`,
where `ExtraValue` is `[]string`), and the API server can be configured (via
structured authentication configuration) to map the OIDC `name` and `email`
claims into that map. When it does, `gitops-reverser` still ignores them:
`resolveUserInfo` reads only `event.User.Username`.

We would like commits to read:

```text
Author: Simon Koudijs <simon@koudijs.dev>
```

## Requested change

Let `gitops-reverser` source the git author **display name** and **email** from
two fixed keys in the audit event's `user.extra` map, falling back to the
current behaviour when those keys are absent or carry an unusable value.

To start simple, the key names are **hardcoded** rather than configurable:

```text
configbutler.ai/claims/display-name    e.g. "Simon Koudijs"
configbutler.ai/claims/email           e.g. "simon@koudijs.dev"
```

These match the extras published by our reference kube-apiserver structured
authentication configuration. Making the key names configurable is a possible
later enhancement — deferred until a second cluster actually needs different
names.

Behaviour:

- If the display-name extra is present and its value is usable, use it for the
  git author `Name`; otherwise use `Username` as today.
- If the email extra is present and its value is usable, use it for the git
  author `Email`; otherwise fall back to `ConstructSafeEmail(Username)` as today.
- When neither extra is present, behaviour is byte-for-byte unchanged. This
  keeps the change fully backward compatible.

### Identity selection (impersonation)

`resolveUserInfo` already prefers the impersonated user over the authenticated
user when impersonation is in play. That behaviour does not change. The new code
reads `Extra` from **the same user that wins** — if the impersonated user is
used, read `ImpersonatedUser.Extra`; otherwise read `User.Extra`. Display name,
email, and username always describe one consistent identity.

### Keeping it safe

`user.extra` values originate from an external identity provider, so they are
untrusted input being placed into a git signature header. Two guards keep this
safe:

- **Multi-value handling.** `ExtraValue` is a `[]string`. Treat an empty slice
  as absent; use the first element otherwise.
- **Value validation before use.** A name or email containing a newline or
  control character would corrupt the git commit object. Before using an extra
  value:
  - *Display name*: reject it if it contains control characters or newlines, or
    is empty/whitespace after trimming. On rejection, fall back to `Username`.
  - *Email*: accept it only if it matches the same valid-email regex already
    used inside `ConstructSafeEmail`. On rejection, fall back to
    `ConstructSafeEmail(Username)`.

Both fallbacks are exactly today's behaviour, so a malformed claim degrades
gracefully rather than producing a broken commit.

`email-verified` is also published as a separate extra
(`configbutler.ai/claims/email-verified`), so `gitops-reverser` could later
decline to use an unverified email. That is a possible enhancement, not part of
this change.

## Implementation pointers

The change is localised to the `git` and `queue` packages. Module
`github.com/ConfigButler/gitops-reverser`, observed at release `0.24.0`:

- **Hardcoded keys.** Define the two extra-key constants near `resolveUserInfo`
  in [`internal/queue/redis_audit_consumer.go`](../internal/queue/redis_audit_consumer.go),
  e.g. `displayNameExtraKey` and `emailExtraKey`.
- **`internal/git/types.go`** — extend `UserInfo` with optional
  `DisplayName string` and `Email string` fields. Adding fields is additive; the
  ~20 existing `UserInfo{Username: ...}` test constructions are unaffected.
- **`internal/queue/redis_audit_consumer.go`** — `resolveUserInfo()` currently
  returns `git.UserInfo{Username: username}`. After selecting the effective
  user (authenticated or impersonated), read that user's `Extra` at the two
  hardcoded keys, apply the multi-value and validation rules above, and populate
  the new `DisplayName` / `Email` fields. Leaving a field empty signals "fall
  back".
- **`internal/git/pending_writes.go`** — `PendingWrite.Author()` returns a bare
  username string. Add an accessor that surfaces the full author identity
  (e.g. `AuthorUserInfo()` returning the `UserInfo` of `Events[0]`) so the
  display name and email reach commit time. Prefer this over a parallel
  `AuthorSignature()` helper.
- **`internal/git/commit.go`** — `commitOptionsFor()` builds the
  `object.Signature`. Use `UserInfo.DisplayName` for `Name` and `UserInfo.Email`
  for `Email` when present; keep `Username` + `ConstructSafeEmail` as the
  fallback. Validation can live here, next to `ConstructSafeEmail`, so the git
  package owns "what is safe in a signature".
- **`internal/git/open_window.go`** — commit-window coalescing keys on
  `Author` (the username) at `open_window.go:50`/`open_window.go:61`. Keep that
  key as the username — it is the stable identity used for coalescing and for
  the grouped commit *message* `{{.Author}}`. Only the commit's `Author:`
  signature gains the display name; the message text is unchanged.

### Scope note

Author identity flows through per-event and grouped (commit-window) commits.
Atomic / reconcile-driven commits set the operator as the author already
(`commitOptionsFor` returns the committer when `PendingWrite.Author()` is empty)
and are intentionally left unchanged by this work.

## Why this matters

`gitops-reverser` turns cluster changes into a git history. A git history is
only useful for audit and `git blame` if the author is a real, recognisable
identity. With OIDC — which is the recommended way to authenticate real humans
to a cluster — the current output is an unreadable URL fragment and a
non-deliverable email address.

The fix does not require `gitops-reverser` to understand OIDC, talk to an
identity provider, or hold any credentials. It only needs to read two extra
fields that the Kubernetes audit event already delivers. The API-server side
(mapping `name`/`email` claims into `user.extra`) is standard, supported
Kubernetes configuration and is the operator's responsibility.

## Verification

With the API server configured to map `name` → `configbutler.ai/claims/display-name`
and `email` → `configbutler.ai/claims/email`, a commit made by an OIDC user
should read:

```text
Author: Simon Koudijs <simon@koudijs.dev>
```

A cluster whose API server publishes neither extra should produce today's
output for an identical change, confirming backward compatibility. A claim
carrying a malformed value (newline in the name, non-email string in the email)
should also fall back to today's output rather than producing a broken commit.
