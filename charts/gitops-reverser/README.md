# GitOps Reverser Helm Chart

This Helm chart deploys the GitOps Reverser application with comprehensive webhook support, security configurations, and monitoring capabilities.

## Features

- **Comprehensive Webhook Support**: Both mutating and validating webhooks with configurable rules
- **Security-First Design**: Pod security contexts, network policies, and RBAC
- **TLS Certificate Management**: Automated certificate management with cert-manager
- **Monitoring Integration**: Prometheus ServiceMonitor and metrics endpoints
- **High Availability**: Pod disruption budgets and leader election
- **Production Ready**: Resource limits, health checks, and proper logging

## Prerequisites

- Kubernetes 1.19+
- Helm 3.0+
- cert-manager (if using TLS certificates)
- Prometheus Operator (if using ServiceMonitor)

## Installation

### Basic Installation

```bash
helm install gitops-reverser ./charts/gitops-reverser
```

### With Custom Values

```bash
helm install gitops-reverser ./charts/gitops-reverser -f my-values.yaml
```

## Configuration

### Webhook Configuration

The chart supports both mutating and validating webhooks:

```yaml
webhook:
  enabled: true
  mutating:
    enabled: true
    failurePolicy: Fail
    rules:
      - operations: ["CREATE", "UPDATE"]
        apiGroups: [""]
        apiVersions: ["v1"]
        resources: ["pods", "configmaps", "secrets"]
  validating:
    enabled: true
    failurePolicy: Ignore
    rules:
      - operations: ["CREATE", "UPDATE", "DELETE"]
        apiGroups: ["*"]
        apiVersions: ["*"]
        resources: ["*"]
```

### Security Configuration

Enhanced security with proper contexts and policies:

```yaml
podSecurityContext:
  runAsNonRoot: true
  runAsUser: 65532
  runAsGroup: 65532
  fsGroup: 65532
  seccompProfile:
    type: RuntimeDefault

securityContext:
  allowPrivilegeEscalation: false
  capabilities:
    drop:
    - ALL
  readOnlyRootFilesystem: true
  runAsNonRoot: true
  runAsUser: 65532

networkPolicy:
  enabled: true
```

### TLS Certificate Management

Automated certificate management with cert-manager:

```yaml
certificates:
  certManager:
    enabled: true
    issuer:
      name: selfsigned-issuer
      kind: Issuer
      create: true
    webhook:
      secretName: webhook-server-cert
    metrics:
      secretName: metrics-server-cert
```

### Monitoring

Prometheus integration with ServiceMonitor:

```yaml
monitoring:
  serviceMonitor:
    enabled: true
    interval: 30s
    scrapeTimeout: 10s
    path: /metrics
    port: https
```

### Resource Management

Production-ready resource limits:

```yaml
resources:
  limits:
    cpu: 500m
    memory: 128Mi
  requests:
    cpu: 10m
    memory: 64Mi

podDisruptionBudget:
  enabled: true
  minAvailable: 1
```

## Values Reference

| Parameter | Description | Default |
|-----------|-------------|---------|
| `replicaCount` | Number of replicas | `1` |
| `image.repository` | Container image repository | `YOUR_IMAGE_REPOSITORY` |
| `image.tag` | Container image tag | `""` (uses appVersion) |
| `image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `webhook.enabled` | Enable webhook functionality | `true` |
| `webhook.mutating.enabled` | Enable mutating webhook | `true` |
| `webhook.validating.enabled` | Enable validating webhook | `true` |
| `certificates.certManager.enabled` | Use cert-manager for certificates | `true` |
| `rbac.create` | Create RBAC resources | `true` |
| `serviceAccount.create` | Create service account | `true` |
| `monitoring.serviceMonitor.enabled` | Create ServiceMonitor | `false` |
| `networkPolicy.enabled` | Create NetworkPolicy | `false` |
| `podDisruptionBudget.enabled` | Create PodDisruptionBudget | `false` |

## Webhook Rules Configuration

### Mutating Webhook Rules

The mutating webhook can be configured to intercept specific resources:

```yaml
webhook:
  mutating:
    rules:
      - operations: ["CREATE", "UPDATE"]
        apiGroups: [""]
        apiVersions: ["v1"]
        resources: ["pods", "configmaps", "secrets"]
      - operations: ["CREATE", "UPDATE"]
        apiGroups: ["apps"]
        apiVersions: ["v1"]
        resources: ["deployments", "replicasets", "daemonsets", "statefulsets"]
    namespaceSelector:
      matchExpressions:
      - key: name
        operator: NotIn
        values: ["kube-system", "kube-public", "kube-node-lease"]
```

### Validating Webhook Rules

The validating webhook watches all resources by default but can be customized:

```yaml
webhook:
  validating:
    rules:
      - operations: ["CREATE", "UPDATE", "DELETE"]
        apiGroups: ["*"]
        apiVersions: ["*"]
        resources: ["*"]
    failurePolicy: Ignore  # Don't block operations if webhook fails
```

## Security Considerations

1. **Pod Security**: The chart uses non-root user (65532) and drops all capabilities
2. **Network Policies**: Optional network policies restrict ingress/egress traffic
3. **RBAC**: Minimal required permissions with separate roles for different functions
4. **TLS**: All webhook and metrics endpoints use TLS encryption
5. **Read-only Filesystem**: Container filesystem is read-only for security

## Troubleshooting

### Common Issues

1. **Certificate Issues**: Ensure cert-manager is installed and running
2. **Webhook Failures**: Check webhook service and certificate configuration
3. **RBAC Errors**: Verify service account has required permissions
4. **Network Connectivity**: Check network policies if enabled

### Debug Commands

```bash
# Check webhook configuration
kubectl get mutatingwebhookconfigurations
kubectl get validatingwebhookconfigurations

# Check certificates
kubectl get certificates -n <namespace>
kubectl describe certificate <cert-name> -n <namespace>

# Check service and endpoints
kubectl get svc -n <namespace>
kubectl get endpoints -n <namespace>

# Check logs
kubectl logs -n <namespace> deployment/gitops-reverser
```

## Upgrading

When upgrading the chart, review the changelog for breaking changes. Always test in a non-production environment first.

```bash
helm upgrade gitops-reverser ./charts/gitops-reverser
```

## Contributing

Please refer to the main project's CONTRIBUTING.md for guidelines on contributing to this chart.

## License

This chart is licensed under the same license as the GitOps Reverser project.