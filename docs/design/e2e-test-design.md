# E2E Test Design

## Overview

The e2e tests exercise `gitops-reverser` against a real k3d cluster, real Flux installs, a real Gitea instance,
and a real Valkey/Redis instance.

The important mental model is:

1. One `go test ./test/e2e/` invocation runs one Go package.
2. That package has one `BeforeSuite`.
3. `BeforeSuite` prepares one shared cluster install for that whole run.
4. Each repo-using e2e test file creates its own Gitea repo (via `SetupRepo(...)`) in its own test namespace.

Repo fixtures are therefore file-local, not package-global. See
[e2e-repo-in-namespace-plan.md](./e2e-repo-in-namespace-plan.md) and
[e2e-repo-in-namespace-follow-up-plan.md](./e2e-repo-in-namespace-follow-up-plan.md) for the migration record.

That sharing is intentional, but it also means a later suite can break an earlier suite's shared fixtures if the
test harness treats run-scoped artifacts like install-scoped artifacts.

## Cross-Cutting Requirements

These requirements should shape any further e2e reorganization:

- all e2e suites should remain visible and browsable in the VS Code Testing pane
- top-level `Describe` blocks should be independently runnable and safe to execute in parallel when selected together
- we should keep a high-signal subset of e2e coverage that finishes within 10 minutes on a normal local development run
- the `Taskfile` is a core part of the harness and remains the source of truth for preparing the real k3d cluster and installing required resources

In practice, that means:

- IDE discoverability matters as much as CLI convenience; the suite layout should stay compatible with standard Go and Ginkgo test discovery instead of hiding scenarios behind shell-only wrappers
- shared setup from `BeforeSuite` may stay, but mutable test state must be isolated so one `Describe` does not depend on another `Describe` having run first
- the default smoke path and focused Task targets should optimize for the "relevant subset under 10 minutes" goal, while full confidence runs can remain broader and slower
- direct `go test` runs should continue to delegate environment preparation to Task targets instead of duplicating cluster bootstrap logic in Go

## Install Mode Strategy

There are three supported install modes:

- `config-dir`
- `helm`
- `plain-manifests-file`

This is an important top-level distinction in the e2e design.

The test cluster is effectively built around one active `gitops-reverser` installation at a time. The main reason is
that only one audit webhook integration can be coupled cleanly in the current setup, so the suite is not modeled as a
multi-install environment where several independent controller installs can coexist side by side.

That means:

- the main suite should assume one standard install is already present
- most specs should validate behavior of the product, not repeatedly validate installer permutations
- the normal baseline install for those tests should be `config-dir`
- the real validation for the alternative install modes should come from the quickstart tests

## Current Entry Points

These are the canonical Task entry points on the current worktree:

| Command | Purpose |
|---|---|
| `task test-e2e` | Smoke suite only (`-ginkgo.label-filter=smoke`) |
| `task test-e2e-full` | Entire e2e package |
| `task test-e2e-manager` | Manager-focused scenarios |
| `task test-e2e-signing` | Commit-signing scenarios |
| `task test-e2e-audit-redis` | Audit webhook to Redis scenarios |
| `task test-image-refresh` | Image rebuild / reload invalidation chain |
| `task test-e2e-quickstart-helm` | Quickstart framework with Helm install |
| `task test-e2e-quickstart-manifest` | Quickstart framework with manifest install |
| `task test-e2e-bi` | Bi-directional Flux plus gitops-reverser scenario |
| `task test-e2e-demo` | Demo repo preparation flow |

`go test ./test/e2e/ -v -ginkgo.v` also works directly, because `BeforeSuite` drives setup through Task. That is
intentional: Task remains the source of truth for creating the real k3d environment, preparing shared services, and
installing the system under test.

The intended interpretation of these entry points is:

- `task test-e2e`, `task test-e2e-manager`, `task test-e2e-signing`, `task test-e2e-audit-redis`, `task test-e2e-bi`, and `task test-image-refresh` are standard controller behavior checks and should normally run on the default `config-dir` install
- `task test-e2e-quickstart-helm` is the important validation for the Helm install path
- `task test-e2e-quickstart-manifest` is the important validation for the single-file manifest install path
- `task test-e2e-full` is a full spec run, but it is still not the primary way to validate all install modes

