# API catalog and watched type architecture

> Status: architecture proposal, captured 2026-06-08.
>
> A simpler greenfield redesign — fewer layers, one unified per-type output
> instead of separate followability and health reports — lives in
> [type-followability.md](type-followability.md). Read that first; this doc is kept
> for the grounded, layer-by-layer reasoning behind it.
>
> Question: how should `APIResourceCatalog` and `WatchedTypeTable` relate when
> both currently carry Kubernetes type identity and policy facts?
>
> Short answer: keep `APIResourceCatalog` as the raw discovery cache, introduce a
> resolved type surface as the policy boundary, and make `WatchedTypeTable` a
> GitTarget-specific projection of already-resolved types.

## Problem

The current implementation has useful pieces, but the boundaries are blurry.

`APIResourceCatalog` owns discovery refresh, group/version trust state, indexes,
and raw type facts. `WatchedTypeTable` owns GitTarget selection, namespace
operation filters, conflict visibility, pending removals, and snapshot/informer
safety.

The overlap is that both objects now handle type identity:

```text
APIResourceEntry
  GVK, GVR, namespaced, verbs, preferred, subresource, allowed, policy reason

WatchedType
  GVK, GVR, namespaced, served version, preferred, scope, namespace operations
```

That overlap forces `WatchedTypeTable` to look back into the raw catalog with
`LookupGVR`, then repeat part of the GVK/GVR validity policy locally. It also
means product policy is spread across `APIResourceCatalog`, `CatalogMapper`,
`RuleGVRResolver`, `WatchedTypeTable`, and the git writer's sensitive-resource
policy.

The neat model is to make raw discovery, type policy, and GitTarget selection
three different layers.

## Current Comparison

| Concern | `APIResourceCatalog` today | `WatchedTypeTable` today | Desired owner |
| --- | --- | --- | --- |
| Discovery refresh | Calls Kubernetes discovery and handles partial failure | Does not refresh discovery | `APIResourceCatalog` |
| Raw indexes | Indexes by GVK, GVR, resource, group/resource, group/version | Calls `LookupGVR` while building a target table | `APIResourceCatalog` |
| Readiness and degraded discovery | Tracks ready, generation, degraded group/versions | Stores `ResolvedAt`; blocks gather on blocking misses and pending removals | Catalog for raw source state; resolved surface for live-set stability |
| Type facts | Stores GVK, GVR, namespaced, verbs, preferred, subresource | Repeats GVK, GVR, namespaced, preferred, served version | Resolved type surface should expose accepted type facts |
| Allowed/disallowed policy | Computes `Allowed` and `PolicyReason` during catalog entry creation | Receives misses and avoids unplanned types indirectly | Resolved type surface/type policy |
| GVK ambiguity | Catalog can return multiple entries for a GVK | Detects conflicts only among selected GVRs | Resolved type surface |
| GVR validation | `LookupGVR` returns one raw catalog entry | Treats the returned GVR as enough to build a watched type | Resolved type surface |
| WatchRule expansion | Raw resource/group indexes support wildcard and omitted-group lookups | Consumes resolved selections after expansion | Watch rule resolver, backed by type surface |
| GitTarget scope | None | Owns namespace ops, cluster-wide selection, target identity | `WatchedTypeTable` |
| Pending removals | None | Owns grace period and sweep safety | Resolved type surface live set |
| Sensitive resource handling | None | None | Type policy/type surface, consumed by git writer |

The important asymmetry: `APIResourceCatalog` is cluster-global; `WatchedTypeTable`
is GitTarget-specific. Any shared type decision between them belongs in a layer
between them, not inside either one.

## Target Layers

```text
APIResourceCatalog
  raw Kubernetes discovery cache
  indexes, generation, readiness, degraded group/versions

APIResourceEnricher
  joins discovery with CRDs and APIServices
  classifies resource origin and records CRD subresource details

TypePolicy
  GitOps Reverser policy over resource types
  allowed/disallowed, sensitive/encrypted, optional policy reasons

ResolvedTypeSurface
  stable interface over catalog + policy
  followability requirements, live-set hysteresis
  exact GVK lookup, exact GVR lookup, candidate listing, one refusal vocabulary

ResourceMapper
  narrow adapter for manifest analysis
  GVK -> GVR only

RuleGVRResolver
  WatchRule selector semantics
  wildcards, omitted groups, preferred version, scope, followability support

WatchedTypeTable
  GitTarget-selected operational view
  namespace operations, conflicts/misses for visibility
```

## APIResourceCatalog Responsibility

