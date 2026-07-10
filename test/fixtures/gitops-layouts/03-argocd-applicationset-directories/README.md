# 03-argocd-applicationset-directories

## What this is
An Argo CD `ApplicationSet` using the **git directory generator**: instead of
writing one `Application` per service, you write one `ApplicationSet` whose git
generator scans the repository for directories matching a glob (`apps/*`) and
templates one `Application` per matching directory. Adding a new service becomes
"create a new folder under `apps/`"; the generator notices the directory and
produces an Application for it. This is the standard way large monorepos manage
dozens or hundreds of apps without hand-maintaining an Application per app. The
Application's name and destination namespace are derived from the directory
basename via Go templating (`{{.path.basename}}`).

## Layout
```
03-argocd-applicationset-directories/
├── README.md
├── bootstrap/
│   └── applicationset.yaml        # ApplicationSet with a git directory generator
└── apps/
    ├── frontend/                  # matched -> Application "frontend"
    │   ├── deployment.yaml
    │   └── service.yaml
    ├── backend/                   # matched -> Application "backend"
    │   ├── deployment.yaml
    │   └── service.yaml
    ├── worker/                    # matched -> Application "worker"
    │   └── deployment.yaml
    ├── disabled-example/          # matched by apps/* but explicitly excluded
    │   └── README.md              # no Kubernetes manifests at all
    ├── empty/                     # matched by apps/* but contains no manifests
    │   └── .gitkeep               # placeholder so git keeps the empty directory
    └── platform/                  # matched -> Application "platform"
        └── monitoring/            # NOT matched: two levels below apps/
            ├── deployment.yaml
            └── service.yaml
```

## What makes it structurally distinct
- **Wildcard directory selection.** `apps/*` matches every immediate
  subdirectory of `apps/`. The set of Applications is not written down anywhere —
  it is *computed* from the directory listing at sync time.
- **Explicit exclusion.** `apps/disabled-example` is matched by `apps/*` but an
  `exclude: true` entry removes it, so no Application is generated. Being present
  under the wildcard is necessary but not sufficient.
- **An empty directory still matches.** `apps/empty` matches `apps/*`, so the
  generator would create an Application named `empty` — pointing at a directory
  with nothing to apply. `.gitkeep` is a non-manifest file that exists only so
  git preserves the otherwise-empty directory.
- **A matched directory with no manifests.** `apps/disabled-example` contains
  only a `README.md` (not KRM). Even without the exclusion, there would be no
  Kubernetes objects to deploy there.
- **A nested directory the glob does NOT match.** `apps/platform/monitoring`
  holds real manifests but sits two levels below `apps/`, so `apps/*` skips it.
  Meanwhile `apps/platform` *is* matched, yielding an Application named
  `platform` whose `path` is `apps/platform` — a directory whose only real
  content lives one level deeper. Depth of the glob silently decides what is and
  is not a deployable unit.
- **The directory name becomes the Application name.** `{{.path.basename}}`
  turns the folder name into the Application `metadata.name` and the destination
  namespace. Renaming a folder renames (and can recreate) an Application.

## Open questions
- If the list of Applications is derived from a directory listing, is *creating a
  new folder* under `apps/` itself a deployment operation — even before any
  manifest is committed into it?
- `apps/empty` matches and would generate an Application, but there is nothing to
  apply. What is the meaning of an Application whose source directory is empty?
- `apps/platform` is matched but its real workloads live in
  `apps/platform/monitoring`, which the glob excludes. How would a reader know,
  from the layout alone, which directories are intended as deployable units?
- The directory basename becomes the Application name and namespace. What happens
  to the running Application if the folder is renamed or moved?
- `.gitkeep` and `disabled-example/README.md` are files that are not Kubernetes
  objects. How does a tool distinguish "directory that should yield an app" from
  "directory that only exists as scaffolding"?
- Nothing inside `apps/frontend/` references the ApplicationSet that generates
  its Application. Starting from a workload directory, how would you discover that
  a generator — not a static Application — owns it?
