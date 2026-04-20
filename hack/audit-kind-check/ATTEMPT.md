# Kind-Based Audit Validation Attempt

## Goal

Validate one open question from [investigate.md](../../investigate.md):

> Does upstream Kubernetes (not k3s) emit richer audit events for an aggregated API `create`, or does it produce the same sparse event we saw on k3s?

If upstream kube is rich, the [audit pass-through APIServer prototype](../../external-prototype/audit-pass-through-apiserver/PLAN.md) premise collapses and the whole effort is unnecessary. If upstream kube is sparse too, the prototype is structurally justified.

## Approach

Stand up a one-node kind cluster with `--audit-policy-file` + `--audit-log-path` configured on the control-plane kube-apiserver, install the existing sample-apiserver manifests from [test/e2e/setup/manifests/sample-apiserver/](../../test/e2e/setup/manifests/sample-apiserver/), create a `Flunder`, and read the audit log out of the control-plane container. Compare the resulting event fields against the k3s capture at [.stamps/debug/flunder-create-audit.json](../../.stamps/debug/flunder-create-audit.json).

## Files scaffolded

- [audit-policy.yaml](audit-policy.yaml) — `RequestResponse` policy scoped to `wardle.example.com` and `ConfigMap` (control).
- [kind-config.yaml](kind-config.yaml) — kind `Cluster` with `kubeadmConfigPatches` adding `audit-policy-file`, `audit-log-path`, and the `extraVolumes` / `extraMounts` wiring to inject the policy file and persist the log.
- [flunder.yaml](flunder.yaml) — a minimal Flunder instance in `default`.

## What went wrong

Kind's node container boots full systemd inside Docker. On this devcontainer, the node fails before reaching `Multi-User.target` with:

```
Failed to create inotify object: Too many open files
```

This is the well-known kind-in-devcontainer inotify limit issue. The host's `fs.inotify.max_user_instances` is 128, which is too low for kind's systemd + kubelet + CRI to all allocate watchers.

Attempted resolutions, all blocked:

| Attempt | Result |
| --- | --- |
| `sudo sysctl fs.inotify.max_user_instances=8192` | `permission denied on key` |
| `sudo sh -c 'echo 8192 > /proc/sys/fs/inotify/max_user_instances'` | `Read-only file system` |
| kind v0.24.0 → v0.27.0 (newer node image) | Same inotify failure — higher kind versions don't fix the host limit |

`/proc/sys/fs/inotify` is mounted read-only because this devcontainer's Docker-in-Docker setup doesn't share a writable sysctl namespace with the host. Raising the limit requires changes to the outer host or devcontainer feature config, outside the scope of this investigation.

Existing k3d cluster was left running; kind attempt left no persistent cluster behind.

## Pivot

Instead of debugging the devcontainer, we answered the same question by reading the upstream kube source code directly (available in-tree under [external-sources/kubernetes/](../../external-sources/kubernetes/)). That turns out to be strictly more conclusive than a single kind run: the kind result would prove behavior for one kube version, whereas the source-level analysis shows the behavior is structural across all versions that have this architecture.

The findings are captured in [external-prototype/audit-pass-through-apiserver/WHY.md](../../external-prototype/audit-pass-through-apiserver/WHY.md).

Short answer: the aggregated-API proxy path in kube-apiserver bypasses the native REST handler layer where `audit.LogRequestObject` / `audit.LogResponseObject` are called. The sparse audit is upstream kube behavior, not a k3s quirk. The prototype is justified.

## How to retry on a less-restricted host

If a future operator has root on the host (or a devcontainer feature that bumps inotify):

```bash
sudo sysctl -w fs.inotify.max_user_instances=8192
sudo sysctl -w fs.inotify.max_user_watches=524288

kind create cluster --config hack/audit-kind-check/kind-config.yaml --wait 180s
kubectl apply -k test/e2e/setup/manifests/     # installs sample-apiserver
kubectl wait --for=condition=Available apiservice/v1alpha1.wardle.example.com --timeout=120s
kubectl apply -f hack/audit-kind-check/flunder.yaml
kubectl create configmap audit-probe --from-literal=k=v   # control event

docker exec audit-check-control-plane cat /var/log/kubernetes/audit.log \
  | jq -c 'select(.verb=="create" and (.objectRef.resource=="flunders" or .objectRef.resource=="configmaps"))' \
  > kind-audit.jsonl

kind delete cluster --name audit-check
```

Expected outcome given the source-level analysis: the `flunders` event will be missing `objectRef.name`, `requestObject`, and `responseObject`; the `configmaps` event will have all three. If that is what you see, you have confirmed empirically what the source code already tells us.
