# Homelab Argo CD

This scenario captures a homelab's Argo CD `Application` declarations. The Git target owns one
app-of-apps folder, and the watched objects all live in Argo CD's `argocd` namespace.

## Repository folder

[`repository/`](repository/) contains one Kustomize root and two `Application` documents. A new
Application created in the Argo CD UI receives a sibling file such as `application-paperless.yaml`
and an entry in the root's `resources:` list.

```text
bootstrap/argocd-applications/
  kustomization.yaml
  application-jellyfin.yaml
  application-nextcloud.yaml
```

The folder is deliberately narrow. It contains the Argo CD declarations that tell Argo what to
deploy; it does not contain the Deployments, Services, or chart-rendered objects that Argo creates.

## Proposed configuration

[`config/gittarget.yaml`](config/gittarget.yaml) chooses a declared `Kustomize` layout and an exact
single source namespace. [`config/watchrule.yaml`](config/watchrule.yaml) subscribes only to
`argoproj.io` `Application` objects. The Kustomize root supplies the source namespace, so the
Application files omit `metadata.namespace`.

The scenario makes the write boundary easy to inspect: every new captured application has one
writable home in `bootstrap/argocd-applications`. It cannot land in the repository's application
source directories or in an Argo-generated resource.

## Scenario contract

- Starting repository: [`repository/`](repository/) with the two application declarations shown.
- Live input: [`input/application-paperless.yaml`](input/application-paperless.yaml).
- Expected Git change:
  [`expected-application-paperless.patch`](expected-application-paperless.patch).
- Expected status: `Ready=True` after the root renders the added Application.
- Boundary: only `Application` declarations in `argocd` are eligible. An Argo-created workload has
  no writable home in this target.

## Argo CD behavior

For a field that both the Argo CD UI and Git can change, the Application's automated sync has
`selfHeal: false`. The Git host also sends a push webhook to Argo CD so a commit is reconciled back
to the cluster. These are the two settings that let one declaration have a live editing path and a
Git reconciliation path; see
[Argo CD and bi-directional GitOps](../../support-boundary/argocd-bi-directional.md).

This is a declaration-editing scenario. It does not reverse Argo-generated application resources,
nor does it reverse a Helm chart rendered by an Application.
