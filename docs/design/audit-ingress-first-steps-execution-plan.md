# Audit ingress first steps execution plan

## Status

Execution-focused handoff plan for implementation agent.

Scope is fixed to:

- single deployment
- extra in-binary audit webserver
- path-based cluster recognition
- Kind remains the e2e cluster target

This document intentionally excludes alternative architecture discussion.

---

## 1. Required outcome

Implement an initial production-ready split where:

- admission webhook keeps running on current webhook server path [`/process-validating-webhook`](cmd/main.go:191)
- audit ingress moves to a separate server in the same binary on a different port
- audit ingress is exposed via a dedicated Service
- cluster identity is derived from request path segment
- ingress TLS requirements for audit are independently configurable

---

## 2. Current code and chart risks that must be addressed

### 2.1 Coupling risks

- Both admission and audit handlers are registered on one webhook server in [`cmd/main.go`](cmd/main.go:101) and [`cmd/main.go`](cmd/main.go:204)
- One service endpoint currently fronts this surface in [`charts/gitops-reverser/templates/services.yaml`](charts/gitops-reverser/templates/services.yaml:3)
- One cert lifecycle currently serves this surface in [`charts/gitops-reverser/templates/certificates.yaml`](charts/gitops-reverser/templates/certificates.yaml:16)

### 2.2 TLS posture risks

- e2e audit kubeconfig currently uses insecure skip verify in [`test/e2e/kind/audit/webhook-config.yaml`](test/e2e/kind/audit/webhook-config.yaml:14)
- cluster docs use insecure skip verify in [`docs/audit-setup/cluster/audit/webhook-config.yaml`](docs/audit-setup/cluster/audit/webhook-config.yaml:11)

### 2.3 Audit ingress hardening gaps in code

In [`internal/webhook/audit_handler.go`](internal/webhook/audit_handler.go:86):

- no request body size limit before decode
- no explicit server-level timeouts
- no concurrency guard for burst traffic
- no path-based cluster ID parser

### 2.4 E2E and docs drift already visible

- Kind README references DNS endpoint while config uses fixed IP and path in [`test/e2e/kind/README.md`](test/e2e/kind/README.md:31) vs [`test/e2e/kind/audit/webhook-config.yaml`](test/e2e/kind/audit/webhook-config.yaml:12)
- Helm README contains defaults not fully aligned with values in [`charts/gitops-reverser/README.md`](charts/gitops-reverser/README.md:183) and [`charts/gitops-reverser/values.yaml`](charts/gitops-reverser/values.yaml:6)

---

## 3. Implementation contract for first step

### 3.1 Runtime topology

Implement two servers in one process:

- admission server
  - existing controller-runtime webhook server
  - keeps current cert and service behavior
- audit server
  - dedicated `http.Server` listener on separate port
  - independent TLS config inputs
  - serves audit paths with cluster path segment

### 3.2 Path-based cluster recognition contract

Accepted path format:

- `/audit-webhook/{clusterID}`

Rules:

- path prefix is fixed to `/audit-webhook` in this phase (not configurable)
- reject requests without `{clusterID}`
- accept any non-empty `{clusterID}` and handle newly seen cluster IDs
- emit structured logs with resolved `clusterID`
- add metric label for `cluster_id`

### 3.3 TLS policy contract for first step

For phase 1:

- keep server TLS mandatory for audit ingress
- support strict CA verification by source cluster configuration
- do not require mTLS in this phase
- preserve option to add mTLS later without path changes

---

## 4. Concrete code work items

### 4.1 Add audit server config model in main

Target file: [`cmd/main.go`](cmd/main.go:253)

Add new app config fields for audit server, separate from webhook server fields:

- audit listen address and port
- audit cert path, cert name, cert key
- audit max request body bytes
- audit read timeout
- audit write timeout
- audit idle timeout

Add flags in [`parseFlags()`](cmd/main.go:270) for above.

### 4.2 Implement dedicated audit server bootstrap

Target file: [`cmd/main.go`](cmd/main.go:77)

Add functions to:

- build audit `http.ServeMux`
- register handler pattern on fixed `/audit-webhook/`
- initialize TLS cert watcher for audit cert files
- construct dedicated `http.Server` with explicit timeouts
- add graceful shutdown using manager context

