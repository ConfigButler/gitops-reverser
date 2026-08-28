# External base and overlay

This scenario gives one environment overlay a writable Kustomize root while treating its shared
base as read-only input. It captures a Deployment change that the overlay can express as an image
tag update.

## Starting repository

[`repository/`](repository/) contains a reusable `podinfo` base and a production overlay:

```text
apps/podinfo/
  base/
    deployment.yaml
    kustomization.yaml
  overlays/prod/
    kustomization.yaml
```

The target path is `apps/podinfo/overlays/prod`. Its Kustomize root includes `../../base`, but the
base is an input to rendering. It is outside the target's write scope.

## Proposed configuration

[`config/gittarget.yaml`](config/gittarget.yaml) declares one `Kustomize` root for the
`podinfo-prod` namespace. [`config/watchrule.yaml`](config/watchrule.yaml) captures Deployments.
The scenario assumes the Deployment image is a supported writable expression in the overlay's
`images:` transformer.

## Scenario contract

- Starting repository:
  [`repository/apps/podinfo/overlays/prod/`](repository/apps/podinfo/overlays/prod/).
- Live input: [`input/deployment-podinfo.yaml`](input/deployment-podinfo.yaml).
- Expected Git change: [`expected-image-update.patch`](expected-image-update.patch).
- Expected status: `Ready=True` after the changed overlay renders successfully.
- Boundary: the target may read `../../base` to render, but it never writes outside the overlay.

## Boundary

The target owns the overlay and does not own a shared base. A request that needs to change a base
field is refused as outside `spec.path`; it does not search for another location with the same
Deployment identity. If the overlay cannot express a live field change through a supported
transformer, the operator reports a refusal rather than editing the rendered base.

The exact support boundary remains the
[Kustomize support contract](../../../design/support-boundary/support-contract.md). This example narrows its
write scope further than that contract: rendering may cross into the base, but writing does not.
