# E2E Gitea Interactions Overview

This document describes how the e2e harness currently interacts with Gitea:

- where the touchpoints live
- what is called and in which order
- what gets written under `.stamps`
- how local checkouts and credentials are modeled today

It is a current-state overview, not a migration plan.

## The Main Layers

The current Gitea flow is split across four layers:

1. Shared suite preparation in
   [test/e2e/e2e_suite_test.go](/workspaces/gitops-reverser/test/e2e/e2e_suite_test.go)
2. Per-repo setup orchestration in
   [test/e2e/suite_repo_test.go](/workspaces/gitops-reverser/test/e2e/suite_repo_test.go)
3. Task and shell setup in
   [test/e2e/Taskfile.yml](/workspaces/gitops-reverser/test/e2e/Taskfile.yml),
   [hack/e2e/gitea-bootstrap.sh](/workspaces/gitops-reverser/hack/e2e/gitea-bootstrap.sh),
   and [hack/e2e/gitea-run-setup.sh](/workspaces/gitops-reverser/hack/e2e/gitea-run-setup.sh)
4. Typed Gitea API and web flows in
   [internal/giteaclient](/workspaces/gitops-reverser/internal/giteaclient)

That split is why the harness feels partly modernized and partly still shell-driven:
the signing and higher-level test flows already use Go helpers, while the repo
bootstrap path still leans on Task and bash.

## Current Call Order

### 1. Shared e2e preparation

At suite start,
[ensureE2EPrepared()](/workspaces/gitops-reverser/test/e2e/e2e_suite_test.go:62)
runs `task prepare-e2e`.

That prepares the shared cluster-side prerequisites:

- cluster readiness
- controller install
- shared services
- port-forwards
- age key material

This step does not create a repo checkout itself.

### 2. Per-file repo setup

Each repo-using e2e file calls
[SetupRepo(...)](/workspaces/gitops-reverser/test/e2e/suite_repo_test.go:60)
from its own `BeforeAll`.

`SetupRepo(...)` then:

1. runs `task e2e-gitea-run-setup`
2. reads the generated stamp files back into a `RepoArtifacts`
3. ensures a dedicated per-repo Gitea user exists
4. adds that user as collaborator on the shared org repo

So the first half of repo setup is still Task/shell-driven, and the second half
already uses typed Go helpers.

### 3. Shared Gitea bootstrap

`task e2e-gitea-run-setup` first calls
`task e2e-gitea-bootstrap-shared`, which depends on:

- `portforward-ensure`
- `_gitea-bootstrap`

The `_gitea-bootstrap` task runs
[hack/e2e/gitea-bootstrap.sh](/workspaces/gitops-reverser/hack/e2e/gitea-bootstrap.sh),
which does two things:

1. waits for the Gitea API to become reachable
2. ensures the shared organization exists, usually `testorg`

This is the cluster-scoped bootstrap for the shared Gitea instance.

### 4. Per-repo shell setup

After shared bootstrap, `_gitea-run-setup` runs
[hack/e2e/gitea-run-setup.sh](/workspaces/gitops-reverser/hack/e2e/gitea-run-setup.sh).

Its current order is:

1. `wait_for_api`
2. `create_token`
3. `ensure_ssh_keys`
4. `generate_known_hosts`
5. `configure_ssh_key_in_gitea`
6. `ensure_repo`
7. `write_secrets_manifest`
8. `ensure_repo_webhook`
9. `ensure_checkout`

This is the main current repo bootstrap pipeline.

### 5. Typed Gitea interactions after bootstrap

Once the repo exists and the stamps are written, the Go side keeps interacting
with Gitea through [test/e2e/gitea_api_test.go](/workspaces/gitops-reverser/test/e2e/gitea_api_test.go)
and [internal/giteaclient](/workspaces/gitops-reverser/internal/giteaclient).

The important current helpers are:

- `EnsureUser(...)`
- `EnsureCollaborator(...)`
- `RegisterUserKeyAsAdmin(...)`
- `GetCommitVerification(...)`
- `VerifySSHKey(...)`

The unusual one is `VerifySSHKey(...)`:

- it fetches Gitea's verification token
- signs it in process
- logs into the web UI
- submits the `verify_ssh` form

That is how the signing tests make Gitea report SSH-signed commits as verified.

## Where We Currently Touch Gitea

The current touchpoints are:

- org bootstrap in
  [hack/e2e/gitea-bootstrap.sh](/workspaces/gitops-reverser/hack/e2e/gitea-bootstrap.sh)
- repo/bootstrap token/key/webhook setup in
  [hack/e2e/gitea-run-setup.sh](/workspaces/gitops-reverser/hack/e2e/gitea-run-setup.sh)
