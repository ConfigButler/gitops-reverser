# Contributing to GitOps Reverser

Contributions are welcome — code, docs, bug reports, and ideas.

For security-sensitive reports, please use the private reporting path in [SECURITY.md](SECURITY.md)
instead of opening a public issue.

## Development environment

Use the DevContainer. It includes Go, kubectl, k3d, Helm, and all required tools.
Everything below assumes you are running inside it.

[![Open in GitHub Codespaces](https://github.com/codespaces/badge.svg)](https://codespaces.new/ConfigButler/gitops-reverser)

See [`.devcontainer/README.md`](.devcontainer/README.md) for setup instructions.

An optional repo-root `.env` file can provide read-only `gh` CLI access inside the devcontainer for
checking PRs, Actions runs, and CI logs. Keep it local-only; the existing `.gitignore` already
covers `.env`.

## Defaults values

You can access common services as part of the e2e cluster
* gitea: http://localhost:13000/ (giteaadmin/giteapassword123)
* redis://default:e2e-valkey-password@127.0.0.1:16379/0

## Before submitting a PR

For small fixes, docs improvements, and obvious bug fixes, feel free to open a PR directly.

For larger changes, new features, architectural shifts, or anything likely to take significant
effort, please open an issue or discussion first so direction can be aligned before you spend a lot
of time on implementation.

As a rough rule: if the change is likely to take more than about 30-60 minutes of focused work, or
more than a few thousand AI tokens / a substantial coding session to prepare, check first.

```bash
task lint      # must pass
task test      # must pass
task test-e2e  # must pass (requires Docker running)
```

## Unit tests

```bash
task test
```

This regenerates manifests and codegen, runs `go fmt` and `go vet`, downloads envtest
assets, and runs all packages except `test/e2e`. Coverage is written to `cover.out`:

```bash
go tool cover -html=cover.out
```

`task test` also enforces a coverage ratchet (`task cover-check`): it fails if total coverage drops
more than ~0.5% below `.coverage-baseline`, a committed high-water mark. When your change improves
coverage the baseline **auto-raises** — commit the updated `.coverage-baseline` alongside your
change so the floor advances. On PRs, [Codecov](https://codecov.io/gh/ConfigButler/gitops-reverser)
reports the merged unit + e2e coverage.

Run a single package or test:

```bash
go test ./internal/controller -v
go test ./internal/git -run TestName -v
```

## E2E tests

```bash
task test-e2e
```

E2E runs against a real k3d cluster with Gitea and Prometheus. The cluster is
**intentionally not torn down on failure** — this makes debugging easier and reruns faster.
Tests are written to append to existing state rather than require a clean slate.

For deeper debugging, see [`test/e2e/E2E_DEBUGGING.md`](test/e2e/E2E_DEBUGGING.md).

### E2E coverage

To measure how much of the controller the e2e suite exercises, build an instrumented image and
flush coverage from the running pod after the suite:

```bash
E2E_COVERAGE=1 task test-e2e
task e2e-coverage-collect   # writes e2e-cover.out
```

CI does this automatically on the `full` matrix and uploads it to Codecov under the `e2e` flag.

## Tilt loop

For controller iteration against the shared e2e cluster:

```bash
tilt up
```

The Tilt UI keeps build/deploy work on the existing `task prepare-e2e` path, then adds a small
`playground` group of manual resources:

- `playground-bootstrap` creates a reusable example repo, the `tilt-playground` namespace, the
  Git credentials, and the `sops-age-key` Secret
- the starter `GitProvider`, `GitTarget`, and `WatchRule` live in
  [`test/playground/`](test/playground/) and are auto-applied by Tilt via kustomize once the
  bootstrap has run
- `playground-cleanup` removes the fixed playground namespace, repo, and local artifacts
- `playground-upsert-*` / `playground-delete-*` mutate watched resources so you can quickly
  confirm create, update, delete, and Secret encryption flows
- `playground-status` prints the current starter resources and the recent repo commit log

The design note for the playground flow lives in
[`docs/design/tilt-playground-plan.md`](docs/design/tilt-playground-plan.md).

**Troubleshooting:**
- envtest errors: run `task setup-envtest` then retry
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

## AI tooling

Repo-specific guidance for AI coding assistants lives in [AGENTS.md](AGENTS.md).

## Pull request process

1. Branch from `main`
2. Make changes, follow the validation steps above
3. Open a PR against `main` — CI runs automatically

### Pull requests from forks

External contributions come from a fork, and GitHub gives fork PRs a **read-only**
`GITHUB_TOKEN` — it can pull images from `ghcr.io` but cannot push to the org's
packages, and it has no access to repository secrets. CI adapts automatically so
that fork PRs still run the **full** pipeline (lint, unit, e2e) without needing any
write access:

- **Images are built but not pushed.** The project image and CI base container are
  built locally in the PR run. Nothing is published to `ghcr.io` from a fork.
- **Images move between jobs as artifacts, not via the registry.** The project image
  is `docker save`d, uploaded, and the e2e job loads it straight into the k3d cluster
  (`IMAGE_DELIVERY_MODE=load`) instead of pulling it.
- **Container-based jobs reuse the published CI image.** `lint`, `test`, and the Helm
  job run inside the last published `ghcr.io/configbutler/gitops-reverser-ci:latest`
  rather than a fresh per-commit build. If your PR changes `.devcontainer/Dockerfile`,
  the toolchain build itself is still validated (the container is built and its tools
  are checked), but those jobs run against the previously published toolchain — a
  maintainer will rebuild/republish the base image when toolchain changes merge.

Nothing is published from a PR regardless of origin: image and chart publishing only
happen on push to `main` (via release-please). Trusted (same-repo) branches keep
pushing per-commit CI images to the registry as before.

Maintainers: this requires the `gitops-reverser-ci` package to be **public** so fork
runs can pull `:latest` without credentials (the project image is already public).
Set it under *Org → Packages → gitops-reverser-ci → Package settings → Change
visibility → Public*.

Thank you for contributing!
