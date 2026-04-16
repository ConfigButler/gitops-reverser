# Commit Signing Guide

This guide covers the part that is easy to get wrong: a commit can be validly signed in Git and
still fail to show a green "verified" badge on the Git hosting platform.

## Two separate checks

There are usually two layers involved:

- Git-level signature validation: the commit object contains a valid signature and Git can verify it
- platform-level trust/display: the Git host decides whether to show that commit as verified for a
  specific account

Those are related, but they are not the same thing.

In practice, a commit can pass local checks like `git verify-commit` and still appear unverified on
GitHub, GitLab, Gitea, or another host if the platform-specific identity mapping does not line up.

## What GitOps Reverser controls

GitOps Reverser controls:

- whether the commit is signed at all
- which signing key is used
- which committer name and email are written into the commit
- which public key it exposes in `.status.signingPublicKey`

That is enough to produce a valid signed commit.

## What the Git host controls

The Git hosting platform controls:

- whether the uploaded public key is treated as a signing key
- which user or bot account owns that key
- whether the committer email matches an accepted identity on that account
- whether the UI shows the commit as verified, unverified, or unknown

That means "the operator signed the commit correctly" and "the platform showed a green badge" are
separate outcomes.

## Why committer identity matters

Platforms generally tie the signature to the **committer** identity, not the Kubernetes audit
author.

That is why `spec.commit.committer.email` matters so much. If the signature is valid but the
committer email does not belong to the account that owns the signing key, the platform may still
show the commit as unverified.

The safe mental model is:

- Kubernetes audit author explains who changed the cluster
- Git committer explains which bot/account wrote the Git commit
- signing verification is usually about that committer account

## SSH signing specifics

With SSH signing, the most common failure mode is not broken cryptography. It is broken identity
matching.

Typical requirements are:

- the public key must be registered as a signing key, not only as an authentication key
- the key must belong to the intended bot or user account
- the committer email must be accepted by that same account

GitOps Reverser can help by publishing `.status.signingPublicKey`, but the final trust decision
still belongs to the platform.

## Local verification vs hosted verification

When troubleshooting, think in this order:

1. Can Git verify the commit locally?
2. Does the registered public key match `.status.signingPublicKey`?
3. Is that key registered as a signing key on the correct account?
4. Does `spec.commit.committer.email` belong to that same account?
5. Does the platform have any extra verification or trust step for that key type?

That sequence helps separate "signing is broken" from "platform trust is not configured yet".

## Gitea note

In the Gitea versions covered by this repository's e2e tests, SSH signing has an extra wrinkle:

- the key must be marked `verified`, not merely uploaded
- that verification currently depends on the `verify_ssh` web flow

So for Gitea, local `git verify-commit` success is necessary but not sufficient for the platform to
show the commit as verified.

## Recommended operator workflow

Use this flow when enabling SSH commit signing:

1. Configure `GitProvider.spec.commit.signing`
2. Wait for `.status.signingPublicKey`
3. Register that public key on the Git host as a signing key
4. Make sure `spec.commit.committer.email` belongs to the same account
5. Trigger a test commit
6. Check both local validity and platform verification

The `spec.commit` field-level configuration lives in [configuration.md](configuration.md).
