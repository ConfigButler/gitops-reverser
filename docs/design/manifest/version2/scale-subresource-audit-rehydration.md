# Subresource Audit Resolution

> Status: design proposal, captured 2026-06-08, revised 2026-06-08.
>
> Trigger: `kubectl scale deployment` updates the live Deployment through the
> `deployments/scale` subresource, but the committed Deployment manifest is not
> updated today.
>
> Scope update: see
> [subresource-scope-reduction.md](subresource-scope-reduction.md). Future work
> should narrow this from generic subresource translation to built-in and CRD
> `/scale` only.

## Problem

Kubernetes subresources are API endpoints below a parent resource:

```text
deployments/scale
deployments/status
pods/exec
customresources/status
customresources/scale
```

Some subresources mutate desired parent state. Others are runtime commands,
status writes, logs, streams, token requests, or proxy operations. GitOps
Reverser must not treat all subresources as manifests.

The concrete event is captured at
[`internal/webhook/testdata/audit-events/deployment-scale-subresource.json`](../../../../internal/webhook/testdata/audit-events/deployment-scale-subresource.json).
Its `objectRef` identifies the parent Deployment, but its `responseObject` is an
`autoscaling/v1 Scale`, not an `apps/v1 Deployment`.

The old ingress behavior dropped every event with `objectRef.subresource != ""`.
That preserves safety, but misses author-preserving desired-state mutations such
as scale.

## Decision

Represent supported mutating subresource events as **field-patch events** against
the already committed parent manifest.

For `deployments/scale`, the event becomes:

```text
Identifier: apps/v1 deployments scale-audit-capture/scale-audit-target
Operation:  UPDATE
FieldPatch:
  Source: deployments/scale
  Assignments:
    - {Path: [spec, replicas], Value: 3}
UserInfo: {Username: system:admin}
```

The Git layer never writes the `Scale` response object and never fetches a full
parent object as commit content.

## Non-Negotiables

- Commit values come from `responseObject.spec` in the audit event only.
- Do not fall back to `requestObject.spec`; request bodies are pre-admission
  intent, not confirmed accepted state.
- Do not hydrate the parent object to build the commit body.
- A live parent GET is allowed only as a **sanitized field-presence gate**.
- Do not add `ParentKind` to `git.FieldPatch`; production resolves the parent by
  GVR/resource identity in the writer.
- Do not add subresources to `WatchedTypeTable`.
- Do not allow subresource names in `WatchRule.resources`.
- Do not write subresource response objects, such as `autoscaling/v1 Scale`, to
  Git.
- Do not mirror `status`, command, log, proxy, attach, exec, token, binding, or
  finalize-style subresources.

## Flow

```text
kube-apiserver audit event
  -> webhook ingress
  -> deny-list gate for hard-refused subresources
  -> canonical Redis stream
  -> audit consumer rule matching on parent GVR
  -> translate responseObject.spec into candidate assignments
  -> GET parent only for sanitized field-presence check
  -> drop unless every assignment path exists in sanitized parent projection
  -> git.Event{FieldPatch: ...}
  -> GitTargetEventStream / BranchWorker
  -> manifestedit.PatchFields on existing Git parent document
  -> commit authored from the audit event user
```

## Field-Patch Event Shape

`git.Event` gets a mutually exclusive field-patch payload:

```go
type Event struct {
    Object     *unstructured.Unstructured // full-object path
    FieldPatch *FieldPatch                // bounded field patch path

    Identifier types.ResourceIdentifier   // parent GVR + namespace/name
    Operation  string
    UserInfo   UserInfo
    // ... Path, GitTargetName, ...
}

type FieldPatch struct {
    Assignments []manifestedit.FieldAssignment
    Source      string // bounded label, e.g. "deployments/scale"
}
```

`ParentKind` is intentionally absent. The audit consumer should not solve GVR to
GVK for Git placement, and carrying Kind would create a half-retained optional
path. The writer already has the live-catalog mapper and should locate the parent
document by the same GVR/resource-identity path used for GVR-only deletes.

## Translation Rules

The generic translator is intentionally small:

1. Use the parent identity from `objectRef`: group, version, resource, namespace,
   and name.
2. Ignore the subresource body's `apiVersion` and `kind`.
3. Require `responseObject.spec`.
4. Walk `responseObject.spec` to leaves and emit assignments rooted at parent
   `spec`.
5. Do not read `requestObject.spec`.
6. Do not read `status`.
7. Drop the event if no candidate assignments are produced.

For scale, `responseObject.spec.replicas: 3` becomes `spec.replicas: 3`.

## Sanitized Parent Gate

After translation, the consumer GETs the current parent object and runs the same
`sanitize.Sanitize` projection as the full-object path.

The patch is allowed only if every candidate assignment path exists in the
sanitized parent projection.

This GET is a gate, not a source of commit values:

- The fetched object is never routed to Git.
- Fetched field values are never copied into assignments.
- A stale or concurrent GET can cause a false drop or false allow of a path, but
  it cannot attribute another user's value to this audit event.
- Fields stripped by the sanitizer, such as Service `spec.clusterIP`, cannot pass.

This keeps the "no guessing" rule while avoiding an allow-list for every future
desired-state subresource: a new subresource must both carry explicit accepted
values and point at fields that the sanitized parent manifest actually exposes.

The gate is all-or-nothing. If any assignment path is absent from the sanitized
parent projection, drop the whole subresource patch.

## Writer Rules

The writer applies field patches only to an existing managed parent document.

- Resolve by parent GVR/resource identity through the same inventory used by
  `manifestanalyzer.PlanDelete`.
- Do not use `ParentKind`.
- Do not create a parent from a partial patch.
- Do not whole-replace from a partial patch.
- Do not patch encrypted/non-editable parents in place.
- Use `manifestedit.PatchFields`, which owns only the assigned paths and leaves
  every other Git field untouched.

