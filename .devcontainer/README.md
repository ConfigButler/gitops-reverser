# Development Container Setup

This directory contains the configuration for the GitOps Reverser development container, which provides a fully-configured environment with all necessary tools pre-installed.

## Overview

The devcontainer provides:
- **Go 1.25.1** (on Debian Bookworm for Docker compatibility)
- **Kubernetes tools**: kubectl, Kind, Kustomize, Kubebuilder, Helm
- **Linting**: golangci-lint with pre-cached dependencies
- **Docker-in-Docker** for running Kind clusters and e2e tests

## Key Features

### ✅ Local Development
- Works with VS Code Dev Containers extension
- Full IDE integration with Go language server
- Pre-installed Kubernetes and Docker extensions

### ✅ GitHub Actions CI/CD
- Same environment used in CI pipeline (`build-devcontainer` job)
- Consistent behavior between local and CI
- Registry caching for fast rebuilds

### ✅ Efficient Caching
- **Go modules**: Cached via Docker layer (rebuilds only when `go.mod`/`go.sum` change)
- **Go tools**: controller-gen and setup-envtest installed in separate layer
- **golangci-lint**: Dependencies pre-cached without requiring source code
- **Docker BuildKit**: Multi-stage builds with registry caching in CI

## Local Usage

### Prerequisites
- Docker Desktop or Docker Engine
- VS Code with [Dev Containers extension](https://marketplace.visualstudio.com/items?itemName=ms-vscode-remote.remote-containers)

### Starting the Container

1. **Open in VS Code**:
   ```bash
   code /home/simon/git/gitops-reverser
   ```

2. **Reopen in Container**:
   - Press `F1` or `Ctrl+Shift+P`
   - Select: `Dev Containers: Reopen in Container`
   - Wait for container to build (first time takes ~5-10 minutes)

3. **Verify Setup**:
   ```bash
   # Inside the container
   go version
   kind version
   kubectl version --client
   golangci-lint version
   ```

### Running Tests

```bash
# Unit tests (no Docker required)
make test

# Linting (uses cached dependencies)
make lint

# E2E tests (requires Docker)
make test-e2e
```

## GitHub Actions Integration

The devcontainer is built once per CI run and reused across jobs:

```yaml
# .github/workflows/ci.yml
jobs:
  build-devcontainer:
    # Builds and pushes to GHCR with caching
    
  lint-and-test:
    needs: build-devcontainer
    container:
      image: ${{ needs.build-devcontainer.outputs.image }}
    
  e2e-test:
    needs: build-devcontainer
    container:
      image: ${{ needs.build-devcontainer.outputs.image }}
      options: --privileged -v /var/run/docker.sock:/var/run/docker.sock
```

### CI Caching Strategy

1. **Registry Cache**: 
   - `type=registry,ref=ghcr.io/configbutler/gitops-reverser-devcontainer:buildcache`
   - Caches all Docker layers across CI runs
   
2. **Module Cache**:
   - Go modules layer only rebuilds when `go.mod`/`go.sum` change
   - Cached in registry for fast rebuilds

3. **Tool Cache**:
   - Go tools and golangci-lint dependencies cached in separate layers
   - Rarely change, so highly cacheable

## Architecture Decisions

### Why Debian Bookworm?

The base image uses `golang:1.25.1-bookworm` instead of the latest `golang:1.25.1` because:
- Latest uses Debian Trixie
- Trixie removed `moby-cli` and related packages
- Docker-in-Docker feature requires Bookworm compatibility
- Setting `"moby": false` uses Docker CE instead

### Why Simplified golangci-lint Caching?

Previous approach:
```dockerfile
# ❌ Old approach - copied all source code
COPY api/ cmd/ internal/ hack/ ./
RUN golangci-lint run || true
RUN rm -rf api cmd internal hack
```

Problems:
- Copied source code unnecessarily
- Cache invalidated on any code change
- Deleted code after linting (confusing)

New approach:
```dockerfile
# ✅ New approach - minimal initialization
RUN mkdir -p /tmp/golangci-init && cd /tmp/golangci-init \
    && go mod init example.com/init \
    && echo 'package main\n\nfunc main() {}' > main.go \
    && golangci-lint run --timeout=5m || true \
    && cd / && rm -rf /tmp/golangci-init
```

Benefits:
- Pre-caches linter dependencies without source code
- Cache stable (doesn't invalidate on code changes)
- Cleaner and more maintainable

### Why Docker-in-Docker?

E2E tests require:
- Kind clusters (Kubernetes in Docker)
- Docker build for test images
- Network isolation

The devcontainer feature `ghcr.io/devcontainers/features/docker-in-docker:2` provides this with:
- `"moby": false` - Use Docker CE (compatible with Bookworm)
- `"dockerDashComposeVersion": "v2"` - Modern Compose CLI

## Troubleshooting

### Container fails to build with Docker-in-Docker error

**Error**:
```
(!) The 'moby' option is not supported on Debian 'trixie'
```

**Solution**: Ensure using `golang:1.25.1-bookworm` base image and `"moby": false` in `devcontainer.json`.

### E2E tests fail with "Cannot connect to Docker"

**Local**: Ensure Docker Desktop is running
```bash
docker info  # Should show Docker daemon info
```

**CI**: Job must include:
```yaml
container:
  options: --privileged -v /var/run/docker.sock:/var/run/docker.sock
```

### Slow rebuild after changing code

This is expected - only Go module cache is preserved. Code changes should not rebuild the entire container.

If you need to rebuild from scratch:
```bash
# Local
Ctrl+Shift+P → "Dev Containers: Rebuild Container Without Cache"

# CI
Clear registry cache by pushing with new tag
```

## Files

- [`Dockerfile`](./Dockerfile) - Multi-stage build with tool installation
- [`devcontainer.json`](./devcontainer.json) - VS Code devcontainer configuration
- [`README.md`](./README.md) - This file

## References

- [VS Code Dev Containers](https://code.visualstudio.com/docs/devcontainers/containers)
- [Docker-in-Docker Feature](https://github.com/devcontainers/features/tree/main/src/docker-in-docker)
- [GitHub Actions Docker Build](https://docs.docker.com/build/ci/github-actions/)