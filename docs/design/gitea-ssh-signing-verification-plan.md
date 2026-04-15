# Gitea SSH Commit Signing Verification — Findings & Plan

This document captures what we learned debugging
`gpg.error.no_gpg_keys_found` on SSH-signed commits in Gitea 1.25.5, the
reusable `internal/giteaclient` library that came out of it, and the
concrete steps to push the e2e suite to a level where SSH commit
verification is asserted end-to-end instead of only checked locally.

Written 2026-04-15 after a live debugging session against
`gitea-e2e` running Gitea 1.25.5.

Keep this as the investigation and migration record. The core signing
path is now implemented and green; remaining items here are optional
cleanup unless explicitly called out otherwise.

---

## Implementation Status

Status as of 2026-04-15:

- ✅ `test/e2e/gitea_api_test.go` now uses thin wrappers over
  `internal/giteaclient` instead of keeping its own open-coded Gitea
  HTTP client.
- ✅ `CreateTestUser(...)` now goes through
  `giteaclient.Client.EnsureUser(...)`, so reruns get a known usable
  password instead of the old "user exists but password is unknown"
  dead-end.
- ✅ `SetupRepo(...)` continues to create a per-repo Gitea user and add
  it as collaborator on the shared `testorg/<repo>` repository.
- ✅ The generated-key and BYOK signing e2e scenarios now perform the
  full SSH-key verification flow against the Gitea web UI before
  asserting commit verification.
- ✅ `internal/giteaclient.Client.VerifySSHKey(...)` now signs the Gitea
  verification token in process via the shared
  `internal/sshsig` helper instead of shelling out to `ssh-keygen`.
- ✅ `assertGiteaVerified(...)` is now strict: it requires
  `verified=true`, a non-nil signer, and the expected signer email.
- ✅ For generated keys, the suite reads `signing.key` from the
  Kubernetes signing Secret so the test can prove possession of the
  generated private key instead of only checking the public key.
- ✅ Repo-level validation for this pass completed successfully:
  `task lint`, `task test`, `docker info`, `task test-e2e`,
  `task test-e2e-quickstart-manifest`, and
  `task test-e2e-quickstart-helm`.

Remaining optional follow-ups:

- A higher-level `giteaclient` fixture/bootstrap helper could still
  reduce duplication between the debug CLI and some tests, but it is no
  longer needed to make signing verification work.
- `internal/giteaclient/webclient.go` can still be hardened further, but
  the current cookie-based login detection, fixture-based tests, and
  green e2e runs already cover the critical path.

---

## TL;DR

