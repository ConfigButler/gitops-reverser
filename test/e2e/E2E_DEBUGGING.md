# E2E Test Infrastructure Debugging Guide

## Quick Access to E2E Services

After running e2e tests, the infrastructure remains running for debugging purposes.

### Kind Cluster Names by Test Type

- `make test-e2e` uses `KIND_CLUSTER_E2E` (default: `gitops-reverser-test-e2e`)
- `make test-e2e-quickstart-helm` uses `KIND_CLUSTER_QUICKSTART_HELM` (default: `gitops-reverser-test-e2e-quickstart-helm`)
- `make test-e2e-quickstart-manifest` uses `KIND_CLUSTER_QUICKSTART_MANIFEST` (default: `gitops-reverser-test-e2e-quickstart-manifest`)

This separation avoids cross-test contamination between end-to-end and quickstart install smoke tests.

### Port-Forward Management

The `test-e2e` target automatically starts port-forwards, so services are immediately accessible:

**Start port-forwards:**
```bash
make setup-port-forwards
```

This exposes:
- **Prometheus**: http://localhost:19090
- **Gitea**: http://localhost:13000 (Username: `testorg`, Password: `gitea`)

**Stop port-forwards:**
```bash
make cleanup-port-forwards
```

**Note:** The `make test-e2e` and `make setup-e2e` targets automatically run `setup-port-forwards`, so services are ready immediately after setup.

## Useful Prometheus Queries

### Check Pod Status
```promql
# Are both controller pods being scraped?
up{job="gitops-reverser"}

# Count of active pods
count(up{job="gitops-reverser"})
```

### Webhook Events
```promql
# Total webhook events across all pods
sum(gitopsreverser_events_received_total)

# Events by leader vs follower
gitopsreverser_events_received_total{role="leader"}
gitopsreverser_events_received_total{role!="leader"}
```

### Resource Metrics
```promql
# CPU usage
process_cpu_seconds_total{job="gitops-reverser"}

# Memory usage
process_resident_memory_bytes{job="gitops-reverser"}

# Goroutines
go_goroutines{job="gitops-reverser"}
```

## Network Architecture

```
Host Machine (port 13000, 19090)
    ↕ (VS Code forwarded ports from devcontainer)
DevContainer
    ↕ (kubectl port-forward)
Kind Cluster
    ├─ prometheus-operator namespace
    │  └─ Prometheus Operator + Prometheus (scrapes metrics via ServiceMonitor)
    ├─ gitea-e2e namespace
    │  └─ Gitea (Git server)
    └─ sut namespace (System Under Test)
       └─ Controller pods (2 replicas, HTTPS metrics)
```

## Debugging Failed Tests

1. **Ensure port-forwards are running:**
   ```bash
   make setup-port-forwards
   ```

2. **Check Prometheus for metrics history:**
   ```bash
   # Visit http://localhost:19090
   ```

3. **Check Gitea for repository state:**
   ```bash
   # Visit http://localhost:13000
   # Username: testorg, Password: gitea
   ```

4. **View controller logs:**
   ```bash
   kubectl logs -n sut -l control-plane=gitops-reverser --tail=100
   ```

5. **Check Prometheus scrape status:**
   Visit http://localhost:19090/targets

## Cleanup

```bash
# Clean up all E2E Kind clusters (all test types)
make cleanup-e2e-clusters

# Or clean one specific cluster name:
make cleanup-cluster KIND_CLUSTER=gitops-reverser-test-e2e

# Infra cleanup inside the active cluster:
make cleanup-gitea-e2e
```

## Available Make Targets

```bash
make setup-port-forwards    # Start port-forwards (Gitea:13000, Prometheus:19090)
make cleanup-port-forwards  # Stop all port-forwards
make ensure-prometheus-operator
make setup-e2e             # Setup Gitea + Prometheus Operator (+ cert-manager)
make test-e2e              # Run e2e tests (includes port-forwards)
make test-e2e-quickstart-helm
make test-e2e-quickstart-manifest
make cleanup-e2e-clusters  # Delete all E2E test clusters
```
