# Windows Devcontainer Setup

Last updated: 2026-02-13

## Why Windows behaves differently

When the repo is on the Windows filesystem and mounted into a Linux devcontainer, the mounted workspace does not behave exactly like a native Linux filesystem.

Typical effects:

- ownership/permission friction on the mounted workspace
- slower file I/O than WSL-native storage
- Linux ACL-based fixes that work under `/go` do not fully apply to the mounted source tree

## Current repo behavior

This repo uses:

- active workspace path: `/workspaces/<repo>`
- `remoteUser`: `vscode`
- post-create hook: `.devcontainer/post-create.sh` (called with `${containerWorkspaceFolder}`)

The post-create script attempts to normalize ownership for the mounted workspace and `/home/vscode` caches.

## Recommended setup (Windows)

1. Use WSL2.
2. Clone the repository inside the Linux filesystem (for example under `~/git/...` in Ubuntu).
3. Open from WSL in VS Code, then reopen in container.

This is the most reliable and fastest setup.

## Quick checks inside devcontainer

```bash
pwd
ls -ld .
id
```

Expected:

- current directory under `/workspaces/<repo>`
- effective user is `vscode`
- workspace is writable by `vscode`

## If workspace is still not writable

Run:

```bash
bash .devcontainer/post-create.sh "${containerWorkspaceFolder:-$(pwd)}"
```

Then verify:

```bash
touch .permission-check && rm .permission-check
go mod tidy
```

## Notes about `/go` vs workspace

- `/go` is container filesystem and uses Linux semantics; ACL/setgid strategy documented in `GO_MODULE_PERMISSIONS.md` applies there.
- `/workspaces/<repo>` is a bind mount from host; behavior depends on host filesystem and Docker Desktop integration.

## References

- [Docker Desktop + WSL2](https://docs.docker.com/desktop/features/wsl/)
- [VS Code Remote - WSL](https://code.visualstudio.com/docs/remote/wsl)
