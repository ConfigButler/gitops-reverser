# E2E Signing Follow-Ups — Working Plan

Two independent follow-ups are tracked here:

1. Per-repo Gitea users for SSH-signing verification.
2. Investigating why the signing e2e batch-template scenario does not
   observe the custom atomic batch message.

Start with (1). Keep (2) separate so the signing-user work can begin
without waiting on the atomic batch investigation.

---

## Decisions For The Next Pass

These are the working decisions for the next implementation context:

- Do **not** delete Gitea users or Gitea repos in this phase. Leaving
  them in place for inspection is acceptable.
- `CreateTestUser(repoName)` must be **idempotent**. Re-running the same
  repo name must succeed and return a usable user.
- `RegisterSigningPublicKeyAs(...)` does **not** need key cleanup in
  this phase. We can add cleanup later if it becomes necessary.
- Keep the existing transport-SSH setup in
  [hack/e2e/gitea-run-setup.sh](/workspaces/gitops-reverser/hack/e2e/gitea-run-setup.sh)
  unchanged for now. The existing SSH-auth e2e flow still depends on it.
- Do not tighten `assertGiteaVerified` until the per-repo-user wiring is
  in place and the signing scenarios pass locally with the new identity.

---

## 1. Per-Repo Gitea Users

### Goal

Each e2e repo gets a dedicated Gitea user. Signing scenarios author
commits using that user's verified email, register signing keys under
that user, and then confirm whether Gitea reports
`verification.verified == true`.

### Scope

- In scope: idempotent Go helpers to create or reuse Gitea users;
  wiring the user into `SetupRepo`; adding the user as a repo
  collaborator; updating signing scenarios to use that identity;
  tightening the Gitea verification assertion once the flow is green.
- Out of scope: deleting users/repos/keys; rewriting
  `gitea-run-setup.sh` in Go; replacing the transport SSH secret flow;
  adding `TRUSTED_SSH_KEYS`; passphrase-protected BYOK.

### Design

#### Helper surface

Add to
[test/e2e/gitea_api_test.go](/workspaces/gitops-reverser/test/e2e/gitea_api_test.go)
or a sibling helper file in the same package:

```go
type giteaTestUser struct {
    Login    string // usually mirrors repo name
    Email    string // "<login>@configbutler.test"
    Password string // random, in-memory only
    ID       int64
    Token    string // optional; keep if needed by RegisterSigningPublicKeyAs
}

func CreateTestUser(login string) (*giteaTestUser, error)
func EnsureRepoCollaborator(owner, repo string, user *giteaTestUser) error
func RegisterSigningPublicKeyAs(user *giteaTestUser, pubKey, title string) (*giteaPublicKey, error)
```

`DeleteTestUser` is intentionally **not** part of the first pass.

#### CreateTestUser behavior

`CreateTestUser(login)` must be idempotent:

1. Normalize `login` and derive email as `<login>@configbutler.test`.
2. Check whether the user already exists.
3. If it does not exist, create it via `POST /admin/users` using:
   `{username, email, password, must_change_password: false, source_id: 0, login_name: <username>}`.
4. If it already exists, treat that as success and return the existing
   user details.
5. If the signing-key helper needs user-scoped auth, mint a PAT for that
   user. Leaving the token in Gitea is acceptable in this phase.
6. All failures must include the response body, matching the existing
   `giteaDo(...)` error style.

Assume this helper may be called for:

- random repo names used by most e2e flows
- fixed names such as `demo`
- reruns after an interrupted test run

#### Repo ownership

Repos stay under the shared `testorg`. The per-repo user is added as a
collaborator with write permission via:

- `PUT /repos/{owner}/{repo}/collaborators/{username}`

That keeps the current org-level webhook wiring and repo bootstrap
shape intact.

#### SetupRepo integration

Extend `RepoArtifacts` in
[test/e2e/suite_repo_test.go](/workspaces/gitops-reverser/test/e2e/suite_repo_test.go):

```go
type RepoArtifacts struct {
    // ... existing fields
    User *giteaTestUser
}
```

Inside `SetupRepo(ctx, namespace, repoName)`:

1. Keep the existing `task e2e-gitea-run-setup` call.
2. Read the repo artifacts as today.
3. Call `CreateTestUser(repoName)`.
4. Call `EnsureRepoCollaborator(giteaOrg(), artifacts.RepoName, user)`.
5. Populate `artifacts.User`.

Do **not** register cleanup from `SetupRepo`. The user and repo are
intentionally left behind for inspection, and `SetupRepo` is used by
shared-fixture flows where automatic cleanup would be awkward.

#### Test changes

In
[test/e2e/signing_e2e_test.go](/workspaces/gitops-reverser/test/e2e/signing_e2e_test.go):

- For the generated-key and BYOK signing scenarios, replace the shared
  signing committer constants with values derived from
  `signingRepo.User`.
- Use those values in the `GitProvider` commit config.
- Replace `RegisterSigningPublicKey(...)` with
  `RegisterSigningPublicKeyAs(signingRepo.User, ...)`.
