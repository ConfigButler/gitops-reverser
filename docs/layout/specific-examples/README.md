# Specific examples: two ecosystems, and the shared prerequisites

> **design**: worked scenarios for the layout model in [`../model.md`](../model.md). The
> `GitTarget` files use `spec.serializeNamespace`, `spec.placement.useKustomize` and `spec.suspend`,
> none of which exist in the current release.
> Date: 2026-08-31.
> Index: [`../../INDEX.md`](../../INDEX.md)

[`../shapes/`](../shapes/README.md) is the cross-product: every folder shape a repository can have,
with the same live object written into all of them, so the only difference between two folders is
the configuration that produced it. **It is the specification, and it is where a layout question is
answered.**

This folder is the remainder — the scenarios that are not a folder *shape* at all, but a folder
shape meeting a **specific ecosystem's** conventions. Both of these are shape 5 or 6 underneath;
what makes them worth their own fixtures is everything around the shape: which objects the deployer
owns, which directory belongs to another controller, and which field is a landmine.

| Scenario | What is specific about it |
|---|---|
| [Homelab Argo CD](homelab-argocd/README.md) | An app-of-apps folder. The interesting part is not the layout but the Argo tracking-id annotation, which leaks into Git and hard-fails another app's sync |
| [Homelab Flux](homelab-flux/README.md) | Flux declarations across two layers — sources and `HelmRelease`s — kept in separate targets, and the `clusters/*/flux-system` directory that belongs to `flux bootstrap` and must never be a target |
| [Shared prerequisites](prerequisites/README.md) | The `GitProvider` specimen every target needs, and the two grants a cross-namespace source requires |

## Why these are not in `shapes/`

A shape fixture answers *"what does a `GitTarget` have to say to produce this folder?"* and holds
one variable at a time. These two hold several: the ecosystem's own objects are in the repository,
the deployer's configuration matters, and the boundary being demonstrated is usually about what the
operator **refuses** to touch rather than where it places a file.

Four earlier scenarios that lived here — a brownfield kustomize adoption, an empty-repo bootstrap, a
multi-namespace cluster tree, and an overlay-scoped target — were **deleted rather than moved**.
Each was a use-case retelling of a shape that specifies the same behavior more precisely and with
the refusal halves included:

| Deleted scenario | Now specified by |
|---|---|
| `brownfield-kustomize` | [shape 5](../shapes/5-kustomize-single-folder/README.md), adopted half |
| `empty-repo-bootstrap` | [shape 5](../shapes/5-kustomize-single-folder/README.md), empty-folder half |
| `tree-multi-namespace` | [shape 3](../shapes/3-tree-serialized/README.md) |
| `overlay-scoped-target` | [shape 6](../shapes/6-kustomize-base-and-overlays/README.md) and [shape 8](../shapes/8-base-owned-field-edit/README.md) |

## Fixture conventions

The same as `shapes/`, so one harness reads both:

- `repository/` — the relevant repository subtree, with each scenario stating whether it is the
  starting state or the state after the illustrated change.
- `config/` — the `GitTarget` and watcher objects that describe the target.
- `input/` — one live object **as the operator receives it from the API server**, not as it is
  written to Git.
- `expected-*.patch` — the exact change proposed for Git, without `index` lines.

[`../model.md`](../model.md#how-it-gets-built) turns both folders into an executable corpus in its
first PR; [`../../design/build-order.md`](../../design/build-order.md) says when, and what the
harness seam already is.
