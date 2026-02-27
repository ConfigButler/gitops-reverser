# Makefile E2E Image + Deployment Refactor Plan

This plan refactors two E2E Makefile abstractions that are currently spread across multiple targets and scripts:

1) **Image resolution**: “what image should the tests use?” (local dev build vs CI-provided image)
2) **Deployment readiness**: “after *any* install method, the controller is running with the desired image”

It is intended to complement:

- `docs/makefile-e2e-overview-and-plan.md` (roadmap)
- `docs/e2e-shared-infra-parallel-plan.md` (shared-infra ownership contract)

## Problems to solve (current state)

- Image logic is duplicated:
  - `E2E_IMAGE := $(if $(PROJECT_IMAGE),...)` hides whether we’re using a CI-provided image or a local build.
  - Multiple targets check `PROJECT_IMAGE` and branch differently (main e2e vs quickstart flows).
- `$(CS)/$(NAMESPACE)/controller.deployed` is not a “deployment contract” yet:
  - It applies `config/` (install-method-specific).
  - It patches a deployment **by name** (`deploy/gitops-reverser`).
  - It implicitly assumes config-dir installation behavior.

## Target contracts (end-state)

### Contract A — `PROJECT_IMAGE` becomes the single source of truth

- If CI pre-builds an image, it passes `PROJECT_IMAGE=<ref>` into `make test-e2e` / quickstart targets.
- If `PROJECT_IMAGE` is **not** provided, Make chooses a default local image reference and builds it.
- Only one place in the Makefile “decides” and “branches” based on whether `PROJECT_IMAGE` was provided.
- After that point, all recipes can assume `PROJECT_IMAGE` is non-empty.

Practical consequences:

- No more `E2E_IMAGE` indirection (or it becomes an alias of `PROJECT_IMAGE`).
- No more scattered `if [ -z "$(PROJECT_IMAGE)" ] ...` checks outside the image-resolve/build step.

### Contract B — `controller.deployed` becomes install-method-agnostic

`$(CS)/$(NAMESPACE)/controller.deployed` means:

- “There is exactly one Deployment in `$(NAMESPACE)`”
- “That Deployment is configured to run `$(PROJECT_IMAGE)`”
- “That Deployment has `imagePullPolicy` set to the desired value for E2E (expected: `IfNotPresent`)”
- “That Deployment has completed rollout”

Constraints:

- It finds the Deployment dynamically (not by a hardcoded name).
- It fails fast if it finds **0** deployments or **>1** deployments (and prints what it found).
- It does **not** apply `config/` or otherwise “install” anything. Installation is a separate concern.

## Proposed Makefile structure

### 1) Resolve/build the image once

Add one stamp target that is the only place allowed to branch on “was `PROJECT_IMAGE` provided?”:

- `$(IS)/project-image.ready`

Recipe behavior:

- If `PROJECT_IMAGE` was provided:
  - Ensure the image exists in the local container runtime (`docker pull` or `docker image inspect` + pull-on-miss).
  - Write the stamp.
- If `PROJECT_IMAGE` was not provided:
  - Set `PROJECT_IMAGE` to the default local tag (e.g. `gitops-reverser:e2e-local`).
  - Build it (reusing `$(IS)/controller.id` for cache correctness and optional annotations).
  - Write the stamp.

Makefile variable pattern to support this cleanly:

- Capture “provided or not” *before* defaulting:
  - `PROJECT_IMAGE_PROVIDED := $(strip $(PROJECT_IMAGE))`
- Then default and export:
  - `PROJECT_IMAGE ?= $(E2E_LOCAL_IMAGE)`
  - `export PROJECT_IMAGE`

Only `$(IS)/project-image.ready` should read `PROJECT_IMAGE_PROVIDED`.

### 2) Always load the resolved image into Kind (all E2E clusters)

Update `$(CS)/image.loaded` to:

- depend on `$(IS)/project-image.ready` and `$(CS)/ready`
- run `kind load docker-image $(PROJECT_IMAGE) --name $(CLUSTER_FROM_CTX)`

Rationale:

- Kind nodes should not need registry credentials for CI images.
- The rest of the system can rely on `imagePullPolicy=IfNotPresent` and fast startup.

### 3) Separate install from “image + rollout” deployment contract

Make `install-$(INSTALL_MODE)` responsible only for “resources exist” (Helm / dist manifests / config-dir).

Make `$(CS)/$(NAMESPACE)/controller.deployed` responsible only for:

1. Assert exactly one Deployment in namespace
2. Patch its container image to `$(PROJECT_IMAGE)`
3. Patch its `imagePullPolicy` (expected: `IfNotPresent`)
4. Wait for rollout readiness

Container selection strategy (no deployment-name assumptions):

- Prefer a single container whose image contains `gitops-reverser` (expected unique match).
- If that’s ambiguous, fail with a diagnostic dump of container names/images.

## Dependency graph impact (high level)

- `$(CS)/$(NAMESPACE)/e2e/prepare` should depend on:
  - `$(CS)/$(NAMESPACE)/namespace.cleaned`
  - `$(CS)/$(NAMESPACE)/install-$(INSTALL_MODE)`
  - `$(CS)/image.loaded`
  - `$(CS)/$(NAMESPACE)/controller.deployed`
  - `$(CS)/portforward.running`
  - `$(CS)/$(NAMESPACE)/age-key.applied`

Quickstart targets should also rely on the same image resolution + loading primitives (no custom branching).

## Phased implementation plan (small PRs)

### PR5a — Normalize `PROJECT_IMAGE` (image resolution)

- Introduce `PROJECT_IMAGE_PROVIDED`, default `PROJECT_IMAGE`, and `export PROJECT_IMAGE`.
- Add `$(IS)/project-image.ready` and move all branching there.
- Update `$(CS)/image.loaded` to load `$(PROJECT_IMAGE)` (not `$(E2E_LOCAL_IMAGE)`).
- Remove `E2E_IMAGE` (or make it a strict alias), and remove `PROJECT_IMAGE` checks from quickstart targets.

Acceptance:

- `make test-e2e` and quickstart targets have exactly one “decision point” for CI vs local image.
- Both local and CI runs result in `$(PROJECT_IMAGE)` existing locally and being loadable into Kind.

### PR5b — Redefine `controller.deployed` as the install-agnostic contract

- Stop applying `config/` in `$(CS)/$(NAMESPACE)/controller.deployed`.
- Implement “exactly one Deployment in namespace” discovery + failure modes.
- Patch image + pull policy + rollout wait without assuming a deployment name.

Acceptance:

- `controller.deployed` works after `install-config-dir`, `install-helm`, and `install-plain-manifests-file`.
- If the namespace contains 0 or >1 deployments, `controller.deployed` fails with a clear diagnostic.

### PR5c — Align quickstart smoke harness with the new contract

- Make quickstart Make targets perform install via `install-$(mode)` + `controller.deployed` (instead of patching in
  `run-quickstart.sh`).
- Remove or simplify the `PROJECT_IMAGE` override logic in `test/e2e/scripts/run-quickstart.sh` (it should no longer
  need to know deployment names or how the install happened).

Acceptance:

- Quickstart smoke tests no longer patch deployments directly and no longer depend on a fixed deployment name.

## Non-goals (for this refactor)

- Changing the Go E2E run-context ownership model from `docs/e2e-shared-infra-parallel-plan.md`.
- Expanding the suite × install-method matrix beyond what already exists.
