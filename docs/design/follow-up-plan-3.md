# E2E Repo-Per-File Follow-Up Plan 3

## Status

**Partially complete — Gitea-side verification does not actually pass.**

Typed Gitea helpers landed in [test/e2e/gitea_api_test.go](/workspaces/gitops-reverser/test/e2e/gitea_api_test.go);
reusable signing assertions landed in [test/e2e/signing_common_test.go](/workspaces/gitops-reverser/test/e2e/signing_common_test.go);
[test/e2e/signing_e2e_test.go](/workspaces/gitops-reverser/test/e2e/signing_e2e_test.go) covers
generated-key and BYOK with local verification. Docs were aligned.

What is **not** working (2026-04-14):

- `assertGiteaVerified` had been weakened to only check that the commit API
  returns a verification block, with a comment claiming `Verified == true`
  was "not achievable" on Gitea 1.25.4. That comment papered over a real
  gap and was wrong relative to the plan's acceptance criteria.
- After tightening the assertion to `Expect(v.Verified).To(BeTrue())`, the
  generated-key scenario fails: Gitea returns
  `verified=false, reason="gpg.error.no_gpg_keys_found"` for the SSH-signed
  commit, even though:
  - the signing public key is registered via `POST /user/keys` as the
    admin user, and
  - `EnsureAdminUserPrimaryEmail` binds the admin user's primary email to
    the committer email.
- `gpg.error.no_gpg_keys_found` is Gitea's GPG-path "no keys" response, so
  the server is almost certainly not going down the SSH verification path
  at all. The current bind-admin-email approach is insufficient.

### Possible next step: a dedicated signing user

The current setup piggybacks on the shared admin account. An alternative
worth trying before declaring a Gitea-version limitation:

1. Create a dedicated Gitea user (e.g. `e2e-signer`) via the admin API,
   with `email == signingCommitterEmail` and the user activated so the
   email is treated as verified.
2. Register the signing public key under that user (either via admin API
   impersonation or by obtaining a token for that user).
3. Leave the admin user's primary email alone.

This is likely to change Gitea's behavior because:

- it removes any ambiguity about whether the admin user's patched primary
  email is actually considered "verified" by the signature-matching code;
- Gitea's SSH signature verification walks users whose verified emails
  match the committer email and then inspects that user's registered
  keys — a purpose-built user gives the cleanest match.

It is **not guaranteed** to fix the issue: if Gitea 1.25.4's
`ParseCommitWithSignature` still routes the commit down the GPG path
(which the current error suggests), a different user won't help. The
cheapest way to know is to try it. If it still fails with the same reason
string, the limitation is on Gitea's signature-detection side and we
should treat Gitea-side verification as explicitly out of scope for this
phase and document it.

## Purpose

This document is the next follow-up after:

- [e2e-repo-in-namespace-plan.md](./e2e-repo-in-namespace-plan.md)
- [e2e-repo-in-namespace-follow-up-plan.md](./e2e-repo-in-namespace-follow-up-plan.md)

It exists to answer one narrower question:

- what is still not done after the repo-per-file refactor and the second follow-up
- what is the smallest remaining plan that still closes the original intent with a high quality bar

This should be the final follow-up for the repo-per-file effort, not another broad redesign.

## Current Assessment

The important result is:

- the repo-per-file refactor itself is now largely done
- the remaining meaningful gap is signing completion
- a smaller documentation alignment pass should happen in the same phase so the docs stop describing the old harness

### Status Matrix

| Area | Status | Evidence | Remaining work |
|---|---|---|---|
| `BeforeSuite` owns shared install prep | Done | `test/e2e/e2e_suite_test.go` only runs `prepare-e2e` | none |
| one repo per e2e file | Done | `SetupRepo(...)` used from each repo-owning file | none |
| repo stamps live under the test namespace | Done | `SetupRepo(...)` passes the test namespace directly | none |
| repo setup no longer re-enters `prepare-e2e` | Done | `e2e-gitea-run-setup` now depends on shared Gitea bootstrap, not `prepare-e2e` | none |
| audit Redis no longer depends on cross-`Describe` ordering | Done | `ensureAuditRedisRepo()` lazily initializes shared file-local fixtures | none |
| optional receiver support remains available | Done | `gitea-run-setup.sh` still treats missing receiver resources as non-fatal | none |
| generated signing flow with local verification | Partially done | current signing suite covers generated key plus `ssh-keygen -Y verify` and `git verify-commit` | strengthen with Gitea-side verification |
| BYOK signing flow | Not done | no e2e scenario currently provisions its own signing Secret | add a real BYOK scenario |
| Gitea-visible signing verification | Not done | current signing tests never register the signing public key with Gitea or assert commit verification through Gitea | add helper plus API assertions |
| docs aligned with the current harness | Not done | `e2e-test-design.md` still describes the old suite-level shared repo env model | update docs in the same phase |

