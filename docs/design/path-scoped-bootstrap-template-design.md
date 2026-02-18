# Path-Scoped Bootstrap Template Design

## Decision

Use path existence in Git as the bootstrap signal:
- if target path contains at least one file, skip bootstrap
- if target path is empty or missing, apply bootstrap template and push

No `GitTarget.status` persistence for bootstrap is needed in this increment.

## Why this is acceptable

- Cheap check: local filesystem walk inside already-cloned worker repo.
- Simple mental model: Git content is source of truth.
- Survives controller restarts without extra status logic.

Tradeoff:
- if users manually add any file in the path, bootstrap will be skipped (intentional with this model).

## Required behavior

- Bootstrap is path-scoped (`GitTarget.spec.path`), not repo-root scoped.
- Trigger on target registration (or first use), not only on unborn branch init.
- Keep normal event writes focused on resource mirroring; no bootstrap healing loop.

## Implementation Order

1. Remove branch-level bootstrap from `PrepareBranch`
- File: `internal/git/git.go`
- Remove `commitBootstrapTemplateIfNeeded(...)` call from unborn-branch flow.
- `PrepareBranch` should only ensure clone/fetch/branch readiness.

2. Make template writer path-aware
- File: `internal/git/bootstrapped_repo_template.go`
- Rename internals from repo bootstrap to path bootstrap semantics.
- Add function to stage template under a base path: `<repo>/<normalizedPath>/...`.
- Keep commit metadata and message explicit, e.g. `chore(bootstrap): initialize path <path>`.

3. Add worker service for path bootstrap
- File: `internal/git/branch_worker.go`
- Add synchronous method: `EnsurePathBootstrapped(path string) error`.
- Steps inside method:
  - ensure repo initialized
  - normalize path
  - check whether path has any file
  - if empty: stage template in that path, commit, push
  - if non-empty: return nil (no-op)

4. Call bootstrap from registration flow
- File: `internal/git/worker_manager.go`
- In `RegisterTarget(...)`, after obtaining/creating worker:
  - call `worker.EnsurePathBootstrapped(path)`
  - return error if bootstrap fails
- Do not add persisted/in-memory bootstrapped path tracking in this increment.

5. Keep encryption scope unchanged in this document
- This document only covers bootstrap trigger semantics.
- Encryption scoping work is separate and can be layered afterward.

6. Tests
- Add/update tests in:
  - `internal/git/git_operations_test.go`
  - `internal/git/branch_worker_test.go`
  - `internal/git/worker_manager_test.go` (or add if missing)
- Must cover:
  - empty path bootstraps once
  - non-empty path skips bootstrap
  - nested distinct paths can both bootstrap when each is empty
  - restart/re-register behavior uses Git content check correctly

## Notes and Edge Cases

- Path normalization must be shared with existing sanitizer rules.
- Repo root path (`""`) is valid and should use same check.
- No automatic cleanup when target path changes.
