# 05-kustomize-overlays

## What this is
The most recognizable general-purpose GitOps layout: per-app `base/` +
`overlays/<env>/`. A `base` holds environment-agnostic Kubernetes objects, and
each overlay layers environment-specific changes on top with a
`kustomization.yaml`. This is the default structure produced and consumed by
`kustomize build` and by Flux `Kustomization` / Argo CD kustomize sources. The
`frontend` app is fully self-contained in this repo; the `backend` production
overlay instead builds on a **remote** base fetched from another repository.

## Layout
```
05-kustomize-overlays/
├── README.md
└── apps/
    ├── frontend/
    │   ├── base/
    │   │   ├── deployment.yaml          # KRM: Deployment
    │   │   ├── service.yaml             # KRM: Service
    │   │   └── kustomization.yaml       # kustomize config (not a cluster obj)
    │   └── overlays/
    │       ├── staging/
    │       │   ├── kustomization.yaml   # namespace, namePrefix, replicas,
    │       │   │                        #   images(newTag), patch, cmGenerator
    │       │   ├── deployment-patch.yaml# partial object (strategic merge)
    │       │   └── config.properties    # NOT YAML - key=value env file
    │       └── production/
    │           ├── .argocd-source-frontend-production.yaml
    │           │                        # HIDDEN Argo CD override, written by
    │           │                        #   Image Updater (NOT a K8s object)
    │           ├── kustomization.yaml   # namespace, nameSuffix, replicas,
    │           │                        #   images(newName+newTag), patch,
    │           │                        #   cmGenerator, secretGenerator
    │           ├── deployment-patch.yaml# partial object (strategic merge)
    │           ├── config.properties    # NOT YAML - key=value env file
    │           └── secrets.env          # NOT YAML - fake key=value secrets
    └── backend/
        ├── base/
        │   ├── deployment.yaml          # KRM: Deployment
        │   ├── service.yaml             # KRM: Service
        │   └── kustomization.yaml       # kustomize config (not a cluster obj)
        └── overlays/
            └── production/
                └── kustomization.yaml   # uses a REMOTE base (github.com/...)
```

## What makes it structurally distinct
- **Plain desired objects vs. build inputs.** The base `deployment.yaml` and
  `service.yaml` are complete, checked-in Kubernetes objects. The
  `kustomization.yaml` files, `deployment-patch.yaml` files, `config.properties`
  and `secrets.env` are *inputs*: nothing runs them as-is; `kustomize build`
  must render the overlay first to get the objects that are actually applied.
- **`kustomization.yaml` looks like KRM but is not a cluster object.** It carries
  `apiVersion: kustomize.config.k8s.io/v1beta1` and `kind: Kustomization`, but
  the API server never stores an object of that kind — it is build config.
- **Patches are partial objects.** `deployment-patch.yaml` has `apiVersion`,
  `kind`, and a `metadata.name`, yet it is intentionally incomplete (no
  selector, no full pod template). It is only meaningful merged onto the base.
- **Generator output has a content hash, so it has no single source document.**
  `configMapGenerator` and `secretGenerator` emit a `ConfigMap`/`Secret` whose
  name gains a hash suffix (e.g. `frontend-config-abc12`) derived from
  `config.properties` / `secrets.env`. The rendered object exists only after a
  build and its name changes whenever the input file changes.
- **`config.properties` and `secrets.env` are not YAML at all** — plain
  `key=value` lines. `secrets.env` holds deliberately fake placeholder values.
- **`namePrefix` / `nameSuffix` mutate object identity.** In staging the base
  `frontend` Deployment is applied as `staging-frontend`; in production as
  `frontend-prod`. The applied object's `metadata.name` never appears verbatim
  in any base file.
- **Namespace is a transform, not a field on the base.** Base objects omit
  `metadata.namespace`; the overlay stamps `frontend-staging` /
  `frontend-production` / `backend-production` at build time.
- **A remote base lives outside the repository entirely.**
  `apps/backend/overlays/production/kustomization.yaml` references
  `github.com/example-org/gitops//apps/backend/base?ref=v1.4.0`. That base is not
  in this checkout; kustomize fetches it over the network at a pinned ref.
- **A hidden dotfile overrides the kustomization, and a controller writes it.**
  `.argocd-source-frontend-production.yaml` is an Argo CD source override scoped
  to the Application named `frontend-production`. Its `kustomize.images` entry
  beats the `images:` transformer in the sibling `kustomization.yaml`, so the
  image that actually deploys is decided by a file a reader inspecting the
  kustomization never opens. Argo CD Image Updater *writes and commits* this file
  on its own, which makes Git an output of the cluster here, exactly as Flux's
  `ImageUpdateAutomation` does in [16-flux-image-automation](../16-flux-image-automation/).
  It is not a Kubernetes object: no `apiVersion`, no `kind`.

## Open questions
- Which files could a tool safely edit in place and round-trip? The base
  objects have a stable identity, but an overlay's effective object identity is
  the product of `namePrefix`/`nameSuffix`/`namespace` — editing the rendered
  output has no obvious single home file.
- If a tool observes a running `ConfigMap` named `frontend-config-abc12`, how
  would it map that hash-suffixed name back to `config.properties`, and what
  does it write when the desired content changes (the hash then changes too)?
- Where does an image bump belong: the base `deployment.yaml` `image:` field, the
  overlay `images:` override, or the `.argocd-source-*.yaml` that outranks both?
  For production the override uses `newName`, so the running image string appears
  in *no* base file.
- If a controller commits `.argocd-source-frontend-production.yaml` on its own
  schedule, is that file safe for anything else to edit — and how should a tool
  that does not know Argo CD's precedence rules report the effective image?
- For `backend` production, the base is remote and pinned to `ref=v1.4.0`. Can a
  tool reason about, or edit, desired state it does not have checked out — and is
  bumping `ref` the only in-repo change available?
- A strategic-merge patch changes an existing object by field. If a tool wants
  to reflect a live change (e.g. a raised memory request), does it edit the base
  container spec or the overlay `deployment-patch.yaml`, and how does it decide?
- The staging and production overlays diverge in *which* constructs they use
  (staging has no Secret; production repoints the image repository). How would a
  tool present "the desired state of frontend" when each environment is a
  different transformation of the same base?
