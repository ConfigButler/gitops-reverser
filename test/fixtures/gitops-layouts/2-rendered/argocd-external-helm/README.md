# argocd-external-helm

## What this is

The most common way platform teams install third-party add-ons with Argo CD: the
chart itself lives in an **external Helm or OCI registry** (upstream projects such
as ingress-nginx, cert-manager, and external-dns), and the Git repo holds almost
none of the rendered Kubernetes manifests. Instead the repo carries only Argo CD
`Application` resources that point at the remote chart, the chart version to pin,
the value overrides, and the occasional plain manifest that has to sit alongside
the chart (a custom ConfigMap, an Ingress, a ClusterIssuer).

This shape is what you get by following the Argo CD docs for Helm charts and the
"apps repo vs. chart repo" separation. Argo CD uses Helm purely to *render* the
chart into manifests; Argo CD itself — not Helm's `helm install` — then owns the
apply/prune/heal lifecycle of the resulting objects. The repo is therefore a mix
of Argo CD configuration, Helm inputs, and a small tail of real KRM.

This fixture also deliberately shows the **three different ways an Application can
carry values**, plus a co-located "hybrid folder" that mixes all the natures in
one directory.

## Layout

```yaml
08-argocd-external-helm/
├── README.md
├── applications/
│   ├── ingress-nginx.yaml      # multi-source Application, valueFiles via $values ref
│   ├── cert-manager.yaml       # single source, inline helm.values (a YAML string)
│   └── external-dns.yaml       # single source, helm.valuesObject (structured mapping)
├── values/
│   ├── ingress-nginx/
│   │   ├── common.yaml         # Helm values -- NOT a Kubernetes object
│   │   └── production.yaml     # Helm values -- NOT a Kubernetes object
│   ├── cert-manager/
│   │   └── production.yaml     # Helm values -- NOT a Kubernetes object (unreferenced)
│   └── external-dns/
│       └── production.yaml     # Helm values -- NOT a Kubernetes object (unreferenced)
├── extras/
│   └── ingress-nginx/
│       └── middleware.yaml     # plain KRM: ConfigMap + Ingress (multi-document)
└── platform/
    └── cert-manager/
        ├── application.yaml    # an Argo CD Application (multi-source)
        ├── values.yaml         # Helm values -- NOT a Kubernetes object
        └── clusterissuer.yaml  # plain KRM (cert-manager.io/v1 ClusterIssuer)
```

## What makes it structurally distinct

- **Most of the deployed resources are never in Git.** The Deployments, Services,
  and CRDs that actually run come from the remote chart; the repo only pins a
  `chart:` name and `targetRevision:` and layers value overrides on top.
- **Five files are YAML but NOT Kubernetes objects.** Every file under `values/`
  (`values/ingress-nginx/common.yaml`, `values/ingress-nginx/production.yaml`,
  `values/cert-manager/production.yaml`, `values/external-dns/production.yaml`) and
  `platform/cert-manager/values.yaml` are Helm value inputs: they have no
  `apiVersion` and no `kind`. They look like manifests to a naive YAML scan but are
  not KRM.
- **Values appear in four physically different forms across the fixture:** as
  external files referenced by a `$values` ref (`applications/ingress-nginx.yaml`),
  as an inline YAML string in `helm.values` (`applications/cert-manager.yaml`), as a
  structured `helm.valuesObject` mapping (`applications/external-dns.yaml`), and as a
  co-located file consumed via `$values` (`platform/cert-manager/`).
- **The `$values` ref source has no chart and no path.** In the multi-source
  Applications, the second source exists only to expose this repo's value files to
  the first source; on its own it deploys nothing.
- **`extras/ingress-nginx/middleware.yaml` is real KRM and multi-document** — a `v1`
  ConfigMap of NGINX snippet config and a `networking.k8s.io/v1` Ingress default
  backend in one file — sitting next to a chart it is not part of.
- **`values/cert-manager/production.yaml` and `values/external-dns/production.yaml`
  are present but unreferenced:** their matching Applications carry values inline
  instead, so these files are inputs that no Application currently points at.
- **`platform/cert-manager/` is the hybrid folder — the most representative
  real-world shape in this fixture.** One directory holds three files of three
  different natures side by side: an Argo CD `Application` (Argo CD configuration),
  a `values.yaml` (Helm input, not KRM), and a `clusterissuer.yaml` (plain
  `cert-manager.io/v1` KRM). The ClusterIssuer additionally depends on CRDs that the
  chart the Application installs must create first.
- **The cert-manager chart is modeled twice.** `applications/cert-manager.yaml` and
  `platform/cert-manager/application.yaml` both install the cert-manager chart and
  both use `metadata.name: cert-manager` — one flat, one co-located.

## Open questions

- How should a folder like `platform/cert-manager/`, whose three files have three
  different natures (Argo CD config, Helm values, plain KRM), be classified — as a
  KRM directory, an Argo CD app source, or none of these?
- Without the chart available locally, what schema validates a value file — how
  would a reader know `controller.replicaCount` or `installCRDs` is meaningful, or
  catch a typo, when the keys are only defined by a remote chart's `values.yaml`?
- Are `values/cert-manager/production.yaml` and `values/external-dns/production.yaml`
  dead configuration, or are they consumed by something outside this repo — and how
  would a scanner tell the difference from structure alone?
- Two Applications install the cert-manager chart under the same `metadata.name`:
  is `platform/cert-manager/` an alternative to the flat `applications/` layout that
  you would pick between, or would both be reconciled into the same namespace?
- What is the `$values` ref source (no chart, no path) "deploying", and how should a
  reader that walks sources reason about a source whose only job is to carry files?
- Does `platform/cert-manager/clusterissuer.yaml` belong to the Application in the
  same folder, or is a separate directory-source Application expected to apply it —
  and if so, would that source also try to apply `application.yaml` and `values.yaml`?
- In `extras/ingress-nginx/middleware.yaml`, what applies the two objects if the
  Application only renders the remote chart — is a directory source expected to pick
  up this file, and how is its relationship to the chart expressed anywhere?
