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
  storefront.yaml
```

The root supplies `namespace: shop`. The Git document omits `metadata.namespace`, while the live
input carries it. The layout establishes that convention at the root rather than waiting for a first
event to do so.

## Proposed configuration

[`config/gittarget.yaml`](config/gittarget.yaml) sets `useKustomize: true` and
`serializeNamespace: false`. It also constrains the source to one exact namespace.
[`config/watchrule.yaml`](config/watchrule.yaml) is the artifact manifest: it states which KRM
types the team wants included in the folder.

The type in this example, `config.shop.example/v1alpha1`, stands for an application configuration
CRD installed in the intent cluster. It is a specimen, not a requirement of GitOps Reverser.

## Scenario contract

- Starting repository: empty at `apps/shop`.
- Live input: [`input/storefront.yaml`](input/storefront.yaml).
- Expected Git change: [`expected-first-write.patch`](expected-first-write.patch).
- Expected status: `Ready=True` after the created root renders the captured KRM object.
- Boundary: a source outside `shop`, an unauthorized namespace, or an unsupported KRM expression is
  refused before a commit.

## Why the two flags belong together here

This is the one pairing in the model that closes its own loop, and it is why creating a root is
worth building at all. `serializeNamespace: false` is only honest when something guarantees the
namespace, and on an empty folder there is nothing to inspect: no kustomization exists to inherit a
convention from, so inference structurally cannot answer. `useKustomize: true` supplies the missing
half — the operator writes the `kustomization.yaml`, puts `namespace: shop` in it, and then
legitimately omits `metadata.namespace` from every document it places. That is also what makes the
created root **meaningful** rather than an empty file: the convention is established rather than
guessed.

The folder is single-namespace by construction rather than by declaration: no `{namespace}` appears
in any path, because no `placement` is declared and the created root places files beside itself. A
second source namespace has nowhere to go that would not collide, and nothing on the target says so.
The WatchRule feeding it is namespaced and names its own source namespace, which is the whole fence
after
[`source-scope-simplification.md`](../../../design/source-scope-simplification.md).

This example is the configuration-as-data path from
[the direction review](../../../future/direction-and-configuration-surface.md). The folder is a
reviewable artifact; the KRM objects in the intent cluster are the editing surface.
