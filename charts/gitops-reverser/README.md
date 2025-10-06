# GitOps Reverser Helm Chart

This Helm chart deploys the GitOps Reverser controller, which enables two-way synchronization between Kubernetes and Git repositories. The chart is production-ready with High Availability (HA) configuration by default.

## Features

- **Namespace Management**: Automatically creates namespace (can be disabled)
- **High Availability**: Runs 2 replicas by default with pod anti-affinity
- **Leader Election**: Automatic leader election for controller instances
- **Webhook Support**: Validating webhook for watching all Kubernetes resources
- **CRD Installation**: Automatically installs GitRepoConfig and WatchRule CRDs
- **Security**: Pod security standards compliant with secure defaults
- **Certificate Management**: Automatic TLS certificate management via cert-manager
- **Pod Disruption Budget**: Ensures availability during cluster maintenance

## Prerequisites

- Kubernetes 1.28+
- Helm 3.8+
- cert-manager 1.13+ (for webhook TLS certificates)

## Custom Resource Definitions (CRDs)

This chart automatically installs the following CRDs:

- `gitrepoconfigs.configbutler.ai` - Defines Git repository configurations for synchronization
- `watchrules.configbutler.ai` - Defines rules for watching Kubernetes resources

**Important CRD Notes:**

1. **Installation**: CRDs are installed automatically during `helm install`
2. **Upgrade**: CRDs are upgraded automatically during `helm upgrade`
3. **Deletion**: CRDs are **NOT** deleted during `helm uninstall` (Helm best practice to prevent data loss)
4. **Manual Deletion**: If you need to remove CRDs, delete them manually after uninstalling the chart

## Installation

### Install cert-manager (if not already installed)

```bash
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml
```

Wait for cert-manager to be ready:

```bash
kubectl wait --for=condition=ready pod -l app.kubernetes.io/instance=cert-manager -n cert-manager --timeout=300s
```

### Install from OCI Registry (Recommended)

```bash
helm install gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser-system \
  --create-namespace
```

### Install with Custom Values

```bash
helm install gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser-system \
  --create-namespace \
  --values custom-values.yaml
```

## Configuration

### Key Configuration Options

| Parameter | Description | Default |
|-----------|-------------|---------|
| `namespaceCreation.enabled` | Create namespace as part of installation | `true` |
| `namespaceCreation.labels` | Additional labels for namespace | `{}` |
| `namespaceCreation.annotations` | Additional annotations for namespace | `{}` |
| `replicaCount` | Number of controller replicas | `2` |
| `image.repository` | Container image repository | `ghcr.io/configbutler/gitops-reverser` |
| `image.tag` | Container image tag | Chart appVersion |
| `controllerManager.leaderElection` | Enable leader election | `true` |
| `webhook.enabled` | Enable validating webhook | `true` |
| `webhook.validating.failurePolicy` | Webhook failure policy | `Ignore` |
| `certificates.certManager.enabled` | Use cert-manager for certificates | `true` |
| `podDisruptionBudget.enabled` | Enable PodDisruptionBudget | `true` |
| `podDisruptionBudget.minAvailable` | Minimum available pods | `1` |
| `resources.requests.cpu` | CPU request | `10m` |
| `resources.requests.memory` | Memory request | `64Mi` |
| `resources.limits.cpu` | CPU limit | `500m` |
| `resources.limits.memory` | Memory limit | `128Mi` |

### Example: Minimal Installation

For testing environments where HA is not required:

```yaml
# minimal-values.yaml
replicaCount: 1
controllerManager:
  leaderElection: false
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

### Example: Production Configuration

```yaml
# production-values.yaml
replicaCount: 3

podDisruptionBudget:
  enabled: true
  minAvailable: 2

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

tolerations:
  - key: "dedicated"
    operator: "Equal"
    value: "gitops-reverser"
    effect: "NoSchedule"
```

### Example: Custom Webhook Configuration

```yaml
# webhook-values.yaml
webhook:
  validating:
    # Use Fail policy for stricter validation
    failurePolicy: Fail
    # Only watch specific namespaces
    namespaceSelector:
      matchExpressions:
        - key: gitops-reverser/watch
          operator: In
          values: ["enabled"]
```

## Usage

### Verify Installation

Check that the controller is running:

```bash
kubectl get pods -n gitops-reverser-system
```

Check the ValidatingWebhookConfiguration:

```bash
kubectl get validatingwebhookconfiguration -l app.kubernetes.io/name=gitops-reverser
```

### View Controller Logs

```bash
kubectl logs -n gitops-reverser-system -l app.kubernetes.io/name=gitops-reverser -f
```

### Access Metrics

The controller exposes Prometheus metrics on port 8080:

```bash
kubectl port-forward -n gitops-reverser-system svc/gitops-reverser-metrics-service 8080:8080
curl http://localhost:8080/metrics
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

## Uninstallation

```bash
helm uninstall gitops-reverser --namespace gitops-reverser-system
```

To also remove the namespace:

```bash
kubectl delete namespace gitops-reverser-system
```

**Note**: CRDs are not automatically deleted by Helm uninstall (by design). To remove them manually:

```bash
kubectl delete crd gitrepoconfigs.configbutler.ai watchrules.configbutler.ai
```

> ⚠️ **Warning**: Deleting CRDs will also delete all custom resources of those types!

## Troubleshooting

### Webhook Certificate Issues

If the webhook is not working, check certificate status:

```bash
kubectl get certificate -n gitops-reverser-system
kubectl describe certificate -n gitops-reverser-system
```

### Leader Election Issues

Check which pod is the leader:

```bash
kubectl get lease -n gitops-reverser-system
```

### Pod Anti-Affinity Issues

If pods are not scheduling, check node distribution:

```bash
kubectl get nodes --show-labels
kubectl describe pod -n gitops-reverser-system
```

## Advanced Configuration

### Using External Certificate Provider

If you don't want to use cert-manager:

```yaml
certificates:
  certManager:
    enabled: false

webhook:
  caBundle: <base64-encoded-ca-bundle>
```

Then manually create the certificate secret:

```bash
kubectl create secret tls webhook-server-cert \
  --cert=path/to/tls.crt \
  --key=path/to/tls.key \
  -n gitops-reverser-system
```

### Network Policies

Enable network policies for additional security:

```yaml
networkPolicy:
  enabled: true
  ingress:
    - from:
      - namespaceSelector: {}
      ports:
      - protocol: TCP
        port: 9443  # webhook port
  egress:
    - to:
      - namespaceSelector: {}
      ports:
      - protocol: TCP
        port: 443  # Kubernetes API
```

## Support

For issues and questions:
- GitHub Issues: https://github.com/configbutler/gitops-reverser/issues
- Documentation: https://github.com/configbutler/gitops-reverser

## License

This chart is licensed under the same license as the GitOps Reverser project.