`APIResourceCatalog` should answer: what did Kubernetes discovery currently say,
and how trustworthy is that observation?

It should own:

- discovery refresh;
- raw discovery source observations;
- preserving previous clean group/versions when discovery is partially degraded;
- generation increments;
- raw indexes for efficient lookups;
- cloning and sorting raw entries;
- catalog metrics facts.

It should not own:

- GitTarget rule semantics;
- snapshot, informer, or sweep behavior;
- sensitive-resource write behavior;
- final "is this type usable by GitOps Reverser?" decisions.

The catalog may still store raw decorations such as preferred version and
subresource shape because those come from discovery or are direct projections of
discovery names. But a caller should not treat a raw catalog entry as a permission
to watch or write that type.

## Resource Origin and Enrichment

Every resource type should expose where GitOps Reverser believes it came from:

```text
kubernetes-internal
crd
aggregated-api
unknown
```

This is not available from `APIResourceList` alone. Discovery tells us that a
group/version/resource is served, but not whether it is built in, CRD-backed, or
proxied through an aggregated API server. So the raw discovery catalog should be
enriched from two additional API surfaces:

| Origin | Evidence | Notes |
| --- | --- | --- |
| `crd` | `apiextensions.k8s.io/v1 CustomResourceDefinition` whose `spec.group`, served `spec.versions[*].name`, and `spec.names.plural` match the GVR | This is the only source that contains CRD `/scale` JSONPaths. |
| `aggregated-api` | `apiregistration.k8s.io/v1 APIService` claiming the resource's group/version and forwarding to a service-backed API server | The APIService identifies the group/version backend; discovery still supplies the resource list underneath it. |
| `kubernetes-internal` | Served by discovery and not matched by CRD or aggregated API evidence | Includes core resources and built-in Kubernetes API groups served by kube-apiserver. |
| `unknown` | Discovery data exists but the enrichment inputs were unavailable or degraded | Must be surfaced as health, not hidden as a successful classification. |

Classification should be stored with provenance:

```go
type ResourceOrigin string

const (
    ResourceOriginKubernetesInternal ResourceOrigin = "kubernetes-internal"
    ResourceOriginCRD                ResourceOrigin = "crd"
    ResourceOriginAggregatedAPI      ResourceOrigin = "aggregated-api"
    ResourceOriginUnknown            ResourceOrigin = "unknown"
)

type ResourceSourceFact struct {
    Origin ResourceOrigin

    // Confidence is "observed" when backed by a clean CRD/APIService snapshot,
    // "inferred" when built-in is inferred by exclusion, and "unknown" when the
    // required enrichment surface was unavailable.
    Confidence string

    // Ref is bounded operator context, for example:
    //   crontabs.stable.example.com
    //   v1beta1.metrics.k8s.io
    // Empty for inferred Kubernetes-internal resources.
    Ref string
}
```

CRD matching is per GVR, not just per group/version, because one CRD defines one
plural resource and may serve multiple versions. Aggregated API matching is
normally per group/version because `APIService` owns a whole group/version path;
the resource list still comes from discovery.

Origin should be a fact on every resolved type and every refused type report. A
health report that says "deployment is healthy" but does not say whether it is
built-in, CRD, or aggregated is incomplete.

## Subresource Facts

The resolved type surface should expose the supported subresources that matter to
GitOps Reverser without making subresources independent watched types.

Minimum shape:

```go
type SubresourceFacts struct {
    Status StatusSubresourceFact
    Scale  ScaleSubresourceFact
}

type StatusSubresourceFact struct {
    Enabled bool
    Source  string // discovery, crd, builtin-registry, unknown
}

type ScaleSubresourceFact struct {
    Enabled bool
    Source  string // discovery, crd, builtin-registry, aggregated-api, unknown

    // ResponseKind is normally autoscaling/v1, Kind=Scale.
    ResponseGVK schema.GroupVersionKind

    // Parent write paths. For built-in resources these come from the built-in
    // registry. For CRDs these are read from CRD spec.versions[*].subresources.scale.
    SpecReplicasPath   string
    StatusReplicasPath string
    LabelSelectorPath  string
    LabelSelectorKind  string // serialized-string, label-selector, unknown

    // UsableForGit is true only when a mutating /scale audit event can be mapped
    // back to a durable parent desired-state field.
    UsableForGit bool
    UnusableReason string
}
```

For CRDs, the scale pointers come directly from the CRD version:

