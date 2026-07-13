# argocd-multicluster-matrix

## What this is
An Argo CD `ApplicationSet` using a **matrix generator** to produce one Application
per (cluster x app) pair. This is the standard way large Argo CD installations fan a
small set of app directories out across a fleet of clusters: a `clusters` generator
enumerates the registered cluster Secrets, a `git` generator enumerates the app
directories under `apps/*`, and the matrix generator takes their Cartesian product.
The template then stamps out an Application for every combination.

The set of rendered Applications is therefore a **product, not a sum**: it is
`(number of selected clusters) x (number of app directories)`, not
`(clusters) + (apps)`.

## Layout
```
12-multicluster-applicationset/
├── README.md                       # this file
├── applicationsets/
│   └── cluster-matrix.yaml         # ApplicationSet with a matrix(clusters x git) generator
├── clusters/
│   ├── eu-production.yaml          # Argo CD cluster Secret (a destination, not a workload)
│   ├── us-production.yaml          # Argo CD cluster Secret
│   └── staging.yaml                # Argo CD cluster Secret (NOT selected by the matrix)
├── apps/
│   ├── frontend/
│   │   ├── deployment.yaml         # rendered once per selected cluster
│   │   └── service.yaml
│   └── backend/
│       ├── deployment.yaml
│       └── service.yaml
└── values/
    ├── eu-production/
    │   ├── frontend.yaml           # NOT KRM — per-cluster values
    │   └── backend.yaml            # NOT KRM
    ├── us-production/
    │   └── frontend.yaml           # NOT KRM — backend values intentionally absent
    └── staging/
        └── frontend.yaml           # NOT KRM — backend values intentionally absent
```

## What makes it structurally distinct
- **The same app directory renders once per cluster.** `apps/frontend/` and
  `apps/backend/` each exist exactly once on disk, but the matrix generator emits one
  Application per selected cluster for each of them. The on-disk file count does not
  reflect the number of live Applications.
- **The number of Applications is a product, not a sum.** With the two
  `environment: production` clusters selected and two app directories, the matrix
  yields `2 x 2 = 4` Applications, named `{{.name}}-{{.path.basename}}` (for example
  `eu-production-frontend`, `us-production-backend`).
- **A cluster Secret is a Kubernetes object that configures a *destination*, not a
  workload.** The files under `clusters/` are real `v1` Secrets in the `argocd`
  namespace carrying the `argocd.argoproj.io/secret-type: cluster` label; they tell
  Argo CD where to deploy, they are not themselves anything that runs in a cluster.
  Their bearer tokens are deliberately fake.
- **The clusters generator selects on the `environment` label.** The selector matches
  `environment: production`, so `staging.yaml` is a registered cluster Secret that the
  matrix does **not** select — it participates in nothing here.
- **The values matrix is sparse.** `values/eu-production/` has both `frontend.yaml`
  and `backend.yaml`, but `values/us-production/` and `values/staging/` have only
  `frontend.yaml`. The per-cluster override grid has holes in it.
- The files under `values/` are **NOT Kubernetes objects**: they are plain YAML value
  documents (no `apiVersion`, no `kind`) consumed out-of-band by a config-management
  plugin or Helm, not applied to any cluster directly.

## Open questions
- What is "the desired state of `apps/frontend`" when that one directory is deployed
  to multiple clusters, each with different per-cluster values and a different image
  tag or replica count?
- When an edit is made to `apps/frontend/deployment.yaml`, it changes every rendered
  copy at once — but which of the N live Applications was the edit actually meant for?
- Conversely, if an edit is meant for exactly one cluster (say only `eu-production`),
  does it belong in the shared `apps/frontend/` directory at all, or only in that
  cluster's `values/` file — and how would a reader tell those two intents apart?
- The `staging` cluster Secret is registered but not selected by the matrix, and
  `values/staging/frontend.yaml` exists for it. What state, if any, does that
  orphaned pairing describe?
- Where two clusters share the same app directory but differ only in a sparse
  `values/` override, which is authoritative when they disagree — the shared manifest
  or the per-cluster value?
