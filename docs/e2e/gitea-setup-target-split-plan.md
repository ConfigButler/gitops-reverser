# E2E Gitea Setup Split Plan (Make Targets + Concrete Artifacts)

## Overview

This plan splits `test/e2e/scripts/setup-gitea.sh` into clear setup levels and maps each level to explicit Make
targets that produce concrete artifact files.

Goal:

1. Keep shared/bootstrap work one-time per cluster context (`CTX`).
2. Keep one active repo per test run/namespace.
3. Store most run artifacts in `$(CS)/$(NAMESPACE)/repo/`.
4. Keep checkout data in `$(CS)/$(NAMESPACE)/repo/<repo-name>/` for easy developer inspection.

## Current Behavior Summary

Today `setup-gitea.sh` does all of this in one execution:

1. Check API readiness through localhost port-forward.
2. Ensure org (`testorg`).
3. Create/reuse token.
4. Generate SSH keypair and configure Gitea user key.
5. Create namespace secrets (`git-creds`, `git-creds-ssh`, `git-creds-invalid`).
6. Create a repo.
7. Clone checkout and configure git.

Call sites:

- `test/e2e/e2e_test.go` in `BeforeAll`
- `test/e2e/quickstart_framework_e2e_test.go` in `setupGiteaRepository`

Suite-level prep (`test/e2e/e2e_suite_test.go`) currently runs `make prepare-e2e`, but repo+secret bootstrap still happens
inside per-spec setup scripts.

## Proposed Split

### Level 1: Cluster-scoped bootstrap (once per CTX)

Artifacts under:

- `.stamps/cluster/$(CTX)/gitea/bootstrap/`

Concrete artifact targets:

1. `$(CS)/gitea/bootstrap/api.ready`
2. `$(CS)/gitea/bootstrap/org-testorg.ready`
3. `$(CS)/gitea/bootstrap/ready` (aggregate stamp only after concrete files exist)

Why this helps:

- Shared infra readiness is explicit and isolated from run-scoped data.
- Per-run token/SSH/secret generation can happen cleanly after infra is prepared.

### Level 2: Run-scoped active repo + credentials (per NAMESPACE)

Artifacts under:

- `.stamps/cluster/$(CTX)/$(NAMESPACE)/repo/`

Concrete artifact targets:

1. `$(CS)/$(NAMESPACE)/repo/active-repo.txt` (contains repo name)
2. `$(CS)/$(NAMESPACE)/repo/token.txt`
3. `$(CS)/$(NAMESPACE)/repo/ssh/id_rsa`
4. `$(CS)/$(NAMESPACE)/repo/ssh/id_rsa.pub`
5. `$(CS)/$(NAMESPACE)/repo/ssh/known_hosts`
6. `$(CS)/$(NAMESPACE)/repo/secrets.yaml` (single multi-doc manifest for `git-creds`, `git-creds-ssh`, `git-creds-invalid`)
7. `$(CS)/$(NAMESPACE)/repo/secrets.applied`
8. `$(CS)/$(NAMESPACE)/repo/repo.ready`

Source inputs:

- `$(CS)/gitea/bootstrap/org-testorg.ready`
- `$(CS)/gitea/bootstrap/api.ready`

### Level 3: Checkout (per active repo name)

Artifacts under:

- `.stamps/cluster/$(CTX)/$(NAMESPACE)/repo/$(REPO_NAME)/`

Concrete artifact targets:

1. `$(CS)/$(NAMESPACE)/repo/$(REPO_NAME)/checkout.path`
2. `$(CS)/$(NAMESPACE)/repo/$(REPO_NAME)/checkout/.git/HEAD`
3. `$(CS)/$(NAMESPACE)/repo/checkout.ready` (aggregate; uses active repo from `active-repo.txt`)

Default checkout location for active repo:

- `$(CS)/$(NAMESPACE)/repo/$(REPO_NAME)/checkout`
- `CHECKOUT_DIR` may override, but should default to the stamp path above.

## Script Refactor Shape

Recommended split while preserving behavior:

1. `test/e2e/scripts/gitea-bootstrap.sh`
2. `test/e2e/scripts/gitea-run-setup.sh` (active repo + token + ssh + secrets + checkout)
3. Keep `test/e2e/scripts/setup-gitea.sh` as compatibility wrapper (calls bootstrap + run setup).

This allows incremental migration without breaking existing tests.

## Makefile Target Contract

Suggested phony wrappers:

1. `e2e-gitea-bootstrap` -> `$(CS)/gitea/bootstrap/ready`
2. `e2e-gitea-run-setup` -> `$(CS)/$(NAMESPACE)/repo/repo.ready` and `$(CS)/$(NAMESPACE)/repo/checkout.ready`
3. `e2e-gitea-run-setup` requires `REPO_NAME` and optional `CHECKOUT_DIR`

Suggested integration:

1. Keep `prepare-e2e` focused on cluster + controller prep only.
2. Add a second Make call from `ensureE2EPrepared()` in `test/e2e/e2e_suite_test.go`:
   - first call: `make ... prepare-e2e`
   - second call: `make ... REPO_NAME=<generated> e2e-gitea-run-setup`
3. The second call must run after `prepare-e2e`.

## Producer/Consumer File Map