Implementation note:

- audit server should be started via manager runnable so lifecycle follows manager start and stop.

### 4.3 Extend audit handler with path identity and guardrails

Target file: [`internal/webhook/audit_handler.go`](internal/webhook/audit_handler.go:50)

Add config fields:

- max request body bytes

Add behavior:

- parse and validate cluster ID from request path
- reject invalid path with `400`
- limit body read size before decode
- include cluster ID in all processing logs
- include cluster ID metric attribute for [`metrics.AuditEventsReceivedTotal`](internal/webhook/audit_handler.go:172)

### 4.4 Keep admission webhook behavior untouched

Do not change current validating webhook registration semantics in [`cmd/main.go`](cmd/main.go:190) and chart registration in [`charts/gitops-reverser/templates/validating-webhook.yaml`](charts/gitops-reverser/templates/validating-webhook.yaml:16).

---

## 5. Helm chart work items

### 5.1 Values schema additions

Target file: [`charts/gitops-reverser/values.yaml`](charts/gitops-reverser/values.yaml:65)

Add explicit `auditIngress` block with:

- `enabled`
- `port`
- `tls.certPath`
- `tls.certName`
- `tls.certKey`
- `tls.secretName`
- `timeouts.read`
- `timeouts.write`
- `timeouts.idle`
- `maxRequestBodyBytes`
- optional fixed `clusterIP` for Kind e2e compatibility

Keep existing webhook block for admission as-is in first phase.

### 5.2 Deployment args and ports

Target file: [`charts/gitops-reverser/templates/deployment.yaml`](charts/gitops-reverser/templates/deployment.yaml:41)

Add container args for audit server flags and add second named container port for audit ingress.

Mount dedicated audit TLS secret path in addition to admission cert mount.

### 5.3 Dedicated audit service template

Target file: [`charts/gitops-reverser/templates/services.yaml`](charts/gitops-reverser/templates/services.yaml:3)

Add new service resource:

- name suffix `-audit`
- port 443 to target audit container port
- optional fixed clusterIP setting
- selector consistent with leader-only routing requirement

Keep current service for admission webhook unchanged.

### 5.4 Dedicated audit certificate

Target file: [`charts/gitops-reverser/templates/certificates.yaml`](charts/gitops-reverser/templates/certificates.yaml:16)

Add second certificate resource for audit service DNS names and audit secret.

Keep existing serving cert for admission webhook.

### 5.5 Chart docs updates

Target file: [`charts/gitops-reverser/README.md`](charts/gitops-reverser/README.md:177)

Update:

- config table with new `auditIngress` settings
- architecture section to show two service surfaces
- examples for per-cluster path URLs in audit kubeconfig
- fix stale defaults and names where currently inconsistent with values

---

## 6. Kustomize and default manifests work items

### 6.1 Add audit service and optional fixed IP patch

Relevant files:

- [`config/webhook/service.yaml`](config/webhook/service.yaml:1)
- [`config/default/webhook_service_fixed_ip_patch.yaml`](config/default/webhook_service_fixed_ip_patch.yaml:1)
- [`config/default/kustomization.yaml`](config/default/kustomization.yaml:44)

Actions:

- add separate audit service manifest
- add separate fixed IP patch for audit service for Kind startup constraints
- keep admission service patch independent

### 6.2 Add audit certificate resource

Relevant files:

- [`config/certmanager/certificate-webhook.yaml`](config/certmanager/certificate-webhook.yaml:1)
- [`config/default/kustomization.yaml`](config/default/kustomization.yaml:126)

Actions:

- add second cert for audit service DNS names
- add replacement wiring for audit service name and namespace into cert DNS entries
- keep admission CA injection for validating webhook intact

### 6.3 Add manager patch entries for audit server args and mounts

Relevant file: [`config/default/manager_webhook_patch.yaml`](config/default/manager_webhook_patch.yaml:1)

Actions:

- add audit-specific args
- add audit TLS volume and mount
- add audit container port

---

## 7. Test plan updates required

### 7.1 Unit tests

#### Audit handler tests

