# repo-per-environment

## What this is
A layout where the environment boundary is a **repository** boundary rather than a
directory boundary. Instead of one repo with `dev/`, `staging/`, and `production/`
folders, each environment gets its own independent Git repository, frequently with
its own access control (e.g. only the SRE team can push to the production repo).
This is common in regulated organisations and in setups where promotion is a
cross-repo pull request or a mirror/sync job rather than a merge inside one repo.

Because the three repositories are genuinely separate, the same application path
(`apps/frontend/`) exists three times, in three repos, with **no shared base in Git**.
There is no `base/` or common overlay anywhere; each repo carries a full, standalone
copy of every manifest.

## Layout
```
11-repo-per-environment/
├── README.md                 # this file — explains the simulation
├── gitops-dev/               # stands in for a whole repository
│   ├── .gitignore
│   ├── apps/
│   │   ├── frontend/
│   │   │   ├── deployment.yaml
│   │   │   └── service.yaml
│   │   └── backend/
│   │       ├── deployment.yaml
│   │       └── service.yaml
│   └── namespaces.yaml
├── gitops-staging/           # stands in for a second, separate repository
│   ├── .gitignore
│   ├── apps/
│   │   ├── frontend/
│   │   │   ├── deployment.yaml
│   │   │   └── service.yaml
│   │   └── backend/
│   │       ├── deployment.yaml
│   │       └── service.yaml
│   └── namespaces.yaml
└── gitops-production/        # stands in for a third, separate repository
    ├── .gitignore
    ├── apps/
    │   ├── frontend/
    │   │   ├── deployment.yaml
    │   │   ├── service.yaml
    │   │   └── hpa.yaml       # production-only object, no counterpart elsewhere
    │   └── backend/
    │       ├── deployment.yaml
    │       └── service.yaml
    └── namespaces.yaml
```

## What makes it structurally distinct
- **The three top-level directories stand in for three independent Git repositories.**
  They are nested inside this one folder only for convenience in this corpus. In the
  real world you would `git clone` each of `gitops-dev`, `gitops-staging`, and
  `gitops-production` separately; they would have unrelated commit histories and
  potentially different owners and permissions.
- Each simulated repo carries its own `.gitignore` at its root so that it reads as a
  repository root rather than as a subdirectory of a larger repo. The `.gitignore`
  files are the only files here that are **not** Kubernetes objects.
- The three repos are **structurally identical but textually divergent**: the same
  file paths in each repo, but different image tags (`frontend:1.8.1` in dev,
  `1.8.2` in staging, `1.8.3` in production; `backend:2.3.8` / `2.3.9` / `2.4.0`),
  different `replicas`, different resource requests, and different namespace names
  (`frontend-dev` / `frontend-staging` / `frontend-production`).
- `gitops-production` additionally contains an `autoscaling/v2` HorizontalPodAutoscaler
  (`apps/frontend/hpa.yaml`) with **no counterpart** in dev or staging. This is drift
  that is intentional, not accidental.
- There is no shared base, kustomization, or common overlay anywhere in the corpus.
  Every manifest is a complete standalone copy.

## Open questions
- What does "promotion" mean here when moving a change from dev to staging to
  production crosses a repository boundary rather than a directory boundary?
- With no shared base to edit, where does an operator make a change that is meant to
  apply identically to a single environment — and how is that change kept consistent
  with the two other repos it was copied from?
- If an edit should apply to **all three** environments at once (say, a new
  security-context field on `frontend`), which repo is the source of truth, and how is
  the same edit fanned out to three repos with three separate histories?
- Is the production-only HorizontalPodAutoscaler considered part of the desired state
  of "frontend", or an environment-specific addition that intentionally has no peer?
- When the same logical object exists three times with divergent field values, which
  copy — if any — is authoritative for a question like "what is frontend's replica
  count"?
