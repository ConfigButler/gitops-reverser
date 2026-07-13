# GitOps layout fixtures

Real-world GitOps repository layouts, checked in so we can design and reason
against concrete examples instead of prose.

> **These fixtures record no verdicts.** Nothing here says which layouts GitOps
> Reverser supports, accepts, or refuses — that is the job of
> [`docs/design/gitops-api/support-contract.md`](../../../docs/design/gitops-api/support-contract.md).
> Each fixture describes *what a layout is*, *what questions it raises*, and — where
> we have run it — *what the real controller actually did*. A fixture may record an
> **observation**; it never records a support decision.

## Why this exists

The compatibility question "does GitOps Reverser support Argo CD / Flux
repositories?" is the wrong question, because Argo CD and Flux are mostly
orchestration *around* folders. The real boundary is decided by **what a folder
contains, how it is rendered, and whether the objects it produces have anywhere to
go back to** — not by which controller points at it.

To find that boundary honestly we need the actual shapes users have. This corpus
is that set of shapes.

## The six families

The directories are grouped by **the decision each layout forces**, not by a
number. One family per decision:

| Family | The question it forces | The thing that decides it |
|---|---|---|
| [1-desired-state/](1-desired-state/) | The files **are** the objects. Mirror and edit them. | every live object has a home file |
| [2-rendered/](2-rendered/) | The files are **inputs to an offline renderer**. Can an edit be inverted back to one of them? | *renderability* |
| [3-expanded/](3-expanded/) | A **controller** materialises the objects, and they have **no home file**. | *provenance* |
| [4-machine-written/](4-machine-written/) | Git is an **output**. Something else already writes here. | *ownership* |
| [5-opaque/](5-opaque/) | The object **is not the object**. We may not be able to read it at all. | *capability* |
| [6-hostile/](6-hostile/) | Every naive parser assumption, broken on purpose. | can we even read the bytes |

Families 2, 3, and 4 are the three axes of the support model — renderability,
provenance, ownership — and they are the reason this grouping exists at all. The
model is argued in
[expansion-boundary-and-corpus-organisation.md](../../../docs/design/gitops-api/expansion-boundary-and-corpus-organisation.md).

### The distinction families 1 and 3 exist to protect

It is tempting to file "anything with an Argo CD or Flux CR in it" under
*expanded*. That is exactly wrong, and it is the mistake the provenance axis exists
to prevent:

- An `Application` in [1-desired-state/argocd-app-of-apps/](1-desired-state/argocd-app-of-apps/)
  is a **file a human wrote**. The root Application points at `applications/`, and
  every child Application is checked in. Nothing is synthesised.
- A Flux `Kustomization` in [1-desired-state/flux-monorepo/](1-desired-state/flux-monorepo/)
  points at a **folder of real files**. Its objects have homes.
- An `Application` in [3-expanded/argocd-applicationset-directories/](3-expanded/argocd-applicationset-directories/)
  is **synthesised from a directory glob**. It exists only in etcd.

All three involve a controller. Only the third has no home in Git. *"Was a
controller involved?"* is the wrong question; *"does this object have a home in
Git?"* is the right one.

## The fixtures

### 1-desired-state — the files are the objects

| Fixture | Structural axis it isolates |
|---|---|
| [argocd-plain](1-desired-state/argocd-plain/) | A directory of plain KRM; multi-doc files, arbitrary filenames, non-manifest YAML, `directory.include`/`exclude` globs |
| [repo-per-environment](1-desired-state/repo-per-environment/) | The environment boundary is a **repository** boundary; no shared base exists in Git |
| [argocd-app-of-apps](1-desired-state/argocd-app-of-apps/) | An `Application` that *describes* a deployment vs. the manifests it *references* vs. an ordinary object merely named `application.yaml`. Every child Application is a checked-in file |
| [flux-monorepo](1-desired-state/flux-monorepo/) | `apps/` + `infrastructure/` + `clusters/`; the strongest Flux fingerprint, and the `Kustomization` CR vs. `kustomization.yaml` collision |

### 2-rendered — the files are inputs to a renderer

| Fixture | Structural axis it isolates |
|---|---|
| [kustomize-overlays](2-rendered/kustomize-overlays/) | `base/` + `overlays/<env>/`; patches, `configMapGenerator`, `secretGenerator`, `namespace`, `namePrefix`, `images`, `replicas`, and a remote base |
| [helm-chart](2-rendered/helm-chart/) | The standard chart format: `templates/` is not KRM, `crds/` is, `values.yaml` is not, `values.schema.json` carries type information |
| [helm-environment-values](2-rendered/helm-environment-values/) | Effective config as a **merge** of chart defaults + common + environment + inline parameters |
| [argocd-external-helm](2-rendered/argocd-external-helm/) | Chart lives in an external registry; the repo holds Applications, versions, values, and stray plain manifests — three natures in one folder |
| [rendered-manifests](2-rendered/rendered-manifests/) | Git holds generated output, not sources; hash-suffixed names with no stable origin |

