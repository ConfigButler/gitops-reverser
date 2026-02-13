# Prometheus E2E Helm/ServiceMonitor Evaluation

## Status
- Date: 2026-02-13
- Decision: **Do not migrate now**
- Scope: `test/e2e` Prometheus setup only

## Context
Current e2e Prometheus setup is manifest/script based:
- setup entrypoint: `Makefile` target `setup-prometheus-e2e`
- deploy script: `test/e2e/scripts/setup-prometheus.sh`
- manifests: `test/e2e/prometheus/deployment.yaml`, `test/e2e/prometheus/rbac.yaml`

Current tests also assume specific Prometheus naming/labels and scrape job naming:
- pod label wait: `app=prometheus`
- service port-forward target: `svc/prometheus:19090`
- PromQL assertions: `job='gitops-reverser-metrics'`

## Evaluated Plan

### Option A: Move to standalone Prometheus Helm chart
Use `prometheus-community/prometheus` with pinned chart version, e2e values file, and Helm lifecycle (`upgrade`, revision history, rollback).

Required changes:
- Replace manifest apply/delete flow in `Makefile` Prometheus targets with Helm install/uninstall.
- Replace `setup-prometheus.sh` behavior with Helm-driven setup.
- Add e2e values file and pin chart version.
- Update port-forward/pod-ready checks that currently assume manual names/labels.

### Option B: Add ServiceMonitor-based scraping
Use an operator-based chart (typically `kube-prometheus-stack`) because ServiceMonitor discovery is provided by Prometheus Operator, not by standalone Prometheus chart.

Required changes (in addition to Option A-level migration):
- Install operator CRDs/controllers via Helm in e2e.
- Ensure ServiceMonitor exists for both install paths:
  - Helm install smoke path (chart templated ServiceMonitor already exists behind values flag).
  - Kustomize e2e path (`make deploy`) requires separate ServiceMonitor manifest.
- Update PromQL tests to avoid hardcoded `job='gitops-reverser-metrics'` assumptions.

## Pros and Cons

### Pros of migrating to Helm
- Native `helm upgrade` workflow and revision/rollback history.
- Better consistency with existing e2e dependency setup style (similar to Gitea).
- Centralized values-based configuration.

### Pros of adding ServiceMonitor path
- Cleaner scrape target management than static scrape config.
- Better alignment with common Kubernetes monitoring practices.
- Reuses chart-level `monitoring.serviceMonitor` support where applicable.

### Cons / Risks
- Standalone Prometheus chart does **not** provide ServiceMonitor consumption.
- ServiceMonitor requires operator stack, increasing e2e complexity and startup time.
- Existing e2e scripts/tests are coupled to current names/labels/job-name; migration requires non-trivial refactors.
- Adds another chart/version dependency surface in CI and local flows.

## Decision
We decided to **not do this now**.

Rationale:
- Current setup is stable and intentionally minimal for e2e signal validation.
- Migration introduces meaningful complexity (especially for ServiceMonitor support).
- The value is primarily operational ergonomics rather than test coverage expansion.

## Revisit Criteria
Re-open this migration when at least one of the following becomes a priority:
- Need Helm revision/rollback behavior for routine e2e debugging.
- Need ServiceMonitor-driven discovery parity with production environments.
- Need more dynamic scrape target management across multiple test topologies.

## Next Step (Deferred)
If revisited, prefer a two-phase approach:
1. Phase 1: standalone Prometheus Helm chart migration (no ServiceMonitor).
2. Phase 2: operator-based monitoring stack + ServiceMonitor migration and test query normalization.