- Do **not** add key cleanup in the `It` blocks for this phase.
- Remove the `EnsureAdminUserPrimaryEmail(...)` binding once the
  per-repo-user path is wired.

Keep the custom-committer scenario focused on what it already tests:

- it should continue to exercise explicit committer override behavior
- it does not need to participate in the Gitea-user verification change

In
[test/e2e/signing_common_test.go](/workspaces/gitops-reverser/test/e2e/signing_common_test.go),
tighten `assertGiteaVerified` only after the new flow is proven:

```go
Expect(v.Verified).To(BeTrue(),
    "Gitea did not report commit as verified.\n  repo=%s/%s\n  commit=%s\n  reason=%q",
    giteaOrg(), repoName, commitHash, v.Reason)
```

#### Keep these pieces for now

Do **not** remove these in the first pass:

- `configure_ssh_key_in_gitea()` in
  [hack/e2e/gitea-run-setup.sh](/workspaces/gitops-reverser/hack/e2e/gitea-run-setup.sh)
- the existing SSH transport secret generation path
- the legacy admin-email helpers until they truly have no callers

The signing-user work should be additive first, cleanup second.

### Implementation order

1. Add `giteaTestUser`, idempotent `CreateTestUser`,
   `EnsureRepoCollaborator`, and `RegisterSigningPublicKeyAs`.
   No deletion helpers yet. `task test` must pass.
2. Extend `RepoArtifacts` and wire `SetupRepo` to create or reuse the
   per-repo user and add the collaborator. No cleanup. `task test` must
   pass.
3. Switch the generated-key and BYOK signing scenarios to consume
   `signingRepo.User`, and remove the admin-email binding from the
   signing `BeforeAll`. Keep `assertGiteaVerified` lenient for this
   step. `task test-e2e-signing` must pass.
4. After both signing scenarios show Gitea verification working with the
   per-repo identity, tighten `assertGiteaVerified` to require
   `Verified == true`. `task test-e2e-signing` must pass.
5. Optional cleanup only after the strict assertion is stable:
   delete dead admin-email helpers if unused. Do not remove the script's
   transport-SSH behavior yet.

### Validation

During implementation:

```bash
task fmt
task lint
task test
task test-e2e-signing
```

Before broad e2e wrap-up:

```bash
docker info
task test-e2e
task test-e2e-quickstart-manifest
task test-e2e-quickstart-helm
```

### Acceptance

- The generated-key and BYOK signing scenarios use a per-repo Gitea
  user instead of mutating the admin user's email.
- `CreateTestUser(repoName)` is idempotent and safe for reruns.
- The repo user is exposed on `RepoArtifacts.User`.
- Gitea users, repos, and registered signing keys are intentionally left
  in place for inspection in this phase.
- The existing SSH-auth repo setup path remains unchanged.
- After step 4, both signing scenarios produce commits that Gitea
  reports as `verification.verified == true`.

### Constraints

- No user, repo, or signing-key deletion in this phase.
- No key cleanup in the signing `It` blocks.
- Do not remove `configure_ssh_key_in_gitea()` yet.
- Do not tighten `assertGiteaVerified` before step 4.
- Do not bundle this with the `gitea-run-setup.sh` Go port.

---

## 2. `batchTemplate` Not Applied To Atomic Commits

This remains a separate follow-up and should not block the per-repo-user
work.

### Symptom

The scenario
`Commit Signing should produce a batch commit with the custom batch message template`
in
[test/e2e/signing_e2e_test.go](/workspaces/gitops-reverser/test/e2e/signing_e2e_test.go)
times out waiting for a commit subject containing `e2e-batch:`.

### Current assessment

Do **not** assume yet that the product path is broken.

Today the code already:

- resolves `BatchTemplate` from `GitProvider.spec.commit.message` in
  [internal/git/types.go](/workspaces/gitops-reverser/internal/git/types.go)
- copies resolved commit config onto the prepared write request in
  [internal/git/branch_worker.go](/workspaces/gitops-reverser/internal/git/branch_worker.go)
- uses that config when generating atomic commits in
  [internal/git/git.go](/workspaces/gitops-reverser/internal/git/git.go)
- has unit coverage for atomic batch-template usage in
  [internal/git/branch_worker_test.go](/workspaces/gitops-reverser/internal/git/branch_worker_test.go)

So the first task is to verify whether the failure is:

- a real product bug
- an e2e setup or timing issue
- the test reading the wrong commit/path/history

### Investigation order

1. Re-run only the signing batch scenario and capture the actual latest
   commit subject for the target path.
2. Confirm the scenario is producing an atomic commit, not a per-event
   commit series.
3. If the remote commit subject still uses the default template, log the
   resolved batch template in:
   - `prepareWriteRequest(...)`
   - `generateAtomicCommit(...)`
4. Only if the template is empty in the runtime path should code changes
   be made.

### Acceptance

- The existing batch-template signing scenario passes without test-side
  workaround logic.
- If a real code bug is found, add a focused unit test for the missing
  runtime path in addition to the existing atomic-template coverage.