## What Is Already Complete

The following items from the previous follow-up plan should be considered complete unless new evidence appears:

1. **Workstream 1: repo setup is now truly file-local**
   - `SetupRepo(...)` no longer relies on install-namespace indirection
   - `e2e-gitea-run-setup` no longer routes through `prepare-e2e`
   - repeated repo setup calls do not reinstall the controller by themselves

2. **Workstream 2: audit Redis ordering coupling is removed**
   - `ensureAuditRedisRepo()` now owns file-local repo initialization
   - either top-level audit `Describe` can safely initialize the shared repo first

That means this plan should not reopen those workstreams unless a new regression appears.

## Facts To Anchor The Remaining Work

These facts should shape the implementation so we do not design against an imaginary future system:

1. The e2e Gitea instance currently reports version `1.25.4`.
2. The live Swagger document exposed by that instance includes:
   - `POST /user/keys`
   - `GET /user/keys`
   - `GET /repos/{owner}/{repo}/git/commits/{sha}`
3. The same Swagger document does **not** expose a dedicated SSH signing-key registration endpoint.
4. The commit API includes repository commit verification data under:
   - `.commit.verification.verified`
   - `.commit.verification.reason`
5. `hack/e2e/gitea-run-setup.sh` still resets the authenticated user's SSH keys via `/user/keys`.
   - that is still a shared-state hazard for parallel execution
   - it is not the main blocker for finishing this phase (but we would like to be resolved in such a way that it is only 'appending')
6. The signing Secret contract is already implemented in Go under `internal/git/signing.go`.
   - `signing.key`
   - optional `signing.pub`
   - optional `passphrase`

The plan below should therefore use the current Gitea 1.25.4 behavior directly, while keeping helper names generic enough that the implementation can evolve later if Gitea grows a more specific signing-key API.

## Goal

Finish the original repo-per-file plan by closing the remaining signing and documentation gaps without expanding scope.

Success means:

- generated signing is proven both locally and through Gitea
- BYOK signing is proven both locally and through Gitea
- the e2e docs no longer describe the obsolete suite-wide shared-repo model
- the current smoke and quickstart validation stays green

## Non-Goals

This plan does not need to:

- make the entire e2e suite fully safe under `ginkgo -p`
- redesign the Gitea bootstrap scripts again (only if reaaaaaaly needed)
- replace shared SSH key mutation with deploy keys or isolated Gitea users
- generalize signing verification across GitHub, GitLab, and Gitea in one phase
- refactor all older manager tests to perfect cleanup patterns unless needed for the new signing work

## Quality Bar

This phase should be finishable, but not cheapened.

The minimum quality bar is:

1. New signing assertions should be implemented in Go, not shell snippets embedded in tests.
2. Gitea API helpers must be idempotent and return useful error messages with response bodies when registration or lookup fails.
3. New signing coverage must not rely on hidden state from previous `It` blocks.
4. New signing tests should prefer `DeferCleanup` for resources they create inside an `It`, so failures do not leave misleading debris behind.
5. The plan should preserve the current repo-per-file harness shape and should not reintroduce package-global repo env coupling.
6. Every new helper should exist because it removes repeated logic from the tests, not because it creates an abstraction layer for its own sake.
7. Any Gitea verification assertion should target the version we actually run in e2e today, not a generic platform assumption copied from another provider.

## Remaining Workstreams

## Workstream 1: Add Typed Gitea Signing Verification Helpers

### Why this is first

The current signing suite can already create and locally verify signed commits.
What it cannot do is prove that Gitea itself recognizes those commits as verified.

