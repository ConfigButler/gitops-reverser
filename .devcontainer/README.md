# Development Container

Quick-start development environment with all tools pre-installed.

## Quick Start

### VS Code
1. Install [Dev Containers extension](https://marketplace.visualstudio.com/items?itemName=ms-vscode-remote.remote-containers)
2. Open project in VS Code: `code .`
3. Press `F1` → `Dev Containers: Reopen in Container`
4. Wait for initial build (~5-10 min first time)

### Verify
```bash
go version        # 1.26.0
kind version      # v0.30.0
kubectl version   # v1.32.3
golangci-lint version  # v2.4.0
```

### Run Tests
```bash
make test         # Unit tests
make lint         # Linting
make test-e2e     # E2E tests (creates Kind cluster)
```

## Architecture

Multi-stage Dockerfile:
- **`ci` stage** - CI base (Go tools, no Docker) - Used in GitHub Actions
- **`dev` stage** - Full dev (extends ci stage, adds Docker+Kind) - Local only

Local dev builds directly from dev stage, no separate initialization needed.

## Files

- `Dockerfile` - Multi-stage container (ci and dev stages)
- `devcontainer.json` - VS Code configuration
- `README.md` - This file

## Troubleshooting

**Container won't build** → Ensure Docker is running  
**Slow rebuild** → Normal, only rebuilds when tools/deps change

See [`docs/COMPLETE_SOLUTION.md`](../docs/COMPLETE_SOLUTION.md) for details.