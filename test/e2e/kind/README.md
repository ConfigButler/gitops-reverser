# Kind Cluster with Audit Webhook Support

This directory contains configuration files to set up a Kind cluster with Kubernetes audit webhook support for e2e testing.

## Overview

The gitops-reverser operator exposes an experimental audit webhook endpoint at `/audit-webhook/{clusterID}` that receives audit events from the Kubernetes API server. This setup configures Kind to send audit events to this endpoint for testing and metrics collection.

## Files

- **[`cluster.yaml`](cluster.yaml)**: Kind cluster configuration with audit webhook support
- **[`audit/policy.yaml`](audit/policy.yaml)**: Kubernetes audit policy (filters noise, captures intent)
- **[`audit/webhook-config.yaml`](audit/webhook-config.yaml)**: Audit webhook configuration (points to operator service)

## Configuration Details

### Cluster Configuration

The [`cluster.yaml`](cluster.yaml:1) mounts the audit policy and webhook configuration files into the Kind control plane node and configures the kube-apiserver with:

- `audit-policy-file`: Filters which events are audited
- `audit-webhook-config-file`: Configures where to send audit events
- `audit-webhook-batch-max-wait`: Maximum wait time before sending a batch (5s)
- `audit-webhook-batch-max-size`: Maximum events in a batch (100)

### Webhook Configuration

The [`webhook-config.yaml`](audit/webhook-config.yaml:1) configures the kube-apiserver to send audit events to:

```
https://10.96.200.200:443/audit-webhook/kind-e2e
```

**Important**: Uses `insecure-skip-tls-verify: true` because the webhook service uses a self-signed certificate from cert-manager.

### Audit Policy

The [`policy.yaml`](audit/policy.yaml:1) is copied from [`docs/audit-setup/cluster/audit/policy.yaml`](../../../docs/audit-setup/cluster/audit/policy.yaml:1) and filters:

- **Noise**: Drops status updates, heartbeats, ephemeral checks
- **Security**: Filters secrets and sensitive operations
- **Intent**: Captures create/update/patch/delete operations on resources

## Usage

### Creating a Cluster

The Makefile automatically uses this configuration when creating Kind clusters:

```bash
make setup-cluster
```

This will create a cluster named `gitops-reverser-test-e2e` with audit webhook support.

### Running E2E Tests

The e2e test suite includes a test that verifies audit events are received:

```bash
make test-e2e
```

Look for the test: **"should receive audit webhook events from kube-apiserver"**

### Quick Verification

Use the provided verification script to check if audit webhook is properly configured:

```bash
./test/e2e/kind/verify-audit.sh
```

This script checks:
- Audit configuration files are mounted in the control plane
- kube-apiserver is configured with audit parameters
- Operator and webhook service are deployed
- Basic audit logging is working

### Verifying Audit Events

After deploying the operator, you can verify audit events are being received:

1. **Check metrics** (via Prometheus at `http://localhost:19090`):
   ```promql
   gitopsreverser_audit_events_received_total
   ```

2. **Check controller logs**:
   ```bash
   kubectl logs -n gitops-reverser-system -l control-plane=controller-manager --tail=100 | grep "audit"
   ```

3. **Create a test resource** to trigger audit events:
   ```bash
   kubectl create configmap audit-test -n gitops-reverser-system --from-literal=test=value
   ```

## Metrics

The audit webhook tracks metrics with labels:

- `gvr`: Group/Version/Resource (e.g., `apps/v1/deployments`)
- `action`: Verb (e.g., `create`, `update`, `delete`)
- `user`: Username who performed the action
- `processed`: Whether the event was processed (filters status updates)

## Troubleshooting

### Audit Events Not Received

1. **Check kube-apiserver logs**:
   ```bash
   docker exec gitops-reverser-test-e2e-control-plane cat /var/log/kubernetes/kube-apiserver-audit.log
   ```

2. **Verify audit webhook service exists**:
   ```bash
   kubectl get svc -n gitops-reverser-system gitops-reverser-audit-webhook-service
   ```

3. **Check if kube-apiserver can reach the webhook**:
   ```bash
   kubectl logs -n kube-system -l component=kube-apiserver
   ```

### File Mounting Issues

If audit configuration files aren't found, ensure paths in [`cluster.yaml`](cluster.yaml:7-11) are correct relative to the project root:

```yaml
hostPath: ./test/e2e/kind/audit/policy.yaml
hostPath: ./test/e2e/kind/audit/webhook-config.yaml
```

## Differences from K3s Setup

The K3s setup in [`docs/audit-setup/cluster/`](../../../docs/audit-setup/cluster/) uses:

- Fixed ClusterIP: `10.43.200.200` (K3s default)
- Service name: Generic reference
- Config location: `/etc/rancher/k3s/audit/`

The Kind setup uses:

- Fixed ClusterIP: `10.96.200.200`
- Path-based cluster identity: `/audit-webhook/kind-e2e`
- Config location: `/etc/kubernetes/` (Kind standard)

## References

- [Kubernetes Audit Documentation](https://kubernetes.io/docs/tasks/debug/debug-cluster/audit/)
- [Kind Extra Mounts](https://kind.sigs.k8s.io/docs/user/configuration/#extra-mounts)
- [Kubeadm Config Patches](https://kind.sigs.k8s.io/docs/user/configuration/#kubeadm-config-patches)
- [Audit Handler Implementation](../../../internal/webhook/audit_handler.go:1)