```yaml
subresources:
  status: {}
  scale:
    specReplicasPath: .spec.replicas
    statusReplicasPath: .status.replicas
    labelSelectorPath: .status.labelSelector
```

CRD pointer rules from Kubernetes matter and should be retained as facts:

- `specReplicasPath` is required, must be dot-notation JSONPath under `.spec`,
  and maps to `Scale.Spec.Replicas`.
- `statusReplicasPath` is required, must be dot-notation JSONPath under
  `.status`, and maps to `Scale.Status.Replicas`.
- `labelSelectorPath` is optional, must be dot-notation JSONPath under `.status`
  or `.spec`, and maps to `Scale.Status.Selector`.
- `labelSelectorPath` must point at a string field containing a serialized label
  selector; it is needed for HPA/VPA support.

This changes the previous deferred CRD-scale story: CRD `/scale` remains
unsupported only until the enriched type object can carry these paths. Once
`ScaleSubresourceFact.SpecReplicasPath` is populated from the CRD, a CRD scale
event has enough information to patch the parent desired field without guessing.

Built-in scalable resources should use the same fields, populated from an
explicit built-in scale pointer registry. That makes built-ins and CRDs look the
same to the translator, health report, and any future GUI:

```yaml
builtinScalePointers:
  - gvr: apps/v1/deployments
    responseGVK:
      group: autoscaling
      version: v1
      kind: Scale
    specReplicasPath: .spec.replicas
    statusReplicasPath: .status.replicas
    labelSelectorPath: .spec.selector
    labelSelectorKind: label-selector
  - gvr: apps/v1/statefulsets
    responseGVK:
      group: autoscaling
      version: v1
      kind: Scale
    specReplicasPath: .spec.replicas
    statusReplicasPath: .status.replicas
    labelSelectorPath: .spec.selector
    labelSelectorKind: label-selector
  - gvr: apps/v1/replicasets
    responseGVK:
      group: autoscaling
      version: v1
      kind: Scale
    specReplicasPath: .spec.replicas
    statusReplicasPath: .status.replicas
    labelSelectorPath: .spec.selector
    labelSelectorKind: label-selector
  - gvr: v1/replicationcontrollers
    responseGVK:
      group: autoscaling
      version: v1
      kind: Scale
    specReplicasPath: .spec.replicas
    statusReplicasPath: .status.replicas
    labelSelectorPath: .spec.selector
    labelSelectorKind: label-selector
```

The registry should be data, not scattered conditionals. It can start small and
only include built-in resources whose scale paths are verified. Discovery still
decides whether the resource and `{resource}/scale` are actually served on the
cluster; the registry only supplies parent paths and response shape for known
built-in types.

The selector field is intentionally typed. A CRD `labelSelectorPath` must point to
a serialized string because that is the CRD contract. Built-in resources can
derive `Scale.Status.Selector` from a structured label selector, so their registry
entry should report both the source path and `labelSelectorKind: label-selector`.
The scale write path still depends only on `specReplicasPath`; selector details
are health and GUI facts.

Aggregated API scale should also land in `ScaleSubresourceFact`, but it is only
usable when the aggregated API surface provides an equivalent trusted parent path.
Plain discovery of `{resource}/scale` is not enough.

Subresource entries from discovery such as `deployments/status` and
`deployments/scale` should be folded into the parent type's `SubresourceFacts`.
They should not appear as standalone `WatchedType` entries.

## Per-Type Health Report

Every resource type should have an actual health report. This report is not just
for selected WatchRule types; it should be available for any cataloged resource
type so operators can answer "why is this type watched, ignored, or unsafe?"

Sketch:

```go
type TypeHealthLevel string

const (
    TypeHealthHealthy  TypeHealthLevel = "healthy"
    TypeHealthDegraded TypeHealthLevel = "degraded"
    TypeHealthRefused  TypeHealthLevel = "refused"
    TypeHealthUnknown  TypeHealthLevel = "unknown"
)

type TypeHealthReport struct {
    Level TypeHealthLevel

    // One-line bounded summary suitable for status, logs, and diagnostics.
    Summary string

    // Conditions are bounded and stable. They should not include object names,
    // request URIs, or unbounded backend messages.
    Conditions []TypeHealthCondition
}

type TypeHealthCondition struct {
    Type    string // Discovery, Source, Policy, Identity, Subresources, Watch, Scale, Followability, Selection
    Status  string // True, False, Unknown
    Reason  string
    Message string
}
```

Recommended conditions:

