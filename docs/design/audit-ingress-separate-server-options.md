# Audit ingress separation and cluster differentiation options

## Status

Design proposal only, updated with webhook ingress best-practice alignment.

## Context

Today both endpoints are served by the same controller-runtime admission-server listener and the same Service:

- Admission webhook endpoint [`/process-validating-webhook`](cmd/main.go:191)
- Audit endpoint [`/audit-webhook`](cmd/main.go:204)
- Single leader-only Service on port [`9443`](charts/gitops-reverser/templates/services.yaml:18) targeting one admission-server port [`9443`](charts/gitops-reverser/values.yaml:70)

This coupling limits independent exposure and independent TLS policy for incoming audit traffic.

## Objectives

- Move audit ingress to a separate audit-server listener and separate port.
- Allow explicit configuration of incoming TLS requirements for audit traffic.
- Support audit streaming from external or secondary clusters.
- Provide cluster differentiation options with trade-offs.
- Align ingress and webhook controls with production best practices.

## Non-goals

- No implementation sequencing in this document.
- No API schema changes in this document.

## Design principles

- Isolate admission reliability from audit ingest throughput concerns.
- Make TLS posture explicit and configurable per ingress surface.
- Keep path-based cluster identity as the initial model, with hardening controls.
- Separate admission and audit operational knobs.
- Keep defaults safe while still operable in day-1 deployments.

---

## Best-practice baseline to adopt

From [`docs/design/best-practices-webhook-ingress.md`](docs/design/best-practices-webhook-ingress.md), these controls are most relevant:

- Listener controls: `readTimeout`, `writeTimeout`, `idleTimeout`, `maxRequestBodyBytes`
- TLS controls: dedicated certs, CA trust, hot-reload support
- Registration controls for admission: `failurePolicy`, `timeoutSeconds`, selectors, tight rules
- Runtime controls: concurrency limit, metrics, request ID logging
- Audit-specific controls: queue, backpressure policy, dedup hinting, and separate endpoint or deployment

---

## Separation options for audit-server

### Option A: Same pod, second HTTP server, separate Service and port

Run a second server process inside the manager binary for audit-server ingest.

Pros:

- Lowest operational complexity.
- Independent port and TLS policy from admission.
- Minimal workload topology change.

Cons:

- Pod-level resource contention is still possible.
- Shared rollout and failure domain remains.

### Option B: Same pod, sidecar audit gateway

Add sidecar proxy for TLS termination and edge controls; manager receives internal traffic.

Pros:

- Mature L7 controls, rate limiting, and request-size guards.
- Useful when ingress policy complexity increases.

Cons:

- More config and operational surface in the same pod.

### Option C: Separate Deployment for audit receiver

Run audit receiver separately from controller manager.

Pros:

- Strongest fault and scaling isolation.
- Cleanest model for multi-cluster ingest growth.
- Independent SLO tuning for admission and audit.

Cons:

- Highest release and operations complexity.

### Recommended architectural target

- Near-term: Option A.
- Long-term scalable target: Option C.

This matches your request for simplicity now, while preserving a clean path to stronger isolation later.

---

## Incoming TLS requirements for audit ingress

### TLS policy modes

1. `strict`
   - Source cluster verifies server cert chain and hostname.
   - Production baseline.

2. `pinned-ca`
   - Like strict, with explicit pinned CA and dedicated audit cert lifecycle.

3. `insecure`
   - Dev and isolated tests only.

4. `mtls`
   - Server verification plus client cert auth.
   - Strongest identity, highest operational overhead.

### Default profile for your current preference

- Separate audit endpoint and separate port.
- Default `strict`.
- `mtls` optional hardening profile.
- Forbid `insecure` outside explicit non-prod environments.

### Config surface recommendation

Use separate top-level config blocks to avoid mixing concerns:

- `admissionWebhooks`
- `auditIngress`

Suggested `auditIngress` fields:

- `enabled`
- `listenAddress`
- `port`
- `pathPrefix`
- `tls.mode`
- `tls.secretName`
- `tls.clientCASecretName`
- `timeouts.read`
- `timeouts.write`
- `timeouts.idle`
- `maxRequestBodyBytes`
- `concurrency.maxInFlight`
- `queue.enabled`
- `queue.size`
- `queue.durability`
- `backpressure.mode`
- `identity.mode`
- `identity.allowedClusters`
- `network.allowedCIDRs`

---

## Cluster differentiation options

### Option 1: Path-based identity

