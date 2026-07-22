# flux-resourceset-inline

## What this is

A flux-operator `ResourceSet`: one document that carries **both** a template and
the inputs it is rendered against. The controller renders `spec.resources` once
per entry in `spec.inputs` and applies the result.

The flux-operator guides present this as a **replacement for base + overlays** —
their own worked example collapses an
`apps/app1/{base,overlays/tenant1,overlays/tenant2}` tree into "a single file".
That tree is exactly [`2-rendered/kustomize-overlays/`](../../2-rendered/kustomize-overlays/).
The two fixtures are the same intent expressed two ways, and the difference is the
whole point of this family: over there, every tenant's `Deployment` is a real file;
here, **no tenant has a file at all**.

## Layout

```yaml
flux-resourceset-inline/
├── README.md
└── tenants/
    └── apps.yaml        # ONE document: template + 3 inputs -> 9 live objects
```

Three inputs × three resources (`Namespace`, `Deployment`, `Service`) = **nine
live objects, zero of which have a home file.** The only artifact in Git is the
`ResourceSet` CR itself.

## What makes it structurally distinct

- **The KRM is nested inside KRM.** `spec.resources` is a list of arbitrary
  Kubernetes objects embedded in the body of another Kubernetes object. A
  `Deployment` here is not a document — it is a *field value*.
- **No `path`, no `sourceRef`.** Flux's own `Kustomization` has
  `spec.path` + `spec.sourceRef` pointing at a folder of files. `ResourceSet`
  deliberately has neither. There is nowhere for it to point, because the content
  is already inside it.
- **A third templating dialect.** Go `text/template` with `<< >>` delimiters —
  not Helm's `{{ }}`, and not Argo CD's fasttemplate. Three ecosystems, three
  incompatible ways to spell "substitute here", none of them YAML.
- **Fan-in is the input count.** `spec.resources[i]` is reached by all three
  inputs; `spec.inputs[j]` is reached by exactly one. Those are different
  write-fan-in numbers on two fields of the same document.
- **`replicas: << inputs.replicas >>`** is an *unquoted* template substitution
  into an integer field. The document is therefore not valid KRM until it is
  rendered — `spec.replicas` holds a string that must become a number.

## Observed behaviour (live cluster, 2026-07-13)

Run against a real flux-operator, the children carry:

```yaml
labels:
  resourceset.fluxcd.controlplane.io/name: tenant-apps
  resourceset.fluxcd.controlplane.io/namespace: flux-system
ownerReferences: <none>
managedFields[].manager: flux-operator
```

plus a `status.inventory` on the `ResourceSet` listing every object it owns.
**There is no `ownerReference` anywhere** — the parent/child link is carried by
labels and an inventory, not by the mechanism Kubernetes provides for it.

## Open questions

- The `ResourceSet` CR is one editable document, and `spec.inputs[j]` has fan-in
  1. Is "bump tenant2 to 1.5.0" therefore an edit to `spec.inputs[1].image` — an
  edit *into a field of a document*, rather than to a document?
- Editing a `Deployment` nested at `spec.resources[1]` means writing KRM into a
  field of another KRM document. Nothing in the writer models that today. Is a
  nested document a document?
- The nine live objects have no file. If they are mirrored anyway, Git gains nine
  files that nothing will ever read and that the controller will fight. What
  prevents that?
- `replicas: << inputs.replicas >>` is unparseable as an integer before
  rendering. Is this file KRM at all, for the purpose of a scan?
- Adding a tenant means adding an entry to `spec.inputs` — not adding a folder and
  not authoring a CR. That is a *third* recipe for "add an app", different from
  both the plain-folder and the kustomize recipes. How is it discovered?
