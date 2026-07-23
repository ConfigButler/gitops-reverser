# CI/Devcontainer Findings

## Current Baseline

Last updated: 2026-06-02

Recent investigation: [E2E full-suite shared-state flake](../spec/e2e-serial-registry.md)

This folder documents why the repository uses its current devcontainer and CI behavior, especially
around Go caches, workspace paths, and k3d cluster access from inside the container.

### 1) Workspace path model

Current devcontainer intentionally uses:

- `workspaceMount`: `source=${localWorkspaceFolder},target=/workspaces/${localWorkspaceFolderBasename},type=bind`
- `workspaceFolder`: `/workspaces/${localWorkspaceFolderBasename}`

Implications:

- Active source tree is `/workspaces/<repo>`.
- `/workspace` may exist in image layers, but it is not the active bind mount for day-to-day development in this repo.

### 2) Post-create ownership model

`devcontainer.json` runs:

```json
"postCreateCommand": "bash .devcontainer/post-create.sh '${containerWorkspaceFolder}'"
```

The script resolves the workspace path dynamically and fixes ownership for:

- the mounted workspace
- `/home/vscode` cache areas used by tools

This avoids hardcoded path assumptions and keeps Linux/macOS/Windows setups more consistent.

### 3) Go cache persistence model

The repository persists heavy Go caches using named Docker volumes:

- `/go/pkg/mod` (`gomodcache`)
- `/home/vscode/.cache/go-build` (`gobuildcache`)

Why:

- Faster rebuild/reopen cycles
- Stable module/build caching independent of repo bind mount
- Fewer permission regressions than putting caches in the workspace tree

### 4) k3d + kubectl access model inside devcontainer

The current working model is:

- Devcontainer does **not** use `--network=host`
- Devcontainer run args include:
  - `--group-add=docker`
  - `--add-host=host.docker.internal:host-gateway`
- `test/e2e/cluster/start-cluster.sh` lets k3d pick the API server port, then
  rewrites kubeconfig server endpoints from `127.0.0.1|localhost|0.0.0.0` to
  `host.docker.internal:<picked-port>` and sets `tls-server-name=localhost`

Why this is required:

- If Docker publishes the k3d API server on host loopback only (`127.0.0.1`), it is not reachable
  via `host.docker.internal` from the container.
- The kubeconfig rewrite to `host.docker.internal` makes in-container `kubectl` stable without host networking.

### 5) Practical verification checklist

After devcontainer rebuild/reopen:

```bash
# 1) Cluster setup (creates the k3d cluster via the e2e Taskfile)
bash test/e2e/cluster/start-cluster.sh

# 2) Confirm API publish bind (expected 0.0.0.0 or ::)
docker inspect k3d-gitops-reverser-test-e2e-server-0 --format '{{json .NetworkSettings.Ports}}'

# 3) Confirm kubeconfig server rewrite
kubectl config view --minify | sed -n '/server:/p;/tls-server-name:/p'

# 4) Confirm cluster access
kubectl get nodes
```

### 6) kind does not run in this devcontainer — do not retry it

A `kind` cluster cannot be started from inside this devcontainer, and no amount of
kind-version bumping fixes it. The host's `fs.inotify.max_user_instances` is 128,
which is too low for kind's systemd + kubelet + CRI to all allocate watchers, and
`/proc/sys/fs/inotify` is mounted **read-only** because the Docker-in-Docker setup
does not share a writable sysctl namespace with the host:

| Attempt | Result |
| --- | --- |
| `sudo sysctl fs.inotify.max_user_instances=8192` | `permission denied on key` |
| `sudo sh -c 'echo 8192 > /proc/sys/fs/inotify/...'` | `Read-only file system` |
| kind v0.24.0 → v0.27.0 (newer node image) | same inotify failure — a newer kind does not fix a host limit |

Raising the limit needs changes to the *outer* host or the devcontainer feature
config. **Use k3d, which is what the e2e suite already does.**

Recorded 2026-07-11 from a since-deleted scratch investigation
(`hack/audit-kind-check/`), which tried to stand up kind to compare aggregated-API
audit richness against k3s. That question was ultimately answered by reading the
upstream kube source in `external-sources/kubernetes/` instead — strictly more
conclusive, since a single kind run proves behaviour for one kube version whereas
the source shows it is structural. The aggregated-API e2e now ships against k3d
(`test/e2e/aggregated_apiserver_e2e_test.go`).

### 7) Related docs in this folder

- `go-module-permissions.md` - why `/go` permissions are managed with shared group + ACLs
- `windows-devcontainer.md` - Windows-specific mount behavior and expected differences
- `ci-root-user.md` - why CI containers run as root
- `git-safe-dir-explained.md` - why `safe.directory` is required in containerized CI
- `e2e-allure-reporting.md` - how CI renders visual Allure reports from Ginkgo JSON
