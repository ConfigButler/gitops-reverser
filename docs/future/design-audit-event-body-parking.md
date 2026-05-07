# Design: audit event body parking

> Status: design - concrete simplification of
> [idea-audit-enrichment-side-channel.md](idea-audit-enrichment-side-channel.md).
> Date: 2026-05-07

## Summary

Keep `apiservice-audit-proxy` exactly as it is today. It continues to send synthetic
`audit.k8s.io/v1 EventList` payloads, but gitops-reverser gives those payloads a clearly separate
receiver:

```text
POST /audit-webhook-additional
```

gitops-reverser changes how it handles duplicate events with the same `auditID`:

- full synthetic events from `apiservice-audit-proxy` are parked in Redis/Valkey by `auditID`
- shallow official kube-apiserver events arrive on `/audit-webhook`
- the handler joins the two signals into one canonical event before writing to the audit stream
- operators choose whether the first event wins or whether gitops-reverser waits for the
  official kube-apiserver event

This is a deliberately smaller design than the earlier `/audit-enrichment` proposal. It avoids
changing the proxy's wire format and behavior, but still makes the source split visible in
configuration, routing, metrics, and mental model.

## Goals

- Preserve the current `apiservice-audit-proxy` wire contract.
- Produce at most one canonical stream event per `auditID`.
- Recover `requestObject`, `responseObject`, and better `objectRef` data for aggregated API
  requests when the proxy-provided event is available.
- Make ordering policy explicit:
  - fast mode: take the first usable event
  - ordered mode: wait for the official kube-apiserver event
- Remove the `{clusterID}` path segment from the audit webhook endpoint.
- Make proxy/additional input visibly separate from official kube-apiserver input.
- Allow deployments that intentionally use only the additional stream.
- Keep the design reusable for future request-body sources, especially
  [end-user supplied commit messages](idea-end-user-commit-messages.md).

## Non-goals

- No change to `apiservice-audit-proxy`.
- No custom enrichment payload format.
- No new audit fan-out hub.
- No attempt to solve multi-cluster ingestion in this pass.
- No long-lived audit archive. Redis parking entries are short-lived join state only.

## Endpoint shape

The official kube-apiserver audit webhook endpoint becomes:

```text
POST /audit-webhook
```

The additional audit webhook endpoint is:

```text
POST /audit-webhook-additional
```

Both endpoints accept the same Kubernetes audit webhook payload:

```text
audit.k8s.io/v1 EventList
```

The difference is semantic:

- `/audit-webhook` is the official kube-apiserver source. Events from this endpoint are the
  preferred source for audit identity, ordering, response status, and timestamps.
- `/audit-webhook-additional` is a supplementary source. Events from this endpoint may provide
  request/response bodies or other completion data for the same `auditID`.

The name is intentionally boring. `additional` says "same audit webhook shape, extra source." It
does not imply a new API, a new envelope, or an enrichment-specific protocol. A slightly cleaner
future bikeshed would be `/audit-webhook-supplemental`, but this design uses
`/audit-webhook-additional` because it is direct and hard to misread.

The previous shape is removed:

```text
POST /audit-webhook/{clusterID}
POST /audit-webhook-additional/{clusterID}
```

This is intentionally breaking. For the current product stage, a path-level cluster identity is
more confusing than useful because the rest of the pipeline does not fully model cluster identity
yet. It is easier to add a first-class multi-cluster contract later than to explain a path field
that appears supported but cannot be used consistently.

Concrete cleanup implied by this:

- delete `extractClusterID`
- remove `clusterID` from `AuditEventQueue.Enqueue`
- remove `cluster_id` from Redis stream entries
- remove `cluster_id` from audit metrics
- remove `clusterID` from audit event IDs and dedupe keys
- update kube-apiserver audit webhook kubeconfig examples to use `/audit-webhook`
- update `apiservice-audit-proxy` webhook target examples to use `/audit-webhook-additional`
- reject `/audit-webhook/{anything}` and `/audit-webhook-additional/{anything}` instead of treating
  them as cluster identities

