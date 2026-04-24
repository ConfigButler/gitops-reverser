# E2E Aggregated API Proxy Hookup Plan

This document records the plan for integrating `apiservice-audit-proxy` into the main project's
e2e environment so aggregated API mutations can produce rich audit-backed commits.

This document is intentionally written to be **self-contained**. It should be enough to hand to an
implementation agent later without needing the rest of the conversation.

## Related Docs

- [E2E Test Design: Aggregated API Server](e2e-aggregated-apiserver-test-design.md)
- [Kubernetes API Discovery and Watching in GitOps Reverser](kubernetes-api-discovery.md)
- [Audit Webhook API Server Connectivity](audit-webhook-api-server-connectivity.md)

This document is about **environment wiring and bootstrap**. The test-design document covers what
the e2e suite must prove once the environment exists.

## Repository Context

The main repo already has:

- Flux bootstrap manifests under `test/e2e/setup/flux/`
- e2e orchestration in `test/e2e/Taskfile.yml`
- aggregated API e2e coverage in `test/e2e/aggregated_apiserver_e2e_test.go`
- gitops-reverser audit webhook kubeconfig generation logic in `hack/`

The full source of the upstream helper project is available locally in:

- `external-sources/apiservice-audit-proxy/`

That local clone includes:

- Go source
- Helm chart
- docs
- e2e setup examples

So the implementation should treat `external-sources/apiservice-audit-proxy/` as the local source
reference when checking how the project works, even though the actual cluster bootstrap should use
the packaged OCI chart.

## Design Bias

Bias hard toward the **simplest reusable approach**.

That means:

- reuse the upstream Helm chart instead of copying its resources into this repo
- reuse upstream chart features instead of adding gitops-reverser-specific manifests when possible
- prefer small upstream changes if they make the integration cleaner
- avoid maintaining a second parallel demo stack in gitops-reverser

If something is awkward in the chart and would force a lot of local glue code here, it is
acceptable to change the upstream `apiservice-audit-proxy` project instead. It is a new project and
is intended for `gitops-reverser`.

## Simple Plan

The intended implementation path is:

1. install `apiservice-audit-proxy` via Flux from the packaged OCI chart
2. enable the sample-apiserver backend through that chart
3. make the HelmRelease non-blocking during bootstrap so a not-yet-present webhook Secret does not
   deadlock Flux
4. let the proxy reference the final webhook Secret from the start, even before that Secret exists
5. create the final webhook Secret later from gitops-reverser e2e wiring
6. wait for the proxy, backend, `APIService`, and discovery checks to become healthy

If that path feels complicated in practice, prefer simplifying the upstream chart rather than
adding more local special-case logic here.

## Naming Decision

Use `aggregated-api` as the repo-facing name for this feature.

That means:

- docs, task names, readiness gates, and values files should prefer `aggregated-api`
- Flux resources that we own should prefer `aggregated-api`
- user-facing language in the repo should prefer "aggregated API" over "Wardle"

Keep `wardle` only where it is part of the upstream sample-apiserver surface:

- API group `wardle.example.com`
- kinds such as `Flunder` and `Fischer`
- any upstream chart or manifest field that must still point at those concrete API identifiers

Recommended practical split:

- **feature name**: `aggregated-api`
- **example API served inside the test**: `wardle.example.com`

If we rename the Kubernetes namespace from `wardle` to `aggregated-api`, do it intentionally and
consistently in the Flux values override. The API group itself still remains `wardle.example.com`.

## Goal

Make the aggregated API stack part of cluster bootstrap so it is installed via Flux and ready for
the e2e suite without a separate late `kubectl apply -k` path.

Target request path:

`kube-apiserver -> apiservice-audit-proxy -> sample-apiserver`

Target audit path:

`apiservice-audit-proxy -> gitops-reverser audit webhook`

## Preferred Outcome

After `task prepare-e2e`, the environment should feel simple:

- Flux has already installed the aggregated API stack
- the proxy is the `APIService` backend
- the sample-apiserver backend is already present through the chart
- the only extra gitops-reverser-specific step is creating the final webhook kubeconfig Secret
- once that Secret exists, the aggregated API becomes ready and the existing e2e tests can run

This should feel like "one normal bootstrap service" rather than a special snowflake path.

## Non-Goals

- Do not solve duplicate suppression in this pass.
- Do not solve production-grade delegated-header trust in this pass.
- Do not redesign the whole shared e2e bootstrap model.
- Do not replace the Wardle sample API with a custom aggregated API.

## Recommended Architecture

### 1. Flux owns the aggregated API stack

Add the aggregated API stack to `test/e2e/setup/flux/` as a first-class bootstrap service, just
like cert-manager, Gitea, Valkey, ingress, and reflector.

Flux should install:

- the `apiservice-audit-proxy` chart
- the sample-apiserver backend enabled through that chart
- the `APIService` registration
- the proxy serving cert machinery
- the backend client cert machinery

This removes the need for the hand-maintained proxy/sample-apiserver manifest bundle under
`test/e2e/setup/manifests/sample-apiserver/`.

The HelmRelease should explicitly depend on cert-manager because the chart relies on certificate
resources as part of the proxy/backend setup.

### 2. GitOps Reverser still owns the final audit webhook Secret contents

The proxy's outbound webhook kubeconfig still depends on the gitops-reverser audit receiver wiring,
which is produced after the controller install and post-install TLS bootstrap.

So the ownership split should be:

- **Flux/bootstrap ownership**: install the aggregated API stack and reference the expected Secret
  name from the beginning
- **gitops-reverser e2e ownership**: generate and create the final
  `audit-pass-through-webhook-kubeconfig` Secret after `_webhook-tls-ready`

### 3. Rely on the normal Kubernetes "missing Secret" behavior

The proxy Deployment can safely reference a Secret that does not exist yet.

That means:

- we do **not** need to delay creating the HelmRelease until the Secret exists
- we do **not** need a bootstrap-time placeholder Secret just to make the chart render
- we can let the proxy pod wait for the Secret to appear

Practical consequence:

- the aggregated API HelmRelease can be present from cluster bootstrap
- the proxy workload may remain unavailable until the webhook Secret is created later
- once the Secret exists, the pod can proceed without re-rendering the chart

This is simpler than treating the Secret as a separate install phase.

Important Flux-specific consequence:

- the aggregated API HelmRelease must not block cluster bootstrap while the webhook Secret is still
  intentionally missing

Preferred approach:

- set `spec.install.disableWait: true`
- set `spec.upgrade.disableWait: true`

Fallback approach if that is not enough:

- make `_flux-setup-ready` gate on "HelmRelease observed / reconciling" rather than "HelmRelease
  Ready"

## Source of Truth Decision

Use the `apiservice-audit-proxy` Helm chart as the source of truth for the proxy/backend demo
stack.

That means:

- do **not** keep evolving a parallel hand-written copy of the same resources in
  `test/e2e/setup/manifests/sample-apiserver/`
- do keep the local clone in `external-sources/apiservice-audit-proxy/` as the reviewable source
  tree that explains what the chart and image are

Use the packaged OCI chart for the actual e2e bootstrap:

- chart: `oci://ghcr.io/configbutler/charts/apiservice-audit-proxy`
- install example: `helm install apiservice-audit-proxy oci://ghcr.io/configbutler/charts/apiservice-audit-proxy --version 0.3.0`
- first pinned version: `0.3.0`
- image: use the chart default that comes with `0.3.0`; do not add a separate image pin unless a
  real mismatch forces it

Use the local source tree for inspection and, if needed, upstream fixes:

- `external-sources/apiservice-audit-proxy/`

The local clone remains useful for:

