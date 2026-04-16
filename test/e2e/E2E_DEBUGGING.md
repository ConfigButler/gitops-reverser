# E2E Test Infrastructure Debugging Guide

## Quick Access to E2E Services

After running e2e tests, the infrastructure remains running for debugging purposes.

The harness is intentionally built to reuse prepared state between runs, so ordinary iteration
should not require rebuilding everything from scratch.

### Cluster Context

- `task test-e2e` defaults to `CTX=k3d-gitops-reverser-test-e2e`
- `task test-e2e-quickstart-helm` defaults to the same `CTX` unless you override it
- `task test-e2e-quickstart-manifest` defaults to the same `CTX` unless you override it

If you want isolation between runs, pass a different `CTX` explicitly.

### Port-Forward Management

The `test-e2e` target automatically starts port-forwards, so services are immediately accessible:

**Start port-forwards:**
```bash
task portforward-ensure
```

This exposes:
- **Prometheus**: http://localhost:19090
- **Gitea**: http://localhost:13000 (Username: `testorg`, Password: `gitea`)

**Stop port-forwards:**
```bash
task clean-port-forwards
```

**Note:** The `task test-e2e` and `task prepare-e2e` targets automatically run
`task portforward-ensure`, so services are ready immediately after setup.

## What Gets Reused Between Runs

Most of the expensive e2e work is cached on purpose:

- shared cluster and install state lives under `.stamps/cluster/<ctx>/`
- local repo checkouts live under `.stamps/repos/<repo>/` unless `REPOS_DIR` or `CHECKOUT_DIR`
  overrides them
- generated repo Secret manifests live under `.stamps/e2e-repo-artifacts/`

The point of these artifacts is to avoid redoing cluster setup, controller install, and repo
fixture work when the relevant inputs have not changed.

In normal day-to-day iteration, keep those artifacts and rerun the e2e target you care about.
That is the fast path.

## When To Reuse And When To Clean

Reuse the existing cluster and `.stamps` when:

- you are iterating on tests or controller code
- the current cluster is healthy
- you want to inspect or reuse the existing repo checkouts

Clean or isolate when:

- the cluster is broken or behaving inconsistently
- you want a completely fresh environment
- you need to compare two runs without shared state

Useful commands:

```bash
task clean-cluster
task clean-cluster CTX=k3d-gitops-reverser-test-e2e
task test-e2e CTX=k3d-gitops-reverser-test-e2e-fresh
```

Avoid deleting `.stamps` wholesale unless you actually want the full rebuild cost. The harness
keeps those artifacts specifically so most reruns stay cheap.

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
    └─ controller namespace (System Under Test; set via `NAMESPACE`)
       └─ Controller pods (2 replicas, HTTPS metrics)
```

## Debugging Failed Tests

1. **Ensure port-forwards are running:**
   ```bash
   task portforward-ensure
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
   kubectl logs -n "${NAMESPACE}" -l control-plane=gitops-reverser --tail=100
   ```

5. **Check Prometheus scrape status:**
   Visit http://localhost:19090/targets

## Cleanup

```bash
# Tear down the default E2E cluster and remove its stamps
task clean-cluster

# Or clean a specific context
task clean-cluster CTX=k3d-gitops-reverser-test-e2e

# Stop local port-forwards
task clean-port-forwards
```

## Available Task Targets

```bash
task portforward-ensure     # Start/verify port-forwards (Gitea:13000, Prometheus:19090)
task clean-port-forwards    # Stop all port-forwards
task prepare-e2e            # Setup install + shared e2e prerequisites
task test-e2e              # Run e2e tests (includes port-forwards)
task test-e2e-quickstart-helm
task test-e2e-quickstart-manifest
task clean-cluster          # Delete the configured E2E cluster
```
