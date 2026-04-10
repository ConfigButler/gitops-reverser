# E2E Test Design

## Overview

The e2e tests validate the gitops-reverser operator end-to-end against a real Kubernetes cluster (k3d), a real Gitea instance, and real Flux reconciliation. This document describes how the tests are structured, what is shared vs per-run, and the design principles that keep things predictable.

## Entry Points

| Command | What it does |
|---|---|
| `make test-e2e` | Classic entrypoint — always runs the full suite |
| `go test ./test/e2e/ -v -ginkgo.v` | Direct Go invocation — BeforeSuite handles all setup |
| IDE Go test plugin | Same as above — BeforeSuite handles all setup |

All three paths are equivalent: `make test-e2e` is fully `.PHONY` and always runs `go test`, which triggers `BeforeSuite` to call `make prepare-e2e` and `make e2e-gitea-run-setup` before any test executes.

## Infrastructure Layers

### Layer 1 — Long-lived cluster resources (created once, persist across runs)

These are created the first time and reused on every subsequent run:

- **k3d cluster** — stamp: `$(CS)/ready`
- **Flux** — stamp: `$(CS)/flux.installed`
- **CRDs** — stamp: `$(CS)/crds.applied`
- **External services via Flux** (Gitea, Prometheus, Valkey/Redis, Cert-Manager, Traefik) — stamp: `$(CS)/flux-setup.ready`
- **Gitea organisation** (`testorg`) — stamp: `$(CS)/gitea/bootstrap/org-{ORG_NAME}.ready`
- **Age encryption key** — stamp: `$(CS)/age-key.txt`
- **Port-forwards** (Gitea :13000, Prometheus :19090, Valkey :16379) — managed by `portforward-ensure` (idempotent, fast-path reuses healthy forwards)

### Layer 2 — Per-run resources (created fresh each `go test` invocation)

These are re-created on each test run, keyed by `REPO_NAME` and `NAMESPACE`:

- **Controller namespace** (`sut`) — cleaned and recreated
- **Operator deployment** — redeployed with current image
- **Gitea repository** — new repo per run, named `e2e-test-{GinkgoRandomSeed}` (or `REPO_NAME` env var)
- **Git credentials** — HTTP access token + SSH keypair, registered with Gitea, applied as Kubernetes Secrets
- **Local git checkout** — at `.stamps/repos/{REPO_NAME}/`
- **SOPS secret** — applied to namespace

### Layer 3 — Per-test resources (created and cleaned up within each `It` block)

- GitProvider, GitTarget, WatchRule, ClusterWatchRule resources
- Applied via `kubectl apply` from Go templates (see `test/e2e/templates/`)
- Cleaned up in `AfterEach` or test-specific `DeferCleanup`

## Repo Lifecycle: One Repo Per `go test` Invocation

Because all test files live in the same Go package (`package e2e`), `go test ./test/e2e/` compiles them together and `BeforeSuite` runs **exactly once** per invocation. This means:

- All test files (`e2e_test.go`, `quickstart_framework_e2e_test.go`, `audit_redis_e2e_test.go`, `bi_directional_e2e_test.go`, `talk_e2e_test.go`) **share one repo** per run.
- Whether you run all suites or filter with `-ginkgo.label-filter=audit-redis`, the same repo is created once.
- The repo name is `e2e-test-{GinkgoRandomSeed}` — the Ginkgo random seed is stable for a given `go test` run (reproducible with `-ginkgo.seed`).

## BeforeSuite: The Setup Contract

`BeforeSuite` in [test/e2e/e2e_suite_test.go](test/e2e/e2e_suite_test.go) calls `ensureE2EPrepared()`, which:

1. Calls `make prepare-e2e` — idempotent, stamp-based, handles cluster/operator/port-forwards
2. Calls `make e2e-gitea-run-setup` — creates or reuses the repo for this run
3. Exports env vars for all test suites to consume:
   - `E2E_REPO_NAME` — active repo name
   - `E2E_CHECKOUT_DIR` — local checkout path
   - `E2E_GIT_SECRET_HTTP` — Kubernetes Secret name for HTTP credentials
   - `E2E_GIT_SECRET_SSH` — Kubernetes Secret name for SSH credentials
   - `E2E_GIT_SECRET_INVALID` — Kubernetes Secret name for error-path testing
   - `SUT_NAMESPACE` — namespace where operator and secrets live
   - `E2E_AGE_KEY_FILE` — path to age key for SOPS tests
