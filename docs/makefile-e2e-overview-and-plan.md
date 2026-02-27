# Makefile E2E Overview and Implementation Plan

This is the high-level roadmap for the Makefile-driven E2E work. For the detailed stamp dependency graph, see
`docs/make-e2e-deps.md`.

## Goals (from Makefile + suite comments)

- Keep `make test-e2e` as the classic developer entrypoint.
- Use a **single seed** to scope a test run (namespaces + stamps), shared by Make and Go.
- Make the E2E model explicit as a **matrix**: `suite` × `install-method`.
- Keep Go focused on scenario logic/assertions; Make owns cluster orchestration (Kind/Helm/kubectl/image/stamps).

## Current reality (as of Feb 26, 2026)

- There is a stamp-based infra model under `.stamps/cluster/<CTX>/...` and `.stamps/image/...`.
- The full-suite Go code already shells out to Make in `BeforeSuite` (currently to ensure port-forwards).
- `Makefile` contains WIP placeholders:
  - `$(CS)/$(NAMESPACE)/e2e/prepare:` exists but has no recipe yet.
  - `test-e2e` is currently wired to only run `cleanup-namespace-sut` (it does not run the suite).
  - `test-e2e-quickstart-manifest` references `build-plain-manifests-installer` (not defined).
  - `$(CS)/portforward.running` is `.PHONY`, so stamps can’t skip restarts.

## Design contract (end-state)

### 1) One “prepare” target that Go calls

Go should call exactly one Make target in `BeforeSuite`:

- `$(CS)/$(NAMESPACE)/e2e/prepare` (stamp; no `go test` inside)

That stamp is responsible for:

- cleaning + recreating the controller namespace (`sut` for now)
- running the selected installer (`INSTALL_MODE=...`)
- ensuring the controller is deployed with the correct image
- ensuring shared test dependencies are ready (port-forwards, age key, etc.)

Then Go runs tests normally and only owns scenario logic/assertions.

### 2) Seed → run namespace (shared by Make + Go)

Inputs/outputs:

- input: `E2E_GINKGO_RANDOM_SEED` (already used by the suite)
- derived: `E2E_RUN_SEED` (defaults to `E2E_GINKGO_RANDOM_SEED`, fallback to timestamp)
- derived: `E2E_RUN_NS=run-$(E2E_RUN_SEED)`

The full suite should start using `E2E_RUN_NS` for test-created resources so concurrent runs don’t collide.
Keep `sut` as the controller namespace initially (parameterize later).

### 3) Makefile matrix: suite × install-method

Keep the matrix intentionally small:

- `suite=full` with `INSTALL_MODE=config-dir` (internal/dev install path)
- `suite=quickstart` with `INSTALL_MODE=helm|plain-manifests-file` (user-facing install paths)

Stable entrypoints:

- `make test-e2e` (alias for full + config-dir)
- `make test-e2e-quickstart-helm`
- `make test-e2e-quickstart-manifest`

Optional later (only if it proves valuable): `suite=full` with `INSTALL_MODE=helm|plain-manifests-file`.

## Implementation plan (small PRs with clear outcomes)

### PR1 — Fix-first correctness (unblocks everything)

- Convert the `// ...` “notes” in `Makefile` into Make comments (`# ...`) so they are not treated as recipe lines.
- Re-wire `make test-e2e` to actually run the full E2E suite again (and keep the alias stable).
- Fix `test-e2e-quickstart-manifest` to depend on `dist/install.yaml` (or rename/add the intended target).
- Make `$(CS)/portforward.running` a real stamp target (remove `.PHONY`) and add a health-check fast-path.

Acceptance:

- `make test-e2e` runs the suite end-to-end on a prepared cluster.
- Quickstart helm+manifest targets work locally without undefined targets.

### PR2 — Implement `e2e/prepare` as the single orchestration contract

- Implement `$(CS)/$(NAMESPACE)/e2e/prepare` (stamp) as the “one target Go calls”.
- Update Go `BeforeSuite` to call `e2e/prepare` (instead of calling `portforward.running` directly).

Acceptance:

- `go test ./test/e2e/...` works when invoked from CI/Make and always provisions infra via `e2e/prepare`.

### PR3 — Run scoping via seed

- Add `E2E_RUN_SEED` + `E2E_RUN_NS` to Make.
- Add a run-namespace stamp and propagate env vars into `go test`.
- Update Go helpers/tests to use `E2E_RUN_NS` for test-created resources.

Acceptance:

- Two runs with different seeds can share a cluster without namespace collisions.

### PR4 — Reduce shell quickstart assertions (Go parity)

- Port `run-quickstart.sh` assertions into Go (commit progression, encryption checks, invalid-credential status).
- Keep the shell script as a thin bootstrap wrapper or retire it once parity is reached.

Acceptance:

- Quickstart smoke tests’ “real assertions” live in Go, not shell.

## Notes / non-goals (for now)

- Don’t expand the suite/install matrix until the `e2e/prepare` contract and run scoping are solid.
- Avoid moving Kubernetes orchestration into Go; prefer Make stamps and verified gates.
