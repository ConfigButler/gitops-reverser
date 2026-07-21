# Multi-source audit-ingress hardening

> **design** — open, not yet built. Index: [`../INDEX.md`](../INDEX.md)
>
> This is deliberately narrow: `ClusterProvider` source connectivity, provider-name fact partitioning,
> and reconcile-time namespace authorization are shipped. This document decides how several source
> control planes may safely share one audit ingress, and how their ingestion is kept fair.

## Current boundary

When author attribution is enabled, a normal source posts an audit `EventList` to
`/audit-webhook/<provider>`. The server requires a certificate signed by the configured audit CA and
checks that the named `ClusterProvider` exists. It does **not** bind the peer certificate to that
provider. A holder of a shared, CA-signed client certificate can therefore submit facts for any existing
provider route.

That is acceptable only inside one privileged control-plane trust domain where every holder of the
credential is authorized to act for every provider. It is not a tenant-isolation or independently
administered-source boundary. The current user-facing limit is documented in
[`../architecture.md`](../architecture.md#optional-attribution) and
[`../../SECURITY.md`](../../SECURITY.md#shared-audit-ingress-trust-model).

The bare `/audit-webhook` route is different: it is enabled only when
`--author-attribution-audit-route-annotation-key` is set, then resolves every event to an audit route from that
annotation. It is suitable only if the upstream control plane stamps the annotation and an untrusted
writer cannot forge it.

## Decision to make before supporting independent sources

Choose one of these authentication contracts; do not describe named paths alone as authentication.

### A. Provider-bound mTLS — default for independent control planes

Give every `ClusterProvider` a client-certificate identity. The handler maps the verified peer
certificate to exactly one provider and requires it to match the route name. The design must specify:

- how the binding is declared and where the public identity is stored;
- safe overlap during client-certificate rotation;
- how deletion or recreation immediately revokes the old identity; and
- audit logs and metrics for a missing, unknown, or mismatched identity without logging credentials.

This keeps the named route and makes the provider name both routing and authenticated sender identity.

### B. Annotation-routed shared ingress — for a trusted shard/control plane

Use one authenticated producer and route individual audit events by an annotation the producer itself
stamps (for example, a virtual-cluster shard identity). The design must prove that normal source users
cannot set or alter that annotation, reject events with a missing or unknown provider, and document the
trust boundary of the shared credential. This is a different deployment model, not a shortcut for
independent clusters.

## Fairness and limits

Neither current route applies a provider-specific ingestion limit. Before supporting many providers on
one instance, define a bounded per-provider policy: maximum request/event rate, memory/Redis work bound,
shed response and retry behaviour, and metrics that distinguish one noisy provider from a global outage.
The policy must preserve the existing correctness rule: a dropped or late fact can only fall back to the
configured committer; it must never attach an author from another provider.

## Acceptance criteria

- An end-to-end remote round trip proves that a mutation from a named provider becomes a commit authored
  by that actor.
- Identical object identities and resource versions from two providers never cross-credit an author.
- Under provider-bound mTLS, a valid certificate for provider A cannot record facts on provider B's route;
  rotation overlaps safely and deletion/recreation rejects the retired identity.
- Under annotation routing, an event with a missing, unknown, or untrusted annotation records no fact.
- A provider exceeding its limit cannot prevent another provider from being ingested, and the resulting
  fallback/shed outcome is observable.

## Explicitly out of scope

This work does not add managed-control-plane support, admission attribution, workload identity for source
cluster watches, or a durable HA delivery queue. Those remain separate product decisions.