Examples:

- `/audit-webhook/cluster-a`
- `/audit-webhook/cluster-b`

Pros:

- Very simple and native to audit webhook URL setup.
- No client cert lifecycle required.

Cons:

- Path value is not strong identity on its own.

Required controls:

- Strict allowlist for accepted cluster IDs.
- Reject unknown path IDs.
- Source network restrictions and logging.

### Option 2: Header-based identity through trusted proxy

Pros:

- Centralized edge identity mapping.

Cons:

- Depends on trusted proxy boundary.

### Option 3: Host or SNI based identity

Pros:

- Useful in DNS-centric ingress designs.

Cons:

- More cert and DNS complexity.

### Option 4: mTLS subject-based identity

Pros:

- Strong cryptographic source identity.

Cons:

- Highest cert issuance and rotation burden.

### Recommended cluster identity path

Given your stated preference:

1. Start with path-based identity.
2. Enforce strict allowlist.
3. Enforce network restrictions.
4. Keep mTLS available as a security profile switch.

---

## Delivery and reliability notes for audit ingestion

Audit ingest differs from admission webhook behavior and should assume:

- bursts
- duplicates
- out-of-order arrival
- potential data loss under severe backpressure depending on source settings

Minimum design controls:

- bounded queue
- explicit full-queue behavior
- optional batching downstream
- dedup hint support using audit event metadata when available

---

## Decision matrix

| Dimension | Path identity no mTLS | Path identity optional mTLS | Mandatory mTLS |
|---|---|---|---|
| Operational simplicity | Highest | Medium | Lowest |
| Security assurance | Medium | Medium to high | Highest |
| Cluster onboarding friction | Lowest | Medium | Highest |
| Cert lifecycle burden | Low | Medium | High |
| Fit for your current goal | Best | Good next step | Too heavy initially |

---

## Helm chart assessment: current state

### What is good today

- Leader-only webhook routing is already implemented in [`charts/gitops-reverser/templates/services.yaml`](charts/gitops-reverser/templates/services.yaml).
- TLS certificate automation exists through cert-manager in [`charts/gitops-reverser/templates/certificates.yaml`](charts/gitops-reverser/templates/certificates.yaml).
- Webhook cert mounting and runtime flags are wired in [`charts/gitops-reverser/templates/deployment.yaml`](charts/gitops-reverser/templates/deployment.yaml).
- Admission webhook settings expose useful controls in [`charts/gitops-reverser/values.yaml`](charts/gitops-reverser/values.yaml).

### What should be improved for your target architecture

1. Split config surfaces
   - Current config mixes admission and audit under [`webhook`](charts/gitops-reverser/values.yaml:66).
   - Introduce explicit `admissionWebhooks` and `auditIngress` blocks.

2. Separate service exposure
   - Today one leader-only service handles webhook traffic.
   - Add dedicated audit service and port; keep admission service independent.

3. Separate certificate lifecycle
   - Current certificate SANs and secret are tied to leader-only service.
   - Add separate cert and secret for audit service DNS names.

4. Add ingress runtime safety knobs
   - Missing explicit timeout and max-body controls for audit ingress.
   - Add `maxInFlight` and queue settings for burst handling.

5. Add audit identity and access controls
   - Add path-prefix and allowlist settings in chart values.
   - Add CIDR allowlist controls and corresponding policy templates.

6. Improve docs consistency
   - Chart README currently states defaults that differ from actual values in at least one place.
   - Align values table and examples to the current chart behavior.

### Risk notes in current chart

- A single webhook port and service remains a coupling point for admission and audit traffic.
- No first-class audit ingress queue or backpressure knobs are represented in chart values.
- Security posture for cross-cluster audit traffic is not explicit as a separate concern.

---

## Reference architecture sketch

```mermaid
graph TD
  A[Source cluster A api server] --> P[/audit-webhook/cluster-a]
  B[Source cluster B api server] --> Q[/audit-webhook/cluster-b]
  P --> S[audit-server separate port]
  Q --> S
  S --> V[Cluster ID allowlist validator]
  V --> R[Queue and backpressure controls]
  R --> E[Event pipeline]
```

## Final position

A separate audit-server listener on a separate port with configurable incoming TLS policy is the correct direction. For cluster differentiation, path-based identity is a practical default when it is combined with strict allowlist and network restrictions. The chart should be evolved to treat audit ingress as its own product surface with dedicated TLS, exposure, identity, and reliability controls.