| Condition | Healthy when | Degraded/refused examples |
| --- | --- | --- |
| `Discovery` | group/version was discovered from trusted data | catalog unavailable, group/version degraded, stale preserved entry |
| `Source` | origin is classified as internal, CRD, or aggregated with evidence | CRD/APIService enrichment unavailable, origin unknown |
| `Policy` | type is allowed by product policy | disallowed resource, sensitive resource without required handling, subresource-only match |
| `Identity` | GVK ↔ GVR is a clean 1:1 mapping in both directions | `ambiguous_gvk` (one GVK served by multiple GVRs), `ambiguous_gvr` (one GVR resolving to multiple Kinds) |
| `Watch` | type supports `get`, `list`, `watch`, and `patch` when selected for a WatchRule | missing required verb, selected scope mismatch |
| `Subresources` | status/scale facts are internally consistent | discovery says scale exists but CRD scale paths are missing |
| `Scale` | scale is disabled or has a known parent replica path when enabled | `scale_path_unresolved`, invalid CRD JSONPath, aggregated scale without path source |
| `Followability` | type is in the live set | denied, missing verb, retained during grace, expired absence |
| `Selection` | GitTarget selection folded cleanly | conflicting GVRs across selections for this target |

Examples:

```yaml
gvr: apps/v1/deployments
source:
  origin: kubernetes-internal
  confidence: inferred
subresources:
  status:
    enabled: true
    source: discovery
  scale:
    enabled: true
    source: builtin-registry
    responseGVK:
      group: autoscaling
      version: v1
      kind: Scale
    specReplicasPath: .spec.replicas
    statusReplicasPath: .status.replicas
    labelSelectorPath: .spec.selector
    labelSelectorKind: label-selector
    usableForGit: true
health:
  level: healthy
  summary: built-in resource is watchable and scale paths are known
```

```yaml
gvr: stable.example.com/v1/crontabs
source:
  origin: crd
  confidence: observed
  ref: crontabs.stable.example.com
subresources:
  status:
    enabled: true
    source: crd
  scale:
    enabled: true
    source: crd
    responseGVK:
      group: autoscaling
      version: v1
      kind: Scale
    specReplicasPath: .spec.replicas
    statusReplicasPath: .status.replicas
    labelSelectorPath: .status.labelSelector
    labelSelectorKind: serialized-string
    usableForGit: true
health:
  level: healthy
  summary: CRD resource is watchable and scale paths are known
```

```yaml
gvr: metrics.k8s.io/v1beta1/pods
source:
  origin: aggregated-api
  confidence: observed
  ref: v1beta1.metrics.k8s.io
subresources:
  status:
    enabled: false
  scale:
    enabled: false
health:
  level: refused
  summary: aggregated metrics resource is read-only or not watchable for Git mirroring
```

The health report should be part of `TypeResult` for exact lookups and part of
catalog diagnostics for list-all views. `WatchedTypeTable` can then copy the
accepted type's health into target status without recalculating catalog facts.

## Followability Requirements

GitOps Reverser should distinguish "served by Kubernetes discovery" from
"followable by GitOps Reverser." A served type is raw cluster fact. A followable
type is safe enough for the product to mirror, watch, snapshot, route audit
events for, and potentially sweep from Git.

The default lookup path should return only followable live types. Callers that
need diagnostics can ask for an inspection result, but ordinary mapper, resolver,
and writer code should not accidentally act on raw or unstable discovery entries.

Each requirement below has a sharp, stable name and exactly one refusal reason.
The refusal reason is the single vocabulary GitOps Reverser uses everywhere a type
can be turned away — default lookup status, `FollowabilityReport.Reasons`, and
health conditions — so an operator always gets the same machine-readable answer to
"why isn't this type picked up?" A type enters the live set only when every
required row passes; the first failing row supplies the refusal reason.

| Requirement | Rule | Required | Refused with |
| --- | --- | --- | --- |
| `parent-resource` | The type is a top-level parent resource, not a subresource. Subresources are folded into the parent's `SubresourceFacts`. | yes | `subresource_only` |
| `policy-allowed` | The type is not on the GitOps Reverser deny list. | yes | `denied_by_policy` |
| `sensitive-handled` | The type is not sensitive, or its sensitivity has supported encryption/write handling. | yes | `sensitive_unsupported` |
| `discovery-trusted` | The backing group/version was discovered from trusted, non-degraded data. | yes | `discovery_degraded` |
| `served-stable` | The type survived transient registration wobble, or is still inside the 60-second removal grace. | yes | `served_unstable`, `absence_expired` |
| `gvk-resolves-to-one-gvr` | The type's GVK is served by exactly one accepted GVR. | yes | `ambiguous_gvk` |
| `gvr-resolves-to-one-gvk` | The type's GVR resolves back to exactly one Kind. | yes | `ambiguous_gvr` |
| `scope-known` | The system knows whether the get/list/watch paths are namespaced or cluster-scoped. | yes | `scope_unknown` |
| `required-verbs` | Discovery advertises `get`, `list`, `watch`, and `patch`. | yes | `missing_verb_<verb>` |
| `source-classified` | Origin is classified as `kubernetes-internal`, `crd`, or `aggregated-api` with evidence. | yes | `source_unknown` |
| `scale-resolved` | When scale routing is required, the parent `SpecReplicasPath` is known and not guessed. | conditional | `scale_path_unresolved` |

