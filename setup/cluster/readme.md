Every node that is running the control plane will need a copy of these files:

Please do create this folder:
sudo mkdir -p /etc/rancher/k3s/audit

Adjust the config file

sudo nano /etc/rancher/k3s/config.yaml
kube-apiserver-arg:
  - "audit-policy-file=/etc/rancher/k3s/audit/policy.yaml"
  - "audit-webhook-config-file=/etc/rancher/k3s/audit/webhook-config.yaml"
  - "audit-webhook-batch-max-wait=1s"   # Optional: Max wait before flushing batch
  - "audit-webhook-batch-max-size=100"  # Optional: Max events in one batch

Do the upgrade in order on your contorl plane nodes:

sudo systemctl restart k3s
# Wait 30 seconds
sudo k3s kubectl get nodes # Verify API is responding

