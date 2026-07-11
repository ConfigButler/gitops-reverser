# Manifest GVK/GVR Mapping Layer

> Status: design, captured 2026-06-04
> Related:
> [current-manifest-support-review.md](current-manifest-support-review.md),
> [reconcile-via-watchlist-mark-and-sweep.md](reconcile-via-watchlist-mark-and-sweep.md),
> [../kubernetes-api-resource-catalog.md](../facts/kubernetes-api-resource-catalog.md),
> [`internal/watch/api_resource_catalog.go`](../../internal/watch/api_resource_catalog.go),
> `internal/watch/rule_gvr_resolver.go`,
> [`internal/manifestanalyzer/analyzer.go`](../../internal/manifestanalyzer/analyzer.go)

## Summary

The manifest materialization plan depends on a layer that does not exist yet:
mapping manifest identity (`apiVersion`, `kind`, namespace, name) to resource
identity (`group`, `version`, `resource`, namespace, name), and back again.

`APIResourceCatalog` and `RuleGVRResolver` already solve the watch side of this
problem: rules become concrete watched GVRs through trusted Kubernetes discovery.
They do not yet provide a manifest-facing `GVK <-> GVR` abstraction, and the
manifest analyzer deliberately has a no-cluster mode. The missing layer must
therefore be an injected interface, not a package-global call to discovery.

This is not decorative plumbing. It is the boundary that decides whether an
existing manifest document is watched, unwatched, stale, ambiguous, disallowed,
or unknowable. Every later phase of the materialized model depends on that
decision being stable and explicit.

## What Kubernetes Actually Provides

Kubernetes gives us an API-surface catalog through discovery. From `/api`,
`/apis`, and the per-group/version discovery documents, a client can learn:

- API groups and versions the server currently supports.
- The preferred version for each API group.
- Resource entries for each group/version.
- The REST resource name, normally plural, such as `configmaps` or
  `deployments`.
- The `kind` served by that resource entry.
- Whether the resource is namespaced.
- The verbs the endpoint advertises, such as `get`, `list`, `watch`, `create`,
  `update`, `patch`, and `delete`.
- Short names, categories, and singular names when the server reports them.

In client-go terms, this is the data returned by discovery calls such as
`ServerGroupsAndResources()`, `ServerPreferredResources()`, and
`ServerResourcesForGroupVersion()`. The project already stores the important
part of that shape in `APIResourceEntry`:

```text
GVR
GVK
Namespaced
Verbs
Preferred
Subresource
Allowed
PolicyReason
```

That is enough for the mapping layer's first job: exact mapping between a served
GVK and its served GVR, plus scope and verb metadata.

Kubernetes does **not** give us everything:

- Discovery is not a live object snapshot.
- Discovery is not OpenAPI schema validation.
- Discovery does not prove the operator's RBAC can list or watch the resource.
- Discovery can partially fail; an incomplete discovery response must not be
  treated as authoritative absence.
- Discovery can expose subresources like `deployments/status` and
  `deployments/scale`, which are not normal mirrored objects.
- Discovery does not mean a later `List()` call will succeed against every
  aggregated API server.

So the right mental model is: discovery tells us what resource types the API
server claims to serve, with enough metadata to map GVK and GVR. Object state,
permissions, and destructive reconciliation still require their own trust gates.

## How Much Theory Matters

In ordinary Kubernetes usage, the types usually line up:

- `apiVersion: v1`, `kind: ConfigMap` maps to `v1/configmaps`.
- `apiVersion: apps/v1`, `kind: Deployment` maps to
  `apps/v1/deployments`.
- A CRD with `spec.names.kind: Widget` and `spec.names.plural: widgets` appears
  in discovery as `example.com/v1`, kind `Widget`, resource `widgets`.

That practical reality is useful. It means the mapper should be boring in the
happy path and should not block the whole project on theoretical edge cases.

But pluralization cannot be guessed safely, and the edge cases matter exactly
where GitOps Reverser is most dangerous:

- Some resource names are irregular or non-obvious.
- Multiple resources can share a kind shape across groups or versions.
- A manifest can use an API version the current cluster no longer serves.
- A CRD can serve multiple versions for the same kind.
- A resource can be served but excluded by GitOps Reverser policy.
- Subresources can share familiar-looking kinds while not being objects we mirror.
- Aggregated APIs can appear in discovery but fail later when listed.

The conclusion is deliberately modest: do not build a giant schema system, but
also do not infer GVRs from kind strings or path layout. Use discovery/catalog
data when available; otherwise mark the mapping as unresolved and keep the
analyzer structure-only.

