# E2E Image and Artifact Reuse Design

## Status
- State: Implemented
- Scope: CI e2e, CI install smoke (helm + manifest), devcontainer/local e2e, IDE direct e2e runs

## Problem Statement
We need one behavior model that satisfies these constraints:
- CI must reuse artifacts built earlier in the pipeline (image, packaged Helm chart, generated `dist/install.yaml`).
- Local/devcontainer runs should be easy (`make test-e2e`) and should auto-build a local image when no prebuilt image is provided.
- IDE/debugger runs (`go test ./test/e2e/...`) should remain usable without manual pre-steps.
- Image selection logic should be centralized and avoid duplicated implementation.

## Goals
- Use a single decision input: `PROJECT_IMAGE`.
- If `PROJECT_IMAGE` is set: reuse it and do not rebuild.
- If `PROJECT_IMAGE` is not set: build/load local image once per run path.
- Keep orchestration primarily in `Makefile`.
- Keep Go `BeforeSuite` as IDE fallback only.
- Keep cluster behavior explicit:
  - `test-e2e` reuses existing cluster state (fast path).
  - install smoke local fallback path performs clean install validation.

## Non-Goals
- No digest-aware image override logic for Helm values beyond repository/tag split.
- No new CI jobs or artifact formats.
- No changes to release publishing.

## Decision Model
`PROJECT_IMAGE` is the source of truth:
- `PROJECT_IMAGE` present:
  - Treat as prebuilt image.
  - Skip local image build/load steps.
  - Skip cluster cleanup in install smoke.
  - Inject into test/install flows.
- `PROJECT_IMAGE` absent:
  - Use local fallback image `$(E2E_LOCAL_IMAGE)` (`gitops-reverser:e2e-local` by default).
  - Build image locally and load it into Kind.
  - For install smoke, clean cluster first to validate clean install behavior.
  - Use that image for test/install flows.

## Execution Flows

### 1) CI: e2e test suite
- Workflow passes `PROJECT_IMAGE` from `docker-build` output.
- `make test-e2e` sees `PROJECT_IMAGE` and skips rebuild/load.
- Go tests run with that exact image.
- Cluster is reused (no cleanup in this target).

Outcome:
- No duplicate image builds in CI.

### 2) CI: install smoke (`helm` and `manifest`)
- Workflow reuses release bundle artifact (`gitops-reverser.tgz`, `dist/install.yaml`).
- Workflow passes the same prebuilt `PROJECT_IMAGE`.
- `make test-e2e-install-helm` and `make test-e2e-install-manifest` skip cluster cleanup and local image rebuild/load.
- Helm mode injects repository/tag via `--set image.repository` and `--set image.tag`.
- Manifest mode applies `dist/install.yaml`, then overrides deployment image via `kubectl set image`.
- `test-e2e-install-manifest` does not regenerate `dist/install.yaml` when `PROJECT_IMAGE` is set.

Outcome:
- Reuse of both chart/manifest artifacts and prebuilt image in CI.

### 3) Devcontainer/local: full e2e via Make
- Run `make test-e2e` with no `PROJECT_IMAGE`.
- Make reuses existing cluster.
- Make auto-builds and Kind-loads `$(E2E_LOCAL_IMAGE)`, then runs tests with it.

Outcome:
- Single command, no manual image prep.

### 4) Devcontainer/local: install smoke via Make
- Run `make test-e2e-install-helm` or `make test-e2e-install-manifest` with no `PROJECT_IMAGE`.
- Make cleans cluster first (clean-install validation), then sets up e2e infra.
- Make auto-builds and Kind-loads `$(E2E_LOCAL_IMAGE)`, then runs smoke install using that image.
- For `test-e2e-install-manifest`, `build-plain-manifests-installer` is run first to regenerate `dist/install.yaml`.

Outcome:
- Automatic local behavior with explicit clean-install validation for smoke tests.