### 3-expanded — a controller materialises objects with no home file

The authored CR in these fixtures is itself ordinary, editable intent. What has no
home is its **output**. That split is the whole support contract, and these
fixtures exist to make it visible.

| Fixture | Structural axis it isolates |
|---|---|
| [argocd-applicationset-directories](3-expanded/argocd-applicationset-directories/) | Directory names *become* Applications; wildcards, exclusions, an empty directory, a nested app directory `apps/*` misses. **Observed: creating a folder deploys an app** |
| [argocd-applicationset-files](3-expanded/argocd-applicationset-files/) | YAML that is **not KRM at all** — ApplicationSet generator input, beside a Helm chart |
| [argocd-multicluster-matrix](3-expanded/argocd-multicluster-matrix/) | A cluster × app matrix: one folder renders N times, with a sparse per-cluster values matrix |
| [flux-helmrelease](3-expanded/flux-helmrelease/) | Helm values inside CRDs — five delivery mechanisms — plus `postBuild.substitute` and `targetNamespace` injected from outside the folder |
| [flux-resourceset-inline](3-expanded/flux-resourceset-inline/) | **KRM nested inside KRM.** One document carries the template *and* its inputs; nine live objects, zero files. The layout flux-operator pitches as a base+overlay replacement |
| [flux-resourceset-pull-requests](3-expanded/flux-resourceset-pull-requests/) | **The inputs are not in the repository.** The desired object set is "however many PRs are open right now" — unknowable from any scan |

### 4-machine-written — Git is an output

| Fixture | Structural axis it isolates |
|---|---|
| [flux-image-automation](4-machine-written/flux-image-automation/) | A controller that **commits to the repo**; a `$imagepolicy` comment is load-bearing, so Git is an output as well as an input |

### 5-opaque — the object is not the object

| Fixture | Structural axis it isolates |
|---|---|
| [sops-encrypted](5-opaque/sops-encrypted/) | A file that is simultaneously a valid Kubernetes object and unreadable: cleartext `metadata`, opaque `data` |

### 6-hostile — parser edges

| Fixture | Structural axis it isolates |
|---|---|
| [mixed-and-hostile](6-hostile/mixed-and-hostile/) | Every naive assumption broken at once — filename implies kind, extension implies format, YAML implies KRM, KRM never nests KRM |

## Fixtures are mixtures; families name the *primary* decision

Real repositories are not pure, and neither are these. A fixture lives in the
family of the decision it primarily isolates, but most touch more than one axis.
The secondary axes are where the surprises are:

| Fixture | Also touches | Because |
|---|---|---|
| `flux-monorepo` | rendered | its `apps/` folders are kustomize base + overlays |
| `flux-monorepo` | expanded | it contains `HelmRelease`s, whose rendered objects have no home |
| `kustomize-overlays` | machine-written | it carries a committed `.argocd-source.yaml` written by Argo CD Image Updater |
| `argocd-external-helm` | desired-state | it holds stray plain manifests beside the Applications |
| `mixed-and-hostile` | expanded | it plants KRO and Crossplane documents — KRM nesting KRM |
| `helm-chart` | desired-state | `crds/` **is** KRM, even though `templates/` is not |

That last row is a live bug, not a curiosity: a repo-scan today reports
`charts/frontend/crds` as the single accepted candidate in `helm-chart`, which
makes an onboarding report say "1 folder supported" about a chart we cannot
meaningfully support at all.

## What the corpus still lacks

Named here so a gap is a decision rather than an oversight:

- **`kro-and-crossplane`** — KRO and Crossplane currently appear only as hostile
  documents inside [6-hostile/](6-hostile/mixed-and-hostile/). They deserve a
  first-class `3-expanded/` fixture, because KRM-nested-in-KRM is mainstream for
  them, not hostile.
- **`argocd-image-updater`** — today only a `.argocd-source.yaml` dotfile inside
  [2-rendered/kustomize-overlays/](2-rendered/kustomize-overlays/). It is a
  machine-writer and belongs in [4-machine-written/](4-machine-written/).
- **`sealed-secrets` / `external-secrets`** — two more shapes of "a Secret that is
  not the Secret", alongside the SOPS case in [5-opaque/](5-opaque/sops-encrypted/).
