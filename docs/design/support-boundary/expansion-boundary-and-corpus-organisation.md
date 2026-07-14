# The expansion boundary, and how to organise this material

> Status: **partly executed.** The corpus reorganisation and the generated baseline
> shipped (2026-07-13); the provenance gate remains unbuilt. The catalogue below is
> no longer inferred — every marker in it has now been **measured against live
> controllers**, and one of its central assumptions did not survive.
> Captured: 2026-07-11 · Measured: 2026-07-13
> Related:
> [README.md](README.md),
> [support-contract.md](support-contract.md),
> [../../facts/expansion-provenance-markers.md](../../facts/expansion-provenance-markers.md) — **the measurements**,
> [orchestrator-knowledge-boundary.md](orchestrator-knowledge-boundary.md),
> [resource-capability-model.md](resource-capability-model.md),
> [kustomize-support-boundary.md](kustomize-support-boundary.md),
> [sealed-secrets-and-external-secrets.md](sealed-secrets-and-external-secrets.md),
> the layout corpus at [`test/fixtures/gitops-layouts/`](../../../test/fixtures/gitops-layouts/)

## What the measurements changed

This doc was written from upstream source reading. It has since been checked against
real Flux, flux-operator, and Argo CD, and the results are pinned as a runnable e2e
spec ([`test/e2e/provenance_markers_e2e_test.go`](../../../test/e2e/provenance_markers_e2e_test.go),
`task test-e2e-bi-directional`). Three things moved:

1. **The structural claim held, and is now proven** — ApplicationSet generates
   *pointers into Git*, ResourceSet generates *the KRM itself*.
2. **The `ownerReference` gate is worse than "incomplete" — it catches one producer
   in five.** Neither HelmRelease-rendered objects nor ResourceSet children carry an
   `ownerReference` *at all*. Defect 1 below is stronger than it was written.
3. **`EnumeratedBy` is proven.** `mkdir` + commit, with no CR authored, deployed an
   app; authoring the CR as well produced exactly the predicted duplicate fight.

## What this proposes

Three things, in order of importance:

1. **A missing axis.** The model has two axes — *renderability* (can an edit be
   written back to one document?) and *ownership* (does that document already have
   another writer?). Both are questions about **files in Git**. Neither asks the
   question that ApplicationSets, ResourceSets, KRO, Crossplane, and HelmReleases
   all raise: **does this live object have a home in Git at all?**
2. **A support contract that fits on one page** — a catalogue of the constructs
   people actually put in GitOps repos, each with a verdict and a reason. Today the
   boundary is asserted in 17 places across two doc trees, in three unrelated
   refusal vocabularies, and nowhere in full.
3. **A topical spine for the corpus and the docs**, so new material has an obvious
   home instead of a new number.

The corpus's fixtures raise 95 open questions. They are not 95 decisions. They
compress to five, and the fifth is the one we have never written down.

## The finding: expansion is a third axis

Every GitOps repository is a **starting point that explodes into more KRM**. That
explosion happens in one of two fundamentally different ways, and the difference
decides everything:

| | Expansion mechanism | Does the resulting object exist in Git as a file? |
|---|---|---|
| **Render-time** | `kustomize build`, `helm template`, a CI script | **Yes** (or it is a deterministic function of files that do) |
| **Controller-side** | ApplicationSet, ResourceSet, KRO, Crossplane, HelmRelease, ESO | **No.** The object is synthesised in-cluster from a template plus inputs |

Renderability covers the first row. It has nothing to say about the second.

This matters because GitOps Reverser is a **live → Git** tool. It watches live
objects and writes them to files. A controller-synthesised object is a live object
like any other — it will be watched, it will be sanitized, and it will be written
to a file that has never existed and that nothing in Git will ever read again. That
is not a lossy edit. It is the creation of a **second source of truth that fights
the controller that made it**.

So the third axis is **provenance**, and it is the mirror image of ownership:

| Axis | Direction | Question | Modelled today |
|---|---|---|---|
| Renderability | Git → live | Can an edit be written back to exactly one document? | ✅ the support boundary |
| Ownership | Git side | Does that document already have another writer? | ⚠️ designed, unbuilt |
| **Provenance** | **live → Git** | **Did a controller synthesise this object, or did a human author it?** | ❌ **not modelled** |

Ownership and provenance are the same concern seen from the two sides of the
Git ↔ cluster boundary: *who else writes this file* and *who else creates this
object*. Naming them as one pair is what makes the third axis obvious rather than
ad hoc.

## The support contract, in one sentence

> **GitOps Reverser edits the intent layer. It never edits the expansion layer.**

