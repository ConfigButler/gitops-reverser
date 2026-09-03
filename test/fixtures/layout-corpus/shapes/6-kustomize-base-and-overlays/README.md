# 6 — Base and three environment overlays

The default kustomize repository shape: one shared base, and `test`, `acceptance` and `prod`
overlays that each stamp their own namespace onto it. The layout question this shape asks is not
"where does a file go" but **"how many targets is this, and which folder may be written?"**

## Starting repository

```text
apps/checkout/
  base/
    kustomization.yaml            # no namespace: portable by design
    deployment.yaml
  overlays/
    test/kustomization.yaml       # namespace: shop-test,  resources: [../../base]
    acceptance/kustomization.yaml # namespace: shop-acc,   resources: [../../base]
    prod/kustomization.yaml       # namespace: shop-prod,  resources: [../../base]
```

## One target per leaf overlay

Three environments are **three `GitTarget` objects**, each rooted at a leaf:
[`config/gittarget-app-root.yaml`](config/gittarget-app-root.yaml) is the same repository pointed at
`apps/checkout` instead of a leaf, which covers four render roots at once:
[`expected-app-root-status.yaml`](expected-app-root-status.yaml) is the `LayoutResolved=Ambiguous`
it earns and the refusal that comes with it. It is the one refusal in this set the operator enforces
today.

[`config/gittarget-prod.yaml`](config/gittarget-prod.yaml) and
[`config/gittarget-test.yaml`](config/gittarget-test.yaml) are the same object with the environment
swapped, and neither declares any layout configuration at all.

A single target at `apps/checkout` is refused, by three independent rules that happen to agree:
it covers four render roots, `base/` has fan-in three *from that target's point of view*, and a
target is a write partition. The argument is cross-cutting — it decides this shape and
[shape 7](../7-kustomize-layered/README.md) identically — so it lives in
[Why only a leaf can be a kustomize target](../README.md#why-only-a-leaf-can-be-a-kustomize-target).

What is specific to this shape: the base is read-only from a leaf because of **L1**, the write jail,
not because of fan-in — from `overlays/prod` the scan holds one render root and nothing has fan-in
above one. Which is also why [mounting the base directly](#what-if-a-target-mounts-the-base) is not
refused at all.

## Scenario contract

- Starting repository: [`repository/`](repository/), the full `apps/checkout` tree.
- Live input: [`input/checkout-config.yaml`](input/checkout-config.yaml) — a `ConfigMap` in
  `shop-prod`.
- Expected Git change: [`expected-checkout-config.patch`](expected-checkout-config.patch): the file
  lands in `overlays/prod/`, joins that root's `resources:`, and omits `metadata.namespace` because
  the overlay's root supplies `namespace: shop-prod`.
- Expected status: `Ready=True`, `LayoutResolved=True`, reason `SingleKustomization`,
  `renderRoot: .`, `serializeNamespace: false` inferred.
- Boundary: the target reads `../../base` to render and never writes into it. A change that only the
  base can express is refused as outside `spec.path`; it is not redirected to another document with
  the same identity.

The same boundary from the write-path side, with an `images:` transformer edit rather than a
placement, is [shape 8](../8-base-owned-field-edit/README.md).

## What if a target mounts the base?

Nothing stops it, and that is the finding. A `GitTarget` with `path: apps/checkout/base` is accepted
and **the base is editable**, because neither write-boundary layer objects:

- **L1 is satisfied.** Every write is inside `spec.path`. The jail is doing its job; the jail is just
  somewhere else.
- **L2 never fires.** Read scope is resolved by following this subtree's kustomizations *outward* —
  `resources:` entries point from an overlay to its base, never the other way — so the scan holds
  `base/` alone. `base/kustomization.yaml` is referenced by nothing in it, so it is the one render
  root, and every file has fan-in one. **The three overlays are not merely unprotected; they are
  invisible.**

So a write here lands in the shared base and reaches `test`, `acceptance` and `prod` on their next
sync. That is not a bug in the boundary — it is the boundary working exactly as specified, on a
`spec.path` that says "the shared default is mine". It is also, in effect,
[Option C of the granularity decision](../../../../../docs/design/support-boundary/gittarget-granularity-and-cross-environment-edits.md)
being exercised today: edit the shared default, reach every overlay that does not override that
field. That option was decided as a **later, narrower verb** — for shared defaults, never as the
answer to "edit every environment" — and mounting the base is the unguarded version of it.

**What still stands between the base and a live object is identity, not the write boundary.** A base
is written namespace-free on purpose, so its documents are namespace-less in the store, while the
live object the target captures is in some real namespace. The two do not match by identity, so the
live object is not an update to `base/deployment.yaml` — it is a *new* object, and placement would
put a second, namespaced Deployment beside the base's own, which the render check at the write path
is the backstop for. Which of those two things happens first is exactly the kind of question this
corpus exists to pin, and no scenario pins it yet.

**The practical answer: do not point a target at a base.** If the intent is "change the default for
every environment", that is a Git-level operation above the operator today. If the intent is "this
folder is one deployable thing", then it is not a base — it is [shape 5](../5-kustomize-single-folder/README.md).

## What if a target mounts an overlay and a new object appears?

**It becomes an environment-specific file, which is what you would expect** — that is the
[scenario contract](#scenario-contract) above: `overlays/prod/checkout-config.yaml`, registered in
`overlays/prod/kustomization.yaml`, namespace omitted because the overlay's root supplies
`shop-prod`. The base is never consulted for placement; a new document goes beside the target's own
root.

The important qualifier is what counts as **new**. Match-first identity runs against the whole render
scope, and the render scope **includes the base**. So an object the base already defines is not a new
object, and there are three outcomes rather than one:

| The live change | Where it goes |
|---|---|
| An object no document in the render scope matches | a new file in the overlay, registered — environment-specific, as expected |
| An `images:` or `replicas:` field of a **base-owned** object | an entry in the **overlay's own** `kustomization.yaml`. Also environment-specific, and shipped — that is [shape 8](../8-base-owned-field-edit/README.md) |
| Any other field of a **base-owned** object | **refused.** `WriteBoundaryRefused`: the matching document is `base/deployment.yaml`, outside the write jail |

The third row is the one that surprises people, and it is deliberate: the operator does **not**
silently invent an overlay override for a field it has no proven way to express. Doing that safely —
authoring a narrow strategic-merge patch into the overlay and proving the rebuild changes that field
and nothing else — is designed and unshipped in
[`patch-authoring.md`](../../../../../docs/design/support-boundary/patch-authoring.md). Until it lands, "add a
file to the overlay" works and "change an inherited field" refuses unless it is an image or a replica
count.

### How that refusal actually happens

Not by a filter that inspects the edit up front: the edit is computed in full, the base file's buffer
goes dirty, and `pathScopePrecondition` refuses the **whole flush** before a byte moves. The refusal
is recorded once on the GitTarget as `WriteBoundaryRefused`, with no per-edit record, and it is
sticky until a resync succeeds.

[Shape 8](../8-base-owned-field-edit/README.md) is that walk-through as an executable scenario: the
same Deployment edited twice, once in a field the overlay can express and once in a field it cannot,
with the four steps, the blast radius, and what it looks like from outside the operator.

## Empty folder

**Half-bootstrappable.** `useKustomize: true` on an empty `apps/checkout/overlays/prod` writes a
root with `namespace: shop-prod` and registers what the operator places — and cannot write
`resources: [../../base]`, which is the line that makes the folder an overlay rather than a
standalone one. Why no flag fixes that, and where the missing half belongs instead, is
[the third empty-folder finding](../README.md#pointing-each-shape-at-an-empty-folder).
