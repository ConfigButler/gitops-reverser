# Shared prerequisites

Every `GitTarget` here refers to a `GitProvider` in the same namespace. The provider owns the repository
connection and writable branch policy; the target owns the folder and its layout. This separation
lets one repository serve several independently scoped targets.

[`config/gitprovider.yaml`](config/gitprovider.yaml) is a minimal SSH provider specimen for the
`demo` namespace. Copy it into each namespace that owns a target, change its name and repository
URL, and provide the referenced credentials and `known_hosts` objects. A public HTTPS repository
can omit `secretRef` and `knownHostsRef`.

```text
namespace demo
  GitProvider/app-repository
  GitTarget/demo -> GitProvider/app-repository
```

The provider carries credentials, so nothing uses it without a grant. Its
`spec.allowedGitBranches` says which namespaces' `GitBranch`es may claim which branches, and each
`GitBranch`'s `spec.allowedGitTargets` says which namespaces' `GitTarget`s may write through it; the
provider's own namespace needs a grant too (`from: Same`).

## Source cluster authority

Every `GitTarget` names its source cluster with `spec.kubeProviderRef`. The examples here use the
`KubeProvider` the chart renders, `local` in `gitops-reverser`, which uses the operator's own
identity. Every target needs a grant from the owner of the `KubeProvider`
(`spec.allowedGitTargets`), and the grant's `sourceNamespaces` bound which source namespaces its
`WatchRule`s may watch, its own included only when listed (`"{consumer.namespace}"`). What may actually be read
is bounded by that provider credential's own RBAC in the source cluster. Concrete specimens are in
[`../../shapes/1-flat-serialized/config/`](../../shapes/1-flat-serialized/config/kubeprovider.yaml)
and [`../../shapes/3-tree-serialized/config/`](../../shapes/3-tree-serialized/config/kubeprovider.yaml).

The source-cluster rule and source-namespace authorization are one grant read twice. A grant in
`allowedGitTargets` decides which configuration namespaces may reference the `KubeProvider` at all,
and its `sourceNamespaces` decide what those namespaces' `GitTarget`s may watch.

## Credentials stay out of this design

This example names a credentials Secret and `known_hosts` ConfigMap but does not include either
object. Repository credentials and SSH host keys are operational inputs, not a layout convention.
The current setup contract is in
[GitProvider configuration](../../../../../docs/configuration.md#gitprovider).