Every repository has an **intent layer** — the documents a human authored and
committed — and an **expansion layer** — the objects a controller derived from
them. We mirror and edit the intent layer, because that is the layer with a home in
Git and the layer a careful human would edit anyway. We refuse the expansion layer,
because expansion is a one-way function and its output has nowhere to go.

This is a *defensible* boundary, not a gap, and for the modern tools it is a
**good** answer rather than a grudging one: a ResourceSet or a HelmRelease puts the
whole of a user's intent in one editable document, which is precisely our sweet
spot. What we decline to do is reach past that document into the objects it
produces — which is also what the tools themselves tell users not to do.

## The expansion catalogue

Originally verified against the upstream checkouts in `external-sources/`. Since
2026-07-13 the first three rows are **measured on live objects** — see
[`expansion-provenance-markers.md`](../../facts/expansion-provenance-markers.md).

### The structural asymmetry that decides everything

> **Argo CD's ApplicationSet generates *pointers into Git*. Flux's ResourceSet
> generates *the KRM itself*.**

An `ApplicationSet` template is a typed stencil of exactly one `Application`
(`ApplicationSetTemplate` = metadata + `ApplicationSpec`). Each generated
`Application` carries `spec.source.{repoURL,path,targetRevision}` — so the
Deployments and Services beneath it are **real files at a real path**. The
generated `Application` CRs are a thin control-plane layer that exists only in etcd;
the fat layer of workload KRM stays in Git and round-trips normally.

A `ResourceSet` has no such layer. Its `spec.resources` is
`[]*apiextensionsv1.JSON` — **arbitrary KRM embedded in the CR itself** — rendered
once per input set through a Go `text/template` with `<< >>` delimiters. There is
no `path`, no `sourceRef`, no `resourcesFrom`. Flux's own `Kustomization` has
`spec.path` + `spec.sourceRef`; `ResourceSet` deliberately does not. The flux-operator
docs are candid about the consequence, describing ResourceSet image automation as
suitable for **"Gitless GitOps"** workflows where "instead of pushing changes to a
Git repository, the updates are applied directly to the cluster."

The consequence for this operator: **for an ApplicationSet repo we can round-trip
the workloads; for a ResourceSet repo there are no workload files to round-trip.**
The ResourceSet CR is the only artifact.

### The generator family, side by side

| | Argo CD `ApplicationSet` | flux-operator `ResourceSet` |
|---|---|---|
| Group/version | `argoproj.io/v1alpha1` | `fluxcd.controlplane.io/v1` |
| What it templates | one `Application` (typed stencil) | arbitrary KRM (`spec.resources`, `spec.resourcesTemplate`) |
| Template engine | `fasttemplate` `{{ }}`, or Go template + sprig when `goTemplate: true` | Go `text/template`, `<< >>` delimiters, sprig |
| Inputs from | 9 generators: `list`, `clusters`, `git` (`directories`/`files`), `scmProvider`, `pullRequest`, `matrix`, `merge`, `clusterDecisionResource`, `plugin` | `spec.inputs` (inline) + `spec.inputsFrom` → `ResourceSetInputProvider` |
| Input providers | — | 22 types: `Static`, `GitHub*`, `GitLab*`, `AzureDevOps*`, `AWSCodeCommit*`, `Gitea*`, `OCIArtifactTag`, `ACR/ECR/GAR`, `ExternalService` |
| Cartesian fan-out | `matrix` generator | `inputStrategy: Permute` |
| Second template layer | `templatePatch` (rendered, then merged onto the Application) | — |
| Generated objects tracked by | **`ownerReference`** (`controllerutil.SetControllerReference`) | **owner labels** `resourceset.fluxcd.controlplane.io/{name,namespace}` + `status.inventory` — **no ownerReference** |
| Writes to Git? | No | No |
| Workload KRM in Git as files? | **Yes** — behind the generated `Application` | **No** — inline in the CR |

Two facts from that table are load-bearing and are addressed below: the different
ownership evidence, and the fact that a `GitHubPullRequest` or `OCIArtifactTag`
input provider means **the input set is not in the repository at all** — it is
queried live from a Git-host or registry API. The desired object set depends on
which pull requests are open right now. No repository scan can see it, ever.

### The whole family, generalised

Every one of these is the same shape — an authored document, a controller, and
objects that exist only in the cluster:

