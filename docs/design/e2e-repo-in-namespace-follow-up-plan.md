# E2E Repo-Per-File Follow-Up Plan

## Status

**Workstream 1 (file-local repo setup) — DONE.** `e2e-gitea-run-setup` no longer routes through
`prepare-e2e`, and repeated `SetupRepo(...)` calls do not trigger controller reinstalls.

**Workstream 2 (audit Redis ordering coupling) — DONE.** `ensureAuditRedisRepo()` owns file-local
lazy initialization; either top-level audit `Describe` can run first safely.

**Workstream 3 (signing completion) — DONE.** Generated-key and BYOK signing scenarios both prove
local verification (`ssh-keygen -Y verify`, `git verify-commit`) and Gitea-side verification via the
typed `GetCommitVerification(...)` helper. See
[follow-up-plan-3.md](./follow-up-plan-3.md) for the implementation of this phase.

Follow-up plan after reviewing the current `e2e-repo-in-namespace` refactor against:

- [e2e-repo-in-namespace-plan.md](./e2e-repo-in-namespace-plan.md)
- current e2e implementation in `test/e2e/`

This document focuses on the gaps that are still open even though the refactor is already largely functional.

## Confirmed Findings

The current branch successfully moved most repo state from package-global env vars to file-local `RepoArtifacts`,
and the refactored smoke and quickstart e2e flows are passing.

However, three meaningful gaps remain:

1. `SetupRepo(...)` still triggers expensive install/setup work.
   - This violates the intended shape where `BeforeSuite` owns shared cluster/install preparation.
   - In practice, each repo setup currently re-enters `prepare-e2e`, which tears down and reinstalls the controller.

2. `audit_redis_e2e_test.go` still has hidden cross-container ordering coupling.
   - The consumer `Describe` depends on `auditRedisRepo` being initialized by the producer `Describe`.
   - That is fragile because they are separate top-level `Ordered` containers.

3. signing is not complete relative to the original plan.
   - local verification is covered
   - operator-generated signing is covered
   - BYOK is not covered
   - Gitea-visible signing verification is not implemented yet

## Goals

The follow-up should finish the refactor without broad redesign:

- keep `BeforeSuite` as the single owner of shared cluster/install preparation
- keep one repo per e2e test file
- remove remaining hidden ordering assumptions
- complete the signing story for both functional and inspectable verification
- preserve the passing behavior of the current smoke and quickstart flows

## Non-Goals

This follow-up does not need to:

- make the whole e2e suite fully `ginkgo -p` safe
- redesign all e2e helper APIs
- split the suite into multiple Go packages
- solve long-term Gitea SSH auth isolation beyond what is needed for signing verification

## Workstream 1: Make Repo Setup Truly File-Local

### Problem

`SetupRepo(...)` currently calls the Task target `e2e-gitea-run-setup`, which routes through `e2e-gitea-bootstrap`,
which routes through `prepare-e2e`.

That means per-file repo setup still re-enters shared install logic and can force:

- `clean-installs`
- controller reinstall
- webhook/cert re-setup
- port-forward restart churn

This is the opposite of the intended split between:

- shared cluster/install state
- per-file repo state

### Proposal

Separate Gitea repo bootstrapping from full e2e preparation.

Introduce two layers explicitly:

1. **Shared suite preparation**
   - still owned by `BeforeSuite`
   - still runs `prepare-e2e`
   - remains responsible for:
     - cluster readiness
     - controller install
     - age key
     - webhook TLS
     - shared services
     - port-forwards
     - Gitea org bootstrap if desired

2. **Per-file repo setup**
   - used by `SetupRepo(...)`
   - must only do:
     - assert shared prerequisites are already present
     - ensure port-forwards are healthy
     - ensure Gitea org/bootstrap state exists
     - run `_gitea-run-setup`

### Concrete Shape

Add a dedicated Task path for repo setup that does **not** call `prepare-e2e`.

One clean option:

- keep `prepare-e2e` unchanged
- add `e2e-gitea-bootstrap-shared`
  - depends on:
    - `_cluster-ready`
    - `_flux-installed`
    - `_flux-setup-ready`
    - `_services-ready`
    - `portforward-ensure`
    - `_gitea-bootstrap`
  - does **not** call `install` or `clean-installs`
- change `e2e-gitea-run-setup` to depend on `e2e-gitea-bootstrap-shared` instead of `e2e-gitea-bootstrap`
- make `_gitea-bootstrap` depend on shared services readiness rather than `prepare-e2e.ready`
  - it only needs the shared Gitea service and a healthy port-forward
  - it can validate org presence directly instead of piggybacking on install stamps

Then `SetupRepo(...)` can keep its current high-level API while finally matching the intended behavior.

### Notes

- `SetupRepo(...)` does not need install-namespace indirection anymore
  - pass `NAMESPACE=<test namespace>` directly
  - require that namespace to already exist before repo setup runs
- the stamp path model introduced in the current refactor is sound and should stay
- direct task usability matters, but the suite-owned flow is more important than making `e2e-gitea-run-setup`
  independently self-healing via reinstall

### Acceptance Criteria

