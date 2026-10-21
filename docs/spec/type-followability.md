# Type followability model

> **spec** — current behaviour. The code depends on this document; change one, change the other. Index: [`../INDEX.md`](../INDEX.md)
>
> Status: implemented in [`internal/typeset`](../../internal/typeset/model.go); captured
> 2026-06-08 as a greenfield proposal. Where the code differs from the proposal, this page
> says so in place, and the code wins.
>
> Supersedes the layered model of `api-catalog-watched-type-architecture.md` (since
> deleted), which grounded in the packages of the time and ended up with seven layers and two
> parallel reports (followability requirements + a health report). This one starts
> from the question we actually want answered and works backward.

## The one question

For every resource type the cluster serves, GitOps Reverser needs exactly one
answer:

> **Is this type followable, and if not, what is the single reason it is not?**

"Followable" means safe enough for the product to mirror, watch, snapshot, route
audit events for, and potentially sweep from Git. Everything else — the health
level, the status conditions, the "why is this ignored?" diagnostic — is a
rendering of that one answer. There is no separate health report.

This matters because the layered model invented condition names that do not map to
a real decision. A `Watch` condition that is `False` does not tell an operator
anything actionable; the type is simply **not followable because it is missing a
required verb**. Naming the check after the failure ("Watch: False") is less clear
than naming it after the requirement and letting the reason speak
(`not followable — missing required verb: watch`). So the model has one list of named
requirement checks, and the failing check *is* the explanation.

## Fewer layers

Three things, not seven. Two are cluster-global; one is per-GitTarget.

```text
cluster ─ scan ─▶  Observation          raw per-type facts: discovery + CRD +
                   (one per served type)  APIService evidence + built-in registry
                       │
                       ▼
                  TypeRegistry           the single decision surface:
                   (policy + identity      observation + policy -> Followability
                    + 60s grace baked in)   one record per known type, lookups,
                       │                     the live set
       ┌───────────────┼─────────────────┐
       ▼               ▼                 ▼
   mapper            live set         TargetView (per GitTarget)
   ByGVK/ByGVR       Followable()      folds followable records into
   (no separate                       namespace scopes; never
    mapper object)                    recomputes followability
```

- **Scan** gathers raw facts and produces an `Observation` per served type. It
  joins discovery with the built-in scale registry and the operator's resource
  policies. It makes no product decisions. The proposal also joined
  `CustomResourceDefinition` and `APIService` evidence; that is not built, so origin
  is inferred from the API group (see `origin` below).
- **`TypeRegistry`** is the only place that turns observations + policy into a
  followability verdict. It owns identity rules, the deny/sensitive policy, the live
  set, and the 60-second removal grace. Every consumer reads from it.
- **`TargetView`** is a GitTarget's projection of *already-followable* records into
  namespace scopes. It never recomputes followability. As built it is the
  `WatchedTypeTable` in `internal/watch`.

The mapper is not a layer: it is `TypeRegistry.ByGVK` (as built, `typeset.Lookup`).
WatchRule expansion is not a layer: it is a function that builds a `TargetView` by asking the registry.

## Identity is 1:1, in both directions

A type is followable only when its identity is a closed bijection:

- exactly one **GVR** serves a given **GVK**, and
- that GVR resolves back to exactly one **Kind**.

The round trip must close: `GVK → GVR → GVK` returns the original GVK and
`GVR → GVK → GVR` returns the original GVR. If either direction forks, the type is
refused (`gvk-not-unique` or `gvr-not-unique`) and never enters the live set.

This is strict on purpose. A served-but-ambiguous identity is where silent
mis-mirroring, wrong-parent writes, and confusing sweeps come from, and it is almost
always a symptom of an unhealthy cluster: duplicate CRDs claiming a kind, an
aggregated API shadowing a built-in group/version, or two resources sharing a kind
across versions. GitOps Reverser does not guess past it. It refuses the type, names
the conflict, and lets the operator fix it. **Keep your cluster healthy** is a
precondition the product relies on, not something it works around.

## The followability output