## Lifecycle Layers

### Layer 1: Cluster-scoped shared infrastructure

These are shared across runs until you explicitly tear down the cluster:

- k3d cluster
- Flux installation
- Flux-managed support services
  - Gitea
  - Valkey
  - Prometheus stack
  - Cert-manager
  - Traefik
- Gitea organization bootstrap
- port-forwards
- age key material under the cluster stamp directory

These live under:

- `.stamps/cluster/<ctx>/...`

The cluster itself is only destroyed by:

- `task clean-cluster`

That is the main reason old repos disappear from Gitea: deleting the cluster also deletes Gitea's persistent data.

### Layer 2: Install-scoped system-under-test resources

These belong to the controller install namespace, usually `gitops-reverser`:

- rendered install manifests
- controller deployment state
- webhook TLS state
- applied SOPS secret state
- `prepare-e2e.ready`

These live under:

- `.stamps/cluster/<ctx>/<namespace>/...`

These are recreated when the install needs to be refreshed.

Although stamps may exist for multiple install renderings under the same namespace tree, the e2e harness still treats
the cluster as having one active install variant for the current run.

### Layer 3: Run-scoped Git fixtures

These belong to the current e2e run, not to the controller install:

- active repo name
- checkout path
- HTTP token
- SSH keypair
- generated `secrets.yaml`
- webhook metadata
- checkout readiness marker

On the current worktree, these live under:

- `.stamps/cluster/<ctx>/<namespace>/git-<repo>/...`

This is intentionally conservative and keeps the Task migration closer to the earlier layout, even though these
artifacts are conceptually more run-scoped than install-scoped.

### Layer 4: Per-suite Kubernetes resources

Each suite creates its own test namespace and applies its own GitProvider, GitTarget, WatchRule, ConfigMaps,
Secrets, CRDs, or Flux resources.

These namespaces are typically named:

- `<ginkgo-seed>-test-manager`
- `<ginkgo-seed>-test-signing`
- `<ginkgo-seed>-test-audit-redis`
- and so on

Those namespaces are created in `BeforeAll` and removed in `AfterAll` for the suite.

An important detail here is that the suite is responsible for seeding those namespaces first:

- each repo-using file calls `SetupRepo(ctx, testNamespace, repoName)` and applies the returned
  `RepoArtifacts.SecretsYAML` into its test namespace
- the shared `sops-age-key` is copied into each test namespace when encryption is needed

`GitProvider` and `GitTarget` do not create those Secrets by themselves. They reference Secrets that the test harness
has already copied into the target namespace.

## BeforeSuite Contract

The central setup happens in [test/e2e/e2e_suite_test.go](/workspaces/gitops-reverser/test/e2e/e2e_suite_test.go).

`ensureE2EPrepared()` is intentionally narrow:

1. Runs `task prepare-e2e` (cluster, controller install, webhook TLS, age key, port-forwards)
2. Sets the kubectl context
3. Ensures `E2E_AGE_KEY_FILE` points at the prepared age key

`BeforeSuite` does **not** bootstrap any Gitea repo. Per-file repo setup is the responsibility of each
repo-using test file via [`SetupRepo(...)`](/workspaces/gitops-reverser/test/e2e/suite_repo_test.go).

The only cluster-level variable still consumed across files is:

- `E2E_AGE_KEY_FILE`

Repo state (names, checkout paths, Secret names, optional receiver webhook info) flows through a typed
`RepoArtifacts` struct returned by `SetupRepo`, not through package-global env vars.

## Repo Model: One Repo Per E2E Test File

Each repo-using e2e file owns its own repo:

| File | Test namespace | Repo |
|---|---|---|
| `e2e_test.go` | `testNamespaceFor("manager")` | `e2e-manager-<seed>` |
| `signing_e2e_test.go` | `testNamespaceFor("signing")` | `e2e-signing-<seed>` |
| `audit_redis_e2e_test.go` | `testNamespaceFor("audit-consumer")` | `e2e-audit-redis-<seed>` |
| `quickstart_framework_e2e_test.go` | `testNamespaceFor("quickstart-framework")` | `e2e-quickstart-framework-<seed>` |
| `bi_directional_e2e_test.go` | `testNamespaceFor("bi-directional")` | `e2e-bi-directional-<seed>` |
| `demo_e2e_test.go` | fixed `vote` | fixed `demo` |
| `image_refresh_test.go` | install namespace only | no repo |

