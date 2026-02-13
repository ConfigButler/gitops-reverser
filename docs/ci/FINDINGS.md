## CI/Devcontainer Findings (Current Baseline)

Last updated: 2026-02-13

This folder documents why the repository uses its current devcontainer and CI behavior, especially around Go caches, workspace paths, and Kind access from inside the container.

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

### 4) Kind + kubectl access model inside devcontainer

The current working model is:

- Devcontainer does **not** use `--network=host`
- Devcontainer run args include:
  - `--group-add=docker`
  - `--add-host=host.docker.internal:host-gateway`
- Kind cluster config sets:
  - `networking.apiServerAddress: "0.0.0.0"`
- `test/e2e/kind/start-cluster.sh` rewrites kubeconfig server endpoints from
  `127.0.0.1|localhost|0.0.0.0` to `host.docker.internal:<port>` and sets
  `tls-server-name=localhost`

Why this is required:

- If Docker publishes Kind API server on host loopback only (`127.0.0.1`), it is not reachable via `host.docker.internal` from the container.
- Binding on `0.0.0.0` plus kubeconfig rewrite makes in-container `kubectl` stable without host networking.

### 5) CI root vs non-root stance

Current recommendation remains:

- CI build containers can run as root (ephemeral build context)
- Production runtime must run non-root (already implemented)

Rationale:

- Keeps CI simpler and less fragile
- Avoids unnecessary permission workarounds
- Preserves security boundary at runtime where it matters most

### 6) Git safe.directory note

`safe.directory` in CI is a normal response to UID mismatch between checkout ownership and container process user. This is not, by itself, evidence that CI must be non-root.

### 7) Practical verification checklist

After devcontainer rebuild/reopen:

```bash
# 1) Kind setup
make setup-cluster

# 2) Confirm API publish bind (expected 0.0.0.0 or ::)
docker inspect gitops-reverser-test-e2e-control-plane --format '{{json .NetworkSettings.Ports}}'

# 3) Confirm kubeconfig server rewrite
kubectl config view --minify | sed -n '/server:/p;/tls-server-name:/p'

# 4) Confirm cluster access
kubectl get nodes
```

### 8) Related docs in this folder

- `KUBECTL_TLS_DEBUG_REPORT.md` - incident timeline and final fix
- `GO_MODULE_PERMISSIONS.md` - why `/go` permissions are managed with shared group + ACLs
- `WINDOWS_DEVCONTAINER_SETUP.md` - Windows-specific mount behavior and expected differences
- `CI_NON_ROOT_USER_ANALYSIS.md` - tradeoffs for CI user model
- `GIT_SAFE_DIRECTORY_EXPLAINED.md` - why `safe.directory` is required in containerized CI
