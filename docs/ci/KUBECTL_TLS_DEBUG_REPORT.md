# kubectl/Kind Connectivity Debug Report (Devcontainer)

Date: 2026-02-13
Scope: Why `kubectl get nodes` failed inside devcontainer after Kind cluster creation, and what fixed it.

## Symptom

Inside devcontainer, after `make setup-cluster`:

```bash
kubectl get nodes
```

failed with connection errors to `host.docker.internal:<random-port>`.

## What We Observed

1. Kind cluster creation succeeded and reported healthy control plane.
2. Kubeconfig rewrite logic changed server endpoint from `127.0.0.1:<port>` to `host.docker.internal:<port>`.
3. Docker port publish for Kind control-plane showed loopback-only host bind:

```text
"6443/tcp":[{"HostIp":"127.0.0.1","HostPort":"<port>"}]
```

## Root Cause

The kubeconfig rewrite alone was not enough.

When Kind publishes API server on host loopback (`127.0.0.1`), that port is reachable from the host itself but not from another container via `host.docker.internal`.

So the devcontainer tried to connect to `host.docker.internal:<port>`, but host had that port bound only to loopback, resulting in connection refused.

## Final Fix Applied

### A) Devcontainer networking model

In `.devcontainer/devcontainer.json`:

- removed `--network=host`
- kept `--group-add=docker`
- added `--add-host=host.docker.internal:host-gateway`

### B) Kind API server bind address

In `test/e2e/kind/cluster-template.yaml`:

```yaml
networking:
  apiServerAddress: "0.0.0.0"
```

This ensures host publish is reachable from devcontainer via `host.docker.internal`.

### C) Kubeconfig rewrite in cluster setup script

In `test/e2e/kind/start-cluster.sh`, after `kind export kubeconfig`:

- detect kubeconfig server host in `{127.0.0.1, localhost, 0.0.0.0}`
- rewrite to `https://host.docker.internal:<port>`
- set `tls-server-name=localhost`

## Verification Steps

```bash
make setup-cluster
docker inspect gitops-reverser-test-e2e-control-plane --format '{{json .NetworkSettings.Ports}}'
kubectl config view --minify | sed -n '/server:/p;/tls-server-name:/p'
kubectl get nodes
```

Expected:

- Docker `HostIp` for `6443/tcp` is `0.0.0.0` or `::`
- kubeconfig server points to `https://host.docker.internal:<port>`
- `tls-server-name: localhost` is set
- `kubectl get nodes` succeeds inside devcontainer

## Why We Kept This Design

This removes the need for `--network=host` while keeping Kind management from inside the devcontainer working reliably. It is easier to reason about, more explicit, and avoids host-network side effects.
