# Initial Seed via client-go WatchListClient

> Status: proposed — not yet implemented
> Scope: `internal/watch` snapshot path, dynamic informer construction, the
> client-go `WatchListClient` feature gate
> Related: [`docs/upgrade-finding.md`](../upgrade-finding.md),
> [`docs/serious-bug/cozystack-bugreport.md`](../serious-bug/cozystack-bugreport.md)

## Goal

Make initial cluster-state seeding flow through a single, cache-backed source of
truth — the dynamic informer cache that `internal/watch` already maintains —
and stop issuing a separate hand-rolled `LIST` per GVR per namespace whenever a
`GitTarget` needs a snapshot.

Use the client-go `WatchListClient` feature (streaming-list, on by default
since client-go v1.35 / v0.36.x) as the *transport* that hydrates that cache,
rather than something to fight against. The bookmark-warning we observed in
production ([`docs/upgrade-finding.md`](../upgrade-finding.md)) was a hint that
we had silently opted into streaming-list without redesigning the seed path
around it; the right response is to redesign, not to disable the gate.

## Assumption

This plan assumes that every aggregated apiserver we watch implements the
WatchList contract correctly — i.e. honors `SendInitialEvents=true` and emits
the `k8s.io/initial-events-end` bookmark. Standard kube-apiserver has done
this since 1.27. Aggregated APIs are the risk surface; see the
[Aggregated-API caveat](#aggregated-api-caveat) section below.

If the assumption proves false in a given cluster, the per-GVR fallback in
Phase B keeps the system functional for misbehaving resources without giving up
the benefits on the rest.

## Today's state — duplicated seeding

Two seed paths run side-by-side:

1. **Implicit informer seed.**
   [`internal/watch/manager.go:977-981`](../../internal/watch/manager.go#L977-L981)
   creates a `dynamicinformer.NewDynamicSharedInformerFactory` per GVR /
   namespace. client-go performs its own `LIST` (or, with `WatchListClient=true`,
   a streaming-list) and hydrates an in-memory `cache.Store`. Only the
   per-event callbacks are wired through `EventRouter`; the populated store is
   otherwise unused.

2. **Explicit by-hand LIST.**
   [`GetClusterStateForGitDest`](../../internal/watch/manager.go#L461) →
   [`listResourcesForGVR`](../../internal/watch/manager.go#L630-L672) calls
   `dc.Resource(gvr).List(...)` again, per matching namespace, against the
   apiserver. This fires every time a `GitTarget` is snapshotted — rule
   changes, periodic re-snapshot, controller restart — and re-LISTs state that
   the informer cache already holds.

For any GVR that already has a running informer, this is two full
materializations of the same cluster state per snapshot: one cached and
unused, one re-LISTed and thrown away.

## The decision

- **Embrace `WatchListClient`.** Keep the client-go feature gate on. Do not
  ship the `--disable-watch-list-client` flag that the earlier upgrade-finding
  document sketched as a workaround.
- **Snapshots read from the informer cache, not the apiserver.** The
  hand-rolled `LIST` path goes away. The dynamic informer becomes the single
  source of truth for cluster state, hydrated by streaming-list semantics on
  the wire.
- **Treat aggregated-API conformance as a known risk and verify it
  explicitly.** See the caveat section; the work item is to audit every
  aggregated API we watch before turning this loose in production.

## Phase A — snapshot from the informer cache

Pure refactor inside `internal/watch`. No client-go feature-gate change yet.

- Replace
  [`listResourcesForGVR`](../../internal/watch/manager.go#L630-L672) with a
  `snapshotFromCache(gvr, namespaces)` helper that calls
  `informer.GetStore().List()` (or a `cache.NewGenericLister`-backed `Lister`
  so namespace filtering stays cheap).
- Snapshot-time work becomes O(in-memory walk), not O(`LIST` ×
  namespaces).
- If a snapshot is requested for a GVR with no active informer yet (rule just
  added, reconcile hasn't run): start the informer on-demand,
  `WaitForCacheSync`, then read the cache. The "initial events" stream *is*
  the snapshot, which is exactly the shape streaming-list optimizes for in
  Phase B.
- `dynamicClientFromConfig`'s seed-only role at
  [`manager.go:309-326`](../../internal/watch/manager.go#L309-L326) goes away.
  The informer factory has its own client; the dynamic client only remains
  for *informer construction*.
- The "refusing to snapshot a partial cluster view" guard at
  [`manager.go:535-540`](../../internal/watch/manager.go#L535-L540) is
  preserved — it now triggers on cache-sync timeout or per-GVR informer
  failure instead of `LIST` failure.

Sketch of the seam:

```go
// Before: re-LISTs the apiserver, per namespace, on every snapshot
list, err := dc.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})

// After: reads the already-hydrated informer cache
objs, err := m.cacheForGVR(gvr).ByNamespace(ns).List(labels.Everything())
```

This phase is independently shippable and is the largest correctness /
performance win. It does not depend on the feature-gate decision; it just
makes Phase B safe to land afterwards.

## Phase B — turn `WatchListClient` on intentionally

Once the snapshot path is cache-backed, streaming-list becomes net-positive
in three ways:

- Cheaper initial sync: one chunked watch instead of a single mega-LIST
  response that the apiserver materializes whole in memory.
- Apiserver-side relief, especially for large resources (Secrets,
  ConfigMaps).
- The cache is consistent with watch events the moment `HasSynced` flips —
  no race window where we re-LIST and miss in-flight deletes.

Work items:

- Keep the feature gate on for the standard API surface. Do not introduce a
  binary-level kill switch.
- For aggregated APIs that we discover (at startup or runtime) to be
  non-conformant: build the informer with a custom `ListWatch` that bypasses
  `ToListWatcherWithWatchListSemantics`. That means dropping below
  `dynamicinformer.NewDynamicSharedInformerFactory` for those GVRs and
  constructing `cache.NewSharedIndexInformer` directly with a plain
  `ListWatch`.
- Detection is **per-GVR**, not per-`APIService`. Cozystack proved why:
  one aggregated server can host both conformant and non-conformant resources
  in different API groups (see the bug report).
- Detection strategy: start the informer with watch-list semantics; if
  `HasSynced` doesn't flip within a deadline (e.g. 60 s, with the
  missing-bookmark warning as a strong signal), tear it down, fall back to a
  plain `ListWatch` for that GVR, and remember the fallback in a process-local
  cache so subsequent restarts use the safe path immediately.
- Add an e2e lane that watches at least one aggregated-API resource and
  asserts the informer syncs and the snapshot is complete.

## Phase C — delete the legacy seed code

After A and B land:

- `listResourcesForGVR`, the per-namespace LIST fan-out in
  `getNamespacesForGVR`, the seed-only dynamic client, and the comments
  about "by-hand" seeding all go.
- `GetClusterStateForGitDest` shrinks to roughly: resolve GVRs → ensure
  informers running and synced → iterate caches.
- The reconciler-startup story becomes "rules → GVRs → start informers →
  snapshot is implicit," which is what the existing design docs were already
  pointing toward but never reached.

## Aggregated-API caveat

This plan **only works if the aggregated apiservers we watch implement the
WatchList contract correctly**. Specifically, each aggregated `Watch`
implementation must:

1. Read `options.SendInitialEvents` from the internal `ListOptions`.
2. Deliver all matching existing objects as `ADDED` events.
3. Emit a final `watch.Bookmark` event whose object carries
   `metadata.annotations["k8s.io/initial-events-end"] = "true"` and a
   `ResourceVersion` equal to the last RV seen during the initial set.
4. Continue with live events thereafter.

Standard kube-apiserver and resources backed by `genericregistry.Store` get
this for free. Hand-written `rest.Storage` implementations in aggregated
servers — exactly the kind that show up in projects like Cozystack — must
implement it themselves. The Cozystack bug report
([`docs/serious-bug/cozystack-bugreport.md`](../serious-bug/cozystack-bugreport.md)) is a worked
example: one API group conformant, one not, in the same binary.

Before relying on Phase B in any production cluster, we need to audit every
aggregated API surface present in that cluster. The audit list per cluster is:

- Enumerate `APIService` objects (`kubectl get apiservices`).
- For each non-local `APIService` we actually watch, confirm one of:
  - the backing implementation is `genericregistry.Store`-based (built on
    `k8s.io/apiserver`'s default registry — emits the bookmark by
    construction), **or**
  - the project has explicit `SendInitialEvents` handling in its `Watch`
    method (grep for `SendInitialEvents` and
    `k8s.io/initial-events-end` in their source), **or**
  - the GVR is on our explicit fallback list and gets the plain-ListWatch
    informer path from Phase B.

This audit is not a one-time activity for the project; it's an operational
check the first time we deploy into a cluster with unfamiliar aggregated APIs.
The Phase B runtime fallback exists so a missed audit degrades gracefully
instead of hanging the reflector.

## Open questions

- **How aggressively should the Phase B detection probe?** A 60 s deadline is
  generous for healthy apiservers but stalls every controller restart by that
  amount when an aggregated API is broken. Options: a shorter deadline with a
  faster fallback; or a configurable per-GVR override in the `WatchRule` /
  `ClusterWatchRule` spec; or a one-time at-startup probe that records
  conformance per `APIService`.
- **Where does the fallback memory live?** Process-local is enough for a
  single-pod controller, but if we move to leader-elected HA the fallback
  decision should arguably be a `Condition` on the `WatchRule` so a new pod
  doesn't re-pay the detection cost on every failover.
- **Cache memory cost.** Today the snapshot LIST path holds objects only for
  the duration of the snapshot. Cache-backed snapshots hold them for the life
  of the informer. For resources with extreme cardinality (e.g. cluster-wide
  Pod watches we'd never want), the `WatchRule` matching is already the gate;
  this just makes it explicit that adding a rule has a steady-state memory
  cost.

## Out of scope

- Multi-cluster snapshotting. The dynamic informer is per-cluster; multi-
  cluster seed is a separate design.
- Server-side filtering beyond label selectors. `WatchListClient` does not
  change the filter contract.
- The audit-ingestion pipeline. Snapshot path and audit path are independent;
  this plan does not touch
  [`docs/design/audit-metrics-overhaul-plan.md`](audit-metrics-overhaul-plan.md).
- A binary-level "disable WatchListClient" flag. Explicitly rejected by the
  decision above. Operators who need to disable the gate in an emergency can
  still set `KUBE_FEATURE_WatchListClient=false` via the chart's `.Values.env`
  ([`charts/gitops-reverser/templates/deployment.yaml:107-125`](../../charts/gitops-reverser/templates/deployment.yaml#L107-L125));
  we just won't ship that as a first-class flag.

## Suggested order of work

1. **Phase A** as one PR — refactor `GetClusterStateForGitDest` onto informer
   caches. Self-contained. Largest single win.
2. **Phase B detection + fallback** as a follow-up PR — per-GVR conformance
   probe and plain-`ListWatch` fallback. Add an e2e lane that exercises a
   known-good aggregated API.
3. **Phase C cleanup** PR — delete the legacy seed code now that nothing
   calls it.
4. **Aggregated-API audit checklist** in `docs/` (or an extension of
   [`docs/aggregated-api-guide.md`](../aggregated-api-guide.md)) so operators
   know what to verify before turning Phase B loose in their cluster.
