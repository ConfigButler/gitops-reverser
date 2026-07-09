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

The Secret is read from the `GitTarget`'s own namespace, on the cluster the operator
runs in. The cluster id the data plane keys on is `<namespace>/<name>/<key>` — the key
is part of the identity, because two `GitTarget`s naming one Secret under different
keys are pointed at different kubeconfigs. Its value is an ordinary kubeconfig.

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

### The source is part of the destination identity

`sourceCluster` names where a folder's content comes *from*, so changing it changes
what the folder means. It therefore participates in the retarget lifecycle
([gittarget-retarget.md](gittarget-retarget.md)) exactly as `branch` and `path` do:
the old materialization is torn down and the folder is rebuilt from a full snapshot
of the new cluster. Rotating a kubeconfig Secret's *contents* is not a retarget —
it is the same cluster, reached with a fresh credential.

## Implementation

The watch manager grew a **cluster context**: the set of things that were previously
Manager-wide singletons and are in fact properties of one cluster.

```go
type clusterContext struct {
    id             string          // "" for the local cluster, else "<ns>/<name>/<key>"
    catalog        *APIResourceCatalog
    registry       *typeset.Registry
    restConfig     *rest.Config
    configVersion  string          // the kubeconfig Secret's resourceVersion
    dynamicClient  dynamic.Interface
    discovery      apiResourceDiscovery
    triggerFactory dynamicinformer.DynamicSharedInformerFactory
}
```

- `Manager.clusters map[string]*clusterContext`. A zero-value Manager (unit tests, and
  every single-cluster install) creates exactly one, keyed `LocalClusterID`.
- `RefreshAPIResourceCatalog` refreshes every **active** cluster — the local one plus
  every cluster some compiled rule points at. It returns the *local* cluster's error
  only: a remote that cannot be reached must fail its own `GitTarget`s (through its
  unready registry), never the local cluster's.
- `openTargetWatch` / `openTargetList` take the cluster id, which reaches them on the
  per-watch `watchFilter`, so each `(GitTarget, GVR, namespace)` watch runs against the
  right cluster.
- Watched-type tables resolve a `GitTarget`'s rules against **its own cluster's** type
  registry. A CRD installed only on the remote is followable only there — and, more
  importantly, a type served only *locally* never resolves for a remote target.
  Mirroring the wrong cluster into a folder is worse than mirroring none.

The source cluster reaches the data plane through the rule store: a
`rulestore.TargetBinding` carries it alongside the branch and path, resolved once when a
rule compiles against its `GitTarget`.

### The one shared surface: the GVK→GVR lookup

`WorkerManager` holds a single `typeset.Lookup`, used when scanning the manifests already
in a Git folder to answer "what resource is this document?". Workers are keyed by
(provider, branch) and can be shared by several `GitTarget`s, so this lookup cannot be
per-target without threading it through every pending write.

It is therefore a **union** over the live cluster registries, consulted in a stable order
(local first, then remote contexts sorted by id). GVK→GVR is derived from the served
resource name, so two clusters serving the same GVK agree on the GVR in every case short
of an outright API-group collision. First answer wins.

## Credentials and RBAC

The operator needs `get` on Secrets in the namespaces where `GitTarget`s live — it already
did, for `GitProvider.spec.secretRef` and the SOPS age keys.

The kubeconfig Secret is read on demand and its contents are **not** retained: a
`rest.Config` is built, the bytes are dropped, and only the Secret's `resourceVersion` is
remembered, so a rotation rebuilds the clients exactly once. No Secret informer is added —
the same reasoning as
[../../future/secret-value-retention-plan.md](../../future/secret-value-retention-plan.md).

A remote cluster is reached over a network the local one is not, so its clients carry
client-side throttling (`--source-cluster-qps` / `--source-cluster-burst`).

The `GitTarget` controller reads and parses the kubeconfig before any watch opens against
it, and reports `Validated=False` / `SourceClusterUnreachable` when it cannot. That is a
legibility gate, not a security one: without it, a typo'd Secret name surfaces only as a
stalled data plane and a repeating log line. It deliberately does not *dial* the cluster —
a controller that blocked on a network round trip would stall every other `GitTarget`
behind it.

## What this unlocks

- Config and credentials live in the cluster the operator runs in; the watched cluster
  holds only the watched resources.
- One reverser can mirror many clusters, because each `GitTarget` carries its own source.
- The watched cluster does not need the `configbutler.ai` CRDs installed at all.
