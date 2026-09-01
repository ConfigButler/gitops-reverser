# 5 — One kustomize folder

One `kustomization.yaml` at `spec.path`, which lists every file beside it and supplies the namespace
they all omit. This is the shape the two flags were designed around, and the one where adoption and
creation differ by two lines of spec.

## Adopting an existing folder

[`repository/`](repository/) is the repository root. The target's path, `apps/checkout`, already has
a root that supplies `namespace: shop`:

```text
apps/checkout/
  kustomization.yaml     # namespace: shop, resources: [web.yaml]
  web.yaml
```

[`config/gittarget.yaml`](config/gittarget.yaml) declares **nothing** — no template, neither flag —
and that is the scenario. A new file is placed beside the root, joins its `resources:`, and inference
omits `metadata.namespace` because the root sets `namespace:` to this resource's own namespace.

Registering into a root that already exists is an **invariant, not a setting**: a file no
kustomization lists is a file nothing renders, which was
[#295](https://github.com/ConfigButler/gitops-reverser/issues/295), and
[#319](https://github.com/ConfigButler/gitops-reverser/pull/319) made it unconditional. `useKustomize`
has no bearing on this half.

The new file is named **`checkout-config.yaml`**, not `configmap-checkout-config.yaml`. The rung
names a new sibling `{name}.yaml` and does not learn a naming convention by looking at the
neighbours; that inference was deliberately deleted. `placement.default: "{kindLower}-{name}.yaml"`
is how to ask for the other convention on purpose.

- Expected Git change: [`expected-checkout-config.patch`](expected-checkout-config.patch) — two
  hunks, the `resources:` entry and the file.
- Expected status: `LayoutResolved=True`, reason `SingleKustomization`, `renderRoot: .`,
  `serializeNamespace: false` **inferred**.

## Creating the folder from empty

[`config/gittarget-empty-folder.yaml`](config/gittarget-empty-folder.yaml) is the same target with
two lines added, and it is the only difference between adopting and creating:

```yaml
  serializeNamespace: false
  placement:
    useKustomize: true
```

The first commit is already a kustomize folder —
[`expected-empty-folder-first-write.patch`](expected-empty-folder-first-write.patch) writes the root
and the document together:

```text
apps/checkout/
  kustomization.yaml     # resources: [checkout-config.yaml], and no namespace:
  checkout-config.yaml   # no metadata.namespace
```

**The created root carries no `namespace:`, and that is the decision rather than an omission.**
`serializeNamespace: false` says this artifact does not encode its deployment namespace, and
creating a root must not quietly change that contract by pinning the namespace one file up, where it
is harder to see and impossible for an installer to override. The folder is a portable artifact: a
Flux `targetNamespace` or an Argo `destination.namespace` places it, exactly as it places
[shape 2](../2-flat-namespace-free/README.md). The full argument, and the four other answers that
were considered, is in
[`../../../design/created-root-namespace.md`](../../../design/created-root-namespace.md).

What the flag buys here is *structure*: an empty folder becomes a kustomize folder with the first
commit, and every later document joins the same root instead of scattering.

Contrast [shape 2](../2-flat-namespace-free/README.md), which is the same omission with the
guarantee in another cluster, and [shape 6](../6-kustomize-base-and-overlays/README.md), where
creating the root is *not* enough because an overlay is defined by a base reference the operator
cannot know.

## Boundary

A second kustomize root inside `spec.path` is a misconfiguration of the target rather than a
placement puzzle: it resolves to `Ambiguous` and does not cause the operator to pick one. A folder
with no root at all falls through to the canonical path — that is [shape 3](../3-tree-serialized/README.md),
not a failure.

Both halves used to be separate use-case scenarios (`brownfield-kustomize` for adoption,
`empty-repo-bootstrap` for creation); they were deleted in favour of this one folder, because the
whole point is that they differ by two lines of spec rather than by being different scenarios. The
bootstrap case is worth one extra note: the type it created was an application-configuration CRD,
which stands for the ordinary case of a team publishing *its own* KRM as the artifact — GitOps
Reverser requires nothing of the type beyond it being followable.
