# Type followability model

> Status: greenfield proposal, captured 2026-06-08
>
> Supersedes the layered model in
> [api-catalog-watched-type-architecture.md](api-catalog-watched-type-architecture.md).
> That doc grounds in today's packages and ends up with seven layers and two
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
than naming it after the requirement and letting the reason speak ("not followable
— missing required verb: patch"). So the model has one list of named requirement
checks, and the failing check *is* the explanation.

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
   (no separate                       namespace/operation scope; never
    mapper object)                    recomputes followability
```

- **Scan** gathers raw facts and produces an `Observation` per served type. It
  joins discovery with `CustomResourceDefinition` and `APIService` evidence and the
  built-in scale registry. It makes no product decisions.
- **`TypeRegistry`** is the only place that turns observations + policy into a
  followability verdict. It owns identity rules, the deny/sensitive policy, the live
  set, and the 60-second removal grace. Every consumer reads from it.
- **`TargetView`** is a GitTarget's projection of *already-followable* records into
  namespaces and operations. It never recomputes followability.

The mapper is not a layer — it is `TypeRegistry.ByGVK`. WatchRule expansion is not a
layer — it is a function that builds a `TargetView` by asking the registry.

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
| `verbs` | discovery advertises `get`, `list`, `watch`, `patch` (detail names the missing verb) | `missing-verb` |
| `origin` | classified `builtin`, `crd`, or `aggregated` with evidence | `origin-unknown` |
| `policy` | product policy permits mirroring this type | `denied-by-policy` |
| `sensitivity` | not sensitive, or sensitivity has supported encryption/write handling | `sensitive-unsupported` |
| `scale` | scale is unused, or its parent replica path is known (not guessed) | `scale-path-unresolved` |

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
origin: { kind: aggregated, confidence: observed, evidence: v1beta1.metrics.k8s.io }
verdict: refused
summary: not followable — missing required verb: watch, patch
checks:
  - { requirement: served,   result: pass }
  - { requirement: trusted,  result: pass }
  - { requirement: identity, result: pass }
  - { requirement: verbs,    result: fail, reason: missing-verb, detail: "watch, patch" }
```

Refused for ambiguous identity:

```yaml
gvk: example.com/v1 Widget
verdict: refused
summary: not followable — GVK served by two GVRs
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
    Summary string  // one line, e.g. "not followable — missing required verb: patch"
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
type TypeRegistry interface {
    Ready() bool
    Generation() uint64

    // Always return the full record. The bool reports whether the type is known to
    // the registry at all; callers gate behavior on record.Followable().
    ByGVK(ctx context.Context, gvk schema.GroupVersionKind) (TypeRecord, bool, error)
    ByGVR(ctx context.Context, gvr schema.GroupVersionResource) (TypeRecord, bool, error)

    // Followable returns only verdict in {followable, retained}. All returns every
    // known type for inventory and "why not" views.
    Followable(ctx context.Context) ([]TypeRecord, error)
    All(ctx context.Context) ([]TypeRecord, error)
}
```

Per-GitTarget projection consumes records; it does not recompute them. A rule that
matches nothing followable returns the refused record so the operator sees why.

```go
type TargetView struct {
    Target TargetID
    Types  []FollowedType
}

type FollowedType struct {
    Record       TypeRecord // copied from the registry, never recomputed
    NamespaceOps map[string]OperationSet
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

`SpecReplicasPath` comes from `CustomResourceDefinition` `spec.versions[*].subresources.scale`
for CRDs and from a small built-in registry for built-ins, so both look identical to
the writer. The CRD JSONPath rules live in
[../../../facts/subresources.md](../../../facts/subresources.md) and are not repeated
here. The scale write path depends only on `SpecReplicasPath`; selector facts are for
reporting. The `scale` requirement is `skip` when scale is unused and only `fail`s
(`scale-path-unresolved`) when a needed parent path is missing.

## Live set and the 60-second grace

Additions are fast; removals are slow. A newly served type that passes every
requirement enters the live set immediately. A previously live type that fails its
next `served`/`trusted` observation is held as `retained` for a fixed **60 seconds**
before it leaves the live set as `refused`/`unknown`. The grace is product safety,
not tuning, so it is not configurable: it stops a short discovery blink from turning
into a large Git sweep.

While retained, `ByGVK`/`ByGVR` still return the record and `Followable()` is still
true, so planning, snapshots, informers, and writer identity keep working. The
verdict reads `retained` and the failing check explains why. Once the grace expires,
the type drops from `Followable()` and a deliberate sweep can act on a stable
absence.

## What each consumer asks

Every consumer reads `TypeRecord`; they differ only in which fields they use and
whether they need the non-followable ones.

| Consumer | Reads | Needs refused records? |
| --- | --- | --- |
| Manifest mapper | `Identity`, `Followable()` | no |
| WatchRule → `TargetView` | `Identity`, verbs, scope, `Followable()` | yes, as `Rejection` |
| Snapshot / informer | `Identity`, scope, `retained` state | no |
| Audit consumer | parent `Identity`, `Subresources.Scale.SpecReplicasPath` | no |
| Git writer | `Sensitive`, `Origin`, `Subresources.Scale` | no |
| CLI / status / GUI | the whole record, including `refused` | yes, via `All()` |

The `/scale` case is the pressure test: the audit consumer calls `ByGVR` for the
parent, checks `Followable()`, and reads `Subresources.Scale.SpecReplicasPath`. If
that path is empty the `scale` check is `fail` and the translator refuses rather than
guessing `.spec.replicas`.

## Relationship to today's code

Greenfield names, but they land on existing packages:

- **Scan / `Observation`** ← today's `APIResourceCatalog` refresh plus the new CRD /
  `APIService` enrichment.
- **`TypeRegistry`** ← the resolved surface that does not exist yet; absorbs
  `CatalogMapper` (becomes `ByGVK`), the GVK/GVR ambiguity policy, the sensitive /
  deny policy, and the live-set hysteresis currently scattered across
  `RuleGVRResolver` and `WatchedTypeTable`.
- **`TargetView`** ← today's `WatchedTypeTable`, stripped of identity, ambiguity, and
  removal-grace logic now owned by the registry.

## References

- [api-catalog-watched-type-architecture.md](api-catalog-watched-type-architecture.md)
- [catalog-mapper-vs-watched-type-table.md](catalog-mapper-vs-watched-type-table.md)
- [subresource-scope-reduction.md](subresource-scope-reduction.md)
- [gvk-gvr-mapping-layer.md](../gvk-gvr-mapping-layer.md)
- [resource-types.md](../../../facts/resource-types.md)
- [subresources.md](../../../facts/subresources.md)
