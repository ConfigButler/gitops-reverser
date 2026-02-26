# Makefile E2E Overview and Next-Step Plan

## Purpose

This document explains the current Makefile-driven E2E model and proposes the next implementation steps to reach:

- seed-based, run-scoped test namespaces
- explicit suite x install-method combinations
- quickstart assertions implemented in Go (not shell)
- CI-aligned developer entrypoints

## Current State

## Full E2E (`make test-e2e`)

Current entrypoint:

- `test-e2e -> $(CS)/e2e.passed`

Current dependency chain for full E2E:

- `$(CS)/e2e.passed` depends on:
  - `install` (default `INSTALL_MODE=config-dir`)
  - `$(CS)/controller.deployed`
  - `$(CS)/portforward.running`
  - `$(CS)/age-key.applied`
  - `test/e2e/**/*.go`

`$(CS)/controller.deployed` currently:

- loads image into Kind (`$(E2E_IMAGE)`)
- deletes validating webhook configuration
- sets image in `config/` via `kustomize edit set image`
- applies `kustomize build config`
- rolls out `deploy/gitops-reverser` in namespace `sut`

Important relationship:

- `controller.deployed` is the cluster-state gate for full suite execution.
- `e2e.passed` is the test-result stamp that runs `go test ./test/e2e`.

## Quickstart E2E

Current entrypoints:

- `make test-e2e-quickstart-helm`
- `make test-e2e-quickstart-manifest`

These flows:

- always cleanup dedicated Kind cluster first
- install cert-manager + gitea via shared stamps
- load image for local runs
- execute `test/e2e/scripts/run-quickstart.sh` in `helm` or `manifest` mode

CI alignment is already strong:

- `.github/workflows/ci.yml` runs:
  - `make test-e2e`
  - matrix: `make test-e2e-quickstart-helm|manifest`

## Seed and Namespace Today

What exists today:

- Ginkgo seed is exported to `make` as `E2E_GINKGO_RANDOM_SEED`.
- quickstart framework installer namespace uses `run-<seed>`.

What is missing:

- full E2E still uses fixed namespace constants (`sut`) in Go helpers/tests.
- Makefile does not derive namespace/build stamp identity from seed.
- no run-scoped namespace stamp used by full E2E test resources.

## Gaps Against Target Vision

1. Seed is propagated but not structurally used for run scoping.
2. Suite x install-method model is partial:
   - quickstart: `helm`, `manifest`
   - full: effectively `config-dir` + extra deploy path
   - `config-dir` is not clearly positioned as either product path or internal test path.
3. Quickstart Go framework lacks parity with `run-quickstart.sh` assertions (commit progression, encrypted secret checks, invalid-credential message checks).
4. Full E2E still contains significant infra-level shell/Kubernetes orchestration in Go (`setup-gitea.sh`, many direct `kubectl`).
5. Local quickstart manifest target has a defect:
   - `test-e2e-quickstart-manifest` calls `build-plain-manifests-installer`, but no such target exists.

## Proposed End-State Model

## Test matrix

Define explicit, stable matrix naming:

- `full-config-dir` (current full suite path)
- `quickstart-helm`
- `quickstart-manifest`

Optional future (only if needed):

- `full-helm`
- `full-manifest`

## Seed contract

Single run contract shared by Make and Go:

- `E2E_RUN_SEED` (defaults to ginkgo seed when available)
- `E2E_RUN_NS=run-$(E2E_RUN_SEED)`
- `E2E_CONTROLLER_NS` (keep `sut` initially, parameterize later)

## Ownership split

- Makefile owns cluster infra and deployment/install orchestration.
- Go tests own scenario logic and assertions.
- Go should not perform install/deploy internals directly.

## Phased Plan

## Phase 0: Stabilize current paths (small PR)

1. Fix missing target reference in `test-e2e-quickstart-manifest`:
   - replace `build-plain-manifests-installer` with existing `dist/install.yaml` dependency path.
2. Remove `.PHONY` from `$(CS)/portforward.running` so stamp behavior is real.
3. Add/refresh Makefile overview doc (this file) and link it from `docs/make-e2e-deps.md`.

## Phase 1: Introduce run seed + run namespace primitives

1. Add Make variables:
   - `E2E_RUN_SEED ?= $(if $(E2E_GINKGO_RANDOM_SEED),$(E2E_GINKGO_RANDOM_SEED),$(shell date +%s))`
   - `E2E_RUN_NS ?= run-$(E2E_RUN_SEED)`
2. Add run namespace stamp:
   - `$(CS)/run-ns.ready` applying/labeling `$(E2E_RUN_NS)`.
3. Pass `E2E_RUN_SEED`, `E2E_RUN_NS`, `E2E_CONTROLLER_NS` into `go test` env.
4. In Go tests, replace hardcoded test namespace constants with env-backed helper.

## Phase 2: Explicit suite x install-method targets

1. Introduce clear top-level targets:
   - `test-e2e-full-config-dir`
   - `test-e2e-quickstart-helm`
   - `test-e2e-quickstart-manifest`
2. Keep `test-e2e` as alias to `test-e2e-full-config-dir` for compatibility.
3. Add `test-e2e-ci` as local equivalent of CI flow:
   - full + quickstart-helm + quickstart-manifest.

## Phase 3: Port `run-quickstart.sh` assertions into Go

Port these checks into `test/e2e/quickstart_framework_e2e_test.go`:

1. resource readiness (`GitProvider`, `GitTarget`, `WatchRule`)
2. generated SOPS secret exists + backup-warning + age-recipient annotation
3. create/update/delete ConfigMap produces expected repo file transitions and commit count increments
4. Secret commit is encrypted (no plaintext/base64 leakage)
5. encrypted file decrypts with generated age key and shows updated value
6. invalid credentials produce actionable `ConnectionFailed` status message

Then reduce `run-quickstart.sh` to thin install/bootstrap wrapper or retire it.

## Phase 4: Minimize infra/Kubernetes orchestration in Go

1. Move remaining setup operations behind Make targets/stamps.
2. Keep Go focused on test scenario inputs + assertions.
3. Add one convenience target for developers (candidate name):
   - `make last-deployment` or `make e2e-prepare` to provision infra without running tests.

## Recommended Immediate Next PR

1. Apply Phase 0 fixes.
2. Start Phase 3 by porting one vertical slice in Go:
   - ConfigMap create/update/delete + repo file + commit assertions.
3. Keep CI job structure unchanged while moving logic from shell to Go.

This gives fast signal improvement without destabilizing the current pipeline.
