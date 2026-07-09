# Separating the config plane from the watched cluster

> Status: implemented
> Related: [README.md](README.md), [../../security-model.md](../../security-model.md)

## Problem

GitOps Reverser built exactly one client:

```go
mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{ … })
```

That single `rest.Config` served two jobs that are conceptually unrelated:

- **the config plane** — where `GitProvider`, `GitTarget`, `WatchRule` and the
  `secretRef` Git credentials are read;
- **the watched cluster** — where the resources it mirrors to Git actually live.

Because `GitProvider.spec.secretRef` is a local reference and
`WatchRule.spec.targetRef` must name a same-namespace `GitTarget`, all four
objects had to sit in one namespace **on the cluster being watched**. Nothing
chose this; it fell out of having one kubeconfig.

For an operator mirroring a cluster they also hand to someone else, that means a
Git write credential — often scoped far more broadly than the one repository a
`GitTarget` names — lives one RBAC rule away from whoever can read Secrets in
that namespace. The isolation rests on a policy decision rather than on a
boundary.

## Shape

`GitTarget` may name the cluster it mirrors, exactly as Flux's `Kustomization`
names the cluster it applies to:

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
spec:
  providerRef: { name: acme }
  branch: main
  path: clusters/acme
  sourceCluster:                 # NEW — omit for "the cluster I run in"
    kubeConfigSecretRef:
      name: acme-workspace-kubeconfig
      key: value.yaml            # default: value.yaml, Flux's convention
```

The Secret is read from the `GitTarget`'s own namespace — the config plane. Its
value is an ordinary kubeconfig.

### Why `GitTarget` and not `WatchRule`

Both were candidates. `GitTarget` wins because a `GitTarget` is already the unit
that owns exactly one materialization: one (provider, branch, folder). Adding
the source cluster makes it one (cluster, provider, branch, folder) — still one
owner, one folder, one desired state. The watch data plane is *already* keyed by
`GitTarget`, so the cluster comes along for free and no two `WatchRule`s can
disagree about which cluster a folder mirrors.

Putting it on `WatchRule` would allow two rules pointing at different clusters to
feed one folder, and the mark-and-sweep would then alternately delete each
cluster's objects. That is not a configuration anyone wants to be able to write.

`WatchRule` keeps its meaning: it watches the namespace **it lives in**, on the
`GitTarget`'s source cluster. A `ClusterWatchRule` watches the whole source
cluster. When the source cluster is remote, the namespace names are resolved on
the remote — a `WatchRule` in config-plane namespace `team-a` watches namespace
`team-a` on the remote cluster.

### The destination is immutable, the source is not

`sourceCluster` is part of the destination identity in the same sense
`providerRef` is: change it and the folder's contents mean something different.
It participates in the retarget lifecycle
([gittarget-retarget.md](gittarget-retarget.md)) rather than being frozen —
rotating a kubeconfig Secret's *contents* is transparent, and repointing at a
different cluster re-materializes the folder.

## Implementation

The watch manager grew a **cluster context**: the set of things that were
previously Manager-wide singletons, now one per distinct source cluster.

```go
type clusterContext struct {
    id          string          // "" for the local cluster, else "<ns>/<name>/<key>"
    restConfig  *rest.Config
    dynamic     dynamic.Interface
    discovery   apiResourceDiscovery
    catalog     *APIResourceCatalog
    registry    *typeset.Registry
    triggers    apiSurfaceTriggers   // the CRD/APIService informers
}
```

- `Manager.clusters map[string]*clusterContext`, plus a `local` context that a
  zero-value Manager (unit tests, and every single-cluster install) uses
  unchanged.
- `RefreshAPIResourceCatalog` iterates the live contexts. A context becomes live
  when a `GitTarget` referencing it is declared and dies when the last one goes
  away.
- `openTargetWatch` / `openTargetList` resolve the context from the `GitTarget`,
  so each per-(GitTarget, GVR, namespace) watch runs against the right cluster.
- Watched-type tables resolve a `GitTarget`'s rules against **its own cluster's**
  type registry, so a CRD installed only on the remote is followable only there.

### The one shared surface: the GVK→GVR lookup

`WorkerManager` holds a single `typeset.Lookup` used when scanning the manifests
already in a Git folder, to answer "what resource is this document?". Workers are
keyed by (provider, branch) and can be shared by several `GitTarget`s, so this
lookup cannot be per-target without threading it through every pending write.

It is therefore a **union** over the live cluster registries, consulted in a
stable order (local first, then remote contexts sorted by id). GVK→GVR is derived
from the served resource name, so two clusters serving the same GVK agree on the
GVR in every case that is not an outright API-group collision. The union records
a metric when two registries disagree, and the first answer wins.

## Credentials and RBAC

The operator now needs `get`/`list`/`watch` on Secrets in the namespaces where
`GitTarget`s live — it already did, for `GitProvider.spec.secretRef` and the SOPS
age keys.

The kubeconfig Secret is read on demand and its contents are **not** retained: a
`rest.Config` is built, the bytes are dropped, and the Secret's
`resourceVersion` is remembered so a rotation rebuilds the clients. No Secret
informer is added — the same reasoning as
[../../future/secret-value-retention-plan.md](../../future/secret-value-retention-plan.md).

## What this unlocks

- Config and credentials live in the cluster the operator runs in; the watched
  cluster holds only the watched resources.
- One reverser can mirror many clusters, because each `GitTarget` carries its own
  source.
- The watched cluster does not need the `configbutler.ai` CRDs installed at all.
