# Testing Guide

## Quickstart

```bash
make lint
make test
make test-e2e
```

The canonical “source of truth” for what runs is the Makefile targets and the CI workflow.

## Prerequisites

- Go (see [`go.mod`](go.mod:1))
- GNU Make

E2E-only:

- Docker (daemon running)
- Kind, kubectl, helm (all invoked by Makefile)

## Unit tests (incl. controller/envtest)

Prefer the Makefile target:

```bash
make test
```

What `make test` does (see [`Makefile`](Makefile:65)):

- regenerates manifests + codegen
- runs `go fmt` and `go vet`
- downloads kubebuilder envtest assets into `./bin`
- runs `go test` for all packages except `/test/e2e`
- writes coverage to `cover.out`

View coverage:

```bash
go tool cover -html=cover.out
```

Run a single package or a single test:

```bash
go test ./internal/controller -v
go test ./internal/git -run TestName -v
```

## End-to-end (E2E) tests

E2E tests live under `./test/e2e/` (for example [`test/e2e/e2e_suite_test.go`](test/e2e/e2e_suite_test.go:1)) and run with Ginkgo:

```bash
make test-e2e
```

Important points:

- E2E runs against a real Kind Kubernetes cluster.
- The test harness installs Gitea and Prometheus (see [`Makefile`](Makefile:199)) so tests can validate:
  - commits/pushes happening as expected
  - metrics being produced as expected
- The cluster is intentionally not torn down on failure (see [`Makefile`](Makefile:79) and [`Makefile`](Makefile:83)):
  - makes debugging easier (you can inspect the live cluster state)
  - makes reruns faster (no full cluster rebuild)
  - tests are written to “append” rather than require a clean-slate cluster

Notes (see [`Makefile`](Makefile:83)):

- `make test-e2e` will attempt to create a Kind cluster if `kind` is installed locally.
  In CI we create the cluster via GitHub Actions first.
- The default cluster name is controlled by `KIND_CLUSTER`.

If you need deeper E2E debugging steps, see [`test/e2e/E2E_DEBUGGING.md`](test/e2e/E2E_DEBUGGING.md:1).

## CI

CI runs in GitHub Actions; workflow definition: [`./.github/workflows/ci.yml`](.github/workflows/ci.yml:1).

At a high level:

- `lint` job runs golangci-lint
- `test` job runs `make test`
- `e2e-test` job provisions Kind and then runs `make test-e2e` inside the CI container

## Troubleshooting

- envtest errors (e.g. missing etcd/kube-apiserver): run `make setup-envtest` and rerun `make test`.
- docker errors for E2E: ensure Docker is installed and the daemon is running.
- permissions issues in devcontainer: see [`docs/ci/GO_MODULE_PERMISSIONS.md`](docs/ci/GO_MODULE_PERMISSIONS.md:1) and
  [`docs/ci/WINDOWS_DEVCONTAINER_SETUP.md`](docs/ci/WINDOWS_DEVCONTAINER_SETUP.md:1).
