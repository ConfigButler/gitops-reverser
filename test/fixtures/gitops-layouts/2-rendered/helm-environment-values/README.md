# helm-environment-values

## What this is

One Helm chart deployed to several environments, where each environment is just
a different stack of values files pointed at the same chart. This is the most
common way an application team owns its own chart: the chart lives in the repo
once, shared settings live in a `common.yaml`, and each environment
(`dev`, `production`) contributes a small override file. An Argo CD `Application`
per environment names the chart as its source and lists the value files, in
order, under `spec.source.helm.valueFiles`, optionally pinning a value like the
image tag inline via `helm.parameters`. Nothing here duplicates the chart per
environment — the environments differ only in which values get layered on top.

## Layout

```yaml
07-helm-environment-values/
├── README.md
├── chart/
│   ├── .argocd-source.yaml     # HIDDEN Argo CD source override (NOT a K8s object)
│   ├── Chart.yaml              # chart metadata (NOT a K8s object)
│   ├── values.yaml             # chart defaults, lowest merge layer
│   └── templates/
│       ├── deployment.yaml     # Helm template, not plain KRM
│       └── service.yaml
├── values/
│   ├── common.yaml             # shared overrides (NOT a K8s object)
│   ├── dev.yaml                # dev-only overrides (NOT a K8s object)
│   └── production.yaml         # prod-only overrides (NOT a K8s object)
└── argocd/
    ├── dev.yaml                # Argo CD Application (control-plane object)
    └── production.yaml         # Argo CD Application (control-plane object)
```yaml

## What makes it structurally distinct

- **The effective config is a merge, not a file.** What actually runs in `dev`
  is `chart/values.yaml` overlaid by `values/common.yaml`, then
  `values/dev.yaml`, then the inline `helm.parameters` entry from
  `argocd/dev.yaml`. No single file on disk contains the resolved configuration;
  it only exists after Helm merges the layers at render time.
- **Layer order is precedence.** In `valueFiles`, later wins over earlier, and
  every file wins over the chart's own `values.yaml`. `helm.parameters` wins over
  all of them. The order of the two lines under `valueFiles` is load-bearing.
- **A hidden dotfile is the outermost layer.** `chart/.argocd-source.yaml` is an
  Argo CD *source override*: Argo CD reads it from the Application's source path
  and merges it over `spec.source`. It therefore beats the `helm.parameters`
  written in `argocd/production.yaml`, and the image tag that actually deploys is
  the `1.8.4` in the dotfile, not the `1.8.3` in the Application. It carries no
  `apiVersion`/`kind`; its top-level keys are Application `spec.source` fields.
  A sibling `.argocd-source-<appname>.yaml` targets one named Application and
  wins over the path-wide file. A directory listing that hides dotfiles shows
  none of this.
- **The same key is deliberately set in more than one layer.** `image.tag`
  appears in `chart/values.yaml` but is intentionally omitted from the
  environment value files and instead pinned inline per environment
  (`1.8.3-rc.4` for dev, `1.8.3` for production). `ingress.host` is set in each
  environment file; `replicaCount` differs between chart default, dev, and prod.
- **`values/*.yaml` are not Kubernetes objects.** `common.yaml`, `dev.yaml`, and
  `production.yaml` have no `apiVersion`/`kind`. They are Helm inputs; only the
  chart templates and the Argo CD `Application`s are (or render to) K8s objects.
- **`chart/templates/*.yaml` are not plain KRM.** They only become Deployments
  and Services after a render against the merged values, so the replica count and
  image reference are not knowable from the template files alone.
- **Two Applications, one chart.** `argocd/dev.yaml` and `argocd/production.yaml`
  differ only in which environment file they layer, the inline tag, and the
  destination namespace. The environment axis lives entirely in the value stack.

Two sibling conventions appear in the wild for the same idea and are worth
noting even though they are not laid out as separate directories here. First,
flat `values-dev.yaml` / `values-production.yaml` files sitting directly beside
the chart (rather than in a `values/` subdirectory), which is the shape Helm's
own `-f values-dev.yaml` examples use. Second, a nested `values/` directory
placed *inside* the chart directory itself, so the overrides travel with the
packaged chart. Both encode the same "chart plus per-environment overlay" idea;
they differ only in where the override files sit relative to the chart.

## Open questions

- When an operator edits an *effective* value (say, the running replica count in
  production), which layer should the change be written back to — the chart
  default, `common.yaml`, `production.yaml`, or the inline `helm.parameters`?
- If a value is set in more than one layer (as `image.tag` and `replicaCount`
  are), how is a tool to know which layer is the intended "home" for a change
  versus an incidental override?
- The `valueFiles` paths (`../values/common.yaml`) are relative to the chart and
  declared in the `Application`, not in the chart. Given only `chart/` and
  `values/`, could a tool reconstruct which files compose each environment?
- The resolved configuration exists only after a Helm merge. Without performing
  that merge, what can be said about what `dev` versus `production` actually run?
- Inline `helm.parameters` silently overrides the value files. Should an edit
  ever target a value file when an inline parameter for the same key would win?
- The flat `values-dev.yaml` and the in-chart nested `values/` conventions carry
  the same intent as this layout — should all three be recognized as the same
  pattern, or treated as structurally different sources?
- `.argocd-source.yaml` is deployment-affecting configuration that a scanner
  ignoring dotfiles never sees, and that a scanner treating all YAML as KRM would
  misclassify as a malformed object. Which is the worse failure?
- Reporting the effective image tag for this folder requires knowing that a
  hidden file overrides the Application. Can that be discovered structurally, or
  only by encoding Argo CD's precedence rules?
