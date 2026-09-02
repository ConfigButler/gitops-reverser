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
- Expected status: `Ready=True`, and nothing is reported about the missing supplier. Why not is the
  next section.

## There is no guard, because there is nothing to check

`serializeNamespace: false` is not checked against this folder, and it cannot be. The guarantee is a
`Kustomization` in the deploying cluster, which the operator cannot see — so a rule requiring
*something in the folder* to supply the namespace would report a fault against a perfectly correct
folder.

Naming the supplier instead (a `false` that also declares *"external, asserted"*) does not help
either, and for a stronger reason: **there is often no single supplier to name.** A raw
namespace-free folder can be consumed by two deployers into two different namespaces, both
correctly. That portability is what the shape is *for*. An assertion field would ask the user to
promise something that is not theirs to promise, and buy nothing, since nothing could check it
either way.

What is left is a division the rest of the model runs along: **guard what is inside the folder, say
nothing about what happens after it leaves.** The next section is a rule on the inside of that line,
and it is enforced.

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
[`model.md`](../../../../../docs/layout/model.md#the-second-guard-one-source-namespace-and-this-one-refuses). Unlike
the supplier question above, it is answerable entirely inside the
cluster: the set of source namespaces reaching a target comes from the rules that name it, not from
the folder — so this shape gets a real fence even though its *supplier* stays unverifiable.

It is a fixture rather than only an argument.
[`config/gittarget-second-namespace.yaml`](config/gittarget-second-namespace.yaml) and
[`config/watchrule-second-namespace.yaml`](config/watchrule-second-namespace.yaml) are this folder
with the mistake made, and
[`expected-second-namespace-status.yaml`](expected-second-namespace-status.yaml) is the refusal.
The corpus runs it.

## Empty folder

**This is the case the refactor exists for.** An empty `apps/checkout` supplies nothing to infer
from, so unset would fall back to writing the namespace — the opposite of what this folder is. Only
an explicit `serializeNamespace: false` produces the intended first commit, and nothing in the
repository will ever prove it was right. Compare [shape 5](../5-kustomize-single-folder/README.md),
where `useKustomize: true` supplies the proof in the same commit.
