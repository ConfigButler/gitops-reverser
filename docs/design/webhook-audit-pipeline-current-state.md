# Webhook Audit Pipeline: Current State, Remaining Hackiness, and Secret Handling

> **This is the authoritative current-state document for the audit ingestion pipeline.**
> For implementation history and the migration journey, see `docs/past/`.

## Purpose

This document captures the current state of the webhook-audit ingestion pipeline after the recent fixes, with a focus on:

1. what is now working
2. how the effective event flow works
3. where the implementation is still uneven or fragile
4. how Secret handling works today
5. what better alternatives exist for Secret handling

This document reflects the code and test state validated on 2026-04-09. All e2e suites pass.

## Executive Summary

The system is now green again:

- `task lint` passes
- `task test` passes
- `task test-e2e` passes
- `task test-e2e-quickstart-manifest` passes
- `task test-e2e-quickstart-helm` passes

The webhook-audit migration is now functionally working. The architecture is operationally uniform:

- audit is the single authority for all live mutating events
- watch is still used for snapshot/reconcile behavior only
- Secrets flow through the audit path the same as any other resource

The accepted tradeoff: raw Secret payloads are persisted in Valkey as `payload_json` before Git-side SOPS encryption applies. This is a deliberate choice — uniformity over a separate Secret ingestion path. The audit queue must be secured with auth and TLS as a result (see `final-redis-required-plan.md`).

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

Now that audit queueing is required:

- live watch-path mutation routing is suppressed for all resources
- audit consumer is the live mutation source of truth

Implemented in:

- [cmd/main.go](../../cmd/main.go)
- [internal/watch/manager.go](../../internal/watch/manager.go)
- [internal/watch/informers.go](../../internal/watch/informers.go)

### Why this matters

This removes the duplicate-source race for live-mutating resources:

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

1. kube-apiserver emits Secret audit event
2. audit webhook receives it
3. producer stores raw Secret audit payload in Redis / Valkey
4. audit consumer reads the stream entry
5. audit consumer routes the `git.Event`
6. Git write path applies SOPS encryption if configured
7. encrypted Secret content is committed to Git

There is no longer a Secret-specific live-path exception.

### Snapshot / reconcile path

Regardless of resource type, watch manager still performs cluster-state snapshotting for GitTargets during:

- initial sync
- rule changes
- discovery changes
- retry loops for unavailable GVRs

That snapshot flow is still watch-manager driven, not audit-consumer driven.

## Current Secret Handling

Secrets are no longer a special case. The watch-path exception for Secrets was removed: when
`AuditLiveEventsEnabled` is true, all live watch events are suppressed for all resources, including
Secrets. The audit policy also no longer excludes Secrets.

The current flow for a live Secret mutation:

1. kube-apiserver emits a Secret audit event (with `data`/`binaryData` in the payload)
2. audit webhook receives it
3. producer stores the raw event JSON in Valkey stream field `payload_json`
4. audit consumer reads it, extracts the object via `extractObject`
5. the object passes through `sanitize.Sanitize` (which currently preserves `data`/`binaryData`)
6. Git write path applies SOPS encryption if configured before committing

**The accepted tradeoff:** raw Secret material is persisted in Valkey before SOPS encryption applies.
This is why the audit queue must be secured. An unauthenticated Valkey with Secrets in the stream
is a plaintext Secret exfiltration point.

**Why this is still the correct choice over the old exception:** the exception created split-brain
provenance (audit for everything, watch for Secrets), which caused author-attribution races and
added hidden complexity. Uniformity is operationally safer even if it means a stricter Valkey
security requirement.

**Future improvement:** producer-side redaction of sensitive fields before writing to `payload_json`.
This would shrink the security blast radius without reintroducing a separate ingestion path.

## Where the Architecture Is Still Uneven

The system is correct and all suites pass, but there are still rough edges worth tracking.

## 1. Audit payload persistence is too raw

The audit producer stores raw `payload_json`.

That is convenient for fidelity and debugging, but it means the security boundary is weak:

- if the audit policy admits sensitive content
- Redis becomes a persistence layer for that content

This is the core reason the Secret exception exists.

## 2. Snapshot/reconcile is still broad and somewhat blunt

Rule changes can trigger snapshot reconciles across affected GitTargets, which is correct, but still noisy:

- all affected targets are collected
- repo and cluster state are re-read
- large atomic batches are emitted

This is robust, but not especially surgical.

It is more “refresh the world for this target” than “precisely reconcile only the minimal impacted subset”.

## 3. Unavailable-GVR handling still affects nearby flows

When a CRD disappears, the watch manager continues tracking unavailable GVRs and retrying them.

That behavior is useful, but it still creates side-channel complexity:

- discovery filtering changes
- retry reconciliation fires later
- unrelated GitTargets can get swept into broader snapshot churn

It is better than before, but still not elegant.

## 4. Snapshot logic and live logic are still conceptually separate systems

The system now has:

- audit-based live mutation ingestion
- watch-based cluster-state reconciliation

Those two systems are coordinated, but they are not the same model.

That means bugs tend to appear at the seams:

- deduplication
- stale state
- source precedence
- rule-change timing

## Future Improvement: Reduce Payload Sensitivity in the Queue

The cleanest long-term fix is to stop storing full raw audit payloads and instead write only the
fields the consumer actually needs for routing and Git writing. This would dramatically reduce the
security blast radius of a compromised Valkey instance.

Near-term practical step: redact `data`, `stringData`, and `binaryData` in the producer before
writing `payload_json`, similar to how the sanitizer works for Git output. This keeps the current
architecture but removes the most sensitive content from the queue.

## Current State Summary

The system is in a good operational state. All validation suites pass.

### Clean and solid

- audit is the single authority for all live mutations (including Secrets)
- audit consumer self-heals `NOGROUP`
- custom-resource audit API group mapping is correct
- username attribution is preserved through the audit path
- CRD delete flow works
- snapshot/reconcile still works correctly via the watch path

### Still uneven

- audit persistence stores raw `payload_json`, including raw Secret material
- snapshot/reconcile is broad rather than surgical
- watch and audit remain two conceptually separate systems coordinated at the seams

## Practical Bottom Line

Functionally solid. The main remaining architectural debt is raw payload persistence in Valkey — and
the mitigation for that is requiring auth on the queue, which is now documented and planned in
`final-redis-required-plan.md`.

The main remaining architectural debt is Secret handling.

Today’s Secret exception is justified, but it is still a workaround for the fact that the audit pipeline persists raw webhook payloads before redaction.
