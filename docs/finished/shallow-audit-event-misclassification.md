# Shallow audit events: definition, recognition, and the `/v1/pods` flood

> Status: root cause confirmed — the flood is `pods/exec`, not pod object creation.
> Date: 2026-05-22
> Cluster under investigation: **CozyStack** (`cluster="cozystack"`, `tenant="tenant-root"`).
> Related: [Audit Ingestion Pipeline](../architecture.md#audit-ingestion-pipeline),
> [apiservice-audit-proxy README](../../external-sources/apiservice-audit-proxy/README.md).

## Summary

After a recent upgrade, `audit-joiner` emits a high-volume flood of warnings:

```
WARNING: official shallow audit event timed out waiting for additional body;
the official event will be dropped. Install or repair apiservice-audit-proxy
so request/response bodies arrive within the wait budget.
  auditID=... gvr=/v1/pods verb=create wait=0.5
```

The volume is confirmed by metrics (see [Evidence](#evidence-from-the-cluster)):
thousands of `official` / `identity_shallow` / `pods` / `create` events.

The early debug Redis stream captured the exact incoming event. It is a
successful streaming `pods/exec` request, not a pod object create:

- `objectRef.resource: pods`
- `objectRef.subresource: exec`
- `verb: create`
- `responseStatus.code: 101`
- no `requestObject` or `responseObject`

That resolves the apparent policy contradiction. Kubernetes audits POST-style
exec requests with verb `create`, while GitOps Reverser's quality metric labels
resource but not subresource. The metric therefore looked like a flood of
bodyless pod creates. The reported audit policy excludes top-level `pods`; a
`pods/exec` audit-policy match is separate and falls through to its later
`RequestResponse` rule.

The fix for this flood is to reject all non-empty `objectRef.subresource` audit
events before the join pipeline. Current WatchRule planning rejects subresources
too; they are not the top-level object state this Git writer mirrors. The
previous audit ingress check rejected only `status` subresources, which was too
narrow.

This document also keeps the shallow-event analysis because the aggregated-API
body gap is real, but that gap did not produce this `/v1/pods` flood.

## Confirmed offending event

The captured event has this load-bearing shape:

```json
{
  "level": "RequestResponse",
  "stage": "ResponseComplete",
  "requestURI": "/api/v1/namespaces/cozy-kubeovn/pods/ovn-central-7955dc78d8-lvwh4/exec?...",
  "verb": "create",
  "objectRef": {
    "resource": "pods",
    "namespace": "cozy-kubeovn",
    "name": "ovn-central-7955dc78d8-lvwh4",
    "apiVersion": "v1",
    "subresource": "exec"
  },
  "responseStatus": {
    "code": 101
  }
}
```

`pods/exec` upgrades into a command stream. It cannot provide a resource object
for Git mirroring and no additional-body proxy can repair it. The joiner used to
see `verb=create` plus empty object bodies, classify it `identity_shallow`, then
wait for a body that can never arrive.

## Background: the two audit channels

GitOps Reverser ingests audit events from two endpoints:

| Endpoint | Sender (intended) | Role |
| --- | --- | --- |
| `/audit-webhook` | kube-apiserver | Canonical audit source — authority for identity, verb, status |
| `/audit-webhook-additional` | `apiservice-audit-proxy` | Supplementary **body** source |

The endpoint the payload arrives on *is* the source role — there is no in-band
marker ([audit_handler.go](../../internal/webhook/audit_handler.go#L534)). The
metric label `source="official"` therefore means only "POSTed to
`/audit-webhook`" — **not** "proven to come from kube-apiserver." That
distinction matters below.

`apiservice-audit-proxy` exists for exactly one reason. Per its
[README](../../external-sources/apiservice-audit-proxy/README.md):

> It exists to recover audit fields that kube-apiserver does not populate for
> **aggregated API requests**: `objectRef.name`, `requestObject`, and
> `responseObject`.

For a request to an **aggregated (`APIService`-backed) API**, kube-apiserver
proxies the call to a separate backend and never sees the request or response
body. Its native audit event for that request is *hollow*. The proxy sits in
front of the backend, observes the bodies, and posts an enriched event to
`/audit-webhook-additional` so the joiner can merge it.

For **built-in resources** — `pods`, `configmaps`, `deployments`, every core and
standard API — kube-apiserver handles the request itself and its native audit
event is **already complete**. The proxy is never in that path and contributes
nothing.

Note CozyStack specifically: the proxy README has a
[CozyStack case study](../../external-sources/apiservice-audit-proxy/README.md)
— the proxy redirects CozyStack's existing `APIService` objects (the
`cozystack-api` aggregated API) to itself. That covers CozyStack's aggregated
API groups; it does **not** put core `v1/pods` behind the proxy.

## What a shallow event actually is

A **shallow event** is the hollow native event kube-apiserver emits for an
**aggregated API request** — the case the additional-body channel was built to
repair. It is *not* simply "an event without a body."

The distinction is visible in a real captured payload — the proxy's checked-in
`audit-lane-a-kube-apiserver-hollow.json`, kube-apiserver's native event for a
`create` on the aggregated `flunders` API:

```jsonc
{
  "level": "RequestResponse",      // policy asked for bodies...
  "verb": "create",
  "objectRef": {
    "resource": "flunders",
    "namespace": "default",
    "apiGroup": "wardle.example.com",
    "apiVersion": "v1alpha1"
    // ...but NO "name"
  }
  // ...and NO "requestObject", NO "responseObject"
}
```

Even though the audit policy requested `RequestResponse`, kube-apiserver could
not fill in `objectRef.name`, `requestObject`, or `responseObject`, because the
real work happened in an aggregated backend it cannot see. **That** is a shallow
event — and note it still carries `level: RequestResponse`.

Contrast the event shapes the joiner must tell apart:

| Event shape | `level` | `objectRef.name` | `requestObject` / `responseObject` | What it is | Correct treatment |
| --- | --- | --- | --- | --- | --- |
| Complete | `Request` / `RequestResponse` | set | present | Built-in resource, or a merged event | Emit as-is |
| **Shallow** | **`RequestResponse`** | **empty** | **empty** | **Aggregated-API hollow event** — policy asked for bodies, apiserver could not supply them | **Wait for an additional body** |
| Bodyless by policy | `Metadata` / `None` | set | empty | Built-in resource the audit policy never captures bodies for | Reject at ingress — no Git write possible, nothing to wait for |

(Bodyless *deletes* are a separate, documented carve-out — `body_shallow_deletable` —
and emit on `objectRef` identity alone; they are not covered by this table.)

## The recognition rule

A shallow event is **recognisable from the event alone**. No API discovery, no
`APIResourceCatalog` lookup, no `APIService` enumeration, no new field anywhere
is needed:

> An audit event is a **genuine shallow event** when `objectRef.name`,
> `requestObject`, and `responseObject` are **all empty** — typically together
> with `level: RequestResponse` (the policy wanted bodies; the apiserver could
> not supply them).

`objectRef.name` is the load-bearing field. It is metadata, not body, so the
audit *level* does not strip it: a built-in resource keeps its name even at
`level: Metadata`. Only the aggregated-API path drops it. So the presence or
absence of `objectRef.name` on the offending event is exactly what tells the two
remaining hypotheses apart.

This matches the **documented** definition in the architecture quality table,
which already says `identity_shallow` means *"No body, **missing `objectRef`
identity**"*. The code drifted from it — see [The classifier
divergence](#the-classifier-divergence).

## Evidence from the cluster

### The audit policy reported as configured

```yaml
apiVersion: audit.k8s.io/v1
kind: Policy
omitStages:
  - "RequestReceived"
rules:
  - level: None
    resources:
    - group: ""
      resources: ["events", "endpoints", "nodes", "pods", "secrets", "bindings", "componentstatuses", "*/status"]
    - group: "authentication.k8s.io"
      resources: ["tokenreviews"]
    - group: "authorization.k8s.io"
      resources: ["subjectaccessreviews", "selfsubjectaccessreviews", "localsubjectaccessreviews", "selfsubjectrulesreviews"]
    - group: "coordination.k8s.io"
      resources: ["leases"]
    - group: "apps"
      resources: ["*/status"]
    - group: "networking.k8s.io"
      resources: ["*/status"]
  - level: None
    users: ["system:serviceaccount:kube-system:horizontal-pod-autoscaler"]
    verbs: ["update", "patch"]
    resources:
    - group: "apps"
      resources: ["*/scale"]
  - level: RequestResponse
    omitManagedFields: true
    verbs:
    - create
    - update
    - patch
    - delete
    - deletecollection
```

Kubernetes audit policy is **first-match-wins**. For a core top-level `pods`
object `create`:

- Rule 1 has a `GroupResources` entry `group: ""`, `resources: [... "pods" ...]`.
  A core pod create matches it. Rule 1 has no `users`/`verbs`/`namespaces`
  selectors, so those are wildcards — the match holds.
- First match wins → **`level: None`** → the event is **not generated at all.**

It never reaches rule 3 (the `RequestResponse` catch-all). So a kube-apiserver
running this exact policy emits **zero** top-level `pods` object audit events of
any level. The captured event is `pods/exec`, which is a separate audit-policy
resource match. The same top-level-resource distinction applies to `secrets`,
`events`, `endpoints`, `nodes`, `bindings`, and the explicit `*/status`
patterns.

### The metric

Query: `gitopsreverser_audit_event_quality_total{verb="create", quality!="complete"}`

```
gitopsreverser_audit_event_quality_total{
  quality="identity_shallow", resource="pods", version="v1",
  source="official", verb="create",
  cluster="cozystack", tenant="tenant-root",
  pod="gitops-reverser-56f948fdd8-ttrm6", ...
}
  min: 34   median: 2127   max: 4116
```

`addQualityMetric` is recorded immediately after classification, before any drop
([audit_handler.go](../../internal/webhook/audit_handler.go#L292)) — so this
counts every such event that actually arrived. Thousands of `official`,
`identity_shallow`, core `v1/pods`, `create` events are reaching the handler.

## The resolved contradiction

The policy says **zero top-level pod object events.** The metric says
**thousands of events labeled `resource="pods"` and `verb="create"`**. Both can
describe the same apiserver because the metric does not label
`objectRef.subresource`, while Kubernetes audit policy distinguishes `pods` from
`pods/exec`.

My earlier draft asserted the cause was a `Metadata`-level policy for pods. That
assertion was unverified and is now retracted. The later H1/H2 split below is
kept as shallow-event analysis, not as the root-cause fork for this flood.

## Earlier hypotheses

Before the debug stream captured `objectRef.subresource: exec`, the analysis
split on `objectRef.name`. That split is no longer needed for this flood, but it
records the remaining shallow-event question for top-level resources.

### H1 — genuine shallow events (the joiner is behaving correctly)

If the offending `pods/create` events have **empty `objectRef.name`** and empty
bodies, they are *genuine shallow events* by the [recognition
rule](#the-recognition-rule). Something is routing core pod creates through an
aggregated/proxied path where the emitting apiserver cannot see the body. In
that case:

- The joiner is **right** to wait for an additional body.
- The real gap is that no additional-body source is enriching these — the
  `apiservice-audit-proxy` deployment does not cover whatever path this is.
- The warning's advice ("install/repair apiservice-audit-proxy") is *correct*.
- This is **not** a classification bug.

### H2 — bodyless-but-identified events (a real misclassification)

If the offending events have a **populated `objectRef.name`** and empty bodies,
they are built-in pod events whose bodies were dropped by an audit policy
(a broad policy auditing pods at `Metadata`, contradicting the pasted one). In
that case:

- They are *not* shallow — they have full identity.
- The classifier still labels them `identity_shallow` and routes them into the
  wait path (see below), so the joiner waits for a body that can never come.
- This **is** a classification bug, and the [proposed fix](#proposed-fix)
  applies.

A debug-level (`V(1)`) log already records `hasRequestObject` /
`hasResponseObject` in `dropShallowOfficial`
([audit_joiner.go](../../internal/webhook/audit_joiner.go#L319)); it does **not**
record `objectRef.name` or `level`. Adding both to that log line is the cheapest
way to settle H1 vs H2 in production.

## The classifier divergence

Independent of which hypothesis holds, the classifier does not match its own
documented contract.

`classifyAuditEventQuality` in
[internal/webhook/audit_joiner.go](../../internal/webhook/audit_joiner.go#L681)
decides quality from body presence and never inspects `objectRef.name` on the
create/update/patch path:

```go
if hasAuditV1ObjectBody(event) {
    return AuditEventQualityComplete
}
if allowsBodylessAuditV1Delete(event) {     // delete-only carve-out
    return AuditEventQualityBodyShallowDeletable
}
return AuditEventQualityIdentityShallow      // <-- everything else bodyless
```

`hasAuditV1ObjectBody` only checks `requestObject` / `responseObject`. The only
place `objectRef.name` is consulted is `allowsBodylessAuditV1Delete`, gated on
`verb == "delete"`. So a bodyless **`create`** / `update` / `patch` with a
complete `objectRef` falls through to `identity_shallow` even though it has full
identity — exactly the H2 failure. The architecture doc says `identity_shallow`
requires *missing* `objectRef` identity; the code assigns it on body absence
alone.

### Contributing history

Commit `605964a` ("gate audit body joins by rule relevance") added a gate that
dropped shallow officials for resources no `WatchRule` could match — but it
gated on the **wrong axis** (WatchRule relevance, not shallow-vs-identified).
Commit `a688620` then removed that gate entirely to fix an unrelated e2e race.
Neither corrected the classification. A later "louder logging" change then made
the pre-existing behaviour impossible to ignore — which is why the flood appears
to be new even though the misroute (under H2) is older.

## Impact

- **Log flood** — one WARN per offending event, on every occurrence (the WARN is
  deliberately not gated by `sync.Once`).
- **Throughput cost** — each such event holds the official canonical gate for
  the full `--audit-event-body-wait` budget (`500ms`), so later official events
  queue behind a wait that is guaranteed to fail.
- **Data loss** — the event is dropped (`audit_shallow_dropped_total`). Under H1
  this means genuinely missing data; under H2 it means a built-in mutation is
  silently not mirrored.
- **Possibly misleading advice** — under H2 the "install apiservice-audit-proxy"
  WARN points operators at a fix that cannot help. Under H1 it is correct.

## Proposed fix

For the confirmed flood:

1. **Gate every non-empty `objectRef.subresource` at audit ingress.** This
   generalizes the previous `status`-only check to current product capability:
   subresources are not top-level resource state and are not supported by
   WatchRule planning.
2. **Keep a literal `pods/exec` regression event.** The regression must keep
   the observed `verb: create`, `subresource: exec`, `responseStatus.code: 101`,
   and empty object bodies so the joiner cannot start waiting on it again.

For top-level bodyless resources, two changes are still worth considering
because they harden diagnosis:

1. **Log `objectRef.name` and `level`** in `dropShallowOfficial` and in the
   `waitForBody` timeout WARN. This settles H1 vs H2 in-cluster without a packet
   capture, and is a one-line change.
2. **Revisit whether the classifier contract and docs should key on identity or
   body repairability.** The earlier draft proposed tightening
   `identity_shallow`
   should require `objectRef.name` to be empty **in addition to** both bodies
   being empty. That needs its own decision because URL-named aggregated
   subresource-free requests can still need body repair.

If **H2** is confirmed, additionally:

3. **Gate the official ingress on `level`.** An official event at `level: None`
   or `level: Metadata` cannot drive a Git write and has nothing to wait for; it
   should be rejected in `classifyAuditIngress`
   ([audit_handler.go](../../internal/webhook/audit_handler.go#L597)) before the
   joiner, with a rate-limited log pointing at the audit policy — not the proxy.

If **H1** is confirmed, the classifier is already correct; the work moves to the
deployment side — identifying the path these pod creates take and extending an
additional-body source to cover it.

None of this needs an `APIResourceCatalog` attribute, `APIService` enumeration,
or extra discovery calls.

## Deferred top-level shallow-event checks

The captured `pods/exec` event is enough to explain and gate this flood. If a
separate top-level resource still arrives bodyless after that gate, these checks
would settle whether it is an aggregated-API hollow event or an audit-policy
body gap:

1. **`objectRef.name` on the top-level event** — empty or populated. This field
   decides H1 vs H2 for a subresource-free bodyless event.
2. **The audit configuration the *relevant* kube-apiserver is actually running**
   on the CozyStack cluster — the live `--audit-policy-file` /
   `--audit-webhook-config-file` flags of the process emitting those events, not
   only the ConfigMap or Helm value the policy was pasted from.
3. **Which sender produced those events** — the CozyStack management control
   plane, a tenant (Kamaji) control plane, or `apiservice-audit-proxy`. The
   `auditID`, `sourceIPs`, and `userAgent` on a sample event will indicate this.
4. **Whether `apiservice-audit-proxy` is deployed** on this cluster and which
   endpoint it posts to (`/audit-webhook` vs `/audit-webhook-additional`).

## Open question

If H2 is confirmed: what should happen to a bodyless-but-identified
`create` / `update` / `patch` — a built-in resource fully identified but with
bodies stripped by policy? Drop it (treat a thin policy as misconfiguration,
with a rate-limited WARN), or emit it as an identity-only event (the
`body_shallow_deletable` carve-out already does this for `delete`)? Left open
deliberately — it is a design decision, not a classification bug.

## References

- [internal/webhook/audit_joiner.go](../../internal/webhook/audit_joiner.go) —
  `classifyAuditEventQuality`, `handleOfficial`, `waitForBody`,
  `dropShallowOfficial`, `allowsBodylessAuditV1Delete`, `hasAuditV1ObjectBody`.
- [internal/webhook/audit_handler.go](../../internal/webhook/audit_handler.go) —
  `classifyAuditIngress`, `auditSourceFromPath`, `addQualityMetric`.
- [docs/architecture.md](../architecture.md#audit-ingestion-pipeline) — the
  pipeline and the quality table the code should match.
- [apiservice-audit-proxy README](../../external-sources/apiservice-audit-proxy/README.md)
  — aggregated-API hollow vs complete events, and the CozyStack case study.
