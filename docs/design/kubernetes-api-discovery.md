# Kubernetes API Discovery and Watching in GitOps Reverser

This document explains how GitOps Reverser decides what it can watch, and gives Kubernetes API background for
learning.

## TL;DR

- A watched resource does **not** need to be a CRD.
- GitOps Reverser can watch any discoverable Kubernetes resource that supports `list` and `watch`, including:
  - Built-in resources (for example `configmaps.v1`)
  - CRD-backed resources
  - Aggregated API resources served through `APIService`
- Practical rule: if the resource appears in API discovery and has `list/watch` verbs, it is a candidate.

## Kubernetes API Theory (Quick Primer)

Kubernetes APIs are addressed as **Group / Version / Resource (GVR)**:

- Group: `""` (core), `apps`, `rbac.authorization.k8s.io`, `metrics.k8s.io`, etc.
- Version: `v1`, `v1beta1`, etc.
- Resource: plural name such as `pods`, `deployments`, `customresources`.

Examples:

- Core API: `"" / v1 / configmaps`
- Apps API: `apps / v1 / deployments`
- Custom API: `shop.example.com / v1 / icecreamorders`

### Where resources come from

Kubernetes can serve resources from multiple backends:

- Built-in APIs (compiled into kube-apiserver)
- CRDs (registered through `CustomResourceDefinition`)
- Aggregated APIs (registered through `APIService`, then served by an extension API server)

From a client perspective, all of these are exposed through API discovery endpoints (`/api` and `/apis`).

## How GitOps Reverser Chooses Watch Targets

At runtime, the watch manager does this:

1. Build requested GVRs from active `WatchRule` and `ClusterWatchRule`.
2. Query API discovery (`ServerPreferredResources`).
3. Keep only resources that:
   - exist in discovery,
   - support both `list` and `watch`,
   - match the requested scope (`Namespaced` vs `Cluster`).
4. Start dynamic informers for the remaining GVRs.

This means the watch decision is based on **discoverability and verbs**, not on "is it a CRD?".

## API Trick: Streaming Lists

Kubernetes has a newer watch mode called **streaming lists**:
[`sendInitialEvents=true`](https://kubernetes.io/docs/reference/using-api/api-concepts/#streaming-lists).
The current upstream docs mark it as `Kubernetes v1.34 [beta]` and enabled by default.

Normally a controller establishes state by:

1. `LIST` the whole collection.
2. Remember the collection `resourceVersion`.
3. `WATCH` changes after that `resourceVersion`.

With streaming lists, a client starts a watch with:

```text
watch=1
sendInitialEvents=true
allowWatchBookmarks=true
resourceVersion=
resourceVersionMatch=NotOlderThan
```

The API server then sends synthetic `ADDED` events for the current objects, sends a `BOOKMARK`
that marks the end of the initial state, and then continues with the normal live watch stream.

For GitOps Reverser this matters because the reconcile/snapshot engine wants exactly this shape:
build a current cluster image, know the `resourceVersion` that image corresponds to, then consume
deltas. It is especially relevant to a future custom tracking engine. It is less immediately useful
for the existing `dynamicinformer` path because informers already hide the list/watch bootstrap and
the current snapshot path does an extra raw `LIST` after the informer cache is synced.

Practical caveats:

- Streaming lists still require `watch` support and RBAC for `watch`.
- `sendInitialEvents=true` requires `resourceVersionMatch=NotOlderThan`.
- The initial objects arrive as watch events, so the client must detect the initial-events `BOOKMARK`
  before treating the tracked set as synced.
- Older clusters or aggregated API servers may not support the option consistently; the client should
  fall back to classic `LIST` then `WATCH`.
- The project's current Kubernetes libraries already expose the knobs in `metav1.ListOptions`:
  `SendInitialEvents`, `AllowWatchBookmarks`, and `ResourceVersionMatch`.

## CRD vs APIService: What This Means in Practice

### CRDs

CRDs are supported when:

- their GVR is in discovery,
- the resource supports `list` and `watch`,
- the rule points to a concrete GVR that matches scope.

### APIService-backed resources

APIService-backed resources are also supported under the same conditions:

- the `APIService` is healthy and available,
- the resource appears in discovery,
- the API server exposes `list` and `watch` for that resource.

So yes, you can watch resources served through `APIService`; CRD is not required.

## Startup Snapshot: GVR Resolution and the Trust Boundary

Separately from the live informer path, the watch manager takes a **cluster
snapshot** (`Manager.GetClusterStateForGitDest`) so the `FolderReconciler` can
diff "what the cluster has" against "what Git has" and converge the mirror. That
diff is destructive by design — anything in Git but not in the cluster snapshot
is deleted from Git.

Because the diff is destructive, the snapshot must never be **silently partial**.
A resource that is missing from the snapshot for any reason other than "it does
not exist in the cluster" is indistinguishable from a deletion, and would wipe
that resource's tracked files on the next reconcile. (This caused a real
data-loss incident: a `ClusterWatchRule` using `apiVersions: ["*"]` produced an
empty snapshot on every controller restart and emptied the Git mirror.)

The snapshot path therefore enforces a strict **trust boundary**:

1. **Wildcard API versions are resolved.** A rule may use the documented
   `apiVersions: ["*"]` form. For the snapshot, each `(group, resource)` with a
   `*` version is resolved to the server's served version via discovery
   (`ServerPreferredResources`). Discovery is loaded lazily — a snapshot whose
   rules all pin concrete versions performs no discovery call.
2. **Anything unresolvable aborts the snapshot.** If a `*` version cannot be
   resolved, or a rule uses a `*` apiGroup / `*` resource (which the snapshot
   cannot enumerate), or a `List()` call fails, `GetClusterStateForGitDest`
   returns an **error** instead of a partial result. No `ClusterStateEvent` is
   emitted and the reconcile is aborted — the mirror is left untouched.
3. **An empty snapshot is authoritative.** Once the snapshot is known to be
   complete, an empty result means the cluster genuinely has no watched
   resources, and the reconciler empties the Git mirror to match. The trust
   decision lives in the snapshot, not in the reconciler.

In short: the snapshot is **a complete cluster view or an error — never a
silently partial one.**

## Current Product Constraints (Important)

- Informer planning currently needs concrete GVRs:
  - one concrete API group (not `*`)
  - one concrete API version (not `*`)
  - concrete resource name (not `*`, no subresource forms)
- Wildcard expansion across discovery is not implemented for the informer path.
  The startup snapshot path *does* resolve `*` API versions (see above); `*`
  groups and `*` resources still abort the snapshot rather than expand.
- A small built-in exclusion list skips noisy resources (for example `pods`, `events`, `leases`, `jobs`).
- RBAC still applies: the operator must be allowed to `get/list/watch` the target resources.
- Scope still applies:
  - `WatchRule` is namespace-scoped
  - `ClusterWatchRule` can watch cluster-scoped and namespaced resources (by rule scope)

## Quick Verification Commands

Use these to check if a resource is likely watchable:

```bash
# 1) Is the resource discoverable?
kubectl api-resources | grep -E 'icecreamorders|<your-resource>'

# 2) Check served API group/version directly (example)
kubectl get --raw /apis/metrics.k8s.io/v1beta1

# 3) Validate operator permissions
kubectl auth can-i list <resource> --all-namespaces
kubectl auth can-i watch <resource> --all-namespaces
```

If a resource is missing from discovery or does not support `watch`, GitOps Reverser will not start an informer for it.
