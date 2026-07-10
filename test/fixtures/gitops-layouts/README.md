# GitOps layout fixtures

Real-world GitOps repository layouts, checked in so we can design and reason
against concrete examples instead of prose.

> **These fixtures record no conclusions.** Nothing here says which layouts
> GitOps Reverser supports, accepts, or refuses. Each fixture describes *what a
> layout is* and *what questions it raises*. Deciding the answers — and pinning
> them as golden reports — is deliberately a later step.

## Why this exists

The compatibility question "does GitOps Reverser support Argo CD / Flux
repositories?" is the wrong question, because Argo CD and Flux are mostly
orchestration *around* folders. The real boundary is determined by **what a
folder contains and how it is rendered**, not by which controller points at it.

To find that boundary honestly we need the actual shapes users have. This corpus
is that set of shapes.

The first-class target — a path whose checked-in files *are* the desired
Kubernetes objects, rather than inputs that must first be rendered — is only one
of the layouts below. The others exist so the edges are visible.

## Reading the corpus

Each fixture directory holds a self-contained example repository plus a
`README.md` with four sections:

| Section | Contains |
|---|---|
| What this is | The real-world pattern and who uses it |
| Layout | A tree of the fixture |
| What makes it structurally distinct | The discriminating facts, including every file that is YAML but *not* a Kubernetes object |
| Open questions | Unanswered questions the layout poses |

## The fixtures

| # | Fixture | Structural axis it isolates |
|---|---|---|
| 01 | [01-argocd-plain](01-argocd-plain/) | A directory of plain KRM; multi-doc files, arbitrary filenames, non-manifest YAML, `directory.include`/`exclude` globs |
| 02 | [02-argocd-app-of-apps](02-argocd-app-of-apps/) | An `Application` that *describes* a deployment vs. the manifests it *references* vs. an ordinary object merely named `application.yaml` |
| 03 | [03-argocd-applicationset-directories](03-argocd-applicationset-directories/) | Directory names *become* Applications; wildcards, exclusions, an empty directory, a directory with no manifests, a nested app directory `apps/*` misses |
| 04 | [04-argocd-applicationset-files](04-argocd-applicationset-files/) | YAML that is **not KRM at all** — ApplicationSet generator input, beside a Helm chart |
| 05 | [05-kustomize-overlays](05-kustomize-overlays/) | `base/` + `overlays/<env>/`; patches, `configMapGenerator`, `secretGenerator`, `namespace`, `namePrefix`, `images`, `replicas`, and a remote base |
| 06 | [06-helm-chart](06-helm-chart/) | The standard chart format: `templates/` is not KRM, `crds/` is, `values.yaml` is not, `values.schema.json` carries type information |
| 07 | [07-helm-environment-values](07-helm-environment-values/) | Effective config as a **merge** of chart defaults + common + environment + inline parameters |
| 08 | [08-argocd-external-helm](08-argocd-external-helm/) | Chart lives in an external registry; the repo holds Applications, versions, values, and stray plain manifests — three natures in one folder |
| 09 | [09-flux-monorepo](09-flux-monorepo/) | `apps/` + `infrastructure/` + `clusters/`; the strongest Flux fingerprint, and the `Kustomization` CR vs. `kustomization.yaml` collision |
| 10 | [10-flux-helmrelease](10-flux-helmrelease/) | Helm values inside CRDs — five delivery mechanisms — plus `postBuild.substitute` and `targetNamespace` injected from outside the folder |
| 11 | [11-repo-per-environment](11-repo-per-environment/) | The environment boundary is a **repository** boundary; no shared base exists in Git |
| 12 | [12-multicluster-applicationset](12-multicluster-applicationset/) | A cluster × app matrix: one folder renders N times, with a sparse per-cluster values matrix |
| 13 | [13-sops-encrypted](13-sops-encrypted/) | A file that is simultaneously a valid Kubernetes object and unreadable: cleartext `metadata`, opaque `data` |
| 14 | [14-rendered-manifests](14-rendered-manifests/) | Git holds generated output, not sources; hash-suffixed names with no stable origin |
| 15 | [15-mixed-and-hostile](15-mixed-and-hostile/) | Every naive assumption broken at once — filename implies kind, extension implies format, YAML implies KRM, KRM never nests KRM |
| 16 | [16-flux-image-automation](16-flux-image-automation/) | A controller that **commits to the repo**; a `$imagepolicy` comment is load-bearing, so Git is an output as well as an input |

## The layout dimensions underneath

Repositories organise primarily around one of four axes, and serious ones combine
two or three. The fixtures above are instances of these combinations; there is no
universal standard that says which is correct.

