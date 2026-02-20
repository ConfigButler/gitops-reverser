# Design plan: `generateWhenMissing`

Goal: make first-time SOPS setup easier by generating an age private key when
the configured encryption Secret does not exist, while keeping failure behavior
safe and explicit.

## Scope and non-goals

In scope:
- SOPS provider with age key in `SOPS_AGE_KEY`.
- Automatic creation of the runtime encryption Secret when missing.
- Status/conditions so operators can see whether key bootstrap succeeded.

Out of scope:
- Automatic recipient rotation and mass re-wrap of existing `*.sops.yaml` files.
- Backup escrow implementation (we can integrate with it later).

## API changes

Add fields to `EncryptionSpec`:

```yaml
spec:
  encryption:
    provider: sops
    secretRef:
      name: sops-age-key
    generateWhenMissing: false
```

Optional extension for safety policy:

```yaml
spec:
  encryption:
    generateWhenMissing: true
```

Rules:
- Default is `false` (BYOK remains default).
- If `true`, controller may create `secretRef.name` only when not found.
- Existing valid Secret is never overwritten.

## Reconcile flow

1. GitTarget reconcile validates provider/branch as today.
2. If encryption is configured and `generateWhenMissing=true`:
   - Fetch `secretRef.name` in GitTarget namespace.
   - If missing, generate one age identity and create Secret with `SOPS_AGE_KEY`.
   - If already exists, use as-is.
3. Continue normal worker registration/bootstrap. Existing logic already derives
   the age recipient from `SOPS_AGE_KEY` and renders `.sops.yaml`.

Key design choice:
- Do key generation in the GitTarget controller reconcile path, not in worker
  write path. This keeps lifecycle/status visible and avoids hidden side effects
  during event processing.

## Secret contract

Generated Secret shape:
- `type: Opaque`
- `data["SOPS_AGE_KEY"]`: exactly one `AGE-SECRET-KEY-...` identity

Recommended metadata:
- `configbutler.ai/age-recipient: age1...` (public metadata, useful for audit)
- `configbutler.ai/backup-warning: REMOVE_AFTER_BACKUP`
  - As long as this annotation exists, controller prints a recurring high-visibility
    warning during reconciliation (`RequeueLongInterval`, currently 10 minutes).

## Status and observability

Add or reuse GitTarget conditions:
- `EncryptionReady=True` when usable key is present.
- `EncryptionReady=False` with reason:
  - `EncryptionSecretMissing` (and auto-generate disabled)
  - `EncryptionSecretInvalid`
  - `EncryptionSecretCreateFailed`

Metrics/logging:
- Counter: generated keys total.
- Counter: generation failures total.
- Structured log on key generation with target and secret names (never log key).

## Concurrency and idempotency

Multiple reconciles may race. Use create-then-handle-AlreadyExists semantics:
- Attempt `Create(secret)`.
- If `AlreadyExists`, fetch and validate instead of failing.
- Never rotate or replace key during this workflow.

This guarantees eventual success without duplicate key churn.

## Security and recovery requirements

Hard requirements:
- Never print key material in logs/events/status.
- RBAC should allow create/get/list/watch Secrets in target namespace.
- Document clearly: losing this Secret means old encrypted files cannot be
  decrypted.

Recommended safeguard:
- Emit a Warning event once after first generation: backup required.

## Rotation plan (separate feature)

Treat rotation as a dedicated workflow/tool:
1. Add new recipient to `.sops.yaml`.
2. Re-wrap (`sops updatekeys`) all encrypted files.
3. Roll out new private key.
4. Remove old recipient and re-wrap again.

Do not couple rotation into `generateWhenMissing`; that should stay bootstrap-only.

## Implementation phase

Single phase (MVP + hardening):
- Add `generateWhenMissing` to API.
- Implement Secret generation + validation in GitTarget reconcile.
- Add explicit recipient annotation on generated secret.
- Add backup warning annotation (`configbutler.ai/backup-warning: REMOVE_AFTER_BACKUP`)
  and recurring controller warning while the annotation remains.
- Add condition updates and tests (unit + e2e):
  - missing secret + enabled flag -> generated key + successful SOPS write.