Every known type carries one `Followability` value: a verdict, a one-line summary,
and the full list of requirement checks in funnel order. This single list replaces
both the old "requirements" table and the old "health conditions" table.

### Verdicts

| Verdict | Meaning | Old health equivalent |
| --- | --- | --- |
| `followable` | every required check passed; in the live set | healthy |
| `retained` | a transient check (`served`/`trusted`) is failing now, but the 60s grace has not elapsed; still treated as live | degraded |
| `refused` | a permanent check failed; will not be followed | refused |
| `unknown` | the registry could not assess the type (catalog unavailable) | unknown |

### Requirements

Each check has a stable kebab-case name and a single reason code on failure. The
reason code is the one vocabulary used everywhere a type is turned away — lookup
results, the live-set report, and operator status — so "why isn't this picked up?"
always has the same machine-readable answer.

| Requirement | Passes when | Fails with |
| --- | --- | --- |
| `served` | discovery serves this as a top-level resource (not a subresource) | `not-served`, `subresource-only` |
| `trusted` | the backing group/version came from trusted, non-degraded discovery | `discovery-degraded`, `catalog-unavailable` |
| `stable` | the type is not mid-disappearance, or is inside the removal grace | `absence-expired` |
| `identity` | GVK ↔ GVR is 1:1 in both directions | `gvk-not-unique`, `gvr-not-unique` |
| `scope` | the type is known namespaced or cluster-scoped | `scope-unknown` |
| `verbs` | discovery advertises `get`, `list`, `watch` (detail names the missing verb) | `missing-verb` |
| `origin` | classified `builtin`, `crd`, or `aggregated` | `origin-unknown` |
| `policy` | product policy permits mirroring this type | `denied-by-policy` |
| `sensitivity` | not sensitive, or sensitivity has supported encryption/write handling | `sensitive-unsupported` |
| `scale` | scale is unused, or its parent replica path is known (not guessed) | `scale-path-unresolved` |

As built, three checks are narrower than the proposal:

- `verbs` does not require `patch`. Mirroring is a read path, so a type without
  `patch` is still mirrorable.
- `origin` is a shape heuristic on the API group: the core group, `*.k8s.io`, and the
  legacy built-in groups are `builtin`, everything else is `crd`, both with confidence
  `inferred`. It never fails for a served type, and nothing is classified `aggregated`.
- `scale` binds a parent replica path only for built-ins. A CRD that serves `/scale`
  has no path, so it fails with `scale-path-unresolved` and is refused.

Verdict derivation is mechanical:

- all required checks `pass` → `followable`;
- only `served`/`trusted` fail and the absence is younger than 60s → `retained`;
- `catalog-unavailable` (the whole catalog is down) → `unknown`;
- any other failed check → `refused`, summarized by the first failing check.

### Examples

Followable built-in:

```yaml
gvk: apps/v1 Deployment
gvr: apps/v1 deployments
scope: Namespaced
origin: { kind: builtin, confidence: inferred }
verdict: followable
summary: followable
checks:
  - { requirement: served,      result: pass }
  - { requirement: trusted,     result: pass }
  - { requirement: stable,      result: pass }
  - { requirement: identity,    result: pass }
  - { requirement: scope,       result: pass }
  - { requirement: verbs,       result: pass }
  - { requirement: origin,      result: pass }
  - { requirement: policy,      result: pass }
  - { requirement: sensitivity, result: pass }
  - { requirement: scale,       result: pass }
subresources:
  scale: { source: builtin-registry, specReplicasPath: .spec.replicas, usable: true }
```

Refused for a missing verb — the case the old `Watch` condition described badly:

```yaml
gvk: metrics.k8s.io/v1beta1 PodMetrics
gvr: metrics.k8s.io/v1beta1 pods
origin: { kind: builtin, confidence: inferred }
verdict: refused
summary: "not followable — missing required verb: watch"
checks:
  - { requirement: served,   result: pass }
  - { requirement: trusted,  result: pass }
  - { requirement: identity, result: pass }
  - { requirement: verbs,    result: fail, reason: missing-verb, detail: "watch" }
```

Refused for ambiguous identity:

```yaml
gvk: example.com/v1 Widget
verdict: refused
summary: "not followable — GVK served by more than one resource: widgets, widgetz"
checks:
  - { requirement: identity, result: fail, reason: gvk-not-unique, detail: "widgets, widgetz" }
```

## Interfaces

The record is the unit everything passes around. It answers "can I act on this?"
and "why not?" in one object, so the safe path and the diagnostic path are the same
call.

```go
// Identity is the one true name of a type. For a followable type the GVK <-> GVR
// bijection is closed, so these two always round-trip.
type Identity struct {
    GVK   schema.GroupVersionKind
    GVR   schema.GroupVersionResource
    Scope Scope // Namespaced | ClusterScoped | Unknown
}

type Origin struct {
    Kind       OriginKind // builtin | crd | aggregated | unknown
    Confidence Confidence // observed | inferred | unknown
    Evidence   string     // bounded, e.g. crontabs.stable.example.com
}

type TypeRecord struct {
    Identity     Identity
    Origin       Origin
    Preferred    bool
    Verbs        []string
    Subresources Subresources
    Sensitive    bool

    Followability Followability
    Generation    uint64
}

type Followability struct {
    Verdict Verdict // followable | retained | refused | unknown
    Summary string  // one line, e.g. "not followable — missing required verb: watch"
    Checks  []Check // every requirement, funnel order
}

type Check struct {
    Requirement Requirement // served, trusted, stable, identity, scope, verbs, origin, policy, sensitivity, scale
    Result      Result      // pass | fail | skip | unknown
    Reason      Reason      // empty on pass; otherwise the single reason code
    Detail      string      // bounded human detail, e.g. "patch"
}

// Followable is the safe-path helper. Most callers never inspect Verdict directly.
func (r TypeRecord) Followable() bool {
    return r.Followability.Verdict == VerdictFollowable ||
        r.Followability.Verdict == VerdictRetained
}
```

The registry is small. Two lookups, two lists. The lookups always return the full
record — including the verdict and every check — so a caller never needs a second
"inspect" call to render the reason.

```go
// As built: the concrete *typeset.Registry, one per source cluster.
func (r *Registry) Ready() bool
func (r *Registry) Generation() uint64
// Revision bumps when the followable set changes (a grace drop included) or the
// scan generation moves: the cache key for per-GitTarget projections.
func (r *Registry) Revision() uint64

// Always return the full record. The bool reports whether the type is known to
// the registry at all; callers gate behavior on record.Followable().
func (r *Registry) ByGVK(gvk schema.GroupVersionKind) (TypeRecord, bool)
func (r *Registry) ByGVR(gvr schema.GroupVersionResource) (TypeRecord, bool)

// Followable returns only verdict in {followable, retained}. All returns every
// known type for inventory and "why not" views.
func (r *Registry) Followable() []TypeRecord
func (r *Registry) All() []TypeRecord
```

Consumers outside `internal/watch` read the smaller `Lookup` interface: `Ready()` and
`ByGVK`.

Per-GitTarget projection consumes records; it does not recompute them. The proposal
below is not built in this shape: the projection is `WatchedTypeTable`
(`internal/watch/watched_type_table.go`), whose `WatchedType` copies GVK, GVR, scope, and
served version from the record and maps each namespace scope to its object selector.
There is no `Rejection`: a rule whose selector matches nothing followable resolves
(`ResourcesResolved=True`) and watches zero types, and why a type is not followable is in
its registry record and in the `type is not followable` log line.

```go
type TargetView struct {
    Target TargetID
    Types  []FollowedType
}

type FollowedType struct {
    Record          TypeRecord // copied from the registry, never recomputed
    NamespaceScopes map[string]struct{}
}

func BuildTargetView(
    reg TypeRegistry, rules []WatchRule,
) (TargetView, []Rejection, error)

type Rejection struct {
    Rule   WatchRule
    Record TypeRecord // the refused/ambiguous record the rule resolved to
}
```

## Subresource and scale facts

Subresources are folded into the parent record, not followed as their own types. The
only subresource fact the writer needs is where a `/scale` mutation lands on the
parent's desired state.