### Unambiguous GVK ↔ GVR identity

GitOps Reverser follows a type only when its identity is a clean 1:1 mapping in
**both** directions. This is two named requirements, not one:

- `gvk-resolves-to-one-gvr`: exactly one GVR serves a given GVK; and
- `gvr-resolves-to-one-gvk`: that GVR resolves back to exactly one Kind.

The round trip must close. `GVK -> GVR -> GVK` must return the original GVK, and
`GVR -> GVK -> GVR` must return the original GVR. If either direction forks, the
type is refused with `ambiguous_gvk` or `ambiguous_gvr` respectively and never
enters the live set; it remains visible only through `Inspect*` and the health
report.

This is deliberately strict, and the strictness is the point. A served but
ambiguous identity is exactly where silent mis-mirroring, wrong-parent writes, and
confusing sweeps come from. The ambiguity is almost always a symptom of an
unhealthy cluster: duplicate CRDs claiming the same kind, an aggregated API
shadowing a built-in group/version, or two resources sharing a kind across
versions. GitOps Reverser should not paper over that with a guess. It refuses the
type, names the conflict in the health report, and lets the operator fix it —
**keep your cluster healthy** is a precondition Reverser relies on, not something
it tries to work around. Keeping ambiguous identities out of the live set removes
a large, recurring source of unclarity in one move.

The deny list belongs in type policy, not in WatchRule expansion. A rule that asks
for a denied resource should resolve to a typed refusal with the policy reason.
The denied type should still appear in inspection diagnostics and health reports;
it should not appear in the default live list.

`patch` is deliberately part of the live-set requirement. GitOps Reverser may not
patch the Kubernetes API directly for every flow, but audit events and
subresource translations include patch-shaped desired-state changes. Treating
patchless resources as live would create an uneven contract for writer and UI
code.

## Live Type Set

The resolved type surface should maintain a hysteresis-protected live set:

```text
raw discovery + enrichment + policy + followability checks
  -> candidate type facts
  -> live type set with removal hysteresis
```

Additions should be fast. When a new CRD becomes served and passes the
requirements, it can enter the live set immediately. Removals should be slow:
once a type is live, a failed refresh, missing group/version, missing resource, or
temporary CRD registration wobble must not remove it from the live set until the
absence has persisted for **60 seconds**.

The 60-second removal grace should be fixed, not configurable. It is product
safety, not tuning. The goal is to avoid turning a short API discovery gap into a
large destructive Git sweep.

Sketch:

```go
const liveTypeRemovalGrace = 60 * time.Second

type FollowabilityStatus string

const (
    FollowabilityLive       FollowabilityStatus = "live"
    FollowabilityPending    FollowabilityStatus = "pending"
    FollowabilityRetained   FollowabilityStatus = "retained"
    FollowabilityRefused    FollowabilityStatus = "refused"
    FollowabilityUnknown    FollowabilityStatus = "unknown"
)

type FollowabilityReport struct {
    Status FollowabilityStatus
    Live bool

    // Reasons are bounded machine-readable requirement outcomes drawn from the
    // single refusal vocabulary, for example: subresource_only, denied_by_policy,
    // sensitive_unsupported, discovery_degraded, served_unstable, absence_expired,
    // ambiguous_gvk, ambiguous_gvr, scope_unknown, missing_verb_patch,
    // source_unknown, scale_path_unresolved.
    Reasons []string

    FirstObserved time.Time
    LastObserved time.Time
    MissingSince *time.Time
    RetainedUntil *time.Time
}
```

`Retained` means the type failed the latest raw observation but remains in the
live set because the 60-second removal grace has not elapsed. During that window,
consumers should continue treating the previous type fact as live for planning,
snapshot, informer, and writer identity purposes. Health should show the degraded
condition, but lookup should not drop the type.

