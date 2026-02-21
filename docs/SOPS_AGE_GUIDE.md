# SOPS + age Guide

This is a practical reference for how `sops` and `age` fit together in this repo, and which assets/commands matter.

## Mental model

- `age` is the crypto system.
- `sops` is the tool that encrypts/decrypts YAML/JSON/etc. and stores metadata.
- You encrypt *to recipients* (public keys).
- You decrypt with matching private keys.

For this repo:
- `.sops.yaml` contains recipient rules (public info).
- Private key material is provided at runtime via Kubernetes Secret env entries (for example `SOPS_AGE_KEY`).
- Secret manifests written to Git are stored as `*.sops.yaml`.

## Important assets

- `age` private key: secret, never commit.
- `age` recipient (public key): safe to commit/share.
- `.sops.yaml`: encryption policy/rules and recipients (usually committed).
- encrypted `*.sops.yaml` files: committed artifacts.

## Install tools

```bash
# macOS
brew install sops age

# Ubuntu/Debian
sudo apt-get update
sudo apt-get install -y age
# sops may come from distro package or release binary
```

## Create a new age keypair

```bash
umask 077
age-keygen -o age-key.txt
```

This creates:
- private key in `age-key.txt` (line starts with `AGE-SECRET-KEY-...`)
- recipient in a comment line like `# public key: age1...`

Get recipient from private key file:

```bash
age-keygen -y age-key.txt
```

## Create/update `.sops.yaml`

Minimal example:

```yaml
creation_rules:
  - path_regex: '.*\.sops\.ya?ml$'
    encrypted_regex: '^(data|stringData)$'
    key_groups:
      - age:
          - "age1xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
```

Notes:
- `path_regex` controls which files are encrypted with this rule.
- `encrypted_regex` keeps Kubernetes Secret payload fields encrypted.
- You can add multiple recipients under `age:`.
- SOPS encrypts to all listed recipients; it does not select only one.

## Encrypt/decrypt with sops

Encrypt file:

```bash
sops --encrypt --input-type yaml --output-type yaml \
  secret.yaml > secret.sops.yaml
```

Decrypt file:

```bash
SOPS_AGE_KEY="$(grep '^AGE-SECRET-KEY-' age-key.txt)" \
  sops --decrypt secret.sops.yaml
```

Or use key file:

```bash
export SOPS_AGE_KEY_FILE="$PWD/age-key.txt"
sops --decrypt secret.sops.yaml
```

## Key material in Kubernetes

In this project, encryption env vars are sourced from `GitTarget.spec.encryption.secretRef`.
Create the runtime secret like this (namespace must match the `GitTarget` namespace):

```bash
kubectl -n sut create secret generic sops-age-key \
  --from-literal=SOPS_AGE_KEY="$(grep '^AGE-SECRET-KEY-' age-key.txt)"
```

Then reference it from `GitTarget.spec.encryption.secretRef.name`.

Optional bootstrap mode:

```yaml
spec:
  encryption:
    provider: sops
    secretRef:
      name: sops-age-key
    generateWhenMissing: true
```

When enabled and the secret is missing, gitops-reverser generates one age key and
creates the secret. Generated secrets include:
- `configbutler.ai/age-recipient: age1...`
- `configbutler.ai/backup-warning: REMOVE_AFTER_BACKUP`

Important: back up the generated private key immediately. Remove the warning
annotation after backup:

```bash
kubectl annotate secret sops-age-key -n <namespace> configbutler.ai/backup-warning-
```

## Rotation (new recipient/key)

1. Generate new keypair.
2. Add new recipient to `.sops.yaml`.
3. Re-wrap existing encrypted files:

```bash
sops updatekeys -y path/to/file.sops.yaml
# or for many files
find . -name '*.sops.yaml' -print0 | xargs -0 -n1 sops updatekeys -y
```

4. Deploy new private key to runtime secret.
5. Remove old recipient from `.sops.yaml` and run `updatekeys` again when ready.

## Repo-specific security note

The bootstrap template currently contains a static recipient in
`internal/git/bootstrapped-repo-template/.sops.yaml`.

- This is a public recipient, not a private key.
- It is not auto-replaced by the controller.
- If you need your own keys, update committed `.sops.yaml` in the repo path and re-wrap files with `sops updatekeys`.

## Design plan: `generateWhenMissing`

Moved to a dedicated document: [`docs/SOPS_GENERATE_WHEN_MISSING_PLAN.md`](docs/SOPS_GENERATE_WHEN_MISSING_PLAN.md)

## Design plan: Flux-aligned key handling

Implications and proposal for optional secretless encryption, Flux-compatible
`*.agekey` generation, and multi-key support:
[`docs/SOPS_FLUX_AGE_KEY_ALIGNMENT_PLAN.md`](docs/SOPS_FLUX_AGE_KEY_ALIGNMENT_PLAN.md)
