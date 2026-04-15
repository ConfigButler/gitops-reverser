# E2E Gitea Interactions Overview

This document describes the current e2e harness interaction model for Gitea.

It is a current-state overview, not a migration plan.

## The Main Layers

The active Gitea flow now lives in three layers:

1. Shared suite preparation in
   [test/e2e/e2e_suite_test.go](/workspaces/gitops-reverser/test/e2e/e2e_suite_test.go)
2. Per-repo setup orchestration in
   [test/e2e/suite_repo_test.go](/workspaces/gitops-reverser/test/e2e/suite_repo_test.go)
   and [test/e2e/repo_setup.go](/workspaces/gitops-reverser/test/e2e/repo_setup.go)
3. Typed Gitea API and web flows in
   [internal/giteaclient](/workspaces/gitops-reverser/internal/giteaclient)

The old Task and shell repo-bootstrap layer is gone. `SetupRepo(...)` now owns
the whole repo fixture flow directly from Go.

## Current Call Order

### 1. Shared e2e preparation

At suite start,
[ensureE2EPrepared()](/workspaces/gitops-reverser/test/e2e/e2e_suite_test.go:62)
runs `task prepare-e2e`.

That still prepares the shared cluster-side prerequisites:

- cluster readiness
- controller install
- shared services
- port-forwards
- age key material

This step does not create repo fixtures itself.

### 2. Per-file repo setup

Each repo-using e2e file calls
[SetupRepo(...)](/workspaces/gitops-reverser/test/e2e/suite_repo_test.go).

`SetupRepo(...)` now performs repo bootstrap directly in Go:

1. wait for the Gitea API to be reachable through the existing port-forward
2. ensure the shared org exists
3. ensure the org repo exists
4. create a run-scoped HTTP access token for the admin user
5. generate a transport SSH keypair in memory
6. register the transport public key on the admin user
7. best-effort configure the Flux receiver webhook for the repo
8. write the namespace-free Secrets manifest used by the test namespace
9. ensure a local checkout exists
10. ensure the dedicated per-repo Gitea user exists
11. ensure that user is collaborator on the repo

There is no Task handoff and no shell script in the middle of that flow.

## Current Gitea Touchpoints

The live harness now touches Gitea through typed helpers only:

- org creation via `EnsureOrg(...)`
- org repo creation via `EnsureOrgRepo(...)`
- access token creation via `CreateAccessToken(...)`
- transport SSH key registration via `RegisterUserKeyAsAdmin(...)`
- collaborator management via `EnsureCollaborator(...)`
- repo webhook listing, deletion, and creation via `ListRepoHooks(...)`,
  `DeleteRepoHook(...)`, and `CreateGiteaWebhook(...)`
- commit verification via `GetCommitVerification(...)`
- SSH signing verification via `VerifySSHKey(...)`

The unusual `verify_ssh` web-form automation still lives under
[internal/giteaclient/webclient.go](/workspaces/gitops-reverser/internal/giteaclient/webclient.go),
but it is now just one part of the typed client surface rather than a separate
special-case setup path.

## Kubernetes And Local Tool Touchpoints

Repo setup still uses a few non-Gitea interactions from Go:

- `kubectl` lookups for the Flux receiver token and webhook path
- `git clone` and `git remote set-url` for the local checkout
- local `git config` for the checkout identity used by manual fixture changes

That is a much smaller seam than before. The harness no longer bounces through
Task and bash just to turn those values into repo artifacts.

## Persisted Artifacts

The current repo setup keeps only the artifacts the suite actually consumes:

- local checkout:
  [`.stamps/repos/<repo>`](/workspaces/gitops-reverser/.stamps/repos)
  unless `REPOS_DIR` or `CHECKOUT_DIR` overrides it
- namespace-free Secret manifest:
  [`.stamps/e2e-repo-artifacts/`](/workspaces/gitops-reverser/.stamps/e2e-repo-artifacts)

The old stamp files are gone:

- no `active-repo.txt`
- no `checkout-path.txt`
- no `token.txt`
- no SSH key files under per-run stamp directories
- no `repo.ready` or `checkout.ready`

The per-run token, transport private key, public key, and webhook bookkeeping
now stay in memory except for the Secret manifest content that the tests
explicitly apply.

## SSH Transport Notes

The repo bootstrap path still creates an SSH Secret for controller validation
tests, but it no longer writes a `known_hosts` file.

That is intentional. The controller already supports SSH auth without
`known_hosts` by falling back to insecure host-key verification when the field
is absent. The e2e harness now relies on that existing product behavior instead
of manufacturing more intermediate files.

## Why This Shape Is Better

- the repo fixture boundary is now visible in one place
- the e2e harness owns its own state instead of reading it back from stamps
- the Gitea HTTP surface is typed and testable
- only the checkout and the Secret manifest remain as persisted run artifacts
- it is much easier to see which pieces are product behavior versus test glue
