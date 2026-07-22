# flux-helmrelease

## What this is

Flux CD installing Helm charts the GitOps way: instead of running `helm install`,
you commit a `HelmRepository`/`OCIRepository` (the chart source) and a
`HelmRelease` (the desired release), and the helm-controller reconciles them.
This is how most Flux users run third-party charts (ingress-nginx, cert-manager,
Prometheus) and their own charts. The interesting part is not the release itself
but where its *values* come from: Flux can assemble a HelmRelease's values from
inline YAML, from ConfigMaps, from Secrets, from a single spliced scalar, and
from ConfigMaps that kustomize generates out of plain values files. This fixture
puts all five in one tree, and then adds a Flux Kustomization that rewrites the
result from outside the app folder.

## Layout

```yaml
10-flux-helmrelease/
├── README.md
├── infrastructure/
│   ├── sources/
│   │   ├── repositories.yaml        # HelmRepository x2 (http + oci) + OCIRepository
│   │   └── kustomization.yaml        # kustomize.config.k8s.io (build file)
│   └── controllers/
│       └── ingress-nginx/
│           ├── helmrelease.yaml      # values mechanisms 1-4
│           ├── values-configmap.yaml # ConfigMaps backing mechanisms 2 and 4
│           └── kustomization.yaml     # kustomize.config.k8s.io (build file)
├── apps/
│   └── frontend/
│       ├── helmrelease.yaml          # values mechanism 5 (generated ConfigMap)
│       ├── values.yaml               # plain Helm values - NOT a k8s object
│       └── kustomization.yaml         # configMapGenerator turns values.yaml into one
└── clusters/
    └── production/
        ├── flux-system/              # written by `flux bootstrap`
        │   ├── gotk-components.yaml   # TRUNCATED (~35k lines in reality)
        │   ├── gotk-sync.yaml         # GitRepository + root Kustomization CR
        │   └── kustomization.yaml     # kustomize.config.k8s.io (build file)
        ├── infrastructure.yaml       # Flux Kustomization CRs (NOT build files)
        ├── apps.yaml                 # Flux Kustomization CR w/ postBuild + targetNamespace
        └── kustomization.yaml         # kustomize.config.k8s.io (build file)
```yaml

## What makes it structurally distinct

- **Five ways values reach a HelmRelease.** The desired state of a Helm release
  is not one document; it is a merge chain:
  1. **inline `spec.values`** — checked straight into `ingress-nginx/helmrelease.yaml`.
  2. **`spec.valuesFrom` → ConfigMap** — a whole values document under one
     `values.yaml` key in `values-configmap.yaml`.
  3. **`spec.valuesFrom` → Secret** — sensitive values from a Secret that is
     created out of band and intentionally **not committed** to this repo.
  4. **`spec.valuesFrom` with `valuesKey` + `targetPath`** — a single scalar
     (`replicas: "3"`) spliced into `controller.replicaCount`, overriding what an
     earlier ConfigMap set.
  5. **generated ConfigMap** — `apps/frontend/values.yaml` is a plain Helm values
     file (no `apiVersion`, no `kind`) that a `configMapGenerator` in
     `apps/frontend/kustomization.yaml` turns into the `frontend-values`
     ConfigMap the frontend HelmRelease consumes. One file that is *not* a
     Kubernetes object becomes one.
  Ordering matters: later `valuesFrom` entries win, so the effective values can
  differ from any single source file.
- **Three source kinds, deliberately side by side.** `repositories.yaml` holds a
  classic HTTP `HelmRepository`, a `type: oci` `HelmRepository`, and an
  `OCIRepository`. They are not interchangeable: the HTTP/oci HelmRepository
  selects a chart in the HelmRelease's `chart` block, while the OCIRepository
  pins one artifact and is referenced through `spec.chartRef`.
- **`clusters/production/apps.yaml` rewrites the app folder from outside it.** It
  is a Flux Kustomization CR (not a build file) that sets `targetNamespace:
  production` — deciding where every rendered object lands — and
  `postBuild.substitute`/`substituteFrom`, which expand `${var}` tokens (like the
  `${cluster_domain}` left unresolved in `apps/frontend/values.yaml`) *after*
  kustomize build. The transform is declared in `clusters/`, the tokens live in
  `apps/`, and the `cluster-vars`/`cluster-secret-vars` inputs are created out of
  band and never committed.
- **Files that are YAML but NOT Kubernetes objects:** every `kustomization.yaml`
  (a kustomize build file) and `apps/frontend/values.yaml` (a Helm values file
  that only becomes a ConfigMap after generation). `gotk-components.yaml` is
  truncated here from its real ~35,000 lines.
- **Things referenced but absent on purpose:** the `ingress-nginx-secret-values`
  Secret (mechanism 3) and the `cluster-vars`/`cluster-secret-vars` substitution
  inputs. The tree does not contain everything the release depends on.

## Open questions

- If the effective values are a merge of inline YAML, two ConfigMaps, a Secret,
  and a scalar splice — with later entries overriding earlier ones — what is the
  "desired values" of the release, and can any single file in the repo be said to
  hold it?
- `apps/frontend/values.yaml` is not a Kubernetes object until a
  `configMapGenerator` runs. Reading the folder statically, how would a tool know
  that this file becomes the ConfigMap the HelmRelease names?
- `clusters/production/apps.yaml` injects `targetNamespace` and rewrites
  `${cluster_domain}` via `postBuild`. What does `apps/frontend/`'s "desired
  state" even mean when a transform declared in another directory changes the
  namespace and the rendered content?
- Two of the inputs (a Secret and the `cluster-vars` ConfigMap) are created out
  of band and never committed. Can a repo-only view ever reconstruct what the
  cluster will actually receive?
- Mechanism 3's Secret is `optional: true` and mechanism 4 overrides mechanism
  2's `replicaCount`. Does the checked-in YAML tell you the final value, or only
  the rules for computing it once cluster state is known?