Once the grace expires, the type leaves the default live set and becomes an
inspection result with `FollowabilityRefused` or `FollowabilityUnknown`,
depending on the reason. A deletion/sweep workflow can then make a deliberate
decision from a stable absence instead of a discovery blink.

This moves the current `WatchedTypeTable` pending-removal behavior one level up.
`WatchedTypeTable` should not be the only place that protects against spurious
type disappearance; all GitOps Reverser components need the same stable type
view.

## Lookup Modes

The type surface should make the safe path the easy path.

```go
type TypeSurface interface {
    Ready() mapping.MapperReadiness
    Generation() uint64

    // Default lookups return only live followable types.
    ForGVK(ctx context.Context, gvk schema.GroupVersionKind) (TypeResult, error)
    ForGVR(ctx context.Context, gvr schema.GroupVersionResource) (TypeResult, error)
    LiveTypes(ctx context.Context) ([]TypeFact, error)

    // Inspection lookups return served, retained, refused, and unstable facts for
    // status, GUI, CLI, and debugging. They never authorize mirroring by
    // themselves.
    InspectGVK(ctx context.Context, gvk schema.GroupVersionKind) (TypeResult, error)
    InspectGVR(ctx context.Context, gvr schema.GroupVersionResource) (TypeResult, error)
    InspectTypes(ctx context.Context) ([]TypeResult, error)
}
```

Default lookup behavior:

- `ForGVK` and `ForGVR` return `Resolved` only for live followable types.
- A denied, ambiguous, missing-verb, source-unknown, subresource-only, or
  expired-missing type returns a refusal status.
- A retained type still returns as live, with health/followability conditions
  explaining that it is retained during the removal grace.
- Callers that need to render "why not?" use `Inspect*`, not raw catalog lookups.

## Component Needs

Different components need different projections of the same type fact. The point
of the abstraction is to avoid each component rediscovering or weakening the
rules.

| Component | Needs live-only lookup? | Extra facts needed | Why |
| --- | --- | --- | --- |
| Manifest analyzer / mapper | yes | GVK, GVR, scope, refusal reason | Map committed manifests to live API identities without accepting denied or ambiguous types. |
| WatchRule resolver | yes | resource names, preferred versions, scope, get/list/watch/patch verbs | Expand user selectors only into followable resources. |
| WatchedTypeTable | yes | namespace operation filters plus copied `TypeFact` health | Project already-live types into a GitTarget view. |
| Snapshot / informer manager | yes | GVR, scope, list/watch/get support, retained-state health | Avoid starting/stopping streams on transient discovery gaps. |
| Audit consumer | yes | parent GVR, source, subresource facts, scale `SpecReplicasPath` | Route events only for live parents and translate `/scale` without guessing. |
| Git writer | yes | sensitivity, source, health, scale field-patch source | Decide write/encryption behavior from the same accepted type facts. |
| CLI / GUI / status | no, also needs inspect | origin, verbs, subresources, followability report, health conditions | Show all cluster resource types, including denied/refused/unstable, and explain what GitOps Reverser will do. |

The `/scale` case is the useful pressure test. The audit consumer should be able
to ask for a parent type and receive:

```yaml
gvr: apps/v1/deployments
followability:
  status: live
  live: true
verbs: [get, list, watch, patch]
subresources:
  scale:
    enabled: true
    specReplicasPath: .spec.replicas
    usableForGit: true
```

If the same call is made for a CRD with `/scale` but no resolved
`SpecReplicasPath`, the default lookup should either refuse the type for scale
routing or return the parent as live with `scale.usableForGit: false`, depending
on whether the parent itself is otherwise followable. In both cases, the scale
translator must not guess `.spec.replicas`.

## Resolved Type Surface Responsibility

The missing abstraction is the policy boundary between raw discovery and consumers.

Sketch:

```go
type TypeFact struct {
    GVK schema.GroupVersionKind
    GVR schema.GroupVersionResource

    Namespaced bool
    Verbs []string
    Preferred bool
    Subresource bool

    Source ResourceSourceFact
    Subresources SubresourceFacts

    Allowed bool
    Sensitive bool
    PolicyReason string

    Followability FollowabilityReport
    Health TypeHealthReport
}

type TypeResult struct {
    Fact TypeFact
    Status mapping.Status
    Reason string
    Generation uint64
}

type TypeSurface interface {
    Ready() mapping.MapperReadiness
    Generation() uint64

    // Default lookups return only live followable types.
    ForGVK(ctx context.Context, gvk schema.GroupVersionKind) (TypeResult, error)
    ForGVR(ctx context.Context, gvr schema.GroupVersionResource) (TypeResult, error)
    LiveTypes(ctx context.Context) ([]TypeFact, error)

    // Inspection lookups return served, retained, refused, and unstable facts for
    // status, GUI, CLI, and debugging.
    InspectGVK(ctx context.Context, gvk schema.GroupVersionKind) (TypeResult, error)
    InspectGVR(ctx context.Context, gvr schema.GroupVersionResource) (TypeResult, error)
    InspectTypes(ctx context.Context) ([]TypeResult, error)
}
```

