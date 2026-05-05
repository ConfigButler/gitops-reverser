# Voter GitOps Demo Instances

## Goal

Run two voter applications next to each other for the demo:

- `demo.configbutler.ai`
- `demo-test.configbutler.ai`

Each instance should be independently routable and should not fight over namespaces, RBAC, Traefik objects, generated
session secrets, sample resources, or Flux reconciliation ownership.

The immediate goal was to get both applications live under the Cloudflare routes first. That is now the local target
state for this branch. The next demo layer is CoffeeConfig promotion: edit config in the test app, let GitOps Reverser
commit it to a test branch, then promote it to production by merging that branch into `main`.

The old checked-in `test/e2e/setup/demo-only/vote` app can be removed in this branch if doing so simplifies the demo.
For now, the local demo manifests keep enough of it around for the existing demo harness and avoid routing traffic to it.

The original upstream manifests are available in this workspace at `external-sources/voter/k8s`. The local demo manifests
can copy from that directory when we need to move fast, and then upstream can be cleaned up into a reusable packaging
story later.

## Current State

`test/e2e/setup/demo-only/voter-gitops` currently creates local, repo-owned voter app instances:

- namespace `voter-production`
- namespace `voter-test`
- app resources copied from `external-sources/voter/k8s`
- local Traefik routes for `demo.configbutler.ai` pointing at `voter-production`
- local Traefik routes for `demo-test.configbutler.ai` pointing at `voter-test`

This is intentionally branch-local and pragmatic. It gets the demo live before we solve the cleaner upstream packaging
work. The time pressure matters here: keeping the manifests local is more cumbersome than ideal, but it gives us direct
control over namespaces, RBAC, ingress, and promotion wiring while the demo is being prepared.

The upstream `external-sources/voter/k8s` manifests are hardcoded for one installation:

- namespace `voter`
- service names `voter-frontend` and `auth-service`
- service accounts `auth-service` and `quiz-access`
- `FORWARD_SA_NAMESPACE=voter`
- `COFFEE_CONFIG_NAME=testnet-coffee`
- example objects in namespace `voter`
- Traefik hosts `voter.z65.nl` and `coffee.z65.nl`
- cluster-scoped `ClusterRole/auth-service`
- cluster-scoped `ClusterRoleBinding/auth-service`

The last two are the immediate bug. They collide with the old checked-in `vote` app, which also creates
`ClusterRole/auth-service` and `ClusterRoleBinding/auth-service`. The live failure:

```text
User "system:serviceaccount:voter:auth-service" cannot get resource "coffeeconfigs"
```

happened because the shared `ClusterRoleBinding/auth-service` was bound to `system:serviceaccount:vote:auth-service`
instead of `system:serviceaccount:voter:auth-service`.

## Demo Harness Coupling

The e2e demo test is still coupled to the old `vote` app. It pins `TESTNAMESPACE=vote` and expects the reverse-GitOps
repository to contain files such as:

- `v1/namespaces/vote.yaml`
- `apps/v1/deployments/vote/vote-frontend.yaml`
- `v1/services/vote/vote-frontend.yaml`
- `traefik.io/v1alpha1/ingressroutes/vote/frontend-static.yaml`

So removing `test/e2e/setup/demo-only/vote` is possible, but the demo test should either be retargeted to the new voter
namespace or changed to verify generic demo repository activity instead of legacy `vote-frontend` files.

## Recommended Fast Path

For time pressure, use branch-local manifests in this repo for the two demo instances, and keep upstream changes small.

Suggested layout:

```text
test/e2e/setup/demo-only/voter-gitops/
  base/
    kustomization.yaml
    app.yaml
    auth-service-rbac.yaml
    quiz-rbac.yaml
    examples/
  crds/
    coffeeconfigs.yaml
  production/
    kustomization.yaml
    namespace.yaml
    ingress-*.yaml
  test/
    kustomization.yaml
    namespace.yaml
    ingress-*.yaml
```

Use two namespaces:

- `voter-production`
- `voter-test`

Use local Kustomize overlays for the immediate demo. GitOps Reverser and Flux-based promotion can come after this is
stable.

This path is a little cumbersome, but it keeps the demo unblocked and keeps the routing manifests obvious. It also avoids
waiting on a polished upstream packaging story before the demo is usable.

## CoffeeConfig Promotion Demo

The demo is specifically about promoting application config, not the whole application. For now, narrow GitOps Reverser
to `CoffeeConfig` only.

The expected flow is:

1. `demo-test.configbutler.ai` runs in namespace `voter-test`.
2. The test app uses `CoffeeConfig/testnet-coffee` as live data.
3. A `WatchRule` in `voter-test` watches only `coffeeconfigs.examples.configbutler.ai`.
4. A `GitTarget` writes those changes to branch `demo-test` in the local Gitea demo repository.
5. A `GitProvider` allows pushes to `demo-test` and reads from `main`.
6. Flux in `voter-test` follows branch `demo-test` so Git changes are picked up directly by the test app.
7. Later, `demo-test` is merged to `main` by PR in the local Gitea demo repository.
8. Flux in `voter-production` follows branch `main` and applies the same generated CoffeeConfig path to
   namespace `voter-production`.

The generated resource is expected to be committed with the test namespace, because it is captured from `voter-test`.
Production must therefore use Flux `Kustomization.spec.targetNamespace: voter-production`, matching the existing
`podinfos-production` idea: intent can come from one namespace-shaped path, while the target environment overrides where
it is applied.

One API limitation is worth calling out: `WatchRule` filters by resource type, not object name. For this demo it can
watch only `CoffeeConfig` resources in the `voter-test` namespace, but it cannot currently express "only
`CoffeeConfig/testnet-coffee`" without an upstream GitOps Reverser feature.

