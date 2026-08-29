# Homelab cluster tree

This scenario mirrors a small home cluster into one repository subtree. The operator captures a
few namespaced resources from two namespaces and one cluster-scoped resource. It does not need a
Kustomize root to decide where a new object goes.

## Resulting repository folder

[`repository/`](repository/) shows the structure after three writes. The tree below uses the
target's path in the real repository; `repository/` in this folder **is** `clusters/home/`.

```text
clusters/home/
  _cluster/rbac.authorization.k8s.io/clusterroles/homelab-viewer.yaml
  media/apps/deployments/jellyfin.yaml
  media/configmaps/jellyfin.yaml
  monitoring/configmaps/grafana.ini.yaml
```

`Tree` derives each path from the object's namespace or cluster scope, API group, resource, and
name. A Deployment named `jellyfin` in another namespace receives another path. This property is
why `Tree` is the safe default for multi-namespace capture.

Two conventions in those paths are load-bearing, and both come from the canonical grammar in
[`new-file-placement-rules.md`](../../new-file-placement-rules.md#template-variables):

- **The core group collapses to nothing.** `configmaps` sits directly under the namespace segment,
  where `rbac.authorization.k8s.io` sits in the ClusterRole path, because the core group has no
  name to write.
- **`_cluster` stands in for the namespace segment** of a cluster-scoped resource. An underscore is
  invalid in a namespace name, so the sentinel cannot collide with a real namespace — which is why
  it is a sentinel and not a reserved word.

## Proposed configuration

[`config/clusterprovider.yaml`](config/clusterprovider.yaml) gives the homelab configuration
namespace permission to choose its source namespaces. The target's
[`config/gittarget.yaml`](config/gittarget.yaml) names those namespaces and declares `Tree` plus
`MultiNamespace`. [`config/watchrule.yaml`](config/watchrule.yaml) selects namespaced content, while
[`config/clusterwatchrule.yaml`](config/clusterwatchrule.yaml) separately selects the ClusterRole.

The separation is intentional. A `WatchRule` owns namespaced subscriptions. A
`ClusterWatchRule` owns cluster-scoped subscriptions. The folder has room for both because its
paths carry scope, but the source API keeps their authority separate.

## Scenario contract

- Starting repository: [`repository/`](repository/) after the three documents shown above.
- Live input: [`input/grafana-dashboards.yaml`](input/grafana-dashboards.yaml).
- First observation, before any commit. A `Tree` folder has no render root, so the reason says so
  rather than leaving the field unexplained:

  ```yaml
  status:
    conditions:
      - type: LayoutResolved
        status: "True"
        reason: None
        message: "no kustomization governs this subtree; new files take the canonical path"
    placement:
      renderRoot: ""
      serializeNamespace: Always
  ```

- Expected Git change:
  [`expected-grafana-dashboards.patch`](expected-grafana-dashboards.patch), after
  a reviewer clears `suspend`.
- Expected status: `Ready=True` with a `Tree` layout and a path derived from the input identity.
- Boundary: content outside `clusters/home`, a namespace not on the target, or an unsupported
  type is refused without probing for a sibling convention.

## Homelab use

This is a useful first mirror for a single-owner cluster: changes made with `kubectl`, a dashboard,
or an operator appear as reviewable Git documents without choosing a bundle convention first. A
GitOps deployer can consume the tree through a recursively scanned directory or through a root
Kustomization that the repository owner maintains.

`serializeNamespace: Always` keeps each namespaced document portable outside this tree. Unlike a
single-namespace Kustomize folder, no common namespace transformer supplies an omitted value.

**The field governs namespaced resources only.** This folder also captures a `ClusterRole` through
its `ClusterWatchRule`, and a `ClusterRole` has no namespace to serialize, so
`serializeNamespace` is ignored for it rather than being an error. A tree is the shape most likely
to carry both kinds, which is why it is worth saying here.

## Boundary

The target has one write scope, `clusters/home`. It does not use one target for a shared base and
several environments. The per-overlay model remains the right choice when a repository has bases
and environment-specific overlays; see [GitTarget granularity].

[GitTarget granularity]: ../../../design/support-boundary/gittarget-granularity-and-cross-environment-edits.md
