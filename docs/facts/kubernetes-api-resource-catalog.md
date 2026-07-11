# Kubernetes API Resource Catalog for GitOps Reverser

## Purpose

GitOps Reverser needs to know which Kubernetes resource types the API server
currently serves before it can turn a `WatchRule` declaration into concrete
watch and snapshot work.

The catalog answer is resource-type shaped:

- Which GVRs are currently served?
- Which GVK belongs to each served resource entry?
- Which group/version is preferred when a rule does not choose one?
- Is a resource namespaced or cluster-scoped?
- Does it support the verbs needed by GitOps Reverser, especially `list` and
  `watch`?

This is different from a snapshot of live Kubernetes objects. Knowing that
`apps/v1/deployments` is served is not the same as listing every `Deployment`.

## Current Project Use

Today the watch manager queries discovery in two places:

1. `internal/watch/discovery.go` uses `ServerPreferredResources()` to filter
   concrete requested GVRs before starting dynamic informers.
2. `internal/watch/manager.go` uses `ServerPreferredResources()` while
   resolving wildcard API versions for the destructive cluster snapshot path.

Both paths currently use discovery as a point lookup. The WatchRule GVR
resolution work needs a stronger model: one shared, in-memory catalog of the API
surface that the API server currently serves.

## Vocabulary

### GVR

A Group / Version / Resource identifies a REST collection:

- Core ConfigMaps: `"" / v1 / configmaps`
- Apps Deployments: `apps / v1 / deployments`
- A CRD-backed resource: `shop.example.com / v1 / icecreamorders`

List and watch operations target a GVR.

### GVK

A Group / Version / Kind identifies the type written in an object:

- Core ConfigMap: `v1`, `ConfigMap`
- Apps Deployment: `apps/v1`, `Deployment`

GitOps Reverser needs both views. GVR is needed to list and watch a collection;
GVK is needed to understand the object type exposed by discovery and carried by
objects.

### Served resource

In this document, a served resource is a resource entry advertised by the
Kubernetes Discovery API for a group/version. A served resource entry can still
be unusable for GitOps Reverser if it is a subresource, lacks required verbs, or
cannot be accessed with the operator's RBAC.

### API resource catalog

`APIResourceCatalog` is GitOps Reverser's in-memory catalog of the API surface
discovered from the Kubernetes API server. It is the proposed source for
resource resolution inside GitOps Reverser.

It is not an OpenAPI schema cache, not a RESTMapper replacement by itself, and
not the snapshot of actual user objects that GitOps Reverser writes to Git.

## What Kubernetes Discovery Provides

The Kubernetes Discovery API publishes the group versions and resources a
cluster supports. For each resource it can provide the resource name, scope,
endpoint information, verbs, alternative names, group, version, and kind. That
is the information GitOps Reverser needs for `APIResourceCatalog`.

Discovery is separate from OpenAPI:

- Discovery answers which API groups, versions, resources, kinds, scopes, and
  operations are exposed.
- OpenAPI describes schemas and endpoint shapes in much more detail.

Kubernetes supports aggregated discovery and unaggregated discovery. Aggregated
discovery publishes the supported resources through the `/api` and `/apis`
discovery endpoints with the aggregated discovery media type. Unaggregated
discovery begins at `/api` and `/apis` and then fetches resource documents per
group/version.

The `client-go` discovery client exposes different cuts of this data:

| Method | Meaning for this project |
|---|---|
| `ServerGroups()` | Groups, supported versions, and preferred versions |
| `ServerResourcesForGroupVersion()` | Resource entries for one group/version |
| `ServerGroupsAndResources()` | Supported resources for all groups and versions |
| `ServerPreferredResources()` | Supported resources only at the server-preferred versions |

For a full catalog, `ServerGroupsAndResources()` is the most direct client-go
starting point. `ServerPreferredResources()` is useful for a preferred-version
view, but it is not the full set of served versions. Keeping all served versions
and preferred-version data may therefore require merging more than one discovery
view, with degraded group/version handling applied to that merge.

## What Catalog We Need

The catalog should keep all served discovery entries first, then expose
filtered views for GitOps Reverser operations.

A practical in-memory entry should capture at least:

```text
APIResourceEntry
  GVR:        group, version, resource
  GVK:        group, version, kind
  Namespaced: bool
  Verbs:      set[string]
  Preferred:  bool
  Subresource bool
  Allowed:    bool
```

Useful indexes:

```text
byGVR[group/version/resource] -> APIResourceEntry
byGroupResource[group/resource] -> []APIResourceEntry
byResource[resource] -> []APIResourceEntry
preferredByGroupResource[group/resource] -> APIResourceEntry
```

