# SOPS Age Key Alignment Plan (GitOps Reverser + Flux)

## Goal

Define a minimal and stable SOPS/Age configuration model that:
- keeps `encryption.provider` open for future providers,
- keeps Age-specific controls under `encryption.age`,
- supports Flux-compatible secret handling.

## Target configuration model

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: GitTarget
metadata:
  name: byok
  namespace: default
spec:
  providerRef:
    name: my-provider
  branch: main
  path: clusters/prod
  encryption:
    provider: sops
    age:
      enabled: true
      recipients:
        publicKeys: []
        extractFromSecret: true
        generateWhenMissing: true   # Requires right to create/update the referenced Secret
    secretRef:
      name: flux-age
```

## Why this shape

- `encryption.provider` stays generic for future encryption methods.
- bootstrap policy file is implicit and fixed to `.sops.yaml` for now.
- `age.enabled` explicitly gates Age behavior.
- `secretRef` remains at `encryption` level so future providers can reuse it if needed.
- `age.recipients.*` keeps recipient source and bootstrap behavior explicit.

## Behavior contract

1. Recipient resolution:
- Start from `encryption.age.recipients.publicKeys`.
- If `extractFromSecret=true`, derive recipients from all `*.agekey` entries in `encryption.secretRef`.
- Union + deduplicate recipients.

2. Missing key behavior:
- If `generateWhenMissing=true` and Secret does not exist, create `encryption.secretRef` with one `identity.agekey`.
- If `generateWhenMissing=true` and Secret exists but has no `*.agekey` entry, add one `identity.agekey`.
- If Secret already has at least one `*.agekey`, do not generate an additional key.

3. Bootstrap behavior:
- Use `.sops.yaml` as the implicit bootstrap target.
- SOPS encrypts to all listed recipients, not one selected recipient.

4. Validation:
- If `encryption.provider=sops` and `age.enabled=true`, recipient set must resolve non-empty.
- If `extractFromSecret=true` or `generateWhenMissing=true`, `encryption.secretRef.name` is required.

5. Security:
- Never log private key material.
- Never write private keys to Git.

## Key update policy (for now)

Automatic rewrap/rotation remains out of scope for controller logic.

For now:
1. Update recipient policy.
2. Run `sops updatekeys` manually.
3. Commit rewrapped files explicitly.

## Concrete implementation actions (source-code mapped)

This section maps the target design to concrete code changes in the current codebase.

### 1. Extend API model in `GitTarget` encryption spec

Current state:
- `EncryptionSpec` exists in `api/v1alpha1/gitprovider_types.go` and currently has
  `provider`, `secretRef`, and top-level `generateWhenMissing`.

Actions:
1. Add `spec.encryption.age.enabled`.
2. Add `spec.encryption.age.recipients.publicKeys`.
3. Add `spec.encryption.age.recipients.extractFromSecret`.
4. Add `spec.encryption.age.recipients.generateWhenMissing`.
5. Keep `spec.encryption.secretRef` at encryption level.
6. Remove/deprecate top-level `spec.encryption.generateWhenMissing` in favor of recipient-scoped flag.
7. Regenerate API artifacts (`make generate`, `make manifests`) when implementing.

Notes:
- Because SOPS support is not released yet, implement this as a clean shape update (no migration path required).

### 2. Validation and defaults

Current state:
- Validation for uniqueness exists in `internal/webhook/gittarget_validator.go`.
- Encryption field validation is mostly runtime/controller-side.

Actions:
1. Add schema/webhook validation for:
- If `age.enabled=true`, recipient resolution inputs must be configured (`publicKeys` and/or `extractFromSecret`).
- If `extractFromSecret=true` or `generateWhenMissing=true`, `encryption.secretRef.name` is required.
- If `generateWhenMissing=true`, `extractFromSecret` must also be `true`.
2. Keep failure reasons actionable so users can fix spec quickly.

### 3. Update encryption secret management to Flux key format

Current state:
- `internal/controller/gittarget_controller.go` (`ensureEncryptionSecret`) creates Secret data key `SOPS_AGE_KEY`.
- RBAC marker currently allows Secret `create` but not `update/patch`.

Actions:
1. Change generated Secret content to Flux-compatible `*.agekey` entries (default `identity.agekey`).
2. Keep generation behavior single-key:
- Missing Secret: create with one `identity.agekey`.
- Existing Secret with no `*.agekey`: add one `identity.agekey`.
- Existing Secret with `*.agekey`: do nothing.
3. Update controller RBAC marker to allow Secret updates (needed when adding `identity.agekey` to an existing Secret).
4. Keep backup-warning annotation behavior.

### 4. Refactor encryption resolution for hybrid mode

Current state:
- `internal/git/encryption.go` expects `secretRef` and resolves env vars from Secret data.
- Runtime assumes `SOPS_AGE_KEY` is present for age recipient derivation.

Actions:
1. Split recipient resolution from private-key/env resolution:
- Recipient set = `publicKeys` union extracted recipients from `*.agekey` (if enabled).
2. Make secret optional for encryption-only mode:
- If `extractFromSecret=false` and `generateWhenMissing=false`, allow public-key-only path.
3. Maintain deterministic recipient output (sort + deduplicate).
4. Keep strict parsing for `*.agekey` values and never log private key material.

### 5. Bootstrap `.sops.yaml` using resolved recipients

Current state:
- `internal/git/branch_worker.go` derives one recipient from one key.
- `internal/git/bootstrapped_repo_template.go` template data has `AgeRecipient string`.
- `internal/git/bootstrapped-repo-template/.sops.yaml` renders one age recipient.

Actions:
1. Change template data to `AgeRecipients []string`.
2. Render all resolved recipients in `.sops.yaml` (`key_groups[].age[]`).
3. Keep bootstrap target file fixed to `.sops.yaml` for now.
4. Ensure bootstrap still skips `.sops.yaml` creation when encryption/age is disabled.

### 6. Tests to add/update during implementation

Actions:
1. Update API/validation tests:
- `internal/webhook/gittarget_validator_test.go`
2. Update controller tests for generation and existing-secret update behavior:
- `internal/controller/gittarget_controller_test.go`
3. Update recipient/env resolution and parsing tests:
- `internal/git/encryption_test.go`
4. Update bootstrap rendering tests for multi-recipient output:
- `internal/git/branch_worker_test.go`
- `internal/git/bootstrapped_repo_template.go` companion tests (if added)
5. Update docs/examples after code lands:
- `docs/SOPS_AGE_GUIDE.md`
- `internal/git/bootstrapped-repo-template/README.md`

## Configuration examples

### Example A: Reverser-only (public keys only)

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: GitTarget
metadata:
  name: encrypt-only
  namespace: default
spec:
  providerRef:
    name: my-provider
  branch: main
  path: clusters/dev
  encryption:
    provider: sops
    age:
      enabled: true
      recipients:
        publicKeys:
          - age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq7k8m6
        extractFromSecret: false
        generateWhenMissing: false
```

### Example B: Hybrid with secret extraction + generation

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: GitTarget
metadata:
  name: hybrid-autogen
  namespace: default
spec:
  providerRef:
    name: my-provider
  branch: main
  path: clusters/dev
  encryption:
    provider: sops
    age:
      enabled: true
      recipients:
        publicKeys:
          - age1yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyv0r2a
        extractFromSecret: true
        generateWhenMissing: true
    secretRef:
      name: sops-age-key
```

### Example C: BYOK Flux secret, no generation

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: GitTarget
metadata:
  name: byok
  namespace: default
spec:
  providerRef:
    name: my-provider
  branch: main
  path: clusters/prod
  encryption:
    provider: sops
    age:
      enabled: true
      recipients:
        publicKeys: []
        extractFromSecret: true
        generateWhenMissing: false
    secretRef:
      name: flux-age
```

Example Secret shape:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: flux-age
  namespace: default
type: Opaque
stringData:
  identity.agekey: |
    AGE-SECRET-KEY-1XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX
```
