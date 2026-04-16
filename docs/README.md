# Documentation

This repository has two kinds of markdown:

- stable user/operator guides
- maintainer notes, design docs, and working plans

If you only want the supported product docs, start with the files below.

## Start here

- [`../README.md`](../README.md): product overview and end-to-end quick start
- [`configuration.md`](configuration.md): chart quickstart values and starter resources
- [`github-setup-guide.md`](github-setup-guide.md): GitHub repository and credential setup
- [`sops-age-guide.md`](sops-age-guide.md): Secret encryption with SOPS + age
- [`bi-directional.md`](bi-directional.md): safe shared-path and handoff patterns
- [`alternatives.md`](alternatives.md): nearby tools and when another approach fits better
- [`../CONTRIBUTING.md`](../CONTRIBUTING.md): contributor workflow and validation commands
- [`../test/e2e/E2E_DEBUGGING.md`](../test/e2e/E2E_DEBUGGING.md): e2e troubleshooting

## Maintainer notes

These directories are useful when changing internals, but they are not the primary user-facing
docs surface:

- [`design/`](design/): architecture notes, investigations, and implementation plans
- [`ci/`](ci/): CI/devcontainer rationale and troubleshooting
- [`future/`](future/): ideas that are intentionally deferred
- [`runbooks/`](runbooks/): operational troubleshooting notes

These root-level files are also working notes rather than polished user docs:

- [`TODO.md`](TODO.md)
- [`next-steps.md`](next-steps.md)
- `*plan*.md`
- `*analysis*.md`

If you are cleaning up documentation, that maintainer-notes layer is usually the first place to
trim, archive, or merge.