Each file calls `SetupRepo(ctx, testNamespace, repoName)` once in `BeforeAll`, stores the returned
`*RepoArtifacts` in a file-local variable, and applies `artifacts.SecretsYAML` to the test namespace.

Repo stamps live under:

- `.stamps/cluster/<ctx>/<test-namespace>/git-<repo>/`

not under the install namespace.

The `audit_redis_e2e_test.go` file additionally uses a `sync.Once`-backed `ensureAuditRedisRepo()`
helper so either of its two top-level `Describe` blocks can initialize the shared file-local repo
first without cross-container ordering coupling.

## Why CI Could Pass Even If This Was Fragile

Two reasons can both be true at once:

### 1. The problematic cleanup was conditional on reinstall paths

The failure pattern only shows up when:

1. `BeforeSuite` creates the shared repo fixtures
2. a later test calls `prepare-e2e` again
3. that reinstall path runs install cleanup
4. cleanup removes the namespace stamp tree that also contained the shared Git fixtures
5. a later suite still expects `E2E_SECRETS_YAML` or checkout metadata to exist

That is exactly why the issue surfaced around `image_refresh_test.go`: it repeatedly runs `prepare-e2e`.

### 2. CI does not necessarily exercise the exact same sequence every time

Current CI e2e matrix in [.github/workflows/ci.yml](/workspaces/gitops-reverser/.github/workflows/ci.yml:248):

- `quickstart-helm-and-makefile-image-refresh`
- `quickstart-manifest`
- `core`

The `core` job currently runs:

- `task test-e2e`
- `task test-e2e-bi`

On this worktree, `task test-e2e` is now smoke-only. Before that, it was broader.

So "CI ran before" does not prove the fixture boundaries were healthy. It only proves the exercised combination of:

- suite selection
- order
- random seed
- reinstall points

did not trip over the shared-fixture deletion at that time.

## Does Anything Delete the Whole `$CS/$NS` Directory?

Yes.

That happens in [hack/cleanup-installs.sh](/workspaces/gitops-reverser/hack/cleanup-installs.sh:32).

Specifically, for each namespace with an install stamp it does:

where:

- `base=.stamps/cluster/${CTX}`
- `ns` is the install namespace, usually `gitops-reverser`

The script still cleans install-scoped data under that namespace tree, but it now preserves `git-*` directories so the
shared repo fixtures survive an install refresh during the same e2e run.

## Current Stamp Layout

On the current worktree, the important paths are:

### Cluster and shared services

- `.stamps/cluster/<ctx>/ready`
- `.stamps/cluster/<ctx>/flux.installed`
- `.stamps/cluster/<ctx>/flux-setup.ready`
- `.stamps/cluster/<ctx>/services.ready`
- `.stamps/cluster/<ctx>/gitea/bootstrap/...`

### Install namespace

- `.stamps/cluster/<ctx>/<namespace>/config-dir/install.yaml`
- `.stamps/cluster/<ctx>/<namespace>/helm/install.yaml`
- `.stamps/cluster/<ctx>/<namespace>/plain-manifests-file/install.yaml`
- `.stamps/cluster/<ctx>/<namespace>/controller.deployed`
- `.stamps/cluster/<ctx>/<namespace>/sops-secret.yaml`
- `.stamps/cluster/<ctx>/<namespace>/sops-secret.applied`
- `.stamps/cluster/<ctx>/<namespace>/webhook-tls.ready`
- `.stamps/cluster/<ctx>/<namespace>/prepare-e2e.ready`

### Run-scoped Gitea artifacts

- `.stamps/cluster/<ctx>/<namespace>/git-<repo>/active-repo.txt`
- `.stamps/cluster/<ctx>/<namespace>/git-<repo>/checkout-path.txt`
- `.stamps/cluster/<ctx>/<namespace>/git-<repo>/token.txt`
- `.stamps/cluster/<ctx>/<namespace>/git-<repo>/secrets.yaml`
- `.stamps/cluster/<ctx>/<namespace>/git-<repo>/checkout.ready`