- repo user and collaborator management in
  [test/e2e/gitea_api_test.go](/workspaces/gitops-reverser/test/e2e/gitea_api_test.go)
- signing-key registration and commit verification through
  [internal/giteaclient](/workspaces/gitops-reverser/internal/giteaclient)
- `verify_ssh` web-form automation in
  [internal/giteaclient/webclient.go](/workspaces/gitops-reverser/internal/giteaclient/webclient.go)
- debugging and manual flow reproduction in
  [cmd/gitea-signing-debug/main.go](/workspaces/gitops-reverser/cmd/gitea-signing-debug/main.go)

## Canonical CLI Commands Still Used

The shell scripts still encode the most direct "manual operator" view of the
Gitea setup flow. That is useful, but these commands should live here as the
single reference instead of being repeated across multiple docs.

### Gitea API reachability

Used in both bootstrap scripts:

```bash
curl -fsS "${API_URL}/version"
```

Keep this one. It is the simplest useful "is Gitea reachable through the
port-forward?" check.

### Ensure the shared org exists

Cluster-scoped bootstrap currently uses:

```bash
curl -sS -o "${tmp}" -w "%{http_code}" \
  -X POST "${API_URL}/orgs" \
  -H "Content-Type: application/json" \
  -u "${GITEA_ADMIN_USER}:${GITEA_ADMIN_PASS}" \
  -d "{\"username\":\"${ORG_NAME}\",\"full_name\":\"Test Organization\",\"description\":\"E2E Test Organization\"}"
```

Keep the endpoint and payload shape as the canonical org-bootstrap reference.
Do not keep re-documenting its error-handling wrapper everywhere else.

### Create a run-scoped access token

Per-repo setup currently uses:

```bash
curl -sS -o "${tmp}" -w "%{http_code}" \
  -X POST "${API_URL}/users/${GITEA_ADMIN_USER}/tokens" \
  -H "Content-Type: application/json" \
  -u "${GITEA_ADMIN_USER}:${GITEA_ADMIN_PASS}" \
  -d "{\"name\":\"${token_name}\",\"scopes\":[\"write:repository\",\"read:repository\",\"write:organization\",\"read:organization\"]}"
```

Then it extracts the token with:

```bash
jq -r '.sha1 // ""' "${tmp}"
```

Keep the endpoint and the expected `.sha1` response shape as the important
reference. The exact shell temp-file dance should not be copied into new places.

### Register the transport SSH key on the admin user

Current script flow:

```bash
curl -fsS -X GET "${API_URL}/user/keys" \
  -H "Content-Type: application/json" \
  -u "${GITEA_ADMIN_USER}:${GITEA_ADMIN_PASS}"
```

```bash
curl -fsS -X DELETE "${API_URL}/user/keys/${key_id}" \
  -H "Content-Type: application/json" \
  -u "${GITEA_ADMIN_USER}:${GITEA_ADMIN_PASS}"
```

```bash
curl -sS -o "${tmp}" -w "%{http_code}" \
  -X POST "${API_URL}/user/keys" \
  -H "Content-Type: application/json" \
  -u "${GITEA_ADMIN_USER}:${GITEA_ADMIN_PASS}" \
  -d "{\"title\":\"E2E Test Key\",\"key\":\"${pub_key_content}\"}"
```

Keep these because they explain the current transport-auth model clearly. Also
keep the warning in mind: this reset-all-keys approach is exactly the behavior
we want to retire from shell first.

### Ensure the repo exists

Current org-repo creation call:

```bash
curl -sS -o "${tmp}" -w "%{http_code}" \
  -X POST "${API_URL}/orgs/${ORG_NAME}/repos" \
  -H "Content-Type: application/json" \
  -u "${GITEA_ADMIN_USER}:${GITEA_ADMIN_PASS}" \
  -d "{\"name\":\"${REPO_NAME}\",\"description\":\"E2E Test Repository\",\"private\":false,\"auto_init\":false}"
```

Keep this one. It captures an important current behavior: repos are created as
public, which is why the default local checkout can pull without embedded
credentials.

### Discover receiver token and webhook path

Current Kubernetes lookups:

```bash
kubectl --context "${CTX}" -n "${FLUX_RECEIVER_NAMESPACE}" get secret "${FLUX_RECEIVER_SECRET_NAME}" \
  -o jsonpath='{.data.token}' | base64 -d
```

```bash
kubectl --context "${CTX}" -n "${FLUX_RECEIVER_NAMESPACE}" get receiver "${FLUX_RECEIVER_NAME}" \
  -o jsonpath='{.status.webhookPath}'
```

