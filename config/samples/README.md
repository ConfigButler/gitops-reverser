# Samples

These samples are quick starting points for common GitOps Reverser setups.

- `quickstart-gitprovider.yaml`: Minimal `GitProvider` with credentials.
- `quickstart-gittarget.yaml`: Minimal `GitTarget` using a non-root `spec.path` and SOPS encryption auto-generation.
- `quickstart-watchrule.yaml`: Minimal `WatchRule` for ConfigMaps.
- `clusterprovider.yaml`: The conventional `default` provider (in-cluster) and a remote one, showing
  `accessFrom` and `allowAnySourceNamespace`.
- `clusterwatchrule.yaml`: Minimal `ClusterWatchRule` for cluster-scoped resources.
- `commitrequest.yaml`: Minimal `CommitRequest` — an on-demand "save" signal that finalizes a
  `GitTarget`'s open commit window. It uses `metadata.generateName`, so apply it with
  `kubectl create -f`: `kubectl apply` refuses a generated name.

Every sample references `example-target` / `example-provider`, the same names the chart's
`quickstart` values use, so the set is internally consistent and can be applied together.
