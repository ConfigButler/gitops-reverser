# Path-Scoped Bootstrap Template Design (Direction Change)

## Why this document

This documents:
1. What was just implemented.
2. Why that direction is wrong for multi-`GitTarget` setups.
3. The new design direction for bootstrap files per `GitTarget.spec.path`.
4. Encryption ownership and runtime behavior for multi-path repos.

## Current Implementation State (as of now)

Recent changes introduced a repository-level bootstrap template:
- `internal/git/bootstrapped-repo-template/.sops.yaml`
- `internal/git/bootstrapped-repo-template/README.md`

And wired bootstrap into branch initialization:
- `PrepareBranch(...)` in `internal/git/git.go` now creates and pushes a bootstrap commit on unborn/empty branches.
- Normal write path no longer ensures `.sops.yaml`.

### What this means behavior-wise

- Bootstrap is currently **branch-scoped** and **repo-root scoped**.
- It does not distinguish multiple `GitTarget` paths sharing the same `(GitProvider, branch)`.

## Why this does not match the required model

You are correct: the system supports multiple `GitTarget`s on the same repo/branch, each with its own folder (`spec.path`).

Current bootstrap behavior is wrong because:
- It initializes only repo root, not each target folder.
- `BranchWorker` is the sharing boundary (per provider+branch), so path-level intent should be handled there.
- A new `GitTarget` path can be introduced long after branch creation, but current bootstrap logic only runs during initial branch preparation.

## Requirement interpretation

Requested behavior:
- When a **new unique `GitTarget` folder path** is introduced, copy `bootstrapped-repo-template` into that folder.
- This is path-level bootstrap, not branch-root bootstrap.

This makes architectural sense in this codebase.

## Encryption placement decision

Encryption behavior must be configured at `GitTarget`, not `GitProvider`.

Reason:
- multiple `GitTarget`s can share one repo/branch but represent different environments (`test`, `prod`).
- those environments may require different key material.
- `sops` already supports path-based key selection via nearest `.sops.yaml`.

Decision:
- `GitProvider` remains read-only transport/auth concerns (clone/fetch/push).
- remove encryption configuration from `GitProvider` in this direction.
- add encryption configuration to `GitTarget`:
  - `spec.encryption.provider` (`sops` for now)
  - `spec.encryption.secretRef.name` (namespace-local secret with sops credentials/env)

Runtime behavior:
- when writing an event for a `GitTarget`, the writer uses that target's encryption credentials.
- `sops` resolves rules from the path-scoped `.sops.yaml` created in the target folder.
- no repo-wide default encryption in this increment (explicitly out of scope).

## Final semantic choice

Bootstrap is **one-time per path registration**.

Meaning:
- when a normalized path is first registered and not yet marked bootstrapped, initialize it.
- after success, persist that this path is bootstrapped.
- do not continuously heal/restore files during normal write flow.

## Proposed design direction

## 1) Move bootstrap trigger from `PrepareBranch` to path-aware registration flow

Remove bootstrap commit logic from `PrepareBranch`.

Bootstrap should happen when a new path is registered for a worker:
- Entry point: `WorkerManager.RegisterTarget(...)`
- Worker key: `(providerNamespace, providerName, branch)`
- Path argument already exists in `RegisterTarget` and is currently only logged.

## 2) Add a BranchWorker service: `EnsurePathBootstrapped(path string)`

`BranchWorker` should expose a synchronous method that:
- ensures local repo is initialized (`PrepareBranch` without bootstrap side effects)
- writes template files under `<path>/` (or repo root if path is empty)
- commits and pushes only if files were newly added

Template source should be renamed to:
- `internal/git/path-bootstrap-template/`

Rationale:
- name reflects path-scoped behavior (not repo-wide bootstrap).
- clear ownership to GitTarget bootstrap concern.

Should these files live under `internal/`?
- yes, for this increment.
- they are controller runtime assets, not external API or user library surface.
- if we later support user-provided templates, that can become a separate API/reference mechanism.

## 3) Track unique bootstrapped paths per worker

`WorkerManager` should track per worker which paths are already bootstrapped (in-memory map keyed by normalized path).

