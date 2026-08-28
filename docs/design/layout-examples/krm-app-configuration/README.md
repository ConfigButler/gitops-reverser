# Application configuration as KRM

This scenario starts with an empty Git repository. A team edits its own configuration CRDs in an
intent cluster, and GitOps Reverser creates the folder that a deployer consumes.

## Resulting repository folder

[`repository/`](repository/) is the result after the first `ShopConfiguration` write. The target
creates the root and registers the first document, so the first commit is already a Kustomize
folder:

```text
apps/shop/
  kustomization.yaml
  shopconfiguration-storefront.yaml
```

The root supplies `namespace: shop`. The Git document omits `metadata.namespace`, while the live
input carries it. The layout establishes that convention at the root rather than waiting for a first
event to do so.

## Proposed configuration

[`config/gittarget.yaml`](config/gittarget.yaml) declares a `Kustomize` layout with `create: true`.
It also constrains the source to one exact namespace.
[`config/watchrule.yaml`](config/watchrule.yaml) is the artifact manifest: it states which KRM
types the team wants included in the folder.

The type in this example, `config.shop.example/v1alpha1`, stands for an application configuration
CRD installed in the intent cluster. It is a specimen, not a requirement of GitOps Reverser.

## Scenario contract

- Starting repository: empty at `apps/shop`.
- Live input: [`input/shopconfiguration-storefront.yaml`](input/shopconfiguration-storefront.yaml).
- Expected Git change: [`expected-first-write.patch`](expected-first-write.patch).
- Expected status: `Ready=True` after the created root renders the captured KRM object.
- Boundary: a source outside `shop`, an unauthorized namespace, or an unsupported KRM expression is
  refused before a commit.

## Why this is a separate layout

The target does not need a general placement template. The layout owns the only structure it needs:
one `kustomization.yaml` and its `resources:` entries. The first write produces a folder that
`kubectl apply -k apps/shop` can build.

`scope: SingleNamespace` also makes the folder's input contract visible. A second source namespace
is refused instead of sharing a file name or changing the target's convention. A team that needs a
multi-tenant artifact uses a `Tree` target or several app folders.

This example is the configuration-as-data path from
[the direction review](../../../future/direction-and-configuration-surface.md). The folder is a
reviewable artifact; the KRM objects in the intent cluster are the editing surface.
