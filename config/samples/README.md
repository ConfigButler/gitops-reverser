# Samples

These samples are quick starting points for common GitOps Reverser setups.

- `quickstart-gitprovider.yaml`: Minimal `GitProvider` with credentials.
- `quickstart-gitbranch.yaml`: Minimal `GitBranch`: the `main` branch of `example-provider`'s
  repository.
- `quickstart-gittarget.yaml`: Minimal `GitTarget` using a non-root `spec.path` and SOPS encryption auto-generation.
- `quickstart-watchrule.yaml`: Minimal `WatchRule` for ConfigMaps.
- `gitreceiver.yaml`: A `GitReceiver` a Git host's push webhook points at, with its token Secret in
  its own namespace. It wakes every `GitProvider` whose `spec.url` a delivery names.
- `kubeprovider.yaml`: A `KubeProvider` for a remote cluster, with its kubeconfig Secret in its own
  namespace, showing `accessFrom` and `allowCrossNamespace`.
- `clusterwatchrule.yaml`: Minimal `ClusterWatchRule` for cluster-scoped resources.
- `fluxclaim.yaml`, `argocdclaim.yaml`: An applier claim holding the Flux `Kustomization` or
  the Argo CD `Application` that applies `example-target`'s folder, for its `spec.applierClaimRef`.
- `commitrequest.yaml`: Minimal `CommitRequest` — an on-demand "save" signal that finalizes a
  `GitTarget`'s open commit window. It uses `metadata.generateName`, so apply it with
  `kubectl create -f`: `kubectl apply` refuses a generated name.

Every sample references `example-target` / `example-branch` / `example-provider`, the same names the chart's
`quickstart` values use, so the set is internally consistent and can be applied together. The
`GitTarget` and the applier claims mirror through `local` in `gitops-reverser`, the `KubeProvider` the chart
renders in its release namespace.
