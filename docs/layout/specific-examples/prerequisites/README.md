# Shared prerequisites

Every `GitTarget` refers to a `GitProvider` in the same namespace. The provider owns the repository
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

The provider is namespace-local because it carries credentials. A `GitTarget` cannot refer to a
provider in another namespace.

## Source cluster authority

The examples here use the operator's in-cluster source. A target that captures objects from a
different source namespace needs one grant, and it belongs to the platform admin: the
`ClusterProvider` delegates the *ability* to choose a source namespace
(`allowAnySourceNamespace`). What may actually be read is bounded by that provider credential's own
RBAC in the source cluster. Concrete specimens are in
[`../../shapes/1-flat-serialized/config/`](../../shapes/1-flat-serialized/config/clusterprovider.yaml)
and [`../../shapes/3-tree-serialized/config/`](../../shapes/3-tree-serialized/config/clusterprovider.yaml).

The source-cluster rule and source-namespace authorization are separate checks. `accessFrom` decides
which configuration namespaces may reference the provider at all; `allowAnySourceNamespace` decides
whether those namespaces' `GitTarget`s may look outside their own.

## Credentials stay out of this design

This example names a credentials Secret and `known_hosts` ConfigMap but does not include either
object. Repository credentials and SSH host keys are operational inputs, not a layout convention.
The current setup contract is in
[GitProvider configuration](../../../configuration.md#gitprovider).
