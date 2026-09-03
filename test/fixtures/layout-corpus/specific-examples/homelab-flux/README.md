# Homelab Flux

This scenario captures Flux declarations in the `flux-system` namespace. A homelab owner can edit a
`GitRepository`, `HelmRepository`, or `HelmRelease` through the Kubernetes API and review the
resulting Git change without asking GitOps Reverser to reverse a chart.

## The rule this scenario exists to demonstrate

> A `GitTarget` must not point at a path another controller writes. Flux's bootstrap directory
> (`clusters/<name>/flux-system`) is the common case, and the `Kustomization` that reconciles a
> folder is not a licence to co-write it.

`clusters/home/flux-system` is owned by `flux bootstrap` and by flux-operator: it holds
`gotk-components.yaml`, `gotk-sync.yaml`, and a `kustomization.yaml` listing both. An operator that
adds `resources:` entries there is a second writer in a folder Flux's own sync loop reconciles,
which is the two-writers-one-folder failure the
[support contract](../../../../../docs/design/support-boundary/support-contract.md) exists to prevent. The
targets below point somewhere else, and the bootstrap directory appears in the tree only to be left
alone.

## Repository folder

[`repository/`](repository/) is rooted at the repository root, as every scenario's is. Here that
matters twice over, because this scenario has two targets on two layers:

```text
clusters/home/flux-system/       # flux bootstrap owns this. GitOps Reverser never writes here.
  gotk-components.yaml
  gotk-sync.yaml
  kustomization.yaml

infrastructure/home/sources/     # spec.path of the flux-sources GitTarget
  kustomization.yaml
  gitrepository-homelab.yaml
  helmrepository-jellyfin.yaml

apps/home/media/                 # spec.path of the flux-media GitTarget
  kustomization.yaml
  helmrelease-jellyfin.yaml
```

The bootstrap directory is shown but not committed to this fixture, because nothing in the scenario
reads or writes it. The split into `infrastructure/` and `apps/` is the shape Flux's documented
repository structures produce, and keeping the cluster layer separate from the application layer is
what those structures exist to achieve.

For a newly created Flux declaration, the folder's kustomize root places a sibling file and
registers it in `resources:`. It does not append the declaration to an existing bundle, because a
bundle is only produced by a template that deliberately collides two identities on one path, and
none is declared here.

**The update case is the more interesting half.** When the `jellyfin` `HelmRepository` that already
lives in the folder changes, it is not moved and not re-placed: match-first identity finds the
document where it is and edits it in place, including when it shares a file with other documents.
So "we do not author bundles" and "we do not move existing files" coexist — placement decides where
a *new* document goes and has no opinion about one that already exists. A user who consolidates two
declarations into one file keeps that choice; the operator will edit inside it forever.

## Proposed configuration

Two targets, one per layer:

| Target | Path | Watches |
|---|---|---|
| [`config/gittarget.yaml`](config/gittarget.yaml) | `infrastructure/home/sources` | `GitRepository`, `HelmRepository` ([`config/watchrule.yaml`](config/watchrule.yaml)) |
| [`config/gittarget-media.yaml`](config/gittarget-media.yaml) | `apps/home/media` | `HelmRelease` ([`config/watchrule-media.yaml`](config/watchrule-media.yaml)) |

Both declare a one-root Kustomize folder with one source namespace. Each folder's
`namespace: flux-system` transformer supplies the namespace for every document in it, so the
committed documents omit `metadata.namespace`.

The `HelmRelease` object living in `flux-system` does not put jellyfin there:
[`helmrelease-jellyfin.yaml`](repository/apps/home/media/helmrelease-jellyfin.yaml) carries
`targetNamespace: media` and `storageNamespace: flux-system`. The declaration and the release it
installs are on different sides of that field, which is exactly why the declaration is the editable
surface and the release is not.

## What the input fixture is for

[`input/bitnami.yaml`](input/bitnami.yaml) is the object as the
operator receives it from the API server, not the document it wants in Git. It carries `uid`,
`resourceVersion`, `generation`, `creationTimestamp`, `managedFields`, a populated `status`, and
Flux's `finalizers.fluxcd.io` finalizer.

None of that reaches
[`expected-bitnami.patch`](expected-bitnami.patch), and the difference
between the two files *is* the sanitization assertion. The finalizer is the one worth naming:
`finalizers.fluxcd.io` is how source-controller keeps the object alive long enough to garbage-collect
its artifact storage. A committed copy would make Git assert a finalizer against an object Flux has
not adopted yet, so deleting a recreated object would block on a controller that never put it there.

## Scenario contract

- Starting repository: [`repository/`](repository/) with its existing sources and media folders.
- Live input: [`input/bitnami.yaml`](input/bitnami.yaml).
- First observation, before any commit:

  ```yaml
  status:
    conditions:
      - type: LayoutResolved
        status: "True"
        reason: SingleKustomization
        message: "render root '.' governs new files"
    placement:
      mode: KustomizeRoot
      renderRoot: .
  ```

- Expected Git change: [`expected-bitnami.patch`](expected-bitnami.patch).
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
[support contract](../../../../../docs/design/support-boundary/support-contract.md) owns that boundary.

A free-standing values file is a separate planned projection, so it is absent from this first
scenario. Inline `HelmRelease.spec.values` are KRM and stay inside the declaration surface.
