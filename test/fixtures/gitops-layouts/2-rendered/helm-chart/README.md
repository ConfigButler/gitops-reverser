# helm-chart

## What this is

A single, self-contained Helm chart in the canonical directory layout produced
by `helm create` and standardized by the Helm project. This is the most
standards-conforming, least ambiguous packaging format in the whole corpus: the
file names, the `templates/` vs `crds/` split, `Chart.yaml`/`Chart.lock`,
`values.yaml`, and `values.schema.json` all mean specific things defined by
Helm, not by local convention. Charts like this are authored by application
teams and by upstream vendors, published to chart repositories
(`https://charts.bitnami.com/bitnami` and the like), and consumed either
directly with `helm install` or as the source of an Argo CD / Flux application.
The chart here is `frontend`, with a conditional `redis` subchart dependency.

## Layout

```
06-helm-chart/
├── README.md
└── charts/
    └── frontend/
        ├── Chart.yaml              # chart metadata + dependencies (NOT a K8s object)
        ├── Chart.lock              # resolved dependency pins (machine-managed)
        ├── values.yaml             # default config values (NOT a K8s object)
        ├── values.schema.json      # JSON Schema for values.yaml
        ├── .helmignore             # package-time ignore globs
        ├── charts/
        │   └── .gitkeep            # vendored subchart dir, empty here
        ├── crds/
        │   └── widgets.example.com.yaml   # plain CRD, Helm treats it specially
        └── templates/
            ├── _helpers.tpl        # partial library, renders no object itself
            ├── deployment.yaml     # Helm template, not plain KRM
            ├── service.yaml
            ├── configmap.yaml
            ├── ingress.yaml        # whole object gated on ingress.enabled
            ├── serviceaccount.yaml
            ├── NOTES.txt           # plain-text post-install message
            └── tests/
                └── test-connection.yaml   # Pod behind a helm.sh/hook: test
```

## What makes it structurally distinct

- **`templates/*.yaml` is not plain KRM.** Every file under `templates/` is a Go
  text/template. As stored, `deployment.yaml` does not parse as a valid
  Deployment — `{{ include "frontend.fullname" . }}` and `{{ .Values.* }}` must
  be rendered against a merged values set and release context first. You cannot
  apply these files with `kubectl apply -f`.
- **`crds/*.yaml` generally IS plain YAML, but Helm treats it specially.**
  `crds/widgets.example.com.yaml` is an ordinary
  `apiextensions.k8s.io/v1` CustomResourceDefinition with no template actions.
  Helm installs everything under `crds/` before the `templates/` resources and
  intentionally never templates, upgrades, or deletes it.
- **`values.yaml` is not KRM.** It has no `apiVersion`/`kind`; it is the default
  input to the templates, addressable as `.Values`.
- **`values.schema.json` carries useful type information.** It is a draft-07 JSON
  Schema constraining `replicaCount`, `image.repository`, `image.tag`, and
  `service.port`, with `required` fields — a machine-readable description of what
  a valid value override looks like.
- **`Chart.yaml` and `Chart.lock` are metadata, not objects.** There is no
  `kind: Chart` in the Kubernetes API. `Chart.lock` is machine-managed and pins
  the `redis` dependency to a resolved version and digest.
- **The chart is valid but not renderable offline.** `helm lint` passes; `helm
  template` fails with "found in Chart.yaml, but missing in charts/ directory:
  redis" until `helm dependency build` fetches the subchart over the network.
  This is the normal state of a real chart repository, which vendors `charts/`
  rarely if ever — so a folder's object set is not derivable from its bytes alone.
- **A template action inside a YAML comment is still a template action.** Helm
  parses the whole file, comments included, so a `#` comment containing example
  braces is a parse error, not documentation. The template comments here are
  worded to avoid it.
- **`_helpers.tpl` and `NOTES.txt` render no object.** The `_`-prefixed partial
  is a template library included by other files; `NOTES.txt` is plain text shown
  after install. Neither becomes a resource.
- **Conditional and looped resources.** `ingress.yaml` is wrapped in
  `{{- if .Values.ingress.enabled }}`, so with default values it renders to
  nothing. `configmap.yaml` `range`s over `.Values.config`. The set of objects a
  release actually contains is decided at render time, not by counting files.
- **A rendered resource does not map back to one template or one value.**
  `deployment.yaml` pulls from `image.repository`, `image.tag`, `replicaCount`,
  `resources`, `service.targetPort`, the `_helpers.tpl` label partials, and
  `Chart.AppVersion`. One value (`service.port`) feeds several objects. The
  file-to-object and value-to-field relationships are many-to-many.

## Open questions

- Should a chart-template folder be rejected outright as un-editable KRM, or
  should a tool instead expose only safe values editing (`values.yaml`
  constrained by `values.schema.json`) and leave the templates alone?
- If `deployment.yaml` cannot be parsed without rendering, on what basis would a
  tool decide it is even a Deployment before a Helm render is performed?
- A rendered object draws from many values and helpers; if an operator wants to
  change the effective image tag, is the correct write target `values.yaml`,
  `Chart.yaml`'s `appVersion`, or an install-time override — and how would a tool
  know which?
- `crds/widgets.example.com.yaml` is appliable as-is while its sibling
  `templates/*.yaml` is not. Should the two directories be treated as different
  kinds of source entirely?
- `ingress.yaml` renders to nothing under default values. Does a resource that
  only conditionally exists belong in an inventory of what the chart manages?
- `Chart.lock` names a `redis` subchart that is not vendored here (`charts/` is
  empty). What is the effective object set before `helm dependency update` runs?