The branch-local implementation uses:

| Concern | Value |
|---|---|
| Test namespace | `voter-test` |
| Production namespace | `voter-production` |
| GitProvider | `voter-test/demo-coffeeconfig` |
| GitTarget | `voter-test/demo-coffeeconfig` |
| GitTarget branch | `demo-test` |
| GitTarget path | `voter-coffee` |
| Watched resource | `examples.configbutler.ai/v1alpha1`, resource `coffeeconfigs` |
| Test Flux source | `voter-test/demo-coffeeconfig`, branch `demo-test` |
| Production Flux source | `voter-production/demo-coffeeconfig`, branch `main` |
| Flux apply path | `/voter-coffee/examples.configbutler.ai/v1alpha1/coffeeconfigs/voter-test/` |

The Cloudflare side only needs both published applications pointing to Traefik:

- `demo.configbutler.ai` -> `http://traefik.traefik-system.svc.cluster.local:80`
- `demo-test.configbutler.ai` -> `http://traefik.traefik-system.svc.cluster.local:80`

With those in place, the in-cluster Traefik `IngressRoute` host matches decide whether traffic goes to
`voter-production` or `voter-test`.

## Required Local Changes For Two Instances

For each instance, make these values unique:

| Concern | `demo.configbutler.ai` | `demo-test.configbutler.ai` |
|---|---|---|
| Namespace | `voter-production` | `voter-test` |
| Local overlay | `production` | `test` |
| Traefik host | `demo.configbutler.ai` | `demo-test.configbutler.ai` |
| Auth service account subject | `voter-production/auth-service` | `voter-test/auth-service` |
| Forward auth URL | `auth-service.voter-production.svc` | `auth-service.voter-test.svc` |
| `FORWARD_SA_NAMESPACE` | `voter-production` | `voter-test` |
| CoffeeConfig name | instance-specific or shared name in each namespace | instance-specific or shared name in each namespace |

Cluster-scoped names must also be unique if both instances are installed from raw YAML:

- `ClusterRole/voter-production-auth-service`
- `ClusterRoleBinding/voter-production-auth-service`
- `ClusterRole/voter-test-auth-service`
- `ClusterRoleBinding/voter-test-auth-service`
- `ServersTransport/voter-production/voter-production-kube-apiserver-transport`
- `ServersTransport/voter-test/voter-test-kube-apiserver-transport`

The CRDs should not be duplicated per instance. Install them once as shared demo infrastructure:

- `coffeeconfigs.examples.configbutler.ai`
- `quizsessions.examples.configbutler.ai`
- `quizsubmissions.examples.configbutler.ai`

## Upstream Changes Needed

Upstream should become instanceable. The important changes are:

1. Split CRDs from app installation.

   Put CRDs in a separate path such as `k8s/crds`, and app resources in a path such as `k8s/app` or
   `k8s/overlays/default`. Flux can install CRDs once, then create multiple app instances.

2. Remove hardcoded namespace assumptions.

   Upstream should support namespace selection through Kustomize overlays, Helm values, or substitutions. The current
   fixed `metadata.namespace: voter` appears in deployments, services, RBAC, example objects, and Traefik resources.

3. Rename cluster-scoped RBAC.

   Never use generic cluster-scoped names like `auth-service` in reusable manifests. Use a release/instance-specific
   name, for example `voter-auth-service`, or template it as `${name}-auth-service`.

4. Make ClusterRoleBinding subjects configurable.

   The subject namespace must match the instance namespace. This is the direct cause of the CoffeeConfig permission
   failure.

5. Make ingress hosts configurable or optional.

   Upstream should not force `voter.z65.nl` or `coffee.z65.nl`. Either move ingress to overlays or expose host values.

6. Make auth-service configuration configurable.

   These should be values, not hardcoded demo constants:

   - `FORWARD_SA_NAMESPACE`
   - `COFFEE_CONFIG_NAME`
   - `ADMIN_PASSWORD`
   - `COOKIE_SECURE`
   - image pull secret name

7. Consider unique cookie/session settings per host.

   If both apps share a parent domain, confirm cookie names and domains do not conflict. Host-only cookies are usually
   fine, but explicit cookie domain settings would need care.

## Should We Vendor All Kubernetes Resources Here?

Short term: yes, if the demo date is close.

Keeping the full Kubernetes resources in this repo, or adding local Flux patches around the upstream repo, is the fastest
way to get two working instances. It gives us complete control over hostnames, RBAC names, namespaces, and cleanup.

Long term: no.

Copying all resources here means the demo fork becomes its own packaging layer. Every upstream app change then has to be
copied or re-patched here, which is exactly the cumbersome feeling. The durable solution is upstream packaging that accepts
instance values.

## Practical Plan

1. Fix the live single-instance bug immediately.

   Rename or patch the voter RBAC so `system:serviceaccount:voter:auth-service` can read CoffeeConfig.

2. Remove the old `vote` app from demo-only setup if the new voter app is now the real demo UI.

   Keep only what the reverse-GitOps demo test still needs, or update that test so it no longer expects
   `vote-frontend` and old Traefik files.

3. Add a second local voter instance.

   Create `voter-production` and `voter-test` overlays with separate hosts and unique cluster-scoped names.

4. Once the demo is stable, upstream the reusable packaging changes.

   The upstream target should be either Kustomize components/overlays or a Helm chart. For this use case, a small Helm
   chart may be less awkward than many JSON patches because it naturally handles names, namespaces, hosts, image tags,
   and environment values.

## Decision

Use local demo manifests to get two instances running quickly, but treat that as a bridge. Upstream should be changed so
`voter` can be installed multiple times by Flux without copying or deeply patching raw Kubernetes YAML.
