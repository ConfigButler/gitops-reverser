# GitOps Reverser Helm Chart

GitOps Reverser enables synchronization from Kubernetes to one or more Git repositories. This Helm chart provides a production-ready deployment with High Availability (HA) by default.

## Quick Start

```bash
# 1. Install cert-manager (if not already installed)
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.19.1/cert-manager.yaml

# 2. Wait for cert-manager to be ready
kubectl wait --for=condition=ready pod -l app.kubernetes.io/instance=cert-manager -n cert-manager --timeout=300s

# 3. Install GitOps Reverser
helm install gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser-system \
  --create-namespace

# 4. Verify installation
kubectl get pods -n gitops-reverser-system
```

That's it! The controller is now running and ready to synchronize your Kubernetes resources with Git.

## Features

- ✅ **Two-way Git synchronization**: Push Kubernetes changes back to Git repositories
- ✅ **High Availability**: 2 replicas with leader election by default
- ✅ **Automatic CRD installation**: GitRepoConfig and WatchRule CRDs installed automatically
- ✅ **Webhook support**: Watch all Kubernetes resources for changes
- ✅ **Production-ready**: Pod disruption budgets, anti-affinity, and resource limits
- ✅ **Certificate management**: Automatic TLS via cert-manager
- ✅ **Prometheus metrics**: Built-in monitoring support

## Prerequisites

- Kubernetes 1.28+
- Helm 3.8+
- cert-manager 1.13+ (for webhook TLS certificates)

## Installation

### From OCI Registry (Recommended)

Install the latest version:

```bash
helm install gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser-system \
  --create-namespace
```

Install a specific version:

```bash
helm install gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --version 0.3.0 \
  --namespace gitops-reverser-system \
  --create-namespace
```

### From GitHub Releases

You can also install using the single YAML manifest:

```bash
kubectl apply -f https://github.com/ConfigButler/gitops-reverser/releases/latest/download/install.yaml
```

## Architecture

### Deployment Topology

The chart deploys 1 replica by default:

```
┌─────────────────────────────────────────┐
│         Kubernetes API Server           │
└──────────────┬──────────────────────────┘
               │
               │ webhook + audit + metrics requests
               ▼
┌──────────────────────────────────────────┐
│        gitops-reverser (Service)         │
│  Ports: admission(9443), audit(9444), metrics(8080) |
└──────────────┬───────────────────────────┘
               │
               ▼
        ┌─────────────┐
        │ Pod 1       │
        │ Controller  │
        │ Active      │
        └─────────────┘
```

**Key Features:**
- **Single-pod operation**: Minimal moving parts while HA work is deferred
- **Single Service topology**: admission, audit, and metrics on one Service

## Configuration

### Common Configuration Examples

#### Minimal (Testing/Development)

Single replica:

```yaml
# minimal-values.yaml
replicaCount: 1
controllerManager:
podDisruptionBudget:
  enabled: false
affinity: {}
```

```bash
helm install gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser-system \
  --create-namespace \
  --values minimal-values.yaml
```

#### Production (Recommended)

Hardened single-replica deployment:

```yaml
# production-values.yaml
replicaCount: 1

podDisruptionBudget:
  enabled: true
  minAvailable: 1

resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    cpu: 1000m
    memory: 512Mi

monitoring:
  serviceMonitor:
    enabled: true
    interval: 30s

nodeSelector:
  node-role.kubernetes.io/worker: ""
```

#### Custom Webhook Configuration

Stricter validation and namespace filtering:

```yaml
# webhook-values.yaml
webhook:
  validating:
    failurePolicy: Fail  # Reject requests if webhook fails
    namespaceSelector:
      matchExpressions:
        - key: gitops-reverser/watch
          operator: In
          values: ["enabled"]
```

### Configuration Reference