### Local Git checkout

- `.stamps/repos/<repo>/`

## Suite Inventory

The e2e package currently contains:

| Suite | Label(s) | Purpose |
|---|---|---|
| `e2e_test.go` | `manager` | Main reconciliation and GitOps behavior |
| `signing_e2e_test.go` | `signing` | Commit signing behavior |
| `audit_redis_e2e_test.go` | `audit-redis` | Audit webhook to Redis producer/consumer |
| `image_refresh_test.go` | `image-refresh` | Build/load/restart invalidation chain |
| `quickstart_framework_e2e_test.go` | `quickstart-framework` | Installer-level quickstart flow |
| `bi_directional_e2e_test.go` | `bi-directional` | Flux plus gitops-reverser shared ownership |
| `demo_e2e_test.go` | `demo` | Demo repo preparation, intentionally leaves resources |

## Smoke Suite Definition

`task test-e2e` now runs only `Label("smoke")`.

The smoke set is intended to answer one question:

> Does the system work end to end at a high level?

Currently that includes:

- core controller health and metrics
- audit webhook receipt
- real GitProvider validation
- secret encryption path
- ConfigMap create/delete Git commits
- cluster-scoped CRD install Git commit
- audit-to-Redis producer/consumer path
- one signing verification path

That keeps the default run closer to a product smoke test than to a full infra regression sweep.

It should also stay anchored to the standard `config-dir` install. The smoke suite is not meant to prove all install
methods on every run.

## Recommended Way To Run E2E

### Default local validation

Use:

```bash
task test-e2e
```

This should stay relatively high-signal and avoid rebuild-heavy or workflow-heavy scenarios.

Conceptually, this means:

- validate the product end to end on the standard `config-dir` install
- do not treat this as an install-matrix job

### Feature-focused runs

Use:

```bash
task test-e2e-manager
task test-e2e-signing
task test-e2e-audit-redis
task test-image-refresh
task test-e2e-bi
```

### Install-path validation

Use:

```bash
task test-e2e-quickstart-helm
task test-e2e-quickstart-manifest
```

These are the important checks for the non-default install modes.

In other words:

- Helm install validation belongs here
- single-file manifest install validation belongs here
- the rest of the suite should not keep re-testing those install paths unless there is a very specific reason

### Full confidence run

Use:

```bash
task test-e2e-full
```

This is better suited for:

- larger refactors
- CI jobs intended to catch slow-path regressions
- release validation

## Notes About Commit Signing Validation

The signing suite now checks both:

- `ssh-keygen -Y verify`
- `git verify-commit`

That is intentional:

- `ssh-keygen -Y verify` proves the raw signature matches the exact reconstructed payload
- `git verify-commit` proves Git itself accepts the signature with SSH trust configured

Using both catches different classes of regression.

## Open Design Question

The main open decision is not whether run-scoped Git fixtures should be reused. They should.

The real question is where they should live:

- under the install namespace stamp tree
- or under a dedicated run-scoped Gitea tree

This worktree currently keeps them under the install namespace stamp tree and relies on cleanup preserving `git-*`
directories during reinstall flows.

## Current Preference

The current preference is to keep this migration conservative and avoid changing the storage model more than needed.

That means the following perspective is valid and intentional:

- the generated secrets, SSH keys, and similar helper artifacts are coupled to the specific e2e run
- they are also coupled to the specific install state that exists in that cluster at that moment
- the long-lived local Git checkout is already tracked separately under `.stamps/repos/<repo>`
- because of that, it is reasonable to keep them under `.stamps/cluster/<ctx>/<namespace>/git-<repo>/` for now

In other words:

- the checkout itself is clearly reusable across runs
- some of the generated credentials and helper manifests are much more ephemeral
- preserving the namespace-scoped layout may be preferable in the short term if it keeps the Task migration closer to the old behavior

So the document should not treat a separated `gitea/runs/` layout as the only correct answer. It is one clean option,
but not the only defensible one.
