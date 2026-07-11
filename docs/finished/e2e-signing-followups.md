# E2E Signing Overview

This document is the short status summary for the e2e signing work.

## What The Signing Suite Proves

The important signing paths are now covered:

- operator-generated SSH signing keys
- BYOK SSH signing keys
- local signature verification with `ssh-keygen -Y verify`
- local Git verification with `git verify-commit`
- Gitea-visible verification after the `verify_ssh` flow

## Why The Suite Checks More Than One Thing

The three verification layers are intentional:

- `ssh-keygen -Y verify` proves the raw SSH signature is structurally valid
- `git verify-commit` proves Git itself accepts the signed commit
- Gitea verification proves the platform-level registration and verification
  flow works for real users

Keeping all three catches different classes of regression.

## Gitea-Specific Lessons Worth Keeping

The long investigation record is no longer useful as a primary document, but a few conclusions are
worth keeping:

- Gitea commit verification depends on the signing key being marked `verified`, not just uploaded
- that verification currently requires the web-only `verify_ssh` flow; there is no equivalent REST
  endpoint for SSH key verification in the Gitea version used by the e2e suite
- the repo `trust_model` affects labeling, not whether a valid verified SSH signature can match
- the error `gpg.error.no_gpg_keys_found` is misleading for SSH signatures; in practice it can mean
  "no verified SSH key matched"

That is why the e2e suite verifies more than local signature validity. It also proves the real
Gitea registration and verification path.

## Useful Implementation Shape

The durable implementation outcome from that work is:

- typed Gitea API helpers live in `internal/giteaclient`
- the UI-only SSH verification flow lives in `internal/giteaclient/webclient.go`
- the e2e suite uses those helpers instead of keeping an open-coded Gitea client in test code

That shape is still the right one unless Gitea gains a real SSH-key verification API.

## Still Open

No signing-specific follow-up from this pass remains open.