### 5) IDE/debugger direct Go run
- Run `go test ./test/e2e/...` directly (no Make entrypoint).
- `BeforeSuite` checks `PROJECT_IMAGE`.
- If missing, it calls Make targets to prepare cluster + local image.

Outcome:
- IDE path works without requiring developers to remember pre-steps.

## Implementation Mapping

### Makefile
- `E2E_LOCAL_IMAGE`: single local fallback image variable.
- `e2e-build-load-image`: local image build + Kind load.
- `test-e2e`: reuses cluster; branches image behavior based on `PROJECT_IMAGE`.
- `test-e2e-install`: shared install-smoke entry with `PROJECT_IMAGE` branching:
  - prebuilt image path: skip cleanup.
  - local fallback path: cleanup cluster, setup infra, build/load local image.
- `test-e2e-install-helm`: wrapper to `test-e2e-install`.
- `test-e2e-install-manifest`:
  - local path: run `build-plain-manifests-installer` first.
  - prebuilt path: use existing manifest artifact.

### Go (`test/e2e/e2e_suite_test.go`)
- `BeforeSuite`:
  - if `PROJECT_IMAGE` is set: no prep
  - else: call Make for cluster/image prep (IDE fallback)

### Kind cluster bootstrap (`test/e2e/kind/start-cluster.sh`)
- Reuses existing Kind cluster if present (no delete/recreate in script).
- Creates cluster only when missing.
- Still exports/re-writes kubeconfig endpoint for devcontainer networking.

### Smoke script (`test/e2e/scripts/install-smoke.sh`)
- Helm mode:
  - parse `PROJECT_IMAGE` into repo/tag and override chart values.
- Manifest mode:
  - apply `dist/install.yaml`
  - if `PROJECT_IMAGE` set, patch deployment image with `kubectl set image`.
- Readiness/diagnostics selector:
  - derive pod selector dynamically from `deployment/gitops-reverser` `.spec.selector.matchLabels`.
  - avoid hardcoded label assumptions across helm/manifest paths.

## Why This Split
- Makefile remains the main orchestration layer.
- Go keeps a minimal safety-net role for IDE/direct execution.
- CI avoids redundant work by honoring prebuilt artifacts and prebuilt image.

## Tradeoffs
- We keep a small amount of orchestration in two places (Make + Go fallback), but avoid duplicated image build logic.
- Manifest image override happens post-apply (`kubectl set image`) rather than regenerating `dist/install.yaml` per image.

## Command Matrix
- CI e2e: `PROJECT_IMAGE=<prebuilt> make test-e2e`
- CI smoke helm: `PROJECT_IMAGE=<prebuilt> make test-e2e-install-helm`
- CI smoke manifest: `PROJECT_IMAGE=<prebuilt> make test-e2e-install-manifest`
- Local full e2e: `make test-e2e`
- Local smoke helm: `make test-e2e-install-helm`
- Local smoke manifest: `make test-e2e-install-manifest`
- IDE direct: `go test ./test/e2e/...`

## Failure Modes and Diagnostics
- Wrong image in pods:
  - Check deployment image: `kubectl -n gitops-reverser get deploy gitops-reverser -o yaml | rg image:`
- Image pull failures in Kind:
  - Ensure local build/load ran or `PROJECT_IMAGE` points to reachable registry.
- Manifest smoke using stale image:
  - Local path: verify `build-plain-manifests-installer` ran before smoke target.
  - CI/prebuilt path: verify artifact source and `kubectl set image` override message.
- Pod readiness says "no matching resources":
  - Verify selector in smoke logs (`Pod selector: ...`) and deployment selector labels.

## Future Improvements
- Add a small shared Make macro/helper to reduce repeated `PROJECT_IMAGE` branching across e2e entrypoints.
- Optionally add an explicit `E2E_AUTO_PREPARE_IMAGE=false` switch for strict mode in advanced local workflows.
