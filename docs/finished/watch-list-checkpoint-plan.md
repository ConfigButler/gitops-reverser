# WATCH-first checkpoint materialization

> Status: IMPLEMENTED on `poc/redis-copy` (2026-06-13). `mirrorTypeObjects` now prefers a
> WATCH-first streaming-list (`streamTypeObjects`) with the consistent `LIST`
> (`listTypeObjects`) as fallback; the bookmark RV pins the checkpoint and a partial watch never
> marks a type `Synced`. A `~60s` stream deadline (`streamCheckpointTimeout`) backstops a backend
> that accepts the watch but never emits the `initial-events-end` bookmark. The process-local
> "GVR requires LIST" cache from the plan below is deliberately deferred (the plan called it
> optional for the first cut).
> Scope: `internal/watch/mirrorTypeObjects`, the demand-driven per-type checkpoint fill.
> Related:
> [materialization-tail-and-live-readiness-review.md](./materialization-tail-and-live-readiness-review.md)
> Rec 6,
> `audit-log-ingestion-and-ordering.md`,
> [../upgrade-finding.md](../design/upgrade-finding.md).

## Goal

Replace the current checkpoint `LIST` with a small WATCH-first path using Kubernetes streaming-list
semantics:

```text
watch=true
sendInitialEvents=true
resourceVersionMatch=NotOlderThan
allowWatchBookmarks=true
```

The checkpoint still produces the same output as today: complete objects for one GVR stored through
`ReplaceTypeObjects`, plus the revision `R` that the `Materializer` records as `Synced`.

The important rule: **keep the existing consistent `LIST` as fallback**. WATCH-first is an
efficiency and cleaner-anchor improvement, not a reason to stop serving clusters or aggregated APIs
that do not implement streaming-list correctly.

This plan assumes a recent Kubernetes API server. We are not designing compatibility for old
kube-apiserver versions that predate streaming-list support; the fallback exists for non-conformant
extension/proxy paths in otherwise supported clusters.

## Current Path

`runTypeCheckpointSync` calls `mirrorTypeObjects`, which currently does a plain dynamic-client list:

```go
list, err := dc.Resource(gvr).List(ctx, metav1.ListOptions{})
```

That is correct but heavier for large tracked types. It also pins the checkpoint to the collection
`resourceVersion` from the LIST, rather than to the `initial-events-end` bookmark used by
streaming-list mode.

## Proposed Shape

Keep `mirrorTypeObjects` as the public orchestration point, but split the fill into two helpers:

```text
mirrorTypeObjects
  ├─ streamTypeObjects(ctx, gvr)  # preferred
  └─ listTypeObjects(ctx, gvr)    # fallback and existing behavior
```

`streamTypeObjects` should:

1. Open `dc.Resource(gvr).Watch(ctx, metav1.ListOptions{...})` with:
   - `SendInitialEvents: ptr.To(true)`
   - `ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan`
   - `AllowWatchBookmarks: true`
2. Fold initial `ADDED` events into the same `map[string]string` written today.
3. Stop only when it receives a bookmark with
   `metadata.annotations["k8s.io/initial-events-end"] == "true"`.
4. Use that bookmark object's `resourceVersion` as checkpoint revision `R`.
5. Return `(items, R, nil)` to `mirrorTypeObjects`.

The live events after the bookmark are intentionally ignored by this helper. Freshness after `R`
continues to belong to the Redis audit tail and the periodic/heal reconcile path.

## LIST Fallback

Fallback to the existing LIST path when WATCH-first cannot prove it has a complete initial set.

Fallback triggers:

- the server rejects `sendInitialEvents` / `resourceVersionMatch=NotOlderThan`;
- the server rejects or closes the watch before the initial-events-end bookmark;
- the watch times out before the initial-events-end bookmark;
- the returned watch stream contains malformed objects that make the initial set untrustworthy.

Do not fallback on ordinary context cancellation or shutdown. That should end the checkpoint sync.

