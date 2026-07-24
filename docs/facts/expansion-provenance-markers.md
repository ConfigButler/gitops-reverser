# Expansion provenance markers, measured

> **reference** — durable background. Index: [`../INDEX.md`](../INDEX.md)
>
> Measured 2026-07-13 against a live cluster: Kubernetes v1.36 (k3d), Flux v2 +
> flux-operator, Argo CD v3.4.5. Every row below was **observed on a real object**,
> not read from upstream source. Where a claim was previously made from source
> reading, this file supersedes it.

GitOps Reverser is a **live → Git** tool. Before it may mirror a live object it has
to answer one question:

> **Does this object have a home in Git — one file that is its source?**

An object a controller *synthesised* has no home. Writing it to Git creates a second
source of truth that fights the controller that made it. So the answer decides
whether the object is mirrored at all, and the answer is read off the **provenance
evidence** the applying controller leaves behind.

This file records what that evidence actually is. It is not what the design docs
assumed.

## The table

| Producer | Evidence on the produced object | `ownerReference`? | `managedFields` manager | The producer's source | Home in Git? |
|---|---|---|---|---|---|
| Argo CD `ApplicationSet` → `Application` | `ownerReference` → the ApplicationSet (`controller: true`, `blockOwnerDeletion: true`) | **yes** | — | a generator | **no** for the `Application`; but it *points at* a real path, so its workloads do |
| Argo CD `Application` → workload | annotation `argocd.argoproj.io/tracking-id` = `<app>:<group>/<Kind>:<ns>/<name>` | no | `argocd-controller` | a folder in Git | **yes** |
| Flux `Kustomization` → workload | labels `kustomize.toolkit.fluxcd.io/{name,namespace}` | no | `kustomize-controller` | `GitRepository` + `spec.path` | **yes** |
| Flux `HelmRelease` → workload | labels `helm.toolkit.fluxcd.io/{name,namespace}`; annotations `meta.helm.sh/release-{name,namespace}`; labels `app.kubernetes.io/managed-by: Helm`, `helm.sh/chart` | no | `helm-controller` | a **chart** | **no** |
| flux-operator `ResourceSet` → anything | labels `resourceset.fluxcd.controlplane.io/{name,namespace}`; plus `status.inventory` on the parent | no | `flux-operator` | `spec.resources` — a template **inside the CR** | **no** |

## Three consequences, in order of how much they hurt

### 1. `ownerReference` is evidence for exactly one of the five

[`resource-capability-model.md`](../design/support-boundary/resource-capability-model.md)
and
[`sealed-secrets-and-external-secrets.md`](../design/support-boundary/sealed-secrets-and-external-secrets.md)
both define a `derived` object as *"Evidence: a controller `ownerReference` on the
live object."*

Measured, that test catches ApplicationSet children and **nothing else in the Flux
family**. Objects rendered by a `HelmRelease` and objects expanded by a
`ResourceSet` carry **zero** `ownerReferences`. A gate keyed on `ownerReference`
would mirror both — which is the exact failure the gate exists to prevent.

(ESO- and sealed-secrets-derived `Secret`s do carry an `ownerReference`, per the
secrets doc; they are not re-measured here.)

### 2. The label prefix is not the discriminator, and it looks exactly like one

```text
kustomize.toolkit.fluxcd.io/name   → the source is a folder of files → MIRROR IT
helm.toolkit.fluxcd.io/name        → the source is a chart           → NEVER MIRROR
```

Sibling prefixes. Same vendor. **Opposite verdicts.** Any rule of the form "strip or
gate on `*.toolkit.fluxcd.io/`" gets one of these two exactly backwards.

This is the empirical core of the argument that provenance is a **Tier 2 claim** —
a small table keyed by *which controller applied this, and what that controller's
source is* — and not a Tier 0 field check. It cannot be collapsed into one field,
because the field is the same in both rows and the answer is not.

### 3. `managedFields[].manager` is the only uniform channel

Every producer sets a distinct, stable applying manager:

| manager | verdict |
|---|---|
| `kustomize-controller` | has a home |
| `argocd-controller` | has a home |
| `helm-controller` | no home |
| `flux-operator` | no home |

