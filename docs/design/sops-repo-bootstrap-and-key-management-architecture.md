# SOPS Repo Bootstrap and Key Management Architecture

## Scope for This Increment

This document is intentionally narrow. It defines only what we will implement now.

## Decisions

- Do not auto-generate key material.
- Do not support a generic controller-level `.sops.yaml` override.
- Ensure root `.sops.yaml` only from the write path, and only for branches we write to.
- For now, only ensure presence of `.sops.yaml`; do not validate content equality.
- "Matches Secret semantics" is defined by `isSecretResource` in `internal/git/content_writer.go`: move this Secret-semantics function to `internal/types/identifier.go` so it can be reused consistently.
- Align naming with Flux-style shape, but mirrored for reverse-gitops encryption:
  - use `spec.encryption.provider`
  - use `spec.encryption.secretRef.name`
  - do not overload existing Git auth `spec.secretRef`
- Do not include `spec.encryption.serviceAccountName` in this increment.

## Behavior Contract

- On any write flow to a branch:
  - If root `.sops.yaml` is missing on that branch, write it as part of the same commit flow.
  - If root `.sops.yaml` exists, leave it unchanged.
- Secret detection for encryption-related behavior uses the same Secret-semantics function everywhere.
- `GitProvider.spec.secretRef` continues to mean Git authentication only.
- Encryption configuration is read from `GitProvider.spec.encryption`.
- Supported provider value in this increment: `sops`.
- Encryption key/credential material is sourced from `spec.encryption.secretRef.name`.
- If encryption reference is absent or invalid, Secret writes fail safely (no plaintext fallback).

## GitProvider API Direction

- Keep existing:
  - `spec.secretRef`: Git credentials for clone/fetch/push.
- Add Flux-aligned mirrored encryption section:
  - `spec.encryption.provider`: required when `spec.encryption` is set; value `sops`.
  - `spec.encryption.secretRef.name`: Secret containing SOPS key material or static cloud credentials.
- Resolution and scope:
  - Referenced Secret is namespace-local to the `GitProvider`.
  - Key material is runtime-only and must never be committed to git.
- Deferred:
  - `spec.encryption.serviceAccountName` is intentionally not included yet.
- Validation/status intent:
  - Surface clear status/condition errors when encryption reference is missing/invalid and Secret semantics are matched.

## `.sops.yaml` Definition Strategy

- For this increment, keep `.sops.yaml` source simple and static:
  - ensure presence in repo root during write flow.
  - do not attempt to store/transport full `.sops.yaml` content in `GitProvider.spec`.
- Rationale:
  - keeps API small and avoids embedding large policy blobs in CRDs.
  - lets users customize `.sops.yaml` directly in git after bootstrap.
  - avoids introducing policy merge/validation semantics in this phase.
- Revisit later if needed:
  - if teams need declarative policy management, add an explicit reference-based source (for example Secret/ConfigMap ref), not an inline multi-line spec blob.

## Implementation Plan

1. Add/keep a single ensure helper in the Git write pipeline that checks/creates root `.sops.yaml` for the branch being written.
2. Keep ensure logic "existence only" (no content reconciliation).
3. Add `GitProvider.spec.encryption` wiring with fields aligned to Flux naming (`provider`, `secretRef.name`) and pass resolved encryption config into write workers.
4. Move `isSecretResource` from `internal/git/content_writer.go` to `internal/types/identifier.go` and use it from call sites that need Secret semantics.
5. Add/update tests for:
   - `.sops.yaml` created when missing during write.
   - `.sops.yaml` untouched when present.
   - `GitProvider` Git auth `spec.secretRef` and encryption `spec.encryption.secretRef.name` are independently resolved and validated.
   - only `provider: sops` is accepted.
   - Secret-semantics helper behavior remains correct after move.

## References

- `docs/design/sops-repo-bootstrap-out-of-scope.md`
- `internal/git/content_writer.go`
- `internal/types/identifier.go`