- code review
- documentation references
- syncing chart expectations with the main repo

## Naming and Namespace Recommendation

Prefer `aggregated-api` for the namespace and Flux release names that this repo owns.

Recommended shape:

- namespace: `aggregated-api`
- Flux source name: `apiservice-audit-proxy`
- Flux release name: `aggregated-api`
- task/readiness stamp names: `_aggregated-api-ready`, `aggregated-api.ready`

Keep the backend API identity unchanged:

- `APIService`: `v1alpha1.wardle.example.com`
- resources: `flunders`, `fischers`

Reasoning:

- `aggregated-api` is clearer in our repo than `wardle`
- `wardle` is still correct for the actual sample-apiserver API group
- this keeps the example API stable while making our bootstrap plumbing easier to understand

## Simplest Integration Shape

Add new Flux resources under `test/e2e/setup/flux/`:

1. namespace for the aggregated API stack
2. OCIRepository for the chart source
3. ConfigMap for e2e-specific values
4. HelmRelease that installs the chart with:
   - `apiService.enabled=true`
   - `testApiserver.enabled=true`
   - `webhookTester.enabled=false`
   - `webhook.kubeconfigSecretName=audit-pass-through-webhook-kubeconfig`
   - chart-generated ownership of `audit-pass-through-webhook-kubeconfig` disabled or absent
   - `dependsOn` pointing at cert-manager
   - `install.disableWait=true`
   - `upgrade.disableWait=true`

The values file should live next to the other Flux values files, for example:

`test/e2e/setup/flux/values/aggregated-api-values.yaml`

Reviewer note:

- this will likely be the first `OCIRepository` under `test/e2e/setup/flux/`

Prefer this over:

- building a custom local image for the first pass
- rendering chart manifests into this repo and owning them here
- keeping the old `test/e2e/setup/manifests/sample-apiserver/` path active

## What To Reuse From Upstream

The implementation should try to reuse these upstream chart capabilities directly:

- `apiService.enabled=true`
- `testApiserver.enabled=true`
- `webhook.kubeconfigSecretName`
- backend client certificate wiring
- proxy serving certificate wiring
- the chart's own resource naming and mounting conventions

If any of those are close but not quite right for gitops-reverser e2e, prefer a small upstream
change over adding a custom local workaround here.

One thing to verify explicitly:

- the chart must **not** create `audit-pass-through-webhook-kubeconfig`
- that Secret must be owned only by gitops-reverser e2e wiring

Disabling `webhookTester.enabled` is part of this, but the implementation should still confirm
there is no other chart path that auto-creates or takes ownership of that Secret.

## Acceptable Upstream Adjustments

If the integration is cleaner with upstream changes, those changes are welcome.

Examples of good upstream adjustments:

- making Secret names easier to pin from values
- making namespace/service naming clearer
- reducing values required for the proxy + sample-apiserver demo stack
- improving chart behavior when the webhook Secret does not exist yet
- simplifying the values needed for a GitOps Reverser-focused install

Examples of poor local-only workarounds to avoid if upstream can be fixed instead:

- carrying a large forked manifest set in this repo
- patching many rendered resources after install
- maintaining duplicate TLS/cert wiring here that already exists in the chart
- inventing a second bootstrap path just for aggregated APIs

## Readiness Model

The current bootstrap has two readiness layers:

- `_flux-setup-ready` for Flux-managed shared services
- later task-specific readiness for components that need extra post-install work

The aggregated API stack should follow the same pattern.

### Phase A: Flux bootstrap ready enough

`_flux-setup-ready` should confirm that Flux has created the aggregated API release objects, but it
should **not** require the proxy Deployment to be Available yet.

Reason:

- the proxy pod is allowed to wait on the missing webhook Secret
- requiring full readiness here would deadlock bootstrap on a Secret that is intentionally created
  later by gitops-reverser e2e logic
- default Flux wait behavior is not compatible with this plan unless explicitly disabled for this
  HelmRelease

