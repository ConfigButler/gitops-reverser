# PR 2 — a cluster-wide selection must not collapse named-namespace scoping

> Phase 2 of [source-namespace addressing](README.md). **Depends on:**
> [PR 1](pr1-namespace-scoped-resync.md) — this PR is the first thing that makes a GitTarget watch one
> GVR in two namespaces at once, which is unsafe until the resync sweep is namespace-scoped.
> **Blocks:** PR 4 (the gate is only as good as the stream scoping underneath it) and PR 5.
> Bug fix — no API change, no CRD regeneration.
>
> **Status: landed.** `SnapshotNamespaces` is now `WatchedType.WatchScopes`, which returns every
> namespace scope including the cluster-wide `""` instead of collapsing to it, and both read sites
> project one stream per scope: `targetWatchSpecs` keys a watch per scope with that scope's own
> operation set, and `snapshotGVRsFromTable` emits one `snapshotGVR` per scope (its `namespaces`
> slice became a single `namespace`, matching `targetWatchKey`). The old assertion was deleted and
> replaced; the three replacements fail against the pre-fix collapse.

## The defect

`SnapshotNamespaces()` returns `nil` when a `WatchedType` has the `""` namespace key, and `nil`
means *all namespaces* at every read site
([watched_type_table.go:78-92](../../../internal/watch/watched_type_table.go#L78-L92)). So a
WatchRule scoped to one namespace and a ClusterWatchRule scoped cluster-wide, on the **same GVR and
the same GitTarget**, fold into one `WatchedType` — and the cluster-wide entry wins. The named
namespace survives only in the plan hash.

The operation sets collapse the same way, not just the namespaces: `targetWatchSpecs` uses
`operationSpec(wt.NamespaceOps[""])` and discards the per-namespace op sets
([target_watch.go:214-224](../../../internal/watch/target_watch.go#L214-L224)). A `CREATE`-only
named rule co-resident with an `UPDATE` cluster-wide rule loses its filter too.

### Why it matters to this workstream

Under [PR 4](pr4-source-namespace-field.md) this is a gate bypass. A ClusterWatchRule may
legitimately select every source namespace once its GitTarget passes provider admission — but a
co-resident WatchRule must not silently inherit that cluster-wide stream. Otherwise a WatchRule
authorized only for `repo-config` receives events from every namespace the credential can read, and
its `allowedSourceNamespaces` check passed only *before* the data plane widened it.

[PR 5](pr5-clusterwatchrule-source-ceiling.md) removes the `""` key **for the namespaced selections**
of any target with a declared ceiling — `scope: Cluster` rules keep emitting `""`, because a
namespace allow-list cannot constrain cluster-scoped types. So the collapse cannot trigger for a
namespaced GVR under a ceiling, but a target that mirrors a GVR both cluster-scoped and namespaced is
not covered by that, and the far more common undeclared case is not covered at all. This PR is what
governs both.

### This behavior is currently asserted as intended

`TestBuildWatchedTypeTable_ClusterWideOverridesNamedNamespaces`
([watched_type_table_test.go:64-82](../../../internal/watch/watched_type_table_test.go#L64-L82))
documents it as "matching the historic `gvrSnapshotEntry` collapse". So this is a design-intent
versus security-intent conflict, not an oversight. The fix must consciously **replace** that test,
not work around it — leaving it green would mean the fix did not land.

## Verified mechanism

`buildWatchedTypeTable`
([watched_type_table.go:128-155](../../../internal/watch/watched_type_table.go#L128-L155)) is a pure
union: both the `""` key (ClusterWatchRule) and `"team-a"` (WatchRule) land in the same
`namespaceOps` map for the same GVR. The collapse happens later, at *read* time — `ClusterWide()`
merely tests for presence of the `""` key
([watched_type_table.go:73-76](../../../internal/watch/watched_type_table.go#L73-L76)), so
`SnapshotNamespaces()` short-circuits to `nil`. Two read sites consume that `nil` as all-namespaces:

- `targetWatchSpecs` ([target_watch.go:214-224](../../../internal/watch/target_watch.go#L214-L224))
- `snapshotGVRsFromTable` ([scope_resolve.go:180](../../../internal/watch/scope_resolve.go#L180))

The `team-a` entry survives in `NamespaceOps` for the plan hash only.

## The fix

Keep cluster-wide and named selections as distinct streams for the same GVR, or subtract the named
scope from the cluster-wide one — and preserve the per-namespace operation sets either way. Both
read sites above must agree; a fix applied to one of them is worse than no fix, because the plan hash
and the running streams then disagree.

> **Do not land this before [PR 1](pr1-namespace-scoped-resync.md).** Distinct concurrent streams for
> one GVR is precisely the fan-out the resync sweep mishandles: the named stream's replay carries a
> `desired` set for one namespace, while the sweep it triggers is scoped to the whole type. Fixing
> the collapse first therefore converts a silent over-watch into silent deletion of the cluster-wide
> stream's manifests.

## Tests

- **Replacement for the old assertion:** a WatchRule scoped to `team-a` and a cluster-wide
  ClusterWatchRule on the same GVR and GitTarget must **not** collapse to one all-namespaces stream.
  This replaces `TestBuildWatchedTypeTable_ClusterWideOverridesNamedNamespaces`.
- **Operation sets:** a `CREATE`-only named rule co-resident with an `UPDATE` cluster-wide rule
  preserves both op sets.
- **Both read sites:** assert on `targetWatchSpecs` *and* `snapshotGVRsFromTable`, so a fix that
  lands in one path only is caught here rather than as a resync anomaly later.

## Done when

- The old test is deleted, not skipped, and its replacement asserts non-collapse.
- Named and cluster-wide streams for one GVR are independently observable in both read paths.
- `task lint`, `task test`, `task test-e2e` pass.