`ForGVK` and `ForGVR` must share the same product policy:

- no trusted data means `CatalogUnavailable`;
- degraded lookup scope means `DiscoveryDegraded`;
- served but excluded means `Disallowed`;
- subresource-only matches are refused;
- one GVK served by more than one GVR is `Ambiguous` (`ambiguous_gvk`);
- one GVR resolving to more than one Kind is `Ambiguous` (`ambiguous_gvr`);
- exact GVR lookup must validate the entry's GVK against the full GVK lookup and
  confirm the round trip `GVR -> GVK -> GVR` returns the original GVR;
- live lookups only return types that pass followability requirements or are
  retained inside the 60-second removal grace.

Those identity points close the current gap: `LookupGVR` is single-valued in the
index because GVR strings are unique, but a unique index key is not proof of a
healthy identity. The required property is a closed GVK ↔ GVR bijection in both
directions. If the GVR's GVK is globally ambiguous, or the GVR itself resolves to
more than one Kind, selecting that GVR is still refused.

`ForGVK` and `ForGVR` should return health even on refusal. A refused result that
only says "Disallowed" forces UI/status code to reverse-engineer the real issue.
A refused result that includes source, policy, subresource, and watch conditions
is useful immediately.

## WatchedTypeTable Responsibility

`WatchedTypeTable` should answer: for this GitTarget, which already-accepted
types are operationally active, under which namespaces and operations?

It should own:

- GitTarget destination identity;
- selected namespace and operation filters;
- cluster-wide versus named namespace stream shape;
- resident table generation;
- blocking snapshot safety;
- conflict and miss visibility for target planning.

It should not own:

- raw GVR to GVK lookup;
- global GVK ambiguity policy;
- allowed/disallowed policy;
- sensitive-resource classification;
- Kubernetes discovery freshness;
- discovery hysteresis and removal grace.

In the target shape, `buildWatchedTypeTable` receives resolved facts, not raw GVRs
that it must validate against the catalog:

```go
type resolvedSelection struct {
    fact TypeFact
    namespace string
    ops []configv1alpha1.OperationType
}
```

Then the table builder only folds selections by accepted type identity and merges
namespace operation sets. If ambiguity or disallowed state exists, it arrives as a
typed miss from the resolver instead of being rediscovered inside the table.

`WatchedType` should retain enough of `TypeFact` to render an operator report:

```go
type WatchedType struct {
    GVK schema.GroupVersionKind
    GVR schema.GroupVersionResource

    Source ResourceSourceFact
    Subresources SubresourceFacts
    Followability FollowabilityReport
    Health TypeHealthReport

    Namespaced bool
    Scope configv1alpha1.ResourceScope
    ServedVersion string
    Preferred bool
    NamespaceOps map[string]OperationSet
}
```

That does not make the watched table responsible for calculating source,
subresource, followability, or health facts. It only preserves the
already-resolved report for status and diagnostics.

## Sensitive Resources

The startup flag for additional sensitive resources is currently consumed by the
git writer. That works for encryption, but it keeps an important type policy
outside the type system.

Prefer `Sensitive bool` on `TypeFact`, backed by the existing
`types.SensitiveResourcePolicy`.

This should mean:

- core `v1/secrets` are always sensitive;
- startup additional sensitive resources are classified at type-policy time;
- writer code can still receive the policy directly during migration;
- eventually the selected type fact can carry sensitivity into write planning.

The field should be named `Sensitive`, not `Secret`, because configured resources
may be Secret-shaped CRDs rather than Kubernetes `Secret` objects.

## RuleGVRResolver Role

`RuleGVRResolver` should keep WatchRule-specific syntax:

- omitted apiGroups;
- wildcard groups/resources/versions;
- preferred-version selection when versions are omitted;
- namespaced versus cluster scope;
- followability capability checks;
- operator-facing miss detail.

