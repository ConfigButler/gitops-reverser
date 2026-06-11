# Subresource Scope Reduction

> Status: improvement proposal, captured 2026-06-08.
>
> Amends:
> [api-catalog-watched-type-architecture.md](api-catalog-watched-type-architecture.md)
> and
> [scale-subresource-audit-rehydration.md](../../../finished/scale-subresource-audit-rehydration.md).
>
> Trigger: after reviewing Kubernetes subresource facts and the recorded
> `deployments/scale` audit event, the previous generic-subresource direction is
> broader than GitOps Reverser needs.

## Decision

For the first simplification pass, support only Kubernetes `/scale` subresources
whose parent replica path is known. Built-in Kubernetes types get that path from
a built-in scale pointer registry; CRDs eventually get the same fields from CRD
`spec.versions[*].subresources.scale`.

Ignore every other subresource for now.

Do not add CRD scale support until the richer API resource object exists. CRD
scale needs that object to carry the parent `specReplicasPath`; until then, a CRD
scale event must fail clearly as "scale path unresolved" rather than guessing
`.spec.replicas`.

Do not add special aggregated-API handling. Aggregated API subresources should
fall through the same unsupported path as any other non-built-in subresource. If
an aggregated API exposes `/scale`, it will not have a known parent replica path
in the current API resource model and should be dropped with the same "scale path
unresolved" outcome.

## Why This Is Enough

The recorded Deployment scale event proves the useful case:

- the request is addressed to `deployments/scale`;
- the audit `responseObject` is `autoscaling/v1 Scale`, not `apps/v1 Deployment`;
- the parent Deployment is still the object that changed;
- the accepted desired value is `responseObject.spec.replicas`;
- the `Scale.metadata.resourceVersion` matched the normal parent Deployment read
  in the local capture.

That is a narrow, valuable GitOps case: a subresource writes parent desired
state, and Kubernetes exposes the accepted value in a standardized response.

Most other subresources do not have that shape. They are observed state, runtime
streams, derived reads, credentials, lifecycle control, proxying, or imperative
actions. Treating them as generic parent `spec` patches invites accidental
mirroring of behavior that is not durable desired state.

## Scope

### In Scope

- `*/scale` on scalable resources whose parent path is known.
- Built-in scalable resources whose paths come from the built-in scale pointer
  registry, such as Deployments, StatefulSets, ReplicaSets, and
  ReplicationControllers.
- Metrics for dropped subresources.
- Parent-resource WatchRule matching. Rules continue to name `deployments`, not
  `deployments/scale`.
- Field-patch writes into existing parent manifests.

### Out Of Scope

- Generic translation of arbitrary `responseObject.spec` trees.
- Subresources in `WatchRule.resources`.
- Subresources in `WatchedTypeTable`.
- Writing subresource response objects to Git.
- CRD scale support until CRD scale pointers are carried on the API resource
  object.
- Aggregated API subresource support.
- Status, exec, attach, portforward, log, proxy, eviction, binding, finalize,
  approval, token, restart, console, VNC, or similar operation subresources.

## Architecture Tune-Back

The API catalog should remain a raw discovery cache for served resources. It may
retain raw subresource entries because Kubernetes discovery reports them, but
GitOps Reverser should not build an operational subresource type system on top of
them.

The resolved type surface, if kept, should resolve parent resources only:

- exact GVK to parent GVR;
- exact GVR to parent type facts;
- top-level resource ambiguity and disallowed policy;
- subresource-only matches refused as a planning miss.

`WatchedTypeTable` stays GitTarget-local and parent-GVR-only. A watched type is
the object we list, watch, snapshot, and write as a manifest. A subresource is an
audit-event modifier on that parent, not a watched type.

This means we can drop the broad direction where every subresource gets a generic
translation opportunity guarded by sanitized parent field presence. Scale support
should be explicit: if the parent replica path is not known, the event is dropped.

## Scale Translation

Replace generic subresource translation with a scale-specific translator.

Input requirements:

- `objectRef.subresource == "scale"`;
- verb maps to a mutating operation;
- `responseObject.spec.replicas` exists;
- parent GVR matches at least one WatchRule or ClusterWatchRule;
- parent replica path is known by the API resource object.