On `RegisterTarget(...)`:
- If path is new for this worker: call `EnsurePathBootstrapped(path)`.
- If already seen: skip.

This keeps steady-state cheap and avoids per-event checks.

Persistence is required (see section below), so in-memory cache is only a fast mirror of persisted status.

## 4) Resolve encryption per GitTarget (not per worker)

`BranchWorker` is shared by `(provider, branch)`, so one shared encryptor is unsafe.

Required change:
- stop caching encryption config as one worker-global writer state.
- for each event batch (or each event), resolve encryption from the owning `GitTarget`.
- use a writer/encryptor scoped to that `GitTarget` credentials.

This prevents cross-target credential bleed when multiple paths share a branch worker.

## 5) Keep normal event write path focused on mirroring resources

`WriteEvents(...)` remains path-aware for resource files only.
No bootstrap enforcement in write loop.

## 6) Path normalization rules (must be shared)

Reuse existing path sanitizer behavior (or equivalent) so these are treated consistently:
- `"clusters/prod"`
- `"/clusters/prod/"` (if ever accepted upstream)
- `""` (repo root)

Use one canonical key for uniqueness tracking.

## 7) Commit strategy

Bootstrap commit should be small and explicit, e.g.:
- `chore(bootstrap): initialize path <path>`

If multiple files are added for same path, keep them in one commit.

## Edge cases to handle explicitly

1. Nested paths (`clusters` and `clusters/prod`):
- Current webhook uniqueness appears to prevent exact duplicates, not prefix overlaps.
- For now: implement it so that we don't accept overlaps.

2. Path updates on an existing `GitTarget`:
- Treat new path as newly introduced and bootstrap it.
- Old path is not cleaned up automatically.

3. Deleted/recreated targets:
- Recreated target with same path should bootstrap only if path has not been seen in current process.
- On controller restart, behavior depends on whether we persist path-bootstrap state.

4. Manual deletion of bootstrap files:
- If we keep one-time semantics, we do not continuously restore them.

## Persistence choice (recommended)

Use `GitTarget.status` for persistence.

Proposed status field:
- `status.bootstrappedPaths: []string`
  - normalized path values successfully bootstrapped by this target.
  - include `""` for repo-root path.

Why list instead of bool:
- supports path changes over time without losing history.
- keeps one-time semantics explicit per normalized path.

Controller startup check (required):
- on startup, list GitTargets and load `status.bootstrappedPaths`.
- seed worker/path bootstrap cache from persisted status.
- for each active GitTarget:
  - normalize `spec.path`
  - if path already present in `status.bootstrappedPaths`, skip bootstrap
  - if missing, run bootstrap once and update status on success

Status update contract:
- append path only after bootstrap commit+push succeeded.
- do not append on partial failure.
- use status patch with retry/conflict handling.

## Implementation impact summary

Will need updates in:
- `internal/git/git.go` (remove branch-level bootstrap from `PrepareBranch`)
- `internal/git/bootstrapped_repo_template.go` (support writing template into arbitrary base path; rename template dir usage)
- `internal/git/worker_manager.go` (track unique paths per worker, call bootstrap)
- `internal/git/branch_worker.go` (new synchronous bootstrap service method)
- `internal/git/encryption.go` (resolve from `GitTarget` instead of `GitProvider`)
- `internal/controller/gittarget_controller.go` and related flows (pass/resolve target encryption context)
- `api/v1alpha1/gittarget_types.go` + CRDs (add `spec.encryption`, add `status.bootstrappedPaths`)
- `api/v1alpha1/gitprovider_types.go` + CRDs (remove/deprecate encryption there in this direction)
- `internal/controller/gittarget_controller.go` (ensure registration/bootstrap is triggered at the right lifecycle point and rehydrated at startup)
- tests in `internal/git/*_test.go`, controller tests, and e2e tests

## Conclusion

Your requested direction is valid and better aligned with the actual multi-target architecture.

Key corrections:
- bootstrap must be **path-scoped at worker registration time**, not **repo-root at branch initialization time**.
- encryption must be **GitTarget-scoped**, with no repo-wide default in this increment.