- **Application-first** — `apps/<app>/`
- **Environment-first** — `environments/<env>/<app>/`
- **Cluster-first** — `clusters/<cluster>/<app>/`
- **Team-first** — `teams/<team>/<app>/`

Of the three ecosystems, only one is genuinely standardised: **Helm charts** have
a strict directory format. **Flux** has a documented convention that `flux
bootstrap` reinforces (`clusters/<cluster>/flux-system/`). **Argo CD** has no
canonical structure at all — its common shapes emerge from App of Apps,
ApplicationSets, Kustomize, and Helm value-file patterns.

## Two things worth knowing before you read any fixture

**Some comments are load-bearing.** A `# {"$imagepolicy": ...}` setter in
[16](16-flux-image-automation/) is the only marker that makes a field automated;
a `{{ ... }}` action inside a *YAML comment* in a Helm template is still parsed by
Helm and will break the chart. Comments are not presentation.

**Some hidden dotfiles decide what deploys.** `.argocd-source.yaml` (path-wide) and
`.argocd-source-<appname>.yaml` (per-Application) are merged over an Argo CD
Application's `spec.source`, so they outrank the values and transformers a reader
would naturally inspect. They appear in [05](05-kustomize-overlays/) — written and
committed by Argo CD Image Updater — and in [07](07-helm-environment-values/).
They carry no `apiVersion`/`kind`.

## Deliberately absent

Two hostile cases cannot be represented as committed files, and are described in
prose inside [15-mixed-and-hostile](15-mixed-and-hostile/) instead:

- a **symlink** escaping the folder
- a genuinely **empty directory** (git cannot store one; the fixtures use `.gitkeep`)

Out of scope for now, roughly in the order they deserve to graduate:

- **Kustomize `components`** — common enough to be worth a fixture soon. Already
  represented as an assertion in
  [`contextual-namespace/unsupported/components/`](../../../internal/manifestanalyzer/testdata/contextual-namespace/unsupported/components/).
- **SealedSecrets and ExternalSecrets** — two more shapes of "a Secret that is not
  the Secret", alongside the SOPS case in [13](13-sops-encrypted/).
- **Jsonnet / Tanka sources**, and **Argo CD config-management plugins** — arbitrary
  programs that emit KRM.

## Conventions

Fixtures share names so they can be compared: apps `frontend` / `backend` /
`worker`, images under `ghcr.io/example-org/`, environments `dev` / `staging` /
`production`, and repo `https://github.com/example-org/gitops.git`.

Every credential, token, key, and ciphertext is **fake** and marked as such. The
`age` recipients are public keys from upstream documentation; the SOPS blocks are
structurally correct with placeholder ciphertext.

`05-kustomize-overlays` checks in a `secrets.env` file on purpose, so the root
[`.gitignore`](../../../.gitignore) carries a scoped `!test/fixtures/gitops-layouts/**/*.env`
negation for it.

## Checking the fixtures are real

The three Helm charts are genuine charts, not sketches:

```bash
helm lint 04-argocd-applicationset-files/chart
helm lint 06-helm-chart/charts/frontend      # renders after `helm dependency build`
helm lint 07-helm-environment-values/chart
```

All three lint clean. `04` and `07` also `helm template` offline; `06` declares an
unvendored `redis` subchart, so rendering it needs `helm dependency build` first —
which is the normal state of a real chart repository.

Every other YAML in the corpus parses, except where a fixture states it must not:
Helm `templates/`, and `15-mixed-and-hostile/templates/deployment.yaml`.

## Relationship to the other corpora

This corpus is **descriptive** and drives no test. Two sibling corpora already
exist and *are* assertion-driven — do not duplicate them here:

- [`internal/manifestanalyzer/testdata/scan-repo/`](../../../internal/manifestanalyzer/testdata/scan-repo/) —
  repo-discovery fixtures, each with a golden JSON report.
- [`internal/manifestanalyzer/testdata/contextual-namespace/`](../../../internal/manifestanalyzer/testdata/contextual-namespace/) —
  the supported/unsupported boundary for kustomize-inherited namespaces.

When a layout here graduates into a decision, that decision belongs in one of
those corpora (as an assertion) and in
[`docs/design/gitops-api/`](../../../docs/design/gitops-api/) (as the reasoning).

## Adding a fixture

Add one whenever a new "can we handle X?" question comes up and the existing
fixtures cannot express it. Keep it small, keep it real — real apiVersions, real
chart names, plausible images — follow the four-section README, and resist
recording a verdict.
