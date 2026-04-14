# E2E Signing Follow-Ups — Implementation Plan

Two independent follow-ups tracked in this document:

1. Per-repo Gitea users, enabling real `verification.verified == true`
   assertions for SSH-signed commits.
2. Fixing `batchTemplate` not being applied to atomic commits.

Each is implementable on its own. Start with (1); (2) is a separate bug.

---

## 1. Per-Repo Gitea Users

### Goal

Every e2e repo owns a dedicated Gitea user. Commits for that repo are
authored by that user, signing keys register under that user, and Gitea
reports SSH-signed commits as `verification.verified == true`.

### Scope

- In scope: Go helpers to create/delete Gitea users and PATs; wiring the
  new user into `SetupRepo`; updating signing scenarios to consume the
  per-repo identity; tightening the Gitea verification assertion.
- Out of scope: rewriting
  [hack/e2e/gitea-run-setup.sh](/workspaces/gitops-reverser/hack/e2e/gitea-run-setup.sh)
  in Go; adding `TRUSTED_SSH_KEYS`; passphrase-protected BYOK.

### Design

#### Go helpers

Add to [test/e2e/gitea_api_test.go](/workspaces/gitops-reverser/test/e2e/gitea_api_test.go)
(or a new `gitea_user_test.go` in the same package):

```go
type giteaTestUser struct {
    Login    string // mirrors repo name
    Email    string // "<login>@configbutler.test"
    Password string // random, in-memory only
    ID       int64
    Token    string // PAT with write:repository, write:user
}

func CreateTestUser(login string) (*giteaTestUser, error)
func DeleteTestUser(login string) error
func RegisterSigningPublicKeyAs(user *giteaTestUser, pubKey, title string) (*giteaPublicKey, error)
```

Implementation:

1. `POST /admin/users` with
   `{username, email, password, must_change_password: false, source_id: 0, login_name: <username>}`.
   Gitea 1.25.x marks the email **verified** when created via this
   endpoint — required for the signing lookup to match committer email
   to user.
2. `POST /users/{username}/tokens` (admin basic auth + `Sudo: <username>`
   header) to mint a PAT scoped to `write:repository,write:user`.
3. `DeleteTestUser` calls `DELETE /admin/users/{username}?purge=true`.
   Treat 404 as success.
4. All failures must include the response body, matching
   [giteaDo](/workspaces/gitops-reverser/test/e2e/gitea_api_test.go) error conventions.

#### Repo ownership

Repos stay under the shared `testorg`. The per-repo user is added as a
**collaborator with write permission** via
`PUT /repos/{owner}/{repo}/collaborators/{username}`. This keeps the
existing org-level webhook wiring and HTTP/SSH secrets untouched.

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

1. Call `CreateTestUser(repoName)` immediately after the repo is created.
2. Add the user as a collaborator on `testorg/<repoName>`.
3. Populate `artifacts.User`.
4. `DeferCleanup(func() { _ = DeleteTestUser(repoName) })` at file scope.

No `It` block calls `CreateTestUser` or `DeleteTestUser` directly.

#### Test changes

In [test/e2e/signing_e2e_test.go](/workspaces/gitops-reverser/test/e2e/signing_e2e_test.go):

- Replace the `signingCommitterName` / `signingCommitterEmail` constants
  with values read from `signingRepo.User` at `It` execution time.
- The GitProvider committer fields use those values.
- Replace `RegisterSigningPublicKey(...)` with
  `RegisterSigningPublicKeyAs(signingRepo.User, ...)`.

In [test/e2e/signing_common_test.go](/workspaces/gitops-reverser/test/e2e/signing_common_test.go),
tighten `assertGiteaVerified`:

```go
Expect(v.Verified).To(BeTrue(),
    "Gitea did not report commit as verified.\n  repo=%s/%s\n  commit=%s\n  reason=%q",
    giteaOrg(), repoName, commitHash, v.Reason)
```

Only tighten this **after** per-repo users are wired in and both signing
scenarios pass green locally.

#### Helpers to delete after step 4

Once no caller remains, remove from
[gitea_api_test.go](/workspaces/gitops-reverser/test/e2e/gitea_api_test.go):

- `EnsureAdminUserPrimaryEmail`
- `EnsureUserEmail`
- `removeUserEmail`

