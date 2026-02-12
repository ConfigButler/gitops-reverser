# Troubleshooting `make setup-cluster` in DevContainer

## Symptom

`make setup-cluster` fails and Kind waits for the control-plane API server, with logs like:

```
Get "https://172.19.0.2:6443/livez?timeout=10s": dial tcp 172.19.0.2:6443: connect: connection refused
```

## Root cause (current setup)

`test/e2e/kind/start-cluster.sh` generates `test/e2e/kind/cluster.ignore.yaml` from `HOST_PROJECT_PATH`.

In the current devcontainer config, `HOST_PROJECT_PATH` is set from `${localWorkspaceFolder}`.  
That produced:

```
hostPath: /home/simon/git/gitops-reverser2/test/e2e/kind/audit
```

But that mounted directory exists and is empty in the container, while the real audit files are under:

```
/workspaces/gitops-reverser2/test/e2e/kind/audit
```

Because the mount source is wrong/empty, kube-apiserver cannot read:

- `/etc/kubernetes/audit/policy.yaml`
- `/etc/kubernetes/audit/webhook-config.yaml`

Then kube-apiserver fails startup, and Kind reports API server connection refused on `:6443`.

## Why this happens

The path strategy differs by Docker mode:

- Host Docker socket mode: daemon needs host-visible paths.
- Docker-in-Docker mode: daemon needs container-visible paths.

Your current config mixes modes and path assumptions, so Kind mount path resolution is inconsistent.

## Fix options

1. Use Docker-in-Docker only (recommended)
- Remove host socket mount from `.devcontainer/devcontainer.json`.
- Set `HOST_PROJECT_PATH` to container workspace path (for example `/workspaces/${localWorkspaceFolderBasename}`).

2. Use host Docker socket only
- Remove `docker-in-docker` feature.
- Keep `HOST_PROJECT_PATH` as host path.

## Quick verification

Before running `make setup-cluster`, verify generated config points to a path with files:

```bash
cat test/e2e/kind/cluster.ignore.yaml
ls -la <hostPath-from-generated-file>
```

Expected: `policy.yaml` and `webhook-config.yaml` are present.

## Immediate workaround

Run setup with a container-visible path explicitly:

```bash
HOST_PROJECT_PATH=/workspaces/$(basename "$PWD") make setup-cluster
```

