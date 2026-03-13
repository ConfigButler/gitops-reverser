# Flux-Managed E2E Services Plan

## Status update

The first Flux migration is now in place.

Completed:

- Flux controllers are bootstrapped from the Makefile
- shared e2e services are applied from `test/e2e/setup/flux-services/`
- `cert-manager`, `gitea`, `prometheus`, and `valkey` are reconciled through Flux `HelmRelease` resources
- `$(CS)/services.ready` is now the single shared-services readiness gate

Still intentionally outside that migration:

- the controller install itself
- Git-backed Flux sources

## Goal

Reduce e2e setup complexity by making Flux manage the shared support services used by the test cluster.

Initial scope:

- `cert-manager`
- `gitea`
- `valkey`

Out of scope for the first pass:

- moving the gitops-reverser controller install itself under Flux
- requiring a Git-backed Flux source for e2e bootstrapping
- changing the existing quickstart install-mode coverage (`helm`, `plain-manifests-file`, `config-dir`)

## Why this direction

The current e2e bootstrap mixes several orchestration styles:

- imperative `kubectl apply` for `cert-manager`
- imperative `helm upgrade --install` for `gitea`
- imperative `helm upgrade --install` for `valkey`
- imperative shell setup for Prometheus operator bootstrap
- separate install-mode logic for the system under test

That works, but it spreads lifecycle logic across the Makefile and shell scripts.

The cleanest first step is to make Flux own the shared service layer while leaving the system-under-test install
flow unchanged. That gives us:

- fewer imperative Helm steps in the Makefile
- a single place for service definitions
- explicit dependency ordering via Flux `dependsOn`
- a more GitOps-shaped e2e environment without introducing circular bootstrap problems

## Recommendation

Create a single Flux-applied services bundle under:

`test/e2e/setup/flux-services/`

That bundle should contain:

- Flux services manifests
- `HelmRepository` resources for upstream charts
- `HelmRelease` resources for `cert-manager`, `gitea`, and `valkey`
- namespaces for those services
- any values/config needed by those Helm releases

The Makefile should always apply this bundle as part of cluster services preparation.

Flux should not initially reconcile from a Git repository.
Instead, the e2e setup should `kubectl apply` the Flux custom resources from the local working tree and then wait for
Flux to reconcile them in-cluster.

This keeps bootstrap simple while still using Flux as the control plane for service installs.

## Current seams in the Makefile

Today the shared services are built around these cluster-scoped stamps:

- `$(CS)/cert-manager.installed`
- `$(CS)/gitea.installed`
- `$(CS)/valkey.installed`
- `$(CS)/prometheus.installed`
- `$(CS)/services.ready`

The imperative Helm/install logic currently lives in:

- `$(CS)/cert-manager.installed`
- `$(CS)/gitea.installed`
- `$(CS)/valkey.installed`

The system under test depends on `$(CS)/services.ready`, and the Go e2e suite always triggers that path through
`prepare-e2e`.

That means the Flux migration should preserve the same high-level contract:

`prepare-e2e` -> shared services ready -> controller install -> image override -> tests

## Target architecture

### 1. Shared cluster bootstrap

Keep a small amount of imperative bootstrap in Make:

- create/reuse the k3d cluster
- install Flux controllers
- apply the local Flux services bundle
- wait for all Flux-managed `HelmRelease` objects to become Ready

This replaces direct Helm commands in Make, but it does not try to eliminate all Make orchestration.

### 2. Flux-owned services bundle

Create a local manifest tree like:

```text
test/e2e/setup/flux-services/
  kustomization.yaml
  namespaces/
    cert-manager.yaml
    gitea-e2e.yaml
    valkey-e2e.yaml
  repositories/
    jetstack.yaml
    gitea.yaml
    valkey.yaml
  releases/
    cert-manager.yaml
    gitea.yaml
    valkey.yaml
  values/
    gitea-values.yaml
    valkey-values.yaml
```

Recommended behavior:

- `cert-manager` installs first
- `gitea` and `valkey` depend only on their repositories and namespaces
- if webhook/certificate timing becomes relevant later, additional `dependsOn` can be added cleanly

### 3. Minimal Makefile contract

Everything managed by Flux should disappear as a service-specific Make target.

The target shape should become:

- `$(CS)/flux.installed`
- `$(CS)/services.ready`

That means:

- no `$(CS)/cert-manager.ready`
- no `$(CS)/gitea.ready`
- no `$(CS)/valkey.ready`
- no service-specific install stamps for Flux-owned services

The Makefile should not know how each service is installed anymore.
It should only know:

1. Flux is installed.
2. The local Flux bundle was applied.
3. All Flux-owned `HelmRelease` objects are Ready.

That is the cleanest simplification boundary.

## Proposed implementation phases

## Phase 1: Flux for cert-manager, Gitea, and Valkey

### Desired outcome

Replace:

- direct `kubectl apply` of remote cert-manager manifest
- direct Helm install of Gitea
- direct Helm install of Valkey

With:

- Flux controllers installed once per e2e cluster context
- local Flux resources applied from `test/e2e/setup/flux-services/`
- one readiness gate based on Flux-managed `HelmRelease` objects

