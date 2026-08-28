# Homelab Flux

This scenario captures Flux declarations in the `flux-system` namespace. A homelab owner can edit a
`HelmRelease`, `GitRepository`, or `HelmRepository` through the Kubernetes API and review the
resulting Git change without asking GitOps Reverser to reverse a chart.

## Repository folder

[`repository/`](repository/) is a regular Flux folder with a local Kustomize root. It holds source
declarations in `sources.yaml` and one application declaration in `media.yaml`.

```text
clusters/home/flux-system/
  kustomization.yaml
  media.yaml
  sources.yaml
```

For a newly created Flux declaration, the `Kustomize` layout creates a sibling file and registers it
in `resources:`. It does not append the release to `media.yaml`, because that bundle is not a
declared layout rule.

## Proposed configuration

[`config/gittarget.yaml`](config/gittarget.yaml) declares a one-root Kustomize folder with one
source namespace. [`config/watchrule.yaml`](config/watchrule.yaml) selects only Flux declaration
types. The folder's `namespace: flux-system` transformer supplies the namespace for every document
in the folder.

## Scenario contract

- Starting repository: [`repository/`](repository/) with its existing `sources.yaml` and
  `media.yaml`.
- Live input: [`input/helmrepository-bitnami.yaml`](input/helmrepository-bitnami.yaml).
- Expected Git change:
  [`expected-helmrepository-bitnami.patch`](expected-helmrepository-bitnami.patch).
- Expected status: `Ready=True` after the root renders the new declaration.
- Boundary: a rendered object has no writable home; only selected Flux declarations can produce a
  Git change.

The configuration captures the layer a person would edit in Git:

- a `GitRepository` or `HelmRepository` changes where Flux obtains source material;
- a `HelmRelease` changes a chart version, source reference, or inline values;
- the chart's rendered Deployments, Services, and Secrets remain expansion output.

## Boundary

This is Flux declaration editing, not Helm inversion. A chart folder is skipped as a unit, and the
operator never turns a rendered Deployment edit into a speculative values change. The current
[support contract](../../support-boundary/support-contract.md) owns that boundary.

A free-standing values file is a separate planned projection, so it is absent from this first
scenario. Inline `HelmRelease.spec.values` are KRM and stay inside the declaration surface.
