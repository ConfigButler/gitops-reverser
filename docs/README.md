# Documentation

This repository has two kinds of markdown:

- stable user/operator guides
- maintainer notes, design docs, and working plans

If you only want the supported product docs, start with the files below.

## Start here

- [`../README.md`](../README.md): product overview and end-to-end quick start
- [`configuration.md`](configuration.md): core configuration objects and how they fit together
- [`installing-apps-as-krm.md`](installing-apps-as-krm.md): installing an app is adding a KRM
  document — Flux `HelmRelease`, Argo CD `Application`, KRO, and core resources all mirror and edit alike
- [`commit-signing.md`](commit-signing.md): how valid Git signatures map to platform verification
- [`github-setup-guide.md`](github-setup-guide.md): GitHub repository and credential setup
- [`attribution-setup-guide.md`](attribution-setup-guide.md): naming real Kubernetes users as commit
  authors via kube-apiserver audit delivery
- [`sops-age-guide.md`](sops-age-guide.md): Secret encryption with SOPS + age
- [`security-model.md`](security-model.md): controller access, trust boundaries, and the Git
  credentials Secret shape
- [`rbac.md`](rbac.md): the two ClusterRoles, and how to run with zero cluster-wide Secret access
- [`bi-directional.md`](bi-directional.md): safe shared-path and handoff patterns
- [`alternatives.md`](alternatives.md): nearby tools and when another approach fits better
- [`UPGRADING.md`](UPGRADING.md): breaking changes and migration steps, newest first
- [`../CONTRIBUTING.md`](../CONTRIBUTING.md): contributor workflow and validation commands
- [`../test/e2e/E2E_DEBUGGING.md`](../test/e2e/E2E_DEBUGGING.md): e2e troubleshooting, reuse,
  and `.stamps`

## Maintainer notes

These directories are useful when changing internals, but they are not the primary user-facing
docs surface:

- [`design/`](design/): architecture notes, investigations, and implementation plans
- [`finished/`](finished/): design docs and reports for work that has shipped or been resolved,
  kept for historical context
- [`ci/`](ci/): CI/devcontainer rationale and troubleshooting
- [`future/`](future/): ideas that are intentionally deferred
- [`audit-setup/`](audit-setup/): cluster-specific audit delivery notes and examples

These root-level files are also working notes rather than polished user docs:

- [`TODO.md`](TODO.md)
- `*plan*.md`
- `*analysis*.md`

If you are cleaning up documentation, that maintainer-notes layer is usually the first place to
trim, archive, or merge.