And remove the `configure_ssh_key_in_gitea()` block from
[hack/e2e/gitea-run-setup.sh](/workspaces/gitops-reverser/hack/e2e/gitea-run-setup.sh).

### Implementation order (one commit per step)

1. Add `CreateTestUser`, `DeleteTestUser`, `RegisterSigningPublicKeyAs`.
   No callers yet. `task test` must pass.
2. Wire `SetupRepo` to create and clean up the per-repo user; expose on
   `RepoArtifacts.User`. No test consumes `User` yet. Signing tests
   still pass using the old shared-admin path.
3. Switch both signing scenarios to consume `signingRepo.User`.
   `task test-e2e-signing` still passes with the current (lenient)
   `assertGiteaVerified`.
4. Tighten `assertGiteaVerified` to require `Verified == true`.
   `task test-e2e-signing` must now pass with the strict assertion.
5. Delete the three admin-email helpers and the
   `configure_ssh_key_in_gitea()` block. Verify
   `task test-e2e-signing` and `task test-e2e` stay green.

### Validation

After every step:

```bash
task fmt
task lint
task test
task test-e2e-signing
```

After step 5, also run:

```bash
task test-e2e
```

### Acceptance

- Both signing scenarios in
  [signing_e2e_test.go](/workspaces/gitops-reverser/test/e2e/signing_e2e_test.go) produce commits
  Gitea reports as `verification.verified == true`.
- No test reads or mutates the admin user's emails or SSH keys.
- Each repo owns exactly one Gitea user, created in `SetupRepo` and
  cleaned up via `DeferCleanup` even on failure.
- The three admin-email helpers and `configure_ssh_key_in_gitea()` are
  deleted.

### Constraints

- Do not tighten `assertGiteaVerified` before step 4.
- Do not add retries around the verification assertion; Gitea computes
  it synchronously.
- Do not bundle this with the `gitea-run-setup.sh` Go port — that is a
  separate effort tracked in
  [gitea-setup-go-migration-analysis.md](./gitea-setup-go-migration-analysis.md).

---

## 2. `batchTemplate` Not Applied To Atomic Commits

### Symptom

The scenario
`Commit Signing should produce a batch commit with the custom batch message template`
in [signing_e2e_test.go](/workspaces/gitops-reverser/test/e2e/signing_e2e_test.go) times out
waiting for a commit message containing `e2e-batch:`. The batch commit
**is** produced, but with the default template:

```
reconcile: sync N resources
```

from [internal/git/types.go:40](/workspaces/gitops-reverser/internal/git/types.go#L40).

### Root cause to investigate

Atomic commit path:

1. [internal/reconcile/git_target_event_stream.go:133](/workspaces/gitops-reverser/internal/reconcile/git_target_event_stream.go#L133)
   `EmitReconcileBatch` tags the request `CommitMode = CommitModeAtomic`
   and enqueues.
2. [internal/git/branch_worker.go:464](/workspaces/gitops-reverser/internal/git/branch_worker.go#L464)
   picks up atomic items.
3. [internal/git/git.go:818](/workspaces/gitops-reverser/internal/git/git.go#L818)
   calls `renderBatchCommitMessage(request, commitConfig)`.
4. [internal/git/commit.go:77](/workspaces/gitops-reverser/internal/git/commit.go#L77)
   reads `config.Message.BatchTemplate`.

`commitConfig` is produced by `resolveWriteRequestCommitConfig(request)`.
The spec's `batchTemplate` is not reaching that config at runtime —
either the reconciler doesn't copy the field into the `WriteRequest`, or
the worker drops it during atomic-item preparation.

### Debugging recipe

1. Log `commitConfig.Message.BatchTemplate` immediately before
   `renderBatchCommitMessage` in `generateAtomicCommit`.
2. Log the same value at the point `EmitReconcileBatch` constructs the
   `ReconcileBatch` / `WriteRequest`.
3. If empty at (2): fix the reconciler to read
   `GitProvider.spec.commit.message.batchTemplate` into the request.
4. If set at (2) but empty at (1): fix the worker's atomic-item prep
   (around [branch_worker.go:847](/workspaces/gitops-reverser/internal/git/branch_worker.go#L847))
   to preserve the template.

### Acceptance

- The existing scenario passes without test-side changes.
- Add one unit-level test covering spec → resolved-config for atomic
  requests, next to the existing cases in
  [internal/git/commit_test.go](/workspaces/gitops-reverser/internal/git/commit_test.go).