- **Kustomize `components`** — already represented as an assertion in
  [`contextual-namespace/unsupported/components/`](../../../internal/manifestanalyzer/testdata/contextual-namespace/unsupported/components/),
  but not as a layout.
- **Jsonnet / Tanka sources**, and **Argo CD config-management plugins** —
  arbitrary programs that emit KRM.

## Two things worth knowing before you read any fixture

**Some comments are load-bearing.** A `# {"$imagepolicy": ...}` setter in
[flux-image-automation](4-machine-written/flux-image-automation/) is the only
marker that makes a field automated; a `{{ ... }}` action inside a *YAML comment*
in a Helm template is still parsed by Helm and will break the chart. Comments are
not presentation.

**Some hidden dotfiles decide what deploys.** `.argocd-source.yaml` (path-wide) and
`.argocd-source-<appname>.yaml` (per-Application) are merged over an Argo CD
Application's `spec.source`, so they outrank the values and transformers a reader
would naturally inspect. They appear in
[2-rendered/kustomize-overlays/](2-rendered/kustomize-overlays/) — written and
committed by Argo CD Image Updater — and in
[2-rendered/helm-environment-values/](2-rendered/helm-environment-values/). They
carry no `apiVersion`/`kind`.

## Deliberately absent

Two hostile cases cannot be represented as committed files, and are described in
prose inside [6-hostile/mixed-and-hostile](6-hostile/mixed-and-hostile/) instead:

- a **symlink** escaping the folder
- a genuinely **empty directory** (git cannot store one; the fixtures use `.gitkeep`)

## Conventions

Fixtures share names so they can be compared: apps `frontend` / `backend` /
`worker`, images under `ghcr.io/example-org/`, environments `dev` / `staging` /
`production`, and repo `https://github.com/example-org/gitops.git`.

Every credential, token, key, and ciphertext is **fake** and marked as such. The
`age` recipients are public keys from upstream documentation; the SOPS blocks are
structurally correct with placeholder ciphertext.

[2-rendered/kustomize-overlays](2-rendered/kustomize-overlays/) checks in a
`secrets.env` file on purpose, so the root [`.gitignore`](../../../.gitignore)
carries a scoped `!test/fixtures/gitops-layouts/**/*.env` negation for it.

## Checking the fixtures are real

The three Helm charts are genuine charts, not sketches:

```bash
helm lint 3-expanded/argocd-applicationset-files/chart
helm lint 2-rendered/helm-chart/charts/frontend      # renders after `helm dependency build`
helm lint 2-rendered/helm-environment-values/chart
```

All three lint clean. `argocd-applicationset-files` and `helm-environment-values`
also `helm template` offline; `helm-chart` declares an unvendored `redis` subchart,
so rendering it needs `helm dependency build` first — which is the normal state of a
real chart repository.

Every other YAML in the corpus parses, except where a fixture states it must not:
Helm `templates/`, and `6-hostile/mixed-and-hostile/templates/deployment.yaml`.

## The behavioural baseline

[`support-today.md`](support-today.md) records what `manifest-analyzer` reports for
each fixture **today**. It is generated, not written:

```bash
task gitops-layouts-baseline
```

Regenerate it whenever the analyzer's behaviour changes, and commit the diff. A
change to the acceptance boundary then shows up in review as exactly which fixtures
moved, and in which direction — which is the point of having a corpus at all.

## Relationship to the other corpora

This corpus is **descriptive** and drives no test. Two sibling corpora already
exist and *are* assertion-driven — do not duplicate them here:

- [`internal/manifestanalyzer/testdata/scan-repo/`](../../../internal/manifestanalyzer/testdata/scan-repo/) —
  repo-discovery fixtures, each with a golden JSON report.
- [`internal/manifestanalyzer/testdata/contextual-namespace/`](../../../internal/manifestanalyzer/testdata/contextual-namespace/) —
  the supported/unsupported boundary for kustomize-inherited namespaces.

When a layout here graduates into a decision, that decision belongs in one of those
corpora (as an assertion) and in
[`docs/design/gitops-api/`](../../../docs/design/gitops-api/) (as the reasoning).

## Adding a fixture

Add one whenever a new "can we handle X?" question comes up and the existing
fixtures cannot express it. Put it in the family of the decision it forces. Keep it
small, keep it real — real apiVersions, real chart names, plausible images — follow
the four-section README (*What this is* / *Layout* / *What makes it structurally
distinct* / *Open questions*), and **resist recording a verdict**.

If you have run the layout against a real controller, add an **Observed behaviour**
section with what the controller actually did. That is a fact, not a verdict, and it
is the most valuable thing a fixture can carry.
