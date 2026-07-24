# Contributing to GitOps Reverser

Contributions are welcome: code, docs, bug reports, and ideas.

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

You can access common services as part of the e2e cluster:

- gitea: <http://localhost:13000/> (giteaadmin/giteapassword123)
- redis://default:e2e-valkey-password@127.0.0.1:16379/0

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

Editing markdown? Follow [`docs/style-guide.md`](docs/style-guide.md). A docs-only change runs
`task lint-docs` instead of the full suite; `git add` new files first so their links resolve.
See [Documentation checks](#documentation-checks) for what that runs.

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
coverage the baseline **auto-raises**, so commit the updated `.coverage-baseline` alongside your
change so the floor advances. On PRs, [Codecov](https://codecov.io/gh/ConfigButler/gitops-reverser)
reports the merged unit + e2e coverage.

Run a single package or test:

```bash
go test ./internal/controller -v
go test ./internal/git -run TestName -v
```

## Dynamic analysis (fuzzing)

The project fuzzes the code that parses untrusted or semi-structured input (manifest
editing in `internal/git/manifestedit`, and audit/admission request decoding in
`internal/webhook`) so hostile input can never crash the controller. `task test`
already replays every fuzz seed and committed crash reproducer as ordinary regression
cases, so you get that coverage for free on every run.

Before a major production release, run a longer discovery pass:

```bash
task fuzz          # release-time discovery run, per target, under the race detector
task fuzz-smoke    # short active-fuzz smoke of each target
```

If fuzzing finds a crash, Go writes the reproducer under
`<package>/testdata/fuzz/<Target>/`. Commit it: it fixes the input as a permanent
regression case, replayed by `task test` thereafter. See
[`docs/finished/dynamic-analysis-fuzzing-plan.md`](docs/finished/dynamic-analysis-fuzzing-plan.md).

## E2E tests

```bash
task test-e2e
```

E2E runs against a real k3d cluster with Gitea and Prometheus. The cluster is
**intentionally not torn down on failure**, which makes debugging easier and reruns faster.
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
[`docs/finished/tilt-playground-plan.md`](docs/finished/tilt-playground-plan.md).

**Troubleshooting:**

- envtest errors: run `task setup-envtest` then retry
- Docker errors: ensure the Docker daemon is running
- Permission issues: see [`docs/ci/go-module-permissions.md`](docs/ci/go-module-permissions.md)
- Windows: see [`docs/ci/windows-devcontainer.md`](docs/ci/windows-devcontainer.md)

## Documentation checks

`task lint-docs` runs all three, and `task lint` includes it. Each owns one thing and none of them
overlaps with the others:

| Task | Tool | Checks |
|---|---|---|
| `task lint-doc-links` | `hack/doccheck` | Every reference resolves, including repo-relative doc paths cited inside Go comments, YAML, and shell. No off-the-shelf tool reads those. |
| `task lint-markdown` | markdownlint-cli2 | Structure: fences, headings, lists, blank lines, bullet style. |
| `task lint-prose` | Vale | English against [`docs/style-guide.md`](docs/style-guide.md): em dashes, American spelling, product names, words to cut. |

```bash
task lint-docs                  # links and structure everywhere; prose on the gated files
task lint-markdown-fix          # apply the safe mechanical fixes
task lint-prose DOCS_SCOPE=all  # the whole tree, to see the prose backlog
```

**Links and structure are checked on every tracked file; only prose is gated on the files
[`.docs-lint-scope`](.docs-lint-scope) lists.** Structure was staged the same way until the residue
was cleared, so markdownlint now gates the whole tree and a new document cannot regress it. Vale
still cannot: 145 of the 167 linted files carry at least one error, almost all of them em dashes,
and [`docs/style-guide.md`](docs/style-guide.md) says that cleanup must not land as one sweeping
commit. That gate stays small and the list grows.

**To add a file to the prose gate**, clean it and add its path to that file:

```bash
vale docs/some-file.md   # what it would cost
```

Editing a doc that is not on the list still passes prose, so an unlisted file can drift. Run `vale`
on anything you touch even when nothing forces you to.

**Reference checking is not staged this way.** `task lint-doc-links` always covers every tracked
markdown, Go, YAML, and shell file, so a moved document breaks the build wherever it is cited.

Only Vale errors fail the build. Warnings and suggestions print and do not block, because
[`.vale.ini`](.vale.ini) reserves the error level for rules a machine can decide alone. There is no
autofix for prose, on purpose. `task lint-markdown-fix` does not clear everything either: over the
whole tree it fixes about two thirds and leaves the rest for a human, led by long lines, missing
fence languages, and bold text used as a heading.

To add a word, a preferred term, or an exception, edit the rule under `.vale/styles/HouseStyle/`;
each file explains what it costs, measured against the real corpus. To suppress one genuinely
exceptional finding, use `<!-- vale HouseStyle.RuleName = NO -->` around the smallest span that
needs it, never a repository-wide exclusion.

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
3. Open a PR against `main`; CI runs automatically

### Pull requests from forks

External contributions come from a fork, and GitHub gives fork PRs a **read-only**
`GITHUB_TOKEN`: it cannot push to the org's packages and has no access to repository
secrets. CI adapts automatically so that fork PRs still run the **full** pipeline
(lint, unit, e2e, image scan) without needing any write access:

- **Images are built but never pushed.** The CI base container and the project image
  are built from your PR's own code in the PR run, so a PR that changes
  `.devcontainer/Dockerfile` is validated against its own toolchain.
- **Images move between jobs as artifacts, not via the registry.** They are
  `docker save`d, uploaded, and later jobs `docker load` them; the e2e job imports the
  project image straight into k3d (`IMAGE_DELIVERY_MODE=load`) instead of pulling it.
- **Same checks as maintainers.** PRs and pushes to `main` run the same jobs from the
  same workflow file ([ci.yml](.github/workflows/ci.yml)); only the image delivery
  differs.

Nothing is published from a PR regardless of origin: image and chart publishing only
happen on push to `main` via [release.yml](.github/workflows/release.yml), after the
full pipeline passed. If this is your first contribution, a maintainer needs to
approve the workflow run before it starts. That is a GitHub safety default rather than a
distrust of your patch.

The full design (trust zones, signing, verification) is described in
[docs/ci-overview.md](docs/ci-overview.md).

Thank you for contributing!
