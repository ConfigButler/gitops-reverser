# Running GitOps Reverser outside a single cluster

> Status: active workstream
> Captured: 2026-07-09
> Related: [../../architecture.md](../../architecture.md),
> [../../security-model.md](../../security-model.md),
> [../../configuration.md](../../configuration.md)

GitOps Reverser was designed for the shape it is most often installed in: one
operator, in one cluster, watching that cluster, writing to Git. Every
assumption that shape allows was taken — a single `rest.Config`, a single
Redis keyspace, a single API surface, one immutable destination per
`GitTarget`.

Operators who run the reverser in a **multi-tenant or multi-cluster** setting
hit the edges of those assumptions, and so does anyone who pairs the reverser
with a GitOps **forward leg** (Flux, Argo CD) on the same branch. This
workstream collects seven changes that move those edges. They are independent;
each is useful on its own.

## The seven

| # | Change | Who it affects | Design |
|---|--------|----------------|--------|
| 1 | **Separate the config plane from the watched cluster** — a `GitTarget` may name the cluster it mirrors | anyone watching a *remote* cluster; unlocks multi-cluster and multi-tenant installs | [config-plane-split.md](config-plane-split.md) |
| 2 | **Ignore writes by an identity or field manager** | **every** install that pairs a reverser with a GitOps forward leg | [identity-write-exclusion.md](identity-write-exclusion.md) |
| 3 | **A Redis key prefix** | anyone running more than one reverser against one Redis/Valkey | [../../configuration.md](../../configuration.md) |
| 4 | **A public `pkg/manifestanalyzer`** | anyone building tooling on the acceptance rules | [../../../pkg/manifestanalyzer/doc.go](../../../pkg/manifestanalyzer/doc.go) |
| 5 | **A `CommitRequest` that can assert an author** | anyone on a control plane whose apiserver audit flags they cannot set | [asserted-commit-author.md](asserted-commit-author.md) |
| 6 | **A movable `GitTarget` destination** | anyone who ever repoints a target | [gittarget-retarget.md](gittarget-retarget.md) |
| 7 | **Degrade gracefully without `apiregistration.k8s.io`** | anyone on an API server that does not serve APIService aggregation | this document, below |

## Why #2 is first

A reverser paired with a Flux (or Argo CD) forward leg on the same branch
**commits its own forward leg's applies**. The loop is:

1. A human edits a ConfigMap. The reverser commits it.
2. The forward leg sees the new commit and applies it back into the cluster.
   The apply is not byte-identical to what the human wrote — the GitOps tool
   stamps its own labels, annotations, and `managedFields` entries onto the
   object.
3. That apply is a live UPDATE. The reverser mirrors it, and commits again.
4. The forward leg sees *that* commit, applies, and so on.

The loop terminates only because the content eventually stops changing — a
convergence property standing in for an invariant nobody declared. Until it
converges it produces a run of machine-authored commits, each one re-triggering
the forward leg.

Before this workstream there was no identity-based filter anywhere on
`GitTargetSpec`, `WatchRuleSpec`, or `ResourceRule` — the only filters were
`operations` and the type matchers. Every reverser deployed alongside a forward
leg had this behavior.

`excludeFieldManagers` fixes it from watch state alone. See
[identity-write-exclusion.md](identity-write-exclusion.md) for why a label
selector cannot: a GitOps tool's labels *persist* on the object, so a selector
would also ignore a human's later edit of a tool-managed resource. The last
writer is a property of the write; the labels are a property of the object.

## #7 — APIService aggregation is not universal

The watch manager keeps its API-resource catalog fresh with two trigger
informers: one on `CustomResourceDefinition`, one on `APIService`. The
`APIService` informer was created unconditionally, so on an API server that does
not serve `apiregistration.k8s.io` — kcp, and other non-aggregating control
planes — client-go's reflector retried and logged forever. Benign, endlessly
repeated, and therefore exactly the kind of noise that hides a real error.

Both trigger informers are now started only when discovery reports their
resource is served, and are (re-)evaluated on every catalog refresh, so an
aggregation layer installed later is picked up without a restart. When a trigger
is unavailable the manager logs one `Info` line naming what it skipped, and the
catalog still refreshes on its 30s tick.

Discovery itself was already tolerant: a group that fails to serve is recorded
in the scan's degraded set, logged edge-triggered, and the catalog stays ready
on the remaining groups.