If multi-cluster support returns later, it should be represented as a real source identity model:
explicit source registration, rule matching, metrics cardinality rules, and file-path semantics.

## Deployment modes

The two endpoints support three deployment modes:

- **Official only.** kube-apiserver posts to `/audit-webhook`; nothing posts to
  `/audit-webhook-additional`. This is the current non-proxy shape, minus the cluster ID path.
- **Official plus additional.** kube-apiserver posts to `/audit-webhook`; `apiservice-audit-proxy`
  posts to `/audit-webhook-additional`. This is the preferred high-fidelity mode.
- **Additional only.** `apiservice-audit-proxy` posts to `/audit-webhook-additional`; kube-apiserver
  is not configured to post to gitops-reverser. In this mode gitops-reverser treats the additional
  stream as canonical because there is no official stream to wait for.

Additional-only mode is explicitly supported because it is useful for local tests, prototype
clusters, or deployments where the operator accepts the proxy's best-effort delivery semantics.
It should be documented as lower authority than official-plus-additional mode because the proxy
event is not kube-apiserver's batched/retried audit webhook delivery.

## Current duplicate shape

For an aggregated API request routed through `apiservice-audit-proxy`, gitops-reverser can receive
two `ResponseComplete` audit events with the same `auditID`:

1. **Synthetic full event** from `apiservice-audit-proxy`
   - arrives through `/audit-webhook-additional`
   - usually arrives first
   - contains captured `requestObject` and `responseObject`
   - may have a better `objectRef.name`
   - is best-effort because the proxy sends it asynchronously once
2. **Official shallow event** from kube-apiserver
   - arrives through `/audit-webhook`
   - usually arrives later because kube-apiserver batches and retries audit webhook delivery
   - carries the official audit stream ordering
   - often lacks bodies for aggregated API requests

Today both can enter the canonical Redis stream. This design makes the canonical stream choose one.

## Redis parking construct

Add a small component, working name `AuditEventJoiner`, used by `AuditHandler` before enqueueing.

It uses Redis/Valkey keys:

```text
audit:body:v1:<auditID>
audit:decision:v1:<auditID>
```

`audit:body:v1:<auditID>` parks the best known full-body event:

```json
{
  "v": 1,
  "auditID": "0f6c...",
  "receivedAt": "2026-05-07T12:34:56Z",
  "event": { "kind": "Event", "apiVersion": "audit.k8s.io/v1" },
  "hasRequestObject": true,
  "hasResponseObject": true
}
```

`audit:decision:v1:<auditID>` records that the canonical stream already received a decision for
that audit ID. Its value is intentionally small:

```json
{
  "v": 1,
  "mode": "wait-official",
  "emittedAt": "2026-05-07T12:34:57Z",
  "emitted": "merged"
}
```

Both keys use the same TTL. Recommended initial value: `5m`.

The body key prevents losing the synthetic payload while waiting for the official event. The
decision key prevents the second arrival from being enqueued later.

## Classification

Because the proxy is unchanged, gitops-reverser should not depend on a new source marker inside the
payload. Classification is based on the HTTP endpoint, a small allowlist, and the event shape:

- **official source:** event received on `/audit-webhook`
- **additional source:** event received on `/audit-webhook-additional`
- **parkable resource:** `objectRef.apiGroup` is listed in
  `--audit-event-body-parking-api-groups`
- **full-body candidate:** additional-source parkable resource with `requestObject != nil` or
  `responseObject != nil`
- **shallow official candidate:** parkable resource with the same `auditID`, same stage, but no
  request/response body
- **unrelated normal event:** no parked body and no duplicate decision

This is deliberately pragmatic. It is good enough for the current duplicate problem because the
configured allowlist tells gitops-reverser which API groups are expected to have a proxy-provided
body twin, and the endpoint tells gitops-reverser whether the event is official or additional.

If a future native kube-apiserver event includes bodies for one of these groups, it is still
official-source input and should emit normally. Body parking is only for additional-source events.

