# Gitea SSH Commit Signing Verification — Findings & Plan

This document captures what we learned debugging
`gpg.error.no_gpg_keys_found` on SSH-signed commits in Gitea 1.25.5, the
reusable `internal/giteaclient` library that came out of it, and the
concrete steps to push the e2e suite to a level where SSH commit
verification is asserted end-to-end instead of only checked locally.

Written 2026-04-15 after a live debugging session against
`gitea-e2e` running Gitea 1.25.5.

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
6. Sign the token:
   `ssh-keygen -Y sign -n gitea -f <privkey> <tokenfile>`.
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

## Current e2e state and what to migrate

Today, [test/e2e/gitea_api_test.go](../../test/e2e/gitea_api_test.go)
contains a compact Gitea HTTP helper layer, and
[test/e2e/signing_e2e_test.go](../../test/e2e/signing_e2e_test.go)
uses it to upload signing keys and spot-check commit verification. The
spot-check today is loose because it cannot assert `verified=true`
without the web-UI verify step.

### What the new library enables

- **Tight assertion**: after a commit lands, the suite can call
  `sess.VerifySSHKey(...)` and then assert
  `GetCommitVerification(...).Verified == true`, closing the loop the
  original investigation prompt wanted.
- **Idempotent, code-path-consistent setup**: both SSH-auth and
  SSH-signing paths can share `EnsureUser`, which always returns a
  usable password; the "user already exists → 422 → no password"
  branch in the current helpers goes away.
- **Per-repo trust model**: the
  [e2e-signing-followups.md](e2e-signing-followups.md) work already
  tracks per-repo Gitea users; adding `trust_model` becomes trivial.

---

## Proposed plan, in order

Each step is independently shippable. Stop after step 2 if you only
want tighter assertions; steps 3+ are broader investments.

### 1. Wire `VerifySSHKey` into the signing e2e suite (small, high value)

After `RegisterSigningPublicKeyAs(...)` in
[test/e2e/signing_e2e_test.go](../../test/e2e/signing_e2e_test.go),
call:

```go
sess, err := giteaclient.NewWebSession(ctx, giteaHost, user.Login, user.Password, false)
Expect(err).NotTo(HaveOccurred())
token, err := userClient.GetVerificationToken(ctx)
Expect(err).NotTo(HaveOccurred())
armored, err := signTokenWithSSHKeygen(workDir, privPath, token)
Expect(err).NotTo(HaveOccurred())
Expect(sess.VerifySSHKey(ctx, pubKeyStr, fingerprint, armored)).To(Succeed())
```

Then change `assertGiteaVerified` to require
`CommitVerification.Verified == true` and a non-nil `Signer` whose
email matches the per-repo user.

Blocker: the existing `SetupRepo` path funnels through
`hack/e2e/gitea-run-setup.sh` / `CreateTestUser` in
[test/e2e/gitea_api_test.go](../../test/e2e/gitea_api_test.go), which
does not capture or rotate passwords reliably. Fix by switching
`CreateTestUser` to `giteaclient.Client.EnsureUser`, which always
returns a known password.

Out-of-scope: cleaning up registered keys. Match the existing
[e2e-signing-followups.md](e2e-signing-followups.md) decision not to
delete.

### 2. Replace `test/e2e/gitea_api_test.go` with thin wrappers over `giteaclient`

Delete open-coded HTTP helpers. Keep Ginkgo-shaped wrappers
(`SetupRepo`, `EnsureRepoCollaborator`, `RegisterSigningPublicKeyAs`,
etc.) that call into the new package. Net lines removed should exceed
net lines added.

Watch-outs:
- `giteaTestUser` is already structurally identical to
  `giteaclient.TestUser`. Unify.
- The test suite's expectation that existing users return an empty
  password string needs to change — `EnsureUser` rotates the password
  on reuse, which is a behaviour change the tests will need to handle.

### 3. Bootstrap convenience on the client

