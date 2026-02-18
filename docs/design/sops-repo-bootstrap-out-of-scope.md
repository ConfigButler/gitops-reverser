# SOPS Repo Bootstrap: Out of Scope (Current Increment)

This file tracks explicit non-goals and deferred items, so the main architecture plan stays small and execution-focused.

## Explicitly Not Doing Now

- Auto-generating AGE or other key material.
- Generic controller-level `.sops.yaml` override support.
- `.sops.yaml` content reconciliation (enforcing exact template match).
- Workload identity integration.
- Full key lifecycle orchestration (rotation, backup, recovery automation).
- Replacing external `sops` invocation with in-process encryption.

## Notes

- `.sops.yaml` remains user-adjustable once present in the repo.
- Future increments can re-introduce any item above with explicit API and operational design.