| Parameter | Description | Default |
|-----------|-------------|---------|
| `replicaCount` | Number of controller replicas (can't be higher than 1 for now, sorry) | `1` |
| `image.repository` | Container image repository | `ghcr.io/configbutler/gitops-reverser` |
| `webhook.validating.failurePolicy` | Webhook failure policy (Ignore/Fail) | `Ignore` |
| `servers.admission.tls.enabled` | Serve admission webhook with TLS (disable only for local/testing) | `true` |
| `servers.admission.tls.secretName` | Secret name for admission TLS cert/key | `<release>-admission-server-cert` |
| `servers.audit.port` | Audit container port | `9444` |
| `servers.audit.tls.enabled` | Serve audit ingress with TLS | `true` |
| `servers.audit.maxRequestBodyBytes` | Max accepted audit request size | `10485760` |
| `servers.audit.timeouts.read` | Audit-server read timeout | `15s` |
| `servers.audit.timeouts.write` | Audit-server write timeout | `30s` |
| `servers.audit.timeouts.idle` | Audit-server idle timeout | `60s` |
| `servers.audit.tls.secretName` | Secret name for audit TLS cert/key | `<release>-audit-server-cert` |
| `servers.metrics.bindAddress` | Metrics listener bind address | `:8080` |
| `servers.metrics.tls.enabled` | Serve metrics with TLS | `false` |
| `servers.metrics.tls.certPath` | Metrics TLS certificate mount path | `/tmp/k8s-metrics-server/metrics-server-certs` |
| `servers.metrics.tls.secretName` | Secret name for metrics TLS cert/key | `<release>-metrics-server-cert` |
| `service.clusterIP` | Optional fixed ClusterIP for single controller Service | `""` |
| `service.ports.admission` | Service port for admission webhook | `9443` |
| `service.ports.audit` | Service port for audit ingress | `9444` |
| `service.ports.metrics` | Service port for metrics | `8080` |
| `certificates.certManager.enabled` | Use cert-manager for certificates | `true` |
| `podDisruptionBudget.enabled` | Enable PodDisruptionBudget | `true` |
| `resources.requests.cpu` | CPU request | `10m` |
| `resources.requests.memory` | Memory request | `64Mi` |
| `resources.limits.cpu` | CPU limit | `500m` |
| `resources.limits.memory` | Memory limit | `128Mi` |

See [`values.yaml`](values.yaml) for complete configuration options.

### Audit Webhook URL Contract

Source clusters must target:

`https://<service>:9444/audit-webhook/<cluster-id>`

The bare path `/audit-webhook` is rejected. Use a non-empty cluster ID segment.

## Custom Resource Definitions (CRDs)

This chart automatically manages the following CRDs:

- **`gitrepoconfigs.configbutler.ai`** - Git repository configurations for synchronization
- **`watchrules.configbutler.ai`** - Rules for watching Kubernetes resources

### CRD Lifecycle

| Operation | Behavior |
|-----------|----------|
| `helm install` | ✅ CRDs installed automatically |
| `helm upgrade` | ✅ CRDs upgraded automatically |
| `helm uninstall` | ⚠️ CRDs NOT deleted (prevents data loss) |

To manually remove CRDs after uninstallation:

```bash
kubectl delete crd gitrepoconfigs.configbutler.ai watchrules.configbutler.ai
```

> ⚠️ **Warning**: Deleting CRDs will also delete all custom resources of those types!

## Verification & Usage

### Verify Installation

```bash
# Check pods (should see 1 replica)
kubectl get pods -n gitops-reverser-system

# Check CRDs
kubectl get crd | grep configbutler

# Check webhook
kubectl get validatingwebhookconfiguration -l app.kubernetes.io/name=gitops-reverser

```

### View Logs

```bash
# All pods
kubectl logs -n gitops-reverser-system -l app.kubernetes.io/name=gitops-reverser -f

```

### Access Metrics

```bash
kubectl port-forward -n gitops-reverser-system svc/gitops-reverser 8080:8080
curl http://localhost:8080/metrics
# If metrics TLS is enabled:
# curl -k https://localhost:8080/metrics
```

## Upgrading

### Standard Upgrade

```bash
helm upgrade gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser-system
```

### Upgrade with New Values

```bash
helm upgrade gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser-system \
  --values new-values.yaml
```

### Migration from Previous Versions

If upgrading from earlier chart versions:

- Single-replica is the default during the current simplified topology phase
- Leader election now enabled by default (required for HA)
- Health probe port changed to 8081
- Certificate secret names are auto-generated

## Uninstallation

```bash
# Uninstall chart
helm uninstall gitops-reverser --namespace gitops-reverser-system

# Delete namespace (optional)
kubectl delete namespace gitops-reverser-system

# Delete CRDs (optional, but removes all custom resources)
kubectl delete crd gitrepoconfigs.configbutler.ai watchrules.configbutler.ai
```

## Troubleshooting

### Webhook Certificate Issues

Check certificate status:

```bash
kubectl get certificate -n gitops-reverser-system
kubectl describe certificate gitops-reverser-admission-server-cert -n gitops-reverser-system
```

If cert-manager is not working:

```bash
# Check cert-manager logs
kubectl logs -n cert-manager -l app=cert-manager -f

# Restart cert-manager
kubectl rollout restart deployment cert-manager -n cert-manager
```

### Pods Not Scheduling

If pods are pending due to anti-affinity rules:

```bash
# Check node count
kubectl get nodes

# If you have only 1 node, keep a single replica or disable affinity
helm upgrade gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser-system \
  --set replicaCount=1 \
  --set affinity=null
```

### Git Authentication Issues

Ensure your Git credentials secret exists:

```bash
# For HTTPS with token
kubectl create secret generic git-credentials \
  --from-literal=username=git \
  --from-literal=password=YOUR_TOKEN \
  -n gitops-reverser-system

# For SSH
kubectl create secret generic git-credentials \
  --from-file=ssh-privatekey=~/.ssh/id_rsa \
  -n gitops-reverser-system
```

### View Controller Events

```bash
kubectl get events -n gitops-reverser-system --sort-by='.lastTimestamp'
```

## Advanced Configuration

### Using External Certificate Provider

If not using cert-manager:

```yaml
certificates:
  certManager:
    enabled: false

webhook:
  caBundle: <base64-encoded-ca-bundle>
```

Create certificate secret manually:

```bash
kubectl create secret tls gitops-reverser-admission-server-cert \
  --cert=path/to/tls.crt \
  --key=path/to/tls.key \
  -n gitops-reverser-system
```

### Custom Resource Limits

For clusters with high resource usage:

```yaml
resources:
  requests:
    cpu: 200m
    memory: 256Mi
  limits:
    cpu: 2000m
    memory: 1Gi
```

## Release Strategy

New versions are automatically released via GitHub Actions:

1. Push to `main` branch triggers release-please
2. Docker images built for `linux/amd64` and `linux/arm64`
3. Helm chart packaged and pushed to `ghcr.io`
4. Release notes include installation instructions

Check available versions:

```bash
# View latest release
helm show chart oci://ghcr.io/configbutler/charts/gitops-reverser

# List releases
curl -s https://api.github.com/repos/ConfigButler/gitops-reverser/releases | jq -r '.[].tag_name'
```

## Support & Contributing

- **Issues**: [GitHub Issues](https://github.com/ConfigButler/gitops-reverser/issues)
- **Documentation**: [Project README](https://github.com/ConfigButler/gitops-reverser)
- **Contributing**: See [CONTRIBUTING.md](https://github.com/ConfigButler/gitops-reverser/blob/main/CONTRIBUTING.md)

## License

This chart is licensed under the same license as the GitOps Reverser project.
