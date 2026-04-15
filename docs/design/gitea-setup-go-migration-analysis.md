# Gitea E2E Setup Migration Overview

This document captures the current migration stance for the Gitea e2e setup
path.

## Current Split

Today the harness is intentionally mixed:

- Go owns orchestration, assertions, signing verification, and most typed Gitea
  interactions
- Task owns environment wiring and dependency ordering
- shell still owns part of the repo bootstrap path in
  [hack/e2e/gitea-run-setup.sh](/workspaces/gitops-reverser/hack/e2e/gitea-run-setup.sh)

## Why Shell Still Exists

Not every remaining step has the same value-to-effort ratio.

The Gitea HTTP surface is the part that benefits most from moving to Go:

- typed payloads
- better errors
- less `curl`/`jq` drift
- easier future parallelization

The shell-native pieces are lower priority:

- `known_hosts` generation
- `secrets.yaml` rendering
- checkout/bootstrap plumbing around local tools

Those pieces can stay in shell for now without blocking the larger harness
direction.

## Recommended Migration Boundary

The next worthwhile seam is narrow:

- move token creation
- move repo ensure/create
- move SSH auth-key registration
- move repo webhook management

Keep the Task entry point working, ideally through a thin Go CLI wrapper.

## Status

This migration is still open and is now tracked in:

- [e2e-finish-plan.md](./e2e-finish-plan.md)

The signing-specific Gitea client work that already landed is documented in:

- [gitea-ssh-signing-verification-plan.md](./gitea-ssh-signing-verification-plan.md)