Keep the fallback local to this checkpoint attempt. A process-local cache of "GVR requires LIST
fallback" is useful but optional for the first cut; it can avoid paying the watch timeout on every
hourly re-anchor for a known non-conformant type.

## Which Types Need This

WATCH-first is useful for any type with many tracked objects, because it avoids forcing the apiserver
and controller through one large list response on every checkpoint/re-anchor. The most likely wins
are:

- `configmaps` and `secrets` when watched across busy namespaces;
- CRDs with many instances;
- cluster-scoped inventory-like types when a `ClusterWatchRule` follows them;
- aggregated API resources that correctly implement streaming-list.

The fallback is primarily for:

- aggregated API servers with hand-written `Watch` implementations that do not emit the
  `k8s.io/initial-events-end` bookmark;
- APIService/proxy paths that pass normal watch events but reject or mishandle
  `sendInitialEvents`.

Standard kube-apiserver-backed resources, including normal built-ins and CRDs, should be expected to
take the WATCH-first path. Aggregated APIs are the risk surface. This matches the production warning
captured in [upgrade-finding.md](../design/upgrade-finding.md): client-go can wait forever for the special
bookmark if an aggregated backend never emits it.

## Tests

Unit tests should not rely on the dynamic fake client magically supporting streaming-list. Add a
small test seam instead:

- a fake watcher that emits existing objects as `ADDED`, then a bookmark annotated with
  `k8s.io/initial-events-end`;
- a fallback test where the watch returns an unsupported/rejected error and `listTypeObjects` is used;
- a timeout/closed-before-bookmark test proving the checkpoint does not mark the type `Synced` from a
  partial watch result unless the LIST fallback succeeds;
- a regression test that the stored checkpoint revision is the bookmark RV, not the last object RV.

E2E plan:

1. Add a focused case to the existing `aggregated-api` lane or a new `watch-list-checkpoint` label.
2. Pre-create two resources before applying the WatchRule.
3. Apply the WatchRule/GitTarget and wait for `GitTarget` `Synced=True`.
4. Assert both pre-created resources are present in Git from the reconcile commit.
5. For fallback coverage, use the existing Wardle aggregated API if it cannot produce the
   initial-events-end bookmark; otherwise keep fallback coverage at unit/integration level unless we
   add a deliberately non-conformant test APIService.

The E2E should prove the real-apiserver happy path. The fallback path is better covered with a
controlled fake watcher unless the test cluster intentionally installs a broken aggregated API.

## Observability

Each completed fill reports which path it took, so the fallback surface is visible without
guessing per cluster (support is per-API-path, not per-cluster — an aggregated backend can fail
streaming-list while the core apiserver supports it):

- **Metric** — `gitopsreverser_materialization_checkpoint_fills_total{path="watch"|"list"}`, a
  low-cardinality counter (2 series). A rising `list` share is the fallback surface; the
  fallback rate is `list / (watch + list)`. Pair it with the log line below to find the type.
- **Log** — `objects-mirror: snapshot loaded ... path=watch|list` per GVR on every fill; the
  reason for a fallback is logged at `-v=1`: `objects-mirror: WATCH-first unavailable, falling
  back to LIST reason=...`.

Operators can also confirm a cluster's support directly: the server-side feature gate via
`kubectl get --raw /metrics | grep -i watchlist` (expects
`kubernetes_feature_enabled{name="WatchList"} 1`), or an empirical probe of one resource for the
`k8s.io/initial-events-end` bookmark. The `WatchList` gate is alpha in Kubernetes 1.27 and beta
(on by default) from 1.32.

## Acceptance Criteria

- `mirrorTypeObjects` prefers WATCH-first and stores the same object envelope format as today.
- A successful watch-list pins `R` from the `initial-events-end` bookmark.
- A non-conformant watch path falls back to the existing LIST behavior.
- No checkpoint is marked `Synced` from a partial watch.
- Existing reconcile/tail behavior is unchanged after the checkpoint lands.
- Each fill reports its path via `gitopsreverser_materialization_checkpoint_fills_total{path}`
  and the per-GVR `snapshot loaded ... path=` log line.
