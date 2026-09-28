# Kubernetes watch options and selection semantics

> **reference:** Kubernetes watch requests, filtering, and event semantics.
> Index: [`../INDEX.md`](../INDEX.md)
>
> Checked: 2026-09-28. Source links are pinned to upstream release tags.
> Feature availability depends on the API server version, feature gates, and API implementation.
> Client library support alone does not establish server support.

**A Kubernetes watch can filter objects by label on the server.** The `labelSelector` query
parameter applies to LIST and WATCH requests. It selects objects of the requested resource
type, such as ConfigMaps labeled `app=payments`. See Kubernetes
[LIST and WATCH filtering](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#list-and-watch-filtering).

Related Kubernetes references:

| Document | Topic |
|---|---|
| [Resource versions](resource-versions.md) | Snapshot watermarks, resume points, and expired history. |
| [API resource catalog](kubernetes-api-resource-catalog.md) and [API discovery](kubernetes-api-discovery.md) | Which resource endpoints exist and advertise `list` and `watch`. |
| [Subresources](subresources.md) | How endpoints such as `/status` and `/scale` relate to the parent object. |

## What one watch covers

A watch is a request against one resource endpoint, identified by group, version, and resource
(GVR). For a namespaced type, the URL chooses one namespace or all namespaces. For example:

```text
/api/v1/namespaces/team-a/secrets
/api/v1/secrets
/apis/apps/v1/namespaces/team-a/deployments
```

The first two address the same resource type with different namespace scopes. A label selector
further narrows the objects returned. Labels do not select API groups or discover resource types;
watching labeled Secrets and ConfigMaps still requires two resource endpoints.

Namespace scope, label selector, and field selector intersect: an object must satisfy all of
them. Watching objects in namespaces whose **Namespace objects** have particular labels needs
separate namespace discovery and selection. There is no `namespaceSelector` watch parameter.
See the upstream [resource URI reference](https://kubernetes.io/docs/reference/using-api/api-concepts/#resource-uris)
and the server
[`SelectionPredicate.Matches`](https://github.com/kubernetes/apiserver/blob/v0.37.0/pkg/storage/selection_predicate.go#L96)
implementation, which combines label and field predicates.

## Selectors and authorization

Kubernetes RBAC does not match requested objects by label. Its
[`RuleAllows` implementation](https://github.com/kubernetes/kubernetes/blob/v1.36.1/plugin/pkg/auth/authorizer/rbac/rbac.go#L178)
matches resource requests by verb, API group, resource, and name. A label selector therefore
does not create a label-based RBAC permission boundary. RBAC restrictions using `resourceNames`
do require a matching `metadata.name` field selector for LIST and WATCH; see
[resource-name restrictions](https://kubernetes.io/docs/reference/access-authn-authz/rbac/#referring-to-resources).

Authorization mechanisms can differ. Kubernetes passes label and field selectors to
[authorization webhooks](https://kubernetes.io/docs/reference/access-authn-authz/webhook/),
which may use them in access decisions. See
[`ResourceAttributes.LabelSelector`](https://github.com/kubernetes/api/blob/v0.37.0/authorization/v1/types.go#L127)
and the [webhook request conversion](https://github.com/kubernetes/apiserver/blob/v0.37.0/plugin/pkg/authorizer/webhook/webhook.go#L320).
Authorization of a filtered request therefore does not universally imply authorization of an
unfiltered request. Each request remains subject to the configured authorizer chain.

## Request options

The REST query parameters below correspond to
[`metav1.ListOptions`](https://github.com/kubernetes/apimachinery/blob/v0.37.0/pkg/apis/meta/v1/types.go#L361).
The dynamic client's
[`Watch` method](https://github.com/kubernetes/client-go/blob/v0.37.0/dynamic/simple.go#L264)
sets `watch=true` itself. Options with feature dependencies need a compatible
server; discovery's `watch` verb alone does not establish support for every option.

| Option | What it controls | Important boundary |
|---|---|---|
| `watch=true` | Streams changes to the selected collection. | One request addresses one GVR and namespace scope. |
| `labelSelector` | Selects objects by `metadata.labels`. | Empty selects everything within the other constraints. |
| `fieldSelector` | Selects objects by supported field values. | Supported fields depend on the resource type. |
| `resourceVersion=R` | Resumes changes after an observed collection version. | An expired version requires rebuilding current state. |
| `sendInitialEvents=true` | Starts with synthetic `ADDED` events representing current selected objects, then streams changes. | Requires streaming-list support and `resourceVersionMatch=NotOlderThan`. |
| `resourceVersionMatch=NotOlderThan` | Sets the freshness bound for initial events. | With an unset RV, initial state is at least as fresh as the start of request processing. Ordinary cursor-resume watches omit this option. |
| `allowWatchBookmarks=true` | Requests `BOOKMARK` progress events. | Ordinary bookmarks have no guaranteed frequency or delivery. |
| `timeoutSeconds` | Bounds the request's total duration. | Activity does not reset it; the client still needs reconnection logic. |
| `shardSelector` | Selects hash ranges of supported metadata fields. | Alpha, gated by `ShardedListAndWatch`; see below. |
| `limit`, `continue` | Paginate a LIST. | These are not watch pagination or event-count controls. |

The [upstream option validation](https://github.com/kubernetes/apimachinery/blob/v0.37.0/pkg/apis/meta/internalversion/validation/validation.go#L53)
defines legal combinations. `resourceVersionMatch=Exact` applies to LIST freshness; watches
use the resume and initial-state modes described above.

## Label selection

For example, these options request the current matching ConfigMaps followed by live changes
on a namespaced ConfigMap endpoint:

```go
sendInitial := true
opts := metav1.ListOptions{
    LabelSelector:        "app=payments",
    SendInitialEvents:    &sendInitial,
    ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
    AllowWatchBookmarks:  true,
}
```

The [label selector syntax](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#label-selectors)
supports equality (`app=payments`), inequality (`app!=payments`), set membership
(`environment in (production,staging)`), exclusion (`tier notin (frontend)`), existence
(`backup`), and absence (`!backup`). Comma-separated requirements are ANDed. Set membership
offers alternatives for one key; arbitrary OR between complete selectors is unsupported.
Negative requirements also match objects missing the key, so use an existence requirement
when absence should be excluded.

A CRD field of type `metav1.LabelSelector` expresses the same selection through `matchLabels`
and `matchExpressions`. Convert it to the query string with
[`metav1.LabelSelectorAsSelector`](https://github.com/kubernetes/apimachinery/blob/v0.37.0/pkg/apis/meta/v1/helpers.go#L36).
An optional field needs an explicit default: that helper treats a nil selector as matching
nothing and an empty selector as matching everything.

### Event types describe membership in the selected collection

The upstream [`watch.Event` types](https://github.com/kubernetes/apimachinery/blob/v0.37.0/pkg/watch/watch.go#L54)
are `ADDED`, `MODIFIED`, `DELETED`, `BOOKMARK`, and `ERROR`. Bookmarks and errors report
progress and stream status. Object events on a filtered watch report transitions into, within,
and out of its result set:

| Matched before? | Matches after? | Watch event |
|---|---|---|
| No | No | None. |
| No | Yes | `ADDED`, including an existing object gaining a matching label. |
| Yes | Yes | `MODIFIED` for an object update. |
| Yes | No | `DELETED`, including a label change that stops matching. |

An actual object deletion also yields `DELETED` when the object previously matched. Therefore,
`DELETED` alone cannot distinguish deletion from leaving the selector. Synthetic initial
`ADDED` events likewise do not prove that objects are newly created.

The kube-apiserver
[`cacheWatcher.convertToWatchEvent` implementation](https://github.com/kubernetes/apiserver/blob/v0.37.0/pkg/storage/cacher/cache_watcher.go#L373)
evaluates both old and new membership. For a membership exit, it sends the **previous object
content with the new event's resourceVersion**. Rechecking the labels on that payload does
not establish whether the object still matches in the cluster.

A label-changing PATCH can produce `ADDED` or `DELETED`; watch event types do not identify
the original API verb or the request actor. Kubernetes
[auditing](https://kubernetes.io/docs/tasks/debug/debug-cluster/audit/) records request information
separately from the object watch protocol.

## Field selection

[Field selectors](https://kubernetes.io/docs/concepts/overview/working-with-objects/field-selectors/)
filter supported fields, for example `metadata.name=example` to follow one named object or
`status.phase=Running` for Pods. They select whole objects; they do not select which fields
appear in the response. Supported fields vary by type, and unsupported fields produce an
error. Field selectors support `=`, `==`, and `!=`, with comma-separated AND requirements.
They do not support label-style `in` and `notin` expressions.

For custom resources, `metadata.name` and `metadata.namespace` are supported. Additional fields
must be declared in the CRD's `spec.versions[*].selectableFields`; see
[selectable fields for custom resources](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions/#crd-selectable-fields).
Arbitrary JSONPath or CEL predicates are not general watch filters.

Field-filtered watches have the same membership-transition issue as label-filtered watches.
For example, a Pod leaving `status.phase=Running` can produce `DELETED` on that filtered watch.

## Initial state, progress, and recovery

Without explicit initial-event settings, upstream documents an unset RV as current-state
initialization and `resourceVersion="0"` as initialization at any available version, potentially
stale. An observed nonzero RV resumes subsequent changes. See the
[watch resourceVersion semantics](https://kubernetes.io/docs/reference/using-api/api-concepts/#semantics-for-watch).
Use streaming initial events when the client needs an explicit snapshot-completion boundary.

A client maintaining a materialized view needs both the current selected set and subsequent
changes. Two Kubernetes patterns support this:

1. **LIST, then WATCH.** List with the intended scope and selectors, retain the list's
   `metadata.resourceVersion`, then watch with the same selection from that RV. If history
   expires before the watch can resume, rebuild from a fresh snapshot.
2. **Streaming initial events.** Request `sendInitialEvents=true` and
   `resourceVersionMatch=NotOlderThan`. Initial objects arrive as synthetic `ADDED` events.
   Request bookmarks with `allowWatchBookmarks=true`. A bookmark annotated
   `k8s.io/initial-events-end: "true"` marks completion, followed by live changes. See
   [streaming lists](https://kubernetes.io/docs/reference/using-api/api-concepts/#streaming-lists)
   and the upstream
   [initial-events contract and annotation](https://github.com/kubernetes/apimachinery/blob/v0.37.0/pkg/apis/meta/v1/types.go#L442).

Initial events represent current state, not a history of all mutations during downtime.
Their object RVs need not appear in mutation order.

[Ordinary bookmarks](https://kubernetes.io/docs/reference/using-api/api-concepts/#watch-bookmarks)
report observation progress. They carry no snapshot-completion guarantee without the
initial-events annotation, and they do not acknowledge a consumer's durable write. The
[`AllowWatchBookmarks` contract](https://github.com/kubernetes/apimachinery/blob/v0.37.0/pkg/apis/meta/v1/types.go#L379)
also permits a server to send no ordinary bookmarks during a session.

On a disconnect or timeout, reconnect from an appropriate observed RV. Handle errors both
when opening the request and as `ERROR` events on an established stream. `410 Gone` means
the requested history is unavailable and current state must be rebuilt. See upstream
[watch recovery](https://kubernetes.io/docs/reference/using-api/api-concepts/#efficient-detection-of-changes).

Use the same selection for the snapshot, initial events, and every reconnection. A cursor
does not identify or restore its selector. If a selector changes, resuming only from the old
cursor can miss newly included objects that have not changed since that point.

## Other capabilities and limits

**Metadata-only watches.**
[Content negotiation](https://kubernetes.io/docs/reference/using-api/api-concepts/#metadata-only-fetches)
can request `PartialObjectMetadata` objects. The client-go
[metadata client's `Watch` method](https://github.com/kubernetes/client-go/blob/v0.37.0/metadata/metadata.go#L253)
implements this with an `Accept` header. The representation omits object content such as
`spec` and `status`; it does not suppress events caused by changes to those fields. Support
depends on the serving API, particularly for aggregated APIs.

**Sharded watches.** The alpha `shardSelector` option partitions by hashes of object UID or
namespace. It requires `ShardedListAndWatch`; unsupported servers can ignore the option.
Clients must verify support before relying on disjoint shards. Sharding partitions workload
across consumers; label selectors express object membership. See
[sharded list and watch](https://kubernetes.io/docs/reference/using-api/api-concepts/#sharded-list-and-watch)
and the [`ShardSelector` contract](https://github.com/kubernetes/apimachinery/blob/v0.37.0/pkg/apis/meta/v1/types.go#L469).

**Application-side processing.** Standard watch options do not provide filters for only
`MODIFIED` events, only changes to `spec`, an annotation value, a user identity, or an arbitrary
combination of kinds. These are outside the `ListOptions` contract. Consumers can apply
additional predicates after receiving events.

**Independent streams.** Overlapping watches can deliver the same object. The API server
preserves event delivery order within each cache watcher; see
[`sendWatchCacheEvent`](https://github.com/kubernetes/apiserver/blob/v0.37.0/pkg/storage/cacher/cache_watcher.go#L415).
That is a per-request property. Merging independent consumers' arrivals requires client-side
coordination; server-side filtering supplies no such merge or deduplication. Initial-state
events remain subject to the snapshot semantics above.