| Authored document (intent) | Controller | Expansion (live only) | Ownership evidence on the child |
|---|---|---|---|
| Argo CD `ApplicationSet` | applicationset-controller | `Application` CRs | `ownerReference` — **measured** |
| flux-operator `ResourceSet` | flux-operator | `spec.resources` × inputs | `resourceset.fluxcd.controlplane.io/{name,namespace}` labels + `status.inventory`; **no `ownerReference`** — **measured** |
| Flux `HelmRelease` | helm-controller | the chart's rendered objects | `helm.toolkit.fluxcd.io/{name,namespace}` labels + `meta.helm.sh/release-*` annotations; **no `ownerReference`** — **measured** |
| KRO `ResourceGraphDefinition` + instance | kro | the graph's objects | `kro.run/*` labels (inferred from our own `sanitize` strip list — still unmeasured) |
| Crossplane `Composition` + XR | crossplane | composed resources | `ownerReference` (still unmeasured) |
| `ExternalSecret` | external-secrets | a `Secret` | `ownerReference` (per `sealed-secrets-and-external-secrets.md`) |
| `SealedSecret` | sealed-secrets | a `Secret` | `ownerReference` (per `sealed-secrets-and-external-secrets.md`) |

Consistent rule, at least three different kinds of evidence. Which is exactly the
problem — and the measurements made it worse than this table originally admitted.

The top three rows are now **observed on live objects**, not read from source; the
full evidence, including the field-manager signal this doc never considered, is in
[`expansion-provenance-markers.md`](../../facts/expansion-provenance-markers.md).
KRO and Crossplane remain unmeasured and are the obvious next lab.

The structural claim ("no home file, therefore never mirror it") never depended on
which marker it turned out to be. The *implementation* of the gate depends on it
entirely — and it turns out the markers are three incompatible vocabularies with one
trap in the middle of them (defect 1).

## Three defects this exposes

### 1. The derived-object gate keys on evidence that four of these five controllers do not emit

[resource-capability-model.md](resource-capability-model.md) knob 4 and
[sealed-secrets-and-external-secrets.md](sealed-secrets-and-external-secrets.md)
both define `derived` as *"Evidence: a controller `ownerReference` on the live
object."*

**Measured against live controllers, that test catches one producer in five.**

| Producer | Sets an `ownerReference`? |
|---|---|
| Argo CD `ApplicationSet` → `Application` | **yes** |
| Argo CD `Application` → workload | no |
| Flux `Kustomization` → workload | no |
| Flux `HelmRelease` → workload | **no** — and this one must be refused |
| flux-operator `ResourceSet` → anything | **no** — and this one must be refused |

The two rows in bold are the failure. Both are objects with **no home in Git**, both
are objects the gate exists to refuse, and **neither carries a single
`ownerReference`**. A gate keyed on one would mirror both — which is precisely the
outcome it was designed to prevent. (ESO- and sealed-secrets-derived `Secret`s *do*
carry one, per the secrets doc; they are not the problem.)

An `ownerReference` is neither necessary nor sufficient. And a label alone is not
sufficient either, which is the subtle half:

> A `Deployment` applied by a Flux `Kustomization` also carries a controller's
> label (`kustomize.toolkit.fluxcd.io/name`). It is **not** derived — it has a
> home file, and mirroring it is exactly right.

The measurements sharpen that into a trap with a name:

```
kustomize.toolkit.fluxcd.io/name   → source is a folder of files → MIRROR IT
helm.toolkit.fluxcd.io/name        → source is a chart           → NEVER MIRROR
```

**Sibling prefixes. Same vendor. Opposite verdicts.** Any rule of the form "gate on
`*.toolkit.fluxcd.io/`" gets exactly one of these two backwards, and it is a coin
flip which. This is the empirical core of the argument that provenance cannot be
collapsed into a field check: *the field is the same in both rows, and the answer is
not.*

So the real test is not "did a controller touch this object?" but:

> **Does this object have a home in Git?** — and the answer depends on *which*
> controller applied it and *what its source is*: a folder of files (home exists —
> mirror it) or a template inside a CR (no home — never mirror it).

That is a **Tier 2 claim**, not a Tier 0 field check. It belongs in the interpreter
registry the [boundary doc](orchestrator-knowledge-boundary.md) already proposes,
as a small table keyed by provenance marker:

| Provenance marker on the live object | Applying manager | What the controller's source is | Verdict |
|---|---|---|---|
| `kustomize.toolkit.fluxcd.io/name` | `kustomize-controller` | a folder in Git | has a home → **mirror it** |
| `argocd.argoproj.io/tracking-id` | `argocd-controller` | a folder in Git | has a home → **mirror it** |
| `resourceset.fluxcd.controlplane.io/name` | `flux-operator` | a CR's `spec.resources` | no home → **never mirror** |
| `helm.toolkit.fluxcd.io/name` | `helm-controller` | a chart | no home → **never mirror** |
| `kro.run/*` | (unmeasured) | a `ResourceGraphDefinition` | no home → **never mirror** |
| controller `ownerReference` | — | another object | no home → **never mirror** |