## Join modes

Configuration:

```text
--audit-event-join-mode=wait-official | first
--audit-event-body-ttl=5m
--audit-event-body-parking-api-groups=wardle.example.com,gitops-reverser.io
--audit-additional-only=false
```

Default: `wait-official`.

Only API groups listed in `--audit-event-body-parking-api-groups` participate in body parking.
This avoids accidentally parking ordinary official audit events that already include bodies. The
list should contain aggregated API groups known to be routed through `apiservice-audit-proxy`.

`--audit-additional-only=true` is a deployment-mode switch, not a join strategy. It means
gitops-reverser should treat `/audit-webhook-additional` as the canonical source because
`/audit-webhook` is not expected to receive matching official events.

### Mode: `wait-official`

This mode optimizes for canonical audit ordering.

When an additional full-body event arrives first:

1. store it in `audit:body:v1:<auditID>`
2. do not enqueue anything yet
3. return `200` to the sender

When the official event arrives:

1. read `audit:body:v1:<auditID>`
2. if present, merge the official event with the parked body event
3. enqueue the merged event to the canonical audit stream
4. write `audit:decision:v1:<auditID>`
5. delete `audit:body:v1:<auditID>`

If no parked body exists when the official event arrives, enqueue the official event as-is and write
the decision key. This avoids building a delayed queue for the less common native-first race.

If `--audit-additional-only=true`, additional events do not park in this mode. They emit directly
because no official event is expected.

Trade-off: most aggregated API events wait for kube-apiserver's batch delivery before they reach the
consumer. Ordering is more correct because the canonical stream advances on the official event.

### Mode: `first`

This mode optimizes for low latency.

When the first event for an `auditID` arrives:

1. enqueue it immediately
2. write `audit:decision:v1:<auditID>`
3. do not keep a parked body

When a later event with the same `auditID` arrives, drop it because the decision key already exists.

If the synthetic full event arrives first, the canonical stream gets bodies quickly. If the official
shallow event arrives first, the canonical stream gets the official event and the later full body is
ignored.

Trade-off: stream ordering can follow the proxy's best-effort send timing instead of
kube-apiserver's official audit delivery timing.

## Merge rule

When joining a parked full event with an official event, the official event remains authoritative for:

- `auditID`
- `level`
- `stage`
- `requestURI`
- `verb`
- `user`
- `impersonatedUser`
- `sourceIPs`
- `userAgent`
- `requestReceivedTimestamp`
- `stageTimestamp`
- `responseStatus`

The parked full event may contribute:

- `requestObject`
- `responseObject`
- `objectRef.name`
- `objectRef.namespace`
- `objectRef.uid`
- `objectRef.resourceVersion`
- proxy truncation annotations

The merge must never replace official identity, timestamps, status, or authorization context with
synthetic values. The proxy event is a body source, not the source of audit authority.

## Handler flow

`AuditHandler` still decodes `auditv1.EventList`. It passes the endpoint role into the joiner. For
each event:

1. validate and filter as today
2. call `AuditEventJoiner.Decide(ctx, source, event)`
3. act on the returned decision:
   - `Parked`: acknowledge request, no stream write
   - `Emit`: enqueue returned event
   - `DropDuplicate`: acknowledge request, no stream write

The queue remains the boundary to downstream processing. The consumer should not have to reason
about two events with the same `auditID` for this case.

## CommitContext fit

The later `CommitContext` work has the same underlying shape: a shallow official audit event needs
request-body content from somewhere else.

The source is different:

- for aggregated APIs behind `apiservice-audit-proxy`, the body source is the proxy's synthetic
  audit event
- for gitops-reverser's own aggregated `CommitContext` API, the body source is the handler-local
  request body stashed by `Audit-ID`

The joiner should therefore be designed around a generic concept:

```text
auditID -> parked request context -> official audit event -> completed canonical event
```

Do not hardcode the implementation as "proxy enrichment only." A small interface is enough:

```go
type AuditBodyStore interface {
    Park(ctx context.Context, auditID string, body AuditBodyEnvelope) error
    Get(ctx context.Context, auditID string) (AuditBodyEnvelope, bool, error)
    Delete(ctx context.Context, auditID string) error
}
```

`CommitContext` can later write to the same kind of store with a different envelope source. It is
still important that `CommitContext` does not take effect from the stash alone; the official audit
event must be the trigger.

## Metrics

Keep this smaller than the exploratory side-channel proposal:

- `audit_join_parked_total{source="body"}`
- `audit_join_emitted_total{source, mode, result="as_is|merged|first|additional_only"}`
- `audit_join_duplicate_dropped_total{mode}`
- `audit_join_body_miss_total{mode}`
- `audit_join_body_orphan_total`

`audit_join_body_orphan_total` can be implemented later with either keyspace notifications or a
small sweeper index. It should not block the first implementation.

## Failure behavior

- **Redis unavailable while handling an additional full-body event.**
  In `wait-official` mode, return `503`; accepting the proxy event without parking loses the body.
  In `first` mode, enqueue directly only if the canonical queue is available.
- **Redis unavailable while handling an official event.**
  Return `503` so kube-apiserver can retry according to its audit webhook behavior.
- **Parked body expires before official event arrives.**
  The official event later enqueues as-is. This is a degraded event, not a poison pill.
- **Proxy event never arrives.**
  Official event enqueues as-is.
- **Official event never arrives in `wait-official` mode.**
  Parked body expires and no canonical event is emitted. This is consistent with "official audit is
  the trigger" mode.
- **Additional-only mode receives only proxy events.**
  Additional events emit directly. Missing kube-apiserver audit delivery is expected in this mode,
  so body-miss metrics should not fire for lack of an official twin.
- **Process crash after enqueue before decision key.**
  A duplicate can be emitted on replay. To avoid this, implementation should write the decision key
  before enqueueing and make enqueue failure clear the decision key, or use a small Lua script for
  the decision transition. This detail belongs in implementation, not in the public API.

## Implementation outline

1. Change the official audit HTTP route to accept only `/audit-webhook`.
2. Remove cluster ID extraction and stream fields.
3. Add `/audit-webhook-additional` with the same `EventList` decoder and authentication posture.
4. Add `AuditEventJoiner` and a Redis-backed `AuditBodyStore`.
5. Add configuration for join mode, body TTL, parking API groups, and additional-only mode.
6. Call the joiner from `AuditHandler` before `Queue.Enqueue`, passing the endpoint source.
7. Add table-driven unit tests for:
   - full first, wait-official parks
   - official after full emits merged
   - official first emits shallow in wait-official
   - full first emits immediately in first mode
   - duplicate after decision drops
   - additional-only mode emits additional events directly
   - full event outside the parking allowlist emits normally
   - Redis failures return the right handler status
8. Add e2e coverage for the aggregated API path proving one canonical stream entry per `auditID`.

## Migration

This is intentionally breaking for the audit webhook URL.

Operators must update kube-apiserver audit webhook kubeconfigs from:

```text
https://<gitops-reverser-audit>/audit-webhook/<cluster-id>
```

to:

```text
https://<gitops-reverser-audit>/audit-webhook
```

`apiservice-audit-proxy` deployments keep using their existing webhook sender behavior. Only the
target URL changes:

```text
https://<gitops-reverser-audit>/audit-webhook-additional
```

## Open questions

- Should `wait-official` or `first` be the shipped default? This design recommends
  `wait-official`, but existing installations may prefer `first` during transition.
- Is `/audit-webhook-additional` the right name? It is explicit and compatible with the existing
  audit webhook mental model. `/audit-webhook-supplemental` is slightly more precise but less
  plain.
- Should parking be configured by API group only, or by group/resource? Group-only is simpler;
  group/resource is safer when one aggregated API group contains both proxied and unproxied
  resources.
- Should the decision transition use Redis Lua from the start, or is ordinary `SET NX` plus cleanup
  enough for v1?