- Gitea's `public_key.verified` column **is** the gate on SSH commit
  signature verification. Unverified keys are filtered out before the
  commit API ever tries to match them
  ([services/asymkey/commit.go:384](../../external-sources/gitea/services/asymkey/commit.go#L384)).
- That column can only be flipped via the **web UI** (POST
  `/user/settings/keys?type=verify_ssh`). **No REST API endpoint
  exists** for SSH key verification — `/user/keys/verify` is not a
  Gitea route; only `/user/gpg_key_verify` exists and it is GPG-only.
- Uploaded via admin (POST `/admin/users/{u}/keys`) the key lands
  `verified=false`. Uploaded via user push over SSH it updates
  `last_used_at` but still leaves `verified=false`.
- The repo `trust_model` does **not** gate verification itself; it only
  changes how a matched signature is *labeled* (trusted vs unmatched).
  An e2e assertion on `verified=true` works under `default`,
  `committer`, and the other two values.
- The `gpg.error.no_gpg_keys_found` error message is GPG-flavoured even
  for SSH signatures — it is Gitea's generic "no key matches" error
  returned from the fall-through path when the SSH branch finds no
  verified keys. That wording wasted significant investigation time.

---

## Background: what was tried and ruled out

Initial hypotheses (from the investigation prompt):

- ❌ **Committer email mismatch** — tested matched and mismatched. No
  change.
- ❌ **Trust model** — tested all four. No change while
  `verified=false`.
- ❌ **SSH auth recognition** — the same key used for push updated
  `last_used_at` but did not set `verified`.
- ❌ **Re-uploading the key for a second user** — Gitea enforces a
  fingerprint unique constraint; same key cannot belong to two users.
- ✅ **`public_key.verified = 0` being the gate** — confirmed by
  reading Gitea source.

---

## The Gitea code path that matters

In Gitea 1.25.5,
[services/asymkey/commit.go:367](../../external-sources/gitea/services/asymkey/commit.go#L367)
implements `parseCommitWithSSHSignature`. The decisive block is:

```go
keys, err := db.Find[asymkey_model.PublicKey](ctx, asymkey_model.FindPublicKeyOptions{
    OwnerID:    committerUser.ID,
    NotKeytype: asymkey_model.KeyTypePrincipal,
})
...
for _, k := range keys {
    if k.Verified {  // ← hard filter
        commitVerification := verifySSHCommitVerification(...)
        if commitVerification != nil {
            return commitVerification
        }
    }
}
```

When every candidate key has `Verified=false`, the loop never enters
`verifySSHCommitVerification`, and the function falls through to
`NoKeyFound` — rendered to clients as the ambiguous
`gpg.error.no_gpg_keys_found`.

The flipping code lives in
[models/asymkey/ssh_key_verify.go:17](../../external-sources/gitea/models/asymkey/ssh_key_verify.go#L17)
(`VerifySSHKey`). It is invoked from exactly one place:
[routers/web/user/setting/keys.go:220](../../external-sources/gitea/routers/web/user/setting/keys.go#L220) —
the web form handler. There is no API wrapper.

---

## The verified flow that works

1. Create user (REST `POST /admin/users`) — capture the generated
   password; you will need it to log into the web UI as that user.
2. Create repo owned by the user (REST `POST /admin/users/{u}/repos`).
3. Upload public key via admin (REST `POST /admin/users/{u}/keys`).
   Lands with `verified=false`.
4. Make an SSH-signed commit, push over HTTPS with the user's
   credentials.
5. GET `/user/gpg_key_token` **as the user** (REST, basic auth). Gitea
   returns a deterministic token derived from user id, creation time,
   and a one-minute time window.
6. Sign the token for the `gitea` namespace.
   - During the original investigation this was reproduced with
     `ssh-keygen -Y sign -n gitea`.
   - The current reusable client path signs in process through
     `internal/giteaclient.Client.VerifySSHKey(...)`.
7. Log into the web UI (POST `/user/login` with `_csrf`, `user_name`,
   `password`, `remember=off`). Capture the `i_like_gitea` session
   cookie.
8. Fetch a fresh CSRF token from `/user/settings/keys`.
9. POST `/user/settings/keys` with form fields:
   - `_csrf` — the token from step 8
   - `type=verify_ssh`
   - `title=none` — Gitea's template sets it unconditionally
   - `content=<armored_public_key>` — required, else "Content cannot
     be empty"
   - `fingerprint=<SHA256:...>` — matches the key Gitea already has
   - `signature=<armored SSH signature from step 6>`
10. Re-query the commit API — now returns `verified=true` with a
    non-nil signer.

Only steps 5–9 are unusual. The rest is ordinary Gitea API traffic.

---

## Reusable library: `internal/giteaclient`

The debugging session produced a focused, reusable Gitea client that
consolidates the machinery the e2e suite currently open-codes in
[test/e2e/gitea_api_test.go](../../test/e2e/gitea_api_test.go).

- [client.go](../../internal/giteaclient/client.go) — `Client` with
  basic auth, `Do()` JSON helper, path-escape and body-truncate
  utilities.
- [users.go](../../internal/giteaclient/users.go) — `GetUser`,
  `CreateUser`, `SetUserPassword`, `EnsureUser`. `EnsureUser` rotates
  passwords on reuse so callers can always re-authenticate as the user
  (important because key verification needs the user's credentials, not
  admin's).
- [keys.go](../../internal/giteaclient/keys.go) — `ListUserKeys`,
  `FindUserKey`, `RegisterUserKeyAsAdmin`, `RegisterUserKeyAsUser`,
  `GetVerificationToken`, `GetKeyRaw`, `NormalizeAuthorizedKey`.
- [repos.go](../../internal/giteaclient/repos.go) — `CreateUserRepo`
  (with `trustModel`), `GetRepo`, `DeleteRepo`,
  `EnsureCollaborator`, `GetCommitVerification`.
- [webclient.go](../../internal/giteaclient/webclient.go) —
  `WebSession` for the UI-only flows. `NewWebSession` performs the
  form-login dance; `VerifySSHKey` POSTs the verify form with the
  exact fields the browser sends. Includes an optional `Debug` flag
  that prints login/verify HTTP results and snippets around flash
  markers.

The debug CLI
[cmd/gitea-signing-debug/main.go](../../cmd/gitea-signing-debug/main.go)
exercises every step and prints a pre/post DIFF of the commit API's
verification block. Useful both as a regression tool and as worked
example for consumers.

---

## Current State

The important outcome of this investigation is now implemented:

- the e2e suite creates or reuses a per-repo Gitea user
- the signing key is registered under that user
- the suite verifies the key through Gitea's web-only `verify_ssh`
  flow
- the suite then asserts `verification.verified == true`

That means the old migration plan is no longer the main thing to read.
What still matters from this document is:

- the explanation of why `public_key.verified` was the missing gate
- the exact verified flow that works against Gitea 1.25.x
- the shape of the reusable `internal/giteaclient` support that came
  out of the investigation

## Optional Cleanup Still Worth Considering

- Add a higher-level `giteaclient` fixture/bootstrap helper if the
  remaining callers still feel repetitive.
- Continue hardening `internal/giteaclient/webclient.go` if future
  Gitea template changes make the HTML parsing more fragile.
- Consider proposing an upstream Gitea API for SSH-key verification so
  the web-form dependency can disappear entirely.
- Keep the local verification layer (`ssh-keygen -Y verify`,
  `git verify-commit`) even though Gitea verification is now green;
  those checks still catch different regressions.

---

## Quick reference

Run the debug tool against a live Gitea:

```
go run ./cmd/gitea-signing-debug \
  --gitea-url http://localhost:13000/api/v1 \
  --admin-user giteaadmin --admin-pass giteapassword123
```

Flags worth knowing:

- `--keep` — leave the user, repo, workdir, and private key in place
  for post-mortem; the workdir path is printed.
- `--trust-model default` — prove trust model does not gate
  verification.
- `--verify-web=false` — skip the UI verify step to reproduce the
  failing-baseline behaviour.
