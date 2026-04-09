# Configuration Guide

This guide covers the chart's starter configuration after the main install from the
[root README](../README.md).

## Recommended Path

For first-time setup, use the Helm chart's optional `quickstart` block. It creates:

- a starter `GitProvider`
- a starter `GitTarget`
- a starter `WatchRule`

That keeps the quickstart on one path instead of asking operators to hand-apply separate sample YAML files.

## Minimal Quickstart Values

Create a small values file like this:

```yaml
quickstart:
  enabled: true
  namespace: default
  gitProvider:
    url: git@github.com:<org>/<repo>.git
    secretRef:
      name: git-creds
  gitTarget:
    path: live-cluster
  watchRule:
    rules:
      - operations: [CREATE, UPDATE, DELETE]
        apiGroups: [""]
        apiVersions: ["v1"]
        resources: ["configmaps"]
```

Apply it with:

```bash
helm upgrade gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser \
  --reuse-values \
  --values quickstart-values.yaml
```

## Defaults

If you only set `quickstart.enabled=true` plus `quickstart.gitProvider.url`, the chart uses:

- namespace: `default`
- `GitProvider`: `example-provider`
- `GitTarget`: `example-target`
- `WatchRule`: `example-watchrule`
- Git credentials Secret: `git-creds`
- target branch: `main`
- target path: `live-cluster`
- encryption: SOPS with an auto-generated age key Secret `sops-age-key`

## Verify

Check the starter resources:

```bash
kubectl get gitprovider,gittarget,watchrule -n default
```

You should see them settle to `Ready=True` once:

- the Git credentials Secret exists
- the repository URL is correct
- the operator can connect and push

Then create a test object:

```bash
kubectl create configmap test-config --from-literal=key=value -n default
```

That should produce a commit in the configured repository within seconds.

## When To Customize Further

Use chart values when you want to change:

- starter resource names
- namespace
- branch or repository path
- Git credentials Secret name
- watched resources and operations

For more advanced setups, create your own `GitProvider`, `GitTarget`, `WatchRule`, or `ClusterWatchRule` resources
instead of relying on the starter block.

The chart value reference for the quickstart keys lives in
[charts/gitops-reverser/README.md](../charts/gitops-reverser/README.md).