Note the first two rows: these objects are *applied by a controller* and must still
be mirrored, because the controller's source is a folder of files. That is why
"was a controller involved?" is the wrong question and "does it have a home in
Git?" is the right one — and why this table cannot be replaced by a single field
check.

The **applying manager** column is new, and it is the one genuinely encouraging
result of the measurements: `metadata.managedFields[].manager` is the only channel
that speaks *one* vocabulary across all five producers, where the labels and
owner-references speak three. It is not a sole primary key — managers accumulate, so
it is evidence about who applied a field rather than a unique owner — but it is the
strongest single signal available and no design here had considered it.

### 2. We destroy the provenance evidence before anything gates on it

This is now a pattern with three instances, and it is the same bug each time:

- [`sanitize.go`](../../../internal/sanitize/sanitize.go) **deletes**
  `ownerReferences` before a document reaches Git — already noted in the secrets doc.
- [`sanitize/types.go`](../../../internal/sanitize/types.go) `isOperationalLabel`
  **strips `kro.run/`** — the only marker that an object was expanded by KRO.
- `isOperationalAnnotation` strips `kustomize.toolkit.fluxcd.io/` and keeps Argo's
  `tracking-id` on an exact-key deny-list.

In each case the evidence that would answer *"should this object be here at all?"*
is thrown away by the code whose job is to decide what a document looks like once
we have already decided to write it. **The gate must run before the sanitizer, on
the live object, not on the sanitized document.**

### 3. Three families of controller bookkeeping would leak into Git today

`isOperationalLabel` strips `kustomize.toolkit.fluxcd.io/`, `kro.run/`, and
`applyset.kubernetes.io/`. Measured against live objects, it does **not** strip any
of these — and all three were observed on real workloads:

| Key | Stamped by | Observed on |
|---|---|---|
| `helm.toolkit.fluxcd.io/{name,namespace}` (labels) | helm-controller | every chart-rendered object |
| `resourceset.fluxcd.controlplane.io/{name,namespace}` (labels) | flux-operator | every ResourceSet child |
| `meta.helm.sh/release-{name,namespace}` (annotations) | Helm, via helm-controller | every chart-rendered object |

Our code contains **zero** occurrences of `ResourceSet`, `ApplicationSet`, or
`fluxcd.controlplane.io` — we know nothing about these kinds.

This is the same family as the Argo CD `tracking-id` hazard already documented in
`types.go`: controller bookkeeping committed to Git, where it is read back as intent.
It is currently moot only because the gate above would refuse these objects
outright — but the gate is unbuilt, and if it lands keyed on `ownerReference`
(defect 1) the objects are mirrored **and** the labels leak. The two defects
compound.

Deliberately *not* on that list: `app.kubernetes.io/managed-by: Helm` and
`helm.sh/chart`. Both are chart-authored labels a user may legitimately want in Git,
and both are indistinguishable from the same labels set by hand — the same trap as
Argo CD's `app.kubernetes.io/instance` under `label` tracking. Stripping them would
delete intent.

## The construct catalogue — the support statement

This is the artifact I think the branch is missing: one table a user, a maintainer,
or an onboarding report can be checked against. Five verdicts, deliberately few:

| Verdict | Meaning |
|---|---|
| **Editable** | We mirror it and write edits back to its file. |
| **Read-only context** | We read it to understand the folder; we never write it. |
| **Not mirrored** | It exists live, but a controller synthesised it and Git has no home for it. |
| **Refused** | The folder cannot be written safely; we say why, and fail the target. |
| **Write-only** | We can describe and replace it, but never read it (SOPS). |

### Delivery and orchestration

| Construct | Layer | Verdict | Why |
|---|---|---|---|
| Plain KRM folder | intent | **Editable** | files *are* the objects — the first-class target |
| Argo CD `Application` (hand-written) | intent | **Editable** | ordinary KRM — shipped |
| Argo CD `Application` generated by an ApplicationSet | expansion | **Not mirrored** | `ownerReference`; no home file |
| Argo CD `ApplicationSet` (the CR) | intent | **Editable as a document** | but its `template` is a fan-out surface — see below |
| App-of-apps root `Application` | intent | Editable, flagged **cluster entry point** | not an onboarding answer |
| Flux `Kustomization`, `GitRepository`, `OCIRepository`, `HelmRepository` | intent | **Editable** | ordinary KRM |
| Flux `HelmRelease` | intent | **Editable** | ordinary KRM — shipped |
| Objects the helm-controller renders from it | expansion | **Not mirrored** | no home file |
| flux-operator `ResourceSet` (the CR) | intent | **Editable as a document** | `spec.inputs` is the supported edit surface |
| Objects a `ResourceSet` expands | expansion | **Not mirrored** | `spec.resources` × inputs; no home file |
| flux-operator `ResourceSetInputProvider` | intent | **Editable** | but `status.exportedInputs` is live-only and unwritable |
| flux-operator `FluxInstance` | intent | Editable, flagged **cluster entry point** | manages Flux itself |
| KRO `ResourceGraphDefinition` / Crossplane `Composition` + claims | intent | **Editable** | ordinary KRM — KRO is shipped and pinned by the corpus |
| Objects they expand | expansion | **Not mirrored** | no home file |