Keep these as the canonical reference for how the Gitea repo webhook is wired
to Flux today.

### List, delete, and recreate Gitea repo webhooks

Current hook-management calls:

```bash
curl -fsS -X GET "${API_URL}/repos/${ORG_NAME}/${REPO_NAME}/hooks" \
  -H "Content-Type: application/json" \
  -u "${GITEA_ADMIN_USER}:${GITEA_ADMIN_PASS}"
```

```bash
curl -fsS -X DELETE "${API_URL}/repos/${ORG_NAME}/${REPO_NAME}/hooks/${hook_id}" \
  -H "Content-Type: application/json" \
  -u "${GITEA_ADMIN_USER}:${GITEA_ADMIN_PASS}"
```

```bash
curl -sS -o "${tmp}" -w "%{http_code}" \
  -X POST "${API_URL}/repos/${ORG_NAME}/${REPO_NAME}/hooks" \
  -H "Content-Type: application/json" \
  -u "${GITEA_ADMIN_USER}:${GITEA_ADMIN_PASS}" \
  -d "${payload}"
```

Keep the endpoint set and payload intent. Do not keep duplicating the current
delete-then-create shell logic in new code.

### Generate `known_hosts`

The current shell-native sequence is:

```bash
kubectl --context "${CTX}" -n "${GITEA_NAMESPACE}" port-forward --address 127.0.0.1 \
  "svc/gitea-ssh" "${local_port}:2222"
```

```bash
ssh-keyscan -p "${local_port}" 127.0.0.1
```

Keep this as a debugging recipe. It is useful to know, but it is also a strong
candidate to stop hand-rolling in multiple places once the setup path moves
further into Go.

### Build namespace-free Secret manifests

Current manifest-generation commands:

```bash
kubectl create secret generic "${SECRET_HTTP_NAME}" \
  --from-literal=username="${GITEA_ADMIN_USER}" \
  --from-literal=password="${token}" \
  --dry-run=client -o yaml
```

```bash
kubectl create secret generic "${SECRET_SSH_NAME}" \
  --from-file=ssh-privatekey="${ssh_dir}/id_rsa" \
  "${ssh_args[@]}" \
  --dry-run=client -o yaml
```

```bash
kubectl create secret generic "${SECRET_INVALID_NAME}" \
  --from-literal=username="invaliduser" \
  --from-literal=password="invalidpassword" \
  --dry-run=client -o yaml
```

Keep these as the canonical description of the current Secret contract. Even if
we later render the YAML in Go, this is still the current runtime shape we must
preserve.

### Clone and normalize the local checkout

Current checkout commands:

```bash
git clone "http://localhost:13000/${ORG_NAME}/${REPO_NAME}.git" "${checkout_dir}"
```

```bash
git -C "${checkout_dir}" remote set-url origin "${repo_url}"
git -C "${checkout_dir}" config user.name "E2E Test"
git -C "${checkout_dir}" config user.email "e2e-test@gitops-reverser.local"
git -C "${checkout_dir}" config commit.gpgsign false
```

Keep these because they describe the useful checkout behavior we do not want to
lose, even if we later stop producing every intermediate auth file on disk.

## `.stamps`: What We Write Today

Per-repo setup writes artifacts under:

` .stamps/cluster/<ctx>/<namespace>/git-<repo>/ `

The important current files are:

- `active-repo.txt`
- `checkout-path.txt`
- `token.txt`
- `ssh/id_rsa`
- `ssh/id_rsa.pub`
- `ssh/known_hosts`
- `secrets.yaml`
- `repo.ready`
- `checkout.ready`
- optionally `receiver-webhook-url.txt`
- optionally `receiver-webhook-id.txt`

There is also a local checkout under `.stamps/repos/<repo>/` unless
`CHECKOUT_DIR` overrides it.

## Why `.stamps` Still Have Real Value

The stamp tree is not just legacy clutter. It still provides several useful
things:

- inspectability: you can see what repo was created, where the checkout lives,
  and what outputs were generated
- rerun behavior: Task can treat the setup as timestamped/generated work instead
  of starting from zero every time
- boundary clarity: shared cluster bootstrap lives separately from per-repo
  setup outputs
- debugging: when a test fails, the files under `.stamps` make it easier to see
  what the harness believed the world looked like
- portability between layers: the shell step can write artifacts once, and the
  Go step can read them back as structured `RepoArtifacts`

So even if the harness moves toward more in-memory setup, keeping some stable
stamp outputs is still valuable.

## Current Checkout Model

The current checkout is created in
[ensure_checkout()](/workspaces/gitops-reverser/hack/e2e/gitea-run-setup.sh:440).

Today it:

