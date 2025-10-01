# Dev Container Setup

This directory contains the development container configuration for the GitOps Reverser project. It provides a consistent development environment both locally and in CI/CD pipelines.

## 🏗️ Architecture

### Separation of Concerns

```
.devcontainer/Dockerfile     → Development tools + cached dependencies
Dockerfile (root)            → Minimal production image (distroless)
```

**Why separate?**
- **Dev container**: Includes Kind, kubectl, golangci-lint, Go modules, etc. (~2GB)
- **Production image**: Only the compiled binary on distroless base (~20MB)
- Mixing them would bloat production images unnecessarily

## 📦 What's Included

The dev container comes pre-installed with:

- **Go 1.25.1** with all project dependencies cached
- **Kubernetes Tools**:
  - Kind v0.30.0
  - kubectl v1.32.3
  - Kustomize v5.4.1
  - Kubebuilder 4.4.0
  - Helm v3.12.3
- **Development Tools**:
  - golangci-lint v2.4.0
  - controller-gen
  - setup-envtest
- **Docker-in-Docker** for Kind clusters

## 🚀 Local Development

### Using with VS Code

1. Install the "Dev Containers" extension
2. Open this project in VS Code
3. Click "Reopen in Container" when prompted
4. Wait for the container to build (first time only)

The container will:
- Mount your workspace
- Install all tools
- Pre-download Go modules
- Create the Kind network

### Manual Docker Usage

```bash
# Build the dev container
docker build -f .devcontainer/Dockerfile -t gitops-reverser-dev .

# Run interactively
docker run -it --privileged -v $(pwd):/workspace gitops-reverser-dev bash

# Inside the container
make test
make lint
make test-e2e
```

## 🔄 CI/CD Integration

### How It Works

Every CI run follows this simple flow:

1. **Build Dev Container** (first job in `.github/workflows/ci.yml`):
   - Builds dev container for the current commit
   - Uses Docker layer caching (rebuilds in ~1-2 min)
   - Pushes with commit SHA tag and `latest` tag
   - Self-contained and always correct

2. **Use in Jobs**:
   - `lint-and-test` job uses the built container
   - `e2e-test` job uses the built container
   - Tools are already installed → no setup time
   - Go modules already cached → no download time

**Key Benefits:**
- ✅ Self-contained - no separate build workflow needed
- ✅ Always sound - exact container for each commit
- ✅ Fast - Docker layer caching keeps rebuilds quick
- ✅ Simple - no fallback logic or edge cases

### Cache Strategy

```yaml
cache-from: type=registry,ref=ghcr.io/.../gitops-reverser-devcontainer:buildcache
cache-to: type=registry,ref=ghcr.io/.../gitops-reverser-devcontainer:buildcache,mode=max
```

Docker BuildKit caches layers in the registry, making rebuilds extremely fast.

## 🎯 Benefits

### Local Development
- ✅ Consistent environment across all developers
- ✅ No manual tool installation
- ✅ Works on any platform (Windows, Mac, Linux)
- ✅ Isolated from host system

### CI/CD Pipeline
- ✅ **~3-5 minutes faster** per CI run (no tool installation)
- ✅ **Consistent** with local dev environment
- ✅ **Reliable** - no flaky package downloads during CI
- ✅ **Cost-effective** - less CI minutes consumed

## 🔧 Maintenance

### Updating Tool Versions

Edit `.devcontainer/Dockerfile`:

```dockerfile
ENV KIND_VERSION=v0.30.0 \
    KUBECTL_VERSION=v1.32.3 \
    ...
```

Push to trigger automatic rebuild.

### Updating Go Dependencies

When `go.mod` or `go.sum` changes:
1. Next CI run rebuilds dev container with new deps
2. New dependencies are cached in the image layer
3. Subsequent CI runs use cached layers (fast)

### Troubleshooting

**Dev container build slow on first run:**
- This is expected - downloading and caching all tools
- Subsequent builds use Docker layer cache (~1-2 min)

**Tools not working in dev container:**
- Rebuild the container: Cmd+Shift+P → "Rebuild Container"
- Check tool versions in Dockerfile

**Kind cluster issues:**
- Ensure Docker-in-Docker is enabled
- Check that `--privileged` flag is set (required for Kind)

## 📚 References

- [Dev Containers Specification](https://containers.dev/)
- [GitHub Actions: Running Jobs in Containers](https://docs.github.com/en/actions/using-jobs/running-jobs-in-a-container)
- [Docker BuildKit Cache](https://docs.docker.com/build/cache/)