### Phase B: aggregated API fully ready

Create a dedicated `_aggregated-api-ready` gate that runs after:

- the gitops-reverser controller is installed
- `_webhook-tls-ready` has generated the final audit receiver kubeconfig material

That gate should:

1. create/update `audit-pass-through-webhook-kubeconfig` in the aggregated API namespace
2. wait for the proxy Deployment to become Available
3. wait for the sample-apiserver backend to become Available
4. wait for `apiservice/v1alpha1.wardle.example.com` to become `Available`
5. confirm discovery shows `flunders`

This is the point where the aggregated API path becomes truly usable for e2e.

Keep the gap between Secret creation and full readiness as short as possible so the cluster does
not spend long with an unhealthy registered `APIService`.

## Minimal Local Ownership

The gitops-reverser repo should only need to own:

- Flux resources that install the chart
- one e2e values file for the chart
- one readiness gate that creates the final webhook kubeconfig Secret and waits for health
- documentation and test wiring

Everything else should ideally stay in or come from `apiservice-audit-proxy`.

## Concrete Implementation Plan

### Phase 1: Move bootstrap ownership into Flux

1. Add a namespace manifest for the aggregated API stack under `test/e2e/setup/flux/namespaces/`.
2. Add an `OCIRepository` source for the packaged chart.
3. Add `test/e2e/setup/flux/values/aggregated-api-values.yaml`.
4. Add a HelmRelease for `apiservice-audit-proxy` to `test/e2e/setup/flux/releases/` with:
   - `dependsOn` on cert-manager
   - `install.disableWait=true`
   - `upgrade.disableWait=true`
5. Include those resources from `test/e2e/setup/flux/kustomization.yaml`.
6. Before committing the HelmRelease, sanity-check the namespace-dependent rendering with
   `helm template` against the override values so that choosing `aggregated-api` does not leave
   stray hard-coded `wardle` namespace references in service URLs, Secret names, or mounts.

### Phase 2: Remove the duplicate manifest path

1. Stop treating `test/e2e/setup/manifests/sample-apiserver/` as the active source of truth.
2. Delete `test/e2e/setup/manifests/sample-apiserver/` once the Flux path has been green for one
   full `prepare-e2e` run.
3. Update docs that still describe the old `kubectl apply -k test/e2e/setup/manifests` ownership
   model for this stack.

### Phase 3: Update task and readiness naming

1. Rename task/readiness language from `sample-apiserver-ready` to `aggregated-api-ready`.
2. Keep concrete checks against:
   - `deployment/<proxy>`
   - `deployment/<backend>`
   - `apiservice/v1alpha1.wardle.example.com`
   - `kubectl api-resources --api-group=wardle.example.com`
3. Grep the whole tree for feature-name uses such as `sample-apiserver-ready` and convert them
   together across `test/e2e/Taskfile.yml`, `hack/`, docs, and CI-related files.
4. Leave `wardle` intact only where it refers to the actual upstream API group or example kinds.

### Phase 4: Fix source and image references

1. Replace stale `external-prototype/audit-pass-through-apiserver` references with
   `external-sources/apiservice-audit-proxy` where local source references still matter.
2. If local image loading remains part of any fast-dev path, rename the default image reference to
   `apiservice-audit-proxy:e2e-local`.
3. Remove doc references to the deleted prototype path.

## Concrete Files Likely To Change

In gitops-reverser:

- `test/e2e/setup/flux/kustomization.yaml`
- `test/e2e/setup/flux/namespaces/*.yaml`
- `test/e2e/setup/flux/releases/*.yaml`
- `test/e2e/setup/flux/values/aggregated-api-values.yaml`
- `test/e2e/Taskfile.yml`
- `hack/e2e/prepare-sample-apiserver-proxy-webhook-kubeconfig.sh` or a renamed equivalent
- docs that still mention the old prototype path or old manifest ownership