The catalog should include resource entries that GitOps Reverser will later
reject for a specific operation. For example, the full API surface can include
a resource without both `list` and `watch`, while the informer candidate view
filters it out.

GitOps Reverser should also skip subresource entries for its current resource
planning unless a later design deliberately adds subresource support.

`Allowed` is the GitOps Reverser watch-policy bit. It must not replace served
discovery state. The current default exclusions in `internal/watch/discovery.go`
should become explicit `Allowed=false` catalog entries: a resource such as
`batch/v1/jobs` can be served by Kubernetes while GitOps Reverser deliberately
refuses to watch it. If that policy becomes configurable, rule status should
surface that the resource was resolved but disallowed rather than reporting it
as not served.

## Can GitOps Reverser Trust Only the Catalog?

Yes, if catalog lookups carry the trust state of the discovery data they depend
on.

That model is desirable:

- Resource resolution reads only the shared in-memory catalog.
- Snapshot planning reads only the shared in-memory catalog.
- Informer planning reads only the shared in-memory catalog.
- Callers do not perform ad hoc discovery lookups with subtly different rules.

The catalog then becomes the local view of Kubernetes API-server truth. Under
that model:

- If a resource type appears in trusted catalog data, it is a served resource
  type that rules may resolve against.
- If a resource type disappears from newly trusted data for its group/version,
  GitOps Reverser may stop tracking it.
- If a rule asks for a resource type absent from trusted catalog data covering
  that lookup scope, that is a not-served resolution result.

The trust condition matters. The discovery client can return partial resource
lists together with an error. A partial refresh must not erase previously
trusted catalog data for failed group/versions and then drive Git deletions for
resource types missing from the partial response.

Recommended rule:

1. Track trust per discovered group/version.
2. A clean group/version refresh may replace that group/version in the active
   catalog, including removals.
3. A failed group/version refresh keeps its last trusted catalog entries, is
   marked degraded, and is retried.
4. Absence is authoritative only for lookup scopes covered by trusted catalog
   data. If degraded or unknown group/version data could change a lookup result,
   the resolver must surface that degraded state instead of manufacturing
   `NotServed`.

This keeps the Kubernetes API server authoritative without treating an
incomplete read as an authoritative absence.

## How the API Surface Changes

The API surface can come from several places:

- Built-in APIs compiled into or enabled on kube-apiserver.
- Custom resources registered through `CustomResourceDefinition`.
- Aggregated APIs registered through `APIService` and served by extension API
  servers behind the aggregation layer.

A `CustomResourceDefinition` is a strong signal that CRD-backed resources may
have been added, changed, or removed.

An `APIService` is a strong signal that an aggregated API path may have been
added, changed, become available, become unavailable, or been removed. The
`APIService` object claims an API URL path; discovery is still needed to learn
the current resource entries exposed through that path.

Built-in API availability can also change across kube-apiserver upgrades,
configuration changes, or feature changes. That is a reason to retain startup
discovery and a recovery refresh path even if dynamic API-extension objects are
watched.

## Where Watches Fit

There is not one object watch that replaces the Discovery API result for every
served resource type. Watches observe object collections. Discovery describes
the supported API surface.

For GitOps Reverser, watches are valuable as **catalog invalidation and
refresh triggers**:

1. Keep a watch open for `CustomResourceDefinition` objects.
2. Keep a watch open for `APIService` objects.
3. On relevant add, update, or delete events, debounce and reload discovery.
4. Publish a catalog generation after trusted group/version data changes.
5. Re-resolve affected WatchRules and reconcile informer/snapshot planning from
   that generation.

These watches make catalog refresh reactive. Discovery remains the authoritative
data source for replacement catalog data.

Implementation preference: use discovery for catalog state and CRD /
`APIService` watches as invalidation signals; streaming lists are optional for
those trigger watches and are not required for correctness.

## Streaming Lists

Kubernetes streaming lists let a watch request include the collection's initial
state. With `sendInitialEvents=true`, the watch stream begins with synthetic
`ADDED` events for existing objects, can send a `BOOKMARK` that marks the end of
the initial state, and then continues as the live watch stream.

The upstream Kubernetes API concepts documentation marks streaming lists as a
Kubernetes v1.34 beta feature enabled by default.

That is a good fit for the CRD and `APIService` trigger watches:

- The initial events establish the current trigger-object collections without a
  separate object `LIST` call.
- The live stream signals future API-surface changes.
- A bookmark is the point after which the initial trigger-object view is synced.

Streaming lists do not mean "stream `ServerGroupsAndResources()`". Discovery is
not the same object collection as CRDs, `APIService` objects, or user resources.
Streaming every preferred GVR would produce object-state watches for many
resource types; it would not by itself create trusted catalog data.

When using streaming lists, expect the watch request shape to include:

```text
watch=1
sendInitialEvents=true
allowWatchBookmarks=true
resourceVersion=
resourceVersionMatch=NotOlderThan
```

Kubernetes requires `resourceVersionMatch=NotOlderThan` with
`sendInitialEvents=true`. GitOps Reverser should not treat the trigger watcher as
initially synced until the initial-events bookmark has been handled.

## Expected Catalog Lifecycle

### Startup

1. Start the catalog component.
2. Build catalog entries from discovery and record group/version trust.
3. Store clean group/versions as the first trusted catalog data.
4. Start or establish the CRD and `APIService` trigger watches.
5. Resolve rules and plan watches/snapshots from the trusted generation.

The exact startup ordering can be adjusted, but rule resolution must not assume
an authoritative absence for a lookup scope before trusted catalog data covers
that scope.

Startup should also close the gap between discovery and trigger watches. A
practical approach is to establish the trigger watches while discovery refresh
is active, or to run another discovery refresh after the trigger watches have
reached their initial bookmark. The trigger-object view tells GitOps Reverser
that discovery may need refresh; it is not the catalog itself.

### Refresh

Refresh triggers should include:

- CRD trigger watch events.
- `APIService` trigger watch events.
- Explicit refresh requests from code that detects a likely stale API-surface
  decision.
- A recovery refresh path for missed events and built-in API changes.

Refresh flow:

1. Debounce triggers.
2. Query discovery.
3. Build candidate catalog entries and group/version trust results.
4. Replace clean group/versions, preserve failed group/versions, and mark
   degraded lookup scopes.
5. Diff old and new catalog data.
6. Re-resolve rules and reconcile dependent watch/snapshot work.

### Failure

Expected failure handling:

- Partial discovery result: keep the old trusted catalog data for failed
  group/versions, retry, and expose degraded catalog-refresh status or metrics.
- Discovery unavailable before trusted catalog data covers a lookup scope: rule
  resolution and snapshot decisions that require absence from that scope should
  wait or fail closed.
- Object list failure for a GVR that is served in trusted catalog data: abort
  the destructive snapshot for that target rather than returning a partial
  object view.

## Rule Resolution Expectations

`APIResourceCatalog` should preserve GitOps Reverser's declared WatchRule
semantics:

- Omitted `apiGroups` means all groups, not the core group.
- Explicit `apiGroups: [""]` means the core API group.
- Omitted `apiVersions` means all versions at the rule level; concrete planning
  may choose the preferred served version when one concrete GVR is required.

The omitted-group behavior is a GitOps Reverser product choice, not a generic
Kubernetes `apiGroups` convention. The WatchRule and ClusterWatchRule CRD field
documentation should state it explicitly.

This creates an important ambiguity case. A bare resource name can exist in more
than one served API group. If an omitted-group rule names a resource that maps to
multiple served resource types, GitOps Reverser should ask for a more specific
group instead of guessing.

Resolution should also keep watch policy visible. A served resource with
`Allowed=false` is not a discovery miss; it is a policy miss that can be shown
in WatchRule or ClusterWatchRule status.

## Current and Proposed Responsibilities

| Area | Current behavior | Catalog direction |
|---|---|---|
| Informer GVR filtering | Point discovery through preferred resources | Resolve and filter against shared catalog data |
| Snapshot wildcard version | Per-snapshot preferred-resource lookup | Resolve against shared catalog data |
| Bare resource with omitted group | Some paths default to core `v1` | Preserve wildcard semantics and resolve from the catalog |
| Default resource exclusions | Hidden in discovery filtering | Store served resources and expose `Allowed=false` policy results |
| API-surface change detection | Periodic and rule-driven rediscovery | CRD/APIService trigger watches plus trusted discovery refresh |
| Deletion safety | Object snapshot must not be partial | Resource-type removals require trusted group/version catalog data |

## Non-goals

`APIResourceCatalog` does not by itself:

- provide OpenAPI schemas,
- prove RBAC for the operator,
- prove an aggregated API server will successfully serve every later list call,
- replace object snapshots,
- remove the need to abort a destructive snapshot when a served GVR cannot be
  listed.

## References

- Kubernetes API Discovery documentation:
  https://kubernetes.io/docs/concepts/overview/kubernetes-api/#discovery-api
- Kubernetes API concepts and streaming lists:
  https://kubernetes.io/docs/reference/using-api/api-concepts/#streaming-lists
- Kubernetes aggregation layer:
  https://kubernetes.io/docs/concepts/extend-kubernetes/api-extension/apiserver-aggregation/
- client-go discovery package:
  https://pkg.go.dev/k8s.io/client-go/discovery
- Related project primer:
  [`kubernetes-api-discovery.md`](./kubernetes-api-discovery.md)
