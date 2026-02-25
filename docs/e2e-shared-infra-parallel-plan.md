# Shared-Infra Parallel E2E Plan

## Goal

Run the three E2E flows in parallel while sharing one Gitea instance and one Prometheus instance in a single Kind cluster, with stable port-forwarding (`13000`, `19090`) and no cross-test contamination.

## Target Architecture

- One shared infra stack per cluster:
  - Gitea in `gitea-e2e`
  - Prometheus in `prometheus-operator`
  - Fixed port-forwards kept as-is (`localhost:13000`, `localhost:19090`)
- Multiple parallel test flows, each with run-scoped isolation:
  - `run-e2e-full-<id>`
  - `run-e2e-quickstart-helm-<id>`
  - `run-e2e-quickstart-manifest-<id>`
- Isolation key carried across resources and metrics:
  - `configbutler.ai/e2e-run-id=<id>`

## Design Principles

- Share only infra components that are safe to share (Gitea, Prometheus, cluster-level operators).
- Isolate everything stateful per test run (namespaces, repo names, resource names, secrets, temp files, checkout dirs).
- Avoid destructive global cleanup in per-run flows.
- Make assertions run-aware via label and metric filtering.

## Implementation Plan

## 1. Introduce Run Context

Define a run context contract used by Make targets, shell scripts, and Go tests:

- `E2E_RUN_ID`
- `E2E_TEST_NAMESPACE`
- `E2E_CONTROLLER_NAMESPACE` (if needed for scenario separation)
- `E2E_GITEA_API_URL` (default `http://localhost:13000/api/v1`)

Actions:

- Add helper code in `test/e2e` to read/generate run context.
- Replace hardcoded namespace usage (`sut`) in Go helpers and tests with run-context values.
- Ensure all generated resource names include `E2E_RUN_ID` or are namespace-scoped.

## 2. Refactor Gitea Setup for Concurrency

Current risks include global git config and fixed temporary SSH key paths.

Actions in `test/e2e/scripts/setup-gitea.sh`:

- Accept parameters/env for:
  - target namespace
  - secret name prefix
  - run id
  - API URL
- Replace global git URL rewrite with safer alternatives:
  - repo-local config, or
  - per-command credentials
- Make SSH key files run-scoped:
  - `/tmp/e2e-ssh-key-<run-id>`
  - `/tmp/e2e-ssh-key-<run-id>.pub`
- Keep shared Gitea org if desired, but enforce unique repo per run.
- Create per-run secret names instead of shared fixed secrets when needed.

## 3. Make Quickstart Flow Namespace-Aware and Non-Destructive

Actions in `test/e2e/scripts/run-quickstart.sh`:

- Replace fixed namespaces with run-context namespace.
- Keep all object names run-scoped.
- Remove or guard destructive reset operations that delete shared cluster-wide resources.
- Preserve shared infra and fixed port-forwarding lifecycle.

Outcome:

- Quickstart Helm/Manifest flows can run in parallel against shared infra without deleting each other’s prerequisites.

## 4. Add Metric Isolation with Labels

Use per-run labels for both resource selection and Prometheus assertions.

Actions:

- Ensure controller emits labels that can identify run context (`run_id`, `test_namespace`, or equivalent).
- Update e2e PromQL queries to filter by run label.
- If needed, create per-run `ServiceMonitor` objects with label selectors targeting run-tagged resources.
- Replace assumptions bound to fixed values (for example fixed `cluster_id='kind-e2e'` assertions) with run-aware checks.

## 5. Move Quickstart Assertions into Go E2E Tests

Rationale: reduce shell duplication, improve reuse and diagnostics consistency.

Actions:

- Port quickstart functional checks from shell to Go specs in `test/e2e`.
- Keep shell scripts for infrastructure/bootstrap only.
- Use table-driven tests for Helm and manifest install variants.
- Reuse existing helper patterns for CRUD, readiness, encryption, and failure-message assertions.

## 6. Add Parallel Orchestration Target

Introduce a new Make target to orchestrate all flows in one shared cluster.

Example target:

- `make test-e2e-all-parallel`

Behavior:

- Ensure cluster + shared infra are started once.
- Launch `full`, `quickstart-helm`, `quickstart-manifest` with distinct run contexts in parallel.
- Aggregate exit codes and print per-run diagnostics.
- Keep existing targets (`test-e2e`, `test-e2e-quickstart-*`) as fallback and for incremental debugging.

## 7. Cleanup and Diagnostics Model

Actions:

- Cleanup by `run_id` only (namespace and run-scoped resources).
- Never kill shared port-forwards during per-run cleanup.
- Persist run-scoped artifacts/logs for failed runs.
- Add helper commands to inspect one run without affecting others.

## Validation Strategy

Follow existing project validation sequence and mandatory gates.

Primary sequence:

1. `make fmt`
2. `make generate`
3. `make manifests` (if API changes)
4. `make vet`
5. `make lint`
6. `make test`
7. `make test-e2e`

Mandatory completion gates:

- `make lint`
- `make test`
- `make test-e2e`
- `make test-e2e-quickstart-manifest`
- `make test-e2e-quickstart-helm`

Additional prerequisite:

- Verify Docker daemon availability before E2E execution (`docker info`).

## Definition of Done

- Three E2E flows can run concurrently in one cluster against one Gitea and one Prometheus instance.
- No flaky cross-run interactions from shared names, secrets, temp files, or cleanup.
- Prometheus assertions are run-aware via labels/filters.
- Fixed port-forwarding remains stable and unchanged.
- Existing mandatory lint/test/e2e targets pass.

## Suggested Rollout Order

1. Run context and namespace/resource parameterization.
2. Gitea script concurrency hardening.
3. Quickstart script de-destructive changes.
4. Metric labeling and query filtering.
5. Quickstart assertion migration to Go tests.
6. Parallel orchestration target and hardening.

## Risks and Mitigations

- Risk: hidden global assumptions in scripts/tests.
  - Mitigation: search and remove fixed names/paths early (`sut`, fixed secret names, `/tmp/e2e-ssh-key`, global git config).
- Risk: metric cardinality or missing run labels.
  - Mitigation: define required metric labels up front and enforce in tests.
- Risk: parallelism increases flakiness from timing.
  - Mitigation: strengthen eventual assertions and per-run diagnostics.