## Responsibilities

The mapping layer owns these decisions:

- Map a manifest GVK to one concrete served GVR.
- Map an event or object GVR to the served GVK used for manifest identity.
- Attach namespaced/cluster-scoped information to the mapping.
- Report whether the mapping is trusted, stale, ambiguous, disallowed, or
  unavailable.
- Tell the materialized manifest store whether a document belongs to a watched
  set for this GitTarget.
- Preserve the analyzer's no-cluster mode by allowing a nil implementation that
  records only structure.

The mapping layer does not:

- validate object schemas,
- convert one API version to another,
- prove RBAC,
- list objects,
- choose Git file placement,
- decide whether unwatched content is refused, reported, or pruned.

## Source Of Truth Versus Mapper Source

Kubernetes discovery is the source of truth for served GVK/GVR mappings. The API
server is the only authority that can say "this cluster serves kind `Deployment`
at `apps/v1` through resource `deployments`" or "this CRD's plural resource name
is `icecreamorders`." GitOps Reverser should not invent that relationship from
English pluralization, generated paths, or hard-coded assumptions.

`MapperSource` does **not** mean there are four competing truths. It describes how
this process obtained, cached, replayed, or intentionally declined to obtain
Kubernetes discovery data.

- `live-catalog`: the controller reads the in-process `APIResourceCatalog`, which
  is refreshed from Kubernetes discovery and shared with watch planning. The API
  truth is current cluster discovery, cached locally.
- `kubeconfig`: a CLI command contacts a cluster through kubeconfig, builds a
  temporary catalog, then maps through the same catalog semantics. The API truth
  is current cluster discovery, fetched on demand.
- `static-snapshot`: tests or offline review load a serialized catalog-shaped
  fixture. The API truth is a captured or declared snapshot, not live truth.
- `structure-only`: no API discovery data is available or desired; the analyzer
  only parses manifest structure. There is no API truth, so mapping is
  deliberately unknown.

That distinction is why `MappingStructureOnly` is not an error. It is the honest
answer for "we know this YAML looks like KRM, but we did not ask any API surface
what REST resource serves it." Conversely, when a mapper is backed by discovery
data, absence can only be trusted if the relevant group/version discovery is not
degraded.

## Interface

> **Status (2026-06-04): implemented.** The interface and the runtime-independent
> implementations live in `internal/mapping`
> (`ResourceMapper`, `StructureOnlyMapper`, `StaticSnapshotMapper`, and the shared
> `ResolveGVK` reduction); the catalog-backed implementation is
> `watch.CatalogMapper`, built on the
> catalog `byGVK`/`LookupGVK` additions
> ([`api_resource_catalog.go`](../../internal/watch/api_resource_catalog.go)).
> The doc warned the concrete Go names could move, and two did: to satisfy the
> repository's no-stutter lint, `MappingResult` is `mapping.Result` and
> `MappingStatus` is `mapping.Status`. The `Mapping*` status constants below kept
> their names. `internal/mapping` has no dependency on `internal/watch`, so the
> analyzer can resolve mappings without importing the watch manager.

The concrete Go names can move, but the interface should make the dependency
explicit:

```go
type ResourceMapper interface {
    Source() MapperSource
    Ready() MapperReadiness
    Generation() uint64

    GVRForGVK(ctx context.Context, gvk schema.GroupVersionKind) (Result, error)
}

type MapperSource string

const (
    MapperSourceLiveCatalog     MapperSource = "live-catalog"
    MapperSourceKubeconfig      MapperSource = "kubeconfig"
    MapperSourceStaticSnapshot  MapperSource = "static-snapshot"
    MapperSourceStructureOnly   MapperSource = "structure-only"
)

type MapperReadiness struct {
    Ready      bool
    Degraded   bool
    Generation uint64
    Reason     string
}

type Result struct {
    GVK schema.GroupVersionKind
    GVR schema.GroupVersionResource

    Namespaced bool
    Verbs      []string
    Preferred  bool
    Allowed    bool

    Status Status
    Reason string
}

type Status string

const (
    MappingResolved           Status = "Resolved"
    MappingUnserved           Status = "Unserved"
    MappingAmbiguous          Status = "Ambiguous"
    MappingDisallowed         Status = "Disallowed"
    MappingSubresource        Status = "Subresource"
    MappingCatalogUnavailable Status = "CatalogUnavailable"
    MappingDiscoveryDegraded  Status = "DiscoveryDegraded"
    MappingStructureOnly      Status = "StructureOnly"
)
```