Target file: [`internal/webhook/audit_handler_test.go`](internal/webhook/audit_handler_test.go:50)

Add table-driven cases for:

- valid path with cluster ID
- missing cluster ID path
- newly seen cluster ID is accepted
- body larger than configured max bytes
- non-POST path handling remains unchanged

### 7.2 Main bootstrap tests

Add new tests for config parsing and audit server bootstrap behavior.

Suggested new file:

- `cmd/main_audit_server_test.go`

Cover:

- default flag values
- custom audit flag parsing
- invalid timeout parsing behavior if introduced
- audit server runnable registration

### 7.3 E2E changes on Kind

#### Keep Kind as the only cluster target

Relevant files:

- [`Makefile`](Makefile:69)
- [`test/e2e/kind/cluster-template.yaml`](test/e2e/kind/cluster-template.yaml:1)
- [`test/e2e/kind/audit/webhook-config.yaml`](test/e2e/kind/audit/webhook-config.yaml:1)
- [`test/e2e/e2e_test.go`](test/e2e/e2e_test.go:367)
- [`test/e2e/helpers.go`](test/e2e/helpers.go:159)

Required changes:

1. Audit webhook URL path must include cluster ID
   - update to `/audit-webhook/<cluster-id>` in Kind webhook config

2. Audit service endpoint target
   - point webhook config to the new dedicated audit service fixed IP

3. Certificate readiness checks
   - extend helper to wait for new audit cert secret in addition to existing secrets

4. E2E validation assertions
   - keep current audit metric checks
   - add validation that cluster IDs from path are accepted, including newly seen IDs

5. Optional strict TLS uplift for e2e
   - phase 1 may keep insecure skip verify for bootstrap simplicity
   - add plan note and TODO test for certificate-authority based verification once secret extraction is automated

### 7.4 Smoke checks for service split

Add e2e checks to verify:

- admission service and audit service both exist
- audit service resolves to leader endpoint only
- audit ingress works on dedicated port and path

---

## 8. Observability and operational requirements

### 8.1 Logging

Every accepted audit request must log:

- cluster ID
- remote address
- request path
- event count
- processing outcome

### 8.2 Metrics

Extend audit metric labels to include cluster dimension.

Ensure cardinality protection:

- sanitize and constrain cluster ID format/length before labeling

### 8.3 Error handling

Audit server must return:

- `400` for malformed path or body
- `405` for method mismatch
- `500` only for internal processing errors

---

## 9. Backward compatibility behavior

Phase 1 behavior for cluster path migration:

- no fallback to bare `/audit-webhook` endpoint
- configuration and docs must explicitly require `/audit-webhook/{clusterID}`

Reason:

- prevents ambiguous identity
- avoids silent insecure defaults

---

## 10. Acceptance criteria for coding agent

Implementation is complete only when all are true:

1. Separate in-binary audit server is active on separate port with separate service exposure.
2. Audit endpoint requires path-based cluster ID on fixed `/audit-webhook/{clusterID}` and accepts newly seen cluster IDs.
3. Admission webhook behavior remains unchanged.
4. Helm and kustomize manifests include independent audit TLS and service resources.
5. Kind e2e setup is updated and passing with new audit path contract.
6. Tests cover path validation and certificate readiness adjustments.
7. Documentation for setup and e2e reflects new service and URL contract.
8. Validation pipeline passes in this sequence:
   - `make fmt`
   - `make generate`
   - `make manifests`
   - `make vet`
   - `make lint`
   - `make test`
   - `make test-e2e`

---

## 11. Handoff checklist

- Update docs for cluster audit config in [`docs/audit-setup/cluster/audit/webhook-config.yaml`](docs/audit-setup/cluster/audit/webhook-config.yaml:1)
- Update Kind docs in [`test/e2e/kind/README.md`](test/e2e/kind/README.md:1)
- Update chart docs in [`charts/gitops-reverser/README.md`](charts/gitops-reverser/README.md:1)
- Keep architecture alternatives in [`docs/design/audit-ingress-separate-webserver-options.md`](docs/design/audit-ingress-separate-webserver-options.md:1) and keep this document implementation-only

This plan is ready to hand to a coding agent for direct execution.
