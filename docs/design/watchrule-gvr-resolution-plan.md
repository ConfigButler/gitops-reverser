# Implementation Plan: WatchRule GVR Resolution

## Error

CI run `26272823512` failed on `E2E (quickstart)` (`E2E (full)` was canceled by
fail-fast). The quickstart test timed out after 180s waiting for a ConfigMap
snapshot file that was never written. The controller log showed:

```text
aborting cluster snapshot ...: failed to list /v1, Resource=deployments:
  the server could not find the requested resource
```

The quickstart `WatchRule` template (`test/e2e/templates/watchrule.tmpl`) uses
bare resource names:

```yaml
rules:
- resources: ["deployments", "services", "configmaps", "secrets"]
- resources: ["ingresses"]
```

The failure comes from inconsistent rule expansion:

- The informer planner in `internal/watch/gvr.go` defaults omitted `apiGroups`
  to core and omitted `apiVersions` to `v1`.
- The snapshot planner in `internal/watch/manager.go` does the same.
- `deployments` is served from `apps/v1`, not core `/v1`.

That bad expansion has two effects:

1. The informer path plans `/v1/deployments`, then discovery filtering silently
   drops the wrong GVR. The requested deployment watch never starts.
2. The snapshot path tries to list `/v1/deployments`. Since commit `48e7d9d`
   snapshot list failures abort instead of being logged and skipped, one bad GVR
   prevents the snapshot for every other resource on that GitTarget.

Commit `48e7d9d` made an existing resolution bug visible. Its stricter snapshot
behavior is still correct for a served resource that fails to list: a partial
object snapshot looks like deletions to the Git mirror.

## Status

Implemented. `APIResourceCatalog` and `RuleGVRResolver` landed in commit
`aa2e463`; the pre-catalog discovery code they replaced was removed in the
follow-up cleanup (see
[`watch-gvr-resolution-cleanup.md`](./watch-gvr-resolution-cleanup.md)). This
document is retained as the design record.

## Requirements

### Keep bare resource rules working

The existing WatchRule style must work again:

```yaml
rules:
- resources: ["deployments"]
```

On a normal cluster that rule should resolve to the served Deployment resource,
for example `apps/v1/deployments`. It must not be rewritten to core
`/v1/deployments` just because `apiGroups` and `apiVersions` were omitted.

