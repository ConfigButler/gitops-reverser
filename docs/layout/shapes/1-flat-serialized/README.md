# 1 — Flat folder, namespaces serialized

One directory, one file per object, and every namespaced document carries its own
`metadata.namespace`. This is the shape a human hands to `kubectl apply -f`, and the only one in the
set that needs no `-R` and no deployer to be correct.

## Starting repository

[`repository/`](repository/) is the repository root. The target's path, `mirror/prod`, already
holds two namespaces:

```text
mirror/prod/
  billing-invoices.yaml
  shop-web.yaml
```

## Configuration

[`config/gittarget.yaml`](config/gittarget.yaml) declares two things, and both are load-bearing.

**Flat is a declared shape.** With no `placement.default` the built-in ladder ends at the canonical
identity path, which is a tree. `"{namespace}-{name}.yaml"` is what asks for one directory. The
`{namespace}` prefix is what keeps two namespaces from colliding on a common name like `config`;
without it, `shop/config` and `billing/config` resolve to one path and
[append into a multi-document file](../../new-file-placement-rules.md) rather than overwrite — legal,
shipped, and probably not what the folder wanted.

**`serializeNamespace: true` matches what inference would do here anyway.** No kustomization governs
the path, so the namespace would be written regardless. Declaring it pins the convention: if someone
later drops a `kustomization.yaml` with `namespace: shop` into this folder, inference would start
omitting the namespace from `shop` documents on their next write, and this folder would quietly stop
being self-describing. The flag makes that a folder property rather than a consequence.

## Scenario contract

- Starting repository: [`repository/`](repository/).
- Live input: [`input/checkout-config.yaml`](input/checkout-config.yaml).
- Expected Git change: [`expected-checkout-config.patch`](expected-checkout-config.patch).
- Expected status: `Ready=True`, `LayoutResolved` reason `None` — there is no render root, and the
  message says so rather than leaving the field unexplained.

## Empty folder

**Works with no extra machinery.** Nothing needs to exist before the first write: the template
resolves from the object's own identity, no root is required, and the namespace comes from the
object. This shape and shape 3 are the two that bootstrap themselves.

## Consumers

`kubectl apply -f mirror/prod/` — one directory, no recursion flag, which is what this shape is for.
A Flux `Kustomization` or Argo `Application` also works, provided neither asserts a namespace: a
target namespace **overrides** what these documents carry rather than filling a blank, and would
collapse both namespaces into one. The measurement behind that is in
[What the consumers actually do](../README.md#what-the-consumers-actually-do).
