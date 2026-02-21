# Kind on Docker-Outside-of-Docker: Findings and DinD Transition Plan

Date: February 21, 2026

## Scope

This document records reproducible findings from the current devcontainer setup and proposes a clean transition plan
from Docker-outside-of-Docker (DOOD, host socket mount) to Docker-in-Docker (DinD) for local Kind-based e2e flows.

## Current Setup

- `.devcontainer/devcontainer.json` uses:
  - `ghcr.io/devcontainers/features/docker-outside-of-docker:1`
  - `HOST_PROJECT_PATH=${localWorkspaceFolder}`
- Kind version in container: `v0.31.0`
- Docker engine in host environment: `29.2.1`

## Reproduced Behavior

All checks below were executed from inside the devcontainer.

1. Docker daemon is reachable:
   - `docker info` succeeds
2. Plain Kind single-node cluster fails:
   - `kind create cluster --name repro-one --image kindest/node:v1.35.0 --wait 2m`
   - Failure: `failed to remove control plane taint ... connect: connection refused`
3. Plain Kind two-node cluster fails:
   - `kind create cluster --name repro-two --config /tmp/repro-two-node.yaml --wait 2m`
   - Failure: `Installing CNI ... failed to download openapi ... connect: connection refused`
4. Project cluster setup fails the same way:
   - `make setup-cluster`
   - Failure: `Installing CNI ... failed to download openapi: unknown`

Conclusion: this is reproducible outside repository-specific logic and is not limited to `test-e2e-quickstart-helm`.

## Key Finding

The failure reproduces with plain `kind create cluster` in this DOOD model. That points to a runtime/interaction issue
between Kind and Docker socket usage from inside a container, not to Helm chart logic or project e2e scripts.

This matches known upstream issue patterns for Kind in DOOD-style environments:

- https://github.com/kubernetes-sigs/kind/issues/2867

## Implemented Interim Workaround (DOOD)

`test/e2e/kind/start-cluster.sh` now includes a compact self-heal path for known DOOD bootstrap flakes:
create with `--retain`, wait for API readiness, apply Kind default CNI/storage with `--validate=false`, then wait for
`kindnet` and node readiness.

- Known issue: https://github.com/kubernetes-sigs/kind/issues/2867 (Kind bootstrap race in DOOD-style setups)

This keeps DOOD usable while preserving a clear migration path to DinD for long-term reliability.

## Notes About Existing Troubleshooting Doc

`.devcontainer/SETUP_CLUSTER_TROUBLESHOOTING.md` focuses on path mapping and audit mount content. That diagnosis can be
valid in some setups, but current observed failures still occur in plain Kind flows where those repo mounts are not
involved. The dominant issue in this environment is now Kind bootstrap instability in DOOD.

## Transition Goal

Move local devcontainer e2e flows to DinD so Kind interacts with a Docker daemon running in the same container
environment, removing host-socket coupling for local cluster lifecycle.

## Clean Transition Plan to DinD

### Phase 1: Devcontainer Runtime Switch

1. In `.devcontainer/devcontainer.json`:
   - Replace `docker-outside-of-docker` with `docker-in-docker`
   - Remove DOOD-specific host coupling where no longer needed:
     - `--add-host=host.docker.internal:host-gateway` (only keep if still required by other workflows)
2. Keep `workspaceMount` as-is so project source remains bind-mounted.
3. Set path env to container-visible path for Kind mounts:
   - `HOST_PROJECT_PATH=/workspaces/${localWorkspaceFolderBasename}`
   - Keep `PROJECT_PATH` unchanged

### Phase 2: Kind Config Alignment

1. Keep `test/e2e/kind/cluster-template.yaml` mount source aligned with container-visible path.
2. Review `test/e2e/kind/start-cluster.sh` kubeconfig rewrite logic:
   - Current rewrite to `host.docker.internal` is DOOD-oriented
   - In DinD, direct endpoint usage is preferred unless a concrete networking reason remains
3. Validate audit mounts from inside Kind control-plane:
   - `/etc/kubernetes/audit/policy.yaml`
   - `/etc/kubernetes/audit/webhook-config.yaml`

### Phase 3: Docs and Developer UX

1. Update `.devcontainer/README.md` to explicitly state DinD mode.
2. Update `.devcontainer/SETUP_CLUSTER_TROUBLESHOOTING.md` with:
   - DinD-first flow
   - DOOD as legacy/optional path (if still supported)
3. Add one short "mode check" snippet:
   - `docker info`
   - `kind create cluster --name smoke --wait 2m && kind delete cluster --name smoke`

### Phase 4: Rollout and Validation

1. Rebuild devcontainer from scratch.
2. Validate cluster bootstrap reliability:
   - Plain Kind single-node create/delete
   - Plain Kind two-node create/delete
3. Validate project targets:
   - `make setup-cluster`
   - `make test-e2e-quickstart-helm`
4. Validate mandatory repo checks after migration:
   - `make lint`
   - `make test`
   - `make test-e2e`

### Phase 5: Rollback Strategy

1. Keep a saved DOOD config variant during transition (for example `devcontainer.dood.json` in docs/reference).
2. If DinD regression appears:
   - Restore previous `devcontainer.json`
   - Rebuild container
3. Keep migration changes scoped and isolated so rollback is one-file plus documentation updates.

## Risks and Mitigations

- Risk: DinD networking differences break port-forward expectations.
  - Mitigation: validate forwarded ports (`13000`, `19090`) in smoke checks.
- Risk: kubeconfig endpoint rewriting becomes incorrect.
  - Mitigation: gate rewrite logic by detected endpoint pattern; prefer no rewrite in DinD.
- Risk: confusion between local DinD and CI setup.
  - Mitigation: document local (DinD) vs CI (host runner) separation explicitly.

## Recommendation

Adopt DinD as the default local devcontainer mode for Kind-driven e2e testing and treat DOOD as unsupported for
reliable Kind lifecycle in this repository.
