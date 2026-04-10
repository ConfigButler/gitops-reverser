# Commit Signing Design

## Status

Ready to implement. This document is scoped to commit signing only.

[gitprovider-commits-api.md](gitprovider-commits-api.md) is the source of truth for the enclosing
API shape. Signing lives at `spec.commit.signing`, and committer identity lives at
`spec.commit.committer`.

---

## Problem

All automated commits created by gitops-reverser are unsigned. In regulated environments, or in
repositories that require signed commits, this blocks adoption. The operator needs to produce signed
commits so GitHub, GitLab, and Gitea can verify commit provenance.

---

## Current state

`internal/git/git.go` calls `worktree.Commit(...)` in two paths:

- Atomic batch commits
- Per-event commits

Both currently set `Author` and `Committer` only. `CommitOptions.SignKey` and `CommitOptions.Signer`
are unset, so every commit is unsigned.

The current committer email is also hardcoded to `noreply@configbutler.ai`. That matters because
platform verification is tied to the committer identity, which is configured separately in
`spec.commit.committer`.

`go-git` already provides the required extension point:

```go
type Signer interface {
    Sign(message io.Reader) ([]byte, error)
}
```

This lets us add SSH signing without changing the surrounding write flow.

---

## Recommendation

Start with SSH signing using `ed25519`.

Why:

1. GitHub, GitLab, and Gitea all support SSH commit signing.
2. SSH keys are easier for users and service accounts to manage than GPG keys.
3. Key generation and parsing fit the current Go dependency set cleanly.
4. The same key material can often be reused for both push auth and signing.

GPG should remain a follow-on option, but it should not shape the initial interface.

---

## What the user configures

From the user's perspective, commit signing is configured in two places under `spec.commit`:

```yaml
spec:
  commit:
    committer:
      name: "GitOps Reverser"
      email: "12345678+gitops-reverser-bot@users.noreply.github.com"
    signing:
      generateWhenMissing: true
      secretRef:
        name: gitops-reverser-signing-key
```

These two blocks solve different problems:

- `commit.signing` tells the operator which key to use, or whether to generate one
- `commit.committer` tells the git platform which account identity this commit claims to come from

The committer email requirement is critical:

- `spec.commit.committer.email` must be a verified email address on the account that owns the
  signing key
- if the key is valid but the committer email does not match that account, the commit may still be
  signed but will show as unverified on the git platform

This is why the email field is not just cosmetic metadata. It is part of whether the platform will
accept the signature as belonging to the expected account.

---

## Interface design

### API location

`CommitSigningSpec` is nested under `spec.commit.signing`.

```go
type CommitSigningSpec struct {
    // SecretRef references the Secret containing the signing key.
    // Expected keys: "signing.key" (OpenSSH private key PEM), optionally "passphrase".
    SecretRef LocalSecretReference `json:"secretRef"`

    // GenerateWhenMissing, if true, causes the operator to generate and persist
    // an ed25519 SSH signing key in the referenced Secret if one is not already present.
    // +optional
    // +kubebuilder:default=false
    GenerateWhenMissing bool `json:"generateWhenMissing,omitempty"`
}
```

`GitProviderStatus` also gets a public-key status field:

```go
type GitProviderStatus struct {
    // SigningPublicKey is the SSH public key in authorized_keys format for the
    // configured signing key. Register this as a signing key on the git platform.
    // +optional
    SigningPublicKey string `json:"signingPublicKey,omitempty"`
}
```

This document intentionally does not redefine `CommitSpec`, `CommitterSpec`, or the message
template API. Those belong to [gitprovider-commits-api.md](gitprovider-commits-api.md).

### Committer identity dependency

Commit verification depends on both:

1. A valid signature from the private key in `spec.commit.signing.secretRef`
2. A committer identity in `spec.commit.committer` that matches the account owning that key

That coupling is operationally important, but the fields still belong in separate sub-blocks:

- `spec.commit.signing` owns key material and generation behavior
- `spec.commit.committer` owns name and email

This keeps the interface cohesive while preserving the distinction between "how we sign" and "who
the platform thinks signed the commit".

### Secret contract

The signing Secret contract should be:

- `signing.key`: OpenSSH private key PEM
- `passphrase`: optional passphrase for encrypted private keys

When the operator generates a key, it may also write `signing.pub` for convenience, but the stable
interface for surfacing the public key is `.status.signingPublicKey`.

### Auto-generation and status behavior

If `generateWhenMissing: true` is set, the controller should generate an `ed25519` SSH key when the
referenced Secret does not yet contain `signing.key`.

Two status rules matter:

1. `.status.signingPublicKey` should be populated whenever signing is configured and a private key is
   available, regardless of whether that key was generated by the operator or supplied by the user.
2. Any "register this key on the git platform" condition should be informational only unless the
   operator has platform-specific proof. A successful signed push does not prove the key is
   registered or that the committer email is verified.

---

## Deferred: remote key registration validation

Remote key-registration checks are intentionally out of scope for the first implementation. That
idea now lives in [idea-commit-signing-key-validation.md](../future/idea-commit-signing-key-validation.md).

The main reason to defer it is that the current API does not ask for a platform account identifier
such as a GitHub username or user ID. We only ask for committer name and email in
`spec.commit.committer`, which is not enough to reliably determine which remote account should be
queried. Even on platforms that expose useful APIs, the operator would need more identity data and
usually additional credentials.

---

## Implementation plan

This section is intentionally more specific than the interface section. It is here to help the
implementing agent carry the design through the existing codebase.

### Step 1: API and CRD wiring

Files:

- `api/v1alpha1/gitprovider_types.go`
- `api/v1alpha1/shared_types.go` if the commit block types are split there
- `config/crd/bases/configbutler.ai_gitproviders.yaml`

Work:

1. Implement the `spec.commit.signing` shape from
   [gitprovider-commits-api.md](gitprovider-commits-api.md).
2. Add `CommitSigningSpec` with:
   - `secretRef`
   - `generateWhenMissing`
3. Add `status.signingPublicKey` to `GitProviderStatus`.
4. Run `make generate` and `make manifests` once the API code is in place.

Important constraint:

- Do not reintroduce committer name or email into `CommitSigningSpec`. Those belong in
  `spec.commit.committer`.

### Step 2: SSH signer implementation

New file:

- `internal/git/signing.go`

Suggested contents:

- `sshCommitSigner` implementing `git.Signer`
- `loadSSHSigner(secret *corev1.Secret) (ssh.Signer, error)`
- `sshPublicKeyFromPrivate(privPEM []byte) (ssh.PublicKey, error)`
- small helpers for building the SSH signature payload/blob

Implementation notes:

- Use `golang.org/x/crypto/ssh`.
- Read `signing.key` and optional `passphrase` from the Secret.
- Keep the sshsig encoding details in code comments and tests rather than in the API section of this
  design doc.

### Step 3: Controller-managed key lifecycle

File:

- `internal/controller/gitprovider_controller.go`

Add an early reconcile step, for example `ensureSigningKey(ctx, provider)`, with this behavior:

1. Return early when `spec.commit == nil` or `spec.commit.signing == nil`.
2. Fetch the referenced Secret.
3. If `signing.key` already exists:
   - derive the public key
   - populate `status.signingPublicKey`
   - do not generate a new key
4. If the key is missing and `generateWhenMissing` is false:
   - leave the Secret alone
   - surface a clear status/error path
5. If the key is missing and `generateWhenMissing` is true:
   - generate an `ed25519` keypair
   - serialize the private key in OpenSSH PEM form
   - write `signing.key`
   - optionally write `signing.pub`
   - populate `status.signingPublicKey`

Important behavior:

- `status.signingPublicKey` should be set for both generated keys and pre-provisioned keys.
- If you add a registration-related condition, keep it informational. Do not clear it based only on
  a successful push.

### Step 4: BranchWorker signer loading

Files:

- `internal/git/branch_worker.go`
- optionally `internal/git/signing_loader.go`

Add a helper similar to `getAuthFromSecret`:

```go
func getSSHSigner(ctx context.Context, c client.Client, provider *configv1alpha1.GitProvider) (git.Signer, error)
```

Behavior:

- Return `nil, nil` when signing is not configured.
- Load the Secret from `spec.commit.signing.secretRef`.
- Parse the private key into an SSH signer.
- Load on each write cycle instead of caching indefinitely, so Secret rotation is naturally picked
  up.

### Step 5: Commit request plumbing

Files:

- `internal/git/types.go`
- `internal/git/branch_worker.go`
- `internal/git/git.go`

Extend `WriteRequest` with the fields needed by the commit paths:

```go
Signer git.Signer
CommitterName  string
CommitterEmail string
```

Then:

1. In `commitAndPushRequest`, resolve committer identity from `spec.commit.committer`.
2. Set the signer and committer fields on the prepared request.
3. In both commit paths, pass `Signer: request.Signer` into `git.CommitOptions`.
4. Replace the hardcoded committer identity with a helper that uses request overrides and falls back
   to today's defaults.

### Step 6: Commit-path coverage

The two current commit paths both need signing support:

- atomic batch commits
- per-event commits

That means the implementation should touch both of the current `worktree.Commit(...)` call sites in
`internal/git/git.go`, not just the per-event path.

### Step 7: Tests

Suggested test additions:

- `internal/git/signing_test.go`
  - signer emits an SSH signature header
  - malformed private key returns a clear error
  - passphrase-protected key path works
- `internal/git/branch_worker_test.go`
  - signed commit path populates `PGPSignature`
  - signing Secret load failure skips the write in the same style as auth failure
- `internal/controller/gitprovider_controller_test.go`
  - generated key is written when `generateWhenMissing: true`
  - existing key is reused
  - `status.signingPublicKey` is populated for an existing key as well as a generated key

### Step 8: User-facing docs

Follow-up docs should cover:

1. Bring-your-own SSH signing key Secret format
2. Auto-generation flow with `generateWhenMissing: true`
3. The requirement that `spec.commit.committer.email` must match the account that owns the signing
   key
4. How to copy `.status.signingPublicKey` and register it on the git platform

---

## Key risks

| Risk | Mitigation |
|---|---|
| Committer email is not verified on the platform account | Keep the requirement in `spec.commit.committer` documentation and do not imply that signing alone is sufficient. |
| Key registration is incomplete but pushes still succeed | Treat registration-related conditions as informational unless backed by platform API checks. |
| Secret is updated while the worker is running | Load signing material per write cycle rather than caching indefinitely. |
| `generateWhenMissing: true` is used without Secret write permissions | Document the RBAC requirement and surface reconciliation errors clearly. |
| GPG support is needed later | Extend `CommitSigningSpec` with a format selector in a backward-compatible follow-on. |

---

## Follow-on: GPG support

If GPG support is added later, extend `CommitSigningSpec` with a format selector:

```go
// SigningFormat selects the signing algorithm. Default: ssh.
// +optional
// +kubebuilder:validation:Enum=ssh;gpg
// +kubebuilder:default=ssh
SigningFormat string `json:"signingFormat,omitempty"`
```

That keeps the initial interface simple while leaving a clean path for future expansion.
