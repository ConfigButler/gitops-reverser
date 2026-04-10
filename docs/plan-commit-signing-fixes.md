# Plan: Commit Signing — Bug Fixes & E2E Test Coverage

## Background

The `feat: commit signing` commit introduced SSH commit signing end-to-end: key generation,
controller-managed secrets, signer loading, and per-commit signing via go-git's `Signer`
interface. The architecture is clean and test coverage at the unit/integration level is solid.

This plan addresses one critical bug, two medium-weight code quality issues, and designs the
e2e test strategy for verifying that signatures are actually valid according to real git
tooling.

---

## Issue 1 — Critical: Wrong signed-data format breaks external verification

**File:** `internal/git/signing.go`, `Sign()` method (~line 1141)

### Problem

The SSHSIG specification (OpenSSH `PROTOCOL.sshsig`) defines the blob that gets cryptographically
signed as:

```
"SSHSIG"            ← 6 raw bytes, NOT length-prefixed
uint32(1)           ← version
string(namespace)   ← SSH wire-format string (uint32 len + bytes)
string(reserved)
string(hash_algorithm)
string(hash)
```

The current implementation constructs this blob via:

```go
toSign := ssh.Marshal(sshsigSignedData{
    Magic:         []byte(sshSignatureMagic),
    ...
})
```

`ssh.Marshal` encodes every `[]byte` field as an SSH wire-format string: `uint32(len) + bytes`.
So `Magic` becomes `uint32(6) + "SSHSIG"` — not the required raw 6-byte prefix.

This means every produced signature is cryptographically valid against the *wrong* data.
Internal unit tests pass because the test reconstructs `toVerify` using the same (incorrect)
`ssh.Marshal` call. But `git verify-commit` and `ssh-keygen -Y verify` both implement the spec
correctly and will reject every signature this code produces.

### Fix

Replace the `ssh.Marshal` approach in `Sign()` with manual binary construction that matches
the spec:

```go
func (s *sshCommitSigner) Sign(message io.Reader) ([]byte, error) {
    payload, err := io.ReadAll(message)
    if err != nil {
        return nil, fmt.Errorf("read commit payload: %w", err)
    }

    digest := sha512.Sum512(payload)

    // Build the signed data blob per PROTOCOL.sshsig:
    //   "SSHSIG" (raw) || uint32(version) || string(ns) || string(reserved)
    //   || string(hash_alg) || string(hash)
    var sigData bytes.Buffer
    sigData.WriteString(sshSignatureMagic)                                   // raw 6 bytes
    _ = binary.Write(&sigData, binary.BigEndian, sshSignatureVersion)        // uint32(1)
    _ = writeSSHPacketString(&sigData, []byte(sshSignatureNamespace))
    _ = writeSSHPacketString(&sigData, nil)                                  // reserved
    _ = writeSSHPacketString(&sigData, []byte(sshSignatureHashAlg))
    _ = writeSSHPacketString(&sigData, digest[:])

    signature, err := signSSHMessage(s.signer, sigData.Bytes())
    ...
}
```

The unit test `TestLoadSSHCommitSigner_ProducesVerifiableSSHSig` must also be updated to
reconstruct `toVerify` using the same corrected binary layout so that it validates against the
actual SSHSIG spec.

---

## Issue 2 — Medium: Duplicated secret key constants

**Files:** `internal/git/signing.go`, `internal/controller/gitprovider_signing.go`

### Problem

`signing.go` defines private constants:

```go
const (
    signingKeyDataKey        = "signing.key"
    signingPublicKeyDataKey  = "signing.pub"
    signingPassphraseDataKey = "passphrase"
)
```

Because they are unexported, the controller package cannot reference them and instead
defines zero-argument wrapper functions that return the same string literals:

```go
func gitpkgSigningKeyDataKey() string        { return "signing.key" }
func gitpkgSigningPublicKeyDataKey() string  { return "signing.pub" }
func gitpkgSigningPassphraseDataKey() string { return "passphrase" }
```

This is fragile: the strings can drift between packages silently.

### Fix

Export the constants from `internal/git/signing.go`:

```go
const (
    SigningKeyDataKey        = "signing.key"
    SigningPublicKeyDataKey  = "signing.pub"
    SigningPassphraseDataKey = "passphrase"
)
```

Remove the three wrapper functions from the controller and reference `gitpkg.SigningKeyDataKey`
etc. directly. The private aliases can remain for internal use if preferred, pointing at the
exported ones.

---

## Issue 3 — Minor: Generated secrets have no owner reference

**File:** `internal/controller/gitprovider_signing.go`, `createGeneratedSigningSecret()`

### Problem

When `generateWhenMissing: true` causes the controller to create a new signing secret, that
secret has no owner reference to the `GitProvider`. Deleting the `GitProvider` will not garbage-
collect the secret.

### Decision required

This may be intentional: key material is valuable and auto-deletion could be dangerous.
Either outcome is acceptable, but it should be an explicit decision rather than an omission.

**Option A — Keep as-is, document it:** Add a comment in `createGeneratedSigningSecret`
explaining that the secret is intentionally not owned by the GitProvider to prevent accidental
key loss on CR deletion.

**Option B — Add owner reference:** Set `metav1.OwnerReference` on the created secret so
Kubernetes GC handles cleanup automatically.

Recommendation: **Option A** for now. Signing keys need careful lifecycle management and
"delete the CR → lose your signing key" is a surprising footgun.

---

## Issue 4 — Minor: Indirect recursion on secret create-race

**File:** `internal/controller/gitprovider_signing.go`, line ~488

