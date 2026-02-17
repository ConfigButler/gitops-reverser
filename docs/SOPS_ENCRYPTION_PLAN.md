# SOPS Encryption Plan For Git Writes

## Goal

Encrypt sensitive Kubernetes resources (starting with `Secret`) before writing them to the git worktree, so committed manifests never contain plaintext secret values.

## Why This Plan Was Reworked

We now need a plan that:
- assumes Secret write support is enabled (or is being enabled now),
- handles existing safety exceptions that previously blocked Secret commits,
- enforces small implementation steps with test checks after each step,
- and separates content-processing logic from generic git operations.

## Current-State Analysis (Code + Tests)

### 1. Secret write exception still present in watch path

Current code still hard-filters core Secrets:
- `internal/watch/resource_filter.go`
- `internal/watch/informers.go`

This means Secret events are dropped before git write logic.

### 2. Existing tests and docs still encode “never commit Secret” behavior

Current expectation appears in:
- `test/e2e/e2e_test.go` (`should never commit Secret manifests...`)
- `README.md` statement that Secrets are intentionally ignored

If Secret writing is now intentionally enabled, these become migration points that must be updated with new encrypted-write expectations.

### 3. `handleCreateOrUpdateOperation` is overloaded

Current write path in `internal/git/git.go` combines:
- object -> ordered YAML rendering,
- write diff/idempotency check,
- filesystem write/stage.

Adding encryption here directly will increase coupling. Refactoring content logic into its own file should be part of this plan.

## Target Design

## 1. Split content pipeline out of `git.go`

Create a dedicated content writer module in `internal/git`, for example:
- `internal/git/content_writer.go`

Responsibilities:
- Render sanitized object to ordered YAML.
- For `Secret` resources: apply encryptor before write.
- Return final bytes for compare/write.

`handleCreateOrUpdateOperation` should then become orchestration only:
- ask content writer for final content,
- perform existing file compare/write/stage.

## 2. Secret handling rule (phase-1 simplification)

For now, no policy matrix:
- If incoming resource is Kubernetes `Secret` (`group=""`, `version="v1"`, `resource="secrets"`), attempt encryption.
- If encryption succeeds, write encrypted content.
- If encryption fails, do not write and emit a warning log.
- If resource is not `Secret`, keep existing write behavior.

## 3. Encryption provider abstraction

Add interface in `internal/git`:
- `type Encryptor interface { Encrypt(ctx context.Context, plain []byte, meta ResourceMeta) ([]byte, error) }`

First implementation:
- `SOPSEncryptor` invoking external `sops` binary.

This keeps the path open for future embedded implementations.

## 4. Runtime config

Add config inputs (flags + Helm values):
- `--sops-binary-path`
- `--sops-config-path` (optional)

Validation:
- invalid values fail startup.

## 5. Failure behavior

- Required behavior for Secret encryption path: if encryption fails, reject write.
- Emit warning logs for encryption failures so operators can diagnose quickly.
- Plaintext Secret values must never be written to git under any runtime condition.

## 6. Performance and caching strategy (required for usability)

Encryption can be expensive, especially with external KMS/Vault-backed SOPS setups.

Required optimizations:
- Keep pre-write deduplication effective so unchanged Secret content does not trigger a new encryption call.
- Add optional in-memory cache for encrypted payload reuse within a process lifetime.
- Track Secret change markers (`uid`, `resourceVersion`, `generation`) in runtime state to skip obvious no-op re-encryption attempts.

In-memory cache design (safe default):
- Cache key: resource identity + canonical plaintext digest.
- Cache value: encrypted output bytes.
- Scope: process memory only (never persisted to git, CR status, annotations, or commit metadata).

Secret marker usage (runtime only):
- Keep last-seen tuple per Secret: `uid`, `resourceVersion`, `generation`.
- If tuple is unchanged, skip encryption/write work for that Secret event.
- `uid` protects against delete/recreate with same name.
- `resourceVersion` and `generation` provide cheap change detection hints.
- These markers are hints for performance; correctness still depends on final content/diff checks.

Security constraints:
- Do not persist plaintext-derived hash metadata in repository content, commit messages, annotations, or labels.
- Reason: plaintext hashes can leak signal and may allow offline guessing attacks for low-entropy secrets.
- If persisted metadata is ever required in the future, it must be a separately reviewed design (for example keyed HMAC with managed key rotation), not part of first implementation.
- Do not persist `uid`/`resourceVersion`/`generation` optimization state outside process memory in the first implementation.

## 7. Lifecycle side-note (future, not part of first implementation)