`WatchRule` and `ClusterWatchRule` use a rule shape inspired by Kubernetes
resource trigger rules such as audit
[`PolicyRule` and `GroupResources`](https://kubernetes.io/docs/reference/config-api/apiserver-audit.v1/#audit-k8s-io-v1-PolicyRule).
GitOps Reverser defines omitted fields for its rules as:

- Omitted `apiGroups` means all groups.
- Explicit `apiGroups: [""]` means the core API group.
- Omitted `apiVersions` means all versions.
- Explicit group and version selections remain constraints.
- `resources` entries are resource names, not guesses about group or version.

The exception is ambiguity. If more than one served API group exposes a resource
named `deployments`, the bare rule no longer identifies one resource type. The
resolver must report the matching groups and require the user to set
`apiGroups` explicitly instead of choosing one.

This choice must be documented on the WatchRule and ClusterWatchRule CRD fields
as part of the implementation. It is not safe to rely on readers inferring it
from generic Kubernetes `apiGroups` conventions.

### Resolve one concrete watch target safely

Snapshot and informer planning need concrete GVRs. Resolution from a rule to
concrete GVRs must be consistent across:

- informer GVR planning,
- snapshot GVR planning,
- namespace planning for namespaced WatchRules,
- WatchRule and ClusterWatchRule status feedback.

For an omitted-group resource declaration:

- One matching served resource type resolves to that resource type.
- No matching resource type in a trusted lookup scope is `NotServed`.
- More than one matching served resource type is `Ambiguous`.
- A degraded lookup scope that could change the answer is
  `DiscoveryDegraded`.

Ambiguous bare resource names must fail resolution with actionable feedback
instead of choosing arbitrarily. The resolver should report every matching
group it found and ask the user to set `apiGroups` explicitly. The ambiguous
rule is not tracked until it is made specific; previously tracked files may be
deleted by the next authoritative reconcile.

### Keep destructive snapshots trustworthy

The Kubernetes API server is authoritative for:

- which resource types are currently served,
- the object state returned for a served resource type.

Those two decisions have different failure modes:

- If a resource type is absent from the trusted API resource catalog, it is no
  longer tracked.
- If a resource type is present in the trusted catalog but its object `List()`
  fails, the snapshot must abort. Returning the other objects would produce a
  partial cluster view.

The source used for served resource types must therefore retain trust at the
group/version level. A discovery failure for one aggregated API must not turn
absence from that failed group/version into `NotServed` or delete unrelated Git
files, and it must not prevent cleanly discovered groups from resolving.

## Single Source Of Truth

### Proposed name: `APIResourceCatalog`

Create a shared `APIResourceCatalog` in `internal/watch`.

This name is deliberate:

- `APIResource` matches the Kubernetes discovery vocabulary and the data shape
  carried by `metav1.APIResource`.
- `Catalog` says this is an indexed view of resource types, not a snapshot of
  live Kubernetes objects.
- It avoids overloading `inventory`, which this project already uses near object
  snapshots and reconciliation state.

The catalog is GitOps Reverser's in-memory view of the trusted Kubernetes API
surface. Rule resolution should read from it instead of each caller performing
its own discovery lookup or applying its own core `/v1` defaults.

The detailed discovery model, refresh triggers, trust rules, and
streaming-list note live in
[`kubernetes-api-resource-catalog.md`](./kubernetes-api-resource-catalog.md).

### Catalog responsibilities

`APIResourceCatalog` should:

- Build the first catalog from Kubernetes discovery with per-group/version trust.
- Retain concrete resource entries by GVR and GVK-related metadata.
- Preserve preferred-version information for rules that need one concrete GVR
  from an omitted or wildcard version declaration.
- Expose indexed lookup by resource name, group/resource, and concrete GVR.
- Expose filtered views for resources that GitOps Reverser can list and watch.
- Refresh when the API surface may have changed.
- Track discovery confidence per group/version so lookups can distinguish a
  cleanly discovered absence from a degraded discovery gap.
- Preserve the last trusted entries for failed group/versions across refreshes.

Catalog refresh triggers should include CRD and `APIService` changes plus a
recovery path for missed or non-object signals. Those watches invalidate the
catalog; discovery rebuilds the replacement catalog.

### Catalog entry shape

A catalog entry needs at least:

```text
APIResourceEntry
  GVR:         group, version, resource
  GVK:         group, version, kind
  Namespaced:  bool
  Verbs:       set[string]
  Preferred:   bool
  Subresource: bool
  Allowed:     bool
```

The catalog can keep the full served API surface and let callers request
operation-specific candidates. Current WatchRule resource planning should ignore
subresources and require the `list` and `watch` verbs where live watch planning
needs them.

`Allowed` is GitOps Reverser policy, not Kubernetes discovery state. The current
built-in exclusion list in `internal/watch/discovery.go` should initialize that
policy bit until the policy is redesigned or made configurable. A default
excluded resource should still remain present in the catalog as served; it
should resolve as explicitly disallowed instead of silently looking
undiscovered.

Full-version discovery and preferred-version selection may require more than one
discovery view. If the catalog uses `ServerGroupsAndResources()` for all served
versions, it must merge preferred-version data from group or preferred-resource
discovery and apply the same degraded group/version handling to that merge.

## Rule Resolver

Add a catalog-backed rule resolver over `APIResourceCatalog`.

Working names:

- `APIResourceCatalog` for the trusted API-surface data.
- `RuleGVRResolver` for the resolver that applies WatchRule semantics to that
  catalog.

`RuleGVRResolver` should return concrete GVRs and structured misses:

```text
Resolve(ruleGroups, ruleVersions, resource) -> ([]GVR, []ResolveMiss)
ResolveMiss
  Resource
  Reason
  Detail
```

Initial miss reasons:

| Reason | Meaning |
|---|---|
| `NotServed` | No resource type in the trusted catalog matches the declaration |
| `Ambiguous` | An omitted-group resource name matches more than one served resource type |
| `Disallowed` | The resource type is served but GitOps Reverser policy excludes it |
| `WildcardGroup` | Planning cannot yet expand an explicit `"*"` group safely |
| `WildcardResource` | Planning cannot yet expand an explicit `"*"` resource safely |
| `CatalogUnavailable` | No resolver source is ready for a lookup that needs API discovery |
| `DiscoveryDegraded` | The relevant group/version discovery state is not trusted enough to return `NotServed` |

Expected resolution behavior:

| Declaration | Behavior |
|---|---|
| Concrete group and concrete version | Validate and use the matching catalog resource |
| Explicit core group `""` | Search only the core group |
| Omitted group | Search all served groups for the named resource |
| Omitted version or version `"*"` | Choose the catalog's preferred served version for the resolved group/resource |
| Multiple resource-name matches across groups | Return `Ambiguous` and require explicit `apiGroups` |
| Matching resource with `Allowed=false` | Return `Disallowed`; do not collapse policy exclusion into `NotServed` |

## Implementation Work

### 1. Add `APIResourceCatalog`

- Add the catalog in `internal/watch`.
- Replace the current point discovery index with catalog construction and lookup.
- Build from discovery across served group/versions, retain preferred-version
  information, and track refresh trust per group/version.
- Add API-surface refresh triggers and a refresh path that re-runs rule planning
  after a catalog generation changes trusted entries.

### 2. Add `RuleGVRResolver`

- Resolve WatchRule and ClusterWatchRule resource declarations against the
  catalog.
- Remove the naive omitted-group-to-core and omitted-version-to-`v1` defaults.
- Keep unresolved results structured so snapshot planning and status reporting
  do not parse strings.

### 3. Wire snapshot planning

`GetClusterStateForGitDest` should plan snapshot GVRs through the resolver.

- `NotServed`, `Ambiguous`, and `Disallowed` resource declarations are recorded
  and omitted from the planned resource types under the API-server-authoritative
  model.
- `WildcardGroup` and `WildcardResource` still abort until the resolver can
  expand those wildcard forms without a partial view.
- Object list failure for any planned served GVR still aborts the snapshot.

### 4. Wire informer planning

`ComputeRequestedGVRs` should use the same resolver.

- Bare declarations such as `deployments` and `ingresses` resolve to their
  served non-core GVRs.
- The discovery filter becomes a catalog-backed safety filter or is removed if
  resolution already proves the same contract.
- The namespace-planning matchers in `manager.go` must use the same omitted
  group/version semantics. Otherwise a namespaced WatchRule that resolves to a
  non-core GVR can accidentally fall back to cluster-wide informer planning.

### 5. Surface resource resolution status

The WatchRule and ClusterWatchRule controllers should report resource resolution
after dependency validation:

- Add a `ResourcesResolved` condition.
- Use `Resolved` and `UnresolvedResources` reasons.
- Keep `Ready` focused on GitTarget/GitProvider wiring. A rule with resolved and
  unresolved resources can still be `Ready=True` while reporting degradation in
  `ResourcesResolved`.
- Emit warning events for unresolved resource declarations with precise reasons.
- Surface `Disallowed` as policy feedback so a served resource excluded by
  defaults or later configuration is not reported as missing from Kubernetes.
- Re-evaluate resource status when a catalog generation changes trusted
  resolution.
- Requeue unresolved resources as a retry fallback while catalog refresh signals
  settle or are degraded.

Example feedback:

```text
"ingresses": not served by any API group
"widgets": ambiguous resource name; set apiGroups to one of ["team-a.example.io", "team-b.example.io"]
"jobs": served as "batch/v1/jobs" but excluded by GitOps Reverser watch policy
```

Optional follow-up: add structured `status.unresolvedResources` data and a
printcolumn if plain `kubectl get watchrule` should expose resolution health.

## Tests

- Unit — `APIResourceCatalog`: builds complete lookup indexes, records
  preferred versions, rejects subresource candidates for current watch planning,
  records built-in watch exclusions as `Allowed=false`,
  preserves last trusted entries for failed group/versions across refreshes, and
  distinguishes cleanly discovered absence from degraded discovery.
- Unit — `RuleGVRResolver`: omitted group resolves `deployments` to `apps/v1`;
  explicit core group stays core; missing resource returns `NotServed`; multiple
  group matches return `Ambiguous`; a served policy exclusion returns
  `Disallowed`; unsupported wildcard forms return their explicit miss reasons.
- Unit — catalog unavailable: snapshot and informer planning fail closed before
  the catalog can answer a lookup.
- Unit — snapshot: a not-served catalog miss does not reach object `List()`; an
  object list failure on a planned served GVR still aborts.
- Unit — informer planning: a bare namespaced WatchRule resolved to a non-core
  GVR still plans namespace-scoped informers for the WatchRule namespace.
- Unit — controller: unresolved resources produce the `ResourcesResolved=False`
  condition and warning feedback.
- E2E — keep `test/e2e/templates/watchrule.tmpl` bare and assert deployment and
  ingress snapshots appear, not only core ConfigMap snapshots.
- Regression — keep the `48e7d9d` wildcard snapshot coverage passing.

## Files Touched

| File | Change |
|---|---|
| `internal/watch/discovery.go` and new catalog files | Build and refresh `APIResourceCatalog` |
| `internal/watch/gvr.go` | Resolve informer GVR planning from the shared resolver |
| `internal/watch/manager.go` | Resolve snapshot and namespace planning from the shared resolver |
| `internal/controller/watchrule_controller.go` | Resource resolution condition and warning feedback |
| `internal/controller/clusterwatchrule_controller.go` | Same for ClusterWatchRule |
| `api/v1alpha1/watchrule_types.go`, `clusterwatchrule_types.go` | Document omitted-group semantics; optional structured unresolved-resource status |
| `internal/watch/*_test.go`, `internal/controller/*_test.go`, `test/e2e/*` | Coverage above |

## Sequencing

1. Implement `APIResourceCatalog` with per-group/version trust, refresh
   triggers, and catalog tests.
2. Implement `RuleGVRResolver` and resolver tests over the catalog.
3. Wire snapshot planning so the quickstart regression stops listing the wrong
   GVR.
4. Wire informer and namespace planning so bare non-core resources are actually
   watched.
5. Add WatchRule and ClusterWatchRule resource-resolution feedback.
6. Add optional structured status only after the core behavior is stable.

## Validation

`task fmt` → `task generate` → `task manifests` → `task vet` → `task lint` →
`task test` → `task test-e2e` (e2e commands run sequentially; Docker required).
