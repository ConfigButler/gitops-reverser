# Samples

These samples are quick starting points for common GitOps Reverser setups.

- `quickstart-gitprovider.yaml`: Minimal `GitProvider` with credentials.
- `quickstart-gittarget.yaml`: Minimal `GitTarget` using a non-root `spec.path` and SOPS encryption auto-generation.
- `quickstart-watchrule.yaml`: Minimal `WatchRule` for ConfigMaps.
- `clusterwatchrule.yaml`: Minimal `ClusterWatchRule` for cluster-scoped resources.
- `commitrequest.yaml`: Minimal `CommitRequest` — an on-demand "save" signal that finalizes a `GitTarget`'s open commit window.
