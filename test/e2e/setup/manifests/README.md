This directory contains the last local manifests that are applied during e2e setup after the Flux-managed shared
services are ready.

Use this for small, cluster-local resources that are still easier to keep as a final `kubectl apply` step than to model
as additional Flux sources.

Organize related resources in subdirectories when that keeps the intent clearer.