On an `AlreadyExists` error from `r.Create`, the code recurses into the full `ensureSigningKey`
flow. This is functionally correct but opaque. A direct `r.Get` + `loadSSHSigner` would be
clearer and remove the re-entrant path.

This is low priority and can be addressed in a follow-up cleanup.

---

## E2E Test Strategy for Commit Signing

### Philosophy

Unit and integration tests already cover:
- Signing key generation and loading
- Correct signature structure (PEM block present, correct type)
- BranchWorker skipping writes on invalid signing keys
- Controller-level key provisioning flows

The gap is **external verification**: does `git` itself consider the commits signed and valid?
That requires running real git tooling against commits in the Gitea instance.

### Approach

Use the existing Gitea-backed e2e infrastructure. The test will:
1. Create a signing secret with `generateWhenMissing: true` in a fresh test namespace.
2. Create a `GitProvider` pointing at the Gitea repo with `commit.signing` configured.
3. Trigger a resource write through a `WatchRule` → commit lands in Gitea.
4. Read the `SigningPublicKey` from the `GitProvider` status.
5. Clone the repo locally (or `git fetch` into the existing checkout) and run verification.

### Verification commands

**Using `ssh-keygen -Y verify`** (most portable, no git config needed):

```bash
# Write the allowed-signers file
echo "gitops-reverser@cluster.local namespaces=\"git\" ${SIGNING_PUBLIC_KEY}" \
  > /tmp/allowed-signers

# Extract the raw commit object
git cat-file commit HEAD > /tmp/commit-payload

# Extract the signature from the commit (PGPSignature field)
git cat-file commit HEAD | \
  awk '/^gpgsig /,/^ -----END SSH SIGNATURE-----/' | \
  sed 's/^ //' | sed 's/^gpgsig //' > /tmp/commit.sig

# Verify
ssh-keygen -Y verify \
  -f /tmp/allowed-signers \
  -I "gitops-reverser@cluster.local" \
  -n git \
  -s /tmp/commit.sig < /tmp/commit-payload
```

**Using `git verify-commit`** (simpler, needs config):

```bash
git config gpg.format ssh
git config gpg.ssh.allowedSignersFile /tmp/allowed-signers
git verify-commit HEAD
```

### New test file

Add `test/e2e/signing_e2e_test.go` as a new `Describe` block following the patterns in
`e2e_test.go`. It should live in its own file to keep the concern isolated.

#### Test structure

```go
var _ = Describe("Commit Signing", Ordered, func() {
    var testNs string

    BeforeAll(func() {
        testNs = testNamespaceFor("signing")
        // create namespace, apply git secrets (same pattern as Manager suite)
    })

    AfterAll(func() {
        cleanupNamespace(testNs)
    })

    It("should produce commits verifiable by ssh-keygen", func() {
        By("creating a GitProvider with signing enabled (generateWhenMissing)")
        // Apply GitProvider manifest with commit.signing block

        By("waiting for SigningPublicKey to appear in status")
        // Eventually: kubectl get gitprovider ... -o jsonpath=.status.signingPublicKey

        By("triggering a commit via a WatchRule")
        // Apply a WatchRule + observed resource, wait for commit

        By("fetching the latest commit from checkout")
        // git -C checkoutDir pull

        By("verifying the commit signature with ssh-keygen")
        // Write allowed-signers file using status.signingPublicKey
        // Run ssh-keygen -Y verify (exec.Command)
        // Assert exit code 0 and output contains "Good"
    })

    It("should leave unsigned commits when signing is not configured", func() {
        // Existing behaviour guard: commits from a non-signing GitProvider
        // must NOT have a PGPSignature field
        // git cat-file commit HEAD | grep -v "^gpgsig"
    })
})
```

#### Helper needed in `helpers.go`

```go
// gitRun runs a git command in the given directory and returns combined output.
func gitRun(dir string, args ...string) (string, error) {
    cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
    out, err := cmd.CombinedOutput()
    return string(out), err
}
```

#### Gitprovider template addition

Add a variant template `test/e2e/templates/gitprovider-signing.tmpl` (or extend the existing
one with an optional `Signing` block) so the test can apply a signed `GitProvider` without
duplicating YAML inline.

### Key constraint

`ssh-keygen` and `git` must be available in the e2e runner environment. They are standard in
the Kind node images and in typical CI containers. No additional tooling needs to be installed.

---

## Summary of work items

| # | Priority | Item | File(s) |
|---|----------|------|---------|
| 1 | Critical | Fix signed-data blob format in `Sign()` to match SSHSIG spec | `internal/git/signing.go` |
| 1a | | Update unit test `TestLoadSSHCommitSigner_ProducesVerifiableSSHSig` to verify against spec-correct format | `internal/git/signing_test.go` |
| 2 | Medium | Export signing key constants; remove controller wrapper functions | `internal/git/signing.go`, `internal/controller/gitprovider_signing.go` |
| 3 | Minor | Document intentional absence of owner reference on generated secrets | `internal/controller/gitprovider_signing.go` |
| 4 | Low | Replace recursive `ensureSigningKey` call on `AlreadyExists` with a direct `Get` | `internal/controller/gitprovider_signing.go` |
| 5 | New | Add `test/e2e/signing_e2e_test.go` with `ssh-keygen -Y verify` assertion | `test/e2e/` |
| 5a | | Add `gitRun` helper to `test/e2e/helpers.go` | `test/e2e/helpers.go` |
| 5b | | Add `gitprovider-signing.tmpl` template | `test/e2e/templates/` |

Items 1 and 1a are blockers — the e2e test (item 5) will fail until the signed-data format is
correct.
