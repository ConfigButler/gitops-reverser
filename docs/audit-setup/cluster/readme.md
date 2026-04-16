# K3s Audit File Placement

This note is a small k3s-oriented example for placing the audit policy and webhook kubeconfig on
control-plane nodes.

Every control-plane node needs the audit files locally on disk:

```bash
sudo mkdir -p /etc/rancher/k3s/audit
```

Copy in:

- [`../../../test/e2e/cluster/audit/policy.yaml`](../../../test/e2e/cluster/audit/policy.yaml)
- your rendered `webhook-config.yaml`

Then update `/etc/rancher/k3s/config.yaml`:

```yaml
kube-apiserver-arg:
  - "audit-policy-file=/etc/rancher/k3s/audit/policy.yaml"
  - "audit-webhook-config-file=/etc/rancher/k3s/audit/webhook-config.yaml"
  - "audit-webhook-batch-max-wait=1s"
  - "audit-webhook-batch-max-size=100"
```

Restart k3s on each control-plane node, one node at a time:

```bash
sudo systemctl restart k3s
sudo k3s kubectl get nodes
```

For the broader connectivity and TLS tradeoffs, use
[`../../design/audit-webhook-api-server-connectivity.md`](../../design/audit-webhook-api-server-connectivity.md)
instead of this short placement note.
