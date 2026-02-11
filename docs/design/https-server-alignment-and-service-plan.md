# HTTPS Server Alignment And Service Plan

## Goal

Improve consistency across the three HTTPS surfaces:

1. admission webhook server
2. audit ingress server
3. metrics server

With Service topology now simplified after removing the leader-only Service.

## Current Operating Mode

Run with **a single pod** for the current phase.

## Single-Replica Checklist

- [ ] `replicaCount: 1` is the chart default for this phase.
- [ ] HA-specific behavior is disabled/ignored by default.
- [ ] Leader-only Service has been removed from active topology.
- [ ] HA reintroduction is explicitly deferred to the planned rewrite.

## Current Constraints

- Leader-only routing is no longer part of active Service topology.
- Kubernetes Service selectors are per-Service, not per-port.

## Decision On "One Service, Three Ports"

### Can we do it technically?

Yes, Kubernetes supports one Service exposing multiple ports.

### Should we do it here?

Yes, this is now the active direction for the single-pod phase.

### Recommended topology (single pod)

Use **one Service with three ports**:

1. admission HTTPS
2. audit HTTPS
3. metrics HTTPS

This minimizes moving parts for the interim single-pod phase.

## Alignment Plan

## 1. Unify server config model

- Introduce a shared internal server config shape for:
  - bind address/port
  - cert path/name/key
  - read/write/idle timeout
  - TLS enabled/insecure mode guard
- Map flags into this model for all three servers.

## 2. Unify TLS/cert watcher bootstrap

- Add one helper that:
  - validates cert config
  - creates optional certwatcher
  - wires `GetCertificate`
  - applies shared TLS defaults (minimum version + HTTP/2 policy)
- Use same helper for metrics, admission, and audit.

## 3. Unify server lifecycle wiring

- Keep all servers manager-managed.
- Reuse one runnable pattern for startup/shutdown + timeout.
- Standardize startup/shutdown logs and error paths.

## 4. Align Helm values and args

- Keep existing keys for compatibility, but normalize structure:
  - `webhook.server`
  - `auditIngress`
  - `controllerManager.metrics`
- Ensure timeout and cert naming is consistent across all three blocks.

## 5. Simplify deployment model now

- Default chart/config to single replica.
- Keep leader-only Service removed from this phase.
- Keep optional leader-election code path only if low-cost; otherwise disable in defaults.

## 6. Service simplification (single service)

- Merge admission, audit, and metrics onto one Service with three target ports.
- Update cert SANs and docs accordingly.

## 7. Tests and rollout checks

- Unit:
  - shared server config parsing
  - shared TLS helper behavior
  - service template rendering for single Service with three ports
- E2E:
  - admission and audit reachable on same Service
  - metrics reachable on same Service
- Validation sequence:
  - `make build`
  - `make lint`
  - `make test-e2e`

## Target Settings Design (Markdown-Only)

This section defines the intended end-state configuration model without implementing it yet.

### End-State Overview

The chart should converge on:

- One Pod replica by default for this phase.
- One Service exposing three HTTPS ports (admission, audit, metrics).
- One shared server settings shape reused by all three listeners.
- Per-surface overrides only where behavior is genuinely different.
- Per-server TLS can be enabled/disabled independently.

### Proposed Helm Values Shape

```yaml
replicaCount: 1

network:
  service:
    enabled: true
    type: ClusterIP
    ports:
      admission: 443
      audit: 8444
      metrics: 8443

servers:
  defaults:
    enableHTTP2: false
    timeouts:
      read: 15s
      write: 30s
      idle: 60s
    tls:
      enabled: true
      certPath: ""
      certName: tls.crt
      certKey: tls.key
      minVersion: VersionTLS12

  admission:
    enabled: true
    bindAddress: :9443
    timeouts: {}   # optional override
    tls:
      enabled: true   # may be set false for local/dev scenarios
      secretName: ""  # optional if cert-manager manages mount/secret

  audit:
    enabled: true
    bindAddress: :9444
    maxRequestBodyBytes: 10485760
    timeouts: {}
    tls:
      enabled: true
      secretName: ""

  metrics:
    enabled: true
    bindAddress: :8080
    secure: true
    timeouts: {}
    tls:
      enabled: true
      secretName: ""
```

If `servers.<name>.tls.enabled` is omitted, inherit from `servers.defaults.tls.enabled`.

### Settings Responsibilities

| Area | Purpose | Notes |
|---|---|---|
| `servers.defaults` | Shared defaults for all HTTPS listeners | Single source of truth for TLS + timeout defaults, including TLS default on/off |
| `servers.admission` | Admission-specific listener settings | Keeps webhook behavior settings separate under `webhook.validating` |
| `servers.audit` | Audit ingress listener settings | Retains audit payload controls like `maxRequestBodyBytes` |
| `servers.metrics` | Metrics listener settings | Supports secure metrics endpoint consistently, but can be intentionally downgraded per environment |
| `network.service` | Cluster Service exposure | Owns externally reachable ports only, not container bind ports |

### Compatibility Mapping (Current -> Target)

| Current key | Target key | Migration intent |
|---|---|---|
| `webhook.server.port` | `servers.admission.bindAddress` | Keep old key as compatibility alias initially |
| `webhook.server.certPath/certName/certKey` | `servers.admission.tls.*` (or inherited defaults) | Prefer inherited defaults unless explicitly overridden |
| `auditIngress.port` | `servers.audit.bindAddress` | Preserve behavior, normalize naming |
| `auditIngress.tls.*` | `servers.audit.tls.*` | Direct move |
| `auditIngress.timeouts.*` | `servers.audit.timeouts.*` | Direct move |
| `controllerManager.metrics.bindAddress` | `servers.metrics.bindAddress` | Unify metrics with same server model |
| `controllerManager.enableHTTP2` | `servers.defaults.enableHTTP2` | Single flag for all listeners in this phase |
| `controllerManager.metrics.secure` (if present) | `servers.metrics.tls.enabled` | Keep compatibility alias during migration |

### CLI Args/Runtime Mapping Direction

Desired runtime model:

- Parse Helm values into one internal server settings struct per surface.
- Apply shared defaulting/validation once.
- Generate listener-specific runtime config from the same code path.

Resulting behavior goals:

- Same TLS validation rules for all listeners.
- Same timeout parsing and error messages for all listeners.
- Same startup/shutdown lifecycle pattern for all listeners.
- If TLS is disabled for a listener, skip cert watcher/bootstrap for that listener and run plain HTTP on its bind address.

### TLS Disable Guardrails

- Keep TLS enabled by default for all listeners.
- Treat TLS-disabled mode as non-production convenience for local/dev/test.
- Emit a startup warning whenever any listener runs with TLS disabled.
- Admission/audit TLS disable should be opt-in only and clearly visible in rendered values.

### Rollout Notes For Settings Refactor

- Keep legacy keys supported during transition.
- Emit clear deprecation warnings when legacy keys are used.
- Switch docs/examples to target keys first; keep compatibility notes adjacent.
- Remove deprecated keys only after at least one stable release carrying warnings.
