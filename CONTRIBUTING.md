# Contributing to GitOps Reverser

Contributions are welcome — code, docs, bug reports, and ideas.

## Development environment

Use the DevContainer. It includes Go, kubectl, k3d, Helm, and all required tools.
Everything below assumes you are running inside it.

See [`.devcontainer/README.md`](.devcontainer/README.md) for setup instructions.

## Before submitting a PR

```bash
make lint      # must pass
make test      # must pass
make test-e2e  # must pass (requires Docker running)
```

## Unit tests

```bash
make test
```

This regenerates manifests and codegen, runs `go fmt` and `go vet`, downloads envtest
assets, and runs all packages except `test/e2e`. Coverage is written to `cover.out`:

```bash
go tool cover -html=cover.out
```

Run a single package or test:

```bash
go test ./internal/controller -v
go test ./internal/git -run TestName -v
```

## E2E tests

```bash
make test-e2e
```

E2E runs against a real k3d cluster with Gitea and Prometheus. The cluster is
**intentionally not torn down on failure** — this makes debugging easier and reruns faster.
Tests are written to append to existing state rather than require a clean slate.

For deeper debugging, see [`test/e2e/E2E_DEBUGGING.md`](test/e2e/E2E_DEBUGGING.md).

**Troubleshooting:**
- envtest errors: run `make setup-envtest` then retry
- Docker errors: ensure the Docker daemon is running
- Permission issues: see [`docs/ci/go-module-permissions.md`](docs/ci/go-module-permissions.md)
- Windows: see [`docs/ci/windows-devcontainer.md`](docs/ci/windows-devcontainer.md)

## Commit format

This project uses [Conventional Commits](https://www.conventionalcommits.org/). The type
determines whether a release is triggered and what version bump applies:

| Type | Effect |
|---|---|
| `feat` | minor bump |
| `fix`, `perf`, `revert` | patch bump |
| `docs`, `test`, `chore`, `ci`, `refactor`, `style`, `build` | no bump |
| `feat!` or `BREAKING CHANGE:` footer | major bump |

Examples: `feat(controller): add SSH key rotation`, `fix(webhook): prevent queue race`

See [`.github/RELEASES.md`](.github/RELEASES.md) for how releases and changelogs are automated.

## Pull request process

1. Branch from `main`
2. Make changes, follow the validation steps above
3. Open a PR against `main` — CI runs automatically

Thank you for contributing!
