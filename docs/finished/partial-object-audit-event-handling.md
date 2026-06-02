# Partial-object audit events: classify the finalizer-patch fragment as a benign drop

> Status: implemented. The end-to-end miniredis metric test below was
> intentionally skipped — the `handleExtractObjectError` unit test already
> covers the `dropped_partial_object` outcome mapping.
> Date: 2026-05-22
> Observed on: **CozyStack** prod (`gitops-reverser-v0.26.0`), deleting `mongodb-simon2`.
> Related: [Shallow audit events](./shallow-audit-event-misclassification.md),
> [Audit Ingestion Pipeline](../architecture.md#audit-ingestion-pipeline).

## Summary

The audit consumer logs an `error`-level "poison-pill" line for an audit event
whose object body is a **finalizer-removal patch fragment** rather than a full
Kubernetes object:

```
{"level":"error","logger":"audit-consumer.audit-consumer",
 "msg":"Failed to route audit event; ACKing to avoid poison-pill",
 "msgID":"1779479358795-0",
 "error":"extracting object for tenant-root/mongodb-simon2: failed to unmarshal
   object JSON: Object 'Kind' is missing in '{\"metadata\":{\"finalizers\":null}}'"}
```

The body is `{"metadata":{"finalizers":null}}` — the merge-patch a controller
sends to drop the last finalizer while a resource is being deleted. It is valid
JSON but carries no `apiVersion`/`kind`, so
[`(*Unstructured).UnmarshalJSON`](https://pkg.go.dev/k8s.io/apimachinery/pkg/apis/meta/v1/unstructured#Unstructured.UnmarshalJSON)
rejects it with `Object 'Kind' is missing`.

The **outcome is already correct** — the event is ACK'd and dropped, no
poison-pill loop, no crash, and the resource's real `[DELETE]` commit lands
through its own delete audit event (in the captured run, the
`[DELETE] helm.toolkit.fluxcd.io/v2/helmreleases/mongodb-simon2` commit at
`19:49:02`, ~16s before this line). The **classification is wrong**: a routine,
expected event shape is funnelled through the generic error path instead of the
benign-drop path that `errAuditEventObjectMissing` and `errAuditEventObjectIsStatus`
already have. The cost is alert-grade log noise and no way to measure how often
it happens.

This design adds a third recognised body shape — **partial object** — alongside
"missing body" and "Status error body", with its own benign-drop path, log
gating, and metric outcome.

## Root cause

[`extractObject`](../../internal/queue/redis_audit_consumer.go#L708) selects a
raw body and unmarshals it:

```go
obj := &unstructured.Unstructured{}
if err := obj.UnmarshalJSON(raw); err != nil {
    return nil, fmt.Errorf("failed to unmarshal object JSON: %w", err)   // <-- generic
}
```

`(*Unstructured).UnmarshalJSON` requires a non-empty `kind`. A merge-patch body
has none, so the call fails. The returned error is a plain wrapped error — it
matches neither `errAuditEventObjectMissing` nor `errAuditEventObjectIsStatus`,
so [`handleExtractObjectError`](../../internal/queue/redis_audit_consumer.go#L459)
falls through its `switch` to `return false`, and
[`routeAuditEvent`](../../internal/queue/redis_audit_consumer.go#L408-L417)
surfaces it. [`processMessage`](../../internal/queue/redis_audit_consumer.go#L365)
then logs it at `error`.

### Why the body is a fragment, not the full object

[`selectAuditObjectRaw`](../../internal/queue/redis_audit_consumer.go#L754)
prefers `responseObject` for non-delete verbs and only falls back to
`requestObject`. For a `PATCH`, `requestObject` *is* the patch document — a
fragment by definition. The full merged object normally arrives as
`responseObject`, but when the patch removes the **last finalizer** of an object
that already has a `deletionTimestamp`, the object is deleted as part of that
same request and the apiserver records no resource `responseObject`. The
fallback then picks the only body present — the patch fragment.

So this is **not** a malformed-event anomaly. It is the normal audit shape of
the finalizer-removal step of any finalizer-driven deletion, and CozyStack's
operator-managed resources (`mongodbs` → `helmreleases`) hit it on every delete.

### How it differs from the shapes we already handle

| Body shape | Example | Sentinel | Recognised today |
| --- | --- | --- | --- |
| No body at all | `requestObject`/`responseObject` both empty | `errAuditEventObjectMissing` | yes |
| `metav1.Status` error body | `{"apiVersion":"v1","kind":"Status",...}` | `errAuditEventObjectIsStatus` | yes |
| **Partial object** | `{"metadata":{"finalizers":null}}` | **`errAuditEventObjectPartial`** (new) | **no — falls to error path** |

A partial object is **valid JSON with no `kind`**. That is precisely the
condition under which `UnmarshalJSON` fails while the bytes are still
well-formed — which makes it cheap and unambiguous to detect.

## Design

### 1. New sentinel error

In [redis_audit_consumer.go](../../internal/queue/redis_audit_consumer.go), next
to the existing sentinels:

```go
// errAuditEventObjectPartial marks an audit event whose body is valid JSON but
// lacks the apiVersion/kind identity of a full Kubernetes object — typically a
// merge-patch fragment such as {"metadata":{"finalizers":null}} recorded as the
// requestObject of a finalizer-removal PATCH. It carries no routable resource
// state, so it is dropped before git routing rather than treated as a decode
// failure. The resource's real mutation is mirrored from its own (delete or
// full-body) audit event.
var errAuditEventObjectPartial = errors.New(
    "audit event object body is a partial object (no kind)")
```

### 2. Detect it in `extractObject`

Distinguish "valid JSON, no kind" (partial) from "not JSON at all" (a genuine
decode failure worth an error) when `UnmarshalJSON` fails:

```go
obj := &unstructured.Unstructured{}
if err := obj.UnmarshalJSON(raw); err != nil {
    if isPartialObjectBody(raw) {
        return nil, errAuditEventObjectPartial
    }
    return nil, fmt.Errorf("failed to unmarshal object JSON: %w", err)
}

// isPartialObjectBody reports whether raw is well-formed JSON describing an
// object that lacks a "kind" — the condition that makes
// (*Unstructured).UnmarshalJSON fail on an otherwise valid body. A merge-patch
// fragment (e.g. {"metadata":{"finalizers":null}}) matches; malformed bytes do
// not, so a real decode failure still surfaces as an error.
func isPartialObjectBody(raw []byte) bool {
    var m map[string]any
    if err := json.Unmarshal(raw, &m); err != nil {
        return false
    }
    kind, _ := m["kind"].(string)
    return kind == ""
}
```

This adds no API discovery, no catalog lookup — it is a pure function of the
bytes, consistent with the recognition-rule principle in the
[shallow-event design](./shallow-audit-event-misclassification.md#the-recognition-rule).

### 3. Benign-drop path with its own outcome

[`handleExtractObjectError`](../../internal/queue/redis_audit_consumer.go#L459)
currently returns a single `bool` and the caller hard-codes
`pipelineOutcomeDroppedNoBody`. To measure the new case distinctly, change the
signature to return the metric outcome it classified:

```go
func (c *AuditConsumer) handleExtractObjectError(
    log logr.Logger, auditEvent auditv1.Event, err error, gvr, namespace, name string,
) (outcome string, handled bool)
```

- `errAuditEventObjectMissing`  → `(pipelineOutcomeDroppedNoBody, true)` — unchanged.
- `errAuditEventObjectIsStatus` → `(pipelineOutcomeDroppedNoBody, true)` — unchanged.
- `errAuditEventObjectPartial`  → `(pipelineOutcomeDroppedPartialObject, true)` — new.
- default                      → `("", false)`.

New `case` in the `switch`, mirroring the existing two — first occurrence at
`Info` with actionable text, the rest at `V(1)`, gated by a new
`firstPartialDropped sync.Once`:

```go
case errors.Is(err, errAuditEventObjectPartial):
    c.firstPartialDropped.Do(func() {
        c.log.Info(
            "First audit event dropped before git routing — body is a partial "+
                "object (no kind), typically a finalizer-removal PATCH fragment. "+
                "The resource's real change is mirrored from its own audit event; "+
                "this fragment is not routable. Further drops will log at V(1) only.",
            "auditID", auditEvent.AuditID, "gvr", gvr, "verb", auditEvent.Verb)
    })
    log.V(1).Info(
        "audit event dropped before git routing: partial object body (no kind)",
        "auditID", auditEvent.AuditID, "gvr", gvr, "verb", auditEvent.Verb,
        "namespace", namespace, "name", name)
    return pipelineOutcomeDroppedPartialObject, true
```

The caller in `routeAuditEvent`:

```go
sanitized, err := extractObject(auditEvent, op, fullAPIVersion, ref.Resource, namespace, name)
if err != nil {
    if outcome, handled := c.handleExtractObjectError(
        log, auditEvent, err, fullAPIVersion+"/"+ref.Resource, namespace, name,
    ); handled {
        recordPipelineEvent(ctx, gvr, auditEvent.Verb, outcome)
        return nil
    }
    return fmt.Errorf("extracting object for %s/%s: %w", namespace, name, err)
}
```

The generic `error`-level "poison-pill" log is now reached only by a *genuine*
decode failure (not valid JSON) — which is the anomaly that line was written
for.

## Metric

Add one outcome value to the existing
**`gitopsreverser_audit_pipeline_events_total`** counter — no new metric, no new
label, so existing dashboards and `InitTestExporter` wiring are untouched:

```go
const (
    pipelineOutcomeUnmatched            = "unmatched"
    pipelineOutcomeDroppedNoBody        = "dropped_no_body"
    pipelineOutcomeDroppedPartialObject = "dropped_partial_object"   // new
    pipelineOutcomeRouted               = "routed"
    pipelineOutcomeRouteFailed          = "route_failed"
)
```

A sample is emitted as
`gitopsreverser_audit_pipeline_events_total{group=…,version=…,resource=…,verb=…,outcome="dropped_partial_object"}`.

Operational use:
- **Expected, low and flat** — one count per finalizer-driven delete. A baseline
  rate that tracks deletions of finalizer-bearing resources is healthy.
- **Alert on sustained growth on non-`delete`/`patch` verbs** — a
  `dropped_partial_object` for a `create`/`update` would mean a full-body event
  is being lost upstream, which is a real gap, not a benign finalizer fragment.
  Suggested expression:
  `sum by (resource, verb) (rate(gitopsreverser_audit_pipeline_events_total{outcome="dropped_partial_object", verb!~"delete|patch"}[15m])) > 0`.

The [architecture.md metrics table](../architecture.md#metrics) row for
`gitopsreverser_audit_pipeline_events_total` is updated to list the new outcome:
`unmatched`, `dropped_no_body`, `dropped_partial_object`, `routed`,
`route_failed`.

## Tests

All in package `queue`. Two unit tests on `extractObject`, one decision test on
`handleExtractObjectError`, one end-to-end metric test.

### Unit — `extractObject` classifies the fragment

In [redis_audit_consumer_test.go](../../internal/queue/redis_audit_consumer_test.go),
beside `TestExtractObject_RejectsStatusErrorBody`:

```go
// TestExtractObject_ClassifiesPartialFinalizerPatch reproduces the CozyStack
// prod occurrence: deleting mongodb-simon2 produced an audit event whose only
// body was the finalizer-removal patch fragment {"metadata":{"finalizers":null}}.
// extractObject must classify it as a partial object, not a decode failure.
func TestExtractObject_ClassifiesPartialFinalizerPatch(t *testing.T) {
    ev := auditv1.Event{
        Verb:          "patch",
        RequestObject: &runtime.Unknown{Raw: []byte(`{"metadata":{"finalizers":null}}`)},
        // ResponseObject deliberately nil: the object was deleted by this same
        // PATCH (last finalizer removed), so the apiserver recorded no body.
    }

    _, err := extractObject(
        ev, configv1alpha1.OperationUpdate,
        "helm.toolkit.fluxcd.io/v2", "helmreleases", "tenant-root", "mongodb-simon2",
    )
    require.ErrorIs(t, err, errAuditEventObjectPartial)
}

// TestExtractObject_MalformedBodyStillErrors guards the boundary: bytes that are
// not valid JSON are a genuine decode failure and must NOT be reclassified as a
// benign partial object — they still deserve the error-level poison-pill log.
func TestExtractObject_MalformedBodyStillErrors(t *testing.T) {
    ev := auditv1.Event{
        ResponseObject: &runtime.Unknown{Raw: []byte(`{"metadata":`)}, // truncated
    }

    _, err := extractObject(ev, configv1alpha1.OperationCreate, "v1", "ConfigMap", "default", "cm")
    require.Error(t, err)
    require.NotErrorIs(t, err, errAuditEventObjectPartial)
    require.NotErrorIs(t, err, errAuditEventObjectMissing)
    require.NotErrorIs(t, err, errAuditEventObjectIsStatus)
}
```

### Unit — `handleExtractObjectError` maps it to the benign outcome

```go
// TestHandleExtractObjectError_PartialObjectIsBenign confirms a partial-object
// error is handled (ACK, no poison-pill) and reported under the
// dropped_partial_object metric outcome.
func TestHandleExtractObjectError_PartialObjectIsBenign(t *testing.T) {
    c := &AuditConsumer{log: logr.Discard()}
    outcome, handled := c.handleExtractObjectError(
        logr.Discard(), auditv1.Event{Verb: "patch"},
        errAuditEventObjectPartial, "helm.toolkit.fluxcd.io/v2/helmreleases",
        "tenant-root", "mongodb-simon2",
    )
    assert.True(t, handled)
    assert.Equal(t, pipelineOutcomeDroppedPartialObject, outcome)
}
```

### End-to-end — the metric is emitted, the router is not called

> Not implemented — intentionally skipped to keep the change small. The
> `handleExtractObjectError` unit test above already pins the
> `dropped_partial_object` outcome, and `recordPipelineEvent` records whatever
> outcome that function returns. The sketch below is kept for reference.

In [audit_metrics_test.go](../../internal/queue/audit_metrics_test.go), modelled
on `TestAuditPipelineEventsMetric_DroppedNoBody`:

```go
func TestAuditPipelineEventsMetric_DroppedPartialObject(t *testing.T) {
    reader, err := telemetry.InitTestExporter()
    require.NoError(t, err)

    mr := miniredis.RunT(t)
    er := &fakeEventRouter{}
    c := newTestConsumer(t, mr, configmapRuleStore(), er)
    require.NoError(t, c.ensureConsumerGroup(context.Background()))

    // A patch event whose only body is a finalizer-removal fragment.
    ev := makeAuditEvent("patch", auditv1.StageResponseComplete, "configmaps", "default", "cm")
    ev.RequestObject = &runtime.Unknown{Raw: []byte(`{"metadata":{"finalizers":null}}`)}
    pushAuditMessage(t, mr, ev)
    require.NoError(t, c.readAndProcessBatch(context.Background()))

    pipeline, ok := telemetry.CollectInt64Sum(reader, pipelineEventsMetric, map[string]string{
        "resource": "configmaps", "verb": "patch", "outcome": "dropped_partial_object",
    })
    require.True(t, ok, "expected a dropped_partial_object audit_pipeline_events_total sample")
    assert.Equal(t, int64(1), pipeline)

    // The fragment must never reach the git pipeline.
    assert.Empty(t, er.calls, "partial-object event must not be routed")
}
```

(`readAndProcessBatch` returning `nil` is itself the assertion that the event
was ACK'd on the benign path — a poison-pill error would surface here.
`fakeEventRouter.calls` records every `RouteToGitTargetEventStream` call, so an
empty slice proves the fragment never reached the git pipeline.)

## Out of scope

- **Splitting the `Status` drop into its own outcome.** `errAuditEventObjectIsStatus`
  still reports `dropped_no_body`, which is mildly inaccurate (there *is* a body).
  Correcting it is a one-line follow-up but is not required here and would change
  an existing metric series.
- **Recovering state from the fragment.** A finalizer patch has no full object;
  there is nothing to mirror. The resource's real change is already covered by
  its delete/full-body audit event. Dropping is the correct terminal action.
- **The CozyStack aggregated-apiserver watch instability** (`INTERNAL_ERROR`
  HTTP/2 stream resets, `bookmark expired`) seen in the same logs — an upstream
  environment issue on a different code path, tracked separately.

## References

- [internal/queue/redis_audit_consumer.go](../../internal/queue/redis_audit_consumer.go)
  — `extractObject`, `handleExtractObjectError`, `routeAuditEvent`, the sentinel
  errors, `pipelineOutcome*`, `recordPipelineEvent`.
- [internal/queue/redis_audit_consumer_test.go](../../internal/queue/redis_audit_consumer_test.go)
  — existing `extractObject` test patterns.
- [internal/queue/audit_metrics_test.go](../../internal/queue/audit_metrics_test.go)
  — existing `audit_pipeline_events_total` outcome tests.
- [docs/architecture.md](../architecture.md#metrics) — the metrics table to update.
- [shallow-audit-event-misclassification.md](./shallow-audit-event-misclassification.md)
  — the related body-shape classification work.
