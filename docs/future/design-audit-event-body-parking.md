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
- Produce at most one canonical stream event per `auditID` within the decision-key TTL
  window. The invariant is explicitly TTL-bounded; see
  [Redis parking construct](#redis-parking-construct).
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

**Scope choice: this is a deliberate expansion of v1 beyond the strict "fix the duplicate
auditID problem" objective.** The path-level cluster identity is removed because it is more
confusing than useful at the current product stage — the rest of the pipeline does not fully
model cluster identity yet. Doing the cleanup now means changes touch each affected file once;
splitting it into a separate later PR means double-touching the handler, the queue interface,
Redis stream entries, and metric labels. Reviewers should expect this scope.

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

**Operations note: parking allowlist drift.** When a cluster operator adds a new aggregated
API behind `apiservice-audit-proxy`, that API's group must also be added to
`--audit-event-body-parking-api-groups`. The
`gitopsreverser_audit_join_body_unexpected_total{group}` metric (see [Metrics](#metrics))
surfaces this drift: any non-zero value means the proxy is sending bodies for an API group the
joiner is not expecting, and those bodies are being ignored. Wire it into the standard
alerting ruleset with a low threshold.

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

`audit:body:v1:<auditID>` parks a *body contribution* — not a full audit event. It carries
exactly the fields the joiner is allowed to merge onto the official event (see
[Merge rule](#merge-rule)), plus identifying and truncation metadata. The shape works
unchanged for both v1's proxy writer and the post-v1 in-process `CommitContext` writer:

```json
{
  "v": 1,
  "auditID": "0f6c...",
  "source": "apiservice-audit-proxy",
  "receivedAt": "2026-05-07T12:34:56Z",
  "requestObject": { },
  "responseObject": { },
  "objectRef": {
    "name": "checkout",
    "namespace": "team-a",
    "uid": "...",
    "resourceVersion": "..."
  },
  "annotations": {
    "audit.k8s.io/proxy.requestObject.truncated": "true"
  },
  "hasRequestObject": true,
  "hasResponseObject": true
}
```

`source` distinguishes the writer. v1 emits `"apiservice-audit-proxy"`; post-v1 the
in-process aggregated-API handler emits `"gitops-reverser-aggregated-api"`. All other body
fields are optional; writers populate what they have.

The additional-endpoint handler builds this envelope by extracting body fields out of the
inbound `auditv1.Event`. The post-v1 in-process handler builds it directly from the
validated request. Both writers produce the same envelope shape; the joiner does not care
which writer produced it.

`audit:decision:v1:<auditID>` records that the canonical stream already saw a decision for
that audit ID. The value is small and includes the lifecycle state:

```json
{
  "v": 1,
  "state": "emitted",
  "mode": "wait-official",
  "claimedAt": "2026-05-07T12:34:57Z",
  "emittedAt": "2026-05-07T12:34:57Z",
  "result": "merged"
}
```

`state` is one of:

- `claimed` — `SET NX` succeeded but the enqueue is still in flight or has not started.
- `emitted` — the enqueue succeeded; this `auditID` is now durably represented in the
  canonical stream.

A `claimed` entry that is never promoted to `emitted` (because the writer crashed between
claim and commit, or the enqueue failed) is `DEL`'d by the writer's release path so the next
arrival can re-claim it. If the writer is killed before it can release, the entry expires
on its own TTL and the next arrival reclaims it then.

The two keys have different TTLs because they have different purposes:

- `audit:body:v1:<auditID>` — body data. TTL configured by `--audit-event-body-ttl`,
  default `5m`. Short because it holds payload bytes.
- `audit:decision:v1:<auditID>` — small dedupe marker. TTL configured by
  `--audit-event-decision-ttl`, default `1h`. Much longer because the key is tiny and the
  invariant it protects is the more important of the two.

The "at most one canonical stream event per `auditID`" goal is **TTL-bounded by
`--audit-event-decision-ttl`**. A retry or delayed sibling that arrives more than this
duration after the original would be treated as a fresh event and re-emitted. Operators who
need a longer dedupe window should raise this TTL; the cost is one tiny Redis key per
`auditID` until it expires.

The body key prevents losing the synthetic payload while waiting for the official event.
The decision key prevents any sibling arriving within its TTL from being enqueued again.

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

### Behavior on allowlist drift

When an additional-source event arrives for a group that is *not* on the parking allowlist,
the joiner must pick a behavior. The choice matters because emitting unexpected additional
events would re-introduce the duplicate-event bug for misconfigured proxied groups, while
unconditionally dropping them would break the additional-only deployment mode (which has no
official source to fall back on). The rule is mode- and source-specific:

- **Official source, any group:** always emit. The allowlist does not gate official events.
- **Additional source, allowlisted group:** enter the join flow per the configured mode
  (`wait-official` or `first`).
- **Additional source, not-allowlisted group, `--audit-additional-only=true`:** emit
  directly. The allowlist is informational in this mode because no official event is
  expected; the additional stream is canonical for this deployment.
- **Additional source, not-allowlisted group, default modes:** drop the event and increment
  `gitopsreverser_audit_join_body_unexpected_total{group}`. This protects the canonical
  stream from duplicates and surfaces the misconfiguration via the metric. The intended
  remediation is for the operator to add the group to
  `--audit-event-body-parking-api-groups`.

This rule is what the operations note about allowlist drift in
[Deployment modes](#deployment-modes) describes.

## Join modes

Configuration:

```text
--audit-event-join-mode=wait-official | first
--audit-event-body-ttl=5m
--audit-event-decision-ttl=1h
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

1. store the body contribution in `audit:body:v1:<auditID>`
2. do not claim a decision and do not enqueue
3. return `200` to the sender

When the official event arrives:

1. claim the decision: `SET NX audit:decision:v1:<auditID>` with `state=claimed`. If the
   key already existed, treat the event as a duplicate and drop it (this can happen on
   restart-driven replay).
2. read `audit:body:v1:<auditID>`
3. if present, merge the official event with the parked body contribution per the
   [Merge rule](#merge-rule)
4. enqueue the merged event (or the official event as-is if no parked body exists) to the
   canonical audit stream
5. on enqueue success: update the decision key to `state=emitted` (and `result=merged` or
   `result=as_is` accordingly). On enqueue failure: `DEL` the decision key so a later
   arrival can re-claim it; return 5xx so kube-apiserver retries the audit webhook.
6. on enqueue success: best-effort `DEL audit:body:v1:<auditID>` (expiry handles the rare
   case where this fails)

If no parked body exists when the official event arrives, the same flow runs without the
merge. This avoids building a delayed queue for the less common native-first race.

If `--audit-additional-only=true`, additional events do not park in this mode. They take
the same claim/enqueue/commit path described under `first` because no official event is
expected.

Trade-off: most aggregated API events wait for kube-apiserver's batch delivery before they
reach the consumer. Ordering is more correct because the canonical stream advances on the
official event.

### Mode: `first`

This mode optimizes for low latency.

When the first event for an `auditID` arrives:

1. claim the decision: `SET NX audit:decision:v1:<auditID>` with `state=claimed`. If the
   key already existed, drop the event as a duplicate (the rare same-instant tie or a
   replay).
2. enqueue immediately
3. on enqueue success: update the decision key to `state=emitted` (with `result=first`).
   On enqueue failure: `DEL` the decision key so the next arrival can re-claim it; return
   5xx.
4. do not keep a parked body

When a later event with the same `auditID` arrives, the `SET NX` claim fails and the event
is dropped as a duplicate.

If the synthetic full event arrives first, the canonical stream gets bodies quickly. If the
official shallow event arrives first, the canonical stream gets the official event and the
later full body is ignored.

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

`AuditHandler` still decodes `auditv1.EventList`. It passes the endpoint role into the joiner.
The joiner exposes a small two-phase contract so the handler controls the commit/release
lifecycle around the enqueue:

```go
type AuditEventJoiner interface {
    // Decide examines the event. For ActionEmit, Decide has already claimed the decision
    // key via SET NX. The caller MUST follow up with CommitDecision on enqueue success or
    // ReleaseDecision on enqueue failure. For ActionParked and ActionDropDuplicate, no
    // claim was made and no follow-up is required.
    Decide(ctx context.Context, source Source, event *auditv1.Event) (Decision, error)

    // CommitDecision promotes a claimed decision to state=emitted. Called after a
    // successful enqueue.
    CommitDecision(ctx context.Context, auditID string, result Result) error

    // ReleaseDecision deletes a claimed-but-not-emitted decision so a retry can re-claim
    // it. Called after a failed enqueue.
    ReleaseDecision(ctx context.Context, auditID string) error
}

type Action int

const (
    ActionParked        Action = iota // acknowledge, no stream write, no follow-up
    ActionEmit                        // enqueue Decision.Event, then Commit or Release
    ActionDropDuplicate               // acknowledge, no stream write, no follow-up
)

type Decision struct {
    Action Action
    // Event is set iff Action == ActionEmit. In wait-official mode where the joiner
    // merged a parked body onto the official event, Event is the merged result.
    Event *auditv1.Event
    // AuditID is set iff Action == ActionEmit. Pass it back to CommitDecision /
    // ReleaseDecision; the handler does not extract it from Event itself.
    AuditID string
    // Result describes what was emitted: as_is, merged, first, additional_only.
    // Used as the result label in metrics and stored on the committed decision key.
    Result Result
}
```

For each event:

1. validate and filter as today
2. call `AuditEventJoiner.Decide(ctx, source, event)`
3. dispatch on `Decision.Action`:
   - `ActionParked`: acknowledge request (200), no stream write, no follow-up
   - `ActionDropDuplicate`: acknowledge request (200), no stream write, no follow-up
   - `ActionEmit`:
     a. enqueue `Decision.Event`
     b. on enqueue success: call `CommitDecision(ctx, Decision.AuditID, Decision.Result)`,
        return 200
     c. on enqueue failure: call `ReleaseDecision(ctx, Decision.AuditID)`, return 5xx
        so the sender retries

The handler is the only thing that knows whether the enqueue succeeded, so it owns the
commit/release call. Decide alone never leaves a permanent decision key behind — only
CommitDecision does.

The queue remains the boundary to downstream processing. The consumer should not have to
reason about two events with the same `auditID` within the decision-key TTL window.

## CommitContext fit

The body parking mechanism is deliberately general. v1 has exactly one writer
(`apiservice-audit-proxy` via `/audit-webhook-additional`), but the design accommodates a
second writer landing in the immediate post-v1 step: gitops-reverser's own aggregated API
handler when [`CommitContext`](design-commit-context-api.md) ships.

The two writers, same store:

- For aggregated APIs behind `apiservice-audit-proxy`, the body source is the proxy's synthetic
  audit event posted to `/audit-webhook-additional`.
- For gitops-reverser's own aggregated `CommitContext` API, the body source is the handler
  itself. When the handler accepts a `CommitContext` request, it reads `Audit-ID` from the
  inbound request header and writes the request body to the same `audit:body:v1:<auditID>`
  key. The official kube-apiserver audit event for the `CommitContext` create call then
  triggers the join.

This means once body parking lands, `CommitContext`'s separate `commitcontext:stash:` namespace
and idempotency marker (described in [design-commit-context-api.md](design-commit-context-api.md))
collapse to "the handler calls `AuditBodyStore.Park`." The marker mechanism is no longer needed
because the parking machinery already deduplicates by `auditID` via the decision key.

To prepare for that without committing to it now, v1 should:

1. **Put a `source` field on the envelope from day one** even though only one source exists.
   Schema:

   ```json
   {
     "v": 1,
     "auditID": "0f6c...",
     "source": "apiservice-audit-proxy",
     "receivedAt": "2026-05-07T12:34:56Z",
     "event": { "kind": "Event", "apiVersion": "audit.k8s.io/v1" },
     "hasRequestObject": true,
     "hasResponseObject": true
   }
   ```

   Future values: `"gitops-reverser-aggregated-api"`, etc. Adding the field now costs nothing
   and avoids a schema-version bump later.

2. **Define collision behaviour explicitly.** In v1 only one writer exists, so collisions are
   theoretical. Once `CommitContext` ships, a request that goes through
   `apiservice-audit-proxy` *and* lands at gitops-reverser's own aggregated API would produce
   two writes for the same `auditID`. v1 keeps it simple: **last write wins** (plain `SET`,
   not `SET NX`, on `audit:body:v1:<auditID>`). The two writes are substantively similar —
   both contain the same request body — so the collision is benign. Source-priority merge
   semantics, if ever needed, are deferred (see Decisions and deferred work).

3. **Keep the `AuditBodyStore` interface source-agnostic.** It does not know whether the
   writer is the additional-endpoint handler or an in-process aggregated-API handler:

   ```go
   type AuditBodyStore interface {
       Park(ctx context.Context, auditID string, body AuditBodyEnvelope) error
       Get(ctx context.Context, auditID string) (AuditBodyEnvelope, bool, error)
       Delete(ctx context.Context, auditID string) error
   }
   ```

4. **Allow the parking allowlist to cover both writers.** `gitops-reverser.io` belongs on the
   allowlist when `CommitContext` is enabled; for v1 the default is whatever proxied groups
   the operator configures.

It is still important that `CommitContext` does not take effect from the parked body alone;
the official audit event must remain the trigger for downstream processing. The
audit-stream-as-source-of-truth principle holds across both writers.

## Metrics

Keep this smaller than the exploratory side-channel proposal. Use the existing
`gitopsreverser_` prefix to match
[`gitopsreverser_audit_events_received_total`](../../internal/telemetry/exporter.go) so
dashboards stay coherent:

- `gitopsreverser_audit_join_parked_total{source="body"}`
- `gitopsreverser_audit_join_emitted_total{source, mode, result="as_is|merged|first|additional_only"}`
- `gitopsreverser_audit_join_duplicate_dropped_total{mode}`
- `gitopsreverser_audit_join_body_miss_total{mode}` — official event arrived in `wait-official`
  mode for an allowlisted group with no parked body.
- `gitopsreverser_audit_join_body_orphan_total` — parked body expired without a matching
  official event. Implementation deferred (see [Decisions and deferred
  work](#decisions-and-deferred-work)).
- `gitopsreverser_audit_join_body_unexpected_total{group}` — additional-source event arrived
  for a group not listed in `--audit-event-body-parking-api-groups`. Persistent non-zero
  values indicate the parking allowlist has drifted away from the set of API groups actually
  routed through `apiservice-audit-proxy` and should be updated. Cheap to implement, valuable
  on Day 2.

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
  A duplicate can be emitted on replay. v1 handles this with `SET NX` on the decision key
  *before* enqueueing: only the writer that successfully claims the decision key proceeds to
  enqueue. On enqueue failure, that writer `DEL`s the decision key so a retry can claim it.
  A Redis Lua transition is a known v1.5 hardening if this proves insufficient under fault
  injection (see [Decisions and deferred work](#decisions-and-deferred-work)).

## Implementation outline

1. Change the official audit HTTP route to accept only `/audit-webhook`.
2. Remove cluster ID extraction and stream fields.
3. Add `/audit-webhook-additional` with the same `EventList` decoder and authentication posture.
4. Add `AuditEventJoiner` and a Redis-backed `AuditBodyStore`. The body envelope carries a
   `source` field from day one (see [CommitContext fit](#commitcontext-fit)).
5. Implement the decision transition as `SET NX` on `audit:decision:v1:<auditID>` *before*
   enqueueing. On enqueue failure, `DEL` the decision key so a retry can re-claim it.
6. Add configuration for join mode, body TTL, parking API groups, and additional-only mode.
7. Call the joiner from `AuditHandler` before `Queue.Enqueue`, passing the endpoint source.
8. Add table-driven unit tests for:
   - full first, wait-official parks
   - official after full emits merged
   - official first emits shallow in wait-official
   - full first emits immediately in first mode
   - duplicate after decision drops
   - additional-only mode emits additional events directly
   - full event outside the parking allowlist emits normally and increments
     `gitopsreverser_audit_join_body_unexpected_total{group}`
   - decision-key claimed but enqueue fails clears the decision key
   - Redis failures return the right handler status
9. Add e2e coverage for the aggregated API path proving one canonical stream entry per
   `auditID`.

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

## Decisions and deferred work

This design commits to the following choices for v1:

- **Default join mode is `wait-official`.** Operators that want lower latency can opt into
  `first`. Ordering correctness is the right default and resolving this in the design avoids
  re-litigating it in PRs.
- **Endpoint name is `/audit-webhook-additional`.** Direct, hard to misread, and compatible
  with the existing audit webhook mental model.
- **Parking allowlist is configured by API group only.** Group/resource granularity adds
  configuration weight without solving any concrete problem in v1. If a future aggregated API
  group contains both proxied and unproxied resources, revisit then.
- **Decision transition uses `SET NX` plus cleanup, not Redis Lua.** The Lua-script
  transition is a known v1.5 hardening if duplicates show up under fault injection; not
  needed for v1.
- **Body store collisions resolve by last-write-wins** (plain `SET` on `audit:body:v1:<auditID>`).
  Source-priority merge semantics are deferred (see [CommitContext fit](#commitcontext-fit)).

Deferred to future work:

- A first-class multi-cluster source identity model (replacing the removed `{clusterID}` path
  segment).
- Source-priority merge semantics for the body store, if multiple writers ever produce
  meaningfully different bodies for the same `auditID`.
- Keyspace-notification-based or sweeper-based implementation of
  `gitopsreverser_audit_join_body_orphan_total`.
- Group/resource granularity for the parking allowlist.
- Redis Lua transition for the decision key.