Where the labels and owner-references speak three different vocabularies, the field
manager speaks one. It is the most promising single signal available, and no design
doc currently considers it.

**Caveat, and it is real:** `managedFields` accumulates. A Deployment applied by
helm-controller and later touched by another actor shows `helm-controller k3s`. The
manager is evidence about *who applied a field*, not a sole primary key — so it
belongs in the evidence table, not as a replacement for it.

## What leaks into Git today

[`internal/sanitize/types.go`](../../internal/sanitize/types.go) strips
`kustomize.toolkit.fluxcd.io/` (labels + annotations), `kro.run/` (labels only),
`applyset.kubernetes.io/`, and — by exact key — `argocd.argoproj.io/tracking-id`,
`argocd.argoproj.io/installation-id`, and `kcp.io/cluster`.

It does **not** strip, and these are all controller bookkeeping observed on real
objects:

| Key | Set by |
|---|---|
| `helm.toolkit.fluxcd.io/{name,namespace}` (labels) | helm-controller |
| `resourceset.fluxcd.controlplane.io/{name,namespace}` (labels) | flux-operator |
| `meta.helm.sh/release-{name,namespace}` (annotations) | Helm, via helm-controller |

Committed to Git, each is read back as user intent. This is the same family as the
Argo CD `tracking-id` hazard the strip list already guards against.

It is currently *moot* only because the provenance gate would refuse these objects
outright — but the gate is unbuilt, and if it lands keyed on `ownerReference` (see
consequence 1) the objects are mirrored **and** the labels leak.

`app.kubernetes.io/managed-by: Helm` and `helm.sh/chart` are deliberately **not**
listed as leaks: they are chart-authored labels that a user may legitimately want in
Git, and they are indistinguishable from the same labels set by hand. This is the
same trap as Argo CD's `app.kubernetes.io/instance` under `label` tracking — see the
note in `types.go`.

## The enumeration finding

An `ApplicationSet` with a `git.directories: apps/*` generator, over a real
repository:

```bash
$ mkdir apps/worker && git add . && git commit && git push    # NO Application CR authored
$ kubectl -n argocd get applications
NAME           PATH            SYNC     HEALTH
app-worker     apps/worker     Synced   Healthy       <-- appeared from the folder alone
```

**Creating a directory is a deployment operation.** The directory listing is a field
of desired state.

And the corollary — what happens if a tool "helpfully" authors the CR as well, which
is what *"add an app = add the manifests and author the CR"* would do:

```text
SharedResourceWarning: ConfigMap/worker-config is part of applications
                       argocd/app-worker and worker-authored-by-hand
```

The generated `Application` flipped to `OutOfSync`, and the workload's `tracking-id`
was **overwritten** to name the hand-authored app — arming the promotion landmine
documented in [`e2e-bi-directional-corner.md`](../spec/e2e-bi-directional-corner.md).

So in a generator-enumerated repository both naive moves are wrong: author the CR and
you get a duplicate that fights the generator; omit it and — in every *other* layout —
nothing deploys. This is why the `EnumeratedBy` claim exists, and why adding an app to
such a repository is
[refused, loudly](../design/support-boundary/expansion-boundary-and-corpus-organisation.md).

## Incidental, but worth knowing

An `ApplicationSet` template using `{{.cluster}}` **without** `spec.goTemplate: true`
does not fail loudly — it renders the literal string. Every element then produces the
same name and Argo reports:

```text
ApplicationSet guestbook contains applications with duplicate name: {{.cluster}}-guestbook
```

Two template engines (`fasttemplate` and Go `text/template` + sprig) behind one
field, selected by a boolean. A third dialect, Go `text/template` with `<< >>`
delimiters, is what flux-operator's `ResourceSet` uses. Any static reading of a
template must know which dialect it is in.

## Reproducing this

The measurements are pinned as a runnable spec in the bi-directional e2e corner —
the only place Argo CD is installed:

```bash
task test-e2e-bi-directional
```

See [`e2e-bi-directional-corner.md`](../spec/e2e-bi-directional-corner.md) for the
corner, and
[`test/fixtures/gitops-layouts/3-expanded/`](../../test/fixtures/gitops-layouts/3-expanded/)
for the layouts these producers correspond to.