- `BeforeSuite` remains the only place that runs `prepare-e2e`
- one call to `SetupRepo(...)` no longer triggers `clean-installs`
- one call to `SetupRepo(...)` no longer restarts or reinstalls the controller
- smoke and quickstart e2e targets still pass

## Workstream 2: Remove Audit Redis Ordering Coupling

### Problem

`audit_redis_e2e_test.go` currently uses a package-global `auditRedisRepo`, initialized in the producer `Describe`
and consumed in the consumer `Describe`.

That means correctness depends on top-level execution order between two separate `Ordered` containers.

### Proposal

Give the file one explicit repo owner path instead of cross-container shared mutable state.

The simplest fix is to move repo setup into a file-local helper that is safe to call from either container:

```go
func ensureAuditRedisRepo() *RepoArtifacts
```

Behavior:

- compute the deterministic repo name once per package run
- use `testNamespaceFor("audit-consumer")` as the repo-owning namespace
- lazily initialize the repo artifacts on first use
- return the same file-local pointer afterward

### Preferred Implementation Detail

Avoid relying on one `Describe` to populate state for another.

A small `sync.Once`-backed helper is a good fit here:

- file-local vars:
  - `auditRedisRepo *RepoArtifacts`
  - `auditRedisRepoOnce sync.Once`
- helper ensures:
  - consumer namespace exists
  - repo setup runs once
  - later callers just reuse the artifacts

This keeps the current "one repo for the whole file" decision intact without requiring a file split.

### Acceptance Criteria

- either top-level audit `Describe` can run first without panicking
- no `nil` access to `auditRedisRepo`
- repo ownership still lives under the consumer namespace stamp path
- `task test-e2e` and any focused audit run still pass

## Workstream 3: Finish Signing Per Plan

### Problem

The current signing suite proves:

- signed commits are produced
- `ssh-keygen -Y verify` works
- `git verify-commit` works

But it does not yet prove:

- BYOK signing flow
- Gitea-visible verification using registered signing keys

### Proposal

Complete signing in two layers.

### 3A. Add Gitea signing-key registration helper

Introduce a small helper dedicated to Gitea signing verification registration, for example:

```go
func RegisterSigningPublicKey(ctx, publicKey string, title string) error
```

Responsibilities:

- call the Gitea API through the existing localhost port-forward
- register the key as a **signing key**, not as a transport SSH auth key
- avoid piggybacking on the shared `configure_ssh_key_in_gitea()` behavior used for clone/push auth

Why split this:

- transport auth keys and signing verification keys are different concerns
- the current script-level SSH-key reset logic is explicitly called out as a parallel hazard in the original plan

### 3B. Cover both signing flows in e2e

Keep the current generated-key case, but strengthen it:

- wait for `.status.signingPublicKey`
- register that public key with Gitea via the new helper
- keep local verification
- add an API-level assertion that the signing key registration succeeded

Add a new BYOK scenario:

- generate an SSH signing key pair in test setup
- store private key in the referenced Secret using the expected signing Secret format
- register the public key with Gitea using the same helper
- trigger a commit
- verify with:
  - `ssh-keygen -Y verify`
  - `git verify-commit`

### 3C. Decide how much Gitea verification to assert automatically

There are two levels of confidence:

1. **Minimum acceptable automated coverage**
   - successfully register signing key in Gitea
   - produce a signed commit
   - keep local verification

2. **Stronger automated coverage**
   - query Gitea commit API and assert the commit is reported as verified/signed

The stronger version is preferred if the API field is stable and easy to assert.
If that field is awkward or version-sensitive, the minimum version is still a real improvement and is much closer to the
original plan than the current state.

### Acceptance Criteria

- signing suite contains one generated-key flow and one BYOK flow
- both flows pass local verification
- both flows register the signing public key with Gitea
- at least one automated assertion confirms Gitea-side signing registration or verification behavior

## Suggested Sequence

Land this as a short follow-up series rather than one giant rewrite:

1. fix Task/SetupRepo shared-vs-local separation
2. fix `audit_redis_e2e_test.go` repo initialization model
3. add signing key registration helper
4. add BYOK signing coverage
5. add Gitea-side verification/assertions
6. run full validation and tidy formatting

This ordering reduces risk:

- workstream 1 removes the biggest harness regression first
- workstream 2 removes correctness fragility
- workstream 3 then builds on a more stable harness

## Validation Plan

Minimum required validation after the follow-up:

```bash
task fmt
task lint
task test
task test-e2e
task test-e2e-quickstart-manifest
task test-e2e-quickstart-helm
```

Recommended focused checks during implementation:

```bash
go test ./test/e2e -run TestE2E -ginkgo.v -ginkgo.label-filter=signing
go test ./test/e2e -run TestE2E -ginkgo.v -ginkgo.label-filter=audit-redis
```

And while validating workstream 1 specifically, confirm from logs that repeated `SetupRepo(...)` calls no longer cause:

- `clean-installs`
- full reinstall
- controller rollout triggered only by repo setup

## Expected End State

After this follow-up, the repo-per-file refactor should actually match the intended shape:

- shared install work happens once in `BeforeSuite`
- repo setup is cheap and file-local
- audit Redis no longer has hidden ordering assumptions
- signing is complete enough to back the original plan
- the current passing smoke and quickstart behavior is preserved
