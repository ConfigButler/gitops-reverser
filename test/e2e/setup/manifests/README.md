This directory contains the last local manifests that are applied during e2e setup after the Flux-managed shared
services are ready.

Use this for small, cluster-local resources that are still easier to keep as a final `kubectl apply` step than to model
as additional Flux sources.

Organize related resources in subdirectories when that keeps the intent clearer.

This directory is applied via the tracked [kustomization.yaml](/workspaces/gitops-reverser2/test/e2e/setup/manifests/kustomization.yaml).
This branch expects the local tunnel credentials file at
`test/e2e/setup/manifests/cloudflared-public/tunnel-credentials.yaml` to exist before `make` applies the manifests.