That missing piece should be implemented once in a helper layer and reused by both generated-key and BYOK scenarios.

### Proposed shape

Add a small helper file in `test/e2e/`, for example:

- `test/e2e/gitea_api_test.go`

This helper should stay in the `e2e` package so it can reuse the current test utilities and environment.

### Suggested helper surface

```go
type giteaPublicKey struct {
    ID          int64
    Title       string
    Key         string
    Fingerprint string
}

type giteaCommitVerification struct {
    Verified bool
    Reason   string
    Signature string
}

func RegisterSigningPublicKey(publicKey string, title string) (*giteaPublicKey, error)
func FindUserPublicKeyByKey(publicKey string) (*giteaPublicKey, error)
func GetCommitVerification(owner, repo, sha string) (*giteaCommitVerification, error)
```

### Important implementation note

For the current Gitea `1.25.4` e2e stack, `RegisterSigningPublicKey(...)` will almost certainly need to use:

- `POST /user/keys`

even though the helper name should stay signing-focused.

That is acceptable because:

- it matches the API surface actually available in this Gitea version
- it still separates signing verification logic from the SSH transport bootstrap script
- the helper can be re-pointed later if a dedicated signing-key endpoint becomes available in a future Gitea version

### Behavior requirements

- registration must be safe to call repeatedly for the same key
- if the key already exists, the helper should return the existing record instead of failing
- commit verification lookup must surface both `verified` and `reason`
- helper failures should include enough HTTP context to debug Gitea-side problems quickly

### Acceptance criteria

- a signing test can register a public key without using shell `curl`
- a signing test can query commit verification without parsing raw `curl` output
- helpers work against the live e2e Gitea instance

## Workstream 2: Strengthen The Existing Generated-Key Signing Scenario

### Current state

The first signing scenario already proves:

- the operator generated a signing key
- `.status.signingPublicKey` was populated
- the resulting commit is locally verifiable

What it does **not** prove is:

- that Gitea knows about the signing key
- that Gitea reports the commit as verified

### Required changes

Extend the first signing scenario so that it:

1. waits for `.status.signingPublicKey`
2. registers that public key with Gitea through the new helper
3. asserts the registration succeeded and returns a real stored key record
4. produces the signed commit as it already does
5. queries the repo commit API for that commit
6. asserts Gitea reports the commit as verified

### Assertion guidance

Prefer asserting:

- `verification.verified == true`

And when it is false, report:

- the raw `verification.reason`
- the commit SHA
- the repo name

Do not overfit to a single hardcoded reason string unless the live API proves that value is stable.

### Acceptance criteria

- generated-key signing still passes `ssh-keygen -Y verify`
- generated-key signing still passes `git verify-commit`
- generated-key signing now also passes a Gitea verification API assertion

## Workstream 3: Add A Real BYOK Signing Scenario

### Current state

There is no e2e coverage yet for:

- a user-provided signing Secret
- `generateWhenMissing: false`

That is the largest remaining product-level gap relative to the original plan.

### Proposed approach

Add one new signing `It` block for BYOK.

The test should:

1. generate an SSH signing keypair in test setup
2. create the referenced Secret in the test namespace
3. set `generateWhenMissing: false`
4. apply a signing-enabled `GitProvider`
5. wait for `status.signingPublicKey`
6. assert that the surfaced public key matches the key created by the test
7. register that public key with Gitea
8. trigger a commit
9. verify the commit locally
10. verify the commit through Gitea

### Recommended implementation detail

Use the existing Go helper from `internal/git/signing.go` instead of shelling out to `ssh-keygen` for the BYOK keypair:

- `git.GenerateSSHSigningKeyPair(...)`

And use the exported Secret keys from the same package:

- `git.SigningKeyDataKey`
- `git.SigningPublicKeyDataKey`
- `git.SigningPassphraseDataKey`

This keeps the e2e scenario aligned with the real signing Secret contract instead of duplicating it.

### Scope decision

The first BYOK scenario should use:

- an unencrypted key

Passphrase-protected BYOK support can stay out of scope for this phase unless the implementation turns out to be nearly free.

### Acceptance criteria

- a BYOK signing Secret is created by the test
- `GitProvider.status.signingPublicKey` matches the BYOK public key
- the BYOK commit passes local verification
- the BYOK commit is reported verified by Gitea