In the local upstream source, if needed:

- `external-sources/apiservice-audit-proxy/charts/apiservice-audit-proxy/values.yaml`
- `external-sources/apiservice-audit-proxy/charts/apiservice-audit-proxy/values-demo.yaml`
- `external-sources/apiservice-audit-proxy/charts/apiservice-audit-proxy/templates/*`
- other files under `external-sources/apiservice-audit-proxy/` as needed

## Implementation Rules

When implementing from this document, follow these rules:

1. Start by checking whether the upstream chart already supports the needed behavior.
2. Prefer changing upstream over adding large local glue in gitops-reverser.
3. Keep the gitops-reverser side focused on Flux install + final Secret creation + readiness.
4. Keep naming consistent: `aggregated-api` for our feature plumbing, `wardle` for the concrete
   sample API group.
5. Do not keep both the old manifest path and the new Flux path active longer than necessary.

## Verification Scope

The acceptance checks should stay aligned with
[e2e-aggregated-apiserver-test-design.md](e2e-aggregated-apiserver-test-design.md):

- aggregated resources are discoverable
- WatchRules can target them
- a `Flunder` create produces a Git commit
- the proxy recovers `objectRef.name`, `requestObject`, and `responseObject`

Do **not** make the hookup plan depend on proving every aggregated API edge case before merge.
The load-bearing proof is that the Flux-bootstrapped environment supports the existing aggregated
API e2e scenarios.

## Done Criteria

This plan is complete when all of the following are true:

1. The aggregated API stack is installed by Flux during normal e2e bootstrap.
2. The bootstrap uses the packaged OCI chart, not a large copied manifest set.
3. The source of the project remains locally available in
   `external-sources/apiservice-audit-proxy/`.
4. The proxy can reference the final webhook Secret before it exists.
5. A later readiness step creates that Secret and waits for the stack to become healthy.
6. The old duplicate manifest-managed stack is deleted.
7. The existing aggregated API e2e tests pass against this environment.

## Main Risks

- Flux bootstrap can deadlock if we accidentally require the aggregated API release to become fully
  healthy before the later webhook Secret exists.
- If `APIService v1alpha1.wardle.example.com` exists while the proxy pod is still blocked on the
  missing Secret, cluster-wide discovery can log errors and unrelated e2e behavior may become
  noisy or flaky during that window.
- Naming drift can make the stack harder to reason about if some parts still use `wardle` as the
  feature name while others use `aggregated-api`.
- Keeping both the Flux-managed chart and the old hand-written manifest stack alive for too long
  invites divergence.
- A values file that quietly bakes in the old `wardle` namespace can make the move to
  `aggregated-api` confusing unless all URLs, Secret names, and service names are updated together.

## Recommended First Cut

The fastest credible first cut is:

1. add the aggregated API HelmRelease to Flux bootstrap
2. give it the final webhook Secret name up front, even before that Secret exists
3. create the webhook Secret later in `_aggregated-api-ready`
4. wait for the proxy/backend/APIService/discovery checks after the Secret appears
5. delete the duplicate manifest-managed stack once the Flux path is green

That gets us to the desired end state quickly without inventing a second bootstrap mechanism just
for this component.

## Summary For A Future Implementation Agent

If you are starting implementation with only this document:

- use Flux to install `apiservice-audit-proxy` from `oci://ghcr.io/configbutler/charts/apiservice-audit-proxy`
  pinned to `0.3.0`
- prefer the chart as the source of truth
- inspect local source in `external-sources/apiservice-audit-proxy/` before inventing local
  workarounds
- keep `aggregated-api` as the feature name and `wardle.example.com` as the actual sample API group
- let the proxy reference the final webhook Secret from bootstrap time
- create that Secret later from gitops-reverser e2e wiring
- make the environment simple enough that the aggregated API test feels like a normal e2e service,
  not a one-off special case