Translation:

```text
autoscaling/v1 Scale responseObject.spec.replicas
  -> parent manifest assignment at resolved replicas path
```

For built-in scalable resources, the API resource object should be populated from
the built-in scale pointer registry, so the translator sees the same shape it
will later see for CRDs:

```text
responseObject.spec.replicas -> parent .spec.replicas
```

For CRDs and aggregated APIs in the first pass:

```text
no known parent replica path -> dropped_scale_path_unresolved
```

Do not read:

- `requestObject.spec`;
- `responseObject.status`;
- arbitrary leaves under `responseObject.spec`;
- the subresource body's `apiVersion` or `kind` as parent identity.

## CRD Scale Deferred

Do not add a separate CRD scale path index in this pass.

The desired future shape is a richer API resource object that can answer:

```text
parent GVR -> scale parent replica path
```

For a built-in Deployment this answer should come from the built-in scale pointer
registry:

```text
apps/v1 deployments -> ["spec", "replicas"]
```

For a CRD with scale enabled, the future answer should come from:

```text
spec.versions[*].subresources.scale.specReplicasPath
```

Until that API resource object exists, CRD scale events are not supported. They
must not be special-cased through a parallel CRD informer, and they must not
default to `.spec.replicas`.

## Metrics

Keep labels bounded:

```text
source, group, version, resource, subresource, verb, outcome
```

Do not label by name, namespace, UID, request URI, or backend identity.

Recommended outcomes:

| Outcome | Meaning |
| --- | --- |
| `routed_scale_subresource` | Scale translated and routed with a known parent path. |
| `dropped_non_scale_subresource` | Subresource is not `scale` and is not supported. |
| `dropped_scale_missing_response_replicas` | Scale response lacks `responseObject.spec.replicas`. |
| `dropped_scale_path_unresolved` | No known parent replica path exists for this resource. |
| `scale_patch_no_parent` | Parent manifest absent from Git. |
| `scale_patch_unsafe` | Parent manifest exists but cannot be patched safely. |

`audit_events_received_total` can continue to expose the raw `subresource` label,
but the explicit dropped outcomes are what make ignored subresources visible.

## Implementation Plan

1. Replace `translateSubresourceToAssignments` with a scale-only translator.
2. Keep `manifestedit.PatchFields` and `git.FieldPatch`; they are the right write
   primitive for Scale.
3. Remove the sanitized parent field-presence gate from subresource routing.
4. Add built-in scale paths through the same API resource field that CRD scale
   will use later; do not scatter built-in path conditionals through the
   translator.
5. Do not add CRD scale path indexing yet.
6. Do not add aggregated API classification for subresource handling.
7. Change webhook ingress to forward only candidate Scale events; drop and metric
   non-scale subresources.
8. Change consumer metrics from generic subresource outcomes to scale-specific
   outcomes.
9. Add tests:
   - built-in Deployment scale routes to `spec.replicas`;
   - built-in Deployment scale path is read from the API resource scale fact, not
     a translator-local conditional;
   - CRD scale is dropped with `dropped_scale_path_unresolved`;
   - generic `responseObject.spec.foo` subresource is dropped;
   - aggregated API scale is dropped with `dropped_scale_path_unresolved`;
   - `WatchRule.resources: ["deployments/scale"]` remains rejected.

## Complexity To Remove

- Generic "walk every leaf of `responseObject.spec`" subresource translation.
- The idea that sanitized parent field presence is enough to authorize unknown
  subresources.
- The sanitized parent field-presence gate for subresource routing.
- A parallel CRD scale-path index.
- Translator-local built-in scale conditionals once the built-in registry
  populates the shared API resource scale fact.
- Aggregated API subresource classification.
- Any future work to add subresource entries to watched-type planning.
- Any resolved type-surface API that treats subresources as first-class selected
  types.
- Broad hard-deny taxonomy as the main safety mechanism. With scale-only support,
  the primary rule is simpler: only `scale` can route; everything else drops.

## Revised Principle

Subresources are not a new manifest surface for GitOps Reverser.

They are ignored by default. `/scale` is the single exception because Kubernetes
standardizes it as a view that writes parent desired replica state. Even `/scale`
routes only when the parent replica path is known.
