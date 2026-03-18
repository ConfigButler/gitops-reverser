# E2E Cluster (k3d) with Audit Webhook

This directory contains the remaining e2e cluster assets after the Kind-specific files were removed.
The e2e environment now uses k3d with kube-apiserver audit webhook flags configured at cluster create time.
By default, the cluster bootstrap also disables packaged k3s Traefik and k3s ServiceLB.

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
- `max-requests-inflight=800` (override with `KUBE_APISERVER_MAX_REQUESTS_INFLIGHT`)
- `max-mutating-requests-inflight=400` (override with `KUBE_APISERVER_MAX_MUTATING_REQUESTS_INFLIGHT`)

It also disables these packaged k3s components by default:

- `traefik`
- `servicelb`

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

Prepare the talk/demo repo and leave it in place:

```bash
make test-e2e-demo
```

This target now uses the same suite-driven repo setup as the normal e2e flow, so each run gets a fresh repo
unless you explicitly pass `REPO_NAME=...`. It seeds that repo from the `vote` namespace plus supporting
cluster-scoped objects, and intentionally keeps the resulting Kubernetes resources and repo state for a live
walkthrough.

Before the suite runs, `make test-e2e-demo` also executes a demo-only prep step that validates the local
cloudflared and pull-secret manifests, installs the quiz CRDs, waits for them, and then applies
`test/e2e/setup/demo-only`.

## Verification

Confirm cluster is up:

```bash
kubectl get nodes
```

If you change the k3s disable flags or kube-apiserver inflight env vars in `start-cluster.sh`, recreate the k3d
cluster for them to take effect.

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
