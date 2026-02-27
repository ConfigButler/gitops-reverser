# Makefile E2E Overview and Implementation Plan

This is the high-level roadmap for the Makefile-driven E2E work. For the detailed stamp dependency graph, see
`docs/make-e2e-deps.md`.

## Goals (from Makefile + suite comments)

- Keep `make test-e2e` as the classic developer entrypoint.
- Use a **single seed** to scope a test run (namespaces + stamps), shared by Make and Go.
- Make the E2E model explicit as a **matrix**: `suite` × `install-method`.
- Keep Go focused on scenario logic/assertions; Make owns cluster orchestration (Kind/Helm/kubectl/image/stamps).

## Current reality (as of Feb 27, 2026)

- There is a stamp-based infra model under `.stamps/cluster/<CTX>/...` and `.stamps/image/...`.
- The full-suite Go code shells out to Make in `BeforeSuite`:
  - runs `$(CS)/$(NAMESPACE)/e2e/prepare` (stamp; no tests inside)
  - runs `portforward-ensure` to guarantee live port-forwards for the test process
  - sets `kubectl config use-context $(CTX)` for helpers that don’t pass `--context`
- `make test-e2e` runs `go test ./test/e2e/...` and the Go suite owns infra prep via the Make targets above.
- Quickstart manifest uses `dist/install.yaml` (no missing Make targets).

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

- input: Ginkgo random seed (internal to the Go suite)
- output: seed-suffixed `INSTALL_NAME` + `NAMESPACE` passed into Make from Go
  - rationale: the install name must include the seed because quickstart installs include cluster-scoped resources
    that would otherwise collide by name

The full suite should start using the seed-suffixed run namespace for test-created resources so concurrent runs
don’t collide. Keep `sut` as the controller namespace initially (parameterize later).

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

### PR1 — Fix-first correctness (done)

- Convert the `// ...` “notes” in `Makefile` into Make comments (`# ...`) so they are not treated as recipe lines.
- Re-wire `make test-e2e` to actually run the full E2E suite again (and keep the alias stable).
- Fix `test-e2e-quickstart-manifest` to depend on `dist/install.yaml` (or rename/add the intended target).
- Make `$(CS)/portforward.running` a real stamp target (remove `.PHONY`) and add a health-check fast-path.

Acceptance:

- `make test-e2e` runs the suite end-to-end on a prepared cluster.
- Quickstart helm+manifest targets work locally without undefined targets.

### PR2 — Implement `e2e/prepare` as the single orchestration contract (done)

- Implement `$(CS)/$(NAMESPACE)/e2e/prepare` (stamp) as the “one target Go calls”.
- Update Go `BeforeSuite` to call `e2e/prepare` (and `portforward-ensure`) and set kubectl context.

Acceptance:

- `go test ./test/e2e/...` works when invoked from CI/Make and always provisions infra via `e2e/prepare`.

### PR3 — Run scoping via seed

- Use the Go suite’s seed as the single run identifier and pass only:
  - `INSTALL_NAME=<base>-<seed>`
  - `NAMESPACE=<base>-<seed>`
- Decide which resources should be seed-scoped:
  - quickstart installs: must be seed-scoped (cluster-scoped resource name collisions)
  - full suite: start with seed-scoped stamps only, then migrate test-created resources next

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

## Why `manifests` and `helm-sync` are `.PHONY`

They are command-style “entrypoint” targets, not real files.

- They don’t produce a file named `manifests` or `helm-sync`; the real outputs are file targets like
  `config/crd/bases/*.yaml` and `charts/gitops-reverser/crds/*.yaml`.
- Marking them `.PHONY` avoids accidental “up to date” behavior if a file named `manifests` or `helm-sync`
  exists in the repo (or gets created by tooling).
- Incremental behavior still comes from the file targets: `.PHONY` only forces Make to *check* the graph when
  you run `make manifests` / `make helm-sync`.