- clones `http://localhost:13000/<org>/<repo>.git`
- reuses the checkout if `.git/` already exists
- updates the `origin` URL when needed
- sets local Git config for:
  - `user.name`
  - `user.email`
  - `commit.gpgsign=false`

Two details matter here:

1. the checkout is a real working repo on disk, which is very useful for test
   readability and debugging
2. the checkout itself does not currently appear to be the main place where
   credentials are stored

That second point is important.

### What Is Actually In `origin` Today

For the standard repo setup path, the `origin` URL is plain:

- `http://localhost:13000/<org>/<repo>.git`

It does **not** embed the admin username or password in the normal checkout
created by `gitea-run-setup.sh`.

That matches the current shell setup, which:

- creates the repo as `private=false`
- clones with a plain HTTP URL
- stores auth separately in generated Secrets and stamp artifacts

So if you remembered "the standard origin URL already contains admin
credentials," that is not true for the normal setup path.

### Why `git pull` Still Works

The setup script currently creates the repo as public by passing
`"private":false` during repo creation.

That means the normal local checkout can pull over plain HTTP without embedded
credentials, which matches what the test helpers do in places like:

- [audit_redis_e2e_test.go](/workspaces/gitops-reverser/test/e2e/audit_redis_e2e_test.go)
- [quickstart_framework_e2e_test.go](/workspaces/gitops-reverser/test/e2e/quickstart_framework_e2e_test.go)
- [demo_e2e_test.go](/workspaces/gitops-reverser/test/e2e/demo_e2e_test.go)

Those pull helpers just run `git pull` in the checkout directory.

### When We *Do* Put Credentials In The Remote URL

There are explicit push-oriented flows where the checkout is rewritten to use an
authenticated URL.

The clearest current example is the bi-directional e2e flow in
[bi_directional_e2e_test.go](/workspaces/gitops-reverser/test/e2e/bi_directional_e2e_test.go):

- it reads the generated HTTP credential Secret
- that Secret currently contains the admin username plus the generated access
  token, not the raw admin account password
- decodes username and password
- builds a URL with `user:password@host`
- runs `git remote set-url origin <authenticated-url>`

That is why the bi-directional suite can do plain `git push` calls from the
working checkout afterward.

The debug tool in
[cmd/gitea-signing-debug/main.go](/workspaces/gitops-reverser/cmd/gitea-signing-debug/main.go)
does the same idea on purpose for its own cloned repo.

## Current Auth Model For Repo Access

The current harness keeps auth mostly outside the local checkout.

The bootstrap step writes a namespace-free
[secrets.yaml](/workspaces/gitops-reverser/hack/e2e/gitea-run-setup.sh:381)
that contains three Secrets:

- `git-creds-<repo>` for HTTP basic auth
- `git-creds-ssh-<repo>` for SSH auth
- `git-creds-invalid-<repo>` for negative-path tests

Those Secrets are the important contract for the controller and test resources.

The current auth ingredients are:

- `token.txt`
  An admin-created access token used to populate the HTTP credentials Secret.
- `ssh/id_rsa` and `ssh/id_rsa.pub`
  The transport SSH keypair used to populate the SSH Secret.
- `ssh/known_hosts`
  Optional host verification data for SSH transport.

The controller then reads those Secrets through
[internal/git/helpers.go](/workspaces/gitops-reverser/internal/git/helpers.go:118),
not from the local checkout.

So the powerful part that should not be lost is not "credentials live inside the
checkout." The powerful part is:

- there is a stable local checkout for humans and tests
- there is a separate stable Secret contract for runtime Git auth
- selected push-oriented flows can opt into an authenticated remote URL when
  they really need direct push from the checkout
- all of that is generated from one setup flow

That separation is good and worth preserving even if the intermediate files move
to memory later.

## Current Signing Flow Versus Transport Flow

It helps to keep these separate:

- transport auth:
  - `token.txt`
  - `id_rsa`
  - `id_rsa.pub`
  - `known_hosts`
  - generated auth Secrets
- signing:
  - signing Secret data like `signing.key` and `signing.pub`
  - in-process SSHSIG generation
  - Gitea `verify_ssh` flow through `internal/giteaclient`

The signing path is already much more Go-native and memory-friendly than the
transport bootstrap path.

## Why This Matters For The Next Refactor

The current setup gives us a good migration boundary:

- keep the checkout as a real on-disk artifact
- keep the Secret contract as a first-class output
- move more of the Gitea HTTP and SSH-material handling into Go
- decide later which intermediate files still deserve to exist under `.stamps`

That way the harness can simplify without losing the things that are genuinely
useful today: debuggable checkouts, stable Secrets, and readable run artifacts.
