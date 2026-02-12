# HTTPS Server Alignment And Service Plan

## Goal

Improve consistency across the three HTTPS surfaces:

1. admission-server
2. audit ingress server
3. metrics-server

With Service topology now simplified after removing the leader-only Service.

## Current Operating Mode

Run with **a single pod** for the current phase.

## Single-Replica Checklist

- [ ] `replicaCount: 1` is the chart default for this phase.
- [ ] HA-specific behavior is disabled/ignored by default.
- [ ] Leader-only Service has been removed from active topology.
- [ ] HA reintroduction is explicitly deferred to the planned rewrite.
- [ ] Service exposure is consolidated to a single Service named only `{{ include "gitops-reverser.fullname" . }}`.

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

### Service naming decision

Use a single Service with the base release fullname only:

- target name: `{{ include "gitops-reverser.fullname" . }}`
- avoid suffixes such as `-webhook`, `-audit`, `-metrics` for the primary Service
- keep distinct named ports for routing/monitoring clarity

## Single Service Necessity Analysis

### Is a single Service still needed?

Yes for this phase, and there is no strong technical reason to keep separate Services right now.

### Why this still makes sense now

- Current services all select the same controller Pod labels, so they do not provide workload isolation.
- Single-replica mode removes the previous leader-vs-all selector split that justified separate routing.
- Operationally, one stable Service name simplifies client configuration and day-2 debugging.
- The design already requires one endpoint surface with different ports, which matches Service named ports well.

### What currently depends on split service names (implementation impact)

- `charts/gitops-reverser/templates/validating-webhook.yaml` references the `-webhook` service name.
- `charts/gitops-reverser/templates/certificates.yaml` SANs include `-webhook` and `-audit` DNS names.
- `charts/gitops-reverser/templates/servicemonitor.yaml` and e2e checks currently expect a dedicated metrics service identity.
- `test/e2e/e2e_test.go` asserts `gitops-reverser-webhook-service` and `gitops-reverser-audit-webhook-service`.

### Conclusion

- Keep the plan to converge to **one Service**.
- Use one canonical Service name only (release fullname).
- Keep multiple ports; do not keep multiple Services unless HA/service-isolation requirements return.

## No-Compatibility Decision

For this refactor, use a direct switch without migration compatibility measures.

Rationale:

- The old settings layout is already causing conceptual drift (`webhook.server`, `auditIngress`, `controllerManager.metrics`).
- A compatibility layer would preserve that drift and increase implementation/testing complexity.
- The project is intentionally converging on one topology and one config model for this phase.
- A hard cut keeps behavior deterministic and easier to reason about during rapid iteration.

## Alignment Plan

## 1. Unify server config model

- Introduce a shared internal server config shape for:
  - bind address/port
  - cert path/name/key
  - read/write/idle timeout
  - TLS enabled/insecure mode guard
- Define baseline defaults in source code, not in Helm values.
- Map flags into this model for all three servers.
- Keep one parser/defaulting path for all listeners (no per-listener parsing forks).

## 2. Unify TLS/cert watcher bootstrap

- Add one helper that:
  - validates cert config
  - creates optional certwatcher
  - wires `GetCertificate`
  - applies shared TLS defaults (minimum version + HTTP/2 policy)
- Use same helper for metrics, admission, and audit.
- Keep TLS-off behavior in the same helper path (no duplicate conditional logic per server).

## 3. Unify server lifecycle wiring

- Keep all servers manager-managed.
- Reuse one runnable pattern for startup/shutdown + timeout.
- Standardize startup/shutdown logs and error paths.
- Build servers through one reusable constructor/builder function that accepts a typed server config.

## 4. Align Helm values and args

- Replace legacy split keys with one canonical settings structure.
- Ensure timeout and cert naming is consistent across all three listeners.

## 5. Simplify deployment model now

- Default chart/config to single replica.
- Keep leader-only Service removed from this phase.
- Keep optional leader-election code path only if low-cost; otherwise disable in defaults.

## 6. Service simplification (single service)

