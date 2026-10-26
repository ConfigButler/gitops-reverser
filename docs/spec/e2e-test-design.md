# E2E Test Design

> **spec** — current behaviour. The code depends on this document; change one, change the other. Index: [`../INDEX.md`](../INDEX.md)

## Overview

The e2e tests exercise `gitops-reverser` against a real k3d cluster, real Flux installs, a real Gitea instance,
and a real Valkey/Redis instance.

The important mental model is:

1. One `go test ./test/e2e/` invocation runs one Go package.
2. That package has one `BeforeSuite`.
3. `BeforeSuite` prepares one shared cluster install for that whole run.
4. Each repo-using e2e test file creates its own Gitea repo (via `SetupRepo(...)`) in its own test namespace.

Repo fixtures are therefore file-local, not package-global.

That sharing is intentional, but it also means a later suite can break an earlier suite's shared fixtures if the
test harness treats run-scoped artifacts like install-scoped artifacts.

## Repo-Per-File Model

The repo-per-file split is an intentional harness decision, not an accident of the current code.

The durable rules are:

- `BeforeSuite` owns shared cluster and install preparation
- each repo-using e2e file owns one repo
- repo state flows through `RepoArtifacts`, not package-global repo env vars
- repo fixtures should stay close to the test file that actually needs them
- the demo flow remains intentionally special with its fixed repo and namespace

This shape won because it gave a good trade-off between isolation and practicality:

- less hidden coupling between e2e files
- better fit for Ginkgo and VS Code test discovery
- no need to duplicate cluster bootstrap logic in Go
- easier future parallelism

What it does not promise:

- `ginkgo -p` safety for every spec: the specs that touch cluster-wide state run `Serial`
  (see [e2e-serial-registry.md](e2e-serial-registry.md))
- removal of all remaining shell from the harness
- total independence from shared cluster-side services

Those broader open items live in [e2e-finish-plan.md](../design/e2e-finish-plan.md).

## Cross-Cutting Requirements

These requirements should shape any further e2e reorganization:

- all e2e suites should remain visible and browsable in the VS Code Testing pane
- top-level `Describe` blocks should be independently runnable and safe to execute in parallel when selected together
- we should keep a high-signal subset of e2e coverage that finishes within 10 minutes on a normal local development run
- the `Taskfile` is a core part of the harness and remains the source of truth for preparing the
  real k3d cluster and installing required resources

In practice, that means:

- IDE discoverability matters as much as CLI convenience; the suite layout should stay compatible
  with standard Go and Ginkgo test discovery instead of hiding scenarios behind shell-only wrappers
- shared setup from `BeforeSuite` may stay, but mutable test state must be isolated so one
  `Describe` does not depend on another `Describe` having run first
- the default `task test-e2e` path and focused Task targets should optimize for useful local
  feedback, while install-matrix confidence comes from dedicated quickstart tasks
- direct `go test` runs should continue to delegate environment preparation to Task targets instead
  of duplicating cluster bootstrap logic in Go

## Harness Direction

The current preferred direction for the harness is:

- keep `_suite_test.go` and `BeforeSuite` focused on shared cluster preparation only
- avoid moving per-test repo setup into suite-global bootstrap
- keep each e2e file centered on one clear concern
- make top-level `Describe` blocks independently runnable without hidden ordering assumptions
- isolate mutable test fixtures so future parallel execution remains realistic

In practice, that means:

- shared infrastructure such as the cluster, controller install, webhook TLS, and port-forwards can stay suite-level
- repo creation, namespace seeding, and other scenario-specific state should stay close to the test
  file that actually needs it
- `Ordered` containers are acceptable when a workflow is inherently staged, but they should not
  become a substitute for cross-file isolation
- install mode should not reshape the product-behavior suites; installer variation belongs primarily
  in the dedicated quickstart/install-path checks

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
| `task test-e2e` | Standard e2e package, excluding the `image-refresh` workflow check and the opt-in corners |
| `task test-e2e-bi-directional` | Bi-directional corner (Flux + Argo CD); the only lane that installs Argo CD |
| `task test-e2e-source-cluster` | Source-cluster corner: kcp workspaces as remote source clusters; the only lane that installs kcp |
| `task test-e2e-ado` | Mirror into a hosted Azure DevOps repository (needs `E2E_ADO_REPO_URL` and `E2E_ADO_PAT`) |
| `task test-e2e-aggregated-api` | Aggregated API server specs only |
| `task test-image-refresh` | Image rebuild / reload invalidation chain |
| `task test-e2e-quickstart-helm` | Quickstart framework with Helm install |
| `task test-e2e-quickstart-manifest` | Quickstart framework with manifest install |
| `task test-e2e-demo` | Demo repo preparation flow |

`go test ./test/e2e/ -v -ginkgo.v` also works directly, because `BeforeSuite` drives setup through Task. That is
intentional: Task remains the source of truth for creating the real k3d environment, preparing shared services, and
installing the system under test.

The intended interpretation of these entry points is:

- `task test-e2e` is the standard controller behavior check and should normally run on the default `config-dir` install
- `task test-image-refresh` validates the rebuild/reload workflow and is run explicitly by CI rather
  than as part of the normal `task test-e2e` path
