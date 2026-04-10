# Commit Signing Design

## Status

Ready to implement. No prior design work exists. This document covers signing only.

Committer identity and commit message configuration are covered in
[gitprovider-commits-api.md](gitprovider-commits-api.md), which was written after this doc and
supersedes the committer identity section that was previously here. The `CommitSigningSpec` struct
described here lives at `spec.commits.signing` in the final API.

---

## Problem

All automated commits created by gitops-reverser are unsigned. In regulated environments or repos
protected by branch policies that require signed commits, this blocks adoption. The operator needs to
produce signed commits so that GitHub/GitLab/Gitea branch protection rules and audit trails can
verify commit provenance.

---

## Current state

`git.go` calls `worktree.Commit(...)` at two call sites:

- `generateAtomicBatchCommit` — [git.go:827](../../internal/git/git.go#L827)
- `createCommitForEvent` — [git.go:1051](../../internal/git/git.go#L1051)

Both pass a bare `CommitOptions` with `Author`/`Committer` only; `SignKey` and `Signer` are nil.
The `Committer.Email` is also hardcoded to `noreply@configbutler.ai` — this matters for signing,
see [Committer identity and email](#committer-identity-and-email) below.

go-git v5.17.2 exposes two signing hooks in `CommitOptions`:

| Field | Type | Notes |
|---|---|---|
| `SignKey` | `*openpgp.Entity` | GPG/OpenPGP private key entity |
| `Signer` | `git.Signer` interface | Any signer; takes precedence over `SignKey` |

`git.Signer` is a one-method interface:

```go
type Signer interface {
    Sign(message io.Reader) ([]byte, error)
}
```

go-git recognises the returned bytes as GPG or SSH based on their header
(`-----BEGIN PGP SIGNATURE-----` vs `-----BEGIN SSH SIGNATURE-----`).

---

## GPG vs SSH: recommendation

**Start with SSH (ed25519).** This decision is driven by the end user, not by implementation
convenience.

### Platform support

All three target platforms have supported SSH commit signing for years:

| Platform | SSH signing available since | GPG signing |
|---|---|---|
| GitHub | June 2022 | Long-standing |
| GitLab | 15.7 (December 2022) | Long-standing |
| Gitea | 1.19 (March 2023) | Long-standing |

### Why SSH wins for end users

1. **Familiar tooling.** Every user who pushes to a Git remote already owns an SSH key and knows
   how to generate one (`ssh-keygen -t ed25519`). GPG requires a separate keyring, an understanding
   of the web of trust, and exporting keys in the right format.

2. **No new Go dependencies.** `golang.org/x/crypto/ssh` is already in `go.mod`. The SSH signature
   wire format (sshsig, ~60 lines of Go) is implementable inline. GPG would also work
   (`github.com/ProtonMail/go-crypto` is already an indirect dependency via go-git), but SSH is
   the simpler path _and_ the better UX.

3. **Auto-generation is trivial.** `crypto/ed25519` is in the stdlib. Key generation, OpenSSH
   private key serialisation, and public key fingerprinting all require no extra packages.

4. **One key, two uses.** On every platform the same SSH key can be registered for both push
   authentication and commit signing. Users who already use SSH push auth can reuse that key.

5. **Operator-native fit.** An operator that manages Git commits on behalf of a service account
   benefits from a key it generates and owns. GPG identity management (UID emails, expiry, subkeys)
   is overhead that adds no security value in this context.

**GPG** should be supported as a follow-on for organisations that already have GPG infrastructure
or whose branch policies mandate it. The API is designed to accommodate it from day one.

---

## Platform first-time setup

### What happens when a commit is signed

1. gitops-reverser generates a commit with a signature block appended.
2. The platform verifies the signature against the public key registered on the **committer's
   account**.
3. If the key matches and the committer email is verified on that account, the commit gets a
   **Verified** badge. Mismatched email = **Unverified**, even if the key is registered.

### GitHub

#### Registering an SSH signing key

1. Go to **Settings → SSH and GPG keys → New SSH key**.
2. Set **Key type = Signing Key** (distinct from an authentication key, though the same key bytes
   can be registered for both purposes separately).
3. Paste the public key (`ssh-ed25519 AAAA... comment`).

#### Email requirement

GitHub checks that the commit's `Committer.Email` is a **verified email** on the account that
owns the signing key. Options for a bot/service account:

- Create a dedicated GitHub user (e.g. `gitops-reverser-bot`). GitHub generates a noreply address
  in the form `{userID}+{username}@users.noreply.github.com` — this is automatically verified.
  Use that address as the committer email.
- Enable "Block command line pushes that expose my email address" on the bot account and use the
  provided noreply address.

#### Branch protection

Under **Branch protection rules → Require signed commits**, GitHub will reject unsigned pushes.
This works for both GPG and SSH signing keys registered on the account.

#### Vigilance mode

If "Vigilance mode" is enabled on any viewer's account, they will see unsigned commits from
gitops-reverser marked as **Unverified** until signing is enabled.

---

### GitLab

#### Registering an SSH signing key

1. Go to **Profile → SSH Keys → Add new key**.
2. In the **Usage type** dropdown, select **Signing** (or **Authentication & Signing** if you want
   the same key to auth pushes too).
3. Paste the public key.

#### Email requirement

GitLab checks the committer email against verified emails on the signing account. GitLab
(self-hosted and .com) provides a noreply address: `{username}@noreply.gitlab.com` (or
`{username}@noreply.{self-hosted-domain}`). This address is verified automatically.

For a dedicated service account/bot user, use that noreply address as the committer email.

#### Push rules (Premium/Ultimate)

Under **Repository → Push rules → Reject unsigned commits**, GitLab will block unsigned pushes.
SSH and GPG are both accepted.

#### Self-hosted note

On self-hosted GitLab, the admin controls which email domains are considered internal. The email
matching requirement is the same, but the noreply pattern uses the instance's configured
external URL.

---

### Gitea

#### Registering an SSH signing key

1. Go to **Settings → SSH / GPG Keys → Add Key**.
2. Gitea does not separate auth and signing key types — the same SSH key covers both. Add the
   public key once.

#### Email requirement

Gitea verifies the commit signature against keys on the committer's account and checks the
committer email against that user's registered emails. Gitea is more permissive by default: the
admin can configure `REQUIRE_SIGNIN_VIEW = false` and email verification may be optional depending
on instance config. For a service account, add the desired committer email to the account in
**Settings → Account → Add Email Address**.

The noreply pattern on Gitea follows the instance's `NO_REPLY_ADDRESS` config key (defaults to
`noreply.{DOMAIN}`).

#### Push protection

Gitea does not have a native "require signed commits" branch rule as of 1.21. Enforcement is
typically done via pre-receive hooks on self-hosted instances.

---

## Committer identity and email

This is the most operationally important detail for getting a "Verified" badge.

### Current hardcoded values (must become configurable)

```go
// internal/git/git.go
Committer: &object.Signature{
    Name:  "GitOps Reverser",
    Email: "noreply@configbutler.ai",  // ← not verified on any user account
}
```

This email is not registered on any GitHub/GitLab/Gitea account. Even with a perfectly valid
signing key, the commit will show **Unverified** on all platforms if this email is left as-is.

### What needs to change

`CommitSigningSpec` must include the committer identity that matches the signing account:

```go
type CommitSigningSpec struct {
    SecretRef      LocalSecretReference `json:"secretRef"`

    // CommitterName is the display name used for the committer signature.
    // Should match the display name of the account that owns the signing key.
    // +optional
    CommitterName string `json:"committerName,omitempty"`

    // CommitterEmail must be a verified email on the account that owns the signing key.
    // On GitHub: use the {userID}+{username}@users.noreply.github.com address.
    // On GitLab: use {username}@noreply.gitlab.com.
    // On Gitea:  use the account's registered email or its configured noreply address.
    // +optional
    CommitterEmail string `json:"committerEmail,omitempty"`
}
```

When `CommitterName`/`CommitterEmail` are set, use them for `CommitOptions.Committer`.
When absent, fall back to the current hardcoded values (preserving backwards compatibility).

The `Author` field (who made the change in the cluster) stays as the Kubernetes username — this
is the audit trail. Only the `Committer` is the signing identity.

---

## Auto-generation of keys

### Should we auto-generate?

**Yes.** The biggest friction point for adopting commit signing in an operator context is the
manual key ceremony. An operator that can generate its own key, store it in a Secret, and surface
the public key for registration turns a 10-step setup into a 3-step one.

### User experience goal

```yaml
# GitProvider spec — user only needs to specify this:
commitSigning:
  generateWhenMissing: true
  secretRef:
    name: gitops-reverser-signing-key
  committerName: "GitOps Reverser"
  committerEmail: "12345678+gitops-reverser-bot@users.noreply.github.com"
```

After the GitProvider reconciles, `kubectl describe gitprovider my-provider` shows:

```
Status:
  Signing Public Key: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA... gitops-reverser@gitops-system
  Conditions:
    - Type: Ready
      Status: False
      Reason: SigningKeyPendingRegistration
      Message: >
        Signing key generated. Register the public key on your git platform before
        commits can be verified. See .status.signingPublicKey.
```

Once the user registers the public key, the next commit will be verified.

### Implementation sketch

**New `CommitSigningSpec` field:**

```go
// GenerateWhenMissing, if true, causes the operator to generate and persist an SSH ed25519
// signing key in the referenced Secret when no key is present.
// +optional
// +kubebuilder:default=false
GenerateWhenMissing bool `json:"generateWhenMissing,omitempty"`
```

**New `GitProviderStatus` field:**

```go
// SigningPublicKey is the SSH public key (authorized_keys format) for the
// generated or configured signing key. Register this as a Signing Key on your
// git platform. Only populated when commitSigning is configured.
// +optional
SigningPublicKey string `json:"signingPublicKey,omitempty"`
```

**Generation logic** (in `gitprovider_controller.go`, during reconcile):

```go
func (r *GitProviderReconciler) ensureSigningKey(
    ctx context.Context,
    provider *v1alpha1.GitProvider,
) error {
    spec := provider.Spec.CommitSigning
    if spec == nil || !spec.GenerateWhenMissing {
        return nil
    }
    secret, _ := r.fetchSecret(ctx, spec.SecretRef.Name, provider.Namespace)
    if secret != nil && len(secret.Data["signing.key"]) > 0 {
        // Key already exists — surface public key in status, nothing to generate.
        pubKey, _ := sshPublicKeyFromPrivate(secret.Data["signing.key"])
        provider.Status.SigningPublicKey = string(ssh.MarshalAuthorizedKey(pubKey))
        return nil
    }
    // Generate ed25519 key pair.
    pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
    // Serialise private key to OpenSSH PEM format.
    privPEM, err := ssh.MarshalPrivateKey(privKey, "gitops-reverser")
    // Store in Secret.
    r.createOrUpdateSecret(ctx, spec.SecretRef.Name, provider.Namespace, map[string][]byte{
        "signing.key": pem.EncodeToMemory(privPEM),
        "signing.pub": ssh.MarshalAuthorizedKey(mustNewSSHPublicKey(pubKey)),
    })
    provider.Status.SigningPublicKey = string(ssh.MarshalAuthorizedKey(mustNewSSHPublicKey(pubKey)))
    return nil
}
```

Key generation uses only stdlib (`crypto/ed25519`, `crypto/rand`) and `golang.org/x/crypto/ssh`
(already in `go.mod`). Zero new dependencies.

---

## SSH signing implementation

### The sshsig wire format

Git SSH signatures follow OpenSSH's `sshsig` protocol. The signature blob written to the commit is:

```
-----BEGIN SSH SIGNATURE-----
<base64 of the sshsig wire format>
-----END SSH SIGNATURE-----
```

The signed payload ("signed data") that goes into the SSH signature algorithm is:

```
"SSHSIG"          (6 bytes, magic preamble)
uint32(1)         (version)
string(namespace) ("git")
string("")        (reserved, empty)
string("sha512")  (hash algorithm)
string(H(msg))    (SHA-512 of the message bytes)
```

The outer sshsig blob sent to the platform is:

```
"SSHSIG"
uint32(1)
string(publicKey wire bytes)
string(namespace)
string("")          (reserved)
string("sha512")
string(signature)   (the ssh.Signature wire-encoded blob)
```

All strings are length-prefixed (4-byte big-endian length followed by bytes) — the same encoding
`golang.org/x/crypto/ssh` uses internally.

### Implementing `git.Signer`

```go
// sshCommitSigner implements git.Signer for SSH commit signing.
type sshCommitSigner struct {
    signer ssh.Signer
}

func (s sshCommitSigner) Sign(message io.Reader) ([]byte, error) {
    msg, err := io.ReadAll(message)
    if err != nil {
        return nil, err
    }
    // Hash the message.
    h := sha512.Sum512(msg)
    // Build the signed data blob.
    signed := buildSSHSigPayload("git", "sha512", h[:])
    // Sign with the SSH key.
    sig, err := s.signer.Sign(rand.Reader, signed)
    if err != nil {
        return nil, fmt.Errorf("ssh sign: %w", err)
    }
    // Build the final sshsig blob and armor it.
    blob := buildSSHSigBlob(s.signer.PublicKey(), "git", "sha512", h[:], sig)
    var buf bytes.Buffer
    b64w := base64.NewEncoder(base64.StdEncoding, &buf)
    b64w.Write(blob)
    b64w.Close()
    // Wrap in PEM-style header.
    var out bytes.Buffer
    out.WriteString("-----BEGIN SSH SIGNATURE-----\n")
    // Insert line breaks every 76 chars (openssh default).
    writeWrapped(&out, buf.Bytes(), 76)
    out.WriteString("-----END SSH SIGNATURE-----\n")
    return out.Bytes(), nil
}
```

`buildSSHSigPayload` and `buildSSHSigBlob` are ~40 lines of straightforward length-prefixed
encoding. No library needed.

### Loading an SSH key from a Secret

Secret key: `signing.key` — OpenSSH PEM private key (the format written by `ssh-keygen -t ed25519`
and by Go's `x/crypto/ssh.MarshalPrivateKey`).

```go
func loadSSHSigner(secret *corev1.Secret) (ssh.Signer, error) {
    privPEM, ok := secret.Data["signing.key"]
    if !ok {
        return nil, errors.New("secret missing 'signing.key'")
    }
    passphrase := secret.Data["passphrase"] // nil = unencrypted
    var signer ssh.Signer
    var err error
    if len(passphrase) > 0 {
        signer, err = ssh.ParsePrivateKeyWithPassphrase(privPEM, passphrase)
    } else {
        signer, err = ssh.ParsePrivateKey(privPEM)
    }
    return signer, err
}
```

---

## API design (revised)

### `CommitSigningSpec`

`CommitSigningSpec` is embedded at `spec.commits.signing` (see
[gitprovider-commits-api.md](gitprovider-commits-api.md) for the full `commits` block). Committer
name and email live in `spec.commits.committer`, not here.

```go
// CommitSigningSpec configures how automated commits are signed.
type CommitSigningSpec struct {
    // SecretRef references the Secret containing the signing key.
    // Expected keys: "signing.key" (OpenSSH private key PEM), optionally "passphrase".
    // +required
    SecretRef LocalSecretReference `json:"secretRef"`

    // Generate, if true, causes the operator to generate and persist an ed25519 SSH
    // signing key in the referenced Secret if one is not already present.
    // +optional
    // +kubebuilder:default=false
    GenerateWhenMissing bool `json:"generateWhenMissing,omitempty"`
}
```

Add to `GitProviderStatus`:

```go
// SigningPublicKey is the SSH public key (authorized_keys format) for the signing key.
// Register this as a "Signing Key" on your git platform.
// Only populated when commitSigning is configured.
// +optional
SigningPublicKey string `json:"signingPublicKey,omitempty"`
```

---

## Implementation steps

### Step 1 — API (CRD)

File: [api/v1alpha1/gitprovider_types.go](../../api/v1alpha1/gitprovider_types.go)

- Add `CommitSigningSpec` struct with fields above.
- Add `CommitSigning *CommitSigningSpec` to `GitProviderSpec`.
- Add `SigningPublicKey string` to `GitProviderStatus`.
- Run `make generate manifests`.

### Step 2 — SSH signer implementation

New file: `internal/git/signing.go`

- `sshCommitSigner` struct implementing `git.Signer`.
- `buildSSHSigPayload(namespace, hashAlgo string, hash []byte) []byte`
- `buildSSHSigBlob(pubKey ssh.PublicKey, namespace, hashAlgo string, hash []byte, sig *ssh.Signature) []byte`
- `loadSSHSigner(secret *corev1.Secret) (ssh.Signer, error)` — parses `signing.key` + optional `passphrase`.
- `sshPublicKeyFromPrivate(privPEM []byte) (ssh.PublicKey, error)` — helper for surfacing the public key.

All using `golang.org/x/crypto/ssh` (already in `go.mod`). No new imports.

### Step 3 — Key generation (in controller)

File: `internal/controller/gitprovider_controller.go`

Add `ensureSigningKey(ctx, provider)` called early in `Reconcile`. Logic:

1. Return early if `CommitSigning == nil` or `GenerateWhenMissing == false`.
2. Fetch Secret. If `signing.key` already present: read public key, populate `Status.SigningPublicKey`, return.
3. Generate `ed25519.GenerateKey(rand.Reader)`.
4. Marshal private key with `ssh.MarshalPrivateKey`.
5. Create or update Secret with `signing.key` and `signing.pub`.
6. Set `Status.SigningPublicKey`.
7. Set condition `SigningKeyPendingRegistration = True` (remains True until first successful signed push, then removed or transitioned).

### Step 4 — `getSSHSigner` helper

File: `internal/git/branch_worker.go` (or a new `internal/git/signing_loader.go`)

```go
// getSSHSigner returns a git.Signer if CommitSigning is configured, or nil otherwise.
func getSSHSigner(ctx context.Context, c client.Client, provider *configv1alpha1.GitProvider) (git.Signer, error)
```

Follows the existing `getAuthFromSecret` pattern exactly.

### Step 5 — Wire into commit path

File: [internal/git/types.go](../../internal/git/types.go)

Add to `WriteRequest`:

```go
// Signer, if non-nil, signs every commit in this request.
Signer git.Signer
// CommitterName and CommitterEmail come from spec.commits.committer and are
// resolved by the BranchWorker before populating the WriteRequest.
CommitterName  string
CommitterEmail string
```

File: [internal/git/branch_worker.go](../../internal/git/branch_worker.go)

In `commitAndPushRequest`, after `getAuthFromSecret`:

```go
signer, err := getSSHSigner(w.ctx, w.Client, provider)
if err != nil {
    log.Error(err, "Failed to load signing key — skipping write")
    return
}
// Committer identity comes from spec.commits.committer (see gitprovider-commits-api.md).
committer := resolveCommitter(provider) // reads spec.commits.committer with defaults
preparedRequest.Signer         = signer
preparedRequest.CommitterName  = committer.Name
preparedRequest.CommitterEmail = committer.Email
```

File: [internal/git/git.go](../../internal/git/git.go)

At both `worktree.Commit` call sites:

```go
&git.CommitOptions{
    Author:    ...,
    Committer: committerSignature(request), // uses overrides when set
    Signer:    request.Signer,              // nil = unsigned
}
```

`Signer: nil` is a no-op in go-git — no conditional needed.

### Step 6 — Tests

- **Unit — sshsig format** (`internal/git/signing_test.go`):
  - Generate an in-memory ed25519 key.
  - Call `sshCommitSigner.Sign(bytes.NewReader(testPayload))`.
  - Assert the output starts with `-----BEGIN SSH SIGNATURE-----`.
  - Parse the output with `ssh.ParseAuthorizedKey` isn't applicable — verify the blob by
    decoding the base64 and checking the magic preamble.

- **Unit — key loading**:
  - Marshal a test key with `ssh.MarshalPrivateKey`, put it in a fake Secret, call `loadSSHSigner`.
  - Assert signer is non-nil and returns a signature.

- **Integration — signed commit** (`internal/git/branch_worker_test.go`):
  - Extend an existing `commitAndPushRequest` test: inject a signing key Secret, run the commit,
    fetch the commit object, assert `PGPSignature != ""` and starts with SSH header.

- **Negative**: malformed `signing.key` → clear error message, worker skips write (consistent with
  auth failure behavior).

- **Auto-generation** (`internal/controller/gitprovider_controller_test.go`):
  - Reconcile a GitProvider with `generateWhenMissing: true` and no pre-existing Secret.
  - Assert Secret is created with `signing.key` present.
  - Assert `Status.SigningPublicKey` is non-empty and starts with `ssh-ed25519`.

### Step 7 — Helm chart

`values.yaml`:

```yaml
# commitSigning:
#   generateWhenMissing: true
#   secretRef:
#     name: gitops-reverser-signing-key
#   committerName: "GitOps Reverser Bot"
#   committerEmail: "12345678+gitops-reverser-bot@users.noreply.github.com"
```

No Secret template needed when `generateWhenMissing: true` — the operator creates it. When not generating,
document the manual Secret format.

### Step 8 — User-facing docs

New or updated [docs/configuration.md](../../docs/configuration.md) section:

1. **Quick path** (`generateWhenMissing: true`): copy the two-field YAML, apply, copy public key from status,
   register on platform.
2. **Bring-your-own key**: `ssh-keygen -t ed25519 -C "gitops-reverser" -f signing`, create Secret,
   register public key.
3. **Per-platform email setup**: GitHub noreply pattern, GitLab noreply pattern, Gitea.
4. **Verification**: `git log --show-signature` after the first signed push.

---

## Key risks

| Risk | Mitigation |
|---|---|
| Committer email not verified on platform | Document clearly; surface in status condition. |
| Key rotation: Secret updated mid-push | Load per-commit cycle (not cached). Next push uses new key. |
| sshsig format divergence from OpenSSH | Validate against `ssh-keygen -Y verify` in a test. |
| `generateWhenMissing: true` + read-only RBAC | Controller needs create/update on Secrets in its namespace. Document in Helm RBAC values. |
| GPG not supported at launch | API field reserved (`SigningFormat` enum) for a clean follow-on. |

---

## Follow-on: GPG support

When adding GPG, extend `CommitSigningSpec`:

```go
// SigningFormat selects the signing algorithm. Default: ssh.
// +optional
// +kubebuilder:validation:Enum=ssh;gpg
// +kubebuilder:default=ssh
SigningFormat string `json:"signingFormat,omitempty"`
```

For GPG: Secret key `signing.asc` (ASCII-armored private key). Use
`github.com/ProtonMail/go-crypto/openpgp` (already an indirect dependency) to parse and sign.
Set `CommitOptions.SignKey` (takes lower precedence than `Signer`; set `Signer = nil` for GPG path).

GPG auto-generation via `openpgp.NewEntity(name, "", email, nil)` is also possible if demand
arises, but manual key management is more expected for GPG.
