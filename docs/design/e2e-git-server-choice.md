# The e2e Git server: stay on Gitea, or move to Forgejo?

> **design** — open, not yet built. Index: [`../INDEX.md`](../INDEX.md)
>
> Scope is test infrastructure only: no shipped code path talks to the Git server. This document
> records what was measured in both upstreams, the decision **not** to adopt a Go SDK either way, and
> — the point it turns on — that the pin forcing this decision is fixable **without** migrating.

## The forcing function is gone

The premise for migrating was that we are stuck. [`test/e2e/setup/flux/releases/gitea.yaml`](../../test/e2e/setup/flux/releases/gitea.yaml)
holds chart `12.5.3` (Gitea 1.25.5) deliberately, because Gitea 1.26 removed the `_csrf` form input
and cookie from `/user/login`, which [`internal/giteaclient/webclient.go`](../../internal/giteaclient/webclient.go)
scrapes in order to log in and verify an SSH signing key.

That premise does not survive contact with the source. **Gitea and Forgejo made the same change**:
both replaced form-token CSRF with Go 1.25's stdlib `http.CrossOriginProtection`.

| | Gitea (`v1.28.0-dev`) | Forgejo |
|---|---|---|
| `_csrf` in templates | **0 matches** | **0 matches** |
| `services/context/csrf.go` | deleted | deleted |
| Replacement | `http.NewCrossOriginProtection()` `routers/web/web.go:170` | same, `routers/web/web.go:228` |
| Guard | `if !options.SignOutRequired && !options.DisableCrossOriginProtection` `web.go:212` | `if !options.SignOutRequired` `web.go:286` |

`http.CrossOriginProtection.Check` returns `nil` when a request carries **neither** `Sec-Fetch-Site`
nor `Origin` (`net/http/csrf.go:154-159`, Go 1.26.5). A plain Go `http.Client` sends neither, so it
passes unconditionally on both. `/user/login` is additionally exempt on both, being `SignOutRequired`.

**Therefore the fix is the same on either server, and it is a deletion:** remove `fetchCSRF`,
`extractCSRFToken`, `csrfInputRE`, and the two `form.Set("_csrf", …)` calls — roughly 45 lines. That
unpins Gitea *in place*. Migration and unpinning are **not** the same piece of work, and an earlier
draft of this document wrongly claimed they were.

## What that leaves

With the pin removable either way, the comparison is no longer "migrate or stay broken". It is a
straight cost comparison, and Gitea wins it on cost.

### Staying on Gitea costs one deletion

Everything else in our integration is already correct for Gitea and stays correct:

- **Signing namespace unchanged.** Gitea still verifies with the literal `"gitea"`
  (`models/asymkey/ssh_key_verify.go:28`), which is exactly what `verification.go:116` signs with.
- **Session cookie unchanged** — `i_like_gitea` (`modules/setting/session.go:37`).
- **Chart source unchanged** — the existing HTTP `HelmRepository` at `dl.gitea.com/charts`.
- **Service names, values, hostnames, scripts** — all unchanged.

### Moving to Forgejo costs four changes, one of them risky

- **Signing namespace becomes `setting.Domain`** (`ssh_key_verify.go:34`). And `setting.Domain` is
  not `[server] DOMAIN`: `modules/setting/server.go:150` defaults it to `localhost`, then `:263-267`
  overwrites it with the **`ROOT_URL` hostname** when that is non-empty and not a bare IP. So the
  namespace becomes `gitea-http.gitea-e2e.svc.cluster.local` — host only, no scheme or port.
  **This is the one genuinely risky item in either direction.** A wrong namespace produces a
  *valid-looking* signature that the server rejects with a generic
  `settings.ssh_invalid_token_signature` flash, so it reads as a credentials or key-material bug.
- **Session cookie becomes `session`** (`modules/setting/session.go:37`, commit `97a3837215`,
  v15.0.0, flagged breaking upstream). There are **zero** `i_like_*` literals anywhere in the Forgejo
  tree. Our `webclient.go:23` hardcodes the old name and `NewWebSession` hard-fails without it.
- **Chart moves to OCI** — `oci://code.forgejo.org/forgejo-helm/forgejo`, chart `17.1.3`,
  appVersion `15.0.5` (verified with `helm show chart`). Our `HelmRepository` must become `type: oci`.
  Whether Flux's OCI `HelmRepository` works in our e2e cluster is the one **untested** item.