`GVRForGVK` must require an exact group/version/kind match. It should not
silently map `extensions/v1beta1 Deployment` to `apps/v1 Deployment`, even though
a human understands the relationship. Version conversion is a later feature and
requires a real conversion source, not a REST mapping guess.

Resource-to-manifest identity is not part of this mapper contract. Delete planning
starts from the GitTarget folder's `ByResourceIdentity` inventory; if that inventory
does not already contain the resource, the planner does not re-derive a manifest
identity through a reverse lookup.

Errors should be reserved for implementation failures: discovery call failed,
snapshot could not be loaded, malformed static data, context cancellation.
Expected lookup outcomes should be returned as `Status` so callers can
make policy decisions without parsing error strings.

## Implementations

These implementations share the same mapping contract. They differ only in how
the catalog data is obtained and how much trust callers can place in freshness.

### Live Catalog Mapper

Used by the controller and watch manager.

This wraps the existing `APIResourceCatalog`. The catalog already carries GVR,
GVK, scope, verbs, preferred-version, subresource, allowed-policy, readiness, and
generation data. The main missing pieces are lookup methods/indexes for GVK and
status-rich mapping results.

Required catalog additions:

- `byGVK[schema.GroupVersionKind] -> []APIResourceEntry`
- exported lookup for exact GVK
- mapping status helpers that preserve degraded lookup state
- generation-aware result reporting

The live mapper must not call Kubernetes discovery directly. The catalog owns
discovery refresh and trust state; the mapper reads that trusted local view.

### Kubeconfig Discovery Mapper

> **Status (2026-06-04): deferred.** `MapperSourceKubeconfig` exists as a constant,
> but the kubeconfig-backed mapper itself is not built yet. The controller uses
> `live-catalog` and tests use `static-snapshot`, so nothing on the B3/M3/M6 path
> needs this; it lands with the optional CLI cluster-check mode (Implementation
> Order step 5).

Used by a CLI or offline command that is allowed to contact a cluster.

This implementation builds an `APIResourceCatalog` from a kubeconfig-backed
discovery client, then exposes the same mapping behavior as the live catalog
mapper. It gives humans a way to run the manifest analyzer in "check this folder
against this cluster's API surface" mode without starting the controller.

This mode can report mapping and watched/unwatched classification, but it should
still not list live objects unless a separate command explicitly asks for a full
plan against cluster state.

This is not a second interpretation of Kubernetes semantics. It is the same
discovery truth as `live-catalog`, just fetched by a short-lived command instead
of by the controller's continuously refreshed catalog.

### Static Snapshot Mapper

Used by tests, CI, and offline review.

This implementation loads a serialized API-resource catalog snapshot. The shape
should be close to `APIResourceEntry`, not raw Kubernetes discovery JSON, because
tests and design fixtures should express the project contract directly.

Static snapshots are also the right way to make manifest materialization tests
deterministic. A fixture can say "this cluster serves apps/v1/deployments and
v1/configmaps" without needing a kube-apiserver.

Because a static snapshot is not live discovery, it should be treated as an
explicit test/review input. It can model old clusters, partial catalogs, policy
exclusions, ambiguity, and degraded discovery on purpose, but it must not be
mistaken for proof about the cluster currently running the controller.

### Structure-Only Mapper

Used by the current `manifest-analyzer` default.

This implementation always returns `MappingStructureOnly`. It is not a failure.
It means the analyzer can inventory valid KRM, duplicate identities,
multi-document files, encryption boundaries, and GVK counts, but cannot decide
whether a document is watched or orphaned.

This is the mode that preserves the analyzer's current no-cluster promise.
It should never produce creates, deletes, watched/unwatched conclusions, or
destructive adoption decisions.

## Manifest Store Integration

The materialized store should carry both identities when a mapper can provide
them:

```text
ManifestIdentity = apiVersion + kind + namespace + name
GVK              = group + version + kind
ResourceIdentity = group + version + resource + namespace + name
mapping.Status   = resolved / unserved / ambiguous / ...
```

Store construction should do the cheap parse first:

1. Read YAML headers and manifest identity.
2. Parse `apiVersion` and `kind` into GVK.
3. Ask the injected mapper for GVR.
4. If resolved, populate the resource-identity index.
5. If unresolved, keep the document in the manifest-identity and GVK indexes and
   attach a diagnostic.

