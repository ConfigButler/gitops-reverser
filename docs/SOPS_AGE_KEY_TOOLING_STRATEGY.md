# SOPS Age Key Tooling Strategy (CLI vs Operator)

## Purpose

Define how Age key lifecycle tooling should integrate with GitOps Reverser and Flux, with a focus on:
- first-time user experience,
- operational safety,
- clear component boundaries.

## Problem statement

GitOps Reverser should stay focused on:
- capturing Kubernetes changes,
- encrypting with resolved recipients,
- bootstrapping `.sops.yaml` recipient policy.

Private-key lifecycle workflows are separate concerns:
- generating and storing Age identities,
- rotating identities,
- validating whether old recipients are still in use.

## Design principles

1. Keep key management explicit and auditable.
2. Keep controller responsibilities small.
3. Prefer least-privilege RBAC for key operations.
4. Make onboarding one-command simple for first-time users.
5. Keep Flux compatibility first-class (`*.agekey`).

## Option A: Companion CLI

### Model

A standalone CLI (or `kubectl` plugin) performs explicit key lifecycle actions on demand.

### Pros

- Best first-time UX (guided init flows).
- Least controller complexity.
- Lower persistent RBAC footprint than always-on automation.
- Easy to use in local dev, CI, and GitOps pipelines.
- Works for Flux-only users (no GitOps Reverser dependency).

### Cons

- Requires users/CI to run commands intentionally.
- Rotation can be forgotten without policy reminders.
- No continuous reconciliation loop for drift correction.

### Best-fit use cases

- Initial setup.
- Periodic manual rotations.
- Platform teams that prefer explicit, reviewable key changes.

## Option B: Key Lifecycle Operator

### Model

A dedicated Kubernetes operator reconciles desired key lifecycle state continuously.

### Pros

- Strong automation for recurring tasks.
- Can enforce org policy (age limits, rotation windows, stale-key alerts).
- Reduces manual operational burden at larger scale.

### Cons

- Higher implementation and maintenance complexity.
- Higher security risk surface (long-lived RBAC over secrets and repos).
- Harder failure modes (partial rewraps, broad commit churn, rollback complexity).
- Can blur boundaries between config reconciliation and key lifecycle governance.

### Best-fit use cases

- Large multi-team environments with strict compliance automation requirements.
- Mature installations after CLI workflows are proven.

## Recommendation

Start with a companion CLI, not an operator.

Rationale:
- Faster path to high-quality onboarding.
- Cleaner architecture boundary for GitOps Reverser.
- Lower risk while SOPS support is pre-release.

Re-evaluate operator path later if policy automation demand becomes strong.

## Existing work and reusable tools

### `age` CLI

Useful primitives:
- `age-keygen` for identity generation.
- `age-keygen -y` for recipient derivation from private key files.

Why it matters:
- battle-tested key format and generation workflow,
- no need to reinvent cryptographic key generation.

### `sops` CLI

Useful primitives:
- encryption/decryption workflows,
- `sops updatekeys` for rewrapping existing encrypted files to new recipient sets.

Why it matters:
- key update operations can stay explicit and operator-driven at first.

### Flux SOPS integration

Useful contract:
- Flux decryption Secret detection via `*.agekey` entries.

Why it matters:
- shared format between GitOps Reverser and Flux avoids duplicated key material models.

References:
- https://fluxcd.io/flux/components/kustomize/kustomizations/
- https://getsops.io/docs/
- https://github.com/FiloSottile/age

## Interface contract with GitOps Reverser

The helper tool should integrate through stable data contracts, not internal controller hooks.

### Inputs expected by GitOps Reverser

1. `GitTarget.spec.encryption.provider` (for this design: `sops`).
2. `GitTarget.spec.encryption.age.enabled`.
3. `GitTarget.spec.encryption.age.recipients.publicKeys`.
4. `GitTarget.spec.encryption.age.recipients.extractFromSecret`.
5. `GitTarget.spec.encryption.age.recipients.generateWhenMissing`.
6. `GitTarget.spec.encryption.secretRef` pointing to Secret entries ending with `.agekey`.

### Outputs produced by helper tool

1. Kubernetes Secret (Flux-compatible):
- `type: Opaque`
- `stringData.identity.agekey: AGE-SECRET-KEY-...`

2. Recipient list:
- derived `age1...` values for `age.recipients.publicKeys`.

3. Optional patch/apply action:
- update target `GitTarget` encryption fields under `spec.encryption`.

### Behavior boundary

- GitOps Reverser consumes recipients and bootstraps `.sops.yaml`.
- Helper tool owns key generation and lifecycle workflows.
- Rewrap (`sops updatekeys`) stays in helper/manual operations, not controller reconcile loops.

## First-time user experience design

### Goal

Enable safe setup in one guided flow with minimal crypto knowledge.

### Suggested MVP flow

1. User chooses target boundary (`namespace/name` of `GitTarget`).
2. Tool generates one Age identity for that boundary.
3. Tool creates Secret with `identity.agekey`.
4. Tool derives and prints recipient (`age1...`).
5. Tool patches/applies `spec.encryption` (provider + age config + secretRef).
6. Tool runs validation checks and prints next steps.

### Why this improves onboarding

- Avoids manual Secret formatting mistakes.
- Avoids confusion around recipient vs private key.
- Produces Flux-compatible artifacts from day one.
- Keeps an explicit audit trail of key lifecycle actions.

## Key usage counters (future helper capability)

### What to count

Count recipient (`age1...`) references in SOPS metadata across `*.sops.yaml` files.

Important:
- count by recipient public key,
- not by Secret entry name (`identity.agekey`).

### Boundary rules

- default boundary: one keypair per GitTarget,
- count only repos/paths owned by that boundary.

### Retirement signal

A recipient is retirement-candidate only when usage count is zero in the chosen boundary.
Initially treat this as report-only guidance.
