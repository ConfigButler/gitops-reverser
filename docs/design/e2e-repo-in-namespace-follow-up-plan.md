# E2E Repo-Per-File Overview

This document is the short current-state summary of the repo-per-file refactor.

## What We Have Now

The e2e harness is intentionally split into two layers:

- `BeforeSuite` prepares shared cluster and install state once
- each repo-using e2e test file creates its own repo fixtures through
  `SetupRepo(...)`

That means mutable Git state is file-local, while expensive cluster setup stays
shared.

## Why It Looks Like This

This shape was chosen to fix the earlier package-global repo coupling without
rewriting the whole harness:

- one file should not be able to corrupt another file's checkout or repo state
- `BeforeSuite` should stay the single owner of heavy install/bootstrap work
- repo fixtures should still be easy to inspect on disk through `.stamps`
- the suite should keep working through normal Go and Ginkgo entry points

## Important Current Decisions

- one repo per repo-using e2e file
- repo artifacts flow through `RepoArtifacts`, not package-global env vars
- repo stamps live under the file's test namespace
- `audit_redis_e2e_test.go` uses file-local lazy repo initialization so either
  top-level `Describe` can run first
- signing coverage now includes generated keys, BYOK, local verification, and
  Gitea-visible verification

## Status

The repo-per-file follow-up work that used to be tracked here is complete.

What remains open for the broader harness is tracked in:

- [e2e-finish-plan.md](./e2e-finish-plan.md)

For the original design intent, see:

- [e2e-test-design.md](./e2e-test-design.md)
- [e2e-repo-in-namespace-plan.md](./e2e-repo-in-namespace-plan.md)

For the signing investigation record, see:

- [gitea-ssh-signing-verification-plan.md](./gitea-ssh-signing-verification-plan.md)
