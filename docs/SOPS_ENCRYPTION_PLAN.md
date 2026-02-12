# SOPS Encryption Plan For Git Writes

## Goal

Encrypt sensitive Kubernetes resources (initially `Secret`) with SOPS before they are written to the Git worktree, so commits contain encrypted payloads instead of plaintext `data`/`stringData`.

## Scope

- In scope:
  - Encrypt on write path (watch event -> sanitize -> git file write).
  - Support SOPS execution strategy for encryption (external binary first iteration).
  - Add runtime configuration for enablement, policy, and SOPS invocation.
  - Support standard SOPS key backends through mounted credentials/config.
  - Tests, docs, and Helm wiring.
- Out of scope (first iteration):
  - Decryption in controller runtime.
  - Re-encrypting existing historical commits.
  - Complex per-namespace/per-rule encryption policies.

## Current Baseline (Why this is needed)

- `internal/sanitize/sanitize.go` preserves `data` and `binaryData`.
- `internal/watch/informers.go` enqueues sanitized objects as-is.
- `internal/git/git.go` writes YAML generated from event object directly to disk.
- Result: if a `WatchRule` includes `secrets` (or `*`), secret payloads are committed in plaintext.

## High-Level Design

### 1. Encryption Hook Point

Add encryption at the final write stage in `internal/git/git.go` inside `handleCreateOrUpdateOperation`:

1. Generate ordered YAML from sanitized object (existing behavior).
2. Apply encryption policy:
  - If resource should be encrypted, run SOPS encryption.
  - If not, keep plaintext YAML.
3. Continue with existing file compare/write/stage logic.

This keeps upstream watch/sanitize flow unchanged and centralizes git-output guarantees.

### 2. Encryption Policy

Introduce explicit policy config (controller process-level first):

- `disabled` (default for backward compatibility).
- `secretsOnly` (recommended default when enabled).
- `matchResources` (future): configurable list of `(group, version, resource)` patterns.

Initial policy decision:
- Encrypt only Kubernetes `Secret` resources (`group=""`, `version="v1"`, `resource="secrets"`).

### 3. SOPS Invocation Model

Use external SOPS binary for first implementation, invoked by the manager process.

Proposed approach:

1. Write plaintext YAML to a secure temp file in `/tmp`.
2. Run SOPS command to produce encrypted YAML.
3. Read encrypted output and remove temp files.
4. Write encrypted output to repo path.

Command strategy:
- Prefer `.sops.yaml`-driven encryption rules.
- Allow optional explicit args passthrough only through an allowlist (for example output/input type and config path), not arbitrary raw flags.

Failure behavior (configurable):
- `failClosed` (recommended): do not write/commit if encryption fails.
- `failOpen` (optional): log error and write plaintext (not recommended for production).

### 3a. Architecture Choice: External Binary vs Embedded Library

This should be an explicit engineering decision, not a hidden assumption.

Option A: External SOPS binary (first iteration)
- Pros:
  - Reuses upstream SOPS behavior exactly (CLI parity with existing workflows).
  - Faster implementation and lower maintenance in this codebase.
  - Keeps cloud KMS/age/PGP backend behavior aligned with standard SOPS usage.
- Cons:
  - Extra process spawn overhead per encrypted object.
  - Runtime dependency management (binary presence, version pinning, CVE tracking).

Option B: Embed encryption implementation directly in `gitops-reverser`
- Pros:
  - No external process execution; simpler runtime dependency surface.
  - Potentially better performance and tighter observability hooks.
- Cons:
  - Higher implementation and long-term maintenance cost.
  - Risk of behavior drift from upstream SOPS semantics and config handling.
  - More complex support burden across key backends.

Decision for this plan:
- Implement Option A first (external binary), behind feature flags.
- Keep abstraction boundary (`Encryptor` interface) so Option B can be added later without reworking git write flow.

Revisit triggers:
- Encryption latency becomes a measurable bottleneck.
- Operational burden from binary distribution/versioning is high.
- There is a strong requirement for in-process crypto execution.

### 4. Runtime Config Model

Add manager flags + Helm values for encryption:

- `--encryption-enabled`
- `--encryption-policy=secretsOnly|disabled`
- `--encryption-provider=sops`
- `--sops-binary-path=/usr/local/bin/sops`
- `--sops-config-path=/etc/sops/.sops.yaml` (optional)
- `--encryption-failure-policy=failClosed|failOpen`

Configuration precedence:
- `--encryption-enabled=false` always disables encryption regardless of policy value.
- `--encryption-enabled=true` requires a non-`disabled` policy.
- Invalid combinations should fail startup with a clear validation error.