But it should not use raw catalog entries as final answers. It can use catalog or
surface candidate listing for expansion, then validate concrete candidates through
the type surface.

Practical target:

```text
WatchRule selector
  -> candidate raw resources for expansion
  -> TypeSurface.ForGVR for each concrete GVR
  -> resolvedSelection or ResolveMiss
  -> WatchedTypeTable fold
```

`ResolveMiss` can remain a watch-domain type, but it should optionally carry the
core mapping status so UI/status/logging does not need to parse reason strings.

## Refactor Path

1. Add a `TypeFact`/`TypeResult` shape in the mapping layer or a small new core
   package that does not import `internal/watch`.
2. Add `ResourceSourceFact`, `SubresourceFacts`, and `TypeHealthReport` to that
   shape.
3. Add `FollowabilityReport`, the fixed 60-second live-set removal grace, and
   live-only default lookup methods.
4. Add enrichment from CRDs and APIServices:
   - CRD GVR source classification;
   - CRD `/status` and `/scale` flags;
   - CRD scale `specReplicasPath`, `statusReplicasPath`, and `labelSelectorPath`;
   - aggregated API group/version source classification.
5. Add a built-in scale pointer registry that populates the same
   `ScaleSubresourceFact` fields used by CRDs.
6. Fold discovery subresource entries into parent resource facts.
7. Add exact GVR resolution beside exact GVK resolution and make it validate
   global GVK uniqueness.
8. Move allowed/disallowed and sensitive classification behind a small type-policy
   object used by the live surface and static tests.
9. Compute per-type health reports during resolution and expose them for accepted
   and refused types.
10. Keep `CatalogMapper` as a thin `ResourceMapper` adapter over the surface.
11. Change `RuleGVRResolver` so concrete GVRs are accepted only through the surface.
12. Change `WatchedTypeTable` so it folds resolved `TypeFact` selections instead of
   calling `catalog.LookupGVR`.
13. Remove local conflict detection from the table once ambiguity is guaranteed to
   arrive as a typed planning miss.
14. Move pending-removal behavior out of `WatchedTypeTable` and into the shared
   live-set hysteresis.
15. Add regression coverage for the GVK ↔ GVR bijection in both directions:
   - one GVK served by two GVRs, only one selected by a GitTarget, still refused
     with `ambiguous_gvk`;
   - one GVR resolving to more than one Kind refused with `ambiguous_gvr`;
   - a clean type whose `GVK -> GVR -> GVK` and `GVR -> GVK -> GVR` round trips
     both close is accepted.
16. Add regression coverage for live-set hysteresis:
   - a newly served CRD can enter the live set immediately;
   - a previously live type remains live while absent for less than 60 seconds;
   - a previously live type leaves the live set after 60 seconds of stable absence;
   - retained types report degraded health but still resolve from default lookups.
17. Add regression coverage for built-in and CRD scale path extraction and health:
   - built-in scale paths populate `ScaleSubresourceFact` from the registry;
   - CRD with status and scale reports both enabled;
   - CRD scale paths are version-specific;
   - CRD scale without usable `specReplicasPath` is unhealthy/refused for scale
     routing;
   - aggregated API scale without a trusted parent path reports
     `scale_path_unresolved`.

## Decision

Do not merge `APIResourceCatalog` and `WatchedTypeTable`.

They have different lifetimes and scopes:

- `APIResourceCatalog` is cluster-global and discovery-shaped.
- `WatchedTypeTable` is GitTarget-local and operation-shaped.

The overlap should be removed by inserting a resolved type surface between them.
That surface becomes the single place where GitOps Reverser turns raw discovery
into accepted or refused type facts.

## References

- [catalog-mapper-vs-watched-type-table.md](catalog-mapper-vs-watched-type-table.md)
- [subresource-scope-reduction.md](subresource-scope-reduction.md)
- [gvk-gvr-mapping-layer.md](../../../finished/gvk-gvr-mapping-layer.md)
- [resource-types.md](../../../facts/resource-types.md)
- [subresources.md](../../../facts/subresources.md)
- [`internal/watch/api_resource_catalog.go`](../../../../internal/watch/api_resource_catalog.go)
- [`internal/watch/watched_type_table.go`](../../../../internal/watch/watched_type_table.go)
- [`internal/watch/rule_gvr_resolver.go`](../../../../internal/watch/rule_gvr_resolver.go)
- [`internal/watch/catalog_mapper.go`](../../../../internal/watch/catalog_mapper.go)
- [`internal/types/sensitive_resource.go`](../../../../internal/types/sensitive_resource.go)