Add a single top-level `SetupSignedCommitTestFixture(ctx, opts)` on
`giteaclient` that returns `(user, repo, keyPair, webSession)` after
doing steps 1–9 of "the verified flow that works" above. The debug
CLI becomes a thin caller. The signing e2e test becomes a thin
caller. Any future consumer (audit-consumer tests, webhook tests)
gets the same flow for free.

### 4. Defensive coverage in `giteaclient`

- Harden the CSRF regex / login heuristic in
  [webclient.go](../../internal/giteaclient/webclient.go) against
  future Gitea template changes: look for the Gitea-specific session
  cookie name instead of body heuristics to detect login success;
  pick up the CSRF from the cookie Gitea also sets (`_csrf`) as a
  fallback.
- Add table-driven tests that parse real Gitea HTML fixtures for the
  login page, the keys settings page (pre-verify and post-verify),
  and the flash-error and flash-success variants.
- Add a `VerifySSHKeyWithKeygen(workDir, privPath)` convenience that
  takes the private key path and does the token fetch + signing
  internally, so consumers do not need to re-implement the
  `ssh-keygen -Y sign -n gitea` shell-out.

### 5. Re-evaluate the signing e2e design

Once `verified=true` can be asserted reliably, revisit
[commit-signing-design.md](commit-signing-design.md) and
[e2e-signing-followups.md](e2e-signing-followups.md). Notes:

- **Keep** `assertLocalSSHVerification` (the `git verify-commit` /
  `ssh-keygen -Y verify` pair). Gitea-side verification proves Gitea
  accepts our signatures; the local checks prove the commits are also
  compatible with vanilla git tooling. Both signals are useful — they
  catch different classes of regression (Gitea wiring vs signing-format
  correctness) and should be layered, not replaced.
- *Optionally* consider unifying BYOK and generated-key assertion
  helpers if they end up near-identical after the Gitea-side assertion
  lands. This is not a strong recommendation: separate helpers are
  fine as long as the tests stay clean and don't leak "how the suite
  was launched" into assertion logic.

---

## Shell scripts vs `giteaclient`: what stays, what moves

Concrete read of the two scripts that drive today's e2e Gitea setup:

### `hack/e2e/gitea-bootstrap.sh` — keep as a script

Cluster-scoped, once-per-cluster work: wait for the API, ensure the
`testorg` org exists, write `api.ready` / `org-<name>.ready` / `ready`
stamps. This is the right home for it:

- It runs before any Go test process exists — it is what makes the
  cluster *ready enough* for tests to start.
- Its outputs are shared across every test run and every repo, so
  file-based stamps are the natural contract.
- It has no per-test dynamic state worth pulling into Go.

No change proposed.

### `hack/e2e/gitea-run-setup.sh` — split

This script mixes two concerns:

**Stays as shell** (cluster/ssh infra, not per-test state):

- `generate_known_hosts` — runs a temporary `kubectl port-forward` and
  shells out to `ssh-keyscan`. This is bash's comfort zone and the
  output (`known_hosts`) is a file consumed by kubectl-created secrets;
  there is no value in rebuilding it in Go.
- The `kubectl create secret ... --dry-run=client -o yaml` manifest
  rendering in `write_secrets_manifest`. The output is a YAML artifact
  consumed by `kubectl apply`; keeping `kubectl` as the renderer means
  the manifests match exactly what a human operator would produce.
- `ensure_checkout` — `git clone` + local git config. Shell is fine.
- Flux-receiver wiring (`read_flux_receiver_token`,
  `wait_for_flux_receiver_path`, `ensure_repo_webhook`). This reaches
  into Kubernetes (`kubectl get receiver`, secret lookups), not Gitea
  user state. Keep it with the bootstrap-adjacent tooling.

**Moves to `internal/giteaclient`** (user/repo/key/token state):

- `create_token` → a `CreateUserToken(login, name, scopes)` method.
  The token value flows back to the caller as a return value instead
  of being persisted in `token.txt`. The caller decides what to do
  with it.
