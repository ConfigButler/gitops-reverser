# GitOps Reverser Helm Chart

GitOps Reverser enables synchronization from Kubernetes to one or more Git repositories.
This Helm chart provides a production-ready single-pod deployment.

## Quick Start

The official end-to-end quickstart lives in the repository root README:

- [`README.md`](../../README.md)

Use that guide for cert-manager, Valkey, Helm install, kube-apiserver audit configuration, Git credentials, starter
`GitProvider`/`GitTarget`/`WatchRule` resources, and first-commit verification.

This chart README stays focused on chart-specific installation, configuration, and operations.

For the chart's optional starter `quickstart` block, see [`docs/configuration.md`](../../docs/configuration.md).

## Features

- ✅ **Two-way Git synchronization**: Push Kubernetes changes back to Git repositories
- ✅ **Single-pod stability**: 1 replica by default while multi-pod support is in progress
- ✅ **Automatic CRD installation**: GitProvider, GitTarget, WatchRule, and ClusterWatchRule CRDs installed automatically
- ✅ **Webhook support**: Watch all Kubernetes resources for changes
- ✅ **Production-ready**: Pod disruption budgets, anti-affinity, and resource limits
- ✅ **Certificate management**: Automatic TLS via cert-manager
- ✅ **Prometheus metrics**: Built-in monitoring support

## Prerequisites

- Kubernetes 1.28+
- Helm 3.8+
- cert-manager 1.13+ (for webhook TLS certificates)
- Valkey or Redis reachable from the controller, with auth enabled (`queue.redis.auth.existingSecret`)

## Installation

### From OCI Registry (Recommended)

Install the latest version:

```bash
helm install gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser \
  --create-namespace
```

After installation, print the chart's post-install instructions with:

```bash
helm get notes gitops-reverser --namespace gitops-reverser
```

Install a specific version:

```bash
helm install gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --version 0.3.0 \
  --namespace gitops-reverser \
  --create-namespace
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
- **Single-pod operation**: minimal moving parts while HA work is deferred
- **Single Service topology**: admission, audit, and metrics on one Service

## Configuration

### Common Configuration Examples

#### Minimal (Testing/Development)

Single replica:

```yaml
# minimal-values.yaml
replicaCount: 1
podDisruptionBudget:
  enabled: false
affinity: {}
```

```bash
helm install gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser \
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
| `servers.admission.tls.secretNameOverride` | Override Secret name for admission TLS cert/key | `<release>-admission-server-cert` |
| `servers.audit.port` | Audit container port | `9444` |
| `servers.audit.tls.enabled` | Serve audit ingress with TLS | `true` |
| `servers.audit.maxRequestBodyBytes` | Max accepted audit request size | `10485760` |
| `servers.audit.timeouts.read` | Audit-server read timeout | `15s` |
| `servers.audit.timeouts.write` | Audit-server write timeout | `30s` |
| `servers.audit.timeouts.idle` | Audit-server idle timeout | `60s` |
| `servers.audit.tls.secretNameOverride` | Override Secret name for audit TLS cert/key | `<release>-audit-server-cert` |
| `auditService.type` | Service type for the dedicated audit Service | `NodePort` |
| `auditService.nodePort` | Fixed NodePort for the audit Service when `auditService.type=NodePort` | `30444` |
| `auditService.clusterIP` | Optional fixed ClusterIP for the dedicated audit Service | `""` |
| `quickstart.enabled` | Create starter `GitProvider`, `GitTarget`, and `WatchRule` resources | `false` |
| `quickstart.namespace` | Namespace for the starter quickstart resources | `default` |
| `quickstart.gitProvider.url` | Repository URL used by the starter `GitProvider` | `""` |
| `quickstart.gitProvider.secretRef.name` | Existing Secret name used by the starter `GitProvider` | `git-creds` |
| `quickstart.gitTarget.path` | Repository path used by the starter `GitTarget` | `live-cluster` |
| `quickstart.watchRule.rules` | Rules used by the starter `WatchRule` | `configmaps create/update/delete` |
| `queue.redis.addr` | Redis endpoint (`host:port`) for required durable audit queueing | `valkey:6379` |
| `queue.redis.auth.existingSecret` | Name of a pre-created Secret holding the Redis password | `valkey-auth` |
| `queue.redis.auth.existingSecretKey` | Key within the Secret that holds the password | `password` |
| `queue.redis.auth.username` | Optional Redis ACL username | `""` |
| `queue.redis.stream` | Redis stream name for audit events | `gitopsreverser.audit.events.v1` |
| `queue.redis.maxLen` | Approximate stream max length (`0` disables trim) | `0` |
| `queue.redis.tls.enabled` | Enable TLS for Redis connection | `false` |
| `servers.metrics.bindAddress` | Metrics listener bind address | `:8080` |
| `servers.metrics.tls.enabled` | Serve metrics with TLS | `false` |
| `servers.metrics.tls.certPath` | Metrics TLS certificate mount path | `/tmp/k8s-metrics-server/metrics-server-certs` |
| `servers.metrics.tls.secretNameOverride` | Override Secret name for metrics TLS cert/key | `<release>-metrics-server-cert` |
| `service.clusterIP` | Optional fixed ClusterIP for single controller Service | `""` |
| `service.ports.admission` | Service port for admission webhook | `9443` |
| `service.ports.audit` | Service port for audit ingress | `9444` |
| `service.ports.metrics` | Service port for metrics | `8080` |
| `certificates.certManager.enabled` | Use cert-manager for certificates | `true` |
| `certificates.audit.enabled` | Let the chart manage audit TLS Secrets via cert-manager | `true` |
| `certificates.audit.rootCA.secretNameOverride` | Override Secret name for the audit root CA | `<release>-audit-root-ca` |
| `certificates.audit.client.duration` | Lifetime of the kube-apiserver audit client cert; long by default to avoid repeated manual kube-apiserver reconfiguration | `87600h` |
| `certificates.audit.client.renewBefore` | Renew the kube-apiserver audit client cert before expiry; shorter values increase control-plane maintenance frequency | `720h` |
| `certificates.audit.client.secretNameOverride` | Override Secret name for the kube-apiserver audit client cert | `<release>-audit-client-cert` |
| `auditKubeconfig.insecureSkipTLSVerify` | Render Helm notes with `insecure-skip-tls-verify: true` for the generated audit kubeconfig example | `false` |
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