4. Sets the current `kubectl` context

**Every test file must read its repo/secret/checkout info exclusively from these env vars**, set by BeforeSuite. No test file should re-derive the repo name or re-run setup independently.

This design means running via a Go IDE plugin works identically to `make test-e2e` — the test itself drives all prerequisite setup through `make prepare-e2e`.

## Specialized Make Targets

In addition to `make test-e2e`, there are focused targets for individual suites:

| Target | Suite label | Notes |
|---|---|---|
| `make test-e2e-quickstart-helm` | `quickstart-framework` | Forces `INSTALL_MODE=helm` |
| `make test-e2e-quickstart-manifest` | `quickstart-framework` | Forces `INSTALL_MODE=plain-manifests-file` |
| `make test-e2e-audit-redis` | `audit-redis` | Uses default `INSTALL_MODE` |
| `make test-e2e-bi` | `bi-directional` | Uses default `INSTALL_MODE` |
| `make test-e2e-demo` | `talk-demo` | Depends on `prepare-e2e-demo`; leaves resources in place |

All targets are fully `.PHONY` and always run `go test` when invoked. Setup is handled by `BeforeSuite` on every invocation.

## Optional Test Suites

Some suites are gated behind environment variables to avoid running expensive or environment-specific tests by default:

| Suite file | Label | Enable flag |
|---|---|---|
| `e2e_test.go` | *(none, always runs)* | — |
| `quickstart_framework_e2e_test.go` | `quickstart-framework` | `E2E_ENABLE_QUICKSTART_FRAMEWORK=true` |
| `audit_redis_e2e_test.go` | `audit-redis` | *(always runs when label matches)* |
| `bi_directional_e2e_test.go` | `bi-directional` | `E2E_ENABLE_BI_DIRECTIONAL=true` |
| `talk_e2e_test.go` | `talk-demo` | `E2E_ENABLE_TALK_FRAMEWORK=true` |

Specialized make targets (`test-e2e-quickstart-helm`, `test-e2e-audit-redis`, `test-e2e-bi`, `test-e2e-demo`) set these flags and run the relevant label filter.

## Design Principles

### Consistency: every test file initialises the same way

All suites read repo/secret/checkout info from the env vars exported by `ensureE2EPrepared()`. No suite should:
- Derive `REPO_NAME` independently
- Call `make` setup targets itself
- Hard-code paths or secret names

### Infrastructure stays up

Cluster, Flux services, port-forwards, and the operator are left running after a test run — even on failure. This allows you to `kubectl` into the cluster and inspect state. `AfterEach` collects diagnostic logs on failure via `dumpFailureDiagnostics()`.

To tear down: `make clean-cluster` (cluster + all stamps), `make clean-port-forwards` (port-forwards only).

### Idempotent setup via stamps

Every infrastructure step is guarded by a stamp file. Running `make prepare-e2e` or `make test-e2e` repeatedly is safe — already-done steps are skipped. The Go `BeforeSuite` leverages this by calling `make prepare-e2e` unconditionally; if everything is current, it completes in seconds.

### One gitops-reverser instance per cluster

The operator creates a cluster-scoped Service and registers a single audit webhook listener. This means only one active gitops-reverser deployment can exist in a cluster at a time — a second instance would conflict on the Service or the webhook endpoint. The e2e setup reflects this: there is one operator namespace (`sut`) and one install, and `prepare-e2e` tears down and recreates that namespace on each run. Running multiple test suites concurrently against the same cluster is not supported.

## Known Issues / Improvement Areas

### `INSTALL_MODE` must be set

`INSTALL_MODE` is required by `ensureE2EPrepared()` but has no default. Running `go test ./test/e2e/` directly without setting it will fail fast with a clear error. `make test-e2e` always sets it.

### `quickstart_framework_e2e_test.go` uses a nanosecond timestamp for resource names

