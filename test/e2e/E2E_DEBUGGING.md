# E2E Test Infrastructure Debugging Guide

## Quick Access to E2E Services

After running e2e tests, the infrastructure remains running for debugging purposes.

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

**Note:** The `make test-e2e` and `make e2e-setup` targets automatically run `setup-port-forwards`, so services are ready immediately after setup.

## Useful Prometheus Queries

### Check Pod Status
```promql
# Are both controller pods being scraped?
up{job="gitops-reverser-metrics"}

# Count of active pods
count(up{job="gitops-reverser-metrics"})
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
process_cpu_seconds_total{job="gitops-reverser-metrics"}

# Memory usage
process_resident_memory_bytes{job="gitops-reverser-metrics"}

# Goroutines
go_goroutines{job="gitops-reverser-metrics"}
```

## Network Architecture

```
Host Machine (port 13000, 19090)
    ↕ (exposed via --network=host)
DevContainer
    ↕ (kubectl port-forward)
Kind Cluster
    ├─ prometheus-e2e namespace
    │  └─ Prometheus (scrapes metrics via HTTPS + bearer token)
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
   kubectl logs -n sut -l control-plane=controller-manager --tail=100
   ```

5. **Check Prometheus scrape status:**
   Visit http://localhost:19090/targets

## Cleanup

```bash
# Clean up all e2e infrastructure
make e2e-cleanup

# Or individually:
make cleanup-prometheus-e2e
make cleanup-gitea-e2e
```

## Available Make Targets

```bash
make setup-port-forwards    # Start port-forwards (Gitea:13000, Prometheus:19090)
make cleanup-port-forwards  # Stop all port-forwards
make e2e-setup             # Setup Gitea + Prometheus + port-forwards
make test-e2e              # Run e2e tests (includes port-forwards)
make e2e-cleanup           # Clean up all infrastructure