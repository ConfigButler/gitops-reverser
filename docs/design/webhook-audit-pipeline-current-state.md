# Webhook Audit Pipeline: Current State, Remaining Hackiness, and Secret Handling

## Purpose

This document captures the current state of the webhook-audit ingestion pipeline after the recent fixes, with a focus on:

1. what is now working
2. how the effective event flow works
3. where the implementation is still uneven or fragile
4. how the Secret exception works today
5. what better alternatives exist for Secret handling

This document reflects the code and test state validated on 2026-04-08.

## Executive Summary

The system is now green again:

- `make lint` passes
- `make test` passes
- `make test-e2e` passes
- `make test-e2e-quickstart-manifest` passes
- `make test-e2e-quickstart-helm` passes

The webhook-audit migration is now functionally working, but the architecture is still a hybrid:

- audit is the authority for most live mutating events when audit Redis is enabled
- watch is still used for snapshot/reconcile behavior
- Secrets are a deliberate exception and still use the watch path for live mutation handling

That hybrid model is good enough to be correct and green, but it is still not as clean as a fully unified ingestion design.

## What Was Broken

There were three distinct failure classes involved.

### 1. Consumer-group loss in Redis / Valkey

The audit consumer could fail with `NOGROUP` when the stream or consumer group disappeared.

This was real, but it was not the full explanation.

### 2. Username attribution loss

Watch-path events had empty `UserInfo`.

Audit-path events had the real user, for example `jane@acme.com`.

Because both paths could emit the same logical mutation, the watch event could win the race and produce the Git commit first. Once the file content was already written, the later audit event often became a no-op, so the correct author never made it into Git history.

### 3. CRD deletion path regression

The remaining red e2e after the first fixes was:

- `Manager should delete Git file when IceCreamOrder CRD is deleted via ClusterWatchRule`

This turned out to be two separate problems stacked together:

1. cluster-scoped watch events were still live-routing in audit mode, which recreated the CRD file after deletion
2. rule-change snapshot reconciliation only refreshed cluster state, not repo state, so it could diff against stale repo contents and incorrectly conclude there was nothing to delete

## Final Fixes That Landed

## Audit authority for live mutations

When `--audit-redis-enabled` is true:

- live watch-path mutation routing is suppressed for all resources except `secrets`
- audit consumer is the live mutation source of truth

Implemented in:

- [cmd/main.go](../../cmd/main.go)
- [internal/watch/manager.go](../../internal/watch/manager.go)
- [internal/watch/informers.go](../../internal/watch/informers.go)

### Why this matters

This removes the duplicate-source race for almost all resources:

- no more watch-vs-audit race for ConfigMaps
- no more watch-vs-audit race for CRDs
- no more empty-author watch event suppressing later audit attribution

## Redis consumer self-healing

The audit consumer now recreates the consumer group when Redis returns `NOGROUP` from:

- `XREADGROUP`
- `XAUTOCLAIM`

Implemented in:

- [internal/queue/redis_audit_consumer.go](../../internal/queue/redis_audit_consumer.go)
- [internal/queue/redis_audit_consumer_test.go](../../internal/queue/redis_audit_consumer_test.go)

## Correct custom-resource API group mapping

Custom-resource audit events were previously misread from `objectRef.apiVersion` alone.

For CRs, audit events commonly carry:

- `objectRef.apiGroup`
- `objectRef.apiVersion`

separately.

That meant `shop.example.com/v1` resources could be interpreted as core `/v1/...` resources.

The consumer now combines:

- `ObjectRef.APIGroup`
- `ObjectRef.APIVersion`

correctly.

Implemented in:

- [internal/queue/redis_audit_consumer.go](../../internal/queue/redis_audit_consumer.go)
- [internal/queue/redis_audit_queue.go](../../internal/queue/redis_audit_queue.go)
- [internal/queue/redis_audit_consumer_test.go](../../internal/queue/redis_audit_consumer_test.go)
- [internal/queue/redis_audit_queue_test.go](../../internal/queue/redis_audit_queue_test.go)

## Snapshot reconcile now refreshes repo state too

During rule-change reconciliation, the watch manager originally emitted only:

- `RequestClusterState`

That was not enough. `FolderReconciler` compares:

- current cluster state
- current repo state

If repo state is stale, deletes can be missed.

The watch manager now emits both:

- `RequestRepoState`
- `RequestClusterState`

for affected GitTargets.

Implemented in:

- [internal/watch/manager.go](../../internal/watch/manager.go)

This is what fixed the last CRD deletion failure.

## Current Effective Pipeline

The easiest way to understand the system now is by resource class.

### Normal resources, audit Redis enabled

Examples:

- ConfigMaps
- CRDs
- custom resources like `IceCreamOrder`

Effective live path:

1. kube-apiserver emits audit event
2. audit webhook receives it
3. producer stores raw audit payload in Redis / Valkey
4. audit consumer reads stream entry
5. audit consumer matches WatchRule / ClusterWatchRule
6. audit consumer routes a `git.Event`
7. BranchWorker writes the file and commits with real user attribution

Watch informers still exist, but they do not route live mutating events for these resources when audit mode is enabled.

### Secrets, audit Redis enabled

Effective live path:

1. kube-apiserver audit policy drops Secret audit events
2. no Secret event enters Redis / Valkey
3. watch informer sees Secret create/update/delete
4. watch path routes the live mutation
5. Git write path applies SOPS encryption if configured
6. encrypted Secret content is committed to Git

This is the one intentional live-path exception.

### Snapshot / reconcile path

Regardless of resource type, watch manager still performs cluster-state snapshotting for GitTargets during:

- initial sync
- rule changes
- discovery changes
- retry loops for unavailable GVRs

That snapshot flow is still watch-manager driven, not audit-consumer driven.

## Why the Secret Exception Exists

The short version is:

- Git encryption is not the risky point
- audit ingestion before Git is the risky point

## What the audit policy currently does

The audit policy in [test/e2e/cluster/audit/policy.yaml](../../test/e2e/cluster/audit/policy.yaml) explicitly excludes Secrets at `level: None`:

- group `""`
- resource `"secrets"`

That means kube-apiserver does not send Secret request/response bodies to the audit webhook at all.

## Why this matters

If Secrets were removed from that exclusion, then for mutating requests the policy would fall through to:

- `level: RequestResponse`

At that point Secret content would be present in audit events before we ever reach Git.

## Where that sensitive content would go

Today, the audit path persists raw event payloads before sanitization:

- audit webhook receives Secret request/response payload
- producer stores the event JSON in Redis stream field `payload_json`
- consumer later parses it

The important detail is that sanitization does not save us here, because persistence happens before sanitization.

Also, the sanitizer currently preserves Secret payload fields:

- `data`
- `binaryData`

See:

- [internal/sanitize/sanitize.go](../../internal/sanitize/sanitize.go)

So even if we later sanitize for Git output, we still would already have written raw Secret data into the audit pipeline.

## Exact behavior of the Secret exception today

The current Secret behavior is the combined result of three choices.

### 1. Audit policy excludes Secret audit events

In [test/e2e/cluster/audit/policy.yaml](../../test/e2e/cluster/audit/policy.yaml):

- Secrets are filtered out
- no Secret audit payload enters the webhook audit flow

### 2. Watch-path live suppression explicitly exempts Secrets

In [internal/watch/informers.go](../../internal/watch/informers.go), when `AuditLiveEventsEnabled` is true:

- live watch routing is skipped for almost everything
- but `g.Resource == "secrets"` is allowed through

So Secrets still emit live `git.Event`s from the informer path.

### 3. Git write path encrypts committed Secret material

Once the watch event reaches the Git layer:

- Secret manifests are written through the existing SOPS-aware content path
- the committed file is encrypted if target encryption is configured

So the current design is:

- do not ingest live Secret mutations through audit
- do ingest live Secret mutations through watch
- do encrypt them before Git persistence

That is why the Secret exception is both:

- a security workaround
- an architectural inconsistency

## Why This Is Still Hacky

The system is correct now, but there are still several hacky edges.

## 1. We still have two authorities depending on resource class

Today the answer to “where does a live mutation come from?” is:

- audit for most resources
- watch for Secrets

That is workable, but it means behavior differs by resource type in a way that is not obvious from the outside.

This increases debugging cost because event provenance is conditional, not uniform.

## 2. The Secret exception is encoded in more than one place

The Secret behavior is not governed by one clean abstraction.

It depends on:

- audit policy excluding Secrets
- watch routing code exempting Secrets
- Git encryption being configured and working

That means the design invariant is spread across policy, runtime logic, and write semantics.

If someone later changes only one layer, the system can become unsafe again.

## 3. Audit payload persistence is too raw

The audit producer stores raw `payload_json`.

That is convenient for fidelity and debugging, but it means the security boundary is weak:

- if the audit policy admits sensitive content
- Redis becomes a persistence layer for that content

This is the core reason the Secret exception exists.

## 4. Snapshot/reconcile is still broad and somewhat blunt

Rule changes can trigger snapshot reconciles across affected GitTargets, which is correct, but still noisy:

- all affected targets are collected
- repo and cluster state are re-read
- large atomic batches are emitted

This is robust, but not especially surgical.