- Add a periodic re-encryption workflow (for example monthly) to support routine key rotation.
- This should re-encrypt Secret files with current key material and create normal git commits.
- Periodic re-encryption must not rely on unchanged `uid`/`resourceVersion`/`generation`; it intentionally rewrites encrypted content on cadence.
- This is explicitly out of scope for initial implementation; include as later lifecycle enhancement.

## Implementation Plan (Small Steps + Mandatory Test Gates)

## Phase 0: Align baseline with Secret-write direction

Changes:
- remove or gate secret-ignore filter in watch path.
- update baseline docs/comments that still say secrets are always ignored.

Test gate:
- `go test ./internal/watch/...`
- targeted e2e spec update/validation for secret behavior (no longer “never commit”, now “never commit plaintext”).

Exit criteria:
- Secret events can reach git write path (at least in enabled mode).

## Phase 1: Extract content logic from `handleCreateOrUpdateOperation`

Changes:
- create content writer module (`internal/git/content_writer.go`).
- move marshal + future encryption hook into module.
- keep behavior identical (no encryption yet).

Test gate:
- `go test ./internal/git/...`

Exit criteria:
- no behavior change, cleaner seam for encryption.

## Phase 2: Add Secret-detection plumbing (no SOPS yet)

Changes:
- add encryption config structs + startup validation.
- add simple Secret detector (`isSecretResource(event)` or equivalent).
- pass config into git write pipeline.

Test gate:
- `go test ./cmd/... ./internal/git/...`

Exit criteria:
- Secret detection decisions tested, still no encryption side effects.

## Phase 3: Add `SOPSEncryptor` and integrate

Changes:
- implement CLI-backed encryptor.
- integrate in content writer when resource is Secret.
- ensure temp files are secure and cleaned up.
- emit metrics/logs for attempts/success/failure.
- reject Secret writes on encryption failure (no plaintext fallback path).
- use runtime Secret markers (`uid`/`resourceVersion`/`generation`) to bypass obvious no-op Secret reprocessing.

Test gate:
- `go test ./internal/git/...`
- add integration tests that assert encrypted output shape (SOPS envelope fields) and absence of plaintext values.

Exit criteria:
- Secret writes are encrypted whenever Secret events reach the write path.

## Phase 4: Packaging + Helm wiring

Changes:
- package pinned `sops` binary in image.
- add chart values/args/env/volume wiring for SOPS config and key material.
- add in-memory caching configuration knobs only if needed (size/TTL), defaulting to conservative values.

Test gate:
- `go test ./...`
- helm template/lint checks used in repo workflow.

Exit criteria:
- deployable encrypted workflow through chart config.

## Phase 5: E2E migration and hardening

Changes:
- replace old e2e assertion “Secret file must not exist” with:
  - file exists,
  - content is encrypted,
  - plaintext secret value not present.
- keep/create negative-path e2e for encryption failure behavior, asserting write is rejected and plaintext is never committed.
- add e2e or integration perf check to verify unchanged Secret updates do not repeatedly invoke encryption.
- add integration tests for marker-based skips and delete/recreate behavior (same name, different `uid`).

Test gate:
- targeted e2e secret scenarios.

Exit criteria:
- e2e reflects new contract: encrypted Secret commits, never plaintext.

## Acceptance Criteria

- When Secret events are processed, Secret manifests are committed encrypted.
- Plaintext secret values do not appear in committed files.
- Non-secret resources remain unchanged.
- Encryption errors block Secret writes.
- No fail-open mode exists for Secret encryption.
- Unchanged Secret content can skip re-encryption through dedupe/cache without persisting plaintext-derived metadata.
- Secret-only marker tracking (`uid`/`resourceVersion`/`generation`) reduces redundant re-encryption attempts.
- `handleCreateOrUpdateOperation` no longer owns content transformation/encryption details directly.

## Risks And Mitigations

- Residual legacy assumptions (“secrets never written”) in tests/docs.
  - Mitigation: Phase 0 alignment before encryption implementation.
- Increased complexity in write path.
  - Mitigation: extract content writer first.
- SOPS binary dependency and runtime config drift.
  - Mitigation: pinned version + startup validation + clear metrics.
- Encryption latency with external backends.
  - Mitigation: dedupe first, then in-memory cache; no persisted plaintext-hash metadata.

## Rollout Recommendation

1. Land Phase 0 and Phase 1 first with no encryption behavior change.
2. Roll out Secret encryption in staging (encrypt-or-skip for Secret writes).
3. Verify no plaintext leakage in git history for new commits.
4. Roll out gradually to production targets.