```go
type Subresources struct {
    Status StatusFact
    Scale  ScaleBinding
}

type ScaleBinding struct {
    Enabled     bool
    Source      string // discovery | crd | builtin-registry | aggregated | unknown
    ResponseGVK schema.GroupVersionKind // normally autoscaling/v1 Scale

    SpecReplicasPath   string
    StatusReplicasPath string
    SelectorPath       string
    SelectorKind       string // serialized-string | label-selector | unknown

    // Usable is true only when a /scale audit event can be mapped back to a durable
    // parent field. False feeds the `scale` requirement's `scale-path-unresolved`.
    Usable bool
}
```

The design takes `SpecReplicasPath` from `CustomResourceDefinition`
`spec.versions[*].subresources.scale` for CRDs and from a small built-in registry for
built-ins, so both look identical to the writer. The CRD JSONPath rules live in
[subresources.md](../facts/subresources.md) and are not repeated here. As built, only
the built-in registry ([`internal/typeset/scale.go`](../../internal/typeset/scale.go))
fills the binding; a CRD parent with `/scale` gets `Usable: false`. The `scale`
requirement is `skip` when the type serves no `/scale` and `fail`s
(`scale-path-unresolved`) when it serves one without a known parent path. Nothing reads
the binding yet: a `/scale` audit event only names who scaled the parent, and the
replica count reaches Git through the parent's own watch event.

## Live set and the 60-second grace

Additions are fast; removals are slow. A newly served type that passes every
requirement enters the live set immediately. A previously live type that fails its
next observation is held as `retained`. A type that discovery stops listing is held
for a fixed **60 seconds** (`RemovalGrace`), after which the registry drops its record.
A type whose group/version discovery reports as failed keeps its last-known facts,
marked untrusted, and stays `retained` for as long as the failure persists. The grace
is product safety, not tuning, so it is not configurable: it stops a short discovery
blink from turning into a large Git sweep.

While retained, `ByGVK`/`ByGVR` still return the record and `Followable()` is still
true, so the watched-type tables and writer identity keep the type. A GitTarget's watch
setup does not run while one of its watched types is retained: it fails closed rather
than act on a reduced view. The verdict reads `retained` and the failing check explains
why. Once the grace expires, the record is gone: the type leaves `Followable()` and the
lookups no longer know it.

## What each consumer asks

Every consumer reads `TypeRecord`; they differ only in which fields they use and
whether they need the non-followable ones.

| Consumer | Reads | Needs refused records? |
| --- | --- | --- |
| Manifest mapper (git writer, `manifest-analyzer`) | `Identity`, `Followable()` | no |
| WatchRule → `WatchedTypeTable` | `Identity`, `Preferred`, `Followable()` | no |
| Target watch setup | `retained` state | no |
| Status, metrics, followability log | the whole record, including `refused` | yes, via `All()` |

The design also had the audit consumer and the git writer read `Subresources.Scale`,
`Sensitive`, and `Origin`. Neither does: the audit path reads no type record, and the
writer takes sensitivity from the operator's sensitive-resource policy.

## Where it lives in the code

- **Scan / `Observation`**: `APIResourceCatalog` in `internal/watch` normalizes one
  discovery result into a `typeset.Scan`; `typeset.ObservationsFromEntries` turns its
  entries into observations. There is no CRD or `APIService` enrichment.
- **`TypeRegistry`**: `typeset.Registry`, one per source cluster, fed by
  `UpdateFromScan` (see [typeset-owns-discovery-grace.md](typeset-owns-discovery-grace.md)).
  It replaced `CatalogMapper` and `RuleGVRResolver`, and owns the GVK/GVR ambiguity
  policy, the deny policy, and the removal grace.
- **`TargetView`**: `WatchedTypeTable` in `internal/watch`, which carries no identity,
  ambiguity, or removal-grace logic of its own.

## References

- [scale-subresource-audit-rehydration.md](scale-subresource-audit-rehydration.md)
- [gvk-gvr-mapping-layer.md](gvk-gvr-mapping-layer.md)
- [resource-types.yaml](../facts/resource-types.yaml)
- [subresources.md](../facts/subresources.md)