It is more “refresh the world for this target” than “precisely reconcile only the minimal impacted subset”.

## 5. Unavailable-GVR handling still affects nearby flows

When a CRD disappears, the watch manager continues tracking unavailable GVRs and retrying them.

That behavior is useful, but it still creates side-channel complexity:

- discovery filtering changes
- retry reconciliation fires later
- unrelated GitTargets can get swept into broader snapshot churn

It is better than before, but still not elegant.

## 6. Snapshot logic and live logic are still conceptually separate systems

The system now has:

- audit-based live mutation ingestion
- watch-based cluster-state reconciliation

Those two systems are coordinated, but they are not the same model.

That means bugs tend to appear at the seams:

- deduplication
- stale state
- source precedence
- rule-change timing

## Is There a Better Alternative for Secrets?

Yes. There are better designs than the current exception.

The current exception is the safest short-term option, but not the cleanest long-term one.

## Option A: Redact Secrets before Redis persistence

This is the most direct improvement.

Design:

1. audit webhook receives a Secret event
2. before enqueueing to Redis, producer detects Secret resource
3. producer removes or replaces sensitive fields:
   - `data`
   - `stringData`
   - `binaryData`
4. only the redacted payload is persisted

### Pros

- keeps audit as the live authority even for Secrets
- removes the watch-path exception
- preserves most of the current audit architecture

### Cons

- must be implemented very carefully
- redaction must happen before any persistence or logging
- if redaction misses an embedded Secret-like payload, we still leak

### Assessment

This is the most realistic near-term improvement.

## Option B: Store only normalized routing fields, not raw audit payloads

Design:

- producer extracts only the minimum fields needed for routing:
  - operation
  - group/version/resource
  - namespace/name
  - user identity
  - sanitized or redacted object form
- raw `payload_json` is not stored

### Pros

- much stronger security boundary
- reduces accidental persistence of sensitive webhook data
- simplifies what the consumer needs to trust

### Cons

- bigger design change
- loses some debugging fidelity from raw audit events
- requires stronger schema/versioning discipline for the queue format

### Assessment

Architecturally cleaner than the current model. This is probably the best long-term design if the project wants audit to be the durable source of truth.

## Option C: Allow Secret audit events but use metadata-only audit level

Design:

- keep Secrets in audit
- do not capture request/response bodies for Secrets
- only use metadata for routing and attribution

### Pros

- avoids raw Secret payload persistence
- still lets audit carry user attribution and intent metadata

### Cons

- the consumer would not have the Secret body to commit
- so audit alone could not write the Secret manifest
- you would still need a second source for Secret contents

### Assessment

Good for observability, not sufficient for Git writing by itself.

It reduces the security problem but does not remove the need for a separate Secret-content path.

## Option D: Dedicated Secret ingestion path

Design:

- explicitly treat Secrets as a first-class separate ingestion mode
- make that separation visible in code and docs
- use a specialized Secret event path that:
  - reads live state from the API
  - encrypts before persistence
  - never stores plaintext in Redis

### Pros

- more honest than a hidden exception
- can be made secure and explicit
- allows Secret-specific guarantees

### Cons

- still means multiple ingestion architectures
- more code and more mental model complexity

### Assessment

Better than the current implicit exception if the project decides Secrets truly require a distinct handling model.

## Recommended Direction

My recommendation is:

### Short term

Keep the current Secret exception.

Reason:

- it is now validated
- it is safer than simply letting Secret audit payloads flow into Redis
- it keeps the system green

### Medium term

Implement producer-side pre-persistence redaction for Secrets and other sensitive resource classes.

Best target:

- redact before writing to Redis
- then remove the watch live-routing exception for Secrets

This would let audit become the single live mutation authority for all resources, which is much cleaner.

### Long term

Consider replacing raw `payload_json` persistence with a normalized, purpose-built audit queue envelope.

That is the cleanest architectural end state:

- smaller security surface
- fewer source races
- clearer contracts between producer and consumer

## Current State Summary

The system is now in a good operational state, but not yet in a perfect architectural state.

### Clean and solid now

- audit consumer self-heals `NOGROUP`
- custom-resource audit mapping is correct
- username attribution is preserved through audit path
- CRD delete flow works again
- full validation suites are green

### Still a bit hacky

- Secrets are a special-case live-path exception
- audit persistence is still too raw
- watch and audit are still split-brain by design, even if much less dangerously than before

## Practical Bottom Line

If the question is “are we in a good place now?”:

- yes, functionally

If the question is “is this the clean final design?”:

- not yet

The main remaining architectural debt is Secret handling.

Today’s Secret exception is justified, but it is still a workaround for the fact that the audit pipeline persists raw webhook payloads before redaction.
