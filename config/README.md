# config

This folder contains simplified raw manifests used primarily for local development and testing,
especially end-to-end (e2e) test workflows.

## Intended use
- Local cluster bring-up.
- E2E test deployments.
- Debugging and iteration with explicit manifests.

## Production guidance
For production deployments, use the Helm chart in `charts/gitops-reverser`.
The Helm chart is the recommended installation and lifecycle management path for production.

## Notes
- These manifests are opinionated toward the local/e2e setup.
- Keep them simple and explicit; avoid reintroducing heavy kustomize indirection here.
