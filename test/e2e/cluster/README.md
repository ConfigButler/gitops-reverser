# E2E Cluster (k3d) with Audit Webhook

This directory contains the remaining e2e cluster assets after the Kind-specific files were removed.
The e2e environment now uses k3d with kube-apiserver audit webhook flags configured at cluster create time.

## Current Files

- [`start-cluster.sh`](start-cluster.sh): creates/reuses a k3d cluster and configures kubeconfig for devcontainer usage
- [`audit/policy.yaml`](audit/policy.yaml): Kubernetes audit policy used by kube-apiserver
- [`audit/webhook-config.yaml`](audit/webhook-config.yaml): webhook target used by kube-apiserver audit backend

## How It Works

`start-cluster.sh` mounts the local audit directory into the k3d server node and sets:

- `audit-policy-file=/etc/kubernetes/audit/policy.yaml`
- `audit-webhook-config-file=/etc/kubernetes/audit/webhook-config.yaml`
- `audit-webhook-batch-max-wait=1s`
- `audit-webhook-batch-max-size=10`

The webhook URL in [`audit/webhook-config.yaml`](audit/webhook-config.yaml) targets:

`https://10.43.200.200:9444/audit-webhook/kind-e2e`

Notes:
- `insecure-skip-tls-verify: true` is intentional for local e2e
- `kind-e2e` in the path is the cluster ID label used by tests/metrics

## Usage

Run the full e2e suite:

```bash
make test-e2e
```

Run quickstart e2e variants:

```bash
make test-e2e-quickstart-manifest
make test-e2e-quickstart-helm
```

## Verification

Confirm cluster is up:

```bash
kubectl get nodes
```

Confirm audit files are mounted in the k3d server node:

```bash
docker exec k3d-gitops-reverser-test-e2e-server-0 ls -l /etc/kubernetes/audit
```

Confirm audit metrics are incrementing:

```promql
gitopsreverser_audit_events_received_total
```

## Troubleshooting

`failed to stat file/directory ... volume mount ...`: this can appear in Docker-outside-of-Docker setups when the host path is valid for the Docker daemon but not visible inside the devcontainer. `start-cluster.sh` includes compatibility symlink logic for this.

No audit events:

1. Check webhook config in the mounted directory.
2. Check the operator service and deployment in `gitops-reverser`.
3. Check controller logs for `audit-handler` entries.

## Reference

- [Kubernetes Audit Documentation](https://kubernetes.io/docs/tasks/debug/debug-cluster/audit/)
- [Audit Handler Implementation](../../../internal/webhook/audit_handler.go)
