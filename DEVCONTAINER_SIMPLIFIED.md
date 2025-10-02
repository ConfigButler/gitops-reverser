# Simplified Dev Container and CI Architecture

## Overview

The dev container and CI setup has been simplified to remove Docker-in-Docker complexity from CI while maintaining full functionality for local development.

## Architecture

### Two-Container Approach

1. **CI Base Container** (`.devcontainer/Dockerfile.ci`)
   - Lightweight container with essential build tools
   - No Docker installed
   - Used in CI for lint, unit tests, and e2e tests
   - Serves as base image for full dev container
   - Published as: `ghcr.io/configbutler/gitops-reverser-ci:latest`

2. **Full Dev Container** (`.devcontainer/Dockerfile`)
   - Extends CI base container
   - Adds Docker and Kind for local development
   - Used for local development in VS Code
   - Published as: `ghcr.io/configbutler/gitops-reverser-devcontainer:latest`

### Benefits

✅ **Simplified CI**: No Docker-in-Docker complexity in GitHub Actions  
✅ **Faster Builds**: CI container is smaller and builds faster  
✅ **Better Caching**: Shared layers between CI and dev containers  
✅ **Easier Maintenance**: Clear separation of concerns  
✅ **Standard Tooling**: Uses GitHub Actions' `helm/kind-action` for Kind setup  

## CI Pipeline Changes

### Before (Docker-in-Docker)
```yaml
container:
  image: devcontainer
  options: --privileged -v /var/run/docker.sock:/var/run/docker.sock

steps:
  - Complex Kind network setup
  - Manual Docker network bridging
  - Custom kubeconfig manipulation
  - 200+ lines of networking diagnostics
```

### After (Native Kind Action)
```yaml
container:
  image: ci-base-container

steps:
  - uses: helm/kind-action@v1
    with:
      cluster_name: gitops-reverser-test-e2e
  - Run e2e tests (no network setup needed)
```

## Local Development

### Dev Container Features
- Docker-in-Docker via `docker-in-docker:2` feature
- Kind for local Kubernetes clusters
- All Go development tools pre-installed
- VSCode extensions pre-configured

### Running E2E Tests Locally

The dev container has Docker and Kind, so you can run e2e tests directly:

```bash
# Create cluster and run tests
make test-e2e

# Or manually:
kind create cluster --name gitops-reverser-test-e2e
make setup-cert-manager
make setup-gitea-e2e
KIND_CLUSTER=gitops-reverser-test-e2e go test ./test/e2e/ -v
```

## CI Pipeline Flow

```
┌─────────────────────┐
│ Build CI Container  │ (Dockerfile.ci)
│ - Go tools          │
│ - kubectl, helm     │
│ - golangci-lint     │
└──────────┬──────────┘
           │
           ├─────────────────────┬──────────────────────┐
           │                     │                      │
           ▼                     ▼                      ▼
   ┌───────────────┐   ┌─────────────────┐   ┌──────────────────┐
   │ Lint & Test   │   │  Build Docker   │   │ Build Dev        │
   │ (CI container)│   │  Image          │   │ Container        │
   └───────┬───────┘   └────────┬────────┘   │ (extends CI)     │
           │                    │             └──────────────────┘
           │                    │
           └────────┬───────────┘
                    │
                    ▼
          ┌──────────────────┐
          │  E2E Tests        │
          │  - Kind via       │
          │    kind-action    │
          │  - No Docker      │
          │    complexity     │
          └──────────────────┘
```

## Migration Notes

### What Changed

1. **New Files**:
   - `.devcontainer/Dockerfile.ci` - CI base container
   - `DEVCONTAINER_SIMPLIFIED.md` - This documentation

2. **Modified Files**:
   - `.devcontainer/Dockerfile` - Now extends CI base
   - `.github/workflows/ci.yml` - Uses Kind action
   - `Makefile` - Kind check is optional

3. **Removed**:
   - 200+ lines of Docker network diagnostics
   - Manual kubeconfig manipulation
   - Complex network bridging logic

### Container Images

Both containers are published to GitHub Container Registry:

```bash
# CI Base (used in GitHub Actions)
docker pull ghcr.io/configbutler/gitops-reverser-ci:latest

# Dev Container (used locally)
docker pull ghcr.io/configbutler/gitops-reverser-devcontainer:latest
```

### Environment Variables

E2E tests now use standard environment variables:

```bash
PROJECT_IMAGE=<image>  # Image to test
KIND_CLUSTER=<name>    # Cluster name (default: gitops-reverser-test-e2e)
```

## Troubleshooting

### CI Container Can't Find Kind

This is expected! The CI container doesn't include Kind. GitHub Actions uses the `helm/kind-action` to set it up.

### Local Dev: Docker Not Available

Ensure the `docker-in-docker` feature is enabled in `.devcontainer/devcontainer.json`.

### E2E Tests Fail Locally

1. Check Docker is running: `docker info`
2. Ensure Kind cluster exists: `kind get clusters`
3. Check kubeconfig: `kubectl cluster-info`

## Performance Improvements

### Build Times
- CI container: ~3-5 minutes (cached: <1 minute)
- Dev container: ~1-2 minutes additional (extends CI)
- Previous DinD setup: ~5-7 minutes

### CI Pipeline
- Removed: Complex network setup (~2-3 minutes)
- Added: Native Kind action (~1 minute)
- Net improvement: ~1-2 minutes per pipeline run

## Future Enhancements

Potential improvements:
- [ ] Multi-stage CI container (build vs runtime tools)
- [ ] ARM64 support for CI container
- [ ] Separate test-only container for faster test runs
- [ ] Cache Go module downloads across jobs

## References

- [Kind Documentation](https://kind.sigs.k8s.io/)
- [helm/kind-action](https://github.com/helm/kind-action)
- [Docker-in-Docker Feature](https://github.com/devcontainers/features/tree/main/src/docker-in-docker)