## Workstream 4: Keep The Signing Suite Maintainable While It Grows

### Problem

The signing file already contains a fair amount of repeated setup and manual cleanup.
Adding Gitea verification and BYOK without tightening the structure will make it harder to maintain.

### Required scope

Do only the cleanup needed to keep this phase readable and robust:

- factor repeated signing verification logic into helper functions
- use `DeferCleanup` inside new signing scenarios
- keep the existing non-signature tests readable

### Recommended boundaries

Do:

- extract small helper functions for:
  - commit hash lookup
  - local verification
  - Gitea verification
  - signing key registration

Do not:

- rewrite the whole file into a generic table-driven mini-framework
- refactor unrelated manager scenarios in the same change just because they also use manual cleanup

### Acceptance criteria

- the new BYOK and generated-key assertions reuse helpers instead of duplicating low-level logic
- failure output stays readable and specific
- cleanup remains reliable even when a signing assertion fails mid-test

## Workstream 5: Align The Design Docs With The Current Harness

### Why this belongs in the same phase

The code now reflects the repo-per-file model, but some design docs still describe the old suite-wide shared repo model.
If we finish signing and leave the docs stale, the repo will still look half-migrated to the next person reading it.

### Required doc updates

Update these docs after the code lands:

1. `docs/design/e2e-repo-in-namespace-follow-up-plan.md`
   - mark Workstreams 1 and 2 complete
   - mark Workstream 3 complete once this phase lands

2. `docs/design/e2e-test-design.md`
   - remove or rewrite sections that still say:
     - `BeforeSuite` runs `e2e-gitea-run-setup`
     - suites primarily consume `E2E_REPO_NAME`, `E2E_CHECKOUT_DIR`, and `E2E_SECRETS_YAML`
   - describe the current repo-per-file `SetupRepo(...)` model instead

3. optionally add a short note in the signing design docs that the current e2e Gitea stack verifies SSH-signed commits via the existing SSH public key database and the commit verification API

### Acceptance criteria

- repo-per-file docs no longer describe the old shared-repo env-var flow as the active harness design
- signing docs and e2e docs do not contradict the implemented test strategy

## Suggested Implementation Sequence

Keep this phase short and intentionally ordered:

1. add typed Gitea API helpers
2. strengthen the existing generated-key signing scenario
3. add the BYOK signing scenario
4. refactor only the signing-local helpers and cleanup patterns needed to keep the file readable
5. update the design docs
6. run the full validation sequence

This order matters:

- step 1 proves the live Gitea verification contract up front
- step 2 upgrades an already-green scenario before adding new scenario surface area
- step 3 lands the largest remaining functional gap
- step 4 keeps the file from turning into unstructured repetition
- step 5 ensures the written design finally matches the code

## Validation Plan

The validation bar should stay high and stay sequential for the heavier e2e commands.

### Focused checks during implementation

```bash
docker info
task fmt
task lint
task test
task test-e2e-signing
```

### Full closeout sequence

```bash
docker info
task fmt
task lint
task test
task test-e2e
task test-e2e-quickstart-manifest
task test-e2e-quickstart-helm
```

Recommended extra spot check while developing the signing helpers:

```bash
go test ./test/e2e -run TestE2E -ginkgo.v -ginkgo.label-filter=signing
```

## Done Definition

This phase is complete when all of the following are true:

1. the existing generated-key signing scenario proves both local verification and Gitea verification
2. a new BYOK signing scenario proves both local verification and Gitea verification
3. the implementation uses a typed Gitea API helper instead of ad hoc shell `curl` calls inside tests
4. the current repo-per-file harness shape remains intact
5. the key repo-per-file and e2e design docs no longer describe the obsolete shared-repo env-var model
6. the full validation sequence passes

## Explicitly Deferred After This Phase

If anything is still left after this plan, it should be treated as a different topic, not as unfinished repo-per-file work:

- replacing shared `/user/keys` mutation with isolated Gitea users or deploy keys
- making the full suite safe under `ginkgo -p`
- adding passphrase-protected BYOK coverage
- introducing provider-specific signing verification flows for GitHub or GitLab

Those are valid future improvements, but they should not block calling the repo-per-file effort complete once this plan lands.
