# Subresource Audit Resolution

> Status: design proposal, captured 2026-06-08, revised 2026-06-08.
>
> Trigger: `kubectl scale deployment` updates the live Deployment through the
> `deployments/scale` subresource, but the committed Deployment manifest is not
> updated today.

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

Everything else may reach the consumer, but it must still pass translation and
the sanitized parent gate.

## Metrics

Keep outcomes bounded by group/version/resource/verb/subresource/outcome. Never
label by object name or request URI.

- `routed_subresource`: translated, passed the sanitized parent gate, and routed.
- `dropped_denied_subresource`: denied at webhook ingress.
- `unmatched`: no parent-GVR rule matched.
- `dropped_unsupported_subresource`: no usable `responseObject.spec`.
- `dropped_subresource_field_not_in_parent`: assignment path absent from sanitized
  parent projection.
- `subresource_patch_no_parent`: parent manifest absent from Git.
- `subresource_patch_unsafe`: parent encrypted or not field-patchable.

There should be no `rehydrated_*` or `fallback_*` outcomes.

## TODO Checklist

- [x] Build `manifestedit.PatchFields` field-patch primitive.
- [x] Add field-patch transport through `git.Event`.
- [x] Teach `GitTargetEventStream` to forward field-patch events.
- [x] Teach `BranchWorker` to apply field patches to existing manifests.
- [x] Forward non-denied mutating subresources through webhook ingress.
- [x] Add consumer translation from subresource event to field-patch event.
- [ ] Remove `FieldPatch.ParentKind` from `internal/git/types.go`.
- [ ] Remove writer `ParentKind` / `ByManifestIdentity` fast path; resolve field
  patches by GVR/resource identity only.
- [ ] Update writer tests to use the production GVR/resource-identity path.
- [ ] Require `responseObject.spec` in `internal/queue/subresource_translate.go`.
- [ ] Drop request-only subresource events; no `requestObject.spec` fallback.
- [ ] Update translator tests: response-only succeeds, request-only drops.
- [ ] Add sanitized live parent projection gate in the audit consumer.
- [ ] Add gate tests: `spec.replicas` passes; stripped fields such as Service
  `spec.clusterIP` drop.
- [ ] Add/drop metrics for the taxonomy above.
- [ ] Add CRD scale path remap from `specReplicasPath`.
- [ ] Run `task lint`.
- [ ] Run `task test`.
- [ ] Check Docker with `docker info`, then run `task test-e2e`.

## Current State

The staged code proves the transport and writer mechanics, but it is still wider
than the intended final design:

- `FieldPatch.ParentKind` still exists and should be removed.
- The writer still has an optional `ParentKind` fast path and should use only the
  GVR/resource-identity path.
- The translator still falls back to `requestObject.spec`; that fallback should
  be removed.
- The sanitized live parent projection gate is not implemented yet.

Until those items are done, the implementation remains deliberately marked
partial.
