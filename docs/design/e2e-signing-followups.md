# E2E Signing Overview

This document is the short status summary for the e2e signing work.

## What The Signing Suite Proves

The important signing paths are now covered:

- operator-generated SSH signing keys
- BYOK SSH signing keys
- local signature verification with `ssh-keygen -Y verify`
- local Git verification with `git verify-commit`
- Gitea-visible verification after the `verify_ssh` flow

The Gitea-specific investigation and final implementation details live in:

- [gitea-ssh-signing-verification-plan.md](./gitea-ssh-signing-verification-plan.md)

## Why The Suite Checks More Than One Thing

The three verification layers are intentional:

- `ssh-keygen -Y verify` proves the raw SSH signature is structurally valid
- `git verify-commit` proves Git itself accepts the signed commit
- Gitea verification proves the platform-level registration and verification
  flow works for real users

Keeping all three catches different classes of regression.

## Still Open

No signing-specific follow-up from this pass remains open.
