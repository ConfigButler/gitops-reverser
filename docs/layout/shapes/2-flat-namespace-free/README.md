# 2 — Flat folder, namespace-free

The same single directory, with `metadata.namespace` deliberately absent from every document. The
folder is a portable artifact: the deployer chooses the namespace it lands in, which is what a Flux
`targetNamespace`, an Argo `destination.namespace`, or `kubectl apply -n` is for.

This shape and [shape 4](../4-tree-namespace-free/README.md) are the two whose supplier lives
outside the repository, and they are the reason `serializeNamespace` has to be a declared field
rather than an inference.

## Starting repository

```text
apps/checkout/
  web.yaml            # no metadata.namespace
```

## Configuration

[`config/gittarget.yaml`](config/gittarget.yaml) declares `"{name}.yaml"` — no `{namespace}` in the
template, so the folder is single-namespace by construction — and `serializeNamespace: false`.

[`config/consumer-flux-kustomization.yaml`](config/consumer-flux-kustomization.yaml) is **not
written by the operator**. It is the missing half of the folder, in a different cluster, and the set
would be dishonest without it: this shape only means something in the presence of a deployer that
supplies what the documents omit.

## Scenario contract

- Starting repository: [`repository/`](repository/).
- Live input: [`input/checkout-config.yaml`](input/checkout-config.yaml) — a `ConfigMap` in `shop`.
- Expected Git change: [`expected-checkout-config.patch`](expected-checkout-config.patch). The
  input's `namespace: shop` does not appear in it. That subtraction is the assertion.
- Expected status: `Ready=True`, and see the guard below.

## The guard has nothing to check

`serializeNamespace: false` is honest only when something guarantees the namespace, and the post-scan
pass re-checks that on every scan by looking at the folder. Here the guarantee is a `Kustomization`
in the deploying cluster, which the operator cannot see, so a guard that only accepts a
`kustomization.yaml` reports `Validated=False` against a perfectly correct folder.

That is the strongest argument for
[naming the supplier](../../model.md#open-questions): a `false` that says *"external, asserted"*
moves the responsibility to the user explicitly, instead of leaving the operator to choose between a
false alarm and no check at all. Until that exists, this shape is the one where the guard has to
stay a report rather than a refusal.

## What if two namespaces reach this target?

The folder is single-namespace *by construction* — there is no `{namespace}` in the template — but
construction is not a fence. [`config/watchrule.yaml`](config/watchrule.yaml) names no
`sourceNamespace`, so it captures its own namespace, `shop`. Nothing stops a **second** `WatchRule`
pointing at the same `GitTarget` and naming `billing`, and nothing today notices when one does.

Then the folder loses information, in two different ways:

- `shop/web` and `billing/invoices` become two namespace-free documents in one folder. The consuming
  Flux `Kustomization` names one namespace, so `billing`'s object is applied into `shop`. The folder
  is exactly what it claims to be; the claim is just false, and Git holds no record of it.
- `shop/config` and `billing/config` both resolve to `config.yaml`, and with the namespace stripped
  their manifest identities are equal. The second object does not collide with the first document —
  it **matches** it. Two live objects, one document, each write flipping it.

[Shape 1](../1-flat-serialized/README.md) has neither problem, because the namespace is in the path
*and* in the bytes. The distinction is not flat-versus-tree: it is that this shape deletes the only
copy of the information that would have told the two objects apart.

**This is decided, and it is a rule rather than a field: an explicit `serializeNamespace: false`
admits exactly one source namespace, and the second is refused.** The argument is in
[the shapes README](../README.md#a-namespace-free-folder-needs-a-fence-around-one-namespace) and in
[`model.md`](../../model.md#the-second-guard-one-source-namespace-and-this-one-refuses); it ships
with the field, in PR 2. Unlike the supplier question above, it is answerable entirely inside the
cluster: the set of source namespaces reaching a target comes from the rules that name it, not from
the folder — so this shape gets a real fence even though its *supplier* stays unverifiable.

## Empty folder

**This is the case the refactor exists for.** An empty `apps/checkout` supplies nothing to infer
from, so unset would fall back to writing the namespace — the opposite of what this folder is. Only
an explicit `serializeNamespace: false` produces the intended first commit, and nothing in the
repository will ever prove it was right. Compare [shape 5](../5-kustomize-single-folder/README.md),
where `useKustomize: true` supplies the proof in the same commit.