### Rendering

| Construct | Verdict | Why |
|---|---|---|
| kustomize: `resources`, `namespace`, `images`, `replicas` | **Editable** | invertible; `images`/`replicas` edit-through is shipped |
| kustomize base + un-fancy overlays | **Editable — planned** | not supported today; the base is read-only context, and render-root scoping is unbuilt |
| kustomize base shared by >1 overlay, edited in place | **Refused** | write-fan-in > 1 (L2) |
| kustomize `patches*`, generators, `components`, `namePrefix`/`nameSuffix`, remote bases | **Refused** | non-invertible; restricted patch authoring is planned, the rest are permanently refused |
| kustomize `helmCharts:` (inflation) | **Refused** | we never render a chart |
| Helm chart (the whole folder: `Chart.yaml` + `templates/` + `values.yaml` + `crds/`) | **Skipped as a unit** | decision 4 — detect the chart by its folder structure and skip it whole; report it as a `helm-chart` layout, not as silence |
| Flux `HelmRelease` / Argo `Application` Helm knobs (chart version, inline values, parameters) | **Editable** | the Helm surface people actually use — see the Helm section below |
| Free-standing Helm values file (`values/production.yaml`) | **Refused today** | not KRM; a `ValuesFile` projection is proposed below |
| CI-rendered `rendered/` output | **Refused** | the next render destroys the edit |

### Machine-written Git and secrets

| Construct | Verdict | Why |
|---|---|---|
| Path written by Flux `ImageUpdateAutomation` | **Refused** | a bot commits here; `$imagepolicy` comments are load-bearing |
| `.argocd-source*.yaml` (Argo CD Image Updater) | **Refused / read-only** | written by a bot, and outranks the config a reader would inspect |
| SOPS `Secret` | **Write-only** | not schema-conformant; the `mac` binds the whole document |
| `SealedSecret` | **Editable** | ordinary KRM; `writeUnit: key` |
| `ExternalSecret` | **Editable** | ordinary KRM; the value simply lives elsewhere |
| `Secret` derived by ESO / sealed-secrets | expansion → **Not mirrored** | `ownerReference` |

## Why we must understand generators without running them

The central use case is *"add something to the test environment."* Generators change
**the recipe for adding a thing**, and getting the recipe wrong is not a
degradation — it produces a duplicate object that fights the controller.

There are three recipes, and nothing inside a workload folder tells you which one
applies:

| Repo shape | To add an app you must… | Getting it wrong produces |
|---|---|---|
| App-of-apps, Flux `Kustomization`-per-app | add the manifests **and** author the `Application`/`Kustomization` CR | nothing deploys |
| Kustomize folder | add the manifests **and** the `resources:` entry | nothing deploys (new-file placement handles this today) |
| ApplicationSet `git.directories`, ResourceSetInputProvider | add the manifests / an input **and author no CR** — the generator picks it up | a **duplicate** `Application` that fights the generator |

The third row is why the corpus keeps asking *"is creating a folder itself a
deployment operation?"* (the two ApplicationSet fixtures). It is. The directory listing is a field
of desired state.

This needs exactly **one new claim** in the vocabulary the
[boundary doc](orchestrator-knowledge-boundary.md) already defines:

| Claim | Meaning | Emitted from |
|---|---|---|
| `EnumeratedBy{glob, by}` | Any folder/file matching this glob becomes a deployed unit automatically. Do not author a CR for it. | `ApplicationSet.spec.generators.git.directories[].path` / `.files[].path`; a `ResourceSetInputProvider` |

Reading it needs a group, a kind, and one field path. It never needs Argo's or
Flux's code, and it never evaluates the generator — consistent with the standing
non-goal.

**Per decision 3 below, we do not currently add an app to such a repository — we
refuse, explicitly.** The claim exists so we can *recognise* the shape and refuse
it precisely, rather than guess and produce a duplicate.

### And the fan-out rule already covers templates

The existing L2 write boundary says: *no in-place edit of a source file that more
than one render path reaches.* **Write-fan-in must be 1.** That rule, unchanged,
already answers the template question — the inputs simply play the role of the
render paths:

| Surface | Fan-in | Verdict |
|---|---|---|
| Kustomize base reached by 2 overlays | 2 | refused (today) |
| `ResourceSet.spec.resources[i]` with 1 input | 1 | **editable** |
| `ResourceSet.spec.resources[i]` with N inputs | N | **refused** — the edit would change every instance |
| `ApplicationSet.spec.template` with N generator results | N | **refused** |
| `ResourceSet.spec.inputs[j].version` | 1 | **editable** — this is the right edit surface |

That is a satisfying result: the boundary we already enforce for kustomize bases is
the *same* boundary, and it lands in the right place for ResourceSets. "Bump one
input's version" is an edit to `spec.inputs[1]`, which has fan-in 1. "Change the
template" does not, and never will.

The honest caveat: a fan-in-1 edit into `spec.resources[i]` is an edit to a KRM
document **nested inside another KRM document**, and the writer has no concept of
that today. The `mixed-and-hostile` fixture's "KRM never nests KRM" assumption-breaker is exactly this,
and ResourceSet/KRO/Crossplane make it mainstream rather than hostile.

## The strategic note worth recording

The flux-operator guides present ResourceSet as a **replacement for base + overlays** —
their own example replaces an `apps/app1/{base,overlays/tenant1,overlays/tenant2}`
tree with "a single file." That tree is exactly the layout **render-root scoping
exists to support**.

So: to the extent ResourceSets succeed, the base+overlay repositories we are
building render-root scoping for get replaced by repositories whose workload KRM is
not in Git as files at all. That does not make render-root scoping wrong — the
base+overlay corpus is enormous and will be for years — but it does mean the
ResourceSet answer ("edit the CR, edit its inputs, never its children") is a story
we need **now** rather than eventually, and it is a story we can tell well and
cheaply, because the CR is one editable document.

## The organisation — done (2026-07-13)

### The corpus: grouped by axis, not by number

Shipped. The fixture numbers `01`–`16` carried no meaning and related shapes sat far
apart. The corpus drives no test and nothing depended on the paths, so the regroup
was cheap — and is now done, before the first golden report could pin a path.

The tree is in [the corpus README](../../../test/fixtures/gitops-layouts/README.md).
Six families, one per decision the corpus forces, and `ls` now shows the model
instead of hiding it.

**One correction to the proposal, and the axis is what caught it.** This doc
originally filed `argocd-app-of-apps` (02) and `flux-monorepo` (09) under
`3-expanded`. That is wrong, and it is exactly the mistake the provenance axis exists
to prevent:

- an app-of-apps' child `Application`s are **files a human wrote and committed**;
- a Flux `Kustomization` points at a **folder of real files**, and the measurements
  confirm its applied objects carry `kustomize.toolkit.fluxcd.io/*` — the marker that
  means *has a home → mirror it*.

Nothing in either is synthesised. Both involve a controller; neither is expansion.
They live in `1-desired-state`, and `3-expanded` is reserved for what the name
says: **a controller materialises objects that have no home file.** The authored CR
in those fixtures is itself ordinary editable intent; it is its *output* that has
nowhere to go.

Two of the proposed new fixtures shipped —
[`flux-resourceset-inline`](../../../test/fixtures/gitops-layouts/3-expanded/flux-resourceset-inline/)
(nine live objects, zero files) and
[`flux-resourceset-pull-requests`](../../../test/fixtures/gitops-layouts/3-expanded/flux-resourceset-pull-requests/)
(the input set is not in the repository at all). `kro-and-crossplane`,
`argocd-image-updater`, `sealed-secrets`, and `external-secrets` remain wanted, and
are now named as gaps in the corpus README rather than left as oversights.

### The baseline is generated now

`support-today.md` is generated by `task gitops-layouts-baseline`, not pasted. It
carries no interpretation: when it disagrees with the support contract, that
disagreement is the backlog.

It earned its keep immediately. Both new ResourceSet fixtures report **"All reported
candidates accepted"** — the analyzer sees an ordinary editable plain folder and says
so. It would onboard, as fully supported, a repository whose nine workloads have no
files, and one whose object set cannot be determined without a GitHub token. The gap
was invisible while ResourceSet was absent from the corpus; it is now a line in a
checked-in baseline.

### The docs: one new page, one new topic, and stop restating

The design tree does not need reshuffling so much as it needs a **front door** and
one **missing chapter**:

| Action | Doc | Rationale |
|---|---|---|
| **Add** | `support-contract.md` | The construct catalogue above, and nothing else. The one page that answers "what do you support?" Today that answer is asserted in 17 places, in 3 unrelated refusal vocabularies, and assembled nowhere. |
| **Add** | this doc, or a successor | The expansion axis, the generator family, `EnumeratedBy`. Currently ApplicationSet appears only as a non-goal, and ResourceSet appears nowhere at all. |
| **Consolidate** | the write-fan-in invariant | Stated four times (`kustomize-support` §4, `gittarget-granularity` §1, `README`, `unreflectable-edits`). `gittarget-granularity` §7 already asks whether to fold these; the answer is yes — keep §1, have the rest cite it. |
| **Amend** | `resource-capability-model.md` knob 4, `sealed-secrets-and-external-secrets.md` "The gate" | `derived` cannot key on `ownerReference` alone. It is a Tier 2 provenance claim, not a Tier 0 field check. |
| **Restructure** | `README.md` "Related:" | Group by topic (boundary / orchestrator / secrets / topology) instead of a flat list of nine links. |

Nothing gets deleted, and no doc gets rewritten. The consolidation is a
cross-reference edit, not a merge.

## Decisions (2026-07-11)

The five questions this doc opened have been answered. Recorded here; the ones that
change another doc are flagged.

### 1. The operator must hold in **both** kinds of watched cluster

The cluster the operator watches may be a **lightweight, workload-less cluster**
whose only job is editing, or **an actual production cluster**. Both are supported,
so the design must hold in both. That resolves the gate question decisively:

| Watched cluster | Is flux-operator / ApplicationSet running? | Consequence |
|---|---|---|
| Lightweight editing cluster | **No** — CRDs only, per the standing rule ("install a controller only if it is an applier you drive"). A `ResourceSet` expander is not an applier we drive. | Children never materialise. The `ResourceSet` CR sits there inert and editable. Nothing to gate. |
| Production cluster | **Yes** | Children materialise as live objects. **The provenance gate is mandatory** — without it we mirror them and create a second source of truth. |

So the gate is **not optional**: the production-cluster case requires it. And where
no expansion controller runs, the CR is cleanly editable with no expansion layer at
all, which is the better editing experience and worth saying out loud.

### 2. Fan-in = 1 stays the ground rule

**We do not change more than one thing.** The write-fan-in invariant holds as
stated, and it is what makes `spec.inputs[j]` writable and a multi-input template
not. Noted for the record that this may not hold forever — a future feature may
want a deliberate, declared fan-out edit — but nothing is designed against that
today, and the rule stays.

### 3. Adding an app to a generator-enumerated repo is **disallowed**, loudly

When `EnumeratedBy` holds — an `ApplicationSet` `git.directories` glob, a
`ResourceSetInputProvider` — creating a new app is a **special case we refuse for
now**, explicitly. Not silently skipped: an explicit refusal with a reason.

This is the right call because the failure mode of guessing is the worst one
available: author the CR and you get a duplicate `Application` fighting the
generator; omit it and nothing deploys. A refusal that says *"this repo enumerates
apps from a glob; adding one is not something I can do safely — do it in Git"* is
honest and costs the user one PR.

The `EnumeratedBy` claim is still worth emitting, because it is what lets us
*recognise* the case in order to refuse it precisely rather than fail confusingly.

### 4. Helm charts are skipped as a unit — including `crds/`

A Helm chart is **recognisable by its folder structure** (`Chart.yaml` +
`templates/`), so detect the chart and skip the whole thing, `crds/` included.

This fixes the report bug directly: `06-helm-chart` today reports exactly one
accepted candidate — `charts/frontend/crds` — which makes the onboarding report say
"1 folder supported" about a repository we cannot meaningfully support. A chart
should surface as an explicit `helm-chart` layout that reports honestly, not as
silence plus an incidental `crds/`.

### 5. `Permute` / `matrix` fan-out: refused, and noted

Fan-in is N > 1, so the verdict follows from decision 2 with nothing new to decide.
Noted because users *will* want per-cell edits on a sparse input matrix (fixture
12), and that want is real — it is simply not something we can serve inside the
fan-in-1 rule. If the rule ever relaxes, this is the first case to revisit.

## Helm: name the surface we already support, do not invent a standard

The instinct that *"we cannot credibly tell the Helm world we do nothing with
Helm"* is right, and the good news is that **we already do more than the messaging
admits**. The proposed fix is not a new "helm-lite" standard — you cannot make the
field adopt a subset you invent, and a standard nobody follows is worse than none.
The fix is to **name the Helm surface that already works**.

Here is what people actually do with Helm in a GitOps repo, and where each lands
today:

| What a user wants to do | Where it lives | Verdict |
|---|---|---|
| Bump a chart version | `HelmRelease.spec.chart.spec.version`; Argo `Application.spec.source.targetRevision` | ✅ **works today** — ordinary KRM edit |
| Change a value | `HelmRelease.spec.values` (inline) | ✅ **works today** |
| Change a value | `Application.spec.source.helm.parameters` | ✅ **works today** (structured) |
| Change a value | `Application.spec.source.helm.values` | ⚠️ works, but the field is an embedded YAML **string** — a blob, not a tree |
| Change a value held in a `ConfigMap` (`valuesFrom`) | a real `ConfigMap` | ✅ **works today** — it is KRM like anything else |
| Install a chart / add an app | add a `HelmRelease` + `HelmRepository`/`OCIRepository` | ✅ **works today** (new-file placement) |
| Change a value in a free-standing values file | `values/production.yaml` | ❌ **the gap** — not KRM, so no object exists to edit |
| Edit the chart's `templates/` | chart source | ❌ refused, permanently — and correctly |
| Edit a rendered object | live only | ❌ not mirrored (expansion layer) |

Read that table and the honest headline is not "no Helm." It is:

> **We support Helm the way GitOps repositories actually use it — as a declaration
> you edit, not a chart you render.** The most common Helm operation in a GitOps
> repo is "bump the chart version" or "change a value on a `HelmRelease`", and both
> work today.

### The one real gap, and the precedent that closes it

The genuine hole is the **free-standing values file** — `values/production.yaml`,
`values/common.yaml` — which `helm-environment-values`, `argocd-external-helm`, and `argocd-multicluster-matrix` are all built around. It is
not KRM, so no object for it ever appears in the cluster, so there is nothing for a
user to edit.

There is an exact precedent for this in the branch already:
[`EncryptedSecret`](write-only-encrypted-secrets.md) projects a document we cannot
store as itself into a kind we *can*, and its punchline is *"a refusal becomes a
capability."* The same move works here — project a values file as a synthetic
`ValuesFile` (or `HelmValues`) object, let the user edit it, write it straight back
to the file.

And the invertibility argument is unusually clean, which is why this is worth
proposing rather than just noting:

- The file is **plain YAML we own end to end**. No renderer stands between it and
  Git — we are not inflating a chart, we are editing a file whose bytes are the
  desired content.
- **Fan-in is 1** whenever exactly one `HelmRelease`/`Application` references it,
  which the analyzer can already determine from `valuesFrom` / `valueFiles`. Shared
  values files (`common.yaml`) have fan-in > 1 and are refused by the existing rule
  with no new machinery.
- It is **not chart inflation** — the boundary that must never move. We still never
  render `templates/`, and we still never learn what a value *means*.

This deserves a design of its own rather than a decision here, and it is the single
highest-leverage thing available for the Helm story: it turns the biggest "we do
nothing with Helm" objection into a supported edit, using a pattern already
designed for another problem.

### What a "helm-lite" recommendation *can* be

Not a standard — a **migration recommendation in the onboarding report**. The
onboarding scan can honestly say: *"this repository keeps its values in
free-standing files; these three are shared and cannot be edited; these four are
single-use and can."* That is a report, not a standard, and it is achievable.

## Residual open questions

1. ~~**Argo CD's triggered-hydration handshake is still unproven in e2e.**~~
   **Closed.** The `selfHeal: false` + push-webhook loop, with a shared field changing
   from *both* sides, is exercised against a real Argo CD in the bi-directional corner
   ([`argocd_bi_directional_e2e_test.go`](../../../test/e2e/argocd_bi_directional_e2e_test.go)),
   alongside its `selfHeal: true` foil where the edit is lost.
2. **What exactly makes a chart a chart?** Decision 4 says "recognisable folder
   structure." `Chart.yaml` + `templates/` is the obvious test, but `mixed-and-hostile`
   plants a `templates/` directory with **no** `Chart.yaml` and unparseable content
   precisely to break it. Which signal is load-bearing, and what happens to a chart
   vendored under `charts/` inside another chart?
3. **Does the provenance gate run on the live object or the sanitized document?**
   Still the one decision that blocks implementation, and the measurements make it
   sharper: `sanitize` destroys `ownerReferences` and `kro.run/` labels, and the gate's
   *best* signal — the applying field manager — lives in `metadata.managedFields`,
   which is stripped even earlier and is not in the sanitized document at all. **The
   gate must run on the live object, before the sanitizer**, and nothing in the
   pipeline is structured that way today.
4. **When does the gate have to exist?** New, and it follows from the topology: in a
   watched cluster with no expansion controllers installed, nothing derives anything,
   and the question is answered by the deployment rather than by code. The gate is
   mandatory only for the *production-cluster* case — which decision 1 lists as
   supported, so it must be built; what remains open is how much of it a
   controller-free watched cluster is entitled to skip.
5. **KRO and Crossplane are still unmeasured.** Their markers are inferred (from our own
   strip list, in KRO's case). Given that three of the five measured producers
   contradicted what was assumed about them, inference is not good enough here. They are
   the obvious next lab, and the corpus has no fixture for either.