- Merge admission, audit, and metrics onto one Service with three target ports.
- Name the Service as release fullname only (no role suffix).
- Update validating webhook client config, cert SANs, ServiceMonitor selector/port, and docs accordingly.

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
- Defaults are centralized in source code; Helm values provide explicit overrides only.

### Proposed Helm Values Shape

```yaml
replicaCount: 1

network:
  service:
    enabled: true
    name: "" # defaults to {{ include "gitops-reverser.fullname" . }}
    type: ClusterIP
    ports:
      admission: 9443
      audit: 8444
      metrics: 8443

servers:
  admission:
    enabled: true
    bindAddress: :9443
    enableHTTP2: false # optional override
    timeouts: {}       # optional override
    tls:
      enabled: true   # may be set false for local/dev scenarios
      secretName: ""  # optional if cert-manager manages mount/secret

  audit:
    enabled: true
    bindAddress: :9444
    maxRequestBodyBytes: 10485760
    enableHTTP2: false
    timeouts: {}
    tls:
      enabled: true
      secretName: ""

  metrics:
    enabled: true
    bindAddress: :8080
    enableHTTP2: false
    timeouts: {}
    tls:
      enabled: true
      secretName: ""
```
If `servers.<name>.tls.enabled` (or timeout/http2 overrides) is omitted, source-code defaults apply.

### Settings Responsibilities

| Area | Purpose | Notes |
|---|---|---|
| `servers.admission` | Admission-specific listener settings | Keeps webhook behavior settings separate under `webhook.validating` |
| `servers.audit` | Audit ingress listener settings | Retains audit payload controls like `maxRequestBodyBytes` |
| `servers.metrics` | Metrics listener settings | Supports secure metrics endpoint consistently, but can be intentionally downgraded per environment |
| `network.service` | Cluster Service exposure | Owns service name and externally reachable ports (not container bind ports) |
| Source code defaults | Runtime baseline behavior | Holds canonical defaults for timeouts, TLS baseline, and HTTP/2 policy |

### Key Mapping (Current -> Target, No Compatibility Layer)

| Current key | Target key |
|---|---|
| `webhook.server.port` | `servers.admission.bindAddress` |
| `webhook.server.certPath/certName/certKey` | `servers.admission.tls.*` (or source-code defaults) |
| `auditIngress.port` | `servers.audit.bindAddress` |
| `auditIngress.tls.*` | `servers.audit.tls.*` |
| `auditIngress.timeouts.*` | `servers.audit.timeouts.*` |
| `controllerManager.metrics.bindAddress` | `servers.metrics.bindAddress` |
| `controllerManager.enableHTTP2` | `servers.<name>.enableHTTP2` or source-code default |

### CLI Args/Runtime Mapping Direction

Desired runtime model:

- Parse Helm values into one internal server settings struct per surface.
- Apply shared defaulting/validation once.
- Generate listener-specific runtime config from the same code path.
- Construct `http.Server` instances via shared functions (for example `buildHTTPServer`, `buildTLSConfig`, `buildServerRunnable`) instead of per-listener copies.

Resulting behavior goals:

- Same TLS validation rules for all listeners.
- Same timeout parsing and error messages for all listeners.
- Same startup/shutdown lifecycle pattern for all listeners.
- If TLS is disabled for a listener, skip cert watcher/bootstrap for that listener and run plain HTTP on its bind address.
- No triple repetition of server setup code for admission/audit/metrics.

### TLS Disable Guardrails

- Keep TLS enabled by default for all listeners.
- Treat TLS-disabled mode as non-production convenience for local/dev/test.
- Emit a startup warning whenever any listener runs with TLS disabled.
- Admission/audit TLS disable should be opt-in only and clearly visible in rendered values.

### Rollout Notes For Settings Refactor

- Use a clean-cut switch to the new settings model.
- Do not ship compatibility aliases or legacy key fallbacks.
- Update chart docs/examples and templates in the same change set.
- Fail fast on invalid/unknown legacy settings to avoid ambiguous runtime behavior.
