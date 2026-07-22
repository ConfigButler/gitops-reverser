# argocd-applicationset-files

## What this is

An Argo CD `ApplicationSet` using the **git files generator**. A single
`ApplicationSet` object watches a directory of small config files
(`deployments/**/*.yaml`) and renders one `Application` per file. This is a
common multi-tenant / multi-environment pattern for teams who want to add an
environment or app by dropping a tiny data file into Git rather than by writing
a full `Application` manifest by hand. Each generated `Application` deploys the
in-repo Helm chart under `chart/`, parameterised from the generator file.

## Layout

```
04-argocd-applicationset-files/
├── README.md
├── applicationset.yaml              # KRM: kind ApplicationSet
├── deployments/
│   ├── dev/
│   │   ├── frontend.yaml            # NOT KRM - generator input (plain data)
│   │   └── backend.yaml             # NOT KRM - generator input (plain data)
│   └── production/
│       ├── frontend.yaml            # NOT KRM - generator input (plain data)
│       └── backend.yaml             # NOT KRM - generator input (plain data)
└── chart/
    ├── Chart.yaml                   # NOT KRM - Helm chart metadata (valid YAML)
    ├── values.yaml                  # NOT KRM - Helm default values (valid YAML)
    └── templates/
        ├── deployment.yaml          # NOT parseable YAML - Helm/Go template
        └── service.yaml             # NOT parseable YAML - Helm/Go template
```

## What makes it structurally distinct

- Four distinct *categories* of `.yaml` file live in one directory tree:
  - **A Kubernetes object:** `applicationset.yaml` (kind `ApplicationSet`).
  - **Generator input that is NOT a Kubernetes object:**
    `deployments/<env>/<app>.yaml`. These files have no `apiVersion`, no `kind`,
    and no `metadata`. They are arbitrary key/value data whose top-level keys
    (`name`, `namespace`, `chartVersion`, `values`) become template parameters.
  - **Helm value/metadata YAML that is NOT a Kubernetes object:** `Chart.yaml`
    and `values.yaml`. They are valid YAML documents but there is no `Chart` or
    `Values` kind in the Kubernetes API.
  - **Helm templates that are NOT even valid standalone YAML:**
    `chart/templates/*.yaml`. They contain Go template actions
    (`{{ .Values.replicas }}`, `{{ include "tenant-app.fullname" . }}`) and only
    become Kubernetes objects after Helm renders them.
- The actual deployed Kubernetes objects (a `Deployment`, a `Service`) exist
  **nowhere as a checked-in document** — they are produced at sync time by
  expanding the chart with per-`Application` parameters.
- The desired state for an environment is spread across three places: the
  `ApplicationSet` template, the generator input file, and the chart defaults.
- `dev/frontend.yaml` and `production/frontend.yaml` share the same `name` but
  differ in `replicas`, so file path (not file content) carries the environment.

## Open questions

- How does a tool decide which `.yaml` file in this tree is a Kubernetes object?
  `apiVersion` + `kind` presence distinguishes `applicationset.yaml` from the
  generator inputs, but `Chart.yaml` also has an `apiVersion` (`v2`) and no
  Kubernetes `kind` — is `apiVersion`-alone enough to misclassify it?
- If a tool wants to reflect a running `Deployment` back into Git, which file
  does it edit — the generator input `values.image`, the chart `values.yaml`
  default, or the `ApplicationSet` parameter list?
- Can the effective desired state be reconstructed without executing the git
  files generator and rendering Helm, i.e. without an Argo CD-equivalent engine?
- The `Deployment`'s final `metadata.name` depends on `include
  "tenant-app.fullname"` and the per-Application `releaseName` — can that name be
  known statically from the files alone?
- If someone adds `deployments/staging/frontend.yaml`, a new `Application`
  appears with no other change. How would a tool notice that a directory listing
  is itself part of the desired state?
