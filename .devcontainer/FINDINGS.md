## Findings: `make lint`, cache behavior, and workspace paths

### 1) `make lint` did not use a warm module cache in this run

`make lint` executes:

```make
lint:
	$(GOLANGCI_LINT) run
```

There is no `GOMODCACHE` override in `Makefile`, so cache behavior depends on the runtime environment defaults.

Evidence collected during debugging:

- `go env` reported:
  - `GOMODCACHE=/go/pkg/mod`
  - `GOCACHE=/home/vscode/.cache/go-build`
- Running `go list ./...` showed many `go: downloading ...` lines, which indicates cache misses (or unavailable cache entries) for current dependencies.
- In restricted execution, writes/access under `/go/pkg/mod/cache/...` were blocked, and module fetches to `proxy.golang.org` were also blocked, which prevented normal dependency resolution.
- This produced a misleading top-level `golangci-lint` error (`no go files to analyze`) even though Go files exist.

Conclusion: in this environment, `make lint` did not have an effectively usable warm module cache path for dependency resolution.

### 2) `/workspace` is valid in a devcontainer, but not always the active workspace mount

Observed runtime paths:

- Active repo path: `/workspaces/gitops-reverser2`
- `/workspace` exists, but only contains files copied during image build steps.

Why this happens:

- `Dockerfile` build steps create image-layer content (here under `/workspace`).
- VS Code Dev Containers then bind-mount your real host repo into the running container (commonly under `/workspaces/<repo>` unless overridden).

Implication in this repo:

- `.devcontainer/devcontainer.json` `postCreateCommand` currently runs `chown` on `/workspace`.
- That command is valid, but it does not affect the mounted repo at `/workspaces/gitops-reverser2` in this session.

### 3) Main cause of the lint failure seen in Codex

Primary cause was execution constraints in this Codex session (restricted network and restricted writable roots), not an intrinsic Go/lint config break in the repository.

When lint was run with elevated permissions (normal module/network access), it completed and reported actionable issues.

### 4) Best-practice model: bind mounts for source, volumes for caches

Use this mental model:

- Source code: bind mount (live, editable, synced with host filesystem).
- Tool and dependency caches: Docker volumes (persistent across container rebuilds, independent of source tree).

For Go specifically:

- `GOMODCACHE` should map to `/go/pkg/mod` (module download cache).
- `GOCACHE` should map to `/home/vscode/.cache/go-build` (compiled package/build cache).

### 5) Recommended improvements for this repo

1. Make workspace targeting explicit (optional but reduces ambiguity).

```json
{
  "workspaceFolder": "/workspaces/${localWorkspaceFolderBasename}"
}
```

2. Avoid hardcoding `/workspace` in post-create logic.

Use `${containerWorkspaceFolder}` or relative paths:

```json
{
  "postCreateCommand": "sudo chown -R vscode:vscode ${containerWorkspaceFolder} || true"
}
```

3. Persist Go caches via named volumes.

```json
{
  "mounts": [
    "source=gomodcache,target=/go/pkg/mod,type=volume",
    "source=gobuildcache,target=/home/vscode/.cache/go-build,type=volume",
    "source=/var/run/docker.sock,target=/var/run/docker.sock,type=bind"
  ]
}
```

Notes:

- The cache targets above are intentionally mapped to Go defaults in this container.
- Earlier advice that swapped these two cache targets is incorrect.

### 6) Practical balance (local machine vs container)

- Keep source code on the host via bind mount for normal editor/Git workflow.
- Keep heavy generated caches and dependencies in container volumes for speed and reproducibility.
- Keep absolute paths out of scripts unless they are the canonical runtime paths for this specific devcontainer configuration.