### Concrete changes

1. Add a new local Flux services directory.
2. Define service namespaces as plain manifests.
3. Define `HelmRepository` resources for:
   - Jetstack
   - Gitea charts
   - Valkey charts
4. Define `HelmRelease` resources for:
   - `cert-manager`
   - `gitea`
   - `valkey`
5. Reference existing e2e values for Gitea and Valkey, or move those values into the new directory if that makes the
   bundle easier to understand.
6. Add `dependsOn` only where it improves determinism; do not over-model dependencies.
7. Replace the old `$(CS)/cert-manager.installed`, `$(CS)/gitea.installed`, and `$(CS)/valkey.installed` recipes with:
   - Flux bootstrap
   - apply local bundle
   - a single `services.ready` wait

### Readiness strategy

Use one Make-level wait:

1. Query all `HelmRelease` objects in the Flux services bundle.
2. Fail unless every one reports `Ready=True`.

Recommended implementation shape:

- label all Flux-owned e2e `HelmRelease` objects consistently
- use one `kubectl`-driven readiness check against that label set

Example direction:

```bash
kubectl --context "${CTX}" get helmreleases.helm.toolkit.fluxcd.io -A \
  -l e2e.configbutler.io/stack=services
```

This keeps the Makefile simple while still leaving detailed failure information in the `HelmRelease` status when
something goes wrong.

If a specific workload later proves flaky even when the `HelmRelease` is Ready, we can add a focused workload-level
wait at that time. It should not be the default shape of the first implementation.

### Makefile simplification target

The Makefile should stop containing:

- chart repo add/update commands
- per-service `helm upgrade --install` logic
- service-specific inline Helm arguments

The Makefile should only contain:

- bootstrap Flux
- apply the local Flux services bundle
- run one readiness check for all Flux-managed `HelmRelease` objects

That is the main line-count and complexity win.

## Phase 2: Evaluate Prometheus

Prometheus is more custom today because it depends on a Prometheus operator setup path.

Recommendation:

- keep Prometheus out of the first Flux migration
- once Phase 1 is stable, decide whether to:
  - manage the operator with Flux and keep local CRs in place, or
  - keep Prometheus imperative if it remains simpler and more reliable

This avoids mixing one straightforward cleanup with a more open-ended refactor.

## Completed step: replace `$(CS)/prometheus.installed`

This increment replaced the old Prometheus-specific stamp target in the Makefile.

### Scope

In scope:

- remove the special-case `$(CS)/prometheus.installed` install path from the Makefile
- move Prometheus bootstrap behind the same Flux-managed service pattern already used for `cert-manager`, `gitea`, and
  `valkey`
- preserve the existing shared-service contract of `$(CS)/services.ready`

Out of scope:

- changing Prometheus scrape behavior
- redesigning test port-forwarding
- migrating the controller install under Flux
- introducing a Git-backed Flux source

### Recommended shape

Add Prometheus as another local Flux-managed service under `test/e2e/setup/flux-services/`.

That likely means:

- a `HelmRepository` for the Prometheus operator chart source
- a `HelmRelease` for the operator stack
- local manifests or Flux-managed resources for the e2e-specific Prometheus instance and `ServiceMonitor`
- the same shared `HelmRelease` readiness gate used by `$(CS)/services.ready`

The key design goal is that `$(CS)/services.ready` should depend on one shared Flux services gate, not on a separate
Prometheus install recipe.

### Makefile target end state

After this step, the preferred shape is:

- keep `$(CS)/flux.installed`
- remove `$(CS)/prometheus.installed`
- keep `$(CS)/services.ready` as the single shared-services readiness target

### Acceptance criteria for this step

This step is done when:

1. Prometheus no longer has a dedicated install recipe in `Makefile`.
2. `prepare-e2e` still reaches a working shared Prometheus endpoint for the existing e2e flow.
3. The full shared-service bootstrap path is expressed through Flux-applied local manifests.
4. `make test-e2e`, `make test-e2e-quickstart-manifest`, and `make test-e2e-quickstart-helm` still pass.

## Phase 3: Optional Flux-managed controller install mode

Only after the service layer is stable, consider a new install mode for the controller itself.

Possible future mode:

- `INSTALL_MODE=flux-helm`

But this should not be part of the first implementation because the current e2e flow patches the controller Deployment
image after install. A Flux-managed Deployment would likely reconcile that change away unless the image override is
designed into the Flux resources.

## Why not use a GitRepository source first

Using Flux without a Git source is the right first step here.

Reasons:

- simpler bootstrap
- fewer moving parts
- no circular dependency on Gitea to install Gitea
- easier local debugging
- still exercises Flux controllers and reconciliation loops

In practice, Flux would reconcile local custom resources we apply directly:

- `HelmRepository`
- `HelmRelease`
- later possibly `Kustomization`

If this works well, a later phase can move the service definitions into a Git-backed source in Gitea for higher
fidelity.

## Proposed file layout

Recommended initial layout:

