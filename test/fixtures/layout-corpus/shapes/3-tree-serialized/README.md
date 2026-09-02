# 3 — Tree, namespaces serialized

The built-in canonical layout, and the only shape in this set that needs no layout configuration at
all: no `placement`, no flag. A path carries the object's identity —
`{namespaceOrCluster}/{groupPath}/{resource}/{name}.yaml` — and the document carries its namespace,
so a file means the same thing wherever it is read.

This is the **viewer** shape. Its first job is that a human, or a diff on a pull request, can see
what is in a cluster.

## Starting repository

[`repository/`](repository/) is the repository root, and the target's path is `clusters/home`:

```text
clusters/home/
  _cluster/rbac.authorization.k8s.io/clusterroles/homelab-viewer.yaml
  billing/configmaps/invoices.yaml
  shop/apps/deployments/web.yaml
```

Two conventions in those paths come from the
[canonical grammar](../../../../../docs/layout/new-file-placement-rules.md#template-variables): **the core group collapses
to nothing**, so `configmaps` sits directly under the namespace segment where
`rbac.authorization.k8s.io` appears in the ClusterRole path; and **`_cluster` stands in for the
namespace segment** of a cluster-scoped resource. An underscore is invalid in a namespace name, so
the sentinel cannot collide with a real one.

## Configuration

[`config/gittarget.yaml`](config/gittarget.yaml) sets `serializeNamespace: true` and nothing else.
Inference would produce the same bytes today — there is no root to inherit from — so the flag is a
statement about the folder rather than a change to it, the same pin as in
[shape 1](../1-flat-serialized/README.md).

**The flag governs namespaced resources only.** This folder also holds a `ClusterRole`, which has no
namespace to serialize, so the field is ignored for it rather than being an error. A tree is the
shape most likely to carry both, which is why it is worth saying here.

## Scenario contract

- Starting repository: [`repository/`](repository/).
- Live input: [`input/checkout-config.yaml`](input/checkout-config.yaml).
- Expected Git change: [`expected-checkout-config.patch`](expected-checkout-config.patch) — a new
  file at `shop/configmaps/checkout-config.yaml`, namespace included.
- Expected status: `Ready=True`, `LayoutResolved` reason `None`, `renderRoot: ""`.

## Empty folder

**Works.** The canonical path is derived from the object, not from the folder, so the first write
into an empty repository is the same write as the thousandth. Nothing to declare.

## Consumers

`kubectl apply -R -f clusters/home/` — the `-R` is required. Flux deploys it with no root in the
repository at all, and Argo CD needs `directory.recurse: true`. Why each of those is so is measured
in [What the consumers actually do](../README.md#what-the-consumers-actually-do).

**Do not give any of them a target namespace.** It would collapse this whole multi-namespace mirror
into one namespace, and `serializeNamespace: true` does not prevent it — which is the single most
important thing to know about consuming this shape.

A closely related use case with the same shape, including its `ClusterWatchRule` and source-cluster
configuration, was a separate `tree-multi-namespace` scenario; it is deleted, because this shape
specifies the same folder more precisely. One thing it carried is worth keeping: a cluster mirror
usually needs **two** rules, not one — a `WatchRule` for namespaced content (which is what
[`config/watchrule.yaml`](config/watchrule.yaml) is) and a separate `ClusterWatchRule` for
cluster-scoped objects like the `ClusterRole` under `_cluster/`, since a cluster-scoped rule carries
its scope in its kind and has no `sourceNamespace` to select.
