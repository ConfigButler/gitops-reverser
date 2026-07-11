# Fact: Kubernetes resourceVersion semantics

> **reference** — durable background. Index: [`../INDEX.md`](../INDEX.md)

Reference: [Kubernetes API concepts — resource versions](https://kubernetes.io/docs/reference/using-api/api-concepts/#resource-versions)

This is a standing reference for how GitOps Reverser may and may not rely on
`metadata.resourceVersion` (RV). It captures the API contract and the
reliability improvements available on recent clusters. Design docs (for example
[HA and GitTarget distribution](../future/ha-gittarget-distribution-plan.md))
link here instead of restating the rules.

## The contract (always true)

- **RV is opaque.** Clients must treat it as an opaque string. Do not parse it,
  do arithmetic on it, or assume it is numeric.
- **RV is scoped to a single resource history.** It is only meaningful within one
  group/resource. Two RVs from the *same* group/resource (for example two
  `apps/deployments`) can be ordered when served by kube-apiserver; RVs from
  *different* resource types (for example `apps/deployments` vs
  `apps/replicasets`) **must not** be compared or collated, even within the same
  API group.
- **Extension/aggregated API servers.** Numeric ordering is only safe when both
  RV strings parse as decimal numbers. Otherwise fall back to equality-only
  comparison.
- **Gaps are normal.** RV is not a promise of contiguous per-type integers. A
  client cannot prove "RV 123 is missing, wait for it." Bounded reorder windows
  plus idempotent replay and snapshot/reconcile correction are the safe pattern,
  not waiting for a specific RV to appear.

## List / watch parameter semantics

- **LIST, no `resourceVersion`** → current collection (consistent; see below).
- **LIST with `resourceVersionMatch=NotOlderThan`** (the default when only
  `resourceVersion` is set) → data at least as fresh as the given RV.
- **LIST with `resourceVersionMatch=Exact`** → exactly that RV, or `410 Gone` if
  it has been compacted.
- **WATCH with `resourceVersion=R`** → streams changes after `R` for that
  resource. This is the resume point a client persists.
- **Watch bookmarks** (`BOOKMARK` events) → periodic RV progress markers on a
  long watch, so a client can keep its persisted RV recent without a real change
  occurring. Use them to reduce how often the `410` fallback fires.

## What changed: consistent reads from the watch cache

`ConsistentListFromCache` graduated to **GA in Kubernetes 1.34** and is stable in
**1.35+**. A consistent LIST now returns a trustworthy collection
`resourceVersion` **cheaply from the watch cache**, instead of forcing a quorum
read against etcd.

Practical effect: you can now lean on RV-based **watermarks** and **watch resume**
more than older client guidance allowed. A consistent LIST gives a precise
collection RV `R`; any later event with a higher comparable RV (same
group/resource) is newer than that snapshot, and anything at or below `R` is
already reflected. This makes the "snapshot at `R`, then apply events with
RV > `R`" pattern reliable and inexpensive.

This does **not** relax the contract above: RV is still opaque, still
per-group-resource, and still not collatable across resource types. The
improvement is about the *cost and consistency of obtaining a watermark / resume
point*, not about cross-resource ordering.

## `410 Gone` (compaction)

A persisted RV can age past the API server's compaction horizon. A request that
uses it (resume watch, or `resourceVersionMatch=Exact`) then returns `410 Gone`.
The required handling is to **relist from current state and reconcile**, not to
fail hard. Keeping the persisted RV recent via watch bookmarks reduces how often
this happens.

## How GitOps Reverser applies this

- Committed YAML never carries RV — it is stripped during sanitization, so no-op
  updates do not produce spurious diffs.
- RV (with `metadata.uid`) is the natural mutation-identity / dedup key inside the
  pipeline and queues; content hashes are a secondary idempotency guard.
- Ordering-sensitive Git writes stay serialized per branch write shard; RV is an
  ordering hint within a group/resource, never a cross-type global clock.
- Snapshot/reconcile is the correction path whenever audit ordering or delivery
  leaves the derived Git view uncertain.