```text
test/e2e/setup/flux-services/
  kustomization.yaml
  namespaces/
    cert-manager.yaml
    gitea-e2e.yaml
    valkey-e2e.yaml
  repositories/
    jetstack.yaml
    gitea-charts.yaml
    valkey.yaml
  releases/
    cert-manager.yaml
    gitea.yaml
    valkey.yaml
  values/
    gitea-values.yaml
    valkey-values.yaml
```

Optional helper location if needed:

```text
hack/e2e/
  install-flux.sh
```

The default preference should be to keep logic in Make small and use at most one focused helper script if readability
improves.

## Proposed Makefile shape

### New stamps

- `$(CS)/flux.installed`
- `$(CS)/services.ready`

### Updated aggregate

`$(CS)/services.ready` should remain the single readiness stamp for shared e2e services.

### What should be removed

Remove or replace:

- `GITEA_HELM_REPO_*` Make variables
- `VALKEY_HELM_REPO_*` Make variables
- direct Helm install recipes for Gitea and Valkey
- remote cert-manager manifest apply recipe
- per-service readiness stamps for Flux-owned services

The chart versions should live with the Flux `HelmRelease` resources instead of the Makefile.

## Flux bootstrap recommendation

Flux itself should be bootstrapped separately from the services bundle.

Recommended shape:

1. Keep one stamp for Flux controller installation:
   - `$(CS)/flux.installed`
2. Install Flux controllers into the cluster with the Flux CLI:
   - `flux install`
3. Wait for Flux controllers in `flux-system` to be Available.
4. After that, apply `test/e2e/setup/flux-services/`.

Why separate it from the services bundle:

- the bundle contains Flux custom resources, so the CRDs/controllers must already exist
- Flux installation changes much less often than the service definitions
- it keeps the failure mode obvious: either Flux itself failed, or a Flux-managed service failed

Alternative:

- commit raw Flux install manifests into the repo and apply them with `kubectl`

That works too, but the Flux CLI is the cleaner bootstrap tool for local e2e because it gives a short, supported way to
install the controllers without carrying a large generated manifest in-tree.

## Flux CLI in the devcontainer

Yes, the Flux CLI should be installed in the devcontainer if we adopt this plan.

Reasoning:

- the Makefile will depend on it for `$(CS)/flux.installed`
- local e2e and CI/devcontainer behavior should match
- Flux bootstrap is easier and less noisy through `flux install` than by vendoring install YAML

Suggested approach:

- add `FLUX_VERSION` alongside the other tool versions in `.devcontainer/Dockerfile`
- install the `flux` binary in the same style as `kubectl`, `helm`, and `k3d`

What I would not do:

- require developers to install Flux manually outside the container
- make the e2e bootstrap fetch and pipe an ad-hoc shell installer every run

If we later decide to avoid the Flux CLI entirely, we can switch to checked-in install manifests and remove the binary.
But for the initial implementation, shipping the CLI in the devcontainer is the simpler and cleaner choice.

## Cleanup model

Cleanup should stay simple.

Recommended approach:

- keep Flux controllers installed for the cluster context once created
- treat Flux-managed services as shared cluster-scoped e2e prerequisites
- continue cleaning only per-test install namespaces during normal install cleanup
- add a dedicated target if full service teardown is needed

This matches the current stamp philosophy of reusing expensive cluster setup work.

## Risks and mitigations

### Risk: readiness becomes less obvious

Mitigation:

- use one labeled `HelmRelease` readiness check
- inspect `HelmRelease` status and events when debugging failures

### Risk: too much abstraction hides failures

Mitigation:

- keep manifests local and readable
- do not generate this bundle dynamically
- prefer explicit YAML over large shell wrappers

### Risk: cert-manager ordering issues

Mitigation:

- install cert-manager through its own `HelmRelease`
- use `dependsOn` only where useful
- keep workload waits explicit in Make

### Risk: trying to migrate too much at once

Mitigation:

- do cert-manager, Gitea, and Valkey first
- postpone Prometheus and controller install-mode changes

## Success criteria

Phase 1 is successful when:

1. The Makefile has materially fewer service-install lines and less inline Helm logic.
2. `prepare-e2e` still remains the single entrypoint used by the Go suite.
3. Shared services are reconciled by Flux from local manifests.
4. `make test-e2e`, `make test-e2e-quickstart-manifest`, and `make test-e2e-quickstart-helm` still pass.
5. Local debugging remains straightforward: inspect Flux `HelmRelease` objects first, then workloads if needed.

## Suggested execution order

1. Add Flux bootstrap target and stamps.
2. Add `test/e2e/setup/flux-services/` with `HelmRepository` and `HelmRelease` YAMLs.
3. Migrate `cert-manager` into Flux.
4. Migrate `gitea` into Flux.
5. Migrate `valkey` into Flux.
6. Rewire `$(CS)/services.ready` to be the single shared-services readiness target.
7. Remove old imperative Helm/install logic from the Makefile.
8. Run full validation.

## Notes for implementation

- Keep the implementation aggressive about deleting now-redundant Makefile code.
- Prefer one shared services folder over per-service bootstrap patterns.
- Prefer stable, checked-in YAML over dynamically rendered e2e service manifests.
- Keep the controller install path separate until image override semantics are redesigned for Flux ownership.