This test creates GitProvider/GitTarget resources with names based on `time.Now().UnixNano()`. This is fine but inconsistent with the seed-based naming used elsewhere. A future improvement would be to use Ginkgo's `CurrentSpecReport().ContainerHierarchyTexts` or a shared counter for deterministic naming.

### Demo suite (`talk_e2e_test.go`) intentionally skips cleanup

The demo suite leaves its namespace and resources in place so a live demo can be run after the test. This is intentional but means the cluster state after a demo run differs from a regular test run.

## Namespace Separation

### Why

Mixing the controller and test resources in one namespace makes teardown destructive: deleting the namespace to clean up test resources also kills the operator. The operator is expensive to redeploy (image build, load into k3d, rollout), so it should survive a test resource cleanup.

### Two namespace roles

| Role | Owner | Default name | Lifecycle |
|---|---|---|---|
| **Controller namespace** | `NAMESPACE` variable | `gitops-reverser` | Persists across runs; managed by `prepare-e2e` |
| **Test namespace** | derived per suite | `{seed}-test-{suite}` | Created in `BeforeAll`, deleted in `AfterAll` |

The controller namespace contains the operator Deployment, Service, ServiceAccount, TLS certificates, and the SOPS age key. It is never touched by individual test suites.

The test namespace contains GitProvider, GitTarget, WatchRule, git credential Secrets, ConfigMaps, and any other resources created during a test run.

### Test namespace naming

```
{GinkgoRandomSeed}-test-{suite-label}
```

| Suite | Suite label | Example |
|---|---|---|
| `e2e_test.go` | `manager` | `8675309-test-manager` |
| `audit_redis_e2e_test.go` | `audit-redis` | `8675309-test-audit-redis` |
| `bi_directional_e2e_test.go` | `bi-directional` | `8675309-test-bi-directional` |
| `quickstart_framework_e2e_test.go` | `quickstart-framework` | `8675309-test-quickstart-framework` |
| `talk_e2e_test.go` | `talk-demo` | `8675309-test-talk-demo` |

The seed ties test namespaces to the repo name (also seed-based), making a run traceable as a whole. Each suite gets its own namespace so suites are fully isolated from each other and can be cleaned up independently.

Names are DNS-compliant and stay well under the 63-character Kubernetes limit.

### Per-suite lifecycle

Each suite's `BeforeAll`:
1. Derives the test namespace name from `testNamespaceFor("<suite-label>")`
2. Creates the namespace via `kubectl create namespace`
3. Applies git credential secrets from `E2E_SECRETS_YAML` (written by `gitea-run-setup.sh` but not applied — each suite applies them to its own namespace)

Each suite's `AfterAll` deletes the test namespace with `--ignore-not-found`.

### RBAC

No RBAC changes are needed. The controller uses a `ClusterRole` bound cluster-wide via `ClusterRoleBinding`, so it already has permission to watch and manage resources in any namespace — including dynamically created test namespaces.

### Secrets

`hack/e2e/gitea-run-setup.sh` generates `secrets.yaml` into the stamp directory but does **not** apply it to Kubernetes. The path is exported as `E2E_SECRETS_YAML` by `BeforeSuite`. Each suite applies it to its own test namespace in `BeforeAll`.

This means secrets are scoped to the test namespace they belong to and disappear with the namespace on cleanup.

### Template organisation

Templates are organised by ownership to make it clear which suite a template belongs to:

```
test/e2e/templates/             # shared — used by ≥2 suites
  gitprovider.tmpl              #   manager, quickstart-framework
  gittarget.tmpl                #   all suites (via helpers.go)
  watchrule.tmpl                #   manager, quickstart-framework
  watchrule-crd.tmpl            #   manager, bi-directional
  icecreamorder-crd.yaml        #   manager, bi-directional
  icecreamorder-instance.tmpl   #   manager, bi-directional

test/e2e/templates/manager/     # manager suite only
  clusterwatchrule-crd.tmpl
  watchrule-configmap.tmpl
  configmap.tmpl

test/e2e/templates/bi-directional/   # bi-directional suite only
  watchrule-secret.tmpl
  flux-gitrepository-http.tmpl
  flux-kustomization.tmpl

test/e2e/templates/talk-demo/   # talk-demo suite only
  watchrule-all.tmpl
  clusterwatchrule-talk.tmpl
```
