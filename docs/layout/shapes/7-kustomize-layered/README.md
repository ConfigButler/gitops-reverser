# 7 — Layered kustomize

A base, a shared layer on top of it, and environments on top of that. It is
[shape 6](../6-kustomize-base-and-overlays/README.md) with one more level, and the extra level is
what turns "a leaf target is a good idea" into "a leaf target is the only writable folder".

## Starting repository

```text
apps/checkout/
  base/
    kustomization.yaml
    deployment.yaml
  layers/observability/
    kustomization.yaml            # resources: [../../base], patches the Deployment
    scrape-annotations.yaml
  envs/
    test/kustomization.yaml       # namespace: shop-test, resources: [../../layers/observability]
    prod/kustomization.yaml       # namespace: shop-prod, resources: [../../layers/observability]
```

## Fan-in decides the write partition

`layers/observability` is consumed by `envs/test` and `envs/prod`, and `base/` by the layer. A
target at `apps/checkout` scans all four kustomizations, so both `envs/*` are render roots, the layer
and the base are referenced by them, and every file below the layer is reached by two roots. The L2
precondition — never write a file more than one render root consumes — refuses the whole flush.

**From the leaf, the arithmetic is different and the outcome is the same.**
[`config/gittarget-prod.yaml`](config/gittarget-prod.yaml) scans `envs/prod` plus the files it reads,
where `envs/prod` is the only root and nothing has fan-in above one. L2 is silent; **L1** — the write
jail at `spec.path` — is what makes the layer and the base read-only. The two preconditions cover
different mistakes rather than the same one twice, and only one of them is a graph fact.

**This is what the user meant by layering influencing the other design decisions.** Adding a layer
does not change where a new file goes — it still goes beside the leaf's root — but it moves more of
the repository out of reach, and it makes more live changes unexpressible inside the target. That is
the trade the shape buys its reuse with.

## Scenario contract, part one: a placement

- Live input: [`input/checkout-config.yaml`](input/checkout-config.yaml), a `ConfigMap` in
  `shop-prod`.
- Expected Git change: [`expected-checkout-config.patch`](expected-checkout-config.patch) — the file
  lands in `envs/prod/`, joins that root's `resources:`, and omits `metadata.namespace` because the
  leaf root supplies `namespace: shop-prod`. Identical to shape 6: the layer changes nothing about
  placement.

## Scenario contract, part two: a refusal

The half worth reviewing. Someone changes `prometheus.io/scrape` on the live Deployment. The only
expression of that field in the repository is the patch in `layers/observability`, which is outside
this target's write scope and consumed by two roots besides.

[`expected-shared-layer-status.yaml`](expected-shared-layer-status.yaml) is the whole result: no file
is written, and `WriteBoundaryRefused` names the boundary rather than searching for another document
carrying the same identity. A set of examples in which every write succeeds would be advertising
rather than specifying, which is why this one is here.

Changing that annotation for every environment at once is a **Git-level operation above the
operator** — the
[cross-environment editing decision](../../../design/support-boundary/gittarget-granularity-and-cross-environment-edits.md)
settles that promotion and factor-into-base are verbs for the layer above, not a reason to widen a
target across environments.

## Mounting the layer, or the base

Both are accepted and both are editable, for the reason
[shape 6 sets out](../6-kustomize-base-and-overlays/README.md#what-if-a-target-mounts-the-base): read
scope follows `resources:` outward, so a target rooted at `layers/observability` scans itself and the
base, sees one render root, and has fan-in one everywhere. The two environments that consume it are
invisible to it. A write there reaches `test` and `prod` both.

Layering makes that sharper rather than different: there are now two shared levels to mount by
mistake, and the deeper one reaches more environments.

## Empty folder

**Not bootstrappable, for the same reason as shape 6 and one more.** `useKustomize: true` cannot
invent `resources: [../../layers/observability]`, and it certainly cannot invent the layer. A layered
repository is scaffolded by a template or by hand, and GitOps Reverser adopts it afterwards — which
[`spec.suspend`](../../model.md) exists to make a dry run rather than a leap.
