# 4 — Tree, namespace-free

Subfolders that group by **type**, not by namespace, and documents that carry no
`metadata.namespace`. The folder is one namespace's worth of content, organized for a human to
browse, and portable into whichever namespace the deployer names.

## Starting repository

```text
apps/checkout/
  configmaps/web.yaml
  deployments/web.yaml
```

## Configuration

[`config/gittarget.yaml`](config/gittarget.yaml) declares `"{resource}/{name}.yaml"` and
`serializeNamespace: false`. Note what is **not** in that template: `{namespace}`. That is not a
detail, it is the precondition — see below.

[`config/consumer-argocd-application.yaml`](config/consumer-argocd-application.yaml) is not written
by the operator. It carries the two opt-in fields this shape needs: `directory.recurse: true`,
because Argo CD reads only the top directory otherwise, and `destination.namespace`, which supplies
what the documents omit.

## Why this shape is single-namespace

`serializeNamespace: false` in a **multi-namespace** tree is the sharpest edge in the model, and it
is worth stating plainly because the folder looks like it should work. Canonical paths would put
`shop/` and `billing/` in separate subtrees, so the tree still *looks* like it distinguishes them —
but the namespace would live only in the path, and a path is not something any applier reads. A
`kubectl apply -R -n shop`, a Flux `targetNamespace`, or an Argo `destination.namespace` puts
**everything under the target path into one namespace**, silently merging two namespaces' objects of
the same name.

Two ways out, and this scenario takes the first:

- **Keep the folder single-namespace**, which a template without `{namespace}` guarantees by
  construction — and which the
  [one-source-namespace rule](../../../../../docs/layout/model.md#the-second-guard-one-source-namespace-and-this-one-refuses)
  turns from construction into enforcement: an explicit `serializeNamespace: false` refuses the
  second source namespace rather than writing the merged folder described above.
- **Nested roots, one per namespace subfolder**, each with its own `kustomization.yaml` carrying its
  own `namespace:`. Fact 2 in [`../../model.md`](../../../../../docs/layout/model.md) measured that this renders
  correctly, and inference already resolves it per document. That route requires leaving
  `serializeNamespace` **unset** rather than `false`: the folder is genuinely non-uniform, which is
  what unset is for, and the rule above refuses the explicit claim precisely because it would be a
  false one. Nothing creates those roots today, and whether `useKustomize` should is
  [an open question](../../../../../docs/layout/model.md#open-questions), deliberately deferred.

## Scenario contract

- Starting repository: [`repository/`](repository/).
- Live input: [`input/checkout-config.yaml`](input/checkout-config.yaml).
- Expected Git change: [`expected-checkout-config.patch`](expected-checkout-config.patch) — a new
  file at `configmaps/checkout-config.yaml` with no namespace in it.
- Expected status: `Ready=True`. Nothing complains about the missing supplier, and that is the
  decided behaviour rather than a gap — see below.

**The supplier is unknowable, and the operator says nothing about it.** This folder is *correct*,
and the configuration that supplies its namespace is a Flux `targetNamespace` or an Argo
`destination.namespace` in another cluster. Two deployers may point at this folder and land it in
two different namespaces, both correctly — being unbound is what the shape is for, so nothing here
reports on it. See
[`model.md`](../../../../../docs/layout/model.md#why-false-needs-no-guard).

The rule that *is* enforced is on the inside of the folder:
[one source namespace](../../../../../docs/layout/model.md#the-second-guard-one-source-namespace-and-this-one-refuses),
where two namespaces would collapse onto one namespace-free document. That loss is visible in the
folder the operator owns, so it is refused.

## Empty folder

**Same gap as shape 2, and the same answer.** Nothing to infer from, so `serializeNamespace: false`
has to be declared, and no scan of the folder can confirm it was right. The supplier is in another
cluster — and since no scan could confirm it in a *populated* folder either, the empty folder is not
a special case here. It is the general one.