This keeps bounded materialization intact. Full YAML node trees are still built
only for documents a plan action touches.

## Watched Classification

Mapping a document to a GVR is not enough to say it is managed by a GitTarget.
The document must also match the GitTarget's effective watch selection.

Classification should be a second step:

```text
document GVK -> mapper -> document GVR
GitTarget rules -> RuleGVRResolver -> watched GVR set
document GVR in watched set? -> tracked/untracked/orphan decision
```

This intentionally reuses the existing WatchRule resolution semantics instead
of making the manifest layer interpret rules on its own.

Status categories:

| Category | Meaning |
|---|---|
| `tracked` | Mapping resolved and GVR is selected by this GitTarget. |
| `unwatched` | Mapping resolved but GVR is not selected by this GitTarget. |
| `unserved` | GVK is not served by trusted catalog data. |
| `ambiguous` | More than one served resource could match. |
| `disallowed` | Served, but excluded by GitOps Reverser resource policy. |
| `unknown` | Mapper is unavailable, degraded, or structure-only. |

Only `tracked` documents can be swept as managed resources. `unwatched`,
`unserved`, `ambiguous`, `disallowed`, and `unknown` documents are acceptance or
status facts; they are not delete targets by default.

## GitTarget Start Conditions

The GitTarget lifecycle should gain an explicit API-surface/mapping gate before
the initial snapshot can make destructive decisions.

Proposed condition:

```text
Type:    APIMappingReady
Reason:  Resolved | CatalogUnavailable | DiscoveryDegraded | MappingFailed
Message: API resource mapping is ready for all watched resource types
```

`Ready=True` should require this gate in addition to the existing validation,
encryption, snapshot, and event-stream gates.

Startup rule:

1. The catalog must have trusted initial data.
2. The GitTarget's WatchRules and ClusterWatchRules must resolve through
   `RuleGVRResolver`.
3. Every watched GVR needed for the initial snapshot must have a resolved watched-type
   table entry carrying its GVK.
4. Any manifest-store acceptance mode that needs watched/unwatched decisions must
   have a mapper source stronger than `structure-only`.
5. If discovery is degraded for a lookup scope that could affect the target, hold
   the target at `APIMappingReady=False` instead of starting a destructive
   snapshot from partial knowledge.

This does not mean every KRM document in the repository must be served before
GitTarget startup. It means the system must know enough to classify the watched
set it is about to manage. Unwatched or unserved documents can still be refused
by adoption policy without being deleted.

## Failure Policy

Mapping failures should be boring and visible:

- `CatalogUnavailable`: wait or fail closed; do not start initial snapshot.
- `DiscoveryDegraded`: preserve last-known-good mappings; do not turn absence
  into deletes.
- `Unserved`: refuse or report the document, depending on adoption policy.
- `Ambiguous`: ask for a more specific rule or static mapping; do not guess.
- `Disallowed`: surface as policy, not as "not served."
- `StructureOnly`: continue inventory-only analysis; skip watched/orphan
  classification.

The destructive invariant is the same as the watch/catalog architecture: an
inability to observe the API surface is not evidence that a resource should be
deleted from Git.

## Implementation Order

1. ✅ Add `byGVK` and exact GVK lookup to `APIResourceCatalog` (also `LookupGVR`
   and the `CatalogLookup` trust state).
2. ✅ Introduce the `ResourceMapper` interface and a catalog-backed implementation
   (`watch.CatalogMapper`).
3. ✅ Add a static-snapshot implementation for unit tests
   (`mapping.StaticSnapshotMapper`), alongside the structure-only mapper.
4. Change manifest store construction to accept a mapper and record the resolved
   `mapping.Status` per document.
5. Keep `manifest-analyzer` structure-only by default; add an optional
   kubeconfig/static-catalog mode later.
6. Add the GitTarget mapping readiness gate before initial snapshot planning.
7. Teach status/reporting to summarize mapped, unmapped, watched, unwatched, and
   degraded counts.

## References

- Kubernetes API discovery:
  https://kubernetes.io/docs/concepts/overview/kubernetes-api/#discovery-api
- Kubernetes API concepts:
  https://kubernetes.io/docs/reference/using-api/api-concepts/
- Kubernetes API definitions:
  https://kubernetes.io/docs/reference/kubernetes-api/definitions/
- client-go discovery:
  https://pkg.go.dev/k8s.io/client-go/discovery
- metav1 `APIResource`:
  https://pkg.go.dev/k8s.io/apimachinery/pkg/apis/meta/v1#APIResource