- **Values churn**, though this direction is a simplification: the five `*.enabled: false` subchart
  toggles are deleted (Forgejo's chart dropped every DB/cache subchart and defaults to SQLite), and
  `fullnameOverride: gitea` keeps Services at `gitea-http` / `gitea-ssh` so no hostname changes.
  The values root is still `gitea:` — the chart is a fork that kept the key — so the entire
  `gitea.config.*` block ports verbatim.

Plus, eventually, ~50 files of `gitea*` → `forgejo*` rename churn to stop the tree lying about what
it talks to.

### Where Gitea is genuinely ahead

Two capabilities exist on Gitea with no equal on Forgejo.

**1. `TRUSTED_SSH_KEYS` is strictly better than Forgejo's equivalent.** Both have a config-level lever
that trusts a known public key without per-user web verification, and both skip identity checks
entirely (`sshsig.Verify(…, "git")` and nothing else):

| | Gitea | Forgejo |
|---|---|---|
| Lever | `[repository.signing] TRUSTED_SSH_KEYS` (a list) | `FORMAT=ssh` + `SIGNING_KEY=<path to .pub>` → `setting.SSHInstanceKey` |
| Where | `services/asymkey/commit.go:396-405` | `models/asymkey/ssh_key_object_verification.go:68-81` |
| Reported `signer.email` | the **commit's own committer email** | the **fixed** configured `SIGNING_EMAIL` |
| Preflight | `GET /api/v1/signing-key.pub` | `GET /api/v1/signing-key.ssh` |

That last row decides it. `assertGiteaVerified` ([`signing_common_test.go:73-96`](../../test/e2e/signing_common_test.go))
asserts `v.Signer.Email == expectedSignerEmail`. On Gitea's path that passes unchanged with an
arbitrary per-test committer; on Forgejo's, the spec would have to pin its committer email to
`SIGNING_EMAIL`, and the assertion stops proving user attribution. Gitea also accepts a *list*, where
Forgejo takes a single key. (The Gitea `app.example.ini` comment claiming this path exposes
`SIGNING_EMAIL` is wrong — the code passes `c.Committer.Email`.)

**2. The SDK option stays open, and only makes sense against Gitea.** `code.gitea.io/sdk/gitea`
(v0.25.1) is actively maintained and tracks Gitea minors. Its version gating — `NewClient` GETs
`/version` and gates features on semver — is *meaningful* against real Gitea and **actively
misleading** against Forgejo, which reports a far higher version so every Gitea feature gate passes
trivially. We are not adopting it (below), but on Gitea it stays a live option; on Forgejo it would
be a liability.

## Decision: do not adopt a Go SDK, on either server

**There is no official Forgejo SDK.** Nothing under the `forgejo` org on `code.forgejo.org`.
`earl-warren/forgejosdk`, from a core Forgejo maintainer, is untouched since 2024-01-01.
`mvdkleijn/forgejo-sdk` is a third-party fork of the Gitea SDK with a single maintainer. Forgejo's own
`go.mod:20` depends on `code.gitea.io/sdk/gitea`; it neither uses nor built a Forgejo SDK.

The Gitea SDK covers all 21 endpoints we call, including the unusual ones
(`POST /users/{login}/tokens`, `GET /user/gpg_key_token`, `POST /admin/users/{u}/keys`,
`POST /admin/users/{owner}/repos`). Coverage is not the problem. Four things are:

- **No SDK can cover `webclient.go`.** SSH signing-key verification is a web form flow, not REST, on
  both servers. Adopting an SDK does not remove hand-rolled HTTP — it *splits* the client into an SDK
  half and a hand-rolled half, with two auth models and two error idioms. The web half is where
  breakage actually happens, so this is the wrong half to modernise.
- **Closed response types are brittle, and Forgejo proves it.**
  `external-sources/forgejo/services/migrations/gitea_sdk_hack.go` uses `go:linkname` to reach the
  SDK's **unexported** `(*Client).getParsedResponse`, purely to add one JSON field the SDK does not
  model. Forgejo could not extend it through supported means.
- **Supply chain, for test-only code.** Five new direct dependencies we do not share, including two
  HTTP-signature libraries and a Windows PuTTY agent shim, for SSH-cert auth we never use — plus an
  `x/crypto` version-floor conflict. Our current client is stdlib-only: zero. Hard to justify on a
  repo running Scorecard, cosign, and govulncheck, for code absent from the shipped binary.
- **Context is client-scoped, not per-call.** Our client threads `ctx` per method. Under parallel
  Ginkgo we would construct a client per context (paying the `/version` round-trip each time) or
  mutate shared state and race.

**Revisit if** we need many more endpoints. The SDK's value rises with breadth of API use, and ours
is narrow and static.

## SSH key verification works on both

The signing spec needs the host to report commits as `Verified`, and that is the one thing driven
through the web UI. Forgejo's `verify_ssh` handler (`routers/web/user/setting/keys.go:207-241`) is
logically identical to Gitea's: same `VerificationToken(doer, 1)` / `lastToken(doer, 0)` pair giving a
two-minute window across the minute boundary, same fallback retry, same flash keys. `AddKeyForm` is
byte-identical between the forks. `GET /user/gpg_key_token` serves the **SSH** token too, despite the
GPG-flavoured name. So this is not a differentiator — it works either way.

**Registering a key is not enough**, on either. `k.Verified` gates the per-user path (Forgejo
`ssh_key_object_verification.go:57-64`, which additionally requires the committer email to be an
*activated* address; Gitea `commit.go:383-390`). The column is written in exactly one place per tree,
reachable only from the web handler. Forgejo's `AddPublicKey` has no `verified` parameter at all,
`POST /admin/users/{u}/keys` creates unverified keys, and no CLI touches it. There is no API
shortcut — it is the web flow, a direct DB write, or a config-level trust lever.

Neither config lever helps the **generated-key** spec ([`signing_e2e_test.go:71`](../../test/e2e/signing_e2e_test.go)),
which mints a key at runtime: both are read at config load and would need a restart mid-suite. So the
web flow stays regardless of server — which is why the CSRF deletion is the work that actually matters.

## Recommendation

**Decouple the two decisions, and do the cheap one first.**

1. **Unpin Gitea now.** Delete the CSRF scraper and move the chart forward off 12.5.x. This is
   required work under *either* outcome, it is a net code deletion, and it removes the only real
   pressure. Also drop the `allowedVersions: <12.6.0` guard in
   [`.github/renovate.json`](../../.github/renovate.json) and the inline pin comment, which exist
   solely to describe a problem that no longer needs a workaround.
2. **Then decide Forgejo on its merits, not under duress.** After step 1 the technical case is close
   to neutral-to-negative: Gitea costs nothing further, keeps the literal signing namespace (avoiding
   the one hazardous change), and has the better trusted-key lever. Forgejo's advantages are a simpler
   chart and project/governance preference — a legitimate reason to move, but not a technical one, and
   it should be stated as such rather than dressed up as a fix.

The honest summary: **the migration was justified by a pin that turns out to be self-inflicted.**
Once the scraper is gone, moving to Forgejo buys a cleaner chart and costs a risky namespace change
plus an untested OCI dependency. If the project preference is strong the migration is entirely
feasible and the plan below holds. If it is not, staying is cheaper and slightly safer.

## Migration plan, if Forgejo is chosen

Estimated **1–2 days**, risk concentrated in step 3.

| # | Step | Size | Risk |
|---|---|---|---|
| 0 | Smoke-test Flux **OCI** `HelmRepository` against `oci://code.forgejo.org/forgejo-helm`. The chart pulls fine with `helm`; Flux OCI support in our cluster is the untested piece. | 30 min | infra unknown — **do first** |
| 1 | `HelmRepository` → `type: oci`; `HelmRelease` → `chart: forgejo`, `version: 17.1.3`. | 30 min | low |
| 2 | Values: delete the five subchart toggles, add `fullnameOverride: gitea`, keep `gitea.config.*` verbatim. | 45 min | low |
| 3 | **SSH signing namespace** → the ROOT_URL hostname. Own commit, own bisect point; land on a scratch cluster first. | half day | **medium — hostile failure mode** |
| 4 | Session cookie constant → `session`. | 10 min | low |
| 5 | Full `task test-e2e`, plus the signing and bi-directional corners. | 1 hour | — |

The CSRF deletion is deliberately **not** in this table — it belongs to step 1 of the recommendation
and ships either way.

### Verify immediately after the first install: SSH reachability

Both Gitea Services today are **headless** (`clusterIP: None`), so DNS returns pod IPs and the
declared Service port is bypassed. Probed from inside the live cluster against `gitea-ssh.gitea-e2e.svc`:

```text
port 22    closed     <- the Service's declared port; nothing listens
port 2222  OPEN       <- the rootless image's real listener
port 13000 OPEN       <- http, on the *ssh* service name, because headless
```

So `repo_setup.go:39`'s `…gitea-ssh…:2222` works by reaching the pod directly, and `SSH_PORT: 22` in
our values is cosmetic — it only sets the advertised clone URL. The apparent contradiction between the
two is not a bug today. But the URL depends on headless behaviour *plus* the rootless listener, not on
a port mapping: if Forgejo's chart Services are not headless, `:2222` breaks. Check this before
debugging anything else.

### Deliberately out of scope

**The `gitea*` → `forgejo*` rename.** `internal/giteaclient`, `GiteaTestInstance`, `waitForGiteaAPI`,
`GITEA_*` Taskfile vars, the `gitea-e2e` namespace, `cmd/gitea-signing-debug`,
`hack/checkout-gitea-repo.sh` — ~50 files of churn with real review and merge-conflict cost. Separate
PR once the functional migration is green. `fullnameOverride: gitea` means nothing functionally
depends on it.

**Crossplane / OpenTofu-managed repositories.** Evaluated and rejected for e2e: each spec creates a
throwaway repo, user, and collaborator inline and uses it milliseconds later (15+ repos per suite
run). A `Workspace` CR reconciled through `tofu plan` is far slower, needs durable remote state that a
recreated k3d cluster destroys, and does not address SSH key *verification* — our actual pain. A
`ForgejoProject` XRD is an interesting **product** direction; it does not belong on the test path.

## Rollback

Every change is confined to `test/e2e/setup/flux/**` and `internal/giteaclient/**`, neither of which
ships. Reverting restores the previous server. No CRD, no API, no user-visible surface is touched.
