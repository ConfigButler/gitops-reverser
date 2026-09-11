# k3s audit file placement

This note is a small k3s-oriented example for placing the audit policy and webhook kubeconfig on
control-plane nodes.

Every control-plane node needs the audit files locally on disk:

```bash
sudo mkdir -p /etc/rancher/k3s/audit
```

Copy in:

- [`../../../test/e2e/cluster/audit/policy.yaml`](../../../test/e2e/cluster/audit/policy.yaml)
- your rendered `webhook-config.yaml`, using `/audit-webhook/default` for the default provider

Then update `/etc/rancher/k3s/config.yaml`:

```yaml
kube-apiserver-arg:
  - "audit-policy-file=/etc/rancher/k3s/audit/policy.yaml"
  - "audit-webhook-config-file=/etc/rancher/k3s/audit/webhook-config.yaml"
  - "audit-webhook-mode=batch"
  - "audit-webhook-version=audit.k8s.io/v1"
  - "audit-webhook-batch-max-wait=1s"
  - "audit-webhook-batch-max-size=100"
```

Restart k3s on each control-plane node, one node at a time:

```bash
sudo systemctl restart k3s
sudo k3s kubectl get nodes
```

Verify a live write using the [attribution guide](../../attribution-setup-guide.md#3-verify-a-live-change).
See [connectivity and TLS](../../facts/audit-webhook-api-server-connectivity.md) for other network layouts.