| Producer target | Output file | Consumed by |
| --- | --- | --- |
| `$(CS)/gitea/bootstrap/api.ready` | `.stamps/cluster/$(CTX)/gitea/bootstrap/api.ready` | run-setup target |
| `$(CS)/gitea/bootstrap/org-testorg.ready` | `.stamps/cluster/$(CTX)/gitea/bootstrap/org-testorg.ready` | run-setup target |
| `$(CS)/$(NAMESPACE)/repo/active-repo.txt` | `.stamps/cluster/$(CTX)/$(NAMESPACE)/repo/active-repo.txt` | checkout target + suite env export |
| `$(CS)/$(NAMESPACE)/repo/token.txt` | `.stamps/cluster/$(CTX)/$(NAMESPACE)/repo/token.txt` | `secrets.yaml`, checkout clone auth |
| `$(CS)/$(NAMESPACE)/repo/ssh/id_rsa` | `.stamps/cluster/$(CTX)/$(NAMESPACE)/repo/ssh/id_rsa` | `secrets.yaml` |
| `$(CS)/$(NAMESPACE)/repo/ssh/id_rsa.pub` | `.stamps/cluster/$(CTX)/$(NAMESPACE)/repo/ssh/id_rsa.pub` | Gitea SSH key registration |
| `$(CS)/$(NAMESPACE)/repo/ssh/known_hosts` | `.stamps/cluster/$(CTX)/$(NAMESPACE)/repo/ssh/known_hosts` | `secrets.yaml` |
| `$(CS)/$(NAMESPACE)/repo/secrets.yaml` | `.stamps/cluster/$(CTX)/$(NAMESPACE)/repo/secrets.yaml` | `secrets.applied` |
| `$(CS)/$(NAMESPACE)/repo/secrets.applied` | `.stamps/cluster/$(CTX)/$(NAMESPACE)/repo/secrets.applied` | `repo.ready` |
| `$(CS)/$(NAMESPACE)/repo/$(REPO_NAME)/checkout.path` | `.stamps/cluster/$(CTX)/$(NAMESPACE)/repo/$(REPO_NAME)/checkout.path` | e2e/quickstart tests needing checkout location |
| `$(CS)/$(NAMESPACE)/repo/$(REPO_NAME)/checkout/.git/HEAD` | `.stamps/cluster/$(CTX)/$(NAMESPACE)/repo/$(REPO_NAME)/checkout/.git/HEAD` | `checkout.ready` |
| `$(CS)/$(NAMESPACE)/repo/repo.ready` | `.stamps/cluster/$(CTX)/$(NAMESPACE)/repo/repo.ready` | suite execution start gate |
| `$(CS)/$(NAMESPACE)/repo/checkout.ready` | `.stamps/cluster/$(CTX)/$(NAMESPACE)/repo/checkout.ready` | suite execution start gate |

Recommended environment variables per producer:

1. Bootstrap level: `CTX`, `GITEA_NAMESPACE`, optional `GITEA_ADMIN_USER`, `GITEA_ADMIN_PASS`.
2. Run setup level: `CTX`, `NAMESPACE`, `REPO_NAME`, optional `CHECKOUT_DIR`, `GITEA_NAMESPACE`.

## Go Test Wiring Plan

1. `test/e2e/e2e_suite_test.go`
   - keep existing `prepare-e2e` invocation
   - add second make invocation after `prepare-e2e`:
     - `make ... REPO_NAME=<repo> e2e-gitea-run-setup`
   - export outputs for test usage:
     - `E2E_REPO_NAME`
     - `E2E_CHECKOUT_DIR` (read from `checkout.path`)
     - `E2E_GIT_SECRET_HTTP`, `E2E_GIT_SECRET_SSH`, `E2E_GIT_SECRET_INVALID` (run-scoped names)
2. `test/e2e/e2e_test.go`
   - remove direct `setup-gitea.sh` call
   - consume suite-exported repo/secret env vars
3. `test/e2e/quickstart_framework_e2e_test.go`
   - remove direct `setup-gitea.sh` call
   - consume suite-exported repo/secret env vars
   - use same active repo model as suite setup

## Feasibility Notes

1. High feasibility: bootstrap remains stable in `prepare-e2e`.
2. High feasibility: single active repo model keeps state simpler under `$(CS)/$(NAMESPACE)/repo/`.
3. Main implementation risk: namespace mismatch (`NAMESPACE` vs `SUT_NAMESPACE` / `QUICKSTART_NAMESPACE`).
   - Mitigation: always pass explicit `NAMESPACE` to run-setup Make call in suite.

## Implementation Phases

1. Phase 1: Keep `prepare-e2e` behavior; add `e2e-gitea-run-setup` writing run artifacts to `$(CS)/$(NAMESPACE)/repo/`.
2. Phase 2: Add second Make invocation in `e2e_suite_test.go` immediately after `prepare-e2e`.
3. Phase 3: Remove direct `setup-gitea.sh` invocations from `e2e_test.go` and quickstart test file.
4. Phase 4: Keep wrapper for compatibility, then trim wrapper to call new split scripts only.

## Validation Gate (after implementation)

Required sequence:

1. `make fmt`
2. `make generate`
3. `make vet`
4. `make lint`
5. `make test`
6. `docker info` (precheck for e2e)
7. `make test-e2e`
8. `make test-e2e-quickstart-manifest`
9. `make test-e2e-quickstart-helm`
