# 01-argocd-plain

## What this is
The simplest Argo CD pattern: a single `Application` that points at a plain
directory of raw Kubernetes manifests in a Git repository. There is no Helm,
no Kustomize, no templating — Argo CD just reads every YAML file under the
`path` and applies it. This is the first layout most teams adopt when they move
from `kubectl apply -f` to GitOps, and it remains common for small services and
for bootstrapping. The `Application` resource itself lives in the `argocd/`
directory and is typically applied out-of-band (by hand or by a bootstrap job),
while the workload manifests live under `apps/frontend/`.

## Layout
```
01-argocd-plain/
├── README.md
├── argocd/
│   └── frontend-application.yaml   # Argo CD Application (control-plane object)
└── apps/
    └── frontend/
        ├── namespace.yaml          # cluster-scoped object
        ├── deployment.yaml
        ├── service.yaml
        ├── ingress.yaml
        ├── rbac-and-config.yaml    # multi-doc: ServiceAccount + ConfigMap
        └── ci-metadata.yaml        # YAML but NOT a Kubernetes object
```

## What makes it structurally distinct
- **Two roles of YAML in one tree.** `argocd/frontend-application.yaml` is an
  Argo CD control-plane object that *describes where to sync from*; the files
  under `apps/frontend/` are the *workloads it syncs*. They are not
  interchangeable even though both are valid YAML.
- **Mixed scope in one directory.** `namespace.yaml` is cluster-scoped (no
  `metadata.namespace`); every other object is namespaced. A reader cannot
  assume every file in an app directory targets the same namespace, or any
  namespace at all.
- **Arbitrary filenames.** `rbac-and-config.yaml` is named for what a human
  thinks it contains, not for a `kind`. Filenames carry no guaranteed relation
  to the objects inside — the only source of truth is `apiVersion`/`kind`.
- **Multi-document files.** `rbac-and-config.yaml` holds two objects
  (`ServiceAccount` + `ConfigMap`) separated by `---`. One file does not mean
  one object.
- **Non-manifest YAML in a manifest directory.** `ci-metadata.yaml` is build
  metadata (`pipeline`, `commit`, `builtAt`) with no `apiVersion`/`kind`. It is
  YAML sitting beside manifests but is not KRM and must not be applied.
- **`directory.include` / `directory.exclude` change the file set.** The
  Application uses `include: '*.yaml'` and `exclude: 'ci-metadata.yaml'`, so the
  *effective* set of applied files is not simply "every file under path" — it is
  filtered by globs declared elsewhere (in the Application), plus `recurse:
  true` decides whether subdirectories are scanned at all.

## Open questions
- Given only the files under `apps/frontend/`, how would a tool decide which are
  Kubernetes objects and which (like `ci-metadata.yaml`) are not?
- The include/exclude globs live in the `Application`, not next to the
  manifests. If you only have the manifest directory, can you know which files
  Argo CD would actually apply?
- Does `recurse: true` here matter when the directory is currently flat, and how
  would adding a subdirectory later change what gets applied?
- If a filename (`rbac-and-config.yaml`) does not name a `kind`, and a file can
  hold many kinds, what can a filename be trusted to tell you at all?
- `namespace.yaml` creates the very namespace the other objects target — does
  apply ordering within a plain directory guarantee it exists first?