- **`gitproviders.configbutler.ai`** - Git repository connectivity and credentials
- **`gittargets.configbutler.ai`** - Branch/path and optional encryption configuration
- **`watchrules.configbutler.ai`** - Namespaced watch rules
- **`clusterwatchrules.configbutler.ai`** - Cluster-scoped watch rules

### CRD Lifecycle

| Operation | Behavior |
|-----------|----------|
| `helm install` | ✅ CRDs installed automatically |
| `helm upgrade` | ✅ CRDs upgraded automatically |
| `helm uninstall` | ⚠️ CRDs NOT deleted (prevents data loss) |

To manually remove CRDs after uninstallation:

```bash
kubectl delete crd gitproviders.configbutler.ai gittargets.configbutler.ai watchrules.configbutler.ai clusterwatchrules.configbutler.ai
```

> ⚠️ **Warning**: Deleting CRDs will also delete all custom resources of those types!

## Verification

### Verify Installation

```bash
# Check pods (should see 1 replica)
kubectl get pods -n gitops-reverser

# Check CRDs
kubectl get crd | grep configbutler

# Check webhook
kubectl get validatingwebhookconfiguration -l app.kubernetes.io/name=gitops-reverser

```

For first-run GitOps Reverser usage, follow the root quickstart instead of duplicating those steps here.

### View Logs

```bash
# All pods
kubectl logs -n gitops-reverser -l app.kubernetes.io/name=gitops-reverser -f

```

### Access Metrics

```bash
kubectl port-forward -n gitops-reverser svc/gitops-reverser 8080:8080
curl http://localhost:8080/metrics
# If metrics TLS is enabled:
# curl -k https://localhost:8080/metrics
```

## Upgrading

### Standard Upgrade

```bash
helm upgrade gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser
```

### Upgrade with New Values

```bash
helm upgrade gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser \
  --values new-values.yaml
```

### Migration from Previous Versions

If upgrading from earlier chart versions:

- Single-replica is the default during the current simplified topology phase
- Leader election remains enabled for safe future multi-pod evolution
- Health probe port changed to 8081
- Certificate secret names are auto-generated

## Uninstallation

```bash
# Uninstall chart
helm uninstall gitops-reverser --namespace gitops-reverser

# Delete namespace (optional)
kubectl delete namespace gitops-reverser

# Delete CRDs (optional, but removes all custom resources)
kubectl delete crd gitproviders.configbutler.ai gittargets.configbutler.ai watchrules.configbutler.ai clusterwatchrules.configbutler.ai
```

## Troubleshooting

### Webhook Certificate Issues

Check certificate status:

```bash
kubectl get certificate -n gitops-reverser
kubectl describe certificate gitops-reverser-admission-server-cert -n gitops-reverser
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
  --namespace gitops-reverser \
  --set replicaCount=1 \
  --set affinity=null
```

### View Controller Events

```bash
kubectl get events -n gitops-reverser --sort-by='.lastTimestamp'
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
  -n gitops-reverser
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
- **Documentation**: [Project README and quickstart](https://github.com/ConfigButler/gitops-reverser)
- **Contributing**: See [CONTRIBUTING.md](https://github.com/ConfigButler/gitops-reverser/blob/main/CONTRIBUTING.md)

## License

This chart is licensed under the same license as the GitOps Reverser project.
