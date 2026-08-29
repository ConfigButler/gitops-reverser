# Homelab Argo CD

This scenario captures a homelab's Argo CD `Application` declarations. The Git target owns one
app-of-apps folder, and the watched objects all live in Argo CD's `argocd` namespace.

## Repository folder

[`repository/`](repository/) contains one Kustomize root and two `Application` documents. A new
Application created in the Argo CD UI receives a sibling file such as `paperless.yaml`
and an entry in the root's `resources:` list.

The tree below shows the target's path in the real repository;
[`repository/`](repository/) in this folder **is** `bootstrap/argocd-applications/`.

```text
bootstrap/argocd-applications/
  kustomization.yaml
  application-jellyfin.yaml
  application-nextcloud.yaml
```

The folder is deliberately narrow. It contains the Argo CD declarations that tell Argo what to
deploy; it does not contain the Deployments, Services, or chart-rendered objects that Argo creates.

## Proposed configuration

[`config/gittarget.yaml`](config/gittarget.yaml) pairs `serializeNamespace: Never` with
`kustomizeRoot: Require`, and constrains the source to one exact namespace.
[`config/watchrule.yaml`](config/watchrule.yaml) subscribes only to `argoproj.io` `Application`
objects. The folder's kustomize root supplies the namespace, so the Application files omit
`metadata.namespace`.

**`Require` is doing real work here.** The root that makes the omission safe is
[`repository/kustomization.yaml`](repository/kustomization.yaml) — a file the *repository owner*
controls, not the operator. Delete it, or delete its `namespace: argocd` line, and every subsequent
document lands in whatever namespace the applier happens to be pointed at, which is a different
object with the same name. `Require` turns that into a refusal with a condition instead of a silent
relocation. `Adopt` would keep writing.

The scenario makes the write boundary easy to inspect: every new captured application has one
writable home in `bootstrap/argocd-applications`. It cannot land in the repository's application
source directories or in an Argo-generated resource.

## What the input fixture is for

[`input/paperless.yaml`](input/paperless.yaml) is the object as the
operator receives it from the API server, not the document it wants in Git. It carries `uid`,
`resourceVersion`, `generation`, `creationTimestamp`, `managedFields`, Argo's
`resources-finalizer.argocd.argoproj.io` finalizer, a populated `status`, and the
`argocd.argoproj.io/tracking-id` annotation.

None of that appears in [`expected-paperless.patch`](expected-paperless.patch),
and the difference between the two files *is* the sanitization assertion. The tracking-id is the one
worth naming: it identifies the Argo CD Application that owns the live object, so a committed copy
makes the document claim ownership on behalf of another Application and hard-fails that
Application's sync. It is denied by exact key rather than by an `argocd.argoproj.io/` prefix strip,
because the rest of that prefix is user data. See
[the tracking-id landmine](../../../spec/e2e-bi-directional-corner.md#the-tracking-id-landmine).

## Scenario contract

- Starting repository: [`repository/`](repository/) with the two application declarations shown.
- Live input: [`input/paperless.yaml`](input/paperless.yaml).
- First observation, before any commit:

  ```yaml
  status:
    conditions:
      - type: LayoutResolved
        status: "True"
        reason: SingleKustomization
        message: "render root '.' governs new files"
    placement:
      renderRoot: .
      serializeNamespace: Never
  ```

- Expected Git change:
  [`expected-paperless.patch`](expected-paperless.patch), after a reviewer
  clears `suspend`.
- Expected status: `Ready=True` after the root renders the added Application.
- Boundary: only `Application` declarations in `argocd` are eligible. An Argo-created workload has
  no writable home in this target.

## Argo CD behavior

For a field that both the Argo CD UI and Git can change, the Application's automated sync has
`selfHeal: false`. The Git host also sends a push webhook to Argo CD so a commit is reconciled back
to the cluster. These are the two settings that let one declaration have a live editing path and a
Git reconciliation path; see
[Argo CD and bi-directional GitOps](../../../design/support-boundary/argocd-bi-directional.md).

This is a declaration-editing scenario. It does not reverse Argo-generated application resources,
nor does it reverse a Helm chart rendered by an Application.
