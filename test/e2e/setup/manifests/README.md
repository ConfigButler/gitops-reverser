# E2E Local Manifests

This directory contains the last local manifests that are applied during e2e setup after the
Flux-managed shared services are ready.

Use this for small, cluster-local resources that are still easier to keep as a final `kubectl apply` step than to model
as additional Flux sources.

The aggregated API e2e stack uses the Kubernetes sample-apiserver ("Wardle") manifests in
`sample-apiserver/`. It intentionally does not install the retired aggregated API audit proxy; the
tests exercise ordinary aggregated API list/watch behavior.

Organize related resources in subdirectories when that keeps the intent clearer.

This directory is applied via the tracked [kustomization.yaml](kustomization.yaml).

Keep this directory CI-safe. Demo-only resources that require local secrets or extra CRD ordering belong under
`test/e2e/setup/demo-only` and are applied only by `task test-e2e-demo` / `task prepare-e2e-demo`.