- `task test-e2e-quickstart-helm` is the important validation for the Helm install path
- `task test-e2e-quickstart-manifest` is the important validation for the single-file manifest install path
- `task test-e2e` is a broad spec run but **excludes** the opt-in corners (its label filter
  defaults to `!image-refresh && !bi-directional && !source-cluster && !ado`), because they need
  what `task prepare-e2e` does not provide (Argo CD, kcp, an Azure DevOps repository): run them
  with their own tasks

## Lifecycle Layers

### Layer 1: Cluster-scoped shared infrastructure

These are shared across runs until you explicitly tear down the cluster:

- k3d cluster
- Flux installation
- Flux-managed support services
  - Gitea
  - Valkey
  - Prometheus operator
  - Cert-manager
  - Reflector
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

- repo name, clone URLs, Secret names and receiver webhook metadata, in the `RepoArtifacts` that
  `SetupRepo` returns
- HTTP token and SSH keypair, written into the generated `secrets.yaml`
- the local checkout

These live under:

- `.stamps/e2e-repo-artifacts/<test-namespace>/<repo>/secrets.yaml`
- `.stamps/repos/<repo>/` (the checkout)

Both are outside the cluster stamp tree, so an install refresh does not touch them.

### Layer 4: Per-suite Kubernetes resources

Each suite creates its own test namespace and applies its own GitProvider, GitTarget, WatchRule, ConfigMaps,
Secrets, CRDs, or Flux resources.

These namespaces are typically named:

- `<ginkgo-seed>-test-manager-watchrule`
- `<ginkgo-seed>-test-signing`
- `<ginkgo-seed>-test-save-request`
- and so on

Those namespaces are created in `BeforeAll` and removed in `AfterAll` for the suite.

An important detail here is that the suite is responsible for seeding those namespaces first:

- each repo-using file calls `SetupRepo(ctx, testNamespace, repoName)` and applies the returned
  `RepoArtifacts.SecretsYAML` into its test namespace
- the shared `sops-age-key` is copied into each test namespace when encryption is needed

`GitProvider` and `GitTarget` do not create those Secrets by themselves. They reference Secrets that the test harness
has already copied into the target namespace.

## BeforeSuite Contract

The central setup happens in [test/e2e/e2e_suite_test.go](../../test/e2e/e2e_suite_test.go).

It is a `SynchronizedBeforeSuite`, and intentionally narrow:

1. On parallel process #1 only: takes the per-cluster run lock, runs `task prepare-e2e` (cluster,
   controller install, webhook TLS, age key, port-forwards), sets the kubectl context, removes
   every `IceCreamOrder` CRD group, ensures the shared Gitea organization
   (`prepareE2EClusterOnce`), waits out the namespaces of an earlier run with the same seed, and
   resets the observability counters
2. In every process: points `E2E_AGE_KEY_FILE` at the prepared age key when it is unset
   (`configureE2EProcess`)

`BeforeSuite` does **not** bootstrap any Gitea repo. Per-file repo setup is the responsibility of each
repo-using test file via `SetupRepo(...)`.

The only cluster-level variable still consumed across files is:

- `E2E_AGE_KEY_FILE`

Repo state (names, checkout paths, Secret names, optional receiver webhook info) flows through a typed
`RepoArtifacts` struct returned by `SetupRepo`, not through package-global env vars.

The audit-consumer specs (`Commit Window Batching`, `Save Request`) each provision their own repo
via `SetupRepo`, like every other file.

## Repo Model: One Repo Per E2E Test File

Each repo-using e2e file owns its own repo, for example:

| File | Test namespace | Repo |
|---|---|---|
| `watchrule_configmap_secret_e2e_test.go` (one of the `manager` files) | `testNamespaceFor("manager-watchrule")` | `e2e-manager-watchrule-<seed>` |
| `signing_e2e_test.go` | `testNamespaceFor("signing")` | `e2e-signing-<seed>` |
| `commit_window_batching_e2e_test.go` | `testNamespaceFor("commit-window-batching")` | `e2e-commit-window-<seed>` |
| `save_request_e2e_test.go` | `testNamespaceFor("save-request")`, and `save-request-bundle` for the bundle describe | `e2e-save-request-<seed>`, `e2e-save-request-bundle-<seed>` |
| `commit_author_attribution_e2e_test.go` | `testNamespaceFor("commit-author")` | `e2e-commit-author-<seed>` |
| `quickstart_framework_e2e_test.go` | `testNamespaceFor("quickstart-framework")` | `e2e-quickstart-framework-<seed>` |
| `flux_bi_directional_e2e_test.go` | `testNamespaceFor("bi-directional")` | `e2e-bi-directional-<seed>` |
| `argocd_bi_directional_e2e_test.go` | `testNamespaceFor("argo-bi-directional")` | `e2e-argo-bi-directional-<seed>` |
| `demo_e2e_test.go` | fixed `vote` | fixed `demo` |
| `image_refresh_test.go` | install namespace only | no repo |

