# 02-argocd-app-of-apps

## What this is
The classic Argo CD bootstrap pattern, known as "app of apps". A single root
`Application` does not point at workloads at all — it points at a directory that
contains *other* `Application` manifests. Argo CD syncs the root, which creates
the child Applications, and each child then syncs its own workloads. Teams use
this to bootstrap an entire cluster from one hand-applied resource: apply
`root`, and everything else follows. It predates `ApplicationSet` and is still
widely used because it needs no generator — child Applications are just static
YAML committed to the repo.

## Layout
```
02-argocd-app-of-apps/
├── README.md
├── bootstrap/
│   └── root.yaml                  # root Application -> path: applications
├── applications/
│   ├── frontend.yaml              # child Application -> manifests/frontend
│   └── backend.yaml               # child Application -> manifests/backend
└── manifests/
    ├── frontend/
    │   ├── application.yaml        # TRAP: an ordinary Deployment named frontend
    │   └── service.yaml
    └── backend/
        ├── deployment.yaml
        └── service.yaml
```

## What makes it structurally distinct
- **Three distinct roles collide in one repo:**
  1. `bootstrap/root.yaml` and `applications/*.yaml` are Argo CD `Application`
     resources — control-plane objects that *describe where deployments come
     from*.
  2. `manifests/frontend/` and `manifests/backend/` hold the actual workloads
     those Applications *reference*.
  3. `manifests/frontend/application.yaml` is an ordinary Kubernetes
     `Deployment` that merely *happens to be named* `application.yaml`.
- **The filename trap is explicit.** `manifests/frontend/application.yaml` has
  `kind: Deployment`. A tool keying off the filename `application.yaml` would
  misclassify it as an Argo CD Application. Only `apiVersion`/`kind` disambiguate.
- **Directory names are conventions, not contracts.** The directory is called
  `applications/` because a human put Applications there, and `manifests/`
  because a human put workloads there — but nothing enforces this. A Deployment
  sits inside `manifests/` under a file named like a control-plane object.
- **Recursion + automation.** `root.yaml` uses `directory.recurse: true` over
  `applications/`, so any Application YAML added to that tree is picked up
  automatically, and `syncPolicy.automated` means adding a file can create real
  cluster objects with no further human step.
- **Same object names at different layers.** There is an `Application` named
  `frontend` (in `applications/`) and a `Deployment` named `frontend` (in
  `manifests/frontend/`). Name alone does not tell you which layer you are in.

## Open questions
- If directory names (`applications/`, `manifests/`) are only conventions, can
  the location of a file be trusted to tell you what role it plays?
- `manifests/frontend/application.yaml` is a Deployment. What signal, other than
  parsing `kind`, could reliably separate a control-plane Application from a
  workload with the same filename?
- The root Application's `directory.recurse: true` means a newly committed
  Application file becomes a live child Application. Is committing that file a
  configuration change, or a deployment action?
- Two objects are both named `frontend` at different layers. When something
  refers to "frontend", how do you know whether it means the Application or the
  Deployment?
- Nothing in `manifests/frontend/` states which Application references it — the
  link lives only in `applications/frontend.yaml`. If you start from the
  workload directory, how do you find its owning Application?