If a Deployment in Git omits `spec.replicas`, a scale patch may still add it:
the parent manifest exists, and the sanitized live Deployment exposes
`spec.replicas` after the scale.

## Denied Subresources

Hard-denied before Redis:

| Pattern | Reason |
| --- | --- |
| `*/status` | Observed state, not desired manifest state. |
| `pods/exec` | Runtime command stream. |
| `pods/attach` | Runtime stream. |
| `pods/portforward` | Runtime stream. |
| `*/proxy` for known proxy resources | Proxy request, not manifest state. |
| `pods/log` | Log retrieval. |
| `pods/eviction` | Operational eviction. |
| `bindings`, `pods/binding` | Scheduler/runtime placement. |
| `*/finalize` | Lifecycle control. |
| `*/approval` | Workflow decision. |
| `*/token` | Credential/token request. |

> Superseded: the broad hard-deny taxonomy above and the sanitized parent gate
> below were the generic-subresource design. The shipped behavior is scale-only —
> see [subresource-scope-reduction.md](subresource-scope-reduction.md). Only
> `/scale` reaches the consumer now; everything else is dropped at webhook ingress.

## Metrics

Keep outcomes bounded by group/version/resource/verb/subresource/outcome. Never
label by object name or request URI.

The shipped consumer outcomes are scale-specific (see the scope-reduction doc):

- `routed_scale_subresource`: built-in scale translated and routed.
- `dropped_non_scale_subresource`: subresource is not `scale`.
- `dropped_scale_missing_response_replicas`: scale response lacks
  `responseObject.spec.replicas`.
- `dropped_scale_path_unresolved`: no known parent replica path (CRD / aggregated API).
- `unmatched`: no parent-GVR rule matched.
- `subresource_patch_no_parent` / `subresource_patch_unsafe`: writer-side outcomes,
  logged with those `reason` strings (not yet counters).

There should be no `rehydrated_*` or `fallback_*` outcomes.

## TODO Checklist

- [x] Build `manifestedit.PatchFields` field-patch primitive.
- [x] Add field-patch transport through `git.Event`.
- [x] Teach `GitTargetEventStream` to forward field-patch events.
- [x] Teach `BranchWorker` to apply field patches to existing manifests.
- [x] Forward non-denied mutating subresources through webhook ingress.
- [x] Add consumer translation from subresource event to field-patch event.
- [x] Remove `FieldPatch.ParentKind` from `internal/git/types.go`.
- [x] Remove writer `ParentKind` / `ByManifestIdentity` fast path; resolve field
  patches by GVR/resource identity only.
- [x] Update writer tests to use the production GVR/resource-identity path.
- [x] Require `responseObject.spec` in `internal/queue/subresource_translate.go`.
- [x] Drop request-only subresource events; no `requestObject.spec` fallback.
- [x] Update translator tests: response-only succeeds, request-only drops.
- [x] **Superseded by the scope reduction** — the generic translator and the
  sanitized live parent projection gate were replaced by a scale-only translator
  (`translateScaleToAssignments`). Today it is keyed on
  `auditutil.BuiltinScaleReplicasPath`; the target shape moves those built-in
  paths into the same API resource scale fact that CRD scale will use. The gate,
  its tests, and `dropped_subresource_field_not_in_parent` were removed; the
  scale-specific outcomes are in the Metrics section above. See
  [subresource-scope-reduction.md](subresource-scope-reduction.md).
- [ ] Add the writer outcome counters `subresource_patch_no_parent` /
  `subresource_patch_unsafe` (today logged with those exact `reason` strings, not yet
  counters).
- [ ] Move built-in scale paths into the shared API resource scale fact, so
  built-ins and future CRD scale paths use the same translator input.
- [ ] Add CRD scale path remap from `specReplicasPath`. Until then a CRD scale is
  **safely dropped** (`dropped_scale_path_unresolved`), never miswritten.
- [ ] Run `task lint`.
- [ ] Run `task test`.
- [ ] Check Docker with `docker info`, then run `task test-e2e`.

## Current State

The scope reduction has landed (see
[subresource-scope-reduction.md](subresource-scope-reduction.md)). The shipped
behavior is scale-only, and the generic translator plus the sanitized parent gate
are gone:

- `FieldPatch.ParentKind` is removed. The writer resolves a field patch's parent
  solely by its objectRef GVR through the same resource-identity inventory the
  GVR-only delete uses (`manifestanalyzer.PlanDelete`); the patch is then applied
  with the parent document's own committed Kind.
- The webhook forwards only `*/scale` mutating events (`IsScaleSubresource`); every
  other subresource is dropped before Redis.
- The consumer translator (`translateScaleToAssignments`) reads only
  `responseObject.spec.replicas` and currently resolves the parent replica path
  from built-in policy (`auditutil.BuiltinScaleReplicasPath`). The target shape is
  to move that built-in path registry into the API resource scale fact, so
  built-ins and CRDs are handled through the same input model. A request-only event
  drops, and a scale whose parent path is unknown (CRD / aggregated API) drops as
  `dropped_scale_path_unresolved` — never defaulted to `.spec.replicas`.
- The sanitized live parent projection gate has been **removed**: with scale-only
  support the accepted value comes straight from the standardized Scale response, so
  no live-parent GET is needed to authorize the patch.

Still narrower-than-final:

- The writer `subresource_patch_no_parent` / `subresource_patch_unsafe` outcomes are
  logged with those `reason` strings, not yet counters.
- CRD scale-path remap from `specReplicasPath` is not implemented. CRD scale is
  safely dropped (`dropped_scale_path_unresolved`), never miswritten.
