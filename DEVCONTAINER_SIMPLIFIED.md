# Simplified Dev Container and CI Architecture

## Overview

The dev container and CI setup has been simplified to remove Docker-in-Docker complexity from CI while maintaining full functionality for local development.

## Architecture

### Two-Container Approach

1. **CI Base Container** (`.devcontainer/Dockerfile.ci`)
   - Lightweight container with essential build tools
   - No Docker installed
   - Used in CI for lint, unit tests, and **running e2e tests**
   - Serves as base image for full dev container
   - Published as: `ghcr.io/configbutler/gitops-reverser-ci:latest`

2. **Full Dev Container** (`.devcontainer/Dockerfile`)
   - Extends CI base container
   - Adds Docker and Kind for local development
   - Used for local development in VS Code
   - Published as: `ghcr.io/configbutler/gitops-reverser-devcontainer:latest`

### Benefits

✅ **Simplified CI**: Uses standard Kind action on GitHub Actions runner  
✅ **Faster Builds**: CI container is smaller and builds faster  
✅ **Better Caching**: Shared layers between CI and dev containers  
✅ **Easier Maintenance**: Clear separation of concerns  
✅ **Standard Tooling**: Uses GitHub Actions' `helm/kind-action` for Kind setup  
✅ **Hybrid Approach**: Kind cluster on runner, tests in container

## CI Pipeline Architecture

### Hybrid Approach

```
GitHub Actions Runner (has Docker)
├── Creates Kind cluster (helm/kind-action)
├── Loads application image into Kind
└── Runs tests in CI container
    ├── Mounts kubeconfig from runner
    ├── Uses network=host to access Kind
    └── Executes test suite
```

### Why Hybrid?

- **Kind needs Docker**: Kind creates clusters using Docker, so it must run on the GitHub Actions runner (which has Docker)
- **Tests don't need Docker**: The test code only needs kubectl/helm to interact with the cluster
- **Best of both worlds**: Cluster setup on runner, test execution in controlled container environment

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
          ┌──────────────────────┐
          │  E2E Tests            │
          │  ┌─────────────────┐ │
          │  │ GitHub Runner   │ │
          │  │ - Kind cluster  │ │
          │  │ - Docker        │ │
          │  └─────────────────┘ │
          │         │             │
          │         ▼             │
          │  ┌─────────────────┐ │
          │  │ CI Container    │ │
          │  │ - Run tests     │ │
          │  │ - Access Kind   │ │
          │  └─────────────────┘ │
          └──────────────────────┘
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

## CI E2E Test Flow

### Step-by-Step

1. **Checkout code** on GitHub Actions runner
2. **Create Kind cluster** using `helm/kind-action` (runs on runner, has Docker)
3. **Load application image** into Kind cluster
4. **Run tests in CI container**:
   - Mount workspace and kubeconfig
   - Use `--network host` to access Kind cluster
   - Execute test prerequisites (cert-manager, Gitea, etc.)
   - Run actual e2e test suite

### Key Points

- **Kind cluster**: Lives on GitHub Actions runner
- **Test execution**: Runs in CI container
- **Communication**: Via network=host and mounted kubeconfig
- **No Docker in container**: CI container doesn't need Docker

## Migration Notes

### What Changed

1. **New Files**:
   - `.devcontainer/Dockerfile.ci` - CI base container
   - `DEVCONTAINER_SIMPLIFIED.md` - This documentation

2. **Modified Files**:
   - `.devcontainer/Dockerfile` - Now extends CI base
   - `.github/workflows/ci.yml` - Hybrid approach (Kind on runner, tests in container)
   - `.golangci.yml` - Updated to v2.4.0 compatible format
   - `Makefile` - Kind check is optional

3. **Removed**:
   - Docker-in-Docker in CI container
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

### Tool Versions

| Tool | Version | Where |
|------|---------|-------|
| Go | 1.25.1 | CI container |
| kubectl | v1.32.3 | CI container + kind-action |
| kustomize | 5.7.1 | CI container |
| helm | v3.12.3 | CI container |
| golangci-lint | v2.4.0 | CI container |
| Kind | v0.30.0 | kind-action (runner) + dev container |
| Docker | latest | GitHub runner + dev container |

## Troubleshooting

### CI Container Can't Find Kind

This is expected! Kind runs on the GitHub Actions runner, not in the CI container. Tests run in the container but access the Kind cluster via network=host.

### Local Dev: Docker Not Available

Ensure the `docker-in-docker` feature is enabled in `.devcontainer/devcontainer.json`.

### E2E Tests Fail Locally

1. Check Docker is running: `docker info`
2. Ensure Kind cluster exists: `kind get clusters`
3. Check kubeconfig: `kubectl cluster-info`

### golangci-lint Config Errors

The config was updated for v2.4.0 compatibility:
- Removed: `skip-files`, `skip-dirs`, `staticcheck.go` (deprecated)
- Using: Simplified v2 format with only supported properties

## Performance Improvements

### Build Times
- CI container: ~2.5 minutes (first build with warming)
- Dev container: ~1 minute additional (extends CI)
- E2E setup: <1 minute (Kind action is fast)

### CI Pipeline
- Simplified: No Docker-in-Docker complexity
- Fast: Standard Kind action
- Clean: Tests run in isolated container

## Future Enhancements

Potential improvements:
- [ ] Cache Kind cluster between test runs
- [ ] Parallel e2e test execution
- [ ] ARM64 support for CI container
- [ ] Separate test-only container variant

## References

- [Kind Documentation](https://kind.sigs.k8s.io/)
- [helm/kind-action](https://github.com/helm/kind-action)
- [Docker-in-Docker Feature](https://github.com/devcontainers/features/tree/main/src/docker-in-docker)
- [golangci-lint v2 Config](https://golangci-lint.run/usage/configuration/)