Each file calls `SetupRepo(ctx, testNamespace, repoName)` once in `BeforeAll`, stores the returned
`*RepoArtifacts` in a file-local variable, and applies `artifacts.SecretsYAML` to the test namespace.

Repo artifacts live under:

- `.stamps/e2e-repo-artifacts/<test-namespace>/<repo>/`
- `.stamps/repos/<repo>/` (the checkout)

not under the cluster stamp tree.

## Why CI Could Pass Even If This Was Fragile

This section is history: it explains a failure from when the repo fixtures lived under the install
namespace's stamp tree. They now live under `.stamps/e2e-repo-artifacts/`, outside it.

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

Current CI e2e matrix in .github/workflows/ci.yml:

- `full-manager`
- `full-core`
- `bi-directional`
- `source-cluster`
- `quickstart-install`
- `image-refresh`

The `full-manager` and `full-core` legs both run `task test-e2e`, each with its own label filter.

`task test-e2e` runs the e2e package minus the opt-in corners (there is no smoke-only subset).

So "CI ran before" does not prove the fixture boundaries were healthy. It only proves the exercised combination of:

- suite selection
- order
- random seed
- reinstall points

did not trip over the shared-fixture deletion at that time.

## Does Anything Delete the Whole `$CS/$NS` Directory?

Yes.

That happens in hack/cleanup-installs.sh.

Specifically, for each namespace with an install stamp it deletes the namespace, then runs:

```bash
find "${base}/${ns}" -mindepth 1 -maxdepth 1 ! -name 'git-*' -exec rm -rf {} +
```

where:

- `base=.stamps/cluster/${CTX}`
- `ns` is the install namespace, usually `gitops-reverser`

The script cleans install-scoped data under that namespace tree and still preserves `git-*` directories, from when
the repo fixtures lived there. `SetupRepo` now writes them outside that tree.

## Current Stamp Layout

On the current worktree, the important paths are:

### Cluster and shared services

- `.stamps/cluster/<ctx>/ready`
- `.stamps/cluster/<ctx>/flux.installed`
- `.stamps/cluster/<ctx>/flux-setup.ready`
- `.stamps/cluster/<ctx>/services.ready`
- `.stamps/cluster/<ctx>/age-key.txt`

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

- `.stamps/e2e-repo-artifacts/<test-namespace>/<repo>/secrets.yaml`

### Local Git checkout

- `.stamps/repos/<repo>/`

## Suite Inventory

The e2e package includes, among others:

| Suite | Label(s) | Purpose |
|---|---|---|
| `controller_basics_e2e_test.go` and the other `manager` files | `manager` | Main reconciliation and GitOps behavior |
| `signing_e2e_test.go` | `signing` | Commit signing behavior |
| `commit_window_batching_e2e_test.go` | `commit-window-batching`, `audit-consumer` | Commit-window batching through the audit pipeline |
| `save_request_e2e_test.go` | `save-request`, `audit-consumer` | SaveRequest finalization through the audit pipeline |
| `commit_author_attribution_e2e_test.go` | `manager` | OIDC claim to Git author attribution |
| `image_refresh_test.go` | `image-refresh` | Build/load/restart invalidation chain |
| `quickstart_framework_e2e_test.go` | `quickstart-framework` | Installer-level quickstart flow |
| `flux_bi_directional_e2e_test.go` | `bi-directional`, `flux` | Flux plus gitops-reverser shared ownership |
| `argocd_bi_directional_e2e_test.go` | `bi-directional`, `argocd` | Argo CD plus gitops-reverser shared ownership |
| the other bi-directional corner files | `bi-directional` and a sub-label | See [e2e-bi-directional-corner.md](e2e-bi-directional-corner.md) |
| `demo_e2e_test.go` | `demo` | Demo repo preparation, intentionally leaves resources |

## Suite Definition

`task test-e2e` runs the standard e2e package, excluding the rebuild-heavy `image-refresh`
workflow check and the opt-in corners (`bi-directional`, `source-cluster`, `ado`). There is no
smoke-only subset and no `Label("smoke")`. The `smoke` label was removed from every spec.

It stays anchored to the standard `config-dir` install. The default run is not meant to
prove all install methods on every invocation; the dedicated `quickstart-*` tasks cover
the Helm and single-file manifest install paths.

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

CI runs `task test-image-refresh` explicitly for the rebuild/reload workflow check.

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
task test-e2e
task test-e2e-quickstart-helm
task test-e2e-quickstart-manifest
```

This is better suited for:

- larger refactors
- CI jobs intended to catch slow-path regressions and install-mode drift
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

Resolved: `SetupRepo` writes them under a dedicated tree, `.stamps/e2e-repo-artifacts/<test-namespace>/<repo>/`,
with the checkout under `.stamps/repos/<repo>/`, so an install refresh cannot remove them.

## Current Preference

Superseded by the resolution above. The earlier preference kept the generated secrets and SSH keys
under `.stamps/cluster/<ctx>/<namespace>/git-<repo>/`, coupled to the install state. They now live
under `.stamps/e2e-repo-artifacts/`, and the long-lived checkout stays under `.stamps/repos/<repo>`.