Helm values section proposal:

```yaml
encryption:
  enabled: false
  policy: secretsOnly
  failurePolicy: failClosed
  sops:
    binaryPath: /usr/local/bin/sops
    configPath: /etc/sops/.sops.yaml
```

### 5. Key Material / Backend Configuration

Do not invent key management inside the operator. Reuse native SOPS backends:

- `age` via mounted secret and `SOPS_AGE_KEY_FILE`.
- cloud KMS via workload identity / IAM env (AWS/GCP/Azure).
- PGP if needed (lower priority).

Helm should support:

- Extra volume mounts for key files and `.sops.yaml`.
- Extra env vars for SOPS backend configuration.

## Implementation Phases

## Phase 1: Core plumbing (code-only, no encryption yet)

- Add `EncryptionConfig` struct and wire it from `cmd/main.go` into git worker path.
- Add policy evaluator utility (`shouldEncrypt(event)`).
- Add unit tests for policy decisions.

Deliverable:
- Feature-flagged no-op framework merged.

## Phase 2: SOPS binary integration

- Implement `SOPSEncryptor` (interface + concrete implementation).
- Integrate into `handleCreateOrUpdateOperation` before file write.
- Implement temp-file execution with strict permissions.
- Add structured logging and metrics:
  - encrypt attempts
  - encrypt success/failure
  - fail-open count

Deliverable:
- Functional encryption when enabled and policy matches.

## Phase 3: Packaging and Helm configuration

- Update `Dockerfile` multi-stage build:
  - Add stage to fetch pinned SOPS release binary.
  - Copy binary into final distroless image (e.g. `/usr/local/bin/sops`).
- Update chart:
  - New `encryption.*` values.
  - Add manager args from values.
  - Document volume/env examples for keys and `.sops.yaml`.

Deliverable:
- Deployable encrypted workflow via Helm settings.

## Phase 4: Test coverage

- Unit tests:
  - `Secret` gets encrypted.
  - non-secret not encrypted (policy `secretsOnly`).
  - encryption failure with `failClosed` blocks write.
  - encryption failure with `failOpen` writes plaintext and emits warning metric.
  - invalid flag combinations are rejected at config validation time.
- Integration tests (git operations):
  - verify resulting file contains SOPS envelope fields and no raw secret values.
- Optional e2e:
  - run with local age key and assert encrypted commits.

Deliverable:
- CI coverage for happy path and failure modes.

## Phase 5: Documentation and migration

- Update `README.md` and chart README with:
  - enabling encryption
  - key backend setup examples
  - operational caveats
- Add migration note:
  - existing plaintext history remains in git; requires manual history rewrite if needed.

Deliverable:
- Operator docs for secure rollout.

## Security Considerations

- Default to `failClosed` when encryption is enabled.
- Treat `failOpen` as development-only or break-glass behavior.
- Ensure temp files are `0600` and cleaned up.
- Ensure temp-file cleanup runs on both success and failure paths.
- Avoid logging plaintext content.
- Prefer `age` or cloud KMS over static PGP workflows.
- Recommend separate repos/branches for encrypted outputs when integrating with downstream GitOps tools.

## Operational Considerations

- Performance:
  - SOPS process spawn per encrypted object adds overhead.
  - Mitigation: keep policy narrow (`secretsOnly`) and batch commit behavior unchanged.
- Determinism:
  - SOPS metadata may vary; deduplication currently happens pre-write on sanitized plaintext.
  - This is acceptable for first iteration but should be documented.
- Compatibility:
  - Downstream consumers (Flux/Argo) must be configured for SOPS decryption if they deploy encrypted files.

## Proposed Acceptance Criteria

- When enabled with `secretsOnly`, committed Secret manifests are SOPS-encrypted and plaintext secret values never appear in repo files.
- Non-secret resources continue to be committed as before.
- If SOPS is missing or misconfigured:
  - `failClosed`: write is rejected and error surfaced.
  - `failOpen`: plaintext write proceeds with explicit warning/metric (non-production only).
- If invalid encryption configuration is provided, manager startup fails with actionable error output.
- Helm users can:
  - enable encryption
  - mount key/config material
  - point to SOPS binary/config path without rebuilding chart templates manually.

## Suggested Rollout

1. Merge framework + binary integration behind feature flag (disabled by default).
2. Run in staging with `enabled=true`, `policy=secretsOnly`, `failClosed`.
3. Validate commit contents and operational metrics.
4. Roll to production.
5. Optionally extend policy beyond `Secret` after proving stability.