- `configure_ssh_key_in_gitea` (reset admin's keys, upload one) → this
  is exactly what `ListUserKeys` + `RegisterUserKeyAsAdmin` /
  `RegisterUserKeyAsUser` already cover. The "reset" step becomes a
  `DeleteUserKey` helper if we need it; for most cases
  `FindUserKey` idempotency is enough.
- `ensure_repo` → `CreateUserRepo` / a new `CreateOrgRepo`. Already
  half-implemented.
- The per-repo user / per-repo password / per-repo signing key flow
  that steps 1–2 of "Proposed plan" introduce has *no* shell
  counterpart today — it is native to the library.

Result: the script shrinks to known_hosts + secrets manifest rendering
+ checkout + flux webhook. Everything Gitea-API-shaped lives in Go,
which is where the tests that consume it already live.

## Boundary: Taskfile, `.stamps`, in-memory e2e state

Worth making explicit because the current structure has some drift.
Proposed rule of thumb:

| Layer               | Scope                                    | Persistence       |
|---------------------|------------------------------------------|-------------------|
| Taskfile targets    | Orchestration + ordering                 | none (task graph) |
| `.stamps/` files    | Cluster-scoped, cross-invocation cache   | disk              |
| `giteaclient` state | Per-test dynamic state                   | in-memory (Go)    |

- **Stamps are for task-graph dependencies** between *shell* steps,
  not for communicating with Go tests. Anything like `ready`,
  `api.ready`, `org-testorg.ready`, `repo.ready` — where a later task
  target wants to skip work if an earlier one already ran — is a
  legitimate stamp.
- **E2E tests should not write to `.stamps/`**. The user's instinct
  here is correct. If a test needs a token, a repo URL, or a password,
  it should get it from a library call that returns the value, not by
  reading a file written by a shell script it happens to run after.
  Files on disk are a global, racy, stale-prone channel; Go struct
  fields are scoped and typed.
- **E2E tests *may read* stamps** for the narrow case of "did the
  cluster-level bootstrap run?" (e.g. `ready`). Reading a disk stamp
  to answer "is the cluster configured" is fine; reading one to get
  dynamic per-test values is not.
- **Today's drift**: `token.txt`, `checkout-path.txt`,
  `active-repo.txt`, `receiver-webhook-url.txt` are per-repo dynamic
  values being routed through disk. Some of that is unavoidable
  (the `kubectl apply`'d secret needs the token baked into YAML before
  Go tests run). The right cut is:
  - Values consumed only by *shell* (secrets manifest input): stay on
    disk, written by the shell that uses them.
  - Values consumed by *Go tests*: should be returned by a Go call,
    not read from a stamp. When we migrate the "user" parts of
    `gitea-run-setup.sh` into `giteaclient`, those values naturally
    stop being written to disk.

This cleanly explains why bootstrap and SSH infra stay shell-shaped
(they produce files for other shell/kubectl steps) while the
user/repo/key machinery becomes pure library (it produces values for
Go code).

## Risks and open questions

- **Gitea version pinning**. The web form field names (`title`,
  `content`, `fingerprint`, `signature`, `_csrf`, `type`) are stable
  in 1.25.x but are internal template contracts and could drift.
  Mitigation: add a smoke test that runs the debug CLI against every
  Gitea version the project supports (currently only 1.25.5).
- **Upstream feature request**. Gitea genuinely should expose an SSH
  key verify REST endpoint (parity with `/user/gpg_key_verify`). A
  small upstream PR adding `POST /user/keys/{id}/verify` taking the
  same `{signature}` body would eliminate the web-form dependency
  entirely. Low-risk, well-scoped, and the tests for it already exist
  in the GPG path to copy from. Worth proposing.
- **Trust model matrix**. The e2e currently tests only one model per
  scenario. We should fix on `default` (or whatever Gitea's instance
  default is) and document that explicitly, so future investigations
  don't re-chase the trust-model red herring.
- **Web session reuse**. `WebSession` is a single-user object. If
  future e2e scenarios want to verify multiple keys across multiple
  users, callers will need to instantiate one per user. This is fine
  but worth noting in the docstring.

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
